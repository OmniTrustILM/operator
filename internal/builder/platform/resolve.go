/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package platform builds the Kubernetes resources for a Platform CR. It
// resolves the CR spec and the operator's versioned wiring profile into the
// generic common.Component render models. Managed-infra
// (CloudNativePG/RabbitMQ/Keycloak) and edge (Ingress/Gateway API/cert-manager)
// builders live in their own files in this package.
package platform

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Well-known intra-platform Service names and ports the operator wires Core to.
// The operator uses clean, unsuffixed Service names.
const (
	authName            = "auth"
	authOPAPoliciesName = "auth-opa-policies"
	schedulerName       = "scheduler"
	feAdministratorName = "fe-administrator"
	depServicePort      = 8080
	opaPort             = 8181
	ephemeralVolumeName = "ephemeral"
	defaultLogLevel     = "INFO"
	defaultSyncPolicy   = "create-only"
	// startupInitialDelaySeconds is the startup.initialDelaySeconds for every stateless
	// component using httpProbes (scheduler/utils/opa/auth/fe).
	startupInitialDelaySeconds = 15
	trustedCertsVolume         = "trusted-certificates-volume"
	trustedCertsMount          = "/etc/ssl/certs"
	feConfigVolume             = "fe-administrator-config-volume"
	feConfigMapName            = "fe-administrator-configmap"
	feConfigFile               = "config.js"
	feConfigMountPath          = "/usr/share/nginx/html/config.js"
	nginxCacheMountPath        = "/var/cache/nginx"
	tmpMountPath               = "/tmp"
	defaultFeURLAPI            = "/api"
	defaultFeURLLogin          = "/login"
	defaultFeURLLogout         = "/logout"
	secretVolumeFileMode       = 0o644
	// binSh is the shell the operator's init/sidecar containers exec their generated scripts with.
	binSh = "/bin/sh"
	// podIndexLabelFieldPath is the downward-API path to the pod-ordinal label Kubernetes
	// auto-stamps on StatefulSet pods from v1.28 (apps.kubernetes.io/pod-index).
	podIndexLabelFieldPath = "metadata.labels['apps.kubernetes.io/pod-index']"
)

// readOnlyRootFS returns a *bool true, for components whose every writable path is
// backed by a mounted volume so a read-only root filesystem is safe (see the
// per-component notes at each call site).
func readOnlyRootFS() *bool { b := true; return &b }

// resolveBundle returns the version bundle the render uses for this Platform: the bundle
// selected by spec.version, or — defensively — the operator's default (DefaultVersion, not
// necessarily its newest) bundle when the version is unknown. The controller already
// REJECTS an unknown spec.version BEFORE rendering (Reconcile resolves bom.BundleFor once
// and degrades with an actionable supported-versions message), so an unknown version never
// reaches a builder in practice; the fallback only keeps the pure builders (and the unit
// tests, which call them directly) total rather than panicking. spec.version=="" →
// the DefaultVersion bundle, so the out-of-the-box render is unchanged.
//
// Resolving here (off p.Spec.Version) is what makes spec.version REAL end to end: every
// Resolve* threads the selected bundle's wiring/topology/image coordinates, so selecting a
// (future) version renders that version's images and env wiring. The map lookup is a few
// reads per reconcile (one per builder call) — negligible — and keeps builder signatures
// stable (each already takes *Platform).
func resolveBundle(p *otilmv1alpha1.Platform) bom.Bundle {
	if b, ok := bom.BundleFor(p.Spec.Version); ok {
		return b
	}
	b, _ := bom.BundleFor("") // DefaultVersion bundle (always present)
	return b
}

// wiringFor returns the wiring profile of the bundle selected by spec.version.
func wiringFor(p *otilmv1alpha1.Platform) bom.WiringProfile { return resolveBundle(p).Wiring }

// imageCommand resolves the container entrypoint override with per-component-over-shared
// precedence (spec.<component>.image.command wins over spec.image.command). It returns
// nil when neither sets a command, so the image's own ENTRYPOINT is used.
func imageCommand(shared, comp otilmv1alpha1.ImageSpec) []string {
	if len(comp.Command) > 0 {
		return comp.Command
	}
	return shared.Command
}

// imageArgs resolves the container args override with the same precedence as
// imageCommand. It returns nil when neither sets args, so the image's own CMD is used.
func imageArgs(shared, comp otilmv1alpha1.ImageSpec) []string {
	if len(comp.Args) > 0 {
		return comp.Args
	}
	return shared.Args
}

// DefaultImageRegistry mutates the platform's effective shared ImageSpec in place,
// defaulting an unset spec.image.registry to the public ILM registry host
// (bom.DefaultImageRegistry). It is registry-only defaulting: the repository defaults
// lazily inside common.ResolveImage (bundle-aware — see resolveRepository), never
// eagerly here, so nothing about "user set ilm" vs. "defaulted" is lost before
// resolution. It is applied once per reconcile (and once in RenderPlatform) BEFORE any
// common.ResolveImage call, so every ILM component image resolves to
// hub.omnitrustregistry.com/<repository>/<name>:<tag> out of the box rather than the
// bare "<name>:<tag>" Docker would treat as docker.io.
//
// A user-set spec.image.registry is left untouched (only an empty field is filled), so
// an explicit override still wins. It does NOT touch per-component image overrides
// (spec.<component>.image) — those flow through ResolveImage's per-field precedence and
// may legitimately point a single component elsewhere. Defaulting here (not via a
// kubebuilder marker on the shared ImageSpec type) keeps the default platform-only:
// ImageSpec is shared with Connector, which has no version bundle and must not inherit
// the platform registry. Nothing here is ever persisted back to the stored CR — see the
// controller's ordering invariant (finalizer handling runs BEFORE this call).
func DefaultImageRegistry(p *otilmv1alpha1.Platform) {
	if p.Spec.Common.Image.Registry == "" {
		p.Spec.Common.Image.Registry = bom.DefaultImageRegistry
	}
}

