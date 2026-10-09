/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsRelease locks which builds count as releases: a build from a release tag carries that
// tag's semantic version, while a build from a branch carries the branch name and a local build
// keeps the "dev" default.
func TestIsRelease(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"main", false},
		{"dev", false},
		{"1.2.0", true},
		{"v1.2.0", true},
		{"1.3.0-rc.1", true},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			original := Version
			Version = c.version
			t.Cleanup(func() { Version = original })

			assert.Equal(t, c.want, IsRelease())
		})
	}
}
