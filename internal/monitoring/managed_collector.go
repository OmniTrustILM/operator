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
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// collectTimeout bounds one scrape's cache Lists; the informer cache serves from
// memory, so this only guards an unsynced/stopped cache.
const collectTimeout = 5 * time.Second

var (
	connectorsManagedDesc = prometheus.NewDesc(
		"ilm_operator_connectors_managed",
		"Current number of Connector resources managed by the ILM operator.",
		nil, nil,
	)
	proxiesManagedDesc = prometheus.NewDesc(
		"ilm_operator_proxies_managed",
		"Current number of Proxy resources managed by the ILM operator.",
		nil, nil,
	)
)

// ManagedCountCollector reports the number of Connector and Proxy CRs the operator
// manages by Listing them from the informer cache at scrape time. Computing the
// counts at scrape time keeps them correct by construction — the previous imperative
// Inc/Dec gauges double-counted when a reconcile errored between the increment and
// the status persist, read zero after an operator restart (existing CRs never
// re-counted), and went negative once those CRs were deleted.
type ManagedCountCollector struct {
	reader client.Reader
}

// NewManagedCountCollector returns a collector backed by the given cache reader
// (typically mgr.GetClient()).
func NewManagedCountCollector(reader client.Reader) *ManagedCountCollector {
	return &ManagedCountCollector{reader: reader}
}

// RegisterManagedCountCollector registers the collector with the controller-runtime
// metrics registry. Call once from main after the manager is constructed.
func RegisterManagedCountCollector(reader client.Reader) error {
	return ctrlmetrics.Registry.Register(NewManagedCountCollector(reader))
}

// Describe implements prometheus.Collector.
func (c *ManagedCountCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- connectorsManagedDesc
	ch <- proxiesManagedDesc
}

// Collect implements prometheus.Collector. A List error (e.g. the cache has not
// synced yet during startup) omits that metric from the scrape rather than
// reporting a wrong value.
func (c *ManagedCountCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	var connectors otilmv1alpha1.ConnectorList
	if err := c.reader.List(ctx, &connectors); err == nil {
		ch <- prometheus.MustNewConstMetric(connectorsManagedDesc, prometheus.GaugeValue, float64(len(connectors.Items)))
	}

	var proxies otilmv1alpha1.ProxyList
	if err := c.reader.List(ctx, &proxies); err == nil {
		ch <- prometheus.MustNewConstMetric(proxiesManagedDesc, prometheus.GaugeValue, float64(len(proxies.Items)))
	}
}
