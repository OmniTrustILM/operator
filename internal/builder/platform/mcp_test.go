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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// mcpPlatform returns a Platform on the 2.21.0 bundle that asks for the MCP server the way the
// CRD's validation requires: MCP enabled, on a managed Keycloak, behind a public host. mutate,
// when set, runs last.
func mcpPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.Version = testVersion221
	p.Spec.Common.HostName = testEdgeHost
	p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{
		Mode: "managed", Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1},
	}
	p.Spec.MCP.Enabled = true
	if mutate != nil {
		mutate(p)
	}
	return p
}

// TestResolveMCPRendersTheDelegationContract pins every environment variable the MCP server gets,
// by name and value. Together they are the contract between the MCP image, Keycloak and Core for
// MCP acting on behalf of the signed-in user.
func TestResolveMCPRendersTheDelegationContract(t *testing.T) {
	c := ResolveMCP(mcpPlatform(nil))

	tests := []struct{ name, want string }{
		{"SPRING_PROFILES_ACTIVE", "http"},
		{"SPRING_AI_MCP_SERVER_TYPE", "SYNC"},
		{"SPRING_AI_MCP_SERVER_PROTOCOL", "STATELESS"},
		{"PORT", "8080"},
		{"MANAGEMENT_ENDPOINT_HEALTH_PROBES_ENABLED", "true"},
		{"ILM_AUTH_METHOD", "oauth-delegation"},
		{"ILM_API_URL", "http://core:8080/api"},
		{"ILM_MCP_ACCESS", testMCPAccessReadOnly},
		{"ILM_MCP_TOOLSETS", "all"},
		{"ILM_MCP_SECURITY_ENABLED", "true"},
		{testMCPIssuerEnv, "https://ilm.example.com/kc/realms/ilm"},
		{testMCPResourceEnv, "https://ilm.example.com/mcp"},
		{testMCPJWKSEnv, "http://ilm-keycloak-service:8080/kc/realms/ilm/protocol/openid-connect/certs"},
		{testMCPTokenEnv, "http://ilm-keycloak-service:8080/kc/realms/ilm/protocol/openid-connect/token"},
		{"ILM_MCP_SECURITY_ALLOWED_ORIGINS", ""},
		{"ILM_MCP_SECURITY_JWS_ALGORITHMS", "RS256"},
		{"ILM_MCP_SECURITY_EXCHANGE_MODE", "delegation"},
		{"ILM_MCP_SECURITY_EXCHANGE_SCOPE", "openid ilm-mcp-upstream"},
		{"ILM_MCP_SECURITY_AUTHORIZATION_SCOPES", "openid,delegation:client:ilm-mcp"},
		{"ILM_MCP_SECURITY_REQUIRED_SCOPE", "mcp:access"},
		{"ILM_MCP_SECURITY_API_AUDIENCE", "ilm"},
		{"ILM_MCP_SECURITY_CLIENT_ID", "ilm-mcp"},
		{"ILM_MCP_SECURITY_ALLOW_HTTP_BACKCHANNEL", "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := envValue(c.Env, tc.name)
			require.Truef(t, ok, "%s must be set", tc.name)
			assert.Equal(t, tc.want, got)
		})
	}
	assert.Len(t, c.Env, len(tests), "every inline variable is in the contract, and none is outside it")

	// The client secret is a reference to the operator-owned Secret, never an inline value, and a
	// required one: the pod waits for the Secret instead of starting without its credential.
	_, inline := envValue(c.Env, testMCPClientSecretEnv)
	assert.False(t, inline, "the client secret must not be an inline value")
	assert.Equal(t, []common.SecretEnvRef{{
		EnvVar: testMCPClientSecretEnv, SecretName: testMCPClientSecret, SecretKey: testMCPClientSecretKey,
	}}, c.SecretEnv)
	assert.Empty(t, c.ConfigMapEnv)
}

