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

// managed_keycloak.go renders the operator-provisioned Keycloak infrastructure for a
// Platform whose keycloak.mode=managed: a Keycloak CR (Keycloak Operator) that SHARES the
// platform database via the mode-agnostic readback (ResolveDatabaseConnection), runs
// SCC-clean (OpenShift restricted-v2), and — when a realm import ConfigMap is configured —
// an optional create-only KeycloakRealmImport. Both are emitted as preset-GVK
// *unstructured.Unstructured.
//
// It is the THIRD managed-infrastructure component and reuses, verbatim, the seams the
// managed PostgreSQL work (managed_database.go) factored out and the managed RabbitMQ work
// (managed_messaging.go) reused: managedLabels, newManagedUnstructured (the preset-GVK
// CRD-neutral constructor), the RFC 7396 JSON-merge-patch helper
// (applyOverridesWithProtectedPaths) behind the per-component `overrides` escape hatch, and
// the render-time error annotation pattern. Only the protected-path SET, the database
// wiring, and the SCC pod template are Keycloak-specific.
//
// WHY UNSTRUCTURED (preset GVK): the same rationale used for CloudNativePG / RabbitMQ
// and the edge's cert-manager / Gateway API objects. Adding a typed k8s.keycloak.org
// dependency would pin a k8s.io graph; an unstructured object with its apiVersion/kind
// preset apply-loops cleanly through SSA because Scheme.ObjectKinds returns the preset GVK
// even for an unregistered type, and a Keycloak Operator schema change is a data fix (a
// string path), not a compile dependency.
//
// KEYCLOAK OPERATOR SCHEMA (k8s.keycloak.org/v2beta1, exercised against Keycloak 26.x):
// the rendered Keycloak CR (spec.db / spec.http / spec.image / spec.instances /
// spec.unsupported.podTemplate) and the KeycloakRealmImport (spec.keycloakCRName /
// spec.realm) are accepted by the Keycloak Operator and Keycloak boots against the shared
// CNPG database. A custom (stock) spec.image requires spec.startOptimized=false (see
// buildKeycloak). NO credential is ever placed in these objects — the platform DB
// credentials are referenced by Secret (usernameSecret/passwordSecret) and the Keycloak
// Operator GENERATES the initial-admin Secret (read back only by the OIDC reconcile action,
// never by provisioning).

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonKeycloakOperatorNotInstalled is the KeycloakReady=False reason when keycloak is
// managed but the cluster does not serve the Keycloak Operator CRDs (Keycloak /
// KeycloakRealmImport). It is exported so the controller and its tests share one
// definition (mirrors ReasonCloudNativePGNotInstalled / ReasonRabbitMQNotInstalled).
const ReasonKeycloakOperatorNotInstalled = "KeycloakOperatorNotInstalled"

// Keycloak Operator API coordinates and the operator-owned naming/contract constants. The
// objects are rendered as unstructured with this apiVersion preset (see the package doc);
// the names are operator-owned so the gating readiness probe is a fixed contract
// independent of the CR.
const (
	// keycloakGroup / keycloakVersion / keycloakAPIVersion are the Keycloak Operator API
	// coordinates: k8s.keycloak.org/v2beta1 is the served+storage version on both the
	// Keycloak and KeycloakRealmImport CRDs (v2alpha1 remains served but is marked
	// deprecated=true, so the API server emits a "Please migrate to v2beta1" warning when the
	// operator uses it — v2beta1 keeps the same spec/status shape the operator renders + reads).
	keycloakGroup      = "k8s.keycloak.org"
	keycloakVersion    = "v2beta1"
	keycloakAPIVersion = keycloakGroup + "/" + keycloakVersion

	// keycloakKind / keycloakRealmImportKind are the Keycloak Operator Kinds the operator
	// renders.
	keycloakKind            = "Keycloak"
	keycloakRealmImportKind = "KeycloakRealmImport"

	// managedKeycloakRole is the component label the managed Keycloak CR carries;
	// managedKeycloakImportRole is the component label the KeycloakRealmImport carries.
	managedKeycloakRole       = "keycloak"
	managedKeycloakImportRole = "keycloak-realm-import"

	// keycloakDBVendor is the Keycloak database vendor for the platform PostgreSQL; the
	// Keycloak CR's spec.db.vendor drives KC_DB on the pods.
	keycloakDBVendor = "postgres"

	// keycloakDBSchema is the PostgreSQL schema Keycloak uses inside the shared platform
	// database (spec.db.schema → KC_DB_SCHEMA), so Keycloak's tables do not collide with the
	// platform's tables in the same database.
	keycloakDBSchema = "keycloak"

	// keycloakDBCredUsernameKey / keycloakDBCredPasswordKey are the keys inside the platform
	// DB-credentials Secret the Keycloak CR's spec.db.{usernameSecret,passwordSecret}
	// reference. They match the wiring profile's DatabaseCred keys (username/password), which
	// the CNPG-generated <cluster>-app Secret and an external basic-auth Secret both carry.
	// spec.db.usernameSecret/passwordSecret each take {name,key}.
	keycloakDBCredUsernameKey = "username"
	keycloakDBCredPasswordKey = "password" //nolint:gosec // G101: a Secret KEY name, not a credential value

	// keycloakDefaultRealm is the default realm name when keycloak.realm is empty.
	keycloakDefaultRealm = "ilm"
	// keycloakDefaultRealmImportKey is the default ConfigMap key holding the realm JSON.
	keycloakDefaultRealmImportKey = "realm.json"

	// OIDCClientID is the platform's well-known OIDC client in the realm. Core authenticates
	// against it; the operator reads ITS secret from Keycloak's admin API (Keycloak generates
	// the secret) and wires it into Core. It is a non-secret identifier. The reconciler's
	// registrar uses it for the Keycloak clientId lookup AND it is the clientId of the client
	// the default realm DEFINES — exported here so the realm-import builder and the registrar
	// share ONE definition (they must never drift).
	OIDCClientID = "ilm"

	// ilmRedirectPath / postLogoutPath are the ilm client's allowed OAuth2 login-redirect and
	// post-logout-redirect paths (relative to the edge host); they shape the confidential ilm
	// client's redirectUris + post.logout.redirect.uris in the operator's default realm.
	//
	// ilmRedirectPath is Core's Spring Security OAuth2 callback for the "internal" provider —
	// <core-context>/login/oauth2/code/<provider> = /api/login/oauth2/code/internal. It MUST match
	// the redirect_uri Core actually sends, or Keycloak rejects the login with "Invalid parameter:
	// redirect_uri". Mirrors the chart's ilm.redirectUri.login.
	ilmRedirectPath = "/api/login/oauth2/code/internal"
	postLogoutPath  = "/administrator/"

	// httpsScheme is the URL scheme prefix the operator prepends to the platform host when
	// building the Keycloak CR hostname and the ilm client's redirect/web-origin/post-logout URLs.
	httpsScheme = "https://"

	// keycloakProtocolOIDC is the OIDC protocol identifier the operator sets on the default ilm
	// client and its protocol mappers.
	keycloakProtocolOIDC = "openid-connect"

	// idTokenClaim / accessTokenClaim / userinfoTokenClaim are the Keycloak protocol-mapper
	// config keys controlling which tokens a mapped claim appears in.
	idTokenClaim       = "id.token.claim"       //nolint:gosec // G101: protocol-mapper config key name, not a credential
	accessTokenClaim   = "access.token.claim"   //nolint:gosec // G101: protocol-mapper config key name, not a credential
	userinfoTokenClaim = "userinfo.token.claim" //nolint:gosec // G101: protocol-mapper config key name, not a credential
)

