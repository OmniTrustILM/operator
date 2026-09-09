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
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// managedKCScheme is a scheme with the otilm types, core types, AND the Keycloak kinds
// (Keycloak + KeycloakRealmImport) registered as unstructured so the fake client can
// store/GET the applied objects (the readiness probe GETs the Keycloak CR back).
func managedKCScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	gv := platformbuilder.ManagedKeycloakGVK().GroupVersion()
	for _, kind := range []string{"Keycloak", "KeycloakRealmImport"} {
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// managedKCPlatformCR is an in-namespace Platform with a managed Keycloak and an external
// database (so the DB-credentials Secret the Keycloak CR references is the caller's).
func managedKCPlatformCR() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{Mode: "external", Host: "pg", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "ilm-db"}},
			Keycloak: &otilmv1alpha1.KeycloakSpec{
				Mode: "managed", Realm: "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.0"},
			},
		},
	}
}

// newManagedKCReconciler builds a Reconciler over a fake client seeded with the given
// objects and the given capability detector.
func newManagedKCReconciler(t *testing.T, det capabilityDetector, seed ...client.Object) *Reconciler {
	t.Helper()
	s := managedKCScheme(t)
	rec := record.NewFakeRecorder(32)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, Capabilities: det, Recorder: rec}
}

func keycloakReadyCondition(t *testing.T, p *otilmv1alpha1.Platform) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, "KeycloakReady")
}

// readyKeycloakCR returns a Ready Keycloak CR for the platform, for seeding the fake client.
func readyKeycloakCR(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	kc := &unstructured.Unstructured{}
	kc.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	kc.SetName(platformbuilder.ManagedKeycloakName(p))
	kc.SetNamespace(p.Namespace)
	_ = unstructured.SetNestedSlice(kc.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	return kc
}

// TestCombinedCoreChecksum locks the Core config-checksum combiner: empty inputs yield "" (so
// StampConfigChecksum stays a no-op and Core carries no annotation), and adding ANY of the three
// roll-triggers — the relayed OIDC client Secret, the trusted-certs bundle, or the in-pod scripts
// ConfigMap — CHANGES the checksum, which is what rolls Core to pick that change up (the OIDC
// secret crux of the crash-loop fix; the scripts crux of the /kc re-registration).
func TestCombinedCoreChecksum(t *testing.T) {
	assert.Empty(t, combinedCoreChecksum("", "", ""), "no roll-triggers → no stamp")

	tcOnly := combinedCoreChecksum(checksumTrustedInput, "", "")
	assert.NotEmpty(t, tcOnly)

	withOIDC := combinedCoreChecksum(checksumTrustedInput, checksumOIDCInput, "")
	assert.NotEmpty(t, withOIDC)
	assert.NotEqual(t, tcOnly, withOIDC,
		"adding the OIDC client secret must change Core's checksum so Core rolls to pick it up")

	assert.NotEmpty(t, combinedCoreChecksum("", checksumOIDCInput, ""),
		"OIDC-only (managed Keycloak without admin-bootstrap trusted-certs) still stamps")
	assert.NotEmpty(t, combinedCoreChecksum("", "", checksumScriptsInput),
		"scripts-only (managed Keycloak in-pod script, no trusted-certs/OIDC yet) still stamps")

	withScripts := combinedCoreChecksum(checksumTrustedInput, checksumOIDCInput, checksumScriptsInput)
	assert.NotEqual(t, withOIDC, withScripts,
		"a scripts-ConfigMap change must change Core's checksum so Core rolls to re-run its postStart")
	assert.Equal(t, withScripts, combinedCoreChecksum(checksumTrustedInput, checksumOIDCInput, checksumScriptsInput), "deterministic")
}

// TestOIDCClientChecksum proves Core's checksum contribution flips from empty to non-empty when
// the operator relays the OIDC client Secret — the signal that rolls Core so its postStart
// re-runs WITH the secret. Unmanaged Keycloak and an absent Secret both contribute "" (no stamp,
// no spurious roll).
func TestOIDCClientChecksum(t *testing.T) {
	s := managedKCScheme(t)
	ctx := context.Background()

	// Unmanaged (external) Keycloak → "" regardless of cluster state.
	ext := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec:       otilmv1alpha1.PlatformSpec{Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}},
	}
	rExt := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	assert.Empty(t, rExt.oidcClientChecksum(ctx, ext), "unmanaged Keycloak contributes no checksum")

	// Managed Keycloak, OIDC client Secret ABSENT → "" (Core starts; the tolerant postStart skips;
	// no spurious roll).
	mgd := managedKCPlatformCR()
	rAbsent := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(mgd).Build(), Scheme: s}
	assert.Empty(t, rAbsent.oidcClientChecksum(ctx, mgd), "absent OIDC client Secret contributes no checksum")

	// Managed Keycloak, OIDC client Secret PRESENT → non-empty (Core rolls to register OIDC).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: platformbuilder.OIDCClientSecretName(mgd), Namespace: mgd.Namespace},
		Data:       map[string][]byte{platformbuilder.OIDCClientSecretKey: []byte("relayed-secret")},
	}
	rPresent := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(mgd, sec).Build(), Scheme: s}
	assert.NotEmpty(t, rPresent.oidcClientChecksum(ctx, mgd),
		"a relayed OIDC client Secret must contribute a checksum (rolls Core)")
}

