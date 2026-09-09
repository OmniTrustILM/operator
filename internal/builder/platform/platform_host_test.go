/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// platform_host_test.go pins the PlatformHost precedence — the single source of the
// platform's public FQDN (spec.edge.host overriding spec.common.hostName) — and proves it
// fans out to EVERY external-host consumer at once: the edge Ingress host + cert SAN, the
// managed Keycloak KC_HOSTNAME (spec.hostname), the ilm client's OIDC redirect/web-origin/
// post-logout URIs, the in-pod OIDC registration script's browser-facing URLs, and the
// gateway CORS origin default. The three cases are exactly the ones that would otherwise only
// surface on a real cluster:
//
//   (a) common.hostName set + edge.host empty → every consumer renders with common.hostName;
//   (b) edge.host set (alongside a different common.hostName) → edge.host WINS;
//   (c) bring-your-own edge (edge.enabled=false) + common.hostName set → Keycloak/OIDC still
//       receive the FQDN (the BYO-ingress posture: the operator renders no edge object, yet
//       the platform must still wire its public address into Keycloak/OIDC).

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// kcHostname extracts spec.hostname.hostname (KC_HOSTNAME source) from the rendered managed
// Keycloak CR.
func kcHostname(t *testing.T, p *otilmv1alpha1.Platform) (string, bool) {
	t.Helper()
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc, "a managed Keycloak must render a Keycloak CR")
	h, found, err := unstructured.NestedString(kc.Object, "spec", "hostname", "hostname")
	require.NoError(t, err)
	return h, found
}

// ilmClientURIs extracts the ilm client's redirect/web-origin/post-logout values from the
// default realm.
func ilmClientURIs(t *testing.T, p *otilmv1alpha1.Platform) (redirects, origins []string, postLogout string) {
	t.Helper()
	c := DefaultKeycloakRealm(p)["clients"].([]interface{})[0].(map[string]interface{})
	redirects, _, _ = unstructured.NestedStringSlice(map[string]interface{}{"r": c["redirectUris"]}, "r")
	origins, _, _ = unstructured.NestedStringSlice(map[string]interface{}{"r": c["webOrigins"]}, "r")
	postLogout = c["attributes"].(map[string]interface{})["post.logout.redirect.uris"].(string)
	return redirects, origins, postLogout
}

// corsOrigins extracts the cors plugin's origins from the rendered global (kong) ConfigMap.
func corsOrigins(t *testing.T, p *otilmv1alpha1.Platform) []interface{} {
	t.Helper()
	cfg := parseKong(t, BuildGlobalConfigMap(p))
	for _, pl := range cfg.Plugins {
		if pl.Name == "cors" {
			return pl.Config["origins"].([]interface{})
		}
	}
	require.Fail(t, "cors plugin not found in kong.yml")
	return nil
}

// TestPlatformHostPrecedence is the focused unit test of the resolver: edge.host (on an
// enabled edge) wins; otherwise common.hostName; otherwise "".
func TestPlatformHostPrecedence(t *testing.T) {
	const commonH = "canonical.example.com"
	t.Run("edge.host wins over common.hostName", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Common.HostName = commonH
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: "edge.example.com"}
		assert.Equal(t, "edge.example.com", PlatformHost(p), "an enabled edge.host overrides common.hostName")
	})
	t.Run("common.hostName when edge.host empty", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Common.HostName = commonH
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true} // enabled edge, no host
		assert.Equal(t, commonH, PlatformHost(p), "empty edge.host falls through to common.hostName")
	})
	t.Run("common.hostName when edge disabled (BYO ingress)", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Common.HostName = commonH
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false, Host: "ignored.example.com"}
		assert.Equal(t, commonH, PlatformHost(p),
			"a disabled edge contributes no host; common.hostName is the FQDN even with BYO ingress")
	})
	t.Run("common.hostName when no edge block", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Common.HostName = commonH
		assert.Equal(t, commonH, PlatformHost(p))
	})
	t.Run("empty when neither set", func(t *testing.T) {
		assert.Empty(t, PlatformHost(basePlatform()), "no edge.host and no common.hostName → host-agnostic render")
	})
}

// TestPlatformHostFanoutFromCommonHostName is case (a): common.hostName set, edge.host empty.
// EVERY consumer (edge Ingress host + cert SAN, KC_HOSTNAME, ilm client URIs, OIDC script,
// CORS default) must derive from common.hostName.
func TestPlatformHostFanoutFromCommonHostName(t *testing.T) {
	const host = "ilm.canonical.example.com"
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Common.HostName = host
		// An ENABLED edge with NO host: the edge renders, but its host must come from
		// common.hostName (edge.host is empty).
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress", ClassName: strPtr("nginx")}
		p.Spec.Gateway.Cors.Enabled = true
	})

	// Edge Ingress: rule host AND TLS SAN both from common.hostName.
	ing := findIngress(t, ResolveEdge(p))
	require.Len(t, ing.Spec.Rules, 1)
	assert.Equal(t, host, ing.Spec.Rules[0].Host, "Ingress rule host from common.hostName when edge.host is empty")
	require.Len(t, ing.Spec.TLS, 1)
	assert.Equal(t, []string{host}, ing.Spec.TLS[0].Hosts, "Ingress TLS SAN from common.hostName")

	// Keycloak KC_HOSTNAME (spec.hostname.hostname) from common.hostName — full URL (scheme + /kc).
	kcHost, found := kcHostname(t, p)
	require.True(t, found, "with a platform host, Keycloak gets a fixed spec.hostname.hostname")
	assert.Equal(t, testHTTPSScheme+host+"/kc", kcHost, "KC_HOSTNAME (full URL) from common.hostName")

	// ilm client redirect/web-origin/post-logout from common.hostName.
	redirects, origins, postLogout := ilmClientURIs(t, p)
	assert.Equal(t, []string{testHTTPSScheme + host + testRedirectPath}, redirects, "ilm redirect (Core's OAuth2 callback) from common.hostName")
	assert.Equal(t, []string{testHTTPSScheme + host}, origins, "ilm web origin from common.hostName")
	assert.Equal(t, testHTTPSScheme+host+testAdminPath, postLogout, "ilm post-logout from common.hostName")

	// In-pod OIDC script's browser-facing issuer from common.hostName.
	script := BuildCoreScriptsConfigMap(p).Data[oidcScriptName]
	assert.Contains(t, script, `"issuerUrl":"https://`+host+`/kc/realms/ilm"`, "OIDC issuer from common.hostName")

	// CORS origin default tightens to the platform origin.
	assert.Equal(t, []interface{}{testHTTPSScheme + host}, corsOrigins(t, p), "CORS default origin from common.hostName")
}