// ManagedKeycloakName returns the stable name of the Keycloak CR the operator renders for a
// Platform: "<platform>-keycloak". It is operator-owned (a protected override path) so the
// gating readiness probe and the (future OIDC) initial-admin Secret name — which the
// follow-up resolves — are a fixed function of the Platform name.
func ManagedKeycloakName(p *otilmv1alpha1.Platform) string {
	return p.Name + "-keycloak"
}

// ManagedKeycloakGVK returns the preset GroupVersionKind of the Keycloak CR the operator
// renders. The reconciler uses it to GET the Keycloak CR (unstructured) when probing
// readiness.
func ManagedKeycloakGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: keycloakGroup, Version: keycloakVersion, Kind: keycloakKind}
}

// ManagedKeycloakServicePort is the in-cluster HTTP port (8080) the Keycloak Operator's
// Service exposes (Keycloak serves plain HTTP behind the gateway; spec.http.httpEnabled=true).
// The OIDC reconcile action uses it to build Core's back-channel token/jwks URLs.
const ManagedKeycloakServicePort = 8080

// KeycloakRelativePath is the HTTP base path the managed Keycloak serves under
// (KC_HTTP_RELATIVE_PATH). The platform gateway routes this prefix to Keycloak and every OIDC
// URL carries it, so the managed Keycloak is reachable through the edge at https://<host>/kc/...
// (the OIDC issuer + the admin console). Mirrors the Helm chart, whose keycloak-optimized image
// bakes /kc in; the operator runs the stock image, so it sets the option explicitly.
const KeycloakRelativePath = "/kc"

// ilm Keycloak theme delivery constants. The theme image is BOM-versioned and staged into the
// managed Keycloak pods at runtime (mirroring the Helm chart) because the Keycloak Operator has
// no first-class theme/volume field — see keycloakPodTemplate.
const (
	// keycloakThemeComponent is the BOM component key for the ilm Keycloak login theme image
	// (hub.omnitrustregistry.com/ilm/keycloak-theme:<tag>). A version bundle that ships this key
	// opts managed Keycloak into the theme; a bundle without it renders no theme (a version-aware
	// capability gate, like Bundle.HasProvisioning).
	keycloakThemeComponent = "keycloak-theme"
	// keycloakLoginTheme is the realm loginTheme the theme image provides (its /themes/<name>
	// directory name). DefaultKeycloakRealm sets spec.realm.loginTheme to this when the bundle
	// ships the theme, so Keycloak renders the ilm login pages.
	keycloakLoginTheme = "ilm"
	// keycloakThemeVolumeName is the dedicated emptyDir the init-theme container stages the theme
	// into and the Keycloak container mounts at keycloakThemeMountPath. It is NOT shared with
	// Keycloak's operator-managed data volumes, so it cannot collide with them.
	keycloakThemeVolumeName = "ilm-theme"
	// keycloakThemeMountPath is where the Keycloak container mounts the staged theme — Keycloak's
	// additional-themes directory. Keycloak's built-in themes (base / keycloak / keycloak.v2 admin)
	// ship inside a classpath JAR, NOT this directory, so mounting here only ADDS the ilm theme
	// (it does not hide the built-ins or break the admin console).
	keycloakThemeMountPath = "/opt/keycloak/themes"
	// keycloakThemeStagePath is where the init-theme container mounts the shared volume and copies
	// the theme image's baked /themes content into.
	keycloakThemeStagePath = "/data"
	// keycloakInitThemeName is the init container name that stages the theme.
	keycloakInitThemeName = "init-theme"
)

// ManagedKeycloakServiceName returns the in-cluster Service name the Keycloak Operator
// creates for the managed Keycloak CR: "<keycloak-cr-name>-service". The OIDC reconcile
// action uses it (with ManagedKeycloakServicePort) to build Core's back-channel token/jwks
// URLs so Core reaches Keycloak directly in-cluster, never via the edge.
func ManagedKeycloakServiceName(p *otilmv1alpha1.Platform) string {
	return ManagedKeycloakName(p) + "-service"
}

// ManagedKeycloakAdminSecretName returns the name of the Secret the Keycloak Operator
// GENERATES holding the initial admin credentials for the managed Keycloak:
// "<keycloak-cr-name>-initial-admin" (keys username/password). The OIDC reconcile action
// reads it read-only to authenticate to Keycloak's admin API; provisioning never touches it.
func ManagedKeycloakAdminSecretName(p *otilmv1alpha1.Platform) string {
	return ManagedKeycloakName(p) + "-initial-admin"
}

// EdgeHost returns the platform edge's configured external host, or "" when there is no
// enabled edge with a host. It is exported so the reconciler can build the browser-facing
// OIDC URLs (issuer/authorization/logout) from the same external host the Keycloak CR's
// hostname derives from (split-horizon DNS safe).
func EdgeHost(p *otilmv1alpha1.Platform) string {
	return edgeHost(p)
}

// ManagedKeycloakRealmImportGVK returns the preset GroupVersionKind of the
// KeycloakRealmImport the operator renders. The reconciler uses it to GET/Create the import
// (unstructured) for the create-only realm import.
func ManagedKeycloakRealmImportGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: keycloakGroup, Version: keycloakVersion, Kind: keycloakRealmImportKind}
}

