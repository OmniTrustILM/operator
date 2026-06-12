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
