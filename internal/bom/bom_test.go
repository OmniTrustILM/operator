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

package bom

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookupReturnsComponentImageForVersion(t *testing.T) {
	img, ok := Lookup("core")
	assert.True(t, ok)
	assert.Equal(t, "core", img.Name)
	assert.NotEmpty(t, img.Tag) // operator's pinned tag for core
}

func TestLookupUnknownComponent(t *testing.T) {
	_, ok := Lookup("does-not-exist")
	assert.False(t, ok)
}

func TestWiringProfileIsPopulated(t *testing.T) {
	w := Wiring()
	// env-var NAMES live here as versioned data — never as literals in the reconciler.
	assert.NotEmpty(t, w.DatabaseURLEnv)
	assert.NotEmpty(t, w.DatabaseURLTemplate)
	assert.NotEmpty(t, w.DatabaseCred.UsernameEnv)
	assert.NotEmpty(t, w.DatabaseCred.PasswordKey)
	assert.NotEmpty(t, w.MessagingHostEnv)
	assert.NotEmpty(t, w.MessagingCred.PasswordEnv)
}

func TestWiringProfileRendersDatabaseURL(t *testing.T) {
	w := Wiring()
	// The profile appends the connection query suffix (?characterEncoding=UTF-8).
	assert.Equal(t, "jdbc:postgresql://db.example.com:5432/ilmdb?characterEncoding=UTF-8",
		w.DatabaseURL("db.example.com", 5432, "ilmdb"))
}

func TestDefaultVersionMatchesCoreTag(t *testing.T) {
	// The operator's platform version tracks the core image tag; keep them in lockstep.
	core, ok := Lookup("core")
	assert.True(t, ok)
	assert.Equal(t, DefaultVersion, core.Tag)
}

func TestAuthWiringProfilePopulated(t *testing.T) {
	w := Wiring()
	// auth env names + derived-Secret coordinates are versioned data here.
	assert.Equal(t, "AUTH_CREATE_UNKNOWN_USERS", w.AuthCreateUsersEnv)
	assert.Equal(t, "AUTH_CREATE_UNKNOWN_ROLES", w.AuthCreateRolesEnv)
	assert.Equal(t, "SYNC_POLICY", w.AuthSyncPolicyEnv)
	assert.Equal(t, "ASPNETCORE_URLS", w.AuthAspNetURLsEnv)
	assert.Equal(t, "http://+:8080", w.AuthAspNetURLs)
	assert.Equal(t, "AUTH_DB_CONNECTION_STRING", w.AuthDBConnEnv)
	assert.Equal(t, "auth-db", w.AuthDBSecretName)
	assert.Equal(t, "connection-string", w.AuthDBSecretKey)
}

// TestAuthDBConnectionStringFormat pins the composed .NET connection string to the exact
// format auth expects: the field order Host;Port;Username;Password;Database;
// Pooling=true must match byte-for-byte (Npgsql parses it positionally-tolerant but the
// platform's contract fixes this layout).
func TestAuthDBConnectionStringFormat(t *testing.T) {
	w := Wiring()
	got := w.AuthDBConnectionString("db.example.com", 5432, "ilm", "ilm", "placeholder")
	assert.Equal(t,
		"Host=db.example.com;Port=5432;Username=ilm;Password=placeholder;Database=ilm;Pooling=true",
		got)
}

// TestMessagingTopology locks the managed-RabbitMQ topology DATA: the cardinality (5 users /
// 2 exchanges / 10 queues / 9 bindings), the load-bearing permission regexes for the
// proxy/core/monitor users, and the defaults (vhost, version).
func TestMessagingTopology(t *testing.T) {
	topo := Messaging()
	assert.Len(t, topo.Users, 5, "five users: administrator/provisioner/proxy/core/monitor")
	assert.Len(t, topo.Exchanges, 2, "two exchanges: czertainly + czertainly-proxy")
	assert.Len(t, topo.Queues, 10, "ten queues (core.* + time-quality.*)")
	assert.Len(t, topo.Bindings, 9, "nine bindings (core.* + time-quality.*)")

	byRole := map[MessagingUserRole]MessagingUser{}
	for _, u := range topo.Users {
		byRole[u.Role] = u
	}

	// administrator + provisioner: admin tags, full ".*" permissions.
	for _, role := range []MessagingUserRole{MessagingUserAdministrator, MessagingUserProvisioner} {
		u := byRole[role]
		assert.Equal(t, []string{"administrator"}, u.Tags, "%s has the administrator tag", role)
		assert.Equal(t, ".*", u.Configure)
		assert.Equal(t, ".*", u.Write)
		assert.Equal(t, ".*", u.Read)
	}

	// proxy: no configure; write czertainly-proxy; read proxy.* — exact chart regexes.
	proxy := byRole[MessagingUserProxy]
	assert.Empty(t, proxy.Tags)
	assert.Equal(t, "", proxy.Configure)
	assert.Equal(t, "^czertainly-proxy$", proxy.Write)
	assert.Equal(t, `^proxy\..*$`, proxy.Read)

	// core: no configure; write czertainly(-proxy)?; read core.* AND the time-quality monitor's
	// request/result queues (the 2.18.0 addition that, missing, crash-loops Core).
	core := byRole[MessagingUserCore]
	assert.Empty(t, core.Tags)
	assert.Equal(t, "", core.Configure)
	assert.Equal(t, "^czertainly(-proxy)?$", core.Write)
	assert.Equal(t, `^core(\..+|-.+)?$|^time-quality\.(config-request|results)$`, core.Read)

	// monitor (time-quality, new in 2.18.0): write czertainly; read time-quality.config.
	monitor := byRole[MessagingUserMonitor]
	assert.Empty(t, monitor.Tags)
	assert.Equal(t, "", monitor.Configure)
	assert.Equal(t, "^czertainly$", monitor.Write)
	assert.Equal(t, `^time-quality\.config$`, monitor.Read)

	assert.Equal(t, "czertainly", DefaultVirtualHost)
	assert.NotEmpty(t, DefaultRabbitMQVersion)
}

