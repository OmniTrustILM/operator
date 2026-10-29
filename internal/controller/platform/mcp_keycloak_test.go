/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/OmniTrustILM/operator/internal/registration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// mcpFeatures are the Keycloak features a platform serving MCP with CIMD enables, in the order the
// builder renders them; delegationFeatures are the ones it keeps with CIMD off.
var (
	mcpFeatures        = []string{featureTokenExchange, featureParamScopes, platformbuilder.KeycloakFeatureCIMD}
	delegationFeatures = []string{featureTokenExchange, featureParamScopes}
)

// mcpKeycloakPlatform is a Platform the CRD admits with MCP on: the 2.21.0 bundle, which carries
// the MCP server, an unpinned managed Keycloak, and the edge host as its public host. It has a
// stable UID for owner-reference matching. mutate, when set, runs last.
func mcpKeycloakPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	p := oidcPlatformCR()
	p.UID = types.UID("platform-uid")
	p.Spec.Version = platformVersion221
	p.Spec.Keycloak.Managed.Version = ""
	p.Spec.MCP.Enabled = true
	if mutate != nil {
		mutate(p)
	}
	return p
}

// featuresOverride is a keycloak.managed.overrides patch that replaces the Keycloak CR's
// spec.features.enabled with features.
func featuresOverride(features ...string) *runtime.RawExtension {
	raw, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{
		"features": map[string]interface{}{"enabled": features},
	}})
	return &runtime.RawExtension{Raw: raw}
}

// cimdOff, mcpOff and externalIdentity are spec changes that take the cimd feature off the
// Keycloak CR: the last moves MCP to an identity provider the platform does not manage, with the
// settings that mode requires.
func cimdOff(p *otilmv1alpha1.Platform) { p.Spec.MCP.CIMD.Enabled = ptr(false) }

func mcpOff(p *otilmv1alpha1.Platform) { p.Spec.MCP.Enabled = false }

func externalIdentity(p *otilmv1alpha1.Platform) {
	p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{
		Mode: "external", Issuer: testMCPIdPIssuer, JWKSetURI: testMCPIdPJWKS, TokenURI: testMCPIdPToken,
		ClientID: "corp-mcp", ClientSecretRef: testMCPIdPSecret, ExchangeMode: "delegation",
	}
}

// mcpExternalPlatform is mcpKeycloakPlatform with MCP signing in through an external identity
// provider. mutate, when set, runs last.
func mcpExternalPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	return mcpKeycloakPlatform(func(p *otilmv1alpha1.Platform) {
		externalIdentity(p)
		if mutate != nil {
			mutate(p)
		}
	})
}

// idpClientSecret is the Secret an external identity's client secret is read from: the one the spec
// references, holding data.
func idpClientSecret(p *otilmv1alpha1.Platform, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: p.Spec.MCP.Identity.ClientSecretRef, Namespace: p.Namespace},
		Data:       data,
	}
}

func idpSecretData(key string) map[string][]byte {
	return map[string][]byte{key: []byte(testMCPIdPSecretValue)}
}

func mcpConfiguredCondition(p *otilmv1alpha1.Platform) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, conditionMCPConfigured)
}

// mcpClientSecretKey is the desired-set key of the platform's MCP client Secret.
func mcpClientSecretKey(r *Reconciler, p *otilmv1alpha1.Platform) objectKey {
	return r.keyForObject(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: platformbuilder.MCPClientSecretName(p), Namespace: p.Namespace,
	}})
}

// readMCPClientSecret returns the MCP client Secret and the client secret it relays, or nil when
// the Secret does not exist.
func readMCPClientSecret(t *testing.T, r *Reconciler, p *otilmv1alpha1.Platform) (*corev1.Secret, string) {
	t.Helper()
	var s corev1.Secret
	err := r.Get(context.Background(), client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.MCPClientSecretName(p)}, &s)
	if apierrors.IsNotFound(err) {
		return nil, ""
	}
	require.NoError(t, err)
	// A seeded Secret holds its value in Data. A relayed one holds it in StringData: unlike the API server, the fake
	// client does not move StringData into Data.
	if v, ok := s.Data[platformbuilder.MCPClientSecretKey]; ok {
		return &s, string(v)
	}
	return &s, s.StringData[platformbuilder.MCPClientSecretKey]
}

// ownedMCPSecret is the MCP client Secret as an earlier pass relayed it: controller-owned by p and
// carrying the operator's selector labels, so the prune treats it as a managed child.
func ownedMCPSecret(p *otilmv1alpha1.Platform) *corev1.Secret {
	s := ownedOIDCSecret(p)
	s.Name = platformbuilder.MCPClientSecretName(p)
	s.Labels[common.NameLabel] = s.Name
	s.Labels[common.ComponentLabel] = mcpServerWorkloadName
	s.Data = map[string][]byte{platformbuilder.MCPClientSecretKey: []byte(testMCPClientSecretValue)}
	return s
}

// loggingContext returns a context whose logger writes every line, debug lines included, into the
// returned buffer.
func loggingContext() (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf), zap.UseDevMode(true))), &buf
}

// assertNoLeak fails when the Platform status or the log carries any of values.
func assertNoLeak(t *testing.T, p *otilmv1alpha1.Platform, logged string, values ...string) {
	t.Helper()
	status, err := json.Marshal(p.Status)
	require.NoError(t, err)
	for _, v := range values {
		assert.NotContains(t, string(status), v, "the status must not carry a secret")
		assert.NotContains(t, logged, v, "the log must not carry a secret")
	}
}

