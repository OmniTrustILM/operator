/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package convert implements the best-effort Helm-values → Platform CR conversion. It is
// exported so any module can reuse it (cmd/values2platform is one such consumer). It converts
// an umbrella Helm chart values.yaml into an equivalent otilm.com/v1alpha1 Platform CR.
//
// SECURITY: the tool NEVER copies a plaintext secret out of values into the CR. For
// every inline secret in values (database/messaging passwords, the trusted-cert and
// admin-cert PEM bundles, the Keycloak client secret, the provisioning API key, SMTP
// credentials) it emits the CR's Secret REFERENCE (e.g. database.credentials.secretRef)
// plus a top-of-file "# TODO: create Secret" block with a ready-to-edit
// `kubectl create secret` line. Secret creation stays a human decision; the tool only
// scaffolds it. MCP's client secret has a reference only with an external identity, where the
// converter copies the name of the Secret that holds it. With the managed identity the operator
// generates the secret, so neither it nor a reference to it is copied, and the report says why.
//
// The conversion is best-effort, not 100%: values with no CR equivalent are flagged with
// "# UNMAPPED: <values path>" and customization the user must move by hand (connectors,
// per-connector SMTP, raw sidecars/initContainers) with "# TODO(customization): ...".
package convert

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// modeExternal is the infra connection mode the converter always emits (external): a
// values.yaml describes a chart install, which the operator adopts by REFERENCING the
// running infra in external mode; opting into managed mode is a separate human decision.
const modeExternal = "external"

// modeManaged is the mode of an operator-provisioned Keycloak: what the chart's bundled Keycloak
// (global.keycloak.enabled) converts to, and the Keycloak MCP's managed identity signs in through.
const modeManaged = "managed"

// coreImageKey is the umbrella values top-level key that carries Core's image (the chart's
// `image` block is Core's image; every other component has its own block).
const coreImageKey = "image"

// Default in-Secret keys, mirroring the operator's wiring profile (pkg/bom). The
// converter emits these as the keys the user must put into the Secrets it scaffolds, so a
// freshly-created Secret needs no key override on the CR.
const (
	defaultUsernameKey    = "username"
	defaultPasswordKey    = "password" //nolint:gosec // in-Secret KEY name, not a credential value
	defaultTrustedCAKey   = "ca.crt"
	defaultAdminCertKey   = "tls.crt"
	defaultAdminKeyKey    = "tls.key"
	defaultProvAPIKeyKey  = "provisioningApiKey"
	defaultKeycloakSecKey = "clientSecret" //nolint:gosec // in-Secret KEY name, not a credential value
	// defaultMCPSecretKey is the key MCP reads an external identity's client secret from when
	// spec.mcp.identity.clientSecretKey is empty.
	defaultMCPSecretKey = "client-secret" //nolint:gosec // in-Secret KEY name, not a credential value
)

// Default names of the Secrets the converter scaffolds references to. Exported so callers
// that materialize those Secrets can reuse the names instead of duplicating string literals.
const (
	DefaultDatabaseSecretName     = "ilm-db"
	DefaultMessagingSecretName    = "ilm-messaging"  //nolint:gosec // Secret object NAME, not a credential
	DefaultTrustedCASecretName    = "ilm-trusted-ca" //nolint:gosec // Secret object NAME, not a credential
	DefaultAdminCertSecretName    = "ilm-admin-cert" //nolint:gosec // Secret object NAME, not a credential
	DefaultProvisioningSecretName = "ilm-provisioning"
	DefaultKeycloakSecretName     = "ilm-keycloak-client" //nolint:gosec // Secret object NAME, not a credential
)

// secretTODO describes one Kubernetes Secret the user must create by hand before applying
// the converted CR. The converter collects these (one per inline secret found in values)
// and renders them as a TODO header block with a ready-to-edit kubectl line.
type secretTODO struct {
	// name is the Secret name the CR references.
	name string
	// keys are the in-Secret keys the operator reads (the wiring-profile defaults).
	keys []string
	// reason is the values path / purpose this Secret replaces, for the comment.
	reason string
	// kubectl is a ready-to-edit `kubectl create secret` line (placeholders, no plaintext).
	kubectl string
}

// Result holds the converted Platform CR plus the human-facing scaffolding the
// tool emits around it: the secret-creation TODOs, the unmapped-values notes, and the
// customization TODOs. Render assembles them into the final YAML document.
type Result struct {
	// Platform is the typed CR built from the values.
	Platform *otilmv1alpha1.Platform
	// Namespace is the namespace the CR is placed in (and the secrets are created in).
	Namespace string
	// secretTODOs lists the Secrets the user must create (never auto-filled with plaintext).
	secretTODOs []secretTODO
	// unmapped lists values paths with no CR equivalent ("# UNMAPPED: <path>").
	unmapped []string
	// customization lists values the user must move by hand ("# TODO(customization): ...").
	customization []string
}

// vals is a convenience alias for the loosely-typed decoded values tree.
type vals = map[string]interface{}

// Convert maps an umbrella chart values tree (already decoded from YAML) into a Platform
// CR plus the secret/unmapped/customization scaffolding. name/namespace name the emitted
// CR (and the namespace the scaffolded Secrets are created in).
//
// It is best-effort: recognized values become typed CR fields, inline secrets become
// Secret references + TODOs (never plaintext), and unrecognized values are flagged rather
// than silently dropped.
func Convert(values vals, name, namespace string) *Result {
	r := &Result{
		Namespace: namespace,
		Platform: &otilmv1alpha1.Platform{
			TypeMeta: metav1.TypeMeta{
				APIVersion: otilmv1alpha1.GroupVersion.String(),
				Kind:       "Platform",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		},
	}
	spec := &r.Platform.Spec

	global := mapOf(values["global"])

	r.mapImage(values, spec)
	r.mapHostName(values, global, spec)
	r.mapDatabase(global, spec)
	r.mapMessaging(global, spec)
	r.mapKeycloak(global, spec)
	r.mapTrustedCertificates(global, spec)
	r.mapProxy(global, spec)
	r.mapLogging(values, spec)
	r.mapAdditionalEnv(values, spec)
	r.mapEdge(values, spec)
	r.mapGateway(values, global, spec)
	r.mapRegisterAdmin(values, spec)
	r.mapProvisioning(global, spec)
	r.mapComponents(values, spec)
	r.mapCoreFeatures(values, global, spec)
	r.mapJavaOpts(values, spec)
	r.flagConnectors(values)
	r.flagGlobalCustomization(global)
	r.flagUnmappedTopLevel(values)

	sort.Strings(r.unmapped)
	sort.Strings(r.customization)
	return r
}

// mapImage maps the umbrella image block (registry/repository/name/tag/pullPolicy) and
// global.image.pullSecrets onto spec.common.image (the shared image for all components).
func (r *Result) mapImage(values vals, spec *otilmv1alpha1.PlatformSpec) {
	img := mapOf(values["image"])
	spec.Common.Image.Registry = str(img["registry"])
	spec.Common.Image.Repository = str(img["repository"])
	spec.Common.Image.Name = str(img["name"])
	spec.Common.Image.Tag = str(img["tag"])
	if pp := str(img["pullPolicy"]); pp != "" {
		spec.Common.Image.PullPolicy = pp
	}
	// global.image.pullSecrets -> spec.image.pullSecrets
	gi := mapOf(mapOf(values["global"])["image"])
	for _, ps := range slice(gi["pullSecrets"]) {
		if s := str(ps); s != "" {
			spec.Common.Image.PullSecrets = append(spec.Common.Image.PullSecrets, s)
		}
	}
	// image.probes (chart per-image probe toggles) has no shared-image equivalent; probes
	// are a per-component override on the operator side.
	if _, ok := img["probes"]; ok {
		r.customization = append(r.customization,
			"image.probes: per-image probe toggles map to per-component spec.<component>.probes — set them on the component(s) you need")
	}
}

// mapHostName maps the chart's canonical public FQDN (global.hostName, or the top-level
// hostName fallback) onto spec.common.hostName — the single source of truth from which the
// edge host/cert, Keycloak KC_HOSTNAME, the OIDC redirect/web-origin/post-logout URIs, and
// the gateway CORS origin all derive (edge.host overrides it per-edge via PlatformHost).
func (r *Result) mapHostName(values, global vals, spec *otilmv1alpha1.PlatformSpec) {
	host := str(global["hostName"])
	if host == "" {
		host = str(values["hostName"])
	}
	if host != "" {
		spec.Common.HostName = host
	}
}

// mapDatabase maps global.database onto spec.database (external mode). The password is
// NEVER copied: it becomes a credentials.secretRef + a create-secret TODO.
func (r *Result) mapDatabase(global vals, spec *otilmv1alpha1.PlatformSpec) {
	db := mapOf(global["database"])
	spec.Database.Mode = modeExternal
	spec.Database.Host = str(db["host"])
	spec.Database.Name = str(db["name"])
	if p, ok := intOf(db["port"]); ok {
		spec.Database.Port = p
	}
	// Credentials are referenced, never inlined.
	_, hasUser := db["username"]
	_, hasPass := db["password"]
	if hasUser || hasPass || spec.Database.Host != "" {
		spec.Database.Credentials = &otilmv1alpha1.CredentialsRef{SecretRef: DefaultDatabaseSecretName}
		if hasPass || hasUser {
			r.addSecretTODO(secretTODO{
				name:   DefaultDatabaseSecretName,
				keys:   []string{defaultUsernameKey, defaultPasswordKey},
				reason: "database credentials (was global.database.username/password)",
				kubectl: fmt.Sprintf(
					"kubectl create secret generic %s -n %s --from-literal=username='<DB_USER>' --from-literal=password='<DB_PASSWORD>'",
					DefaultDatabaseSecretName, r.Namespace),
			})
		}
	}
	// pgBouncer toggle (global.database.pgBouncer.enabled) maps to spec.database.pgBouncer.managed
	// — the operator's sole pooler switch: managed=true renders a CloudNativePG Pooler for a
	// managed DB. (Setting it on an external DB is harmless; the pooler render is managed-DB only.)
	if pb := mapOf(db["pgBouncer"]); len(pb) > 0 {
		if en, ok := boolOf(pb["enabled"]); ok && en {
			spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: true}
		}
	}
}

