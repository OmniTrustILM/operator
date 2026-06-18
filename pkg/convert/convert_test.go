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

package convert

import (
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// decode parses an inline YAML values document into the loosely-typed tree the converter
// consumes, failing the test on a parse error.
func decode(t *testing.T, doc string) vals {
	t.Helper()
	var v vals
	require.NoError(t, yaml.Unmarshal([]byte(doc), &v))
	return v
}

// plaintextSecrets are sensitive literals used in the test fixtures. The rendered CR must
// NEVER contain any of them — the central secrets-safety guarantee of the tool.
var plaintextSecrets = []string{
	"54R0dFPL5Iwo2BS",   // db password
	"s0qKH5qItTWoxpBt",  // keycloak client secret
	"super-mq-pass",     // messaging password
	"my-api-key",        // provisioning api key
	"BEGIN CERTIFICATE", // any inline PEM
}

// assertNoPlaintextSecrets fails if any known plaintext secret leaked into the output.
func assertNoPlaintextSecrets(t *testing.T, rendered string) {
	t.Helper()
	for _, s := range plaintextSecrets {
		assert.NotContainsf(t, rendered, s, "rendered CR leaked plaintext secret %q", s)
	}
}

// TestConvert_CoreFieldMapping covers the happy-path scalar mappings: image, database
// (external), messaging, edge, proxy, logging, additionalEnv.
func TestConvert_CoreFieldMapping(t *testing.T) {
	values := decode(t, `
global:
  image:
    pullSecrets: [regcred]
  database:
    host: db.example.com
    port: 5432
    name: ilmdb
    username: ilmuser
    password: 54R0dFPL5Iwo2BS
  messaging:
    host: mq.example.com
    port: 5672
    virtualHost: ilm
    username: mquser
    password: super-mq-pass
  httpProxy: http://proxy:3128
image:
  registry: harbor.example.com
  repository: ilm
  name: core
  tag: "1.2.3"
  pullPolicy: Always
additionalEnv:
  variables:
    - name: OTEL_SDK_DISABLED
      value: "false"
logging:
  level: DEBUG
ingress:
  enabled: true
  class: nginx
  certificate:
    source: letsencrypt
letsEncrypt:
  email: ops@example.com
  environment: staging
`)
	r := Convert(values, "ilm", "ilm-ns")
	spec := r.Platform.Spec

	// metadata
	assert.Equal(t, "Platform", r.Platform.Kind)
	assert.Equal(t, "otilm.com/v1alpha1", r.Platform.APIVersion)
	assert.Equal(t, "ilm", r.Platform.Name)
	assert.Equal(t, "ilm-ns", r.Platform.Namespace)

	// shared image
	assert.Equal(t, "harbor.example.com", spec.Common.Image.Registry)
	assert.Equal(t, "ilm", spec.Common.Image.Repository)
	assert.Equal(t, "1.2.3", spec.Common.Image.Tag)
	assert.Equal(t, "Always", spec.Common.Image.PullPolicy)
	assert.Equal(t, []string{"regcred"}, spec.Common.Image.PullSecrets)

	// database (external) — credentials are a REF, never the password
	assert.Equal(t, "external", spec.Database.Mode)
	assert.Equal(t, "db.example.com", spec.Database.Host)
	assert.Equal(t, "ilmdb", spec.Database.Name)
	assert.Equal(t, int32(5432), spec.Database.Port)
	require.NotNil(t, spec.Database.Credentials)
	assert.Equal(t, dbSecretName, spec.Database.Credentials.SecretRef)

	// messaging (external) — host mapped, credentials a ref
	assert.Equal(t, "external", spec.Messaging.Mode)
	assert.Equal(t, "rabbitmq", spec.Messaging.BrokerType)
	assert.Equal(t, "mq.example.com", spec.Messaging.Host)
	assert.Equal(t, "ilm", spec.Messaging.VirtualHost)
	require.NotNil(t, spec.Messaging.Credentials)
	assert.Equal(t, messagingSecret, spec.Messaging.Credentials.SecretRef)

	// proxy
	assert.True(t, spec.Common.Proxy.Enabled)
	assert.Equal(t, "http://proxy:3128", spec.Common.Proxy.HTTP)

	// logging + additionalEnv
	assert.Equal(t, "DEBUG", spec.Common.Logging.Level)
	require.Len(t, spec.AdditionalEnv, 1)
	assert.Equal(t, "OTEL_SDK_DISABLED", spec.AdditionalEnv[0].Name)

	// edge + letsEncrypt
	require.NotNil(t, spec.Edge)
	assert.True(t, spec.Edge.Enabled)
	assert.Equal(t, "ingress", spec.Edge.Type)
	require.NotNil(t, spec.Edge.ClassName)
	assert.Equal(t, "nginx", *spec.Edge.ClassName)
	require.NotNil(t, spec.Edge.TLS)
	assert.Equal(t, "letsEncrypt", spec.Edge.TLS.Source)
	require.NotNil(t, spec.Edge.TLS.LetsEncrypt)
	assert.Equal(t, "ops@example.com", spec.Edge.TLS.LetsEncrypt.Email)
	assert.Equal(t, "staging", spec.Edge.TLS.LetsEncrypt.Environment)

	// SECRET SAFETY: render and assert no plaintext leaked
	rendered, err := r.Render()
	require.NoError(t, err)
	assertNoPlaintextSecrets(t, rendered)
}

// TestConvert_PgBouncerEnabledMapsToManaged asserts the Helm pgBouncer toggle
// (global.database.pgBouncer.enabled) converts to the CR's sole pooler switch,
// spec.database.pgBouncer.managed=true — so a converted managed-DB platform keeps the
// connection pooler the chart shipped. (The old code set the no-op Enabled field, which left
// managed=false and silently DISABLED the pooler, crash-looping Core on Flyway migrations.)
func TestConvert_PgBouncerEnabledMapsToManaged(t *testing.T) {
	values := decode(t, `
global:
  database:
    host: db.example.com
    name: ilmdb
    pgBouncer:
      enabled: true
`)
	spec := Convert(values, "ilm", "ilm-ns").Platform.Spec
	require.NotNil(t, spec.Database.PgBouncer, "an enabled Helm pgBouncer must produce a pgBouncer block")
	assert.True(t, spec.Database.PgBouncer.Managed,
		"Helm pgBouncer.enabled must map to CR pgBouncer.managed=true (pooler ON)")
}

// TestConvert_InlineSecretsBecomeRefsAndTODOs is the security heart of the suite: every
// inline secret must become a Secret REFERENCE plus a "# TODO: create" line, with no
// plaintext anywhere in the output.
func TestConvert_InlineSecretsBecomeRefsAndTODOs(t *testing.T) {
	values := decode(t, `
global:
  database:
    host: db.example.com
    name: ilmdb
    username: ilmuser
    password: 54R0dFPL5Iwo2BS
  keycloak:
    enabled: true
    clientSecret: s0qKH5qItTWoxpBt
  trusted:
    certificates: |
      -----BEGIN CERTIFICATE-----
      MIIBdummy
      -----END CERTIFICATE-----
  provisioning:
    apiUrl: https://prov.example.com
    apiKey: my-api-key
registerAdmin:
  enabled: true
  admin:
    username: admin
    email: admin@example.com
    certificate: |
      -----BEGIN CERTIFICATE-----
      MIIBadmin
      -----END CERTIFICATE-----
`)
	r := Convert(values, "ilm", "ilm")
	spec := r.Platform.Spec

	// refs, not values
	require.NotNil(t, spec.Database.Credentials)
	assert.Equal(t, dbSecretName, spec.Database.Credentials.SecretRef)
	assert.Equal(t, trustedCASecret, spec.Common.TrustedCertificates.SecretRef)
	require.NotNil(t, spec.RegisterAdmin)
	require.NotNil(t, spec.RegisterAdmin.Certificate)
	require.NotNil(t, spec.RegisterAdmin.Certificate.SecretRef)
	assert.Equal(t, adminCertSecret, *spec.RegisterAdmin.Certificate.SecretRef)
	require.NotNil(t, spec.Provisioning)
	assert.Equal(t, provisioningSecret, spec.Provisioning.APIKeySecretRef)

	// secret TODOs cover each inline secret
	names := map[string]bool{}
	for _, s := range r.secretTODOs {
		names[s.name] = true
	}
	assert.True(t, names[dbSecretName], "expected db secret TODO")
	assert.True(t, names[trustedCASecret], "expected trusted-ca secret TODO")
	assert.True(t, names[adminCertSecret], "expected admin-cert secret TODO")
	assert.True(t, names[provisioningSecret], "expected provisioning secret TODO")
	assert.True(t, names[keycloakSecret], "expected keycloak secret TODO")

	rendered, err := r.Render()
	require.NoError(t, err)
	assertNoPlaintextSecrets(t, rendered)

	// the rendered header carries a create-secret line for each Secret + a kubectl line
	assert.Contains(t, rendered, "TODO: create the following Secrets")
	assert.Contains(t, rendered, "kubectl create secret generic "+dbSecretName)
	assert.Contains(t, rendered, "kubectl create secret tls "+adminCertSecret)
	// the CR body references the Secret name
	assert.Contains(t, rendered, "secretRef: "+dbSecretName)
}

// TestConvert_UnmappedAndCustomizationFlags asserts the honest-gaps behavior: unknown
// top-level keys are flagged "# UNMAPPED", and connectors / bundled infra / keycloak get
// "# TODO(customization)" notes.
func TestConvert_UnmappedAndCustomizationFlags(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
  keycloak: { enabled: true }
logging:
  level: INFO
  audit:
    enabled: true
apiGateway:
  hostAliases:
    resolveInternalKeycloak: true
  logging:
    level: debug
ejbcaNgConnector:
  enabled: true
messagingService:
  image: { tag: "4.2.0" }
somethingTotallyUnknown:
  foo: bar
`)
	r := Convert(values, "ilm", "ilm")

	joinedUnmapped := strings.Join(r.unmapped, "\n")
	joinedCustom := strings.Join(r.customization, "\n")

	// genuinely unknown top-level key -> UNMAPPED
	assert.Contains(t, joinedUnmapped, "somethingTotallyUnknown")
	// logging.audit -> UNMAPPED
	assert.Contains(t, joinedUnmapped, "logging.audit")
	// apiGateway.logging.level + hostAliases -> UNMAPPED
	assert.Contains(t, joinedUnmapped, "apiGateway.logging.level")
	assert.Contains(t, joinedUnmapped, "apiGateway.hostAliases")

	// connectors -> customization (separate CRD)
	assert.Contains(t, joinedCustom, "ejbcaNgConnector")
	assert.Contains(t, joinedCustom, "Connector CRD")
	// bundled messaging -> customization (managed-mode decision)
	assert.Contains(t, joinedCustom, "messagingService")
	// keycloak managed sizing -> customization
	assert.Contains(t, joinedCustom, "keycloak.managed")

	// the footer renders both note kinds
	rendered, err := r.Render()
	require.NoError(t, err)
	assert.Contains(t, rendered, "# UNMAPPED:")
	assert.Contains(t, rendered, "# TODO(customization):")
}

// TestConvert_PerComponentOverrides covers per-component image/env/resources/replicas.
func TestConvert_PerComponentOverrides(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
authService:
  replicaCount: 3
  image:
    registry: harbor.example.com
    repository: ilm
    name: czertainly-auth
    tag: dev
    pullPolicy: Always
  resources:
    requests: { cpu: "500m", memory: 512Mi }
    limits: { memory: 1Gi }
  additionalEnv:
    variables:
      - name: ASPNETCORE_FORWARDEDHEADERS_ENABLED
        value: "true"
`)
	r := Convert(values, "ilm", "ilm")
	as := r.Platform.Spec.Auth.ComponentSpec

	require.NotNil(t, as.Replicas)
	assert.Equal(t, int32(3), *as.Replicas)
	assert.Equal(t, "czertainly-auth", as.Image.Name)
	assert.Equal(t, "dev", as.Image.Tag)
	require.NotNil(t, as.Resources)
	assert.Equal(t, "500m", as.Resources.Requests.Cpu().String())
	assert.Equal(t, "512Mi", as.Resources.Requests.Memory().String())
	assert.Equal(t, "1Gi", as.Resources.Limits.Memory().String())
	require.Len(t, as.Env, 1)
	assert.Equal(t, "ASPNETCORE_FORWARDEDHEADERS_ENABLED", as.Env[0].Name)
	assert.Equal(t, "true", as.Env[0].Value)
}

