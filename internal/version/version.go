// Package version holds build-time version information for the operator.
package version

// Build-time version variables, set via -ldflags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)
