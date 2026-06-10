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

// oidc_core.go renders Core's IN-POD bootstrap wiring: a ConfigMap holding the bootstrap
// scripts mounted on Core, plus a single lifecycle.postStart hook that runs whichever apply —
// register-admin.sh (the first-admin registration, certificate method) and/or
// register-internal-keycloak.sh (the managed-Keycloak OIDC provider registration).
//
// WHY IN-POD: BOTH of Core's target endpoints are effectively LOCALHOST-ONLY — the local-admin
// API (POST /api/v1/local/admins) and the settings OIDC API
// (PUT /api/v1/settings/authentication/oauth2Providers/internal). They must be called from
// inside the Core pod via http://localhost:<coreport>/...; a cross-pod call from the operator
// (core.<ns>:8080) is rejected (401 / transport error). So instead of the operator calling Core,
// the operator:
//   - renders a ConfigMap holding the applicable script(s), mounted on Core;
//   - for the cert admin: register-admin.sh reads $ADMIN_CERT (the admin client cert, sourced via
//     secretKeyRef on Core by ResolveCore), strips it, and POSTs the first admin;
//   - for managed Keycloak: injects $INTERNAL_OAUTH_SECRET via secretKeyRef from the operator-owned
//     OIDC client Secret (<platform>-oidc-client / clientSecret), which the reconciler populates
//     with the Keycloak-GENERATED "ilm" client secret (no operator-minted credential), and
//     register-internal-keycloak.sh PUTs the provider;
//   - adds ONE lifecycle.postStart exec that runs the applicable script(s) in sequence.
//
// Each script's localhost-wait (nc -z localhost <coreport>, then localhost:<opaport>) makes the
// postStart robust against Core not having finished binding its port yet, and both POST/PUT are
// idempotent (the postStart is fire-and-forget). The OIDC provider config splits horizons:
// issuer/auth/logout/postLogout are BROWSER-FACING (the platform hostname over HTTPS); token/jwks
// are BACK-CHANNEL (the in-cluster Keycloak Service over HTTP).
//
// SCC SAFETY: this adds only a ConfigMap volume, secretKeyRef envs, and a postStart exec — no
// new container, no new privilege, no writable path. The Core container stays SCC-clean
// (restricted-v2): the postStart runs in the existing container's security context.

import (
	"fmt"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// oidcClientSecretSuffix names the operator-owned OIDC client Secret the reconciler
	// populates with the Keycloak-generated "ilm" client secret: "<platform>-oidc-client".
	oidcClientSecretSuffix = "-oidc-client" //nolint:gosec // G101: a Secret NAME suffix, not a credential value
	// OIDCClientSecretKey is the key inside that Secret holding the client secret value. It is
	// exported so the reconciler (which writes the Secret) and this builder (which references
	// it from Core) share ONE definition and never drift.
	OIDCClientSecretKey = "clientSecret" //nolint:gosec // G101: a Secret KEY name, not a credential value
	// oidcInternalOAuthEnv is the env var Core's postStart script reads the client secret from
	// ($INTERNAL_OAUTH_SECRET). Sourced via secretKeyRef — never inlined.
	oidcInternalOAuthEnv = "INTERNAL_OAUTH_SECRET"

	// oidcScriptsConfigMapSuffix names the ConfigMap holding register-internal-keycloak.sh:
	// "<platform>-core-scripts".
	oidcScriptsConfigMapSuffix = "-core-scripts"
	// oidcScriptName is the script key in that ConfigMap (and its filename once mounted).
	oidcScriptName = "register-internal-keycloak.sh"
	// adminScriptName is the first-admin registration script key in that ConfigMap (and its
	// filename once mounted). Like the OIDC script it runs IN-POD because Core's local-admin
	// API (POST /api/v1/local/admins) is localhost-only.
	adminScriptName = "register-admin.sh"
	// oidcScriptsMountPath is where the scripts ConfigMap is mounted on Core.
	oidcScriptsMountPath = "/opt/ilm/scripts"
	// oidcScriptsVolumeName is the pod volume name for the scripts ConfigMap.
	oidcScriptsVolumeName = "core-scripts"
	// oidcScriptFileMode is the (octal) mode the script file is projected with so it is
	// executable inside the Core container.
	oidcScriptFileMode = 0o755
	// coreScriptsRole is the component-label value the scripts ConfigMap carries.
	coreScriptsRole = "core-scripts"
)

