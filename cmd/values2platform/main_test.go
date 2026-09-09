/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/OmniTrustILM/operator/pkg/convert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_EndToEnd writes a values file to a temp dir, runs the command through run(), and
// asserts the scaffolded CR is produced with secrets referenced (never inlined).
func TestRun_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
global:
  database:
    host: db.example.com
    name: ilmdb
    username: ilmuser
    password: SUPER-SECRET-PW
ingress:
  enabled: true
`), 0o600))

	var out bytes.Buffer
	require.NoError(t, run([]string{"-f", path, "-name", "prod", "-namespace", "prod-ns"}, &out))

	got := out.String()
	assert.Contains(t, got, "kind: Platform")
	assert.Contains(t, got, "name: prod")
	assert.Contains(t, got, "namespace: prod-ns")
	assert.Contains(t, got, "secretRef: "+convert.DefaultDatabaseSecretName)
	// the password must NOT appear anywhere
	assert.NotContains(t, got, "SUPER-SECRET-PW")
}

// TestRun_PositionalArg asserts the values path can be a positional argument too.
func TestRun_PositionalArg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(path, []byte("global: {}\n"), 0o600))

	var out bytes.Buffer
	require.NoError(t, run([]string{path}, &out))
	assert.Contains(t, out.String(), "kind: Platform")
}

// TestRun_MissingFile asserts a missing path is a clean error, not a panic.
func TestRun_MissingFile(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"-f", "/no/such/values.yaml"}, &out)
	require.Error(t, err)
}

// TestRun_NoArgs asserts running with no values path errors with guidance.
func TestRun_NoArgs(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no values file")
}
