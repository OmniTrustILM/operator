/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

// Shared string-literal constants used across the connector controller test files. Centralizing
// these de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a
// single source of truth.
const (
	// validation_cel_healthcheck_test.go — the Connector each healthCheck admission spec submits.
	testCELHealthCheckName = "cel-healthcheck"

	// validation_cel_volumes_test.go — the mount path of the volume each volume admission spec
	// submits.
	testStateMountPath = "/var/lib/state"
)
