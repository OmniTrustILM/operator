/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package convert

import (
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

// testNamespace is the target namespace used across the Convert() call sites below.
const testNamespace = "ilm-ns"

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
	"54R0dFPL5Iwo2BS",      // db password
	"s0qKH5qItTWoxpBt",     // keycloak client secret
	"super-mq-pass",        // messaging password
	"my-api-key",           // provisioning api key
	"BEGIN CERTIFICATE",    // any inline PEM
	"SYNTHETIC_MCP_SECRET", // MCP client secret
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
	r := Convert(values, "ilm", testNamespace)
	spec := r.Platform.Spec

	// metadata
	assert.Equal(t, "Platform", r.Platform.Kind)
	assert.Equal(t, "otilm.com/v1alpha1", r.Platform.APIVersion)
	assert.Equal(t, "ilm", r.Platform.Name)
	assert.Equal(t, testNamespace, r.Platform.Namespace)

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
	assert.Equal(t, DefaultDatabaseSecretName, spec.Database.Credentials.SecretRef)

	// messaging (external) — host mapped, credentials a ref
	assert.Equal(t, "external", spec.Messaging.Mode)
	assert.Equal(t, "rabbitmq", spec.Messaging.BrokerType)
	assert.Equal(t, "mq.example.com", spec.Messaging.Host)
	assert.Equal(t, "ilm", spec.Messaging.VirtualHost)
	require.NotNil(t, spec.Messaging.Credentials)
	assert.Equal(t, DefaultMessagingSecretName, spec.Messaging.Credentials.SecretRef)

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
	spec := Convert(values, "ilm", testNamespace).Platform.Spec
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
	assert.Equal(t, DefaultDatabaseSecretName, spec.Database.Credentials.SecretRef)
	assert.Equal(t, DefaultTrustedCASecretName, spec.Common.TrustedCertificates.SecretRef)
	require.NotNil(t, spec.RegisterAdmin)
	require.NotNil(t, spec.RegisterAdmin.Certificate)
	require.NotNil(t, spec.RegisterAdmin.Certificate.SecretRef)
	assert.Equal(t, DefaultAdminCertSecretName, *spec.RegisterAdmin.Certificate.SecretRef)
	require.NotNil(t, spec.Provisioning)
	assert.Equal(t, DefaultProvisioningSecretName, spec.Provisioning.APIKeySecretRef)

	// secret TODOs cover each inline secret
	names := map[string]bool{}
	for _, s := range r.secretTODOs {
		names[s.name] = true
	}
	assert.True(t, names[DefaultDatabaseSecretName], "expected db secret TODO")
	assert.True(t, names[DefaultTrustedCASecretName], "expected trusted-ca secret TODO")
	assert.True(t, names[DefaultAdminCertSecretName], "expected admin-cert secret TODO")
	assert.True(t, names[DefaultProvisioningSecretName], "expected provisioning secret TODO")
	assert.True(t, names[DefaultKeycloakSecretName], "expected keycloak secret TODO")

	rendered, err := r.Render()
	require.NoError(t, err)
	assertNoPlaintextSecrets(t, rendered)

	// the rendered header carries a create-secret line for each Secret + a kubectl line
	assert.Contains(t, rendered, "TODO: create the following Secrets")
	assert.Contains(t, rendered, "kubectl create secret generic "+DefaultDatabaseSecretName)
	assert.Contains(t, rendered, "kubectl create secret tls "+DefaultAdminCertSecretName)
	// the CR body references the Secret name
	assert.Contains(t, rendered, "secretRef: "+DefaultDatabaseSecretName)
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

// TestConvertMapsCoreFeatureValues proves the 2.19.0 Core values reach the CR instead of the
// UNMAPPED footer: workloadType, platformInstanceId, the messaging time-quality toggle (local
// and global forms), and the monitor sidecar block.
func TestConvertMapsCoreFeatureValues(t *testing.T) {
	values := vals{
		"workloadType":       "StatefulSet",
		"platformInstanceId": 42,
		"messaging":          vals{"timeQuality": vals{"enabled": true}},
	}
	values["timeQualityMonitor"] = vals{
		"enabled": true,
		"image": vals{
			"registry": "hub.omnitrustregistry.com", "repository": "ilm-private",
			"name": "time-quality-monitor", "tag": "1.0.0",
			"pullSecrets": []interface{}{"registry-credentials"},
			"resources":   vals{"requests": vals{"cpu": "50m", "memory": "150M"}},
			"probes":      vals{"liveness": vals{"initialDelaySeconds": 9}},
		},
		"logging": vals{"level": "DEBUG"},
	}
	r := Convert(values, "ilm", "ilm")
	spec := r.Platform.Spec

	assert.Equal(t, otilmv1alpha1.WorkloadKindStatefulSet, spec.Core.WorkloadType)
	require.NotNil(t, spec.Core.InstanceID)
	assert.Equal(t, int32(42), *spec.Core.InstanceID)
	assert.True(t, spec.Messaging.TimeQuality.Enabled)
	require.NotNil(t, spec.Core.TimeQualityMonitor)
	assert.True(t, spec.Core.TimeQualityMonitor.Enabled)

	m := spec.Core.TimeQualityMonitor
	require.NotNil(t, m)
	assert.Equal(t, "ilm-private", m.Image.Repository)
	assert.Equal(t, "1.0.0", m.Image.Tag)
	assert.Equal(t, []string{"registry-credentials"}, m.Image.PullSecrets)
	require.NotNil(t, m.Resources, "the chart nests resources under image — they must NOT be dropped")
	assert.Equal(t, "50m", m.Resources.Requests.Cpu().String())

	out, err := r.Render()
	require.NoError(t, err)
	for _, key := range []string{"workloadType", "platformInstanceId", "timeQualityMonitor", "messaging"} {
		// The trailing newline pins this to the BARE top-level report (flagUnmappedTopLevel
		// appends just the key, so that line reads "# UNMAPPED: <key>\n"). Without it, the
		// nested per-sub-key reports required below ("timeQualityMonitor.image.probes (...)")
		// would false-positive: they legitimately start with "# UNMAPPED: timeQualityMonitor".
		assert.NotContains(t, out, "# UNMAPPED: "+key+"\n",
			"a block whose keys were all consumed must not be reported as unmapped")
	}

	// Everything the CR cannot express must be reported BY NAME. Marking the block known is
	// what stops the top-level reporter, so the nested reporter owes the user the difference.
	assert.Contains(t, out, "timeQualityMonitor.image.probes")
	assert.Contains(t, out, "timeQualityMonitor.logging")
}

// TestConvertMessagingBlockIsReportedPerSubKey pins the other half of the messaging contract.
// The chart-local messaging block is only PARTLY mapped — timeQuality maps, the broker
// coordinates come from global.messaging — and flagUnmappedTopLevel inspects top-level keys
// only, so the block is all-or-nothing there. Left out of knownTopLevel it prints
// "# UNMAPPED: messaging" even when its only key was fully consumed (the case above); put in
// without per-sub-key accounting it swallows the coordinates silently. This asserts the third
// option: registered, with every UNCONSUMED sub-key named by its exact path.
func TestConvertMessagingBlockIsReportedPerSubKey(t *testing.T) {
	r := Convert(vals{
		"messaging": vals{
			"timeQuality": vals{"enabled": true},
			"host":        "rabbit.local",
			"port":        5672,
		},
	}, "ilm", "ilm")
	out, err := r.Render()
	require.NoError(t, err)

	assert.True(t, r.Platform.Spec.Messaging.TimeQuality.Enabled)
	assert.NotContains(t, out, "# UNMAPPED: messaging\n",
		"the whole block must never be reported once it is registered")
	assert.Contains(t, out, "messaging.host")
	assert.Contains(t, out, "messaging.port")
	assert.NotContains(t, out, "messaging.timeQuality",
		"a sub-key that WAS consumed must not be reported")
}

// TestConvertChartConnectorBlocksRouteToConnectorCRD pins the connector routing for the
// umbrella chart's full per-connector subchart surface — including the three aliases the
// converter previously did not know (externalAuthorityProvider, otpkiConnector,
// timestampFormattingConnector): before they were added to connectorKeys, a values.yaml
// enabling one of them rendered a bare "# UNMAPPED: <key>" footer line instead of the
// Connector-CRD customization guidance every other connector block gets. Each case asserts
// both halves: the key appears in the "Connector CRD" TODO, and the bare top-level UNMAPPED
// report is gone.
func TestConvertChartConnectorBlocksRouteToConnectorCRD(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "externalAuthorityProvider was unmapped", key: "externalAuthorityProvider"},
		{name: "otpkiConnector was unmapped", key: "otpkiConnector"},
		{name: "timestampFormattingConnector was unmapped", key: "timestampFormattingConnector"},
		{name: "ejbcaNgConnector already mapped (regression guard)", key: "ejbcaNgConnector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Convert(vals{tt.key: vals{"enabled": true}}, "ilm", testNamespace)
			out, err := r.Render()
			require.NoError(t, err)

			joinedCustom := strings.Join(r.customization, "\n")
			assert.Contains(t, joinedCustom, tt.key,
				"the connector block must be named in the Connector-CRD customization TODO")
			assert.Contains(t, joinedCustom, "Connector CRD",
				"the guidance must route the user to the separate Connector CRD")
			assert.NotContains(t, out, "# UNMAPPED: "+tt.key+"\n",
				"a chart connector block must not fall through to the bare unmapped footer")
		})
	}

	// All chart connector aliases together produce ONE sorted customization line naming each.
	values := vals{}
	for _, tt := range tests {
		values[tt.key] = vals{"enabled": true}
	}
	r := Convert(values, "ilm", testNamespace)
	joinedCustom := strings.Join(r.customization, "\n")
	assert.Contains(t, joinedCustom,
		"ejbcaNgConnector, externalAuthorityProvider, otpkiConnector, timestampFormattingConnector",
		"the connector TODO lists every found connector block, sorted")
}