// ResolveCore resolves the Core component's render model from the Platform spec.
// Env-var names, service URLs, and the connection-string format come from the
// versioned wiring profile (bom.Wiring()); the CR's core.env entries are applied
// last as overrides. Core runs with the OPA sidecar, a wait-for-auth init
// container, a /tmp ephemeral volume, and readiness/startup probes (see the
// volume/lifecycle notes below).
func ResolveCore(p *otilmv1alpha1.Platform) common.Component {
	b := resolveBundle(p)
	w := b.Wiring
	image, policy := common.ResolveImage(b.Lookup, "core", p.Spec.Common.Image, p.Spec.Core.Image)

	c := common.Component{
		Name: "core", Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.Core.Image.PullSecrets, timeQualityMonitorPullSecrets(p)),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		// Recreate: Core runs its Flyway schema migrations at start-up, and the migrations
		// THEMSELVES are safe under concurrency — Flyway takes a lock, so when several Core
		// pods start together exactly one migrates and the rest wait. That confirmed contract
		// is what makes multi-replica Core supported (workloadType: StatefulSet). What
		// Recreate buys is the VERSION boundary on the single-replica Deployment path: a
		// RollingUpdate would leave the OLD Core serving traffic against a schema the NEW pod
		// has already migrated. Recreate terminates the old pod first, so one Core VERSION
		// owns the schema at a time. It matches the chart's deploy-once model and is IGNORED
		// for a StatefulSet, which has its own update strategy. See Component.Recreate.
		Recreate: true,
		Command:  imageCommand(p.Spec.Common.Image, p.Spec.Core.Image),
		Args:     imageArgs(p.Spec.Common.Image, p.Spec.Core.Image),
		Env:      coreEnv(p, w),
		ConfigMapEnv: []common.ConfigMapEnvRef{
			{EnvVar: w.MessagingHostEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingHostKey},
			{EnvVar: w.MessagingPortEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingPortKey},
		},
		SecretEnv:      coreSecretEnv(p, w),
		InitContainers: coreInitContainers(p),
		Sidecars:       []corev1.Container{opaSidecar(p)},
		// OPA must start BEFORE Core: Core's OIDC postStart (register-internal-keycloak.sh) blocks
		// on the OPA sidecar's :8181 before PUTting Core's settings API, and the kubelet starts
		// containers in order + runs postStart synchronously — so OPA-after-Core deadlocks. Mirrors
		// the chart's core-deployment container order (auth-opa before core).
		SidecarsFirst: true,
		Volumes:       []corev1.Volume{ephemeralVolume()},
		VolumeMounts:  []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: "/tmp"}},
		Probes:        coreProbes(),
		// Read-only root filesystem: ENABLED. Core is a Spring Boot (JVM) service; its
		// only writable path is the in-memory /tmp ephemeral volume (the JVM honours
		// java.io.tmpdir=/tmp). Validated end-to-end against the live core image on Kind
		// (managed-platform e2e): Core reaches Ready with a read-only root.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
		// IN-POD bootstrap wiring is layered below via withCoreInPodScripts: it mounts a
		// "core-scripts" ConfigMap at /opt/ilm/scripts and runs register-admin.sh (the first-admin
		// registration, certificate method) and/or register-internal-keycloak.sh (managed-Keycloak
		// OIDC provider) via a lifecycle.postStart hook — because BOTH of Core's target endpoints
		// (POST /api/v1/local/admins and the settings OIDC PUT) are localhost-only and a cross-pod
		// call from the operator is rejected.
	}

	// Layer Core's IN-POD bootstrap wiring (the scripts ConfigMap volume/mount, the postStart
	// exec, and — for managed Keycloak — the $INTERNAL_OAUTH_SECRET secretKeyRef) BEFORE the user
	// overrides, so a user could still extend it. It runs register-admin.sh (certificate admin)
	// and/or register-internal-keycloak.sh (managed Keycloak), both targeting Core's
	// localhost-only APIs. A no-op when neither the cert admin nor a managed Keycloak applies.
	c = withCoreInPodScripts(p, c)

	// PROXY_INSTANCE_ID: when proxy support is enabled, Core needs a stable per-pod
	// identity (a downward-API env), sourced from the pod name via fieldRef
	// metadata.name. Omitted when proxy is disabled.
	if p.Spec.Common.Proxy.Enabled {
		c.FieldRefEnv = append(c.FieldRefEnv, common.FieldRefEnv{
			EnvVar: w.ProxyInstanceIDEnv, FieldPath: "metadata.name",
		})
	}

	// PLATFORM_INSTANCE_ID (derived): a StatefulSet Core with no explicit id takes a stable,
	// unique id per pod from the pod ORDINAL — exposed by Kubernetes as the
	// apps.kubernetes.io/pod-index label and projected via the downward API, exactly as the
	// Helm chart derives it. coreInitContainers pairs this with the fail-fast guard.
	if coreDerivesInstanceIDFromPodIndex(p) {
		c.FieldRefEnv = append(c.FieldRefEnv, common.FieldRefEnv{
			EnvVar: w.PlatformInstanceIDEnv, FieldPath: podIndexLabelFieldPath,
		})
	}

	// Time-quality-monitor sidecar (2.19.0+, opt-in). It is appended AFTER the OPA sidecar so
	// OPA stays first: SidecarsFirst renders sidecars before the main container, and Core's
	// postStart hook blocks on OPA's port.
	if TimeQualityMonitorEnabled(p) {
		c.Sidecars = append(c.Sidecars, timeQualityMonitorSidecar(p))
	}

	// PLATFORM_INSTANCE_ID (explicit): an operator-assigned instance id renders as a plain
	// value. It is placed before applyComponentSpec so a user's own spec.core.env entry of the
	// same name still wins (last-wins container-env semantics). The wiring name is EMPTY before
	// 2.19.0, so this renders on 2.19.0+ only.
	if p.Spec.Core.InstanceID != nil {
		c.Env = append(c.Env, common.EnvPair{
			Name:  w.PlatformInstanceIDEnv,
			Value: strconv.FormatInt(int64(*p.Spec.Core.InstanceID), 10),
		})
	}

	// Layer the user's per-component overrides (env/resources/replicas/refs/volumes/
	// probes/securityContext/pod meta/scheduling/init+sidecar/serviceAccount/service) onto
	// the operator-derived Core component. Defaults stay intact when the spec is empty.
	applyComponentSpec(p, &c, p.Spec.Core.ComponentSpec)
	return c
}

// sharedDBAndMessagingEnv returns the inline and ConfigMap-backed env vars that both
// Core and scheduler require: the JDBC connection URL, the messaging virtual
// host, and the ConfigMap-backed broker host/port. It does NOT include credentials —
// those are always secret-backed and added separately by each caller via
// sharedCredSecretEnv.
func sharedDBAndMessagingEnv(p *otilmv1alpha1.Platform, w bom.WiringProfile) ([]common.EnvPair, []common.ConfigMapEnvRef) {
	// Resolve the database coordinates mode-agnostically: external → the spec coordinates,
	// managed → the CloudNativePG-generated Service/port/database. The JDBC URL is built
	// from the resolved facts identically in both modes.
	db := ResolveDatabaseConnection(p)
	// Resolve the broker vhost mode-agnostically too: external → the spec virtualHost,
	// managed → the operator-provisioned vhost (spec.messaging.virtualHost or the default).
	mq := ResolveMessagingConnection(p)
	env := []common.EnvPair{
		{Name: w.DatabaseURLEnv, Value: w.DatabaseURL(db.Host, db.Port, db.Name)},
		{Name: w.MessagingVHostEnv, Value: mq.VirtualHost},
	}
	cmEnv := []common.ConfigMapEnvRef{
		{EnvVar: w.MessagingHostEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingHostKey},
		{EnvVar: w.MessagingPortEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingPortKey},
	}
	return env, cmEnv
}

// sharedCredSecretEnv returns the secretKeyRef wiring for the database and messaging
// credentials shared between Core and scheduler. When the resolved credentials
// Secret name is empty the corresponding refs are omitted (the pod is still valid but the
// service will fail to connect — the operator validates presence at admission time).
func sharedCredSecretEnv(p *otilmv1alpha1.Platform, w bom.WiringProfile) []common.SecretEnvRef {
	var refs []common.SecretEnvRef
	// Mode-agnostic DB credentials Secret: the caller's Secret (external) or the
	// CloudNativePG-generated <cluster>-app Secret (managed). Either way the credentials
	// are injected by secretKeyRef from that Secret — never copied into the rendered
	// object. The OUTPUT env-var names stay BOM application contracts; the INPUT in-Secret
	// keys are the resolved effective keys (the user's external mapping, or the upstream
	// operator's keys for managed) — see ResolveDatabaseConnection.
	if db := ResolveDatabaseConnection(p); db.CredentialsSecretName != "" {
		refs = append(refs,
			common.SecretEnvRef{EnvVar: w.DatabaseCred.UsernameEnv, SecretName: db.CredentialsSecretName, SecretKey: db.UsernameKey},
			common.SecretEnvRef{EnvVar: w.DatabaseCred.PasswordEnv, SecretName: db.CredentialsSecretName, SecretKey: db.PasswordKey},
		)
	}
	// Mode-agnostic messaging credentials Secret: the caller's Secret (external) or the
	// Messaging-Topology-Operator-generated Core-user Secret (managed). Same INPUT-key
	// mapping rule as the database creds above; the OUTPUT BROKER_* env names stay BOM
	// contracts.
	if mq := ResolveMessagingConnection(p); mq.CredentialsSecretName != "" {
		refs = append(refs,
			common.SecretEnvRef{EnvVar: w.MessagingCred.UsernameEnv, SecretName: mq.CredentialsSecretName, SecretKey: mq.UsernameKey},
			common.SecretEnvRef{EnvVar: w.MessagingCred.PasswordEnv, SecretName: mq.CredentialsSecretName, SecretKey: mq.PasswordKey},
		)
	}
	return refs
}

