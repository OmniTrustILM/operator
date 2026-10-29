/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"cmp"
	"maps"
	"slices"
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

func mcpIdentityPlatform(identity *otilmv1alpha1.MCPIdentitySpec) *otilmv1alpha1.Platform {
	return mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity = identity })
}

// mcpExternalPlatform returns mcpPlatform with MCP signing in through an external identity provider,
// set to the settings the mode requires and nothing else. mutate, when set, runs last.
func mcpExternalPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	return mcpPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{
			Mode:            testMCPIdentityExternal,
			Issuer:          testMCPIdPIssuer,
			JWKSetURI:       testMCPIdPJWKS,
			TokenURI:        testMCPIdPToken,
			ClientID:        testMCPIdPClientID,
			ClientSecretRef: testMCPIdPClientSecret,
			ExchangeMode:    testMCPExchangeDelegation,
		}
		if mutate != nil {
			mutate(p)
		}
	})
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
		{testMCPResourceEnv, testMCPResourceURL},
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
			require.Truef(t, ok, testMustBeSet, tc.name)
			assert.Equal(t, tc.want, got)
		})
	}
	// With no spec.common.proxy, MCP gets none of Core's proxy variables: the contract is all of it.
	assert.Len(t, c.Env, len(tests), "every inline variable is in the contract, and none is outside it")

	// The client secret is a reference to the operator-owned Secret, never an inline value, and a
	// required one: the pod waits for the Secret instead of starting without its credential. With no
	// spec.common.trustedCertificates, it is the only Secret MCP references.
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
				testMCPResourceEnv: testMCPResourceURL,
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
				require.Truef(t, ok, testMustBeSet, name)
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

// mcpOptionalSecurityEnv are the variables external mode passes to MCP only when the spec sets the
// matching setting, so that MCP's own default applies otherwise. Managed mode sets all of them.
var mcpOptionalSecurityEnv = []string{
	testMCPAPIAudienceEnv, testMCPExchangeScopeEnv, testMCPAuthScopesEnv, testMCPRequiredScopeEnv,
}

// externalMCPEnv returns the environment an external identity gives MCP: managed mode's variables in
// managed mode's order, without those the identity provider decides (decided), and with the values in
// overrides in their place. It fails the test when an override names a variable managed mode does not
// render.
func externalMCPEnv(t *testing.T, managed []common.EnvPair, decided []string, overrides ...map[string]string) []common.EnvPair {
	t.Helper()
	want := map[string]string{}
	for _, e := range managed {
		if !slices.Contains(decided, e.Name) {
			want[e.Name] = e.Value
		}
	}
	for _, override := range overrides {
		maps.Copy(want, override)
	}
	var env []common.EnvPair
	for _, e := range managed {
		if value, ok := want[e.Name]; ok {
			env = append(env, common.EnvPair{Name: e.Name, Value: value})
		}
	}
	require.Len(t, env, len(want), "every expected variable is one managed mode renders")
	return env
}