// TestConvert_Gateway covers apiGateway.trustedIps/cors/logging + global messaging
// remoteAccess -> spec.gateway.
func TestConvert_Gateway(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
  messaging:
    remoteAccess: true
apiGateway:
  trustedIps: "0.0.0.0/0,::/0"
  logging:
    request: true
  cors:
    enabled: true
    origins: ["https://app.example.com"]
    exposedHeaders: ["X-Auth-Token"]
`)
	r := Convert(values, "ilm", "ilm")
	gw := r.Platform.Spec.Gateway

	assert.Equal(t, []string{"0.0.0.0/0", "::/0"}, gw.TrustedIPs)
	assert.True(t, gw.Logging.Request)
	assert.True(t, r.Platform.Spec.Messaging.Management.Expose)
	assert.True(t, gw.Cors.Enabled)
	assert.Equal(t, []string{"https://app.example.com"}, gw.Cors.Origins)
	assert.Equal(t, []string{"X-Auth-Token"}, gw.Cors.ExposedHeaders)
}

// TestConvert_IngressExternalCertSource asserts the chart "external" cert source maps to
// the operator's "secret" (bring-your-own TLS) source with a customization note.
func TestConvert_IngressExternalCertSource(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
  hostName: ilm.example.com
ingress:
  enabled: true
  certificate:
    source: external
`)
	r := Convert(values, "ilm", "ilm")
	require.NotNil(t, r.Platform.Spec.Edge)
	require.NotNil(t, r.Platform.Spec.Edge.TLS)
	assert.Equal(t, "secret", r.Platform.Spec.Edge.TLS.Source)
	// The public FQDN is the canonical spec.common.hostName; the enabled edge derives its host
	// from it (PlatformHost), so the converter leaves edge.host empty (no redundant mirror).
	assert.Equal(t, "ilm.example.com", r.Platform.Spec.Common.HostName)
	assert.Empty(t, r.Platform.Spec.Edge.Host)
	assert.Contains(t, strings.Join(r.customization, "\n"), "edge.tls.source=secret")
}

