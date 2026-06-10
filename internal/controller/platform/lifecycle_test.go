/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package platform

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// lifecyclePlatform builds a credentialled Platform in its own namespace, applying an
// optional mutator (e.g. to set deletionPolicy).
func lifecyclePlatform(ns string, mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	for _, n := range []string{dbSecretRef, messagingSecretRef} {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
		})).To(Succeed())
	}
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: dbHost, Port: 5432, Name: dbName, Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
			},
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", BrokerType: "rabbitmq", Host: brokerHost, Port: 5672,
				VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
			},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

var _ = Describe("Platform measured readiness", func() {
	// ---------------------------------------------------------------
	// Available/Progressing are MEASURED from Core/auth Deployment
	// readiness, not asserted. envtest has no kubelet, so the specs manipulate the
	// Deployments' status directly to simulate not-ready / ready transitions.
	// ---------------------------------------------------------------
	Context("MeasuredReadiness", func() {
		It("reports Progressing/Available=False while Core has 0 ready replicas, then Running once ready", func() {
			const ns = "ilm-readiness"
			p := lifecyclePlatform(ns, nil)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the Core Deployment to be applied")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("simulating Core with 0 ready replicas (and auth not ready)")
			markDeploymentNotReady(ns, "core")
			markDeploymentNotReady(ns, "auth")

			By("verifying the platform reports Progressing / Available=False / Progressing=True")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseProgressing))
				g.Expect(meta.IsStatusConditionFalse(got.Status.Conditions, "Available")).To(BeTrue(),
					"Available must be False while a required Deployment is not ready")
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Progressing")).To(BeTrue(),
					"Progressing must be True while a required Deployment is rolling out")
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Degraded")).To(BeFalse(),
					"a not-yet-ready required Deployment must NOT be Degraded")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("now simulating both required Deployments becoming ready")
			markRequiredDeploymentsReady(ns)

			By("verifying the platform transitions to Running / Available=True / Progressing=False")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Available")).To(BeTrue())
				g.Expect(meta.IsStatusConditionFalse(got.Status.Conditions, "Progressing")).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("stays Progressing when Core is ready but the auth provider (auth) is not", func() {
			const ns = "ilm-readiness-auth"
			p := lifecyclePlatform(ns, nil)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the auth Deployment to be applied")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "auth", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("marking Core ready but auth NOT ready (Available is gated on the auth provider)")
			markDeploymentReadyByName(ns, "core")
			markDeploymentNotReady(ns, "auth")

			By("verifying the platform stays Progressing (Available is gated on auth)")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseProgressing))
				g.Expect(meta.IsStatusConditionFalse(got.Status.Conditions, "Available")).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})
})

var _ = Describe("Platform deletion safety", func() {
	// ---------------------------------------------------------------
	// Finalizer + spec.deletionPolicy: the reconciler adds the finalizer before any
	// work; deletion runs the handler, removes the finalizer, and the object is gone.
	// deletionPolicy defaults to Retain.
	//
	// These envtest specs use an EXTERNAL DB/broker (lifecyclePlatform sets Mode
	// "external"), so there is no managed infrastructure to tear down here — they assert
	// object lifecycle only (finalizer add, then removed under both Delete and Retain).
	// The Retain-vs-Delete behavioral DIFFERENCE for managed infrastructure (Retain leaves
	// the managed CNPG/RabbitMQ/Keycloak CRs intact; Delete reclaims them) is asserted by
	// the fake-client unit tests in managed_database_controller_test.go /
	// managed_messaging_controller_test.go / managed_keycloak_controller_test.go
	// (HandleDeletionManagedRetainLeavesCluster etc., and TestPruneNeverTouchesManagedDatabase).
	// Do not assume this file proves the managed-infra retain guarantee.
	// ---------------------------------------------------------------
	Context("Finalizer", func() {
		It("adds the finalizer on create and defaults deletionPolicy to Retain", func() {
			const ns = "ilm-finalizer-add"
			p := lifecyclePlatform(ns, nil)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying the operator finalizer is added")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(&got, platformFinalizer)).To(BeTrue(),
					"the Platform must carry the operator finalizer")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying deletionPolicy defaulted to Retain (apiserver default)")
			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
			Expect(got.Spec.DeletionPolicy).To(Equal(otilmv1alpha1.PlatformDeletionPolicyRetain))
		})

		It("runs the deletion handler, removes the finalizer, and the object is gone (deletionPolicy=Delete)", func() {
			const ns = "ilm-finalizer-del"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyDelete
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			key := types.NamespacedName{Name: "ilm", Namespace: ns}
			By("waiting for the finalizer to be added (so deletion will run the handler)")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(&got, platformFinalizer)).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("deleting the Platform")
			var toDelete otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, key, &toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

			By("verifying the finalizer is removed and the object is fully gone")
			Eventually(func() bool {
				var got otilmv1alpha1.Platform
				err := k8sClient.Get(ctx, key, &got)
				return apierrors.IsNotFound(err)
			}, platformTimeout, platformInterval).Should(BeTrue(),
				"the deletion handler must remove the finalizer so the object is reclaimed")
		})

		It("does not block deletion under the default Retain policy either", func() {
			const ns = "ilm-finalizer-retain"
			p := lifecyclePlatform(ns, nil) // deletionPolicy defaults to Retain
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			key := types.NamespacedName{Name: "ilm", Namespace: ns}
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(&got, platformFinalizer)).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			var toDelete otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, key, &toDelete)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

			Eventually(func() bool {
				var got otilmv1alpha1.Platform
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &got))
			}, platformTimeout, platformInterval).Should(BeTrue(),
				"Retain must not block deletion of the operator's own children (owner-ref GC)")
		})
	})
})

// markDeploymentReadyByName marks a single named Deployment's status ready (desired ==
// available + Available=True), for specs that need to ready only one component.
func markDeploymentReadyByName(ns, name string) {
	key := types.NamespacedName{Name: name, Namespace: ns}
	Eventually(func() error {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &dep); err != nil {
			return err
		}
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		dep.Status.Replicas = desired
		dep.Status.ReadyReplicas = desired
		dep.Status.AvailableReplicas = desired
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
			Reason: "MinimumReplicasAvailable", LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now(),
		}}
		return k8sClient.Status().Update(ctx, &dep)
	}, platformTimeout, platformInterval).Should(Succeed())
}
