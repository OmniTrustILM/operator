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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/registration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are direct-call unit tests for the password-admin reconcile action
// (reconcileAdminKeycloakUser), mirroring oidc_provider_test.go: they reuse the fake client
// (oidcScheme), the localOIDCFake registrar (now also implementing EnsureRealmUser), the
// withKeycloakReady stamper, and keycloakAdminSecret seeder. The action gates on the same
// KeycloakReady condition gateKeycloak sets, so these isolate it by stamping that condition.

// adminPWSecretName is the caller-provided admin password Secret name the direct-call tests use.
const adminPWSecretName = "admin-pw"

// passwordAdminPlatform returns a managed-Keycloak Platform with a password-only registerAdmin
// (certificate disabled) referencing the admin password Secret.
func passwordAdminPlatform() *otilmv1alpha1.Platform {
	p := oidcPlatformCR() // managed Keycloak, external DB, edge host
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Username:    "root-operator",
		Name:        "Platform Root",
		Email:       "root@example.com",
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(false)},
		Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: true, SecretRef: adminPWSecretName},
	}
	return p
}

// adminPasswordSecret returns the caller-provided admin password Secret (adminPWSecretName)
// under the given key (empty key → the default "password") holding value.
func adminPasswordSecret(p *otilmv1alpha1.Platform, key, value string) *corev1.Secret {
	if key == "" {
		key = "password"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPWSecretName, Namespace: p.Namespace},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func adminUserReadyCondition(p *otilmv1alpha1.Platform) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, conditionAdminUserReady)
}

// theAdminPassword is the sensitive value the no-leak assertions check never appears in any
// condition/event the action produces.
const theAdminPassword = "s3cr3t-admin-pw-7b3c"

// TestReconcileAdminUserExternalKeycloakNoOp verifies an external Keycloak triggers no call and
// sets no AdminUserReady condition (the CEL forbids this combination at admission, but the
// action must still be a safe no-op for a hand-mutated object).
func TestReconcileAdminUserExternalKeycloakNoOp(t *testing.T) {
	p := passwordAdminPlatform()
	p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.False(t, requeue, "external Keycloak needs no realm user and no requeue")
	assert.Equal(t, 0, len(reg.userCalls), "external Keycloak must not call the registrar")
	assert.Nil(t, adminUserReadyCondition(p), "external mode sets no AdminUserReady condition")
}

// TestReconcileAdminUserPasswordDisabledNoOp verifies a managed Keycloak with the password
// method disabled (or absent) triggers no call and drops any stale condition.
func TestReconcileAdminUserPasswordDisabledNoOp(t *testing.T) {
	p := oidcPlatformCR() // managed Keycloak, no registerAdmin
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
		// no Password block
	}
	// Pretend a prior generation had it on.
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionAdminUserReady, Status: metav1.ConditionTrue, Reason: reasonAdminUserReady,
		Message: "admin Keycloak user ensured", ObservedGeneration: p.Generation,
	})
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.False(t, requeue)
	assert.Equal(t, 0, len(reg.userCalls), "password-disabled must not call the registrar")
	assert.Nil(t, adminUserReadyCondition(p), "password-disabled drops the stale AdminUserReady condition")
}

// TestReconcileAdminUserKeycloakNotReadyWaits verifies a managed Keycloak whose KeycloakReady is
// not True defers the realm-user creation (WaitingForKeycloak + requeue, no call).
func TestReconcileAdminUserKeycloakNotReadyWaits(t *testing.T) {
	p := passwordAdminPlatform() // KeycloakReady NOT set
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p)

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.True(t, requeue, "a not-yet-ready Keycloak requests a requeue")
	assert.Equal(t, 0, len(reg.userCalls), "no call before Keycloak is ready")
	cond := adminUserReadyCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonAdminUserWaitingForKeycloak, cond.Reason)
}

// TestReconcileAdminUserPasswordSecretMissingWaits verifies that with Keycloak ready and admin
// creds present but the password Secret absent, the action defers (WaitingForPassword, no call).
func TestReconcileAdminUserPasswordSecretMissingWaits(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p)) // NO password Secret seeded

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.True(t, requeue)
	assert.Equal(t, 0, len(reg.userCalls), "no call before the password Secret exists")
	cond := adminUserReadyCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, reasonAdminUserWaitingForPassword, cond.Reason)
}

// TestReconcileAdminUserSuccessCreatesAndShortCircuits verifies the happy path: with Keycloak
// ready, admin creds present, and the password Secret present, the registrar is called once
// with the composed Keycloak URL + admin creds + the realm user (identity + password), and a
// subsequent reconcile does NOT re-call (idempotent for the generation — no password reset).
func TestReconcileAdminUserSuccessCreatesAndShortCircuits(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "", theAdminPassword))

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.False(t, requeue, "a successful ensure needs no requeue")
	require.Equal(t, 1, len(reg.userCalls), "the registrar is called exactly once")

	cond := adminUserReadyCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonAdminUserReady, cond.Reason)

	// The captured call carries the admin creds, the realm, the namespace-qualified Keycloak
	// base URL, and the realm user (identity + the read-back password).
	call := reg.userCalls[len(reg.userCalls)-1]
	assert.Equal(t, "kc-admin", call.adminUsername)
	assert.Equal(t, "kc-admin-pw", call.adminPassword)
	assert.Equal(t, "ilm", call.realm)
	assert.Equal(t, "http://ilm-keycloak-service.ns:8080/kc", call.keycloakBaseURL,
		"the Keycloak base URL must be NAMESPACE-QUALIFIED and carry the /kc relative path")
	assert.Equal(t, "root-operator", call.user.Username)
	assert.Equal(t, "root@example.com", call.user.Email)
	assert.Equal(t, "Platform Root", call.user.FirstName, "registerAdmin.name maps to firstName")
	assert.Equal(t, theAdminPassword, call.user.Password, "the read-back password is handed to the registrar")

	// Short-circuit: a second reconcile (same generation, AdminUserReady=True) must NOT re-call —
	// so the password is never reset on a steady-state reconcile.
	requeue = r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.False(t, requeue)
	assert.Equal(t, 1, len(reg.userCalls), "must not re-ensure while AdminUserReady=True for the generation")
}