// TestConvert_EnabledEdgeWithoutHostNameFlagged asserts that an enabled ingress with no
// chart hostName is flagged (the CRD requires a host when the edge is enabled) rather than
// emitting a CR the apiserver rejects, and that edge.host is left empty.
func TestConvert_EnabledEdgeWithoutHostNameFlagged(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
ingress:
  enabled: true
`)
	r := Convert(values, "ilm", "ilm")
	require.NotNil(t, r.Platform.Spec.Edge)
	assert.True(t, r.Platform.Spec.Edge.Enabled)
	assert.Empty(t, r.Platform.Spec.Edge.Host)
	assert.Empty(t, r.Platform.Spec.Common.HostName)
	assert.Contains(t, strings.Join(r.customization, "\n"), "public FQDN is REQUIRED when edge.enabled=true")
}

// TestConvert_JavaOptsMapsToJVMComponentEnv asserts the (removed) chart javaOpts is mapped
// to a JAVA_OPTS env entry on EACH JVM component (core, auth, scheduler, and
// the deploy-provisioning block) and NOT on the non-JVM components (fe-administrator/OPA),
// with a NOTE explaining the field moved.
func TestConvert_JavaOptsMapsToJVMComponentEnv(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
javaOpts: "-XX:MaxRAMPercentage=75.0"
`)
	r := Convert(values, "ilm", "ilm")
	spec := r.Platform.Spec

	want := "-XX:MaxRAMPercentage=75.0"
	// JVM components carry JAVA_OPTS.
	assert.Equal(t, want, envValue(spec.Core.Env, javaOptsEnvName), "core JAVA_OPTS")
	assert.Equal(t, want, envValue(spec.Auth.Env, javaOptsEnvName), "auth JAVA_OPTS")
	assert.Equal(t, want, envValue(spec.Scheduler.Env, javaOptsEnvName), "scheduler JAVA_OPTS")
	// Non-JVM components do NOT.
	assert.Empty(t, envValue(spec.FeAdministrator.Env, javaOptsEnvName), "fe-administrator must not get JAVA_OPTS")
	assert.Empty(t, envValue(spec.AuthOpaPolicies.Env, javaOptsEnvName), "auth-opa-policies must not get JAVA_OPTS")

	// The NOTE documents the move.
	assert.Contains(t, strings.Join(r.customization, "\n"), "javaOpts is no longer a CR field")
	// And no plaintext-secret regressions on render.
	rendered, err := r.Render()
	require.NoError(t, err)
	assertNoPlaintextSecrets(t, rendered)
}

