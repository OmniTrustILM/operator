/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package capabilities

import (
	"context"
	"errors"
	"testing"
	"time"

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

// blockingMapper is a stubMapper whose RESTMapping waits until released, as a discovery
// request does against a stalled API server.
type blockingMapper struct {
	stubMapper
	entered  chan struct{}
	released chan struct{}
}

func (m *blockingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	m.entered <- struct{}{}
	<-m.released
	return m.stubMapper.RESTMapping(gk, versions...)
}

var certManager = schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}

func TestDetectorAvailable(t *testing.T) {
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
			Group: certManager.Group, Version: "v1", Resource: "certificates",
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

func TestDetectorAvailableContext(t *testing.T) {
	t.Run("reports what Available reports", func(t *testing.T) {
		m := &stubMapper{err: &meta.NoKindMatchError{GroupKind: certManager, SearchedVersions: []string{"v1"}}}
		ok, err := New(m).AvailableContext(context.Background(), certManager, "v1")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, []string{"v1"}, m.gotVersions, "the version is forwarded to the mapper")
	})

	t.Run("returns the context's error when it ends during a lookup", func(t *testing.T) {
		m := &blockingMapper{entered: make(chan struct{}, 1), released: make(chan struct{})}
		defer close(m.released)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := New(m).AvailableContext(ctx, certManager, "v1")
			done <- err
		}()
		<-m.entered
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("AvailableContext waited for the mapper after its context ended")
		}
	})

	t.Run("does not ask the mapper once the context has ended", func(t *testing.T) {
		m := &stubMapper{mapping: &meta.RESTMapping{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ok, err := New(m).AvailableContext(ctx, certManager, "v1")
		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, ok)
		assert.Empty(t, m.gotGK.Kind, "no discovery is started for an ended context")
	})
}