// coreEnv composes Core's inline (static/derived) environment variables. Secret-
// and ConfigMap-backed vars are added separately (see coreSecretEnv and the
// ConfigMapEnv wiring in ResolveCore). Env-var names come from the wiring profile.
func coreEnv(p *otilmv1alpha1.Platform, w bom.WiringProfile) []common.EnvPair {
	headerName := p.Spec.Core.ClientCertHeader
	if headerName == "" {
		headerName = w.HeaderNameValue
	}
	logLevel := p.Spec.Common.Logging.Level
	if logLevel == "" {
		logLevel = defaultLogLevel
	}

	// Shared DB URL + messaging vhost from the helper; Core adds its extra static
	// service-URL, header, proxy, and provisioning env on top.
	sharedEnv, _ := sharedDBAndMessagingEnv(p, w)

	env := []common.EnvPair{
		{Name: w.HeaderEnabledEnv, Value: "true"},
		{Name: w.HeaderNameEnv, Value: headerName},
		{Name: w.OPABaseURLEnv, Value: w.OPABaseURL},
		{Name: w.AuthURLEnv, Value: w.AuthURL},
		{Name: w.SchedulerURLEnv, Value: w.SchedulerURL},
		{Name: w.LoggingLevelEnv, Value: logLevel},
		{Name: w.ProvisioningURLEnv, Value: provisioningAPIURL(p)},
		{Name: w.ProxyEnabledEnv, Value: strconv.FormatBool(p.Spec.Common.Proxy.Enabled)},
	}
	// Splice shared DB/messaging env at a stable position (after the header env,
	// before the proxy env) for a deterministic env ordering.
	env = append(env[:2], append(sharedEnv, env[2:]...)...)

	// Time-quality integration (2.19.0+). The wiring name is EMPTY in earlier bundles and
	// buildContainerEnv drops a nameless variable, so this renders on 2.19.0+ only — the same
	// version-gating the 2.17.0 bundle uses to omit the proxy/provisioning env.
	env = append(env, common.EnvPair{
		Name:  w.TimeQualityEnabledEnv,
		Value: strconv.FormatBool(p.Spec.Messaging.TimeQuality.Enabled),
	})

	// Proxy URLs are only injected when set.
	if p.Spec.Common.Proxy.HTTP != "" {
		env = append(env, common.EnvPair{Name: w.HTTPProxyEnv, Value: p.Spec.Common.Proxy.HTTP})
	}
	if p.Spec.Common.Proxy.HTTPS != "" {
		env = append(env, common.EnvPair{Name: w.HTTPSProxyEnv, Value: p.Spec.Common.Proxy.HTTPS})
	}
	if p.Spec.Common.Proxy.NoProxy != "" {
		env = append(env, common.EnvPair{Name: w.NoProxyEnv, Value: p.Spec.Common.Proxy.NoProxy})
	}
	return env
}

// coreSecretEnv lists Core's secret-backed env vars, all sourced via secretKeyRef
// from user-provided Secrets — the operator never copies the values into the
// rendered objects. DB and messaging creds come from the shared helper; Core adds
// its own trusted-certificates, provisioning-API-key, and (when the optional admin
// bootstrap is enabled) admin-certificate on top.
func coreSecretEnv(p *otilmv1alpha1.Platform, w bom.WiringProfile) []common.SecretEnvRef {
	refs := sharedCredSecretEnv(p, w)
	// TRUSTED_CERTIFICATES is sourced from the effective trusted-certs Secret: the
	// caller's SecretRef verbatim, or — when the admin bootstrap requires Core to also
	// trust a generated admin CA — the operator-composed trusted-certificates Secret
	// (see TrustedCertsSecretName / reconcileTrustedCerts). The value is never inlined.
	//
	// Optional=true: the trusted-cert Secret can legitimately lag behind Core (the
	// operator-composed bundle, or a cert-manager CA Secret being minted), so an
	// optional secretKeyRef lets Core START rather than wedge with
	// CreateContainerConfigError. When the Secret later appears, the trusted-certs
	// checksum annotation already wired on Core's pod template (reconcileTrustedCerts +
	// StampConfigChecksum) changes the pod-template hash and rolls Core to pick it up.
	if ref := TrustedCertsSecretName(p); ref != "" {
		refs = append(refs, common.SecretEnvRef{
			EnvVar: w.TrustedCertificates.Env, SecretName: ref, SecretKey: TrustedCertsSecretKey(p),
			Optional: true,
		})
	}
	if ref := provisioningAPIKeySecretRef(p); ref != "" {
		refs = append(refs, common.SecretEnvRef{
			EnvVar: w.ProvisioningAPIKey.Env, SecretName: ref, SecretKey: ProvisioningAPIKeyKey(p),
		})
	}
	// ADMIN_CERT: only when registerAdmin is enabled, sourced via secretKeyRef from
	// the admin client-certificate Secret's tls.crt key (the caller's SecretRef for
	// source=provided, or the cert-manager-populated admin-certificate-secret for
	// source=generated). Omitted when admin bootstrap is disabled. The cert value is
	// never inlined — only referenced.
	//
	// Optional=true: for source=generated the admin cert Secret is minted by
	// cert-manager and is not present the instant Core is applied; an optional
	// secretKeyRef lets Core START (no CreateContainerConfigError wedge). When the cert
	// Secret later appears, the trusted-certs checksum roll (above) re-rolls Core, which
	// then resolves ADMIN_CERT. The reconciler also gates the registration action on the
	// cert being present, so a missing cert never registers a half-configured admin.
	if ref := adminCertSecretRef(p); ref != "" {
		refs = append(refs, common.SecretEnvRef{
			EnvVar: w.AdminCert.Env, SecretName: ref, SecretKey: AdminCertKey(p),
			Optional: true,
		})
	}
	return refs
}

// coreProbes returns Core's readiness and startup probes against the HTTP service
// port. Readiness hits the readiness endpoint (gating Service traffic); startup hits
// the liveness endpoint with a long failure budget (45 × 10s) so a slow boot — notably
// the Flyway schema migration — is tolerated before liveness/traffic gating begins.
//
// Core intentionally has NO liveness probe, consistent with every other platform
// component (httpProbes omits liveness fleet-wide) and with the chart, which ships
// Core's image.probes.liveness.enabled=false. A liveness probe only earns its keep when
// it recovers an unrecoverable in-process deadlock; Core has none. Its real-world effect
// is the opposite: a probe that trips during a transient stall (a long migration, a GC
// pause, a slow dependency) kills a healthy-but-busy pod, converting a recoverable blip
// into a hard restart — and, mid-migration through a pooled connection, into schema
// corruption. Readiness already removes a wedged pod from traffic without killing it;
// startup already covers a slow boot. Liveness here is pure downside.
func coreProbes() common.Probes {
	port := intstr.FromInt32(depServicePort)
	httpGet := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: port}}}
	}
	readiness := httpGet("/api/v1/health/readiness")
	readiness.InitialDelaySeconds = 15
	startup := httpGet("/api/v1/health/liveness")
	startup.InitialDelaySeconds = 15
	startup.PeriodSeconds = 10
	startup.FailureThreshold = 45
	return common.Probes{Readiness: readiness, Startup: startup}
}

