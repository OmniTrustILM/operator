/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// MCPRealmSpec is what the operator needs to keep MCP's objects in a realm.
type MCPRealmSpec struct {
	// PublicHost is the platform's public host, from which MCP's resource URL https://<host>/mcp is built.
	PublicHost string
	// CIMD is whether the realm holds MCP's mcp-cimd client profile and policy; false removes them.
	CIMD bool
	// DelegationGroup is the top-level group whose members MCP may act for; empty lets MCP act for every user.
	DelegationGroup string
}

// keycloakAdminRealmPathTmpl is the admin REST root of a realm: /admin/realms/<realm>.
const keycloakAdminRealmPathTmpl = "/admin/realms/%s"

// secretRotationAttributes carry a client's secret-rotation state; client.secret.rotated holds the previous secret.
// Keycloak clears them on every client update unless a secret-rotation policy applies to the client, which revokes the
// previous secret while clients may still use it. The registrar therefore never updates a client that carries one of
// them, and reads only whether each is set.
var secretRotationAttributes = []string{
	"client.secret.rotation.enabled",
	"client.secret.expiration.time",
	"client.secret.remaining.expiration.time",
	"client.secret.rotated",
	"client.secret.rotated.creation.time",
	"client.secret.rotated.expiration.time",
}

// authzKind is a kind of object in the resource server of the admin-permissions client, where fine-grained admin
// permissions v2 keep their policies and permissions.
type authzKind struct {
	label      string // names the kind in error messages
	search     string // the lookup by name, relative to the resource server
	collection string // the typed endpoint that creates, reads and corrects, relative to the resource server
}

var (
	clientPolicyKind = authzKind{label: "client policy", search: "/policy/search", collection: "/policy/client"}
	permissionKind   = authzKind{label: "permission", search: "/permission/search", collection: "/permission/scope"}
)

// permissionParts are the endpoints that list a permission's resources, scopes and policies, which its typed
// representation leaves out, with the field each one fills for the comparison.
var permissionParts = []struct{ path, field string }{
	{"resources", keyResources},
	{"scopes", keyScopes},
	{"associatedPolicies", keyPolicies},
}

// clientPolicyList is one of the realm's two client policy lists. Keycloak reads and writes each list as a whole.
type clientPolicyList struct {
	path string // relative to the realm's admin root
	key  string // the key the document at path holds the list under
}

var (
	clientProfilesList = clientPolicyList{path: "/client-policies/profiles", key: keyProfiles}
	clientPoliciesList = clientPolicyList{path: "/client-policies/policies", key: keyPolicies}
)

// mcpRealm is one pass against one realm. Each step records what later steps need.
type mcpRealm struct {
	registrar *HTTPOIDCRegistrar
	baseURL   string // Keycloak's base URL without a trailing slash
	realm     string
	root      string // the realm's admin REST root
	token     string
	spec      MCPRealmSpec

	scopeIDs     map[string]string // client scope IDs by name
	mcpUUID      string            // ilm-mcp's internal ID
	blockedDrift []string          // ilm-mcp fields left uncorrected because the client carries secret-rotation state
	authzRoot    string            // the admin-permissions resource server, relative to root
}

// EnsureMCPRealm implements OIDCRegistrar against a live Keycloak. It authenticates once and keeps, in this order, the
// client scopes mcp:access, ilm-mcp-upstream and delegation:client; the client ilm-mcp and its optional scope
// ilm-mcp-upstream; fine-grained admin permissions v2; the realm default optional scopes; the client profiles and
// policies; and the delegation client policy and permission. Then it reads ilm-mcp's secret. Each object is looked up
// by name, created when it is missing, and corrected with a merged PUT only when a field the operator owns differs.
//
// OIDCRegistrar.EnsureMCPRealm states what it removes and how it treats an ilm-mcp client that carries secret-rotation
// state. Any other failure ends the pass with a *Error carrying the step and at most the HTTP status, never a response
// body; the next pass resumes from the objects already in place.
func (h *HTTPOIDCRegistrar) EnsureMCPRealm(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string, spec MCPRealmSpec) (string, error) {
	if spec.PublicHost == "" {
		return "", &Error{Message: "MCP needs the platform's public host", Retryable: false}
	}
	m, err := h.openMCPRealm(ctx, keycloakBaseURL, realm, adminUsername, adminPassword)
	if err != nil {
		return "", err
	}
	m.spec = spec
	return m.ensure(ctx)
}