// TestMCPEnvExternalIdentity proves an external identity provider replaces what managed mode derives
// from the platform's own Keycloak. The issuer, the key-set and token endpoints, the client and the
// exchange mode come from the spec, each optional setting is passed only when the spec sets it, and
// the client secret is a reference to the Secret the spec names. Every other variable is what managed
// mode renders, in managed mode's order.
func TestMCPEnvExternalIdentity(t *testing.T) {
	const (
		exchangeScope = "openid corp-upstream"
		requiredScope = "mcp:use"
		apiAudience   = "corp-ilm"
		authScopes    = "openid,delegation:client:mcp-corp"
	)
	// The variables the identity provider decides. Every other managed variable keeps its value.
	identityEnv := append([]string{
		testMCPIssuerEnv, testMCPJWKSEnv, testMCPTokenEnv, testMCPClientIDEnv, testMCPExchangeModeEnv,
	}, mcpOptionalSecurityEnv...)
	managed := ResolveMCP(mcpPlatform(nil)).Env
	// What external mode must keep as it is in managed mode: the resource URL derived from the host,
	// the Core API, the HTTP switch and the authentication method.
	unchanged := map[string]string{
		testMCPResourceEnv:    testMCPResourceURL,
		"ILM_API_URL":         "http://core:8080/api",
		testMCPBackchannelEnv: "true",
		"ILM_AUTH_METHOD":     "oauth-delegation",
	}
	required := map[string]string{
		testMCPIssuerEnv:       testMCPIdPIssuer,
		testMCPJWKSEnv:         testMCPIdPJWKS,
		testMCPTokenEnv:        testMCPIdPToken,
		testMCPClientIDEnv:     testMCPIdPClientID,
		testMCPExchangeModeEnv: testMCPExchangeDelegation,
	}

	tests := []struct {
		name string
		// mutate changes the platform on top of the required settings.
		mutate func(*otilmv1alpha1.Platform)
		// want holds the optional settings the case sets, and the values it changes.
		want map[string]string
		// secretKey is the key the client secret is read from, client-secret when empty.
		secretKey string
	}{
		{name: "required settings only"},
		{
			name:   "standard exchange mode",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.ExchangeMode = testMCPExchangeStandard },
			want:   map[string]string{testMCPExchangeModeEnv: testMCPExchangeStandard},
		},
		{
			name:   "exchange scope alone",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.ExchangeScope = exchangeScope },
			want:   map[string]string{testMCPExchangeScopeEnv: exchangeScope},
		},
		{
			name:   "required scope alone",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.RequiredScope = requiredScope },
			want:   map[string]string{testMCPRequiredScopeEnv: requiredScope},
		},
		{
			name:   "API audience alone",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Identity.APIAudience = apiAudience },
			want:   map[string]string{testMCPAPIAudienceEnv: apiAudience},
		},
		{
			name: "authorization scopes alone, comma-joined",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.MCP.Identity.AuthorizationScopes = []string{"openid", "delegation:client:mcp-corp"}
			},
			want: map[string]string{testMCPAuthScopesEnv: authScopes},
		},
		{
			name: "every optional setting and another client secret key",
			mutate: func(p *otilmv1alpha1.Platform) {
				id := p.Spec.MCP.Identity
				id.ExchangeScope = exchangeScope
				id.RequiredScope = requiredScope
				id.APIAudience = apiAudience
				id.AuthorizationScopes = []string{"openid", "delegation:client:mcp-corp"}
				id.ClientSecretKey = testMCPIdPCustomSecretKey
			},
			want: map[string]string{
				testMCPExchangeScopeEnv: exchangeScope,
				testMCPRequiredScopeEnv: requiredScope,
				testMCPAPIAudienceEnv:   apiAudience,
				testMCPAuthScopesEnv:    authScopes,
			},
			secretKey: testMCPIdPCustomSecretKey,
		},
		{
			name: "no Keycloak block and another platform name",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Keycloak = nil
				p.Name = "corp"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ResolveMCP(mcpExternalPlatform(tc.mutate))

			wantEnv := externalMCPEnv(t, managed, identityEnv, required, tc.want)
			assert.Equal(t, wantEnv, c.Env, "managed mode's variables and order, with the identity provider's values")
			for name, value := range unchanged {
				got, ok := envValue(c.Env, name)
				require.Truef(t, ok, testMustBeSet, name)
				assert.Equal(t, value, got, name)
			}
			assert.Equal(t, []common.SecretEnvRef{{
				EnvVar: testMCPClientSecretEnv, SecretName: testMCPIdPClientSecret,
				SecretKey: cmp.Or(tc.secretKey, testMCPIdPClientSecretKey),
			}}, c.SecretEnv, "the client secret is a required reference to the Secret the spec names")
		})
	}
}

// TestMCPIdentityMode proves which identity mode MCP renders in. Only an explicit external mode is
// external: no identity block, an identity whose mode is empty (a Platform built in Go skips the
// CRD's default) and mode managed are all managed. Neither mode applies while MCP does not render.
func TestMCPIdentityMode(t *testing.T) {
	tests := []struct {
		name                      string
		platform                  *otilmv1alpha1.Platform
		wantManaged, wantExternal bool
	}{
		{"no identity block", mcpPlatform(nil), true, false},
		{"identity with an empty mode", mcpIdentityPlatform(&otilmv1alpha1.MCPIdentitySpec{}), true, false},
		{
			"identity with mode managed",
			mcpIdentityPlatform(&otilmv1alpha1.MCPIdentitySpec{Mode: testMCPIdentityManaged}), true, false,
		},
		{"identity with mode external", mcpExternalPlatform(nil), false, true},
		{"MCP off", mcpPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Enabled = false }), false, false},
		{
			"external identity, MCP off",
			mcpExternalPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Enabled = false }), false, false,
		},
		{
			"external identity, bundle with no MCP image",
			mcpExternalPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion220 }), false, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantManaged, MCPManagedIdentity(tc.platform), "MCPManagedIdentity")
			assert.Equal(t, tc.wantExternal, MCPExternalIdentity(tc.platform), "MCPExternalIdentity")
		})
	}
}