// TestConvertMapsLoggingFormat proves logging.format reaches spec.common.logging.format in the
// form the CRD enum accepts: the chart trims and lower-cases the value before rendering it.
func TestConvertMapsLoggingFormat(t *testing.T) {
	r := Convert(vals{"logging": vals{"level": "INFO", "format": " ECS "}}, "ilm", "ilm")
	assert.Equal(t, "ecs", r.Platform.Spec.Common.Logging.Format)

	r = Convert(vals{"logging": vals{"level": "INFO"}}, "ilm", "ilm")
	assert.Empty(t, r.Platform.Spec.Common.Logging.Format, "an unset format leaves Core's own default")
}

// reportedLine returns the footer entry the converter recorded for a values path: the bare path, or the path
// followed by its reason in parentheses.
func reportedLine(r *Result, path string) (string, bool) {
	for _, line := range r.unmapped {
		if line == path || strings.HasPrefix(line, path+" (") {
			return line, true
		}
	}
	return "", false
}

// unmappedLine returns the footer entry for a values path, failing the test when the path was not reported.
func unmappedLine(t *testing.T, r *Result, path string) string {
	t.Helper()
	line, ok := reportedLine(r, path)
	require.Truef(t, ok, "%s was not reported as unmapped", path)
	return line
}

// TestConvertMapsMCP proves the umbrella's MCP values reach spec.mcp: the three global.mcp settings, the access
// level, and the mcpServer component overrides. Everything the fixture sets is read by the conversion, so none of it
// is reported as unmapped.
func TestConvertMapsMCP(t *testing.T) {
	values := decode(t, `
global:
  mcp:
    enabled: true
    cimd:
      enabled: false
    delegation:
      group: ilm-mcp-users
mcpServer:
  access: read-write
  replicaCount: 2
  image:
    registry: harbor.example.com
    repository: ilm
    name: mcp-server
    tag: "0.3.0"
    pullPolicy: Always
    resources:
      requests: { cpu: "200m", memory: 384Mi }
      limits: { memory: 1Gi }
  additionalEnv:
    variables:
      - name: MCP_EXTRA_FLAG
        value: "true"
`)
	r := Convert(values, "ilm", testNamespace)
	mcp := r.Platform.Spec.MCP

	assert.True(t, mcp.Enabled)
	require.NotNil(t, mcp.CIMD.Enabled, "an explicit false is a setting, not an absence")
	assert.False(t, *mcp.CIMD.Enabled)
	assert.Equal(t, "ilm-mcp-users", mcp.Delegation.Group)
	assert.Equal(t, "read-write", mcp.Access)

	require.NotNil(t, mcp.Replicas)
	assert.Equal(t, int32(2), *mcp.Replicas)
	assert.Equal(t, "harbor.example.com", mcp.Image.Registry)
	assert.Equal(t, "ilm", mcp.Image.Repository)
	assert.Equal(t, "mcp-server", mcp.Image.Name)
	assert.Equal(t, "0.3.0", mcp.Image.Tag)
	assert.Equal(t, "Always", mcp.Image.PullPolicy)
	require.NotNil(t, mcp.Resources)
	assert.Equal(t, "200m", mcp.Resources.Requests.Cpu().String())
	assert.Equal(t, "1Gi", mcp.Resources.Limits.Memory().String())
	require.Len(t, mcp.Env, 1)
	assert.Equal(t, "MCP_EXTRA_FLAG", mcp.Env[0].Name)
	assert.Equal(t, "true", mcp.Env[0].Value)

	assert.Empty(t, r.unmapped, "every value in the fixture is read, so none is reported")

	// The fixture has no managed Keycloak and no public host. The Platform's admission rules say so when it is
	// applied, so the converter adds no MCP prerequisite note of its own.
	for _, note := range r.customization {
		assert.NotContains(t, strings.ToLower(note), "mcp")
	}
}