// ephemeralVolume returns the /tmp scratch volume: an in-memory emptyDir (medium
// Memory, 1Mi size limit).
func ephemeralVolume() corev1.Volume {
	sizeLimit := resource.MustParse("1Mi")
	return corev1.Volume{
		Name: ephemeralVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: &sizeLimit,
			},
		},
	}
}

// opaSidecar returns the OPA policy-engine sidecar ("auth-opa"). It runs as a
// server on :8181 and pulls its policy bundle from the auth-opa-policies Service.
// The builder SCC-hardens it automatically (filling the four SCC fields around the
// read-only-root flag set here).
//
// Read-only root filesystem: ENABLED. OPA pulls its bundle over HTTP into memory and
// writes nothing to disk by default, so a read-only root is safe.
func opaSidecar(p *otilmv1alpha1.Platform) corev1.Container {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, "opa", p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/health?bundle=true",
				Port:   intstr.FromInt32(opaPort),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: 5,
		TimeoutSeconds:      5,
		PeriodSeconds:       10,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
	return corev1.Container{
		Name:            "auth-opa",
		Image:           image,
		ImagePullPolicy: policy,
		Ports:           []corev1.ContainerPort{{ContainerPort: opaPort}},
		Args: []string{
			"run",
			"--server",
			"--addr=0.0.0.0:8181",
			fmt.Sprintf("--set=services.nginx.url=http://%s:%d", authOPAPoliciesName, depServicePort),
			"--set=bundles.nginx.service=nginx",
			"--set=bundles.nginx.resource=bundles/bundle.tar.gz",
		},
		ReadinessProbe:  probe,
		StartupProbe:    probe.DeepCopy(),
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: readOnlyRootFS()},
	}
}

// ResolveScheduler resolves the scheduler component's render model.
// It is a Java service that requires the platform's database and messaging broker.
// Credentials are sourced from the same platform-level Secrets as Core (no
// per-service Secrets are invented). The broker host/port come from the shared
// messaging ConfigMap. An init container blocks startup until the broker is reachable.
//
// DB credentials: like Core, scheduler injects JDBC_USERNAME/JDBC_PASSWORD by
// secretKeyRef straight from the user-referenced database credentials Secret
// (sharedCredSecretEnv) — the operator never copies the values into a rendered Secret.
// (auth-db is the one composed Secret, and only because .NET needs a transformed
// connection string; plain user/pass needs no transform, so no scheduler Secret is
// rendered.)
func ResolveScheduler(p *otilmv1alpha1.Platform) common.Component {
	b := resolveBundle(p)
	w := b.Wiring
	image, policy := common.ResolveImage(b.Lookup, schedulerName, p.Spec.Common.Image, p.Spec.Scheduler.Image)

	logLevel := p.Spec.Common.Logging.Level
	if logLevel == "" {
		logLevel = defaultLogLevel
	}

	sharedEnv, sharedCMEnv := sharedDBAndMessagingEnv(p, w)
	// Scheduler has PORT and LOGGING_LEVEL_COM_CZERTAINLY as inline env;
	// DB URL and messaging vhost come from the shared helper.
	env := []common.EnvPair{
		{Name: "PORT", Value: fmt.Sprintf("%d", depServicePort)},
		{Name: w.LoggingLevelEnv, Value: logLevel},
	}
	env = append(env, sharedEnv...)

	c := common.Component{
		Name: schedulerName, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.Scheduler.Image.PullSecrets),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		Command:        imageCommand(p.Spec.Common.Image, p.Spec.Scheduler.Image),
		Args:           imageArgs(p.Spec.Common.Image, p.Spec.Scheduler.Image),
		Env:            env,
		ConfigMapEnv:   sharedCMEnv,
		SecretEnv:      sharedCredSecretEnv(p, w),
		InitContainers: []corev1.Container{waitForMessagingInitContainer(p)},
		Volumes:        []corev1.Volume{ephemeralVolume()},
		VolumeMounts:   []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: "/tmp"}},
		Probes:         schedulerProbes(),
		// Read-only root filesystem: ENABLED. JVM service whose only writable path is the
		// in-memory /tmp ephemeral volume. Validated against the live scheduler
		// image on Kind (managed-platform e2e): reaches Ready with a read-only root.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.Scheduler.ComponentSpec)
	return c
}

// httpProbes builds a readiness+startup probe pair from per-component paths and
// the readiness initial delay. The startup initial delay is the fleet-wide default
// (startupInitialDelaySeconds) and the liveness probe is intentionally omitted for
// these stateless components.
func httpProbes(readinessPath string, readinessInitialDelay int32, startupPath string) common.Probes {
	return httpProbesOn(readinessPath, readinessInitialDelay, startupPath, depServicePort)
}

// httpProbesOn is httpProbes parameterized by the container port, for a component whose
// primary port is not the shared depServicePort (e.g. the provisioning-rabbitmq service on
// 8077). The probe parameters (timeouts/thresholds) are the same fleet-wide defaults.
func httpProbesOn(readinessPath string, readinessInitialDelay int32, startupPath string, containerPort int32) common.Probes {
	port := intstr.FromInt32(containerPort)
	httpGet := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: port}}}
	}
	readiness := httpGet(readinessPath)
	readiness.InitialDelaySeconds = readinessInitialDelay
	readiness.TimeoutSeconds = 5
	readiness.PeriodSeconds = 10
	readiness.SuccessThreshold = 1
	readiness.FailureThreshold = 3
	startup := httpGet(startupPath)
	startup.InitialDelaySeconds = startupInitialDelaySeconds
	startup.TimeoutSeconds = 5
	startup.PeriodSeconds = 10
	startup.SuccessThreshold = 1
	startup.FailureThreshold = 45
	return common.Probes{Readiness: readiness, Startup: startup}
}

// schedulerProbes returns scheduler's readiness (path /health/readiness) and
// startup (path /health/liveness) probes; liveness is intentionally omitted.
func schedulerProbes() common.Probes {
	return httpProbes("/health/readiness", 15, "/health/liveness")
}

// Script-local wiring for the broker-reachability wait loops (wait-for-messaging-service on
// scheduler, wait-for-auth on Core). These env var names are deliberately NOT part of the
// version wiring profile: nothing but these two generated scripts reads them, and they carry
// only non-secret coordinates.
//
// SECURITY: in external mode the broker host is spec.messaging.host — user-supplied data. It
// is passed as a Kubernetes env VALUE (kubelet sets env verbatim; no shell ever evaluates it)
// and read back as a QUOTED parameter expansion, which the shell never re-parses as syntax.
// Command substitution, backticks, quotes, `;`, `|` and newlines in the host are therefore
// inert: they cannot reach a shell command position at all. The CRD's charset pattern on the
// field is defence in depth. Interpolating the host into the script source (the earlier shape)
// executed whatever it contained, once per poll iteration.
const (
	mqWaitHostEnv = "MQ_WAIT_HOST"
	mqWaitPortEnv = "MQ_WAIT_PORT"
	// mqWaitLoop polls the broker's AMQP port, reading the coordinates from the quoted env
	// vars above rather than from interpolated script text.
	mqWaitLoop = `while ! nc -z "$MQ_WAIT_HOST" "$MQ_WAIT_PORT"; do sleep 1; done`
)

// messagingWaitEnv returns the script-local env carrying the resolved broker coordinates for
// the wait loops: plain values, never a credential and never a Secret reference.
func messagingWaitEnv(mq MessagingConnection) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: mqWaitHostEnv, Value: mq.Host},
		{Name: mqWaitPortEnv, Value: strconv.Itoa(int(mq.Port))},
	}
}

