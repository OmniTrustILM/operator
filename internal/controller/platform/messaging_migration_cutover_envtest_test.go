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

// The unit tests prove what the staged cutover DECIDES. These two prove what it does to a
// running platform — which only a live reconcile can show, because the thing under test is
// which objects the pass applies and which it deliberately leaves alone.
//
// Two facts about envtest shape the fixtures, and both are used rather than worked around:
//
//   - it serves no rabbitmq.com CRDs, so a managed-messaging platform's target topology can
//     never report Ready. That is exactly stage 1's blocking case, so the first spec drives it
//     directly. The second spec needs stage 1 SATISFIED, so its platform has an external broker
//     (the operator renders no topology for one, and there is therefore nothing to wait for) —
//     the staged ordering after stage 1 is the same either way, and stage 1's own gate is
//     covered by the first spec and by the fake-client tests.
//   - it runs no workload controller, so nothing ever reports a rollout as finished. The second
//     spec plays that part itself (markRolledOut), which is the same thing the readiness specs
//     do for the kubelet.

import (
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
)

// feAdministratorWorkloadName is the front-end component's workload name. It is the cleanest
// witness that a pass rendered the target bundle: it is not a message producer (so the fence
// never touches it) and its image is tagged with the platform version itself.
const feAdministratorWorkloadName = "fe-administrator"

// cutoverPasses bounds the reconciles a spec drives the staged cutover through by hand. Each
// stage needs a pass or two, so this is generous — it exists only so a cutover that never
// advances fails the spec instead of looping forever.
const cutoverPasses = 12

// deployedProvisioning switches a fixture Platform onto the operator-managed provisioning
// service — the component whose fenced zero replicas is what makes the cutover's ordering
// load-bearing, since Core's proxy-path init container cannot complete without it.
func deployedProvisioning(p *otilmv1alpha1.Platform) {
	p.Spec.Common.Proxy.Enabled = true
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		Mode:   "deploy",
		Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: provisioningBootstrapSecret},
	}
}

// markRolledOut plays the workload controller envtest does not run: it reports the named
// Deployment as fully rolled out onto whatever pod template it currently carries. It re-reads
// first, so a template the operator has just re-applied is followed rather than raced.
//
//nolint:unparam // ns is intentionally a parameter, as in every other namespaced spec helper
func markRolledOut(ns, name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &dep)).To(Succeed())
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas = desired
		dep.Status.UpdatedReplicas = desired
		dep.Status.ReadyReplicas = desired
		dep.Status.AvailableReplicas = desired
		g.Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())
	}, platformTimeout, platformInterval).Should(Succeed())
}

// cutoverFence is the fence the drain hands the cutover: all three producers stopped, at the
// counts restore has to write back.
func cutoverFence() []otilmv1alpha1.FencedWorkload {
	return []otilmv1alpha1.FencedWorkload{
		{Name: gatewayWorkloadName, Kind: "Deployment", Replicas: 1},
		{Name: schedulerWorkloadName, Kind: "Deployment", Replicas: 1},
		{Name: provisioningWorkloadName, Kind: "Deployment", Replicas: 1},
	}
}

