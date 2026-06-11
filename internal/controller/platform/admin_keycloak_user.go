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
	"errors"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/registration"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultAdminPasswordKey is the in-Secret key the admin password is read from when
// registerAdmin.password.passwordKey is unset (matching the CRD default).
const defaultAdminPasswordKey = "password"

// Admin-user (password method) status condition type and reasons. AdminUserReady is an
// ADJUNCT signal (like OIDCConfigured): it reports the password admin's
// realm-user creation without ever flipping the Platform to Degraded. It is set only for an
// enabled password method on a managed Keycloak. Reasons carry no identity material.
const (
	// conditionAdminUserReady reports the outcome of the optional password-admin reconcile
	// action: True once the Keycloak realm user exists.
	conditionAdminUserReady = "AdminUserReady"
	// reasonAdminUserWaitingForKeycloak: the managed Keycloak (or its generated admin
	// credentials) is not yet Ready, so the realm-user creation is deferred. A non-fatal,
	// self-healing waiting state (requeue).
	reasonAdminUserWaitingForKeycloak = "WaitingForKeycloak"
	// reasonAdminUserWaitingForPassword: the referenced password Secret/key is not yet present.
	// A non-fatal waiting state (requeue); the Secret watch re-enqueues on its creation.
	reasonAdminUserWaitingForPassword = "WaitingForPassword"
	// reasonAdminUserReady: the Keycloak realm user exists (created now, or already present —
	// idempotent success).
	reasonAdminUserReady = "Ready"
	// reasonAdminUserFailed: a transient/unexpected failure ensuring the realm user; the
	// reconcile requeues. The message carries only a step name + at most an HTTP status code.
	reasonAdminUserFailed = "AdminUserFailed"
)

// reconcileAdminKeycloakUser ensures the optional PASSWORD admin — a Keycloak realm user with
// the superadmin attribute — exists, once the managed Keycloak is Ready. It is an ADJUNCT
// reconcile action (like reconcileOIDCProvider): it NEVER flips
// the Platform to Degraded — every not-yet-ready / transient outcome is a non-fatal
// AdminUserReady=False plus a requeue request (returned as true). It returns false (no requeue)
// when the password method is disabled or the realm user is already ensured for the current
// spec generation.
//
// Gate: registerAdmin enabled AND registerAdmin.password present and enabled AND Keycloak
// managed. Otherwise it is inactive and drops any stale AdminUserReady condition. (The CRD's
// PlatformSpec CEL already requires keycloak.mode=managed for an enabled password method, so the
// managed check is belt-and-suspenders for a hand-mutated object.)
//
// Flow:
//   - inactive (password disabled / external Keycloak) → drop the stale condition, no requeue.
//   - already AdminUserReady=True for the current generation → short-circuit (no calls).
//   - managed Keycloak CR not Ready → AdminUserReady=False/WaitingForKeycloak, requeue.
//   - admin creds not yet generated → AdminUserReady=False/WaitingForKeycloak, requeue.
//   - password Secret/key missing → AdminUserReady=False/WaitingForPassword, requeue.
//   - ensure the realm user (idempotent): success → AdminUserReady=True; a transient error →
//     AdminUserReady=False/AdminUserFailed, requeue.
//
// SECURITY: the admin password, the Keycloak admin credentials, the access token, and any
// response body NEVER reach logs, events, status, or conditions. The password is read read-only
// and handed to the registrar once; condition messages are generic (a step name + at most an
// HTTP status code), and the username is non-sensitive.
func (r *Reconciler) reconcileAdminKeycloakUser(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	if !passwordAdminActive(p) {
		meta.RemoveStatusCondition(&p.Status.Conditions, conditionAdminUserReady)
		return false
	}

	// Short-circuit: already ensured for THIS spec generation. The condition's
	// observedGeneration guards against re-ensuring every reconcile while still re-running
	// after a spec change bumps the generation.
	if cond := meta.FindStatusCondition(p.Status.Conditions, conditionAdminUserReady); cond != nil &&
		cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == p.Generation {
		return false
	}

	// Gate (a): the managed Keycloak CR must be Ready — gateKeycloak sets KeycloakReady=True
	// only once the Keycloak CR reports Ready, so we key off that condition (no extra GET).
	if !meta.IsStatusConditionTrue(p.Status.Conditions, conditionKeycloakReady) {
		r.setAdminUserReady(p, metav1.ConditionFalse, reasonAdminUserWaitingForKeycloak,
			"waiting for the managed Keycloak to become ready")
		return true
	}

	// Read the Keycloak admin credentials read-only from the Operator-generated initial-admin
	// Secret (reused from the OIDC wiring). A missing Secret/key is the just-provisioned race.
	adminUser, adminPass, ok, err := r.readKeycloakAdminCreds(ctx, p)
	if err != nil || !ok {
		r.setAdminUserReady(p, metav1.ConditionFalse, reasonAdminUserWaitingForKeycloak,
			"waiting for the Keycloak admin credentials")
		return true
	}

	// Read the admin password read-only from the caller-provided Secret. A missing Secret/key
	// is non-fatal: requeue (the Secret watch re-enqueues on its creation).
	password, ok, err := r.readAdminPassword(ctx, p)
	if err != nil || !ok {
		r.setAdminUserReady(p, metav1.ConditionFalse, reasonAdminUserWaitingForPassword,
			"waiting for the admin password Secret")
		return true
	}

	// Ensure the realm user (idempotent by exact username — the registrar does NOT reset the
	// password if the user already exists). The identity comes from the CR; the password is
	// handed over once and never logged.
	ra := p.Spec.RegisterAdmin
	username := ra.Username
	if username == "" {
		username = adminUserDefaultUsername
	}
	ensureErr := r.OIDCRegistrar.EnsureRealmUser(ctx,
		r.keycloakBaseURL(p), platformbuilder.KeycloakRealmName(p), adminUser, adminPass,
		registration.RealmUser{
			Username:  username,
			Email:     ra.Email,
			FirstName: ra.Name,
			LastName:  ra.LastName,
			Password:  password,
		})
	if ensureErr != nil {
		// Transient/unexpected failure: non-fatal. The message carries only a step name + at
		// most a status code (never the password/creds/token); requeue to retry.
		r.setAdminUserReady(p, metav1.ConditionFalse, reasonAdminUserFailed, adminUserFailureMessage(ensureErr))
		log.FromContext(ctx).Info("admin Keycloak user not yet ensured; will retry",
			"reason", reasonAdminUserFailed, "error", ensureErr.Error())
		return true
	}

	r.setAdminUserReady(p, metav1.ConditionTrue, reasonAdminUserReady, "admin Keycloak user ensured")
	return false
}

