/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package registration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testKCAdminUser  = "kc-admin"
	testKCAdminPass  = "super-secret-admin-password"
	testKCRealm      = "ilm"
	testOIDCClientID = "ilm"
	// testClientSecret is the value Keycloak's admin API returns and the reconciler relays
	// into the operator-owned Secret. It is the sensitive payload the no-leak assertions
	// check never appears in an error string.
	testClientSecret  = "kc-generated-client-secret-9f8e7d"
	testKCClientUUID  = "c0ffee00-1234-5678-9abc-def012345678"
	testKCAccessToken = "eyJ-fake-admin-access-token"

	// Keycloak admin REST path fragments used to build the fake-server mux routes.
	adminRealmsPrefix    = "/admin/realms/"
	clientsPathSuffix    = "/clients"
	clientByIDPathPrefix = "/clients/"
)

// fakeKeycloak is an httptest server emulating Keycloak's master-realm token endpoint, the
// admin clients lookup, and the client-secret read. Each handler asserts the request shape and
// returns the canned token / client / secret.
func fakeKeycloak(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// Token endpoint: password grant, client_id=admin-cli, the admin creds in the form body.
	mux.HandleFunc(keycloakTokenPath, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "password", r.Form.Get("grant_type"))
		assert.Equal(t, "admin-cli", r.Form.Get("client_id"))
		assert.Equal(t, testKCAdminUser, r.Form.Get("username"))
		assert.Equal(t, testKCAdminPass, r.Form.Get("password"))
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`{"access_token":"` + testKCAccessToken + `","token_type":"Bearer"}`))
	})
	// Clients lookup: GET /admin/realms/ilm/clients?clientId=ilm with the bearer token.
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientsPathSuffix, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, bearerPrefix+testKCAccessToken, r.Header.Get("Authorization"))
		assert.Equal(t, testOIDCClientID, r.URL.Query().Get("clientId"))
		w.Header().Set(contentTypeKey, contentTypeJSON)
		// Return more than one client to prove the exact clientId match is used.
		_, _ = w.Write([]byte(`[{"id":"other-uuid","clientId":"account"},{"id":"` + testKCClientUUID + `","clientId":"` + testOIDCClientID + `"}]`))
	})
	// Client-secret read: GET /admin/realms/ilm/clients/<uuid>/client-secret.
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientByIDPathPrefix+testKCClientUUID+"/client-secret", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, bearerPrefix+testKCAccessToken, r.Header.Get("Authorization"))
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`{"type":"secret","value":"` + testClientSecret + `"}`))
	})
	return httptest.NewServer(mux)
}

// TestHTTPOIDCRegistrarFetchSuccess verifies the happy path: the registrar authenticates to
// Keycloak, resolves the ilm client's internal id, and returns the GENERATED client secret —
// the value the reconciler relays into the operator-owned OIDC client Secret (Core then reads
// it in-pod). The registrar makes NO call to Core (the in-pod postStart owns the PUT now).
func TestHTTPOIDCRegistrarFetchSuccess(t *testing.T) {
	kc := fakeKeycloak(t)
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	secret, err := reg.FetchClientSecret(context.Background(), kc.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.NoError(t, err)
	assert.Equal(t, testClientSecret, secret, "the secret fetched from Keycloak must be returned verbatim")
}

// TestHTTPOIDCRegistrarFetchBaseURLTrailingSlash verifies the Keycloak paths are appended
// cleanly even when the base URL carries a trailing slash.
func TestHTTPOIDCRegistrarFetchBaseURLTrailingSlash(t *testing.T) {
	kc := fakeKeycloak(t)
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	secret, err := reg.FetchClientSecret(context.Background(), kc.URL+"/", testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.NoError(t, err)
	assert.Equal(t, testClientSecret, secret)
}

// TestHTTPOIDCRegistrarKeycloakAuthFailureNoLeak verifies a Keycloak 401 yields an error
// carrying only the status code — never the admin password or the body.
func TestHTTPOIDCRegistrarKeycloakAuthFailureNoLeak(t *testing.T) {
	const leakyBody = "invalid_grant for user " + testKCAdminUser + " password " + testKCAdminPass
	kc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(leakyBody))
	}))
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	_, err := reg.FetchClientSecret(context.Background(), kc.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.Error(t, err)

	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, http.StatusUnauthorized, pErr.StatusCode)
	// SECURITY: no admin credentials / body leak into the error.
	assert.NotContains(t, err.Error(), testKCAdminPass)
	assert.NotContains(t, err.Error(), testKCAdminUser)
	assert.NotContains(t, err.Error(), leakyBody)
}

