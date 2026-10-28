/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package registration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testMCPHost            = "ilm.example.invalid"
	testMCPGroup           = "ilm-mcp-users"
	testOtherGroup         = "ilm-mcp-admins"
	testKept               = "kept"
	testMemberID           = "member-user-id"
	testRotatedSecret      = "previous-ilm-mcp-secret-6e5d4c"
	testCreationTime       = "1761600000"
	testCreationAttribute  = "client.secret.creation.time"
	testRotatedAttribute   = "client.secret.rotated"
	testUnrelatedAttribute = "kcev.unrelated"
	testLeakyKCBody        = "failure for " + testKCAdminUser + ": " + testClientSecret + " " + testKCAdminPass + " " + testKCAccessToken

	testRealmRoot  = adminRealmsPrefix + testKCRealm
	testScopesPath = testRealmRoot + "/client-scopes"
	testGroupsPath = testRealmRoot + "/groups"
	testAuthzRoot  = testRealmRoot + "/clients/{id}/authz/resource-server"

	// Routes of the fake admin API that tests address directly.
	routeToken            = http.MethodPost + " " + keycloakTokenPath
	routeUpdateRealm      = "PUT " + testRealmRoot
	routeClients          = "GET " + testRealmRoot + "/clients"
	routeCreateClient     = "POST " + testRealmRoot + "/clients"
	routeUpdateClient     = "PUT " + testRealmRoot + "/clients/{id}"
	routeClientSecret     = "GET " + testRealmRoot + "/clients/{id}/client-secret"
	routeCreateScope      = "POST " + testScopesPath
	routeUpdateScope      = "PUT " + testScopesPath + "/{id}"
	routeReadProfiles     = "GET " + testRealmRoot + "/client-policies/profiles"
	routeWriteProfiles    = "PUT " + testRealmRoot + "/client-policies/profiles"
	routeReadPolicies     = "GET " + testRealmRoot + "/client-policies/policies"
	routeWritePolicies    = "PUT " + testRealmRoot + "/client-policies/policies"
	routeCreateGroup      = "POST " + testGroupsPath
	routeCreatePolicy     = "POST " + testAuthzRoot + "/policy/client"
	routeCreatePermission = "POST " + testAuthzRoot + "/permission/scope"
	routeUpdatePermission = "PUT " + testAuthzRoot + "/permission/scope/{pid}"
	routeAnyOther         = "/"

	// Labels the fake records, one per write request.
	writeCreateAccessScope     = "create client scope mcp:access"
	writeCreateUpstreamScope   = "create client scope ilm-mcp-upstream"
	writeCreateDelegationScope = "create client scope delegation:client"
	writeCreateMCPClient       = "create client ilm-mcp"
	writeAddUpstreamOptional   = "add optional scope ilm-mcp-upstream to ilm-mcp"
	writeEnableFGAP            = "enable admin permissions"
	writeAddAccessDefault      = "add realm default optional scope mcp:access"
	writeAddDelegationDefault  = "add realm default optional scope delegation:client"
	writeProfiles              = "write client profiles"
	writePolicies              = "write client policies"
	writeCreateClientPolicy    = "create client policy ilm-mcp-delegating-client"
	writeCreateAllUsers        = "create permission ilm-mcp-all-users-delegation"
	writeCreateGroupPermission = "create permission ilm-mcp-selected-users-delegation"
	writeCreateGroup           = "create group ilm-mcp-users"
	writeCorrectMCPClient      = "correct client ilm-mcp"
	writeCorrectAccessScope    = "correct client scope mcp:access"
	writeCorrectAllUsers       = "correct permission ilm-mcp-all-users-delegation"
	writeDeleteAllUsers        = "delete permission ilm-mcp-all-users-delegation"
	writeDeleteGroupPermission = "delete permission ilm-mcp-selected-users-delegation"
)

// The bodies Keycloak 26.8.1 accepts for a first pass in realm ilm with public host ilm.example.invalid.
const (
	bodyAccessScope = `{"name": "mcp:access", "protocol": "openid-connect",
	  "attributes": {"include.in.token.scope": "true", "display.on.consent.screen": "true"},
	  "protocolMappers": [
	    {"name": "mcp-public-resource", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
	     "consentRequired": false,
	     "config": {"included.custom.audience": "https://ilm.example.invalid/mcp", "access.token.claim": "true",
	                "id.token.claim": "false", "introspection.token.claim": "true"}},
	    {"name": "mcp-exchange-requester", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
	     "consentRequired": false,
	     "config": {"included.client.audience": "ilm-mcp", "access.token.claim": "true",
	                "id.token.claim": "false", "introspection.token.claim": "true"}}]}`

	bodyUpstreamScope = `{"name": "ilm-mcp-upstream", "protocol": "openid-connect",
	  "attributes": {"include.in.token.scope": "true", "display.on.consent.screen": "true"},
	  "protocolMappers": [
	    {"name": "ilm-api-audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
	     "consentRequired": false,
	     "config": {"included.client.audience": "ilm", "access.token.claim": "true", "id.token.claim": "false",
	                "introspection.token.claim": "true"}},
	    {"name": "Username", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-property-mapper",
	     "consentRequired": false,
	     "config": {"introspection.token.claim": "true", "userinfo.token.claim": "true", "user.attribute": "username",
	                "id.token.claim": "true", "lightweight.claim": "false", "access.token.claim": "true",
	                "claim.name": "username", "jsonType.label": "String"}},
	    {"name": "Groups", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper",
	     "consentRequired": false,
	     "config": {"aggregate.attrs": "false", "introspection.token.claim": "true", "userinfo.token.claim": "true",
	                "multivalued": "true", "user.attribute": "groups", "id.token.claim": "true",
	                "access.token.claim": "true", "claim.name": "roles"}}]}`

	bodyDelegationScope = `{"name": "delegation:client",
	  "description": "OpenID Connect scope for agent/client token exchange delegation",
	  "protocol": "openid-connect",
	  "attributes": {
	    "include.in.token.scope": "true", "parameterized.scope.type": "client-delegation",
	    "display.on.consent.screen": "true", "always.display.consent": "true", "is.parameterized.scope": "true",
	    "consent.screen.text": "${clientDelegationScopeConsentText}"},
	  "protocolMappers": [
	    {"name": "client may_act client_id", "protocol": "openid-connect",
	     "protocolMapper": "oidc-parameterized-scope-mapper", "consentRequired": false,
	     "config": {"id.token.claim": "true", "introspection.token.claim": "true", "access.token.claim": "true",
	                "claim.name": "may_act.client_id", "jsonType.label": "String"}},
	    {"name": "client may_act sub", "protocol": "openid-connect",
	     "protocolMapper": "oidc-parameterized-scope-client-sub-mapper", "consentRequired": false,
	     "config": {"id.token.claim": "true", "introspection.token.claim": "true", "access.token.claim": "true",
	                "claim.name": "may_act.sub", "jsonType.label": "String"}},
	    {"name": "client delegation audience", "protocol": "openid-connect",
	     "protocolMapper": "oidc-parameterized-scope-audience-mapper", "consentRequired": false,
	     "config": {"introspection.token.claim": "true", "access.token.claim": "true"}}]}`

	bodyMCPClient = `{"clientId": "ilm-mcp", "protocol": "openid-connect", "enabled": true, "publicClient": false,
	  "bearerOnly": false, "clientAuthenticatorType": "client-secret", "standardFlowEnabled": false,
	  "implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "serviceAccountsEnabled": true,
	  "fullScopeAllowed": false, "consentRequired": false,
	  "attributes": {"standard.token.exchange.enabled": "true",
	                 "standard.token.exchange.enableRefreshRequestedTokenType": "NO"}}`

	bodyProfiles = `{"profiles": [
	  {"name": "mcp-pkce", "executors": [{"executor": "pkce-enforcer", "configuration": {"auto-configure": true}}]},
	  {"name": "mcp-cimd", "executors": [{"executor": "client-id-metadata-document", "configuration": {
	     "cimd-allow-permitted-domains": ["claude.ai", "localhost", "127.0.0.1"],
	     "accept-public-client-with-confidential-client-only-grant": true,
	     "cimd-resource-indicator-allow-list": ["https://ilm.example.invalid/mcp"]}}]}]}`

	bodyPolicies = `{"policies": [
	  {"name": "mcp-pkce", "enabled": true,
	   "conditions": [{"condition": "client-scopes", "configuration": {"scopes": ["mcp:access"], "type": "Optional"}}],
	   "profiles": ["mcp-pkce"]},
	  {"name": "mcp-cimd", "enabled": true,
	   "conditions": [{"condition": "client-id-uri", "configuration": {"client-id-uri-scheme": ["https"],
	                   "client-id-uri-allow-permitted-domains": ["claude.ai"]}}],
	   "profiles": ["mcp-cimd"]}]}`

	bodyProfilesWithoutCIMD = `{"profiles": [
	  {"name": "mcp-pkce", "executors": [{"executor": "pkce-enforcer", "configuration": {"auto-configure": true}}]}]}`

	bodyPoliciesWithoutCIMD = `{"policies": [
	  {"name": "mcp-pkce", "enabled": true,
	   "conditions": [{"condition": "client-scopes", "configuration": {"scopes": ["mcp:access"], "type": "Optional"}}],
	   "profiles": ["mcp-pkce"]}]}`

	bodyClientPolicyTmpl = `{"name": "ilm-mcp-delegating-client", "type": "client", "logic": "POSITIVE",
	  "decisionStrategy": "UNANIMOUS", "clients": [%q]}`

	bodyAllUsersPermission = `{"name": "ilm-mcp-all-users-delegation", "type": "scope", "logic": "POSITIVE",
	  "decisionStrategy": "UNANIMOUS", "resourceType": "Users", "scopes": ["delegate"],
	  "policies": ["ilm-mcp-delegating-client"]}`

	bodyGroupPermissionTmpl = `{"name": "ilm-mcp-selected-users-delegation", "type": "scope", "logic": "POSITIVE",
	  "decisionStrategy": "UNANIMOUS", "resourceType": "Groups", "resources": [%q], "scopes": ["delegate-members"],
	  "policies": ["ilm-mcp-delegating-client"]}`
)

// ilmClientMappersJSON is what GET .../clients/{ilm}/protocol-mappers/models returns for the operator's ilm client.
const ilmClientMappersJSON = `[
  {"id": "ilm-username-mapper", "name": "Username", "protocol": "openid-connect",
   "protocolMapper": "oidc-usermodel-property-mapper", "consentRequired": false,
   "config": {"introspection.token.claim": "true", "userinfo.token.claim": "true", "user.attribute": "username",
              "id.token.claim": "true", "lightweight.claim": "false", "access.token.claim": "true",
              "claim.name": "username", "jsonType.label": "String"}},
  {"id": "ilm-groups-mapper", "name": "Groups", "protocol": "openid-connect",
   "protocolMapper": "oidc-usermodel-attribute-mapper", "consentRequired": false,
   "config": {"aggregate.attrs": "false", "introspection.token.claim": "true", "userinfo.token.claim": "true",
              "multivalued": "true", "user.attribute": "groups", "id.token.claim": "true",
              "access.token.claim": "true", "claim.name": "roles"}},
  {"id": "ilm-audience-mapper", "name": "Audience", "protocol": "openid-connect",
   "protocolMapper": "oidc-audience-mapper", "consentRequired": false,
   "config": {"included.client.audience": "ilm", "id.token.claim": "true", "access.token.claim": "true",
              "userinfo.token.claim": "false"}}]`