// TestResolveMCPAccess proves ILM_MCP_ACCESS carries spec.mcp.access, and falls back to the CRD's
// default for a Platform built in Go, which leaves the field empty.
func TestResolveMCPAccess(t *testing.T) {
	tests := []struct{ name, access, want string }{
		{"unset renders the CRD default", "", testMCPAccessReadOnly},
		{"read-only", testMCPAccessReadOnly, testMCPAccessReadOnly},
		{"read-write", testMCPAccessReadWrite, testMCPAccessReadWrite},
		{"full", testMCPAccessFull, testMCPAccessFull},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ResolveMCP(mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Access = tc.access }))
			got, ok := envValue(c.Env, "ILM_MCP_ACCESS")
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveMCPFollowsTheHostRealmAndPlatformName proves the URLs MCP is configured with come from
// the platform: its public host, its Keycloak realm and its Keycloak Service.
func TestResolveMCPFollowsTheHostRealmAndPlatformName(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		want   map[string]string
	}{
		{
			name:   "a custom realm is in every Keycloak URL",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Realm = "custom" },
			want: map[string]string{
				testMCPIssuerEnv:   "https://ilm.example.com/kc/realms/custom",
				testMCPResourceEnv: "https://ilm.example.com/mcp",
				testMCPJWKSEnv:     "http://ilm-keycloak-service:8080/kc/realms/custom/protocol/openid-connect/certs",
				testMCPTokenEnv:    "http://ilm-keycloak-service:8080/kc/realms/custom/protocol/openid-connect/token",
			},
		},
		{
			name: "the host of an enabled edge wins over common.hostName",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: "edge.example.com"}
			},
			want: map[string]string{
				testMCPIssuerEnv:   "https://edge.example.com/kc/realms/ilm",
				testMCPResourceEnv: "https://edge.example.com/mcp",
			},
		},
		{
			name:   "the Keycloak Service is named after the platform",
			mutate: func(p *otilmv1alpha1.Platform) { p.Name = "prod" },
			want: map[string]string{
				testMCPJWKSEnv:  "http://prod-keycloak-service:8080/kc/realms/ilm/protocol/openid-connect/certs",
				testMCPTokenEnv: "http://prod-keycloak-service:8080/kc/realms/ilm/protocol/openid-connect/token",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ResolveMCP(mcpPlatform(tc.mutate))
			for name, want := range tc.want {
				got, ok := envValue(c.Env, name)
				require.Truef(t, ok, "%s must be set", name)
				assert.Equal(t, want, got, name)
			}
		})
	}
}

// TestResolveMCPAppliesComponentOverrides proves the shared per-component override surface works
// on the MCP server like on every other component: image, replicas, resources and env layer onto
// the operator's defaults.
func TestResolveMCPAppliesComponentOverrides(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.MCP.Image = otilmv1alpha1.ImageSpec{Tag: testMCPTag}
		p.Spec.MCP.Replicas = i32Ptr(3)
		p.Spec.MCP.Resources = &resources
		p.Spec.MCP.Env = []otilmv1alpha1.EnvVar{{Name: "JAVA_OPTS", Value: "-Xmx512m"}}
	})
	c := ResolveMCP(p)

	assert.Equal(t, "hub.omnitrustregistry.com/ilm/mcp-server:develop-latest", c.Image,
		"the tag overrides the bundle's; the image name still comes from the bundle")
	assert.Equal(t, int32(3), c.Replicas)
	assert.Equal(t, resources, c.Resources, "the resources block replaces the operator's defaults as a whole")
	require.NotEmpty(t, c.Env)
	assert.Equal(t, common.EnvPair{Name: "JAVA_OPTS", Value: "-Xmx512m"}, c.Env[len(c.Env)-1],
		"a user variable lands after the operator's, so it wins a duplicate name")
}

// TestResolveMCPDefaults pins the workload's identity, image and default resource block.
func TestResolveMCPDefaults(t *testing.T) {
	c := ResolveMCP(mcpPlatform(nil))

	assert.Equal(t, testMCPName, c.Name)
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
	assert.Equal(t, corev1.ServiceTypeClusterIP, c.ServiceType)
	assert.Equal(t, testMCPImage, c.Image)

	assert.Equal(t, "100m", c.Resources.Requests.Cpu().String())
	assert.Equal(t, "256Mi", c.Resources.Requests.Memory().String())
	assert.Equal(t, "1", c.Resources.Limits.Cpu().String())
	assert.Equal(t, "768Mi", c.Resources.Limits.Memory().String())
}

