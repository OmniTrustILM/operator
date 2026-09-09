/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kong gateway constants. The gateway runs DB-less: it reads its declarative
// config from a mounted ConfigMap at boot.
const (
	// gatewayName is the gateway component role (Deployment/Service/SA name and the
	// component label).
	gatewayName = "api-gateway"
	// gatewayConsumerPort is the data-plane (proxy) port external traffic hits.
	gatewayConsumerPort = 8000
	// gatewayAdminPort is the Admin API port.
	gatewayAdminPort = 8001
	// gatewayStatusPort is the status/health-listener port.
	gatewayStatusPort = 8100
	// gatewayConsumerPortName / gatewayAdminPortName name the container ports.
	gatewayConsumerPortName = "consumer-http"
	gatewayAdminPortName    = "admin-http"

	// gatewayConfigVolume is the pod volume projecting the kong.yml ConfigMap.
	gatewayConfigVolume = "api-gateway-config-volume"
	// gatewayDeclarativeMount is where the declarative config is mounted.
	gatewayDeclarativeMount = "/kong/declarative"
	// gatewayDeclarativeConfig is the absolute path Kong reads its config from.
	gatewayDeclarativeConfig = "/kong/declarative/kong.yml"

	// globalConfigMapName is the shared ConfigMap holding kong.yml (component role "global").
	globalConfigMapName = "global-configmap"
	// gatewayConfigKey is the key under the ConfigMap data holding the declarative config.
	gatewayConfigKey = "kong.yml"

	// gatewayDevStdout is the path Kong writes its access logs to (stdout), so logs
	// surface via the pod's stdout stream.
	gatewayDevStdout = "/dev/stdout"

	// gatewayLogLevel is Kong's log level.
	gatewayLogLevel = "info"
	// gatewayPlugins is the comma-separated bundle of Kong plugins enabled on the
	// data plane (KONG_PLUGINS).
	gatewayPlugins = "request-transformer,cors,file-log,response-transformer,post-function"
)

// ResolveGateway resolves the Kong API gateway component's render model. The
// gateway runs in DB-less mode (KONG_DATABASE=off): its routing and plugins come
// entirely from the kong.yml declarative config mounted from the global ConfigMap
// (see BuildGlobalConfigMap).
//
// The container security context is the operator's default SCC-clean profile
// (non-root, drop ALL caps, seccomp RuntimeDefault, no privilege escalation),
// applied by common.BuildDeployment — Kong runs unprivileged on its ports.
//
// Read-only root filesystem: ENABLED. Kong writes only under its prefix, which is
// pinned to /tmp (KONG_PREFIX=/tmp/) and backed by the in-memory ephemeral volume,
// so a read-only root is safe.
func ResolveGateway(p *otilmv1alpha1.Platform) common.Component {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, gatewayName, p.Spec.Common.Image, p.Spec.Gateway.Image)

	c := common.Component{
		Name: gatewayName, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.Gateway.Image.PullSecrets),
		Replicas: 1, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.Gateway.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.Gateway.Image),
		// Primary container port is the consumer (proxy) port; the admin port is added
		// as an additional named port (consumer-http / admin-http).
		Port:            gatewayConsumerPort,
		PrimaryPortName: gatewayConsumerPortName,
		AdditionalPorts: []corev1.ContainerPort{
			{Name: gatewayAdminPortName, ContainerPort: gatewayAdminPort},
		},
		// The Service exposes admin/consumer/status.
		ServicePorts: []corev1.ServicePort{
			{Name: "admin", Port: gatewayAdminPort, Protocol: corev1.ProtocolTCP},
			{Name: "consumer", Port: gatewayConsumerPort, Protocol: corev1.ProtocolTCP},
			{Name: "status", Port: gatewayStatusPort, Protocol: corev1.ProtocolTCP},
		},
		Env:                    gatewayEnv(p),
		Probes:                 gatewayProbes(),
		Volumes:                []corev1.Volume{gatewayConfigVolumeSource(), gatewayEphemeralVolume()},
		VolumeMounts:           gatewayVolumeMounts(),
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.Gateway.ComponentSpec)
	return c
}