// RemoveMCPCIMD implements OIDCRegistrar against a live Keycloak: it authenticates once and runs removeMCPCIMD. Errors
// follow EnsureMCPRealm's rules.
func (h *HTTPOIDCRegistrar) RemoveMCPCIMD(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string) error {
	m, err := h.openMCPRealm(ctx, keycloakBaseURL, realm, adminUsername, adminPassword)
	if err != nil {
		return err
	}
	return m.removeMCPCIMD(ctx)
}

// openMCPRealm authenticates once and returns a pass against the realm.
func (h *HTTPOIDCRegistrar) openMCPRealm(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string) (*mcpRealm, error) {
	token, err := h.adminToken(ctx, keycloakBaseURL, adminUsername, adminPassword)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(keycloakBaseURL, "/")
	return &mcpRealm{
		registrar: h, baseURL: baseURL, realm: realm, token: token, scopeIDs: map[string]string{},
		root: baseURL + fmt.Sprintf(keycloakAdminRealmPathTmpl, url.PathEscape(realm)),
	}, nil
}

// ensure runs the steps in order and reads ilm-mcp's secret.
func (m *mcpRealm) ensure(ctx context.Context) (string, error) {
	steps := []func(context.Context) error{
		m.ensureClientScopes,
		m.ensureMCPClient,
		m.ensureMCPOptionalScope,
		// Enabling FGAP creates the admin-permissions client with the realm default optional scopes of that moment,
		// so it comes before mcp:access and delegation:client become realm defaults.
		m.enableAdminPermissions,
		m.ensureRealmOptionalScopes,
		// Once Keycloak has lost its cimd feature it refuses every list write that still holds mcp-cimd, and accepts
		// the lists without it, so with CIMD off the removal comes before any other list write.
		m.removeUnconfiguredCIMD,
		// Keycloak refuses a policy that names a missing profile, so the profiles come first.
		m.ensureClientProfiles,
		m.ensureClientPolicies,
		m.ensureDelegatingClientPolicy,
		// Removing the other form's permission first fails closed: should the configured permission's create then
		// fail, no user is delegable, rather than every user while a group is configured.
		m.removeOtherDelegationPermission,
		m.ensureDelegationPermission,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return "", err
		}
	}
	secret, err := m.registrar.clientSecret(ctx, m.baseURL, m.realm, m.mcpUUID, m.token)
	if err != nil {
		return "", err
	}
	if len(m.blockedDrift) > 0 {
		return secret, &Error{
			Message: fmt.Sprintf("client %s carries secret-rotation state, so these fields were left uncorrected: %s",
				mcpClientID, strings.Join(m.blockedDrift, ", ")),
			Retryable: false,
		}
	}
	return secret, nil
}

// ensureClientScopes keeps mcp:access, ilm-mcp-upstream and delegation:client, and records their IDs. One read of the
// realm's client scopes, which carries their mappers, serves all three.
func (m *mcpRealm) ensureClientScopes(ctx context.Context) error {
	ilmMappers, err := m.ilmMapperCopies(ctx)
	if err != nil {
		return err
	}
	var existing []object
	if _, err := m.call(ctx, http.MethodGet, "/client-scopes", nil, nil, &existing, "reading the client scopes"); err != nil {
		return err
	}
	desired := []desiredScope{mcpAccessScope(mcpResource(m.spec.PublicHost)), mcpUpstreamScope(ilmMappers), delegationScope()}
	for _, scope := range desired {
		id, err := m.ensureClientScope(ctx, scope, findByName(existing, scope.name))
		if err != nil {
			return err
		}
		m.scopeIDs[scope.name] = id
	}
	return nil
}

// ilmMapperCopies returns the ilm client's Username and Groups mappers without their IDs.
func (m *mcpRealm) ilmMapperCopies(ctx context.Context) ([]object, error) {
	ilm, err := m.findClient(ctx, ilmClientID)
	if err != nil {
		return nil, err
	}
	if ilm == nil {
		// The realm import creates the ilm client; until then there is nothing to copy.
		return nil, &Error{Message: "client ilm is not in the realm yet", Retryable: true}
	}
	var mappers []object
	path := clientPath(stringField(ilm, keyID)) + "/protocol-mappers/models"
	if _, err := m.call(ctx, http.MethodGet, path, nil, nil, &mappers, "reading the mappers of client ilm"); err != nil {
		return nil, err
	}
	copies := make([]object, 0, len(copiedILMMappers))
	for _, name := range copiedILMMappers {
		mapper := findByName(mappers, name)
		if mapper == nil {
			return nil, &Error{
				Message:   fmt.Sprintf("client ilm has no %s mapper for %s to copy", name, mcpUpstreamScopeName),
				Retryable: false,
			}
		}
		copies = append(copies, withoutKeys(mapper, keyID))
	}
	return copies, nil
}

