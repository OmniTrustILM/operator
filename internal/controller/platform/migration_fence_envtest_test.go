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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// The migration fence's whole claim is an OWNERSHIP claim: a workload scaled to zero stays at
// zero across the operator's own reconciles, because the fence re-writes .spec.replicas behind
// every Server-Side Apply under its own field manager. Only a real apiserver tracks
// managedFields, so this envtest — repeated reconciles against a live workload, then a look at
// who actually owns the field — is the one place that claim can be proved rather than asserted.
//
// The specs drive Reconcile DIRECTLY (a reconciler bound to the uncached test client) so the
// number of passes is exact. The suite's manager is reconciling the same Platform in the
// background, which only strengthens the result: whichever actor applies, the zero must hold.

// fenceFieldManagerName is the field manager the fence is CONTRACTED to write with, stated
// literally: it is what an operator reads out of managedFields when diagnosing a stuck
// workload, so a rename of the production constant must fail this test rather than move the
// implementation and its expectation together.
const fenceFieldManagerName = "ilm-operator-migration-fence"

// fenceReconciler returns a reconciler bound to the uncached test client, so a spec can run an
// exact number of reconcile passes and read the result back without informer lag.
func fenceReconciler() *Reconciler {
	return &Reconciler{
		Client:        k8sClient,
		Scheme:        mgr.GetScheme(),
		Capabilities:  fakeCaps,
		OIDCRegistrar: fakeOIDC,
		BrokerAdmins:  fakeBrokerAdmins.factory,
		Recorder:      mgr.GetEventRecorderFor("ilm-operator"), //nolint:staticcheck // the controller-runtime record.EventRecorder API is intentionally retained
	}
}

// platformKey is the NamespacedName of a spec's Platform (every fixture names it "ilm").
func platformKey(ns string) types.NamespacedName {
	return types.NamespacedName{Name: "ilm", Namespace: ns}
}

// getPlatform re-reads the Platform for a status write (each write needs a current
// resourceVersion).
func getPlatform(ns string) *otilmv1alpha1.Platform {
	var p otilmv1alpha1.Platform
	ExpectWithOffset(1, k8sClient.Get(ctx, platformKey(ns), &p)).To(Succeed())
	return &p
}

// startMigration puts a platform into the state fenceWorkloads is entered from: spec.version
// asking for the target, and a migration recorded on status.upgrade with NO workloads fenced
// yet.
//
// spec.version must name the TARGET. A recorded migration whose target the spec no longer asks
// for is, by the trigger layer's own matrix, a REVERT — the engine would abort it and lift the
// fence, which is the opposite of what these specs are here to observe.
func startMigration(ns string) {
	requestPlatformVersion(ns, platformVersion219)
	recordMigration(ns, otilmv1alpha1.MigrationPhaseFencing)
}

// awaitWorkload waits for a rendered workload to exist and returns its (possibly nil)
// .spec.replicas, so a spec can pin the count the fence is expected to record.
func awaitWorkload(ns, name string, obj client.Object) {
	EventuallyWithOffset(1, func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)
	}, platformTimeout, platformInterval).Should(Succeed())
}

// workloadSpecReplicas reads .spec.replicas off a live workload, returning -1 when the field
// is absent — a value no fenced or restored workload may ever show, so "unset" can never be
// mistaken for a fence.
func workloadSpecReplicas(ns, kind, name string) int32 {
	obj, err := workloadObject(kind)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)).To(Succeed())
	switch w := obj.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas == nil {
			return -1
		}
		return *w.Spec.Replicas
	case *appsv1.StatefulSet:
		if w.Spec.Replicas == nil {
			return -1
		}
		return *w.Spec.Replicas
	}
	return -1
}