// ManagedKeycloakLabels returns the recommended labels the managed Keycloak realm-import
// carries (the same managed-by/instance labels the rendered objects use), exported so the
// reconciler's create-only import path stamps the identical label set.
func ManagedKeycloakLabels(p *otilmv1alpha1.Platform) map[string]string {
	return managedLabels(p, managedKeycloakImportRole)
}

// KeycloakRealmName returns the realm name the platform uses (keycloak.realm or the "ilm"
// default), exported so the reconciler stamps it on the realm import when the user's JSON
// omits it.
func KeycloakRealmName(p *otilmv1alpha1.Platform) string {
	return keycloakRealm(p)
}

// KeycloakRealmImportConfigMap returns the configured realm-import ConfigMap name + key, or
// ("","") when no realm import is configured (the key defaults to "realm.json"). Exported so
// the reconciler reads the user ConfigMap and gates the KeycloakDependencies on the
// KeycloakRealmImport CRD only when an import is configured.
func KeycloakRealmImportConfigMap(p *otilmv1alpha1.Platform) (name, key string) {
	return keycloakRealmImportConfigMap(p)
}

// KeycloakManaged reports whether the Platform's OIDC provider is operator-provisioned
// (mode=managed with the managed block present). It is the single predicate the builder,
// the gating, and the deletion path share so they never drift. A nil keycloak block is
// external (the platform configures OIDC in the application database).
func KeycloakManaged(p *otilmv1alpha1.Platform) bool {
	return p.Spec.Keycloak != nil && p.Spec.Keycloak.Mode == keycloakModeManaged && p.Spec.Keycloak.Managed != nil
}

// keycloakModeManaged / keycloakModeExternal are the spec.keycloak.mode literals.
const (
	keycloakModeManaged  = "managed"
	keycloakModeExternal = "external"
)

// keycloakRealm returns the realm name the platform uses, defaulting to "ilm" when empty
// (the CRD also defaults this, but the builder is a pure function callable without
// apiserver defaulting in unit tests).
func keycloakRealm(p *otilmv1alpha1.Platform) string {
	if p.Spec.Keycloak != nil && p.Spec.Keycloak.Realm != "" {
		return p.Spec.Keycloak.Realm
	}
	return keycloakDefaultRealm
}

// keycloakRealmImportConfigMap returns the configured realm-import ConfigMap name + key, or
// ("","") when no realm import is configured. The key defaults to "realm.json".
func keycloakRealmImportConfigMap(p *otilmv1alpha1.Platform) (name, key string) {
	if !KeycloakManaged(p) {
		return "", ""
	}
	ri := p.Spec.Keycloak.Managed.RealmImport
	if ri == nil || ri.ConfigMapRef == "" {
		return "", ""
	}
	key = ri.Key
	if key == "" {
		key = keycloakDefaultRealmImportKey
	}
	return ri.ConfigMapRef, key
}

// ResolveManagedKeycloak returns the Keycloak Operator objects the operator provisions for
// a managed OIDC provider, or nil when keycloak is external (or managed but mis-specified).
// It renders, in a stable order, the Keycloak CR and a KeycloakRealmImport — ALWAYS, for a
// managed Keycloak — both as preset-GVK unstructured objects with NO owner reference (the
// reconciler decides ownership per the deletion-safety contract: managed CRs carry no
// controller ownerRef and are prune-excluded, so a transient de-render never deletes Keycloak).
//
// The realm import ALWAYS exists because the operator imports a realm out of the box: by
// default its own VERSION-BUNDLED realm (DefaultKeycloakRealm), which defines the confidential
// "ilm" OIDC client so Core's OIDC wiring works without the caller authoring a realm. When the
// caller DOES supply a realm ConfigMap (keycloak.managed.realmImport), the RECONCILER reads it
// and replaces spec.realm with the user's representation at reconcile time (this pure builder
// only emits the default representation — it cannot read a ConfigMap). The reconciler treats
// the import as create-only.
//
// SECURITY: no credential is placed in these objects. The platform DB credentials are
// referenced via spec.db.{usernameSecret,passwordSecret} (secretKeyRef), never inlined; the
// Keycloak Operator generates the initial-admin Secret AND the "ilm" client secret itself.
func ResolveManagedKeycloak(p *otilmv1alpha1.Platform) []client.Object {
	if !KeycloakManaged(p) {
		return nil
	}
	return []client.Object{buildKeycloak(p), buildKeycloakRealmImport(p)}
}

// keycloakAdditionalOptions builds the Keycloak CR's spec.additionalOptions: always
// http-relative-path=/kc (KC_HTTP_RELATIVE_PATH), plus log-level (KC_LOG_LEVEL) when
// spec.keycloak.managed.logLevel is set. Each entry is the Keycloak Operator's {name,value}
// server-option shape (the kebab-cased server option, which the operator maps to the matching
// KC_* setting on the pods). A nil/empty logLevel leaves Keycloak at its default (info).
func keycloakAdditionalOptions(m *otilmv1alpha1.ManagedKeycloakSpec) []interface{} {
	opts := []interface{}{
		map[string]interface{}{"name": "http-relative-path", "value": KeycloakRelativePath},
	}
	if m != nil && m.LogLevel != "" {
		opts = append(opts, map[string]interface{}{"name": "log-level", "value": m.LogLevel})
	}
	return opts
}

