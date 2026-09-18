/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// These specs exercise the rollout-strategy CEL (XValidation) on the Connector CRD at the
// apiserver admission layer: each one asserts the apiserver rejects a cross-field-invalid
// CR with the rule's own message, and accepts the corrected one. The reconciler plays no
// part — admission happens before any reconcile — so nothing here awaits readiness.
var _ = Describe("Connector CEL validation", func() {
	bound := func(v intstr.IntOrString) *intstr.IntOrString { return &v }

	Context("TestStrategyRollingUpdateUnderRecreate", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-recreate-bounds")
		})

		It("should reject rolling-update bounds under type Recreate", func() {
			conn := newConnector("cel-recreate-bounds", ns)
			conn.Spec.Strategy = &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "Recreate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: bound(intstr.FromInt32(0))},
			}

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rollingUpdate is valid only with type: RollingUpdate"))
		})
	})

	Context("TestStrategyBothBoundsZero", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-both-zero")
		})

		It("should reject maxSurge and maxUnavailable both at zero", func() {
			conn := newConnector("cel-both-zero", ns)
			conn.Spec.Strategy = &otilmv1alpha1.DeploymentStrategySpec{
				Type: "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{
					MaxSurge:       bound(intstr.FromInt32(0)),
					MaxUnavailable: bound(intstr.FromString("0%")),
				},
			}

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maxSurge and maxUnavailable may not both be zero"))
		})
	})

	Context("TestStrategyZeroSurgeAccepted", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-zero-surge")
		})

		It("should accept zero surge beside a non-zero maxUnavailable", func() {
			conn := newConnector("cel-zero-surge", ns)
			conn.Spec.Strategy = &otilmv1alpha1.DeploymentStrategySpec{
				Type: "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{
					MaxSurge:       bound(intstr.FromInt32(0)),
					MaxUnavailable: bound(intstr.FromInt32(1)),
				},
			}

			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		})
	})

	Context("TestStrategyInvalidBounds", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-invalid-bounds")
		})

		It("should reject bounds the Deployment API would reject", func() {
			invalidBounds := []struct {
				name           string
				maxSurge       *intstr.IntOrString
				maxUnavailable *intstr.IntOrString
			}{
				{name: "invalid-surge-percent", maxSurge: bound(intstr.FromString("nope"))},
				{name: "negative-surge-integer", maxSurge: bound(intstr.FromInt32(-1))},
				{name: "invalid-unavailable-percent", maxUnavailable: bound(intstr.FromString("nope"))},
				{name: "negative-unavailable-integer", maxUnavailable: bound(intstr.FromInt32(-1))},
			}

			for _, tt := range invalidBounds {
				conn := newConnector("cel-"+tt.name, ns)
				conn.Spec.Strategy = &otilmv1alpha1.DeploymentStrategySpec{
					Type: "RollingUpdate",
					RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{
						MaxSurge:       tt.maxSurge,
						MaxUnavailable: tt.maxUnavailable,
					},
				}

				err := k8sClient.Create(ctx, conn)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("must be a non-negative integer or percentage"))
			}
		})
	})

	Context("TestStrategyUnknownType", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-cel-unknown-type")
		})

		It("should reject a strategy type outside the enum", func() {
			conn := newConnector("cel-unknown-type", ns)
			conn.Spec.Strategy = &otilmv1alpha1.DeploymentStrategySpec{Type: "OnDelete"}

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})
	})
})
