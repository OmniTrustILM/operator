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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// TestTeardownPlatformVersion locks the DELETION precedence, deliberately the reverse of
// effectivePlatformVersion: the running reality (status.observedVersion) wins over the
// requested spec.version, so teardown renders the objects that actually exist.
func TestTeardownPlatformVersion(t *testing.T) {
	cases := []struct {
		name, spec, observed, want string
	}{
		{"blocked upgrade: the running pin wins over the requested version", platformVersion219, platformVersion218, platformVersion218},
		{"empty spec.version follows the pin, not the operator default", "", platformVersion219, platformVersion219},
		{"no pin yet: fall back to the requested spec.version", platformVersion218, "", platformVersion218},
		{"nothing set: empty (the bundle layer resolves the default)", "", "", ""},
		{"agreeing spec and pin", platformVersion218, platformVersion218, platformVersion218},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &otilmv1alpha1.Platform{}
			p.Spec.Version = c.spec
			p.Status.ObservedVersion = c.observed
			assert.Equal(t, c.want, teardownPlatformVersion(p))
		})
	}
}

// TestTeardownRenderPlatforms locks the version SET teardown renders against. The running
// version is always first; the requested version is added ONLY when an upgrade is genuinely in
// flight (set, resolvable, and different), because a released upgrade applies the new version's
// managed objects before status.observedVersion is persisted — a crash in between would
// otherwise orphan them (see handleDeletion).
func TestTeardownRenderPlatforms(t *testing.T) {
	cases := []struct {
		name, spec, observed string
		want                 []string
	}{
		{
			name: "upgrade in flight: both the running and the requested version render",
			spec: platformVersion218, observed: platformVersion217,
			want: []string{platformVersion217, platformVersion218},
		},
		{
			name: "agreeing spec and pin: one render",
			spec: platformVersion218, observed: platformVersion218,
			want: []string{platformVersion218},
		},
		{
			name: "empty spec.version: the pin alone (nothing else was ever applied)",
			spec: "", observed: platformVersion218,
			want: []string{platformVersion218},
		},
		{
			name: "unresolvable requested version rendered nothing, so it is skipped",
			spec: "9.9.9", observed: platformVersion218,
			want: []string{platformVersion218},
		},
		{
			name: "no pin yet: the requested version alone",
			spec: platformVersion218, observed: "",
			want: []string{platformVersion218},
		},
		{
			name: "nothing set: a single empty render (the bundle layer defaults it)",
			spec: "", observed: "",
			want: []string{""},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &otilmv1alpha1.Platform{}
			p.Spec.Version = c.spec
			p.Status.ObservedVersion = c.observed

			got := teardownRenderPlatforms(p)
			versions := make([]string, 0, len(got))
			for _, rp := range got {
				versions = append(versions, rp.Spec.Version)
			}
			assert.Equal(t, c.want, versions)
			assert.Equal(t, c.spec, p.Spec.Version,
				"the render pin must live on deep copies only — never on the stored spec")
		})
	}
}

// TestMergeManagedObjects locks the dedupe the teardown union relies on: objects the two version
// renders share are deleted once, order is primary-first, and objects that differ in GVK,
// namespace or name are all kept (dropping one would orphan it).
func TestMergeManagedObjects(t *testing.T) {
	obj := func(kind, ns, name string) client.Object {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: kind})
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	names := func(objs []client.Object) []string {
		out := make([]string, 0, len(objs))
		for _, o := range objs {
			out = append(out, o.GetObjectKind().GroupVersionKind().Kind+"/"+o.GetNamespace()+"/"+o.GetName())
		}
		return out
	}

	primary := []client.Object{obj("Vhost", "ns", "vhost"), obj("Exchange", "ns", "czertainly")}
	extra := []client.Object{
		obj("Vhost", "ns", "vhost"),               // identical → deduped
		obj("Exchange", "ns", "czertainly-proxy"), // new name → kept
		obj("Queue", "ns", "czertainly"),          // same name, other kind → kept
		obj("Exchange", "other-ns", "czertainly"), // same name, other namespace → kept
	}

	assert.Equal(t, []string{
		"Vhost/ns/vhost",
		"Exchange/ns/czertainly",
		"Exchange/ns/czertainly-proxy",
		"Queue/ns/czertainly",
		"Exchange/other-ns/czertainly",
	}, names(mergeManagedObjects(primary, extra)))

	assert.Empty(t, mergeManagedObjects(nil, nil), "no renders, nothing to delete")
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
		{"newer is not a downgrade", platformVersion219, platformVersion218, false},
		{"unparseable requested → not blocked", "not-a-version", platformVersion218, false},
		{"unparseable running → not blocked", platformVersion218, "not-a-version", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isPlatformDowngrade(c.requested, c.running))
		})
	}
}
