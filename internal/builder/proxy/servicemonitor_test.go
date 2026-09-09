/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func TestBuildServiceMonitorNilCases(t *testing.T) {
	assert.Nil(t, BuildServiceMonitor(newProxy()), "nil metrics")

	px := newProxy()
	px.Spec.Metrics = &otilmv1alpha1.ProxyMetricsSpec{Enabled: true}
	assert.Nil(t, BuildServiceMonitor(px), "no serviceMonitor block")

	px.Spec.Metrics.ServiceMonitor = &otilmv1alpha1.ServiceMonitorSpec{Enabled: false}
	assert.Nil(t, BuildServiceMonitor(px), "serviceMonitor disabled")
}

func TestBuildServiceMonitorDefaultsToProxyMetricsPath(t *testing.T) {
	px := newProxy()
	px.Spec.Metrics = &otilmv1alpha1.ProxyMetricsSpec{
		Enabled:        true,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true, Labels: map[string]string{"release": "prom"}},
	}
	sm := BuildServiceMonitor(px)
	require.NotNil(t, sm)
	require.Len(t, sm.Spec.Endpoints, 1)
	assert.Equal(t, "/metrics", sm.Spec.Endpoints[0].Path, "proxy serves /metrics, not /v1/metrics")
	assert.Equal(t, "http", sm.Spec.Endpoints[0].Port)
	assert.Equal(t, "prom", sm.Labels["release"])
	assert.Equal(t, SelectorLabels(px), sm.Spec.Selector.MatchLabels)
}

func TestBuildServiceMonitorExplicitPath(t *testing.T) {
	px := newProxy()
	px.Spec.Metrics = &otilmv1alpha1.ProxyMetricsSpec{
		Enabled:        true,
		Path:           ptr.To("/custom"),
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
	}
	assert.Equal(t, "/custom", BuildServiceMonitor(px).Spec.Endpoints[0].Path)
}
