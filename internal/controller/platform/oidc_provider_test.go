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
	"encoding/json"
	"errors"
	"sync"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/registration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// localOIDCFake is a goroutine-safe OIDCRegistrar stand-in local to these direct-call unit
// tests (the envtest suite has its own fakeOIDC). It records the calls and returns a configured
// (secret, err): a successful fetch returns the canned secret the reconciler must relay into
// the operator-owned OIDC client Secret.
type localOIDCFake struct {
	mu        sync.Mutex
	err       error
	secret    string
	calls     []oidcCall
	userErr   error
	userCalls []realmUserCall
}

func (f *localOIDCFake) FetchClientSecret(_ context.Context, kcURL, realm, user, pass, clientID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, oidcCall{kcURL, realm, user, pass, clientID})
	if f.err != nil {
		return "", f.err
	}
	return f.secret, nil
}

// EnsureRealmUser records the call and returns the configured user-outcome (the password-admin
// stand-in for the direct-call unit tests).
func (f *localOIDCFake) EnsureRealmUser(_ context.Context, kcURL, realm, user, pass string, ru registration.RealmUser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls = append(f.userCalls, realmUserCall{kcURL, realm, user, pass, ru})
	return f.userErr
}

func (f *localOIDCFake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func (f *localOIDCFake) last() (oidcCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return oidcCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// oidcScheme registers the otilm types, core, and apps types so the fake client can store the
// Platform, the Keycloak admin Secret, the relayed OIDC client Secret, and Core's Deployment.
func oidcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	return s
}

// oidcPlatformCR is a managed-Keycloak Platform with an external database and an edge host
// (so the browser-facing OIDC URLs are populated). Its KeycloakReady condition is seeded True
// by the helpers below to isolate the OIDC reconcile from gateKeycloak.
func oidcPlatformCR() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns", Generation: 1},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{Mode: "external", Host: "pg", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "ilm-db"}},
			Keycloak: &otilmv1alpha1.KeycloakSpec{
				Mode: "managed", Realm: "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.0"},
			},
			Edge: &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost},
		},
	}
}

// withKeycloakReady stamps KeycloakReady=True (as gateKeycloak would once the CR is Ready).
func withKeycloakReady(p *otilmv1alpha1.Platform) *otilmv1alpha1.Platform {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionKeycloakReady, Status: metav1.ConditionTrue, Reason: "Reconciled",
		Message: "managed Keycloak reconciled", ObservedGeneration: p.Generation,
	})
	return p
}

// keycloakAdminSecret returns the Operator-generated initial-admin Secret for seeding.
func keycloakAdminSecret(p *otilmv1alpha1.Platform) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: platformbuilder.ManagedKeycloakAdminSecretName(p), Namespace: p.Namespace},
		Data:       map[string][]byte{"username": []byte("kc-admin"), "password": []byte(kcAdminPassword)},
	}
}

// newOIDCReconciler builds a Reconciler over a fake client seeded with the given objects and
// the given OIDCRegistrar.
func newOIDCReconciler(t *testing.T, reg registration.OIDCRegistrar, seed ...client.Object) *Reconciler {
	t.Helper()
	s := oidcScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, OIDCRegistrar: reg}
}

func oidcConfiguredCondition(p *otilmv1alpha1.Platform) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, conditionOIDCConfigured)
}

// readOIDCClientSecret reads the operator-owned OIDC client Secret's clientSecret value from
// the reconciler's client (the relayed credential), or "" / false when the Secret is absent.
func readOIDCClientSecret(t *testing.T, r *Reconciler, p *otilmv1alpha1.Platform) (string, bool) {
	t.Helper()
	var s corev1.Secret
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: p.Namespace, Name: platformbuilder.OIDCClientSecretName(p),
	}, &s); err != nil {
		return "", false
	}
	// CreateOrUpdate writes StringData, which the fake client materializes into Data.
	if v, ok := s.Data[platformbuilder.OIDCClientSecretKey]; ok {
		return string(v), true
	}
	if v, ok := s.StringData[platformbuilder.OIDCClientSecretKey]; ok {
		return v, true
	}
	return "", false
}

// TestReconcileOIDCExternalNoCall verifies an external Keycloak triggers no call and sets no
// OIDCConfigured condition.
func TestReconcileOIDCExternalNoCall(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec:       otilmv1alpha1.PlatformSpec{Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}},
	}
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.False(t, requeue, "external Keycloak needs no OIDC wiring and no requeue")
	assert.Equal(t, 0, reg.count(), "external Keycloak must not call the registrar")
	assert.Nil(t, oidcConfiguredCondition(p), "external mode sets no OIDCConfigured condition")
}

