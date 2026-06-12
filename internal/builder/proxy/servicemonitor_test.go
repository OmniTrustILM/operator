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
