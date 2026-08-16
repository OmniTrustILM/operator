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
	"encoding/json"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// managedKCPlatform returns a Platform with a managed Keycloak (instances/version) and any
// extra mutation applied, in a namespace so the rendered objects carry it. The database is
// external by default so the db-from-connection readback wires the spec coordinates.
func managedKCPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: testPGHost, Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testILMDB},
			},
			Keycloak: &otilmv1alpha1.KeycloakSpec{
				Mode:  "managed",
				Realm: "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{
					Instances: 2,
					Version:   "26.0",
				},
			},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func TestResolveManagedKeycloakExternalRendersNothing(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"},
		},
	}
	assert.Nil(t, ResolveManagedKeycloak(p), "external mode renders no Keycloak objects")
	assert.False(t, KeycloakManaged(p))
}

// TestOIDCWiringHelpers exercises the exported accessors the Core↔Keycloak OIDC reconcile
// action uses: the in-cluster Keycloak Service name/port, the Operator-generated initial-admin
// Secret name, and the edge host the browser-facing OIDC URLs derive from.
func TestOIDCWiringHelpers(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
	})
	assert.Equal(t, "ilm-keycloak-service", ManagedKeycloakServiceName(p),
		"the back-channel Service name is <keycloak>-service")
	assert.Equal(t, 8080, ManagedKeycloakServicePort)
	assert.Equal(t, "ilm-keycloak-initial-admin", ManagedKeycloakAdminSecretName(p),
		"the admin Secret is the Operator-generated <keycloak>-initial-admin")
	assert.Equal(t, testEdgeHost, EdgeHost(p), "the edge host drives the browser-facing OIDC URLs")
}

// TestEdgeHostEmptyWithoutEnabledEdge verifies EdgeHost is empty when the edge is nil or
// disabled (the browser-facing OIDC URLs are then omitted).
func TestEdgeHostEmptyWithoutEnabledEdge(t *testing.T) {
	p := managedKCPlatform(nil) // no edge
	assert.Empty(t, EdgeHost(p), "no enabled edge → no edge host")
	p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false, Host: "ignored.example.com"}
	assert.Empty(t, EdgeHost(p), "a disabled edge yields no host")
}

func TestResolveManagedKeycloakNilBlockRendersNothing(t *testing.T) {
	// A Platform with no keycloak block at all is external (OIDC configured in the DB).
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"}}
	assert.Nil(t, ResolveManagedKeycloak(p))
	assert.False(t, KeycloakManaged(p))
	assert.Nil(t, KeycloakDependencies(p))
}

// TestResolveManagedKeycloakAlwaysRendersRealmImport asserts that a managed Keycloak ALWAYS
// renders a KeycloakRealmImport (alongside the Keycloak CR) — even without a user realm
// ConfigMap — so OIDC works out of the box. The default import carries the operator's bundled
// realm with the confidential "ilm" client.
func TestResolveManagedKeycloakAlwaysRendersRealmImport(t *testing.T) {
	p := managedKCPlatform(nil)
	objs := ResolveManagedKeycloak(p)
	assert.Len(t, findManagedObjs(objs, keycloakKind), 1, "exactly one Keycloak CR")
	assert.Len(t, findManagedObjs(objs, keycloakRealmImportKind), 1, "the realm import is always rendered for a managed Keycloak")
	assert.Len(t, objs, 2, "a managed Keycloak renders the CR + the realm import")

	imp := findManagedObj(objs, keycloakRealmImportKind)
	require.NotNil(t, imp)
	assert.Equal(t, "ilm-keycloak-realm", imp.GetName())
	cr, _, _ := unstructured.NestedString(imp.Object, "spec", "keycloakCRName")
	assert.Equal(t, testKeycloakName, cr, "the import points at the Keycloak CR by name")
	realmName, _, _ := unstructured.NestedString(imp.Object, "spec", "realm", "realm")
	assert.Equal(t, "ilm", realmName, "the default import names the realm")
}

func TestResolveManagedKeycloakWithRealmImport(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: testRealmName, Key: "ilm_realm.json"}
	})
	objs := ResolveManagedKeycloak(p)
	assert.Len(t, findManagedObjs(objs, keycloakKind), 1)
	imp := findManagedObj(objs, keycloakRealmImportKind)
	require.NotNil(t, imp, "a realm import is rendered when configMapRef is set")
	assert.Equal(t, "ilm-keycloak-realm", imp.GetName())
	cr, _, _ := unstructured.NestedString(imp.Object, "spec", "keycloakCRName")
	assert.Equal(t, testKeycloakName, cr, "the import points at the Keycloak CR by name")
	realmName, _, _ := unstructured.NestedString(imp.Object, "spec", "realm", "realm")
	assert.Equal(t, "ilm", realmName, "the import skeleton names the realm")
}

