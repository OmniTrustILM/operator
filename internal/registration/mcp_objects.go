/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package registration

import "maps"

// object is a Keycloak representation in its JSON form. The registrar builds the representations it wants and reads
// the ones Keycloak stores in this one shape, so one comparison and one merge serve every kind of object. Lists inside
// an object are []any, as JSON decoding produces them.
type object = map[string]any

// Names of the realm objects the operator keeps for MCP. The operator owns these names; OIDCRegistrar.EnsureMCPRealm
// states what it creates, corrects and removes.
const (
	mcpClientID               = "ilm-mcp"
	mcpAccessScopeName        = "mcp:access"
	mcpUpstreamScopeName      = "ilm-mcp-upstream"
	delegationScopeName       = "delegation:client"
	mcpPKCEName               = "mcp-pkce"
	mcpCIMDName               = "mcp-cimd"
	mcpClientPolicyName       = "ilm-mcp-delegating-client"
	mcpAllUsersPermissionName = "ilm-mcp-all-users-delegation"
	mcpGroupPermissionName    = "ilm-mcp-selected-users-delegation"
)

// ilmClientID is the client Core signs in with. ilm-mcp-upstream copies its Username and Groups mappers, and the
// exchanged token carries it as audience. adminPermissionsClientID is the client Keycloak creates for fine-grained
// admin permissions v2; its resource server holds the delegation policy and permissions.
const (
	ilmClientID              = "ilm"
	adminPermissionsClientID = "admin-permissions"
)

// Keys of Keycloak representations.
const (
	keyID               = "id"
	keyName             = "name"
	keyClientID         = "clientId"
	keyDescription      = "description"
	keyProtocol         = "protocol"
	keyAttributes       = "attributes"
	keyProtocolMappers  = "protocolMappers"
	keyProtocolMapper   = "protocolMapper"
	keyConsentRequired  = "consentRequired"
	keyConfig           = "config"
	keyEnabled          = "enabled"
	keyExecutors        = "executors"
	keyConditions       = "conditions"
	keyConfiguration    = "configuration"
	keyProfiles         = "profiles"
	keyType             = "type"
	keyLogic            = "logic"
	keyDecisionStrategy = "decisionStrategy"
	keyResourceType     = "resourceType"
	keyResources        = "resources"
	keyScopes           = "scopes"
	keyPolicies         = "policies"
)

// Values of Keycloak representations. Keycloak keeps mapper settings and scope attributes as the strings "true" and
// "false".
const (
	openIDConnectProtocol   = "openid-connect"
	kcTrue                  = "true"
	kcFalse                 = "false"
	accessTokenClaim        = "access.token.claim"
	idTokenClaim            = "id.token.claim"
	introspectionTokenClaim = "introspection.token.claim"
	includeInTokenScope     = "include.in.token.scope"
	displayOnConsentScreen  = "display.on.consent.screen"
	includedClientAudience  = "included.client.audience"
	includedCustomAudience  = "included.custom.audience"
	mcpPublicResourceMapper = "mcp-public-resource"
	claudeDomain            = "claude.ai"
	cimdExecutor            = "client-id-metadata-document"
	clientIDURICondition    = "client-id-uri"
	clientPolicyType        = "client"
	scopePermissionType     = "scope"
	positiveLogic           = "POSITIVE"
	unanimousStrategy       = "UNANIMOUS"
	usersResourceType       = "Users"
	groupsResourceType      = "Groups"
)

// Names of the ilm client's mappers for the username and roles claims.
const (
	ilmUsernameMapper = "Username"
	ilmGroupsMapper   = "Groups"
)

// copiedILMMappers are the ilm client's mappers that ilm-mcp-upstream copies, in the order it lists them.
var copiedILMMappers = []string{ilmUsernameMapper, ilmGroupsMapper}

// desiredScope is a client scope as the registrar keeps it: the top-level fields it owns, and the mappers it owns,
// matched by name.
type desiredScope struct {
	name    string
	owned   object
	mappers []object
}

// representation is the body that creates the scope together with its mappers.
func (d desiredScope) representation() object {
	rep := object{keyName: d.name, keyProtocolMappers: d.mappers}
	maps.Copy(rep, d.owned)
	return rep
}

// mcpResource is MCP's public resource URL on the platform's public host.
func mcpResource(publicHost string) string {
	return "https://" + publicHost + "/mcp"
}

