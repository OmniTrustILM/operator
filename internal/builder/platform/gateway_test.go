/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
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