// TestDefaultKeycloakRealmDefinesILMClient locks in the operator's default bundled realm: it
// names the realm, is enabled, and DEFINES exactly the confidential "ilm" OIDC client Core
// uses — with NO client secret (Keycloak generates it; the operator never mints credentials).
func TestDefaultKeycloakRealmDefinesILMClient(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
	})
	realm := DefaultKeycloakRealm(p)

	assert.Equal(t, "ilm", realm["realm"], "the default realm is named for the platform realm")
	assert.Equal(t, true, realm["enabled"], "the default realm is enabled")

	clients, ok := realm["clients"].([]interface{})
	require.True(t, ok, "the default realm carries a clients array")
	require.Len(t, clients, 1, "the default realm defines exactly the ilm client")
	c := clients[0].(map[string]interface{})

	// Identity + confidential client-secret posture (Core authenticates with a secret).
	assert.Equal(t, OIDCClientID, c["clientId"], "the client is the well-known ilm client")
	assert.Equal(t, "ilm", OIDCClientID, "the exported client id is ilm (must match the registrar lookup)")
	assert.Equal(t, false, c["publicClient"], "the ilm client is confidential")
	assert.Equal(t, false, c["bearerOnly"], "the ilm client is not bearer-only (Core does the auth-code flow)")
	assert.Equal(t, "client-secret", c["clientAuthenticatorType"], "the ilm client authenticates with a client secret")
	assert.Equal(t, true, c["standardFlowEnabled"], "the auth-code (standard) flow is enabled")
	assert.Equal(t, true, c["directAccessGrantsEnabled"], "direct access grants enabled")
	assert.Equal(t, "openid-connect", c["protocol"])

	// SECURITY: no client secret is minted/inlined — Keycloak generates it.
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "the operator must NOT mint/inline the ilm client secret (Keycloak generates it)")

	// Redirect/web-origin/post-logout are derived from the EXTERNAL edge host.
	redirects, _, _ := unstructured.NestedStringSlice(map[string]interface{}{"r": c["redirectUris"]}, "r")
	assert.Equal(t, []string{"https://ilm.example.com/api/login/oauth2/code/internal"}, redirects, "login redirect (Core's OAuth2 callback) from the edge host")
	origins, _, _ := unstructured.NestedStringSlice(map[string]interface{}{"r": c["webOrigins"]}, "r")
	assert.Equal(t, []string{"https://ilm.example.com"}, origins, "web origin from the edge host")
	attrs := c["attributes"].(map[string]interface{})
	assert.Equal(t, "https://ilm.example.com/administrator/", attrs["post.logout.redirect.uris"], "post-logout from the edge host")

	// Protocol mappers Core consumes: Groups→roles, Username, Audience(ilm).
	mappers, ok := c["protocolMappers"].([]interface{})
	require.True(t, ok)
	require.Len(t, mappers, 3, "Groups + Username + Audience mappers")
	names := map[string]map[string]interface{}{}
	for _, m := range mappers {
		mm := m.(map[string]interface{})
		names[mm["name"].(string)] = mm
	}
	require.Contains(t, names, "Groups")
	assert.Equal(t, "roles", names["Groups"]["config"].(map[string]interface{})["claim.name"], "groups → roles claim")
	require.Contains(t, names, "Username")
	require.Contains(t, names, "Audience")
	assert.Equal(t, OIDCClientID, names["Audience"]["config"].(map[string]interface{})["included.client.audience"], "audience includes the ilm client (Core validates aud=ilm)")
}

// TestDefaultKeycloakRealmNoEdgeFallsBackToWildcard asserts that without an edge host the ilm
// client still renders a usable (wildcard) redirect set — a dev/test posture; production sets
// edge.host. It must STILL carry no client secret.
func TestDefaultKeycloakRealmNoEdgeFallsBackToWildcard(t *testing.T) {
	p := managedKCPlatform(nil) // no edge
	realm := DefaultKeycloakRealm(p)
	c := realm["clients"].([]interface{})[0].(map[string]interface{})
	redirects, _, _ := unstructured.NestedStringSlice(map[string]interface{}{"r": c["redirectUris"]}, "r")
	assert.Equal(t, []string{"*"}, redirects, "no edge host → wildcard redirect (dev/test)")
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "still no minted client secret")
}

// TestDefaultKeycloakRealmUserProfileAllowsAdminAttributes locks in the realm's declarative
// user-profile configuration. Since Keycloak 24 unmanaged attributes are DISABLED by default
// and the admin REST API SILENTLY DROPS `attributes` on user create — which broke the
// registerAdmin.password flow: EnsureRealmUser's groups:["superadmin"] was never stored, so
// the realm's Groups→roles mapper had nothing to map and the first admin never got superadmin
// (caught by the gated managed e2e). The default realm must therefore ship a user profile with
// unmanagedAttributePolicy=ADMIN_EDIT: admins (the operator) can write the attribute, while
// end users CANNOT self-edit it (ENABLED would let a user grant themselves superadmin via the
// account API). The profile must also keep Keycloak's four built-in attributes — providing a
// user-profile config REPLACES the default wholesale.
func TestDefaultKeycloakRealmUserProfileAllowsAdminAttributes(t *testing.T) {
	realm := DefaultKeycloakRealm(managedKCPlatform(nil))

	components, ok := realm["components"].(map[string]interface{})
	require.True(t, ok, "the default realm carries a components map (the user-profile provider lives there)")
	providers, ok := components["org.keycloak.userprofile.UserProfileProvider"].([]interface{})
	require.True(t, ok, "the realm defines the user-profile provider component")
	require.Len(t, providers, 1)
	provider := providers[0].(map[string]interface{})
	assert.Equal(t, "declarative-user-profile", provider["providerId"])

	config := provider["config"].(map[string]interface{})
	raw, ok := config["kc.user.profile.config"].([]interface{})
	require.True(t, ok, "kc.user.profile.config is the (single-element) JSON-string config value")
	require.Len(t, raw, 1)
	var up map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(raw[0].(string)), &up), "the user-profile config is valid JSON")

	assert.Equal(t, "ADMIN_EDIT", up["unmanagedAttributePolicy"],
		"unmanaged attributes must be admin-writable (operator sets groups:[superadmin]) but NOT user-editable")

	attrs, ok := up["attributes"].([]interface{})
	require.True(t, ok, "the profile keeps an attributes array (config replaces Keycloak's default)")
	got := map[string]bool{}
	for _, a := range attrs {
		got[a.(map[string]interface{})["name"].(string)] = true
	}
	for _, builtin := range []string{"username", "email", "firstName", "lastName"} {
		assert.True(t, got[builtin], "the profile must keep Keycloak's built-in %q attribute", builtin)
	}
}

