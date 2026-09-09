/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"errors"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/registration"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// oidcClientID is the platform's well-known OIDC client in Keycloak (the "ilm" client).
// The operator reads ITS secret from Keycloak and wires it into Core; there is no
// user-supplied client secret. It is a non-secret identifier. It is an ALIAS of the builder's
// platformbuilder.OIDCClientID — the SAME client the operator's default realm import DEFINES —
// so the realm the operator imports and the client the registrar looks up never drift.
const oidcClientID = platformbuilder.OIDCClientID

// reconcileOIDCProvider relays the managed Keycloak's GENERATED "ilm" client secret into an
// operator-owned Secret that Core reads IN-POD, so Core self-registers its internal OIDC
// provider via the lifecycle.postStart hook (register-internal-keycloak.sh). It is
// fetch-and-relay, NOT a cross-pod Core PUT: Core's settings API is effectively
// localhost-only, so the operator does NOT PUT Core; it only READS the
// Keycloak-generated client secret (no operator-minted credential) and writes it into the
// Secret the in-pod step consumes via $INTERNAL_OAUTH_SECRET.
//
// It is an ADJUNCT reconcile action (like admin registration): it NEVER flips the Platform to
// Degraded — every not-yet-ready / transient outcome is a non-fatal OIDCConfigured=False plus a
// requeue request (returned as true). It returns false (no requeue) when Keycloak is external/
// unmanaged or the wiring is already done for the current spec generation.
//
// Semantics of OIDCConfigured=True: the operator has WIRED OIDC — the Keycloak client exists,
// its generated secret has been relayed into <platform>-oidc-client, and Core self-registers
// the provider in-pod from it. (The terminal in-pod PUT is fire-and-forget inside Core; the
// e2e proves it landed by exec-curling Core's settings API.)
//
// Flow:
//   - Keycloak not managed → drop any stale OIDCConfigured condition, no requeue.
//   - already OIDCConfigured=True for the current generation → short-circuit (no calls).
//   - managed Keycloak CR not Ready → OIDCConfigured=False/WaitingForKeycloak, requeue.
//   - admin creds not yet generated → OIDCConfigured=False/WaitingForKeycloak, requeue.
//   - fetch the client secret from Keycloak + write the OIDC client Secret: success →
//     OIDCConfigured=True; a transient fetch error → OIDCConfigured=False/OIDCConfigFailed,
//     requeue; a Secret-write error → requeue (non-fatal).
//
// NOTE on Core readiness: this NO LONGER gates on Core being Ready. The in-pod
// mechanism means Core consumes the secret itself once the Secret exists; populating it as soon
// as Keycloak is Ready is exactly what lets Core converge (its secretKeyRef is optional, so Core
// starts even before the Secret appears, then the postStart wires OIDC). Gating on Core would
// re-introduce an ordering coupling the in-pod design removes.
//
// SECURITY: the Keycloak admin credentials, the fetched client secret, the access token, and
// any response body NEVER reach logs, events, status, or conditions. The fetched secret is
// written ONLY into the operator-owned Secret's data. Condition messages are generic (a step
// name + at most an HTTP status code).
func (r *Reconciler) reconcileOIDCProvider(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) bool {
	// Only managed Keycloak needs OIDC wiring. External Keycloak configures OIDC providers in
	// the application database; drop any stale condition from a previous managed generation.
	// (The now-stale OIDC client Secret + scripts ConfigMap are de-rendered, so the prune
	// reclaims them — they are not added to the desired set on this path.)
	if !platformbuilder.KeycloakManaged(p) {
		meta.RemoveStatusCondition(&p.Status.Conditions, conditionOIDCConfigured)
		return false
	}

	// PRESERVE the operator-owned OIDC client Secret across EVERY managed-Keycloak path below —
	// including the not-yet-ready / transient-failure early returns. The Secret is a
	// controller-owned, label-matched child the post-apply prune would otherwise reclaim when it
	// is absent from the desired set; a transient Keycloak flap (KeycloakReady briefly False, a
	// fetch 5xx while Keycloak reboots) must NEVER prune a healthy relayed Secret out from under
	// Core (which reads it via secretKeyRef). This mirrors how reconcileAuthDBSecret preserves
	// the auth-DB Secret while the database is not yet ready. Adding the key is harmless when the
	// Secret does not exist yet (the prune only deletes objects that DO exist). This preservation
	// only protects the Secret because Reconcile invokes reconcileOIDCProvider BEFORE pruneOrphans
	// (see the ORDERING note at its call site) — so the key is in the desired set the prune reads.
	desired.add(r, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: platformbuilder.OIDCClientSecretName(p), Namespace: p.Namespace}})

	// Short-circuit: already configured for THIS spec generation. The condition's
	// observedGeneration guards against re-fetching every reconcile while still re-wiring after
	// a spec change bumps the generation.
	if cond := meta.FindStatusCondition(p.Status.Conditions, conditionOIDCConfigured); cond != nil &&
		cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == p.Generation {
		return false
	}

	// Gate (a): the managed Keycloak CR must be Ready — gateKeycloak sets KeycloakReady=True
	// only once the Keycloak CR reports Ready, so we key off that condition (no extra GET).
	if !meta.IsStatusConditionTrue(p.Status.Conditions, conditionKeycloakReady) {
		r.setOIDCConfigured(p, metav1.ConditionFalse, reasonWaitingForKeycloakOIDC, "waiting for the managed Keycloak to become ready")
		return true
	}

	// Read the Keycloak admin credentials read-only from the Operator-generated initial-admin
	// Secret. A missing Secret/key is the just-provisioned race (the Operator creates it
	// shortly after the Keycloak CR goes Ready): requeue without leaking anything.
	adminUser, adminPass, ok, err := r.readKeycloakAdminCreds(ctx, p)
	if err != nil || !ok {
		r.setOIDCConfigured(p, metav1.ConditionFalse, reasonWaitingForKeycloakOIDC, "waiting for the Keycloak admin credentials")
		return true
	}

	// Fetch the GENERATED client secret from Keycloak's admin API. The registrar keeps the
	// no-leak discipline (returns a *registration.Error carrying only a step + status code).
	// A not-yet-present client (realm import lag) / Keycloak-still-booting is retryable.
	clientSecret, fetchErr := r.OIDCRegistrar.FetchClientSecret(ctx,
		r.keycloakBaseURL(p), platformbuilder.KeycloakRealmName(p), adminUser, adminPass, oidcClientID)
	if fetchErr != nil {
		r.setOIDCConfigured(p, metav1.ConditionFalse, reasonOIDCConfigFailed, oidcConfigFailureMessage(fetchErr))
		// Log the full registrar error (leak-free by design: step name + at most an HTTP status
		// code — never the admin creds, token, fetched secret, or response body) so a persistent
		// OIDCConfigFailed is debuggable from operator logs, not just the condition.
		log.FromContext(ctx).Info("OIDC client-secret fetch not yet complete; will retry",
			"reason", reasonOIDCConfigFailed, "error", fetchErr.Error())
		return true
	}

	// Relay the fetched secret into the operator-owned OIDC client Secret Core reads in-pod.
	// A write failure is non-fatal (transient SSA/update conflict): requeue to retry.
	if err := r.reconcileOIDCClientSecret(ctx, p, clientSecret, desired); err != nil {
		r.setOIDCConfigured(p, metav1.ConditionFalse, reasonOIDCConfigFailed, "OIDC client secret relay failed")
		log.FromContext(ctx).Info("OIDC client-secret relay not yet complete; will retry", "reason", reasonOIDCConfigFailed)
		return true
	}

	r.setOIDCConfigured(p, metav1.ConditionTrue, reasonOIDCConfigured,
		"OIDC wired: client secret relayed; Core self-registers the provider in-pod")
	return false
}