// buildKeycloak renders the Keycloak CR for the managed OIDC provider. The spec is built
// from the typed surface (instances/version) plus the SHARED-database wiring (spec.db from
// ResolveDatabaseConnection — vendor/host/port/database + usernameSecret/passwordSecret
// referencing the platform DB-credentials Secret), the SCC-clean pod template, and the
// edge hostname when set; then the caller's Overrides JSON-merge patch is applied onto it,
// with the operator-owned paths protected. A protected-path or malformed override surfaces
// as an error embedded in the returned object's annotation so the reconciler can degrade
// with a clear message.
func buildKeycloak(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	m := p.Spec.Keycloak.Managed

	// Resolve the ilm theme image for the selected version bundle ("" when the bundle ships no
	// theme); it drives both the pod template's init-theme container and (paired with it via the
	// same gate) the realm loginTheme in DefaultKeycloakRealm.
	themeImage, themePullPolicy := resolveKeycloakThemeImage(p)

	spec := map[string]interface{}{
		"instances": int64(managedKeycloakInstances(m)),
		// spec.db wires Keycloak at the SHARED platform database (the mode-agnostic readback):
		// vendor postgres, host/port/database from the resolved connection, and the
		// username/password by reference into the platform DB-credentials Secret (the CNPG
		// <cluster>-app Secret for managed PG, or the caller's Secret for external PG). The
		// Keycloak Operator passes these to KC_DB_* env on the pods.
		"db": keycloakDBBlock(p),
		// spec.http: the platform runs Keycloak BEHIND the gateway (Kong), which terminates
		// TLS; Keycloak itself serves plain HTTP in-cluster (spec.http.httpEnabled=true).
		"http": map[string]interface{}{
			"httpEnabled": true,
		},
		// spec.ingress.enabled=false: the platform routes external traffic to Keycloak through its
		// OWN edge (Ingress/Gateway) + the API gateway's /kc route — NEVER a per-Keycloak Ingress.
		// Left at its default, the Keycloak Operator creates an Ingress for spec.hostname's host,
		// which COLLIDES with the platform edge Ingress (same host + path "/"); the nginx admission
		// webhook then rejects it and the Keycloak CR sticks at Ready=Unknown/HasErrors, so it never
		// provisions (no realm import, no OIDC relay, no internal provider). Disabling it is always
		// correct for the platform's gateway-routed model. (A bring-your-own-edge platform still
		// reaches Keycloak via the gateway; the override hatch can re-enable this if truly needed.)
		"ingress": map[string]interface{}{
			"enabled": false,
		},
		// spec.proxy.headers=xforwarded: the gateway (Kong) terminates TLS and forwards plain HTTP
		// to Keycloak with X-Forwarded-* headers. Without this, Keycloak IGNORES those headers and
		// resolves the scheme/host from the raw in-cluster connection (http://…:8080), advertising
		// http:// frontend URLs — which breaks the admin console and the OIDC issuer when reached
		// over the HTTPS edge (the admin console SPA fails its API/redirect calls →
		// "somethingWentWrong"). Always set: a managed Keycloak is always behind the gateway.
		// Mirrors the chart's KC_PROXY_HEADERS=xforwarded.
		"proxy": map[string]interface{}{
			"headers": "xforwarded",
		},
		// spec.additionalOptions: always KC_HTTP_RELATIVE_PATH=/kc (so Keycloak serves every
		// endpoint under /kc — matching the gateway's /kc route and the OIDC URLs; the stock
		// community image is not pre-built with a relative path, unlike the chart's
		// keycloak-optimized image which bakes it in), plus the optional KC_LOG_LEVEL when
		// spec.keycloak.managed.logLevel is set. Built above as keycloakAdditionalOptions(m).
		"additionalOptions": keycloakAdditionalOptions(m),
		// spec.unsupported.podTemplate carries the SCC-clean pod security so Keycloak runs
		// under OpenShift restricted-v2 (runAsNonRoot, drop ALL caps, seccomp RuntimeDefault,
		// no privilege escalation, no hard-coded runAsUser). The Keycloak Operator exposes pod
		// customization under spec.unsupported.podTemplate (a core/v1 PodTemplateSpec).
		"unsupported": map[string]interface{}{
			"podTemplate": keycloakPodTemplate(themeImage, themePullPolicy),
		},
	}

	// Keycloak version → image. The Keycloak Operator selects the engine version via
	// spec.image (a fully-qualified image ref). The operator composes the community image
	// "quay.io/keycloak/keycloak:<version>" from the requested version; when the version is
	// empty the Keycloak Operator applies its own default image (image omitted).
	//
	// spec.startOptimized=false is REQUIRED whenever we set a custom spec.image to the stock
	// community image: per the CRD, "if [startOptimized is] left unspecified the operator will
	// assume custom images have already been augmented" — i.e. it starts Keycloak with
	// `--optimized`. The stock quay.io/keycloak/keycloak image is NOT pre-augmented (no prior
	// `kc.sh build`), so `--optimized` makes the server crash-loop on first start with
	// "The '--optimized' flag was used for first ever server start." Setting startOptimized
	// false lets Keycloak build at startup against the resolved DB and boot. A user supplying a
	// pre-built optimized image via the override hatch can set startOptimized=true there (it is
	// not a protected path).
	if m.Version != "" {
		spec["image"] = keycloakImageForVersion(m.Version)
		spec["startOptimized"] = false
	}

	// spec.hostname: when the platform has a public FQDN (PlatformHost — spec.edge.host or
	// spec.common.hostName), Keycloak's external hostname (KC_HOSTNAME) is the FULL public URL —
	// scheme AND the /kc relative path: "https://<host>/kc". Keycloak's Hostname v2 takes the
	// scheme and base path from this value; a BARE hostname would make Keycloak resolve the scheme
	// from the (plain-HTTP, in-cluster) request and advertise http:// URLs, breaking the admin
	// console and the OIDC issuer over the HTTPS edge. The path here MUST match
	// KC_HTTP_RELATIVE_PATH (/kc) per Keycloak's hostname-v2 contract. Mirrors the chart's
	// KC_HOSTNAME=https://<hostName><httpRelativePath>. When there is NO host, hostname.strict
	// MUST be set false: Keycloak's Hostname v2 (26.x) REFUSES TO START with "hostname is not
	// configured; either configure hostname, or set hostname-strict to false" unless one of the
	// two is set. strict=false lets Keycloak resolve the hostname dynamically from the gateway's
	// forwarded headers (trusted via spec.proxy.headers above). Sourcing from PlatformHost (not
	// edgeHost) means a bring-your-own-edge platform (edge.enabled=false) that sets common.hostName
	// still gets a fixed KC_HOSTNAME.
	if h := PlatformHost(p); h != "" {
		spec["hostname"] = map[string]interface{}{"hostname": httpsScheme + h + KeycloakRelativePath}
	} else {
		spec["hostname"] = map[string]interface{}{"strict": false}
	}

	u := newManagedUnstructured(p, keycloakAPIVersion, keycloakKind, ManagedKeycloakName(p), managedKeycloakRole, spec)

	// Apply the caller's Overrides JSON-merge patch (RFC 7396) onto the rendered spec,
	// rejecting the operator-owned protected paths. On error, stamp the object so the
	// reconciler degrades with an actionable, leak-free message.
	if err := applyManagedKeycloakOverrides(u, m.Overrides); err != nil {
		setManagedKeycloakError(u, err)
	}
	return u
}