// ensureClientScope creates a missing scope together with its mappers. For a stored scope it corrects the owned
// fields and the owned mappers, each with a merged PUT only when it differs, and adds the mappers the scope lacks.
// Mappers the operator does not own stay.
func (m *mcpRealm) ensureClientScope(ctx context.Context, desired desiredScope, current object) (string, error) {
	if current == nil {
		location, err := m.call(ctx, http.MethodPost, "/client-scopes", nil, desired.representation(), nil,
			"creating client scope "+desired.name)
		if err != nil {
			return "", err
		}
		return createdID(location, "client scope "+desired.name)
	}
	id := stringField(current, keyID)
	scopePath := "/client-scopes/" + url.PathEscape(id)
	if !covers(current, desired.owned) {
		// Keycloak applies only the fields and attributes present, and keeps the mappers, which the body leaves out.
		body := mergeOwned(withoutKeys(current, keyProtocolMappers), desired.owned)
		if _, err := m.call(ctx, http.MethodPut, scopePath, nil, body, nil, "correcting client scope "+desired.name); err != nil {
			return "", err
		}
	}
	stored := objectsIn(current[keyProtocolMappers])
	for _, mapper := range desired.mappers {
		if err := m.ensureScopeMapper(ctx, scopePath, desired.name, mapper, findByName(stored, stringField(mapper, keyName))); err != nil {
			return "", err
		}
	}
	return id, nil
}

// ensureScopeMapper adds a missing mapper to a scope, or corrects a stored one whose type or owned settings differ.
// Keycloak adds settings of its own to stored mappers, so only the settings the operator sets are compared.
func (m *mcpRealm) ensureScopeMapper(ctx context.Context, scopePath, scope string, desired, current object) error {
	name := stringField(desired, keyName)
	if current == nil {
		_, err := m.call(ctx, http.MethodPost, scopePath+"/protocol-mappers/models", nil, desired, nil,
			fmt.Sprintf("adding mapper %s to client scope %s", name, scope))
		return err
	}
	owned := object{keyProtocolMapper: desired[keyProtocolMapper], keyConfig: desired[keyConfig]}
	if covers(current, owned) {
		return nil
	}
	_, err := m.call(ctx, http.MethodPut, scopePath+"/protocol-mappers/models/"+url.PathEscape(stringField(current, keyID)),
		nil, mergeOwned(current, owned), nil, fmt.Sprintf("correcting mapper %s of client scope %s", name, scope))
	return err
}

// ensureMCPClient creates ilm-mcp, whose secret and service account Keycloak generates, or corrects its owned fields.
// The correction sends the stored representation without its secret, every other attribute included, and is never
// sent while the client carries secret-rotation state.
func (m *mcpRealm) ensureMCPClient(ctx context.Context) error {
	current, err := m.findClient(ctx, mcpClientID)
	if err != nil {
		return err
	}
	desired := mcpClient()
	if current == nil {
		location, err := m.call(ctx, http.MethodPost, "/clients", nil, desired, nil, "creating client "+mcpClientID)
		if err != nil {
			return err
		}
		m.mcpUUID, err = createdID(location, "client "+mcpClientID)
		return err
	}
	m.mcpUUID = stringField(current, keyID)
	owned := withoutKeys(desired, keyClientID)
	drift := differingFields(current, owned)
	switch {
	case len(drift) == 0:
		return nil
	case hasSecretRotationState(current):
		// The update would clear the rotation state and revoke the previous secret, so the drift is reported instead.
		// Only the drifted fields' names leave this step, never an attribute value.
		m.blockedDrift = drift
		return nil
	}
	_, err = m.call(ctx, http.MethodPut, clientPath(m.mcpUUID), nil, mergeOwned(current, owned), nil,
		"correcting client "+mcpClientID)
	return err
}

