/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	proxybuilder "github.com/OmniTrustILM/operator/internal/builder/proxy"
)

const (
	timeout  = time.Second * 10
	interval = time.Millisecond * 250

	extraConfigMapName = "extra-cm"
)

// unsignedToken returns an unsigned JWT carrying the given exp (0 = no exp claim).
func unsignedToken(expUnix int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := map[string]any{"v": 1, "config": map[string]any{}}
	if expUnix != 0 {
		claims["exp"] = expUnix
	}
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// newTokenSecret builds the config-token Secret named "cfg" in the given namespace.
func newTokenSecret(ns, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns},
		StringData: map[string]string{"configToken": token},
	}
}

// newProxyCR builds a Proxy referencing the per-namespace "cfg" token Secret.
func newProxyCR(name, ns string) *otilmv1alpha1.Proxy {
	return &otilmv1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: otilmv1alpha1.ProxySpec{
			ConfigTokenSecretRef: otilmv1alpha1.ConfigTokenRef{Name: "cfg"},
		},
	}
}

var nsCounter int

func newNamespace() string {
	nsCounter++
	name := fmt.Sprintf("proxy-test-%d", nsCounter)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	})).To(Succeed())
	return name
}

// degradedReason polls the Proxy and returns the Degraded condition's reason when
// the condition is True, otherwise "".
func degradedReason(name, ns string, px *otilmv1alpha1.Proxy) func() string {
	return func() string {
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, px)
		if c := apimeta.FindStatusCondition(px.Status.Conditions, condDegraded); c != nil && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
		return ""
	}
}