// OIDCClientSecretName returns the operator-owned OIDC client Secret name for a Platform:
// "<platform>-oidc-client". The reconcile action writes the Keycloak-generated "ilm" client
// secret into it (key OIDCClientSecretKey); Core sources $INTERNAL_OAUTH_SECRET from it via
// secretKeyRef. Exported so the controller and tests share the name.
func OIDCClientSecretName(p *otilmv1alpha1.Platform) string {
	return p.Name + oidcClientSecretSuffix
}

// OIDCScriptsConfigMapName returns the name of the Core scripts ConfigMap for a Platform:
// "<platform>-core-scripts". Exported so tests can locate it.
func OIDCScriptsConfigMapName(p *otilmv1alpha1.Platform) string {
	return p.Name + oidcScriptsConfigMapSuffix
}

// withCoreInPodScripts layers Core's IN-POD bootstrap wiring onto the resolved Core component:
// the scripts ConfigMap volume + mount and the lifecycle.postStart exec that runs whichever
// localhost-only bootstrap scripts apply — register-admin.sh (the first-admin registration, when
// the certificate method is enabled) and register-internal-keycloak.sh (the managed-Keycloak
// OIDC provider registration). Both of Core's target endpoints (POST /api/v1/local/admins and
// the settings OIDC PUT) are LOCALHOST-ONLY — a cross-pod call from the operator is rejected —
// so the operator delegates them to a postStart hook inside the Core pod. It is a NO-OP when
// neither applies (the out-of-the-box render is unchanged).
//
// For managed Keycloak it also adds the $INTERNAL_OAUTH_SECRET secretKeyRef (OPTIONAL so Core
// STARTS even before the reconciler has relayed the OIDC client Secret — paired with the tolerant
// OIDC script that skips on an empty secret, and the config-checksum roll that re-runs the hook
// once the Secret is relayed). The admin cert is already on Core as $ADMIN_CERT (a secretKeyRef
// wired in ResolveCore), which register-admin.sh reads. A postStart is fire-and-forget; both
// scripts wait for localhost + are idempotent, so they are safe if Core is mid-boot.
func withCoreInPodScripts(p *otilmv1alpha1.Platform, c common.Component) common.Component {
	adminCert := RegisterAdminCertEnabled(p)
	managedKC := KeycloakManaged(p)
	if !adminCert && !managedKC {
		return c
	}

	// Project only the scripts that apply, and build the postStart command to run them in
	// sequence (admin first, then OIDC — mirrors the chart's core-deployment postStart). The
	// commands are joined with ";" (not "&&") so one failing does not skip the other.
	var items []corev1.KeyToPath
	var cmds []string

	if adminCert {
		items = append(items, corev1.KeyToPath{Key: adminScriptName, Path: adminScriptName})
		cmds = append(cmds, fmt.Sprintf("%s/%s", oidcScriptsMountPath, adminScriptName))
	}
	if managedKC {
		// $INTERNAL_OAUTH_SECRET via secretKeyRef from the operator-owned OIDC client Secret.
		// Optional so a missing Secret does not wedge Core (CreateContainerConfigError).
		c.SecretEnv = append(c.SecretEnv, common.SecretEnvRef{
			EnvVar:     oidcInternalOAuthEnv,
			SecretName: OIDCClientSecretName(p),
			SecretKey:  OIDCClientSecretKey,
			Optional:   true,
		})
		items = append(items, corev1.KeyToPath{Key: oidcScriptName, Path: oidcScriptName})
		cmds = append(cmds, fmt.Sprintf("%s/%s \"$%s\"", oidcScriptsMountPath, oidcScriptName, oidcInternalOAuthEnv))
	}

	// Scripts ConfigMap volume + read-only mount at /opt/ilm/scripts, projecting each applicable
	// script as an executable file.
	mode := int32(oidcScriptFileMode)
	c.Volumes = append(c.Volumes, corev1.Volume{
		Name: oidcScriptsVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: OIDCScriptsConfigMapName(p)},
				Items:                items,
				DefaultMode:          &mode,
			},
		},
	})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name: oidcScriptsVolumeName, MountPath: oidcScriptsMountPath, ReadOnly: true,
	})
	c.Lifecycle = &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"sh", "-c", strings.Join(cmds, "; ")}},
		},
	}
	return c
}