// TestResolveManagedKeycloakDefaultImportCarriesILMClient asserts the rendered (no-ConfigMap)
// realm import inlines the operator's default realm WITH the ilm client — so the import the
// reconciler applies out of the box already defines the OIDC client the registrar looks up.
func TestResolveManagedKeycloakDefaultImportCarriesILMClient(t *testing.T) {
	p := managedKCPlatform(nil)
	imp := findManagedObj(ResolveManagedKeycloak(p), keycloakRealmImportKind)
	require.NotNil(t, imp)
	clients, found, err := unstructured.NestedSlice(imp.Object, "spec", "realm", "clients")
	require.NoError(t, err)
	require.True(t, found, "the default import inlines the realm clients")
	require.Len(t, clients, 1)
	c := clients[0].(map[string]interface{})
	assert.Equal(t, "ilm", c["clientId"])
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "the rendered import must not carry a minted client secret")
}

// TestEnsureILMClientInjectsWhenAbsent asserts that EnsureILMClient adds the confidential
// "ilm" OIDC client to a realm representation that does not define it — the case where a
// caller supplies their own realm ConfigMap that omits the operator's OIDC client. Without
// this the realm imports without the ilm client and reconcileOIDCProvider can never close
// OIDCConfigured (the registrar's clientId lookup finds nothing). The injected client carries
// NO secret (Keycloak generates it).
func TestEnsureILMClientInjectsWhenAbsent(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
	})
	// A user realm with NO clients at all (the minimal realm the e2e supplied).
	realm := map[string]interface{}{"realm": "ilm", "enabled": true}

	EnsureILMClient(p, realm)

	clients, ok := realm["clients"].([]interface{})
	require.True(t, ok, "EnsureILMClient must create the clients array")
	require.Len(t, clients, 1, "exactly the injected ilm client")
	c := clients[0].(map[string]interface{})
	assert.Equal(t, OIDCClientID, c["clientId"], "the injected client is the well-known ilm client")
	assert.Equal(t, false, c["publicClient"], "the injected ilm client is confidential")
	_, hasSecret := c["secret"]
	assert.False(t, hasSecret, "the injected ilm client must carry no minted secret (Keycloak generates it)")
}

// TestEnsureILMClientAppendsAlongsideOtherClients asserts EnsureILMClient preserves a user's
// other clients and only ADDS the ilm client when it is missing.
func TestEnsureILMClientAppendsAlongsideOtherClients(t *testing.T) {
	p := managedKCPlatform(nil)
	realm := map[string]interface{}{
		"realm":   "ilm",
		"enabled": true,
		"clients": []interface{}{
			map[string]interface{}{"clientId": "some-other-app"},
		},
	}

	EnsureILMClient(p, realm)

	clients := realm["clients"].([]interface{})
	require.Len(t, clients, 2, "the user's client is preserved and the ilm client is appended")
	ids := map[string]bool{}
	for _, c := range clients {
		ids[c.(map[string]interface{})["clientId"].(string)] = true
	}
	assert.True(t, ids["some-other-app"], "the user's client is preserved")
	assert.True(t, ids[OIDCClientID], "the ilm client is added")
}

// TestEnsureILMClientLeavesUserDefinedILMClientUntouched asserts that when the caller's realm
// ALREADY defines an "ilm" client, EnsureILMClient does not clobber or duplicate it — the
// caller's representation wins (they may have tuned redirect URIs / mappers). The operator
// still reads back whatever secret Keycloak generates for it.
func TestEnsureILMClientLeavesUserDefinedILMClientUntouched(t *testing.T) {
	p := managedKCPlatform(nil)
	realm := map[string]interface{}{
		"realm":   "ilm",
		"enabled": true,
		"clients": []interface{}{
			map[string]interface{}{"clientId": OIDCClientID, "name": "user-tuned-ilm"},
		},
	}

	EnsureILMClient(p, realm)

	clients := realm["clients"].([]interface{})
	require.Len(t, clients, 1, "no duplicate ilm client is added")
	c := clients[0].(map[string]interface{})
	assert.Equal(t, "user-tuned-ilm", c["name"], "the caller's ilm client definition is left untouched")
}