func TestGateKeycloakExternalIsReadyNoCondition(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec:       otilmv1alpha1.PlatformSpec{Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}},
	}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{}}, p)

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready, "an external Keycloak is always ready from gateKeycloak's view")
	assert.False(t, requeue)
	assert.Nil(t, keycloakReadyCondition(t, p), "external mode sets no KeycloakReady condition")
}

// TestGateKeycloakWaitsForDatabase: a managed Keycloak is NOT provisioned until the platform
// database is ready (dbReady=false) — so the Keycloak Operator's StatefulSet never starts and
// crash-loops against a Postgres that is not yet accepting connections. The gate reports
// KeycloakReady=False/WaitingForDatabase, requeues, and applies NO Keycloak CR.
func TestGateKeycloakWaitsForDatabase(t *testing.T) {
	p := managedKCPlatformCR()
	// Keycloak Operator present, but the platform database is not ready yet (dbReady=false).
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p)

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), false)
	require.NoError(t, err)
	assert.False(t, ready, "the managed Keycloak waits for the database")
	assert.True(t, requeue, "waiting on the database requests a requeue")

	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForDatabase", cond.Reason)

	// No Keycloak CR is applied while the database is not ready, so its StatefulSet never starts.
	var kc unstructured.Unstructured
	kc.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: platformbuilder.ManagedKeycloakName(p)}, &kc)
	assert.True(t, apierrors.IsNotFound(err), "no Keycloak CR is applied until the database is ready")
}

func TestGateKeycloakOperatorAbsentNotReady(t *testing.T) {
	p := managedKCPlatformCR()
	// Keycloak Operator absent (k8s.keycloak.org group not served at all).
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: false}}, p)

	desired := newDesiredSet()
	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), desired, true)
	require.NoError(t, err, "a missing Keycloak CRD is non-fatal")
	assert.False(t, ready, "without the Keycloak Operator the managed Keycloak is not ready")
	assert.True(t, requeue, "absent CRD requests a requeue to self-heal")
	assert.NotEmpty(t, desired, "an active managed-keycloak gate keeps its objects desired across a CRD flap")

	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, platformbuilder.ReasonKeycloakOperatorNotInstalled, cond.Reason)
	assert.Contains(t, cond.Message, "spec.keycloak.mode=external", "message must be actionable")
}

func TestGateKeycloakPresentCRNotReadyWaits(t *testing.T) {
	p := managedKCPlatformCR()
	// Operator present, but the Keycloak CR has not been created yet (no CR, so not Ready).
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p)

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.False(t, ready, "the Keycloak CR is not yet Ready")
	assert.True(t, requeue)

	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForKeycloak", cond.Reason)

	// The Keycloak CR must have been APPLIED.
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakName}, &applied))
	assert.Equal(t, keycloakGroup, applied.GroupVersionKind().Group)
	// Deletion safety: the applied CR carries NO controller owner reference.
	assert.Empty(t, applied.GetOwnerReferences(), "the managed Keycloak CR must carry no owner reference")
}

func TestGateKeycloakReadyWhenCRReady(t *testing.T) {
	p := managedKCPlatformCR()
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p))

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready, "a Ready Keycloak CR is ready")
	assert.False(t, requeue)

	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Reconciled", cond.Reason)
}

