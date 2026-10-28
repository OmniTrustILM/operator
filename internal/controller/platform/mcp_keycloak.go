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
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/registration"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// MCP status condition type and reasons. Like OIDCConfigured, MCPConfigured is an adjunct signal:
// it reports MCP's Keycloak setup and never flips the Platform to Degraded. It exists only while
// spec.mcp.enabled is true. Reasons and messages carry no identity material.
const (
	// conditionMCPConfigured reports whether MCP's objects are in the managed Keycloak's realm and
	// the ilm-mcp client secret is relayed into <platform>-mcp-client, which the MCP server reads.
	conditionMCPConfigured = "MCPConfigured"
	// reasonMCPWaitingForKeycloak: the managed Keycloak or its admin credentials are not ready yet.
	// The step requeues.
	reasonMCPWaitingForKeycloak = "WaitingForKeycloak"
	// reasonMCPConfigured: the objects are in place and the client secret is relayed.
	reasonMCPConfigured = "Configured"
	// reasonMCPConfigFailed: the admin API pass or the Secret write failed, or the pass left drift
	// on ilm-mcp uncorrected. The step requeues.
	reasonMCPConfigFailed = "MCPConfigFailed"
	// reasonMCPUnsupportedVersion: MCP is enabled on a platform version whose bundle has no MCP
	// server. A spec.version change clears it, and so does turning MCP off.
	reasonMCPUnsupportedVersion = "UnsupportedVersion"
)

// mcpServerWorkloadName is the MCP server's workload name, which the Secret relaying its client
// secret carries as its component label. It mirrors the builder's MCP component name.
const mcpServerWorkloadName = "mcp-server"

// reconcileMCPKeycloak keeps MCP's objects in the managed Keycloak's realm through the admin API
// and relays the generated ilm-mcp client secret into the operator-owned Secret
// <platform>-mcp-client, which the MCP Deployment reads by secretKeyRef. Like
// reconcileOIDCProvider it is an adjunct that never degrades the Platform; it returns true to
// request a requeue.
//
// The admin API pass runs once per spec generation, and again whenever the relayed Secret is missing
// or holds no client secret: the MCP Deployment's secretKeyRef is required, so new MCP pods cannot
// start without it. Every path that keeps MCP on adds the Secret to the desired set, waits and
// failures included, so the prune that follows reclaims it only once MCP is off or its platform
// version has no MCP server.
//
// The pass asks for MCP's CIMD client policy only while the rendered Keycloak CR enables the cimd
// feature. An override can replace spec.features without cimd while spec.mcp.cimd stays on; the
// Keycloak gate then removes the policy before the feature goes, earlier in the same reconcile,
// and storing it again would leave it in a realm whose Keycloak restarts without the feature.
//
// SECURITY: the admin credentials and the client secret reach only the registrar and the Secret's
// data. Conditions and logs carry the registrar's step and at most an HTTP status code, or the fixed
// reason a Secret write failed.
func (r *Reconciler) reconcileMCPKeycloak(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) bool {
	if !p.Spec.MCP.Enabled {
		meta.RemoveStatusCondition(&p.Status.Conditions, conditionMCPConfigured)
		return false
	}
	if !platformbuilder.MCPEnabled(p) {
		setMCPConfigured(p, metav1.ConditionFalse, reasonMCPUnsupportedVersion, "MCP needs platform version 2.21.0 or later")
		return false
	}

	secretName := platformbuilder.MCPClientSecretName(p)
	desired.add(r, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: p.Namespace}})

	if cond := meta.FindStatusCondition(p.Status.Conditions, conditionMCPConfigured); cond != nil &&
		cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == p.Generation &&
		r.mcpClientSecretRelayed(ctx, p) {
		return false
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, conditionKeycloakReady) {
		setMCPConfigured(p, metav1.ConditionFalse, reasonMCPWaitingForKeycloak, "waiting for the managed Keycloak to become ready")
		return true
	}
	adminUser, adminPass, ok, err := r.readKeycloakAdminCreds(ctx, p)
	if err != nil || !ok {
		setMCPConfigured(p, metav1.ConditionFalse, reasonMCPWaitingForKeycloak, "waiting for the Keycloak admin credentials")
		return true
	}

	clientSecret, ensureErr := r.OIDCRegistrar.EnsureMCPRealm(ctx, r.keycloakBaseURL(p), platformbuilder.KeycloakRealmName(p),
		adminUser, adminPass, registration.MCPRealmSpec{
			PublicHost:      platformbuilder.PlatformHost(p),
			CIMD:            platformbuilder.MCPCIMDEnabled(p) && renderedKeycloakEnablesCIMD(p),
			DelegationGroup: p.Spec.MCP.Delegation.Group,
		})
	// A pass that left drift on a client with secret-rotation state still returns the secret, which
	// the rotation may have changed, so it is relayed before the error is reported.
	if clientSecret != "" {
		if err := r.relayClientSecret(ctx, p, relayedSecret{
			name: secretName, key: platformbuilder.MCPClientSecretKey, component: mcpServerWorkloadName,
		}, clientSecret, desired); err != nil {
			setMCPConfigured(p, metav1.ConditionFalse, reasonMCPConfigFailed, fmt.Sprintf("MCP client secret relay failed (%s)", err))
			log.FromContext(ctx).Info("MCP client-secret relay not yet complete; will retry",
				"reason", reasonMCPConfigFailed, "error", err.Error())
			return true
		}
	}
	// Every failure requeues, a non-retryable one too: a lost create race or an expired admin token
	// ends a pass that the next one completes, and a rotating client changes the secret to relay.
	if ensureErr != nil {
		message := mcpConfigFailureMessage(ensureErr)
		setMCPConfigured(p, metav1.ConditionFalse, reasonMCPConfigFailed, message)
		log.FromContext(ctx).Info("MCP's Keycloak objects not yet in place; will retry",
			"reason", reasonMCPConfigFailed, "error", message)
		return true
	}

	setMCPConfigured(p, metav1.ConditionTrue, reasonMCPConfigured,
		"MCP's Keycloak objects are in place and its client secret is relayed")
	return false
}