// TestReconcileOIDCUnmanagedNilNoCall verifies a nil keycloak block (external by default)
// triggers no call and no condition.
func TestReconcileOIDCUnmanagedNilNoCall(t *testing.T) {
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"}}
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.False(t, requeue)
	assert.Equal(t, 0, reg.count())
	assert.Nil(t, oidcConfiguredCondition(p))
}

// TestReconcileOIDCKeycloakNotReadyWaits verifies a managed Keycloak whose KeycloakReady is
// not True defers the wiring (WaitingForKeycloak + requeue, no call).
func TestReconcileOIDCKeycloakNotReadyWaits(t *testing.T) {
	p := oidcPlatformCR() // KeycloakReady NOT set
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.True(t, requeue, "a not-yet-ready Keycloak requests a requeue")
	assert.Equal(t, 0, reg.count(), "no call before Keycloak is ready")
	cond := oidcConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForKeycloak", cond.Reason)
}

// TestReconcileOIDCAdminSecretMissingWaits verifies that with Keycloak ready but the
// Operator-generated initial-admin Secret not yet present, the wiring defers (no call). This
// no longer depends on Core readiness (the in-pod rework removed the Core gate).
func TestReconcileOIDCAdminSecretMissingWaits(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p) // no admin Secret seeded

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.True(t, requeue)
	assert.Equal(t, 0, reg.count(), "no call before the admin Secret exists")
	cond := oidcConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, "WaitingForKeycloak", cond.Reason)
}

// TestReconcileOIDCSuccessRelaysSecretAndShortCircuits verifies the happy path: with Keycloak
// ready and the admin Secret present, the registrar is called once with the composed Keycloak
// URL + admin creds + the ilm clientId, the fetched secret is RELAYED into the operator-owned
// OIDC client Secret, OIDCConfigured=True is set, and a subsequent reconcile does NOT re-call.
// It does NOT depend on Core being ready (the in-pod mechanism removed that gate).
func TestReconcileOIDCSuccessRelaysSecretAndShortCircuits(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	reg := &localOIDCFake{secret: "kc-generated-secret-xyz"}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p)) // NO Core Deployment seeded

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.False(t, requeue, "a successful wiring needs no requeue")
	require.Equal(t, 1, reg.count(), "the registrar is called exactly once")

	cond := oidcConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Configured", cond.Reason)

	// The captured fetch call carries the admin creds, the realm, the namespace-qualified
	// Keycloak base URL, and the ilm clientId (the client the default realm defines).
	call, ok := reg.last()
	require.True(t, ok)
	assert.Equal(t, "kc-admin", call.adminUsername)
	assert.Equal(t, kcAdminPassword, call.adminPassword)
	assert.Equal(t, "ilm", call.realm)
	assert.Equal(t, "http://ilm-keycloak-service.ns:8080/kc", call.keycloakBaseURL,
		"the Keycloak base URL must be NAMESPACE-QUALIFIED and carry the /kc relative path")
	assert.Equal(t, "ilm", call.clientID)

	// The fetched secret was RELAYED into the operator-owned OIDC client Secret (never logged).
	got, present := readOIDCClientSecret(t, r, p)
	require.True(t, present, "the OIDC client Secret must be created")
	assert.Equal(t, "kc-generated-secret-xyz", got, "the fetched secret must be relayed verbatim")

	// Short-circuit: a second reconcile (same generation, OIDCConfigured=True) must NOT re-call.
	requeue = r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.False(t, requeue)
	assert.Equal(t, 1, reg.count(), "must not re-fetch while OIDCConfigured=True for the generation")
}

// TestReconcileOIDCNoCrossPodCorePUT locks the rework invariant: the registrar interface is
// FETCH-ONLY (no Core base URL is passed, no Core PUT happens at the operator layer). The
// in-pod postStart owns the PUT now, so the controller never contacts Core for OIDC. Asserted
// structurally via the recorded call having no Core coordinate.
func TestReconcileOIDCNoCrossPodCorePUT(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	reg := &localOIDCFake{secret: "s"}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))
	require.False(t, r.reconcileOIDCProvider(context.Background(), p, newDesiredSet()))
	call, ok := reg.last()
	require.True(t, ok)
	// The oidcCall struct has no coreBaseURL field anymore — the fetch carries only Keycloak
	// coordinates + the clientId. This compiles only because the Core PUT was removed.
	assert.Equal(t, "ilm", call.clientID)
	assert.Contains(t, call.keycloakBaseURL, "keycloak", "the only base URL is Keycloak's, never Core's")
}