func TestGateKeycloakRejectedOverrideIsHardError(t *testing.T) {
	p := managedKCPlatformCR()
	bad, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"db": map[string]interface{}{"host": "evil"}},
	})
	p.Spec.Keycloak.Managed.Overrides = &runtime.RawExtension{Raw: bad}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p)

	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.Error(t, err, "a protected-field override is a deterministic user error → hard error (degrade)")
	assert.Contains(t, err.Error(), "spec.db")
}

func TestGateKeycloakTransientDetectionErrorRequeues(t *testing.T) {
	p := managedKCPlatformCR()
	r := newManagedKCReconciler(t, programmableDetector{err: assertAnError()}, p)

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err, "a transient detector error must not become a hard reconcile error")
	assert.False(t, ready)
	assert.True(t, requeue)
	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, platformbuilder.ReasonKeycloakOperatorNotInstalled, cond.Reason)
}

// --- Realm import (create-only) ----------------------------------------------

// TestGateKeycloakRealmImportDefaultRealmWhenNoConfigMap asserts the OIDC out-of-the-box
// behavior: with NO user realm ConfigMap, a managed Keycloak still gets a KeycloakRealmImport
// created — carrying the operator's default bundled realm WITH the confidential "ilm" OIDC
// client (and NO minted client secret). This is what lets reconcileOIDCProvider find the ilm
// client and close OIDCConfigured without the caller authoring a realm.
func TestGateKeycloakRealmImportDefaultRealmWhenNoConfigMap(t *testing.T) {
	p := managedKCPlatformCR() // no RealmImport configured
	p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p))

	ready, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready)

	// The default realm import was created with the ilm client inlined.
	var imp unstructured.Unstructured
	imp.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakRealmImportRes}, &imp))
	cr, _, _ := unstructured.NestedString(imp.Object, "spec", "keycloakCRName")
	assert.Equal(t, keycloakName, cr, "the default import points at the Keycloak CR")
	realmName, _, _ := unstructured.NestedString(imp.Object, "spec", "realm", "realm")
	assert.Equal(t, "ilm", realmName)
	clients, found, err := unstructured.NestedSlice(imp.Object, "spec", "realm", "clients")
	require.NoError(t, err)
	require.True(t, found, "the default realm defines the ilm client")
	require.Len(t, clients, 1)
	c := clients[0].(map[string]interface{})
	assert.Equal(t, "ilm", c["clientId"])
	assert.Equal(t, false, c["publicClient"], "the ilm client is confidential")
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "the operator must not mint the ilm client secret (Keycloak generates it)")
	assert.Empty(t, imp.GetOwnerReferences(), "the realm import carries no owner reference")
}

func TestGateKeycloakRealmImportConfigMapMissingDefersImport(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef, Key: realmConfigMapKey}
	// Operator present + Keycloak CR Ready, but the realm-import ConfigMap is absent.
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p))

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err, "a missing realm-import ConfigMap is non-fatal")
	assert.True(t, ready, "the Keycloak itself is provisioned; only the import is deferred")
	assert.True(t, requeue, "a missing ConfigMap requests a requeue to self-heal")

	cond := keycloakReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, "RealmImportConfigMapMissing", cond.Reason)

	// No KeycloakRealmImport was created (the ConfigMap is missing).
	var imp unstructured.Unstructured
	imp.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakRealmImportRes}, &imp)
	assert.True(t, apierrors.IsNotFound(err), "no import is created while the ConfigMap is missing")
}