// BuildCoreScriptsConfigMap renders the Core scripts ConfigMap holding the IN-POD bootstrap
// scripts that apply: register-admin.sh (the first-admin registration, when the certificate
// method is enabled) and/or register-internal-keycloak.sh (the OIDC provider registration, for a
// managed Keycloak). It returns nil when neither applies (no ConfigMap rendered).
//
// Both scripts target Core's LOCALHOST-only management APIs (POST /api/v1/local/admins and the
// settings OIDC PUT), which is why they run in-pod via Core's postStart rather than cross-pod
// from the operator. Their bodies carry NO secret — only non-secret URLs/identifiers; the
// sensitive inputs arrive at runtime via env ($ADMIN_CERT, $INTERNAL_OAUTH_SECRET), both sourced
// from Secrets by secretKeyRef on the Core container.
func BuildCoreScriptsConfigMap(p *otilmv1alpha1.Platform) *corev1.ConfigMap {
	data := map[string]string{}
	if RegisterAdminCertEnabled(p) {
		data[adminScriptName] = registerAdminScript(p)
	}
	if KeycloakManaged(p) {
		data[oidcScriptName] = registerInternalKeycloakScript(p)
	}
	if len(data) == 0 {
		return nil
	}
	c := common.Component{Name: coreScriptsRole, Instance: p.Name, Namespace: p.Namespace}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OIDCScriptsConfigMapName(p),
			Namespace: p.Namespace,
			Labels:    c.Labels(),
		},
		Data: data,
	}
}

// registerAdminScript composes register-admin.sh for the certificate admin method: it waits for
// Core + the OPA sidecar on localhost, strips the PEM armor/whitespace from $ADMIN_CERT (the
// admin client certificate, sourced via secretKeyRef on Core) down to the base64 DER body Core
// expects, then POSTs the first admin to Core's LOCALHOST-only local-admin API
// (POST http://localhost:<coreport>/api/v1/local/admins). The body mirrors the operator's prior
// cross-pod AdminRequest (username, firstName from spec.registerAdmin.name, email, enabled,
// certificateData) — a subset of Core's AddUserRequestDto; Core defaults the rest. Core's create
// is idempotent, so the fire-and-forget postStart is safe to re-run.
//
// SECURITY: the script carries NO secret — the certificate arrives at runtime via $ADMIN_CERT;
// only the non-secret admin identity (username/name/email, operator-authored in the CR) is
// composed in. Mirrors the chart's scripts/register-admin.sh.
func registerAdminScript(p *otilmv1alpha1.Platform) string {
	ra := p.Spec.RegisterAdmin
	// lastName is OPTIONAL: emit the JSON field ONLY when set (matching the password method's
	// omitempty), so an unset surname does not POST an empty "lastName" to Core.
	lastName := ""
	if ra.LastName != "" {
		lastName = fmt.Sprintf("\n    \"lastName\": \"%s\",", ra.LastName)
	}
	return fmt.Sprintf(`#!/bin/sh

# Register the FIRST platform admin with Core's local-admin API (POST /api/v1/local/admins),
# which Core accepts ONLY from localhost — so this runs IN-POD (a cross-pod POST is rejected).
# A postStart hook is fire-and-forget; Core's create is idempotent, so re-runs are safe.

# Wait for Core + the OPA sidecar to be listening on localhost.
while ! nc -z localhost %d; do sleep 1; done
while ! nc -z localhost %d; do sleep 1; done

# Strip the PEM armor + ALL whitespace from $ADMIN_CERT (sourced via secretKeyRef on Core) down
# to the SINGLE-LINE base64 DER body Core decodes. The echo is INTENTIONALLY UNQUOTED: the shell
# then field-splits the multi-line PEM on newlines and echo re-joins it as one line — awk's
# [[:blank:]] removes spaces/tabs but NOT newlines, so a quoted echo would leave embedded newlines
# that break the JSON body (Core: "Unable to read HTTP message"). Mirrors the chart's register-admin.sh.
CERT=$( echo $ADMIN_CERT | awk '{gsub(/[[:blank:]]/,""); print}' )
CERT=$( echo $CERT | awk '{gsub(/-----BEGINCERTIFICATE-----/,""); print}' )
CERT=$( echo $CERT | awk '{gsub(/-----ENDCERTIFICATE-----/,""); print}' )

curl -X POST \
  -H 'content-type: application/json' \
  -d '
  {
    "username": "%s",
    "firstName": "%s",%s
    "email": "%s",
    "enabled": true,
    "certificateData": "'"$CERT"'"
  }' \
  http://localhost:%d/api/v1/local/admins
`,
		depServicePort, opaPort,
		ra.Username, ra.Name, lastName, ra.Email,
		depServicePort)
}