// findClient returns the client with exactly this clientId, or nil when the realm has none. Keycloak includes the
// client's secret in the representation; the pass never needs it from there, so it is dropped at once.
func (m *mcpRealm) findClient(ctx context.Context, clientID string) (object, error) {
	var clients []object
	if _, err := m.call(ctx, http.MethodGet, "/clients", url.Values{keyClientID: {clientID}}, nil, &clients,
		"looking up client "+clientID); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(clients, func(c object) bool { return stringField(c, keyClientID) == clientID })
	if i < 0 {
		return nil, nil
	}
	delete(clients[i], "secret")
	return clients[i], nil
}

// ensureMCPOptionalScope makes ilm-mcp-upstream an optional scope of ilm-mcp, which requests it in the exchange.
func (m *mcpRealm) ensureMCPOptionalScope(ctx context.Context) error {
	return m.ensureAssigned(ctx, clientPath(m.mcpUUID)+"/optional-client-scopes",
		"optional scopes of client "+mcpClientID, mcpUpstreamScopeName)
}

// enableAdminPermissions switches fine-grained admin permissions v2 on with a partial realm update, which changes only
// that flag and the admin-permissions client Keycloak creates for it.
func (m *mcpRealm) enableAdminPermissions(ctx context.Context) error {
	var realm struct {
		AdminPermissionsEnabled bool `json:"adminPermissionsEnabled"`
	}
	if _, err := m.call(ctx, http.MethodGet, "", nil, nil, &realm, "reading the realm"); err != nil {
		return err
	}
	if realm.AdminPermissionsEnabled {
		return nil
	}
	_, err := m.call(ctx, http.MethodPut, "", nil, object{"adminPermissionsEnabled": true}, nil,
		"enabling fine-grained admin permissions")
	return err
}

// ensureRealmOptionalScopes makes mcp:access and delegation:client realm default optional scopes, so the clients
// Keycloak creates from metadata documents may request them.
func (m *mcpRealm) ensureRealmOptionalScopes(ctx context.Context) error {
	return m.ensureAssigned(ctx, "/default-optional-client-scopes", "realm default optional scopes",
		mcpAccessScopeName, delegationScopeName)
}

// ensureAssigned adds each named client scope that the scope list at path lacks.
func (m *mcpRealm) ensureAssigned(ctx context.Context, path, list string, names ...string) error {
	var assigned []object
	if _, err := m.call(ctx, http.MethodGet, path, nil, nil, &assigned, "reading the "+list); err != nil {
		return err
	}
	for _, name := range names {
		if findByName(assigned, name) != nil {
			continue
		}
		step := fmt.Sprintf("adding %s to the %s", name, list)
		if _, err := m.call(ctx, http.MethodPut, path+"/"+url.PathEscape(m.scopeIDs[name]), nil, nil, nil, step); err != nil {
			return err
		}
	}
	return nil
}

// ensureClientProfiles upserts MCP's client profiles.
func (m *mcpRealm) ensureClientProfiles(ctx context.Context) error {
	return m.ensureNamedEntries(ctx, clientProfilesList, mcpClientProfiles(mcpResource(m.spec.PublicHost), m.spec.CIMD))
}

// ensureClientPolicies upserts MCP's client policies.
func (m *mcpRealm) ensureClientPolicies(ctx context.Context) error {
	return m.ensureNamedEntries(ctx, clientPoliciesList, mcpClientPolicies(m.spec.CIMD))
}