func TestGateKeycloakRealmImportCreatesFromConfigMap(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef, Key: realmConfigMapKey}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: realmConfigMapRef, Namespace: "ns"},
		Data:       map[string]string{realmConfigMapKey: `{"realm":"ilm","enabled":true,"sslRequired":"external"}`},
	}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p), cm)

	ready, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready)

	// The KeycloakRealmImport was created with the realm representation inlined + the
	// keycloakCRName pointing at the Keycloak CR, carrying NO owner reference.
	var imp unstructured.Unstructured
	imp.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakRealmImportRes}, &imp))
	cr, _, _ := unstructured.NestedString(imp.Object, "spec", "keycloakCRName")
	assert.Equal(t, keycloakName, cr)
	realmName, _, _ := unstructured.NestedString(imp.Object, "spec", "realm", "realm")
	assert.Equal(t, "ilm", realmName, "the realm representation from the ConfigMap is inlined")
	enabled, _, _ := unstructured.NestedBool(imp.Object, "spec", "realm", "enabled")
	assert.True(t, enabled, "the rest of the realm representation is inlined too")
	assert.Empty(t, imp.GetOwnerReferences(), "the realm import carries no owner reference")

	// REGRESSION (managed-Keycloak OIDC): the user's ConfigMap realm omits the "ilm" OIDC
	// client, but the operator MUST ensure it is present — otherwise reconcileOIDCProvider's
	// registrar finds no client and OIDCConfigured never closes (the gated managed e2e caught
	// exactly this). The injected client carries no minted secret (Keycloak generates it).
	clients, found, err := unstructured.NestedSlice(imp.Object, "spec", "realm", "clients")
	require.NoError(t, err)
	require.True(t, found, "the operator ensures the ilm client even when the user realm omits it")
	require.Len(t, clients, 1, "exactly the injected ilm client")
	c := clients[0].(map[string]interface{})
	assert.Equal(t, "ilm", c["clientId"], "the well-known ilm client the registrar looks up")
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "the operator must not mint the ilm client secret (Keycloak generates it)")
}

// TestGateKeycloakRealmImportIsCreateOnly proves the import is NOT re-applied once it exists:
// a pre-existing import (with a user edit) is left untouched even though the ConfigMap differs.
func TestGateKeycloakRealmImportIsCreateOnly(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef, Key: realmConfigMapKey}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: realmConfigMapRef, Namespace: "ns"},
		Data:       map[string]string{realmConfigMapKey: `{"realm":"ilm","displayName":"FROM-CONFIGMAP"}`},
	}
	// A pre-existing import carrying a different displayName (a user edit).
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	existing.SetName(keycloakRealmImportRes)
	existing.SetNamespace("ns")
	_ = unstructured.SetNestedField(existing.Object, keycloakName, "spec", "keycloakCRName")
	_ = unstructured.SetNestedField(existing.Object, "USER-EDITED", "spec", "realm", "displayName")

	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p), cm, existing)

	ready, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready)

	var imp unstructured.Unstructured
	imp.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakRealmImportRes}, &imp))
	displayName, _, _ := unstructured.NestedString(imp.Object, "spec", "realm", "displayName")
	assert.Equal(t, "USER-EDITED", displayName, "create-only: an existing import must not be overwritten from the ConfigMap")
}

func TestGateKeycloakRealmImportMalformedJSONIsHardError(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef, Key: realmConfigMapKey}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: realmConfigMapRef, Namespace: "ns"},
		Data:       map[string]string{realmConfigMapKey: `{not valid json`},
	}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p), cm)

	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.Error(t, err, "malformed realm JSON is a deterministic user error → hard error")
	assert.Contains(t, err.Error(), "realm-representation JSON")
}

func TestGateKeycloakRealmImportMissingKeyIsHardError(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef, Key: realmConfigMapKey}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: realmConfigMapRef, Namespace: "ns"},
		Data:       map[string]string{"some-other-key": `{}`},
	}
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p, readyKeycloakCR(p), cm)

	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.Error(t, err, "a ConfigMap missing the configured key is a deterministic user error")
	assert.Contains(t, err.Error(), realmConfigMapKey)
}

// TestGateKeycloakNoLeak asserts the KeycloakReady condition carries no connection
// coordinate — only the generic field/remedy text.
func TestGateKeycloakNoLeak(t *testing.T) {
	p := managedKCPlatformCR()
	r := newManagedKCReconciler(t, programmableDetector{available: map[string]bool{keycloakGroup: true}}, p)
	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)

	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	condStr := string(b)
	conn := platformbuilder.ResolveDatabaseConnection(p)
	assert.NotContains(t, condStr, conn.Host, "the resolved DB host must not appear in conditions")
	assert.NotContains(t, condStr, conn.CredentialsSecretName, "the credentials Secret name must not appear in conditions")
}

// --- Deletion safety ---------------------------------------------------------

