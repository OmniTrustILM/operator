/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// mcpServerComponent is the bundle component key AND the name of the MCP server's workload,
	// Service, ServiceAccount and main container.
	mcpServerComponent = "mcp-server"
	// mcpPort is the port the MCP server listens on (its PORT variable) and its Service exposes.
	mcpPort = 8080
	// mcpPublicPath is the path the MCP server is published under. Its resource URL, which the
	// server checks tokens against, is the platform host plus this path, so the gateway route that
	// serves it must use the same path.
	mcpPublicPath = "/mcp"
	// mcpClientID is the client id of MCP's client in the platform's own Keycloak, with the managed
	// identity. The server is configured with it, and it is the actor named in the delegated tokens
	// the server sends to Core, so Core's internal OAuth2 provider must allow it as a delegate.
	mcpClientID = "ilm-mcp"
	// mcpClientSecretSuffix names the operator-owned Secret holding the client secret of MCP's
	// Keycloak client: "<platform>-mcp-client".
	mcpClientSecretSuffix = "-mcp-client"
	// MCPClientSecretKey is the key inside that Secret holding the client secret value. It is exported
	// so the reconciler (which writes the Secret) and this builder (which references it from the MCP
	// Deployment) share ONE definition and never drift.
	MCPClientSecretKey = "clientSecret"
	// mcpClientSecretEnv is the env var the MCP server reads its client secret from. Sourced via
	// secretKeyRef — never inlined.
	mcpClientSecretEnv = "ILM_MCP_SECURITY_CLIENT_SECRET" //nolint:gosec // G101: an env var NAME, not a credential value
	// mcpDefaultAccess is the access level the CRD defaults spec.mcp.access to, for a Platform built
	// in Go: that skips apiserver defaulting, so the field may be empty.
	mcpDefaultAccess = "read-only"
	// mcpIdentityModeExternal is the spec.mcp.identity.mode of an identity provider the platform does
	// not manage. Any other mode, and an absent identity, means the managed one, which is also the
	// CRD's default: a Platform built in Go skips apiserver defaulting, so the mode may be empty.
	mcpIdentityModeExternal = "external"
	// mcpDefaultClientSecretKey is the key of the referenced Secret that holds an external client's
	// secret when spec.mcp.identity.clientSecretKey is empty. The CRD sets no default for it.
	mcpDefaultClientSecretKey = "client-secret"

	// mcpReadinessPath and mcpLivenessPath are the Spring Boot actuator health groups the probes
	// target; MANAGEMENT_ENDPOINT_HEALTH_PROBES_ENABLED switches both groups on.
	mcpReadinessPath = "/actuator/health/readiness"
	mcpLivenessPath  = "/actuator/health/liveness"
	// mcpStartupBudgetSeconds is how long the startup probe keeps probing before the kubelet
	// restarts the container. The liveness and readiness probes only begin once the startup probe
	// has succeeded, so a slow JVM start is never cut short by them.
	mcpStartupBudgetSeconds = 300
)

// MCPEnabled reports whether the platform renders the MCP server: the CR asks for it AND the
// selected version bundle carries the MCP server image.
//
// The bundle gate makes the same CR portable across versions — the image is a 2.21.0 component,
// so a platform pinned to an older version renders no MCP objects instead of referencing an image
// that release never had (the same version-gating TimeQualityMonitorEnabled applies to the
// time-quality-monitor image).
func MCPEnabled(p *otilmv1alpha1.Platform) bool {
	if !p.Spec.MCP.Enabled {
		return false
	}
	_, ok := resolveBundle(p).Lookup(mcpServerComponent)
	return ok
}

// MCPCIMDEnabled reports whether sign-in with OAuth Client ID Metadata Documents is on: an unset
// spec.mcp.cimd.enabled counts as on, and only an explicit false turns it off. It reads that
// setting alone, so callers gate on MCPEnabled first.
func MCPCIMDEnabled(p *otilmv1alpha1.Platform) bool {
	cimd := p.Spec.MCP.CIMD.Enabled
	return cimd == nil || *cimd
}

