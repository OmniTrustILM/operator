/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"encoding/json"
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coreHasOIDCSecretEnv reports whether Core's resolved component carries the
// INTERNAL_OAUTH_SECRET secretKeyRef into the operator-owned OIDC client Secret (optional).
func coreOIDCSecretEnv(c common.Component) (common.SecretEnvRef, bool) {
	for _, e := range c.SecretEnv {
		if e.EnvVar == oidcInternalOAuthEnv {
			return e, true
		}
	}
	return common.SecretEnvRef{}, false
}

// coreHasScriptsVolume reports whether Core mounts the core-scripts ConfigMap volume.
func coreHasScriptsVolume(c common.Component) bool {
	for _, v := range c.Volumes {
		if v.Name == oidcScriptsVolumeName && v.ConfigMap != nil {
			return true
		}
	}
	return false
}

// TestCoreManagedKeycloakGetsInPodOIDCWiring verifies that for a MANAGED Keycloak, ResolveCore
// layers the in-pod OIDC wiring: the INTERNAL_OAUTH_SECRET secretKeyRef (optional) into the
// operator-owned OIDC client Secret, the core-scripts ConfigMap volume + read-only mount, and
// a lifecycle.postStart exec running register-internal-keycloak.sh with $INTERNAL_OAUTH_SECRET.
func TestCoreManagedKeycloakGetsInPodOIDCWiring(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: "ilm.example.com"}
	})
	c := ResolveCore(p)

	// INTERNAL_OAUTH_SECRET via secretKeyRef from <platform>-oidc-client / clientSecret, optional.
	env, ok := coreOIDCSecretEnv(c)
	require.True(t, ok, "managed Keycloak must wire INTERNAL_OAUTH_SECRET on Core")
	assert.Equal(t, OIDCClientSecretName(p), env.SecretName)
	assert.Equal(t, OIDCClientSecretKey, env.SecretKey)
	assert.True(t, env.Optional, "the secretKeyRef must be optional so Core starts before the Secret exists (no deadlock)")

	// Scripts ConfigMap volume + read-only mount at /opt/ilm/scripts.
	assert.True(t, coreHasScriptsVolume(c), "managed Keycloak must mount the core-scripts ConfigMap volume")
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == oidcScriptsVolumeName {
			mounted = true
			assert.Equal(t, oidcScriptsMountPath, m.MountPath)
			assert.True(t, m.ReadOnly, "the scripts mount must be read-only")
		}
	}
	assert.True(t, mounted, "the scripts volume must be mounted on Core")

	// lifecycle.postStart exec runs the script with $INTERNAL_OAUTH_SECRET.
	require.NotNil(t, c.Lifecycle, "managed Keycloak must add a Core lifecycle")
	require.NotNil(t, c.Lifecycle.PostStart, "the lifecycle must carry a postStart hook")
	require.NotNil(t, c.Lifecycle.PostStart.Exec, "the postStart must be an exec hook")
	cmd := strings.Join(c.Lifecycle.PostStart.Exec.Command, " ")
	assert.Contains(t, cmd, oidcScriptsMountPath+"/"+oidcScriptName, "the postStart must run the mounted script")
	assert.Contains(t, cmd, "$"+oidcInternalOAuthEnv, "the postStart must pass the client secret env to the script")
}

// TestCoreExternalKeycloakNoInPodOIDCWiring verifies that for an EXTERNAL Keycloak, ResolveCore
// renders NONE of the in-pod OIDC wiring (no INTERNAL_OAUTH_SECRET env, no scripts volume, no
// lifecycle) — the out-of-the-box / external render is unchanged.
func TestCoreExternalKeycloakNoInPodOIDCWiring(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
	})
	c := ResolveCore(p)

	_, ok := coreOIDCSecretEnv(c)
	assert.False(t, ok, "external Keycloak must NOT wire INTERNAL_OAUTH_SECRET")
	assert.False(t, coreHasScriptsVolume(c), "external Keycloak must NOT mount the scripts ConfigMap")
	assert.Nil(t, c.Lifecycle, "external Keycloak must NOT add a Core postStart lifecycle")
}