// ensureNamedEntries upserts the desired entries by name into one of the realm's client policy lists. Keycloak
// replaces the whole list on a write, so the pass writes it back, other entries unchanged, and only when an entry was
// added or corrected.
func (m *mcpRealm) ensureNamedEntries(ctx context.Context, list clientPolicyList, desired []object) error {
	entries, err := m.readList(ctx, list)
	if err != nil {
		return err
	}
	changed := false
	for _, want := range desired {
		switch i := indexByName(entries, stringField(want, keyName)); {
		case i < 0:
			entries = append(entries, want)
		case !covers(entries[i], want):
			stored, _ := entries[i].(object)
			entries[i] = mergeOwned(stored, want)
		default:
			continue
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return m.writeList(ctx, list, entries)
}

// removeUnconfiguredCIMD removes mcp-cimd when CIMD is off.
func (m *mcpRealm) removeUnconfiguredCIMD(ctx context.Context) error {
	if m.spec.CIMD {
		return nil
	}
	return m.removeMCPCIMD(ctx)
}

// removeMCPCIMD writes the realm's client policies and then its client profiles without mcp-cimd, each only when it
// holds mcp-cimd, and leaves every other entry as stored. A stored mcp-cimd makes Keycloak fail every sign-in, token
// request and client update in the realm once its cimd feature is gone, so it has to go while the feature is on.
// Keycloak refuses a policy that names a missing profile, so the policies go first.
func (m *mcpRealm) removeMCPCIMD(ctx context.Context) error {
	for _, list := range []clientPolicyList{clientPoliciesList, clientProfilesList} {
		entries, err := m.readList(ctx, list)
		if err != nil {
			return err
		}
		i := indexByName(entries, mcpCIMDName)
		if i < 0 {
			continue
		}
		if err := m.writeList(ctx, list, slices.Delete(entries, i, i+1)); err != nil {
			return err
		}
	}
	return nil
}

// readList returns the entries of one of the realm's client policy lists. Keycloak answers with the list, empty or not,
// and a write replaces it as a whole. A document that holds no list is therefore an error: taking it for an empty one
// would write back only MCP's entries and delete every other entry of the realm.
func (m *mcpRealm) readList(ctx context.Context, list clientPolicyList) ([]any, error) {
	step := "reading the realm's client " + list.key
	var document object
	if _, err := m.call(ctx, http.MethodGet, list.path, nil, nil, &document, step); err != nil {
		return nil, err
	}
	entries, ok := document[list.key].([]any)
	if !ok {
		return nil, &Error{Message: step + ": Keycloak's response holds no list", Retryable: true}
	}
	return entries, nil
}

// writeList replaces one of the realm's client policy lists with entries.
func (m *mcpRealm) writeList(ctx context.Context, list clientPolicyList, entries []any) error {
	_, err := m.call(ctx, http.MethodPut, list.path, nil, object{list.key: entries}, nil, "writing the realm's client "+list.key)
	return err
}

// ensureDelegatingClientPolicy keeps the client policy that matches ilm-mcp, in the resource server of the
// admin-permissions client.
func (m *mcpRealm) ensureDelegatingClientPolicy(ctx context.Context) error {
	adminPermissions, err := m.findClient(ctx, adminPermissionsClientID)
	if err != nil {
		return err
	}
	if adminPermissions == nil {
		return &Error{Message: "client admin-permissions is not in the realm yet", Retryable: true}
	}
	m.authzRoot = clientPath(stringField(adminPermissions, keyID)) + "/authz/resource-server"
	return m.ensureAuthz(ctx, clientPolicyKind, delegatingClientPolicy(m.mcpUUID))
}

// removeOtherDelegationPermission deletes the operator's own delegation permission of the form not configured: a
// group narrows delegation only once the all-users permission is gone. It is the only DELETE the registrar sends.
func (m *mcpRealm) removeOtherDelegationPermission(ctx context.Context) error {
	other := mcpGroupPermissionName
	if m.spec.DelegationGroup != "" {
		other = mcpAllUsersPermissionName
	}
	stored, err := m.searchAuthz(ctx, permissionKind, other)
	if err != nil || stored == nil {
		return err
	}
	_, err = m.call(ctx, http.MethodDelete, m.authzRoot+"/policy/"+url.PathEscape(stringField(stored, keyID)), nil, nil, nil,
		"deleting permission "+other)
	return err
}

// ensureDelegationPermission keeps the permission that lets ilm-mcp delegate, for every user or for the members of the
// configured group.
func (m *mcpRealm) ensureDelegationPermission(ctx context.Context) error {
	groupID := ""
	if m.spec.DelegationGroup != "" {
		id, err := m.ensureGroup(ctx)
		if err != nil {
			return err
		}
		groupID = id
	}
	return m.ensureAuthz(ctx, permissionKind, delegationPermission(groupID))
}

// ensureGroup returns the ID of the configured top-level group, which it creates when it is missing. Group membership
// is left to the realm's administrators.
func (m *mcpRealm) ensureGroup(ctx context.Context) (string, error) {
	name := m.spec.DelegationGroup
	var groups []object
	query := url.Values{"search": {name}, "exact": {kcTrue}}
	if _, err := m.call(ctx, http.MethodGet, "/groups", query, nil, &groups, "looking up group "+name); err != nil {
		return "", err
	}
	for _, group := range groups {
		// The search also returns the parents of nested groups with that name; only the top-level group counts.
		if stringField(group, keyName) == name && stringField(group, "path") == "/"+name {
			return stringField(group, keyID), nil
		}
	}
	location, err := m.call(ctx, http.MethodPost, "/groups", nil, object{keyName: name}, nil, "creating group "+name)
	if err != nil {
		return "", err
	}
	return createdID(location, "group "+name)
}

// ensureAuthz keeps one client policy or permission of the admin-permissions resource server: it creates a missing
// one, and corrects a stored one with a merged PUT when an owned field differs.
func (m *mcpRealm) ensureAuthz(ctx context.Context, kind authzKind, desired object) error {
	name := stringField(desired, keyName)
	found, err := m.searchAuthz(ctx, kind, name)
	if err != nil {
		return err
	}
	if found == nil {
		_, err := m.call(ctx, http.MethodPost, m.authzRoot+kind.collection, nil, desired, nil, "creating "+kind.label+" "+name)
		return err
	}
	id := stringField(found, keyID)
	typedPath := m.authzRoot + kind.collection + "/" + url.PathEscape(id)
	var typed object
	if _, err := m.call(ctx, http.MethodGet, typedPath, nil, nil, &typed, "reading "+kind.label+" "+name); err != nil {
		return err
	}
	stored, wanted, update := object{}, desired, desired
	maps.Copy(stored, typed)
	if kind == permissionKind {
		// Only the permission's resources show it narrowed to some users; its typed form stays the same.
		if err := m.readPermissionParts(ctx, id, name, stored); err != nil {
			return err
		}
		wanted = maps.Clone(desired)
		wanted[keyResources] = storedPermissionResources(desired)
		update = delegationPermissionUpdate(desired)
	}
	if covers(stored, wanted) {
		return nil
	}
	_, err = m.call(ctx, http.MethodPut, typedPath, nil, mergeOwned(typed, update), nil, "correcting "+kind.label+" "+name)
	return err
}

// readPermissionParts fills stored with the names of a permission's resources, scopes and policies.
func (m *mcpRealm) readPermissionParts(ctx context.Context, id, name string, stored object) error {
	for _, part := range permissionParts {
		var items []object
		path := m.authzRoot + "/policy/" + url.PathEscape(id) + "/" + part.path
		step := fmt.Sprintf("reading the %s of permission %s", part.field, name)
		if _, err := m.call(ctx, http.MethodGet, path, nil, nil, &items, step); err != nil {
			return err
		}
		stored[part.field] = objectNames(items)
	}
	return nil
}

// searchAuthz finds the client policy or permission with this name, or returns nil: Keycloak answers a miss with 204
// and no body.
func (m *mcpRealm) searchAuthz(ctx context.Context, kind authzKind, name string) (object, error) {
	var found object
	if _, err := m.call(ctx, http.MethodGet, m.authzRoot+kind.search, url.Values{keyName: {name}}, nil, &found,
		"looking up "+kind.label+" "+name); err != nil {
		return nil, err
	}
	if stringField(found, keyID) == "" {
		return nil, nil
	}
	return found, nil
}

// call sends one admin request below the realm's admin root and returns the response's Location header. It sends
// body as JSON when it is non-nil, and decodes a 2xx response with content into out when out is non-nil. Any other
// status becomes a *Error naming the step and the status; the response body is drained, never read into the error.
func (m *mcpRealm) call(ctx context.Context, method, path string, query url.Values, body, out any, step string) (string, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", &Error{Message: step + ": the request could not be encoded", Retryable: false}
		}
		payload = bytes.NewReader(encoded)
	}
	endpoint := m.root + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return "", &Error{Message: step + ": the request could not be created", Retryable: false}
	}
	req.Header.Set("Authorization", bearerPrefix+m.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.registrar.client().Do(req)
	if err != nil {
		// Never surface err.Error(): it carries Keycloak's address.
		return "", &Error{Message: step + ": " + transportErrContactingKeycloak, Retryable: true}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("%s failed with status %d", step, resp.StatusCode),
			Retryable:  retryableStatus(resp.StatusCode),
		}
	}
	if out != nil {
		decoder := json.NewDecoder(resp.Body)
		decoder.UseNumber()
		// An empty body, as in a 204, leaves out as it is.
		if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return "", &Error{StatusCode: resp.StatusCode, Message: step + ": Keycloak's response could not be decoded", Retryable: true}
		}
	}
	return resp.Header.Get("Location"), nil
}