// keycloakDBBlock renders the Keycloak CR spec.db block wiring Keycloak at the SHARED
// platform database via the mode-agnostic readback. The username/password are referenced
// from the platform DB-credentials Secret (secretKeyRef) — never inlined. The schema keeps
// Keycloak's tables in a dedicated PostgreSQL schema (KC_DB_SCHEMA=keycloak) so they do not
// collide with the platform's tables in the same database.
func keycloakDBBlock(p *otilmv1alpha1.Platform) map[string]interface{} {
	conn := ResolveDatabaseConnection(p)
	return map[string]interface{}{
		"vendor":   keycloakDBVendor,
		"host":     conn.Host,
		"port":     int64(conn.Port),
		"database": conn.Name,
		"schema":   keycloakDBSchema,
		// usernameSecret / passwordSecret reference the platform DB-credentials Secret by name
		// + key (each takes {name,key}), so Keycloak authenticates to the shared DB by
		// reference.
		"usernameSecret": map[string]interface{}{
			"name": conn.CredentialsSecretName,
			"key":  keycloakDBCredUsernameKey,
		},
		"passwordSecret": map[string]interface{}{
			"name": conn.CredentialsSecretName,
			"key":  keycloakDBCredPasswordKey,
		},
	}
}

// keycloakPodTemplate returns the spec.unsupported.podTemplate for the managed Keycloak pods:
// the SCC-clean pod/container security (OpenShift restricted-v2) and — when the selected version
// bundle ships the ilm Keycloak theme (themeImage != "") — the runtime theme delivery.
//
// SCC: pod-level seccompProfile RuntimeDefault + runAsNonRoot, and a container-level
// securityContext that drops ALL capabilities, disallows privilege escalation, and runs non-root
// WITHOUT a hard-coded runAsUser (so OpenShift assigns the UID from the namespace range). The
// Keycloak Operator emits an informational warning "The name of the keycloak container cannot be
// modified" — it IGNORES the container NAME but still merges the container's fields onto its main
// container BY POSITION, so the securityContext (and the themes volumeMount below) land regardless.
//
// THEME DELIVERY (mirrors the Helm chart): the Keycloak Operator builds + manages the StatefulSet
// and exposes NO first-class theme/volume field (an open upstream request, #44276), so the
// recommended operator-native runtime path is to layer the theme through this (explicitly
// "unsupported") podTemplate, which the Operator MERGES onto its generated pod:
//   - spec.volumes: a dedicated "ilm-theme" emptyDir (not shared with Keycloak's own data volumes);
//   - spec.initContainers: "init-theme" runs the BOM theme image and copies its baked /themes into
//     that volume. It is SHORT-LIVED (copy then exit 0), so — unlike a long-running init container
//     (the documented PodInitializing wedge, #30231) — it cannot block the Keycloak container;
//   - the keycloak container (position 0) mounts "ilm-theme" at /opt/keycloak/themes. Mounting
//     there only ADDS the ilm theme (the built-ins ship in a classpath JAR), and the realm's
//     loginTheme=ilm (DefaultKeycloakRealm) selects it. The theme files + the realm that selects
//     them are gated on the SAME bundle key, so they are always rendered together.
func keycloakPodTemplate(themeImage, themePullPolicy string) map[string]interface{} {
	keycloakContainer := map[string]interface{}{
		"name": "keycloak", // Operator ignores the name but merges this container's fields onto its main container
		"securityContext": map[string]interface{}{
			"runAsNonRoot":             true,
			"allowPrivilegeEscalation": false,
			"capabilities": map[string]interface{}{
				"drop": []interface{}{"ALL"},
			},
			"seccompProfile": map[string]interface{}{
				"type": "RuntimeDefault",
			},
		},
	}

	podSpec := map[string]interface{}{
		"securityContext": map[string]interface{}{
			"runAsNonRoot": true,
			"seccompProfile": map[string]interface{}{
				"type": "RuntimeDefault",
			},
		},
		"containers": []interface{}{keycloakContainer},
	}

	// Layer the ilm theme only when this version bundle ships it (themeImage != "").
	if themeImage != "" {
		keycloakContainer["volumeMounts"] = []interface{}{
			map[string]interface{}{
				"name":      keycloakThemeVolumeName,
				"mountPath": keycloakThemeMountPath,
			},
		}
		podSpec["initContainers"] = []interface{}{keycloakInitThemeContainer(themeImage, themePullPolicy)}
		podSpec["volumes"] = []interface{}{
			map[string]interface{}{
				"name":     keycloakThemeVolumeName,
				"emptyDir": map[string]interface{}{},
			},
		}
	}

	return map[string]interface{}{"spec": podSpec}
}

// keycloakInitThemeContainer renders the SCC-clean init-theme initContainer that stages the ilm
// Keycloak theme: it runs the BOM theme image (which bakes the theme under /themes) and copies it
// into the shared "ilm-theme" emptyDir mounted at /data, which the Keycloak container then sees at
// /opt/keycloak/themes. It is SCC-clean AND readOnlyRootFilesystem — its only write target is the
// mounted volume — so it satisfies OpenShift restricted-v2 with no exceptions. Mirrors the chart's
// init-theme container (`cp -a /themes/. /data/`).
func keycloakInitThemeContainer(image, pullPolicy string) map[string]interface{} {
	return map[string]interface{}{
		"name":            keycloakInitThemeName,
		"image":           image,
		"imagePullPolicy": pullPolicy,
		"command":         []interface{}{"/bin/sh", "-c", "cp -a /themes/. " + keycloakThemeStagePath + "/"},
		"securityContext": map[string]interface{}{
			"runAsNonRoot":             true,
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities": map[string]interface{}{
				"drop": []interface{}{"ALL"},
			},
			"seccompProfile": map[string]interface{}{
				"type": "RuntimeDefault",
			},
		},
		"volumeMounts": []interface{}{
			map[string]interface{}{
				"name":      keycloakThemeVolumeName,
				"mountPath": keycloakThemeStagePath,
			},
		},
	}
}

// keycloakThemeEnabled reports whether the selected version bundle ships the ilm Keycloak theme.
// It is the SINGLE gate the pod-template theme delivery AND the realm loginTheme key off, so the
// theme files and the realm that selects them never drift.
func keycloakThemeEnabled(p *otilmv1alpha1.Platform) bool {
	_, ok := resolveBundle(p).Lookup(keycloakThemeComponent)
	return ok
}

// resolveKeycloakThemeImage returns the fully-qualified ilm Keycloak theme image + pull policy for
// this Platform's selected version bundle, or ("","") when the bundle ships no theme. The image
// resolves through the shared ResolveImage path, so a spec.common.image registry/repository
// override (an air-gapped mirror) redirects the theme image too; there is no per-theme CR image
// field — the coordinate is operator-owned, versioned in the BOM.
func resolveKeycloakThemeImage(p *otilmv1alpha1.Platform) (image, pullPolicy string) {
	if !keycloakThemeEnabled(p) {
		return "", ""
	}
	ref, policy := common.ResolveImage(resolveBundle(p).Lookup, keycloakThemeComponent,
		p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})
	return ref, string(policy)
}