// TestReconcileMCPKeycloakRelaysTheSecret: with the managed Keycloak Ready and its admin
// credentials present, the step hands the registrar the realm, the admin credentials and what the
// CR asks for, relays the returned secret into the operator-owned <platform>-mcp-client Secret, and
// sets MCPConfigured=True for the generation.
func TestReconcileMCPKeycloakRelaysTheSecret(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		want   registration.MCPRealmSpec
	}{
		{
			name: "defaults: CIMD on, MCP acts for every user",
			want: registration.MCPRealmSpec{PublicHost: testEdgeHost, CIMD: true},
		},
		{
			name: "CIMD off, MCP acts for one group",
			mutate: func(p *otilmv1alpha1.Platform) {
				cimdOff(p)
				p.Spec.MCP.Delegation.Group = testMCPDelegationGroup
			},
			want: registration.MCPRealmSpec{PublicHost: testEdgeHost, DelegationGroup: testMCPDelegationGroup},
		},
		{
			name: "CIMD on, but an override leaves the Keycloak CR without the feature",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Keycloak.Managed.Overrides = featuresOverride(delegationFeatures...)
			},
			want: registration.MCPRealmSpec{PublicHost: testEdgeHost},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := withKeycloakReady(mcpKeycloakPlatform(tt.mutate))
			reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
			r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))
			desired := newDesiredSet()

			requeue := r.reconcileMCPKeycloak(context.Background(), p, desired)

			assert.False(t, requeue, "a completed pass needs no requeue")
			assert.Equal(t, []mcpRealmCall{{
				keycloakBaseURL: "http://ilm-keycloak-service.ns:8080/kc", realm: "ilm",
				adminUsername: "kc-admin", adminPassword: kcAdminPassword, spec: tt.want,
			}}, reg.mcpCallList())

			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, reasonMCPConfigured, cond.Reason)
			assert.Equal(t, p.Generation, cond.ObservedGeneration)

			secret, value := readMCPClientSecret(t, r, p)
			require.NotNil(t, secret, "the MCP client Secret must be written")
			assert.Equal(t, testMCPClientSecretValue, value, "the secret is relayed verbatim")
			assert.Equal(t, map[string]string{
				common.NameLabel:      platformbuilder.MCPClientSecretName(p),
				common.InstanceLabel:  p.Name,
				common.ComponentLabel: mcpServerWorkloadName,
				common.PartOfLabel:    common.PartOfValue,
				common.ManagedByLabel: common.ManagedByValue,
			}, secret.Labels)
			owner := metav1.GetControllerOf(secret)
			require.NotNil(t, owner, "the Platform controller-owns the Secret")
			assert.Equal(t, p.UID, owner.UID)
			assert.True(t, desired.has(mcpClientSecretKey(r, p)), "the relayed Secret is desired")
		})
	}
}

// TestReconcileMCPKeycloakWaitsForKeycloak: until the managed Keycloak is Ready and its admin
// credentials exist, the step calls nothing, reports WaitingForKeycloak and requeues, and still
// keeps the MCP client Secret desired so a Keycloak outage never prunes it.
func TestReconcileMCPKeycloakWaitsForKeycloak(t *testing.T) {
	tests := []struct {
		name        string
		ready       bool
		adminSecret bool
		wantMessage string
	}{
		{name: testKeycloakNotReady, adminSecret: true, wantMessage: "waiting for the managed Keycloak to become ready"},
		{name: "the admin credentials are missing", ready: true, wantMessage: "waiting for the Keycloak admin credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mcpKeycloakPlatform(nil)
			if tt.ready {
				withKeycloakReady(p)
			}
			seed := []client.Object{p}
			if tt.adminSecret {
				seed = append(seed, keycloakAdminSecret(p))
			}
			reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
			r := newOIDCReconciler(t, reg, seed...)
			desired := newDesiredSet()

			requeue := r.reconcileMCPKeycloak(context.Background(), p, desired)

			assert.True(t, requeue, "a wait requeues")
			assert.Empty(t, reg.mcpCallList(), "nothing calls Keycloak before it is ready")
			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, reasonMCPWaitingForKeycloak, cond.Reason)
			assert.Equal(t, tt.wantMessage, cond.Message)
			assert.True(t, desired.has(mcpClientSecretKey(r, p)), "a wait keeps the relayed Secret")
		})
	}
}

// TestReconcileMCPKeycloakReportsUnsupportedVersion: MCP on a platform version whose bundle has no
// MCP server renders nothing of MCP. The condition says why, nothing calls Keycloak, nothing
// requeues, and the MCP client Secret is left to the prune.
func TestReconcileMCPKeycloakReportsUnsupportedVersion(t *testing.T) {
	p := withKeycloakReady(mcpKeycloakPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = platformVersion220 }))
	reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
	r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p), ownedMCPSecret(p))
	desired := newDesiredSet()

	requeue := r.reconcileMCPKeycloak(context.Background(), p, desired)

	assert.False(t, requeue, "a spec change clears it, so there is nothing to wait for")
	assert.Empty(t, reg.mcpCallList())
	cond := mcpConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMCPUnsupportedVersion, cond.Reason)
	assert.Equal(t, "MCP needs platform version 2.21.0 or later", cond.Message)
	assert.False(t, desired.has(mcpClientSecretKey(r, p)))

	require.NoError(t, r.pruneOrphans(context.Background(), p, desired))
	secret, _ := readMCPClientSecret(t, r, p)
	assert.Nil(t, secret, "the prune reclaims the MCP client Secret")
}

// TestReconcileMCPKeycloakFailureIsSanitized: a failed admin API pass leaves MCPConfigured False
// with the step and status code the registrar reports, relays nothing, requeues whether or not the
// registrar calls the error retryable, and keeps the admin credentials out of the status and the
// log. The next pass then completes.
func TestReconcileMCPKeycloakFailureIsSanitized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a step that failed with a status",
			err:  &registration.Error{StatusCode: 500, Message: "creating client ilm-mcp", Retryable: true},
			want: "MCP configuration failed: creating client ilm-mcp (status 500)",
		},
		{
			name: "a transport error",
			err:  &registration.Error{Message: "transport error contacting Keycloak", Retryable: true},
			want: "MCP configuration failed: transport error contacting Keycloak",
		},
		{
			name: "a non-retryable step",
			err:  &registration.Error{StatusCode: 409, Message: "creating client scope mcp:access"},
			want: "MCP configuration failed: creating client scope mcp:access (status 409)",
		},
		{
			name: "an error of another type, whatever its text",
			err:  errors.New("unexpected: " + kcAdminPassword),
			want: "MCP configuration failed: contacting Keycloak",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := withKeycloakReady(mcpKeycloakPlatform(nil))
			reg := &localOIDCFake{mcpErr: tt.err}
			r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))
			ctx, logged := loggingContext()

			requeue := r.reconcileMCPKeycloak(ctx, p, newDesiredSet())

			assert.True(t, requeue, "every failure retries")
			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, reasonMCPConfigFailed, cond.Reason)
			assert.Equal(t, tt.want, cond.Message)
			secret, _ := readMCPClientSecret(t, r, p)
			assert.Nil(t, secret, "a failed pass relays nothing")
			assert.NotEmpty(t, logged.String(), "the failure is logged")
			assertNoLeak(t, p, logged.String(), kcAdminPassword)

			reg.setMCPOutcome(testMCPClientSecretValue, nil)
			assert.False(t, r.reconcileMCPKeycloak(ctx, p, newDesiredSet()), "the next pass completes")
			assert.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, conditionMCPConfigured))
			_, value := readMCPClientSecret(t, r, p)
			assert.Equal(t, testMCPClientSecretValue, value)
			assertNoLeak(t, p, logged.String(), kcAdminPassword, testMCPClientSecretValue)
		})
	}
}

