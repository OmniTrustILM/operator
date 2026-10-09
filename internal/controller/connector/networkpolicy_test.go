/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	connbuilder "github.com/OmniTrustILM/operator/internal/builder/connector"
)

// waitForReconcile waits until the reconciler has finished a pass over the Connector's current
// generation.
func waitForReconcile(key types.NamespacedName) {
	EventuallyWithOffset(1, func(g Gomega) {
		var c otilmv1alpha1.Connector
		g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
		g.Expect(c.Status.ObservedGeneration).To(Equal(c.Generation))
	}, timeout, interval).Should(Succeed())
}

// userNetworkPolicy is a NetworkPolicy a user wrote for the connector named key.Name. Nothing
// controls it.
func userNetworkPolicy(key types.NamespacedName) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{connbuilder.ConnectorLabel: key.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

// setNetworkPolicy writes spec.networkPolicy on the Connector, retrying on conflicts.
func setNetworkPolicy(key types.NamespacedName, np *otilmv1alpha1.WorkloadNetworkPolicySpec) {
	EventuallyWithOffset(1, func(g Gomega) {
		var c otilmv1alpha1.Connector
		g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
		c.Spec.NetworkPolicy = np
		g.Expect(k8sClient.Update(ctx, &c)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

var _ = Describe("Connector NetworkPolicy", func() {
	It("renders the policy the builder describes, owned by the Connector", func() {
		ns := createTestNamespace("test-netpol-render")
		key := types.NamespacedName{Name: "netpol-render", Namespace: ns}
		Expect(k8sClient.Create(ctx, newConnector(key.Name, ns))).To(Succeed())

		Eventually(func(g Gomega) {
			var np networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			owner := metav1.GetControllerOf(&np)
			g.Expect(owner).NotTo(BeNil())
			g.Expect(owner.Kind).To(Equal("Connector"))
			g.Expect(owner.Name).To(Equal(key.Name))

			var c otilmv1alpha1.Connector
			g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
			// The apiserver defaults nothing the builder leaves unset, so the live policy equals
			// the rendered one and the reconciler never rewrites it.
			g.Expect(np.Spec).To(Equal(connbuilder.BuildNetworkPolicy(&c, testOperatorNamespace).Spec))
			g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
			g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector).NotTo(BeNil())
			g.Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).
				To(HaveKeyWithValue("kubernetes.io/metadata.name", testOperatorNamespace))
		}, timeout, interval).Should(Succeed())
	})

	It("follows a change of the Service port", func() {
		ns := createTestNamespace("test-netpol-port")
		Expect(k8sClient.Create(ctx, newConnector("netpol-port", ns))).To(Succeed())
		key := types.NamespacedName{Name: "netpol-port", Namespace: ns}
		waitForReconcile(key)

		Eventually(func(g Gomega) {
			var c otilmv1alpha1.Connector
			g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
			c.Spec.Service.Port = 9443
			g.Expect(k8sClient.Update(ctx, &c)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var np networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(1))
			g.Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(9443))
		}, timeout, interval).Should(Succeed())
	})

	It("restores a policy edited by hand", func() {
		ns := createTestNamespace("test-netpol-drift")
		Expect(k8sClient.Create(ctx, newConnector("netpol-drift", ns))).To(Succeed())
		key := types.NamespacedName{Name: "netpol-drift", Namespace: ns}

		By("dropping the operator from the admitted sources")
		Eventually(func(g Gomega) {
			var np networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
			np.Spec.Ingress[0].From = np.Spec.Ingress[0].From[:1]
			g.Expect(k8sClient.Update(ctx, &np)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var np networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			g.Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
		}, timeout, interval).Should(Succeed())
	})

	It("renders the policy only while enabled", func() {
		ns := createTestNamespace("test-netpol-toggle")
		conn := newConnector("netpol-toggle", ns)
		conn.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		key := types.NamespacedName{Name: "netpol-toggle", Namespace: ns}
		waitForReconcile(key)

		var np networkingv1.NetworkPolicy
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &np))).To(BeTrue(), "a disabled Connector renders no policy")

		By("enabling it")
		setNetworkPolicy(key, &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(true)})
		Eventually(func() error { return k8sClient.Get(ctx, key, &np) }, timeout, interval).Should(Succeed())

		By("disabling it again")
		setNetworkPolicy(key, &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)})
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &np))
		}, timeout, interval).Should(BeTrue())
	})

	It("leaves a same-named policy it does not control untouched while enabled, and reports it", func() {
		ns := createTestNamespace("test-netpol-foreign-on")
		key := types.NamespacedName{Name: "netpol-foreign-on", Namespace: ns}
		user := userNetworkPolicy(key)
		Expect(k8sClient.Create(ctx, user)).To(Succeed())

		Expect(k8sClient.Create(ctx, newConnector(key.Name, ns))).To(Succeed())
		waitForReconcile(key)

		var np networkingv1.NetworkPolicy
		Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
		Expect(np.OwnerReferences).To(BeEmpty(), "the operator must not adopt the user's policy")
		Expect(np.Spec).To(Equal(user.Spec), "the operator must not rewrite the user's policy")

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(ctx, &events, client.InNamespace(ns))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(SatisfyAll(
				HaveField("Reason", "NetworkPolicyNotOwned"),
				HaveField("Type", corev1.EventTypeWarning),
				HaveField("InvolvedObject.Name", key.Name),
			)))
		}, timeout, interval).Should(Succeed())
	})

	It("leaves a same-named policy it does not control in place while disabled", func() {
		ns := createTestNamespace("test-netpol-foreign")
		key := types.NamespacedName{Name: "netpol-foreign", Namespace: ns}
		Expect(k8sClient.Create(ctx, userNetworkPolicy(key))).To(Succeed())

		conn := newConnector(key.Name, ns)
		conn.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		waitForReconcile(key)

		var np networkingv1.NetworkPolicy
		Expect(k8sClient.Get(ctx, key, &np)).To(Succeed(), "the user's policy must survive the reconcile")
		Expect(np.OwnerReferences).To(BeEmpty())
	})

	It("stores an empty networkPolicy block as enabled", func() {
		ns := createTestNamespace("test-netpol-default")
		conn := newConnector("netpol-default", ns)
		conn.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())

		var stored otilmv1alpha1.Connector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: conn.Name, Namespace: ns}, &stored)).To(Succeed())
		Expect(stored.Spec.NetworkPolicy.Enabled).To(HaveValue(BeTrue()))
	})
})
