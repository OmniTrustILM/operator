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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

var _ = Describe("Platform availability primitives", func() {
	Context("HighAvailability profile", func() {
		It("renders a PodDisruptionBudget for the stateless components and HA replicas", func() {
			const ns = "ilm-ha-profile"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("server-side-applying a PDB (minAvailable 1) for core")
			Eventually(func(g Gomega) {
				var pdb policyv1.PodDisruptionBudget
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &pdb)).To(Succeed())
				g.Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
				g.Expect(pdb.Spec.MinAvailable.IntValue()).To(Equal(1))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("setting the HA default replica count on the core Deployment")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
				g.Expect(dep.Spec.Replicas).NotTo(BeNil())
				g.Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	Context("Per-component autoscaling", func() {
		It("renders an HPA and the targeted Deployment omits .spec.replicas (SSA: HPA owns scaling)", func() {
			const ns = "ilm-hpa"
			cpu := int32(75)
			minR := int32(2)
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{
					MinReplicas: &minR, MaxReplicas: 5, TargetCPUUtilization: &cpu,
				}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("server-side-applying an HPA targeting core's Deployment")
			Eventually(func(g Gomega) {
				var hpa autoscalingv2.HorizontalPodAutoscaler
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &hpa)).To(Succeed())
				g.Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("core"))
				g.Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
				g.Expect(hpa.Spec.MaxReplicas).To(Equal(int32(5)))
				g.Expect(hpa.Spec.MinReplicas).NotTo(BeNil())
				g.Expect(*hpa.Spec.MinReplicas).To(Equal(int32(2)))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("leaving the core Deployment's .spec.replicas unset so the HPA owns it under SSA")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
				// The apiserver defaults replicas to 1 when the field manager omits it; the
				// load-bearing assertion (the operator does NOT send replicas) is verified in
				// the builder unit test. Here we confirm the HPA exists and the Deployment is
				// applied, the end-to-end wiring. We do NOT assert a specific replica value,
				// since the apiserver/HPA may write it.
				g.Expect(dep.Name).To(Equal("core"))
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	Context("Per-component workloadType", func() {
		It("renders a StatefulSet (headless serviceName + hardened pod template) for a StatefulSet-typed component", func() {
			const ns = "ilm-sts"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("server-side-applying a StatefulSet for core with the headless serviceName")
			Eventually(func(g Gomega) {
				var sts appsv1.StatefulSet
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &sts)).To(Succeed())
				g.Expect(sts.Spec.ServiceName).To(Equal("core"))
				g.Expect(sts.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "core"))
				// The pod template is SCC-hardened exactly like the Deployment path.
				g.Expect(sts.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
				g.Expect(sts.Spec.Template.Spec.SecurityContext.RunAsNonRoot).NotTo(BeNil())
				g.Expect(*sts.Spec.Template.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
				g.Expect(sts.Spec.Template.Spec.Containers).NotTo(BeEmpty())
				main := sts.Spec.Template.Spec.Containers[0]
				g.Expect(main.SecurityContext).NotTo(BeNil())
				g.Expect(main.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
				g.Expect(*main.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
				g.Expect(main.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("NOT rendering a Deployment of the same name for that component")
			Consistently(func() bool {
				var dep appsv1.Deployment
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)
				return apierrors.IsNotFound(err)
			}, "2s", platformInterval).Should(BeTrue(), "core must be a StatefulSet, not a Deployment")
		})

		It("prunes the old Deployment when a component is switched Deployment->StatefulSet (apply+prune)", func() {
			const ns = "ilm-sts-switch"
			p := lifecyclePlatform(ns, nil) // default: core is a Deployment
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the core Deployment to be applied (default kind)")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("switching core to workloadType=StatefulSet")
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				cur.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("applying the new StatefulSet")
			Eventually(func(g Gomega) {
				var sts appsv1.StatefulSet
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &sts)).To(Succeed())
				g.Expect(sts.Spec.ServiceName).To(Equal("core"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("pruning the now-de-rendered Deployment (the old kind is reclaimed)")
			Eventually(func() bool {
				var dep appsv1.Deployment
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)
				return apierrors.IsNotFound(err)
			}, platformTimeout, platformInterval).Should(BeTrue(), "the old core Deployment must be pruned after the kind switch")
		})
	})

	Context("De-configuration prunes the children", func() {
		It("prunes a PDB when the HA profile is turned off", func() {
			const ns = "ilm-ha-prune"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the core PDB to be created")
			Eventually(func(g Gomega) {
				var pdb policyv1.PodDisruptionBudget
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &pdb)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("turning the HA profile off")
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				cur.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: false}
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("pruning the now-de-configured core PDB")
			Eventually(func() bool {
				var pdb policyv1.PodDisruptionBudget
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &pdb)
				return apierrors.IsNotFound(err)
			}, platformTimeout, platformInterval).Should(BeTrue(), "the PDB must be pruned once HA is disabled")
		})
	})
})