// TestResolveManagedKeycloakSetsRelativePath asserts the managed Keycloak serves under /kc
// (KC_HTTP_RELATIVE_PATH via additionalOptions) — matching the gateway's /kc route + the OIDC URLs.
func TestResolveManagedKeycloakSetsRelativePath(t *testing.T) {
	kc := findManagedObj(ResolveManagedKeycloak(managedKCPlatform(nil)), keycloakKind)
	require.NotNil(t, kc)
	spec, _ := kc.Object["spec"].(map[string]interface{})
	opts, _ := spec["additionalOptions"].([]interface{})
	found := false
	for _, o := range opts {
		if m, ok := o.(map[string]interface{}); ok && m["name"] == "http-relative-path" && m["value"] == KeycloakRelativePath {
			found = true
		}
	}
	assert.True(t, found, "managed Keycloak must set http-relative-path=/kc via additionalOptions")
}

// kcAdditionalOption returns the value of the named spec.additionalOptions entry on the
// rendered managed Keycloak CR, and whether it was present.
func kcAdditionalOption(t *testing.T, p *otilmv1alpha1.Platform, name string) (string, bool) {
	t.Helper()
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	spec, _ := kc.Object["spec"].(map[string]interface{})
	opts, _ := spec["additionalOptions"].([]interface{})
	for _, o := range opts {
		if m, ok := o.(map[string]interface{}); ok && m["name"] == name {
			v, _ := m["value"].(string)
			return v, true
		}
	}
	return "", false
}

// TestResolveManagedKeycloakLogLevel covers the optional KC_LOG_LEVEL wiring: omitted by
// default (Keycloak's own default applies), and emitted as a log-level additionalOption when
// spec.keycloak.managed.logLevel is set — without disturbing the always-present /kc relative path.
func TestResolveManagedKeycloakLogLevel(t *testing.T) {
	t.Run("default: no log-level option", func(t *testing.T) {
		_, found := kcAdditionalOption(t, managedKCPlatform(nil), testLogLevel)
		assert.False(t, found, "no logLevel set → no log-level additionalOption (Keycloak defaults to info)")
	})
	t.Run("set: log-level option carries the value", func(t *testing.T) {
		p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Managed.LogLevel = "debug" })
		v, found := kcAdditionalOption(t, p, testLogLevel)
		require.True(t, found, "logLevel set → a log-level additionalOption is rendered (KC_LOG_LEVEL)")
		assert.Equal(t, "debug", v)
	})
	t.Run("relative-path stays set alongside log-level", func(t *testing.T) {
		p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Managed.LogLevel = "warn" })
		v, found := kcAdditionalOption(t, p, "http-relative-path")
		require.True(t, found, "http-relative-path must remain set when logLevel is added")
		assert.Equal(t, KeycloakRelativePath, v)
	})
}

func TestResolveManagedKeycloakRendersCR(t *testing.T) {
	p := managedKCPlatform(nil)
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc, "the Keycloak CR must be rendered")

	// GVK + identity.
	assert.Equal(t, keycloakAPIVersion, kc.GetAPIVersion())
	// Pin the literal: v2beta1 is the served+storage version on Keycloak 26.x; v2alpha1 is
	// deprecated and re-triggers the API server's "Please migrate to v2beta1" warning.
	assert.Equal(t, "k8s.keycloak.org/v2beta1", kc.GetAPIVersion())
	assert.Equal(t, keycloakKind, kc.GetKind())
	assert.Equal(t, testKeycloakName, kc.GetName(), "Keycloak CR name is <platform>-keycloak")
	assert.Equal(t, "ns", kc.GetNamespace())

	// Recommended labels (managed-by/instance drive the prune-exclusion).
	labels := kc.GetLabels()
	assert.Equal(t, common.ManagedByValue, labels[common.ManagedByLabel])
	assert.Equal(t, "ilm", labels[common.InstanceLabel])
	assert.Equal(t, managedKeycloakRole, labels[common.ComponentLabel])

	// instances.
	instances, found, err := unstructured.NestedInt64(kc.Object, "spec", "instances")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(2), instances)

	// version → image.
	image, _, _ := unstructured.NestedString(kc.Object, "spec", "image")
	assert.Contains(t, image, "26.0", "the version selects the Keycloak image tag")

	// version → startOptimized=false. Setting a custom (stock community) image REQUIRES this:
	// the Keycloak Operator otherwise assumes a custom image is pre-augmented and starts with
	// --optimized, which makes the stock image crash-loop on first start (observed with
	// Keycloak 26.4.0). It must be present AND false whenever the version-derived image is set.
	startOptimized, soFound, _ := unstructured.NestedBool(kc.Object, "spec", "startOptimized")
	assert.True(t, soFound, "spec.startOptimized must be set when a version-derived image is used")
	assert.False(t, startOptimized, "spec.startOptimized must be false for the non-augmented stock image")

	// http.httpEnabled (Keycloak behind the gateway serves plain HTTP in-cluster).
	httpEnabled, _, _ := unstructured.NestedBool(kc.Object, "spec", "http", "httpEnabled")
	assert.True(t, httpEnabled)
}

