package platform

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("Workload kind switch", func() {
	It("stops the old workload before the new kind appears (no migration recorded)", func() {
		const ns = "ilm-workload-switch"
		p := lifecyclePlatform(ns, nil)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		r := fenceReconciler() // the suite's fully-wired Reconciler constructor
		reconcile := func() {
			EventuallyWithOffset(1, func() error {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: platformKey(ns)})
				return err
			}, platformTimeout, platformInterval).Should(Succeed())
		}

		By("converging the scheduler as a Deployment")
		reconcile()
		key := types.NamespacedName{Name: "scheduler", Namespace: ns}
		Eventually(func() error {
			var dep appsv1.Deployment
			return k8sClient.Get(ctx, key, &dep)
		}, platformTimeout, platformInterval).Should(Succeed())

		By("flipping scheduler.workloadType to StatefulSet")
		Eventually(func(g Gomega) {
			live := getPlatform(ns)
			live.Spec.Scheduler.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
			g.Expect(k8sClient.Update(ctx, live)).To(Succeed())
		}, platformTimeout, platformInterval).Should(Succeed())
		reconcile()

		By("verifying the Deployment is terminating, NO StatefulSet exists yet, and the marker is recorded")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
		Expect(dep.GetDeletionTimestamp()).NotTo(BeNil(), "the superseded Deployment must be stopping")
		var sts appsv1.StatefulSet
		err := k8sClient.Get(ctx, key, &sts)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the new kind must NOT exist while the old one still has pods — that is the whole point")
		marker := meta.FindStatusCondition(getPlatform(ns).Status.Conditions, conditionWorkloadKindSwitch)
		Expect(marker).NotTo(BeNil())
		Expect(marker.Status).To(Equal(metav1.ConditionTrue),
			"the durable marker is what keeps a messaging migration out while the switch runs")

		By("simulating the garbage collector finishing (envtest runs none)")
		dep.Finalizers = nil
		Expect(k8sClient.Update(ctx, &dep)).To(Succeed())

		By("verifying the marker SURVIVES the gap in which neither kind exists")
		// The suite's manager is reconciling this Platform concurrently (see suite_test.go), so a
		// bare point-in-time check taken AFTER a separate Eventually resolves can race it: the
		// background reconcile can legitimately notice the Deployment gone, apply the StatefulSet,
		// and retire the marker before that later check runs. Snapshotting the marker in the SAME
		// poll iteration that first confirms the Deployment is gone shrinks the window to two
		// back-to-back reads with no yield to another reconcile in between.
		var markerTrueAtGap bool
		Eventually(func(g Gomega) {
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue(),
				"the superseded Deployment must actually be gone before this counts as the gap")
			markerTrueAtGap = meta.IsStatusConditionTrue(getPlatform(ns).Status.Conditions, conditionWorkloadKindSwitch)
		}, platformTimeout, platformInterval).Should(Succeed())
		Expect(markerTrueAtGap).To(BeTrue(),
			"with both objects gone, only the marker still says a switch is under way")

		By("verifying the next reconciles apply the StatefulSet and then retire the marker")
		reconcile()
		Eventually(func() error {
			var s appsv1.StatefulSet
			return k8sClient.Get(ctx, key, &s)
		}, platformTimeout, platformInterval).Should(Succeed())
		Eventually(func() bool {
			reconcile()
			return meta.IsStatusConditionTrue(getPlatform(ns).Status.Conditions, conditionWorkloadKindSwitch)
		}, platformTimeout, platformInterval).Should(BeFalse(),
			"once every rendered workload exists and none of the old kind is left, the marker is cleared")
	})

	It("refuses a Core kind switch while a migration holds Core in CuttingOver", func() {
		const ns = "ilm-workload-switch-migrating"
		p := lifecyclePlatform(ns, func(p *otilmv1alpha1.Platform) {
			p.Spec.Messaging.Mode = "managed"
			p.Spec.Messaging.Host = ""
			p.Spec.Messaging.Credentials = nil
			p.Spec.Messaging.Managed = &otilmv1alpha1.ManagedMessagingSpec{
				Replicas: 1, Storage: otilmv1alpha1.StorageSpec{Size: "1Gi"},
			}
		})
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		r := fenceReconciler()
		By("converging Core as a Deployment")
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: platformKey(ns)})
		coreKey := types.NamespacedName{Name: "core", Namespace: ns}
		Eventually(func() error {
			var dep appsv1.Deployment
			return k8sClient.Get(ctx, coreKey, &dep)
		}, platformTimeout, platformInterval).Should(Succeed())

		By("recording a migration mid-cutover and flipping core.workloadType in the same edit")
		now := metav1.Now()
		Eventually(func(g Gomega) {
			live := getPlatform(ns)
			live.Status.ObservedVersion = platformVersion218
			live.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
				FromVersion: platformVersion218, ToVersion: platformVersion219,
				Phase:     otilmv1alpha1.MigrationPhaseCuttingOver,
				StartedAt: now, PhaseStartedAt: now,
			}
			g.Expect(k8sClient.Status().Update(ctx, live)).To(Succeed())
		}, platformTimeout, platformInterval).Should(Succeed())
		Eventually(func(g Gomega) {
			live := getPlatform(ns)
			live.Spec.Version = platformVersion219
			live.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
			g.Expect(k8sClient.Update(ctx, live)).To(Succeed())
		}, platformTimeout, platformInterval).Should(Succeed())
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: platformKey(ns)})

		By("verifying the pass was refused and the running Core Deployment is untouched")
		Eventually(func(g Gomega) {
			got := getPlatform(ns)
			cond := meta.FindStatusCondition(got.Status.Conditions, conditionDegraded)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(reasonMigrationWorkloadKindChanged))
		}, platformTimeout, platformInterval).Should(Succeed())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, coreKey, &dep)).To(Succeed())
		Expect(dep.GetDeletionTimestamp()).To(BeNil(), "a refused switch must not have deleted anything")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, coreKey, &appsv1.StatefulSet{}))).To(BeTrue())
	})
})