// keycloakDelegationScopeJSON is Keycloak's own delegation:client as a realm created after the delegation features
// stores it: userinfo.token.claim is on the two may_act mappers.
const keycloakDelegationScopeJSON = `{"id": "keycloak-delegation-scope", "name": "delegation:client",
  "description": "OpenID Connect scope for agent/client token exchange delegation",
  "protocol": "openid-connect",
  "attributes": {
    "include.in.token.scope": "true", "parameterized.scope.type": "client-delegation",
    "display.on.consent.screen": "true", "always.display.consent": "true", "is.parameterized.scope": "true",
    "consent.screen.text": "${clientDelegationScopeConsentText}"},
  "protocolMappers": [
    {"id": "keycloak-audience-mapper", "name": "client delegation audience", "protocol": "openid-connect",
     "protocolMapper": "oidc-parameterized-scope-audience-mapper", "consentRequired": false,
     "config": {"introspection.token.claim": "true", "access.token.claim": "true"}},
    {"id": "keycloak-client-id-mapper", "name": "client may_act client_id", "protocol": "openid-connect",
     "protocolMapper": "oidc-parameterized-scope-mapper", "consentRequired": false,
     "config": {"id.token.claim": "true", "introspection.token.claim": "true", "access.token.claim": "true",
                "claim.name": "may_act.client_id", "jsonType.label": "String", "userinfo.token.claim": "true"}},
    {"id": "keycloak-sub-mapper", "name": "client may_act sub", "protocol": "openid-connect",
     "protocolMapper": "oidc-parameterized-scope-client-sub-mapper", "consentRequired": false,
     "config": {"id.token.claim": "true", "introspection.token.claim": "true", "access.token.claim": "true",
                "claim.name": "may_act.sub", "jsonType.label": "String", "userinfo.token.claim": "true"}}]}`

var (
	allUsersSpec = MCPRealmSpec{PublicHost: testMCPHost, CIMD: true}
	groupSpec    = MCPRealmSpec{PublicHost: testMCPHost, CIMD: true, DelegationGroup: testMCPGroup}
	cimdOffSpec  = MCPRealmSpec{PublicHost: testMCPHost}
)

// --- The fake Keycloak admin API ----------------------------------------------------------

// fakeKeycloakAdmin is a stateful stand-in for the parts of the Keycloak 26.8.1 admin API that EnsureMCPRealm uses.
// It answers in the forms Keycloak 26.8.1 uses: 201 with a Location header for scopes, mappers, clients and groups,
// 201 with the created body for authz policies and permissions, 204 for an authz search miss, and 409 for a duplicate
// name. Like Keycloak, it stores userinfo.token.claim on mappers posted inline with a scope, gives a new client the
// realm default optional scopes of that moment, creates the admin-permissions client when FGAP is enabled, and returns
// scope and mapper lists in an order that changes from read to read. A permission update without resources keeps the
// stored ones, and one with an empty list resets the permission to its resource type. With cimdOff set, it refuses
// client policy list writes that use a provider of the cimd feature, as Keycloak does once the feature is gone. It
// records every request and labels every write.
type fakeKeycloakAdmin struct {
	srv *httptest.Server

	mu        sync.Mutex
	nextID    int
	reads     int
	requests  []kcRequest
	writes    []string
	hits      map[string]int
	overrides map[kcHit]func(http.ResponseWriter)

	cimdOff          bool
	adminPermissions bool
	clients          []object
	clientMappers    map[string][]object
	clientOptional   map[string][]string
	scopes           []object
	realmOptional    []string
	profiles         []any
	policies         []any
	groups           []object
	members          map[string][]string
	authz            []object
}

// kcRequest is one request the fake received, with the route that served it.
type kcRequest struct {
	method, route, path string
	query               url.Values
	body                []byte
}

// kcHit is the nth request a route received since the last reset.
type kcHit struct {
	route string
	nth   int
}

func (h kcHit) String() string { return fmt.Sprintf("%s #%d", h.route, h.nth) }

// newFakeKeycloakAdmin starts the fake with realm ilm as a realm created before the delegation features: the ilm
// client with its mappers, three of Keycloak's default optional scopes, FGAP off and no delegation:client. Seeds
// adjust that state before the server starts.
func newFakeKeycloakAdmin(t *testing.T, seeds ...func(*fakeKeycloakAdmin)) *fakeKeycloakAdmin {
	t.Helper()
	kc := &fakeKeycloakAdmin{
		hits:           map[string]int{},
		overrides:      map[kcHit]func(http.ResponseWriter){},
		clientMappers:  map[string][]object{},
		clientOptional: map[string][]string{},
		members:        map[string][]string{},
	}
	for _, name := range []string{"address", "phone", "offline_access"} {
		id := kc.newID()
		kc.scopes = append(kc.scopes, object{keyID: id, keyName: name, keyProtocol: openIDConnectProtocol, keyProtocolMappers: []object{}})
		kc.realmOptional = append(kc.realmOptional, id)
	}
	ilmID := kc.addClient(object{keyClientID: ilmClientID, "publicClient": false, "fullScopeAllowed": true})
	kc.clientMappers[ilmID] = decodeObjects(t, ilmClientMappersJSON)
	for _, seed := range seeds {
		seed(kc)
	}
	mux := http.NewServeMux()
	kc.route(mux)
	kc.srv = httptest.NewServer(mux)
	t.Cleanup(kc.srv.Close)
	return kc
}

// withKeycloakDelegationScope seeds a realm created after the delegation features: Keycloak's own delegation:client,
// already a realm default optional scope.
func withKeycloakDelegationScope(t *testing.T) func(*fakeKeycloakAdmin) {
	return func(kc *fakeKeycloakAdmin) {
		scope := decodeObject(t, keycloakDelegationScopeJSON)
		scope[keyProtocolMappers] = anyObjects(scope[keyProtocolMappers])
		kc.scopes = append(kc.scopes, scope)
		kc.realmOptional = append(kc.realmOptional, stringField(scope, keyID))
	}
}

// foreignProfile and foreignPolicy are client policy list entries another tool owns.
func foreignProfile() object {
	return object{keyName: "foreign-profile", keyExecutors: []any{}}
}

func foreignPolicy() object {
	return object{keyName: "foreign-policy", keyEnabled: true, keyConditions: []any{}, keyProfiles: []any{"foreign-profile"}}
}

// withForeignEntries adds the foreign profile and policy to the realm's lists.
func withForeignEntries(kc *fakeKeycloakAdmin) {
	kc.profiles = append(kc.profiles, foreignProfile())
	kc.policies = append(kc.policies, foreignPolicy())
}

// withGroupMember seeds the delegation group with one member.
func withGroupMember(kc *fakeKeycloakAdmin) {
	kc.members[kc.addGroup(testMCPGroup)] = []string{testMemberID}
}

// withNestedGroup seeds a group with the delegation group's name below another group, as Keycloak's search returns it.
func withNestedGroup(kc *fakeKeycloakAdmin) {
	kc.groups = append(kc.groups, object{keyID: "nested-group-id", keyName: testMCPGroup, "path": "/team/" + testMCPGroup})
	kc.members["nested-group-id"] = []string{testMemberID}
}

func (kc *fakeKeycloakAdmin) route(mux *http.ServeMux) {
	realm, authz := testRealmRoot, testAuthzRoot
	handlers := []struct {
		route string
		serve func(http.ResponseWriter, *http.Request, []byte)
	}{
		{routeToken, kc.serveToken},
		{"GET " + realm, kc.serveRealm},
		{routeUpdateRealm, kc.updateRealm},
		{routeClients, kc.listClients},
		{routeCreateClient, kc.createClient},
		{routeUpdateClient, kc.updateClient},
		{"GET " + realm + "/clients/{id}/protocol-mappers/models", kc.listClientMappers},
		{"GET " + realm + "/clients/{id}/optional-client-scopes", kc.listClientOptional},
		{"PUT " + realm + "/clients/{id}/optional-client-scopes/{scopeID}", kc.addClientOptional},
		{routeClientSecret, kc.serveClientSecret},
		{"GET " + testScopesPath, kc.listScopes},
		{routeCreateScope, kc.createScope},
		{routeUpdateScope, kc.updateScope},
		{"POST " + testScopesPath + "/{id}/protocol-mappers/models", kc.createScopeMapper},
		{"PUT " + testScopesPath + "/{id}/protocol-mappers/models/{mapper}", kc.updateScopeMapper},
		{"GET " + realm + "/default-optional-client-scopes", kc.listRealmOptional},
		{"PUT " + realm + "/default-optional-client-scopes/{id}", kc.addRealmOptional},
		{routeReadProfiles, kc.listProfiles},
		{routeWriteProfiles, kc.writeProfileList},
		{routeReadPolicies, kc.listPolicies},
		{routeWritePolicies, kc.writePolicyList},
		{"GET " + testGroupsPath, kc.searchGroups},
		{routeCreateGroup, kc.createGroup},
		{"GET " + authz + "/policy/search", kc.searchAuthz},
		{"GET " + authz + "/permission/search", kc.searchAuthz},
		{routeCreatePolicy, kc.createAuthz},
		{"GET " + authz + "/policy/client/{pid}", kc.serveAuthz},
		{"PUT " + authz + "/policy/client/{pid}", kc.updateAuthz},
		{routeCreatePermission, kc.createAuthz},
		{"GET " + authz + "/permission/scope/{pid}", kc.serveAuthz},
		{routeUpdatePermission, kc.updateAuthz},
		{"GET " + authz + "/policy/{pid}/{part}", kc.serveAuthzPart},
		{"DELETE " + authz + "/policy/{pid}", kc.deleteAuthz},
		{routeAnyOther, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNotFound) }},
	}
	for _, h := range handlers {
		mux.HandleFunc(h.route, kc.record(h.route, h.serve))
	}
}

// record logs the request, answers it from an override when a test set one for this hit of the route, refuses a
// request without the admin token, and otherwise serves it from the fake's state.
func (kc *fakeKeycloakAdmin) record(route string, serve func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		kc.mu.Lock()
		defer kc.mu.Unlock()
		kc.requests = append(kc.requests, kcRequest{method: r.Method, route: route, path: r.URL.Path, query: r.URL.Query(), body: body})
		kc.hits[route]++
		if r.Method == http.MethodGet {
			kc.reads++
		}
		if override := kc.overrides[kcHit{route, kc.hits[route]}]; override != nil {
			override(w)
			return
		}
		if route != routeToken && r.Header.Get("Authorization") != bearerPrefix+testKCAccessToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		serve(w, r, body)
	}
}

func (kc *fakeKeycloakAdmin) serveToken(w http.ResponseWriter, _ *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("grant_type") != "password" || form.Get("client_id") != "admin-cli" ||
		form.Get("username") != testKCAdminUser || form.Get("password") != testKCAdminPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, object{"access_token": testKCAccessToken})
}

func (kc *fakeKeycloakAdmin) serveRealm(w http.ResponseWriter, _ *http.Request, _ []byte) {
	writeJSON(w, http.StatusOK, object{"realm": testKCRealm, keyEnabled: true, "adminPermissionsEnabled": kc.adminPermissions})
}

// updateRealm applies a partial realm update. Enabling FGAP creates the admin-permissions client.
func (kc *fakeKeycloakAdmin) updateRealm(w http.ResponseWriter, _ *http.Request, body []byte) {
	if rawObject(body)["adminPermissionsEnabled"] == true && !kc.adminPermissions {
		kc.adminPermissions = true
		kc.addClient(object{keyClientID: adminPermissionsClientID, "serviceAccountsEnabled": true, "authorizationServicesEnabled": true})
	}
	kc.writes = append(kc.writes, writeEnableFGAP)
	w.WriteHeader(http.StatusNoContent)
}

func (kc *fakeKeycloakAdmin) listClients(w http.ResponseWriter, r *http.Request, _ []byte) {
	found := []object{}
	for _, c := range kc.clients {
		if c[keyClientID] == r.URL.Query().Get("clientId") {
			rep := maps.Clone(c)
			rep["optionalClientScopes"] = vary(kc, kc.scopeNames(kc.clientOptional[stringField(c, keyID)]))
			found = append(found, rep)
		}
	}
	writeJSON(w, http.StatusOK, found)
}

func (kc *fakeKeycloakAdmin) createClient(w http.ResponseWriter, _ *http.Request, body []byte) {
	rep := rawObject(body)
	if kc.findClient(stringField(rep, keyClientID)) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	id := kc.addClient(rep)
	kc.writes = append(kc.writes, "create client "+stringField(rep, keyClientID))
	kc.created(w, "clients", id)
}

// updateClient applies a client update as Keycloak does: the fields present are set, and of the attributes only the
// ones present.
func (kc *fakeKeycloakAdmin) updateClient(w http.ResponseWriter, r *http.Request, body []byte) {
	client := kc.clientByID(r.PathValue("id"))
	if client == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for key, value := range rawObject(body) {
		attributes, isAttributes := value.(object)
		if key != keyAttributes || !isAttributes {
			client[key] = value
			continue
		}
		stored, _ := client[keyAttributes].(object)
		maps.Copy(stored, attributes)
	}
	kc.writes = append(kc.writes, "correct client "+stringField(client, keyClientID))
	w.WriteHeader(http.StatusNoContent)
}