// TestReconcileMCPKeycloakNamesWhyTheRelayFailed: when the Secret write fails, MCPConfigured is False and names why,
// the step requeues, and neither the condition nor the log carries the failure's own text.
func TestReconcileMCPKeycloakNamesWhyTheRelayFailed(t *testing.T) {
	for _, tt := range relayFailures {
		t.Run(tt.name, func(t *testing.T) {
			p := withKeycloakReady(mcpKeycloakPlatform(nil))
			r := newRelayFailureReconciler(t, &localOIDCFake{mcpSecret: testMCPClientSecretValue}, tt.inject, p.Namespace,
				platformbuilder.MCPClientSecretName(p), p, keycloakAdminSecret(p))
			ctx, logged := loggingContext()

			assert.True(t, r.reconcileMCPKeycloak(ctx, p, newDesiredSet()), "a failed write retries")

			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, reasonMCPConfigFailed, cond.Reason)
			assert.Equal(t, "MCP client secret relay failed ("+tt.reason+")", cond.Message)
			assert.Contains(t, logged.String(), tt.reason, "the log names the reason")
			assertNoLeak(t, p, logged.String(), kcAdminPassword, testMCPClientSecretValue, testRelayDenial, testForeignOwner)
		})
	}
}

// TestReconcileMCPKeycloakSkipsAtSameGeneration: once MCPConfigured is True for the spec generation
// and the relayed Secret holds a client secret, a pass calls nothing, yet keeps the Secret desired
// so the prune leaves it; a new generation runs the admin API pass again. The Secret is seeded with
// Data, as the API server stores a relayed StringData (the fake client keeps StringData apart).
func TestReconcileMCPKeycloakSkipsAtSameGeneration(t *testing.T) {
	ctx := context.Background()
	p := withKeycloakReady(mcpKeycloakPlatform(nil))
	setMCPConfigured(p, metav1.ConditionTrue, reasonMCPConfigured, "configured by an earlier pass")
	reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
	r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p), ownedMCPSecret(p))

	desired := newDesiredSet()
	assert.False(t, r.reconcileMCPKeycloak(ctx, p, desired))
	assert.Empty(t, reg.mcpCallList(), "no admin API pass at the same generation")
	assert.True(t, desired.has(mcpClientSecretKey(r, p)))
	require.NoError(t, r.pruneOrphans(ctx, p, desired))
	secret, _ := readMCPClientSecret(t, r, p)
	assert.NotNil(t, secret, "the prune keeps the relayed Secret")

	p.Generation = 2
	assert.False(t, r.reconcileMCPKeycloak(ctx, p, newDesiredSet()))
	assert.Len(t, reg.mcpCallList(), 1, "a new generation runs the pass again")
}

// TestReconcileMCPKeycloakRecreatesADeletedSecret: MCPConfigured is True for the spec generation,
// but the relayed Secret is gone or holds no client secret. The MCP Deployment's secretKeyRef is
// required, so new MCP pods could not start: the step runs the admin API pass again and writes the
// Secret back.
func TestReconcileMCPKeycloakRecreatesADeletedSecret(t *testing.T) {
	tests := []struct {
		name string
		seed func(*otilmv1alpha1.Platform) []client.Object
	}{
		{name: "the Secret was deleted", seed: func(*otilmv1alpha1.Platform) []client.Object { return nil }},
		{
			name: "the Secret lost its client secret",
			seed: func(p *otilmv1alpha1.Platform) []client.Object {
				emptied := ownedMCPSecret(p)
				emptied.Data = nil
				return []client.Object{emptied}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := withKeycloakReady(mcpKeycloakPlatform(nil))
			setMCPConfigured(p, metav1.ConditionTrue, reasonMCPConfigured, "configured by an earlier pass")
			reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
			r := newOIDCReconciler(t, reg, append([]client.Object{p, keycloakAdminSecret(p)}, tt.seed(p)...)...)

			assert.False(t, r.reconcileMCPKeycloak(context.Background(), p, newDesiredSet()))

			assert.Len(t, reg.mcpCallList(), 1, "the admin API pass runs again at the same generation")
			_, value := readMCPClientSecret(t, r, p)
			assert.Equal(t, testMCPClientSecretValue, value, "the client secret is relayed again")
			assert.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, conditionMCPConfigured))
		})
	}
}

// TestReconcileMCPKeycloakRelaysTheSecretWhenACorrectionIsBlocked: when ilm-mcp carries
// secret-rotation state, the registrar leaves its drift uncorrected and returns the secret with a
// non-retryable error. The step still relays the secret, and MCPConfigured is False with the
// error's message.
func TestReconcileMCPKeycloakRelaysTheSecretWhenACorrectionIsBlocked(t *testing.T) {
	const blocked = "client ilm-mcp carries secret-rotation state, so these fields were left uncorrected: fullScopeAllowed"
	p := withKeycloakReady(mcpKeycloakPlatform(nil))
	reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue, mcpErr: &registration.Error{Message: blocked}}
	r := newOIDCReconciler(t, reg, p, keycloakAdminSecret(p))
	ctx, logged := loggingContext()

	requeue := r.reconcileMCPKeycloak(ctx, p, newDesiredSet())

	assert.True(t, requeue, "the pass runs again: the client may rotate its secret")
	_, value := readMCPClientSecret(t, r, p)
	assert.Equal(t, testMCPClientSecretValue, value, "the secret is relayed despite the error")
	cond := mcpConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMCPConfigFailed, cond.Reason)
	assert.Equal(t, "MCP configuration failed: "+blocked, cond.Message)
	assertNoLeak(t, p, logged.String(), kcAdminPassword, testMCPClientSecretValue)
}

// TestReconcileMCPKeycloakRemovesTheConditionWhenOff: with spec.mcp.enabled false the step drops a
// stale MCPConfigured, calls nothing, and leaves the MCP client Secret out of the desired set, so
// the prune reclaims it.
func TestReconcileMCPKeycloakRemovesTheConditionWhenOff(t *testing.T) {
	ctx := context.Background()
	p := withKeycloakReady(mcpKeycloakPlatform(mcpOff))
	setMCPConfigured(p, metav1.ConditionTrue, reasonMCPConfigured, "configured before MCP was turned off")
	reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
	r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p), ownedMCPSecret(p))
	desired := newDesiredSet()

	assert.False(t, r.reconcileMCPKeycloak(ctx, p, desired))
	assert.Nil(t, mcpConfiguredCondition(p))
	assert.Empty(t, reg.mcpCallList())
	assert.False(t, desired.has(mcpClientSecretKey(r, p)))

	require.NoError(t, r.pruneOrphans(ctx, p, desired))
	secret, _ := readMCPClientSecret(t, r, p)
	assert.Nil(t, secret, "the prune reclaims the MCP client Secret")
}

