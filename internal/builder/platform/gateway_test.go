/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

// envMap flattens a Component's inline env pairs into a name->value map.
func envMap(c common.Component) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func TestResolveGatewayWorkload(t *testing.T) {
	c := ResolveGateway(basePlatform())

	assert.Equal(t, gatewayName, c.Name)
	assert.Equal(t, "ilm", c.Instance)
	assert.Equal(t, "ilm-system", c.Namespace)
	// Image comes from the BOM (kong:3.9.1) joined with the shared registry/repository.
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/kong:3.9.1", c.Image)

	// Primary container port is the consumer (proxy) port, named consumer-http; the
	// admin port is an additional named port.
	assert.Equal(t, int32(gatewayConsumerPort), c.Port)
	assert.Equal(t, gatewayConsumerPortName, c.PrimaryPortName)
	require.Len(t, c.AdditionalPorts, 1)
	assert.Equal(t, gatewayAdminPortName, c.AdditionalPorts[0].Name)
	assert.Equal(t, int32(gatewayAdminPort), c.AdditionalPorts[0].ContainerPort)

	// Service exposes admin/consumer/status.
	ports := map[string]int32{}
	for _, p := range c.ServicePorts {
		ports[p.Name] = p.Port
	}
	assert.Equal(t, map[string]int32{"admin": 8001, "consumer": 8000, "status": 8100}, ports)
}

func TestResolveGatewayEnv(t *testing.T) {
	env := envMap(ResolveGateway(basePlatform()))

	assert.Equal(t, "off", env["KONG_DATABASE"])
	assert.Equal(t, "/tmp/", env["KONG_PREFIX"])
	assert.Equal(t, gatewayDeclarativeConfig, env["KONG_DECLARATIVE_CONFIG"])
	assert.Equal(t, "0.0.0.0:8001, 0.0.0.0:8444 ssl", env["KONG_ADMIN_LISTEN"])
	assert.Equal(t, "0.0.0.0:8100", env["KONG_STATUS_LISTEN"])
	assert.Equal(t, gatewayPlugins, env["KONG_PLUGINS"])
	assert.Equal(t, gatewayLogLevel, env["KONG_LOG_LEVEL"])
	// Stdout/stderr log destinations.
	assert.Equal(t, "/dev/stdout", env["KONG_PROXY_ACCESS_LOG"])
	assert.Equal(t, "/dev/stderr", env["KONG_PROXY_ERROR_LOG"])

	// KONG_TRUSTED_IPS is omitted unless trustedIps is set.
	_, ok := env["KONG_TRUSTED_IPS"]
	assert.False(t, ok, "KONG_TRUSTED_IPS must be omitted when trustedIps is empty")
}

func TestResolveGatewayTrustedIPs(t *testing.T) {
	p := basePlatform()
	p.Spec.Gateway.TrustedIPs = []string{"0.0.0.0/0", "::/0"}
	env := envMap(ResolveGateway(p))
	assert.Equal(t, "0.0.0.0/0,::/0", env["KONG_TRUSTED_IPS"])
}