func (kc *fakeKeycloakAdmin) listClientMappers(w http.ResponseWriter, r *http.Request, _ []byte) {
	if kc.clientByID(r.PathValue("id")) == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, vary(kc, kc.clientMappers[r.PathValue("id")]))
}

func (kc *fakeKeycloakAdmin) listClientOptional(w http.ResponseWriter, r *http.Request, _ []byte) {
	if kc.clientByID(r.PathValue("id")) == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, vary(kc, kc.scopeRefs(kc.clientOptional[r.PathValue("id")])))
}

func (kc *fakeKeycloakAdmin) addClientOptional(w http.ResponseWriter, r *http.Request, _ []byte) {
	id := r.PathValue("id")
	client, scope := kc.clientByID(id), kc.scopeByID(r.PathValue("scopeID"))
	if client == nil || scope == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !slices.Contains(kc.clientOptional[id], stringField(scope, keyID)) {
		kc.clientOptional[id] = append(kc.clientOptional[id], stringField(scope, keyID))
	}
	kc.writes = append(kc.writes, fmt.Sprintf("add optional scope %s to %s", stringField(scope, keyName), stringField(client, keyClientID)))
	w.WriteHeader(http.StatusNoContent)
}

func (kc *fakeKeycloakAdmin) serveClientSecret(w http.ResponseWriter, r *http.Request, _ []byte) {
	client := kc.clientByID(r.PathValue("id"))
	if client == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, object{keyType: "secret", "value": client["secret"]})
}

func (kc *fakeKeycloakAdmin) listScopes(w http.ResponseWriter, _ *http.Request, _ []byte) {
	scopes := make([]object, 0, len(kc.scopes))
	for _, s := range kc.scopes {
		rep := maps.Clone(s)
		rep[keyProtocolMappers] = vary(kc, scopeMappers(s))
		scopes = append(scopes, rep)
	}
	writeJSON(w, http.StatusOK, vary(kc, scopes))
}

// createScope stores a new scope and its inline mappers. Keycloak sets userinfo.token.claim to id.token.claim on an
// inline mapper that lacks it.
func (kc *fakeKeycloakAdmin) createScope(w http.ResponseWriter, _ *http.Request, body []byte) {
	scope := rawObject(body)
	name := stringField(scope, keyName)
	if kc.scopeByName(name) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	id := kc.newID()
	scope[keyID] = id
	mappers := anyObjects(scope[keyProtocolMappers])
	for _, m := range mappers {
		m[keyID] = kc.newID()
		config, _ := m[keyConfig].(object)
		if _, set := config["userinfo.token.claim"]; !set && config[idTokenClaim] != nil {
			config["userinfo.token.claim"] = config[idTokenClaim]
		}
	}
	scope[keyProtocolMappers] = mappers
	kc.scopes = append(kc.scopes, scope)
	kc.writes = append(kc.writes, "create client scope "+name)
	kc.created(w, "client-scopes", id)
}

// updateScope applies the fields and attributes present, and leaves the mappers alone, as Keycloak does.
func (kc *fakeKeycloakAdmin) updateScope(w http.ResponseWriter, r *http.Request, body []byte) {
	scope := kc.scopeByID(r.PathValue("id"))
	if scope == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for key, value := range rawObject(body) {
		switch key {
		case keyName, keyProtocol, keyDescription:
			scope[key] = value
		case keyAttributes:
			attributes, _ := scope[keyAttributes].(object)
			if attributes == nil {
				attributes = object{}
				scope[keyAttributes] = attributes
			}
			present, _ := value.(object)
			maps.Copy(attributes, present)
		}
	}
	kc.writes = append(kc.writes, "correct client scope "+stringField(scope, keyName))
	w.WriteHeader(http.StatusNoContent)
}

// createScopeMapper adds a mapper as posted: Keycloak stores a mapper added this way without userinfo.token.claim.
func (kc *fakeKeycloakAdmin) createScopeMapper(w http.ResponseWriter, r *http.Request, body []byte) {
	scope := kc.scopeByID(r.PathValue("id"))
	if scope == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	mapper := rawObject(body)
	if findByName(scopeMappers(scope), stringField(mapper, keyName)) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	id := kc.newID()
	mapper[keyID] = id
	scope[keyProtocolMappers] = append(scopeMappers(scope), mapper)
	kc.writes = append(kc.writes, fmt.Sprintf("add mapper %s to %s", stringField(mapper, keyName), stringField(scope, keyName)))
	kc.created(w, "client-scopes/"+r.PathValue("id")+"/protocol-mappers/models", id)
}

func (kc *fakeKeycloakAdmin) updateScopeMapper(w http.ResponseWriter, r *http.Request, body []byte) {
	scope := kc.scopeByID(r.PathValue("id"))
	var mapper object
	if scope != nil {
		mapper = findByID(scopeMappers(scope), r.PathValue("mapper"))
	}
	if mapper == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	update := rawObject(body)
	for _, key := range []string{keyName, keyProtocol, keyProtocolMapper, keyConsentRequired, keyConfig} {
		if value, ok := update[key]; ok {
			mapper[key] = value
		}
	}
	kc.writes = append(kc.writes, fmt.Sprintf("correct mapper %s of %s", stringField(mapper, keyName), stringField(scope, keyName)))
	w.WriteHeader(http.StatusNoContent)
}

func (kc *fakeKeycloakAdmin) listRealmOptional(w http.ResponseWriter, _ *http.Request, _ []byte) {
	writeJSON(w, http.StatusOK, vary(kc, kc.scopeRefs(kc.realmOptional)))
}

func (kc *fakeKeycloakAdmin) addRealmOptional(w http.ResponseWriter, r *http.Request, _ []byte) {
	scope := kc.scopeByID(r.PathValue("id"))
	if scope == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !slices.Contains(kc.realmOptional, stringField(scope, keyID)) {
		kc.realmOptional = append(kc.realmOptional, stringField(scope, keyID))
	}
	kc.writes = append(kc.writes, "add realm default optional scope "+stringField(scope, keyName))
	w.WriteHeader(http.StatusNoContent)
}

// listProfiles and listPolicies answer a realm without entries with an empty list, as Keycloak 26.8.1 does.
func (kc *fakeKeycloakAdmin) listProfiles(w http.ResponseWriter, _ *http.Request, _ []byte) {
	writeJSON(w, http.StatusOK, object{keyProfiles: nonNil(kc.profiles)})
}

// nonNil returns list, or an empty list when it is nil, which JSON encodes as null.
func nonNil(list []any) []any {
	if list == nil {
		return []any{}
	}
	return list
}

// writeProfileList refuses, once the cimd feature is gone, a list that holds one of its executors: Keycloak validates
// every entry of a list write, disabled ones included.
func (kc *fakeKeycloakAdmin) writeProfileList(w http.ResponseWriter, _ *http.Request, body []byte) {
	profiles, _ := rawObject(body)[keyProfiles].([]any)
	if kc.cimdOff && slices.ContainsFunc(anyObjects(profiles), usesCIMD) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kc.profiles = profiles
	kc.writes = append(kc.writes, writeProfiles)
	w.WriteHeader(http.StatusNoContent)
}

// usesCIMD reports whether a client profile or policy uses an executor or condition of Keycloak's cimd feature.
func usesCIMD(entry object) bool {
	for _, executor := range anyObjects(entry[keyExecutors]) {
		if executor["executor"] == cimdExecutor {
			return true
		}
	}
	for _, condition := range anyObjects(entry[keyConditions]) {
		if condition["condition"] == clientIDURICondition {
			return true
		}
	}
	return false
}

func (kc *fakeKeycloakAdmin) listPolicies(w http.ResponseWriter, _ *http.Request, _ []byte) {
	writeJSON(w, http.StatusOK, object{keyPolicies: nonNil(kc.policies)})
}

func (kc *fakeKeycloakAdmin) writePolicyList(w http.ResponseWriter, _ *http.Request, body []byte) {
	policies, _ := rawObject(body)[keyPolicies].([]any)
	if slices.ContainsFunc(anyObjects(policies), kc.refusesPolicy) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kc.policies = policies
	kc.writes = append(kc.writes, writePolicies)
	w.WriteHeader(http.StatusNoContent)
}

// refusesPolicy reports whether Keycloak refuses a client policy: one that names a missing profile, whose
// client-scopes condition names a missing scope, or, once the cimd feature is gone, that uses its condition.
func (kc *fakeKeycloakAdmin) refusesPolicy(policy object) bool {
	if kc.cimdOff && usesCIMD(policy) {
		return true
	}
	for _, profile := range anyList(policy[keyProfiles]) {
		if indexByName(kc.profiles, fmt.Sprint(profile)) < 0 {
			return true
		}
	}
	for _, condition := range anyObjects(policy[keyConditions]) {
		configuration, _ := condition[keyConfiguration].(object)
		for _, scope := range anyList(configuration[keyScopes]) {
			if condition["condition"] == "client-scopes" && kc.scopeByName(fmt.Sprint(scope)) == nil {
				return true
			}
		}
	}
	return false
}

func (kc *fakeKeycloakAdmin) searchGroups(w http.ResponseWriter, r *http.Request, _ []byte) {
	search, exact := r.URL.Query().Get("search"), r.URL.Query().Get("exact") == kcTrue
	found := []object{}
	for _, g := range kc.groups {
		name := stringField(g, keyName)
		if name == search || (!exact && strings.Contains(name, search)) {
			found = append(found, g)
		}
	}
	writeJSON(w, http.StatusOK, found)
}

func (kc *fakeKeycloakAdmin) createGroup(w http.ResponseWriter, _ *http.Request, body []byte) {
	name := stringField(rawObject(body), keyName)
	if kc.groupByName(name) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	id := kc.addGroup(name)
	kc.writes = append(kc.writes, "create group "+name)
	kc.created(w, "groups", id)
}

// authzServer refuses an authz call addressed to any client but admin-permissions.
func (kc *fakeKeycloakAdmin) authzServer(w http.ResponseWriter, r *http.Request) bool {
	if client := kc.findClient(adminPermissionsClientID); client != nil && client[keyID] == r.PathValue("id") {
		return true
	}
	w.WriteHeader(http.StatusNotFound)
	return false
}

func (kc *fakeKeycloakAdmin) searchAuthz(w http.ResponseWriter, r *http.Request, _ []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	found := kc.authzByName(r.URL.Query().Get(keyName))
	if found == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, object{keyID: found[keyID], keyName: found[keyName], keyType: found[keyType],
		keyLogic: found[keyLogic], keyDecisionStrategy: found[keyDecisionStrategy], keyConfig: object{}})
}

func (kc *fakeKeycloakAdmin) createAuthz(w http.ResponseWriter, r *http.Request, body []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	rep := rawObject(body)
	if status := kc.refuseAuthz(rep); status != 0 {
		w.WriteHeader(status)
		return
	}
	if kc.authzByName(stringField(rep, keyName)) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	stored := maps.Clone(rep)
	stored[keyID] = kc.newID()
	if stored[keyType] == scopePermissionType {
		stored[keyResources] = storedResources(rep, nil)
	}
	kc.authz = append(kc.authz, stored)
	kc.writes = append(kc.writes, fmt.Sprintf("create %s %s", authzLabel(stored), stringField(stored, keyName)))
	rep[keyID] = stored[keyID]
	writeJSON(w, http.StatusCreated, rep)
}

// refuseAuthz returns the status FGAP v2 answers a permission with: 400 for a decision strategy other than UNANIMOUS
// or a policy that does not exist, 0 when it accepts it.
func (kc *fakeKeycloakAdmin) refuseAuthz(rep object) int {
	if rep[keyType] != scopePermissionType {
		return 0
	}
	if rep[keyDecisionStrategy] != unanimousStrategy {
		return http.StatusBadRequest
	}
	for _, policy := range anyList(rep[keyPolicies]) {
		if kc.authzByName(fmt.Sprint(policy)) == nil {
			return http.StatusBadRequest
		}
	}
	return 0
}

// storedResources is what FGAP stores as a permission's resources: the ones named, or the resource type itself for a
// permission that names none. An update without a resources key keeps the current ones.
func storedResources(rep object, current any) any {
	resources, present := rep[keyResources].([]any)
	switch {
	case len(resources) > 0:
		return resources
	case present || current == nil:
		return []any{rep[keyResourceType]}
	default:
		return current
	}
}

