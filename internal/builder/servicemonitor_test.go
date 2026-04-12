package builder_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildServiceMonitor(t *testing.T) {
	path := "/v1/metrics"
	interval := "30s"
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled: true,
		Path:    &path,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled:  true,
			Interval: &interval,
			Labels: map[string]string{
				"release": "prometheus",
			},
		},
	}

	sm := builder.BuildServiceMonitor(conn)
	require.NotNil(t, sm)

	// Name and namespace
	assert.Equal(t, testConnectorName, sm.Name)
	assert.Equal(t, "default", sm.Namespace)

	// Labels: Labels(conn) merged with spec.metrics.serviceMonitor.labels
	expectedLabels := builder.Labels(conn)
	expectedLabels["release"] = "prometheus"
	assert.Equal(t, expectedLabels, sm.Labels)

	// Selector matches SelectorLabels
	assert.Equal(t, builder.SelectorLabels(conn), sm.Spec.Selector.MatchLabels)

	// Exactly one endpoint
	require.Len(t, sm.Spec.Endpoints, 1)
	ep := sm.Spec.Endpoints[0]
	assert.Equal(t, "http", ep.Port)
	assert.Equal(t, path, ep.Path)
	assert.Equal(t, interval, string(ep.Interval))

	// NamespaceSelector
	assert.Equal(t, []string{"default"}, sm.Spec.NamespaceSelector.MatchNames)
}

func TestBuildServiceMonitorDefaultPath(t *testing.T) {
	interval := "60s"
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled: true,
		// Path not set — should default to "/v1/metrics"
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled:  true,
			Interval: &interval,
		},
	}

	sm := builder.BuildServiceMonitor(conn)
	require.NotNil(t, sm)

	require.Len(t, sm.Spec.Endpoints, 1)
	assert.Equal(t, "/v1/metrics", sm.Spec.Endpoints[0].Path)
}

func TestBuildServiceMonitorDisabled(t *testing.T) {
	// No metrics spec at all → nil
	conn := newTestConnector()

	sm := builder.BuildServiceMonitor(conn)
	assert.Nil(t, sm)
}

func TestBuildServiceMonitorMetricsEnabledButSMDisabled(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled: true,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled: false,
		},
	}

	sm := builder.BuildServiceMonitor(conn)
	assert.Nil(t, sm)
}

func TestBuildServiceMonitorNilServiceMonitor(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: nil,
	}

	sm := builder.BuildServiceMonitor(conn)
	assert.Nil(t, sm)
}
