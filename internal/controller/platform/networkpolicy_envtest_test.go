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

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

var _ = Describe("Platform NetworkPolicies", func() {
	// The default-deny NetworkPolicy set is DEFAULT ON (opt-out). These specs confirm the
	// end-to-end apply (owner-referenced, the right selectors) and the prune when disabled.
	const (
		npDenyIngress = "ilm-default-deny-ingress"
		npAllowEdge   = "ilm-allow-edge-to-gateway"
		npAllowEgress = "ilm-allow-egress"
	)

	Context("Default-on (opt-out)", func() {
		It("server-side-applies the ingress default-deny + edge-allow + egress policies, owner-referenced", func() {
			const ns = "ilm-netpol-on"
			p := lifecyclePlatform(ns, nil) // networkPolicy unset => default ON
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("applying the ingress default-deny selecting all platform pods, intra-namespace allow")
			Eventually(func(g Gomega) {
				var np networkingv1.NetworkPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npDenyIngress, Namespace: ns}, &np)).To(Succeed())
				g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/part-of", "ilm"))
				g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", "ilm"))
				g.Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
				g.Expect(np.Spec.Ingress).To(HaveLen(1))
				g.Expect(np.Spec.Ingress[0].From).To(HaveLen(1))
				// Intra-namespace allow: an empty podSelector, no namespaceSelector.
				g.Expect(np.Spec.Ingress[0].From[0].PodSelector).NotTo(BeNil())
				g.Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(BeEmpty())
				g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector).To(BeNil())
				// Owner-referenced by THIS Platform (so it is GC'd with the Platform and the
				// prune's owner-ref check authorizes deletes).
				g.Expect(np.OwnerReferences).NotTo(BeEmpty())
				g.Expect(np.OwnerReferences[0].Kind).To(Equal("Platform"))
				g.Expect(np.OwnerReferences[0].Name).To(Equal("ilm"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("applying the edge-allow policy selecting the api-gateway, from the ingress-nginx namespace")
			Eventually(func(g Gomega) {
				var np networkingv1.NetworkPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npAllowEdge, Namespace: ns}, &np)).To(Succeed())
				g.Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "api-gateway"))
				g.Expect(np.Spec.Ingress).To(HaveLen(1))
				g.Expect(np.Spec.Ingress[0].From).To(HaveLen(1))
				g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector).NotTo(BeNil())
				g.Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).
					To(HaveKeyWithValue("kubernetes.io/metadata.name", "ingress-nginx"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("applying the permissive egress policy")
			Eventually(func(g Gomega) {
				var np networkingv1.NetworkPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npAllowEgress, Namespace: ns}, &np)).To(Succeed())
				g.Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
				g.Expect(np.Spec.Egress).To(HaveLen(1))
				g.Expect(np.Spec.Egress[0].To).To(BeEmpty())
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	Context("Disabled renders none / prune reclaims", func() {
		It("renders no NetworkPolicies when networkPolicy.enabled=false", func() {
			const ns = "ilm-netpol-off"
			off := false
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.NetworkPolicy = &otilmv1alpha1.NetworkPolicySpec{Enabled: &off}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the platform to reconcile (the messaging ConfigMap is always applied)")
			Eventually(func(g Gomega) {
				var np networkingv1.NetworkPolicy
				err := k8sClient.Get(ctx, types.NamespacedName{Name: npDenyIngress, Namespace: ns}, &np)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("never rendering any of the policies")
			Consistently(func(g Gomega) {
				for _, name := range []string{npDenyIngress, npAllowEdge, npAllowEgress} {
					var np networkingv1.NetworkPolicy
					err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &np)
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), name)
				}
			}, "2s", platformInterval).Should(Succeed())
		})

		It("prunes the NetworkPolicies when networkPolicy is turned off after being on", func() {
			const ns = "ilm-netpol-prune"
			p := lifecyclePlatform(ns, nil) // default ON
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the ingress default-deny to be created")
			Eventually(func(g Gomega) {
				var np networkingv1.NetworkPolicy
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: npDenyIngress, Namespace: ns}, &np)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("disabling networkPolicy")
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				off := false
				cur.Spec.NetworkPolicy = &otilmv1alpha1.NetworkPolicySpec{Enabled: &off}
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("pruning all three now-de-rendered policies")
			Eventually(func(g Gomega) {
				for _, name := range []string{npDenyIngress, npAllowEdge, npAllowEgress} {
					var np networkingv1.NetworkPolicy
					err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &np)
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), name)
				}
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})
})