// TestReconcileMCPExternalNeverCallsTheRegistrar: with an external identity the operator writes
// nothing to Keycloak, in whatever state Keycloak is. The step makes no realm pass and no mcp-cimd
// removal, does not wait for Keycloak, and keeps no MCP client Secret desired, so the prune reclaims
// the one that managed mode relayed. The Secret the spec references is not the operator's and stays.
func TestReconcileMCPExternalNeverCallsTheRegistrar(t *testing.T) {
	tests := []struct {
		name  string
		ready bool
	}{
		{name: "Keycloak is Ready and its admin credentials exist", ready: true},
		{name: testKeycloakNotReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p := mcpExternalPlatform(nil)
			if tt.ready {
				withKeycloakReady(p)
			}
			reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
			referenced := idpClientSecret(p, idpSecretData(testMCPIdPSecretKey))
			r := newOIDCPruneReconciler(t, reg, p, keycloakAdminSecret(p), ownedMCPSecret(p), referenced)
			desired := newDesiredSet()

			assert.False(t, r.reconcileMCPKeycloak(ctx, p, desired), "nothing to wait for")

			assert.Empty(t, reg.mcpCallList(), "no realm pass")
			assert.Zero(t, reg.removeCIMDCount(), "no mcp-cimd removal")
			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, reasonMCPExternal, cond.Reason, "an external identity does not wait for Keycloak")
			assert.False(t, desired.has(mcpClientSecretKey(r, p)), "the relayed Secret is not desired")

			require.NoError(t, r.pruneOrphans(ctx, p, desired))
			relayed, _ := readMCPClientSecret(t, r, p)
			assert.Nil(t, relayed, "the prune reclaims the Secret managed mode relayed")
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(referenced), &corev1.Secret{}),
				"the prune leaves the Secret the spec references")
		})
	}
}

// TestReconcileMCPExternalReportsTheSecret: MCPConfigured follows the Secret the MCP Deployment reads
// its client secret from. It is True with reason External while the Secret holds a value under the
// key, and False with reason MissingSecret, naming the Secret and the key, when the Secret does not
// exist or has no value there. The step needs no requeue, the platform stays out of Degraded, and
// neither the status nor the log carries the secret or a provider URL.
func TestReconcileMCPExternalReportsTheSecret(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*otilmv1alpha1.Platform)
		data       map[string][]byte
		want       metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "the Secret holds the client secret under the default key", data: idpSecretData(testMCPIdPSecretKey),
			want: metav1.ConditionTrue, wantReason: reasonMCPExternal,
		},
		{
			name: "the Secret holds it under the key the spec names", data: idpSecretData(testMCPIdPCustomKey),
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.ClientSecretKey = testMCPIdPCustomKey },
			want:   metav1.ConditionTrue, wantReason: reasonMCPExternal,
		},
		{name: "the Secret does not exist", want: metav1.ConditionFalse, wantReason: reasonMCPMissingSecret},
		{
			name: "the Secret has no such key", data: idpSecretData("another-key"),
			want: metav1.ConditionFalse, wantReason: reasonMCPMissingSecret,
		},
		{
			name: "the Secret holds the default key, but the spec names another", data: idpSecretData(testMCPIdPSecretKey),
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.ClientSecretKey = testMCPIdPCustomKey },
			want:   metav1.ConditionFalse, wantReason: reasonMCPMissingSecret,
		},
		{
			name: "the key holds an empty value", data: map[string][]byte{testMCPIdPSecretKey: {}},
			want: metav1.ConditionFalse, wantReason: reasonMCPMissingSecret,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := withKeycloakReady(mcpExternalPlatform(tt.mutate))
			seed := []client.Object{p, keycloakAdminSecret(p)}
			if tt.data != nil {
				seed = append(seed, idpClientSecret(p, tt.data))
			}
			reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
			r := newOIDCReconciler(t, reg, seed...)
			ctx, logged := loggingContext()
			name, key := platformbuilder.MCPClientSecret(p)

			assert.False(t, r.reconcileMCPKeycloak(ctx, p, newDesiredSet()), "the Secret watch re-runs the step, so it never requeues")

			cond := mcpConfiguredCondition(p)
			require.NotNil(t, cond)
			assert.Equal(t, tt.want, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
			assert.Equal(t, p.Generation, cond.ObservedGeneration)
			if tt.want == metav1.ConditionTrue {
				assert.Equal(t, fmt.Sprintf("MCP signs in through an external identity provider and its client secret is in Secret %q", name), cond.Message)
			} else {
				assert.Equal(t, fmt.Sprintf("Secret %q does not exist or has no value under key %q", name, key), cond.Message)
			}
			assert.Nil(t, meta.FindStatusCondition(p.Status.Conditions, conditionDegraded), "MCPConfigured is an adjunct: it never degrades the platform")
			assert.Empty(t, reg.mcpCallList(), "the operator writes nothing to Keycloak")
			assertNoLeak(t, p, logged.String(), testMCPIdPSecretValue, testMCPIdPIssuer, testMCPIdPJWKS, testMCPIdPToken, "://")
		})
	}
}

// TestReconcileMCPExternalFollowsTheSecretWithoutAGenerationChange: the step has no same-generation
// short-circuit, so the condition tracks the Secret as it is created, emptied and created again while
// the spec does not change. The Secret watch is what re-runs the step.
func TestReconcileMCPExternalFollowsTheSecretWithoutAGenerationChange(t *testing.T) {
	ctx := context.Background()
	p := mcpExternalPlatform(nil)
	r := newOIDCReconciler(t, &localOIDCFake{}, p)
	reason := func() string {
		t.Helper()
		require.False(t, r.reconcileMCPKeycloak(ctx, p, newDesiredSet()))
		cond := mcpConfiguredCondition(p)
		require.NotNil(t, cond)
		return cond.Reason
	}

	assert.Equal(t, reasonMCPMissingSecret, reason(), "no Secret yet")

	secret := idpClientSecret(p, idpSecretData(testMCPIdPSecretKey))
	require.NoError(t, r.Create(ctx, secret))
	assert.Equal(t, reasonMCPExternal, reason(), "the Secret is created")

	secret.Data = nil
	require.NoError(t, r.Update(ctx, secret))
	assert.Equal(t, reasonMCPMissingSecret, reason(), "the client secret is removed")

	secret.Data = idpSecretData(testMCPIdPSecretKey)
	require.NoError(t, r.Update(ctx, secret))
	assert.Equal(t, reasonMCPExternal, reason(), "the client secret is back")
}