// TestOIDCClientIDMatchesDefaultRealmClient locks the cross-package invariant that was the
// OIDC-residual root cause: the clientId the registrar looks up (the controller's oidcClientID)
// MUST be a client the operator's DEFAULT realm import actually DEFINES — otherwise the
// admin-API secret fetch 404s and OIDCConfigured never closes. It asserts both are the SAME
// exported builder constant and that the default realm contains exactly that confidential
// client with no minted secret.
func TestOIDCClientIDMatchesDefaultRealmClient(t *testing.T) {
	// The registrar lookup id and the builder's exported id are one definition.
	assert.Equal(t, platformbuilder.OIDCClientID, oidcClientID, "the registrar clientId must be the builder's OIDCClientID")

	p := oidcPlatformCR()
	realm := platformbuilder.DefaultKeycloakRealm(p)
	clients, ok := realm["clients"].([]interface{})
	require.True(t, ok)
	var found map[string]interface{}
	for _, c := range clients {
		cm := c.(map[string]interface{})
		if cm["clientId"] == oidcClientID {
			found = cm
			break
		}
	}
	require.NotNil(t, found, "the default realm must define the client the registrar looks up (%q)", oidcClientID)
	assert.Equal(t, false, found["publicClient"], "the looked-up client must be confidential (the registrar reads a client-secret)")
	_, hasSecret := found["secret"]
	assert.False(t, hasSecret, "Keycloak generates the secret; the operator must not mint it")
}

// TestReconcileOIDCReWiresOnGenerationBump verifies that a spec change (a higher Generation)
// re-runs the wiring even though OIDCConfigured=True for the old generation.
func TestReconcileOIDCReWiresOnGenerationBump(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	reg := &localOIDCFake{secret: "s"}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))

	require.False(t, r.reconcileOIDCProvider(context.Background(), p, newDesiredSet()))
	require.Equal(t, 1, reg.count())

	// Bump the generation (a spec change) — the condition's observedGeneration no longer
	// matches, so the wiring re-runs.
	p.Generation = 2
	require.False(t, r.reconcileOIDCProvider(context.Background(), p, newDesiredSet()))
	assert.Equal(t, 2, reg.count(), "a generation bump must re-run the wiring")
}

// TestReconcileOIDCTransientErrorRequeuesNoLeak verifies a transient fetch failure yields
// OIDCConfigured=False/OIDCConfigFailed + requeue. The condition message now SURFACES the
// registrar's leak-free step message + the status code (so a stuck OIDCConfigFailed is
// actionable, not an opaque "status N") — but never the admin creds or the client secret.
func TestReconcileOIDCTransientErrorRequeuesNoLeak(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	// A realistic leak-free registrar message (the registrar's messages name only the failing
	// step — never a secret or a coordinate; see internal/registration/oidc.go).
	reg := &localOIDCFake{err: &registration.Error{StatusCode: 503, Message: "Keycloak client lookup failed with status 503", Retryable: true}}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))

	desired := newDesiredSet()
	requeue := r.reconcileOIDCProvider(context.Background(), p, desired)
	assert.True(t, requeue, "a transient failure requeues")
	assert.Equal(t, 1, reg.count())

	// FLAP-SAFETY: even though the fetch failed, the OIDC client Secret key is PRESERVED in the
	// desired set so a transient Keycloak failure never prunes a previously-relayed Secret out
	// from under Core.
	assert.True(t, desired.has(r.keyForObject(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: platformbuilder.OIDCClientSecretName(p), Namespace: p.Namespace,
	}})), "the OIDC client Secret must be preserved in the desired set across a transient failure")

	// No secret was relayed (the fetch failed).
	_, present := readOIDCClientSecret(t, r, p)
	assert.False(t, present, "no OIDC client Secret is written when the fetch fails")

	cond := oidcConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "OIDCConfigFailed", cond.Reason)
	assert.Contains(t, cond.Message, "503", "the status code appears")
	assert.Contains(t, cond.Message, "Keycloak client lookup failed", "the leak-free step message is surfaced (actionable)")

	// No-leak scan over the whole conditions blob: no admin creds, no client secret.
	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	condStr := string(b)
	assert.NotContains(t, condStr, kcAdminPassword, "the admin password must never appear in conditions")
}