// gatewayEnv composes Kong's DB-less environment. KONG_TRUSTED_IPS is only set when
// trustedIps is configured. No secret values are referenced here.
func gatewayEnv(p *otilmv1alpha1.Platform) []common.EnvPair {
	env := []common.EnvPair{
		{Name: "KONG_DATABASE", Value: "off"},
		{Name: "KONG_PREFIX", Value: "/tmp/"},
		{Name: "KONG_PROXY_ACCESS_LOG", Value: gatewayDevStdout},
		{Name: "KONG_ADMIN_ACCESS_LOG", Value: gatewayDevStdout},
		{Name: "KONG_PROXY_ERROR_LOG", Value: "/dev/stderr"},
		{Name: "KONG_ADMIN_ERROR_LOG", Value: "/dev/stderr"},
		{Name: "KONG_ADMIN_LISTEN", Value: "0.0.0.0:8001, 0.0.0.0:8444 ssl"},
		{Name: "KONG_STATUS_LISTEN", Value: "0.0.0.0:8100"},
		{Name: "KONG_DECLARATIVE_CONFIG", Value: gatewayDeclarativeConfig},
		{Name: "KONG_PLUGINS", Value: gatewayPlugins},
		{Name: "KONG_LOG_LEVEL", Value: gatewayLogLevel},
	}
	if ips := strings.Join(p.Spec.Gateway.TrustedIPs, ","); ips != "" {
		env = append(env, common.EnvPair{Name: "KONG_TRUSTED_IPS", Value: ips})
	}
	return env
}

// gatewayProbes returns the gateway's readiness and startup probes. Both run
// `kong health` via exec (liveness is intentionally omitted). Startup tolerates a
// long boot (failureThreshold 45).
func gatewayProbes() common.Probes {
	kongHealth := func(initialDelay, failureThreshold int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"kong", "health"}},
			},
			InitialDelaySeconds: initialDelay,
			TimeoutSeconds:      5,
			PeriodSeconds:       10,
			SuccessThreshold:    1,
			FailureThreshold:    failureThreshold,
		}
	}
	return common.Probes{
		Readiness: kongHealth(5, 3),
		Startup:   kongHealth(15, 45),
	}
}

// gatewayVolumeMounts returns the gateway container's volume mounts: the declarative
// config (read at boot) and a /tmp scratch volume (KONG_PREFIX).
func gatewayVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: gatewayConfigVolume, MountPath: gatewayDeclarativeMount},
		{Name: ephemeralVolumeName, MountPath: tmpMountPath},
	}
}

// gatewayConfigVolumeSource projects the global ConfigMap's kong.yml as a single
// named item under /kong/declarative.
func gatewayConfigVolumeSource() corev1.Volume {
	return corev1.Volume{
		Name: gatewayConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: globalConfigMapName},
				Items:                []corev1.KeyToPath{{Key: gatewayConfigKey, Path: gatewayConfigKey}},
			},
		},
	}
}

// gatewayEphemeralVolume returns Kong's /tmp scratch volume: an in-memory emptyDir
// sized 10Mi (Kong writes its prefix and runtime files here so the root filesystem
// stays read-only).
func gatewayEphemeralVolume() corev1.Volume {
	v := ephemeralVolume()
	tenMi := resource.MustParse("10Mi")
	v.EmptyDir.SizeLimit = &tenMi
	return v
}

// BuildGlobalConfigMap renders the shared global ConfigMap holding the Kong
// declarative config (kong.yml). The rendered config routes to the operator's clean
// intra-platform Service names (core, fe-administrator, utils). Routes and
// plugins are gated by the Platform spec; the kong.yml carries no secret values.
func BuildGlobalConfigMap(p *otilmv1alpha1.Platform) *corev1.ConfigMap {
	labels := map[string]string{
		common.NameLabel:      gatewayConfigMapRole,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: gatewayConfigMapRole,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      globalConfigMapName,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{gatewayConfigKey: renderKongYAML(p)},
	}
}

