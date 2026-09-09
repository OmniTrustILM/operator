/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReconcileAuthDBSecretManagedReadback proves the mode-agnostic readback: for a
// managed database, reconcileAuthDBSecret reads the CNPG-generated <cluster>-app Secret
// (NOT a CR field) and composes the .NET connection string against the resolved
// <cluster>-rw host + the "ilm" app database — exactly the same code path external mode
// uses, only the source of the coordinates/Secret differs.
func TestReconcileAuthDBSecretManagedReadback(t *testing.T) {
	s := managedDBScheme(t)
	p := managedDBPlatformCR()

	// Seed the CNPG-generated app Secret (username/password) the readback reads. The CR
	// sets no spec.version, so the default-version bundle (== bom.Wiring()) is resolved.
	bundle, ok := bom.BundleFor(p.Spec.Version)
	require.True(t, ok)
	w := bundle.Wiring
	appSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm-db-app", Namespace: "ns"},
		Data: map[string][]byte{
			w.DatabaseCred.UsernameKey: []byte("ilm"),
			w.DatabaseCred.PasswordKey: []byte("cnpg-generated-pw"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, appSecret).Build()
	r := &Reconciler{Client: c, Scheme: s}

	require.NoError(t, r.reconcileAuthDBSecret(context.Background(), p, bundle, newDesiredSet()))

	// The composed Secret holds the .NET connection string built from the MANAGED host. With the
	// pooler default-on for a managed database the host is the PgBouncer Pooler Service
	// (ilm-db-pooler), port 5432, the "ilm" app database, and the generated password.
	var composed corev1.Secret
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: w.AuthDBSecretName}, &composed))
	connStr := string(composed.Data[w.AuthDBSecretKey])
	if connStr == "" {
		connStr = composedStringData(composed, w.AuthDBSecretKey)
	}
	assert.Contains(t, connStr, "Host=ilm-db-pooler", "managed readback wires the default-on PgBouncer Pooler host")
	assert.Contains(t, connStr, "Port=5432")
	assert.Contains(t, connStr, "Database=ilm", "managed readback wires the bootstrapped app database")
	assert.Contains(t, connStr, "Password=cnpg-generated-pw", "the generated password is read from <cluster>-app")
}

// composedStringData reads a value the fake client may have left in StringData (the
// CreateOrUpdate path sets StringData; the fake client surfaces it there until applied).
func composedStringData(s corev1.Secret, key string) string {
	if s.StringData != nil {
		return s.StringData[key]
	}
	return ""
}

// TestReconcileAuthDBSecretManagedNoLeak asserts no credential/coordinate leaks into the
// Platform status/conditions from the managed readback composition.
func TestReconcileAuthDBSecretManagedNoLeak(t *testing.T) {
	s := managedDBScheme(t)
	p := managedDBPlatformCR()
	appSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm-db-app", Namespace: "ns"},
		Data:       map[string][]byte{"username": []byte("ilm"), "password": []byte("topsecret")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, appSecret).Build()
	r := &Reconciler{Client: c, Scheme: s}
	bundle, ok := bom.BundleFor(p.Spec.Version)
	require.True(t, ok)
	require.NoError(t, r.reconcileAuthDBSecret(context.Background(), p, bundle, newDesiredSet()))

	// Nothing about the password (or the host) is reflected on the CR status.
	assert.NotContains(t, conditionsString(t, p), "topsecret")
	assert.NotContains(t, conditionsString(t, p), "ilm-db-rw")
}

// --- Deletion safety ---------------------------------------------------------

// seedManagedClusterForDeletion returns a fake client seeded with a Platform (the given
// policy) and its managed CNPG Cluster, plus a FakeRecorder, for the deletion tests.
func seedManagedClusterForDeletion(t *testing.T, policy otilmv1alpha1.PlatformDeletionPolicy) (*Reconciler, *otilmv1alpha1.Platform, *record.FakeRecorder) {
	t.Helper()
	s := managedDBScheme(t)
	p := managedDBPlatformCR()
	p.Spec.DeletionPolicy = policy

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	cluster.SetName(platformbuilder.ManagedDatabaseName(p))
	cluster.SetNamespace(p.Namespace)

	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, cluster).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: rec}, p, rec
}

func TestHandleDeletionManagedRetainLeavesCluster(t *testing.T) {
	r, p, rec := seedManagedClusterForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyRetain)

	require.NoError(t, r.handleDeletion(context.Background(), p), "deletion must never block")

	// The Cluster MUST still exist (Retain protects the database + data).
	var cluster unstructured.Unstructured
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &cluster),
		"Retain must leave the managed Cluster intact")

	// A Warning Event names the retained database (object name only — no coordinate).
	e := drainEvent(rec)
	assert.Contains(t, e, "Warning")
	assert.Contains(t, e, "RetainedDatabase")
	assert.Contains(t, e, dbSecretRef)
}

func TestHandleDeletionManagedDefaultPolicyRetains(t *testing.T) {
	// Empty policy defaults to Retain — the Cluster is left intact.
	r, p, _ := seedManagedClusterForDeletion(t, "")
	require.NoError(t, r.handleDeletion(context.Background(), p))
	var cluster unstructured.Unstructured
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	assert.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &cluster),
		"the default (empty) policy is Retain — the Cluster survives")
}

func TestHandleDeletionManagedDeleteRemovesCluster(t *testing.T) {
	r, p, rec := seedManagedClusterForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyDelete)

	require.NoError(t, r.handleDeletion(context.Background(), p))

	// The Cluster MUST be gone (Delete reclaims it; CNPG GCs its PVCs).
	var cluster unstructured.Unstructured
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &cluster)
	assert.True(t, apierrors.IsNotFound(err), "Delete must reclaim the managed Cluster")

	e := drainEvent(rec)
	assert.Contains(t, e, "DeletedDatabase")
	assert.Contains(t, e, dbSecretRef)
}

func TestHandleDeletionExternalNoManagedTeardown(t *testing.T) {
	// An external database has nothing to tear down regardless of policy.
	s := managedDBScheme(t)
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database:       otilmv1alpha1.DatabaseSpec{Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"}},
			DeletionPolicy: otilmv1alpha1.PlatformDeletionPolicyDelete,
		},
	}
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}
	require.NoError(t, r.handleManagedDatabaseDeletion(context.Background(), p, otilmv1alpha1.PlatformDeletionPolicyDelete),
		"external database deletion is a no-op")
}

// TestPruneNeverTouchesManagedDatabase is the deletion-safety guard at the prune layer:
// the managed CNPG kinds are EXCLUDED from the prune's managed-GVK list, so a managed
// Cluster (present in the cluster, absent from the desired set) is NEVER pruned even
// though it carries the operator's labels.
func TestPruneNeverTouchesManagedDatabase(t *testing.T) {
	for _, kind := range unstructuredManagedGVKs() {
		assert.NotEqual(t, cnpgGroup, kind.Group,
			"the prune must never list/delete CloudNativePG kinds (deletion safety)")
	}
}

// drainEvent returns the first buffered Event from a FakeRecorder, or "".
func drainEvent(rec *record.FakeRecorder) string {
	select {
	case e := <-rec.Events:
		return e
	default:
		return ""
	}
}

// conditionsString renders a Platform's conditions for no-leak assertions.
func conditionsString(t *testing.T, p *otilmv1alpha1.Platform) string {
	t.Helper()
	var b strings.Builder
	for _, c := range p.Status.Conditions {
		b.WriteString(c.Type + " " + string(c.Status) + " " + c.Reason + " " + c.Message + "\n")
	}
	return b.String()
}