// TestResolveMCPProbes proves the three probes hit Spring Boot's actuator health groups on the
// container port, and that the startup probe gives the JVM 300 seconds to come up.
func TestResolveMCPProbes(t *testing.T) {
	c := ResolveMCP(mcpPlatform(nil))

	tests := []struct {
		name  string
		probe *corev1.Probe
		path  string
	}{
		{"readiness", c.Probes.Readiness, testMCPReadinessPath},
		{"liveness", c.Probes.Liveness, testMCPLivenessPath},
		{"startup", c.Probes.Startup, testMCPLivenessPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.probe)
			require.NotNil(t, tc.probe.HTTPGet)
			assert.Equal(t, tc.path, tc.probe.HTTPGet.Path)
			assert.Equal(t, intstr.FromInt32(8080), tc.probe.HTTPGet.Port)
		})
	}
	require.NotNil(t, c.Probes.Startup)
	startup := c.Probes.Startup
	assert.Equal(t, int32(300), startup.InitialDelaySeconds+startup.PeriodSeconds*startup.FailureThreshold,
		"the startup probe allows 300 seconds from the start of the container")
}

// TestResolveMCPKeepsTheRootFilesystemReadOnly proves the JVM's only writable path is the
// platform's in-memory /tmp volume, which is what makes a read-only root safe.
func TestResolveMCPKeepsTheRootFilesystemReadOnly(t *testing.T) {
	c := ResolveMCP(mcpPlatform(nil))

	require.NotNil(t, c.ReadOnlyRootFilesystem)
	assert.True(t, *c.ReadOnlyRootFilesystem)
	assert.Equal(t, []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: tmpMountPath}}, c.VolumeMounts)
	require.Len(t, c.Volumes, 1)
	assert.Equal(t, ephemeralVolumeName, c.Volumes[0].Name)
	require.NotNil(t, c.Volumes[0].EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, c.Volumes[0].EmptyDir.Medium)
}

// TestMCPEnabledNeedsTheBundleEntry proves the MCP server renders only when the CR asks for it AND
// the selected version bundle carries its image, so one CR stays portable across platform versions.
func TestMCPEnabledNeedsTheBundleEntry(t *testing.T) {
	tests := []struct {
		name    string
		version string
		enabled bool
		want    bool
	}{
		{"2.21.0 enabled", testVersion221, true, true},
		{"2.21.0 disabled", testVersion221, false, false},
		{"2.20.0 enabled has no MCP image", testVersion220, true, false},
		{"2.20.0 disabled", testVersion220, false, false},
		{"2.19.0 enabled has no MCP image", testVersion219, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
				p.Spec.Version = tc.version
				p.Spec.MCP.Enabled = tc.enabled
			})
			assert.Equal(t, tc.want, MCPEnabled(p))
		})
	}
}

// TestMCPCIMDEnabled proves an unset spec.mcp.cimd.enabled counts as on, and only an explicit
// false turns it off.
func TestMCPCIMDEnabled(t *testing.T) {
	tests := []struct {
		name string
		cimd *bool
		want bool
	}{
		{"unset counts as on", nil, true},
		{"explicitly on", ptr.To(true), true},
		{"explicitly off", ptr.To(false), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.MCP.CIMD.Enabled = tc.cimd })
			assert.Equal(t, tc.want, MCPCIMDEnabled(p))
		})
	}
}

// TestMCPClientSecretName proves the operator-owned client Secret is named after the platform, and
// pins the key the reconcile step writes into.
func TestMCPClientSecretName(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Name = "prod" })
	assert.Equal(t, "prod-mcp-client", MCPClientSecretName(p))
	assert.Equal(t, testMCPClientSecretKey, MCPClientSecretKey)
}