// mapMessaging maps global.messaging onto spec.messaging (external mode). Passwords are
// referenced, never inlined. remoteAccess maps to the gateway (handled in mapGateway).
func (r *Result) mapMessaging(global vals, spec *otilmv1alpha1.PlatformSpec) {
	ms := mapOf(global["messaging"])
	spec.Messaging.Mode = modeExternal
	spec.Messaging.BrokerType = "rabbitmq"
	spec.Messaging.Host = str(ms["host"])
	if vh := str(ms["virtualHost"]); vh != "" {
		spec.Messaging.VirtualHost = vh
	}
	if p, ok := intOf(ms["port"]); ok {
		spec.Messaging.Port = p
	}
	_, hasUser := ms["username"]
	_, hasPass := ms["password"]
	if hasUser || hasPass {
		spec.Messaging.Credentials = &otilmv1alpha1.CredentialsRef{SecretRef: DefaultMessagingSecretName}
		r.addSecretTODO(secretTODO{
			name:   DefaultMessagingSecretName,
			keys:   []string{defaultUsernameKey, defaultPasswordKey},
			reason: "messaging credentials (was global.messaging.username/password)",
			kubectl: fmt.Sprintf(
				"kubectl create secret generic %s -n %s --from-literal=username='<MQ_USER>' --from-literal=password='<MQ_PASSWORD>'",
				DefaultMessagingSecretName, r.Namespace),
		})
	} else if spec.Messaging.Host != "" {
		// External broker host given without inline creds — still needs a ref.
		spec.Messaging.Credentials = &otilmv1alpha1.CredentialsRef{SecretRef: DefaultMessagingSecretName}
	}
	// External mode REQUIRES host + credentials.secretRef (CRD XValidation). The chart's
	// bundled broker has neither (it relies on the in-chart messaging-rabbitmq subchart), so
	// flag the decision instead of emitting a CR the apiserver will reject.
	if spec.Messaging.Host == "" {
		r.customization = append(r.customization,
			"messaging: no external broker host found in values (the chart bundled RabbitMQ). Choose ONE: set messaging.host + messaging.credentials.secretRef to your external broker, OR set messaging.mode=managed and fill messaging.managed (RabbitMQ operators). The emitted external-mode block is INCOMPLETE until you do.")
	}
}

// mapKeycloak maps global.keycloak onto spec.keycloak. The chart's clientSecret is an
// inline secret -> a TODO (the operator wires the OIDC client secret by reference / via
// managed-Keycloak readback; external mode configures OIDC in the app DB).
func (r *Result) mapKeycloak(global vals, spec *otilmv1alpha1.PlatformSpec) {
	kc := mapOf(global["keycloak"])
	if len(kc) == 0 {
		return
	}
	enabled, _ := boolOf(kc["enabled"])
	if !enabled {
		return
	}
	// The chart's global.keycloak is the INTERNAL bundled Keycloak; on the operator side
	// that is keycloak.mode=managed (Keycloak Operator). The CR carries only mode/realm
	// here; the managed sizing is a deliberate manual decision.
	spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: modeManaged, Realm: "ilm"}
	r.customization = append(r.customization,
		"global.keycloak.enabled=true -> keycloak.mode=managed: fill keycloak.managed (instances/version/storage) per docs/design/examples/platform_managed_keycloak.yaml (Keycloak Operator must be installed)")
	if _, ok := kc["clientSecret"]; ok {
		r.addSecretTODO(secretTODO{
			name:   DefaultKeycloakSecretName,
			keys:   []string{defaultKeycloakSecKey},
			reason: "Keycloak OIDC client secret (was global.keycloak.clientSecret) — managed Keycloak generates this and the operator reads it back; create only if you wire an external client secret",
			kubectl: fmt.Sprintf(
				"kubectl create secret generic %s -n %s --from-literal=clientSecret='<KEYCLOAK_CLIENT_SECRET>'",
				DefaultKeycloakSecretName, r.Namespace),
		})
	}
}

// mapTrustedCertificates maps global.trusted.certificates (an inline PEM bundle) onto
// spec.common.trustedCertificates.secretRef + a create-secret TODO. The PEM is NEVER inlined.
func (r *Result) mapTrustedCertificates(global vals, spec *otilmv1alpha1.PlatformSpec) {
	tr := mapOf(global["trusted"])
	if _, ok := tr["certificates"]; !ok {
		return
	}
	spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: DefaultTrustedCASecretName}
	r.addSecretTODO(secretTODO{
		name:   DefaultTrustedCASecretName,
		keys:   []string{defaultTrustedCAKey},
		reason: "trusted CA bundle (was global.trusted.certificates, inline PEM)",
		kubectl: fmt.Sprintf(
			"kubectl create secret generic %s -n %s --from-file=ca.crt=./trusted-ca.pem",
			DefaultTrustedCASecretName, r.Namespace),
	})
}

// mapProxy maps the chart's global.httpProxy/httpsProxy/noProxy onto spec.common.proxy.
func (r *Result) mapProxy(global vals, spec *otilmv1alpha1.PlatformSpec) {
	http := str(global["httpProxy"])
	https := str(global["httpsProxy"])
	no := str(global["noProxy"])
	if http == "" && https == "" && no == "" {
		return
	}
	spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true, HTTP: http, HTTPS: https, NoProxy: no}
}

// mapLogging maps the top-level logging.level and logging.format onto spec.common.logging.
// The chart's logging.audit toggle has no CR field and is flagged unmapped.
func (r *Result) mapLogging(values vals, spec *otilmv1alpha1.PlatformSpec) {
	lg := mapOf(values["logging"])
	if len(lg) == 0 {
		return
	}
	if lvl := str(lg["level"]); lvl != "" {
		spec.Common.Logging.Level = lvl
	}
	// The chart trims and lower-cases the format; the CRD enum accepts only the lower-case names.
	if f := strings.ToLower(strings.TrimSpace(str(lg["format"]))); f != "" {
		spec.Common.Logging.Format = f
	}
	if _, ok := lg["audit"]; ok {
		r.unmapped = append(r.unmapped, "logging.audit (no Platform CR field; audit logging is a platform-config concern, not operator-modeled)")
	}
}

// mapAdditionalEnv maps the top-level additionalEnv.variables onto spec.additionalEnv
// (non-sensitive inline env applied to every component).
func (r *Result) mapAdditionalEnv(values vals, spec *otilmv1alpha1.PlatformSpec) {
	ae := mapOf(values["additionalEnv"])
	for _, v := range slice(ae["variables"]) {
		m := mapOf(v)
		name := str(m["name"])
		if name == "" {
			continue
		}
		spec.AdditionalEnv = append(spec.AdditionalEnv, otilmv1alpha1.EnvVar{Name: name, Value: str(m["value"])})
	}
}