// mcpClientSecretRelayed reports whether <platform>-mcp-client exists and holds a client secret,
// read from the cache. The API server stores a relayed StringData under Data.
func (r *Reconciler) mcpClientSecretRelayed(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.MCPClientSecretName(p)}, &s); err != nil {
		return false
	}
	return len(s.Data[platformbuilder.MCPClientSecretKey]) > 0
}

// renderedKeycloakEnablesCIMD reports whether the Keycloak CR the gate renders for p, overrides
// included, enables the cimd feature. It renders the same object the gate applies, before the gate's
// CIMD guard puts cimd back on its own copy while the policy removal is pending.
func renderedKeycloakEnablesCIMD(p *otilmv1alpha1.Platform) bool {
	kc, ok := keycloakCRObject(p).(*unstructured.Unstructured)
	return ok && keycloakFeatureEnabled(kc, platformbuilder.KeycloakFeatureCIMD)
}

// setMCPConfigured sets the MCPConfigured condition with a message that carries no identity
// material, stamping observedGeneration.
func setMCPConfigured(p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionMCPConfigured, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation,
	})
}

// mcpConfigFailureMessage renders a leak-free message for a failed MCP pass: the registrar's
// *registration.Error step message and, when there is one, its HTTP status code. Both are leak-free
// by design: they name the failing step, never a credential, token, secret or response body. Any
// other error is described generically, since its text is not known to be leak-free.
func mcpConfigFailureMessage(err error) string {
	var rErr *registration.Error
	if !errors.As(err, &rErr) {
		return "MCP configuration failed: contacting Keycloak"
	}
	if rErr.StatusCode > 0 {
		return fmt.Sprintf("MCP configuration failed: %s (status %d)", rErr.Message, rErr.StatusCode)
	}
	return "MCP configuration failed: " + rErr.Message
}