// waitForMessagingInitContainer returns the "wait-for-messaging-service" init
// container used by scheduler: a simple nc loop that blocks until the
// broker AMQP port is reachable. The coordinates arrive as env values (see mqWaitLoop).
func waitForMessagingInitContainer(p *otilmv1alpha1.Platform) corev1.Container {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, "curl", p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})
	// Resolve the broker host/port mode-agnostically so a managed broker waits on the
	// RabbitMQ Service, an external one on the caller's host.
	mq := ResolveMessagingConnection(p)
	script := mqWaitLoop + " &&\necho \"messaging service seems to be started\"\n"
	return corev1.Container{
		Name:            "wait-for-messaging-service",
		Image:           image,
		ImagePullPolicy: policy,
		Env:             messagingWaitEnv(mq),
		Command:         []string{binSh, "-c", script},
		// Read-only root filesystem: ENABLED. A pure nc-poll loop writes nothing to disk.
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: readOnlyRootFS()},
	}
}

// ResolveUtils resolves the utils component's render model.
// It is a minimal Java service with no database or messaging dependencies.
// It is only included when spec.utils.enabled is true.
func ResolveUtils(p *otilmv1alpha1.Platform) common.Component {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, "utils", p.Spec.Common.Image, p.Spec.Utils.Image)

	logLevel := p.Spec.Common.Logging.Level
	if logLevel == "" {
		logLevel = defaultLogLevel
	}

	c := common.Component{
		Name: "utils", Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.Utils.Image.PullSecrets),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.Utils.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.Utils.Image),
		Env: []common.EnvPair{
			{Name: "PORT", Value: fmt.Sprintf("%d", depServicePort)},
			{Name: "LOG_LEVEL", Value: logLevel},
		},
		Volumes:      []corev1.Volume{ephemeralVolume()},
		VolumeMounts: []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: "/tmp"}},
		Probes:       utilsProbes(),
		// Read-only root filesystem: ENABLED. Minimal JVM service whose only writable path
		// is the in-memory /tmp ephemeral volume. Validated against the live utils
		// image on Kind (managed-platform e2e): reaches Ready with a read-only root.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.Utils.ComponentSpec)
	return c
}

// utilsProbes returns utils's readiness (path /health/readiness) and startup
// (path /health/liveness) probes; liveness is intentionally omitted.
func utilsProbes() common.Probes {
	return httpProbes("/health/readiness", 15, "/health/liveness")
}

// ResolveAuthOpaPolicies resolves the auth-opa-policies component's render model.
// It is an nginx bundle server that serves the OPA policy bundle to Core's OPA
// sidecar. It has no environment variables, and a single ephemeral volume backs both
// /var/cache/nginx and /tmp.
//
// Read-only root filesystem: ENABLED. nginx's only writable paths are its cache
// (/var/cache/nginx) and /tmp, both backed by the in-memory ephemeral volume, so a
// read-only root is safe (the standard hardened-nginx carve-out).
func ResolveAuthOpaPolicies(p *otilmv1alpha1.Platform) common.Component {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, authOPAPoliciesName, p.Spec.Common.Image, p.Spec.AuthOpaPolicies.Image)
	c := common.Component{
		Name: authOPAPoliciesName, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.AuthOpaPolicies.Image.PullSecrets),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.AuthOpaPolicies.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.AuthOpaPolicies.Image),
		// No env vars — nginx serves a static bundle; all configuration is baked in.
		Volumes: []corev1.Volume{ephemeralVolume()},
		// The same ephemeral volume is mounted at both /var/cache/nginx and /tmp.
		VolumeMounts: []corev1.VolumeMount{
			{Name: ephemeralVolumeName, MountPath: "/var/cache/nginx"},
			{Name: ephemeralVolumeName, MountPath: "/tmp"},
		},
		Probes:                 opaProbes(),
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.AuthOpaPolicies.ComponentSpec)
	return c
}

// opaProbes returns auth-opa-policies' readiness (path /index.html) and startup
// (same path) probes; liveness is intentionally omitted.
func opaProbes() common.Probes {
	return httpProbes("/index.html", 5, "/index.html")
}

// ResolveAuth resolves the auth component's render model. auth
// is the .NET authentication service. Its container is named "auth". It receives
// static AUTH_* / SYNC_POLICY / ASPNETCORE_URLS env inline and its database connection
// string via secretKeyRef from an operator-managed Secret (see the controller's
// reconcileAuthDBSecret): the operator composes that string so the credential lives
// ONLY in that Secret and is never inlined here, in env values, or anywhere in the
// rendered objects. A trusted-CA bundle is volume-mounted at /etc/ssl/certs when configured.
func ResolveAuth(p *otilmv1alpha1.Platform) common.Component {
	b := resolveBundle(p)
	w := b.Wiring
	image, policy := common.ResolveImage(b.Lookup, authName, p.Spec.Common.Image, p.Spec.Auth.Image)

	syncPolicy := p.Spec.Auth.SyncPolicy
	if syncPolicy == "" {
		syncPolicy = defaultSyncPolicy
	}

	c := common.Component{
		Name: authName, Instance: p.Name, Namespace: p.Namespace,
		ContainerName: "auth", // the container is named "auth", not "auth"
		Image:         image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.Auth.Image.PullSecrets),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		// Recreate: auth runs DB schema migrations at startup — never run two migrating
		// pods concurrently (see Component.Recreate / Core above).
		Recreate: true,
		Command:  imageCommand(p.Spec.Common.Image, p.Spec.Auth.Image),
		Args:     imageArgs(p.Spec.Common.Image, p.Spec.Auth.Image),
		Env: []common.EnvPair{
			{Name: w.AuthCreateUsersEnv, Value: strconv.FormatBool(p.Spec.Auth.Create.CreateUnknownUsers)},
			{Name: w.AuthCreateRolesEnv, Value: strconv.FormatBool(p.Spec.Auth.Create.CreateUnknownRoles)},
			{Name: w.AuthSyncPolicyEnv, Value: syncPolicy},
			{Name: w.AuthAspNetURLsEnv, Value: w.AuthAspNetURLs},
		},
		// The connection string is composed by the operator and read from the
		// operator-managed Secret — never inlined. The Secret is created by the
		// controller before the workload is applied.
		SecretEnv: []common.SecretEnvRef{
			{EnvVar: w.AuthDBConnEnv, SecretName: w.AuthDBSecretName, SecretKey: w.AuthDBSecretKey},
		},
		Probes: authProbes(),
		// Read-only root filesystem: ENABLED. auth is a .NET (ASP.NET Core)
		// service; unlike the JVM components it writes outside the image layers (the
		// data-protection keyring + general temp files), so it needs a writable /tmp. We
		// add the in-memory /tmp ephemeral volume here (the JVM components already carry
		// one) and point the runtime at it (TMPDIR=/tmp) so a read-only root is safe.
		// Validated against the live auth image on Kind (managed-platform e2e).
		Volumes:                []corev1.Volume{ephemeralVolume()},
		VolumeMounts:           []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: tmpMountPath}},
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	// TMPDIR pins the process temp dir to the writable /tmp ephemeral volume so the .NET
	// runtime (and any libraries honouring TMPDIR) never attempts to write to the now
	// read-only root. Prepended so an explicit auth.env override could still win.
	c.Env = append([]common.EnvPair{{Name: "TMPDIR", Value: tmpMountPath}}, c.Env...)

	// Trusted CA bundle: mounted as a read-only volume at /etc/ssl/certs. The cert value
	// is never inlined. Uses the effective trusted-certs Secret name so auth mounts
	// the same bundle Core trusts (the operator-composed Secret when source=generated).
	if ref := TrustedCertsSecretName(p); ref != "" {
		mode := int32(secretVolumeFileMode)
		c.Volumes = append(c.Volumes, corev1.Volume{
			Name: trustedCertsVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: ref, DefaultMode: &mode},
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name: trustedCertsVolume, MountPath: trustedCertsMount, ReadOnly: true,
		})
	}
	applyComponentSpec(p, &c, p.Spec.Auth.ComponentSpec)
	return c
}