func TestResolveGatewayProbesAreKongHealthExec(t *testing.T) {
	c := ResolveGateway(basePlatform())

	require.NotNil(t, c.Probes.Readiness)
	require.NotNil(t, c.Probes.Startup)
	// liveness is disabled by default.
	assert.Nil(t, c.Probes.Liveness)

	for _, pr := range []*corev1.Probe{c.Probes.Readiness, c.Probes.Startup} {
		require.NotNil(t, pr.Exec, "probe must be an exec probe")
		assert.Equal(t, []string{"kong", "health"}, pr.Exec.Command)
	}
	// Startup tolerates a long boot.
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

func TestResolveGatewayDeclarativeMount(t *testing.T) {
	c := ResolveGateway(basePlatform())

	// The kong.yml ConfigMap is projected at the declarative path.
	var mountPath string
	for _, vm := range c.VolumeMounts {
		if vm.Name == gatewayConfigVolume {
			mountPath = vm.MountPath
		}
	}
	assert.Equal(t, gatewayDeclarativeMount, mountPath)

	// The config volume sources the global ConfigMap's kong.yml key.
	var found bool
	for _, v := range c.Volumes {
		if v.Name == gatewayConfigVolume {
			require.NotNil(t, v.ConfigMap)
			assert.Equal(t, globalConfigMapName, v.ConfigMap.Name)
			require.Len(t, v.ConfigMap.Items, 1)
			assert.Equal(t, gatewayConfigKey, v.ConfigMap.Items[0].Key)
			found = true
		}
	}
	assert.True(t, found, "gateway must mount the global ConfigMap's kong.yml")
}

// parseKong unmarshals the rendered kong.yml back into the typed config for
// structural assertions.
func parseKong(t *testing.T, p *corev1.ConfigMap) kongConfig {
	t.Helper()
	require.Contains(t, p.Data, gatewayConfigKey)
	var cfg kongConfig
	require.NoError(t, yaml.Unmarshal([]byte(p.Data[gatewayConfigKey]), &cfg))
	return cfg
}

// serviceByName returns the named kong service, or false if absent.
func serviceByName(cfg kongConfig, name string) (kongService, bool) {
	for _, s := range cfg.Services {
		if s.Name == name {
			return s, true
		}
	}
	return kongService{}, false
}

func TestBuildGlobalConfigMapBaseRoutes(t *testing.T) {
	cm := BuildGlobalConfigMap(basePlatform())

	assert.Equal(t, globalConfigMapName, cm.Name)
	assert.Equal(t, "ilm-system", cm.Namespace)
	assert.Equal(t, gatewayConfigMapRole, cm.Labels[common.ComponentLabel],
		"component label makes the global-configmap selectable by role (role global)")

	cfg := parseKong(t, cm)
	assert.Equal(t, "2.1", cfg.FormatVersion)
	assert.True(t, cfg.Transform)

	// core route /api points at the operator's clean Service name (NOT core-service).
	core, ok := serviceByName(cfg, "core")
	require.True(t, ok)
	assert.Equal(t, gatewayCoreHost, core.Host)
	assert.Equal(t, gatewayUpstreamPort, core.Port)
	require.Len(t, core.Routes, 1)
	assert.Equal(t, []string{gatewayCoreAPIPath}, core.Routes[0].Paths)

	// login/logout routes via the core-login-logout service.
	loginSvc, ok := serviceByName(cfg, "core-login-logout")
	require.True(t, ok)
	assert.Equal(t, gatewayCoreHost, loginSvc.Host)
	assert.Equal(t, gatewayCoreAPIPath, loginSvc.Path)
	assert.Equal(t, []string{"/login", "/logout"}, loginSvc.Routes[0].Paths)

	// fe-administrator route /administrator points at the clean fe Service name.
	fe, ok := serviceByName(cfg, "fe-administrator")
	require.True(t, ok)
	assert.Equal(t, gatewayFeHost, fe.Host)
	assert.Equal(t, []string{gatewayFeBasePath}, fe.Routes[0].Paths)
}

func TestBuildGlobalConfigMapConditionalsOff(t *testing.T) {
	// Default Platform: utils off, no cors, no file-log, no /mq, no /kc.
	cfg := parseKong(t, BuildGlobalConfigMap(basePlatform()))

	_, hasUtils := serviceByName(cfg, "utils")
	assert.False(t, hasUtils, "utils route must be absent when utils is disabled")
	_, hasMq := serviceByName(cfg, testMessagingService)
	assert.False(t, hasMq, "/mq route must be absent when management.expose is off")
	// No /kc route for a non-managed Keycloak (the default Platform): an external Keycloak uses
	// its own URL, so the gateway never proxies it.
	_, hasKc := serviceByName(cfg, "keycloak")
	assert.False(t, hasKc, "/kc route must be absent when Keycloak is not managed")
	// The MCP server is off by default, and its route must not point the gateway at a Service that
	// does not exist.
	_, hasMCP := serviceByName(cfg, testMCPName)
	assert.False(t, hasMCP, "the MCP route must be absent when the MCP server is off")

	assert.Empty(t, cfg.Plugins, "no plugins when cors and logging.request are off")
}

// TestBuildGlobalConfigMapKeycloakRouteWhenManaged asserts the gateway proxies /kc to the managed
// Keycloak Service so its OIDC issuer + admin console are reachable through the edge.
func TestBuildGlobalConfigMapKeycloakRouteWhenManaged(t *testing.T) {
	p := managedKCPlatform(nil)
	cfg := parseKong(t, BuildGlobalConfigMap(p))
	kc, ok := serviceByName(cfg, "keycloak")
	require.True(t, ok, "managed Keycloak must add a /kc gateway route")
	assert.Equal(t, ManagedKeycloakServiceName(p), kc.Host)
	assert.Equal(t, ManagedKeycloakServicePort, kc.Port)
	require.Len(t, kc.Routes, 1)
	assert.Equal(t, []string{KeycloakRelativePath}, kc.Routes[0].Paths, "the gateway routes /kc to Keycloak")
	assert.False(t, kc.Routes[0].StripPath, "strip_path=false so the /kc prefix reaches Keycloak")
	assert.True(t, kc.Routes[0].PreserveHost, "preserve_host so Keycloak builds correct URLs")
}

func TestBuildGlobalConfigMapUtilsRoute(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	utils, ok := serviceByName(cfg, "utils")
	require.True(t, ok, "utils route must be present when utils is enabled")
	assert.Equal(t, gatewayUtilsHost, utils.Host, "utils route uses the clean Service name")
	assert.Equal(t, []string{gatewayUtilsPath}, utils.Routes[0].Paths)
}

func TestBuildGlobalConfigMapMqRouteAbsentForExternal(t *testing.T) {
	// EXTERNAL RabbitMQ: /mq must NOT be rendered even with management.expose on. The operator
	// does not own an external broker and must not assume or proxy its management endpoint;
	// /mq is only meaningful for managed messaging (the operator-provisioned RabbitmqCluster,
	// whose in-cluster management Service the operator knows).
	p := basePlatform() // external messaging (rabbitmq)
	p.Spec.Messaging.Management.Expose = true
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	_, ok := serviceByName(cfg, testMessagingService)
	assert.False(t, ok, "/mq route must be absent for external messaging even with management.expose on")
}

func TestBuildGlobalConfigMapMqRouteManaged(t *testing.T) {
	// Managed RabbitMQ: the /mq upstream host is the RabbitmqCluster broker Service
	// "<name>-messaging" (resolved from the messaging connection), matching the AMQP host.
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Management.Expose = true
	})
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	mq, ok := serviceByName(cfg, testMessagingService)
	require.True(t, ok, "/mq route must be present for managed rabbitmq + management.expose")
	assert.Equal(t, ManagedMessagingName(p), mq.Host,
		"/mq host must resolve to the managed broker Service name")
	assert.Equal(t, "ilm-messaging", mq.Host, "managed broker Service is <name>-messaging")
	assert.Equal(t, gatewayMessagingMgmtPort, mq.Port, "/mq targets the broker management port")
	// Two routes make the UI load with OR without the trailing slash (paired with the broker's
	// management.path_prefix=/mq): the UI under /mq/ forwarded unstripped, and the bare /mq
	// (regex-anchored) stripped to "/" so RabbitMQ's own "/" -> "/mq/" redirect fires.
	require.Len(t, mq.Routes, 2)
	ui := mq.Routes[0]
	assert.Equal(t, "rabbitmq", ui.Name)
	assert.Equal(t, []string{gatewayMqPath + "/"}, ui.Paths, "UI route serves /mq/ (and deeper)")
	assert.False(t, ui.StripPath, "UI route forwards /mq/ unstripped so RabbitMQ sees its path_prefix")
	assert.True(t, ui.PreserveHost)
	redir := mq.Routes[1]
	assert.Equal(t, "rabbitmq-slash-redirect", redir.Name)
	assert.Equal(t, []string{gatewayMqPath}, redir.Paths, "the shorter /mq prefix catches only the bare path")
	assert.True(t, redir.StripPath, "bare /mq strips to / so RabbitMQ issues its / -> /mq/ redirect")
	assert.True(t, redir.PreserveHost, "preserve_host so RabbitMQ's redirect targets the edge host")
}

