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
