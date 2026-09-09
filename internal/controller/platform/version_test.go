/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
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

// TestTeardownRenderPlatforms locks the version SET teardown renders against, for an
// UNPINNED platform: one copy per bom.AllVersions() (every bundle this operator ships,
// released and preview — reclaiming whichever bundle's managed objects were actually
// applied, however a partially applied upgrade or downgrade left them), PLUS one
// legacy-scope copy that pins the RUNNING version (teardownPlatformVersion) but forces
// spec.messaging.virtualHost to bom.LegacyUnscopedVirtualHost — reproducing the UNSCOPED
// object names an operator predating vhost-scoped naming rendered, so those are reclaimed
// too (see handleDeletion).
func TestTeardownRenderPlatforms(t *testing.T) {
	p := &otilmv1alpha1.Platform{}
	p.Spec.Version = platformVersion219
	p.Status.ObservedVersion = platformVersion218

	got := teardownRenderPlatforms(p)

	allVersions := bom.AllVersions()
	require.Len(t, got, len(allVersions)+1,
		"one render per bom.AllVersions() bundle, plus the legacy-scope variant")

	for i, v := range allVersions {
		assert.Equal(t, v, got[i].Spec.Version, "render %d must pin bundle version %s", i, v)
		assert.Empty(t, got[i].Spec.Messaging.VirtualHost,
			"a bundle-version render keeps the platform's own (unpinned) vhost")
	}

	legacy := got[len(got)-1]
	assert.Equal(t, platformVersion218, legacy.Spec.Version,
		"the legacy-scope variant pins the RUNNING version (status.observedVersion wins), not the requested one")
	assert.Equal(t, bom.LegacyUnscopedVirtualHost, legacy.Spec.Messaging.VirtualHost,
		"the legacy-scope variant forces the pre-scoping vhost so it renders unscoped names")

	assert.Equal(t, platformVersion219, p.Spec.Version,
		"the render pin must live on deep copies only — never on the stored spec")
	assert.Empty(t, p.Spec.Messaging.VirtualHost,
		"the legacy-vhost pin must live on a deep copy only — never on the stored spec")
}

// TestTeardownRenderPlatformsSkipsLegacyCopyForPinnedVhost proves a user-pinned
// spec.messaging.virtualHost gets NO extra legacy-scope render: its own vhost already renders
// unscoped names on every bundle-version copy above (a user-pinned vhost is unscoped on
// every bundle, unconditionally — see topologyScope in the builder package), so a synthetic
// legacy-vhost copy would only repeat a set mergeManagedObjects already dedupes away.
func TestTeardownRenderPlatformsSkipsLegacyCopyForPinnedVhost(t *testing.T) {
	const customVhost = "custom-vhost"

	p := &otilmv1alpha1.Platform{}
	p.Spec.Version = platformVersion219
	p.Status.ObservedVersion = platformVersion218
	p.Spec.Messaging.VirtualHost = customVhost

	got := teardownRenderPlatforms(p)

	allVersions := bom.AllVersions()
	require.Len(t, got, len(allVersions), "a pinned vhost gets no extra legacy-scope render")
	for i, v := range allVersions {
		assert.Equal(t, v, got[i].Spec.Version, "render %d must pin bundle version %s", i, v)
		assert.Equal(t, customVhost, got[i].Spec.Messaging.VirtualHost,
			"a bundle-version render keeps the platform's own configured vhost")
	}

	assert.Equal(t, platformVersion219, p.Spec.Version,
		"the render pin must live on deep copies only — never on the stored spec")
}

// TestTeardownRenderPlatformsRetainDeletesNothing pins the deletion-safety invariant across
// the larger rendered set: however many platform copies teardownRenderPlatforms renders,
// deletionPolicy=Retain must still leave every managed object in the cluster untouched. It
// exercises an UNPINNED platform running the 2.19.0 bundle — the one case where the
// legacy-scope rescue copy still adds names the per-version copies do not already cover,
// since 2.19.0's own default vhost ("/") diverges from the legacy one (see
// teardownRenderPlatforms) — through the real deletion path, not just the render count.
func TestTeardownRenderPlatformsRetainDeletesNothing(t *testing.T) {
	s := managedMQScheme(t)

	p := managedMQPlatformCR()
	p.Spec.Version = platformVersion219
	p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyRetain
	p.Status.ObservedVersion = platformVersion219

	legacy := managedMQPlatformCR()
	legacy.Spec.Version = platformVersion219
	legacy.Spec.Messaging.VirtualHost = bom.LegacyUnscopedVirtualHost

	current := managedMQPlatformCR()
	current.Spec.Version = platformVersion219

	legacyObjs := platformbuilder.ResolveManagedMessaging(legacy)
	currentObjs := platformbuilder.ResolveManagedMessaging(current)
	require.NotEmpty(t, legacyObjs)
	require.NotEmpty(t, currentObjs)

	seed := append([]client.Object{p}, seedTopologyUnion(legacyObjs, currentObjs)...)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(32)}

	require.NoError(t, r.handleDeletion(context.Background(), p))

	assert.Equal(t, len(legacyObjs), countTopologyObjects(t, r, legacy),
		"Retain must leave the legacy-scoped rescue-copy topology intact")
	assert.Equal(t, len(currentObjs), countTopologyObjects(t, r, current),
		"Retain must leave the current (unpinned, vhost-scoped) topology intact")
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