// mapEdge maps the chart's ingress block (+ letsEncrypt) onto spec.edge. The public FQDN
// is NOT duplicated on edge.host: the chart's single global.hostName lives on
// spec.common.hostName (mapHostName), which the edge inherits via PlatformHost. Set
// edge.host only when an edge needs a host distinct from the platform-wide hostName.
func (r *Result) mapEdge(values vals, spec *otilmv1alpha1.PlatformSpec) {
	ing := mapOf(values["ingress"])
	if len(ing) == 0 {
		return
	}
	enabled, _ := boolOf(ing["enabled"])
	edge := &otilmv1alpha1.EdgeSpec{Enabled: enabled, Type: "ingress"}
	if cls := str(ing["class"]); cls != "" {
		edge.ClassName = ptrTo(cls)
	}
	if ann := stringMap(ing["annotations"]); len(ann) > 0 {
		edge.Annotations = ann
	}
	// ingress.certificate.source -> edge.tls.source (chart "external" == operator "secret").
	cert := mapOf(ing["certificate"])
	if src := str(cert["source"]); src != "" {
		tls := &otilmv1alpha1.EdgeTLSSpec{}
		switch src {
		case "letsencrypt", "letsEncrypt":
			tls.Source = "letsEncrypt"
			le := mapOf(values["letsEncrypt"])
			tls.LetsEncrypt = &otilmv1alpha1.LetsEncryptSpec{
				Email:       str(le["email"]),
				Environment: str(le["environment"]),
			}
		case "internal":
			tls.Source = "internal"
		case "external":
			// Chart "external" == bring-your-own TLS Secret on the operator side.
			tls.Source = "secret"
			r.customization = append(r.customization,
				"ingress.certificate.source=external -> edge.tls.source=secret: set edge.tls.secretRef to your kubernetes.io/tls Secret name")
		default:
			tls.Source = src
		}
		edge.TLS = tls
	}
	// The canonical FQDN lives on spec.common.hostName (mapHostName); an enabled edge uses it
	// automatically (PlatformHost = edge.host | common.hostName) and the apiserver accepts an
	// enabled edge with only common.hostName set. So edge.host is left empty — set it by hand
	// only to give the edge a host that differs from the platform-wide hostName.
	spec.Edge = edge
	if spec.Common.HostName == "" && enabled {
		r.customization = append(r.customization,
			"a public FQDN is REQUIRED when edge.enabled=true but no global.hostName was found — set common.hostName (or edge.host)")
	}
}

// mapGateway maps apiGateway.* onto spec.gateway, and global.messaging.remoteAccess onto
// spec.messaging.management.expose (the /mq toggle moved to messaging).
func (r *Result) mapGateway(values, global vals, spec *otilmv1alpha1.PlatformSpec) {
	ag := mapOf(values["apiGateway"])
	gw := otilmv1alpha1.GatewaySpec{}
	changed := false
	changed = mapGatewayTrustedIPs(ag, &gw) || changed
	changed = r.mapGatewayLogging(ag, &gw) || changed
	changed = mapGatewayCors(ag, &gw) || changed
	// global.messaging.remoteAccess -> messaging.management.expose (the /mq toggle is a
	// messaging property now; the gateway only renders the route).
	if ra, ok := boolOf(mapOf(global["messaging"])["remoteAccess"]); ok && ra {
		spec.Messaging.Management.Expose = true
	}
	if ha := mapOf(ag["hostAliases"]); len(ha) > 0 {
		r.unmapped = append(r.unmapped, "apiGateway.hostAliases (host-alias injection for split-horizon DNS is operator-derived from edge.host / managed Keycloak; no direct CR field)")
	}
	if changed {
		spec.Gateway = gw
	}
}

// mapGatewayTrustedIPs maps apiGateway.trustedIps (a comma-separated list) onto gw.TrustedIPs.
// Returns true if it set any field.
func mapGatewayTrustedIPs(ag vals, gw *otilmv1alpha1.GatewaySpec) bool {
	ti := str(ag["trustedIps"])
	if ti == "" {
		return false
	}
	for _, part := range strings.Split(ti, ",") {
		if p := strings.TrimSpace(part); p != "" {
			gw.TrustedIPs = append(gw.TrustedIPs, p)
		}
	}
	return true
}

// mapGatewayLogging maps apiGateway.logging onto gw.Logging, recording apiGateway.logging.level
// as unmapped. Returns true if it set any field.
func (r *Result) mapGatewayLogging(ag vals, gw *otilmv1alpha1.GatewaySpec) bool {
	lg := mapOf(ag["logging"])
	if len(lg) == 0 {
		return false
	}
	changed := false
	if req, ok := boolOf(lg["request"]); ok && req {
		gw.Logging = otilmv1alpha1.GatewayLoggingSpec{Request: true}
		changed = true
	}
	if _, ok := lg["level"]; ok {
		r.unmapped = append(r.unmapped, "apiGateway.logging.level (Kong log level is not a Platform CR field; only apiGateway.logging.request maps, to gateway.logging.request)")
	}
	return changed
}

// mapGatewayCors maps apiGateway.cors onto gw.Cors. Returns true if it set any field.
func mapGatewayCors(ag vals, gw *otilmv1alpha1.GatewaySpec) bool {
	cors := mapOf(ag["cors"])
	if len(cors) == 0 {
		return false
	}
	if en, ok := boolOf(cors["enabled"]); ok && en {
		gw.Cors = otilmv1alpha1.GatewayCorsSpec{
			Enabled:        true,
			Origins:        stringSlice(cors["origins"]),
			ExposedHeaders: stringSlice(cors["exposedHeaders"]),
		}
		return true
	}
	return false
}

// mapRegisterAdmin maps the chart's registerAdmin (admin client cert) onto
// spec.registerAdmin. The admin cert PEM is NEVER inlined: it becomes a secretRef + TODO.
func (r *Result) mapRegisterAdmin(values vals, spec *otilmv1alpha1.PlatformSpec) {
	ra := mapOf(values["registerAdmin"])
	if len(ra) == 0 {
		return
	}
	enabled, _ := boolOf(ra["enabled"])
	if !enabled {
		return
	}
	admin := mapOf(ra["admin"])
	// The chart only ever provisioned a client-CERTIFICATE admin, so it maps onto the
	// certificate method (the password method is a new, operator-only capability with no
	// chart equivalent — added manually post-migration if wanted).
	out := &otilmv1alpha1.RegisterAdminSpec{
		Enabled:  true,
		Username: str(admin["username"]),
		Name:     str(admin["name"]),
		Email:    str(admin["email"]),
		Certificate: &otilmv1alpha1.AdminCertificateSpec{
			Enabled:   ptrTo(true),
			Source:    "provided",
			SecretRef: ptrTo(DefaultAdminCertSecretName),
		},
	}
	spec.RegisterAdmin = out
	if _, ok := admin["certificate"]; ok {
		r.addSecretTODO(secretTODO{
			name:   DefaultAdminCertSecretName,
			keys:   []string{defaultAdminCertKey, defaultAdminKeyKey},
			reason: "admin client certificate (was registerAdmin.admin.certificate, inline PEM) — kubernetes.io/tls Secret",
			kubectl: fmt.Sprintf(
				"kubectl create secret tls %s -n %s --cert=./admin.crt --key=./admin.key",
				DefaultAdminCertSecretName, r.Namespace),
		})
	}
}

// mapProvisioning maps the chart's global.provisioning (Core's remote-proxy provisioning)
// onto spec.provisioning. The API key is NEVER inlined: it becomes a secretRef + TODO.
func (r *Result) mapProvisioning(global vals, spec *otilmv1alpha1.PlatformSpec) {
	pr := mapOf(global["provisioning"])
	if len(pr) == 0 {
		return
	}
	prov := &otilmv1alpha1.ProvisioningSpec{Mode: modeExternal, APIURL: str(pr["apiUrl"])}
	if _, ok := pr["apiKey"]; ok {
		prov.APIKeySecretRef = DefaultProvisioningSecretName
		r.addSecretTODO(secretTODO{
			name:   DefaultProvisioningSecretName,
			keys:   []string{defaultProvAPIKeyKey},
			reason: "provisioning API key (was global.provisioning.apiKey)",
			kubectl: fmt.Sprintf(
				"kubectl create secret generic %s -n %s --from-literal=provisioningApiKey='<PROVISIONING_API_KEY>'",
				DefaultProvisioningSecretName, r.Namespace),
		})
	}
	spec.Provisioning = prov
}

