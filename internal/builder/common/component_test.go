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

package common

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/bom"
	"github.com/stretchr/testify/assert"
)

func TestResolveImagePrefersComponentOverShared(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{Registry: "shared.io", Repository: "ilm", PullPolicy: "IfNotPresent"}
	comp := otilmv1alpha1.ImageSpec{Name: "core", Repository: "team", Tag: "9.9.9"}
	ref, policy := ResolveImage(bom.Lookup, "core", shared, comp)
	assert.Equal(t, "shared.io/team/core:9.9.9", ref)
	assert.Equal(t, "IfNotPresent", string(policy))
}

func TestResolveImageFallsBackToBOMTag(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{Registry: "hub.omnitrustregistry.com", Repository: "ilm"}
	ref, _ := ResolveImage(bom.Lookup, "core", shared, otilmv1alpha1.ImageSpec{Name: "core"})
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.18.0", ref)
}

func TestResolveImageConnectorStyleRepositoryTag(t *testing.T) {
	// No registry/name (Connector-style): the non-empty segments join to repository:tag.
	// nil bundleLookup: the version-agnostic Connector has no BOM bundle to fall back to.
	ref, policy := ResolveImage(nil, "", otilmv1alpha1.ImageSpec{},
		otilmv1alpha1.ImageSpec{Repository: "harbor.example.com/ilm/connector", Tag: "1.2.3"})
	assert.Equal(t, "harbor.example.com/ilm/connector:1.2.3", ref)
	assert.Equal(t, "IfNotPresent", string(policy)) // default when unset
}

func TestResolveImageSharedFillsWhatComponentOmits(t *testing.T) {
	// comp sets repository; shared supplies the tag → per-field merge.
	shared := otilmv1alpha1.ImageSpec{Tag: "1.0"}
	ref, _ := ResolveImage(nil, "", shared, otilmv1alpha1.ImageSpec{Repository: "r"})
	assert.Equal(t, "r:1.0", ref)
}

func TestResolveImageBOMFillsName(t *testing.T) {
	// Known component, no name/tag on comp/shared → both filled from the bundle.
	ref, _ := ResolveImage(bom.Lookup, "core", otilmv1alpha1.ImageSpec{Repository: "ilm"}, otilmv1alpha1.ImageSpec{})
	assert.Equal(t, "ilm/core:2.18.0", ref)
}

// TestResolveImageNilLookupSkipsBundleFallback proves a nil bundleLookup (the
// version-agnostic Connector path) never fills a name/tag from any bundle, even for a
// component name that the default bundle WOULD resolve.
func TestResolveImageNilLookupSkipsBundleFallback(t *testing.T) {
	ref, _ := ResolveImage(nil, "core", otilmv1alpha1.ImageSpec{Repository: "ilm"}, otilmv1alpha1.ImageSpec{})
	assert.Equal(t, "ilm", ref) // no name/tag filled — only the repository segment remains
}

func TestResolveImagePullPolicySharedWhenComponentEmpty(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{PullPolicy: "Always"}
	_, policy := ResolveImage(nil, "", shared, otilmv1alpha1.ImageSpec{Repository: "r", Tag: "1"})
	assert.Equal(t, "Always", string(policy))
}

func TestResolveImageDegenerateInputs(t *testing.T) {
	tests := []struct {
		name      string
		component string
		shared    otilmv1alpha1.ImageSpec
		comp      otilmv1alpha1.ImageSpec
		wantRef   string
	}{
		{"all empty", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{}, ""},
		{"only registry", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Registry: "reg.io"}, "reg.io"},
		{"repository no tag", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Repository: "repo"}, "repo"},
		{"tag only, no segments", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Tag: "1.2.3"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, _ := ResolveImage(nil, tc.component, tc.shared, tc.comp)
			assert.Equal(t, tc.wantRef, ref)
		})
	}
}