// audienceMapper adds one audience to access tokens and to introspection, not to ID tokens.
func audienceMapper(name, audienceKey, audience string) object {
	return object{
		keyName: name, keyProtocol: openIDConnectProtocol, keyProtocolMapper: "oidc-audience-mapper", keyConsentRequired: false,
		keyConfig: object{audienceKey: audience, accessTokenClaim: kcTrue, idTokenClaim: kcFalse, introspectionTokenClaim: kcTrue},
	}
}

// scopeAttributes are the attributes of MCP's own scopes: listed in the token's scope claim and shown on the consent
// screen.
func scopeAttributes() object {
	return object{includeInTokenScope: kcTrue, displayOnConsentScreen: kcTrue}
}

// mcpAccessScope is the scope MCP clients request at sign-in. Its two audiences let MCP accept the token for its
// public resource and let ilm-mcp present it to Keycloak as the subject token of the exchange.
func mcpAccessScope(resource string) desiredScope {
	return desiredScope{
		name:  mcpAccessScopeName,
		owned: object{keyProtocol: openIDConnectProtocol, keyAttributes: scopeAttributes()},
		mappers: []object{
			audienceMapper(mcpPublicResourceMapper, includedCustomAudience, resource),
			audienceMapper("mcp-exchange-requester", includedClientAudience, mcpClientID),
		},
	}
}

// mcpUpstreamScope is the scope ilm-mcp requests in the exchange. It gives the exchanged token the ilm audience and the
// user's username and roles claims, through copies of the ilm client's own mappers, so ILM reads the same claims from
// an exchanged token as from its own.
func mcpUpstreamScope(ilmMappers []object) desiredScope {
	return desiredScope{
		name:    mcpUpstreamScopeName,
		owned:   object{keyProtocol: openIDConnectProtocol, keyAttributes: scopeAttributes()},
		mappers: append([]object{audienceMapper("ilm-api-audience", includedClientAudience, ilmClientID)}, ilmMappers...),
	}
}

// delegationScope is Keycloak's own parameterized delegation:client scope, from which a client requests
// delegation:client:ilm-mcp and the user's token gets the may_act claim the exchange checks. Keycloak adds it only to
// realms created after the token-exchange-delegation and parameterized-scopes features were switched on, so the
// operator creates it, as Keycloak represents it, in older realms.
func delegationScope() desiredScope {
	mayAct := func(name, mapperType, claim string) object {
		return object{
			keyName: name, keyProtocol: openIDConnectProtocol, keyProtocolMapper: mapperType, keyConsentRequired: false,
			keyConfig: object{idTokenClaim: kcTrue, introspectionTokenClaim: kcTrue, accessTokenClaim: kcTrue,
				"claim.name": claim, "jsonType.label": "String"},
		}
	}
	return desiredScope{
		name: delegationScopeName,
		owned: object{
			keyDescription: "OpenID Connect scope for agent/client token exchange delegation",
			keyProtocol:    openIDConnectProtocol,
			keyAttributes: object{
				includeInTokenScope: kcTrue, "parameterized.scope.type": "client-delegation",
				displayOnConsentScreen: kcTrue, "always.display.consent": kcTrue, "is.parameterized.scope": kcTrue,
				"consent.screen.text": "${clientDelegationScopeConsentText}",
			},
		},
		mappers: []object{
			mayAct("client may_act client_id", "oidc-parameterized-scope-mapper", "may_act.client_id"),
			mayAct("client may_act sub", "oidc-parameterized-scope-client-sub-mapper", "may_act.sub"),
			{
				keyName: "client delegation audience", keyProtocol: openIDConnectProtocol,
				keyProtocolMapper: "oidc-parameterized-scope-audience-mapper", keyConsentRequired: false,
				keyConfig: object{introspectionTokenClaim: kcTrue, accessTokenClaim: kcTrue},
			},
		},
	}
}

// mcpClient is ilm-mcp, the confidential client MCP exchanges tokens as. Its service account exists only to obtain
// MCP's actor token, and the registrar assigns it no roles. Browser, implicit and direct-access flows are off, Full
// Scope Allowed is off so its tokens carry only what its scopes map, and the exchange may not issue refresh tokens.
func mcpClient() object {
	return object{
		keyClientID: mcpClientID, keyProtocol: openIDConnectProtocol, keyEnabled: true, "publicClient": false,
		"bearerOnly": false, "clientAuthenticatorType": "client-secret", "standardFlowEnabled": false,
		"implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "serviceAccountsEnabled": true,
		"fullScopeAllowed": false, keyConsentRequired: false,
		keyAttributes: object{
			"standard.token.exchange.enabled":                         kcTrue,
			"standard.token.exchange.enableRefreshRequestedTokenType": "NO",
		},
	}
}