// quantities flattens a resource list into resource name -> quantity, so a test can compare exactly which resources
// were converted.
func quantities(list corev1.ResourceList) map[string]string {
	out := make(map[string]string, len(list))
	for name, quantity := range list {
		out[string(name)] = quantity.String()
	}
	return out
}

// TestConvertMapsMCPResourcesFromTheChartsKey pins where MCP's resources come from. The mcp-server chart keeps them under
// image, so that key is converted. The top-level key the component pass reads for every service still works when it is
// the only one set, and the chart's key wins, whole and unmerged, when both are set.
func TestConvertMapsMCPResourcesFromTheChartsKey(t *testing.T) {
	tests := []struct {
		name         string
		values       string
		wantRequests map[string]string
		wantLimits   map[string]string
	}{
		{
			name:         "the chart's key",
			values:       `mcpServer: {image: {resources: {requests: {cpu: 200m, memory: 384Mi}, limits: {memory: 1Gi}}}}`,
			wantRequests: map[string]string{"cpu": "200m", "memory": "384Mi"},
			wantLimits:   map[string]string{"memory": "1Gi"},
		},
		{
			name:         "the top-level key alone",
			values:       `mcpServer: {resources: {requests: {cpu: 900m}, limits: {memory: 2Gi}}}`,
			wantRequests: map[string]string{"cpu": "900m"},
			wantLimits:   map[string]string{"memory": "2Gi"},
		},
		{
			name: "both keys: the chart's wins",
			values: `mcpServer: {image: {resources: {requests: {cpu: 200m}}},
  resources: {requests: {cpu: 900m, memory: 2Gi}, limits: {memory: 4Gi}}}`,
			wantRequests: map[string]string{"cpu": "200m"},
			wantLimits:   map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Convert(decode(t, tt.values), "ilm", testNamespace)

			resources := r.Platform.Spec.MCP.Resources
			require.NotNil(t, resources)
			assert.Equal(t, tt.wantRequests, quantities(resources.Requests))
			assert.Equal(t, tt.wantLimits, quantities(resources.Limits))
		})
	}
}

