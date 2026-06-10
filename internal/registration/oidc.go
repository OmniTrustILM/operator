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

package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oidcConfigTimeout bounds a single Keycloak admin-API call.
const oidcConfigTimeout = 30 * time.Second

// keycloakTokenPath is Keycloak's master-realm token endpoint (password grant,
// client_id=admin-cli) used to obtain an admin access token:
// /realms/master/protocol/openid-connect/token.
const keycloakTokenPath = "/realms/master/protocol/openid-connect/token"

// keycloakAdminClientsPathTmpl / keycloakAdminClientSecretPathTmpl are the Keycloak admin
// REST paths used to (1) look up the platform OIDC client's internal id by clientId
// (GET /admin/realms/<realm>/clients?clientId=<id>) and (2) read that client's secret
// (GET /admin/realms/<realm>/clients/<id>/client-secret → {"value":...}).
const (
	keycloakAdminClientsPathTmpl      = "/admin/realms/%s/clients"
	keycloakAdminClientSecretPathTmpl = "/admin/realms/%s/clients/%s/client-secret" //nolint:gosec // G101: a URL path template, not a credential
)

// keycloakAdminUsersPathTmpl is the Keycloak admin REST path used to look up a realm user by
// exact username (GET /admin/realms/<realm>/users?username=<u>&exact=true) and to create one
// (POST /admin/realms/<realm>/users).
const keycloakAdminUsersPathTmpl = "/admin/realms/%s/users"

// superadminGroupAttribute is the user-attribute value the operator's default realm maps to
// the "roles" claim (via the oidc-usermodel-attribute-mapper on user.attribute "groups"), so a
// realm user carrying groups: ["superadmin"] surfaces as roles: ["superadmin"] and Core grants
// it superadmin. It is a non-secret realm convention.
const superadminGroupAttribute = "superadmin"

// transportErrContactingKeycloak is the generic, leak-free message used for any HTTP transport
// failure contacting Keycloak. It deliberately omits err.Error() (which can include the host).
const transportErrContactingKeycloak = "transport error contacting Keycloak"

// bearerPrefix is the Authorization header scheme prefix prepended to the admin access token.
const bearerPrefix = "Bearer "

// OIDCRegistrar fetches the platform OIDC client secret from Keycloak's admin API so the
// reconciler can relay it into an operator-owned Secret that Core reads IN-POD. It is
// injected into the Platform reconciler so tests can substitute a fake; the production
// implementation is HTTPOIDCRegistrar.
//
// Why this is fetch-only: Core's internal OIDC provider is configured by an IN-CORE-POD
// lifecycle.postStart hook (register-internal-keycloak.sh, rendered by the operator) that curls
// http://localhost:<coreport>/api/v1/settings/authentication/oauth2Providers/internal — Core's
// settings API is effectively localhost-only and a cross-pod PUT fails at transport. So the
// operator does NOT PUT Core itself; it only READS the Keycloak-GENERATED "ilm" client secret
// (no operator-minted credential), and the reconciler writes it into a Secret the in-pod
// postStart step reads via $INTERNAL_OAUTH_SECRET.
//
// SECURITY: implementations must NEVER log, return, or otherwise surface the admin
// credentials, the fetched client secret, the Keycloak access token, or any HTTP response
// body. Returned errors carry only a step name and at most an HTTP status code.
type OIDCRegistrar interface {
	// FetchClientSecret authenticates to Keycloak at keycloakBaseURL using the master-realm
	// admin credentials, resolves the clientID client's internal id in realm, and reads its
	// generated secret. It returns the secret VALUE on success, or a leak-free *Error the
	// caller should requeue on (carrying only a step name + HTTP status code). The returned
	// secret is relayed verbatim into the operator-owned OIDC client Secret — it is never
	// logged or placed in status/conditions/events.
	FetchClientSecret(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword, clientID string) (string, error)

	// EnsureRealmUser idempotently ensures the realm USER described by user exists in realm,
	// authenticating with the master-realm admin credentials. It is the password admin
	// method's terminal step: the created user carries the superadmin attribute
	// (groups: ["superadmin"]) and the supplied password (temporary=false). It is IDEMPOTENT
	// by exact username — if the user already exists it returns nil WITHOUT resetting the
	// password or re-creating the user, so a re-reconcile never clobbers a rotated password.
	// On any transient/unexpected failure it returns a leak-free *Error (a step name + at most
	// an HTTP status code). SECURITY: the password, admin creds, and access token are NEVER
	// logged, returned, or placed in errors.
	EnsureRealmUser(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string, user RealmUser) error
}