// mcpClientProfiles are MCP's client profiles. mcp-pkce makes Keycloak require PKCE. mcp-cimd lets clients sign in
// with a client ID metadata document from the trusted domains, and needs Keycloak's cimd feature. Keycloak enforces
// its resource allow list only with the experimental resource-indicators feature, which the operator does not
// enable; tokens' audiences come from the mcp:access mappers either way.
func mcpClientProfiles(resource string, cimd bool) []object {
	profiles := []object{{
		keyName:      mcpPKCEName,
		keyExecutors: []any{object{"executor": "pkce-enforcer", keyConfiguration: object{"auto-configure": true}}},
	}}
	if cimd {
		profiles = append(profiles, object{
			keyName: mcpCIMDName,
			keyExecutors: []any{object{"executor": cimdExecutor, keyConfiguration: object{
				"cimd-allow-permitted-domains":                             []any{claudeDomain, "localhost", "127.0.0.1"},
				"accept-public-client-with-confidential-client-only-grant": true,
				"cimd-resource-indicator-allow-list":                       []any{resource},
			}}},
		})
	}
	return profiles
}

// mcpClientPolicies apply MCP's client profiles. The client-id-uri condition votes only while Keycloak resolves a
// client ID, so PKCE needs its own policy, whose client-scopes condition votes on authorization and token requests
// for mcp:access.
func mcpClientPolicies(cimd bool) []object {
	policies := []object{{
		keyName: mcpPKCEName, keyEnabled: true,
		keyConditions: []any{object{"condition": "client-scopes",
			keyConfiguration: object{keyScopes: []any{mcpAccessScopeName}, keyType: "Optional"}}},
		keyProfiles: []any{mcpPKCEName},
	}}
	if cimd {
		policies = append(policies, object{
			keyName: mcpCIMDName, keyEnabled: true,
			keyConditions: []any{object{"condition": clientIDURICondition, keyConfiguration: object{
				"client-id-uri-scheme":                  []any{"https"},
				"client-id-uri-allow-permitted-domains": []any{claudeDomain},
			}}},
			keyProfiles: []any{mcpCIMDName},
		})
	}
	return policies
}

// delegatingClientPolicy is the FGAP v2 client policy that matches ilm-mcp, by its internal ID.
func delegatingClientPolicy(mcpClientUUID string) object {
	return object{
		keyName: mcpClientPolicyName, keyType: clientPolicyType, keyLogic: positiveLogic, keyDecisionStrategy: unanimousStrategy,
		"clients": []any{mcpClientUUID},
	}
}

// delegationPermission is the FGAP v2 scope permission that lets ilm-mcp delegate: for every user when groupID is
// empty, otherwise for the members of the group with that ID. FGAP v2 refuses any decision strategy other than
// UNANIMOUS on a permission.
func delegationPermission(groupID string) object {
	if groupID == "" {
		// No resources: the permission covers every user.
		return object{
			keyName: mcpAllUsersPermissionName, keyType: scopePermissionType, keyLogic: positiveLogic,
			keyDecisionStrategy: unanimousStrategy, keyResourceType: usersResourceType,
			keyScopes: []any{"delegate"}, keyPolicies: []any{mcpClientPolicyName},
		}
	}
	return object{
		keyName: mcpGroupPermissionName, keyType: scopePermissionType, keyLogic: positiveLogic,
		keyDecisionStrategy: unanimousStrategy, keyResourceType: groupsResourceType, keyResources: []any{groupID},
		keyScopes: []any{"delegate-members"}, keyPolicies: []any{mcpClientPolicyName},
	}
}

// delegationPermissionUpdate is the body that corrects a stored delegation permission. Keycloak keeps a permission's
// resources when an update leaves the key out, so the all-users form names an empty list, which restores every user
// to a permission narrowed to some of them.
func delegationPermissionUpdate(permission object) object {
	if _, named := permission[keyResources]; named {
		return permission
	}
	update := maps.Clone(permission)
	update[keyResources] = []any{}
	return update
}

// storedPermissionResources is how Keycloak lists a delegation permission's resources: the group's ID, or, for the
// all-users permission, which names none, the one resource named after its resource type.
func storedPermissionResources(permission object) []any {
	if resources, ok := permission[keyResources].([]any); ok {
		return resources
	}
	return []any{permission[keyResourceType]}
}