// gatewayConfigMapRole is the component-label value carried by the global ConfigMap.
const gatewayConfigMapRole = "global"

// Declarative-config defaults: the upstream HTTP port shared by the internal
// services and the route paths.
const (
	gatewayUpstreamPort = 8080
	gatewayCoreAPIPath  = "/api"
	gatewayFeBasePath   = "/administrator"
	gatewayKcPath       = "/kc"
	gatewayMqPath       = "/mq"
	gatewayUtilsPath    = "/utils"

	// Internal upstream Service names. These are the operator's clean, unsuffixed
	// names; the gateway routes to these.
	gatewayCoreHost  = "core"
	gatewayFeHost    = feAdministratorName
	gatewayUtilsHost = "utils"
	// gatewayMessagingMgmtPort is the RabbitMQ management plugin's listener port the /mq
	// route targets. The management HOST is NOT hardcoded — it is resolved per-mode from
	// the messaging connection (see renderKongYAML): managed → the RabbitmqCluster broker
	// Service ("<name>-messaging"), external → spec.messaging.host. Only the port is a fixed
	// default here (RabbitMQ's well-known management port).
	gatewayMessagingMgmtPort = 15672
)

// kongConfig is the top-level DB-less declarative document Kong reads. Field order
// is preserved by the YAML marshaller.
type kongConfig struct {
	FormatVersion string        `yaml:"_format_version"`
	Transform     bool          `yaml:"_transform"`
	Services      []kongService `yaml:"services"`
	Plugins       []kongPlugin  `yaml:"plugins,omitempty"`
}

// kongService is one upstream service and its routes.
type kongService struct {
	Name     string      `yaml:"name"`
	Host     string      `yaml:"host"`
	Port     int         `yaml:"port"`
	Protocol string      `yaml:"protocol,omitempty"`
	Path     string      `yaml:"path,omitempty"`
	Routes   []kongRoute `yaml:"routes"`
}

// kongRoute is a route attached to a service.
type kongRoute struct {
	Name         string   `yaml:"name"`
	StripPath    bool     `yaml:"strip_path"`
	PreserveHost bool     `yaml:"preserve_host"`
	Paths        []string `yaml:"paths"`
}

// kongPlugin is a global plugin (cors, file-log).
type kongPlugin struct {
	Name   string                 `yaml:"name"`
	Config map[string]interface{} `yaml:"config"`
}

