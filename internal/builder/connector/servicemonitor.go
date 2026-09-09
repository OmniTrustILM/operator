/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildServiceMonitor constructs a Prometheus ServiceMonitor for the given Connector
// via the shared Component builder (same /v1/metrics default path, "http" port, and
// per-namespace selector as the pre-Component rendering). Returns nil if metrics or
// the ServiceMonitor are not configured and enabled.
func BuildServiceMonitor(conn *otilmv1alpha1.Connector) *monitoringv1.ServiceMonitor {
	return common.BuildServiceMonitor(component(conn, ""), conn.Spec.Metrics)
}