// TestResolveMCPRendersAnEmptyIdentityAsManaged proves an identity block that names no external mode
// renders the managed MCP server, byte for byte as a Platform with no identity block does.
func TestResolveMCPRendersAnEmptyIdentityAsManaged(t *testing.T) {
	want := ResolveMCP(mcpPlatform(nil))
	identities := map[string]*otilmv1alpha1.MCPIdentitySpec{
		"an empty identity": {},
		"mode managed":      {Mode: testMCPIdentityManaged},
	}
	for name, identity := range identities {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, ResolveMCP(mcpIdentityPlatform(identity)))
		})
	}
}

// TestMCPClientSecret proves where MCP's client secret is read from: the operator-owned Secret in
// managed mode, and the Secret the spec references in external mode, under the key it names or
// client-secret by default.
func TestMCPClientSecret(t *testing.T) {
	tests := []struct {
		name              string
		platform          *otilmv1alpha1.Platform
		wantName, wantKey string
	}{
		{"managed", mcpPlatform(nil), testMCPClientSecret, testMCPClientSecretKey},
		{
			"an identity with an empty mode", mcpIdentityPlatform(&otilmv1alpha1.MCPIdentitySpec{}),
			testMCPClientSecret, testMCPClientSecretKey,
		},
		{"external, default key", mcpExternalPlatform(nil), testMCPIdPClientSecret, testMCPIdPClientSecretKey},
		{
			"external, named key",
			mcpExternalPlatform(func(p *otilmv1alpha1.Platform) {
				p.Spec.MCP.Identity.ClientSecretKey = testMCPIdPCustomSecretKey
			}),
			testMCPIdPClientSecret, testMCPIdPCustomSecretKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, key := MCPClientSecret(tc.platform)
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantKey, key)
		})
	}
}