// TestHTTPOIDCRegistrarClientSecretServerErrorNoLeak verifies a Keycloak 5xx on the
// client-secret read yields a retryable error carrying only the status code — never the body.
func TestHTTPOIDCRegistrarClientSecretServerErrorNoLeak(t *testing.T) {
	const leakyBody = "stacktrace mentioning " + testClientSecret
	mux := http.NewServeMux()
	mux.HandleFunc(keycloakTokenPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`{"access_token":"` + testKCAccessToken + `"}`))
	})
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientsPathSuffix, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`[{"id":"` + testKCClientUUID + `","clientId":"` + testOIDCClientID + `"}]`))
	})
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientByIDPathPrefix+testKCClientUUID+"/client-secret", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(leakyBody))
	})
	kc := httptest.NewServer(mux)
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	_, err := reg.FetchClientSecret(context.Background(), kc.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.Error(t, err)

	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, http.StatusInternalServerError, pErr.StatusCode)
	assert.True(t, pErr.Retryable, "5xx must be retryable")
	// SECURITY: the body must never appear in the error.
	assert.NotContains(t, err.Error(), leakyBody)
}

// TestHTTPOIDCRegistrarClientNotFoundRetryable verifies that when the platform client is not
// yet present in the realm (e.g. the realm import has not created it), the registrar returns a
// retryable error and the secret read is never reached.
func TestHTTPOIDCRegistrarClientNotFoundRetryable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(keycloakTokenPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`{"access_token":"` + testKCAccessToken + `"}`))
	})
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientsPathSuffix, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`[{"id":"x","clientId":"account"}]`)) // no "ilm" client
	})
	secretRead := false
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+clientByIDPathPrefix, func(w http.ResponseWriter, _ *http.Request) {
		secretRead = true
		w.WriteHeader(http.StatusOK)
	})
	kc := httptest.NewServer(mux)
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	_, err := reg.FetchClientSecret(context.Background(), kc.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.Error(t, err)
	assert.False(t, secretRead, "the client-secret read must not be attempted when the client is not yet present")
	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable, "a not-yet-present client is a retryable wait")
}

// TestHTTPOIDCRegistrarTransportError verifies a connection failure to Keycloak yields a
// retryable, host-free error.
func TestHTTPOIDCRegistrarTransportError(t *testing.T) {
	kc := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		// Intentionally empty: the server is closed before any request is made, so this
		// handler is never invoked — the test exercises the transport (connect) failure path.
	}))
	url := kc.URL
	kc.Close() // close immediately so the token POST fails to connect

	reg := NewHTTPOIDCRegistrar()
	_, err := reg.FetchClientSecret(context.Background(), url, testKCRealm,
		testKCAdminUser, testKCAdminPass, testOIDCClientID)
	require.Error(t, err)
	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable, "a transport error must be retryable")
	assert.NotContains(t, err.Error(), strings.TrimPrefix(url, "http://"), "the host:port must not leak")
}

// --- EnsureRealmUser (password admin method) -------------------------------------------

// testAdminPassword is the sensitive payload the no-leak assertions check never appears in an
// error string. It is the value the operator reads from the caller-provided Secret. The
// identity constants are the realm-user fields asserted on the Keycloak admin API request.
const (
	testAdminPassword = "super-secret-admin-pw-2f9a"
	testAdminUsername = "root-operator"
	testAdminName     = "Platform Root"
	testAdminLastName = "Operator"
	testAdminEmail    = "root@secret.example.com"
)

// usersServer is a configurable Keycloak users-endpoint stand-in: the token endpoint plus the
// users lookup (returning the seeded existing users) and the create handler (capturing the POST
// body and returning the configured status). It records whether a create was attempted.
type usersServer struct {
	srv          *httptest.Server
	existing     []string // usernames the lookup reports present
	createStatus int      // status the create POST returns (default 201)
	createBody   []byte   // the captured create request body
	createCalled bool
}

func newUsersServer(t *testing.T) *usersServer {
	t.Helper()
	us := &usersServer{createStatus: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc(keycloakTokenPath, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, testKCAdminUser, r.Form.Get("username"))
		assert.Equal(t, testKCAdminPass, r.Form.Get("password"))
		w.Header().Set(contentTypeKey, contentTypeJSON)
		_, _ = w.Write([]byte(`{"access_token":"` + testKCAccessToken + `"}`))
	})
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+"/users", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, bearerPrefix+testKCAccessToken, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodGet:
			assert.Equal(t, "true", r.URL.Query().Get("exact"), "the lookup must be an exact-username match")
			q := r.URL.Query().Get("username")
			w.Header().Set(contentTypeKey, contentTypeJSON)
			for _, u := range us.existing {
				if u == q {
					_, _ = w.Write([]byte(`[{"id":"u-uuid","username":"` + u + `"}]`))
					return
				}
			}
			_, _ = w.Write([]byte(`[]`))
		case http.MethodPost:
			us.createCalled = true
			us.createBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(us.createStatus)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	us.srv = httptest.NewServer(mux)
	t.Cleanup(us.srv.Close)
	return us
}

func testRealmUser() RealmUser {
	return RealmUser{Username: testAdminUsername, Email: testAdminEmail, FirstName: testAdminName, LastName: testAdminLastName, Password: testAdminPassword}
}

