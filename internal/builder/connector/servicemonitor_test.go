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

package connector_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/connector"
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

	sm := connector.BuildServiceMonitor(conn)
	require.NotNil(t, sm)

	// Name and namespace
	assert.Equal(t, testConnectorName, sm.Name)
	assert.Equal(t, "default", sm.Namespace)

	// Labels: Labels(conn) merged with spec.metrics.serviceMonitor.labels
	expectedLabels := connector.Labels(conn)
	expectedLabels["release"] = "prometheus"
	assert.Equal(t, expectedLabels, sm.Labels)

	// Selector matches SelectorLabels
	assert.Equal(t, connector.SelectorLabels(conn), sm.Spec.Selector.MatchLabels)

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

	sm := connector.BuildServiceMonitor(conn)
	require.NotNil(t, sm)

	require.Len(t, sm.Spec.Endpoints, 1)
	assert.Equal(t, "/v1/metrics", sm.Spec.Endpoints[0].Path)
}

func TestBuildServiceMonitorDisabled(t *testing.T) {
	// No metrics spec at all → nil
	conn := newTestConnector()

	sm := connector.BuildServiceMonitor(conn)
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

	sm := connector.BuildServiceMonitor(conn)
	assert.Nil(t, sm)
}

func TestBuildServiceMonitorMetricsDisabledSMEnabled(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled: false,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled: true,
		},
	}

	sm := connector.BuildServiceMonitor(conn)
	assert.Nil(t, sm, "expected nil when Metrics.Enabled is false even if ServiceMonitor.Enabled is true")
}

func TestBuildServiceMonitorNilServiceMonitor(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: nil,
	}

	sm := connector.BuildServiceMonitor(conn)
	assert.Nil(t, sm)
}