// TestMCPNoProxyListsItsOwnPlainHTTPHosts proves what MCP's NO_PROXY holds while HTTP_PROXY or
// HTTPS_PROXY is set: the value Core gets, kept first, then the host of every plain-http URL MCP is given
// that the value does not list yet, so that MCP's calls inside the cluster never reach the proxy. Those
// are the Core API, the managed Keycloak's Service with the managed identity, and the key-set and token
// hosts of an external identity that are http. An https host goes through the proxy's tunnel, so it stays
// off the list. With no proxy URL nothing is added, and Core's NO_PROXY is never changed.
func TestMCPNoProxyListsItsOwnPlainHTTPHosts(t *testing.T) {
	const (
		httpsJWKS  = "https://idp.example.com/realms/corp/protocol/openid-connect/certs"
		httpsToken = "https://idp.example.com/realms/corp/protocol/openid-connect/token"
		keycloak   = "ilm-keycloak-service"
	)
	endpoints := func(jwks, token string) func(*otilmv1alpha1.Platform) {
		return func(p *otilmv1alpha1.Platform) {
			p.Spec.MCP.Identity.JWKSetURI, p.Spec.MCP.Identity.TokenURI = jwks, token
		}
	}
	tests := []struct {
		name      string
		build     func(func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform
		endpoints func(*otilmv1alpha1.Platform)
		proxy     otilmv1alpha1.OutboundProxySpec
		// want is MCP's NO_PROXY, empty for none.
		want string
	}{
		{
			name: "managed identity: the Core API and the managed Keycloak's Service", build: mcpPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP}, want: "core," + keycloak,
		},
		{
			name: "managed identity: after the entries the user wrote", build: mcpPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP, NoProxy: testNoProxyEntries}, want: testNoProxyEntries + ",core," + keycloak,
		},
		{
			name: "managed identity: a host the user lists already is not added again, whatever separates the entries", build: mcpPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP, NoProxy: "localhost|core, " + keycloak}, want: "localhost|core, " + keycloak,
		},
		{
			name: "managed identity: HTTPS_PROXY alone is a proxy", build: mcpPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{HTTPS: testProxyHTTPS}, want: "core," + keycloak,
		},
		{
			name: "external identity with https endpoints: the Core API alone", build: mcpExternalPlatform, endpoints: endpoints(httpsJWKS, httpsToken),
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP}, want: "core",
		},
		{
			name: "external identity with http endpoints on one Service host: that host once", build: mcpExternalPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP}, want: "core,kc.idp.svc",
		},
		{
			name: "external identity with an http key set and an https token endpoint", build: mcpExternalPlatform, endpoints: endpoints(testMCPIdPJWKS, httpsToken),
			proxy: otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP}, want: "core,kc.idp.svc",
		},
		{
			name: "external identity with http endpoints on two hosts, one with a cluster domain", build: mcpExternalPlatform,
			endpoints: endpoints("http://a.idp.svc/certs", "http://pod-0.b.idp.svc.cluster.local:8080/token"),
			proxy:     otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP, NoProxy: "localhost"}, want: "localhost,core,a.idp.svc,pod-0.b.idp.svc.cluster.local",
		},
		{
			name: "no proxy URL: the hosts to bypass are Core's value as it is", build: mcpPlatform,
			proxy: otilmv1alpha1.OutboundProxySpec{NoProxy: testNoProxyEntries}, want: testNoProxyEntries,
		},
		{name: "no proxy at all: no NO_PROXY", build: mcpPlatform},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.build(func(p *otilmv1alpha1.Platform) {
				if tc.endpoints != nil {
					tc.endpoints(p)
				}
				p.Spec.Common.Proxy = tc.proxy
			})

			got, set := envValue(ResolveMCP(p).Env, testNoProxyEnv)
			assert.Equal(t, tc.want != "", set, "MCP has NO_PROXY only when there is something to put in it")
			assert.Equal(t, tc.want, got)

			core, coreSet := envValue(ResolveCore(p).Env, testNoProxyEnv)
			assert.Equal(t, tc.proxy.NoProxy != "", coreSet)
			assert.Equal(t, tc.proxy.NoProxy, core, "Core's NO_PROXY is the value the spec holds, whatever MCP adds")
		})
	}
}