// RealmUser is the identity + password the operator ensures as a Keycloak realm user for the
// password admin method. SECURITY: Password is sensitive — it flows only into the create
// request body and is never logged or surfaced.
type RealmUser struct {
	// Username is the realm user's login name (the exact-match key used for idempotency).
	Username string
	// Email is the realm user's email (optional).
	Email string
	// FirstName is the realm user's first name (mapped from registerAdmin.name; optional).
	FirstName string
	// LastName is the realm user's surname (mapped from registerAdmin.lastName; optional). When
	// empty Keycloak prompts the user to complete their profile (add a last name) at first login.
	LastName string
	// Password is the realm user's password, read from the caller-provided Secret. It is set
	// as a non-temporary credential on create and NEVER logged or returned.
	Password string
}

// HTTPOIDCRegistrar is the production OIDCRegistrar: it talks to Keycloak's admin API over
// plain HTTP (in-cluster Service traffic). It holds no per-Platform state, so a single
// instance is shared across all reconciles.
type HTTPOIDCRegistrar struct {
	// httpClient is the HTTP client used for the Keycloak calls. When nil a default client
	// with oidcConfigTimeout is used.
	httpClient *http.Client
}

// NewHTTPOIDCRegistrar returns the production OIDCRegistrar with a bounded-timeout HTTP client.
func NewHTTPOIDCRegistrar() *HTTPOIDCRegistrar {
	return &HTTPOIDCRegistrar{httpClient: &http.Client{Timeout: oidcConfigTimeout}}
}

// keycloakTokenResponse / keycloakClientRepr / keycloakClientSecretResponse are the subsets
// of Keycloak's admin-API responses the registrar decodes. They carry no fields beyond what
// is needed (the token, the client id, and the client secret value).
type keycloakTokenResponse struct {
	AccessToken string `json:"access_token"`
}

type keycloakClientRepr struct {
	ID       string `json:"id"`
	ClientID string `json:"clientId"`
}

type keycloakClientSecretResponse struct {
	Value string `json:"value"`
}

// keycloakUserRepr is the subset of Keycloak's user representation the registrar decodes from
// the exact-username lookup — only the username, to confirm the match (the operator never
// reads back any other user field).
type keycloakUserRepr struct {
	Username string `json:"username"`
}

// keycloakCreateUserRequest / keycloakCredential are the on-the-wire body the registrar POSTs
// to create a realm user: the identity, enabled=true, the superadmin attribute, and a single
// non-temporary password credential. Field names/JSON tags match Keycloak's UserRepresentation.
type keycloakCreateUserRequest struct {
	Username    string               `json:"username"`
	Email       string               `json:"email,omitempty"`
	FirstName   string               `json:"firstName,omitempty"`
	LastName    string               `json:"lastName,omitempty"`
	Enabled     bool                 `json:"enabled"`
	Attributes  map[string][]string  `json:"attributes,omitempty"`
	Credentials []keycloakCredential `json:"credentials,omitempty"`
}

type keycloakCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// FetchClientSecret implements OIDCRegistrar against a live Keycloak. It runs the admin
// flow in order — authenticate, resolve the client's internal id, read its secret — and
// returns the secret value or a *Error carrying only a step name + status code on any
// failure (never the admin creds, the token, the fetched secret, or a response body).
func (h *HTTPOIDCRegistrar) FetchClientSecret(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword, clientID string) (string, error) {
	token, err := h.adminToken(ctx, keycloakBaseURL, adminUsername, adminPassword)
	if err != nil {
		return "", err
	}
	id, err := h.clientUUID(ctx, keycloakBaseURL, realm, clientID, token)
	if err != nil {
		return "", err
	}
	return h.clientSecret(ctx, keycloakBaseURL, realm, id, token)
}