// buildKeycloakRealmImport renders the KeycloakRealmImport CR for the managed Keycloak: it
// points at the Keycloak CR by name (spec.keycloakCRName) and carries a RealmRepresentation
// (spec.realm). When the caller supplies their own realm ConfigMap (keycloak.managed.
// realmImport), the RECONCILER reads it and replaces spec.realm with the user's representation
// at reconcile time (this pure builder cannot read a ConfigMap). When no ConfigMap is supplied,
// this default representation — the operator's VERSION-BUNDLED realm (DefaultKeycloakRealm),
// which DEFINES the confidential "ilm" OIDC client so Core's OIDC wiring works out of the box —
// is what gets imported. The reconciler treats the import create-only either way.
//
// KeycloakRealmImport requires spec.keycloakCRName (string) + spec.realm (a
// RealmRepresentation; spec.realm.realm is the realm name).
func buildKeycloakRealmImport(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"keycloakCRName": ManagedKeycloakName(p),
		// The default realm representation (with the ilm client). The reconciler overrides
		// spec.realm with the user's ConfigMap representation when keycloak.managed.realmImport
		// is set; otherwise this operator-bundled realm is imported as-is.
		"realm": DefaultKeycloakRealm(p),
	}
	return newManagedUnstructured(p, keycloakAPIVersion, keycloakRealmImportKind,
		ManagedKeycloakName(p)+"-realm", managedKeycloakImportRole, spec)
}

// DefaultKeycloakRealm returns the operator's VERSION-BUNDLED realm representation for a
// managed Keycloak: the realm named keycloakRealm(p), enabled, containing the confidential
// "ilm" OIDC client Core uses. It is the realm the operator imports when the caller does NOT
// supply their own realm ConfigMap (keycloak.managed.realmImport) — so managed-Keycloak OIDC
// works OUT OF THE BOX without the caller authoring a realm.
//
// SECURITY: this representation carries NO client secret. The "ilm" client is confidential
// (publicClient=false, clientAuthenticatorType=client-secret) but its `secret` is OMITTED, so
// the Keycloak Operator GENERATES it on import; the reconciler then reads it back via the
// admin API and wires it to Core. The operator never mints or inlines a client credential.
// It is exported so the reconciler can inline it into the create-only import (and tests can
// lock the client shape in).
func DefaultKeycloakRealm(p *otilmv1alpha1.Platform) map[string]interface{} {
	realm := map[string]interface{}{
		"realm":   keycloakRealm(p),
		"enabled": true,
	}
	// loginTheme: the ilm theme, when this version bundle ships it (the SAME gate the pod
	// template's init-theme container keys off, so the theme files and the realm that selects
	// them are always rendered together). NOTE: KeycloakRealmImport is create-only, so this
	// applies to FRESH realms; an already-imported realm keeps its loginTheme (update it via the
	// Keycloak admin API). A caller-supplied realm (keycloak.managed.realmImport) is honored
	// as-is and may set its own loginTheme.
	if keycloakThemeEnabled(p) {
		realm["loginTheme"] = keycloakLoginTheme
	}
	realm["components"] = defaultKeycloakRealmComponents()
	EnsureILMClient(p, realm)
	return realm
}

// keycloakUserProfileConfig is the default realm's declarative user-profile configuration:
// Keycloak's shipped default profile (the four built-in attributes + the user-metadata group —
// providing a config REPLACES the default wholesale, so they must be restated) plus
// unmanagedAttributePolicy=ADMIN_EDIT. Since Keycloak 24 unmanaged attributes are DISABLED by
// default and the admin REST API SILENTLY DROPS `attributes` on user create — without this the
// registerAdmin.password flow was broken end-to-end: EnsureRealmUser's groups:["superadmin"]
// was never stored, the realm's Groups→roles mapper had nothing to map, and the first admin
// never received superadmin in Core (caught by the gated managed e2e). ADMIN_EDIT (not
// ENABLED) on purpose: the operator (an admin) can write the attribute, but end users CANNOT
// edit their own unmanaged attributes — ENABLED would let a user grant themselves superadmin
// through the account API. NOTE: KeycloakRealmImport is create-only, so this applies to FRESH
// realms; a caller-supplied realm (keycloak.managed.realmImport) that enables
// registerAdmin.password must itself allow admin-writable unmanaged attributes (or declare the
// groups attribute) for the superadmin attribute to be stored.
const keycloakUserProfileConfig = `{"attributes":[` +
	`{"name":"username","displayName":"${username}","validations":{"length":{"min":3,"max":255},"username-prohibited-characters":{},"up-username-not-idn-homograph":{}},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false},` +
	`{"name":"email","displayName":"${email}","validations":{"email":{},"length":{"max":255}},"required":{"roles":["user"]},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false},` +
	`{"name":"firstName","displayName":"${firstName}","validations":{"length":{"max":255},"person-name-prohibited-characters":{}},"required":{"roles":["user"]},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false},` +
	`{"name":"lastName","displayName":"${lastName}","validations":{"length":{"max":255},"person-name-prohibited-characters":{}},"required":{"roles":["user"]},"permissions":{"view":["admin","user"],"edit":["admin","user"]},"multivalued":false}],` +
	`"groups":[{"name":"user-metadata","displayHeader":"User metadata","displayDescription":"Attributes, which refer to user metadata"}],` +
	`"unmanagedAttributePolicy":"ADMIN_EDIT"}`

// defaultKeycloakRealmComponents returns the realm `components` map carrying the declarative
// user-profile provider configured per keycloakUserProfileConfig (the realm-export shape:
// provider class → [ { providerId, config } ] with the profile as a single JSON-string value).
func defaultKeycloakRealmComponents() map[string]interface{} {
	return map[string]interface{}{
		"org.keycloak.userprofile.UserProfileProvider": []interface{}{
			map[string]interface{}{
				"providerId": "declarative-user-profile",
				"config": map[string]interface{}{
					"kc.user.profile.config": []interface{}{keycloakUserProfileConfig},
				},
			},
		},
	}
}