// TestMCPGetsTrustAndProxyLikeCore proves MCP gets the trusted certificates and the outbound proxy the
// way Core does, in both identity modes: the same optional Secret reference, and the same proxy
// variables with the same values, placed after the variables MCP already had. PROXY_ENABLED stays with
// Core, which alone reads it. With neither setting, MCP has none of them. While a proxy URL is set,
// MCP's NO_PROXY also lists its own hosts, which TestMCPNoProxyListsItsOwnPlainHTTPHosts covers.
func TestMCPGetsTrustAndProxyLikeCore(t *testing.T) {
	const caKey = "ca.crt"
	trust := func(secret, key string) []common.SecretEnvRef {
		return []common.SecretEnvRef{{EnvVar: testTrustedCertsEnv, SecretName: secret, SecretKey: key, Optional: true}}
	}
	generatedAdminCert := func() *otilmv1alpha1.RegisterAdminSpec {
		return &otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
		}
	}

	modes := []struct {
		name  string
		build func(func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform
	}{
		{"managed identity", mcpPlatform},
		{"external identity", mcpExternalPlatform},
	}
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		// wantTrust is the reference MCP holds after its client secret, and wantProxy the variables it
		// holds after the ones it already had.
		wantTrust []common.SecretEnvRef
		wantProxy map[string]string
	}{
		{name: "neither is set"},
		{
			name:      "trusted certificates",
			mutate:    func(p *otilmv1alpha1.Platform) { p.Spec.Common.TrustedCertificates.SecretRef = testTrustSecret },
			wantTrust: trust(testTrustSecret, caKey),
		},
		{
			name: "trusted certificates under another key",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: testTrustSecret, CAKey: testTLSCA}
			},
			wantTrust: trust(testTrustSecret, testTLSCA),
		},
		{
			name: "the bundle the operator composes from the administrator certificate's CA and a bundle of the user's",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.TrustedCertificates.SecretRef = testTrustSecret
				p.Spec.RegisterAdmin = generatedAdminCert()
			},
			wantTrust: trust(composedTrustedCertsSecretName, caKey),
		},
		{
			name:      "the bundle the operator composes from the administrator certificate's CA alone",
			mutate:    func(p *otilmv1alpha1.Platform) { p.Spec.RegisterAdmin = generatedAdminCert() },
			wantTrust: trust(composedTrustedCertsSecretName, caKey),
		},
		{
			name: "both proxy URLs",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true, HTTP: testProxyHTTP, HTTPS: testProxyHTTPS}
			},
			wantProxy: map[string]string{testHTTPProxyEnv: testProxyHTTP, testHTTPSProxyEnv: testProxyHTTPS},
		},
		{
			name: "the hosts to bypass the proxy for, with no proxy URL",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true, NoProxy: testNoProxyEntries}
			},
			wantProxy: map[string]string{testNoProxyEnv: testNoProxyEntries},
		},
		{
			name: "only the proxy settings that are set",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true, HTTPS: testProxyHTTPS}
			},
			wantProxy: map[string]string{testHTTPSProxyEnv: testProxyHTTPS},
		},
		{
			name: "trusted certificates and the proxy together",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Common.TrustedCertificates.SecretRef = testTrustSecret
				p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{HTTP: testProxyHTTP}
			},
			wantTrust: trust(testTrustSecret, caKey),
			wantProxy: map[string]string{testHTTPProxyEnv: testProxyHTTP},
		},
	}
	for _, mode := range modes {
		for _, tc := range tests {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				p := mode.build(tc.mutate)
				core, mcp := ResolveCore(p), ResolveMCP(p)

				secretName, secretKey := MCPClientSecret(p)
				wantSecrets := append([]common.SecretEnvRef{{
					EnvVar: testMCPClientSecretEnv, SecretName: secretName, SecretKey: secretKey,
				}}, tc.wantTrust...)
				assert.Equal(t, wantSecrets, mcp.SecretEnv, "the certificates come after the client secret")
				coreTrust, _ := secretEnvFor(core.SecretEnv, testTrustedCertsEnv)
				mcpTrust, _ := secretEnvFor(mcp.SecretEnv, testTrustedCertsEnv)
				assert.Equal(t, coreTrust, mcpTrust, "the reference is Core's")

				assertMCPProxyEnv(t, p, core, mcp, ResolveMCP(mode.build(nil)).Env, tc.wantProxy)
			})
		}
	}
}

// assertMCPProxyEnv asserts that MCP's proxy variables are Core's, placed after the variables MCP already
// had (before), and that they are exactly wantProxy. While a proxy URL is set MCP's NO_PROXY is not
// Core's, because it lists MCP's own hosts as well, so it is left out of both comparisons.
func assertMCPProxyEnv(t *testing.T, p *otilmv1alpha1.Platform, core, mcp common.Component, before []common.EnvPair, wantProxy map[string]string) {
	t.Helper()
	proxy := p.Spec.Common.Proxy
	ownList := proxy.HTTP != "" || proxy.HTTPS != ""
	got, want := slices.Clone(mcp.Env), slices.Clone(before)
	for _, name := range []string{testHTTPProxyEnv, testHTTPSProxyEnv, testNoProxyEnv} {
		if name == testNoProxyEnv && ownList {
			got = slices.DeleteFunc(got, func(e common.EnvPair) bool { return e.Name == name })
			continue
		}
		if value, ok := wantProxy[name]; ok {
			want = append(want, common.EnvPair{Name: name, Value: value})
		}
		coreValue, _ := envValue(core.Env, name)
		mcpValue, _ := envValue(mcp.Env, name)
		assert.Equal(t, coreValue, mcpValue, "%s is Core's", name)
	}
	assert.Equal(t, want, got, "the proxy variables follow the ones MCP already had")
	_, coreReads := envValue(core.Env, testProxyEnabledEnv)
	_, mcpGets := envValue(mcp.Env, testProxyEnabledEnv)
	assert.True(t, coreReads, "Core reads PROXY_ENABLED")
	assert.False(t, mcpGets, "MCP does not")
}
