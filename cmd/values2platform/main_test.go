/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_EndToEnd writes a values file to a temp dir, runs the CLI through run(), and
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
	assert.Contains(t, got, "secretRef: "+dbSecretName)
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