// reconcileOIDCClientSecret writes the Keycloak-generated "ilm" client secret into the
// operator-owned, owner-referenced OIDC client Secret (<platform>-oidc-client, key
// clientSecret) that Core sources $INTERNAL_OAUTH_SECRET from via secretKeyRef. This Secret
// is the only sanctioned place that fetched credential lives. It is owner-referenced (GC'd with the
// Platform) and recorded in the desired set so the post-apply prune keeps it.
//
// SECURITY (critical): the fetched client secret is written ONLY to this Secret's stringData
// and is NEVER logged or placed into the Platform status, conditions, or events. The returned
// error carries no secret material.
func (r *Reconciler) reconcileOIDCClientSecret(ctx context.Context, p *otilmv1alpha1.Platform, clientSecret string, desired desiredSet) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: platformbuilder.OIDCClientSecretName(p), Namespace: p.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			common.NameLabel:      platformbuilder.OIDCClientSecretName(p),
			common.InstanceLabel:  p.Name,
			common.ComponentLabel: "core",
			common.PartOfLabel:    common.PartOfValue,
			common.ManagedByLabel: common.ManagedByValue,
		}
		secret.Type = corev1.SecretTypeOpaque
		// StringData holds the only copy of the relayed secret; clear any prior Data so the
		// write is idempotent (CreateOrUpdate preserves existing Data otherwise).
		secret.Data = nil
		secret.StringData = map[string]string{platformbuilder.OIDCClientSecretKey: clientSecret}
		return controllerutil.SetControllerReference(p, secret, r.Scheme)
	})
	if err != nil {
		return err // err carries no secret material
	}
	desired.add(r, secret)
	return nil
}