// EnsureILMClient guarantees the realm representation defines the confidential "ilm" OIDC
// client Core uses, IN PLACE. It is the single point that makes managed-Keycloak OIDC work
// regardless of the realm's SOURCE: the operator's default realm calls it, AND the reconciler
// calls it on a CALLER-SUPPLIED realm (keycloak.managed.realmImport) — because
// reconcileOIDCProvider unconditionally fetches the "ilm" client secret for ANY managed
// Keycloak, so the imported realm MUST contain that client or OIDCConfigured can never close
// (the gated managed e2e caught a caller realm that omitted it).
//
// Behavior:
//   - No "ilm" client present → append defaultKeycloakILMClient(p) (creating spec.realm.clients
//     if absent).
//   - An "ilm" client ALREADY present (caller tuned it) → leave it untouched (their
//     representation wins; the operator still reads back whatever secret Keycloak generates).
//
// SECURITY: the injected client carries NO secret — Keycloak generates it; the operator never
// mints or inlines a client credential (see DefaultKeycloakRealm).
func EnsureILMClient(p *otilmv1alpha1.Platform, realm map[string]interface{}) {
	existing, _ := realm["clients"].([]interface{})
	for _, c := range existing {
		if cm, ok := c.(map[string]interface{}); ok && cm["clientId"] == OIDCClientID {
			return // the caller already defines the ilm client — do not clobber or duplicate it
		}
	}
	realm["clients"] = append(existing, defaultKeycloakILMClient(p))
}

// defaultKeycloakILMClient returns the confidential "ilm" OIDC client definition for the
// operator's default realm: a confidential client-secret client with the standard +
// direct-access flows, the login redirect URI + post-logout redirect derived from the edge
// host, the Groups/Username/Audience protocol mappers Core consumes, and fullScopeAllowed.
// This is exactly the client Core authenticates against, so it must exist in the realm for
// Core's OIDC wiring to work. The `secret` is intentionally OMITTED so Keycloak generates it
// (see DefaultKeycloakRealm).
//
// The redirectUris / webOrigins / post-logout URIs are populated from the platform's public
// FQDN (PlatformHost — spec.edge.host or spec.common.hostName, the same host the Keycloak CR's
// spec.hostname and Core's browser-facing OIDC URLs derive from), so split-horizon DNS does not
// break browser redirects. When there is no host the redirect set falls back to a wildcard so
// the client is still usable (a dev/test posture); production platforms set a host.
func defaultKeycloakILMClient(p *otilmv1alpha1.Platform) map[string]interface{} {
	return map[string]interface{}{
		"clientId":                  OIDCClientID,
		"name":                      OIDCClientID,
		"enabled":                   true,
		"protocol":                  keycloakProtocolOIDC,
		"publicClient":              false,
		"bearerOnly":                false,
		"clientAuthenticatorType":   "client-secret",
		"standardFlowEnabled":       true,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": true,
		"serviceAccountsEnabled":    false,
		"fullScopeAllowed":          true,
		"redirectUris":              keycloakILMRedirectURIs(p),
		"webOrigins":                keycloakILMWebOrigins(p),
		"attributes": map[string]interface{}{
			"post.logout.redirect.uris": keycloakILMPostLogoutURIs(p),
		},
		"protocolMappers":     defaultKeycloakILMProtocolMappers(),
		"defaultClientScopes": []interface{}{"web-origins", "acr", "profile", "roles", "email"},
	}
}

// keycloakILMRedirectURIs returns the allowed login redirect URIs for the ilm client: the
// platform host's OAuth2 callback (https://<host>/api/login/oauth2/code/internal) when the
// platform has a public FQDN (PlatformHost), else a wildcard so the client is usable without a
// host (dev/test). This MUST match the redirect_uri Core's Spring Security sends for the
// "internal" provider, or Keycloak rejects the login ("Invalid parameter: redirect_uri").
func keycloakILMRedirectURIs(p *otilmv1alpha1.Platform) []interface{} {
	if h := PlatformHost(p); h != "" {
		return []interface{}{httpsScheme + h + ilmRedirectPath}
	}
	return []interface{}{"*"}
}

// keycloakILMWebOrigins returns the allowed web origins for the ilm client: the platform
// host's origin (https://<host>) when the platform has a public FQDN, else a wildcard.
func keycloakILMWebOrigins(p *otilmv1alpha1.Platform) []interface{} {
	if h := PlatformHost(p); h != "" {
		return []interface{}{httpsScheme + h}
	}
	return []interface{}{"*"}
}

// keycloakILMPostLogoutURIs returns the allowed post-logout redirect URI for the ilm client:
// the platform host's administrator path (https://<host>/administrator/) when the platform has
// a public FQDN, else a wildcard.
func keycloakILMPostLogoutURIs(p *otilmv1alpha1.Platform) string {
	if h := PlatformHost(p); h != "" {
		return httpsScheme + h + postLogoutPath
	}
	return "*"
}

// defaultKeycloakILMProtocolMappers returns the protocol mappers the ilm client needs for
// Core's token claims: a Groups→"roles" attribute mapper, a Username property mapper, and an
// Audience mapper including the "ilm" audience (Core validates aud=ilm; the registrar PUTs
// audiences:["ilm"]). They carry no secret.
func defaultKeycloakILMProtocolMappers() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"name":           "Groups",
			"protocol":       keycloakProtocolOIDC,
			"protocolMapper": "oidc-usermodel-attribute-mapper",
			"config": map[string]interface{}{
				"aggregate.attrs":  "false",
				"multivalued":      "true",
				"user.attribute":   "groups",
				"claim.name":       "roles",
				idTokenClaim:       "true",
				accessTokenClaim:   "true",
				userinfoTokenClaim: "true",
			},
		},
		map[string]interface{}{
			"name":           "Username",
			"protocol":       keycloakProtocolOIDC,
			"protocolMapper": "oidc-usermodel-property-mapper",
			"config": map[string]interface{}{
				"user.attribute":   "username",
				"claim.name":       "username",
				"jsonType.label":   "String",
				idTokenClaim:       "true",
				accessTokenClaim:   "true",
				userinfoTokenClaim: "true",
			},
		},
		map[string]interface{}{
			"name":           "Audience",
			"protocol":       keycloakProtocolOIDC,
			"protocolMapper": "oidc-audience-mapper",
			"config": map[string]interface{}{
				"included.client.audience": OIDCClientID,
				idTokenClaim:               "true",
				accessTokenClaim:           "true",
				userinfoTokenClaim:         "false",
			},
		},
	}
}