// TestBundleForEmptyResolvesDefault proves an empty version selects the DefaultVersion
// bundle (the operator's newest) — the out-of-the-box behaviour when spec.version is unset.
func TestBundleForEmptyResolvesDefault(t *testing.T) {
	empty, ok := BundleFor("")
	assert.True(t, ok, "empty version resolves the default bundle")
	def, ok := BundleFor(DefaultVersion)
	assert.True(t, ok, "the DefaultVersion key exists")
	// The empty-version bundle IS the DefaultVersion bundle.
	assert.Equal(t, def.Wiring, empty.Wiring)
	assert.Equal(t, def.Components, empty.Components)
	assert.Equal(t, def.RabbitMQVersion, empty.RabbitMQVersion)
}

// TestBundleForKnownVersion proves the shipped version resolves and carries the expected
// version-specific data (the core image tag + the infra defaults the upgrade guard reads).
func TestBundleForKnownVersion(t *testing.T) {
	b, ok := BundleFor("2.18.0")
	assert.True(t, ok)
	img, found := b.Lookup("core")
	assert.True(t, found)
	assert.Equal(t, "2.18.0", img.Tag)
	// Infra default versions live in the bundle (Phase-2 upgrade guard reads them).
	// Pin all three exactly so a silent regression of the managed PostgreSQL / RabbitMQ /
	// Keycloak default fails the fast `make test` loop instead of only the managed e2e.
	assert.Equal(t, "18", b.CNPGVersion)
	assert.Equal(t, "4.3.1", b.RabbitMQVersion)
	assert.Equal(t, "26.6.3", b.KeycloakVersion)
	assert.Len(t, b.Messaging.Users, 5, "the bundle carries the managed-RabbitMQ topology (incl. the monitor user)")
}

// TestBundleForUnknownVersion proves an unknown version returns ok=false so the reconciler
// degrades (rather than rendering against a non-existent bundle). The supported set grows
// over time, which is why this is a runtime check, not a frozen CEL enum.
func TestBundleForUnknownVersion(t *testing.T) {
	_, ok := BundleFor("9.9.9")
	assert.False(t, ok)
}

// TestSupportedVersionsIncludesDefault proves SupportedVersions is derived from the bundle
// keys (so the degraded message can never list a version BundleFor would reject) and
// includes the default.
func TestSupportedVersionsIncludesDefault(t *testing.T) {
	vs := SupportedVersions()
	assert.NotEmpty(t, vs)
	assert.Contains(t, vs, DefaultVersion)
	// Every listed version must actually resolve.
	for _, v := range vs {
		_, ok := BundleFor(v)
		assert.True(t, ok, "SupportedVersions lists %q but BundleFor rejects it", v)
	}
}

// TestDefaultVersionIsNewest pins the invariant DefaultVersion must hold: it equals the
// HIGHEST (newest) bundle key, so an unset spec.version resolves the operator's newest.
func TestDefaultVersionIsNewest(t *testing.T) {
	vs := SupportedVersions() // sorted ascending
	assert.Equal(t, vs[len(vs)-1], DefaultVersion, "DefaultVersion must be the newest shipped bundle")
}

// TestPackageWrappersResolveDefaultBundle proves the version-agnostic package wrappers
// (Lookup/Wiring/Messaging) resolve the SAME data as BundleFor("") — they are the
// backward-compat surface for callers that do not select a version.
func TestPackageWrappersResolveDefaultBundle(t *testing.T) {
	def, _ := BundleFor("")
	assert.Equal(t, def.Wiring, Wiring())
	assert.Equal(t, def.Messaging, Messaging())
	img, ok := Lookup("core")
	defImg, defOK := def.Lookup("core")
	assert.Equal(t, defOK, ok)
	assert.Equal(t, defImg, img)
	assert.Equal(t, def.RabbitMQVersion, DefaultRabbitMQVersion)
}