// replicasFieldOwners returns the field managers that claim .spec.replicas in a workload's
// managedFields — the apiserver's own record of who owns the field, and therefore the only
// authoritative answer to "who is holding this workload down".
func replicasFieldOwners(ns, kind, name string) []string {
	obj, err := workloadObject(kind)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)).To(Succeed())

	var owners []string
	for _, e := range obj.GetManagedFields() {
		if e.FieldsV1 == nil {
			continue
		}
		var fields map[string]json.RawMessage
		ExpectWithOffset(1, json.Unmarshal(e.FieldsV1.Raw, &fields)).To(Succeed())
		raw, ok := fields["f:spec"]
		if !ok {
			continue
		}
		var spec map[string]json.RawMessage
		ExpectWithOffset(1, json.Unmarshal(raw, &spec)).To(Succeed())
		if _, claimed := spec["f:replicas"]; claimed {
			owners = append(owners, e.Manager)
		}
	}
	return owners
}

// fenceProducers drives fenceWorkloads by hand on a freshly read Platform.
//
// The read-and-retry matters: the suite's manager is reconciling the same in-flight migration,
// so a hand-driven status write can lose a benign optimistic-lock race. Both halves of the
// fence are idempotent, so a retry repeats no effect — it just gets a current object.
func fenceProducers(ns string) {
	EventuallyWithOffset(1, func() error {
		return fenceReconciler().fenceWorkloads(ctx, getPlatform(ns))
	}, platformTimeout, platformInterval).Should(Succeed())
}

// restoreProducer lifts the fence from one workload, with the same read-and-retry.
func restoreProducer(ns string, w otilmv1alpha1.FencedWorkload) {
	EventuallyWithOffset(1, func() error {
		return fenceReconciler().restoreWorkload(ctx, getPlatform(ns), w)
	}, platformTimeout, platformInterval).Should(Succeed())
}

// expectFenceHoldsAcrossReconciles runs five full reconciles and asserts the workload is at
// zero after every one of them, and stays there afterwards. Five passes is the point: a fence
// that merely survives the pass that set it would pass a single-reconcile test and still be
// un-fenced by the next ordinary apply.
func expectFenceHoldsAcrossReconciles(ns, kind, name string) {
	r := fenceReconciler()
	for i := 1; i <= 5; i++ {
		// The suite's manager reconciles the same Platform concurrently, so a pass can lose a
		// benign optimistic-lock race; retry until one completes rather than assert on a
		// scheduling accident. Every completed pass still ends in the fence.
		EventuallyWithOffset(1, func() error {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: platformKey(ns)})
			return err
		}, platformTimeout, platformInterval).Should(Succeed(), "reconcile %d must succeed", i)
		EventuallyWithOffset(1, func() int32 {
			return workloadSpecReplicas(ns, kind, name)
		}, "5s", platformInterval).Should(BeZero(), "reconcile %d must not un-fence %s %q", i, kind, name)
	}
	ConsistentlyWithOffset(1, func() int32 {
		return workloadSpecReplicas(ns, kind, name)
	}, "2s", platformInterval).Should(BeZero(), "the fence must hold between reconciles too")
}