// MCPClientSecretName returns the operator-owned Secret name holding the client secret of MCP's
// Keycloak client with the managed identity: "<platform>-mcp-client". The MCP Deployment reads it by
// secretKeyRef (key MCPClientSecretKey); with the external identity it reads the Secret
// MCPClientSecret names instead, and nothing creates this one. Exported so the controller and the
// builder share the name.
func MCPClientSecretName(p *otilmv1alpha1.Platform) string {
	return p.Name + mcpClientSecretSuffix
}

// externalIdentityMode reports whether spec.mcp.identity selects an identity provider the platform does
// not manage. It reads that setting alone, so callers gate on MCPEnabled first.
func externalIdentityMode(p *otilmv1alpha1.Platform) bool {
	identity := p.Spec.MCP.Identity
	return identity != nil && identity.Mode == mcpIdentityModeExternal
}

// MCPExternalIdentity reports whether the platform renders the MCP server AND signs its users in
// through an identity provider the platform does not manage (spec.mcp.identity.mode external). The
// operator then prepares nothing in Keycloak for MCP and registers no MCP delegate in ILM.
func MCPExternalIdentity(p *otilmv1alpha1.Platform) bool {
	return MCPEnabled(p) && externalIdentityMode(p)
}

// MCPManagedIdentity reports whether the platform renders the MCP server AND signs its users in
// through its own Keycloak, which the operator prepares for MCP: spec.mcp.identity is absent, or its
// mode is managed or empty.
func MCPManagedIdentity(p *otilmv1alpha1.Platform) bool {
	return MCPEnabled(p) && !externalIdentityMode(p)
}

// MCPClientSecret returns the Secret the MCP server reads its client secret from, and the key that
// holds it. With the managed identity it is the operator-owned Secret (MCPClientSecretName and
// MCPClientSecretKey). With the external identity it is the Secret spec.mcp.identity.clientSecretRef
// names, under spec.mcp.identity.clientSecretKey or "client-secret" when that is empty. It reads the
// identity mode alone, so callers gate on MCPEnabled first.
func MCPClientSecret(p *otilmv1alpha1.Platform) (name, key string) {
	if !externalIdentityMode(p) {
		return MCPClientSecretName(p), MCPClientSecretKey
	}
	identity := p.Spec.MCP.Identity
	key = identity.ClientSecretKey
	if key == "" {
		key = mcpDefaultClientSecretKey
	}
	return identity.ClientSecretRef, key
}

// ResolveMCP resolves the MCP server component's render model: a stateless Spring Boot service
// with no database or messaging dependency. It is only included when MCPEnabled.
func ResolveMCP(p *otilmv1alpha1.Platform) common.Component {
	b := resolveBundle(p)
	w := b.Wiring
	image, policy := common.ResolveImage(b.Lookup, mcpServerComponent, p.Spec.Common.Image, p.Spec.MCP.Image)
	secretName, secretKey := MCPClientSecret(p)

	c := common.Component{
		Name: mcpServerComponent, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.MCP.Image.PullSecrets),
		Replicas: 1, Port: mcpPort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.MCP.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.MCP.Image),
		Env:     mcpEnv(p, w),
		// The client secret is a reference to a Secret, never an inlined value, and a required one: the
		// pod waits for it instead of starting without the credential it exchanges tokens with. With the
		// managed identity that Secret is the one the reconciler relays Keycloak's secret into, and with
		// the external identity the one the spec names. The trusted certificates follow it as the
		// optional reference Core has.
		SecretEnv: append([]common.SecretEnvRef{
			{EnvVar: mcpClientSecretEnv, SecretName: secretName, SecretKey: secretKey},
		}, trustedCertsSecretEnv(p, w)...),
		Volumes:      []corev1.Volume{ephemeralVolume()},
		VolumeMounts: []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: tmpMountPath}},
		Probes:       mcpProbes(),
		Resources:    mcpDefaultResources(),
		// Read-only root filesystem: ENABLED. A JVM service whose only writable path is the
		// in-memory /tmp ephemeral volume.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}
	applyComponentSpec(p, &c, p.Spec.MCP.ComponentSpec)
	return c
}