// TestCoreNoKeycloakBlockNoInPodOIDCWiring verifies a nil keycloak block (external by default)
// renders no in-pod OIDC wiring.
func TestCoreNoKeycloakBlockNoInPodOIDCWiring(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak = nil })
	c := ResolveCore(p)
	_, ok := coreOIDCSecretEnv(c)
	assert.False(t, ok)
	assert.False(t, coreHasScriptsVolume(c))
	assert.Nil(t, c.Lifecycle)
}

// TestBuildCoreScriptsConfigMapManaged verifies the scripts ConfigMap is rendered for a managed
// Keycloak with the register-internal-keycloak.sh script, and that the script (a) PUTs the
// LOCALHOST settings endpoint (the whole point of the in-pod rework), (b) carries the browser-
// facing URLs from the edge host + the back-channel URLs from the in-cluster Keycloak Service,
// (c) uses the ilm clientId + audiences ["ilm"], and (d) takes the secret as $1 (no inline secret).
func TestBuildCoreScriptsConfigMapManaged(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: "ilm.example.com"}
	})
	cm := BuildCoreScriptsConfigMap(p)
	require.NotNil(t, cm, "managed Keycloak must render the core-scripts ConfigMap")
	assert.Equal(t, OIDCScriptsConfigMapName(p), cm.Name)
	assert.Equal(t, p.Namespace, cm.Namespace)

	script, ok := cm.Data[oidcScriptName]
	require.True(t, ok, "the ConfigMap must carry %q", oidcScriptName)

	// THE in-pod invariant: the PUT targets http://localhost:<coreport>/...
	assert.Contains(t, script, "http://localhost:8080/api/v1/settings/authentication/oauth2Providers/internal",
		"the script must PUT the LOCALHOST settings endpoint (Core's settings API is localhost-only)")
	// Localhost readiness waits (Core port + OPA port).
	assert.Contains(t, script, "nc -z localhost 8080")
	assert.Contains(t, script, "nc -z localhost 8181")

	// Browser-facing URLs from the edge host (HTTPS).
	assert.Contains(t, script, `"issuerUrl":"https://ilm.example.com/kc/realms/ilm"`)
	assert.Contains(t, script, "https://ilm.example.com/kc/realms/ilm/protocol/openid-connect/auth")
	assert.Contains(t, script, "https://ilm.example.com/kc/realms/ilm/protocol/openid-connect/logout")
	assert.Contains(t, script, "https://ilm.example.com/administrator/")

	// Back-channel URLs from the in-cluster Keycloak Service (HTTP, namespace-qualified).
	assert.Contains(t, script, "http://ilm-keycloak-service.ns:8080/kc/realms/ilm/protocol/openid-connect/token")
	assert.Contains(t, script, "http://ilm-keycloak-service.ns:8080/kc/realms/ilm/protocol/openid-connect/certs")

	// clientId ilm + audiences ["ilm"]; the secret comes from $1 (never inlined).
	assert.Contains(t, script, `"clientId":"ilm"`)
	assert.Contains(t, script, `"audiences":["ilm"]`)
	assert.Contains(t, script, "CLIENT_SECRET=$1", "the client secret must be the positional $1, never inlined")
	assert.Contains(t, script, `-d "${BODY_HEAD}${CLIENT_SECRET}${BODY_TAIL}"`,
		"the secret is concatenated at runtime inside ONE double-quoted expansion")

	// The empty-secret branch SKIPS gracefully (exit 0) and never `exit 1`: during early
	// bring-up $INTERNAL_OAUTH_SECRET resolves empty (the operator relays the Secret only after
	// Keycloak is Ready), and a non-zero postStart exit would crash-loop Core. The operator's
	// config-checksum roll re-runs this hook WITH the secret once it appears.
	assert.Contains(t, script, "exit 0", "the empty-secret branch must skip gracefully (exit 0)")
	assert.NotContains(t, script, "exit 1", "an empty secret must NOT fail the postStart (would crash-loop Core)")
	assert.Contains(t, script, "skipping internal OIDC provider registration",
		"the empty-secret branch must log that it is skipping registration")
}

// TestBuildCoreScriptsConfigMapExternalNil verifies no scripts ConfigMap is rendered for an
// external / absent Keycloak.
func TestBuildCoreScriptsConfigMapExternalNil(t *testing.T) {
	ext := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
	})
	assert.Nil(t, BuildCoreScriptsConfigMap(ext), "external Keycloak renders no scripts ConfigMap")

	none := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak = nil })
	assert.Nil(t, BuildCoreScriptsConfigMap(none), "no keycloak block renders no scripts ConfigMap")
}

