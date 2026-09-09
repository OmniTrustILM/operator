/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// The unit tests prove what the cleanup DECIDES — which class it deletes, and what stops it.
// This one proves it is wired into a running platform, and it proves the two facts a fake
// client cannot: that the phase reaches its end through the real reconcile, and that the
// migration it finishes stays finished.
//
// envtest serves no rabbitmq.com CRDs, so no source topology object can exist here — which IS
// the cleanup's terminal case (a Kind the cluster does not serve is positive proof that no
// object of it is left), and the case that must lead to the migration finishing rather than to
// a wedged phase or a spurious failure. What that leaves uncovered is an EXTANT source
// topology: the class-by-class reclaim, the final barrier and the blocked terminal are driven
// against a fake client that can hold rabbitmq.com objects.

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

var _ = Describe("Messaging migration cleanup", func() {
	Context("A cleanup with nothing left to reclaim", func() {
		It("finishes the migration, restores whatever is still fenced and does not start again", func() {
			const ns = "ilm-migration-cleanup"
			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseCleaningUp, managedMessagingPlatform,
				otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 1})

			By("discarding the record and pinning the version the platform reached, in one write")
			Eventually(func(g Gomega) {
				p := getPlatform(ns)
				g.Expect(p.Status.Upgrade).To(BeNil())
				g.Expect(p.Status.ObservedVersion).To(Equal(platformVersion219))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("bringing back a workload the fence was still holding")
			// A completed cutover leaves nothing fenced; this fixture is the safety-net case — an
			// interrupted or forced cutover — and the migration may not end with a workload parked
			// at zero replicas and no record left to bring it back.
			Eventually(func() int32 {
				return workloadSpecReplicas(ns, kindDeployment, schedulerWorkloadName)
			}, platformTimeout, platformInterval).Should(Equal(int32(1)))

			By("reporting a finished migration without leaking a coordinate")
			cond := migrationConditionOf(ns)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonMigrationCompleted))
			Expect(cond.Message).NotTo(ContainSubstring(sourceVirtualHost))
			Expect(cond.Message).NotTo(ContainSubstring(adminPassword))

			By("going on rendering the target bundle")
			Eventually(func() string {
				return workloadImages(ns, feAdministratorWorkloadName)
			}, platformTimeout, platformInterval).Should(ContainSubstring(":" + platformVersion219))

			By("never starting the same migration again")
			// The record and the reported version were discarded together for this reason: a
			// cleared record beside a status still naming the source version reads as a fresh
			// upgrade request, and the engine would fence the producers all over again.
			for i := 0; i < 3; i++ {
				reconcileOnce(ns)
			}
			Consistently(func(g Gomega) {
				p := getPlatform(ns)
				g.Expect(p.Status.Upgrade).To(BeNil())
				g.Expect(p.Status.ObservedVersion).To(Equal(platformVersion219))
			}, "2s", platformInterval).Should(Succeed())
			Expect(workloadSpecReplicas(ns, kindDeployment, schedulerWorkloadName)).To(Equal(int32(1)))
		})
	})
})