func authzLabel(o object) string {
	if o[keyType] == clientPolicyType {
		return "client policy"
	}
	return "permission"
}

// serveAuthz answers the typed representation. A permission's carries neither its resources, scopes nor policies.
func (kc *fakeKeycloakAdmin) serveAuthz(w http.ResponseWriter, r *http.Request, _ []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	stored := kc.authzByID(r.PathValue("pid"))
	wantType := scopePermissionType
	if strings.Contains(r.Pattern, "/policy/client/") {
		wantType = clientPolicyType
	}
	if stored == nil || stored[keyType] != wantType {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rep := object{}
	for _, key := range []string{keyID, keyName, keyDescription, keyType, keyLogic, keyDecisionStrategy, keyResourceType} {
		if value, ok := stored[key]; ok {
			rep[key] = value
		}
	}
	if wantType == clientPolicyType {
		rep["clients"] = vary(kc, anyList(stored["clients"]))
	}
	writeJSON(w, http.StatusOK, rep)
}

func (kc *fakeKeycloakAdmin) updateAuthz(w http.ResponseWriter, r *http.Request, body []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	stored := kc.authzByID(r.PathValue("pid"))
	if stored == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rep := rawObject(body)
	if status := kc.refuseAuthz(rep); status != 0 {
		w.WriteHeader(status)
		return
	}
	// Keycloak sets the description from the body, so an update without one clears it.
	delete(stored, keyDescription)
	for _, key := range []string{keyName, keyDescription, keyLogic, keyDecisionStrategy, "clients", keyResourceType, keyScopes, keyPolicies} {
		if value, ok := rep[key]; ok {
			stored[key] = value
		}
	}
	if stored[keyType] == scopePermissionType {
		stored[keyResources] = storedResources(rep, stored[keyResources])
	}
	kc.writes = append(kc.writes, fmt.Sprintf("correct %s %s", authzLabel(stored), stringField(stored, keyName)))
	writeJSON(w, http.StatusCreated, rep)
}

// serveAuthzPart answers a permission's resources, scopes or associated policies, by name.
func (kc *fakeKeycloakAdmin) serveAuthzPart(w http.ResponseWriter, r *http.Request, _ []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	stored := kc.authzByID(r.PathValue("pid"))
	key := map[string]string{"resources": keyResources, "scopes": keyScopes, "associatedPolicies": keyPolicies}[r.PathValue("part")]
	if stored == nil || key == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	parts := []object{}
	for _, name := range anyList(stored[key]) {
		parts = append(parts, object{keyName: name})
	}
	writeJSON(w, http.StatusOK, vary(kc, parts))
}

func (kc *fakeKeycloakAdmin) deleteAuthz(w http.ResponseWriter, r *http.Request, _ []byte) {
	if !kc.authzServer(w, r) {
		return
	}
	i := slices.IndexFunc(kc.authz, func(o object) bool { return o[keyID] == r.PathValue("pid") })
	if i < 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	kc.writes = append(kc.writes, fmt.Sprintf("delete %s %s", authzLabel(kc.authz[i]), stringField(kc.authz[i], keyName)))
	kc.authz = slices.Delete(kc.authz, i, i+1)
	w.WriteHeader(http.StatusNoContent)
}

// --- Fake state helpers; callers hold the lock or run before the server starts ---------------

func (kc *fakeKeycloakAdmin) newID() string {
	kc.nextID++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", kc.nextID)
}

// addClient stores a client as Keycloak creates one: with an ID, Keycloak's defaults, a generated secret when it is
// confidential, and the realm default optional scopes of that moment.
func (kc *fakeKeycloakAdmin) addClient(rep object) string {
	id := kc.newID()
	client := object{"surrogateAuthRequired": false, "alwaysDisplayInConsole": false, "redirectUris": []any{},
		"webOrigins": []any{}, "notBefore": 0, "frontchannelLogout": false, "nodeReRegistrationTimeout": -1,
		"defaultClientScopes": []any{"web-origins", "acr", "profile", "roles", "basic", "email"},
		"access":              object{"view": true, "configure": true, "manage": true}}
	maps.Copy(client, rep)
	attributes := object{"realm_client": kcFalse, testCreationAttribute: testCreationTime}
	if present, ok := rep[keyAttributes].(object); ok {
		maps.Copy(attributes, present)
	}
	client[keyID], client[keyAttributes] = id, attributes
	if client["publicClient"] != true {
		client["secret"] = testClientSecret
	}
	kc.clients = append(kc.clients, client)
	kc.clientOptional[id] = slices.Clone(kc.realmOptional)
	return id
}

func (kc *fakeKeycloakAdmin) addGroup(name string) string {
	id := kc.newID()
	kc.groups = append(kc.groups, object{keyID: id, keyName: name, "path": "/" + name, "subGroupCount": 0, "subGroups": []any{}})
	return id
}

func (kc *fakeKeycloakAdmin) created(w http.ResponseWriter, collection, id string) {
	w.Header().Set("Location", kc.srv.URL+testRealmRoot+"/"+collection+"/"+id)
	w.WriteHeader(http.StatusCreated)
}

func (kc *fakeKeycloakAdmin) findClient(clientID string) object {
	i := slices.IndexFunc(kc.clients, func(c object) bool { return c[keyClientID] == clientID })
	if i < 0 {
		return nil
	}
	return kc.clients[i]
}

func (kc *fakeKeycloakAdmin) clientByID(id string) object { return findByID(kc.clients, id) }
func (kc *fakeKeycloakAdmin) scopeByID(id string) object  { return findByID(kc.scopes, id) }
func (kc *fakeKeycloakAdmin) scopeByName(name string) object {
	return findByName(kc.scopes, name)
}
func (kc *fakeKeycloakAdmin) authzByID(id string) object { return findByID(kc.authz, id) }
func (kc *fakeKeycloakAdmin) authzByName(name string) object {
	return findByName(kc.authz, name)
}

// groupByName returns the top-level group with this name.
func (kc *fakeKeycloakAdmin) groupByName(name string) object {
	i := slices.IndexFunc(kc.groups, func(g object) bool { return g[keyName] == name && g["path"] == "/"+name })
	if i < 0 {
		return nil
	}
	return kc.groups[i]
}

func (kc *fakeKeycloakAdmin) scopeRefs(ids []string) []object {
	refs := []object{}
	for _, id := range ids {
		refs = append(refs, object{keyID: id, keyName: stringField(kc.scopeByID(id), keyName)})
	}
	return refs
}

func (kc *fakeKeycloakAdmin) scopeNames(ids []string) []any {
	names := []any{}
	for _, id := range ids {
		names = append(names, stringField(kc.scopeByID(id), keyName))
	}
	return names
}

func (kc *fakeKeycloakAdmin) scopeMapper(scope, mapper string) object {
	return findByName(scopeMappers(kc.scopeByName(scope)), mapper)
}

func (kc *fakeKeycloakAdmin) entryNamed(entries []any, name string) object {
	if i := indexByName(entries, name); i >= 0 {
		entry, _ := entries[i].(object)
		return entry
	}
	return nil
}

func scopeMappers(scope object) []object {
	mappers, _ := scope[keyProtocolMappers].([]object)
	return mappers
}

func findByID(list []object, id string) object {
	i := slices.IndexFunc(list, func(o object) bool { return o[keyID] == id })
	if i < 0 {
		return nil
	}
	return list[i]
}

// vary rotates a list by the number of reads so far: Keycloak returns mapper and scope lists in no fixed order.
func vary[T any](kc *fakeKeycloakAdmin, list []T) []T {
	if len(list) < 2 {
		return append([]T{}, list...)
	}
	k := kc.reads % len(list)
	return append(slices.Clone(list[k:]), list[:k]...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set(contentTypeKey, contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func rawObject(raw []byte) object {
	var o object
	_ = json.Unmarshal(raw, &o)
	return o
}

func anyList(v any) []any {
	list, _ := v.([]any)
	return list
}

func anyObjects(v any) []object {
	list := []object{}
	for _, item := range anyList(v) {
		if o, ok := item.(object); ok {
			list = append(list, o)
		}
	}
	return list
}

// --- Test-side access to the fake; each takes the lock ---------------------------------------

// edit changes the fake's state, as an administrator or another tool would between two passes.
func (kc *fakeKeycloakAdmin) edit(change func()) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	change()
}

// reset clears the request log, the write labels and the overrides, so a test sees only the next pass.
func (kc *fakeKeycloakAdmin) reset() {
	kc.edit(func() {
		kc.requests, kc.writes = nil, nil
		kc.hits, kc.overrides = map[string]int{}, map[kcHit]func(http.ResponseWriter){}
	})
}

// respond makes the nth request of route since the last reset get this answer instead of the fake's own.
func (kc *fakeKeycloakAdmin) respond(route string, nth int, answer func(http.ResponseWriter)) {
	kc.edit(func() { kc.overrides[kcHit{route, nth}] = answer })
}

func (kc *fakeKeycloakAdmin) writeLog() []string {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return slices.Clone(kc.writes)
}

func (kc *fakeKeycloakAdmin) requestLog() []kcRequest {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return slices.Clone(kc.requests)
}

// hitSequence names every request since the last reset by its route and its count on that route.
func (kc *fakeKeycloakAdmin) hitSequence() []kcHit {
	counts := map[string]int{}
	var hits []kcHit
	for _, r := range kc.requestLog() {
		counts[r.route]++
		hits = append(hits, kcHit{r.route, counts[r.route]})
	}
	return hits
}

// body returns the body of the nth request of route since the last reset.
func (kc *fakeKeycloakAdmin) body(t *testing.T, route string, nth int) string {
	t.Helper()
	n := 0
	for _, r := range kc.requestLog() {
		if r.route == route {
			if n++; n == nth {
				return string(r.body)
			}
		}
	}
	t.Fatalf("no request #%d for %s", nth, route)
	return ""
}

func (kc *fakeKeycloakAdmin) hitCount(route string) int {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return kc.hits[route]
}

// deletes counts the DELETE requests since the last reset.
func (kc *fakeKeycloakAdmin) deletes() int {
	n := 0
	for _, r := range kc.requestLog() {
		if r.method == http.MethodDelete {
			n++
		}
	}
	return n
}

// snapshot returns a deep copy of what read returns, taken under the lock.
func (kc *fakeKeycloakAdmin) snapshot(t *testing.T, read func() any) object {
	t.Helper()
	kc.mu.Lock()
	raw, err := json.Marshal(read())
	kc.mu.Unlock()
	require.NoError(t, err)
	var o object
	require.NoError(t, json.Unmarshal(raw, &o))
	return o
}

func (kc *fakeKeycloakAdmin) mcpClientRep(t *testing.T) object {
	return kc.snapshot(t, func() any { return kc.findClient(mcpClientID) })
}

func (kc *fakeKeycloakAdmin) scope(t *testing.T, name string) object {
	return kc.snapshot(t, func() any { return kc.scopeByName(name) })
}

func (kc *fakeKeycloakAdmin) authzObject(t *testing.T, name string) object {
	return kc.snapshot(t, func() any { return kc.authzByName(name) })
}

// optionalScopes returns the names of a client's optional scopes, or of the realm's default optional scopes when
// clientID is empty.
func (kc *fakeKeycloakAdmin) optionalScopes(clientID string) []any {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	if clientID == "" {
		return kc.scopeNames(kc.realmOptional)
	}
	return kc.scopeNames(kc.clientOptional[stringField(kc.findClient(clientID), keyID)])
}

func (kc *fakeKeycloakAdmin) listNames(list func() []any) []string {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	var names []string
	for _, entry := range anyObjects(list()) {
		names = append(names, stringField(entry, keyName))
	}
	return names
}

// lists returns deep copies of the realm's client profiles and client policies.
func (kc *fakeKeycloakAdmin) lists(t *testing.T) (profiles, policies []any) {
	t.Helper()
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return jsonList(t, kc.profiles), jsonList(t, kc.policies)
}

// list returns a deep copy of one of the realm's client policy lists, by the key Keycloak holds it under.
func (kc *fakeKeycloakAdmin) list(t *testing.T, key string) []any {
	t.Helper()
	profiles, policies := kc.lists(t)
	if key == keyProfiles {
		return profiles
	}
	return policies
}

func (kc *fakeKeycloakAdmin) delegationGroupMembers() []string {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return slices.Clone(kc.members[stringField(kc.groupByName(testMCPGroup), keyID)])
}

func (kc *fakeKeycloakAdmin) groupID(t *testing.T, name string) string {
	t.Helper()
	kc.mu.Lock()
	defer kc.mu.Unlock()
	group := kc.groupByName(name)
	require.NotNil(t, group, "group %s", name)
	return stringField(group, keyID)
}

// --- Test helpers ------------------------------------------------------------------------

func ensureMCPRealm(kc *fakeKeycloakAdmin, spec MCPRealmSpec) (string, error) {
	return NewHTTPOIDCRegistrar().EnsureMCPRealm(context.Background(), kc.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, spec)
}

// configured returns a fake whose realm a first pass with spec has set up, with the request log cleared.
func configured(t *testing.T, spec MCPRealmSpec, seeds ...func(*fakeKeycloakAdmin)) *fakeKeycloakAdmin {
	t.Helper()
	kc := newFakeKeycloakAdmin(t, seeds...)
	_, err := ensureMCPRealm(kc, spec)
	require.NoError(t, err)
	require.NotNil(t, kc.mcpClientRep(t), "the first pass sets the realm up")
	kc.reset()
	return kc
}

// configuredWithCIMDAndForeignEntries returns a realm set up with CIMD on whose client policy lists also hold another
// tool's profile and policy.
func configuredWithCIMDAndForeignEntries(t *testing.T) *fakeKeycloakAdmin {
	t.Helper()
	kc := configured(t, allUsersSpec)
	kc.edit(func() { withForeignEntries(kc) })
	return kc
}

// jsonList returns a list as JSON decodes it, so lists built in Go compare equal to lists read back.
func jsonList(t *testing.T, list any) []any {
	t.Helper()
	raw, err := json.Marshal(list)
	require.NoError(t, err)
	var decoded []any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	return decoded
}

// withoutNamed returns list without the entries named name.
func withoutNamed(list []any, name string) []any {
	return slices.DeleteFunc(slices.Clone(list), func(entry any) bool {
		o, _ := entry.(object)
		return stringField(o, keyName) == name
	})
}

func decodeObject(t *testing.T, raw string) object {
	t.Helper()
	var o object
	require.NoError(t, json.Unmarshal([]byte(raw), &o))
	return o
}

func decodeObjects(t *testing.T, raw string) []object {
	t.Helper()
	var list []object
	require.NoError(t, json.Unmarshal([]byte(raw), &list))
	return list
}

// canonicalScope is a scope without its IDs, its mappers sorted by name, as JSON.
func canonicalScope(t *testing.T, scope object) string {
	t.Helper()
	require.NotNil(t, scope, "the scope exists")
	scope = maps.Clone(scope)
	delete(scope, keyID)
	mappers := anyObjects(scope[keyProtocolMappers])
	for _, m := range mappers {
		delete(m, keyID)
	}
	sort.Slice(mappers, func(i, j int) bool { return stringField(mappers[i], keyName) < stringField(mappers[j], keyName) })
	scope[keyProtocolMappers] = mappers
	raw, err := json.Marshal(scope)
	require.NoError(t, err)
	return string(raw)
}

func attributesOf(o object) object {
	attributes, _ := o[keyAttributes].(object)
	return attributes
}

func configOf(o object) object {
	config, _ := o[keyConfig].(object)
	return config
}

// writesExcept returns the write requests of the last pass that are not the admin token request.
func writesExcept(kc *fakeKeycloakAdmin) []kcRequest {
	var writes []kcRequest
	for _, r := range kc.requestLog() {
		if r.method != http.MethodGet && r.route != routeToken {
			writes = append(writes, r)
		}
	}
	return writes
}

// requireSanitized checks a failure is a *Error with the status and retry flag, carrying no response body,
// credential, token, client secret or Keycloak address.
func requireSanitized(t *testing.T, kc *fakeKeycloakAdmin, err error, status int, retryable bool) {
	t.Helper()
	var rErr *Error
	require.ErrorAs(t, err, &rErr)
	assert.Equal(t, status, rErr.StatusCode)
	assert.Equal(t, retryable, rErr.Retryable)
	for _, leak := range []string{testLeakyKCBody, testClientSecret, testKCAdminPass, testKCAccessToken, testRotatedSecret,
		strings.TrimPrefix(kc.srv.URL, "http://")} {
		assert.NotContains(t, err.Error(), leak)
	}
}

func answerStatus(status int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(testLeakyKCBody))
	}
}

// --- Tests -------------------------------------------------------------------------------

// TestEnsureMCPRealmCreatesEverythingInAnEmptyRealm runs a first pass in a realm created before the delegation
// features. Each object is created before the objects that name it, and FGAP is enabled before the realm default
// optional scopes, so the admin-permissions client it creates does not inherit mcp:access and delegation:client. Every
// body is one Keycloak 26.8.1 accepts.
func TestEnsureMCPRealmCreatesEverythingInAnEmptyRealm(t *testing.T) {
	kc := newFakeKeycloakAdmin(t)

	secret, err := ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)
	assert.Equal(t, testClientSecret, secret)

	assert.Equal(t, []string{
		writeCreateAccessScope,
		writeCreateUpstreamScope,
		writeCreateDelegationScope,
		writeCreateMCPClient,
		writeAddUpstreamOptional,
		writeEnableFGAP,
		writeAddAccessDefault,
		writeAddDelegationDefault,
		writeProfiles,
		writePolicies,
		writeCreateClientPolicy,
		writeCreateAllUsers,
	}, kc.writeLog())

	mcpUUID := stringField(kc.mcpClientRep(t), keyID)
	assert.JSONEq(t, bodyAccessScope, kc.body(t, routeCreateScope, 1))
	assert.JSONEq(t, bodyUpstreamScope, kc.body(t, routeCreateScope, 2))
	assert.JSONEq(t, bodyDelegationScope, kc.body(t, routeCreateScope, 3))
	assert.JSONEq(t, bodyMCPClient, kc.body(t, routeCreateClient, 1))
	assert.JSONEq(t, `{"adminPermissionsEnabled": true}`, kc.body(t, routeUpdateRealm, 1))
	assert.JSONEq(t, bodyProfiles, kc.body(t, routeWriteProfiles, 1))
	assert.JSONEq(t, bodyPolicies, kc.body(t, routeWritePolicies, 1))
	assert.JSONEq(t, fmt.Sprintf(bodyClientPolicyTmpl, mcpUUID), kc.body(t, routeCreatePolicy, 1))
	assert.JSONEq(t, bodyAllUsersPermission, kc.body(t, routeCreatePermission, 1))

	assert.Contains(t, kc.optionalScopes(mcpClientID), mcpUpstreamScopeName)
	assert.Subset(t, kc.optionalScopes(""), []any{mcpAccessScopeName, delegationScopeName})
	adminPermissionsScopes := kc.optionalScopes(adminPermissionsClientID)
	assert.NotContains(t, adminPermissionsScopes, mcpAccessScopeName, "FGAP must be enabled before the realm defaults change")
	assert.NotContains(t, adminPermissionsScopes, delegationScopeName, "FGAP must be enabled before the realm defaults change")

	requests := kc.requestLog()
	require.NotEmpty(t, requests)
	assert.Equal(t, routeToken, requests[0].route, "the pass authenticates first")
	assert.Equal(t, 1, kc.hitCount(routeToken), "the pass authenticates once")
	assert.Equal(t, routeClientSecret, requests[len(requests)-1].route, "the secret is read last")
	assert.Zero(t, kc.hitCount(routeAnyOther), "no request outside the admin API the registrar uses")
}

