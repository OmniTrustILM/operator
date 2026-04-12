package builder

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultMetricsPath = "/v1/metrics"

// BuildServiceMonitor constructs a Prometheus ServiceMonitor for the given Connector.
// Returns nil if metrics or ServiceMonitor are not configured and enabled.
func BuildServiceMonitor(conn *otilmv1alpha1.Connector) *monitoringv1.ServiceMonitor {
	if conn.Spec.Metrics == nil {
		return nil
	}
	if conn.Spec.Metrics.ServiceMonitor == nil {
		return nil
	}
	if !conn.Spec.Metrics.ServiceMonitor.Enabled {
		return nil
	}

	// Merge Labels(conn) with spec.metrics.serviceMonitor.labels
	labels := make(map[string]string)
	for k, v := range Labels(conn) {
		labels[k] = v
	}
	for k, v := range conn.Spec.Metrics.ServiceMonitor.Labels {
		labels[k] = v
	}

	// Resolve metrics path with default
	metricsPath := defaultMetricsPath
	if conn.Spec.Metrics.Path != nil && *conn.Spec.Metrics.Path != "" {
		metricsPath = *conn.Spec.Metrics.Path
	}

	// Resolve scrape interval
	var interval monitoringv1.Duration
	if conn.Spec.Metrics.ServiceMonitor.Interval != nil {
		interval = monitoringv1.Duration(*conn.Spec.Metrics.ServiceMonitor.Interval)
	}

	endpoint := monitoringv1.Endpoint{
		Port:     "http",
		Path:     metricsPath,
		Interval: interval,
	}

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildResourceName(conn),
			Namespace: conn.Namespace,
			Labels:    labels,
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: SelectorLabels(conn),
			},
			Endpoints: []monitoringv1.Endpoint{endpoint},
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{conn.Namespace},
			},
		},
	}
}