// TestResolveManagedKeycloakDBFromConnectionExternal asserts spec.db is wired from the
// mode-agnostic readback for an EXTERNAL database: the spec coordinates + the user's
// credentials Secret by reference (never inlined).
func TestResolveManagedKeycloakDBFromConnectionExternal(t *testing.T) {
	p := managedKCPlatform(nil) // external DB: pg.example.com / ilm / ilm-db
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)

	vendor, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "vendor")
	assert.Equal(t, "postgres", vendor)
	host, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "host")
	assert.Equal(t, testPGHost, host, "host comes from the resolved DB connection")
	port, _, _ := unstructured.NestedInt64(kc.Object, "spec", "db", "port")
	assert.Equal(t, int64(5432), port)
	database, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "database")
	assert.Equal(t, "ilm", database)
	schema, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "schema")
	assert.Equal(t, "keycloak", schema, "Keycloak uses a dedicated schema in the shared database")

	// The credentials are referenced by Secret name + key — never inlined.
	uName, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "usernameSecret", "name")
	assert.Equal(t, testILMDB, uName)
	uKey, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "usernameSecret", "key")
	assert.Equal(t, "username", uKey)
	pName, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "passwordSecret", "name")
	assert.Equal(t, testILMDB, pName)
	pKey, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "passwordSecret", "key")
	assert.Equal(t, "password", pKey)
}

// TestResolveManagedKeycloakDBFromConnectionManaged asserts spec.db is wired from the
// managed-PostgreSQL readback: the CNPG <cluster>-rw Service host + the <cluster>-app Secret
// — proving Keycloak shares the platform database mode-agnostically.
func TestResolveManagedKeycloakDBFromConnectionManaged(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database = otilmv1alpha1.DatabaseSpec{
			Mode:    "managed",
			Managed: &otilmv1alpha1.ManagedDatabaseSpec{Instances: 1, Storage: otilmv1alpha1.StorageSpec{Size: "10Gi"}},
		}
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)

	host, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "host")
	assert.Equal(t, "ilm-db-pooler", host, "managed Keycloak wires through the default-on PgBouncer Pooler (chart parity)")
	database, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "database")
	assert.Equal(t, "ilm", database, "managed DB readback wires the bootstrapped app database")
	secretName, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "passwordSecret", "name")
	assert.Equal(t, "ilm-db-app", secretName, "managed DB readback references the CNPG-generated app Secret")
}

// TestResolveManagedKeycloakSCCPodTemplate asserts the SCC-clean pod security (restricted-v2):
// runAsNonRoot, drop ALL caps, seccomp RuntimeDefault, no privilege escalation, NO hard-coded
// runAsUser.
func TestResolveManagedKeycloakSCCPodTemplate(t *testing.T) {
	p := managedKCPlatform(nil)
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)

	// Pod-level security context.
	podRunAsNonRoot, _, _ := unstructured.NestedBool(kc.Object, "spec", "unsupported", "podTemplate", "spec", "securityContext", "runAsNonRoot")
	assert.True(t, podRunAsNonRoot)
	podSeccomp, _, _ := unstructured.NestedString(kc.Object, "spec", "unsupported", "podTemplate", "spec", "securityContext", "seccompProfile", "type")
	assert.Equal(t, "RuntimeDefault", podSeccomp)

	// Container-level security context.
	containers, found, err := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, containers, 1)
	c := containers[0].(map[string]interface{})
	sc := c["securityContext"].(map[string]interface{})
	assert.Equal(t, true, sc["runAsNonRoot"])
	assert.Equal(t, false, sc["allowPrivilegeEscalation"])
	assert.NotContains(t, sc, "runAsUser", "no hard-coded runAsUser (OpenShift assigns the UID)")
	caps := sc["capabilities"].(map[string]interface{})
	assert.Equal(t, []interface{}{"ALL"}, caps["drop"])
	seccomp := sc["seccompProfile"].(map[string]interface{})
	assert.Equal(t, "RuntimeDefault", seccomp["type"])
}

