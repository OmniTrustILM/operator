/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

var _ = Describe("Connector volume CEL validation", func() {
	Context("TestVolumeBothSources", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-volume-both")
		})

		It("should reject a volume carrying both an emptyDir and a claim", func() {
			conn := newConnector("cel-volume-both", ns)
			conn.Spec.Volumes = []otilmv1alpha1.VolumeSpec{{
				Name:                  "state",
				MountPath:             "/var/lib/state",
				EmptyDir:              &otilmv1alpha1.EmptyDirSpec{},
				PersistentVolumeClaim: &otilmv1alpha1.PVCSpec{ClaimName: "state"},
			}}

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("either emptyDir or persistentVolumeClaim, not both"))
		})
	})

	Context("TestVolumeClaimAccepted", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-volume-claim")
		})

		It("should accept a volume backed by a claim", func() {
			conn := newConnector("cel-volume-claim", ns)
			conn.Spec.Volumes = []otilmv1alpha1.VolumeSpec{{
				Name:                  "state",
				MountPath:             "/var/lib/state",
				PersistentVolumeClaim: &otilmv1alpha1.PVCSpec{ClaimName: "state"},
			}}

			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		})
	})

	Context("TestVolumeNoSourceAccepted", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-volume-none")
		})

		It("should accept a volume naming no source, which stays an emptyDir", func() {
			conn := newConnector("cel-volume-none", ns)
			conn.Spec.Volumes = []otilmv1alpha1.VolumeSpec{{Name: "state", MountPath: "/var/lib/state"}}

			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		})
	})

	Context("TestVolumeEmptyClaimName", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-volume-empty-claim")
		})

		It("should reject a claim source with no claim name", func() {
			conn := newConnector("cel-volume-empty-claim", ns)
			conn.Spec.Volumes = []otilmv1alpha1.VolumeSpec{{
				Name:                  "state",
				MountPath:             "/var/lib/state",
				PersistentVolumeClaim: &otilmv1alpha1.PVCSpec{},
			}}

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("claimName"))
		})
	})
})
