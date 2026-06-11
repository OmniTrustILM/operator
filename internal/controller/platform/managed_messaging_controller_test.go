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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
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
