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
	"context"
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Deletion safety ---------------------------------------------------------

// seedManagedMessagingForDeletion returns a fake client seeded with a Platform (the given
// policy) and its full managed RabbitMQ topology, plus a FakeRecorder, for the deletion
// tests. Seeding EVERY rendered object lets the Delete path exercise the whole teardown.
func seedManagedMessagingForDeletion(t *testing.T, policy otilmv1alpha1.PlatformDeletionPolicy) (*Reconciler, *otilmv1alpha1.Platform, *record.FakeRecorder) {
	t.Helper()
	s := managedMQScheme(t)
	p := managedMQPlatformCR()
	p.Spec.DeletionPolicy = policy

	seed := []client.Object{p}
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		u := obj.(*unstructured.Unstructured)
		seed = append(seed, u.DeepCopy())
	}

	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: rec}, p, rec
}

// countTopologyObjects lists how many of the platform's rendered managed-messaging objects
// still exist in the fake client (by GETting each by GVK + name).
func countTopologyObjects(t *testing.T, r *Reconciler, p *otilmv1alpha1.Platform) int {
	t.Helper()
	n := 0
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		u := obj.(*unstructured.Unstructured)
		var got unstructured.Unstructured
		got.SetGroupVersionKind(u.GroupVersionKind())
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(u), &got); err == nil {
			n++
		}
	}
	return n
}

func TestHandleDeletionManagedMessagingRetainLeavesCluster(t *testing.T) {
	r, p, rec := seedManagedMessagingForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyRetain)
	total := len(platformbuilder.ResolveManagedMessaging(p))

	require.NoError(t, r.handleDeletion(context.Background(), p), "deletion must never block")

	// Every managed object MUST still exist (Retain protects the broker + data).
	assert.Equal(t, total, countTopologyObjects(t, r, p), "Retain must leave the whole managed topology intact")

	// A Warning Event names the retained broker (object name only — no coordinate).
	e := drainEvent(rec)
	assert.Contains(t, e, "Warning")
	assert.Contains(t, e, "RetainedMessaging")
	assert.Contains(t, e, messagingSecretRef)
}

func TestHandleDeletionManagedMessagingDefaultPolicyRetains(t *testing.T) {
	// Empty policy defaults to Retain — the cluster is left intact.
	r, p, _ := seedManagedMessagingForDeletion(t, "")
	total := len(platformbuilder.ResolveManagedMessaging(p))
	require.NoError(t, r.handleDeletion(context.Background(), p))
	assert.Equal(t, total, countTopologyObjects(t, r, p), "the default (empty) policy is Retain — the topology survives")
}

func TestHandleDeletionManagedMessagingDeleteRemovesAll(t *testing.T) {
	r, p, rec := seedManagedMessagingForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyDelete)

	require.NoError(t, r.handleDeletion(context.Background(), p))

	// The whole topology MUST be gone (Delete reclaims it; the Cluster Operator GCs PVCs).
	assert.Equal(t, 0, countTopologyObjects(t, r, p), "Delete must reclaim the whole managed topology")

	// Specifically the RabbitmqCluster is gone.
	var cluster unstructured.Unstructured
	cluster.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: messagingSecretRef}, &cluster)
	assert.True(t, apierrors.IsNotFound(err), "Delete must reclaim the RabbitmqCluster")

	e := drainEvent(rec)
	assert.Contains(t, e, "DeletedMessaging")
	assert.Contains(t, e, messagingSecretRef)
}

// topologyObjectNames returns the metadata.names of a rendered managed-messaging set, for
// asserting WHICH version's topology a render produced (2.19.0 renamed the exchanges).
func topologyObjectNames(objs []client.Object) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

