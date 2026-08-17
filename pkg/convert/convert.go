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
// scaffolds it.
//
// The conversion is best-effort, not 100%: values with no CR equivalent are flagged with
// "# UNMAPPED: <values path>" and customization the user must move by hand (connectors,
// per-connector SMTP, raw sidecars/initContainers) with "# TODO(customization): ...".
package convert

import (
	"fmt"
	"sort"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// modeExternal is the infra connection mode the converter always emits (external): a
// values.yaml describes a chart install, which the operator adopts by REFERENCING the
// running infra in external mode; opting into managed mode is a separate human decision.
const modeExternal = "external"

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
	spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "managed", Realm: "ilm"}
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

// mapLogging maps the top-level logging.level onto spec.common.logging.level. The chart's
// logging.audit toggle has no CR field and is flagged unmapped.
func (r *Result) mapLogging(values vals, spec *otilmv1alpha1.PlatformSpec) {
	lg := mapOf(values["logging"])
	if len(lg) == 0 {
		return
	}
	if lvl := str(lg["level"]); lvl != "" {
		spec.Common.Logging.Level = lvl
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
	}
}

// mapComponents fills each platform component's shared override surface (image, env,
// resources, replicas) from its umbrella values block. Core's image comes from the
// top-level `image` block (the chart's core image), the others from their own block.
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

// writeHeader writes the top-of-file banner and the prerequisite-Secrets TODO block.
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
			fmt.Fprintf(b, "#   * %s  (keys: %s)\n", s.name, strings.Join(s.keys, ", "))
			fmt.Fprintf(b, "#     %s\n", s.reason)
			fmt.Fprintf(b, "#     %s\n", s.kubectl)
		}
	}
	b.WriteString(bannerRule)
}

// writeFooter writes the UNMAPPED and TODO(customization) notes after the CR.
func (r *Result) writeFooter(b *strings.Builder) {
	if len(r.unmapped) == 0 && len(r.customization) == 0 {
		return
	}
	b.WriteString("\n" + bannerRule)
	b.WriteString("# NOTES — gaps the converter could not (or should not) map automatically.\n")
	b.WriteString(bannerRule)
	for _, c := range r.customization {
		fmt.Fprintf(b, "# TODO(customization): %s\n", c)
	}
	for _, u := range r.unmapped {
		fmt.Fprintf(b, "# UNMAPPED: %s\n", u)
	}
}