// TestReconcileMCPExternalReportsUnsupportedVersion: an external identity on a platform version whose
// bundle has no MCP server reports UnsupportedVersion like the managed one, whatever the Secret holds.
func TestReconcileMCPExternalReportsUnsupportedVersion(t *testing.T) {
	p := withKeycloakReady(mcpExternalPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = platformVersion220 }))
	reg := &localOIDCFake{mcpSecret: testMCPClientSecretValue}
	r := newOIDCReconciler(t, reg, p, idpClientSecret(p, idpSecretData(testMCPIdPSecretKey)))

	assert.False(t, r.reconcileMCPKeycloak(context.Background(), p, newDesiredSet()))

	cond := mcpConfiguredCondition(p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMCPUnsupportedVersion, cond.Reason)
	assert.Equal(t, "MCP needs platform version 2.21.0 or later", cond.Message)
	assert.Empty(t, reg.mcpCallList())
}

// newMCPStampRecorder builds a Reconciler over a fake client that holds p and seed and records the
// server-side applies it is asked for instead of performing them. It returns the Reconciler with a
// function that runs one apply pass and reports the checksum/config stamp on the Deployments of the
// MCP server, Core and the gateway.
func newMCPStampRecorder(t *testing.T, p *otilmv1alpha1.Platform, seed ...client.Object) (*Reconciler, func() map[string]string) {
	t.Helper()
	s := oidcPruneScheme(t)
	applied := map[string]*appsv1.Deployment{}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{p}, seed...)...).WithInterceptorFuncs(interceptor.Funcs{
		// Record the Deployments a pass applies; the apply itself does not matter here.
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			if dep, ok := obj.(*appsv1.Deployment); ok {
				applied[dep.Name] = dep.DeepCopy()
			}
			return nil
		},
	}).Build()
	r := &Reconciler{Client: c, Scheme: s}
	mig := migrationRender{version: p.Spec.Version, bundle: bundleFor(t, p.Spec.Version)}
	return r, func() map[string]string {
		t.Helper()
		clear(applied)
		_, handled, _, err := r.composeAndApplyBase(context.Background(), p, mig, newDesiredSet(), false)
		require.NoError(t, err)
		require.False(t, handled)
		got := map[string]string{}
		for _, name := range []string{mcpServerWorkloadName, coreDeploymentName, gatewayWorkloadName} {
			require.Contains(t, applied, name)
			got[name] = applied[name].Spec.Template.Annotations[platformbuilder.ConfigChecksumAnnotation]
		}
		return got
	}
}

// TestMCPChecksumRollsTheDeployment: a pass stamps the relayed MCP client Secret's checksum on the
// mcp-server Deployment, so relaying a different secret rolls it, while Core and the gateway ignore
// that Secret. Before the Secret exists, or with MCP off, there is nothing to stamp.
func TestMCPChecksumRollsTheDeployment(t *testing.T) {
	ctx := context.Background()
	p := mcpKeycloakPlatform(nil)
	r, stamps := newMCPStampRecorder(t, p)

	before := stamps()
	assert.Empty(t, before[mcpServerWorkloadName], "nothing to stamp before the Secret exists")

	secret := ownedMCPSecret(p)
	require.NoError(t, r.Create(ctx, secret))
	first := stamps()
	assert.Equal(t, checksum.ComputeSecretChecksum(secret), first[mcpServerWorkloadName])

	secret.Data = map[string][]byte{platformbuilder.MCPClientSecretKey: []byte("a-rotated-mcp-client-secret")}
	require.NoError(t, r.Update(ctx, secret))
	second := stamps()
	assert.NotEqual(t, first[mcpServerWorkloadName], second[mcpServerWorkloadName], "a new secret rolls the MCP server")

	for _, name := range []string{coreDeploymentName, gatewayWorkloadName} {
		assert.Equal(t, before[name], first[name], "%s ignores the MCP client Secret", name)
		assert.Equal(t, before[name], second[name], "%s ignores the MCP client Secret", name)
	}

	assert.Empty(t, r.mcpConfigChecksum(ctx, mcpKeycloakPlatform(mcpOff), checksumTrustedInput),
		"MCP off: nothing to stamp, a composed bundle included")
}

// TestMCPExternalChecksumFollowsTheReferencedSecret: with an external identity a pass stamps the
// checksum of the Secret the spec references on the mcp-server Deployment, so a client secret that
// changes there rolls the server. The relayed <platform>-mcp-client Secret that managed mode follows
// plays no part, and Core and the gateway ignore the referenced Secret. Before it exists there is
// nothing to stamp.
func TestMCPExternalChecksumFollowsTheReferencedSecret(t *testing.T) {
	ctx := context.Background()
	p := mcpExternalPlatform(nil)
	r, stamps := newMCPStampRecorder(t, p)

	before := stamps()
	assert.Empty(t, before[mcpServerWorkloadName], "nothing to stamp before the Secret exists")

	secret := idpClientSecret(p, idpSecretData(testMCPIdPSecretKey))
	require.NoError(t, r.Create(ctx, secret))
	first := stamps()
	assert.Equal(t, checksum.ComputeSecretChecksum(secret), first[mcpServerWorkloadName])

	secret.Data = map[string][]byte{testMCPIdPSecretKey: []byte("a-rotated-idp-client-secret")}
	require.NoError(t, r.Update(ctx, secret))
	second := stamps()
	assert.NotEqual(t, first[mcpServerWorkloadName], second[mcpServerWorkloadName], "a new client secret rolls the MCP server")

	require.NoError(t, r.Create(ctx, ownedMCPSecret(p)))
	assert.Equal(t, second, stamps(), "the Secret managed mode relays is not followed")

	for _, name := range []string{coreDeploymentName, gatewayWorkloadName} {
		assert.Equal(t, before[name], first[name], "%s ignores the referenced Secret", name)
		assert.Equal(t, before[name], second[name], "%s ignores the referenced Secret", name)
	}
}

