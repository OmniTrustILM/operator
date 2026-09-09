/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package monitoring provides Prometheus metrics and Kubernetes event reason constants.
package monitoring

// Event reason constants for Kubernetes events emitted by the ILM operator.
const (
	ReasonDeployed           = "Deployed"
	ReasonUpdated            = "Updated"
	ReasonDeleting           = "Deleting"
	ReasonDegraded           = "Degraded"
	ReasonRecovered          = "Recovered"
	ReasonRegistered         = "Registered"
	ReasonRegistrationFailed = "RegistrationFailed"
	ReasonConfigChanged      = "ConfigChanged"
	ReasonMissingSecret      = "MissingSecret"
	ReasonMissingConfigMap   = "MissingConfigMap"
	ReasonTokenExpired       = "ConfigTokenExpired" //nolint:gosec // G101: event reason name, not a credential value
	ReasonMissingTokenKey    = "MissingTokenKey"    //nolint:gosec // G101: event reason name, not a credential value
	// ReasonServiceMonitorMissing reports the ServiceMonitor capability gate: the
	// monitoring.coreos.com CRD is not served on this cluster.
	ReasonServiceMonitorMissing = "ServiceMonitorCRDNotInstalled"
)