// authProbes returns auth's readiness and startup probes (both
// against /health); liveness is intentionally omitted.
func authProbes() common.Probes {
	return httpProbes("/health", 5, "/health")
}

// ResolveFeAdministrator resolves the fe-administrator component's render model.
// fe-administrator is a static nginx front-end. Its single static config file
// (config.js) is mounted (subPath) from the fe-administrator ConfigMap at
// /usr/share/nginx/html/config.js; nginx's writable paths (/var/cache/nginx, /tmp)
// are backed by an in-memory ephemeral volume so the root filesystem stays read-only.
func ResolveFeAdministrator(p *otilmv1alpha1.Platform) common.Component {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, feAdministratorName, p.Spec.Common.Image, p.Spec.FeAdministrator.Image)

	c := common.Component{
		Name: feAdministratorName, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.FeAdministrator.Image.PullSecrets),
		Replicas: 1, Port: depServicePort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.FeAdministrator.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.FeAdministrator.Image),
		// No environment variables — runtime configuration is served from config.js.
		Volumes: []corev1.Volume{feConfigVolumeSource(), ephemeralVolume()},
		// config.js is mounted via subPath so it appears as a single file inside the
		// served document root; the ephemeral volume backs both nginx writable paths.
		VolumeMounts: []corev1.VolumeMount{
			{Name: feConfigVolume, MountPath: feConfigMountPath, SubPath: feConfigFile},
			{Name: ephemeralVolumeName, MountPath: nginxCacheMountPath},
			{Name: ephemeralVolumeName, MountPath: tmpMountPath},
		},
		Probes: feAdministratorProbes(),
		// Read-only root filesystem: ENABLED. The served root is a baked-in image layer;
		// the only writable paths (nginx cache + /tmp) are backed by the in-memory
		// ephemeral volume, and config.js is a read-only subPath mount.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.FeAdministrator.ComponentSpec)
	return c
}

// feConfigVolumeSource returns the pod volume that projects the fe-administrator
// ConfigMap's config.js as a single named item.
func feConfigVolumeSource() corev1.Volume {
	return corev1.Volume{
		Name: feConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: feConfigMapName},
				Items:                []corev1.KeyToPath{{Key: feConfigFile, Path: feConfigFile}},
			},
		},
	}
}

// feAdministratorProbes returns fe-administrator's readiness and startup probes
// against "/"; liveness is intentionally omitted.
func feAdministratorProbes() common.Probes {
	return httpProbes("/", 5, "/")
}

// provisioningAPIURL returns the provisioning API base URL Core is wired to:
//
//   - mode=deploy → the in-cluster URL of the operator-rendered provisioning Service
//     (http://provisioning-rabbitmq:8077), so Core calls the bundled service the operator
//     manages, not the (ignored) spec.apiURL;
//   - mode=external → the configured spec.provisioning.apiURL (today's behaviour);
//   - provisioning not configured (nil block) → "".
//
// It is the single nil-safe accessor the env wiring and the configured/gate helpers read.
func provisioningAPIURL(p *otilmv1alpha1.Platform) string {
	if p.Spec.Provisioning == nil {
		return ""
	}
	if ProvisioningDeploy(p) {
		return fmt.Sprintf("http://%s:%d", provisioningName, provisioningPort)
	}
	return p.Spec.Provisioning.APIURL
}

// provisioningAPIKeySecretRef returns the Secret name Core's PROVISIONING_API_KEY is sourced
// from: the deploy bootstrap Secret (mode=deploy — the same Secret + key that backs the
// deployed service's SECURITY_API_KEY) or the caller's spec.apiKeySecretRef (mode=external).
// "" when unset.
func provisioningAPIKeySecretRef(p *otilmv1alpha1.Platform) string {
	pr := p.Spec.Provisioning
	if pr == nil {
		return ""
	}
	if ProvisioningDeploy(p) {
		return pr.Deploy.BootstrapSecretRef
	}
	return pr.APIKeySecretRef
}

// provisioningConfigured reports whether the platform points Core at a provisioning API —
// either an external one (a non-empty spec.provisioning.apiURL) or the operator-
// deployed bundled service (mode=deploy). It gates both Core's PROVISIONING_API_URL wiring
// and the provision-instance-queue init container, so the init container also runs against
// the deployed service.
func provisioningConfigured(p *otilmv1alpha1.Platform) bool {
	return provisioningAPIURL(p) != ""
}

// coreDerivesInstanceIDFromPodIndex reports whether Core takes its platform instance id from
// the pod ORDINAL: a StatefulSet Core, no explicit spec.core.instanceId, and a bundle whose
// wiring names the instance-id env var (2.19.0+). It is the single predicate the downward-API
// projection and the fail-fast init guard share, so they can never drift apart.
func coreDerivesInstanceIDFromPodIndex(p *otilmv1alpha1.Platform) bool {
	return p.Spec.Core.WorkloadType == otilmv1alpha1.WorkloadKindStatefulSet &&
		p.Spec.Core.InstanceID == nil &&
		wiringFor(p).PlatformInstanceIDEnv != ""
}

// coreInitContainers returns Core's ordered init containers. The instance-id guard runs FIRST
// when Core derives its id from the pod ordinal (it must fail the pod immediately, not after
// the dependency wait loops have burned minutes); wait-for-auth always follows; the
// provision-instance-queue init container is appended only on the proxy path
// (spec.proxy.enabled AND a provisioning API configured). All are SCC-hardened by the common
// BuildDeployment path.
func coreInitContainers(p *otilmv1alpha1.Platform) []corev1.Container {
	var inits []corev1.Container
	if coreDerivesInstanceIDFromPodIndex(p) {
		inits = append(inits, verifyInstanceIDInitContainer(p))
	}
	inits = append(inits, waitForAuthInitContainer(p))
	if p.Spec.Common.Proxy.Enabled && provisioningConfigured(p) {
		inits = append(inits, provisionInstanceQueueInitContainer(p))
	}
	return inits
}

// verifyInstanceIDInitContainer returns the "verify-instance-id" init container: a one-shot
// guard that FAILS the pod when the pod-index-derived instance id resolved to an empty value.
//
// WHY IT EXISTS: Kubernetes auto-stamps the apps.kubernetes.io/pod-index label only from
// v1.28. On an older cluster the downward-API reference silently resolves to "", Core falls
// back to deriving its id from the pod IP, and two pods whose IPv4 addresses share their last
// two octets then issue certificates with IDENTICAL serial numbers — a silent, unrecoverable
// data defect. The Helm chart catches this at TEMPLATE time from .Capabilities.KubeVersion;
// the operator has no such render-time cluster fact, so it checks the value itself, in the
// pod, before Core starts.
func verifyInstanceIDInitContainer(p *otilmv1alpha1.Platform) corev1.Container {
	w := wiringFor(p)
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, "curl", p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})
	script := fmt.Sprintf(`if [ -z "$%s" ]; then
echo "%s is empty: a StatefulSet core derives it from the apps.kubernetes.io/pod-index label, which Kubernetes auto-stamps only on v1.28+. On an older cluster core would fall back to deriving the id from the pod IP and could issue duplicate certificate serial numbers. Upgrade the cluster to v1.28+, or set spec.core.instanceId on a single-replica core." >&2
exit 1
fi
echo "instance id resolved"
`, w.PlatformInstanceIDEnv, w.PlatformInstanceIDEnv)
	return corev1.Container{
		Name:            "verify-instance-id",
		Image:           image,
		ImagePullPolicy: policy,
		Env: []corev1.EnvVar{{
			Name: w.PlatformInstanceIDEnv,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: podIndexLabelFieldPath},
			},
		}},
		Command: []string{binSh, "-c", script},
		// Read-only root filesystem: ENABLED. The guard writes nothing to disk.
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: readOnlyRootFS()},
	}
}