// managedKeycloakInstances returns the configured instance count, defaulting to 1 (the CRD
// also defaults this, but the builder is a pure function callable without apiserver
// defaulting in unit tests).
func managedKeycloakInstances(m *otilmv1alpha1.ManagedKeycloakSpec) int32 {
	if m.Instances < 1 {
		return 1
	}
	return m.Instances
}

// keycloakImageForVersion composes the community Keycloak image for a version:
// quay.io/keycloak/keycloak:<version>, the operand image the Keycloak Operator runs. This is
// the non-augmented stock image, which is why buildKeycloak pairs it with
// spec.startOptimized=false (private mirrors are covered by the override hatch).
func keycloakImageForVersion(version string) string {
	return "quay.io/keycloak/keycloak:" + version
}

// edgeHost returns the platform edge's configured host, or "" when there is no enabled
// edge with a host. It is the edge-only derivation; the platform's public FQDN (used by
// Keycloak/OIDC/CORS/fe) is PlatformHost, which also honors spec.common.hostName.
func edgeHost(p *otilmv1alpha1.Platform) string {
	if e := p.Spec.Edge; e != nil && e.Enabled {
		return e.Host
	}
	return ""
}

// PlatformHost returns the platform's single canonical public FQDN — the one source every
// external-host consumer reads (the edge Ingress/Gateway host + cert SAN, Keycloak
// KC_HOSTNAME, the ilm client's OIDC redirect/web-origin/post-logout URIs, the in-pod OIDC
// registration script's browser-facing URLs, the gateway CORS origin default, and the
// fe-administrator URLs). Precedence:
//
//   - spec.edge.host when a host is set on an ENABLED edge (the per-edge override), else
//   - spec.common.hostName (the canonical FQDN), else
//   - "" (host-agnostic render — the dev/test posture, e.g. bring-your-own ingress with no
//     host yet; consumers keep their wildcard fallbacks).
//
// This serves bring-your-own-edge (edge.enabled=false, so edgeHost is ""): the platform
// still learns its public address from common.hostName for Keycloak/OIDC/CORS/fe wiring.
//
// The name PlatformHost is the deliberate public API for "the platform's FQDN"; the minor
// package-name stutter is preferred over the ambiguous bare "Host".
//
//nolint:revive // intentional name (see doc above): PlatformHost reads clearer than Host.
func PlatformHost(p *otilmv1alpha1.Platform) string {
	if h := edgeHost(p); h != "" {
		return h
	}
	return p.Spec.Common.HostName
}

// managedKeycloakOverrideProtectedPaths are the operator-owned paths a caller's Overrides
// JSON-merge patch may NOT touch on the Keycloak CR. metadata.name/namespace/ownerReferences
// keep the object identity + ownership the operator controls; spec.db keeps the
// database-sharing contract (the platform database Keycloak shares, by reference);
// spec.hostname keeps the edge-host wiring; and spec.proxy keeps the gateway X-Forwarded
// trust (xforwarded) — overriding it would make Keycloak advertise wrong-scheme URLs behind
// the edge. A patch touching any of these is rejected with a clear error.
var managedKeycloakOverrideProtectedPaths = [][]string{
	{"metadata", "name"},
	{"metadata", "namespace"},
	{"metadata", "ownerReferences"},
	{"spec", "db"},
	{"spec", "hostname"},
	{"spec", "proxy"},
}

// applyManagedKeycloakOverrides applies a caller's RFC 7396 JSON-merge patch onto the
// rendered Keycloak CR, rejecting any patch that addresses a protected (operator-owned)
// path. A nil/empty patch is a no-op. It reuses the shared merge/patch helper
// (applyOverridesWithProtectedPaths) defined alongside the managed-database builder; only
// the protected-path set and the field-name prefix differ.
func applyManagedKeycloakOverrides(u *unstructured.Unstructured, overrides *runtime.RawExtension) error {
	return applyOverridesWithProtectedPaths(u, overrides, managedKeycloakOverrideProtectedPaths,
		"keycloak.managed.overrides")
}

// managedKeycloakErrorAnnotation carries a render-time error (a rejected override) out of
// the pure builder to the reconciler, which surfaces it as a Degraded condition. It is
// operator-internal and stripped before apply. SECURITY: the error text names only a field
// path / parse failure — never a secret or coordinate.
const managedKeycloakErrorAnnotation = "otilm.com/managed-keycloak-error"

// setManagedKeycloakError stamps a render-time error onto the object via an annotation so
// the reconciler can detect-and-degrade rather than applying an invalid object.
func setManagedKeycloakError(u *unstructured.Unstructured, err error) {
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[managedKeycloakErrorAnnotation] = err.Error()
	u.SetAnnotations(ann)
}

// ManagedKeycloakRenderError returns the render-time error carried by a managed-keycloak
// object (a rejected override or malformed patch), or nil when the object rendered cleanly.
// The reconciler calls this on each ResolveManagedKeycloak object before apply.
func ManagedKeycloakRenderError(obj client.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	if msg := u.GetAnnotations()[managedKeycloakErrorAnnotation]; msg != "" {
		return errFromString(msg)
	}
	return nil
}

// KeycloakDependencies returns the upstream-operator / CRD-bundle prerequisites a managed
// Keycloak needs, or nil for external (which needs none — OIDC is configured in the
// application database). For managed it requires BOTH the Keycloak Operator's Keycloak CRD
// AND the KeycloakRealmImport CRD — the operator ALWAYS imports a realm (its own bundled realm
// with the "ilm" client by default, or the caller's). The reconciler probes each via the
// capability detector and gates the KeycloakReady condition on their presence; the result
// mirrors exactly what ResolveManagedKeycloak renders so the two never drift. The actionable
// message names the install remedy and the external escape hatch.
func KeycloakDependencies(p *otilmv1alpha1.Platform) []CRDDependency {
	if !KeycloakManaged(p) {
		return nil
	}
	msg := "keycloak.mode=managed requires the Keycloak Operator (k8s.keycloak.org); " +
		"install the Keycloak Operator or set spec.keycloak.mode=external to configure OIDC in the application database"
	return []CRDDependency{
		{
			GroupKind: schema.GroupKind{Group: keycloakGroup, Kind: keycloakKind},
			Versions:  []string{keycloakVersion},
			Reason:    ReasonKeycloakOperatorNotInstalled,
			Message:   msg,
		},
		{
			GroupKind: schema.GroupKind{Group: keycloakGroup, Kind: keycloakRealmImportKind},
			Versions:  []string{keycloakVersion},
			Reason:    ReasonKeycloakOperatorNotInstalled,
			Message:   msg,
		},
	}
}