// renderKongYAML builds the Kong declarative config (kong.yml) for the Platform.
// It always emits the core, core-login-logout, and fe-administrator services and
// routes; the /utils, /mq, and cors/file-log entries are gated by the CR. The intra-
// platform upstream hosts are the operator's clean Service names; the /mq upstream host
// is resolved per-mode from the messaging connection (see gatewayMessagingMgmtHost). The
// /mq route is additionally gated on brokerType==rabbitmq (Service Bus has no management
// UI). The /kc route proxies the MANAGED Keycloak (its OIDC endpoints + admin console) to the
// in-cluster Keycloak Service, so the managed Keycloak is reachable through the edge at
// https://<host>/kc/... — Keycloak serves under that prefix (KC_HTTP_RELATIVE_PATH=/kc) and
// every OIDC URL carries it. Only for a MANAGED Keycloak (external Keycloak uses its own URL).
func renderKongYAML(p *otilmv1alpha1.Platform) string {
	login := p.Spec.FeAdministrator.URL.Login
	if login == "" {
		login = defaultFeURLLogin
	}
	logout := p.Spec.FeAdministrator.URL.Logout
	if logout == "" {
		logout = defaultFeURLLogout
	}
	feBase := p.Spec.FeAdministrator.URL.Base
	if feBase == "" {
		feBase = gatewayFeBasePath
	}

	cfg := kongConfig{
		FormatVersion: "2.1",
		Transform:     true,
		Services: []kongService{
			{
				Name: "core", Host: gatewayCoreHost, Port: gatewayUpstreamPort, Protocol: "http",
				Routes: []kongRoute{{
					Name: "protocols_route", StripPath: false, PreserveHost: true,
					Paths: []string{gatewayCoreAPIPath},
				}},
			},
			{
				Name: "core-login-logout", Host: gatewayCoreHost, Port: gatewayUpstreamPort, Protocol: "http",
				Path: gatewayCoreAPIPath,
				Routes: []kongRoute{{
					Name: "core-oauth2_route", StripPath: false, PreserveHost: true,
					Paths: []string{login, logout},
				}},
			},
			{
				Name: "fe-administrator", Host: gatewayFeHost, Port: gatewayUpstreamPort, Protocol: "http",
				Routes: []kongRoute{{
					Name: "fe-administrator_route-cert", StripPath: true, PreserveHost: true,
					Paths: []string{feBase},
				}},
			},
		},
	}

	// /kc — the MANAGED Keycloak. Keycloak serves under KC_HTTP_RELATIVE_PATH=/kc, the OIDC
	// issuer/auth/logout (browser) and token/jwks (back-channel) URLs all carry /kc, and the
	// browser MUST reach Keycloak for the OIDC login redirect — so the gateway proxies /kc to the
	// in-cluster Keycloak Service (preserve_host so Keycloak builds correct URLs; strip_path=false
	// so the /kc prefix reaches Keycloak). Only for a MANAGED Keycloak: an external Keycloak is
	// reached at its own URL, and Core's in-pod OIDC registration is not rendered either.
	if KeycloakManaged(p) {
		cfg.Services = append(cfg.Services, kongService{
			Name: "keycloak", Host: ManagedKeycloakServiceName(p), Port: ManagedKeycloakServicePort, Protocol: "http",
			Routes: []kongRoute{{
				Name: "keycloak_route", StripPath: false, PreserveHost: true,
				Paths: []string{KeycloakRelativePath},
			}},
		})
	}

	// /mq — RabbitMQ management UI. Gated on messaging.management.expose AND
	// messaging.mode=managed: the route only makes sense when the operator MANAGES the
	// broker (and therefore owns/knows its in-cluster management Service). For an EXTERNAL
	// broker the operator does not own the broker and must not assume or proxy its
	// management endpoint — so /mq is never rendered for external messaging. Managed
	// messaging is always RabbitMQ (the operator provisions a RabbitmqCluster), so this
	// implies brokerType=rabbitmq; Azure Service Bus is external-only and has no management
	// UI. Upstream host = the managed broker Service ("<name>-messaging") on the management
	// port (15672), paired with the broker's management.path_prefix=/mq (managed_messaging.go,
	// same expose gate) so RabbitMQ serves the UI/HTTP-API under /mq with /mq-prefixed assets.
	//
	// TWO plain-prefix routes make the UI load with OR without the trailing slash, plugin-free,
	// leaning on Kong's longest-prefix route priority (no regex — Kong rejects a non-"~/"-prefixed
	// regex path):
	//   - "rabbitmq" — path "/mq/": the UI + HTTP API under /mq/ (and deeper), forwarded UNSTRIPPED
	//     so RabbitMQ sees its own /mq prefix and its relative asset/API refs resolve under /mq/.
	//     For "/mq/…" this is the LONGER (more specific) prefix, so Kong prefers it over "/mq".
	//   - "rabbitmq-slash-redirect" — path "/mq": only the BARE /mq (no trailing slash) can match it
	//     ("/mq/…" is taken by the longer "/mq/" route), and it STRIPS the prefix to "/" so
	//     RabbitMQ's OWN "/" -> "/mq/" redirect (a path_prefix behavior — verified: GET / => 301
	//     Location /mq/) fires, sending the browser to /mq/ where the assets resolve. Without it,
	//     bare /mq serves the index whose relative assets resolve to ROOT and 404 — RabbitMQ does
	//     NOT redirect "/mq" -> "/mq/" itself (only the bare "/"), so this strip-to-root route is
	//     what closes the no-trailing-slash gap. No loop: the redirect target /mq/ hits the other route.
	// preserve_host keeps the public Host so RabbitMQ's redirect targets the edge host.
	if p.Spec.Messaging.Management.Expose && MessagingManaged(p) {
		cfg.Services = append(cfg.Services, kongService{
			Name: "messaging-service", Host: gatewayMessagingMgmtHost(p), Port: gatewayMessagingMgmtPort,
			Routes: []kongRoute{
				{
					Name: "rabbitmq", StripPath: false, PreserveHost: true,
					Paths: []string{gatewayMqPath + "/"},
				},
				{
					Name: "rabbitmq-slash-redirect", StripPath: true, PreserveHost: true,
					Paths: []string{gatewayMqPath},
				},
			},
		})
	}

	// /utils — utility services, gated by utils.enabled (same gate as the workload).
	if p.Spec.Utils.Enabled {
		cfg.Services = append(cfg.Services, kongService{
			Name: "utils", Host: gatewayUtilsHost, Port: gatewayUpstreamPort,
			Routes: []kongRoute{{
				Name: "v1-utils-route", StripPath: true, PreserveHost: true,
				Paths: []string{gatewayUtilsPath},
			}},
		})
	}

	cfg.Plugins = gatewayPluginList(p)

	out, err := yaml.Marshal(cfg)
	if err != nil {
		// kongConfig is a fixed, marshalable shape; an error here is a programmer bug.
		panic("platform: marshal kong.yml: " + err.Error())
	}
	return string(out)
}