// adminUserDefaultUsername is the realm user's username when registerAdmin.username is unset.
// It mirrors the certificate method's default Subject CommonName ("Administrator") so the two
// methods bootstrap the same identity out of the box.
const adminUserDefaultUsername = "Administrator"

// passwordAdminActive reports whether the PASSWORD admin method is active: the bootstrap is
// enabled, the password sub-block is present and enabled, AND Keycloak is managed. Otherwise the
// action is a no-op (and drops any stale condition).
func passwordAdminActive(p *otilmv1alpha1.Platform) bool {
	ra := p.Spec.RegisterAdmin
	if ra == nil || !ra.Enabled || ra.Password == nil || !ra.Password.Enabled {
		return false
	}
	return platformbuilder.KeycloakManaged(p)
}

// readAdminPassword reads the admin password read-only from registerAdmin.password.secretRef
// under the effective passwordKey (default "password"). ok is false when the ref is empty, the
// Secret is absent (NotFound), or the key is missing/empty; err is set only on a non-NotFound API
// error. SECURITY: the password bytes are never logged or surfaced.
func (r *Reconciler) readAdminPassword(ctx context.Context, p *otilmv1alpha1.Platform) (string, bool, error) {
	pw := p.Spec.RegisterAdmin.Password
	if pw == nil || pw.SecretRef == "" {
		return "", false, nil // nothing to read (should not happen when active; CEL requires secretRef)
	}
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: pw.SecretRef}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil // not yet present — requeue
		}
		return "", false, err
	}
	key := pw.PasswordKey
	if key == "" {
		key = defaultAdminPasswordKey
	}
	raw := s.Data[key]
	if len(raw) == 0 {
		return "", false, nil
	}
	return string(raw), true, nil
}

// setAdminUserReady sets the AdminUserReady condition with the given status/reason and a
// GENERIC message (never the password or any identity material), stamping observedGeneration.
func (r *Reconciler) setAdminUserReady(p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionAdminUserReady, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation,
	})
}

// adminUserFailureMessage renders a generic, leak-free condition message for a failed
// realm-user ensure. It surfaces the registrar's *registration.Error step Message plus the
// HTTP status code — both leak-free BY DESIGN (the registrar's messages name only the failing
// step, e.g. "Keycloak user create failed with status N"; they never carry the password, the
// admin creds, the token, or a response body). A transport error (status 0) drops the
// "(status N)" suffix; a non-registration error is described generically.
func adminUserFailureMessage(err error) string {
	var rErr *registration.Error
	if errors.As(err, &rErr) {
		if rErr.StatusCode > 0 {
			return fmt.Sprintf("admin user creation failed: %s (status %d)", rErr.Message, rErr.StatusCode)
		}
		return fmt.Sprintf("admin user creation failed: %s", rErr.Message)
	}
	return "admin user creation failed: contacting Keycloak"
}
