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
	"github.com/stretchr/testify/require"
)

// Test-local version literals. These are DELIBERATELY independent of the production
// version2170/version2180/version2190 consts in bom.go: this suite is a full-matrix
// characterization of the bundle DATA, and reusing the production consts here would let
// a bundle's key silently drift with its own const instead of being pinned by an
// independent literal.
const (
	testVersion2170 = "2.17.0"
	testVersion2180 = "2.18.0"
	testVersion2190 = "2.19.0"
	testVersion2100 = "2.10.0"
	testVersion290  = "2.9.0"
)

// Test-local literals for the 2.19.0 time-quality/provider queue names, independent of
// the production queue name consts in bom.go for the same reason as the version
// literals above.
const (
	testQueueProviderStatusPoll       = "provider.status-poll"
	testQueueTimeQualityConfig        = "time-quality.config"
	testQueueTimeQualityConfigRequest = "time-quality.config-request"
	testQueueTimeQualityResults       = "time-quality.results"
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

// TestDefaultVersionMatchesCoreTag pins a release invariant: the DEFAULT bundle's
// version-aligned core tag must equal DefaultVersion. The tag is a literal in the
// bundle data (see TestBundleIdentityImmuneToDefaultVersion) — this test is what
// forces the release-day flip PR to retag core when it moves DefaultVersion.
func TestDefaultVersionMatchesCoreTag(t *testing.T) {
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

// TestMessagingTopology locks the 2.18.0 managed-RabbitMQ topology DATA: the cardinality
// (5 users / 2 exchanges / 10 queues / 9 bindings), the load-bearing permission regexes for
// the proxy/core/monitor users, and the defaults (vhost, version).
//
// PINNED on purpose — the czertainly exchanges, the 10/9 queue/binding cardinality and the
// pre-rename permission regexes are 2.18.0 facts that live platforms still run on.
// TestBundle2190 locks the 2.19.0 topology in full, and TestPackageWrappersResolveDefaultBundle
// proves Messaging() resolves the default, so nothing is lost by pinning here.
func TestMessagingTopology(t *testing.T) {
	b, ok := BundleFor(testVersion2180)
	require.True(t, ok, "the 2.18.0 bundle must resolve")
	topo := b.Messaging
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

	assert.Equal(t, "czertainly", LegacyUnscopedVirtualHost)
	assert.Equal(t, LegacyUnscopedVirtualHost, DefaultVirtualHost,
		"the deprecated alias must keep resolving to the legacy vhost (external modules import it)")
	assert.NotEmpty(t, DefaultRabbitMQVersion)
}

// TestBundleForEmptyResolvesDefault proves an empty version selects the DefaultVersion
// bundle — the out-of-the-box behaviour when spec.version is unset.
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
	b, ok := BundleFor(testVersion2180)
	assert.True(t, ok)
	img, found := b.Lookup("core")
	assert.True(t, found)
	assert.Equal(t, testVersion2180, img.Tag)
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

// TestSupportedVersionsExplicit pins the advertised (released) version set exactly — a new
// bundle changes this list only once its own release-day flip PR marks it Released and
// updates this expectation.
func TestSupportedVersionsExplicit(t *testing.T) {
	assert.Equal(t, []string{testVersion2170, testVersion2180, testVersion2190}, SupportedVersions())
}

// TestSupportedVersionsExcludesPreviewBundle pins the Released filter with a SYNTHETIC preview
// bundle, because the production data alone cannot: all three shipped bundles are Released
// today, so a mutation that deletes the `if b.Released` guard in SupportedVersions leaves
// TestSupportedVersionsExplicit and TestSupportedVersionsIncludesDefault equally green either
// way. Inserting an unreleased bundle directly into the package-level bundles map — the seam
// this internal test package has — makes the filter's absence observable again: SupportedVersions
// must drop the synthetic key while AllVersions and BundleFor still reach it, exactly as a real
// (not-yet-released) bundle would behave. The key is a version string no production bundle will
// ever use, and t.Cleanup removes it before any other test in this package runs.
func TestSupportedVersionsExcludesPreviewBundle(t *testing.T) {
	const previewVersion = "9.9.9-preview-synthetic"
	require.NotContains(t, bundles, previewVersion, "the synthetic key must not collide with real bundle data")
	bundles[previewVersion] = Bundle{Released: false}
	t.Cleanup(func() { delete(bundles, previewVersion) })

	assert.NotContains(t, SupportedVersions(), previewVersion,
		"a preview (Released=false) bundle must never appear in the advertised set")
	assert.Contains(t, AllVersions(), previewVersion,
		"AllVersions is the full key set — released and preview both")

	b, ok := BundleFor(previewVersion)
	assert.True(t, ok, "an explicit spec.version reaches a preview bundle exactly like a released one")
	assert.False(t, b.Released)
}

// TestDefaultVersionIsReleased is the ONE invariant DefaultVersion must satisfy: it must name
// a RELEASED bundle (an empty spec.version can never land a fresh install on a preview). It
// does NOT need to be the NEWEST released bundle — a release-day flip PR is free to mark a
// bundle Released without moving DefaultVersion to it, so a released-but-not-default bundle can
// exist between a bundle's release and the day DefaultVersion moves to it (2.19.0's flip did
// both at once); moving DefaultVersion is a separate, deliberate decision either PR is free to
// leave alone.
func TestDefaultVersionIsReleased(t *testing.T) {
	b, ok := BundleFor(DefaultVersion)
	assert.True(t, ok)
	assert.True(t, b.Released, "DefaultVersion must point at a released bundle")
}

// TestVersionOrderingIsSemver proves ordering is numeric per segment, not lexicographic
// (lexicographic would sort 2.9.0 after 2.10.0).
func TestVersionOrderingIsSemver(t *testing.T) {
	assert.True(t, semverLess(testVersion290, testVersion2100))
	assert.True(t, semverLess(testVersion2180, testVersion2190))
	assert.False(t, semverLess(testVersion2190, testVersion2180))
	assert.False(t, semverLess(testVersion2180, testVersion2180))
}

// TestSortVersionsIsSemverNotLexicographic exercises the ONE sort both SupportedVersions and
// AllVersions run, with ADVERSARIAL keys that lexicographic ordering gets wrong (2.10.0 <
// 2.9.0 as strings). Swapping sortVersions' comparison for sort.Strings must fail here.
func TestSortVersionsIsSemverNotLexicographic(t *testing.T) {
	cases := []struct {
		name     string
		in, want []string
	}{
		{"double-digit minor sorts after single-digit", []string{testVersion2100, testVersion290}, []string{testVersion290, testVersion2100}},
		{"double-digit patch sorts after single-digit", []string{"2.18.10", "2.18.9"}, []string{"2.18.9", "2.18.10"}},
		{"already ascending is preserved", []string{testVersion290, testVersion2100, "3.0.0"}, []string{testVersion290, testVersion2100, "3.0.0"}},
		{"mixed majors", []string{"10.0.0", "9.9.9", "2.100.0"}, []string{"2.100.0", "9.9.9", "10.0.0"}},
		{"single element", []string{testVersion2190}, []string{testVersion2190}},
		{"empty", []string{}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sortVersions(c.in))
		})
	}
}

