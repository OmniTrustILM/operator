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

package capabilities

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stubMapper is a minimal meta.RESTMapper whose RESTMapping returns a preset
// mapping or error; the other methods are unused by the Detector and panic if
// called so a future change that starts using them is caught immediately.
type stubMapper struct {
	mapping *meta.RESTMapping
	err     error
	// gotGK / gotVersions capture the last RESTMapping arguments for assertions.
	gotGK       schema.GroupKind
	gotVersions []string
}

func (m *stubMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	m.gotGK = gk
	m.gotVersions = versions
	return m.mapping, m.err
}

func (m *stubMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	panic("unused")
}
func (m *stubMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	panic("unused")
}
func (m *stubMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	panic("unused")
}
func (m *stubMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	panic("unused")
}
func (m *stubMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	panic("unused")
}
func (m *stubMapper) ResourceSingularizer(string) (string, error) { panic("unused") }

func TestDetectorAvailable(t *testing.T) {
	certManager := schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}

	t.Run("present when the mapper returns a mapping", func(t *testing.T) {
		m := &stubMapper{mapping: &meta.RESTMapping{}}
		ok, err := New(m).Available(certManager, "v1")
		require.NoError(t, err)
		assert.True(t, ok, "a successful RESTMapping means the GroupKind is served")
		assert.Equal(t, certManager, m.gotGK, "the GroupKind is forwarded to the mapper")
		assert.Equal(t, []string{"v1"}, m.gotVersions, "the version is forwarded to the mapper")
	})

	t.Run("absent on NoKindMatchError -> (false, nil)", func(t *testing.T) {
		m := &stubMapper{err: &meta.NoKindMatchError{GroupKind: certManager, SearchedVersions: []string{"v1"}}}
		ok, err := New(m).Available(certManager, "v1")
		require.NoError(t, err, "a no-match error is not propagated; it means 'not installed'")
		assert.False(t, ok)
	})

	t.Run("absent on NoResourceMatchError -> (false, nil)", func(t *testing.T) {
		m := &stubMapper{err: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
			Group: "cert-manager.io", Version: "v1", Resource: "certificates",
		}}}
		ok, err := New(m).Available(certManager, "v1")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("transient error is propagated", func(t *testing.T) {
		sentinel := errors.New("discovery temporarily unavailable")
		m := &stubMapper{err: sentinel}
		ok, err := New(m).Available(certManager, "v1")
		require.Error(t, err, "a non-no-match error must propagate so the caller can retry")
		assert.ErrorIs(t, err, sentinel)
		assert.False(t, ok)
	})

	t.Run("no version supplied forwards an empty version list", func(t *testing.T) {
		m := &stubMapper{mapping: &meta.RESTMapping{}}
		ok, err := New(m).Available(certManager)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Empty(t, m.gotVersions, "with no versions the preferred mapping is requested")
	})
}