// TestEnsureMCPRealmCreatesTheDelegationScopeWhenMissing checks a realm created before the delegation features gets
// Keycloak's own delegation:client: once Keycloak has stored the posted scope, it equals the scope Keycloak creates
// in a new realm, and it is a realm default optional scope.
func TestEnsureMCPRealmCreatesTheDelegationScopeWhenMissing(t *testing.T) {
	kc := newFakeKeycloakAdmin(t)

	_, err := ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)

	assert.Contains(t, kc.writeLog(), writeCreateDelegationScope)
	assert.JSONEq(t, canonicalScope(t, decodeObject(t, keycloakDelegationScopeJSON)),
		canonicalScope(t, kc.scope(t, delegationScopeName)))
	assert.Contains(t, kc.optionalScopes(""), delegationScopeName)
}

// TestEnsureMCPRealmReusesKeycloaksDelegationScope checks a realm created after the delegation features keeps
// Keycloak's own delegation:client: no create, no correction of the scope or its mappers, the same ID.
func TestEnsureMCPRealmReusesKeycloaksDelegationScope(t *testing.T) {
	kc := newFakeKeycloakAdmin(t, withKeycloakDelegationScope(t))

	_, err := ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)

	assert.Equal(t, []string{
		writeCreateAccessScope,
		writeCreateUpstreamScope,
		writeCreateMCPClient,
		writeAddUpstreamOptional,
		writeEnableFGAP,
		writeAddAccessDefault,
		writeProfiles,
		writePolicies,
		writeCreateClientPolicy,
		writeCreateAllUsers,
	}, kc.writeLog(), "everything but delegation:client is set up")
	assert.Contains(t, kc.optionalScopes(""), delegationScopeName)
	assert.Equal(t, "keycloak-delegation-scope", stringField(kc.scope(t, delegationScopeName), keyID))
	assert.JSONEq(t, canonicalScope(t, decodeObject(t, keycloakDelegationScopeJSON)),
		canonicalScope(t, kc.scope(t, delegationScopeName)))
}

