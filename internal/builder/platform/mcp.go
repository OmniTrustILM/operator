/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"fmt"
	"strconv"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
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
	// mcpClientID is the client id of MCP's Keycloak client. The server is configured with it, and
	// it is the actor named in the delegated tokens the server sends to Core, so Core's internal
	// OAuth2 provider must allow it as a delegate.
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
// Keycloak client: "<platform>-mcp-client". The MCP Deployment reads it by secretKeyRef (key
// MCPClientSecretKey). Exported so the controller and the builder share the name.
func MCPClientSecretName(p *otilmv1alpha1.Platform) string {
	return p.Name + mcpClientSecretSuffix
}

// ResolveMCP resolves the MCP server component's render model: a stateless Spring Boot service
// with no database or messaging dependency. It is only included when MCPEnabled.
func ResolveMCP(p *otilmv1alpha1.Platform) common.Component {
	image, policy := common.ResolveImage(resolveBundle(p).Lookup, mcpServerComponent, p.Spec.Common.Image, p.Spec.MCP.Image)

	c := common.Component{
		Name: mcpServerComponent, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, p.Spec.MCP.Image.PullSecrets),
		Replicas: 1, Port: mcpPort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, p.Spec.MCP.Image),
		Args:    imageArgs(p.Spec.Common.Image, p.Spec.MCP.Image),
		Env:     mcpEnv(p),
		// The client secret is a reference to the operator-owned Secret and a required one: the pod
		// waits for it instead of starting without the credential it exchanges tokens with.
		SecretEnv: []common.SecretEnvRef{
			{EnvVar: mcpClientSecretEnv, SecretName: MCPClientSecretName(p), SecretKey: MCPClientSecretKey},
		},
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
// calls, the access level it offers, and the security contract for acting on behalf of the
// signed-in user. The client secret is the one variable left out, because it is a Secret reference
// (see ResolveMCP).
func mcpEnv(p *otilmv1alpha1.Platform) []common.EnvPair {
	access := p.Spec.MCP.Access
	if access == "" {
		access = mcpDefaultAccess
	}
	host := PlatformHost(p)
	realm := KeycloakRealmName(p)
	// The issuer is the browser-facing realm URL, reached through the edge: the one Keycloak puts in
	// the tokens it signs. The key and token endpoints are the managed Keycloak's in-cluster Service,
	// so the server verifies and exchanges tokens without leaving the cluster.
	issuer := fmt.Sprintf("https://%s%s/realms/%s", host, KeycloakRelativePath, realm)
	openIDConnect := fmt.Sprintf("http://%s:%d%s/realms/%s/protocol/openid-connect",
		ManagedKeycloakServiceName(p), ManagedKeycloakServicePort, KeycloakRelativePath, realm)

	return []common.EnvPair{
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
		{Name: "ILM_MCP_SECURITY_ISSUER", Value: issuer},
		{Name: "ILM_MCP_SECURITY_RESOURCE", Value: "https://" + host + mcpPublicPath},
		{Name: "ILM_MCP_SECURITY_JWK_SET_URI", Value: openIDConnect + "/certs"},
		{Name: "ILM_MCP_SECURITY_TOKEN_URI", Value: openIDConnect + "/token"},
		// The Core API, the key set and the token endpoint above are plain in-cluster HTTP, which the
		// server accepts only with this switch.
		{Name: "ILM_MCP_SECURITY_ALLOW_HTTP_BACKCHANNEL", Value: "true"},
		{Name: "ILM_MCP_SECURITY_CLIENT_ID", Value: mcpClientID},
		{Name: "ILM_MCP_SECURITY_API_AUDIENCE", Value: "ilm"},
		// Delegation: the server exchanges the signed-in user's token for one that names it as the
		// actor. The exchange asks for the scopes that select the user and audience mappings ILM needs;
		// the login scopes the server advertises add delegation:client:ilm-mcp, which a client
		// requests at sign-in so the user can consent to the delegation.
		{Name: "ILM_MCP_SECURITY_EXCHANGE_MODE", Value: "delegation"},
		{Name: "ILM_MCP_SECURITY_EXCHANGE_SCOPE", Value: "openid ilm-mcp-upstream"},
		{Name: "ILM_MCP_SECURITY_AUTHORIZATION_SCOPES", Value: "openid,delegation:client:ilm-mcp"},
		{Name: "ILM_MCP_SECURITY_REQUIRED_SCOPE", Value: "mcp:access"},
		// Empty: no browser origin is allowed to call the server.
		{Name: "ILM_MCP_SECURITY_ALLOWED_ORIGINS", Value: ""},
		{Name: "ILM_MCP_SECURITY_JWS_ALGORITHMS", Value: "RS256"},
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