// TestOIDCConfigFailureMessage locks the leak-free message format: it surfaces the registrar's
// step Message AND the status code so a stuck OIDCConfigFailed names the actual failing step
// (e.g. "OIDC client not found in Keycloak realm" — the exact signature of an imported realm
// that lacks the ilm client) instead of an opaque "status 200". A transport error (status 0)
// drops the "(status N)" suffix; a non-registration error is described generically.
func TestOIDCConfigFailureMessage(t *testing.T) {
	// The bug-signature message: a 200 with the leak-free "client not found" step. The OLD
	// formatter rendered only "status 200" (a red herring); the new one names the step.
	notFound := &registration.Error{StatusCode: 200, Message: "OIDC client not found in Keycloak realm", Retryable: true}
	msg := oidcConfigFailureMessage(notFound)
	assert.Contains(t, msg, "OIDC client not found in Keycloak realm", "the step message is surfaced")
	assert.Contains(t, msg, "status 200", "the status code is included")

	// A transport error (status 0) keeps its step message without a "(status 0)" suffix.
	transport := &registration.Error{StatusCode: 0, Message: "transport error contacting Keycloak", Retryable: true}
	tmsg := oidcConfigFailureMessage(transport)
	assert.Contains(t, tmsg, "transport error contacting Keycloak")
	assert.NotContains(t, tmsg, "status 0", "a status-0 transport error must not render a misleading status code")

	// A non-registration error falls back to the generic phrase.
	generic := oidcConfigFailureMessage(errors.New("some wrapped error"))
	assert.Equal(t, "OIDC configuration failed: contacting Keycloak or Core", generic)
}

// TestReconcileOIDCDropsStaleConditionWhenSwitchedToExternal verifies that switching a
// previously-managed Keycloak to external drops the stale OIDCConfigured condition.
func TestReconcileOIDCDropsStaleConditionWhenSwitchedToExternal(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	// Pretend it was configured before.
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionOIDCConfigured, Status: metav1.ConditionTrue, Reason: "Configured",
		Message: "OIDC wired", ObservedGeneration: p.Generation,
	})
	// Now switch to external.
	p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileOIDCProvider(context.Background(), p, newDesiredSet())
	assert.False(t, requeue)
	assert.Equal(t, 0, reg.count())
	assert.Nil(t, oidcConfiguredCondition(p), "switching to external drops the stale OIDCConfigured condition")
}

// oidcPruneScheme registers everything pruneOrphans lists (otilm + the typed managed kinds)
// so a fake client can serve the prune's List calls in the survives-the-prune tests below.
func oidcPruneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	return s
}

// newOIDCPruneReconciler builds a prune-capable Reconciler (full typed scheme + a
// capability detector that reports the upstream CRDs absent so the unstructured prune path
// is skipped + a fake Recorder for prune Events) seeded with the given objects.
func newOIDCPruneReconciler(t *testing.T, reg registration.OIDCRegistrar, seed ...client.Object) *Reconciler {
	t.Helper()
	s := oidcPruneScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, OIDCRegistrar: reg, Capabilities: noCapsDetector{}, Recorder: record.NewFakeRecorder(64)}
}

// ownedOIDCSecret returns an operator-owned, labelled OIDC client Secret for platform p — the
// exact shape reconcileOIDCClientSecret writes (controller owner ref + the operator selector
// labels) — so the prune's owner-ref + label-selector matching treats it as a managed child.
func ownedOIDCSecret(p *otilmv1alpha1.Platform) *corev1.Secret {
	yes := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: platformbuilder.OIDCClientSecretName(p), Namespace: p.Namespace,
			Labels: map[string]string{
				common.NameLabel:      platformbuilder.OIDCClientSecretName(p),
				common.InstanceLabel:  p.Name,
				common.ComponentLabel: "core",
				common.PartOfLabel:    common.PartOfValue,
				common.ManagedByLabel: common.ManagedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "otilm.com/v1alpha1", Kind: "Platform",
				Name: p.Name, UID: p.UID, Controller: &yes,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{platformbuilder.OIDCClientSecretKey: []byte("relayed")},
	}
}

func oidcSecretExists(t *testing.T, r *Reconciler, p *otilmv1alpha1.Platform) bool {
	t.Helper()
	var s corev1.Secret
	err := r.Get(context.Background(), client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.OIDCClientSecretName(p)}, &s)
	return err == nil
}

