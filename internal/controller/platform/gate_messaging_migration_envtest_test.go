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

// Two claims about the migration gate can only be checked against a running reconciler:
// that a migration in a waiting phase goes on rendering the SOURCE version (the re-pin
// really beats the pin resolvePlatformVersion put on the target), and that an operator which
// dies mid-migration picks up exactly where the persisted status says it was. Both are
// properties of the WHOLE Reconcile, so both are driven through it here.
//
// The suite's manager reconciles the same Platform in the background. That is deliberate: it
// means every assertion below has to hold no matter how many passes ran or which actor ran
// them, which is the same thing a real cluster demands.

import (
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// awaitObservedVersion waits until the platform reports the version it has settled on.
func awaitObservedVersion(ns, version string) {
	EventuallyWithOffset(1, func() string {
		return getPlatform(ns).Status.ObservedVersion
	}, platformTimeout, platformInterval).Should(Equal(version))
}

// requestPlatformVersion sets spec.version, retrying past the benign optimistic-lock races the
// background reconciler creates.
func requestPlatformVersion(ns, version string) {
	EventuallyWithOffset(1, func(g Gomega) {
		var p otilmv1alpha1.Platform
		g.Expect(k8sClient.Get(ctx, platformKey(ns), &p)).To(Succeed())
		p.Spec.Version = version
		g.Expect(k8sClient.Update(ctx, &p)).To(Succeed())
	}, platformTimeout, platformInterval).Should(Succeed())
}

// recordMigration writes a migration onto status.upgrade at the given phase and fenced set —
// the state a restarted operator would read back, written here directly so a spec can start
// from any point in the machine.
func recordMigration(ns string, phase otilmv1alpha1.MigrationPhase, fenced ...otilmv1alpha1.FencedWorkload) {
	EventuallyWithOffset(1, func(g Gomega) {
		var p otilmv1alpha1.Platform
		g.Expect(k8sClient.Get(ctx, platformKey(ns), &p)).To(Succeed())
		now := metav1.Now()
		p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
			FromVersion: platformVersion218, ToVersion: platformVersion219, Phase: phase,
			Fenced: fenced, StartedAt: now, PhaseStartedAt: now,
		}
		g.Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
	}, platformTimeout, platformInterval).Should(Succeed())
}

// workloadImages joins every image a Deployment's pod template pulls, so a spec can assert
// WHICH version's bundle produced it — the bundle's component tags are the visible difference
// between the source and target renders.
func workloadImages(ns, name string) string {
	var dep appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &dep); err != nil {
		return ""
	}
	var images []string
	for _, c := range dep.Spec.Template.Spec.InitContainers {
		images = append(images, c.Image)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		images = append(images, c.Image)
	}
	return strings.Join(images, " ")
}

// migrationConditionOf returns the platform's MessagingMigration condition, or nil.
func migrationConditionOf(ns string) *metav1.Condition {
	return meta.FindStatusCondition(getPlatform(ns).Status.Conditions, conditionMessagingMigration)
}

// reconcileOnce drives one full pass on a reconciler that holds no state from any previous
// pass — the operator restarting, in other words. It retries past the benign optimistic-lock
// races the background manager creates.
func reconcileOnce(ns string) {
	r := fenceReconciler()
	EventuallyWithOffset(1, func() error {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: platformKey(ns)})
		return err
	}, platformTimeout, platformInterval).Should(Succeed())
}

// beginMigrationFixture brings a platform up on 2.18.0, points spec.version at 2.19.0 and
// records a migration at the given phase.
//
// The two writes don't need to be atomic: resolving 2.19.0 (an unreleased/preview bundle)
// never depended on a migration already being recorded — Bundle.Released only gates
// advertising, not whether an explicit spec.version resolves. Between the two writes the
// background reconciler may already start its own migration (beginMigration) from
// spec.version alone; recordMigration's retry-until-succeeds Eventually simply pins
// status.Upgrade to the exact phase the spec wants to start from.
func beginMigrationFixture(ns string, phase otilmv1alpha1.MigrationPhase, mutate func(*otilmv1alpha1.Platform), fenced ...otilmv1alpha1.FencedWorkload) {
	p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
		p.Spec.Version = platformVersion218
		if mutate != nil {
			mutate(p)
		}
	})
	ExpectWithOffset(1, k8sClient.Create(ctx, p)).To(Succeed())

	By("waiting for the platform to settle on its source version")
	awaitObservedVersion(ns, platformVersion218)
	var gw appsv1.Deployment
	awaitWorkload(ns, "api-gateway", &gw)
	var sched appsv1.Deployment
	awaitWorkload(ns, "scheduler", &sched)

	By("requesting the target version and recording the migration")
	requestPlatformVersion(ns, platformVersion219)
	recordMigration(ns, phase, fenced...)
}