// mcpEnv returns the MCP server's inline environment: the Spring runtime switches, the Core API it
// calls, the access level it offers, the security contract for acting on behalf of the signed-in user,
// and the outbound proxy settings it shares with Core. The client secret and the trusted certificates
// are left out, because they are Secret references (see ResolveMCP).
func mcpEnv(p *otilmv1alpha1.Platform, w bom.WiringProfile) []common.EnvPair {
	access := p.Spec.MCP.Access
	if access == "" {
		access = mcpDefaultAccess
	}
	idp := mcpIdentity(p)

	env := []common.EnvPair{
		{Name: "SPRING_PROFILES_ACTIVE", Value: "http"},
		{Name: "SPRING_AI_MCP_SERVER_TYPE", Value: "SYNC"},
		{Name: "SPRING_AI_MCP_SERVER_PROTOCOL", Value: "STATELESS"},
		{Name: "PORT", Value: strconv.Itoa(mcpPort)},
		{Name: "MANAGEMENT_ENDPOINT_HEALTH_PROBES_ENABLED", Value: "true"},
		{Name: "ILM_AUTH_METHOD", Value: "oauth-delegation"},
		{Name: "ILM_API_URL", Value: fmt.Sprintf("http://%s:%d%s", gatewayCoreHost, gatewayUpstreamPort, gatewayCoreAPIPath)},
		{Name: "ILM_MCP_ACCESS", Value: access},
		{Name: "ILM_MCP_TOOLSETS", Value: "all"},
		{Name: "ILM_MCP_SECURITY_ENABLED", Value: "true"},
		{Name: "ILM_MCP_SECURITY_ISSUER", Value: idp.Issuer},
		{Name: "ILM_MCP_SECURITY_RESOURCE", Value: "https://" + PlatformHost(p) + mcpPublicPath},
		{Name: "ILM_MCP_SECURITY_JWK_SET_URI", Value: idp.JWKSetURI},
		{Name: "ILM_MCP_SECURITY_TOKEN_URI", Value: idp.TokenURI},
		// The server accepts plain HTTP for the Core API, the key set and the token endpoint only with
		// this switch. The Core API is plain in-cluster HTTP, as are the endpoints of the platform's own
		// Keycloak; those of an external provider may be HTTPS.
		{Name: "ILM_MCP_SECURITY_ALLOW_HTTP_BACKCHANNEL", Value: "true"},
		{Name: "ILM_MCP_SECURITY_CLIENT_ID", Value: idp.ClientID},
	}
	// With an external identity, the audience and the scopes are optional: the server has its own
	// default for each, and an empty value would override it, so an unset setting renders nothing.
	env = appendIfSet(env, "ILM_MCP_SECURITY_API_AUDIENCE", idp.APIAudience)
	env = append(env, common.EnvPair{Name: "ILM_MCP_SECURITY_EXCHANGE_MODE", Value: idp.ExchangeMode})
	env = appendIfSet(env, "ILM_MCP_SECURITY_EXCHANGE_SCOPE", idp.ExchangeScope)
	env = appendIfSet(env, "ILM_MCP_SECURITY_AUTHORIZATION_SCOPES", strings.Join(idp.AuthorizationScopes, ","))
	env = appendIfSet(env, "ILM_MCP_SECURITY_REQUIRED_SCOPE", idp.RequiredScope)
	env = append(env,
		// Empty: no browser origin is allowed to call the server.
		common.EnvPair{Name: "ILM_MCP_SECURITY_ALLOWED_ORIGINS", Value: ""},
		common.EnvPair{Name: "ILM_MCP_SECURITY_JWS_ALGORITHMS", Value: "RS256"},
	)
	// The Core API is always plain http to Core's Service, so that host bypasses the proxy; the key set
	// and the token endpoint add theirs when they are plain http as well.
	noProxyHosts := append([]string{gatewayCoreHost}, plainHTTPHosts(idp.JWKSetURI, idp.TokenURI)...)
	return append(env, outboundProxyEnv(p, w, noProxyHosts...)...)
}

