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

package common

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildServiceMonitorNilWhenDisabled(t *testing.T) {
	c := Component{Name: "core", Namespace: "ilm", Port: 8080}
	assert.Nil(t, BuildServiceMonitor(c, nil), "nil metrics => no ServiceMonitor")
	assert.Nil(t, BuildServiceMonitor(c, &otilmv1alpha1.MetricsSpec{Enabled: false}), "disabled metrics => none")
	assert.Nil(t, BuildServiceMonitor(c, &otilmv1alpha1.MetricsSpec{Enabled: true}), "no ServiceMonitor block => none")
	assert.Nil(t, BuildServiceMonitor(c, &otilmv1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: false},
	}), "ServiceMonitor disabled => none")
}

func TestBuildServiceMonitorRendered(t *testing.T) {
	c := Component{Name: "core", Instance: "ilm", Namespace: "ilm", Port: 8080}
	interval := "30s"
	sm := BuildServiceMonitor(c, &otilmv1alpha1.MetricsSpec{
		Enabled: true,
		Path:    strPtr("/api/v1/metrics"),
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled:  true,
			Interval: &interval,
			Labels:   map[string]string{"release": "prom"},
		},
	})
	require.NotNil(t, sm)
	assert.Equal(t, "core", sm.Name)
	assert.Equal(t, "ilm", sm.Namespace)
	assert.Equal(t, "prom", sm.Labels["release"], "user labels merged on")
	assert.Equal(t, "core", sm.Labels[NameLabel], "operator labels present")
	require.Len(t, sm.Spec.Endpoints, 1)
	assert.Equal(t, "http", sm.Spec.Endpoints[0].Port)
	assert.Equal(t, "/api/v1/metrics", sm.Spec.Endpoints[0].Path)
	assert.EqualValues(t, "30s", sm.Spec.Endpoints[0].Interval)
	assert.Equal(t, []string{"ilm"}, sm.Spec.NamespaceSelector.MatchNames)
	assert.Equal(t, c.SelectorLabels(), sm.Spec.Selector.MatchLabels)
}

func TestBuildServiceMonitorDefaultPathAndPrimaryPortName(t *testing.T) {
	// A component with a custom primary port name (e.g. the gateway) scrapes that named
	// port; the path defaults when not overridden.
	c := Component{Name: "api-gateway", Instance: "ilm", Namespace: "ilm", Port: 8000, PrimaryPortName: "consumer-http"}
	sm := BuildServiceMonitor(c, &otilmv1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
	})
	require.NotNil(t, sm)
	assert.Equal(t, "consumer-http", sm.Spec.Endpoints[0].Port)
	assert.Equal(t, defaultMetricsPath, sm.Spec.Endpoints[0].Path)
}

func TestSANameOverrideAndDefault(t *testing.T) {
	assert.Equal(t, "core", Component{Name: "core"}.SAName(), "defaults to the component name")
	assert.Equal(t, "custom", Component{Name: "core", ServiceAccountName: "custom"}.SAName())
}

func TestMergeLabelsOperatorWins(t *testing.T) {
	user := map[string]string{"team": "x", NameLabel: "evil"}
	op := map[string]string{NameLabel: "core"}
	out := mergeLabels(user, op)
	assert.Equal(t, "x", out["team"], "user label preserved")
	assert.Equal(t, "core", out[NameLabel], "operator label wins the conflict")
	// Empty user map returns the operator map unchanged (same reference).
	assert.Equal(t, op, mergeLabels(nil, op))
}