var _ = Describe("Messaging migration cutover", func() {
	Context("A cutover whose target topology is not declared", func() {
		It("renders the target bundle but withholds Core, and releases no producer", func() {
			const ns = "ilm-migration-cutover-topology"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseCuttingOver,
				func(p *otilmv1alpha1.Platform) {
					managedMessagingPlatform(p)
					deployedProvisioning(p)
				}, cutoverFence()...)

			By("rendering the TARGET bundle — the source re-pin the waiting phases apply is over")
			// Fencing and Draining put spec.version back to the source in memory so nothing of
			// the target could be applied. The cutover is the step that ends that. fe-administrator
			// is where it shows: it is not a message producer, so it is not fenced, and its image
			// is tagged with the platform version itself.
			Eventually(func() string {
				return workloadImages(ns, feAdministratorWorkloadName)
			}, platformTimeout, platformInterval).Should(And(
				ContainSubstring(":"+platformVersion219),
				Not(ContainSubstring(":"+platformVersion218)),
			))

			By("withholding Core while the target topology is not declared")
			// envtest serves no rabbitmq.com CRDs, so not one target topology object can report
			// Ready — stage 1's blocking case. Core must stay on the pod template it is serving.
			Consistently(func() string {
				return workloadImages(ns, "core")
			}, "3s", platformInterval).Should(And(
				ContainSubstring(":"+platformVersion218),
				Not(ContainSubstring(":"+platformVersion219)),
			), "Core may not be rolled onto a topology the broker has not declared")

			By("releasing no producer and handing over to nothing")
			Consistently(func(g Gomega) {
				u := getPlatform(ns).Status.Upgrade
				g.Expect(u).NotTo(BeNil())
				g.Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseCuttingOver))
				g.Expect(u.Fenced).To(ConsistOf(cutoverFence()))
			}, "2s", platformInterval).Should(Succeed())
			Expect(workloadSpecReplicas(ns, "Deployment", gatewayWorkloadName)).To(BeZero())
			Expect(workloadSpecReplicas(ns, "Deployment", provisioningWorkloadName)).To(BeZero())

			By("reporting the stage it is waiting on, without leaking a coordinate")
			cond := migrationConditionOf(ns)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(string(otilmv1alpha1.MigrationPhaseCuttingOver)))
			Expect(cond.Message).To(ContainSubstring(cutoverStageTopology.String()))
			Expect(cond.Message).NotTo(ContainSubstring(sourceVirtualHost))

			By("re-entering the same stage from the persisted state alone")
			for i := 0; i < 3; i++ {
				reconcileOnce(ns)
			}
			u := getPlatform(ns).Status.Upgrade
			Expect(u).NotTo(BeNil())
			Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseCuttingOver))
			Expect(u.Fenced).To(ConsistOf(cutoverFence()), "a repeated stage releases nothing twice")
			Expect(workloadImages(ns, "core")).To(ContainSubstring(":" + platformVersion218))
		})
	})

	Context("A cutover whose stages are satisfied one at a time", func() {
		It("restores provisioning before Core, then reopens the gateway and hands over", func() {
			const ns = "ilm-migration-cutover-stages"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseCuttingOver, deployedProvisioning, cutoverFence()...)

			By("withholding Core while the provisioning service is still fenced")
			// THE DEADLOCK. Core's provision-instance-queue init container retries against the
			// provisioning API until it answers, and the fence holds that service at zero. A
			// cutover that rolled Core here would wait for a Ready that can never arrive, past
			// the point of no return and with no deadline left to rescue it.
			Consistently(func() string {
				return workloadImages(ns, "core")
			}, "3s", platformInterval).Should(And(
				ContainSubstring(":"+platformVersion218),
				Not(ContainSubstring(":"+platformVersion219)),
			), "Core may not roll while the service its init container blocks on is fenced")

			By("releasing every workload Core's init containers block on, and nothing else")
			// The invariant, checked on every pass rather than at the end: Core may carry the
			// target template only once NO workload its init containers wait on is still fenced.
			// A fenced dependency sits at zero replicas, so Core's pod would block in its init
			// containers forever — and this phase has no deadline left to rescue it.
			var rolled bool
			for pass := 0; pass < cutoverPasses && !rolled; pass++ {
				markRolledOut(ns, provisioningWorkloadName)
				markRolledOut(ns, schedulerWorkloadName)
				reconcileOnce(ns)

				p := getPlatform(ns)
				rolled = strings.Contains(workloadImages(ns, coreDeploymentName), ":"+platformVersion219)
				for _, dependency := range platformbuilder.CoreInitServiceDependencies(p) {
					if _, fenced := fencedWorkloadFor(p, dependency); fenced {
						Expect(rolled).To(BeFalse(),
							"Core was rolled while %q, which its init containers block on, is fenced at zero replicas", dependency)
					}
				}
			}
			Expect(rolled).To(BeTrue(), "the cutover reached the Core roll")

			Eventually(func() []otilmv1alpha1.FencedWorkload {
				return getPlatform(ns).Status.Upgrade.Fenced
			}, platformTimeout, platformInterval).Should(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: "Deployment", Replicas: 1},
			), "only the door is still shut")
			Expect(workloadSpecReplicas(ns, "Deployment", provisioningWorkloadName)).To(Equal(int32(1)))
			Expect(workloadSpecReplicas(ns, "Deployment", schedulerWorkloadName)).To(Equal(int32(1)),
				"the scheduler is back up on the TARGET template, publishing only to the target topology")
			Expect(workloadSpecReplicas(ns, "Deployment", gatewayWorkloadName)).To(BeZero(),
				"the door stays shut until the platform is actually serving from the target")

			By("holding the hand-over until Core is Ready ON THE TARGET ROLLOUT")
			// Core has been Ready throughout — on the source pod template. Plain readiness would
			// have handed over long ago; the rollout check is what makes it wait.
			Consistently(func(g Gomega) {
				markRolledOut(ns, provisioningWorkloadName)
				markRolledOut(ns, schedulerWorkloadName)
				reconcileOnce(ns)
				g.Expect(getPlatform(ns).Status.Upgrade.Phase).To(Equal(otilmv1alpha1.MigrationPhaseCuttingOver))
			}, "2s", platformInterval).Should(Succeed())

			By("reopening the gateway, handing over, and finishing the migration")
			// The hand-over reopens the gateway and leaves the fence empty. This platform's
			// broker is EXTERNAL, so the cleanup it hands to has no topology of its own to
			// reclaim and finishes on the very next pass.
			Eventually(func(g Gomega) {
				markRolledOut(ns, provisioningWorkloadName)
				markRolledOut(ns, schedulerWorkloadName)
				markRolledOut(ns, coreDeploymentName)
				reconcileOnce(ns)
				p := getPlatform(ns)
				g.Expect(p.Status.Upgrade).To(BeNil())
				g.Expect(p.Status.ObservedVersion).To(Equal(platformVersion219))
			}, platformTimeout, platformInterval).Should(Succeed())

			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", gatewayWorkloadName)
			}, platformTimeout, platformInterval).Should(Equal(int32(1)), "the door is reopened at its recorded count")
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", schedulerWorkloadName)
			}, platformTimeout, platformInterval).Should(Equal(int32(1)),
				"the scheduler stays up on the target template, as it has been since stage 2")

			By("re-entering afterwards without undoing any of it")
			for i := 0; i < 3; i++ {
				reconcileOnce(ns)
			}
			Expect(getPlatform(ns).Status.Upgrade).To(BeNil())
			Expect(getPlatform(ns).Status.ObservedVersion).To(Equal(platformVersion219))
			Expect(workloadSpecReplicas(ns, "Deployment", gatewayWorkloadName)).To(Equal(int32(1)))
		})
	})
})
