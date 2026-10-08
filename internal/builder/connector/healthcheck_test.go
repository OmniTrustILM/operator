/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/connector"
)

func TestResolveHealthCheck(t *testing.T) {
	tests := []struct {
		name        string
		healthCheck *otilmv1alpha1.HealthCheckSpec
		want        connector.HealthCheck
	}{
		{
			name:        "unset checks v2, then v1, every 30s within 10s",
			healthCheck: nil,
			want:        connector.HealthCheck{Enabled: true, Paths: []string{"/v2/health", "/v1/health"}, Period: 30 * time.Second, Timeout: 10 * time.Second},
		},
		{
			name:        "an empty block takes the same defaults",
			healthCheck: &otilmv1alpha1.HealthCheckSpec{},
			want:        connector.HealthCheck{Enabled: true, Paths: []string{"/v2/health", "/v1/health"}, Period: 30 * time.Second, Timeout: 10 * time.Second},
		},
		{
			name:        "an explicit path is checked alone",
			healthCheck: &otilmv1alpha1.HealthCheckSpec{Path: "/v1/health"},
			want:        connector.HealthCheck{Enabled: true, Paths: []string{"/v1/health"}, Period: 30 * time.Second, Timeout: 10 * time.Second},
		},
		{
			name:        "period and timeout",
			healthCheck: &otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 15, TimeoutSeconds: 5},
			want:        connector.HealthCheck{Enabled: true, Paths: []string{"/v2/health", "/v1/health"}, Period: 15 * time.Second, Timeout: 5 * time.Second},
		},
		{
			name:        "switched off",
			healthCheck: &otilmv1alpha1.HealthCheckSpec{Enabled: ptr.To(false)},
			want:        connector.HealthCheck{Enabled: false, Paths: []string{"/v2/health", "/v1/health"}, Period: 30 * time.Second, Timeout: 10 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := newTestConnector()
			conn.Spec.HealthCheck = tt.healthCheck

			assert.Equal(t, tt.want, connector.ResolveHealthCheck(conn))
		})
	}
}

func TestServiceEndpoint(t *testing.T) {
	conn := newTestConnector()
	conn.Namespace = "ilm-signing-test"
	conn.Spec.Service.Port = 8443

	assert.Equal(t, "http://test-connector.ilm-signing-test.svc.cluster.local:8443", connector.ServiceEndpoint(conn))
}