// TestResolveManagedKeycloakAppliesTheme locks the ilm theme delivery for a version bundle that
// ships it (the default bundle): the pod template gains an SCC-clean init-theme container
// that stages the BOM keycloak-theme image into a dedicated emptyDir, the Keycloak container mounts
// it at /opt/keycloak/themes, and the default realm selects it via loginTheme=ilm. This is the
// operator's mirror of the chart's runtime theme layering; the inner loop locks the render so a
// regression is caught without the Kind e2e. DefaultImageRegistry is applied first, mirroring how
// RenderPlatform/the reconciler default the shared image coordinates before rendering.
func TestResolveManagedKeycloakAppliesTheme(t *testing.T) {
	p := managedKCPlatform(nil) // spec.version "" → the default bundle (ships keycloak-theme)
	DefaultImageRegistry(p)
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)

	// init-theme initContainer: BOM theme image, the copy command, SCC-clean + readOnlyRootFilesystem.
	initContainers, found, err := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "initContainers")
	require.NoError(t, err)
	require.True(t, found, "podTemplate carries the init-theme initContainer when the bundle ships the theme")
	require.Len(t, initContainers, 1)
	ic := initContainers[0].(map[string]interface{})
	assert.Equal(t, keycloakInitThemeName, ic["name"])
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/keycloak-theme:0.1.4", ic["image"],
		"theme image resolves from the BOM joined with the shared registry/repository")
	assert.Equal(t, []interface{}{"/bin/sh", "-c", "cp -a /themes/. /data/"}, ic["command"])
	icSC := ic["securityContext"].(map[string]interface{})
	assert.Equal(t, true, icSC["runAsNonRoot"])
	assert.Equal(t, false, icSC["allowPrivilegeEscalation"])
	assert.Equal(t, true, icSC["readOnlyRootFilesystem"], "init-theme writes only to the mounted volume")
	assert.NotContains(t, icSC, "runAsUser", "no hard-coded runAsUser (OpenShift assigns the UID)")
	assert.Equal(t, []interface{}{"ALL"}, icSC["capabilities"].(map[string]interface{})["drop"])
	assert.Equal(t, "RuntimeDefault", icSC["seccompProfile"].(map[string]interface{})["type"])
	icMounts := ic["volumeMounts"].([]interface{})
	require.Len(t, icMounts, 1)
	assert.Equal(t, keycloakThemeVolumeName, icMounts[0].(map[string]interface{})["name"])
	assert.Equal(t, keycloakThemeStagePath, icMounts[0].(map[string]interface{})["mountPath"])

	// Dedicated theme emptyDir volume.
	volumes, found, err := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, volumes, 1)
	vol := volumes[0].(map[string]interface{})
	assert.Equal(t, keycloakThemeVolumeName, vol["name"])
	_, hasEmptyDir := vol["emptyDir"]
	assert.True(t, hasEmptyDir, "the theme volume is an emptyDir")

	// The Keycloak container (position 0) mounts the theme at /opt/keycloak/themes.
	containers, _, _ := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "containers")
	require.Len(t, containers, 1)
	kcMounts := containers[0].(map[string]interface{})["volumeMounts"].([]interface{})
	require.Len(t, kcMounts, 1)
	assert.Equal(t, keycloakThemeVolumeName, kcMounts[0].(map[string]interface{})["name"])
	assert.Equal(t, keycloakThemeMountPath, kcMounts[0].(map[string]interface{})["mountPath"])

	// The default realm selects the theme.
	assert.Equal(t, keycloakLoginTheme, DefaultKeycloakRealm(p)["loginTheme"], "the bundled realm sets loginTheme=ilm")
}

// TestResolveManagedKeycloakNoThemeForOlderBundle asserts the theme is a version-aware capability:
// a bundle that does NOT ship keycloak-theme (the pre-rebrand 2.17.0) renders no init-theme
// container, no theme volume, no themes mount, and no realm loginTheme — so the same CR is portable
// across versions without a dangling, unpullable theme image.
func TestResolveManagedKeycloakNoThemeForOlderBundle(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = "2.17.0" })
	DefaultImageRegistry(p)
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)

	_, hasInit, _ := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "initContainers")
	assert.False(t, hasInit, "no init-theme container when the bundle ships no theme")
	_, hasVols, _ := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "volumes")
	assert.False(t, hasVols, "no theme volume when the bundle ships no theme")
	containers, _, _ := unstructured.NestedSlice(kc.Object, "spec", "unsupported", "podTemplate", "spec", "containers")
	require.Len(t, containers, 1)
	_, hasMounts := containers[0].(map[string]interface{})["volumeMounts"]
	assert.False(t, hasMounts, "the Keycloak container has no themes mount when the bundle ships no theme")

	_, hasLoginTheme := DefaultKeycloakRealm(p)["loginTheme"]
	assert.False(t, hasLoginTheme, "no realm loginTheme when the bundle ships no theme")
}

func TestResolveManagedKeycloakHostnameFromEdge(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	hostname, found, _ := unstructured.NestedString(kc.Object, "spec", "hostname", "hostname")
	require.True(t, found, "spec.hostname.hostname is set from the edge host")
	// KC_HOSTNAME is the FULL public URL — scheme + the /kc relative path — NOT a bare hostname,
	// so Keycloak advertises https:// URLs behind the TLS-terminating gateway (a bare hostname
	// would resolve the scheme from the plain-HTTP in-cluster request → http:// URLs → broken
	// admin console / OIDC issuer). Mirrors the chart's KC_HOSTNAME=https://<host><relativePath>.
	assert.Equal(t, "https://ilm.example.com/kc", hostname)
	// With a fixed hostname configured, strict handling (the operator default) is correct: we
	// do NOT force strict=false (that knob is only for the no-edge, header-resolved case).
	_, strictFound, _ := unstructured.NestedBool(kc.Object, "spec", "hostname", "strict")
	assert.False(t, strictFound, "with an edge hostname, spec.hostname.strict is left to the operator default")
}