// gatewayMessagingMgmtHost resolves the RabbitMQ management UI host the /mq route targets.
// The route is only rendered for managed messaging (see the gate above), so this is always
// the operator-provisioned RabbitmqCluster broker Service ("<name>-messaging"); the
// management plugin listens on the same broker host as AMQP (only the port differs: 15672
// vs 5672), so the connection's Host is correct.
func gatewayMessagingMgmtHost(p *otilmv1alpha1.Platform) string {
	return ResolveMessagingConnection(p).Host
}

// gatewayPluginList returns the global Kong plugins gated by the CR: the cors plugin
// (gateway.cors.enabled) and the file-log plugin (gateway.logging.request). Returns
// nil when neither is enabled so the "plugins" key is omitted.
func gatewayPluginList(p *otilmv1alpha1.Platform) []kongPlugin {
	var plugins []kongPlugin
	if p.Spec.Gateway.Cors.Enabled {
		origins := p.Spec.Gateway.Cors.Origins
		if len(origins) == 0 {
			// Default the allowed origin to the platform's public FQDN
			// (https://<PlatformHost>) when one is known, so the out-of-the-box CORS posture is
			// the platform's own origin rather than wide-open. With no host (dev/test) fall back
			// to the wildcard "*".
			if h := PlatformHost(p); h != "" {
				origins = []string{"https://" + h}
			} else {
				origins = []string{"*"}
			}
		}
		exposed := p.Spec.Gateway.Cors.ExposedHeaders
		if len(exposed) == 0 {
			exposed = []string{"X-Auth-Token"}
		}
		plugins = append(plugins, kongPlugin{
			Name: "cors",
			Config: map[string]interface{}{
				"origins":            origins,
				"credentials":        true,
				"max_age":            3600,
				"exposed_headers":    exposed,
				"preflight_continue": false,
			},
		})
	}
	if p.Spec.Gateway.Logging.Request {
		plugins = append(plugins, kongPlugin{
			Name:   "file-log",
			Config: map[string]interface{}{"path": gatewayDevStdout},
		})
	}
	return plugins
}
