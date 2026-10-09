/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package version holds build-time version information for the operator.
package version

import "github.com/Masterminds/semver/v3"

// Build-time version variables, set via -ldflags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// IsRelease reports whether this binary is a release build. A release is built from a tag, so its
// Version is a semantic version; a build from a branch carries the branch name (for example
// "main"), and a local build keeps the "dev" default.
func IsRelease() bool {
	_, err := semver.NewVersion(Version)
	return err == nil
}
