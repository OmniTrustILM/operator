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

// Package projectcheck guards the kubebuilder PROJECT scaffold metadata. The
// PROJECT file is generated metadata, but a stale resources list (missing a
// kind the repo actually ships) silently breaks `kubebuilder edit`/`create`
// scaffolding and downstream OLM bundle generation, so we pin it with a test.
package projectcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// projectFile is the minimal shape of the kubebuilder PROJECT file we assert on.
type projectFile struct {
	Domain    string `json:"domain"`
	Resources []struct {
		Kind    string `json:"kind"`
		Domain  string `json:"domain"`
		Path    string `json:"path"`
		Version string `json:"version"`
		Controller bool `json:"controller"`
	} `json:"resources"`
}

func loadProject(t *testing.T) projectFile {
	t.Helper()
	// projectcheck lives at internal/projectcheck; PROJECT is two levels up.
	raw, err := os.ReadFile(filepath.Join("..", "..", "PROJECT"))
	require.NoError(t, err, "read PROJECT")
	var p projectFile
	require.NoError(t, yaml.Unmarshal(raw, &p), "unmarshal PROJECT")
	return p
}

func TestProjectRegistersAllShippedKinds(t *testing.T) {
	p := loadProject(t)
	assert.Equal(t, "otilm.com", p.Domain)

	got := map[string]struct {
		domain, path, version string
		controller            bool
	}{}
	for _, r := range p.Resources {
		got[r.Kind] = struct {
			domain, path, version string
			controller            bool
		}{r.Domain, r.Path, r.Version, r.Controller}
	}

	const wantPath = "github.com/OmniTrustILM/operator/api/v1alpha1"
	for _, kind := range []string{"Connector", "Platform", "Proxy"} {
		r, ok := got[kind]
		require.Truef(t, ok, "PROJECT must register kind %q", kind)
		assert.Equalf(t, "otilm.com", r.domain, "%s domain", kind)
		assert.Equalf(t, wantPath, r.path, "%s api path", kind)
		assert.Equalf(t, "v1alpha1", r.version, "%s version", kind)
		assert.Truef(t, r.controller, "%s must be controller:true", kind)
	}
}