func TestBuildGlobalConfigMapMqRouteAbsentForServiceBus(t *testing.T) {
	// Azure Service Bus is external-only and has no management UI, so /mq is never rendered
	// for it (covered both by the managed-only gate and the no-mgmt-UI rationale).
	p := basePlatform()
	p.Spec.Messaging.BrokerType = "servicebus"
	p.Spec.Messaging.Management.Expose = true
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	_, ok := serviceByName(cfg, testMessagingService)
	assert.False(t, ok, "/mq route must be absent for brokerType=servicebus even with management.expose on")
}

// removedHeaders returns the request-transformer config that strips the named request headers, as
// it reads back from a rendered kong.yml.
func removedHeaders(names ...string) map[string]interface{} {
	headers := make([]interface{}, len(names))
	for i, name := range names {
		headers[i] = name
	}
	return map[string]interface{}{"remove": map[string]interface{}{"headers": headers}}
}

// TestBuildGlobalConfigMapMCPRoute pins the gateway service that publishes the MCP server: its
// upstream, the two public paths, unbuffered responses, and the removal of Core's client-certificate
// header.
func TestBuildGlobalConfigMapMCPRoute(t *testing.T) {
	cfg := parseKong(t, BuildGlobalConfigMap(mcpPlatform(nil)))

	svc, ok := serviceByName(cfg, testMCPName)
	require.True(t, ok, "an enabled MCP server must add a gateway service")
	assert.Equal(t, testMCPName, svc.Host, "the gateway reaches MCP by its Service name")
	assert.Equal(t, 8080, svc.Port)
	assert.Equal(t, "http", svc.Protocol)
	assert.Empty(t, svc.Path, "no service path, so the public path reaches MCP unchanged")

	require.Len(t, svc.Routes, 1)
	route := svc.Routes[0]
	assert.Equal(t, "mcp", route.Name)
	assert.Equal(t, []string{"/mcp", testMCPMetadataPath}, route.Paths,
		"the MCP endpoint and, by prefix, its protected-resource metadata under /mcp too")
	assert.False(t, route.StripPath, "strip_path=false so MCP sees the path it is served under")
	assert.True(t, route.PreserveHost, "preserve_host so MCP builds its URLs from the public host")
	require.NotNil(t, route.ResponseBuffering, "response buffering must be switched off explicitly")
	assert.False(t, *route.ResponseBuffering, "MCP streams its responses, so the gateway must not buffer them")

	require.Len(t, route.Plugins, 1)
	assert.Equal(t, testRequestTransformer, route.Plugins[0].Name)
	assert.Equal(t, removedHeaders(testDefaultClientCertHeader), route.Plugins[0].Config,
		"the route strips the client-certificate header so it never reaches MCP")

	// Core still reads that header, so the removal is scoped to the MCP route: it is not a global
	// plugin, and no other route sets plugins or buffering.
	assert.Empty(t, cfg.Plugins, "the header removal must not be a global plugin")
	for _, other := range cfg.Services {
		if other.Name == testMCPName {
			continue
		}
		for _, r := range other.Routes {
			assert.Empty(t, r.Plugins, "route %s must keep the client-certificate header", r.Name)
			assert.Nil(t, r.ResponseBuffering, "route %s keeps the gateway's default buffering", r.Name)
		}
	}
}