// javaOptsEnvName is the env var the chart renders the (now-removed) javaOpts value into on
// the JVM components.
const javaOptsEnvName = "JAVA_OPTS"

// bannerRule is the horizontal-rule comment line used to frame the header/footer banners
// in the rendered scaffold output.
const bannerRule = "# ---------------------------------------------------------------------------\n"

// mapJavaOpts maps the chart's top-level javaOpts (a JVM tuning string) onto a JAVA_OPTS
// env entry on EACH JVM component — core, auth, scheduler, and the
// deploy-provisioning block (a JVM service too). The special-case root spec.javaOpts is
// gone, so JVM tuning is plain per-component env now; this preserves the migrator's
// today-behavior. It is NOT applied to fe-administrator / OPA / the gateway (non-JVM). A
// NOTE is emitted so the reader knows the field moved.
func (r *Result) mapJavaOpts(values vals, spec *otilmv1alpha1.PlatformSpec) {
	jo := str(values["javaOpts"])
	if jo == "" {
		return
	}
	addJavaOpts := func(comp *otilmv1alpha1.ComponentSpec) {
		for _, e := range comp.Env {
			if e.Name == javaOptsEnvName {
				return // a per-component JAVA_OPTS override already wins; do not duplicate
			}
		}
		comp.Env = append(comp.Env, otilmv1alpha1.EnvVar{Name: javaOptsEnvName, Value: jo})
	}
	addJavaOpts(&spec.Core.ComponentSpec)
	addJavaOpts(&spec.Auth.ComponentSpec)
	addJavaOpts(&spec.Scheduler.ComponentSpec)
	if spec.Provisioning != nil && spec.Provisioning.Deploy != nil {
		addJavaOpts(&spec.Provisioning.Deploy.ComponentSpec)
	}
	r.customization = append(r.customization,
		"javaOpts is no longer a CR field; mapped to per-component JAVA_OPTS env on the JVM components (core, auth, scheduler"+
			deployProvisioningJavaOptsNote(spec)+"). Adjust per component as needed.")
}

// deployProvisioningJavaOptsNote extends the javaOpts NOTE when a deploy-provisioning block
// also received the JAVA_OPTS env, so the reader knows it was wired there too.
func deployProvisioningJavaOptsNote(spec *otilmv1alpha1.PlatformSpec) string {
	if spec.Provisioning != nil && spec.Provisioning.Deploy != nil {
		return ", provisioning.deploy"
	}
	return ""
}