// TestEnsureMCPRealmCorrectsOnlyOwnedFields drifts owned fields next to fields the operator does not own. A pass
// corrects exactly the owned ones, each with one merged write, and keeps the rest.
func TestEnsureMCPRealmCorrectsOnlyOwnedFields(t *testing.T) {
	tests := []struct {
		name   string
		drift  func(kc *fakeKeycloakAdmin)
		writes []string
		check  func(t *testing.T, kc *fakeKeycloakAdmin)
	}{
		{
			name: "client ilm-mcp: fullScopeAllowed is corrected, an unrelated attribute and a foreign policy are kept",
			drift: func(kc *fakeKeycloakAdmin) {
				client := kc.findClient(mcpClientID)
				client["fullScopeAllowed"] = true
				attributesOf(client)[testUnrelatedAttribute] = "keep"
				withForeignEntries(kc)
			},
			writes: []string{writeCorrectMCPClient},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				client := kc.mcpClientRep(t)
				assert.Equal(t, false, client["fullScopeAllowed"])
				assert.Equal(t, "keep", attributesOf(client)[testUnrelatedAttribute])
				assert.Equal(t, testCreationTime, attributesOf(client)[testCreationAttribute])
				assert.Equal(t, testClientSecret, client["secret"])

				put := decodeObject(t, kc.body(t, routeUpdateClient, 1))
				assert.NotContains(t, put, "secret", "the correction sends the representation without its secret")
				assert.Equal(t, testCreationTime, attributesOf(put)[testCreationAttribute])
				assert.Equal(t, "keep", attributesOf(put)[testUnrelatedAttribute])
				for key := range client {
					if key != "secret" {
						assert.Contains(t, put, key, "the correction sends the whole stored representation")
					}
				}
				assert.Contains(t, kc.listNames(func() []any { return kc.policies }), "foreign-policy")
			},
		},
		{
			name: "client scopes and mappers: an attribute, a mapper setting and a missing mapper",
			drift: func(kc *fakeKeycloakAdmin) {
				access := kc.scopeByName(mcpAccessScopeName)
				attributesOf(access)[includeInTokenScope] = kcFalse
				attributesOf(access)["gui.order"] = "7"
				config := configOf(kc.scopeMapper(mcpAccessScopeName, mcpPublicResourceMapper))
				config[includedCustomAudience] = "https://drift.example.invalid/mcp"
				config["foreign.setting"] = "keep"
				upstream := kc.scopeByName(mcpUpstreamScopeName)
				upstream[keyProtocolMappers] = slices.DeleteFunc(scopeMappers(upstream), func(m object) bool { return m[keyName] == ilmGroupsMapper })
			},
			writes: []string{writeCorrectAccessScope, "correct mapper mcp-public-resource of mcp:access",
				"add mapper Groups to ilm-mcp-upstream"},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				access := kc.scope(t, mcpAccessScopeName)
				assert.Equal(t, kcTrue, attributesOf(access)[includeInTokenScope])
				assert.Equal(t, "7", attributesOf(access)["gui.order"])
				assert.NotContains(t, decodeObject(t, kc.body(t, routeUpdateScope, 1)), keyProtocolMappers,
					"a scope correction leaves the mappers out")
				resource := findByName(anyObjects(access[keyProtocolMappers]), mcpPublicResourceMapper)
				assert.Equal(t, "https://"+testMCPHost+"/mcp", configOf(resource)[includedCustomAudience])
				assert.Equal(t, "keep", configOf(resource)["foreign.setting"])
				groups := findByName(anyObjects(kc.scope(t, mcpUpstreamScopeName)[keyProtocolMappers]), ilmGroupsMapper)
				assert.Equal(t, "roles", configOf(groups)["claim.name"])
			},
		},
		{
			name: "client profiles and policies: MCP's entries are corrected, foreign entries kept",
			drift: func(kc *fakeKeycloakAdmin) {
				withForeignEntries(kc)
				cimd := kc.entryNamed(kc.profiles, mcpCIMDName)
				cimd[keyExecutors] = []any{object{"executor": cimdExecutor, keyConfiguration: object{}}}
				pkce := kc.entryNamed(kc.policies, mcpPKCEName)
				pkce[keyEnabled], pkce[keyDescription] = false, testKept
			},
			writes: []string{writeProfiles, writePolicies},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				profiles := decodeObject(t, kc.body(t, routeWriteProfiles, 1))[keyProfiles]
				policies := decodeObject(t, kc.body(t, routeWritePolicies, 1))[keyPolicies]
				assert.Contains(t, anyList(profiles), anyList(decodeObject(t, bodyProfiles)[keyProfiles])[1], "mcp-cimd is restored")
				assert.Contains(t, anyList(profiles), foreignProfile())
				pkce, _ := anyList(decodeObject(t, bodyPolicies)[keyPolicies])[0].(object)
				pkce[keyDescription] = testKept
				assert.Contains(t, anyList(policies), pkce, "mcp-pkce is enabled again and keeps its description")
				assert.Contains(t, anyList(policies), foreignPolicy())
			},
		},
		{
			name: "delegating client policy: logic is corrected, a description is kept",
			drift: func(kc *fakeKeycloakAdmin) {
				policy := kc.authzByName(mcpClientPolicyName)
				policy[keyLogic], policy[keyDescription] = "NEGATIVE", testKept
			},
			writes: []string{"correct client policy ilm-mcp-delegating-client"},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				policy := kc.authzObject(t, mcpClientPolicyName)
				assert.Equal(t, positiveLogic, policy[keyLogic])
				assert.Equal(t, testKept, policy[keyDescription])
				assert.Equal(t, []any{stringField(kc.mcpClientRep(t), keyID)}, policy["clients"])
			},
		},
		{
			name: "delegation permission: its scope is corrected, its resource and a description are kept",
			drift: func(kc *fakeKeycloakAdmin) {
				permission := kc.authzByName(mcpAllUsersPermissionName)
				permission[keyScopes], permission[keyDescription] = []any{"view"}, testKept
			},
			writes: []string{writeCorrectAllUsers},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				permission := kc.authzObject(t, mcpAllUsersPermissionName)
				assert.Equal(t, []any{"delegate"}, permission[keyScopes])
				assert.Equal(t, []any{usersResourceType}, permission[keyResources])
				assert.Equal(t, testKept, permission[keyDescription])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := configured(t, allUsersSpec)
			kc.edit(func() { tt.drift(kc) })

			_, err := ensureMCPRealm(kc, allUsersSpec)
			require.NoError(t, err)

			assert.Equal(t, tt.writes, kc.writeLog())
			tt.check(t, kc)

			kc.reset()
			_, err = ensureMCPRealm(kc, allUsersSpec)
			require.NoError(t, err)
			assert.Empty(t, kc.writeLog(), "the corrected realm needs no further write")
		})
	}
}

// TestEnsureMCPRealmNeverDeletes checks a pass deletes nothing while the delegation form stays the same, even with
// drift and foreign objects next to MCP's.
func TestEnsureMCPRealmNeverDeletes(t *testing.T) {
	tests := []struct {
		name   string
		first  MCPRealmSpec
		second MCPRealmSpec
		change func(kc *fakeKeycloakAdmin)
		check  func(t *testing.T, kc *fakeKeycloakAdmin)
	}{
		{name: "all-users form again", first: allUsersSpec, second: allUsersSpec},
		{name: "group form again", first: groupSpec, second: groupSpec},
		{
			name: "drift next to foreign objects", first: allUsersSpec, second: allUsersSpec,
			change: func(kc *fakeKeycloakAdmin) {
				access := kc.scopeByName(mcpAccessScopeName)
				access[keyProtocolMappers] = append(scopeMappers(access), object{keyID: "foreign-mapper-id", keyName: "foreign-mapper",
					keyProtocol: openIDConnectProtocol, keyProtocolMapper: "oidc-audience-mapper", keyConfig: object{}})
				attributesOf(access)[includeInTokenScope] = kcFalse
				kc.authz = append(kc.authz, object{keyID: "foreign-policy-id", keyName: "foreign-client-policy", keyType: clientPolicyType,
					keyLogic: positiveLogic, keyDecisionStrategy: unanimousStrategy, "clients": []any{}})
				mcpID := stringField(kc.findClient(mcpClientID), keyID)
				kc.clientOptional[mcpID] = append(kc.clientOptional[mcpID], stringField(kc.scopeByName("phone"), keyID))
			},
			check: func(t *testing.T, kc *fakeKeycloakAdmin) {
				assert.NotNil(t, findByName(anyObjects(kc.scope(t, mcpAccessScopeName)[keyProtocolMappers]), "foreign-mapper"))
				assert.NotNil(t, kc.authzObject(t, "foreign-client-policy"))
				assert.Contains(t, kc.optionalScopes(mcpClientID), "phone")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := configured(t, tt.first, withGroupMember)
			if tt.change != nil {
				kc.edit(func() { tt.change(kc) })
			}

			secret, err := ensureMCPRealm(kc, tt.second)
			require.NoError(t, err)
			assert.Equal(t, testClientSecret, secret)

			assert.Zero(t, kc.deletes(), "no DELETE while the form stays the same")
			permission := mcpAllUsersPermissionName
			if tt.second.DelegationGroup != "" {
				permission = mcpGroupPermissionName
			}
			assert.NotNil(t, kc.authzObject(t, permission))
			assert.NotNil(t, kc.authzObject(t, mcpClientPolicyName))
			assert.Equal(t, []string{testMemberID}, kc.delegationGroupMembers())
			if tt.check != nil {
				tt.check(t, kc)
			}
		})
	}
}

// TestEnsureMCPRealmSwitchingFormsRemovesOnlyTheOtherPermission checks a change of delegation form deletes exactly
// the operator's own permission of the other form, and keeps the group and its members.
func TestEnsureMCPRealmSwitchingFormsRemovesOnlyTheOtherPermission(t *testing.T) {
	tests := []struct {
		name     string
		from, to MCPRealmSpec
		writes   []string
		kept     string
	}{
		{"all users to a group", allUsersSpec, groupSpec,
			[]string{writeDeleteAllUsers, writeCreateGroupPermission}, mcpGroupPermissionName},
		{"a group back to all users", groupSpec, allUsersSpec,
			[]string{writeDeleteGroupPermission, writeCreateAllUsers}, mcpAllUsersPermissionName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := configured(t, tt.from, withGroupMember)

			_, err := ensureMCPRealm(kc, tt.to)
			require.NoError(t, err)

			assert.Equal(t, 1, kc.deletes(), "exactly one DELETE")
			assert.Equal(t, tt.writes, kc.writeLog(), "the other form's permission goes before the configured one is created")
			assert.Equal(t, []string{testMemberID}, kc.delegationGroupMembers(), "the group and its members stay")
			assert.NotNil(t, kc.authzObject(t, tt.kept))
			assert.NotNil(t, kc.authzObject(t, mcpClientPolicyName))
			assert.Len(t, kc.listNames(func() []any {
				var permissions []any
				for _, o := range kc.authz {
					if o[keyType] == scopePermissionType {
						permissions = append(permissions, o)
					}
				}
				return permissions
			}), 1, "only the configured form's permission remains")
		})
	}
}

// TestEnsureMCPRealmIsIdempotent checks a second pass over a configured realm sends no write, although Keycloak
// returns lists in another order and stores mappers with keys the registrar did not send.
func TestEnsureMCPRealmIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		spec  MCPRealmSpec
		seeds []func(*fakeKeycloakAdmin)
	}{
		{name: "all-users form", spec: allUsersSpec},
		{name: "all-users form without CIMD", spec: cimdOffSpec},
		{name: "group form", spec: groupSpec},
		{name: "Keycloak's own delegation scope", spec: allUsersSpec, seeds: []func(*fakeKeycloakAdmin){withKeycloakDelegationScope(t)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := configured(t, tt.spec, tt.seeds...)

			secret, err := ensureMCPRealm(kc, tt.spec)
			require.NoError(t, err)
			assert.Equal(t, testClientSecret, secret)

			assert.Empty(t, kc.writeLog())
			assert.Empty(t, writesExcept(kc), "the second pass only reads")
		})
	}
}

// TestEnsureMCPRealmIsIdempotentAfterPartialFailure fails the permission create once with a 500. The pass returns a
// retryable error, and the next pass creates the permission without creating anything twice.
func TestEnsureMCPRealmIsIdempotentAfterPartialFailure(t *testing.T) {
	kc := newFakeKeycloakAdmin(t)
	kc.respond(routeCreatePermission, 1, answerStatus(http.StatusInternalServerError))

	secret, err := ensureMCPRealm(kc, allUsersSpec)
	require.Error(t, err)
	assert.Empty(t, secret)
	requireSanitized(t, kc, err, http.StatusInternalServerError, true)
	assert.Nil(t, kc.authzObject(t, mcpAllUsersPermissionName))
	firstPass := kc.writeLog()

	kc.reset()
	secret, err = ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)
	assert.Equal(t, testClientSecret, secret)
	assert.Equal(t, []string{writeCreateAllUsers}, kc.writeLog())
	assert.NotContains(t, firstPass, writeCreateAllUsers)
	assert.NotNil(t, kc.authzObject(t, mcpAllUsersPermissionName))
}

// TestEnsureMCPRealmGroupForm checks the group form: the group is created when it is missing and reused when it
// exists, its members untouched, and the group permission is the only delegation permission.
func TestEnsureMCPRealmGroupForm(t *testing.T) {
	tests := []struct {
		name        string
		seeds       []func(*fakeKeycloakAdmin)
		createGroup bool
		members     []string
	}{
		{name: "creates the missing group", createGroup: true},
		{name: "uses the existing group", seeds: []func(*fakeKeycloakAdmin){withGroupMember}, members: []string{testMemberID}},
		{name: "creates a top-level group next to a nested one of that name", seeds: []func(*fakeKeycloakAdmin){withNestedGroup}, createGroup: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := newFakeKeycloakAdmin(t, tt.seeds...)

			_, err := ensureMCPRealm(kc, groupSpec)
			require.NoError(t, err)

			for _, r := range kc.requestLog() {
				if r.method == http.MethodGet && r.path == testGroupsPath {
					assert.Equal(t, url.Values{"search": {testMCPGroup}, "exact": {kcTrue}}, r.query, "the group is looked up by exact name")
				}
			}
			writes := kc.writeLog()
			require.NotEmpty(t, writes)
			assert.Equal(t, tt.createGroup, slices.Contains(writes, writeCreateGroup))
			assert.Equal(t, writeCreateGroupPermission, writes[len(writes)-1])
			groupID := kc.groupID(t, testMCPGroup)
			assert.JSONEq(t, fmt.Sprintf(bodyGroupPermissionTmpl, groupID), kc.body(t, routeCreatePermission, 1))
			permission := kc.authzObject(t, mcpGroupPermissionName)
			assert.Equal(t, groupsResourceType, permission[keyResourceType])
			assert.Equal(t, []any{groupID}, permission[keyResources])
			assert.Nil(t, kc.authzObject(t, mcpAllUsersPermissionName))
			if tt.members == nil {
				assert.Empty(t, kc.delegationGroupMembers())
			} else {
				assert.Equal(t, tt.members, kc.delegationGroupMembers())
			}
			assert.Zero(t, kc.hitCount(routeAnyOther), "nothing touches group membership")
		})
	}
}