var _ = Describe("Messaging migration state", func() {
	Context("A migration in a waiting phase holds the platform on its source version", func() {
		It("fences the producers, advances to the drain, and never renders the target bundle", func() {
			const ns = "ilm-migration-hold"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseFencing, func(p *otilmv1alpha1.Platform) {
				p.Spec.Scheduler.Replicas = ptr(int32(3))
			})

			By("recording the complete fence and handing over to the drain")
			Eventually(func(g Gomega) {
				u := getPlatform(ns).Status.Upgrade
				g.Expect(u).NotTo(BeNil())
				g.Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseDraining),
					"with no producer pods left running, fencing hands over")
				g.Expect(u.Fenced).To(ConsistOf(
					otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 1},
					otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3},
				), "each producer's kind and prior count are recorded before anything is patched")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("holding the producers at zero")
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(BeZero())
			Consistently(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "api-gateway")
			}, "2s", platformInterval).Should(BeZero())

			By("reporting the phase on the migration condition")
			Eventually(func() string {
				c := migrationConditionOf(ns)
				if c == nil {
					return ""
				}
				return c.Reason
			}, platformTimeout, platformInterval).Should(Equal(string(otilmv1alpha1.MigrationPhaseDraining)))
			Expect(migrationConditionOf(ns).Status).To(Equal(metav1.ConditionTrue))

			By("rendering the SOURCE bundle even though spec.version names the target")
			// The re-pin is the only thing standing between the recorded migration and a
			// target-version render: resolvePlatformVersion has already pinned spec.version to
			// 2.19.0 in memory, and every builder reads its bundle off that field.
			Consistently(func() string {
				return workloadImages(ns, "core")
			}, "3s", platformInterval).Should(And(
				ContainSubstring(":"+platformVersion218),
				Not(ContainSubstring(":"+platformVersion219)),
			), "no part of the target bundle may render while the source virtual host still holds traffic")

			By("reporting the version the platform is still RUNNING, not the one it is moving to")
			Consistently(func() string {
				return getPlatform(ns).Status.ObservedVersion
			}, "2s", platformInterval).Should(Equal(platformVersion218))

			By("resuming from the persisted status alone, three times over")
			// A reconciler built fresh each pass is the operator restarting: it carries nothing
			// from the pass before, so whatever it does next comes from status.upgrade only.
			for i := 0; i < 3; i++ {
				reconcileOnce(ns)
			}
			u := getPlatform(ns).Status.Upgrade
			Expect(u).NotTo(BeNil())
			Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseDraining), "a resume re-enters the recorded phase")
			Expect(u.Fenced).To(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 1},
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3},
			), "re-entry must not re-record the fence over the counts it has to restore")
			Expect(workloadSpecReplicas(ns, "Deployment", "scheduler")).To(BeZero())
			Expect(workloadImages(ns, "core")).To(ContainSubstring(":" + platformVersion218))
		})
	})

	Context("A migration resumed mid-flight", func() {
		It("re-enters the recorded phase without re-fencing what is already fenced", func() {
			const ns = "ilm-migration-resume"
			// The persisted state of an operator that crashed after recording the fence: the
			// counts on status are the only surviving evidence of what the platform was running.
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseDraining, nil,
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 4},
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 7},
			)

			By("re-asserting the fence from the recorded state")
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(BeZero())

			for i := 0; i < 3; i++ {
				reconcileOnce(ns)
			}

			u := getPlatform(ns).Status.Upgrade
			Expect(u).NotTo(BeNil())
			Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseDraining))
			Expect(u.Fenced).To(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 4},
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 7},
			), "the recorded counts are what restore writes back; a resume must never overwrite them with the fenced zeroes")
			Expect(workloadImages(ns, "core")).To(ContainSubstring(":" + platformVersion218))
		})
	})

	Context("A workloadType flip while the fence holds a component", func() {
		It("is refused, so no workload of the other kind ever comes up outside the fence", func() {
			const ns = "ilm-migration-kindflip"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseDraining, nil,
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 2},
			)

			By("flipping the fenced gateway to a StatefulSet")
			Eventually(func(g Gomega) {
				var p otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, platformKey(ns), &p)).To(Succeed())
				p.Spec.Gateway.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
				g.Expect(k8sClient.Update(ctx, &p)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("refusing the change rather than chasing it")
			Eventually(func() string {
				c := migrationConditionOf(ns)
				if c == nil {
					return ""
				}
				return c.Reason
			}, platformTimeout, platformInterval).Should(Equal(reasonMigrationWorkloadKindChanged))
			Expect(getPlatform(ns).Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseDegraded))

			By("never creating the other-kind workload")
			// The apiserver would default its .spec.replicas to one, and the fence — which
			// addresses the recorded Deployment — would not touch it: a producer publishing to
			// the very virtual host the migration is draining.
			Consistently(func() error {
				var sts appsv1.StatefulSet
				return k8sClient.Get(ctx, types.NamespacedName{Name: "api-gateway", Namespace: ns}, &sts)
			}, "3s", platformInterval).ShouldNot(Succeed())
			Expect(getPlatform(ns).Status.Upgrade.Fenced).To(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 2},
			), "the refusal leaves the fence exactly as it was")
		})
	})

	Context("A migration reverted while it is still reversible", func() {
		It("restores every producer at its recorded count and clears the record", func() {
			const ns = "ilm-migration-abort"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseFencing, func(p *otilmv1alpha1.Platform) {
				p.Spec.Scheduler.Replicas = ptr(int32(2))
			})

			By("waiting for the fence to take hold")
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(BeZero())

			By("reverting spec.version to the version the platform is running")
			requestPlatformVersion(ns, platformVersion218)

			By("unwinding the migration completely")
			Eventually(func() *otilmv1alpha1.UpgradeStatus {
				return getPlatform(ns).Status.Upgrade
			}, platformTimeout, platformInterval).Should(BeNil())
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(Equal(int32(2)))
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "api-gateway")
			}, platformTimeout, platformInterval).Should(Equal(int32(1)))

			Expect(migrationConditionOf(ns).Reason).To(Equal(reasonMigrationAborted))
			Expect(getPlatform(ns).Status.ObservedVersion).To(Equal(platformVersion218))
		})
	})
})