// componentMap is the chart-values-key -> Platform-component binding for the per-component
// override pass. Each entry knows how to fetch the matching ComponentSpec pointer from the
// spec so mapComponent can fill it uniformly.
type componentMap struct {
	// valuesKey is the umbrella values top-level key (e.g. "auth").
	valuesKey string
	// get returns the ComponentSpec to populate for this component.
	get func(spec *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec
}

// platformComponents lists the umbrella values keys that map to a Platform component's
// shared override surface (image/replicas/resources/env). Connectors and bundled infra
// (keycloakInternal, messagingService, pgBouncer, opa) are handled separately.
func platformComponents() []componentMap {
	return []componentMap{
		{coreImageKey, func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec { return &s.Core.ComponentSpec }},
		{"authService", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec { return &s.Auth.ComponentSpec }},
		{"authOpaPolicies", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec {
			return &s.AuthOpaPolicies.ComponentSpec
		}},
		{"schedulerService", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec {
			return &s.Scheduler.ComponentSpec
		}},
		{"feAdministrator", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec {
			return &s.FeAdministrator.ComponentSpec
		}},
		{"utilsService", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec { return &s.Utils.ComponentSpec }},
		{"mcpServer", func(s *otilmv1alpha1.PlatformSpec) *otilmv1alpha1.ComponentSpec { return &s.MCP.ComponentSpec }},
	}
}

// mapComponents fills each platform component's shared override surface (image, env,
// resources, replicas) from its umbrella values block. Core's image comes from the
// top-level `image` block (the chart's core image), the others from their own block. The
// MCP server is one of them; mapMCP adds its settings and accounts for the rest of its block.
func (r *Result) mapComponents(values vals, spec *otilmv1alpha1.PlatformSpec) {
	for _, cm := range platformComponents() {
		block := mapOf(values[cm.valuesKey])
		if len(block) == 0 {
			continue
		}
		comp := cm.get(spec)
		r.mapComponentImage(block, comp, cm.valuesKey)
		r.mapComponentEnv(block, comp)
		r.mapComponentResources(block, comp)
		r.mapComponentReplicas(block, comp)
		// Per-component logging.level is wired by the operator as an env var; surface it as
		// a hint rather than silently dropping (it is not a typed component field).
		if lg := mapOf(block["logging"]); len(lg) > 0 {
			if _, ok := lg["level"]; ok && cm.valuesKey != coreImageKey {
				r.customization = append(r.customization, fmt.Sprintf(
					"%s.logging.level: set per-component log level via spec.%s.env (LOGGING_LEVEL_COM_CZERTAINLY) or the platform-wide spec.logging.level",
					cm.valuesKey, componentSpecPath(cm.valuesKey)))
			}
		}
	}
	// utils.enabled (global.utils.enabled in the chart) toggles the component.
	if en, ok := boolOf(mapOf(mapOf(values["global"])["utils"])["enabled"]); ok {
		spec.Utils.Enabled = en
	}
	if en, ok := boolOf(mapOf(values["utilsService"])["enabled"]); ok {
		spec.Utils.Enabled = en
	}
	r.mapMCP(values, spec)
}

// mapComponentImage fills a component's image override. For Core (valuesKey "image") the
// registry/repository/name are already the shared spec.image; only per-component overrides
// (tag/name) differ, so it fills name/tag/pullPolicy.
func (r *Result) mapComponentImage(block vals, comp *otilmv1alpha1.ComponentSpec, valuesKey string) {
	img := block
	if valuesKey != coreImageKey {
		img = mapOf(block["image"])
	}
	if len(img) == 0 {
		return
	}
	comp.Image.Registry = str(img["registry"])
	comp.Image.Repository = str(img["repository"])
	comp.Image.Name = str(img["name"])
	comp.Image.Tag = str(img["tag"])
	if pp := str(img["pullPolicy"]); pp != "" {
		comp.Image.PullPolicy = pp
	}
}

// mapComponentEnv fills a component's additionalEnv.variables onto comp.Env (non-sensitive
// inline env). Secret-bearing additionalEnv (.secrets/.configMaps) is flagged.
func (r *Result) mapComponentEnv(block vals, comp *otilmv1alpha1.ComponentSpec) {
	ae := mapOf(block["additionalEnv"])
	for _, v := range slice(ae["variables"]) {
		m := mapOf(v)
		if name := str(m["name"]); name != "" {
			comp.Env = append(comp.Env, otilmv1alpha1.EnvVar{Name: name, Value: str(m["value"])})
		}
	}
}

// mapComponentResources copies a component's resources block verbatim (requests/limits) into
// comp.Resources.
func (r *Result) mapComponentResources(block vals, comp *otilmv1alpha1.ComponentSpec) {
	if rr := resourcesFrom(block); rr != nil {
		comp.Resources = rr
	}
}

// mapComponentReplicas copies a component's replicaCount/replicas onto comp.Replicas.
func (r *Result) mapComponentReplicas(block vals, comp *otilmv1alpha1.ComponentSpec) {
	for _, k := range []string{"replicaCount", "replicas"} {
		if n, ok := intOf(block[k]); ok {
			comp.Replicas = ptrTo(n)
			return
		}
	}
}

// Reasons the report gives for an MCP value that is not converted, or notes it adds to the output.
const (
	// mcpClientSecretNote is shared by every values path that holds MCP's client secret or a
	// reference to it, with the managed identity.
	mcpClientSecretNote = "not copied: the operator manages MCP's client secret. It relays the one Keycloak " +
		"generates for the ilm-mcp client into a Secret it owns, so there is no Secret to create or reference"
	// mcpExternalClientSecretNote is the same reason with an external identity, which reads its client secret
	// from a Secret the operator never creates. The name of that Secret comes from
	// mcpServer.security.clientSecretRef only, so a value or a reference under global.mcp is not it.
	mcpExternalClientSecretNote = "not copied: with an external identity MCP reads its client secret from the " +
		"Secret that spec.mcp.identity.clientSecretRef names, and the operator never creates that Secret. The " +
		"conversion takes the name from mcpServer.security.clientSecretRef only"
	// mcpOwnKeycloakNote is the reason for an issuer, key-set or token endpoint with the managed identity,
	// where MCP signs in through the platform's own Keycloak. It says the operator derives them, so the line
	// cannot be read as if the conversion kept another provider.
	mcpOwnKeycloakNote = "not converted: MCP signs in through the platform's own Keycloak, whose issuer and " +
		"endpoints the operator derives"
	// mcpSecurityNote is the reason for every other mcpServer.security value with the managed identity.
	mcpSecurityNote = "not converted: the operator derives MCP's security settings to match the Keycloak " +
		"objects it provisions for MCP"
	// mcpResourceNote is the reason for mcpServer.security.resource with an external identity.
	mcpResourceNote = "not converted: the operator derives MCP's resource from the platform's public host, " +
		"as https://<host>/mcp"
	// mcpNoFieldNote is the reason for every other mcpServer.security value with an external identity.
	mcpNoFieldNote = "not converted: the Platform has no field for it"
	// mcpOffNote is the reason for every mcpServer.security value of an identity provider that the values name
	// while MCP is off. The Platform checks an identity block whether or not MCP is on, so it is mapped only for
	// MCP that is on.
	mcpOffNote = "not converted: MCP is off (global.mcp.enabled is not true), so its identity provider settings " +
		"are left out. Turn MCP on and convert again"
	// mcpNetworkPolicyNote is the reason for mcpServer.networkPolicy.
	mcpNetworkPolicyNote = "not converted: the operator renders no NetworkPolicy for MCP alone. After migration MCP " +
		"is isolated only by the platform-wide NetworkPolicies (spec.networkPolicy), which admit ingress from the " +
		"platform's namespace and the operator and allow all egress. A policy you had enabled to limit MCP to " +
		"listed peers is not recreated, so the isolation is looser"
	// mcpGroupNote is the reason for a delegation group that is not a string. Leaving it out of the
	// Platform widens MCP from one group's members to every user, so the report must say so.
	mcpGroupNote = "not converted: the value is not a string, and without spec.mcp.delegation.group MCP acts " +
		"for every user. Set the group by hand"
	// mcpGroupExternalNote is the reason for a delegation group with an external identity, which the Platform
	// rejects because the operator creates the group in its own Keycloak.
	mcpGroupExternalNote = "not converted: a delegation group applies to the managed identity only, because the " +
		"operator creates it in the platform's Keycloak. With an external identity, limit who MCP acts for at " +
		"the identity provider"
	// mcpIdentityMissingNote is the note for a setting the Platform requires with an external identity and the
	// values lack. It names the Platform field to set, the values path that was empty and what the field holds.
	mcpIdentityMissingNote = "spec.mcp.identity.%s: no %s found in values, and an external identity requires it. " +
		"Set it by hand to %s; the emitted identity block is INCOMPLETE until you do"
	// mcpExchangeModeNote says the exchange mode was filled in. A chart may default to the other mode, so the
	// note names it.
	mcpExchangeModeNote = "spec.mcp.identity.exchangeMode: no mcpServer.security.exchangeMode found in values, so " +
		"it is standard, the MCP server's own default. Set delegation by hand if your chart ran MCP in delegation mode"
)

const (
	// keycloakPath is the path the platform's own managed Keycloak is served under on the platform's public host
	// (KC_HTTP_RELATIVE_PATH). It is the builder's KeycloakRelativePath, copied because importing the builder would
	// bring the operator's whole render tree into this standalone package; a test keeps the two equal.
	keycloakPath = "/kc"
	// mcpDefaultExchangeMode is the exchange mode the MCP server uses when none is set, and so the one an external
	// identity gets when the values leave it out.
	mcpDefaultExchangeMode = "standard"
	// mcpClientSecretNamePlaceholder stands in for the name of the Secret that holds MCP's client secret, in the
	// Secret the report asks you to create. The values name none, or name one that is not a valid Secret name, and
	// the converter never invents one.
	mcpClientSecretNamePlaceholder = "<MCP_CLIENT_SECRET_NAME>" //nolint:gosec // a placeholder for a Secret object NAME, not a credential
)

// mcpIdentityKind says what the conversion does with mcpServer.security.
type mcpIdentityKind int

const (
	// managedIdentity leaves MCP on the platform's own Keycloak: the values name no issuer, or the one of that
	// Keycloak. No identity block is emitted.
	managedIdentity mcpIdentityKind = iota
	// externalIdentity maps the values onto spec.mcp.identity: they name another issuer and turn MCP on.
	externalIdentity
	// deferredIdentity names another issuer while MCP is off. No identity block is emitted, and the values are
	// reported.
	deferredIdentity
)

// mcpServerRead lists what the conversion reads from the umbrella's mcpServer block: the access
// level, the override surface mapComponents fills for every component, and image.resources, which
// mapMCP reads because the chart keeps the container's resources there. A block that is read only
// in part lists the sub-keys it reads; any other entry is read whole. It must match those reads: a
// value listed here that nothing reads would vanish without a report.
var mcpServerRead = map[string][]string{
	"access":        nil,
	"replicaCount":  nil,
	"replicas":      nil,
	"resources":     nil,
	"image":         {"registry", "repository", "name", "tag", "pullPolicy", "resources"},
	"additionalEnv": {"variables"},
}

// mcpIdentityRead lists the mcpServer.security values mapMCPIdentity copies onto an external identity. It
// must match those reads: a value listed here that nothing reads would vanish without a report.
var mcpIdentityRead = []string{
	"issuer", "jwkSetUri", "tokenUri", "clientId", "clientSecretRef", "exchangeMode",
	"exchangeScope", "requiredScope", "authorizationScopes", "apiAudience",
}

// mcpSecurityEnv names the environment variable of an mcpServer.security value that the Platform has no
// field for. The operator renders the variable itself, and an entry in spec.mcp.env overrides it.
var mcpSecurityEnv = map[string]string{
	"allowedOrigins":       "ILM_MCP_SECURITY_ALLOWED_ORIGINS",
	"jwsAlgorithms":        "ILM_MCP_SECURITY_JWS_ALGORITHMS",
	"allowHttpBackchannel": "ILM_MCP_SECURITY_ALLOW_HTTP_BACKCHANNEL",
}

// mapMCP maps the umbrella's MCP values onto spec.mcp. The settings live under global.mcp. The
// component's own values live under mcpServer: mapComponents has already filled its image,
// replicas, resources and env through the component table, so what is read here is the access
// level, the identity provider (mcpServer.security) and the resources the chart keeps under image.
// The component table reads a top-level resources key, which the chart does not have, so the
// chart's key wins when both are set.
//
// global.mcp.cimd.enabled is copied only when the values set it. The chart defaults it to true and
// the Platform counts an unset value as true, so an absent value stays absent instead of becoming
// a second spelling of the default.
//
// spec.mcp.identity is emitted only for MCP that the values turn on. The Platform checks an identity
// block whether or not MCP is on, so an incomplete one would be rejected for a feature that is off.
//
// MCP's client secret is never copied. With the managed identity neither is a reference to it: the
// operator generates the secret with MCP's Keycloak client and relays it itself. With an external
// identity the reference is the name of the Secret that holds it, which mapMCPIdentity copies. Every
// other mcpServer value the Platform does not take is reported by its exact path. The report names
// paths only, never values.
func (r *Result) mapMCP(values vals, spec *otilmv1alpha1.PlatformSpec) {
	mcp := mapOf(mapOf(values["global"])["mcp"])
	server := mapOf(values["mcpServer"])

	enabled, _ := boolOf(mcp["enabled"])
	spec.MCP.Enabled = enabled
	kind := r.mapMCPIdentity(mapOf(server["security"]), enabled, spec)
	if cimd, ok := boolOf(mapOf(mcp["cimd"])["enabled"]); ok {
		spec.MCP.CIMD.Enabled = ptrTo(cimd)
	}
	r.mapMCPDelegationGroup(mapOf(mcp["delegation"])["group"], kind == externalIdentity, spec)
	r.flagMCPClientSecrets(mcp, spec.MCP.Identity)

	spec.MCP.Access = str(server["access"])
	if resources := resourcesFrom(mapOf(server["image"])); resources != nil {
		spec.MCP.Resources = resources
	}
	r.flagUnmappedMCPServerKeys(server, kind)
}

// mapMCPIdentity maps mcpServer.security onto spec.mcp.identity and reports what it did. A set issuer
// names an identity provider the platform does not manage, unless it is the platform's own Keycloak
// (ownKeycloakIssuer), which stays on the managed identity with no identity block. Without an issuer there
// is nothing to name the provider, so endpoints alone do not make an external identity. With MCP off the
// provider is not mapped either (deferredIdentity).
//
// With an external identity the Platform requires an issuer, a key-set and a token endpoint, a client, a
// client secret reference and an exchange mode. An exchange mode the values leave unset is filled with
// the MCP server's own default. Any other of them the values lack stays empty and is reported with the
// Platform field to set: the converter never invents an endpoint, a client or a Secret name.
func (r *Result) mapMCPIdentity(security vals, enabled bool, spec *otilmv1alpha1.PlatformSpec) mcpIdentityKind {
	issuer := str(security["issuer"])
	switch {
	case issuer == "" || ownKeycloakIssuer(spec, issuer):
		return managedIdentity
	case !enabled:
		return deferredIdentity
	}
	secret := mapOf(security["clientSecretRef"])
	identity := &otilmv1alpha1.MCPIdentitySpec{
		Mode:                modeExternal,
		Issuer:              issuer,
		JWKSetURI:           str(security["jwkSetUri"]),
		TokenURI:            str(security["tokenUri"]),
		ClientID:            str(security["clientId"]),
		ClientSecretRef:     str(secret["name"]),
		ClientSecretKey:     str(secret["key"]),
		ExchangeMode:        str(security["exchangeMode"]),
		ExchangeScope:       str(security["exchangeScope"]),
		RequiredScope:       str(security["requiredScope"]),
		AuthorizationScopes: stringSlice(security["authorizationScopes"]),
		APIAudience:         str(security["apiAudience"]),
	}
	if identity.ExchangeMode == "" {
		identity.ExchangeMode = mcpDefaultExchangeMode
		r.customization = append(r.customization, mcpExchangeModeNote)
	}
	for _, required := range []struct{ field, valuesPath, value, what string }{
		{"jwkSetUri", "mcpServer.security.jwkSetUri", identity.JWKSetURI, "the identity provider's key-set endpoint"},
		{"tokenUri", "mcpServer.security.tokenUri", identity.TokenURI, "the identity provider's token endpoint"},
		{"clientId", "mcpServer.security.clientId", identity.ClientID, "the ID of MCP's client at the identity provider"},
		{"clientSecretRef", "mcpServer.security.clientSecretRef.name", identity.ClientSecretRef,
			"the name of a Secret in this namespace that holds MCP's client secret"},
	} {
		if required.value == "" {
			r.customization = append(r.customization,
				fmt.Sprintf(mcpIdentityMissingNote, required.field, required.valuesPath, required.what))
		}
	}
	spec.MCP.Identity = identity
	return externalIdentity
}

// ownKeycloakIssuer reports whether the issuer belongs to the Keycloak the converted Platform manages for
// itself: https://<host>/kc/... on the platform's public host, whichever realm it names, in whatever case the
// host is written and with the https default port 443 spelled out or not. The Platform rejects such an issuer
// with an external identity, because it prepares that Keycloak for MCP's managed identity only, so the
// conversion leaves MCP on the managed identity. The host is the one the builder's PlatformHost resolves. The
// converter sets no edge.host, which makes it spec.common.hostName; a test keeps that true.
func ownKeycloakIssuer(spec *otilmv1alpha1.PlatformSpec, issuer string) bool {
	host := strings.ToLower(spec.Common.HostName)
	if spec.Keycloak == nil || spec.Keycloak.Mode != modeManaged || host == "" {
		return false
	}
	issuer = strings.ToLower(issuer)
	return strings.HasPrefix(issuer, "https://"+host+keycloakPath+"/") ||
		strings.HasPrefix(issuer, "https://"+host+":443"+keycloakPath+"/")
}

// mapMCPDelegationGroup copies global.mcp.delegation.group onto the Platform with the managed identity,
// which creates the group in the platform's Keycloak. An external identity rejects a group, so it is
// reported instead. A group that is not a string is reported with the managed identity too: leaving it
// out widens MCP from one group's members to every user.
func (r *Result) mapMCPDelegationGroup(group interface{}, external bool, spec *otilmv1alpha1.PlatformSpec) {
	if isEmpty(group) {
		return
	}
	if external {
		r.unmapped = append(r.unmapped, "global.mcp.delegation.group ("+mcpGroupExternalNote+")")
		return
	}
	spec.MCP.Delegation.Group = str(group)
	if spec.MCP.Delegation.Group == "" {
		r.unmapped = append(r.unmapped, "global.mcp.delegation.group ("+mcpGroupNote+")")
	}
}

// flagMCPClientSecrets reports the client secret values under global.mcp, which are never copied. With
// the managed identity (a nil identity) the operator generates the secret, so no Secret is needed. With an
// external identity MCP reads it from the Secret spec.mcp.identity.clientSecretRef names, so an inline
// value is scaffolded as a Secret to create, like any other inline secret. The scaffold is a command line,
// so it carries the name and key the values give that Secret only when each is a valid Secret name or key,
// and otherwise the placeholder name or MCP's default key.
func (r *Result) flagMCPClientSecrets(mcp vals, identity *otilmv1alpha1.MCPIdentitySpec) {
	note := mcpClientSecretNote
	if identity != nil {
		note = mcpExternalClientSecretNote
	}
	for _, key := range []string{"clientSecret", "clientSecretRef"} {
		if !isEmpty(mcp[key]) {
			r.unmapped = append(r.unmapped, "global.mcp."+key+" ("+note+")")
		}
	}
	if identity != nil && !isEmpty(mcp["clientSecret"]) {
		name, key := mcpClientSecretNamePlaceholder, defaultMCPSecretKey
		if len(validation.IsDNS1123Subdomain(identity.ClientSecretRef)) == 0 {
			name = identity.ClientSecretRef
		}
		if len(validation.IsConfigMapKey(identity.ClientSecretKey)) == 0 {
			key = identity.ClientSecretKey
		}
		r.addSecretTODO(secretTODO{
			name: name,
			keys: []string{key},
			reason: "MCP's client secret at the identity provider (was global.mcp.clientSecret): the Secret that " +
				"spec.mcp.identity.clientSecretRef must name. Skip it if that Secret already exists",
			kubectl: fmt.Sprintf(
				"kubectl create secret generic %s -n %s --from-literal=%s='<MCP_CLIENT_SECRET>'", name, r.Namespace, key),
		})
	}
}

// flagUnmappedMCPServerKeys reports every non-empty mcpServer value the conversion does not read,
// by its exact values path. flagUnmappedTopLevel looks at top-level keys only and the component
// table reads just part of each block, so without this a key such as toolsets, or an image sub-key
// such as digest, would vanish with no note. kind says what became of mcpServer.security: an external
// identity reads part of it.
func (r *Result) flagUnmappedMCPServerKeys(server vals, kind mcpIdentityKind) {
	for key, v := range server {
		if isEmpty(v) {
			continue
		}
		read, isRead := mcpServerRead[key]
		switch {
		case key == "security":
			r.flagUnmappedMCPSecurityKeys(mapOf(v), kind)
		case key == "networkPolicy":
			r.unmapped = append(r.unmapped, "mcpServer.networkPolicy ("+mcpNetworkPolicyNote+")")
		case !isRead:
			r.unmapped = append(r.unmapped, "mcpServer."+key)
		case read != nil: // a block read only in part: report the sub-keys it does not read
			for sub, subValue := range mapOf(v) {
				if !isEmpty(subValue) && !slices.Contains(read, sub) {
					r.unmapped = append(r.unmapped, "mcpServer."+key+"."+sub)
				}
			}
		}
	}
}

// flagUnmappedMCPSecurityKeys reports each non-empty mcpServer.security value the conversion does not
// map, with the reason that fits it. An external identity maps the values in mcpIdentityRead; the managed
// identity maps none, because the operator derives its settings, and a deferred one maps none because MCP
// is off.
func (r *Result) flagUnmappedMCPSecurityKeys(security vals, kind mcpIdentityKind) {
	for key, v := range security {
		if isEmpty(v) || (kind == externalIdentity && slices.Contains(mcpIdentityRead, key)) {
			continue
		}
		r.unmapped = append(r.unmapped, "mcpServer.security."+key+" ("+mcpSecurityReason(key, kind)+")")
	}
}

// mcpSecurityReason returns why an mcpServer.security value is not converted. With the managed identity an
// issuer, key-set or token endpoint is the platform's own Keycloak, which the operator derives.
func mcpSecurityReason(key string, kind mcpIdentityKind) string {
	switch kind {
	case deferredIdentity:
		return mcpOffNote
	case externalIdentity:
		if key == "resource" {
			return mcpResourceNote
		}
		if env, ok := mcpSecurityEnv[key]; ok {
			return mcpNoFieldNote + ". The operator renders " + env + " itself, and spec.mcp.env overrides it"
		}
		return mcpNoFieldNote
	}
	switch key {
	case "issuer", "jwkSetUri", "tokenUri":
		return mcpOwnKeycloakNote
	case "clientSecretRef":
		return mcpClientSecretNote
	}
	return mcpSecurityNote
}

// timeQualityMonitorSecretName is the Secret the converter references for the monitor's broker
// credentials in external mode (the values are never transcribed — see mapTimeQualityMonitor).
const timeQualityMonitorSecretName = "time-quality-monitor-credentials"

// mapCoreFeatures maps the chart's Core-shape values onto the CR: the workload kind, the
// explicit platform instance id, Core's time-quality messaging toggle and the
// time-quality-monitor sidecar. Without these the values would land in the UNMAPPED footer and
// a migrated platform would quietly lose them.
//
// The time-quality toggle is read from BOTH the chart-local messaging block and
// global.messaging, and EITHER being true enables it — the chart composes them with `or`, so
// neither wins over the other.
//
// The chart-local "messaging" block is PARTLY mapped (timeQuality here; the broker coordinates
// come from global.messaging, via mapMessaging), and flagUnmappedTopLevel inspects TOP-LEVEL
// keys only — so the block is all-or-nothing there. It is registered in knownTopLevel and
// accounted for sub-key by sub-key instead: without the registration an input whose only
// messaging key was timeQuality would still print "# UNMAPPED: messaging"; without the
// accounting the coordinates would vanish silently. Same contract as timeQualityMonitor.
func (r *Result) mapCoreFeatures(values, global vals, spec *otilmv1alpha1.PlatformSpec) {
	if wt := str(values["workloadType"]); wt != "" {
		spec.Core.WorkloadType = otilmv1alpha1.WorkloadKind(wt)
	}
	if id, ok := intOf(values["platformInstanceId"]); ok {
		spec.Core.InstanceID = ptrTo(id)
	}
	for _, block := range []vals{mapOf(global["messaging"]), mapOf(values["messaging"])} {
		if enabled, ok := boolOf(mapOf(block["timeQuality"])["enabled"]); ok && enabled {
			spec.Messaging.TimeQuality.Enabled = true
		}
	}
	r.flagUnmappedMessagingKeys(mapOf(values["messaging"]))
	r.mapTimeQualityMonitor(mapOf(values["timeQualityMonitor"]), spec)
}

// flagUnmappedMessagingKeys reports every CHART-LOCAL messaging sub-key the converter does not
// consume, by its exact values path.
//
// Only messaging.timeQuality maps (in mapCoreFeatures); the broker coordinates are read from
// global.messaging by mapMessaging, so a chart-local host/port/user block is genuinely dropped
// and the user has to be told which keys those were. Keys are reported in sorted order so the
// rendered footer is deterministic.
func (r *Result) flagUnmappedMessagingKeys(msg vals) {
	keys := make([]string, 0, len(msg))
	for key, v := range msg {
		if key == "timeQuality" || isEmpty(v) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r.unmapped = append(r.unmapped, "messaging."+key+
			" (the operator reads the broker coordinates from global.messaging -> spec.messaging; this chart-local key is not consumed)")
	}
}

// mapTimeQualityMonitor maps the chart's timeQualityMonitor block onto
// spec.core.timeQualityMonitor, and reports by name every sub-key that has no CR field.
//
// The chart nests the sidecar's probes AND resources UNDER image (its own convention), so the
// resources are read from there and converted by resourcesFrom — the same round-trip component
// resources use, never a second implementation.
//
// The monitor's CREDENTIALS are deliberately not mapped: the chart carries them as inline
// plaintext (global.messaging.timeQualityMonitorUsername/Password) and this converter never
// transcribes a secret — it records the Secret to create instead, and points the CR at it.
func (r *Result) mapTimeQualityMonitor(tqm vals, spec *otilmv1alpha1.PlatformSpec) {
	if len(tqm) == 0 {
		return
	}
	defer r.flagUnmappedMonitorKeys(tqm)

	enabled, ok := boolOf(tqm["enabled"])
	if !ok || !enabled {
		// A disabled monitor maps to nothing (the CR's own default is disabled). Its customised
		// sub-keys are still reported by the deferred call above, so nothing disappears silently.
		return
	}

	img := mapOf(tqm["image"])
	m := &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled: true,
		Image: otilmv1alpha1.ImageSpec{
			Registry:    str(img["registry"]),
			Repository:  str(img["repository"]),
			Name:        str(img["name"]),
			Tag:         str(img["tag"]),
			Digest:      str(img["digest"]),
			PullPolicy:  str(img["pullPolicy"]),
			PullSecrets: stringSlice(img["pullSecrets"]),
		},
		Resources: resourcesFrom(img),
	}
	if spec.Messaging.Mode == modeExternal {
		r.addSecretTODO(secretTODO{
			name: timeQualityMonitorSecretName,
			keys: []string{defaultUsernameKey, defaultPasswordKey},
			reason: "the time-quality-monitor sidecar's broker credentials " +
				"(was global.messaging.timeQualityMonitorUsername/Password, never copied from values.yaml)",
			kubectl: fmt.Sprintf(
				"kubectl create secret generic %s -n %s --from-literal=username='<MONITOR_USER>' --from-literal=password='<MONITOR_PASSWORD>'",
				timeQualityMonitorSecretName, r.Namespace),
		})
		m.Credentials = &otilmv1alpha1.CredentialsRef{SecretRef: timeQualityMonitorSecretName}
	}
	spec.Core.TimeQualityMonitor = m
}

