package monitoring

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconciliationsTotal counts the total number of reconciliations performed.
	ReconciliationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ilm_operator_reconciliations_total",
			Help: "Total number of reconciliations performed by the ILM operator.",
		},
		[]string{"connector", "namespace", "result"},
	)

	// ReconciliationDurationSeconds tracks the duration of reconciliation loops.
	ReconciliationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ilm_operator_reconciliation_duration_seconds",
			Help:    "Duration in seconds of reconciliation loops performed by the ILM operator.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"connector", "namespace"},
	)

	// ConnectorsManaged tracks the current number of connectors managed by the operator.
	ConnectorsManaged = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "ilm_operator_connectors_managed",
			Help: "Current number of Connector resources managed by the ILM operator.",
		},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconciliationsTotal,
		ReconciliationDurationSeconds,
		ConnectorsManaged,
	)
}