// clientPath is the path of the client with this internal ID, relative to the realm's admin root.
func clientPath(id string) string {
	return "/clients/" + url.PathEscape(id)
}

// createdID returns the ID that ends the Location header of a create.
func createdID(location, what string) (string, error) {
	if u, err := url.Parse(location); err == nil {
		if id := u.Path[strings.LastIndex(u.Path, "/")+1:]; id != "" {
			return id, nil
		}
	}
	// The object exists now, so the next pass finds it by name.
	return "", &Error{Message: "Keycloak returned no ID for the new " + what, Retryable: true}
}

// hasSecretRotationState reports whether a client carries any secret-rotation attribute with a value. It reads only
// whether each is set.
func hasSecretRotationState(client object) bool {
	attributes, _ := client[keyAttributes].(object)
	for _, name := range secretRotationAttributes {
		if value, ok := attributes[name]; ok && value != nil && value != "" {
			return true
		}
	}
	return false
}

// covers reports whether actual holds every value wanted sets. An object covers another when it holds each of its
// keys with a covering value, whatever else it holds. A list covers another of the same length when each wanted
// element is covered by a stored element of its own, in any order, as Keycloak returns lists in no fixed order. Other
// values compare as text, so a stored "true" covers true.
func covers(actual, wanted any) bool {
	switch want := wanted.(type) {
	case object:
		have, ok := actual.(object)
		if !ok {
			return false
		}
		for key, value := range want {
			stored, present := have[key]
			if !present || !covers(stored, value) {
				return false
			}
		}
		return true
	case []any:
		have, ok := actual.([]any)
		return ok && coversList(have, want)
	default:
		return fmt.Sprint(actual) == fmt.Sprint(wanted)
	}
}