// registerInternalKeycloakScript composes register-internal-keycloak.sh for a managed
// Keycloak, parameterized with the operator-resolved provider config: it takes the client
// secret as $1, waits for localhost:<coreport> + localhost:<opaport>, then curls a PUT to the
// LOCALHOST settings endpoint with the provider body (scope ["openid"], audiences
// [<clientId>], skew 60).
//
// URL split: issuer/authorization/logout/postLogout are BROWSER-FACING
// (https://<host>/realms/<realm>/...); token/jwks are BACK-CHANNEL
// (http://<keycloak-service>.<ns>:<port>/realms/<realm>/...). When the platform has no
// hostname the browser-facing URLs fall back to the back-channel realm base so the body stays
// well-formed (a dev/test posture; production sets a hostname).
func registerInternalKeycloakScript(p *otilmv1alpha1.Platform) string {
	realm := keycloakRealm(p)

	// Back-channel realm base (in-cluster Keycloak Service, HTTP), namespace-qualified for
	// robustness (Core is co-located, but the qualified form matches the OPERATOR-side wiring
	// and is correct if Core ever runs elsewhere).
	backendRealm := fmt.Sprintf("http://%s.%s:%d%s/realms/%s",
		ManagedKeycloakServiceName(p), p.Namespace, ManagedKeycloakServicePort, KeycloakRelativePath, realm)
	tokenURL := backendRealm + "/protocol/openid-connect/token"
	jwksURL := backendRealm + "/protocol/openid-connect/certs"

	// Browser-facing realm base (platform hostname, HTTPS). No hostname → fall back to the
	// back-channel base so the body is still valid.
	host := oidcBrowserHost(p)
	var issuer, authz, logout, postLogout string
	if host != "" {
		browserRealm := fmt.Sprintf("https://%s%s/realms/%s", host, KeycloakRelativePath, realm)
		issuer = browserRealm
		authz = browserRealm + "/protocol/openid-connect/auth"
		logout = browserRealm + "/protocol/openid-connect/logout"
		postLogout = fmt.Sprintf("https://%s%s", host, postLogoutPath)
	} else {
		issuer = backendRealm
		authz = backendRealm + "/protocol/openid-connect/auth"
		logout = backendRealm + "/protocol/openid-connect/logout"
		postLogout = backendRealm
	}

	// The body is Core's expected provider payload (field order + scope/audiences/skew).
	// $CLIENT_SECRET is the positional $1 (from $INTERNAL_OAUTH_SECRET). The non-secret fields
	// are hard-quoted (operator-composed, no shell metacharacters) and only $CLIENT_SECRET is
	// interpolated via the shell.
	return fmt.Sprintf(`#!/bin/sh

# The OIDC client secret arrives as $1 (from $INTERNAL_OAUTH_SECRET). The operator relays it
# into the OIDC client Secret only AFTER the managed Keycloak is Ready, so during early
# bring-up the secretKeyRef resolves to empty. Skip gracefully (exit 0) instead of failing the
# postStart hook (a non-zero exit kills the container and crash-loops Core): the operator folds
# the relayed Secret into Core's config-checksum, which rolls Core ONCE the secret appears, so
# this hook re-runs WITH the secret and registers the provider.
if [ -z "$1" ]; then
  echo "INTERNAL_OAUTH_SECRET not yet populated; skipping internal OIDC provider registration (the operator will roll Core to re-run this hook once the Keycloak client secret is relayed)."
  exit 0
fi

# Assign the first parameter to a variable
CLIENT_SECRET=$1

# Wait for services to be ready (LOCALHOST — Core's settings API is localhost-only)
while ! nc -z localhost %d; do sleep 1; done
while ! nc -z localhost %d; do sleep 1; done

# Perform the cURL request against the LOCALHOST settings endpoint
curl -X PUT \
  -H 'content-type: application/json' \
  -d '
  {
    "issuerUrl":"%s",
    "clientId": "%s",
    "clientSecret": "'"$CLIENT_SECRET"'",
    "authorizationUrl": "%s",
    "tokenUrl": "%s",
    "logoutUrl": "%s",
    "jwkSetUrl": "%s",
    "scope": ["openid"],
    "audiences": ["%s"],
    "postLogoutUrl": "%s",
    "skew": 60
  }' \
  http://localhost:%d/api/v1/settings/authentication/oauth2Providers/internal
`,
		depServicePort, opaPort,
		issuer, OIDCClientID, authz, tokenURL, logout, jwksURL, OIDCClientID, postLogout,
		depServicePort)
}

// oidcBrowserHost returns the platform hostname used for the BROWSER-FACING OIDC URLs in the
// in-pod script. It is PlatformHost — spec.edge.host or, failing that, spec.common.hostName —
// the same public FQDN the Keycloak CR's external hostname (KC_HOSTNAME) and the default
// realm's ilm-client redirect URIs derive from, so a bring-your-own-edge platform that sets
// only common.hostName still emits correct browser-facing issuer/auth/logout URLs.
func oidcBrowserHost(p *otilmv1alpha1.Platform) string {
	return PlatformHost(p)
}
