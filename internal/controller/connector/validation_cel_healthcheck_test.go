/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	connbuilder "github.com/OmniTrustILM/operator/internal/builder/connector"
)

// pathRuleMessage is the message of the path's admission rule.
const pathRuleMessage = "healthCheck.path must be a URL path: a leading /, printable characters other than ? and #, and % only in two-hex-digit escapes"

// These specs run the healthCheck schema through apiserver admission. Each rejection names the
// offending field or rule.
var _ = Describe("Connector healthCheck validation", func() {
	var ns string
	var specs int

	BeforeEach(func() {
		specs++
		ns = createTestNamespace(fmt.Sprintf("test-cel-healthcheck-%d", specs))
	})

	DescribeTable("rejects",
		func(spec otilmv1alpha1.HealthCheckSpec, wantInError string) {
			conn := newConnector("cel-healthcheck", ns)
			conn.Spec.HealthCheck = &spec

			err := k8sClient.Create(ctx, conn)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantInError))
		},
		Entry("a timeout above the period", otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 20, TimeoutSeconds: 21},
			"healthCheck.timeoutSeconds must be at most healthCheck.periodSeconds"),
		Entry("a period under 10 seconds", otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 5, TimeoutSeconds: 1}, "periodSeconds"),
		Entry("a timeout over 30 seconds", otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 60, TimeoutSeconds: 31}, "timeoutSeconds"),
		Entry("a relative path", otilmv1alpha1.HealthCheckSpec{Path: "v2/health"}, pathRuleMessage),
		Entry("a path with a broken escape", otilmv1alpha1.HealthCheckSpec{Path: "/health%zz"}, pathRuleMessage),
		Entry("a path with a control character", otilmv1alpha1.HealthCheckSpec{Path: "/health\x01"}, pathRuleMessage),
		Entry("a path with a query", otilmv1alpha1.HealthCheckSpec{Path: "/health?verbose=true"}, pathRuleMessage),
		Entry("a path with a fragment", otilmv1alpha1.HealthCheckSpec{Path: "/health#details"}, pathRuleMessage),
		Entry("a path over 1024 bytes", otilmv1alpha1.HealthCheckSpec{Path: "/" + strings.Repeat("a", 1024)}, "1024"),
	)

	DescribeTable("accepts",
		func(spec otilmv1alpha1.HealthCheckSpec) {
			conn := newConnector("cel-healthcheck", ns)
			conn.Spec.HealthCheck = &spec

			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		},
		Entry("a path with a percent-escape", otilmv1alpha1.HealthCheckSpec{Path: "/health%2Fliveness"}),
		Entry("a block that sets every field", otilmv1alpha1.HealthCheckSpec{Path: "/v1/health", PeriodSeconds: 60, TimeoutSeconds: 30}),
		Entry("the shortest period alone", otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 10}),
		Entry("the longest timeout alone", otilmv1alpha1.HealthCheckSpec{TimeoutSeconds: 30}),
	)

	It("defaults an empty block to what an absent block resolves to", func() {
		conn := newConnector("cel-healthcheck", ns)
		conn.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())

		var stored otilmv1alpha1.Connector
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: conn.Name, Namespace: ns}, &stored)).To(Succeed())
		absent := stored.DeepCopy()
		absent.Spec.HealthCheck = nil
		resolved := connbuilder.ResolveHealthCheck(absent)

		Expect(stored.Spec.HealthCheck.Enabled).To(HaveValue(Equal(resolved.Enabled)))
		Expect(time.Duration(stored.Spec.HealthCheck.PeriodSeconds) * time.Second).To(Equal(resolved.Period))
		Expect(time.Duration(stored.Spec.HealthCheck.TimeoutSeconds) * time.Second).To(Equal(resolved.Timeout))
	})
})