// TestReconcileAdminUserHonoursPasswordKey verifies the action reads the password under the
// configured passwordKey (not the default "password").
func TestReconcileAdminUserHonoursPasswordKey(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	p.Spec.RegisterAdmin.Password.PasswordKey = "adminPassword"
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "adminPassword", "pw-under-custom-key"))

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	require.False(t, requeue)
	require.Equal(t, 1, len(reg.userCalls))
	assert.Equal(t, "pw-under-custom-key", reg.userCalls[0].user.Password, "the configured passwordKey is honoured")
}

// TestReconcileAdminUserReRunsOnGenerationBump verifies a spec change (a higher Generation)
// re-ensures even though AdminUserReady=True for the old generation (the realm-user ensure is
// idempotent server-side, so re-running is safe and never resets an existing user's password).
func TestReconcileAdminUserReRunsOnGenerationBump(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "", theAdminPassword))

	require.False(t, r.reconcileAdminKeycloakUser(context.Background(), p))
	require.Equal(t, 1, len(reg.userCalls))

	p.Generation = 2
	require.False(t, r.reconcileAdminKeycloakUser(context.Background(), p))
	assert.Equal(t, 2, len(reg.userCalls), "a generation bump re-runs the (idempotent) ensure")
}

// TestReconcileAdminUserTransientErrorRequeuesNoLeak verifies a transient ensure failure yields
// AdminUserReady=False/AdminUserFailed + requeue, surfaces the registrar's leak-free step
// message + status code, and NEVER leaks the password.
func TestReconcileAdminUserTransientErrorRequeuesNoLeak(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	reg := &localOIDCFake{userErr: &registration.Error{StatusCode: 503, Message: "Keycloak user create failed with status 503", Retryable: true}}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "", theAdminPassword))

	requeue := r.reconcileAdminKeycloakUser(context.Background(), p)
	assert.True(t, requeue, "a transient failure requeues")
	require.Equal(t, 1, len(reg.userCalls))

	cond := adminUserReadyCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonAdminUserFailed, cond.Reason)
	assert.Contains(t, cond.Message, "503")
	assert.Contains(t, cond.Message, "Keycloak user create failed", "the leak-free step message is surfaced")

	// No-leak scan over the whole conditions blob: the admin password must never appear.
	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	assert.NotContains(t, string(b), theAdminPassword, "the admin password must never appear in conditions")
}

// TestReconcileAdminUserNoLeakOnSuccess scans the conditions after a SUCCESSFUL ensure: the
// generic success message carries no password.
func TestReconcileAdminUserNoLeakOnSuccess(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "", theAdminPassword))

	require.False(t, r.reconcileAdminKeycloakUser(context.Background(), p))
	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	assert.NotContains(t, string(b), theAdminPassword, "the admin password must never appear in conditions on success")
}

// TestPasswordAdminActive locks the gate predicate's truth table.
func TestPasswordAdminActive(t *testing.T) {
	t.Run("nil registerAdmin → inactive", func(t *testing.T) {
		assert.False(t, passwordAdminActive(oidcPlatformCR()))
	})
	t.Run("password enabled + managed Keycloak → active", func(t *testing.T) {
		assert.True(t, passwordAdminActive(passwordAdminPlatform()))
	})
	t.Run("password enabled + external Keycloak → inactive", func(t *testing.T) {
		p := passwordAdminPlatform()
		p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external"}
		assert.False(t, passwordAdminActive(p))
	})
	t.Run("password present but disabled → inactive", func(t *testing.T) {
		p := passwordAdminPlatform()
		p.Spec.RegisterAdmin.Password.Enabled = false
		assert.False(t, passwordAdminActive(p))
	})
	t.Run("bootstrap disabled → inactive", func(t *testing.T) {
		p := passwordAdminPlatform()
		p.Spec.RegisterAdmin.Enabled = false
		assert.False(t, passwordAdminActive(p))
	})
}

// TestReconcileAdminUserDefaultUsername verifies that with registerAdmin.username unset, the
// realm user defaults to "Administrator" (mirroring the certificate method's default Subject CN).
func TestReconcileAdminUserDefaultUsername(t *testing.T) {
	p := withKeycloakReady(passwordAdminPlatform())
	p.Spec.RegisterAdmin.Username = ""
	reg := &localOIDCFake{}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p), adminPasswordSecret(p, "", theAdminPassword))

	require.False(t, r.reconcileAdminKeycloakUser(context.Background(), p))
	require.Equal(t, 1, len(reg.userCalls))
	assert.Equal(t, adminUserDefaultUsername, reg.userCalls[0].user.Username)
	// Sanity: confirm the realm passed matches the platform's realm name.
	assert.Equal(t, platformbuilder.KeycloakRealmName(p), reg.userCalls[0].realm)
}
