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

// Package bom is the operator's bill-of-materials: a map of VERSIONED bundles,
// each holding the tested set of component image coordinates AND the wiring
// profile (default connection-string template, Secret-key names, target env-var
// names), the managed-RabbitMQ messaging topology, and the managed-infra default
// versions for ONE platform version. It is versioned DATA — the reconciler selects
// a bundle (spec.version, defaulting to the operator's newest) and reads it, the CR
// overrides it, so no env-var names are hard-coded in reconcile logic.
//
// Decoupling: because the BOM is a MAP keyed by version (not a single compile-time
// constant), one operator build can manage a supported RANGE of platform versions —
// a canary version in one namespace, a fix taken without moving the platform.
package bom

import (
	"sort"
	"strconv"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// DefaultVersion is the newest RELEASED bundle the operator ships — the FRESH-INSTALL
// fallback, i.e. the version a brand-new Platform with an empty spec.version resolves to
// (and then pins on status.observedVersion).
//
// It is NOT what an EXISTING Platform floats to: a platform already reconciled once follows
// its pinned status.observedVersion whenever spec.version is empty (pin-on-create), so
// bumping DefaultVersion in a new operator build never silently upgrades a running platform.
//
// Every bundle is keyed by its OWN version const — never by DefaultVersion — so flipping
// DefaultVersion can never relabel a bundle or retag its images (the "demotion trap").
const DefaultVersion = "2.18.0"

// DefaultImageRegistry and DefaultImageRepository are the registry host and
// repository the ILM component images ship under (the public registry the
// platform components are published to). They are
// versioned DATA: the platform builder defaults the effective shared ImageSpec's
// registry to DefaultImageRegistry when spec.image.registry is unset, so an
// out-of-the-box CR resolves a component like "core" to
// hub.omnitrustregistry.com/ilm/core:<tag> instead of the bare "core:<tag>" (which
// Docker would resolve to docker.io). DefaultImageRepository is not spec-defaulted —
// it resolves lazily inside image resolution (see ResolveImage), so a component's own
// Image.Repository (e.g. "ilm-private") can still override it. A user-set
// spec.image.registry/repository still wins (see ResolveImage precedence).
//
// These are SHARED across bundles today (every supported platform version ships from
// the same public registry); a bundle that needs a different registry would carry its
// own RegistryDefaults — see Bundle.
const (
	DefaultImageRegistry   = "hub.omnitrustregistry.com"
	DefaultImageRepository = "ilm"
)

// Bundle version keys (also used as component tags for the version-aligned
// components core + frontend-administrator). Every bundle is keyed by its OWN
// version const — never by DefaultVersion — so flipping DefaultVersion can never
// relabel a bundle or retag its images (the "demotion trap").
const (
	version2170 = "2.17.0"
	version2180 = "2.18.0"
	version2190 = "2.19.0"
)

// componentAuthOPAPolicies is the operator's component identity for the
// configured policies-server image (published as auth-opa-policies), reused as
// both the bundle map key and the published image name.
const componentAuthOPAPolicies = "auth-opa-policies"

// ComponentProxy is the operator's component identity for the ILM proxy image —
// the restricted-zone broker bridge deployed via the Proxy CRD. Exported because
// the proxy controller (a different package) resolves its image by this key. It
// exists only in 2.18.0+ bundles (the component did not exist pre-rebrand).
const ComponentProxy = "proxy"

// componentFeAdministrator is the operator's component identity (bundle map key) for
// the administrator frontend; imageNameFeAdministrator is its PUBLISHED image name —
// the two differ, like auth and scheduler.
const (
	componentFeAdministrator = "fe-administrator"
	imageNameFeAdministrator = "frontend-administrator"
)

// componentUtils is the operator's component identity (bundle map key) for the utils
// service; imageNameUtils is its PUBLISHED image name — the two differ.
const (
	componentUtils = "utils"
	imageNameUtils = "utils-service"
)

// componentAPIGateway is the operator's component identity (bundle map key) for the
// API gateway component, published under the "kong" image name (Kong is the
// underlying gateway engine).
const componentAPIGateway = "api-gateway"

// componentKeycloakTheme is the operator's component identity for the ilm Keycloak
// login theme, reused as both the bundle map key and the published image name (the
// same pattern as componentAuthOPAPolicies above).
const componentKeycloakTheme = "keycloak-theme"

// Tags shared by every CURRENT bundle: opa and curl are third-party/base images
// pinned identically across all supported platform versions today. This is a dedup of
// today's data, not a promise of cross-version alignment — a bundle that needs a
// different pin simply stops using the const and sets its own literal.
const (
	tagOpa1100Static = "1.10.0-static"
	tagCurl8160      = "8.16.0"
)

// Image is the per-component image coordinates from a bundle. Repository is set only
// for components published outside the default repository (e.g. ilm-private) — empty
// means DefaultImageRepository applies (resolved lazily in the builder, never eagerly
// on the CR).
type Image struct {
	Name       string
	Tag        string
	Repository string
}

// Bundle is everything version-specific for ONE platform version: the per-component
// image coordinates, the wiring profile (env-var names + connection-string template +
// Secret-key names), the managed-RabbitMQ messaging topology, and the managed-infra
// default versions (CNPG/RabbitMQ/Keycloak). Selecting a platform version (spec.version
// → BundleFor) selects exactly one Bundle; the reconciler threads it through render so a
// version bump is a DATA edit (a new key in `bundles`), never a code change.
type Bundle struct {
	// Components maps the operator's component identity (clean Service name, labels) to
	// the PUBLISHED image coordinates. The KEY differs from Image.Name for several
	// components (auth, scheduler, frontend-administrator) — see the bundle data.
	Components map[string]Image
	// Wiring is this version's default mapping from connection facts to component
	// configuration (env-var names, connection-string template, Secret-key names).
	Wiring WiringProfile
	// Messaging is this version's managed-RabbitMQ topology (users/exchanges/queues/
	// bindings) the managed-messaging builder loops over.
	Messaging MessagingTopology

	// HasProvisioning marks whether this platform version ships the bundled
	// provisioning-rabbitmq service. It is a versioned CAPABILITY flag: 2.18.0+ has it,
	// 2.17.0 does not (the component did not exist pre-rebrand). The reconciler skips the
	// provisioning Deployment + Core's provisioning env for a version that lacks it, even if
	// the CR sets provisioning.mode=deploy (surfaced as a non-fatal condition), so the same CR
	// is portable across versions.
	HasProvisioning bool

	// Released marks a bundle whose platform artifacts are published. Unreleased
	// (preview) bundles resolve ONLY via an explicit spec.version — they are excluded
	// from the advertised SupportedVersions(), are not eligible to be DefaultVersion,
	// and a LIVE platform cannot be upgraded onto one (see the controller's
	// preview-upgrade guard). The release-day flip PR sets Released and moves
	// DefaultVersion.
	Released bool

	// Managed-infrastructure default versions for this platform version. They are the
	// engine versions the operator provisions when the managed block leaves version
	// empty AND the upstream operator's own default is not desired. Today the managed
	// builders pass the version through only when the CR sets it (an empty version lets
	// the upstream operator pick), so these are primarily the data the upgrade guard
	// (Phase 2) reads to validate a version move — they live HERE so the guard reasons
	// over per-bundle data, not scattered Go literals.
	RabbitMQVersion string
	CNPGVersion     string
	KeycloakVersion string
}

// Lookup returns the bundle's image coordinates for a component name. The wiring
// profile and messaging topology are read directly off the exported Bundle.Wiring /
// Bundle.Messaging fields (Lookup is a method because the map read needs the ok-bool).
func (b Bundle) Lookup(name string) (Image, bool) {
	img, ok := b.Components[name]
	return img, ok
}

// bundles is the operator's full set of version-keyed bundles. Adding a supported
// platform version is a DATA edit here (a new key) — the reconciler resolves the bundle
// at runtime, so no code change is needed to carry an additional version. DefaultVersion
// must name a RELEASED bundle (TestDefaultVersionIsReleased). Every bundle is keyed by
// its own version const, never by DefaultVersion.
var bundles = map[string]Bundle{
	version2180: {
		// The map KEY is the operator's component identity (clean Service name, labels);
		// Image.Name is the PUBLISHED image repository name, which differs for several
		// components (auth, scheduler, frontend-administrator). The published coordinates
		// resolve to hub.omnitrustregistry.com/ilm/<name>:<tag> and are exercised by the
		// full-managed e2e, which pulls them. auth-opa-policies is the configured policies
		// server image (auth-opa-policies:1.4.1), distinct from the bare OPA engine sidecar (opa).
		Components: map[string]Image{
			"core":                   {Name: "core", Tag: version2180},
			"auth":                   {Name: "auth", Tag: "1.6.3"},
			componentAuthOPAPolicies: {Name: componentAuthOPAPolicies, Tag: "1.4.1"},
			ComponentProxy:           {Name: "proxy", Tag: "1.0.0"},
			"opa":                    {Name: "opa", Tag: tagOpa1100Static},
			"curl":                   {Name: "curl", Tag: tagCurl8160},
			"scheduler":              {Name: "scheduler", Tag: "1.1.0"},
			componentFeAdministrator: {Name: imageNameFeAdministrator, Tag: version2180},
			componentUtils:           {Name: imageNameUtils, Tag: "1.0.2"},
			componentAPIGateway:      {Name: "kong", Tag: "3.9.1"},
			// provisioning is the bundled provisioning-rabbitmq service, rendered natively when
			// provisioning.mode=deploy. It resolves to
			// hub.omnitrustregistry.com/ilm/provisioning-rabbitmq:<tag> like every other ILM
			// component. provisioning-rabbitmq is a 2.18.0 addition (it does not exist in 2.17.0),
			// pinned here to the 2.18.0 chart's released tag.
			"provisioning": {Name: "provisioning-rabbitmq", Tag: "1.0.0"},
			// keycloak-theme is the ilm Keycloak login theme, staged into the managed Keycloak
			// pods by an init container and selected via the realm's loginTheme=ilm (see
			// builder/platform managed_keycloak.go). It resolves to
			// hub.omnitrustregistry.com/ilm/keycloak-theme:<tag> like every other ILM component.
			// A bundle that ships this key opts managed Keycloak into the theme; a bundle without
			// it (e.g. the pre-rebrand 2.17.0) cleanly renders no theme.
			componentKeycloakTheme: {Name: componentKeycloakTheme, Tag: "0.1.4"},
		},
		Wiring:          wiring2180,
		Messaging:       messagingTopology2180,
		HasProvisioning: true, // provisioning-rabbitmq ships in 2.18.0
		Released:        true,
		// Managed-infra default engine versions for this bundle — the LATEST each upstream
		// operator supports, validated end-to-end (a managed everything-deploy migrated Core
		// 134/134 first-try, 0 restarts, with PostgreSQL 18.4, RabbitMQ 4.3.1, and Keycloak
		// 26.6.3 on CloudNativePG 1.29, RabbitMQ Cluster Operator 2.21, Keycloak Operator 26.6).
		// When the CR leaves a managed block's version empty the operator omits it so the
		// upstream operator picks its own matched default (e.g. the RabbitMQ operator ships 4.2.6),
		// so the quickstart PINS these to get the latest; these values also back the upgrade
		// guard's per-bundle reasoning and document the validated set.
		RabbitMQVersion: "4.3.1",
		CNPGVersion:     "18",
		KeycloakVersion: "26.6.3",
	},
	// 2.19.0 — PREVIEW until the operator ships it as default: resolvable only via
	// explicit spec.version, excluded from SupportedVersions, not
	// DefaultVersion-eligible, and live platforms cannot upgrade onto it
	// (preview-upgrade guard) until the flip PR sets Released. Image coordinates
	// verified against the released helm-charts 2.19.0 tag.
	//
	// COMPLETENESS: parts of this bundle are carried as version DATA that no builder reads
	// yet. The wiring's TimeQualityEnabledEnv (MESSAGING_TIME_QUALITY_ENABLED) and
	// PlatformInstanceIDEnv (PLATFORM_INSTANCE_ID), and the "time-quality-monitor" image
	// below, are recorded here so the version contract is complete and reviewable — but
	// nothing renders them. A platform pinned to 2.19.0 today therefore comes up WITHOUT the
	// time-quality integration, WITHOUT the time-quality-monitor sidecar, and WITHOUT the
	// instance-id env var. The data stays: consuming it is the remaining work, and removing
	// it would lose the verified coordinates.
	version2190: {
		Components: map[string]Image{
			"core":                   {Name: "core", Tag: version2190},
			"auth":                   {Name: "auth", Tag: "1.7.0"},
			componentAuthOPAPolicies: {Name: componentAuthOPAPolicies, Tag: "1.4.1"},
			ComponentProxy:           {Name: "proxy", Tag: "1.0.0"},
			"opa":                    {Name: "opa", Tag: tagOpa1100Static},
			"curl":                   {Name: "curl", Tag: tagCurl8160},
			"scheduler":              {Name: "scheduler", Tag: "1.1.1"},
			componentFeAdministrator: {Name: imageNameFeAdministrator, Tag: version2190},
			componentUtils:           {Name: imageNameUtils, Tag: "1.0.2"},
			componentAPIGateway:      {Name: "kong", Tag: "3.9.1"},
			"provisioning":           {Name: "provisioning-rabbitmq", Tag: "1.0.0"},
			componentKeycloakTheme:   {Name: componentKeycloakTheme, Tag: "0.1.4"},
			"time-quality-monitor":   {Name: "time-quality-monitor", Tag: "1.0.0", Repository: "ilm-private"},
		},
		Wiring:          wiring2190,
		Messaging:       messagingTopology2190,
		HasProvisioning: true,
		Released:        false,
		RabbitMQVersion: "4.3.1",
		CNPGVersion:     "18",
		KeycloakVersion: "26.6.3",
	},
	version2170: {
		// 2.17.0 is the pre-rebrand CZERTAINLY release. Its images are republished under the
		// SAME registry/repository as 2.18.0 (hub.omnitrustregistry.com/ilm) — only the tags
		// differ (core + frontend-administrator at 2.17.0, scheduler at 1.0.5), and there is NO
		// provisioning-rabbitmq component. The wiring is the 2.17.0 application contract
		// (RABBITMQ_* broker env, no provisioning/proxy/broker-vhost env) and the managed
		// messaging topology is a single broker user (2.17.0 used one credential, not the
		// five-user 2.18.0 split).
		Components: map[string]Image{
			"core":                   {Name: "core", Tag: version2170},
			"auth":                   {Name: "auth", Tag: "1.6.3"},
			componentAuthOPAPolicies: {Name: componentAuthOPAPolicies, Tag: "1.4.1"},
			"opa":                    {Name: "opa", Tag: tagOpa1100Static},
			"curl":                   {Name: "curl", Tag: tagCurl8160},
			"scheduler":              {Name: "scheduler", Tag: "1.0.5"},
			componentFeAdministrator: {Name: imageNameFeAdministrator, Tag: version2170},
			componentUtils:           {Name: imageNameUtils, Tag: "1.0.2"},
			componentAPIGateway:      {Name: "kong", Tag: "3.9.1"},
			// No provisioning component in 2.17.0 (it arrived in 2.18.0).
		},
		Wiring:          wiring2170,
		Messaging:       messagingTopology2170,
		HasProvisioning: false,
		Released:        true,
		// Managed-infra engine versions for 2.17.0 — pinned to what THIS platform version
		// (pre-rebrand core:2.17.0) was validated against. They are deliberately NOT bumped to
		// 2.18.0's newer baseline (18/4.3.1/26.6.3): core 2.17.0 was not validated on those, and
		// a given platform bundle should reproduce its tested engine set. A 2.17.0→2.18.0 platform
		// upgrade therefore advances the managed-infra baseline (16→18 etc.); that is a MAJOR
		// managed-infra bump, which the upgrade guard gates behind UpgradeAcknowledged (it is not
		// silent), so the operator performs it only when the user opts in.
		RabbitMQVersion: "4.2.0",
		CNPGVersion:     "16",
		KeycloakVersion: "26.4.0",
	},
}

// BundleFor returns the bundle for a platform version. An empty version selects the
// DefaultVersion bundle (the operator's newest RELEASED bundle — a preview is reachable
// only by naming it explicitly). ok is false for an unknown version —
// the caller (the reconciler) degrades with an actionable supported-versions message
// rather than rendering against a non-existent bundle. The supported set grows over
// time, so this is a RUNTIME check, not a frozen CEL enum.
func BundleFor(version string) (Bundle, bool) {
	if version == "" {
		version = DefaultVersion
	}
	b, ok := bundles[version]
	return b, ok
}

// SupportedVersions returns the RELEASED platform versions this operator ships,
// semver-ascending — the advertised set used in the unknown-version degraded message
// and by the CLI. Preview bundles (Released=false) are excluded: they resolve only via
// an explicit spec.version (see AllVersions for the full key set). Derived from the
// bundle data so the advertised set can never list a version BundleFor rejects.
func SupportedVersions() []string {
	out := make([]string, 0, len(bundles))
	for v, b := range bundles {
		if b.Released {
			out = append(out, v)
		}
	}
	return sortVersions(out)
}

// AllVersions returns every bundle key (released and preview), semver-ascending.
func AllVersions() []string {
	out := make([]string, 0, len(bundles))
	for v := range bundles {
		out = append(out, v)
	}
	return sortVersions(out)
}

// sortVersions sorts a version list ASCENDING by semver IN PLACE and returns it. It is the
// single ordering used by SupportedVersions and AllVersions, so the advertised set and the
// full key set can never disagree on "newest is last" — the invariant the DefaultVersion
// check and the CLI both rely on. Ordering is semver, never lexicographic (2.9.0 < 2.10.0).
func sortVersions(out []string) []string {
	sort.Slice(out, func(i, j int) bool { return semverLess(out[i], out[j]) })
	return out
}

// semverLess orders two version strings numerically per segment via the module's
// existing semver dependency — 2.9.0 < 2.10.0, which lexicographic sorting gets
// wrong. Unparsable keys (never expected; bundle keys are controlled X.Y.Z data)
// fall back to string ordering deterministically.
func semverLess(a, b string) bool {
	va, errA := semver.NewVersion(a)
	vb, errB := semver.NewVersion(b)
	if errA != nil || errB != nil {
		return a < b
	}
	return va.LessThan(vb)
}

// defaultBundle returns the DefaultVersion bundle (which always exists). It backs the
// package-level Lookup/Wiring/Messaging wrappers used by callers that do not select a
// version (e.g. the version-agnostic Connector path, and call sites that predate
// spec.version), so their behaviour is unchanged.
func defaultBundle() Bundle {
	b, ok := bundles[DefaultVersion]
	if !ok { // unreachable: DefaultVersion is always a key
		panic("bom: DefaultVersion " + DefaultVersion + " has no bundle")
	}
	return b
}

// Lookup returns the DefaultVersion bundle's image coordinates for a component name.
// Prefer Bundle.LookupImage on a version-resolved bundle; this wrapper resolves the
// default version for version-agnostic callers.
func Lookup(name string) (Image, bool) { return defaultBundle().Lookup(name) }

// CredentialEnv binds the default Secret keys (username/password) to the
// env-var names that receive them. All four are versioned DATA; the CR can
// override them via the component's env/secretRefs.
type CredentialEnv struct {
	UsernameKey string
	UsernameEnv string
	PasswordKey string
	PasswordEnv string
}

// WiringProfile is the platform version's DEFAULT mapping from connection facts
// to component configuration. It lives in the bundle as versioned data — never as
// literals in the reconciler — and the CR's per-component env/secretRefs take
// precedence over it.
type WiringProfile struct {
	DatabaseURLEnv      string // env var that receives the rendered connection URL
	DatabaseURLTemplate string // rendered with {host} {port} {name}
	DatabaseURLQuery    string // query suffix appended to the rendered URL (e.g. "?characterEncoding=UTF-8")
	DatabaseCred        CredentialEnv

	// Messaging broker coordinates: the host/port are non-secret and are sourced
	// from a ConfigMap (see MessagingConfigMap*); the credentials are secret-backed.
	MessagingHostEnv  string
	MessagingPortEnv  string
	MessagingVHostEnv string
	MessagingCred     CredentialEnv
	// MessagingConfigMapName is the ConfigMap holding the broker host/port.
	MessagingConfigMapName string
	// MessagingHostKey / MessagingPortKey are the keys in that ConfigMap.
	MessagingHostKey string
	MessagingPortKey string

	// Intra-platform service base URLs (Core talks to these by their in-cluster
	// Service names — the operator uses clean, unsuffixed names).
	OPABaseURLEnv   string
	OPABaseURL      string
	AuthURLEnv      string
	AuthURL         string
	SchedulerURLEnv string
	SchedulerURL    string

	// Header (client-certificate) forwarding wiring.
	HeaderEnabledEnv string
	HeaderNameEnv    string
	HeaderNameValue  string // default header name when the CR leaves it empty

	// Logging / proxy / provisioning env-var names. JVM tuning (JAVA_OPTS) is not wired
	// here: it is a plain per-component env override the user sets directly on
	// spec.<component>.env, so the operator injects none of its own.
	LoggingLevelEnv    string
	ProxyEnabledEnv    string
	ProxyInstanceIDEnv string
	HTTPProxyEnv       string
	HTTPSProxyEnv      string
	NoProxyEnv         string
	ProvisioningURLEnv string
	// TimeQualityEnabledEnv is the env var toggling Core's time-quality messaging
	// integration (2.19.0+; empty in earlier bundles renders nothing).
	TimeQualityEnabledEnv string
	// PlatformInstanceIDEnv is the env var carrying Core's certificate-serial
	// instance id (2.19.0+; empty in earlier bundles renders nothing).
	PlatformInstanceIDEnv string

	// Trusted-certificates and provisioning-API-key Secret keys (env names + the
	// in-Secret keys). Values are always secret-backed, never inlined.
	TrustedCertificates SecretKeyEnv
	ProvisioningAPIKey  SecretKeyEnv

	// Provisioning is the wiring for the bundled provisioning-rabbitmq service rendered
	// when core.provisioning.mode=deploy (its env-var names, the bootstrap exchange/queue
	// defaults, and the in-Secret key defaults). It is versioned DATA so a provisioning
	// env/config rename on a platform upgrade is a new bundle's wiring, not a code change.
	Provisioning ProvisioningWiring

	// AdminCert binds the admin client-certificate Secret's cert key to Core's
	// ADMIN_CERT env (the optional first-admin bootstrap). The value is always
	// secret-backed (the admin cert Secret's tls.crt), never inlined. The Secret name
	// itself comes from spec.registerAdmin (the caller's SecretRef, or the generated
	// admin-certificate-secret) — only the env var name and the in-Secret key live here.
	// AdminPrivateKeyKey is the DEFAULT key the admin client private key lives under
	// (tls.key); it is the single source of that default for the spec's optional
	// registerAdmin.privateKeyKey override (source=provided only).
	AdminCert          SecretKeyEnv
	AdminPrivateKeyKey string

	// auth (.NET) wiring. The operator composes the .NET connection string
	// (see AuthDBConnectionString) and stores it in the AuthDBSecretName Secret under
	// AuthDBSecretKey; auth reads it via secretKeyRef into AuthDBConnEnv.
	AuthCreateUsersEnv string // AUTH_CREATE_UNKNOWN_USERS
	AuthCreateRolesEnv string // AUTH_CREATE_UNKNOWN_ROLES
	AuthSyncPolicyEnv  string // SYNC_POLICY
	AuthAspNetURLsEnv  string // ASPNETCORE_URLS
	AuthAspNetURLs     string // ASPNETCORE_URLS value (binds Kestrel to the HTTP port)
	AuthDBConnEnv      string // AUTH_DB_CONNECTION_STRING (target env var)
	AuthDBSecretName   string // operator-managed Secret holding the composed connection string
	AuthDBSecretKey    string // key within that Secret
}

// AuthDBConnectionString composes the .NET / Npgsql connection string auth
// expects:
//
//	Host=<host>;Port=<port>;Username=<u>;Password=<pw>;Database=<name>;Pooling=true
//
// host/port are passed in already pgBouncer-resolved by the caller (the operator's
// external database.host/port point at whatever the platform fronts the DB with,
// pgBouncer included). The returned value is sensitive and must live ONLY inside the
// operator-managed Secret — never in status, conditions, events, or logs.
func (w WiringProfile) AuthDBConnectionString(host string, port int32, name, username, password string) string {
	return "Host=" + host +
		";Port=" + strconv.Itoa(int(port)) +
		";Username=" + username +
		";Password=" + password +
		";Database=" + name +
		";Pooling=true"
}

// SecretKeyEnv binds a single in-Secret key to the env-var that receives it.
type SecretKeyEnv struct {
	Env string // target env var name
	Key string // key within the referenced Secret
}

// ProvisioningWiring is the platform version's wiring for the bundled provisioning-rabbitmq
// service (rendered when core.provisioning.mode=deploy). It holds the service's env
// contract as versioned DATA: the env-var names, the bootstrap exchange/response-queue
// defaults, and the default in-Secret keys for the bootstrap (API key + JWT signing key).
// No secret VALUE is ever held here — only env-var names and in-Secret KEY names.
//
// The env-var names and the bootstrap secret keys (securityApiKey/tokenSigningKey) are the
// provisioning-rabbitmq image's documented configuration contract.
type ProvisioningWiring struct {
	// Service env: the listening port and log level env-var names (the service is a Spring
	// Boot app like Core, so it shares those contracts). JVM tuning is a plain per-component
	// env override, so no JAVA_OPTS name is wired here.
	PortEnv         string // PORT
	LoggingLevelEnv string // LOGGING_LEVEL_COM_CZERTAINLY

	// Broker (admin/provisioner) connection env: the service authenticates as the
	// provisioner user to manage queues/exchanges. Host/port are non-secret (sourced from
	// the messaging ConfigMap); username/password are secret-backed.
	BrokerHostEnv  string // RABBITMQ_HOST
	BrokerPortEnv  string // RABBITMQ_PORT
	BrokerVHostEnv string // RABBITMQ_VIRTUAL_HOST
	BrokerCred     CredentialEnv

	// Proxy connection env: the AMQP URL proxies use, and the proxy user's credentials the
	// service embeds into the per-proxy JWT tokens it issues. Credentials are secret-backed.
	ProxyAMQPURLEnv  string // PROXY_AMQP_URL
	ProxyCred        CredentialEnv
	ProxyExchangeEnv string // PROXY_EXCHANGE
	ResponseQueueEnv string // PROXY_RESPONSE_QUEUE

	// Bootstrap security env: the X-API-Key feature toggle, the API key, and the JWT
	// signing key. The API key and signing key are secret-backed (the deploy
	// bootstrapSecretRef); only the toggle is inline.
	SecurityEnabledEnv string       // SECURITY_API_KEY_ENABLED
	APIKey             SecretKeyEnv // SECURITY_API_KEY + default in-Secret key
	TokenSigningKey    SecretKeyEnv // TOKEN_SIGNING_KEY + default in-Secret key

	// Bootstrap topology defaults: the proxy exchange the service binds per-proxy queues
	// to, and the response queue for proxy replies.
	DefaultExchange      string // czertainly-proxy
	DefaultResponseQueue string // core
}

// DatabaseURL renders DatabaseURLTemplate with the given connection facts and
// appends the profile's query suffix (e.g. "?characterEncoding=UTF-8").
func (w WiringProfile) DatabaseURL(host string, port int32, name string) string {
	return strings.NewReplacer(
		"{host}", host,
		"{port}", strconv.Itoa(int(port)),
		"{name}", name,
	).Replace(w.DatabaseURLTemplate) + w.DatabaseURLQuery
}

// wiring2180 is the wiring profile for platform version 2.18.0. Renaming an env var on a
// platform upgrade is a change in a NEW bundle's wiring (data), not in the reconciler;
// older bundles keep their own names, so a multi-version operator wires each version
// correctly.
var wiring2180 = WiringProfile{
	DatabaseURLEnv:      "JDBC_URL",
	DatabaseURLTemplate: "jdbc:postgresql://{host}:{port}/{name}",
	DatabaseURLQuery:    "?characterEncoding=UTF-8",
	DatabaseCred: CredentialEnv{ //nolint:gosec // G101: Kubernetes Secret key names, not credential values
		UsernameKey: "username", UsernameEnv: "JDBC_USERNAME",
		PasswordKey: "password", PasswordEnv: "JDBC_PASSWORD",
	},

	MessagingHostEnv:       "BROKER_HOST",
	MessagingPortEnv:       "BROKER_PORT",
	MessagingVHostEnv:      "BROKER_VIRTUAL_HOST",
	MessagingConfigMapName: "messaging-configmap",
	MessagingHostKey:       "messaging.host",
	MessagingPortKey:       "messaging.amqp.port",
	MessagingCred: CredentialEnv{ //nolint:gosec // G101: Kubernetes Secret key names, not credential values
		UsernameKey: "username", UsernameEnv: "BROKER_USERNAME",
		PasswordKey: "password", PasswordEnv: "BROKER_PASSWORD",
	},

	// Intra-platform service URLs. Core reaches OPA on its own pod (localhost:8181);
	// auth and scheduler are reached by their clean Service names.
	OPABaseURLEnv:   "OPA_BASE_URL",
	OPABaseURL:      "http://localhost:8181",
	AuthURLEnv:      "AUTH_SERVICE_BASE_URL",
	AuthURL:         "http://auth:8080",
	SchedulerURLEnv: "SCHEDULER_BASE_URL",
	SchedulerURL:    "http://scheduler:8080",

	HeaderEnabledEnv: "HEADER_ENABLED",
	HeaderNameEnv:    "HEADER_NAME",
	HeaderNameValue:  "ssl-client-cert",

	LoggingLevelEnv:    "LOGGING_LEVEL_COM_CZERTAINLY",
	ProxyEnabledEnv:    "PROXY_ENABLED",
	ProxyInstanceIDEnv: "PROXY_INSTANCE_ID",
	HTTPProxyEnv:       "HTTP_PROXY",
	HTTPSProxyEnv:      "HTTPS_PROXY",
	NoProxyEnv:         "NO_PROXY",
	ProvisioningURLEnv: "PROVISIONING_API_URL",

	TrustedCertificates: SecretKeyEnv{Env: "TRUSTED_CERTIFICATES", Key: "ca.crt"},
	ProvisioningAPIKey:  SecretKeyEnv{Env: "PROVISIONING_API_KEY", Key: "provisioningApiKey"},

	// Bundled provisioning-rabbitmq service wiring (core.provisioning.mode=deploy). The
	// env-var names + bootstrap secret keys are the provisioning-rabbitmq image's
	// configuration contract.
	Provisioning: ProvisioningWiring{
		PortEnv:         "PORT",
		LoggingLevelEnv: "LOGGING_LEVEL_COM_CZERTAINLY",

		BrokerHostEnv:  "RABBITMQ_HOST",
		BrokerPortEnv:  "RABBITMQ_PORT",
		BrokerVHostEnv: "RABBITMQ_VIRTUAL_HOST",
		BrokerCred: CredentialEnv{ //nolint:gosec // G101: Kubernetes Secret key names, not credential values
			UsernameKey: "username", UsernameEnv: "RABBITMQ_USERNAME",
			PasswordKey: "password", PasswordEnv: "RABBITMQ_PASSWORD",
		},

		ProxyAMQPURLEnv: "PROXY_AMQP_URL",
		ProxyCred: CredentialEnv{ //nolint:gosec // G101: Kubernetes Secret key names, not credential values
			UsernameKey: "username", UsernameEnv: "PROXY_RABBITMQ_USERNAME",
			PasswordKey: "password", PasswordEnv: "PROXY_RABBITMQ_PASSWORD",
		},
		ProxyExchangeEnv: "PROXY_EXCHANGE",
		ResponseQueueEnv: "PROXY_RESPONSE_QUEUE",

		SecurityEnabledEnv: "SECURITY_API_KEY_ENABLED",
		// The deploy bootstrap Secret carries the API key under "securityApiKey" and the JWT
		// signing key under "tokenSigningKey".
		APIKey:          SecretKeyEnv{Env: "SECURITY_API_KEY", Key: "securityApiKey"},
		TokenSigningKey: SecretKeyEnv{Env: "TOKEN_SIGNING_KEY", Key: "tokenSigningKey"},

		DefaultExchange:      exchangeCzertainlyProxy,
		DefaultResponseQueue: "core",
	},
	// ADMIN_CERT is sourced from the admin client-certificate Secret's tls.crt key, so a
	// generated (cert-manager tls.crt) or provided (kubernetes.io/tls) admin cert Secret
	// resolves identically.
	AdminCert:          SecretKeyEnv{Env: "ADMIN_CERT", Key: "tls.crt"},
	AdminPrivateKeyKey: "tls.key",

	AuthCreateUsersEnv: "AUTH_CREATE_UNKNOWN_USERS",
	AuthCreateRolesEnv: "AUTH_CREATE_UNKNOWN_ROLES",
	AuthSyncPolicyEnv:  "SYNC_POLICY",
	AuthAspNetURLsEnv:  "ASPNETCORE_URLS",
	AuthAspNetURLs:     "http://+:8080",
	AuthDBConnEnv:      "AUTH_DB_CONNECTION_STRING",
	AuthDBSecretName:   "auth-db",
	AuthDBSecretKey:    "connection-string",
}

// wiring2170 is the wiring profile for platform version 2.17.0 (the pre-rebrand CZERTAINLY
// release). It is wiring2180 with the 2.17.0 broker contract — RABBITMQ_* env (2.18.0 renamed
// these to BROKER_*), no broker-vhost env, and no provisioning/proxy env (those features
// arrived in 2.18.0). The builder OMITS any env whose wiring name is empty, so clearing those
// names version-gates them off; deriving from wiring2180 keeps the large shared surface
// (DB/auth/OPA/header/logging/proxy-passthrough/trusted-cert/admin) defined in exactly one place.
var wiring2170 = func() WiringProfile {
	w := wiring2180 // value copy (WiringProfile holds only value-type fields)
	w.MessagingHostEnv = "RABBITMQ_HOST"
	w.MessagingPortEnv = "RABBITMQ_PORT"
	w.MessagingVHostEnv = ""         // 2.17.0 Core has no broker-vhost env → omitted by the empty-name skip
	w.MessagingCred = CredentialEnv{ //nolint:gosec // G101: Kubernetes Secret key names, not credential values
		UsernameKey: "username", UsernameEnv: "RABBITMQ_USERNAME",
		PasswordKey: "password", PasswordEnv: "RABBITMQ_PASSWORD",
	}
	// 2.17.0 has no bundled provisioning service and no native proxy wiring on Core.
	w.ProvisioningURLEnv = ""             // no PROVISIONING_API_URL
	w.ProvisioningAPIKey = SecretKeyEnv{} // no PROVISIONING_API_KEY
	w.ProxyEnabledEnv = ""                // no PROXY_ENABLED
	w.ProxyInstanceIDEnv = ""             // no PROXY_INSTANCE_ID
	w.Provisioning = ProvisioningWiring{} // no provisioning-rabbitmq service in 2.17.0
	return w
}()

// wiring2190 is the 2.19.0 wiring: wiring2180 with the LOGGING_LEVEL rename (core,
// scheduler, AND the provisioning service at the released 2.19.0 chart tag), the two
// new 2.19.0 env vars, and the provisioning proxy-exchange rename. Plain value-copy
// is a fully independent copy: WiringProfile and its nested structs contain only
// value fields (no maps/slices).
var wiring2190 = func() WiringProfile {
	w := wiring2180
	w.LoggingLevelEnv = "LOGGING_LEVEL_COM_OTILM"
	w.TimeQualityEnabledEnv = "MESSAGING_TIME_QUALITY_ENABLED"
	w.PlatformInstanceIDEnv = "PLATFORM_INSTANCE_ID"
	w.Provisioning.DefaultExchange = exchangeIlmProxy
	w.Provisioning.LoggingLevelEnv = "LOGGING_LEVEL_COM_OTILM"
	return w
}()

// Wiring returns the DefaultVersion bundle's wiring profile. Prefer
// Bundle.WiringProfileData on a version-resolved bundle; this wrapper resolves the
// default version for version-agnostic callers.
func Wiring() WiringProfile { return defaultBundle().Wiring }

// --- Managed RabbitMQ messaging topology (versioned data) --------------------
//
// The messaging topology below is the operator's data-as-code definition of the platform's
// RabbitMQ users, exchanges, queues, and bindings. Keeping it in the BUNDLE — as versioned
// data the managed-messaging builder loops over — means a platform-version topology change
// (a new queue, a tweaked permission regex) is a DATA edit in a new bundle, not scattered Go
// literals in the builder. The user/permission/exchange/queue/binding definitions are
// app-level names tied to the application's vhost/naming (czertainly), NOT the operator's
// clean Service names.

// MessagingUserRole identifies one of the four platform broker users by its operator-
// stable role. The role is the User CR's name suffix and the key under which the
// builder/readback look up the user's generated credentials Secret, so it is a fixed
// contract (a protected override surface).
type MessagingUserRole string

// The four platform messaging user roles.
const (
	// MessagingUserAdministrator is the full-access administrator user (admin tags).
	MessagingUserAdministrator MessagingUserRole = "administrator"
	// MessagingUserProvisioner is the provisioner user (admin tags), used by the remote-
	// proxy provisioning flow.
	MessagingUserProvisioner MessagingUserRole = "provisioner"
	// MessagingUserProxy is the proxy user (no tags), scoped to the czertainly-proxy
	// exchange and proxy.* routing.
	MessagingUserProxy MessagingUserRole = "proxy"
	// MessagingUserCore is the Core user (no tags), scoped to the czertainly exchanges and
	// core.* / core-* routing. The platform's Core and scheduler authenticate as
	// this user.
	MessagingUserCore MessagingUserRole = "core"
	// MessagingUserMonitor is the time-quality-monitor user (no tags): it publishes on the
	// czertainly exchange and consumes the time-quality.config queue. The time-quality monitor is
	// an EXTERNAL/optional component — the platform declares this user + the time-quality topology
	// (so Core can publish/consume), but does NOT deploy the monitor itself. Provisioning the user
	// (+ its generated Secret) is for parity with the chart, so an external monitor can connect.
	MessagingUserMonitor MessagingUserRole = "monitor"
)

// MessagingUser is one broker user and its vhost permissions, as versioned data. NO
// password is held here — the Messaging Topology Operator GENERATES the per-user
// credentials Secret; the operator reads it back by reference.
type MessagingUser struct {
	// Role is the operator-stable user role (the User CR name suffix + Secret lookup key).
	Role MessagingUserRole
	// Tags are the RabbitMQ user tags (e.g. "administrator"); empty for a plain user.
	Tags []string
	// Configure/Write/Read are the user's vhost permission regexes. An empty Configure
	// means "no configure permission".
	Configure string
	Write     string
	Read      string
}

// Exchange types the topologies declare. The platform's direct exchange carries the
// components' own traffic (one routing key per static queue); the TOPIC exchange is the
// proxy exchange, the one whose bindings are created at runtime as remote proxies enrol —
// which is why a consumer of this data identifies the proxy exchange by TYPE rather than by
// a version-specific name (2.19.0 renamed both).
const (
	ExchangeTypeDirect = "direct"
	ExchangeTypeTopic  = "topic"
)

// MessagingExchange is one exchange in the topology.
type MessagingExchange struct {
	// Name is the exchange name (an app-level name, e.g. "czertainly").
	Name string
	// Type is the exchange type (ExchangeTypeDirect / ExchangeTypeTopic).
	Type string
	// Durable marks the exchange durable.
	Durable bool
}

// MessagingQueue is one queue in the topology.
type MessagingQueue struct {
	// Name is the queue name (an app-level name, e.g. "core.audit-logs").
	Name string
	// Durable marks the queue durable.
	Durable bool
	// Arguments are optional RabbitMQ x-arguments for the queue (e.g.
	// {"x-max-length": int64(1), "x-overflow": "drop-head"}). nil for a plain queue; rendered
	// into the Queue CR's spec.arguments only when non-empty. Integer values MUST be int64 (the
	// unstructured render rejects a plain int).
	Arguments map[string]interface{}
}

// Queue x-argument names the topologies declare.
const (
	// queueArgMaxLength bounds how many messages the queue retains.
	queueArgMaxLength = "x-max-length"
	// queueArgOverflow selects what the broker does once that bound is reached.
	queueArgOverflow = "x-overflow"
)

// IsLatestOnlyRetention reports whether the queue is declared as a LATEST-ONLY RETENTION
// queue: one bounded to a single message, whose publisher keeps refreshing it so consumers
// can read the current value at any time.
//
// Such a queue is DESIGNED to be non-empty in steady state, which makes it the one class a
// drain must not wait on — a migration that required it to empty would never finish. The
// answer is derived from the declared arguments rather than from a list of names, so a queue
// added to a future bundle with the same retention shape inherits the rule automatically.
func (q MessagingQueue) IsLatestOnlyRetention() bool {
	limit, declared := q.Arguments[queueArgMaxLength].(int64)
	return declared && limit == 1
}

// MessagingBinding is one exchange→queue binding in the topology.
type MessagingBinding struct {
	// Source is the source exchange name.
	Source string
	// Destination is the destination queue name.
	Destination string
	// RoutingKey is the binding's routing key.
	RoutingKey string
}

// MessagingTopology is the full set of users, exchanges, queues, and bindings the
// operator provisions on a managed RabbitMQ vhost, as versioned data. The default vhost
// name IS held here (DefaultVirtualHost); spec.messaging.virtualHost overrides it per
// platform.
type MessagingTopology struct {
	// DefaultVirtualHost is the broker vhost this version's platform provisions and
	// connects to when spec.messaging.virtualHost is empty. It is per-bundle DATA
	// (2.18.0: "czertainly"; 2.19.0: "/"); spec.messaging.virtualHost overrides it.
	DefaultVirtualHost string
	// Users are the platform broker users + their vhost permission regexes.
	Users []MessagingUser
	// Exchanges are the platform's exchanges (per bundle: one direct + one proxy topic
	// exchange, whose names the bundle carries — 2.19.0 renamed both).
	Exchanges []MessagingExchange
	// Queues are the platform's queues (the core.* set + the bare "core" queue, plus the
	// time-quality.* monitor queues from 2.18.0).
	Queues []MessagingQueue
	// Bindings are the bundle's direct-exchange→queue bindings (one per queue's routing key).
	Bindings []MessagingBinding
}

// HasUserRole reports whether the topology provisions a broker user in the given role. It is
// the data-level question behind two version-agnostic behaviours: which generated credentials
// Secret a component may be pointed at, and whether a version's topology carries the dedicated
// administrator user the management-API paths authenticate as.
func (t MessagingTopology) HasUserRole(role MessagingUserRole) bool {
	for _, u := range t.Users {
		if u.Role == role {
			return true
		}
	}
	return false
}

// LegacyUnscopedVirtualHost is the pre-2.19 messaging vhost name, and the ONE vhost whose
// managed-topology object names are rendered UNSCOPED. Live 2.17.0/2.18.0 platforms carry
// those unscoped names in their clusters, so the operator must keep composing them
// byte-identically or every applied rabbitmq.com CR would be re-identified and orphaned
// (those kinds are deliberately excluded from pruning). Every other vhost — including the
// 2.19.0 "/" — gets a vhost-derived scope, which is what lets a source and a target
// topology coexist during a migration.
const LegacyUnscopedVirtualHost = "czertainly"

// DefaultVirtualHost is the pre-2.19 messaging vhost name.
//
// Deprecated: use LegacyUnscopedVirtualHost, or read the per-bundle
// MessagingTopology.DefaultVirtualHost; this alias remains for module compatibility.
const DefaultVirtualHost = LegacyUnscopedVirtualHost

// Exchange names used by the topology (app-level names, tied to the application naming).
const (
	exchangeCzertainly      = "czertainly"
	exchangeCzertainlyProxy = "czertainly-proxy"
)

// Queue names used by the topologies (app-level names, tied to the application
// naming). Each is referenced both as a queue Name and as a binding Destination
// (and, for the time-quality queues, as a routing key), so they are named once here.
const (
	queueCoreAuditLogs     = "core.audit-logs"
	queueCoreNotifications = "core.notifications"
	queueCoreScheduler     = "core.scheduler"
	queueCoreActions       = "core.actions"
	queueCoreValidation    = "core.validation"
	queueCoreEvents        = "core.events"

	queueTimeQualityConfig        = "time-quality.config"
	queueTimeQualityConfigRequest = "time-quality.config-request"
	queueTimeQualityResults       = "time-quality.results"
)

// 2.19.0 renamed the exchanges (czertainly→ilm, czertainly-proxy→ilm-proxy) and moved
// the default vhost to "/" — helm-charts commit 6aa78a4. provider.status-poll is the
// 2.19.0-new provider status queue — helm-charts commit 55afa9b.
const (
	exchangeIlm      = "ilm"
	exchangeIlmProxy = "ilm-proxy"

	queueProviderStatusPoll = "provider.status-poll"
)

// Routing keys used by the czertainly/ilm-exchange→queue bindings (app-level publish
// keys). They are unchanged by the 2.19.0 exchange rename and repeat once per topology
// below, so they are named once here.
const (
	routingKeyAuditLogs    = "audit-logs"
	routingKeyNotification = "notification"
	routingKeyAction       = "action"
	routingKeyScheduler    = "scheduler"
	routingKeyValidation   = "validation"
	routingKeyEvent        = "event"
)

// latestOnlyQueueArguments returns the RabbitMQ x-arguments for a "keep only the
// latest message" queue (x-max-length 1, drop-head overflow) — the time-quality.config
// and time-quality.config-request queues use this in every topology that carries them.
// It returns a FRESH map on every call: MessagingQueue.Arguments is a reference type,
// and map literals must never be shared between MessagingTopology values.
func latestOnlyQueueArguments() map[string]interface{} {
	return map[string]interface{}{queueArgMaxLength: int64(1), queueArgOverflow: "drop-head"}
}

// messagingTopology2180 is the platform's RabbitMQ topology for platform version 2.18.0:
// the user permission regexes, exchange/queue/binding names, and routing keys the platform
// requires on its messaging vhost.
var messagingTopology2180 = MessagingTopology{
	DefaultVirtualHost: LegacyUnscopedVirtualHost,
	Users: []MessagingUser{
		// administrator + provisioner have admin tags and full ".*" permissions.
		{Role: MessagingUserAdministrator, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		{Role: MessagingUserProvisioner, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		// proxy: no configure; write only to czertainly-proxy; read only proxy.* queues.
		{Role: MessagingUserProxy, Tags: nil, Configure: "", Write: "^czertainly-proxy$", Read: `^proxy\..*$`},
		// core: no configure; write czertainly or czertainly-proxy; read core.* / core-* AND the
		// time-quality monitor's request/result queues. Core (2.18.0) consumes the monitor's
		// config-request and results queues at startup — without this read grant the broker denies
		// access and Core crash-loops ("read access ... refused").
		{Role: MessagingUserCore, Tags: nil, Configure: "", Write: "^czertainly(-proxy)?$", Read: `^core(\..+|-.+)?$|^time-quality\.(config-request|results)$`},
		// monitor (time-quality, new in 2.18.0): publish on the czertainly exchange; consume the
		// time-quality.config queue. Provisioned for an external monitor; not deployed here.
		{Role: MessagingUserMonitor, Tags: nil, Configure: "", Write: "^czertainly$", Read: `^time-quality\.config$`},
	},
	Exchanges: []MessagingExchange{
		{Name: exchangeCzertainly, Type: ExchangeTypeDirect, Durable: true},
		{Name: exchangeCzertainlyProxy, Type: ExchangeTypeTopic, Durable: true},
	},
	Queues: []MessagingQueue{
		{Name: "core", Durable: true},
		{Name: queueCoreAuditLogs, Durable: true},
		{Name: queueCoreNotifications, Durable: true},
		{Name: queueCoreScheduler, Durable: true},
		{Name: queueCoreActions, Durable: true},
		{Name: queueCoreValidation, Durable: true},
		{Name: queueCoreEvents, Durable: true},
		// time-quality monitor queues (new in 2.18.0). config + config-request keep only the
		// latest message (x-max-length 1, drop-head); results is a plain queue. Core publishes its
		// config snapshot here at startup and consumes config-request/results.
		{Name: queueTimeQualityConfig, Durable: true, Arguments: latestOnlyQueueArguments()},
		{Name: queueTimeQualityConfigRequest, Durable: true, Arguments: latestOnlyQueueArguments()},
		{Name: queueTimeQualityResults, Durable: true},
	},
	// Bindings from the czertainly direct exchange to each queue (routing key = the app's publish
	// key). The time-quality.* bindings (routing key == queue name) are new in 2.18.0.
	Bindings: []MessagingBinding{
		{Source: exchangeCzertainly, Destination: queueCoreAuditLogs, RoutingKey: routingKeyAuditLogs},
		{Source: exchangeCzertainly, Destination: queueCoreNotifications, RoutingKey: routingKeyNotification},
		{Source: exchangeCzertainly, Destination: queueCoreActions, RoutingKey: routingKeyAction},
		{Source: exchangeCzertainly, Destination: queueCoreScheduler, RoutingKey: routingKeyScheduler},
		{Source: exchangeCzertainly, Destination: queueCoreValidation, RoutingKey: routingKeyValidation},
		{Source: exchangeCzertainly, Destination: queueCoreEvents, RoutingKey: routingKeyEvent},
		{Source: exchangeCzertainly, Destination: queueTimeQualityConfig, RoutingKey: queueTimeQualityConfig},
		{Source: exchangeCzertainly, Destination: queueTimeQualityConfigRequest, RoutingKey: queueTimeQualityConfigRequest},
		{Source: exchangeCzertainly, Destination: queueTimeQualityResults, RoutingKey: queueTimeQualityResults},
	},
}

// messagingTopology2190 is the 2.19.0 topology: vhost "/", ilm/ilm-proxy exchanges,
// the 2.18.0 queue set plus provider.status-poll, and the user permission regexes
// retargeted to the renamed exchanges (core additionally reads provider.status-poll).
var messagingTopology2190 = MessagingTopology{
	DefaultVirtualHost: "/",
	Users: []MessagingUser{
		// administrator + provisioner have admin tags and full ".*" permissions.
		{Role: MessagingUserAdministrator, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		{Role: MessagingUserProvisioner, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
		// proxy: no configure; write only to ilm-proxy; read only proxy.* queues.
		{Role: MessagingUserProxy, Tags: nil, Configure: "", Write: "^ilm-proxy$", Read: `^proxy\..*$`},
		// core: no configure; write ilm or ilm-proxy; read core.* / core-* AND the 2.19.0-new
		// provider.status-poll queue AND the time-quality monitor's request/result queues. Core
		// consumes the monitor's config-request and results queues at startup — without this read
		// grant the broker denies access and Core crash-loops ("read access ... refused").
		{Role: MessagingUserCore, Tags: nil, Configure: "", Write: "^ilm(-proxy)?$", Read: `^core(\..+|-.+)?$|^provider\.status-poll$|^time-quality\.(config-request|results)$`},
		// monitor (time-quality): publish on the ilm exchange; consume the time-quality.config
		// queue. Provisioned for an external monitor; not deployed here.
		{Role: MessagingUserMonitor, Tags: nil, Configure: "", Write: "^ilm$", Read: `^time-quality\.config$`},
	},
	Exchanges: []MessagingExchange{
		{Name: exchangeIlm, Type: ExchangeTypeDirect, Durable: true},
		{Name: exchangeIlmProxy, Type: ExchangeTypeTopic, Durable: true},
	},
	Queues: []MessagingQueue{
		{Name: "core", Durable: true},
		{Name: queueCoreAuditLogs, Durable: true},
		{Name: queueCoreNotifications, Durable: true},
		{Name: queueCoreScheduler, Durable: true},
		{Name: queueCoreActions, Durable: true},
		{Name: queueCoreValidation, Durable: true},
		{Name: queueCoreEvents, Durable: true},
		// provider.status-poll is the 2.19.0-new provider status queue.
		{Name: queueProviderStatusPoll, Durable: true},
		// time-quality monitor queues (carried over from 2.18.0). config + config-request keep
		// only the latest message (x-max-length 1, drop-head); results is a plain queue. Core
		// publishes its config snapshot here at startup and consumes config-request/results.
		{Name: queueTimeQualityConfig, Durable: true, Arguments: latestOnlyQueueArguments()},
		{Name: queueTimeQualityConfigRequest, Durable: true, Arguments: latestOnlyQueueArguments()},
		{Name: queueTimeQualityResults, Durable: true},
	},
	// Bindings from the renamed ilm direct exchange to each queue (routing key = the app's
	// publish key). The provider.status-poll binding is new in 2.19.0; the rest carry over from
	// 2.18.0 retargeted to the ilm exchange.
	Bindings: []MessagingBinding{
		{Source: exchangeIlm, Destination: queueCoreAuditLogs, RoutingKey: routingKeyAuditLogs},
		{Source: exchangeIlm, Destination: queueCoreNotifications, RoutingKey: routingKeyNotification},
		{Source: exchangeIlm, Destination: queueCoreActions, RoutingKey: routingKeyAction},
		{Source: exchangeIlm, Destination: queueCoreScheduler, RoutingKey: routingKeyScheduler},
		{Source: exchangeIlm, Destination: queueCoreValidation, RoutingKey: routingKeyValidation},
		{Source: exchangeIlm, Destination: queueCoreEvents, RoutingKey: routingKeyEvent},
		{Source: exchangeIlm, Destination: queueProviderStatusPoll, RoutingKey: queueProviderStatusPoll},
		{Source: exchangeIlm, Destination: queueTimeQualityConfig, RoutingKey: queueTimeQualityConfig},
		{Source: exchangeIlm, Destination: queueTimeQualityConfigRequest, RoutingKey: queueTimeQualityConfigRequest},
		{Source: exchangeIlm, Destination: queueTimeQualityResults, RoutingKey: queueTimeQualityResults},
	},
}

// messagingTopology2170 is the platform's managed-RabbitMQ topology for platform version
// 2.17.0: a SINGLE broker user (2.17.0 used one messaging credential, not the five-user 2.18.0
// split). The lone user takes the operator-stable "core" role — so Core's broker-credentials
// readback finds it at the <cluster>-core-user Secret exactly as in 2.18.0 — with full vhost
// permissions, since it is the only user. The exchange/queue/binding set is the czertainly
// direct exchange plus the core.* queues 2.17.0 Core uses; there is no czertainly-proxy
// exchange (that is the 2.18.0 proxy/provisioning path).
var messagingTopology2170 = MessagingTopology{
	DefaultVirtualHost: LegacyUnscopedVirtualHost,
	Users: []MessagingUser{
		{Role: MessagingUserCore, Tags: []string{"administrator"}, Configure: ".*", Write: ".*", Read: ".*"},
	},
	Exchanges: []MessagingExchange{
		{Name: exchangeCzertainly, Type: ExchangeTypeDirect, Durable: true},
	},
	Queues: []MessagingQueue{
		{Name: "core", Durable: true},
		{Name: queueCoreAuditLogs, Durable: true},
		{Name: queueCoreNotifications, Durable: true},
		{Name: queueCoreScheduler, Durable: true},
		{Name: queueCoreActions, Durable: true},
		{Name: queueCoreValidation, Durable: true},
		{Name: queueCoreEvents, Durable: true},
	},
	Bindings: []MessagingBinding{
		{Source: exchangeCzertainly, Destination: queueCoreAuditLogs, RoutingKey: routingKeyAuditLogs},
		{Source: exchangeCzertainly, Destination: queueCoreNotifications, RoutingKey: routingKeyNotification},
		{Source: exchangeCzertainly, Destination: queueCoreActions, RoutingKey: routingKeyAction},
		{Source: exchangeCzertainly, Destination: queueCoreScheduler, RoutingKey: routingKeyScheduler},
		{Source: exchangeCzertainly, Destination: queueCoreValidation, RoutingKey: routingKeyValidation},
		{Source: exchangeCzertainly, Destination: queueCoreEvents, RoutingKey: routingKeyEvent},
	},
}

// Messaging returns the DefaultVersion bundle's managed-RabbitMQ messaging topology.
// Prefer Bundle.MessagingTopologyData on a version-resolved bundle; this wrapper resolves
// the default version for version-agnostic callers.
func Messaging() MessagingTopology { return defaultBundle().Messaging }

// DefaultRabbitMQVersion is the RabbitMQ version the DefaultVersion bundle records as its
// validated managed-broker version. Prefer the version-resolved Bundle.RabbitMQVersion;
// this wrapper resolves the default version.
var DefaultRabbitMQVersion = defaultBundle().RabbitMQVersion