// TestPlatformHostEdgeHostWins is case (b): edge.host set ALONGSIDE a different
// common.hostName. edge.host must win at every consumer (the per-edge override).
func TestPlatformHostEdgeHostWins(t *testing.T) {
	const edgeH = "ilm.edge.example.com"
	const commonH = "ilm.canonical.example.com"
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Common.HostName = commonH
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress", ClassName: strPtr("nginx"), Host: edgeH}
		p.Spec.Gateway.Cors.Enabled = true
	})

	ing := findIngress(t, ResolveEdge(p))
	assert.Equal(t, edgeH, ing.Spec.Rules[0].Host, "edge.host wins for the Ingress host")
	assert.Equal(t, []string{edgeH}, ing.Spec.TLS[0].Hosts, "edge.host wins for the TLS SAN")

	kcHost, found := kcHostname(t, p)
	require.True(t, found)
	assert.Equal(t, testHTTPSScheme+edgeH+"/kc", kcHost, "edge.host wins for KC_HOSTNAME (full URL)")

	redirects, origins, postLogout := ilmClientURIs(t, p)
	assert.Equal(t, []string{testHTTPSScheme + edgeH + testRedirectPath}, redirects, "edge.host wins for the ilm redirect")
	assert.Equal(t, []string{testHTTPSScheme + edgeH}, origins, "edge.host wins for the ilm web origin")
	assert.Equal(t, testHTTPSScheme+edgeH+testAdminPath, postLogout, "edge.host wins for the ilm post-logout")

	script := BuildCoreScriptsConfigMap(p).Data[oidcScriptName]
	assert.Contains(t, script, `"issuerUrl":"https://`+edgeH+`/kc/realms/ilm"`, "edge.host wins for the OIDC issuer")
	assert.NotContains(t, script, commonH, "common.hostName must not leak when edge.host overrides it")

	assert.Equal(t, []interface{}{testHTTPSScheme + edgeH}, corsOrigins(t, p), "edge.host wins for the CORS default origin")
}

// TestPlatformHostBYOEdgeStillWiresKeycloakOIDC is case (c): bring-your-own edge
// (edge.enabled=false) + common.hostName set. The operator renders NO edge object, yet
// Keycloak/OIDC/CORS must still receive the FQDN — the whole point of common.hostName.
func TestPlatformHostBYOEdgeStillWiresKeycloakOIDC(t *testing.T) {
	const host = "ilm.byo.example.com"
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Common.HostName = host
		// BYO ingress: an explicitly disabled edge (the user runs their own ingress).
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false}
		p.Spec.Gateway.Cors.Enabled = true
	})

	// No operator-rendered edge object (the user owns the ingress).
	assert.Nil(t, ResolveEdge(p), "a disabled edge renders no edge objects (BYO ingress)")

	// Keycloak STILL gets a fixed KC_HOSTNAME from common.hostName (so it does not fall back to
	// hostname.strict=false despite the absent edge). KC_HOSTNAME is the full public URL —
	// scheme + the /kc relative path — so Keycloak advertises https:// URLs behind the gateway.
	kcHost, found := kcHostname(t, p)
	require.True(t, found, "BYO edge + common.hostName must still set a fixed Keycloak hostname")
	assert.Equal(t, testHTTPSScheme+host+"/kc", kcHost, "KC_HOSTNAME (full URL) from common.hostName under BYO edge")

	// ilm client + OIDC script still carry the FQDN.
	redirects, origins, postLogout := ilmClientURIs(t, p)
	assert.Equal(t, []string{testHTTPSScheme + host + testRedirectPath}, redirects)
	assert.Equal(t, []string{testHTTPSScheme + host}, origins)
	assert.Equal(t, testHTTPSScheme+host+testAdminPath, postLogout)

	script := BuildCoreScriptsConfigMap(p).Data[oidcScriptName]
	assert.Contains(t, script, `"issuerUrl":"https://`+host+`/kc/realms/ilm"`,
		"OIDC issuer from common.hostName even with no operator edge")

	assert.Equal(t, []interface{}{testHTTPSScheme + host}, corsOrigins(t, p), "CORS default origin from common.hostName under BYO edge")
}