// TestPruneReclaimsOIDCSecretWhenAbsentFromDesired locks the bug MECHANISM (the managed-Keycloak
// OIDC failure): the relayed OIDC client Secret is a controller-owned, label-matched child, so
// pruneOrphans DELETES it whenever it is absent from the reconcile's desired set. This is why
// reconcileOIDCProvider (which is the ONLY producer of that desired-set key) MUST run BEFORE
// pruneOrphans — if the prune runs first, it reclaims a healthy relayed Secret out from under
// Core even while OIDCConfigured stays True. The companion survives-the-prune tests prove the
// in-order flow keeps it.
func TestPruneReclaimsOIDCSecretWhenAbsentFromDesired(t *testing.T) {
	p := newPrunePlatform("ns") // stable UID for owner-ref matching
	r := newOIDCPruneReconciler(t, &localOIDCFake{}, p, ownedOIDCSecret(p))

	require.True(t, oidcSecretExists(t, r, p), "precondition: the relayed OIDC Secret exists")

	// A prune with the OIDC Secret absent from desired (exactly the pre-fix state, where the
	// prune runs before reconcileOIDCProvider has contributed the key) reclaims it.
	require.NoError(t, r.pruneOrphans(context.Background(), p, newDesiredSet()))
	assert.False(t, oidcSecretExists(t, r, p),
		"a controller-owned OIDC Secret absent from desired is pruned — so its desired key must be added BEFORE the prune")
}

// TestOIDCSecretSurvivesPruneAfterReconcileSuccess proves the FIX invariant on the success path:
// when reconcileOIDCProvider relays the Secret and records its key in the SAME desired set the
// post-apply prune consumes, a following pruneOrphans KEEPS the Secret. This is the in-order
// flow the controller must use (reconcileOIDCProvider before pruneOrphans).
func TestOIDCSecretSurvivesPruneAfterReconcileSuccess(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	p.UID = types.UID("platform-uid") // owner-ref match for the prune
	reg := &localOIDCFake{secret: "kc-generated"}
	r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p))

	desired := newDesiredSet()
	requeue := r.reconcileOIDCProvider(context.Background(), p, desired)
	require.False(t, requeue, "a successful wiring needs no requeue")
	require.True(t, oidcSecretExists(t, r, p), "the OIDC Secret was relayed")

	// The post-apply prune over the SAME desired set must KEEP the relayed Secret.
	require.NoError(t, r.pruneOrphans(context.Background(), p, desired))
	assert.True(t, oidcSecretExists(t, r, p),
		"the relayed OIDC Secret must survive the prune (its key is in the desired set populated before the prune)")
}

// TestOIDCSecretSurvivesPruneOnShortCircuit proves the FIX invariant on the STEADY-STATE
// (short-circuit) path: once OIDCConfigured=True for the generation, reconcileOIDCProvider does
// no Keycloak calls but STILL records the OIDC Secret's key in desired (the prune-preservation
// add), so a following pruneOrphans keeps the already-relayed Secret. Without that preserve — or
// if the prune ran first — a steady-state reconcile would reclaim the Secret while the condition
// stayed True (the exact observed failure).
func TestOIDCSecretSurvivesPruneOnShortCircuit(t *testing.T) {
	p := withKeycloakReady(oidcPlatformCR())
	p.UID = types.UID("platform-uid")
	// Already configured for this generation → reconcileOIDCProvider short-circuits.
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionOIDCConfigured, Status: metav1.ConditionTrue, Reason: "Configured",
		Message: "OIDC wired", ObservedGeneration: p.Generation,
	})
	reg := &localOIDCFake{secret: "unused"}
	// The Secret already exists (a prior reconcile relayed it).
	r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p), ownedOIDCSecret(p))

	desired := newDesiredSet()
	requeue := r.reconcileOIDCProvider(context.Background(), p, desired)
	require.False(t, requeue, "short-circuit needs no requeue")
	assert.Equal(t, 0, reg.count(), "short-circuit must not re-fetch from Keycloak")
	assert.True(t, desired.has(r.keyForObject(ownedOIDCSecret(p))),
		"the short-circuit path must still record the OIDC Secret key for prune-preservation")

	// The prune over that desired set keeps the existing Secret.
	require.NoError(t, r.pruneOrphans(context.Background(), p, desired))
	assert.True(t, oidcSecretExists(t, r, p),
		"a steady-state reconcile must not prune the already-relayed OIDC Secret")
}