// plainHTTPHosts returns the host of each of the URLs that is an http URL, without its port. An https
// URL goes through a proxy's TLS tunnel, so its host is not one to bypass the proxy for.
func plainHTTPHosts(urls ...string) []string {
	var hosts []string
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && u.Scheme == "http" && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

func appendIfSet(env []common.EnvPair, name, value string) []common.EnvPair {
	if value == "" {
		return env
	}
	return append(env, common.EnvPair{Name: name, Value: value})
}

// mcpIdentity returns the identity provider settings the MCP server is configured with: those of
// spec.mcp.identity with the external identity, and those of the platform's own Keycloak with the
// managed one, in the same shape.
func mcpIdentity(p *otilmv1alpha1.Platform) otilmv1alpha1.MCPIdentitySpec {
	if externalIdentityMode(p) {
		return *p.Spec.MCP.Identity
	}
	return managedMCPIdentity(p)
}

// managedMCPIdentity returns the settings of the platform's own Keycloak, which the operator prepares
// for MCP, in the shape of the external ones.
func managedMCPIdentity(p *otilmv1alpha1.Platform) otilmv1alpha1.MCPIdentitySpec {
	realm := KeycloakRealmName(p)
	// The issuer is the browser-facing realm URL, reached through the edge: the one Keycloak puts in
	// the tokens it signs. The key and token endpoints are the managed Keycloak's in-cluster Service,
	// so the server verifies and exchanges tokens without leaving the cluster.
	openIDConnect := fmt.Sprintf("http://%s:%d%s/realms/%s/protocol/openid-connect",
		ManagedKeycloakServiceName(p), ManagedKeycloakServicePort, KeycloakRelativePath, realm)
	return otilmv1alpha1.MCPIdentitySpec{
		Issuer:      fmt.Sprintf("https://%s%s/realms/%s", PlatformHost(p), KeycloakRelativePath, realm),
		JWKSetURI:   openIDConnect + "/certs",
		TokenURI:    openIDConnect + "/token",
		ClientID:    mcpClientID,
		APIAudience: "ilm",
		// Delegation: the server exchanges the signed-in user's token for one that names it as the
		// actor. The exchange asks for the scopes that select the user and audience mappings ILM needs;
		// the login scopes the server advertises add delegation:client:ilm-mcp, which a client
		// requests at sign-in so the user can consent to the delegation.
		ExchangeMode:        "delegation",
		ExchangeScope:       "openid ilm-mcp-upstream",
		AuthorizationScopes: []string{"openid", "delegation:client:ilm-mcp"},
		RequiredScope:       "mcp:access",
	}
}

// mcpProbes returns the MCP server's readiness, liveness and startup probes, all HTTP GETs against
// the actuator health groups on the container port. They share httpProbesOn's fleet-wide timing.
// The startup probe waits on the liveness group, starting at once, for exactly
// mcpStartupBudgetSeconds; readiness and liveness have no initial delay because they only begin
// after startup succeeds.
func mcpProbes() common.Probes {
	probes := httpProbesOn(mcpReadinessPath, 0, mcpLivenessPath, mcpPort)
	probes.Startup.InitialDelaySeconds = 0
	probes.Startup.FailureThreshold = mcpStartupBudgetSeconds / probes.Startup.PeriodSeconds
	liveness := probes.Readiness.DeepCopy()
	liveness.HTTPGet.Path = mcpLivenessPath
	probes.Liveness = liveness
	return probes
}

// mcpDefaultResources returns the resource block the MCP server runs with until spec.mcp.resources
// replaces it. The requests are declared so that a CPU or memory utilization target on
// spec.mcp.autoscaling has a base to measure against: utilization is a percentage of the request.
func mcpDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("768Mi"),
		},
	}
}