// TestConvert_JavaOptsWiresDeployProvisioning asserts javaOpts also lands on the
// deploy-provisioning block when one is present (it is a JVM service too).
func TestConvert_JavaOptsWiresDeployProvisioning(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
  provisioning:
    apiUrl: https://prov.example.com
javaOpts: "-Xmx2g"
`)
	r := Convert(values, "ilm", "ilm")
	// Force a deploy block to prove the JVM mapping reaches it (the converter emits
	// external mode by default; here we assert the wiring path explicitly).
	require.NotNil(t, r.Platform.Spec.Provisioning)
	r.Platform.Spec.Provisioning.Deploy = &otilmv1alpha1.ProvisioningDeploySpec{}
	r.mapJavaOpts(values, &r.Platform.Spec)
	assert.Equal(t, "-Xmx2g", envValue(r.Platform.Spec.Provisioning.Deploy.Env, javaOptsEnvName))
	assert.Contains(t, strings.Join(r.customization, "\n"), "provisioning.deploy")
}

// envValue returns the value of the JAVA_OPTS env entry, or "" if absent. The JVM-tuning
// env is the only one these javaOpts tests inspect, so the name is fixed.
func envValue(env []otilmv1alpha1.EnvVar, name string) string { //nolint:unparam // name is fixed by the only callers; kept explicit for readability
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestConvert_EmptyValues asserts an empty values document yields a minimal CR (required
// external DB/messaging blocks present) and a clean render with no panic.
func TestConvert_EmptyValues(t *testing.T) {
	r := Convert(vals{}, "ilm", "ilm")
	assert.Equal(t, "external", r.Platform.Spec.Database.Mode)
	assert.Equal(t, "external", r.Platform.Spec.Messaging.Mode)
	rendered, err := r.Render()
	require.NoError(t, err)
	assert.Contains(t, rendered, "kind: Platform")
	// no secrets detected -> the header says so
	assert.Contains(t, rendered, "No inline secrets detected")
}

// TestConvert_MessagingExternalWithoutHostFlagged asserts the incomplete-external-broker
// case (chart bundled RabbitMQ, no host) is flagged rather than silently emitting an
// invalid CR.
func TestConvert_MessagingExternalWithoutHostFlagged(t *testing.T) {
	values := decode(t, `
global:
  database: { host: db, name: d, username: u, password: p }
  messaging:
    remoteAccess: false
`)
	r := Convert(values, "ilm", "ilm")
	assert.Empty(t, r.Platform.Spec.Messaging.Host)
	assert.Contains(t, strings.Join(r.customization, "\n"), "no external broker host")
}