// mcpIdentityModes are the ways MCP signs in, each with the Secret its client secret is read from.
var mcpIdentityModes = []struct {
	name         string
	mutate       func(*otilmv1alpha1.Platform)
	clientSecret func(*otilmv1alpha1.Platform) *corev1.Secret
}{
	{name: "managed identity", clientSecret: ownedMCPSecret},
	{
		name: "external identity", mutate: externalIdentity,
		clientSecret: func(p *otilmv1alpha1.Platform) *corev1.Secret {
			return idpClientSecret(p, idpSecretData(testMCPIdPSecretKey))
		},
	},
}

// trustedCertsPlatform is mcpKeycloakPlatform in the given identity mode with the user's
// trusted-certificates Secret referenced, and, when compose is set, an admin certificate the operator
// generates, which makes the operator compose that Secret and its own admin CA into one bundle.
func trustedCertsPlatform(mutate func(*otilmv1alpha1.Platform), compose bool) *otilmv1alpha1.Platform {
	return mcpKeycloakPlatform(func(p *otilmv1alpha1.Platform) {
		if mutate != nil {
			mutate(p)
		}
		p.Spec.Common.TrustedCertificates.SecretRef = userTrustSecretRef
		if compose {
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
			}
		}
	})
}

// userTrustSecret is the user's trusted-certificates Secret, holding one CA.
func userTrustSecret(p *otilmv1alpha1.Platform) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userTrustSecretRef, Namespace: p.Namespace},
		Data:       map[string][]byte{caCrtKey: []byte(userCAValue)},
	}
}

// TestMCPChecksumFollowsTheComposedTrustBundle: in both identity modes the checksum on the mcp-server
// Deployment follows the trusted-certificates bundle the operator composes, as Core's does, so a
// bundle that changes rolls the server, which reads it from an environment variable.
func TestMCPChecksumFollowsTheComposedTrustBundle(t *testing.T) {
	for _, mode := range mcpIdentityModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			p := trustedCertsPlatform(mode.mutate, true)
			clientSecret, trust := mode.clientSecret(p), userTrustSecret(p)
			r, stamps := newMCPStampRecorder(t, p, clientSecret, trust)

			first := stamps()[mcpServerWorkloadName]
			assert.NotEmpty(t, first)
			assert.NotEqual(t, checksum.ComputeSecretChecksum(clientSecret), first, "the composed bundle is part of the checksum")
			assert.Equal(t, first, stamps()[mcpServerWorkloadName], "the checksum is stable while nothing changes")

			trust.Data[caCrtKey] = []byte("A-ROTATED-CA")
			require.NoError(t, r.Update(ctx, trust))
			assert.NotEqual(t, first, stamps()[mcpServerWorkloadName], "a changed bundle rolls the MCP server")
		})
	}
}

// TestMCPChecksumIgnoresAUserTrustSecretTheOperatorDoesNotCompose: without an admin CA to fold in, the
// operator references the user's trusted-certificates Secret as it is, so, as for Core, its content is
// not part of the checksum: the checksum stays the client Secret's own, which an operator upgrade
// must not change.
func TestMCPChecksumIgnoresAUserTrustSecretTheOperatorDoesNotCompose(t *testing.T) {
	for _, mode := range mcpIdentityModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			p := trustedCertsPlatform(mode.mutate, false)
			clientSecret, trust := mode.clientSecret(p), userTrustSecret(p)
			r, stamps := newMCPStampRecorder(t, p, clientSecret, trust)

			first := stamps()[mcpServerWorkloadName]
			assert.Equal(t, checksum.ComputeSecretChecksum(clientSecret), first)

			trust.Data[caCrtKey] = []byte("A-ROTATED-CA")
			require.NoError(t, r.Update(ctx, trust))
			assert.Equal(t, first, stamps()[mcpServerWorkloadName], "the user's Secret is not followed")
		})
	}
}

// TestNextRequeueResultRetriesMCPOnTheAdjunctCadence: MCP's Keycloak step retries on the cadence of
// the other Keycloak adjuncts.
func TestNextRequeueResultRetriesMCPOnTheAdjunctCadence(t *testing.T) {
	assert.Equal(t, ctrl.Result{RequeueAfter: adminRegisterRequeueAfter},
		nextRequeueResult(requeueSignals{mcp: true, ready: true}))
}

// liveKeycloak is the managed Keycloak CR as the cluster holds it: Ready or not, and enabling
// features.
func liveKeycloak(p *otilmv1alpha1.Platform, ready bool, features ...string) *unstructured.Unstructured {
	kc := readyKeycloakCR(p)
	if !ready {
		_ = unstructured.SetNestedSlice(kc.Object, []interface{}{
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionFalse)},
		}, "status", "conditions")
	}
	if len(features) > 0 {
		_ = unstructured.SetNestedStringSlice(kc.Object, features, "spec", "features", "enabled")
	}
	return kc
}