// seedTopologyUnion returns the DEDUPLICATED union of several rendered topology sets as fake-
// client seed objects (the fake client rejects a duplicate name, and two version renders share
// most of a topology). It models a cluster mid-upgrade, holding both versions' objects.
func seedTopologyUnion(sets ...[]client.Object) []client.Object {
	var out []client.Object
	seen := make(map[string]struct{})
	for _, set := range sets {
		for _, obj := range set {
			u := obj.(*unstructured.Unstructured)
			key := u.GroupVersionKind().String() + "/" + u.GetNamespace() + "/" + u.GetName()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, u.DeepCopy())
		}
	}
	return out
}

// namesContaining filters names down to those containing sub.
func namesContaining(names []string, sub string) []string {
	var out []string
	for _, n := range names {
		if strings.Contains(n, sub) {
			out = append(out, n)
		}
	}
	return out
}

// TestHandleDeletionRendersTeardownFromRunningVersion is the version-mismatch deletion-safety
// guard. A platform whose requested spec.version was REFUSED by the version guards (here the
// 2.19.0 preview, blocked by PreviewVersionUpgradeBlocked) keeps running the version pinned on
// status.observedVersion — and 2.19.0 RENAMED the messaging topology (exchange czertainly →
// ilm). Teardown must therefore render from the RUNNING version: rendering from the raw
// spec.version would try to delete 2.19.0-named objects that never existed and ORPHAN the live
// 2.18.0 topology under deletionPolicy=Delete.
func TestHandleDeletionRendersTeardownFromRunningVersion(t *testing.T) {
	s := managedMQScheme(t)

	// The LIVE topology: what the platform actually runs (the pinned 2.18.0 bundle).
	running := managedMQPlatformCR()
	running.Spec.Version = platformVersion218
	live := platformbuilder.ResolveManagedMessaging(running)
	require.NotEmpty(t, live)

	// Precondition that gives this test teeth: the two bundles render DIFFERENT object names —
	// the running 2.18.0 topology carries czertainly-named exchanges, the requested 2.19.0 one
	// does not.
	requested := managedMQPlatformCR()
	requested.Spec.Version = platformVersion219
	liveNames := topologyObjectNames(live)
	requestedNames := topologyObjectNames(platformbuilder.ResolveManagedMessaging(requested))
	require.NotEmpty(t, namesContaining(liveNames, "exchange-czertainly"),
		"precondition: the 2.18.0 topology declares czertainly-named exchange CRs")
	require.Empty(t, namesContaining(requestedNames, "exchange-czertainly"),
		"precondition: the 2.19.0 topology renamed the exchanges (so a spec-version render misses the live CRs)")

	// The CR as the API server holds it while being deleted: an explicit spec.version the
	// preview guard refused, with status.observedVersion still on the running version.
	p := managedMQPlatformCR()
	p.Spec.Version = platformVersion219
	p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyDelete
	p.Status.ObservedVersion = platformVersion218

	seed := []client.Object{p}
	for _, obj := range live {
		seed = append(seed, obj.(*unstructured.Unstructured).DeepCopy())
	}
	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	require.NoError(t, r.handleDeletion(context.Background(), p))

	// Every LIVE (2.18.0) object is reclaimed — nothing is orphaned.
	assert.Equal(t, 0, countTopologyObjects(t, r, running),
		"teardown must reclaim the RUNNING version's topology, not the requested version's")

	// The pin is render-only: the object handleFinalizer then Updates keeps the user's spec.
	assert.Equal(t, platformVersion219, p.Spec.Version,
		"the teardown version pin must never mutate the stored spec")
}