// TestBuildCoreScriptsConfigMapNoEdgeFallsBack verifies that with NO edge host the script's
// browser-facing URLs fall back to the back-channel realm base so the body stays well-formed
// (a dev/test posture; production sets a hostname).
func TestBuildCoreScriptsConfigMapNoEdgeFallsBack(t *testing.T) {
	p := managedKCPlatform(nil) // no edge
	cm := BuildCoreScriptsConfigMap(p)
	require.NotNil(t, cm)
	script := cm.Data[oidcScriptName]
	// issuerUrl falls back to the back-channel realm base (no https://<host>).
	assert.Contains(t, script, `"issuerUrl":"http://ilm-keycloak-service.ns:8080/kc/realms/ilm"`)
	assert.NotContains(t, script, "https://", "with no edge host there are no browser-facing https URLs")
}

// TestRenderPlatformBaseIncludesScriptsConfigMapForManagedKeycloak verifies the scripts
// ConfigMap is part of the rendered base set for a managed Keycloak, and absent for external.
func TestRenderPlatformBaseIncludesScriptsConfigMapForManagedKeycloak(t *testing.T) {
	managed := managedKCPlatform(nil)
	var found bool
	for _, o := range RenderPlatformBase(managed) {
		if o.GetName() == OIDCScriptsConfigMapName(managed) {
			found = true
		}
	}
	assert.True(t, found, "RenderPlatformBase must include the core-scripts ConfigMap for a managed Keycloak")

	external := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
	})
	for _, o := range RenderPlatformBase(external) {
		assert.NotEqual(t, OIDCScriptsConfigMapName(external), o.GetName(),
			"external Keycloak must not render the core-scripts ConfigMap")
	}
}

// adminCertPlatform builds a certificate-admin platform (no managed Keycloak) for the in-pod
// register-admin.sh wiring tests.
func adminCertPlatform() *otilmv1alpha1.Platform {
	return adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled: true, Username: "admin", Name: "Platform Administrator", LastName: "Admin", Email: "admin@example.com",
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
	})
}

// TestCoreCertAdminGetsInPodRegisterScript verifies that with the certificate admin method
// enabled (and no managed Keycloak), ResolveCore wires the in-pod register-admin.sh: the
// core-scripts volume + a postStart that runs register-admin.sh (and NOT the OIDC script), with
// no INTERNAL_OAUTH_SECRET env. Core's local-admin API is localhost-only, so this runs in-pod.
func TestCoreCertAdminGetsInPodRegisterScript(t *testing.T) {
	c := ResolveCore(adminCertPlatform())
	assert.True(t, coreHasScriptsVolume(c), "cert admin must mount the core-scripts volume")
	_, hasOIDC := coreOIDCSecretEnv(c)
	assert.False(t, hasOIDC, "cert admin alone must NOT add INTERNAL_OAUTH_SECRET (no managed Keycloak)")
	require.NotNil(t, c.Lifecycle)
	require.NotNil(t, c.Lifecycle.PostStart)
	require.NotNil(t, c.Lifecycle.PostStart.Exec)
	cmd := strings.Join(c.Lifecycle.PostStart.Exec.Command, " ")
	assert.Contains(t, cmd, oidcScriptsMountPath+"/"+adminScriptName, "postStart must run register-admin.sh")
	assert.NotContains(t, cmd, oidcScriptName, "no managed Keycloak → no OIDC script in the postStart")
}

// TestCoreAdminAndKeycloakRunBothScripts verifies that when BOTH the certificate admin and a
// managed Keycloak are configured, Core's postStart runs register-admin.sh AND
// register-internal-keycloak.sh, and the scripts ConfigMap holds both.
func TestCoreAdminAndKeycloakRunBothScripts(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
			Enabled: true, Username: "admin",
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
		}
	})
	c := ResolveCore(p)
	require.NotNil(t, c.Lifecycle)
	cmd := strings.Join(c.Lifecycle.PostStart.Exec.Command, " ")
	assert.Contains(t, cmd, oidcScriptsMountPath+"/"+adminScriptName, "postStart runs the admin script")
	assert.Contains(t, cmd, oidcScriptsMountPath+"/"+oidcScriptName, "postStart runs the OIDC script")
	_, hasOIDC := coreOIDCSecretEnv(c)
	assert.True(t, hasOIDC, "managed Keycloak adds INTERNAL_OAUTH_SECRET")

	cm := BuildCoreScriptsConfigMap(p)
	require.NotNil(t, cm)
	assert.Contains(t, cm.Data, adminScriptName)
	assert.Contains(t, cm.Data, oidcScriptName)
}