// newCIMDGateReconciler builds a reconciler for the Keycloak gate over a fake client seeded with
// seed, with the Keycloak Operator's kinds served. applied returns the last Keycloak CR the gate
// sent, or nil. failReads, when set, fails every read of the Keycloak CR.
func newCIMDGateReconciler(t *testing.T, reg registration.OIDCRegistrar, failReads error, seed ...client.Object) (*Reconciler, func() *unstructured.Unstructured) {
	t.Helper()
	s := managedKCScheme(t)
	var last *unstructured.Unstructured
	isKeycloak := func(obj client.Object) bool {
		return obj.GetObjectKind().GroupVersionKind() == platformbuilder.ManagedKeycloakGVK()
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failReads != nil && isKeycloak(obj) {
				return failReads
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && isKeycloak(obj) {
				last = u.DeepCopy()
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := &Reconciler{
		Client: c, Scheme: s, OIDCRegistrar: reg, Recorder: record.NewFakeRecorder(32),
		Capabilities: programmableDetector{available: map[string]bool{keycloakGroup: true}},
	}
	return r, func() *unstructured.Unstructured { return last }
}

// enabledFeatures returns the features a Keycloak CR lists in spec.features.enabled.
func enabledFeatures(t *testing.T, kc *unstructured.Unstructured) []string {
	t.Helper()
	require.NotNil(t, kc, "the gate applied no Keycloak CR")
	features, _, err := unstructured.NestedStringSlice(kc.Object, "spec", "features", "enabled")
	require.NoError(t, err)
	return features
}

// removalFails is how a failed RemoveMCPCIMD call reports.
var removalFails = &registration.Error{StatusCode: 503, Message: "writing the client policies", Retryable: true}

// TestKeycloakGateKeepsCIMDUntilThePolicyIsRemoved: while MCP's mcp-cimd client policy may still be
// in the realm, the gate keeps cimd on the Keycloak CR it applies and requeues: the removal failed,
// the Keycloak is not Ready, or its admin credentials are missing.
func TestKeycloakGateKeepsCIMDUntilThePolicyIsRemoved(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*otilmv1alpha1.Platform)
		ready        bool
		adminSecret  bool
		removeErr    error
		wantCalls    int
		wantFeatures []string
	}{
		{
			name: "CIMD switched off and the removal fails", mutate: cimdOff, ready: true, adminSecret: true,
			removeErr: removalFails, wantCalls: 1, wantFeatures: mcpFeatures,
		},
		{
			name: "MCP turned off and the removal fails", mutate: mcpOff, ready: true, adminSecret: true,
			removeErr: removalFails, wantCalls: 1, wantFeatures: []string{platformbuilder.KeycloakFeatureCIMD},
		},
		{name: testKeycloakNotReady, mutate: cimdOff, adminSecret: true, wantFeatures: mcpFeatures},
		{name: "the admin credentials are missing", mutate: cimdOff, ready: true, wantFeatures: mcpFeatures},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mcpKeycloakPlatform(tt.mutate)
			seed := []client.Object{p, liveKeycloak(p, tt.ready, mcpFeatures...)}
			if tt.adminSecret {
				seed = append(seed, keycloakAdminSecret(p))
			}
			reg := &localOIDCFake{removeCIMDErr: tt.removeErr}
			r, applied := newCIMDGateReconciler(t, reg, nil, seed...)

			_, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

			require.NoError(t, err)
			assert.True(t, requeue, "the gate retries the removal")
			assert.Equal(t, tt.wantCalls, reg.removeCIMDCount())
			assert.Equal(t, tt.wantFeatures, enabledFeatures(t, applied()), "the applied CR keeps cimd")
		})
	}
}

// TestKeycloakGateDropsCIMDAfterThePolicyIsRemoved: once RemoveMCPCIMD succeeds against a Ready
// Keycloak, the gate applies the Keycloak CR without cimd, whether CIMD was switched off or MCP
// turned off, and holds nothing back.
func TestKeycloakGateDropsCIMDAfterThePolicyIsRemoved(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*otilmv1alpha1.Platform)
		wantFeatures []string
	}{
		{name: "CIMD switched off", mutate: cimdOff, wantFeatures: delegationFeatures},
		{name: "MCP turned off", mutate: mcpOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mcpKeycloakPlatform(tt.mutate)
			reg := &localOIDCFake{}
			r, applied := newCIMDGateReconciler(t, reg, nil, p, liveKeycloak(p, true, mcpFeatures...), keycloakAdminSecret(p))

			ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

			require.NoError(t, err)
			assert.True(t, ready)
			assert.False(t, requeue, "nothing holds once the policy is gone")
			assert.Equal(t, 1, reg.removeCIMDCount())
			assert.Equal(t, tt.wantFeatures, enabledFeatures(t, applied()), "the applied CR drops cimd")
		})
	}
}

// TestKeycloakGateSkipsTheRegistrarWithoutCIMD: a Keycloak that does not have the cimd feature holds
// no mcp-cimd to remove, so the gate never calls RemoveMCPCIMD and applies the rendered CR as it
// is.
func TestKeycloakGateSkipsTheRegistrarWithoutCIMD(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*otilmv1alpha1.Platform)
		live         func(*otilmv1alpha1.Platform) []client.Object
		wantFeatures []string
	}{
		{
			name: "MCP never turned on", mutate: mcpOff,
			live: func(p *otilmv1alpha1.Platform) []client.Object { return []client.Object{liveKeycloak(p, true)} },
		},
		{
			name: "CIMD off from the start", mutate: cimdOff, wantFeatures: delegationFeatures,
			live: func(p *otilmv1alpha1.Platform) []client.Object {
				return []client.Object{liveKeycloak(p, true, delegationFeatures...)}
			},
		},
		{
			name: "no Keycloak CR yet", mutate: cimdOff, wantFeatures: delegationFeatures,
			live: func(*otilmv1alpha1.Platform) []client.Object { return nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mcpKeycloakPlatform(tt.mutate)
			reg := &localOIDCFake{}
			seed := append([]client.Object{p, keycloakAdminSecret(p)}, tt.live(p)...)
			r, applied := newCIMDGateReconciler(t, reg, nil, seed...)

			_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

			require.NoError(t, err)
			assert.Zero(t, reg.removeCIMDCount(), "no cimd in the live CR: nothing to remove")
			assert.Equal(t, tt.wantFeatures, enabledFeatures(t, applied()))
		})
	}
}

// TestKeycloakGateSkipsTheRegistrarWhileCIMDStays: a rendered Keycloak CR that keeps cimd removes
// nothing, so the gate neither calls the registrar nor holds the apply.
func TestKeycloakGateSkipsTheRegistrarWhileCIMDStays(t *testing.T) {
	p := mcpKeycloakPlatform(nil)
	reg := &localOIDCFake{}
	r, applied := newCIMDGateReconciler(t, reg, nil, p, liveKeycloak(p, true, mcpFeatures...), keycloakAdminSecret(p))

	_, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Zero(t, reg.removeCIMDCount())
	assert.Equal(t, mcpFeatures, enabledFeatures(t, applied()))
}

// TestKeycloakGateAppliesNothingWhenTheLiveCRCannotBeRead: without the live Keycloak CR the gate
// cannot tell whether cimd is going away, so it stops before applying anything, with a leak-free
// error that a refusal for now turns into a retry.
func TestKeycloakGateAppliesNothingWhenTheLiveCRCannotBeRead(t *testing.T) {
	p := mcpKeycloakPlatform(cimdOff)
	reg := &localOIDCFake{}
	r, applied := newCIMDGateReconciler(t, reg, apierrors.NewServiceUnavailable("keycloak read refused"), p, keycloakAdminSecret(p))

	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

	require.Error(t, err)
	assert.Equal(t, "reading the managed Keycloak failed (ServiceUnavailable)", err.Error())
	assert.True(t, isTransient(err), "a read the apiserver refuses for now is retried, not degraded")
	assert.Nil(t, applied(), "no Keycloak CR is applied")
	assert.Zero(t, reg.removeCIMDCount())
}

// cimdRealm is a registrar whose realm tracks MCP's mcp-cimd client policy the way the admin API
// keeps it: EnsureMCPRealm stores it with CIMD on and removes it with CIMD off, and a successful
// RemoveMCPCIMD removes it. stores counts the passes that stored it anew.
type cimdRealm struct {
	*localOIDCFake
	stored bool
	stores int
}