// TestVersionListsShareTheSemverSort proves the public lists go through that same helper: both
// come back ascending, and AllVersions is a superset of SupportedVersions (the previews are the
// difference), so "newest is last" holds for both.
func TestVersionListsShareTheSemverSort(t *testing.T) {
	for _, vs := range [][]string{SupportedVersions(), AllVersions()} {
		require.NotEmpty(t, vs)
		assert.Equal(t, sortVersions(append([]string{}, vs...)), vs, "the list must already be semver-ascending")
	}
	for _, v := range SupportedVersions() {
		assert.Contains(t, AllVersions(), v, "AllVersions must include every released version")
	}
	assert.Equal(t, testVersion2190, AllVersions()[len(AllVersions())-1],
		"the newest bundle is last in both lists")
	assert.Equal(t, testVersion2190, SupportedVersions()[len(SupportedVersions())-1],
		"2.19.0 is released, so it is advertised too")
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

func TestDefaultBundleHasProxyComponent(t *testing.T) {
	b, ok := BundleFor(DefaultVersion)
	assert.True(t, ok)
	img, ok := b.Lookup(ComponentProxy)
	assert.True(t, ok, "default bundle must carry the proxy component image")
	assert.Equal(t, "proxy", img.Name)
	assert.NotEmpty(t, img.Tag)
}

func TestPreRebrandBundleHasNoProxyComponent(t *testing.T) {
	b, ok := BundleFor(testVersion2170)
	assert.True(t, ok)
	_, ok = b.Lookup(ComponentProxy)
	assert.False(t, ok, "the proxy component did not exist pre-rebrand")
}

// TestBundleIdentityImmuneToDefaultVersion pins the demotion-trap fix (a
// characterization test, deliberately green before AND after the refactor): every
// bundle is keyed by its own version literal and version-aligned tags are literals,
// so changing DefaultVersion can never relabel a bundle or retag its images.
func TestBundleIdentityImmuneToDefaultVersion(t *testing.T) {
	for _, key := range []string{testVersion2170, testVersion2180} {
		_, ok := BundleFor(key)
		assert.True(t, ok, "bundle %q must be keyed by its literal version", key)
	}
	b, _ := BundleFor(testVersion2180)
	assert.Equal(t, testVersion2180, b.Components["core"].Tag, "core tag must be a literal, not DefaultVersion")
	assert.Equal(t, testVersion2180, b.Components["fe-administrator"].Tag, "fe-administrator tag must be a literal, not DefaultVersion")
}

// TestTopologyCarriesVhost pins the per-bundle vhost the migration trigger compares:
// pre-2.19 topologies declare "czertainly".
func TestTopologyCarriesVhost(t *testing.T) {
	for _, v := range []string{testVersion2170, testVersion2180} {
		b, ok := BundleFor(v)
		assert.True(t, ok)
		assert.Equal(t, "czertainly", b.Messaging.DefaultVirtualHost, testBundleContext, v)
	}
}

// TestTopologyHasUserRole pins the per-bundle user-role fact two behaviours key on: which
// generated credentials Secret the administrator paths may reference, and which SOURCE
// topologies a messaging migration is supported from. 2.17.0's single-user layout declares no
// administrator ROLE (its lone user is Core, merely TAGGED administrator); 2.18.0 onwards do.
func TestTopologyHasUserRole(t *testing.T) {
	b217, _ := BundleFor(testVersion2170)
	assert.False(t, b217.Messaging.HasUserRole(MessagingUserAdministrator))
	assert.True(t, b217.Messaging.HasUserRole(MessagingUserCore))

	for _, v := range []string{testVersion2180, testVersion2190} {
		b, _ := BundleFor(v)
		assert.True(t, b.Messaging.HasUserRole(MessagingUserAdministrator), testBundleContext, v)
		assert.True(t, b.Messaging.HasUserRole(MessagingUserProvisioner), testBundleContext, v)
	}
}

// TestLatestOnlyRetentionQueues pins which queues a consumer of this data may treat as DESIGNED
// to sit non-empty — the exemption a messaging migration's drain must not wait on, derived from
// the declared arguments so a future bundle's retention queue inherits it. Getting this wrong in
// either direction is serious: a missed exemption hangs every migration, a spurious one lets a
// queue holding real messages be reclaimed.
func TestLatestOnlyRetentionQueues(t *testing.T) {
	retention := func(version string) []string {
		b, ok := BundleFor(version)
		assert.True(t, ok, testBundleContext, version)
		var names []string
		for _, q := range b.Messaging.Queues {
			if q.IsLatestOnlyRetention() {
				names = append(names, q.Name)
			}
		}
		return names
	}

	want := []string{testQueueTimeQualityConfig, "time-quality.config-request"}
	assert.ElementsMatch(t, want, retention(testVersion2180))
	assert.ElementsMatch(t, want, retention(testVersion2190))
	assert.Empty(t, retention(testVersion2170), "2.17.0 declares no time-quality queues at all")

	assert.False(t, MessagingQueue{Name: "plain"}.IsLatestOnlyRetention(), "a queue with no arguments retains nothing")
	assert.False(t, MessagingQueue{
		Name: "bounded", Arguments: map[string]interface{}{queueArgMaxLength: int64(5000)},
	}.IsLatestOnlyRetention(), "a merely bounded queue is still expected to empty")
	assert.False(t, MessagingQueue{
		Name: "mistyped", Arguments: map[string]interface{}{queueArgMaxLength: 1},
	}.IsLatestOnlyRetention(), "arguments are int64 by contract; anything else is not a retention queue")
}

// TestBundle2190 pins the ENTIRE 2.19.0 contract, extracted from the
// helm-charts 2.18.0..HEAD diff. Full-matrix on purpose: partial assertions let a
// provisioning-exchange bug through review once already.
func TestBundle2190(t *testing.T) {
	b, ok := BundleFor(testVersion2190)
	assert.True(t, ok, "2.19.0 must resolve via explicit spec.version")
	assert.True(t, b.Released, "2.19.0 is released: the operator reached CR parity with the Helm chart")
	assert.True(t, b.HasProvisioning)

	// Advertised set now carries 2.19.0, and it is the fresh-install default.
	assert.Equal(t, []string{testVersion2170, testVersion2180, testVersion2190}, SupportedVersions())
	assert.Equal(t, []string{testVersion2170, testVersion2180, testVersion2190}, AllVersions())
	assert.Equal(t, testVersion2190, DefaultVersion)

	// Complete image matrix — VERIFIED against the released helm-charts 2.19.0 tag
	// (2026-08-03): auth bumped to 1.7.0 and scheduler to 1.1.1 in the release cut.
	assert.Equal(t, map[string]Image{
		"core":                 {Name: "core", Tag: testVersion2190},
		"auth":                 {Name: "auth", Tag: "1.7.0"},
		"auth-opa-policies":    {Name: "auth-opa-policies", Tag: "1.4.1"},
		"proxy":                {Name: "proxy", Tag: "1.0.0"},
		"opa":                  {Name: "opa", Tag: "1.10.0-static"},
		"curl":                 {Name: "curl", Tag: "8.16.0"},
		"scheduler":            {Name: "scheduler", Tag: "1.1.1"},
		"fe-administrator":     {Name: "frontend-administrator", Tag: testVersion2190},
		"utils":                {Name: "utils-service", Tag: "1.0.2"},
		"api-gateway":          {Name: "kong", Tag: "3.9.1"},
		"provisioning":         {Name: "provisioning-rabbitmq", Tag: "1.0.0"},
		"keycloak-theme":       {Name: "keycloak-theme", Tag: "0.1.4"},
		"time-quality-monitor": {Name: "time-quality-monitor", Tag: "1.0.0", Repository: "ilm-private"},
	}, b.Components)

	// Wiring: the 2.19.0 env changes + the provisioning proxy-exchange rename. The
	// released tag renamed the provisioning service's logging var too (it was still
	// _CZERTAINLY at development HEAD — the release cut changed it).
	assert.Equal(t, "LOGGING_LEVEL_COM_OTILM", b.Wiring.LoggingLevelEnv)
	assert.Equal(t, "MESSAGING_TIME_QUALITY_ENABLED", b.Wiring.TimeQualityEnabledEnv)
	assert.Equal(t, "PLATFORM_INSTANCE_ID", b.Wiring.PlatformInstanceIDEnv)
	assert.Equal(t, "ilm-proxy", b.Wiring.Provisioning.DefaultExchange,
		"PROXY_EXCHANGE default must follow the 2.19.0 exchange rename (charts 6aa78a4)")
	assert.Equal(t, "LOGGING_LEVEL_COM_OTILM", b.Wiring.Provisioning.LoggingLevelEnv,
		"provisioning-rabbitmq logging env IS renamed at the released 2.19.0 tag")

	// Messaging topology: vhost, exchanges (name+type+durability), users, queues, bindings.
	m := b.Messaging
	assert.Equal(t, "/", m.DefaultVirtualHost)
	assert.Equal(t, []MessagingExchange{
		{Name: "ilm", Type: "direct", Durable: true},
		{Name: "ilm-proxy", Type: "topic", Durable: true},
	}, m.Exchanges)
	assert.Equal(t, []MessagingUser{
		{Role: MessagingUserAdministrator, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		{Role: MessagingUserProvisioner, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		{Role: MessagingUserProxy, Tags: nil, Configure: "", Write: "^ilm-proxy$", Read: `^proxy\..*$`},
		{Role: MessagingUserCore, Tags: nil, Configure: "", Write: "^ilm(-proxy)?$", Read: `^core(\..+|-.+)?$|^provider\.status-poll$|^time-quality\.(config-request|results)$`},
		{Role: MessagingUserMonitor, Tags: nil, Configure: "", Write: "^ilm$", Read: `^time-quality\.config$`},
	}, m.Users)
	assert.Equal(t, []MessagingQueue{
		{Name: "core", Durable: true},
		{Name: "core.audit-logs", Durable: true},
		{Name: "core.notifications", Durable: true},
		{Name: "core.scheduler", Durable: true},
		{Name: "core.actions", Durable: true},
		{Name: "core.validation", Durable: true},
		{Name: "core.events", Durable: true},
		{Name: testQueueProviderStatusPoll, Durable: true},
		{Name: testQueueTimeQualityConfig, Durable: true, Arguments: map[string]interface{}{"x-max-length": int64(1), "x-overflow": "drop-head"}},
		{Name: testQueueTimeQualityConfigRequest, Durable: true, Arguments: map[string]interface{}{"x-max-length": int64(1), "x-overflow": "drop-head"}},
		{Name: testQueueTimeQualityResults, Durable: true},
	}, m.Queues)
	assert.Equal(t, []MessagingBinding{
		{Source: "ilm", Destination: "core.audit-logs", RoutingKey: "audit-logs"},
		{Source: "ilm", Destination: "core.notifications", RoutingKey: "notification"},
		{Source: "ilm", Destination: "core.actions", RoutingKey: "action"},
		{Source: "ilm", Destination: "core.scheduler", RoutingKey: "scheduler"},
		{Source: "ilm", Destination: "core.validation", RoutingKey: "validation"},
		{Source: "ilm", Destination: "core.events", RoutingKey: "event"},
		{Source: "ilm", Destination: testQueueProviderStatusPoll, RoutingKey: testQueueProviderStatusPoll},
		{Source: "ilm", Destination: testQueueTimeQualityConfig, RoutingKey: testQueueTimeQualityConfig},
		{Source: "ilm", Destination: testQueueTimeQualityConfigRequest, RoutingKey: testQueueTimeQualityConfigRequest},
		{Source: "ilm", Destination: testQueueTimeQualityResults, RoutingKey: testQueueTimeQualityResults},
	}, m.Bindings)

	// Engine versions carry over from 2.18.0 (no engine bumps this cycle).
	b18, _ := BundleFor(testVersion2180)
	assert.Equal(t, b18.RabbitMQVersion, b.RabbitMQVersion)
	assert.Equal(t, b18.CNPGVersion, b.CNPGVersion)
	assert.Equal(t, b18.KeycloakVersion, b.KeycloakVersion)
}