// TestRegisterAdminScriptContent verifies register-admin.sh targets Core's LOCALHOST local-admin
// API with the operator-resolved identity and reads/strips the cert from $ADMIN_CERT — i.e.
// in-pod, never a cross-pod core.<ns> Service URL.
func TestRegisterAdminScriptContent(t *testing.T) {
	cm := BuildCoreScriptsConfigMap(adminCertPlatform())
	require.NotNil(t, cm)
	s := cm.Data[adminScriptName]
	assert.Contains(t, s, "http://localhost:8080/api/v1/local/admins", "must POST to Core's localhost local-admin API")
	assert.Contains(t, s, `"username":"admin"`)
	assert.Contains(t, s, `"firstName":"Platform Administrator"`)
	assert.Contains(t, s, `"lastName":"Admin"`)
	assert.Contains(t, s, `"email":"admin@example.com"`)
	assert.Contains(t, s, "$ADMIN_CERT", "must read the admin cert from the ADMIN_CERT env")
	assert.Contains(t, s, "echo $ADMIN_CERT", "echo must be UNQUOTED to flatten the multi-line PEM — a quoted echo leaves embedded newlines that break the JSON body")
	assert.Contains(t, s, "BEGINCERTIFICATE", "must strip the PEM armor")
	assert.NotContains(t, s, "core.", "must target localhost, NOT a cross-pod core.<ns> Service URL")
}

// TestBuildCoreScriptsConfigMapNeitherNil verifies no scripts ConfigMap is rendered when neither
// the certificate admin nor a managed Keycloak applies.
func TestBuildCoreScriptsConfigMapNeitherNil(t *testing.T) {
	assert.Nil(t, BuildCoreScriptsConfigMap(basePlatform()),
		"no cert admin + no managed Keycloak → no scripts ConfigMap")
}

// ---- in-pod script request bodies: JSON-composed, heredoc-delivered ---------

// quotedHeredocBodies returns the bodies of every QUOTED heredoc (<<'EOF') in a rendered script
// plus "outside" — the concatenation of everything else, i.e. the text the shell actually
// executes. It fails the test unless at least one quoted heredoc is present, which is the
// property that makes a CR-supplied value inert.
func quotedHeredocBodies(t *testing.T, script string) (bodies []string, outside string) {
	t.Helper()
	const openTok, closeTok = "<<'EOF'\n", "\nEOF\n"
	rest := script
	for {
		i := strings.Index(rest, openTok)
		if i < 0 {
			outside += rest
			break
		}
		outside += rest[:i+len(openTok)]
		rest = rest[i+len(openTok):]
		j := strings.Index(rest, closeTok)
		require.GreaterOrEqual(t, j, 0, "a quoted heredoc was opened but never terminated")
		bodies = append(bodies, rest[:j])
		rest = rest[j:]
	}
	require.NotEmpty(t, bodies, "the request body must be emitted inside a QUOTED heredoc (<<'EOF')")
	return bodies, outside
}

// shellStatements returns a script with its comment lines removed, so an assertion about the
// COMMANDS a script runs is not confounded by prose in the surrounding comments.
func shellStatements(script string) string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// hostileValues is the shared table of values that, in the earlier shape (interpolated into a
// SINGLE-quoted `curl -d '...'` body), would have closed the quote and handed the remainder to
// the shell. The last entry is not an attack at all: an apostrophe is legitimate in a human name
// and the old shape broke on it, so O'Brien is a regression test for real user data.
func hostileValues() []struct {
	name  string
	value string
} {
	return []struct {
		name  string
		value string
	}{
		{name: "command substitution", value: "$(id)"},
		{name: "backtick command substitution", value: "`id`"},
		{name: "embedded double quote", value: `a"b`},
		{name: "embedded single quote", value: `a'b`},
		{name: "embedded newline", value: "a\nb"},
		{name: "variable reference", value: "a$VAR"},
		{name: "command separator", value: "a;b"},
		{name: "pipeline", value: "a|b"},
		{name: "the legitimate surname O'Brien", value: "O'Brien"},
	}
}