// TestRenderPlatformWithoutMCPAddsNothing proves a Platform that keeps MCP off renders no object
// named after the MCP server, and the same objects as the render with spec.mcp absent. That holds
// for the defaults the API server fills into an empty mcp block, for a disabled section whose other
// fields are set, and for an enabled one on a bundle that has no MCP image.
func TestRenderPlatformWithoutMCPAddsNothing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
	}{
		{
			name: "MCP off with the default access and CIMD on",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.MCP = otilmv1alpha1.MCPSpec{
					Access: testMCPAccessReadOnly,
					CIMD:   otilmv1alpha1.MCPCIMDSpec{Enabled: ptr.To(true)},
				}
			},
		},
		{
			name: "disabled, with every other field set",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.MCP = otilmv1alpha1.MCPSpec{
					ComponentSpec: otilmv1alpha1.ComponentSpec{
						Image:    otilmv1alpha1.ImageSpec{Tag: testMCPTag},
						Replicas: i32Ptr(3),
						Metrics: &otilmv1alpha1.MetricsSpec{
							Enabled:        true,
							ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
						},
					},
					Access:     testMCPAccessFull,
					CIMD:       otilmv1alpha1.MCPCIMDSpec{Enabled: ptr.To(false)},
					Delegation: otilmv1alpha1.MCPDelegationSpec{Group: "mcp-users"},
				}
				p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			},
		},
		{
			name: "enabled on a bundle with no MCP image",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Version = testVersion220
				p.Spec.MCP.Enabled = true
				p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
				p.Spec.MCP.Enabled = false
				tc.mutate(p)
			})
			absent := p.DeepCopy()
			absent.Spec.MCP = otilmv1alpha1.MCPSpec{}

			got := RenderPlatform(p, testOperatorNamespace)

			for _, o := range got {
				assert.NotEqualf(t, testMCPName, o.GetName(), "%T must not render", o)
			}
			assert.Equal(t, RenderPlatform(absent, testOperatorNamespace), got,
				"the render must equal the one with spec.mcp absent")
		})
	}
}

// TestRenderPlatformWithMCP proves enabling MCP adds exactly a ServiceAccount, a Deployment and a
// Service named after the MCP server, carrying the standard labels the platform NetworkPolicies
// select, wired through the same chokepoints as every other component.
func TestRenderPlatformWithMCP(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.AdditionalEnv = []otilmv1alpha1.EnvVar{{Name: "OTEL_SDK_DISABLED", Value: "false"}}
	})
	off := p.DeepCopy()
	off.Spec.MCP.Enabled = false

	objs := RenderPlatform(p, testOperatorNamespace)
	assert.Len(t, objs, len(RenderPlatform(off, testOperatorNamespace))+3,
		"enabling MCP adds a ServiceAccount, a Deployment and a Service, and nothing else")

	var (
		sa       *corev1.ServiceAccount
		dep      *appsv1.Deployment
		svc      *corev1.Service
		policies []*networkingv1.NetworkPolicy
	)
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.ServiceAccount:
			if v.Name == testMCPName {
				sa = v
			}
		case *appsv1.Deployment:
			if v.Name == testMCPName {
				dep = v
			}
		case *corev1.Service:
			if v.Name == testMCPName {
				svc = v
			}
		case *networkingv1.NetworkPolicy:
			policies = append(policies, v)
		}
	}
	require.NotNil(t, sa, "ServiceAccount %q must render", testMCPName)
	require.NotNil(t, dep, "Deployment %q must render", testMCPName)
	require.NotNil(t, svc, "Service %q must render", testMCPName)

	wantLabels := map[string]string{
		common.NameLabel:      testMCPName,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: testMCPName,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
	assert.Equal(t, wantLabels, sa.Labels)
	assert.Equal(t, wantLabels, dep.Labels)
	assert.Equal(t, wantLabels, svc.Labels)
	assert.Equal(t, wantLabels, dep.Spec.Template.Labels, "the pods carry the standard labels")

	selector := map[string]string{common.NameLabel: testMCPName, common.InstanceLabel: p.Name}
	assert.Equal(t, selector, dep.Spec.Selector.MatchLabels)
	assert.Equal(t, selector, svc.Spec.Selector)
	assert.Equal(t, testMCPName, dep.Spec.Template.Spec.ServiceAccountName)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(8080), svc.Spec.Ports[0].Port)
	assert.Equal(t, intstr.FromString("http"), svc.Spec.Ports[0].TargetPort)

	// The pods are isolated like every other platform pod: the default-deny ingress and the egress
	// policy select them, and the edge policy, which admits the ingress controller to the gateway
	// alone, does not.
	selects := map[string]bool{}
	for _, np := range policies {
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		require.NoError(t, err)
		selects[np.Name] = sel.Matches(labels.Set(dep.Spec.Template.Labels))
	}
	assert.Equal(t, map[string]bool{
		npDefaultDenyIngressName: true,
		npAllowEdgeName:          false,
		npAllowEgressName:        true,
	}, selects)

	// The platform-wide passthroughs reach MCP like every other component.
	assert.Equal(t, testVersion221, dep.Spec.Template.Annotations[PlatformVersionAnnotation])
	main := mainContainer(dep)
	assert.Equal(t, testMCPName, main.Name)
	env := map[string]corev1.EnvVar{}
	for _, e := range main.Env {
		env[e.Name] = e
	}
	assert.Equal(t, "false", env["OTEL_SDK_DISABLED"].Value)

	// The client secret reaches the container as a required secretKeyRef.
	secret := env[testMCPClientSecretEnv]
	require.NotNil(t, secret.ValueFrom)
	require.NotNil(t, secret.ValueFrom.SecretKeyRef)
	assert.Equal(t, testMCPClientSecret, secret.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, testMCPClientSecretKey, secret.ValueFrom.SecretKeyRef.Key)
	assert.Nil(t, secret.ValueFrom.SecretKeyRef.Optional, "the Secret is required")
}

