/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

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
	proxybuilder "github.com/OmniTrustILM/operator/internal/builder/proxy"
)

// waitForProxyReconcile waits until the reconciler has finished a pass over the Proxy's current
// generation.
func waitForProxyReconcile(key types.NamespacedName) {
	EventuallyWithOffset(1, func(g Gomega) {
		var px otilmv1alpha1.Proxy
		g.Expect(k8sClient.Get(ctx, key, &px)).To(Succeed())
		g.Expect(px.Status.ObservedGeneration).To(Equal(px.Generation))
	}, timeout, interval).Should(Succeed())
}

// setProxyNetworkPolicy writes spec.networkPolicy.enabled on the Proxy, retrying on conflicts.
func setProxyNetworkPolicy(key types.NamespacedName, enabled bool) {
	EventuallyWithOffset(1, func(g Gomega) {
		var px otilmv1alpha1.Proxy
		g.Expect(k8sClient.Get(ctx, key, &px)).To(Succeed())
		px.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(enabled)}
		g.Expect(k8sClient.Update(ctx, &px)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

var _ = Describe("Proxy NetworkPolicy", func() {
	It("renders the policy the builder describes, owned by the Proxy", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR("np-render", ns))).To(Succeed())
		key := types.NamespacedName{Name: "np-render", Namespace: ns}

		Eventually(func(g Gomega) {
			var np networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			owner := metav1.GetControllerOf(&np)
			g.Expect(owner).NotTo(BeNil())
			g.Expect(owner.Kind).To(Equal("Proxy"))

			var px otilmv1alpha1.Proxy
			g.Expect(k8sClient.Get(ctx, key, &px)).To(Succeed())
			// The live policy equals the rendered one, so the reconciler never rewrites it.
			g.Expect(np.Spec).To(Equal(proxybuilder.BuildNetworkPolicy(&px, testOperatorNamespace).Spec))
			g.Expect(np.Spec.Ingress[0].Ports).To(HaveLen(2))
		}, timeout, interval).Should(Succeed())
	})

	It("renders the policy only while enabled", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		px := newProxyCR("np-toggle", ns)
		px.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())
		key := types.NamespacedName{Name: "np-toggle", Namespace: ns}

		By("waiting for a pass over the disabled Proxy")
		waitForProxyReconcile(key)
		var np networkingv1.NetworkPolicy
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &np))).To(BeTrue(), "a disabled Proxy renders no policy")

		By("enabling it")
		setProxyNetworkPolicy(key, true)
		Eventually(func() error { return k8sClient.Get(ctx, key, &np) }, timeout, interval).Should(Succeed())

		By("disabling it again")
		setProxyNetworkPolicy(key, false)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &np))
		}, timeout, interval).Should(BeTrue())
	})

	It("never adopts, rewrites or deletes a same-named policy it does not control", func() {
		ns := newNamespace()
		Expect(k8sClient.Create(ctx, newTokenSecret(ns, unsignedToken(0)))).To(Succeed())
		key := types.NamespacedName{Name: "np-foreign", Namespace: ns}
		user := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{proxybuilder.ProxyLabel: key.Name}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		}
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxyCR(key.Name, ns))).To(Succeed())
		waitForProxyReconcile(key)

		By("leaving it untouched while enabled, and reporting it")
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

		By("leaving it in place once disabled")
		setProxyNetworkPolicy(key, false)
		waitForProxyReconcile(key)
		Expect(k8sClient.Get(ctx, key, &np)).To(Succeed(), "the user's policy must survive the reconcile")
		Expect(np.OwnerReferences).To(BeEmpty())
	})

	It("stores an empty networkPolicy block as enabled", func() {
		ns := newNamespace()
		px := newProxyCR("np-default", ns)
		px.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{}
		Expect(k8sClient.Create(ctx, px)).To(Succeed())

		var stored otilmv1alpha1.Proxy
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: px.Name, Namespace: ns}, &stored)).To(Succeed())
		Expect(stored.Spec.NetworkPolicy.Enabled).To(HaveValue(BeTrue()))
	})
})