// TestEnsureRealmUserCreatesWhenAbsent verifies that an absent user is CREATED with the
// superadmin attribute (groups: ["superadmin"]) and a single NON-temporary password credential,
// and that enabled=true is sent.
func TestEnsureRealmUserCreatesWhenAbsent(t *testing.T) {
	us := newUsersServer(t)
	reg := NewHTTPOIDCRegistrar()

	err := reg.EnsureRealmUser(context.Background(), us.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.NoError(t, err)
	require.True(t, us.createCalled, "an absent user must be created")

	var body keycloakCreateUserRequest
	require.NoError(t, json.Unmarshal(us.createBody, &body))
	assert.Equal(t, testAdminUsername, body.Username)
	assert.Equal(t, testAdminEmail, body.Email)
	assert.Equal(t, testAdminName, body.FirstName)
	assert.Equal(t, testAdminLastName, body.LastName, "lastName must be set so Keycloak does not prompt to complete the profile at first login")
	assert.True(t, body.Enabled, "the user must be enabled")
	assert.Equal(t, []string{"superadmin"}, body.Attributes["groups"], "the superadmin attribute drives Core's roles claim")
	require.Len(t, body.Credentials, 1)
	assert.Equal(t, "password", body.Credentials[0].Type)
	assert.Equal(t, testAdminPassword, body.Credentials[0].Value)
	assert.False(t, body.Credentials[0].Temporary, "the password must be permanent (not a forced reset on first login)")
}

// TestEnsureRealmUserIdempotentWhenExists verifies the IDEMPOTENCY contract: an already-present
// user (exact-username match) returns nil WITHOUT any create POST — so a re-reconcile never
// resets the password or re-creates the user.
func TestEnsureRealmUserIdempotentWhenExists(t *testing.T) {
	us := newUsersServer(t)
	us.existing = []string{testAdminUsername}
	reg := NewHTTPOIDCRegistrar()

	err := reg.EnsureRealmUser(context.Background(), us.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.NoError(t, err)
	assert.False(t, us.createCalled, "an existing user must NOT be re-created or have its password reset")
}

// TestEnsureRealmUserExactMatchNotFuzzy verifies the exact-username check guards against a
// server returning a non-matching user: a lookup that yields only a different username is
// treated as absent, so the user is created.
func TestEnsureRealmUserExactMatchNotFuzzy(t *testing.T) {
	us := newUsersServer(t)
	us.existing = []string{"root-other"} // a different username than the one we ensure
	reg := NewHTTPOIDCRegistrar()

	err := reg.EnsureRealmUser(context.Background(), us.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.NoError(t, err)
	assert.True(t, us.createCalled, "a lookup that returns only a different username must not count as a match")
}

// TestEnsureRealmUserConflictIsIdempotent verifies a 409 on create (the user appeared between
// the existence check and the POST — a benign race) is treated as success.
func TestEnsureRealmUserConflictIsIdempotent(t *testing.T) {
	us := newUsersServer(t)
	us.createStatus = http.StatusConflict
	reg := NewHTTPOIDCRegistrar()

	err := reg.EnsureRealmUser(context.Background(), us.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.NoError(t, err, "a 409 Conflict on create is idempotent success")
}

// TestEnsureRealmUserCreateFailureNoLeak verifies a 5xx on create yields a retryable error
// carrying ONLY the status code — never the password, admin creds, token, or response body.
func TestEnsureRealmUserCreateFailureNoLeak(t *testing.T) {
	us := newUsersServer(t)
	us.createStatus = http.StatusInternalServerError
	reg := NewHTTPOIDCRegistrar()

	err := reg.EnsureRealmUser(context.Background(), us.srv.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.Error(t, err)
	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, http.StatusInternalServerError, pErr.StatusCode)
	assert.True(t, pErr.Retryable, "5xx must be retryable")
	// SECURITY: no password / admin creds / token in the error.
	assert.NotContains(t, err.Error(), testAdminPassword)
	assert.NotContains(t, err.Error(), testKCAdminPass)
	assert.NotContains(t, err.Error(), testKCAccessToken)
}

// TestEnsureRealmUserAuthFailureNoLeak verifies a Keycloak admin-auth 401 yields a status-only
// error and never reaches the users endpoint (no user lookup/create on a failed token).
func TestEnsureRealmUserAuthFailureNoLeak(t *testing.T) {
	usersHit := false
	mux := http.NewServeMux()
	mux.HandleFunc(keycloakTokenPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid_grant for " + testKCAdminUser))
	})
	mux.HandleFunc(adminRealmsPrefix+testKCRealm+"/users", func(http.ResponseWriter, *http.Request) {
		usersHit = true
	})
	kc := httptest.NewServer(mux)
	defer kc.Close()

	reg := NewHTTPOIDCRegistrar()
	err := reg.EnsureRealmUser(context.Background(), kc.URL, testKCRealm,
		testKCAdminUser, testKCAdminPass, testRealmUser())
	require.Error(t, err)
	assert.False(t, usersHit, "no users call must be made when admin auth fails")
	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, http.StatusUnauthorized, pErr.StatusCode)
	assert.NotContains(t, err.Error(), testKCAdminPass)
	assert.NotContains(t, err.Error(), testAdminPassword)
}
