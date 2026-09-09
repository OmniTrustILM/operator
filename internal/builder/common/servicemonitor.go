/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultMetricsPath is the metrics endpoint path used when the component's MetricsSpec
// does not override it (matches the Connector default and the platform service shape).
const defaultMetricsPath = "/v1/metrics"

// BuildServiceMonitor renders a Prometheus ServiceMonitor for a component from its
// MetricsSpec, or nil when metrics / the ServiceMonitor are not enabled. It is the
// CRD-agnostic per-component ServiceMonitor builder (the Connector has its own with the
// same shape); the operator emits a precise ServiceMonitor PER component.
//
// The endpoint scrapes the component Service's named "http" port (the primary port) at
// the configured path/interval; the selector matches the component's pods, and the
// ServiceMonitor is labelled with the component's standard labels plus any user labels
// from spec.metrics.serviceMonitor.labels (user labels win). The Service-port-targeted
// scrape mirrors the Connector ServiceMonitor (metrics.port is reserved for a future
// separate metrics port).
func BuildServiceMonitor(c Component, metrics *otilmv1alpha1.MetricsSpec) *monitoringv1.ServiceMonitor {
	if metrics == nil || !metrics.Enabled {
		return nil
	}
	if metrics.ServiceMonitor == nil || !metrics.ServiceMonitor.Enabled {
		return nil
	}

	labels := make(map[string]string)
	for k, v := range c.Labels() {
		labels[k] = v
	}
	for k, v := range metrics.ServiceMonitor.Labels {
		labels[k] = v
	}

	metricsPath := defaultMetricsPath
	if metrics.Path != nil && *metrics.Path != "" {
		metricsPath = *metrics.Path
	}

	var interval monitoringv1.Duration
	if metrics.ServiceMonitor.Interval != nil {
		interval = monitoringv1.Duration(*metrics.ServiceMonitor.Interval)
	}

	portName := c.PrimaryPortName
	if portName == "" {
		portName = "http"
	}

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.ResourceName(),
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector:  metav1.LabelSelector{MatchLabels: c.SelectorLabels()},
			Endpoints: []monitoringv1.Endpoint{{Port: portName, Path: metricsPath, Interval: interval}},
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{c.Namespace},
			},
		},
	}
}