// TestHandleDeletionRendersTeardownForPartiallyAppliedUpgrade covers the OTHER half of the
// version-mismatch deletion-safety guard: a RELEASED upgrade (2.17.0 → 2.18.0), which no
// version guard blocks.
//
// The reconciler APPLIES the requested version's managed objects BEFORE it persists
// status.observedVersion, so a failure in between (or a losing status write) leaves a platform
// whose status still reads 2.17.0 while 2.18.0-only topology objects — the extra broker users,
// the czertainly-proxy exchange, the time-quality queues — are already live. Rendering teardown
// from the observed version ALONE would then orphan every one of them: managed kinds are
// prune-excluded, so nothing else ever reclaims them. Teardown therefore renders the
// deduplicated UNION of the observed and requested versions.
func TestHandleDeletionRendersTeardownForPartiallyAppliedUpgrade(t *testing.T) {
	s := managedMQScheme(t)

	observed := managedMQPlatformCR()
	observed.Spec.Version = platformVersion217
	requested := managedMQPlatformCR()
	requested.Spec.Version = platformVersion218

	observedObjs := platformbuilder.ResolveManagedMessaging(observed)
	requestedObjs := platformbuilder.ResolveManagedMessaging(requested)
	require.NotEmpty(t, observedObjs)
	require.NotEmpty(t, requestedObjs)

	// Precondition that gives this test teeth: 2.18.0 declares topology objects 2.17.0 never
	// had (the extra broker users and the proxy exchange), so an observed-version-only render
	// would leave them behind.
	observedNames := topologyObjectNames(observedObjs)
	requestedNames := topologyObjectNames(requestedObjs)
	require.Empty(t, namesContaining(observedNames, "exchange-czertainly-proxy"),
		"precondition: 2.17.0 declares no proxy exchange")
	require.NotEmpty(t, namesContaining(requestedNames, "exchange-czertainly-proxy"),
		"precondition: 2.18.0 adds the proxy exchange")
	require.Empty(t, namesContaining(observedNames, "messaging-provisioner"),
		"precondition: 2.17.0 declares a single broker user")
	require.NotEmpty(t, namesContaining(requestedNames, "messaging-provisioner"),
		"precondition: 2.18.0 splits the broker users")

	// The cluster mid-upgrade: BOTH topologies live (the 2.18.0 apply already landed), while
	// status.observedVersion still reads 2.17.0 because the status write never happened.
	p := managedMQPlatformCR()
	p.Spec.Version = platformVersion218
	p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyDelete
	p.Status.ObservedVersion = platformVersion217

	seed := append([]client.Object{p}, seedTopologyUnion(observedObjs, requestedObjs)...)
	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	require.NoError(t, r.handleDeletion(context.Background(), p))

	// BOTH bundles' topologies are reclaimed — the partially applied upgrade orphans nothing.
	assert.Equal(t, 0, countTopologyObjects(t, r, observed),
		"teardown must reclaim the OBSERVED version's topology")
	assert.Equal(t, 0, countTopologyObjects(t, r, requested),
		"teardown must reclaim the partially applied REQUESTED version's topology too")

	// One Event per component, not one per rendered version.
	e := drainEvent(rec)
	assert.Contains(t, e, "DeletedMessaging")

	// The union render stays render-only: the stored spec is untouched.
	assert.Equal(t, platformVersion218, p.Spec.Version,
		"the teardown version pin must never mutate the stored spec")
}

// TestHandleDeletionRetainIgnoresTheUnionRender pins the Retain contract against the union
// render: rendering MORE versions must still delete NOTHING under deletionPolicy=Retain.
func TestHandleDeletionRetainIgnoresTheUnionRender(t *testing.T) {
	s := managedMQScheme(t)

	observed := managedMQPlatformCR()
	observed.Spec.Version = platformVersion217
	requested := managedMQPlatformCR()
	requested.Spec.Version = platformVersion218

	p := managedMQPlatformCR()
	p.Spec.Version = platformVersion218
	p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyRetain
	p.Status.ObservedVersion = platformVersion217

	observedObjs := platformbuilder.ResolveManagedMessaging(observed)
	requestedObjs := platformbuilder.ResolveManagedMessaging(requested)
	seed := append([]client.Object{p}, seedTopologyUnion(observedObjs, requestedObjs)...)
	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	require.NoError(t, r.handleDeletion(context.Background(), p))

	assert.Equal(t, len(observedObjs), countTopologyObjects(t, r, observed),
		"Retain must leave the observed version's topology intact")
	assert.Equal(t, len(requestedObjs), countTopologyObjects(t, r, requested),
		"Retain must leave the partially applied version's topology intact")
}