var _ = Describe("Messaging migration fence", func() {
	Context("A fenced workload stays at zero across the operator's own reconciles", func() {
		It("records the prior counts, holds the producers at zero for five reconciles, then restores them", func() {
			const ns = "ilm-fence-deployment"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Scheduler.Replicas = ptr(int32(3))
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the producers to be applied with their configured counts")
			var sched appsv1.Deployment
			awaitWorkload(ns, "scheduler", &sched)
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(Equal(int32(3)))
			var gw appsv1.Deployment
			awaitWorkload(ns, "api-gateway", &gw)

			By("fencing the producers: the complete record is written before the first patch")
			startMigration(ns)
			fenceProducers(ns)

			recorded := getPlatform(ns).Status.Upgrade.Fenced
			Expect(recorded).To(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 1},
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3},
			), "the fence must record each workload's kind and the count it carried")

			By("holding the scheduler at zero across five reconciles")
			expectFenceHoldsAcrossReconciles(ns, "Deployment", "scheduler")
			Expect(workloadSpecReplicas(ns, "Deployment", "api-gateway")).To(BeZero(),
				"every fenced producer is held down, not just the first")

			By("attributing .spec.replicas to the fence's own field manager")
			Expect(replicasFieldOwners(ns, "Deployment", "scheduler")).To(ConsistOf(fenceFieldManagerName),
				"the fence — and nothing else — must own the fenced workload's replica count")

			By("restoring the exact recorded count and clearing the entry")
			restored := otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3}
			restoreProducer(ns, restored)
			Expect(workloadSpecReplicas(ns, "Deployment", "scheduler")).To(Equal(int32(3)))
			Expect(getPlatform(ns).Status.Upgrade.Fenced).To(ConsistOf(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 1},
			), "only the restored workload leaves the list")

			By("re-entering a completed restore (crash-safety): idempotent, no second effect")
			restoreProducer(ns, restored)
			Expect(workloadSpecReplicas(ns, "Deployment", "scheduler")).To(Equal(int32(3)))

			By("handing .spec.replicas back to the operator's apply once the entry is gone")
			reconcileOnce(ns)
			// The reconciler's apply claims the field again the moment the entry is gone, which
			// is the load-bearing half. The fence's own claim may LINGER alongside it, because
			// Server-Side Apply shares ownership rather than seizing it when the applied value
			// equals what is already there (restore wrote the same count the render sends). That
			// is harmless: the fence writes only for workloads still on the list, and the next
			// value the apply changes takes the field outright.
			Eventually(func() []string {
				return replicasFieldOwners(ns, "Deployment", "scheduler")
			}, platformTimeout, platformInterval).Should(ContainElement("ilm-operator"),
				"a workload removed from the list is rendered with its replicas again")
			Expect(workloadSpecReplicas(ns, "Deployment", "scheduler")).To(Equal(int32(3)))
			Expect(workloadSpecReplicas(ns, "Deployment", "api-gateway")).To(BeZero(),
				"the still-listed workload is unaffected by another's restore")
		})

		It("holds a StatefulSet-typed gateway at zero and records its kind", func() {
			const ns = "ilm-fence-statefulset"
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Gateway.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
				p.Spec.Gateway.Replicas = ptr(int32(2))
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the gateway StatefulSet")
			var gw appsv1.StatefulSet
			awaitWorkload(ns, "api-gateway", &gw)
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, "StatefulSet", "api-gateway")
			}, platformTimeout, platformInterval).Should(Equal(int32(2)))

			By("fencing it as a StatefulSet")
			startMigration(ns)
			fenceProducers(ns)
			Expect(getPlatform(ns).Status.Upgrade.Fenced).To(ContainElement(
				otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "StatefulSet", Replicas: 2},
			), "the effective workload kind must be recorded, not assumed")

			expectFenceHoldsAcrossReconciles(ns, "StatefulSet", "api-gateway")
			Expect(replicasFieldOwners(ns, "StatefulSet", "api-gateway")).To(ConsistOf(fenceFieldManagerName))

			By("restoring the StatefulSet's recorded count")
			restoreProducer(ns, otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "StatefulSet", Replicas: 2})
			Expect(workloadSpecReplicas(ns, "StatefulSet", "api-gateway")).To(Equal(int32(2)))
		})

		It("holds an HPA-configured component at zero (the operator never re-asserts a count)", func() {
			const ns = "ilm-fence-hpa"
			cpu := int32(75)
			minR := int32(2)
			p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Scheduler.Autoscaling = &otilmv1alpha1.AutoscalingSpec{
					MinReplicas: &minR, MaxReplicas: 5, TargetCPUUtilization: &cpu,
				}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for the HPA-owned scheduler Deployment")
			var sched appsv1.Deployment
			awaitWorkload(ns, "scheduler", &sched)

			By("fencing the HPA-owned component")
			startMigration(ns)
			fenceProducers(ns)

			// The count under an HPA is whatever the cluster currently runs, which is what
			// restore must put back — the fence records the LIVE value rather than a spec one.
			Expect(getPlatform(ns).Status.Upgrade.Fenced).To(ContainElement(
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 1},
			))

			// envtest runs no HPA controller, so this pins the half of the assumption the
			// operator is responsible for: with autoscaling configured, neither the render nor
			// the apply ever writes a replica count over the fence's zero.
			expectFenceHoldsAcrossReconciles(ns, "Deployment", "scheduler")
			Expect(replicasFieldOwners(ns, "Deployment", "scheduler")).To(ConsistOf(fenceFieldManagerName),
				"the fence owns .spec.replicas outright even where an HPA is configured")
		})
	})
})
