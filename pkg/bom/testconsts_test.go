/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package bom

// Shared string-literal constants used across the bom test files. Centralizing these
// de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a single
// source of truth.
const (
	// testBundleContext is the assertion context every per-bundle loop adds, so a failure names
	// the version it was looking at.
	testBundleContext = "bundle %s"
)