// client returns the configured HTTP client or a default bounded-timeout one.
func (h *HTTPOIDCRegistrar) client() *http.Client {
	if h.httpClient != nil {
		return h.httpClient
	}
	return &http.Client{Timeout: oidcConfigTimeout}
}

// adminToken obtains an admin access token from Keycloak's master-realm token endpoint via
// the resource-owner password grant with the built-in admin-cli public client. The
// credentials go in the form body (never logged); only the access_token is read back.
func (h *HTTPOIDCRegistrar) adminToken(ctx context.Context, keycloakBaseURL, adminUsername, adminPassword string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", "admin-cli")
	form.Set("username", adminUsername)
	form.Set("password", adminPassword)

	endpoint := strings.TrimRight(keycloakBaseURL, "/") + keycloakTokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &Error{StatusCode: 0, Message: "creating Keycloak token request", Retryable: false}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client().Do(req)
	if err != nil {
		// Generic transport phrase — never surface err.Error() (it can include the host).
		return "", &Error{StatusCode: 0, Message: transportErrContactingKeycloak, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain (never inspect) the body — it may echo an error referencing the admin user.
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Keycloak admin authentication failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}
	var tr keycloakTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil || tr.AccessToken == "" {
		return "", &Error{StatusCode: resp.StatusCode, Message: "decoding Keycloak token response", Retryable: true}
	}
	return tr.AccessToken, nil
}

// clientUUID resolves the internal id of the OIDC client identified by clientID in realm, via
// GET /admin/realms/<realm>/clients?clientId=<clientID> with the bearer token. The response is
// an array of client representations; the operator matches the exact clientId. A not-found
// client is a transient state (the realm import may not have created it yet) → retryable.
func (h *HTTPOIDCRegistrar) clientUUID(ctx context.Context, keycloakBaseURL, realm, clientID, token string) (string, error) {
	endpoint := strings.TrimRight(keycloakBaseURL, "/") + fmt.Sprintf(keycloakAdminClientsPathTmpl, url.PathEscape(realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", &Error{StatusCode: 0, Message: "creating Keycloak clients request", Retryable: false}
	}
	q := req.URL.Query()
	q.Set("clientId", clientID)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", bearerPrefix+token)

	resp, err := h.client().Do(req)
	if err != nil {
		return "", &Error{StatusCode: 0, Message: transportErrContactingKeycloak, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Keycloak client lookup failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}
	var clients []keycloakClientRepr
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return "", &Error{StatusCode: resp.StatusCode, Message: "decoding Keycloak clients response", Retryable: true}
	}
	for _, c := range clients {
		if c.ClientID == clientID && c.ID != "" {
			return c.ID, nil
		}
	}
	// The client is not present yet (e.g. the realm import has not created it). Retry.
	return "", &Error{StatusCode: resp.StatusCode, Message: "OIDC client not found in Keycloak realm", Retryable: true}
}

// clientSecret reads the secret of the client with internal id via
// GET /admin/realms/<realm>/clients/<id>/client-secret → {"value": "..."} with the bearer
// token. SECURITY: the secret value is returned to the caller only; it is never logged and the
// error paths drain the body without inspecting it.
func (h *HTTPOIDCRegistrar) clientSecret(ctx context.Context, keycloakBaseURL, realm, id, token string) (string, error) {
	endpoint := strings.TrimRight(keycloakBaseURL, "/") + fmt.Sprintf(keycloakAdminClientSecretPathTmpl, url.PathEscape(realm), url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", &Error{StatusCode: 0, Message: "creating Keycloak client-secret request", Retryable: false}
	}
	req.Header.Set("Authorization", bearerPrefix+token)

	resp, err := h.client().Do(req)
	if err != nil {
		return "", &Error{StatusCode: 0, Message: transportErrContactingKeycloak, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Keycloak client-secret read failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}
	var sr keycloakClientSecretResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil || sr.Value == "" {
		return "", &Error{StatusCode: resp.StatusCode, Message: "decoding Keycloak client-secret response", Retryable: true}
	}
	return sr.Value, nil
}

// EnsureRealmUser implements OIDCRegistrar against a live Keycloak. It runs the admin flow in
// order — authenticate, look up the user by exact username, and (only if absent) create it.
// Existence is the idempotency key: an already-present user returns nil WITHOUT touching its
// password. On any failure it returns a *Error carrying only a step name + status code (never
// the admin creds, the token, the password, or a response body).
func (h *HTTPOIDCRegistrar) EnsureRealmUser(ctx context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string, user RealmUser) error {
	token, err := h.adminToken(ctx, keycloakBaseURL, adminUsername, adminPassword)
	if err != nil {
		return err
	}
	exists, err := h.realmUserExists(ctx, keycloakBaseURL, realm, user.Username, token)
	if err != nil {
		return err
	}
	if exists {
		// IDEMPOTENT: the user is already present. Do NOT reset the password or re-create —
		// a re-reconcile (or a password-Secret rotation re-enqueue) must never clobber it.
		return nil
	}
	return h.createRealmUser(ctx, keycloakBaseURL, realm, token, user)
}

// realmUserExists reports whether a user with the exact username exists in realm, via
// GET /admin/realms/<realm>/users?username=<u>&exact=true with the bearer token. The response
// is an array of user representations; the operator confirms the exact-username match (Keycloak
// honours exact=true, but the explicit check guards against any server-side fuzziness). SECURITY:
// the error paths drain the body without inspecting it; nothing user-identifying is logged.
func (h *HTTPOIDCRegistrar) realmUserExists(ctx context.Context, keycloakBaseURL, realm, username, token string) (bool, error) {
	endpoint := strings.TrimRight(keycloakBaseURL, "/") + fmt.Sprintf(keycloakAdminUsersPathTmpl, url.PathEscape(realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, &Error{StatusCode: 0, Message: "creating Keycloak users request", Retryable: false}
	}
	q := req.URL.Query()
	q.Set("username", username)
	q.Set("exact", "true")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", bearerPrefix+token)

	resp, err := h.client().Do(req)
	if err != nil {
		return false, &Error{StatusCode: 0, Message: transportErrContactingKeycloak, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Keycloak user lookup failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}
	var users []keycloakUserRepr
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return false, &Error{StatusCode: resp.StatusCode, Message: "decoding Keycloak users response", Retryable: true}
	}
	for _, u := range users {
		if u.Username == username {
			return true, nil
		}
	}
	return false, nil
}

// createRealmUser POSTs a new realm user to /admin/realms/<realm>/users with the identity, the
// superadmin attribute (groups: ["superadmin"]), and a single non-temporary password credential.
// A 2xx (Keycloak returns 201 Created) is success; a 409 Conflict (the user was created between
// the existence check and this POST — a benign race) is also treated as success (idempotent).
// SECURITY (critical): the password lives ONLY in the request body; it is never logged. The
// error paths drain the body without inspecting it and carry only a step name + status code.
func (h *HTTPOIDCRegistrar) createRealmUser(ctx context.Context, keycloakBaseURL, realm, token string, user RealmUser) error {
	body, err := json.Marshal(keycloakCreateUserRequest{
		Username:   user.Username,
		Email:      user.Email,
		FirstName:  user.FirstName,
		LastName:   user.LastName,
		Enabled:    true,
		Attributes: map[string][]string{"groups": {superadminGroupAttribute}},
		Credentials: []keycloakCredential{{
			Type: "password", Value: user.Password, Temporary: false,
		}},
	})
	if err != nil {
		// The marshal error could echo field values (the password); never wrap it verbatim.
		return &Error{StatusCode: 0, Message: "encoding Keycloak user request", Retryable: false}
	}

	endpoint := strings.TrimRight(keycloakBaseURL, "/") + fmt.Sprintf(keycloakAdminUsersPathTmpl, url.PathEscape(realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &Error{StatusCode: 0, Message: "creating Keycloak user request", Retryable: false}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerPrefix+token)

	resp, err := h.client().Do(req)
	if err != nil {
		return &Error{StatusCode: 0, Message: transportErrContactingKeycloak, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain (never inspect) the body — it may echo an error referencing the user/password.
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusConflict:
		// The user was created concurrently (existence check raced the POST): idempotent success.
		return nil
	default:
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("Keycloak user create failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}
}
