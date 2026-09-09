/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"encoding/json"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// guardTestBundle is the operator's default version bundle, passed to the managed-infra
// gate entry points (gateDatabase/gateMessaging/gateKeycloak) in unit tests so the major-
// version upgrade guard has the bundle's reference infra versions. The dedicated upgrade-
// guard tests (gate_managed_upgrade_test.go) drive the guard directly; the gate tests here
// pin a version equal to (or the same major as) the bundle and seed no running cluster, so
// the guard is a no-op (first creation / same major) for them.
func guardTestBundle() bom.Bundle {
	b, _ := bom.BundleFor("")
	return b
}

// managedDBScheme is a scheme with the otilm types, core types, AND the CNPG Cluster GVK
// registered as unstructured so the fake client can store/GET the applied Cluster (the
// readiness probe GETs it back). The Pooler GVK is registered too for completeness.
func managedDBScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	clusterGVK := platformbuilder.ManagedDatabaseClusterGVK()
	s.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(clusterGVK.GroupVersion().WithKind(clusterGVK.Kind+"List"), &unstructured.UnstructuredList{})
	poolerGVK := clusterGVK.GroupVersion().WithKind("Pooler")
	s.AddKnownTypeWithName(poolerGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(poolerGVK.GroupVersion().WithKind("PoolerList"), &unstructured.UnstructuredList{})
	return s
}

// managedDBPlatformCR is an in-namespace Platform with a managed database.
func managedDBPlatformCR() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "managed",
				Managed: &otilmv1alpha1.ManagedDatabaseSpec{
					Instances: 1, Version: "16",
					Storage: otilmv1alpha1.StorageSpec{Size: "10Gi"},
				},
			},
		},
	}
}

// newManagedDBReconciler builds a Reconciler over a fake client seeded with the given
// objects and the given capability detector.
func newManagedDBReconciler(t *testing.T, det capabilityDetector, seed ...client.Object) *Reconciler {
	t.Helper()
	s := managedDBScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, Capabilities: det}
}

func databaseReadyCondition(t *testing.T, p *otilmv1alpha1.Platform) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, "DatabaseReady")
}

// readyClusterSecret returns a CNPG-app Secret and a Ready CNPG Cluster for the platform,
// for seeding the fake client (the readiness probe + app-Secret presence check pass).
func readyClusterAndSecret(p *otilmv1alpha1.Platform) (*unstructured.Unstructured, *corev1.Secret) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	cluster.SetName(platformbuilder.ManagedDatabaseName(p))
	cluster.SetNamespace(p.Namespace)
	_ = unstructured.SetNestedField(cluster.Object, "Cluster in healthy state", "status", "phase")

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      platformbuilder.ResolveDatabaseConnection(p).CredentialsSecretName,
		Namespace: p.Namespace,
	}}
	return cluster, secret
}

func TestGateDatabaseExternalIsReadyNoCondition(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{
			Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"},
		}},
	}
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{}}, p)

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready, "an external database is always ready from gateDatabase's view")
	assert.False(t, requeue)
	assert.Nil(t, databaseReadyCondition(t, p), "external mode sets no DatabaseReady condition")
}

func TestGateDatabaseCNPGAbsentNotReady(t *testing.T) {
	p := managedDBPlatformCR()
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: false}}, p)

	desired := newDesiredSet()
	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), desired)
	require.NoError(t, err, "a missing CNPG CRD is non-fatal")
	assert.False(t, ready, "without CloudNativePG the managed database is not ready")
	assert.True(t, requeue, "absent CRD requests a requeue to self-heal")
	assert.NotEmpty(t, desired, "an active managed-DB gate keeps its objects desired across a CRD flap")

	cond := databaseReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, platformbuilder.ReasonCloudNativePGNotInstalled, cond.Reason)
	assert.Contains(t, cond.Message, "spec.database.mode=external", "message must be actionable")
}

func TestGateDatabaseCNPGPresentClusterNotReadyWaits(t *testing.T) {
	p := managedDBPlatformCR()
	// CNPG present, but the Cluster has not been created yet (no Cluster, no app Secret).
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p)

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready, "the Cluster is not yet Ready / its app Secret is absent")
	assert.True(t, requeue)

	cond := databaseReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForDatabase", cond.Reason)

	// The Cluster must have been APPLIED (so CNPG can start provisioning it).
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &applied))
	assert.Equal(t, common.ManagedByValue, applied.GetLabels()[common.ManagedByLabel])
	// Deletion safety: the applied Cluster carries NO controller owner reference.
	assert.Empty(t, applied.GetOwnerReferences(), "the managed Cluster must carry no owner reference")
}

func TestGateDatabaseReadyWhenClusterReadyAndSecretPresent(t *testing.T) {
	p := managedDBPlatformCR()
	cluster, secret := readyClusterAndSecret(p)
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready, "a Ready Cluster with its app Secret present is ready")
	assert.False(t, requeue)

	cond := databaseReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Reconciled", cond.Reason)
}

func TestGateDatabaseReadyViaReadyCondition(t *testing.T) {
	// A Cluster reporting status.conditions[type=Ready]=True (no phase set) is also ready.
	p := managedDBPlatformCR()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	cluster.SetName(platformbuilder.ManagedDatabaseName(p))
	cluster.SetNamespace(p.Namespace)
	_ = unstructured.SetNestedSlice(cluster.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	_, secret := readyClusterAndSecret(p)

	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)
	ready, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestGateDatabaseClusterReadyButSecretMissingWaits(t *testing.T) {
	p := managedDBPlatformCR()
	cluster, _ := readyClusterAndSecret(p) // seed the Ready Cluster but NOT the app Secret
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster)

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready, "a Ready Cluster without its generated app Secret is still waiting")
	assert.True(t, requeue)
	cond := databaseReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, "WaitingForDatabase", cond.Reason)
}

func TestGateDatabaseRejectedOverrideIsHardError(t *testing.T) {
	p := managedDBPlatformCR()
	bad, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"bootstrap": map[string]interface{}{
			"initdb": map[string]interface{}{"database": "evil"}}},
	})
	p.Spec.Database.Managed.Overrides = &runtime.RawExtension{Raw: bad}
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p)

	_, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.Error(t, err, "a protected-field override is a deterministic user error → hard error (degrade)")
	assert.Contains(t, err.Error(), "spec.bootstrap.initdb.database")
}

func TestGateDatabaseTransientDetectionErrorRequeues(t *testing.T) {
	p := managedDBPlatformCR()
	r := newManagedDBReconciler(t, programmableDetector{err: assertAnError()}, p)

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err, "a transient detector error must not become a hard reconcile error")
	assert.False(t, ready)
	assert.True(t, requeue)
	cond := databaseReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, platformbuilder.ReasonCloudNativePGNotInstalled, cond.Reason)
}

// TestGateDatabaseNoLeak asserts the DatabaseReady condition carries no connection
// coordinate or credential — only the generic field/remedy text.
func TestGateDatabaseNoLeak(t *testing.T) {
	p := managedDBPlatformCR()
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p)
	_, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)

	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	condStr := string(b)
	// Resolved host/Secret name must never appear in the condition.
	conn := platformbuilder.ResolveDatabaseConnection(p)
	assert.NotContains(t, condStr, conn.Host, "the resolved DB host must not appear in conditions")
	assert.NotContains(t, condStr, conn.CredentialsSecretName, "the credentials Secret name must not appear in conditions")
}