// TestRenderPlatformHADefaultsIncludeMCP proves the HA profile treats the stateless MCP server like
// the other stateless components, and an explicit autoscaling block renders its HPA.
func TestRenderPlatformHADefaultsIncludeMCP(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	})
	objs := RenderPlatform(p, testOperatorNamespace)

	dep := deploymentByName(objs, testMCPName)
	require.NotNil(t, dep)
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(2), *dep.Spec.Replicas, "HA default replicas")
	pdb := pdbByName(objs, testMCPName)
	require.NotNil(t, pdb, "HA renders a PDB")
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable)
	require.NotNil(t, dep.Spec.Template.Spec.Affinity, "HA spreads the replicas")

	scaled := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.MCP.Autoscaling = &otilmv1alpha1.AutoscalingSpec{
			MaxReplicas: 4, TargetCPUUtilization: ptr.To(int32(75)),
		}
	})
	objs = RenderPlatform(scaled, testOperatorNamespace)
	hpa := hpaByName(objs, testMCPName)
	require.NotNil(t, hpa, "autoscaling renders an HPA")
	assert.Equal(t, int32(4), hpa.Spec.MaxReplicas)
	assert.Len(t, hpa.Spec.Metrics, 1)
	assert.Nil(t, deploymentByName(objs, testMCPName).Spec.Replicas, "the HPA owns the replica count")
}

// TestMCPServiceMonitorFollowsTheWorkload proves the MCP server gets a ServiceMonitor only when it
// is itself rendered, so a section that renders nothing never asks for the Prometheus CRDs.
func TestMCPServiceMonitorFollowsTheWorkload(t *testing.T) {
	p := mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.MCP.Metrics = &otilmv1alpha1.MetricsSpec{
			Enabled:        true,
			ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
		}
	})

	objs := ResolvePlatformServiceMonitors(p)
	require.Len(t, objs, 1)
	assert.Equal(t, testMCPName, objs[0].GetName())
	assert.NotNil(t, ServiceMonitorDependencies(p))

	p.Spec.MCP.Enabled = false
	assert.Empty(t, ResolvePlatformServiceMonitors(p), "no ServiceMonitor while MCP is off")
	assert.Nil(t, ServiceMonitorDependencies(p))

	p.Spec.MCP.Enabled = true
	p.Spec.Version = testVersion220
	assert.Empty(t, ResolvePlatformServiceMonitors(p), "no ServiceMonitor on a bundle with no MCP image")
}