func (f *cimdRealm) EnsureMCPRealm(ctx context.Context, kcURL, realm, user, pass string, spec registration.MCPRealmSpec) (string, error) {
	if spec.CIMD && !f.stored {
		f.stores++
	}
	f.stored = spec.CIMD
	return f.localOIDCFake.EnsureMCPRealm(ctx, kcURL, realm, user, pass, spec)
}

func (f *cimdRealm) RemoveMCPCIMD(ctx context.Context, kcURL, realm, user, pass string) error {
	if err := f.localOIDCFake.RemoveMCPCIMD(ctx, kcURL, realm, user, pass); err != nil {
		return err
	}
	f.stored = false
	return nil
}

// TestReconcileMCPKeycloakFollowsAnOverrideThatDropsCIMD: an override that replaces
// spec.features.enabled without cimd, while spec.mcp.cimd stays on, takes the feature away from
// Keycloak. In one pass, in Reconcile's order and while the old pods still report Ready, the gate
// removes mcp-cimd before it applies the CR without cimd, or keeps cimd when the removal fails. The
// MCP step then keeps CIMD off either way, so its pass leaves no mcp-cimd behind and nothing stores
// it again for a Keycloak that restarts without the feature.
func TestReconcileMCPKeycloakFollowsAnOverrideThatDropsCIMD(t *testing.T) {
	const organization = "organization"
	tests := []struct {
		name         string
		removeErr    error
		wantFeatures []string
	}{
		{
			name:         "the gate removes mcp-cimd and drops cimd",
			wantFeatures: []string{featureTokenExchange, featureParamScopes, organization},
		},
		{
			name: "the removal fails, so the gate keeps cimd for now", removeErr: removalFails,
			wantFeatures: []string{featureTokenExchange, featureParamScopes, organization, platformbuilder.KeycloakFeatureCIMD},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p := mcpKeycloakPlatform(func(p *otilmv1alpha1.Platform) {
				p.Spec.Keycloak.Managed.Overrides = featuresOverride(featureTokenExchange, featureParamScopes, organization)
			})
			realm := &cimdRealm{
				localOIDCFake: &localOIDCFake{mcpSecret: testMCPClientSecretValue, removeCIMDErr: tt.removeErr},
				stored:        true,
			}
			r, applied := newCIMDGateReconciler(t, realm, nil, p, liveKeycloak(p, true, mcpFeatures...), keycloakAdminSecret(p))
			desired := newDesiredSet()

			_, _, err := r.gateKeycloak(ctx, p, guardTestBundle(), desired, true)
			require.NoError(t, err)
			require.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, conditionKeycloakReady),
				"the pods that predate the change still report Ready")
			r.reconcileMCPKeycloak(ctx, p, desired)

			assert.Equal(t, 1, realm.removeCIMDCount(), "the gate removes mcp-cimd before cimd goes")
			assert.Equal(t, tt.wantFeatures, enabledFeatures(t, applied()))
			calls := realm.mcpCallList()
			require.Len(t, calls, 1)
			assert.False(t, calls[0].spec.CIMD, "the rendered Keycloak CR drops cimd, so the step keeps CIMD off")
			assert.False(t, realm.stored, "the realm keeps no mcp-cimd")
			assert.Zero(t, realm.stores, "nothing stores mcp-cimd again")
		})
	}
}

// TestMCPExternalSwitchRemovesCIMDFirst: MCP moves from the managed identity to an external one on a
// managed Keycloak whose live CR still enables cimd. The rendered CR then carries none of MCP's
// features. In Reconcile's order the gate first removes mcp-cimd through the admin API, and applies the
// CR without features once that succeeds, or keeps cimd while the removal fails. Either way the MCP
// step makes no realm pass, so nothing stores mcp-cimd again, and MCP's other Keycloak objects stay.
func TestMCPExternalSwitchRemovesCIMDFirst(t *testing.T) {
	tests := []struct {
		name         string
		removeErr    error
		wantRequeue  bool
		wantFeatures []string
		wantStored   bool
	}{
		{name: "the gate removes mcp-cimd and drops the features"},
		{
			name: "the removal fails, so the gate keeps cimd for now", removeErr: removalFails, wantRequeue: true,
			wantFeatures: []string{platformbuilder.KeycloakFeatureCIMD}, wantStored: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p := mcpExternalPlatform(nil)
			realm := &cimdRealm{
				localOIDCFake: &localOIDCFake{mcpSecret: testMCPClientSecretValue, removeCIMDErr: tt.removeErr},
				stored:        true,
			}
			r, applied := newCIMDGateReconciler(t, realm, nil, p, liveKeycloak(p, true, mcpFeatures...), keycloakAdminSecret(p))
			desired := newDesiredSet()

			_, requeue, err := r.gateKeycloak(ctx, p, guardTestBundle(), desired, true)
			require.NoError(t, err)
			r.reconcileMCPKeycloak(ctx, p, desired)

			assert.Equal(t, tt.wantRequeue, requeue)
			assert.Equal(t, 1, realm.removeCIMDCount(), "the gate removes mcp-cimd before cimd goes")
			assert.Equal(t, tt.wantFeatures, enabledFeatures(t, applied()))
			assert.Empty(t, realm.mcpCallList(), "the MCP step makes no realm pass")
			assert.Equal(t, tt.wantStored, realm.stored)
			assert.Zero(t, realm.stores, "nothing stores mcp-cimd again")
		})
	}
}

// TestKeycloakGateAppliesNothingWhenCIMDCannotBeKept: an override that turns spec.features into a
// scalar leaves no list to keep cimd in, so the gate stops before it applies a CR without cimd.
func TestKeycloakGateAppliesNothingWhenCIMDCannotBeKept(t *testing.T) {
	p := mcpKeycloakPlatform(func(p *otilmv1alpha1.Platform) {
		cimdOff(p)
		raw, err := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"features": "all"}})
		require.NoError(t, err)
		p.Spec.Keycloak.Managed.Overrides = &runtime.RawExtension{Raw: raw}
	})
	reg := &localOIDCFake{removeCIMDErr: removalFails}
	r, applied := newCIMDGateReconciler(t, reg, nil, p, liveKeycloak(p, true, mcpFeatures...), keycloakAdminSecret(p))

	_, _, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.features is not an object")
	assert.Equal(t, 1, reg.removeCIMDCount(), "the removal was tried first")
	assert.Nil(t, applied(), "no Keycloak CR is applied")
}