// TestRegisterAdminScriptBodyIsJSONEncoded is the structural guard for register-admin.sh's
// request body. For each hostile (or merely apostrophe-bearing) value placed in EVERY
// CR-supplied identity field it asserts:
//
//   - the body is emitted inside a QUOTED heredoc, so the shell expands nothing in it;
//   - the value never appears in text the shell executes;
//   - the heredoc body round-trips through json.Unmarshal with the field values EXACTLY equal to
//     the inputs — so O'Brien reaches Core as O'Brien instead of breaking the request;
//   - no single-quoted `-d '...'` body carrying CR data remains.
func TestRegisterAdminScriptBodyIsJSONEncoded(t *testing.T) {
	for _, tc := range hostileValues() {
		t.Run(tc.name, func(t *testing.T) {
			p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Username:    tc.value,
				Name:        tc.value,
				LastName:    tc.value,
				Email:       tc.value,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
			})
			cm := BuildCoreScriptsConfigMap(p)
			require.NotNil(t, cm)
			script := cm.Data[adminScriptName]

			// STRUCTURE: one quoted heredoc holds the whole body; the old single-quoted
			// `-d '...'` construction is gone for good.
			bodies, outside := quotedHeredocBodies(t, script)
			require.Len(t, bodies, 1, "register-admin.sh emits exactly one heredoc body")
			assert.Contains(t, script, "<<'EOF'", "the heredoc delimiter must be QUOTED")
			assert.Contains(t, script, `-d "${BODY}"`, "curl must send the composed body")
			assert.NotContains(t, script, "-d '", "no single-quoted body may carry CR data")
			assert.NotContains(t, bodies[0], "\n",
				"the JSON body is one line, so no value can forge the heredoc terminator")

			// ROUND-TRIP: the body is valid JSON and every field equals its input exactly.
			var got adminRequest
			require.NoError(t, json.Unmarshal([]byte(bodies[0]), &got),
				"the heredoc body must be valid JSON (encoding/json composed it)")
			assert.Equal(t, tc.value, got.Username)
			assert.Equal(t, tc.value, got.FirstName)
			assert.Equal(t, tc.value, got.LastName)
			assert.Equal(t, tc.value, got.Email)
			assert.True(t, got.Enabled, "the admin is registered enabled")
			assert.Equal(t, adminCertPlaceholder, got.CertificateData,
				"the certificate slot stays a placeholder the pod fills in at runtime")

			// INERTNESS: the value appears ONLY in the heredoc body.
			assert.NotContains(t, outside, tc.value,
				"the identity value must not appear anywhere the shell evaluates it")
		})
	}
}

// TestRegisterAdminScriptOmitsUnsetLastName locks the optional-surname behaviour through the
// JSON path: an unset spec.registerAdmin.lastName omits the KEY entirely (omitempty) rather than
// POSTing an empty "lastName" to Core.
func TestRegisterAdminScriptOmitsUnsetLastName(t *testing.T) {
	p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled: true, Username: "admin", Name: "Platform Administrator", Email: "admin@example.com",
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
	})
	cm := BuildCoreScriptsConfigMap(p)
	require.NotNil(t, cm)
	bodies, _ := quotedHeredocBodies(t, cm.Data[adminScriptName])
	require.Len(t, bodies, 1)
	assert.NotContains(t, bodies[0], "lastName", "an unset surname must omit the JSON field entirely")

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodies[0]), &raw))
	assert.NotContains(t, raw, "lastName")
}

// TestRegisterAdminScriptCertSubstitutionIsBase64Safe locks the ONE runtime substitution in
// register-admin.sh: the base64 $CERT replaces the placeholder via sed with a "|" delimiter.
// Base64's charset (A-Za-z0-9+/=) contains neither the delimiter nor sed's "&" nor a backslash,
// so the certificate's contents cannot subvert the substitution.
func TestRegisterAdminScriptCertSubstitutionIsBase64Safe(t *testing.T) {
	cm := BuildCoreScriptsConfigMap(adminCertPlatform())
	require.NotNil(t, cm)
	script := cm.Data[adminScriptName]
	assert.Contains(t, script, `sed "s|`+adminCertPlaceholder+`|${CERT}|"`,
		"the cert substitution must use a | delimiter, which base64 cannot contain")
	assert.NotContains(t, script, `"'"$CERT"'"`,
		"the cert must no longer be spliced into a single-quoted shell body")
}