// TestEnsureMCPRealmFollowsAChangedDelegationGroup checks that configuring another group moves the group permission
// to that group, created when it is missing, and leaves the previous group and its members alone.
func TestEnsureMCPRealmFollowsAChangedDelegationGroup(t *testing.T) {
	kc := configured(t, groupSpec, withGroupMember)
	other := MCPRealmSpec{PublicHost: testMCPHost, CIMD: true, DelegationGroup: testOtherGroup}

	_, err := ensureMCPRealm(kc, other)
	require.NoError(t, err)

	assert.Equal(t, []string{"create group " + testOtherGroup, "correct permission " + mcpGroupPermissionName}, kc.writeLog())
	assert.Equal(t, []any{kc.groupID(t, testOtherGroup)}, kc.authzObject(t, mcpGroupPermissionName)[keyResources])
	assert.Equal(t, []string{testMemberID}, kc.delegationGroupMembers(), "the previous group and its members stay")
	assert.Zero(t, kc.deletes())

	kc.reset()
	_, err = ensureMCPRealm(kc, other)
	require.NoError(t, err)
	assert.Empty(t, kc.writeLog(), "the moved permission needs no further write")
}

// TestEnsureMCPRealmWithoutCIMDRemovesTheCIMDPolicy checks that with CIMD off a pass takes mcp-cimd out of the realm's
// client policies and then its client profiles, before any other list write, keeps every other entry, and leaves a
// realm that the next pass does not write to. Keycloak without its cimd feature refuses list writes that still hold
// mcp-cimd, so a realm that has already lost the feature recovers only because the removal comes first.
func TestEnsureMCPRealmWithoutCIMDRemovesTheCIMDPolicy(t *testing.T) {
	pkceProfile := anyList(decodeObject(t, bodyProfilesWithoutCIMD)[keyProfiles])
	pkcePolicy := anyList(decodeObject(t, bodyPoliciesWithoutCIMD)[keyPolicies])
	tests := []struct {
		name       string
		prepare    func(t *testing.T) *fakeKeycloakAdmin
		listWrites []string // the writes of the realm's client policy lists, in order
		onlyLists  bool     // the pass writes nothing but these lists
		profiles   []any    // the client profiles the realm holds afterwards
		policies   []any
	}{
		{
			name:       "a realm that never had CIMD gets only mcp-pkce",
			prepare:    func(t *testing.T) *fakeKeycloakAdmin { return newFakeKeycloakAdmin(t) },
			listWrites: []string{writeProfiles, writePolicies},
			profiles:   pkceProfile,
			policies:   pkcePolicy,
		},
		{
			name:       "a realm set up with CIMD loses mcp-cimd, policies first",
			prepare:    configuredWithCIMDAndForeignEntries,
			listWrites: []string{writePolicies, writeProfiles},
			onlyLists:  true,
			profiles:   append(slices.Clone(pkceProfile), foreignProfile()),
			policies:   append(slices.Clone(pkcePolicy), foreignPolicy()),
		},
		{
			name: "a realm whose Keycloak already lost the cimd feature recovers before mcp-pkce is corrected",
			prepare: func(t *testing.T) *fakeKeycloakAdmin {
				kc := configuredWithCIMDAndForeignEntries(t)
				kc.edit(func() {
					kc.cimdOff = true
					kc.entryNamed(kc.policies, mcpPKCEName)[keyEnabled] = false
				})
				return kc
			},
			listWrites: []string{writePolicies, writeProfiles, writePolicies},
			onlyLists:  true,
			profiles:   append(slices.Clone(pkceProfile), foreignProfile()),
			policies:   append(slices.Clone(pkcePolicy), foreignPolicy()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := tt.prepare(t)

			_, err := ensureMCPRealm(kc, cimdOffSpec)
			require.NoError(t, err)

			writes := kc.writeLog()
			assert.Equal(t, tt.listWrites, slices.DeleteFunc(slices.Clone(writes), func(w string) bool {
				return w != writeProfiles && w != writePolicies
			}))
			if tt.onlyLists {
				assert.Equal(t, tt.listWrites, writes, "nothing but the lists is written")
			} else {
				assert.JSONEq(t, bodyProfilesWithoutCIMD, kc.body(t, routeWriteProfiles, 1))
				assert.JSONEq(t, bodyPoliciesWithoutCIMD, kc.body(t, routeWritePolicies, 1))
			}
			profiles, policies := kc.lists(t)
			assert.Equal(t, jsonList(t, tt.profiles), profiles)
			assert.Equal(t, jsonList(t, tt.policies), policies)
			assert.Zero(t, kc.deletes(), "mcp-cimd leaves with the list writes, never a DELETE")

			kc.reset()
			_, err = ensureMCPRealm(kc, cimdOffSpec)
			require.NoError(t, err)
			assert.Empty(t, kc.writeLog(), "the next pass writes nothing")
		})
	}
}

// TestRemoveMCPCIMD checks the removal the operator runs before Keycloak loses its cimd feature: it writes the client
// policies and then the client profiles without mcp-cimd, keeps every other entry as stored, writes nothing when
// mcp-cimd is absent, never sends a DELETE, and fails with a sanitized error.
func TestRemoveMCPCIMD(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T) *fakeKeycloakAdmin
		writes  []string
		status  int // the status of the error the call returns, 0 when it succeeds
	}{
		{
			name:    "removes mcp-cimd from the policies, then the profiles, and keeps mcp-pkce and foreign entries",
			prepare: configuredWithCIMDAndForeignEntries,
			writes:  []string{writePolicies, writeProfiles},
		},
		{
			name: "writes nothing when mcp-cimd is absent",
			prepare: func(t *testing.T) *fakeKeycloakAdmin {
				kc := configured(t, cimdOffSpec)
				kc.edit(func() { withForeignEntries(kc) })
				return kc
			},
		},
		{
			name: "also removes mcp-cimd once Keycloak has lost the cimd feature",
			prepare: func(t *testing.T) *fakeKeycloakAdmin {
				kc := configuredWithCIMDAndForeignEntries(t)
				kc.edit(func() { kc.cimdOff = true })
				return kc
			},
			writes: []string{writePolicies, writeProfiles},
		},
		{
			name: "a failed write is retryable and carries no body",
			prepare: func(t *testing.T) *fakeKeycloakAdmin {
				kc := configuredWithCIMDAndForeignEntries(t)
				kc.respond(routeWritePolicies, 1, answerStatus(http.StatusInternalServerError))
				return kc
			},
			status: http.StatusInternalServerError,
		},
		{
			name: "a failed login is retryable and carries no body",
			prepare: func(t *testing.T) *fakeKeycloakAdmin {
				kc := configuredWithCIMDAndForeignEntries(t)
				kc.respond(routeToken, 1, answerStatus(http.StatusInternalServerError))
				return kc
			},
			status: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := tt.prepare(t)
			profilesBefore, policiesBefore := kc.lists(t)

			err := NewHTTPOIDCRegistrar().RemoveMCPCIMD(context.Background(), kc.srv.URL, testKCRealm,
				testKCAdminUser, testKCAdminPass)

			assert.Zero(t, kc.deletes(), "removal is the list write, never a DELETE")
			assert.Equal(t, tt.writes, kc.writeLog())
			if tt.status != 0 {
				requireSanitized(t, kc, err, tt.status, true)
				return
			}
			require.NoError(t, err)
			profiles, policies := kc.lists(t)
			assert.Equal(t, withoutNamed(profilesBefore, mcpCIMDName), profiles, "every other profile stays as stored")
			assert.Equal(t, withoutNamed(policiesBefore, mcpCIMDName), policies, "every other policy stays as stored")
		})
	}
}

// TestMCPRealmRefusesAClientPolicyDocumentWithoutItsList: Keycloak writes a client policy list as a whole, so a pass that
// took an answer without the list for an empty one would write back only MCP's entries and delete every other profile
// or policy of the realm, and a removal would report mcp-cimd gone while it is still stored. Such an answer ends the
// pass with a retryable *Error and no write to that list, so the realm keeps every entry it holds.
func TestMCPRealmRefusesAClientPolicyDocumentWithoutItsList(t *testing.T) {
	ensure := func(kc *fakeKeycloakAdmin) error {
		_, err := ensureMCPRealm(kc, allUsersSpec)
		return err
	}
	remove := func(kc *fakeKeycloakAdmin) error {
		return NewHTTPOIDCRegistrar().RemoveMCPCIMD(context.Background(), kc.srv.URL, testKCRealm, testKCAdminUser, testKCAdminPass)
	}
	passes := []struct {
		name             string
		pass             func(*fakeKeycloakAdmin) error
		key, read, write string
	}{
		{"ensuring reads the profiles", ensure, keyProfiles, routeReadProfiles, routeWriteProfiles},
		{"ensuring reads the policies", ensure, keyPolicies, routeReadPolicies, routeWritePolicies},
		{"removing reads the policies", remove, keyPolicies, routeReadPolicies, routeWritePolicies},
		{"removing reads the profiles", remove, keyProfiles, routeReadProfiles, routeWriteProfiles},
	}
	documents := []struct{ name, body string }{
		{"a document without the list", `{}`},
		{"a null list", `{"profiles": null, "policies": null}`},
	}
	for _, tt := range passes {
		for _, document := range documents {
			t.Run(tt.name+"/"+document.name, func(t *testing.T) {
				kc := configuredWithCIMDAndForeignEntries(t)
				before := kc.list(t, tt.key)
				kc.respond(tt.read, 1, func(w http.ResponseWriter) { _, _ = w.Write([]byte(document.body)) })

				err := tt.pass(kc)

				requireSanitized(t, kc, err, 0, true)
				assert.Contains(t, err.Error(), "client "+tt.key)
				assert.Zero(t, kc.hitCount(tt.write), "nothing is written to a list that was not read")
				assert.Equal(t, before, kc.list(t, tt.key), "the realm keeps every entry it holds")
			})
		}
	}
}

// TestEnsureMCPRealmRestoresANarrowedAllUsersPermission narrows the all-users permission to one user, which only its
// resources show. The next pass restores every user with an update that names an empty resource list, as Keycloak
// keeps a permission's resources when an update leaves the key out. The pass after that writes nothing.
func TestEnsureMCPRealmRestoresANarrowedAllUsersPermission(t *testing.T) {
	kc := configured(t, allUsersSpec)
	kc.edit(func() { kc.authzByName(mcpAllUsersPermissionName)[keyResources] = []any{testMemberID} })

	_, err := ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)

	assert.Equal(t, []string{writeCorrectAllUsers}, kc.writeLog())
	assert.Equal(t, []any{}, decodeObject(t, kc.body(t, routeUpdatePermission, 1))[keyResources],
		"the update names an empty resource list")
	assert.Equal(t, []any{usersResourceType}, kc.authzObject(t, mcpAllUsersPermissionName)[keyResources],
		"every user is delegable again")
	assert.Zero(t, kc.deletes())

	kc.reset()
	_, err = ensureMCPRealm(kc, allUsersSpec)
	require.NoError(t, err)
	assert.Empty(t, kc.writeLog(), "the restored permission needs no further write")
	assert.Zero(t, kc.deletes())
}

