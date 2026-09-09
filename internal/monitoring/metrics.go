/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

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

	// The connectors/proxies managed counts are NOT imperative gauges: they are
	// computed from the informer cache at scrape time by ManagedCountCollector
	// (managed_collector.go), so they cannot drift on reconcile retries or
	// operator restarts.
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconciliationsTotal,
		ReconciliationDurationSeconds,
	)
}