var _ = Describe("Proxy controller", func() {
	It("renders children with the token env wiring, checksum, and status", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p1", ns))).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p1", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())

		c := deploy.Spec.Template.Spec.Containers[0]
		var tokenEnv *corev1.EnvVar
		for i := range c.Env {
			if c.Env[i].Name == proxybuilder.EnvConfigToken {
				tokenEnv = &c.Env[i]
			}
		}
		Expect(tokenEnv).NotTo(BeNil())
		Expect(tokenEnv.ValueFrom.SecretKeyRef.Name).To(Equal("cfg"))
		Expect(deploy.Spec.Template.Annotations[proxybuilder.ChecksumAnnotation]).NotTo(BeEmpty())

		var svc corev1.Service
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "p1", Namespace: ns}, &svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(2))

		var sa corev1.ServiceAccount
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "p1", Namespace: ns}, &sa)).To(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p1", Namespace: ns}, &px)
			return px.Status.ObservedVersion
		}, timeout, interval).ShouldNot(BeEmpty())
		Expect(px.Finalizers).To(ContainElement(finalizerName))
		Expect(px.Status.ConfigChecksum).NotTo(BeEmpty())
		Expect(px.Status.Phase).To(Equal(otilmv1alpha1.ProxyPhaseDeploying))
	})

	It("degrades with MissingSecret when the token Secret is absent, and recovers when it appears", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newProxyCR("p2", ns))).To(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(degradedReason("p2", ns, &px), timeout, interval).Should(Equal("MissingSecret"))
		Expect(px.Status.Phase).To(Equal(otilmv1alpha1.ProxyPhaseFailed))
		avail := apimeta.FindStatusCondition(px.Status.Conditions, condAvailable)
		Expect(avail).NotTo(BeNil())
		Expect(avail.Status).To(Equal(metav1.ConditionFalse), "a Failed proxy must not report Available=True")

		// Applying the Secret must clear the stale Degraded condition.
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Eventually(func() metav1.ConditionStatus {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p2", Namespace: ns}, &px)
			if c := apimeta.FindStatusCondition(px.Status.Conditions, condDegraded); c != nil {
				return c.Status
			}
			return metav1.ConditionUnknown
		}, timeout, interval).Should(Equal(metav1.ConditionFalse))
		Expect(apimeta.FindStatusCondition(px.Status.Conditions, condDegraded).Reason).To(Equal("ConfigHealthy"))
	})

	It("degrades with MissingTokenKey when the key is absent from the Secret", func() {
		ns := newNamespace()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns},
			StringData: map[string]string{"wrongKey": "x"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p3", ns))).To(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(degradedReason("p3", ns, &px), timeout, interval).Should(Equal("MissingTokenKey"))
	})

	It("degrades with ConfigTokenExpired for an expired token but still renders children", func() {
		ns := newNamespace()
		expired := unsignedToken(time.Now().Add(-time.Hour).Unix())
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, expired))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p4", ns))).To(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(degradedReason("p4", ns, &px), timeout, interval).Should(Equal("ConfigTokenExpired"))

		var deploy appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "p4", Namespace: ns}, &deploy)).
			To(Succeed(), "children must still be rendered for an expired token")
	})

	It("rolls the Deployment when the token Secret changes", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p5", ns))).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p5", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())
		before := deploy.Spec.Template.Annotations[proxybuilder.ChecksumAnnotation]

		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cfg", Namespace: ns}, &secret)).To(Succeed())
		secret.StringData = map[string]string{"configToken": unsignedToken(time.Now().Add(24 * time.Hour).Unix())}
		Expect(k8sClient.Update(ctx, &secret)).To(Succeed())

		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p5", Namespace: ns}, &deploy)
			return deploy.Spec.Template.Annotations[proxybuilder.ChecksumAnnotation]
		}, timeout, interval).ShouldNot(Equal(before))
	})

	It("creates and prunes the PDB when toggled", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p6", ns)
		px.Spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var pdb policyv1.PodDisruptionBudget
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p6", Namespace: ns}, &pdb)
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "p6", Namespace: ns}, px); err != nil {
				return err
			}
			px.Spec.PodDisruptionBudget.Enabled = false
			return k8sClient.Update(ctx, px)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "p6", Namespace: ns}, &pdb)
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})

	It("reports the ServiceMonitor capability gate when the CRD is not served", func() {
		// envtest installs only the operator's own CRDs, so the monitoring.coreos.com
		// ServiceMonitor CRD is absent here — exactly the gate's skip path.
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p7", ns)
		px.Spec.Metrics = &otilmv1alpha1.ProxyMetricsSpec{
			Enabled:        true,
			ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
		}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p7", Namespace: ns}, px)
			if c := apimeta.FindStatusCondition(px.Status.Conditions, condServiceMonitorReady); c != nil && c.Status == metav1.ConditionFalse {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal("ServiceMonitorCRDNotInstalled"))
	})

	It("transitions to Running when the Deployment reports readiness", func() {
		// envtest runs no kubelet/deployment controller, so the test drives the
		// Deployment status subresource directly to exercise the phase logic.
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p9", ns))).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p9", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "p9", Namespace: ns}, &deploy); err != nil {
				return err
			}
			deploy.Status.Replicas = 1
			deploy.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, &deploy)
		}, timeout, interval).Should(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(func() otilmv1alpha1.ProxyPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p9", Namespace: ns}, &px)
			return px.Status.Phase
		}, timeout, interval).Should(Equal(otilmv1alpha1.ProxyPhaseRunning))

		avail := apimeta.FindStatusCondition(px.Status.Conditions, condAvailable)
		Expect(avail).NotTo(BeNil())
		Expect(avail.Status).To(Equal(metav1.ConditionTrue))
		degraded := apimeta.FindStatusCondition(px.Status.Conditions, condDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
		Expect(px.Status.ReadyReplicas).To(Equal(int32(1)))
	})

	It("reports Failed with ReplicaFailure when the Deployment cannot make pods", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p10", ns))).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p10", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "p10", Namespace: ns}, &deploy); err != nil {
				return err
			}
			deploy.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentReplicaFailure,
				Status: corev1.ConditionTrue,
				Reason: "FailedCreate",
			}}
			return k8sClient.Status().Update(ctx, &deploy)
		}, timeout, interval).Should(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(degradedReason("p10", ns, &px), timeout, interval).Should(Equal("ReplicaFailure"))
		Expect(px.Status.Phase).To(Equal(otilmv1alpha1.ProxyPhaseFailed))
		avail := apimeta.FindStatusCondition(px.Status.Conditions, condAvailable)
		Expect(avail).NotTo(BeNil())
		Expect(avail.Status).To(Equal(metav1.ConditionFalse), "a Failed proxy must not report Available=True")
	})

	It("reports Updating while a rollout is partially ready", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p11", ns)
		px.Spec.Replicas = ptr.To(int32(2))
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p11", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "p11", Namespace: ns}, &deploy); err != nil {
				return err
			}
			deploy.Status.Replicas = 2
			deploy.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, &deploy)
		}, timeout, interval).Should(Succeed())

		var got otilmv1alpha1.Proxy
		Eventually(func() otilmv1alpha1.ProxyPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p11", Namespace: ns}, &got)
			return got.Status.Phase
		}, timeout, interval).Should(Equal(otilmv1alpha1.ProxyPhaseUpdating))
	})

	It("settles in ScaledDown when spec.replicas is 0", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p12", ns)
		px.Spec.Replicas = ptr.To(int32(0))
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var got otilmv1alpha1.Proxy
		Eventually(func() otilmv1alpha1.ProxyPhase {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p12", Namespace: ns}, &got)
			return got.Status.Phase
		}, timeout, interval).Should(Equal(otilmv1alpha1.ProxyPhaseScaledDown))

		prog := apimeta.FindStatusCondition(got.Status.Conditions, condProgressing)
		Expect(prog).NotTo(BeNil())
		Expect(prog.Status).To(Equal(metav1.ConditionFalse))
		Expect(prog.Reason).To(Equal("ScaledToZero"))
	})

	It("rolls the Deployment when a referenced ConfigMap changes", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: extraConfigMapName, Namespace: ns},
			Data:       map[string]string{"ca.crt": "v1"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		px := newProxyCR("p13", ns)
		mount := "/etc/proxy/extra"
		px.Spec.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
			{Name: extraConfigMapName, Type: otilmv1alpha1.RefTypeVolume, MountPath: &mount},
		}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p13", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())
		before := deploy.Spec.Template.Annotations[proxybuilder.ChecksumAnnotation]
		Expect(deploy.Spec.Template.Spec.Volumes).To(HaveLen(1))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: extraConfigMapName, Namespace: ns}, cm)).To(Succeed())
		cm.Data["ca.crt"] = "v2"
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())

		Eventually(func() string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p13", Namespace: ns}, &deploy)
			return deploy.Spec.Template.Annotations[proxybuilder.ChecksumAnnotation]
		}, timeout, interval).ShouldNot(Equal(before))
	})

	It("degrades with MissingConfigMap when a referenced ConfigMap is absent, and recovers", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p14", ns)
		px.Spec.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
			{Name: "absent-cm", Type: otilmv1alpha1.RefTypeEnv},
		}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var got otilmv1alpha1.Proxy
		Eventually(degradedReason("p14", ns, &got), timeout, interval).Should(Equal("MissingConfigMap"))

		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "absent-cm", Namespace: ns},
			Data:       map[string]string{"k": "v"},
		})).To(Succeed())
		Eventually(func() metav1.ConditionStatus {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p14", Namespace: ns}, &got)
			if c := apimeta.FindStatusCondition(got.Status.Conditions, condDegraded); c != nil {
				return c.Status
			}
			return metav1.ConditionUnknown
		}, timeout, interval).Should(Equal(metav1.ConditionFalse))
	})

	It("stamps spec.serviceAccount name and annotations onto the live ServiceAccount", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("p15", ns)
		saName := "egress-identity"
		px.Spec.ServiceAccount = &otilmv1alpha1.ServiceAccountSpec{
			Name:        &saName,
			Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/egress"},
		}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var sa corev1.ServiceAccount
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: ns}, &sa)
		}, timeout, interval).Should(Succeed())
		Expect(sa.Annotations).To(HaveKeyWithValue("eks.amazonaws.com/role-arn", "arn:aws:iam::1:role/egress"),
			"workload-identity annotations must reach the live ServiceAccount")

		var deploy appsv1.Deployment
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "p15", Namespace: ns}, &deploy)
		}, timeout, interval).Should(Succeed())
		Expect(deploy.Spec.Template.Spec.ServiceAccountName).To(Equal(saName))
	})

	It("removes the finalizer on deletion", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("p8", ns))).To(Succeed())

		var px otilmv1alpha1.Proxy
		Eventually(func() []string {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "p8", Namespace: ns}, &px)
			return px.Finalizers
		}, timeout, interval).Should(ContainElement(finalizerName))

		Expect(k8sClient.Delete(ctx, &px)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "p8", Namespace: ns}, &px)
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})
})
