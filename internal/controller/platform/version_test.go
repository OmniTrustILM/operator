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

package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// TestEffectivePlatformVersion locks the pin-on-create precedence: explicit spec.version wins;
// otherwise the pinned status.observedVersion is followed (so an operator upgrade never floats a
// running platform onto a new default); only a brand-new version-less platform resolves to ""
// (which the bundle layer maps to DefaultVersion).
func TestEffectivePlatformVersion(t *testing.T) {
	cases := []struct {
		name, spec, observed, want string
	}{
		{"explicit spec.version wins over the pin", platformVersion218, platformVersion217, platformVersion218},
		{"empty spec.version follows the pinned observedVersion", "", platformVersion217, platformVersion217},
		{"first reconcile: nothing pinned yet → empty (default resolved downstream)", "", "", ""},
		{"explicit spec.version with no pin yet", platformVersion218, "", platformVersion218},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &otilmv1alpha1.Platform{}
			p.Spec.Version = c.spec
			p.Status.ObservedVersion = c.observed
			assert.Equal(t, c.want, effectivePlatformVersion(p))
		})
	}
}

// TestIsPlatformDowngrade locks the downgrade predicate: strictly-older requested → true; same
// or newer → false; an unparseable version is never treated as a downgrade (it is caught
// separately as an unsupported version, and refusing to render on a parse quirk would be worse).
func TestIsPlatformDowngrade(t *testing.T) {
	cases := []struct {
		name, requested, running string
		want                     bool
	}{
		{"older major.minor is a downgrade", platformVersion217, platformVersion218, true},
		{"older patch is a downgrade", platformVersion218, "2.18.1", true},
		{"same version is not a downgrade", platformVersion218, platformVersion218, false},
		{"newer is not a downgrade", "2.19.0", platformVersion218, false},
		{"unparseable requested → not blocked", "not-a-version", platformVersion218, false},
		{"unparseable running → not blocked", platformVersion218, "not-a-version", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isPlatformDowngrade(c.requested, c.running))
		})
	}
}
