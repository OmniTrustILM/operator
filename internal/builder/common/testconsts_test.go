/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

// Shared string-literal constants used across the common builder test files. Centralizing these
// de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a single
// source of truth.
const (
	// hpa_test.go — the scaleTargetRef API version every HPA the builder produces carries,
	// whichever workload kind it targets.
	testAppsV1APIVersion = "apps/v1"
)
