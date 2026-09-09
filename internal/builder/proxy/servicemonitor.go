/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// defaultMetricsPath is the proxy binary's metrics endpoint. It deliberately differs
// from the platform components' /v1/metrics — the reason ProxyMetricsSpec exists.
const defaultMetricsPath = "/metrics"

// BuildServiceMonitor constructs a Prometheus ServiceMonitor for the given Proxy.
// Returns nil unless metrics and the ServiceMonitor are both enabled. Rendering is
// capability-gated by the controller (the CRD may not be installed).
func BuildServiceMonitor(px *otilmv1alpha1.Proxy) *monitoringv1.ServiceMonitor {
	if px.Spec.Metrics == nil || !px.Spec.Metrics.Enabled {
		return nil
	}
	smSpec := px.Spec.Metrics.ServiceMonitor
	if smSpec == nil || !smSpec.Enabled {
		return nil
	}

	labels := make(map[string]string)
	for k, v := range Labels(px) {
		labels[k] = v
	}
	for k, v := range smSpec.Labels {
		labels[k] = v
	}

	metricsPath := defaultMetricsPath
	if px.Spec.Metrics.Path != nil && *px.Spec.Metrics.Path != "" {
		metricsPath = *px.Spec.Metrics.Path
	}

	var interval monitoringv1.Duration
	if smSpec.Interval != nil {
		interval = monitoringv1.Duration(*smSpec.Interval)
	}

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildResourceName(px),
			Namespace: px.Namespace,
			Labels:    labels,
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector:          metav1.LabelSelector{MatchLabels: SelectorLabels(px)},
			Endpoints:         []monitoringv1.Endpoint{{Port: "http", Path: metricsPath, Interval: interval}},
			NamespaceSelector: monitoringv1.NamespaceSelector{MatchNames: []string{px.Namespace}},
		},
	}
}