// readKeycloakAdminCreds reads the Keycloak Operator-generated initial-admin Secret
// read-only and returns its username/password. ok is false when the Secret or either key is
// absent/empty (the just-provisioned race); err is set only on a non-NotFound API error.
// SECURITY: the credential bytes are never logged or surfaced.
func (r *Reconciler) readKeycloakAdminCreds(ctx context.Context, p *otilmv1alpha1.Platform) (username, password string, ok bool, err error) {
	name := platformbuilder.ManagedKeycloakAdminSecretName(p)
	var s corev1.Secret
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, &s); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return "", "", false, nil // not yet generated — requeue
		}
		return "", "", false, getErr
	}
	u := string(s.Data["username"])
	pw := string(s.Data["password"])
	if u == "" || pw == "" {
		return "", "", false, nil
	}
	return u, pw, true, nil
}

// keycloakBaseURL is the in-cluster base URL of the managed Keycloak's Service (HTTP), used by
// the OPERATOR (in its own install namespace) to reach Keycloak's admin API. The Service name
// is QUALIFIED with the Platform's namespace (<kc>-service.<namespace>) so it resolves from the
// operator's namespace — a bare name would resolve against the operator's namespace search
// domain and fail. The admin-API path is appended by the registrar. It is a non-secret
// coordinate but is never placed in status/conditions.
func (r *Reconciler) keycloakBaseURL(p *otilmv1alpha1.Platform) string {
	return fmt.Sprintf("http://%s.%s:%d%s",
		platformbuilder.ManagedKeycloakServiceName(p), p.Namespace, platformbuilder.ManagedKeycloakServicePort,
		platformbuilder.KeycloakRelativePath)
}

// setOIDCConfigured sets the OIDCConfigured condition with the given status/reason and a
// GENERIC message (never identity material), stamping observedGeneration.
func (r *Reconciler) setOIDCConfigured(p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionOIDCConfigured, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation,
	})
}

// oidcConfigFailureMessage renders a leak-free condition message for a failed OIDC wiring. It
// surfaces the registrar's *registration.Error STEP MESSAGE plus the HTTP status code — both
// leak-free BY DESIGN (the registrar's messages name only the failing step, e.g. "OIDC client
// not found in Keycloak realm" or "Keycloak admin authentication failed with status N"; they
// never carry the response body, the admin credentials, the token, or the client secret). The
// step message is what makes a persistent OIDCConfigFailed actionable instead of an opaque
// "status 200". A transport error (status 0) still carries its generic step message; a
// non-registration error is described generically.
func oidcConfigFailureMessage(err error) string {
	var rErr *registration.Error
	if errors.As(err, &rErr) {
		if rErr.StatusCode > 0 {
			return fmt.Sprintf("OIDC configuration failed: %s (status %d)", rErr.Message, rErr.StatusCode)
		}
		return fmt.Sprintf("OIDC configuration failed: %s", rErr.Message)
	}
	return "OIDC configuration failed: contacting Keycloak or Core"
}