// TestEnsureMCPRealmLeavesARotatingClientAlone gives ilm-mcp secret-rotation state and drift. No client PUT is sent,
// every other object is still ensured, and the secret comes back with a non-retryable error naming the drift.
func TestEnsureMCPRealmLeavesARotatingClientAlone(t *testing.T) {
	fullScope := func(client object) { client["fullScopeAllowed"] = true }
	exchangeOff := func(client object) { attributesOf(client)["standard.token.exchange.enabled"] = kcFalse }
	tests := []struct {
		name      string
		attribute string
		value     string
		drift     func(client object)
		names     []string // what the error names; empty: no error
		corrected bool
	}{
		{"rotation enabled", "client.secret.rotation.enabled", kcTrue, fullScope, []string{"fullScopeAllowed"}, false},
		{"expiration time", "client.secret.expiration.time", "1761686400", fullScope, []string{"fullScopeAllowed"}, false},
		{"remaining expiration time", "client.secret.remaining.expiration.time", "600", fullScope, []string{"fullScopeAllowed"}, false},
		{"rotated secret", testRotatedAttribute, testRotatedSecret, fullScope, []string{"fullScopeAllowed"}, false},
		{"rotated creation time", "client.secret.rotated.creation.time", "1761600000", fullScope, []string{"fullScopeAllowed"}, false},
		{"rotated expiration time", "client.secret.rotated.expiration.time", "1761603600", fullScope, []string{"fullScopeAllowed"}, false},
		{"drift in an owned attribute", testRotatedAttribute, testRotatedSecret, exchangeOff,
			[]string{"attributes.standard.token.exchange.enabled"}, false},
		{"rotation state without drift", testRotatedAttribute, testRotatedSecret, func(object) {
			// The client keeps the configuration the first pass set.
		}, nil, false},
		{"an empty rotation attribute is no rotation state", testRotatedAttribute, "", fullScope, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := configured(t, allUsersSpec)
			kc.edit(func() {
				client := kc.findClient(mcpClientID)
				attributesOf(client)[tt.attribute] = tt.value
				tt.drift(client)
				attributesOf(kc.scopeByName(mcpAccessScopeName))[includeInTokenScope] = kcFalse
				kc.authz = slices.DeleteFunc(kc.authz, func(o object) bool { return o[keyName] == mcpAllUsersPermissionName })
			})
			before := kc.mcpClientRep(t)

			secret, err := ensureMCPRealm(kc, allUsersSpec)

			assert.Equal(t, testClientSecret, secret, "the secret comes back either way")
			assert.Equal(t, tt.corrected, slices.Contains(kc.writeLog(), writeCorrectMCPClient))
			assert.Contains(t, kc.writeLog(), writeCorrectAccessScope, "the other objects are still corrected")
			assert.Contains(t, kc.writeLog(), writeCreateAllUsers, "the other objects are still created")
			if !tt.corrected {
				assert.Equal(t, before, kc.mcpClientRep(t), "the client is left as it is")
			}
			if len(tt.names) == 0 {
				require.NoError(t, err)
				return
			}
			requireSanitized(t, kc, err, 0, false)
			for _, name := range tt.names {
				assert.Contains(t, err.Error(), name)
			}
		})
	}
}

// TestEnsureMCPRealmReturnsTheClientSecret checks the pass returns ilm-mcp's secret, for a new and for an existing
// client, read from its own endpoint and never rotated or sent anywhere.
func TestEnsureMCPRealmReturnsTheClientSecret(t *testing.T) {
	tests := []struct {
		name string
		kc   func(t *testing.T) *fakeKeycloakAdmin
	}{
		{"a new client", func(t *testing.T) *fakeKeycloakAdmin { return newFakeKeycloakAdmin(t) }},
		{"an existing client", func(t *testing.T) *fakeKeycloakAdmin { return configured(t, allUsersSpec) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := tt.kc(t)

			secret, err := ensureMCPRealm(kc, allUsersSpec)
			require.NoError(t, err)

			assert.Equal(t, testClientSecret, secret)
			var secretReads []kcRequest
			for _, r := range kc.requestLog() {
				assert.NotContains(t, string(r.body), testClientSecret, "the secret is never sent")
				if r.route == routeClientSecret {
					secretReads = append(secretReads, r)
				}
			}
			require.Len(t, secretReads, 1)
			assert.Equal(t, testRealmRoot+"/clients/"+stringField(kc.mcpClientRep(t), keyID)+"/client-secret", secretReads[0].path)
			assert.Zero(t, kc.hitCount(routeAnyOther), "no POST to client-secret, which would rotate it")
		})
	}
}

// TestEnsureMCPRealmErrorsCarryNoBody fails every request of three passes in turn: a first pass, a pass that
// corrects every kind of object, and a pass that switches forms. Each failure is a *Error with the status, retryable
// for a 5xx, and never carries the response body, a credential, a token, the client secret or Keycloak's address.
func TestEnsureMCPRealmErrorsCarryNoBody(t *testing.T) {
	scenarios := []struct {
		name    string
		prepare func(t *testing.T) *fakeKeycloakAdmin
		spec    MCPRealmSpec
	}{
		{"first pass", func(t *testing.T) *fakeKeycloakAdmin { return newFakeKeycloakAdmin(t) }, groupSpec},
		{"corrections", func(t *testing.T) *fakeKeycloakAdmin {
			kc := configured(t, allUsersSpec)
			kc.edit(func() { driftEveryKind(kc) })
			return kc
		}, allUsersSpec},
		{"switching forms", func(t *testing.T) *fakeKeycloakAdmin { return configured(t, allUsersSpec) }, groupSpec},
		{"switching CIMD off", func(t *testing.T) *fakeKeycloakAdmin { return configured(t, allUsersSpec) }, cimdOffSpec},
	}
	for _, sc := range scenarios {
		baseline := sc.prepare(t)
		_, err := ensureMCPRealm(baseline, sc.spec)
		require.NoError(t, err, sc.name)
		for _, hit := range baseline.hitSequence() {
			t.Run(sc.name+"/"+hit.String()+"/500", func(t *testing.T) {
				kc := sc.prepare(t)
				kc.respond(hit.route, hit.nth, answerStatus(http.StatusInternalServerError))
				secret, err := ensureMCPRealm(kc, sc.spec)
				assert.Empty(t, secret)
				requireSanitized(t, kc, err, http.StatusInternalServerError, true)
			})
			if !strings.HasPrefix(hit.route, http.MethodGet) {
				continue
			}
			t.Run(sc.name+"/"+hit.String()+"/undecodable", func(t *testing.T) {
				kc := sc.prepare(t)
				kc.respond(hit.route, hit.nth, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"` + testLeakyKCBody)) })
				_, err := ensureMCPRealm(kc, sc.spec)
				requireSanitized(t, kc, err, http.StatusOK, true)
			})
		}
	}

	t.Run("a refused request is not retryable", func(t *testing.T) {
		kc := newFakeKeycloakAdmin(t)
		kc.respond(routeCreateScope, 1, answerStatus(http.StatusBadRequest))
		_, err := ensureMCPRealm(kc, allUsersSpec)
		requireSanitized(t, kc, err, http.StatusBadRequest, false)
		assert.Contains(t, err.Error(), "creating client scope mcp:access")
	})
	t.Run("a transport error is retryable", func(t *testing.T) {
		kc := newFakeKeycloakAdmin(t)
		kc.respond(routeCreateScope, 1, func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
		})
		_, err := ensureMCPRealm(kc, allUsersSpec)
		requireSanitized(t, kc, err, 0, true)
	})
}

// driftEveryKind changes one owned field of every kind of MCP object, so the next pass corrects each kind.
func driftEveryKind(kc *fakeKeycloakAdmin) {
	attributesOf(kc.scopeByName(mcpAccessScopeName))[includeInTokenScope] = kcFalse
	configOf(kc.scopeMapper(mcpAccessScopeName, mcpPublicResourceMapper))[includedCustomAudience] = "https://drift.example.invalid/mcp"
	upstream := kc.scopeByName(mcpUpstreamScopeName)
	upstream[keyProtocolMappers] = slices.DeleteFunc(scopeMappers(upstream), func(m object) bool { return m[keyName] == ilmGroupsMapper })
	kc.findClient(mcpClientID)["fullScopeAllowed"] = true
	kc.entryNamed(kc.profiles, mcpPKCEName)[keyExecutors] = []any{}
	kc.entryNamed(kc.policies, mcpPKCEName)[keyEnabled] = false
	kc.authzByName(mcpClientPolicyName)[keyLogic] = "NEGATIVE"
	kc.authzByName(mcpAllUsersPermissionName)[keyScopes] = []any{"view"}
}

// TestEnsureMCPRealmChecksItsPrerequisites covers what the pass needs from the realm and from Keycloak's answers.
func TestEnsureMCPRealmChecksItsPrerequisites(t *testing.T) {
	tests := []struct {
		name      string
		spec      MCPRealmSpec
		seed      func(kc *fakeKeycloakAdmin)
		route     string
		nth       int
		answer    func(http.ResponseWriter)
		retryable bool
		message   string
	}{
		{name: "no public host", spec: MCPRealmSpec{CIMD: true}, message: "public host"},
		{
			name: "no ilm client yet", spec: allUsersSpec, retryable: true, message: "client ilm",
			seed: func(kc *fakeKeycloakAdmin) {
				kc.clients = slices.DeleteFunc(kc.clients, func(c object) bool { return c[keyClientID] == ilmClientID })
			},
		},
		{
			name: "an ilm client without a Groups mapper", spec: allUsersSpec, message: "Groups",
			seed: func(kc *fakeKeycloakAdmin) {
				id := stringField(kc.findClient(ilmClientID), keyID)
				kc.clientMappers[id] = slices.DeleteFunc(kc.clientMappers[id], func(m object) bool { return m[keyName] == ilmGroupsMapper })
			},
		},
		{
			name: "no admin-permissions client", spec: allUsersSpec, retryable: true, message: "admin-permissions",
			route: routeClients, nth: 3, answer: func(w http.ResponseWriter) { writeJSON(w, http.StatusOK, []object{}) },
		},
		{
			name: "a create without a Location header", spec: allUsersSpec, retryable: true, message: "client scope mcp:access",
			route: routeCreateScope, nth: 1, answer: func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seeds []func(*fakeKeycloakAdmin)
			if tt.seed != nil {
				seeds = append(seeds, tt.seed)
			}
			kc := newFakeKeycloakAdmin(t, seeds...)
			if tt.answer != nil {
				kc.respond(tt.route, tt.nth, tt.answer)
			}

			secret, err := ensureMCPRealm(kc, tt.spec)

			assert.Empty(t, secret)
			requireSanitized(t, kc, err, 0, tt.retryable)
			assert.Contains(t, err.Error(), tt.message)
			if tt.spec.PublicHost == "" {
				assert.Empty(t, kc.requestLog(), "nothing is sent without a public host")
			}
		})
	}
}

// TestCoversComparesOwnedFieldsRegardlessOfOrder pins the comparison every correction relies on.
func TestCoversComparesOwnedFieldsRegardlessOfOrder(t *testing.T) {
	tests := []struct {
		name           string
		actual, wanted any
		covered        bool
	}{
		{"equal text", "true", "true", true},
		{"a stored string for a boolean", "true", true, true},
		{"another value", "false", true, false},
		{"extra stored keys", object{"a": "1", "b": "2"}, object{"a": "1"}, true},
		{"a missing key", object{"b": "2"}, object{"a": "1"}, false},
		{"not an object", "x", object{"a": "1"}, false},
		{"a list in another order", []any{"b", "a", "c"}, []any{"a", "b", "c"}, true},
		{"a list with another element", []any{"a", "d"}, []any{"a", "b"}, false},
		{"a longer list", []any{"a", "b"}, []any{"a"}, false},
		{"not a list", "a", []any{"a"}, false},
		{"objects in another order", []any{object{"n": "y", "x": "1"}, object{"n": "x"}}, []any{object{"n": "x"}, object{"n": "y"}}, true},
		{"nested drift", object{"c": object{"k": "v2"}}, object{"c": object{"k": "v"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.covered, covers(tt.actual, tt.wanted))
		})
	}
}

// TestMCPRealmCallRefusesWhatItCannotSend covers the request-building failures of a single admin call.
func TestMCPRealmCallRefusesWhatItCannotSend(t *testing.T) {
	tests := []struct {
		name string
		root string
		body any
	}{
		{"a body that is not JSON", "http://keycloak.invalid", make(chan int)},
		{"an unusable address", "http://keycloak.invalid/\x7f", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &mcpRealm{registrar: NewHTTPOIDCRegistrar(), root: tt.root, token: testKCAccessToken}
			_, err := m.call(context.Background(), http.MethodPut, "/client-scopes", nil, tt.body, nil, "correcting a scope")
			var rErr *Error
			require.ErrorAs(t, err, &rErr)
			assert.False(t, rErr.Retryable)
			assert.Contains(t, err.Error(), "correcting a scope")
			assert.NotContains(t, err.Error(), "keycloak.invalid")
		})
	}
}