// TestResolveManagedKeycloakProxyHeaders locks the X-Forwarded trust: the managed Keycloak runs
// behind the gateway (Kong) which terminates TLS and forwards plain HTTP, so spec.proxy.headers
// MUST be "xforwarded" — otherwise Keycloak ignores the forwarded scheme/host and advertises
// http:// URLs that break the admin console and OIDC issuer over the HTTPS edge. It is set
// regardless of whether a public host is configured (Keycloak is always behind the gateway).
// TestResolveManagedKeycloakDisablesOperatorIngress locks spec.ingress.enabled=false: the platform
// routes to Keycloak via its own edge + the gateway's /kc route, so the Keycloak Operator must NOT
// create its own Ingress — that would collide with the platform edge Ingress on the same host+path
// and wedge the Keycloak CR at Ready=Unknown/HasErrors (no realm import, no OIDC relay).
func TestResolveManagedKeycloakDisablesOperatorIngress(t *testing.T) {
	kc := findManagedObj(ResolveManagedKeycloak(managedKCPlatform(nil)), keycloakKind)
	require.NotNil(t, kc)
	enabled, found, err := unstructured.NestedBool(kc.Object, "spec", "ingress", "enabled")
	require.NoError(t, err)
	require.True(t, found, "spec.ingress.enabled must be set")
	assert.False(t, enabled, "the Keycloak Operator's own Ingress must be disabled (platform routes via the gateway/edge)")
}

func TestResolveManagedKeycloakProxyHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
	}{
		{"with edge host", func(p *otilmv1alpha1.Platform) {
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: testEdgeHost}
		}},
		{"no host", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kc := findManagedObj(ResolveManagedKeycloak(managedKCPlatform(tc.mutate)), keycloakKind)
			require.NotNil(t, kc)
			headers, found, err := unstructured.NestedString(kc.Object, "spec", "proxy", "headers")
			require.NoError(t, err)
			require.True(t, found, "spec.proxy.headers must be set (Keycloak is behind the gateway)")
			assert.Equal(t, "xforwarded", headers)
		})
	}
}

func TestResolveManagedKeycloakNoEdgeSetsHostnameStrictFalse(t *testing.T) {
	p := managedKCPlatform(nil) // no edge
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	// With no edge host, spec.hostname.strict MUST be false: Keycloak's Hostname v2 (26.x)
	// refuses to start unless a hostname is configured OR hostname-strict is false. The operator
	// resolves the hostname dynamically from the gateway's forwarded headers instead.
	// Observed with Keycloak 26.4.0: without this the Keycloak pod CrashLoopBackOff'd on
	// "hostname is not configured; either configure hostname, or set hostname-strict to false".
	strict, found, err := unstructured.NestedBool(kc.Object, "spec", "hostname", "strict")
	require.NoError(t, err)
	require.True(t, found, "with no edge host, spec.hostname.strict must be set")
	assert.False(t, strict, "spec.hostname.strict must be false so Keycloak boots without a fixed hostname")
	// And no fixed hostname is set in this case.
	_, hnFound, _ := unstructured.NestedString(kc.Object, "spec", "hostname", "hostname")
	assert.False(t, hnFound, "with no edge host, spec.hostname.hostname is not set")
}

func TestResolveManagedKeycloakNoVersionOmitsImage(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Managed.Version = "" })
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	_, found, _ := unstructured.NestedString(kc.Object, "spec", "image")
	assert.False(t, found, "with no version the Keycloak Operator default image is used (image omitted)")
	// startOptimized is coupled to the version-derived image: with no custom image set, the
	// Keycloak Operator's default (already-augmented) image is used, so startOptimized is left
	// unspecified (its default behavior is correct for the operator's own image).
	_, soFound, _ := unstructured.NestedBool(kc.Object, "spec", "startOptimized")
	assert.False(t, soFound, "with no version-derived image, spec.startOptimized should be omitted")
}

func TestResolveManagedKeycloakInstancesDefault(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Managed.Instances = 0 })
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	instances, _, _ := unstructured.NestedInt64(kc.Object, "spec", "instances")
	assert.Equal(t, int64(1), instances)
}

// TestResolveManagedKeycloakNoOwnerRef asserts the rendered objects carry no owner reference
// (the deletion-safety contract — they are tracked by labels and torn down only by the
// deletion handler).
func TestResolveManagedKeycloakNoOwnerRef(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: testRealmName}
	})
	for _, o := range ResolveManagedKeycloak(p) {
		u := o.(*unstructured.Unstructured)
		assert.Empty(t, u.GetOwnerReferences(), "managed Keycloak objects carry no owner reference")
	}
}

// --- Overrides (RFC 7396 JSON-merge patch) -----------------------------------

func TestManagedKeycloakOverridesMergeApplies(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"image": "my-mirror/keycloak:26.0",
				"additionalOptions": []interface{}{
					map[string]interface{}{"name": testLogLevel, "value": "DEBUG"},
				},
			},
		})
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	require.NoError(t, ManagedKeycloakRenderError(kc), "a non-protected override must apply cleanly")

	image, _, _ := unstructured.NestedString(kc.Object, "spec", "image")
	assert.Equal(t, "my-mirror/keycloak:26.0", image, "an unprotected override replaces the rendered value")
	// The operator-rendered fields survive the merge (instances untouched).
	instances, _, _ := unstructured.NestedInt64(kc.Object, "spec", "instances")
	assert.Equal(t, int64(2), instances)
	// The db wiring survives the merge.
	vendor, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "vendor")
	assert.Equal(t, "postgres", vendor)
}