// coversList pairs each wanted element with a stored element of its own that covers it.
func coversList(have, want []any) bool {
	if len(have) != len(want) {
		return false
	}
	used := make([]bool, len(have))
	for _, w := range want {
		matched := false
		for i, h := range have {
			if !used[i] && covers(h, w) {
				used[i], matched = true, true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// differingFields names the fields of wanted that actual does not cover, sorted. The fields of a nested object are
// named after it, as in attributes.<name>.
func differingFields(actual, wanted object) []string {
	var fields []string
	for key, want := range wanted {
		if nested, ok := want.(object); ok {
			stored, _ := actual[key].(object)
			for _, field := range differingFields(stored, nested) {
				fields = append(fields, key+"."+field)
			}
			continue
		}
		if stored, present := actual[key]; !present || !covers(stored, want) {
			fields = append(fields, key)
		}
	}
	slices.Sort(fields)
	return fields
}

// mergeOwned returns a copy of current with every owned field set. Nested objects merge key by key, so attributes and
// mapper settings the operator does not own keep their stored values; any other owned value replaces the stored one.
func mergeOwned(current, owned object) object {
	merged := object{}
	maps.Copy(merged, current)
	for key, value := range owned {
		nested, isObject := value.(object)
		stored, storedIsObject := merged[key].(object)
		if isObject && storedIsObject {
			merged[key] = mergeOwned(stored, nested)
			continue
		}
		merged[key] = value
	}
	return merged
}

// withoutKeys returns a shallow copy of o without the given keys.
func withoutKeys(o object, keys ...string) object {
	copied := maps.Clone(o)
	for _, key := range keys {
		delete(copied, key)
	}
	return copied
}

// stringField returns the string at key, or "" when it is absent or not a string.
func stringField(o object, key string) string {
	s, _ := o[key].(string)
	return s
}

// findByName returns the first object named name, or nil.
func findByName(list []object, name string) object {
	i := slices.IndexFunc(list, func(o object) bool { return stringField(o, keyName) == name })
	if i < 0 {
		return nil
	}
	return list[i]
}

// indexByName returns the index of the first entry named name, or -1.
func indexByName(entries []any, name string) int {
	return slices.IndexFunc(entries, func(e any) bool {
		o, ok := e.(object)
		return ok && stringField(o, keyName) == name
	})
}

// objectsIn returns the objects of a decoded JSON list, skipping anything else.
func objectsIn(v any) []object {
	list, _ := v.([]any)
	objects := make([]object, 0, len(list))
	for _, item := range list {
		if o, ok := item.(object); ok {
			objects = append(objects, o)
		}
	}
	return objects
}

// objectNames returns the names of the objects in list.
func objectNames(list []object) []any {
	names := make([]any, 0, len(list))
	for _, o := range list {
		names = append(names, stringField(o, keyName))
	}
	return names
}