// flagUnmappedMonitorKeys reports every timeQualityMonitor sub-key the CR cannot express, by
// its exact values path and with the reason.
//
// It exists because mapCoreFeatures marks the whole block KNOWN, and flagUnmappedTopLevel
// inspects top-level keys only — so without this, a monitor's custom entrypoint, probe timings
// or log level would vanish with no note anywhere. Nested reporting through r.unmapped is the
// converter's established idiom (see the logging.audit and apiGateway.* entries).
func (r *Result) flagUnmappedMonitorKeys(tqm vals) {
	img := mapOf(tqm["image"])
	unmappable := []struct {
		block     vals
		key, path string
		note      string
	}{
		{img, "command", "timeQualityMonitor.image.command",
			"the sidecar runs the monitor image's own entrypoint; there is no per-sidecar command field"},
		{img, "args", "timeQualityMonitor.image.args",
			"the sidecar runs the monitor image's own args; there is no per-sidecar args field"},
		{img, "securityContext", "timeQualityMonitor.image.securityContext",
			"the operator SCC-hardens every container it renders (runAsNonRoot, all capabilities dropped, seccomp RuntimeDefault, no hard-coded runAsUser)"},
		{img, "probes", "timeQualityMonitor.image.probes",
			"the monitor's /health liveness+readiness probes are operator-rendered and not configurable per field"},
		{tqm, "logging", "timeQualityMonitor.logging",
			"the sidecar's log level follows spec.common.logging.level, like every other component"},
	}
	for _, u := range unmappable {
		if v, ok := u.block[u.key]; ok && !isEmpty(v) {
			r.unmapped = append(r.unmapped, u.path+" ("+u.note+")")
		}
	}
}