// TestHandleDeletionReclaimsLegacyScopedTopologyFor2190 closes the residual teardown hazard
// left once a user-pinned vhost is unscoped unconditionally (topologyScope): an UNPINNED
// platform still has one, on the 2.19.0 bundle, whose OWN default vhost ("/") diverges from
// the legacy one. An operator predating vhost-scoped naming rendered UNSCOPED object names
// regardless of vhost, but THIS operator renders SCOPED names for an unpinned 2.19.0 platform
// — so unless teardown ALSO reclaims the legacy-scoped names, the objects it actually created
// (while still running that older operator) are orphaned: rabbitmq.com kinds are
// prune-excluded, so nothing else would ever reclaim them.
func TestHandleDeletionReclaimsLegacyScopedTopologyFor2190(t *testing.T) {
	s := managedMQScheme(t)

	// What actually EXISTS in the cluster: this platform's topology, rendered before vhost
	// scoping shipped (i.e. unscoped, as if it had the legacy vhost's empty scope).
	legacy := managedMQPlatformCR()
	legacy.Spec.Version = platformVersion219
	legacy.Spec.Messaging.VirtualHost = bom.LegacyUnscopedVirtualHost
	legacyObjs := platformbuilder.ResolveManagedMessaging(legacy)
	require.NotEmpty(t, legacyObjs)

	// Precondition that gives this test teeth: the platform's own (unpinned) 2.19.0 default
	// vhost renders a DIFFERENT (scoped) Vhost CR name than the legacy-scoped one actually
	// seeded.
	p := managedMQPlatformCR()
	p.Spec.Version = platformVersion219
	p.Status.ObservedVersion = platformVersion219
	p.Spec.DeletionPolicy = otilmv1alpha1.PlatformDeletionPolicyDelete
	require.NotEqual(t, platformbuilder.ManagedMessagingVhostName(legacy), platformbuilder.ManagedMessagingVhostName(p),
		"precondition: the 2.19.0 default vhost scopes the object names differently from the legacy vhost")
	require.Contains(t, topologyObjectNames(legacyObjs), platformbuilder.ManagedMessagingVhostName(legacy),
		"precondition: the seeded legacy render actually carries the unscoped Vhost CR name")

	seed := []client.Object{p}
	for _, obj := range legacyObjs {
		seed = append(seed, obj.(*unstructured.Unstructured).DeepCopy())
	}
	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	require.NoError(t, r.handleDeletion(context.Background(), p))

	assert.Equal(t, 0, countTopologyObjects(t, r, legacy),
		"teardown must reclaim the legacy-scoped topology an unpinned 2.19.0 platform actually created")

	e := drainEvent(rec)
	assert.Contains(t, e, "DeletedMessaging")
}

func TestHandleDeletionExternalMessagingNoManagedTeardown(t *testing.T) {
	// An external broker has nothing to tear down regardless of policy.
	s := managedMQScheme(t)
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging:      otilmv1alpha1.MessagingSpec{Mode: "external", Host: "mq", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"}},
			DeletionPolicy: otilmv1alpha1.PlatformDeletionPolicyDelete,
		},
	}
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}
	require.NoError(t, r.handleManagedMessagingDeletion(context.Background(), p, otilmv1alpha1.PlatformDeletionPolicyDelete),
		"external broker deletion is a no-op")
}

// TestPruneNeverTouchesManagedMessaging is the deletion-safety guard at the prune layer:
// the managed rabbitmq.com kinds are EXCLUDED from the prune's managed-GVK list, so a
// managed object (present in the cluster, absent from the desired set) is NEVER pruned even
// though it carries the operator's labels.
func TestPruneNeverTouchesManagedMessaging(t *testing.T) {
	for _, kind := range unstructuredManagedGVKs() {
		assert.NotEqual(t, rabbitmqGroup, kind.Group,
			"the prune must never list/delete rabbitmq.com kinds (deletion safety)")
	}
}