// TestBuildGlobalConfigMapMCPRouteRemovesCustomClientCertHeader proves the route strips the header
// Core is configured to read: a platform that renames it must not leak the client certificate to
// MCP under the new name.
func TestBuildGlobalConfigMapMCPRouteRemovesCustomClientCertHeader(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Core.ClientCertHeader = testCustomClientCertHeader })

	svc, ok := serviceByName(parseKong(t, BuildGlobalConfigMap(p)), testMCPName)
	require.True(t, ok, "an enabled MCP server must add a gateway service")
	require.Len(t, svc.Routes, 1)
	require.Len(t, svc.Routes[0].Plugins, 1)
	assert.Equal(t, removedHeaders(testCustomClientCertHeader), svc.Routes[0].Plugins[0].Config,
		"the route strips the configured header, not the default name")
}

// TestBuildGlobalConfigMapMCPRouteFollowsMCPEnabled proves the route renders only when the MCP
// server does: asking for MCP on a version whose bundle carries no MCP image renders neither the
// workload nor a gateway route to it.
func TestBuildGlobalConfigMapMCPRouteFollowsMCPEnabled(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		want   bool
	}{
		{"mcp.enabled is false", func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Enabled = false }, false},
		{"the version bundle carries no MCP image", func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion220 }, false},
		{"the MCP server renders", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := serviceByName(parseKong(t, BuildGlobalConfigMap(mcpPlatform(tc.mutate))), testMCPName)
			assert.Equal(t, tc.want, ok)
		})
	}
}