// connectorKeys are the umbrella values keys that are CONNECTORS, not platform components.
// They are managed by the Connector CRD (a separate resource), not the Platform CR, so the
// converter flags them rather than mapping them into the Platform. The list mirrors the
// umbrella chart's per-connector subchart aliases (Chart.yaml dependencies), so every
// connector block a chart install can carry is routed to the Connector-CRD guidance instead
// of falling through to a bare "# UNMAPPED" footer line.
var connectorKeys = []string{
	"commonCredentialProvider", "ejbcaNgConnector", "externalAuthorityProvider",
	"pyAdcsConnector", "otpkiConnector",
	"hashicorpVaultConnector", "timestampFormattingConnector",
	"x509ComplianceProvider", "cryptosenseDiscoveryProvider",
	"ctLogsDiscoveryProvider", "networkDiscoveryProvider", "keystoreEntityProvider",
	"softwareCryptographyProvider", "emailNotificationProvider", "webhookNotificationProvider",
	"registerConnectors",
}

// flagConnectors emits a customization TODO for every connector block found in values:
// connectors are a SEPARATE Connector CRD, not part of the Platform CR.
func (r *Result) flagConnectors(values vals) {
	var found []string
	for _, k := range connectorKeys {
		if _, ok := values[k]; ok {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return
	}
	sort.Strings(found)
	r.customization = append(r.customization, fmt.Sprintf(
		"connectors (%s) are managed by the SEPARATE Connector CRD (otilm.com Connector), not the Platform CR — convert each to a Connector resource (see docs/design/connector-operator.md). SMTP/credentials in emailNotificationProvider.smtp stay inline secrets there too.",
		strings.Join(found, ", ")))
}

// flagGlobalCustomization flags the chart's global passthrough surface (raw
// initContainers/sidecars/volumes) the user must move to spec.common by hand, since the
// chart shape (free-form) does not 1:1 round-trip through typed conversion safely. (The
// fleet-wide passthrough now lives under spec.common — the former spec.global.)
func (r *Result) flagGlobalCustomization(global vals) {
	for _, k := range []string{"initContainers", "sidecarContainers", "additionalVolumes", "additionalVolumeMounts", "additionalPorts"} {
		if v, ok := global[k]; ok && !isEmpty(v) {
			r.customization = append(r.customization, fmt.Sprintf(
				"global.%s -> spec.common.%s: move this raw passthrough by hand (the converter does not transcribe free-form pod-spec fragments to avoid silently mangling them)",
				k, normalizeGlobalKey(k)))
		}
	}
	ae := mapOf(global["additionalEnv"])
	if _, ok := ae["secrets"]; ok {
		r.customization = append(r.customization, "global.additionalEnv.secrets -> spec.common.additionalEnvFrom.secrets (Secret NAMES only)")
	}
	if _, ok := ae["configMaps"]; ok {
		r.customization = append(r.customization, "global.additionalEnv.configMaps -> spec.common.additionalEnvFrom.configMaps (ConfigMap NAMES only)")
	}
}

// knownTopLevel is the set of umbrella values top-level keys the converter understands
// (mapped, flagged, or deliberately ignored). Anything outside it is flagged "# UNMAPPED".
var knownTopLevel = func() map[string]bool {
	keys := []string{
		"global", "image", "additionalEnv", "logging", "ingress", "letsEncrypt",
		"apiGateway", "registerAdmin", "authService", "authOpaPolicies",
		"schedulerService", "feAdministrator", "utilsService",
		// "mcpServer" is registered although mapMCP reads only part of the block: flagUnmappedTopLevel
		// is top-level-only, so the keys left over are reported one by one (flagUnmappedMCPServerKeys).
		"mcpServer",
		// Core-shape values handled by mapCoreFeatures. "messaging" is registered even though it
		// is only PARTLY mapped (timeQuality maps; the broker coordinates come from
		// global.messaging): flagUnmappedTopLevel is top-level-only, so leaving it out would
		// report the whole block even when every key in it was consumed. The difference is owed
		// back key by key — flagUnmappedMessagingKeys, like flagUnmappedMonitorKeys.
		"workloadType", "platformInstanceId", "timeQualityMonitor", "messaging",
		// bundled infra / sidecars handled with notes:
		"keycloakInternal", "messagingService", "pgBouncer", "opa", "utilsService",
	}
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	for _, k := range connectorKeys {
		m[k] = true
	}
	return m
}()

// flagUnmappedTopLevel walks the values top level and flags any key the converter neither
// maps nor recognizes, plus the bundled-infra blocks that map to managed-mode decisions.
func (r *Result) flagUnmappedTopLevel(values vals) {
	// Bundled-infra blocks: the operator provisions these via managed mode (a deliberate
	// decision), so flag them as customization rather than silent drops.
	for key, note := range map[string]string{
		"messagingService": "messagingService (the bundled RabbitMQ) -> messaging.mode=managed (RabbitMQ operators) if you want the operator to provision the broker; otherwise point messaging at your external broker",
		"keycloakInternal": "keycloakInternal (the bundled Keycloak) -> keycloak.mode=managed (Keycloak Operator); see mapKeycloak note",
		"pgBouncer":        "pgBouncer settings -> database.pgBouncer (managed-DB Pooler) — only the enabled toggle maps; pgbouncer.ini section settings are not a CR field",
		"opa":              "opa (Core's OPA sidecar image/probes) is operator-managed; per-image probe toggles are not a CR field",
	} {
		if v, ok := values[key]; ok && !isEmpty(v) {
			r.customization = append(r.customization, note)
		}
	}
	for k, v := range values {
		if knownTopLevel[k] || isEmpty(v) {
			continue
		}
		r.unmapped = append(r.unmapped, k)
	}
}

// addSecretTODO records a Secret the user must create, de-duplicating by name (the same
// Secret can be referenced from multiple values paths).
func (r *Result) addSecretTODO(s secretTODO) {
	for _, e := range r.secretTODOs {
		if e.name == s.name {
			return
		}
	}
	r.secretTODOs = append(r.secretTODOs, s)
}

// Render assembles the final YAML document: the secret-creation TODO header, the typed
// Platform CR, and the unmapped/customization notes footer.
func (r *Result) Render() (string, error) {
	crYAML, err := yaml.Marshal(r.Platform)
	if err != nil {
		return "", fmt.Errorf("marshal Platform CR: %w", err)
	}
	var b strings.Builder
	r.writeHeader(&b)
	b.Write(crYAML)
	r.writeFooter(&b)
	return b.String(), nil
}

// writeHeader writes the top-of-file banner and the prerequisite-Secrets TODO block. Text that came from
// the values passes through commentText, so it stays on its comment line.
func (r *Result) writeHeader(b *strings.Builder) {
	b.WriteString(bannerRule)
	b.WriteString("# Platform CR scaffolded by values2platform (best-effort).\n")
	b.WriteString("# Review every field before applying. This is a SCAFFOLD, not a drop-in config.\n")
	b.WriteString("#\n")
	b.WriteString("# SECRETS ARE NOT COPIED. The tool never transcribes plaintext secrets from your\n")
	b.WriteString("# values.yaml. For each inline secret it emitted a Secret REFERENCE in the CR and\n")
	b.WriteString("# listed the Secret to create below. Create these Secrets BEFORE `kubectl apply`.\n")
	if len(r.secretTODOs) == 0 {
		b.WriteString("#\n# (No inline secrets detected in the input values.)\n")
	} else {
		b.WriteString("#\n# TODO: create the following Secrets (edit the placeholder values):\n")
		// Stable order for deterministic output.
		todos := append([]secretTODO{}, r.secretTODOs...)
		sort.Slice(todos, func(i, j int) bool { return todos[i].name < todos[j].name })
		for _, s := range todos {
			b.WriteString("#\n")
			fmt.Fprintf(b, "#   * %s  (keys: %s)\n", commentText(s.name), commentText(strings.Join(s.keys, ", ")))
			fmt.Fprintf(b, "#     %s\n", commentText(s.reason))
			fmt.Fprintf(b, "#     %s\n", commentText(s.kubectl))
		}
	}
	b.WriteString(bannerRule)
}

// writeFooter writes the UNMAPPED and TODO(customization) notes after the CR. Their paths name keys of
// the values, so they pass through commentText as well.
func (r *Result) writeFooter(b *strings.Builder) {
	if len(r.unmapped) == 0 && len(r.customization) == 0 {
		return
	}
	b.WriteString("\n" + bannerRule)
	b.WriteString("# NOTES — gaps the converter could not (or should not) map automatically.\n")
	b.WriteString(bannerRule)
	for _, c := range r.customization {
		fmt.Fprintf(b, "# TODO(customization): %s\n", commentText(c))
	}
	for _, u := range r.unmapped {
		fmt.Fprintf(b, "# UNMAPPED: %s\n", commentText(u))
	}
}
