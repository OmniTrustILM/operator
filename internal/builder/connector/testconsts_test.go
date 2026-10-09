/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector_test

// Shared string-literal constants used across the connector builder test files. Centralizing
// these de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a
// single source of truth.
const (
	// healthcheck_test.go — the Health Interface's v2 path and its v1 path.
	testV2HealthPath = "/v2/health"
	testV1HealthPath = "/v1/health"

	// deployment_test.go — names both the claim-backed volume and the claim it mounts.
	testHSMState = "hsm-state"
)