// Placeholders and fixed values of the per-instance queue registration request. The queue
// name and routing key are resolved IN THE POD from `hostname`, so the rendered JSON carries
// the literal ${HOSTNAME} placeholder that the script substitutes at runtime.
const (
	hostnamePlaceholder        = "${HOSTNAME}"
	queueExpiresProperty       = "x-expires"
	instanceQueueExpiresMillis = "1800000"
	// defaultInstanceQueueRoutingKey is the binding key each replica's queue is bound
	// under when spec.provisioning.deploy.routingKey is unset.
	defaultInstanceQueueRoutingKey = "proxymessage.*." + hostnamePlaceholder
	// jsonNull is the value an unusable queue-argument value degrades to, so the composed
	// body stays valid JSON and the builder stays a total function.
	jsonNull = "null"
)

// provisionQueueRequest is the POST body the provision-instance-queue init container sends to
// the provisioning API's /api/v1/queues endpoint. It exists so the body is composed by
// encoding/json — which escapes every value — instead of by string interpolation.
//
// Properties values are json.RawMessage because a queue argument is arbitrary JSON defined by
// the provisioning service, not by the operator. encoding/json COMPACTS every RawMessage it
// writes, so no argument value can introduce a newline into the one-line body — the property
// the quoted heredoc relies on to keep its terminator unforgeable, and the one that lets a
// single `read -r` capture the whole body; provisionQueueArgumentValue guarantees each raw
// value is valid JSON first, keeping the marshal infallible.
type provisionQueueRequest struct {
	Name       string                     `json:"name"`
	Exchange   string                     `json:"exchange"`
	RoutingKey string                     `json:"routingKey"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// provisionQueueRoutingKey resolves the binding key of the per-instance queue:
// spec.provisioning.deploy.routingKey when set, else the operator default
// "proxymessage.*.${HOSTNAME}". The ${HOSTNAME} token in either is substituted from the pod's
// own hostname at runtime.
func provisionQueueRoutingKey(p *otilmv1alpha1.Platform) string {
	if d := p.Spec.Provisioning.Deploy; d != nil && d.RoutingKey != "" {
		return d.RoutingKey
	}
	return defaultInstanceQueueRoutingKey
}

// provisionQueueProperties resolves the queue arguments sent with the registration request:
// spec.provisioning.deploy.queueArguments when set — REPLACING the default outright, since a
// provisioning service ignores arguments it does not recognise and falls back to its own
// defaults — else the operator default (x-expires: 1800000). A repeated name is rejected at
// admission by the field's list-map key; should one still reach here, the last entry wins and
// the rendered order stays stable (encoding/json sorts map keys).
func provisionQueueProperties(p *otilmv1alpha1.Platform) map[string]json.RawMessage {
	d := p.Spec.Provisioning.Deploy
	if d == nil || len(d.QueueArguments) == 0 {
		return map[string]json.RawMessage{queueExpiresProperty: json.RawMessage(instanceQueueExpiresMillis)}
	}
	props := make(map[string]json.RawMessage, len(d.QueueArguments))
	for _, a := range d.QueueArguments {
		props[a.Name] = provisionQueueArgumentValue(a.Value)
	}
	return props
}

// provisionQueueArgumentValue returns a queue argument's value as a raw JSON message, falling
// back to JSON null for an absent or non-parseable value. It keeps the request marshal
// infallible (encoding/json rejects an invalid RawMessage), so the builder never has to
// return an error for a value admission already validated.
func provisionQueueArgumentValue(v apiextensionsv1.JSON) json.RawMessage {
	if len(v.Raw) > 0 && json.Valid(v.Raw) {
		return json.RawMessage(v.Raw)
	}
	return json.RawMessage(jsonNull)
}

// provisionInstanceQueueInitContainer returns the "provision-instance-queue" init
// container rendered on Core when proxy support is enabled and a provisioning API
// is configured (see coreInitContainers). It registers this pod's own per-instance
// AMQP queue with the provisioning API by POSTing to <apiURL>/api/v1/queues, naming
// the queue after the pod hostname, and retries the TRANSIENT failures until the call
// succeeds (so the queue exists before Core starts consuming) while failing fast on a
// permanent one. The optional X-API-Key header is
// sourced via secretKeyRef from the provisioning Secret (spec.provisioning.
// apiKeySecretRef) — never inlined — and the request omits the header when no key
// is configured. The builder SCC-hardens it.
//
// The request body's exchange, routing key and queue arguments are the configurable trio at
// spec.provisioning.deploy ({exchange,routingKey,queueArguments}); each falls back to the
// operator default when unset (see provisionQueueRoutingKey / provisionQueueProperties).
//
// The proxy exchange the queue is bound to is VERSION-DEPENDENT (2.18.0: czertainly-proxy;
// 2.19.0: ilm-proxy), so it is rendered from provisioningExchange — the SAME resolution the
// provisioning builder uses (spec.provisioning.deploy.exchange override, else the bundle
// wiring's DefaultExchange). Hard-coding it would make this loop retry forever against a
// platform whose bundle declares a different exchange.
//
// SECURITY: the request body is built in Go with encoding/json (so every value is correctly
// JSON-escaped) and emitted into the script inside a QUOTED heredoc (<<'EOF'), on which the
// shell performs NO parameter expansion and NO command substitution. A spec-supplied exchange,
// routing key or queue-argument value is therefore inert DATA: it can neither run a command in
// this container (which holds the provisioning API key) nor reshape the JSON.
//
// The heredoc feeds `read -r` rather than sitting inside a command substitution
// ($(cat <<'EOF' ... EOF)) — the same shape the Core bootstrap scripts use, for the same reason:
// the nested form is mis-parsed by bash 3.2 when the body holds an unbalanced quote, and a
// queueArguments value is arbitrary JSON, so `a'b` produces exactly that. A single `read -r` is
// enough because encoding/json emits the body on ONE line, and -r keeps every backslash escape
// verbatim.
//
// Only the fixed ${HOSTNAME} placeholder is substituted at runtime, from `hostname` — a trusted
// value, never from the CR. The CRD's charset pattern on the exchange field is defence in depth.
// The endpoint is passed with curl's --url flag so a spec.provisioning.apiURL beginning with "-"
// is taken as a URL rather than parsed as a curl option.
func provisionInstanceQueueInitContainer(p *otilmv1alpha1.Platform) corev1.Container {
	b := resolveBundle(p)
	w := b.Wiring
	image, policy := common.ResolveImage(b.Lookup, "curl", p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})

	// PROVISIONING_API_URL is inline (non-secret); the API key, when configured, is
	// referenced via secretKeyRef from the user-provided provisioning Secret so the
	// value never lands in the rendered manifest, env value, or logs.
	env := []corev1.EnvVar{{Name: w.ProvisioningURLEnv, Value: provisioningAPIURL(p)}}
	if ref := provisioningAPIKeySecretRef(p); ref != "" {
		env = append(env, corev1.EnvVar{
			Name: w.ProvisioningAPIKey.Env,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: ref},
					Key:                  ProvisioningAPIKeyKey(p),
				},
			},
		})
	}

	// The POST body, encoded in Go so EVERY value is correctly JSON-escaped. Every value is a
	// plain string or a RawMessage already proven valid JSON, so json.Marshal is infallible
	// and the builder stays a pure, error-free function. The output is a SINGLE line
	// (encoding/json escapes any newline inside a value and compacts every RawMessage), so no
	// value can produce a line that prematurely terminates the heredoc below — which is also
	// why the single `read -r` there captures the whole body.
	body, _ := json.Marshal(provisionQueueRequest{
		Name:       hostnamePlaceholder,
		Exchange:   provisioningExchange(p, w.Provisioning),
		RoutingKey: provisionQueueRoutingKey(p),
		Properties: provisionQueueProperties(p),
	})

	// Per-pod, self-idempotent loop: read the JSON body with `read -r` from a QUOTED heredoc
	// (the shell expands nothing inside it, and the un-nested form sidesteps the bash 3.2
	// command-substitution quirk — see the SECURITY note above), substitute the queue name from
	// this pod's own hostname, include the X-API-Key header only when PROVISIONING_API_KEY is
	// set, and POST. Both curl branches send the one composed body under the same timeouts.
	//
	// The outcome is CLASSIFIED, not blindly retried: only a transient transport failure and a
	// transient status (408, 429, any 5xx) sleep and try again. A permanent transport failure
	// (curl exit 1 unsupported protocol, 3 malformed URL, 60 TLS verification) and any other
	// 4xx print the status and the response body and exit non-zero — so a wrong API key or a
	// wrong URL fails the pod visibly in `kubectl logs` instead of hanging Core's startup
	// behind an init container that loops forever on a permanent error. This is plain POSIX sh
	// (no bashisms, no pipefail), so it behaves identically under dash and busybox ash.
	script := fmt.Sprintf(`HOSTNAME=$(hostname)
read -r BODY <<'%[1]s'
%[2]s
%[1]s
BODY=$(printf '%%s' "$BODY" | sed "s/\${HOSTNAME}/${HOSTNAME}/g")
while true; do
  if [ -n "${PROVISIONING_API_KEY:-}" ]; then
    OUT=$(curl -sS --connect-timeout 5 --max-time 30 -w '\n%%{http_code}' -X POST --url "${PROVISIONING_API_URL}/api/v1/queues" \
      -H "Content-Type: application/json" \
      -H "X-API-Key: ${PROVISIONING_API_KEY}" \
      -d "${BODY}")
    RC=$?
  else
    OUT=$(curl -sS --connect-timeout 5 --max-time 30 -w '\n%%{http_code}' -X POST --url "${PROVISIONING_API_URL}/api/v1/queues" \
      -H "Content-Type: application/json" \
      -d "${BODY}")
    RC=$?
  fi
  if [ "$RC" -ne 0 ]; then
    case "$RC" in
      1|3|60)
        echo "Provisioning request to ${PROVISIONING_API_URL} failed permanently (curl exit ${RC}), not retrying"
        exit 1
        ;;
      *)
        echo "Provisioning request to ${PROVISIONING_API_URL} failed (curl exit ${RC}), retrying in 5s..."
        sleep 5
        continue
        ;;
    esac
  fi
  CODE=$(printf '%%s' "$OUT" | tail -n 1)
  RESPONSE=$(printf '%%s' "$OUT" | sed '$d')
  case "$CODE" in
    2??)
      echo "Instance queue provisioned for ${HOSTNAME}"
      exit 0
      ;;
    408|429|5??)
      echo "Provisioning API at ${PROVISIONING_API_URL} not ready (HTTP ${CODE}), retrying in 5s..."
      sleep 5
      ;;
    *)
      echo "Queue provisioning failed (HTTP ${CODE}): ${RESPONSE}"
      exit 1
      ;;
  esac
done
`, heredocMarker, body)
	return corev1.Container{
		Name:            "provision-instance-queue",
		Image:           image,
		ImagePullPolicy: policy,
		Env:             env,
		Command:         []string{binSh, "-c", script},
		// Read-only root filesystem: ENABLED. A curl/POST retry loop writes nothing to disk.
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: readOnlyRootFS()},
	}
}