// TestRenderedKongPluginsAreEnabledOnTheGateway proves every plugin the declarative config names,
// global or on a route, is in KONG_PLUGINS: Kong refuses a config that names a plugin it was not
// started with, so a plugin missing from that list would stop the gateway at boot.
func TestRenderedKongPluginsAreEnabledOnTheGateway(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Gateway.Cors.Enabled = true
		p.Spec.Gateway.Logging.Request = true
	})
	enabled := strings.Split(envMap(ResolveGateway(p))["KONG_PLUGINS"], ",")

	cfg := parseKong(t, BuildGlobalConfigMap(p))
	var named []string
	for _, plugin := range cfg.Plugins {
		named = append(named, plugin.Name)
	}
	for _, svc := range cfg.Services {
		for _, route := range svc.Routes {
			for _, plugin := range route.Plugins {
				named = append(named, plugin.Name)
			}
		}
	}

	require.Contains(t, named, testRequestTransformer, "the MCP route's plugin must be among those checked")
	for _, name := range named {
		assert.Contains(t, enabled, name, "KONG_PLUGINS must enable %s", name)
	}
}

// TestCoreEnvUsesClientCertHeaderName proves Core reads the client certificate from the header
// clientCertHeaderName names: spec.core.clientCertHeader, or the version bundle's default when that
// is empty. Kong strips the same header from the requests it routes to MCP, so both resolve it
// through that one function.
func TestCoreEnvUsesClientCertHeaderName(t *testing.T) {
	tests := []struct{ name, header, want string }{
		{"an unset header falls back to the bundle default", "", testDefaultClientCertHeader},
		{"a custom header replaces the default", testCustomClientCertHeader, testCustomClientCertHeader},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := basePlatform()
			p.Spec.Core.ClientCertHeader = tc.header
			w := wiringFor(p)

			assert.Equal(t, tc.want, clientCertHeaderName(p))
			got, ok := envValue(coreEnv(p, w), w.HeaderNameEnv)
			require.Truef(t, ok, "%s must be set", w.HeaderNameEnv)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildGlobalConfigMapCorsAndFileLog(t *testing.T) {
	p := basePlatform()
	p.Spec.Gateway.Cors.Enabled = true
	p.Spec.Gateway.Logging.Request = true
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	require.Len(t, cfg.Plugins, 2)
	byName := map[string]kongPlugin{}
	for _, pl := range cfg.Plugins {
		byName[pl.Name] = pl
	}

	cors, ok := byName["cors"]
	require.True(t, ok)
	assert.Equal(t, []interface{}{"*"}, cors.Config["origins"], "cors defaults to all origins")
	assert.Equal(t, []interface{}{"X-Auth-Token"}, cors.Config["exposed_headers"])
	assert.Equal(t, true, cors.Config["credentials"])

	fileLog, ok := byName["file-log"]
	require.True(t, ok)
	assert.Equal(t, "/dev/stdout", fileLog.Config["path"])
}

func TestBuildGlobalConfigMapCorsCustomOrigins(t *testing.T) {
	p := basePlatform()
	p.Spec.Gateway.Cors.Enabled = true
	p.Spec.Gateway.Cors.Origins = []string{"https://app.example.com"}
	p.Spec.Gateway.Cors.ExposedHeaders = []string{"X-Custom"}
	cfg := parseKong(t, BuildGlobalConfigMap(p))

	require.Len(t, cfg.Plugins, 1)
	assert.Equal(t, []interface{}{"https://app.example.com"}, cfg.Plugins[0].Config["origins"])
	assert.Equal(t, []interface{}{"X-Custom"}, cfg.Plugins[0].Config["exposed_headers"])
}

func TestBuildGlobalConfigMapNoSecretsInKongYAML(t *testing.T) {
	// The kong.yml carries only routing/plugin config — never credentials or
	// connection coordinates from referenced Secrets.
	cfg := BuildGlobalConfigMap(basePlatform())
	body := cfg.Data[gatewayConfigKey]
	for _, forbidden := range []string{"password", "username", "db-creds", "mq-creds", "5432", "db.example.com"} {
		assert.NotContains(t, body, forbidden, "kong.yml must not leak secret values or DB coordinates")
	}
}
