/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"slices"
	"time"

	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// defaultHealthPaths are the Health Interface's v2 path and then its v1 path. Older connectors
// serve only the v1 path.
var defaultHealthPaths = []string{"/v2/health", "/v1/health"}

// These defaults apply to an absent spec.healthCheck. They match the CRD's defaults for a
// present one.
const (
	defaultHealthCheckEnabled        = true
	defaultHealthCheckPeriodSeconds  = 30
	defaultHealthCheckTimeoutSeconds = 10
)

// HealthCheck is the resolved spec.healthCheck, with every default applied.
type HealthCheck struct {
	Enabled bool
	// Paths are tried in order. A health report or a missing answer ends the walk.
	Paths   []string
	Period  time.Duration
	Timeout time.Duration
}

// ResolveHealthCheck fills in the defaults that API-server defaulting gives only a present
// spec.healthCheck.
func ResolveHealthCheck(conn *otilmv1alpha1.Connector) HealthCheck {
	spec := ptr.Deref(conn.Spec.HealthCheck, otilmv1alpha1.HealthCheckSpec{})
	paths := slices.Clone(defaultHealthPaths)
	if spec.Path != "" {
		paths = []string{spec.Path}
	}
	return HealthCheck{
		Enabled: ptr.Deref(spec.Enabled, defaultHealthCheckEnabled),
		Paths:   paths,
		Period:  secondsOrDefault(spec.PeriodSeconds, defaultHealthCheckPeriodSeconds),
		Timeout: secondsOrDefault(spec.TimeoutSeconds, defaultHealthCheckTimeoutSeconds),
	}
}

func secondsOrDefault(seconds, defaultSeconds int32) time.Duration {
	if seconds == 0 {
		seconds = defaultSeconds
	}
	return time.Duration(seconds) * time.Second
}