// The operator-owned Services the wait-for-auth init container polls, in the two groups the
// script polls them in: the ones before the broker loop and the ones after it.
//
// THEY ARE A LIST RATHER THAN LITERALS IN THE SCRIPT because they are a CONTRACT, not a detail
// of one container. Every Service named here is a workload that has to be running before Core's
// pod can ever become Ready, so anything that stops a platform workload — the messaging
// migration's fence above all — has to know the set. waitForAuthInitContainer and
// CoreInitServiceDependencies are both built from these, so the poll and the set can never
// drift apart.
var (
	waitForAuthServicesBeforeBroker = []string{authName, authOPAPoliciesName}
	waitForAuthServicesAfterBroker  = []string{schedulerName}
)

// serviceWaitLoops renders one nc-poll loop per Service name, chained with && in the order
// given. The names are compile-time constants, so they are safe to interpolate — unlike the
// broker's coordinates, which mqWaitLoop reads from quoted env vars instead.
func serviceWaitLoops(names []string) string {
	loops := make([]string, 0, len(names))
	for _, name := range names {
		loops = append(loops, fmt.Sprintf("while ! nc -z %s %d; do sleep 1; done", name, depServicePort))
	}
	return strings.Join(loops, " &&\n")
}

// CoreInitServiceDependencies names the platform workloads Core's init containers block on, so
// Core's pod cannot reach Ready while any of them is stopped: the Services wait-for-auth polls,
// plus — on the proxy path — the bundled provisioning service the provision-instance-queue init
// container POSTs to.
//
// It exists so a caller that STOPS platform workloads can ask which ones Core starts behind. The
// messaging migration's cutover is that caller: it fences the platform's message producers, and
// rolling Core while one of these is held at zero replicas is a deadlock with no deadline behind
// it.
//
// THE BROKER IS DELIBERATELY ABSENT even though wait-for-auth polls it too: it is not a workload
// this operator scales — managed, it is an upstream operator's cluster; external, it is somebody
// else's host entirely — so it can never be one of the workloads a caller is holding down.
// An EXTERNAL provisioning API is absent for the same reason.
func CoreInitServiceDependencies(p *otilmv1alpha1.Platform) []string {
	deps := make([]string, 0, len(waitForAuthServicesBeforeBroker)+len(waitForAuthServicesAfterBroker)+1)
	deps = append(deps, waitForAuthServicesBeforeBroker...)
	deps = append(deps, waitForAuthServicesAfterBroker...)
	if p.Spec.Common.Proxy.Enabled && provisioningConfigured(p) && ProvisioningDeploy(p) {
		deps = append(deps, provisioningName)
	}
	return deps
}

// waitForAuthInitContainer returns the "wait-for-auth" init
// container: a shell loop that blocks until auth, auth-opa-policies, the
// message broker, and scheduler are all reachable. The builder SCC-hardens
// it automatically. (The proxy-path provision-instance-queue init container is
// rendered separately by coreInitContainers.)
//
// The Service loops are rendered from waitForAuthServices{Before,After}Broker — the same lists
// CoreInitServiceDependencies answers from — so a Service added to the poll is a Service every
// caller that has to keep Core startable learns about. The broker loop is mqWaitLoop, whose
// coordinates arrive as env values because in external mode the host is CR-supplied (see the
// mqWaitLoop security note).
func waitForAuthInitContainer(p *otilmv1alpha1.Platform) corev1.Container {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, "curl", p.Spec.Common.Image, otilmv1alpha1.ImageSpec{})
	// Resolve the broker host/port mode-agnostically so a managed broker waits on the
	// RabbitMQ Service, an external one on the caller's host.
	mq := ResolveMessagingConnection(p)
	script := fmt.Sprintf(`%s &&
echo "auth service seems to be started" &&
%s &&
%s &&
echo "messaging and scheduler service seems to be started"
`,
		serviceWaitLoops(waitForAuthServicesBeforeBroker),
		mqWaitLoop,
		serviceWaitLoops(waitForAuthServicesAfterBroker),
	)
	return corev1.Container{
		Name:            "wait-for-auth",
		Image:           image,
		ImagePullPolicy: policy,
		Env:             messagingWaitEnv(mq),
		Command:         []string{binSh, "-c", script},
		// Read-only root filesystem: ENABLED. A pure nc-poll loop writes nothing to disk.
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: readOnlyRootFS()},
	}
}