// TestConvertCopiesMCPCIMDOnlyWhenSet pins the rule for global.mcp.cimd.enabled. The Platform counts an unset value as
// true, which is also the chart's default, so the converter copies the setting only when the values file carries it,
// true or false, and otherwise leaves the field unset.
func TestConvertCopiesMCPCIMDOnlyWhenSet(t *testing.T) {
	tests := []struct {
		name   string
		values string
		want   *bool
	}{
		{name: "no mcp block", values: `global: {}`, want: nil},
		{name: "no cimd block", values: `global: {mcp: {enabled: true}}`, want: nil},
		{name: "empty cimd block", values: `global: {mcp: {enabled: true, cimd: {}}}`, want: nil},
		{name: "the chart default is copied", values: `global: {mcp: {cimd: {enabled: true}}}`, want: ptrTo(true)},
		{name: "false is copied", values: `global: {mcp: {cimd: {enabled: false}}}`, want: ptrTo(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Convert(decode(t, tt.values), "ilm", testNamespace)
			assert.Equal(t, tt.want, r.Platform.Spec.MCP.CIMD.Enabled)
		})
	}
}

// TestConvertWithoutMCPValuesLeavesMCPOff pins that a values file that never turns MCP on converts to a Platform with
// an untouched MCP section and nothing to report: either no MCP values at all, or only the chart's own off defaults.
func TestConvertWithoutMCPValuesLeavesMCPOff(t *testing.T) {
	tests := []struct {
		name   string
		values string
	}{
		{name: "no MCP values", values: `global: {hostName: ilm.example.com}`},
		{name: "the chart's off defaults", values: `
global:
  mcp:
    enabled: false
    clientSecret: ""
    clientSecretRef: {}
    delegation:
      group: ""
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Convert(decode(t, tt.values), "ilm", testNamespace)
			assert.Equal(t, otilmv1alpha1.MCPSpec{}, r.Platform.Spec.MCP)
			assert.Empty(t, r.unmapped)
			assert.Empty(t, r.secretTODOs)
		})
	}
}

// TestConvertDoesNotCopyTheMCPClientSecret proves MCP's client secret never leaves the values file: neither the value
// nor a reference to a Secret holding it reaches the Platform or the report, and the report says why. The operator
// generates the secret with MCP's Keycloak client and relays it, so no Secret is scaffolded either.
func TestConvertDoesNotCopyTheMCPClientSecret(t *testing.T) {
	values := decode(t, `
global:
  mcp:
    enabled: true
    clientSecret: SYNTHETIC_MCP_SECRET
    clientSecretRef:
      name: synthetic-mcp-secret-ref
      key: synthetic-mcp-secret-key
mcpServer:
  security:
    clientSecretRef:
      name: synthetic-mcp-secret-ref
      key: synthetic-mcp-secret-key
`)
	r := Convert(values, "ilm", testNamespace)
	rendered, err := r.Render()
	require.NoError(t, err)

	assertNoPlaintextSecrets(t, rendered)
	for _, ref := range []string{"synthetic-mcp-secret-ref", "synthetic-mcp-secret-key"} {
		assert.NotContains(t, rendered, ref, "a reference to MCP's client secret is not copied either")
	}
	assert.Empty(t, r.secretTODOs, "the operator manages the secret, so none is scaffolded")

	for _, path := range []string{"global.mcp.clientSecret", "global.mcp.clientSecretRef", "mcpServer.security.clientSecretRef"} {
		assert.Contains(t, unmappedLine(t, r, path), "the operator manages MCP's client secret", path)
	}
}

// TestConvertReportsMCPSecurityAsUnmapped pins that the values the operator derives, or does not render for MCP at all,
// are reported, not dropped. mcpServer.security is reported key by key; an issuer, key set or token endpoint says it
// points MCP at another identity provider and that the conversion did not keep it. mcpServer.networkPolicy is reported
// as one block that says what isolates MCP after the migration.
func TestConvertReportsMCPSecurityAsUnmapped(t *testing.T) {
	values := decode(t, `
mcpServer:
  security:
    clientId: ilm-mcp
    exchangeMode: delegation
    exchangeScope: ""
    issuer: https://idp.example.invalid/realms/other
    jwkSetUri: https://idp.example.invalid/realms/other/protocol/openid-connect/certs
    tokenUri: https://idp.example.invalid/realms/other/protocol/openid-connect/token
  networkPolicy:
    enabled: true
    ingress:
      - podSelector: {matchLabels: {app.kubernetes.io/name: api-gateway}}
`)
	r := Convert(values, "ilm", testNamespace)
	rendered, err := r.Render()
	require.NoError(t, err)

	for _, path := range []string{"mcpServer.security.clientId", "mcpServer.security.exchangeMode"} {
		assert.Contains(t, unmappedLine(t, r, path), "the operator derives", path)
	}
	// The operator renders no policy for MCP alone, so a reader who had enabled one must learn that MCP falls under the
	// looser platform-wide policies, not that the operator derived an equivalent.
	networkPolicy := unmappedLine(t, r, "mcpServer.networkPolicy")
	assert.Contains(t, networkPolicy, "renders no NetworkPolicy for MCP alone")
	assert.Contains(t, networkPolicy, "platform-wide")
	assert.Contains(t, networkPolicy, "looser")
	assert.NotContains(t, networkPolicy, "derives")
	for _, path := range []string{"mcpServer.security.issuer", "mcpServer.security.jwkSetUri", "mcpServer.security.tokenUri"} {
		line := unmappedLine(t, r, path)
		assert.Contains(t, line, "not converted", path)
		assert.Contains(t, line, "another identity provider", path)
	}

	// The report names paths only. No endpoint reaches the output, so nothing reads as if the conversion kept
	// the other provider.
	assert.NotContains(t, rendered, "idp.example.invalid")
	_, bareBlock := reportedLine(r, "mcpServer.security")
	assert.False(t, bareBlock, "security is accounted for key by key")
	_, emptyValue := reportedLine(r, "mcpServer.security.exchangeScope")
	assert.False(t, emptyValue, "an empty value is a default, not a setting to report")
}

// TestConvertReportsUnmappedMCPServerKeys pins that an mcpServer value the Platform does not take is reported by its
// exact path, never dropped: keys with no Platform field, such as toolsets, and the nested keys of the blocks the
// conversion reads only in part (image, additionalEnv). The values it does read are not reported, and no value is
// printed.
func TestConvertReportsUnmappedMCPServerKeys(t *testing.T) {
	values := decode(t, `
mcpServer:
  access: read-only
  replicaCount: 1
  toolsets: dropped-toolsets
  apiUrl: http://dropped.invalid/api
  nameOverride: dropped-name
  service:
    name: dropped-service
  fullnameOverride: ""
  nodeSelector: {}
  tolerations: []
  image:
    tag: "0.3.0"
    digest: dropped-digest
    pullSecrets: []
  additionalEnv:
    variables:
      - { name: MCP_EXTRA_FLAG, value: "true" }
    configMaps: []
    secrets: [dropped-secret]
`)
	r := Convert(values, "ilm", testNamespace)
	rendered, err := r.Render()
	require.NoError(t, err)

	for _, path := range []string{
		"mcpServer.toolsets", "mcpServer.apiUrl", "mcpServer.nameOverride", "mcpServer.service",
		"mcpServer.image.digest", "mcpServer.additionalEnv.secrets",
	} {
		assert.Equal(t, path, unmappedLine(t, r, path), "a value with no mapping is reported by its bare path")
	}
	for _, path := range []string{
		"mcpServer", "mcpServer.access", "mcpServer.replicaCount", "mcpServer.image", "mcpServer.image.tag",
		"mcpServer.additionalEnv", "mcpServer.additionalEnv.variables",
	} {
		_, reported := reportedLine(r, path)
		assert.Falsef(t, reported, "%s is read by the conversion, so it must not be reported", path)
	}
	for _, path := range []string{
		"mcpServer.fullnameOverride", "mcpServer.nodeSelector", "mcpServer.tolerations",
		"mcpServer.image.pullSecrets", "mcpServer.additionalEnv.configMaps",
	} {
		_, reported := reportedLine(r, path)
		assert.Falsef(t, reported, "%s is empty, so there is nothing to report", path)
	}

	assert.NotContains(t, rendered, "dropped-", "the report names paths only, never values")
}

// TestConvertReportsAnMCPDelegationGroupItCannotCopy pins that a delegation group the converter cannot read as a string,
// such as an unquoted number, is reported. Dropping it silently would widen MCP from one group's members to every user.
func TestConvertReportsAnMCPDelegationGroupItCannotCopy(t *testing.T) {
	r := Convert(decode(t, `global: {mcp: {enabled: true, delegation: {group: 2024}}}`), "ilm", testNamespace)

	assert.Empty(t, r.Platform.Spec.MCP.Delegation.Group)
	assert.Contains(t, unmappedLine(t, r, "global.mcp.delegation.group"), "MCP acts for every user")
}
