/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package rabbitmq

// Shared string-literal constants used across the rabbitmq client test files. Centralizing
// these de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a
// single source of truth.
const (
	// testBrokerURL is a management-API base URL for the cases that never dial: constructor
	// defaults and interface conformance.
	testBrokerURL = "http://broker.example:15672"
	// testHTTPScheme is the scheme an httptest server's URL carries, trimmed off to recover the
	// bare host:port a broker error must never mention.
	testHTTPScheme = "http://"
)