func TestManagedKeycloakOverridesRejectProtectedDB(t *testing.T) {
	// spec.db is the database-sharing contract — protected.
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"db": map[string]interface{}{"host": "evil-db"},
			},
		})
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	err := ManagedKeycloakRenderError(kc)
	require.Error(t, err, "overriding the db wiring (the database-sharing contract) must be rejected")
	assert.Contains(t, err.Error(), "spec.db")
	// The protected field must NOT have been hijacked.
	host, _, _ := unstructured.NestedString(kc.Object, "spec", "db", "host")
	assert.Equal(t, testPGHost, host, "the rejected override must not retarget the database")
}

func TestManagedKeycloakOverridesRejectProtectedHostname(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"hostname": map[string]interface{}{"hostname": "evil.example.com"},
			},
		})
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	err := ManagedKeycloakRenderError(kc)
	require.Error(t, err, "overriding spec.hostname must be rejected")
	assert.Contains(t, err.Error(), "spec.hostname")
}

func TestManagedKeycloakOverridesRejectProtectedProxy(t *testing.T) {
	// spec.proxy keeps the gateway X-Forwarded trust (xforwarded) — overriding it would make
	// Keycloak advertise wrong-scheme URLs behind the edge, so it is protected.
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"proxy": map[string]interface{}{"headers": "forwarded"},
			},
		})
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	err := ManagedKeycloakRenderError(kc)
	require.Error(t, err, "overriding spec.proxy must be rejected")
	assert.Contains(t, err.Error(), "spec.proxy")
	// The protected field must NOT have been hijacked.
	headers, _, _ := unstructured.NestedString(kc.Object, "spec", "proxy", "headers")
	assert.Equal(t, "xforwarded", headers, "the rejected override must not change the proxy headers")
}

func TestManagedKeycloakOverridesRejectProtectedMetadataName(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = rawExt(t, map[string]interface{}{
			"metadata": map[string]interface{}{"name": "hijack"},
		})
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	err := ManagedKeycloakRenderError(kc)
	require.Error(t, err, "overriding metadata.name must be rejected")
	assert.Contains(t, err.Error(), "metadata.name")
	assert.Equal(t, testKeycloakName, kc.GetName(), "the rejected override must not rename the Keycloak CR")
}

func TestManagedKeycloakOverridesMalformedRejected(t *testing.T) {
	p := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.Overrides = &runtime.RawExtension{Raw: []byte(`"not an object"`)}
	})
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	require.Error(t, ManagedKeycloakRenderError(kc), "a non-object override must be rejected")
}

func TestKeycloakDependencies(t *testing.T) {
	// External: no deps.
	ext := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external"}}}
	assert.Nil(t, KeycloakDependencies(ext))

	// Managed ALWAYS requires BOTH the Keycloak CRD AND the KeycloakRealmImport CRD — the
	// operator always imports a realm (the bundled default with the ilm client, or the user's).
	p := managedKCPlatform(nil)
	deps := KeycloakDependencies(p)
	require.Len(t, deps, 2)
	assert.Equal(t, keycloakGroup, deps[0].GroupKind.Group)
	assert.Equal(t, keycloakKind, deps[0].GroupKind.Kind)
	assert.Equal(t, ReasonKeycloakOperatorNotInstalled, deps[0].Reason)
	assert.Contains(t, deps[0].Message, "spec.keycloak.mode=external", "the message names the escape hatch")
	assert.Equal(t, keycloakRealmImportKind, deps[1].GroupKind.Kind, "the second dep probes the KeycloakRealmImport CRD")

	// Managed WITH a user realm ConfigMap: still both deps.
	pi := managedKCPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Keycloak.Managed.RealmImport = &otilmv1alpha1.KeycloakRealmImportSpec{ConfigMapRef: testRealmName}
	})
	depsi := KeycloakDependencies(pi)
	require.Len(t, depsi, 2)
	assert.Equal(t, keycloakRealmImportKind, depsi[1].GroupKind.Kind, "the second dep probes the KeycloakRealmImport CRD")
}

// TestManagedKeycloakNoLeak asserts the rendered Keycloak CR carries no credential material —
// only the non-secret sizing/db-coordinates Keycloak needs, with the DB password referenced
// by Secret name (never the value).
func TestManagedKeycloakNoLeak(t *testing.T) {
	p := managedKCPlatform(nil)
	kc := findManagedObj(ResolveManagedKeycloak(p), keycloakKind)
	require.NotNil(t, kc)
	body, err := json.Marshal(kc.Object)
	require.NoError(t, err)
	s := string(body)
	// The Secret NAME may appear (it's a reference), but no password key/value is inlined.
	assert.NotContains(t, s, "your-strong-password")
	// The db block references the Secret, it does not carry a literal password value.
	_, found, _ := unstructured.NestedString(kc.Object, "spec", "db", "password")
	assert.False(t, found, "no literal db password on the Keycloak CR (referenced by Secret only)")
}