// seedManagedKeycloakForDeletion returns a fake client seeded with a Platform (the given
// policy) and its managed Keycloak CR + realm import, plus a FakeRecorder, for the deletion
// tests.
func seedManagedKeycloakForDeletion(t *testing.T, policy otilmv1alpha1.PlatformDeletionPolicy) (*Reconciler, *otilmv1alpha1.Platform, *record.FakeRecorder) {
	t.Helper()
	s := managedKCScheme(t)
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: realmConfigMapRef}
	p.Spec.DeletionPolicy = policy

	seed := []client.Object{p}
	for _, obj := range platformbuilder.ResolveManagedKeycloak(p) {
		u := obj.(*unstructured.Unstructured)
		seed = append(seed, u.DeepCopy())
	}

	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: rec}, p, rec
}

// countKeycloakObjects lists how many of the platform's rendered managed-keycloak objects
// still exist in the fake client.
func countKeycloakObjects(t *testing.T, r *Reconciler, p *otilmv1alpha1.Platform) int {
	t.Helper()
	n := 0
	for _, obj := range platformbuilder.ResolveManagedKeycloak(p) {
		u := obj.(*unstructured.Unstructured)
		var got unstructured.Unstructured
		got.SetGroupVersionKind(u.GroupVersionKind())
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(u), &got); err == nil {
			n++
		}
	}
	return n
}

func TestHandleDeletionManagedKeycloakRetainLeavesCR(t *testing.T) {
	r, p, rec := seedManagedKeycloakForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyRetain)
	total := len(platformbuilder.ResolveManagedKeycloak(p))
	require.Equal(t, 2, total, "the CR + realm import are both seeded")

	require.NoError(t, r.handleDeletion(context.Background(), p), "deletion must never block")

	// Both managed objects MUST still exist (Retain protects Keycloak + the realm).
	assert.Equal(t, total, countKeycloakObjects(t, r, p), "Retain must leave the Keycloak CR + realm import intact")

	e := drainEvent(rec)
	assert.Contains(t, e, "Warning")
	assert.Contains(t, e, "RetainedKeycloak")
	assert.Contains(t, e, keycloakName)
}

func TestHandleDeletionManagedKeycloakDefaultPolicyRetains(t *testing.T) {
	r, p, _ := seedManagedKeycloakForDeletion(t, "")
	total := len(platformbuilder.ResolveManagedKeycloak(p))
	require.NoError(t, r.handleDeletion(context.Background(), p))
	assert.Equal(t, total, countKeycloakObjects(t, r, p), "the default (empty) policy is Retain — Keycloak survives")
}

func TestHandleDeletionManagedKeycloakDeleteRemovesAll(t *testing.T) {
	r, p, rec := seedManagedKeycloakForDeletion(t, otilmv1alpha1.PlatformDeletionPolicyDelete)

	require.NoError(t, r.handleDeletion(context.Background(), p))

	// The Keycloak CR + realm import MUST be gone (Delete reclaims them).
	assert.Equal(t, 0, countKeycloakObjects(t, r, p), "Delete must reclaim the Keycloak CR + realm import")

	var kc unstructured.Unstructured
	kc.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: keycloakName}, &kc)
	assert.True(t, apierrors.IsNotFound(err), "Delete must reclaim the Keycloak CR")

	e := drainEvent(rec)
	assert.Contains(t, e, "DeletedKeycloak")
	assert.Contains(t, e, keycloakName)
}

func TestHandleDeletionExternalKeycloakNoManagedTeardown(t *testing.T) {
	// An external Keycloak has nothing to tear down regardless of policy.
	s := managedKCScheme(t)
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Keycloak:       &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"},
			DeletionPolicy: otilmv1alpha1.PlatformDeletionPolicyDelete,
		},
	}
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}
	require.NoError(t, r.handleManagedKeycloakDeletion(context.Background(), p, otilmv1alpha1.PlatformDeletionPolicyDelete),
		"external Keycloak deletion is a no-op")
}

// TestPruneNeverTouchesManagedKeycloak is the deletion-safety guard at the prune layer: the
// managed k8s.keycloak.org kinds are EXCLUDED from the prune's managed-GVK list, so a managed
// Keycloak (present in the cluster, absent from the desired set) is NEVER pruned even though
// it carries the operator's labels.
func TestPruneNeverTouchesManagedKeycloak(t *testing.T) {
	for _, kind := range unstructuredManagedGVKs() {
		assert.NotEqual(t, keycloakGroup, kind.Group,
			"the prune must never list/delete k8s.keycloak.org kinds (deletion safety)")
	}
}