// TestRegisterInternalKeycloakScriptBodyIsJSONEncoded is the structural guard for
// register-internal-keycloak.sh's request body. spec.keycloak.realm and the platform hostname
// both reach it through composed URLs, so each is driven with every hostile value and the
// assertions mirror the admin script's: quoted heredocs, the value never in executable text, and
// a body that round-trips through json.Unmarshal with the URLs exactly as composed.
func TestRegisterInternalKeycloakScriptBodyIsJSONEncoded(t *testing.T) {
	fields := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform, string)
	}{
		{
			name: "keycloak realm",
			mutate: func(p *otilmv1alpha1.Platform, v string) {
				p.Spec.Keycloak.Realm = v
			},
		},
		{
			name: "platform hostName",
			mutate: func(p *otilmv1alpha1.Platform, v string) {
				p.Spec.Common.HostName = v
			},
		},
		{
			name: "edge host",
			mutate: func(p *otilmv1alpha1.Platform, v string) {
				p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: v}
			},
		},
	}
	for _, f := range fields {
		for _, tc := range hostileValues() {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				p := managedKCPlatform(nil)
				f.mutate(p, tc.value)
				cm := BuildCoreScriptsConfigMap(p)
				require.NotNil(t, cm)
				script := cm.Data[oidcScriptName]

				// STRUCTURE: two quoted heredocs (the body split at the secret slot), and no
				// single-quoted body carrying CR data.
				bodies, outside := quotedHeredocBodies(t, script)
				require.Len(t, bodies, 2, "the body is split at the clientSecret slot")
				assert.Contains(t, script, "<<'EOF'", "the heredoc delimiters must be QUOTED")
				assert.NotContains(t, script, "-d '", "no single-quoted body may carry CR data")
				for _, b := range bodies {
					assert.NotContains(t, b, "\n",
						"each half is one line, so no value can forge the heredoc terminator")
				}

				// ROUND-TRIP: rejoining the halves around a secret yields valid JSON whose URLs
				// carry the value verbatim.
				const secret = "s3cr3t-from-keycloak"
				var got oidcProviderRequest
				require.NoError(t, json.Unmarshal([]byte(bodies[0]+secret+bodies[1]), &got),
					"the rejoined body must be valid JSON (encoding/json composed it)")
				assert.Equal(t, secret, got.ClientSecret,
					"the runtime secret lands in the clientSecret slot")
				assert.Contains(t, got.IssuerURL, tc.value, "the issuer URL carries the value verbatim")
				assert.Equal(t, OIDCClientID, got.ClientID)
				assert.Equal(t, []string{oidcScopeOpenID}, got.Scope)
				assert.Equal(t, []string{OIDCClientID}, got.Audiences)
				assert.Equal(t, oidcSkewSeconds, got.Skew)

				// INERTNESS: the value appears ONLY inside the heredoc bodies.
				assert.NotContains(t, outside, tc.value,
					"the value must not appear anywhere the shell evaluates it")
			})
		}
	}
}

// TestRegisterInternalKeycloakScriptKeepsSecretOutOfExtraArgv locks the secret-handling
// invariant of the split-body shape: the secret is concatenated inside ONE double-quoted
// expansion in curl's -d argument (its exposure is unchanged from before), and it never enters a
// sed program or any other command.
func TestRegisterInternalKeycloakScriptKeepsSecretOutOfExtraArgv(t *testing.T) {
	cm := BuildCoreScriptsConfigMap(managedKCPlatform(nil))
	require.NotNil(t, cm)
	script := cm.Data[oidcScriptName]

	assert.Contains(t, script, `-d "${BODY_HEAD}${CLIENT_SECRET}${BODY_TAIL}"`,
		"the secret must be concatenated inside ONE double-quoted expansion")
	assert.NotContains(t, shellStatements(script), "sed",
		"the client secret must never enter a sed program")
	assert.Equal(t, 1, strings.Count(script, "${CLIENT_SECRET}"),
		"the secret is referenced exactly once, in curl's -d argument")
	assert.NotContains(t, script, clientSecretPlaceholder,
		"the placeholder is consumed by the split; it must not survive into the script")
}
