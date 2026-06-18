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

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestRenderPlatformWorkloadTypeStatefulSet asserts that a component with
// workloadType=StatefulSet renders a StatefulSet (with the headless serviceName and the
// SAME SCC-hardened pod template the Deployment path carries) and NO Deployment of that
// name, while the other components stay Deployments (default). This is the render-layer
// wiring for ComponentSpec.WorkloadType.
func TestRenderPlatformWorkloadTypeStatefulSet(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet

	var coreSTS *appsv1.StatefulSet
	deployNames := map[string]bool{}
	stsNames := map[string]bool{}
	for _, o := range RenderPlatform(p) {
		switch v := o.(type) {
		case *appsv1.Deployment:
			deployNames[v.Name] = true
		case *appsv1.StatefulSet:
			stsNames[v.Name] = true
			if v.Name == coreComponentName {
				coreSTS = v
			}
		}
	}

	// Core is now a StatefulSet, NOT a Deployment.
	require.NotNil(t, coreSTS, "core must render as a StatefulSet when workloadType=StatefulSet")
	assert.False(t, deployNames[coreComponentName], "core must NOT also render as a Deployment")
	assert.Equal(t, coreComponentName, coreSTS.Spec.ServiceName, "the StatefulSet must reference core's headless Service")
	require.NotNil(t, coreSTS.Spec.Selector)
	assert.Equal(t, coreComponentName, coreSTS.Spec.Selector.MatchLabels["app.kubernetes.io/name"])

	// The StatefulSet's pod template is SCC-hardened exactly like the Deployment path:
	// every container (main, init, sidecar) carries the four restricted-v2 fields.
	all := append(append([]corev1.Container{}, coreSTS.Spec.Template.Spec.InitContainers...), coreSTS.Spec.Template.Spec.Containers...)
	require.NotEmpty(t, all)
	for _, c := range all {
		sc := c.SecurityContext
		require.NotNilf(t, sc, "core/%s (StatefulSet) must carry a SecurityContext", c.Name)
		require.NotNil(t, sc.RunAsNonRoot)
		assert.Truef(t, *sc.RunAsNonRoot, "core/%s must run as non-root", c.Name)
		assert.Nilf(t, sc.RunAsUser, "core/%s must not pin a UID", c.Name)
		require.NotNil(t, sc.AllowPrivilegeEscalation)
		assert.Falsef(t, *sc.AllowPrivilegeEscalation, "core/%s must not allow privilege escalation", c.Name)
		require.NotNil(t, sc.Capabilities)
		assert.Equalf(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "core/%s must drop ALL caps", c.Name)
		require.NotNil(t, sc.SeccompProfile)
		assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
	}

	// The pod-level security context is hardened too.
	ps := coreSTS.Spec.Template.Spec
	require.NotNil(t, ps.SecurityContext)
	require.NotNil(t, ps.SecurityContext.RunAsNonRoot)
	assert.True(t, *ps.SecurityContext.RunAsNonRoot)

	// Other components remain Deployments (default), not StatefulSets.
	for _, comp := range []string{"auth", "scheduler", gatewayName} {
		assert.Truef(t, deployNames[comp], "%s must remain a Deployment (default)", comp)
		assert.Falsef(t, stsNames[comp], "%s must NOT be a StatefulSet (default)", comp)
	}
}

// TestRenderPlatformWorkloadTypeDefaultsToDeployment asserts the additive, non-breaking
// default: with no workloadType set, EVERY component renders as a Deployment and there are
// NO StatefulSets in the output.
func TestRenderPlatformWorkloadTypeDefaultsToDeployment(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true

	for _, o := range RenderPlatform(p) {
		_, isSTS := o.(*appsv1.StatefulSet)
		assert.Falsef(t, isSTS, "no StatefulSet must be rendered by default (got %q)", o.GetName())
	}

	// And core specifically is a Deployment.
	var coreDep *appsv1.Deployment
	for _, o := range RenderPlatform(p) {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == coreComponentName {
			coreDep = d
		}
	}
	require.NotNil(t, coreDep, "core must render as a Deployment by default")
}

// TestRenderPlatformWorkloadTypeStatefulSetOmitsReplicasUnderHPA asserts a
// StatefulSet-typed component that ALSO configures autoscaling omits .spec.replicas (the
// HPA owns scaling), exactly like the Deployment path under SSA.
func TestRenderPlatformWorkloadTypeStatefulSetOmitsReplicasUnderHPA(t *testing.T) {
	p := basePlatform()
	cpu := int32(80)
	p.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
	p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 5, TargetCPUUtilization: &cpu}

	var coreSTS *appsv1.StatefulSet
	for _, o := range RenderPlatform(p) {
		if s, ok := o.(*appsv1.StatefulSet); ok && s.Name == coreComponentName {
			coreSTS = s
		}
	}
	require.NotNil(t, coreSTS)
	assert.Nil(t, coreSTS.Spec.Replicas, "an HPA-owned StatefulSet must omit .spec.replicas")
}

func TestRenderPlatformObjects(t *testing.T) {
	// utils disabled (default); check core + scheduler + auth-opa-policies.
	objs := RenderPlatform(basePlatform())
	w := bom.Wiring()

	// objKey identifies a rendered object by kind + name for presence assertions.
	type objKey struct{ kind, name string }
	got := map[objKey]client.Object{}
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.ConfigMap:
			got[objKey{"ConfigMap", v.Name}] = o
		case *corev1.ServiceAccount:
			got[objKey{"ServiceAccount", v.Name}] = o
		case *appsv1.Deployment:
			got[objKey{"Deployment", v.Name}] = o
		case *corev1.Service:
			got[objKey{"Service", v.Name}] = o
		}
	}

	for _, want := range []objKey{
		{"ConfigMap", w.MessagingConfigMapName},
		// core
		{"ServiceAccount", "core"},
		{"Deployment", "core"},
		{"Service", "core"},
		// scheduler
		{"ServiceAccount", "scheduler"},
		{"Deployment", "scheduler"},
		{"Service", "scheduler"},
		// auth-opa-policies
		{"ServiceAccount", testOPAPolicies},
		{"Deployment", testOPAPolicies},
		{"Service", testOPAPolicies},
		// auth
		{"ServiceAccount", "auth"},
		{"Deployment", "auth"},
		{"Service", "auth"},
		// fe-administrator (incl. its config.js ConfigMap)
		{"ConfigMap", "fe-administrator-configmap"},
		{"ServiceAccount", testFeAdmin},
		{"Deployment", testFeAdmin},
		{"Service", testFeAdmin},
		// api-gateway (Kong) incl. its declarative-config ConfigMap (global-configmap).
		{"ConfigMap", globalConfigMapName},
		{"ServiceAccount", gatewayName},
		{"Deployment", gatewayName},
		{"Service", gatewayName},
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("RenderPlatform missing %s/%s", want.kind, want.name)
		}
	}

	// The operator-managed auth connection-string Secret (auth-db) is NOT
	// part of the render model — it is composed at reconcile time, never rendered.
	if _, ok := got[objKey{"Secret", "auth-db"}]; ok {
		t.Errorf("auth-db Secret must not be in RenderPlatform output (reconcile-time only)")
	}

	// utils must NOT appear when disabled (default).
	for _, absent := range []objKey{
		{"ServiceAccount", "utils"},
		{"Deployment", "utils"},
		{"Service", "utils"},
	} {
		if _, ok := got[absent]; ok {
			t.Errorf("RenderPlatform should NOT include %s/%s when utils disabled", absent.kind, absent.name)
		}
	}
}

// TestRenderPlatformAllContainersSCCHardened asserts that EVERY container — main,
// sidecar, AND init — across the full RenderPlatform output carries the four SCC-
// critical fields (RunAsNonRoot=true, AllowPrivilegeEscalation=false, drop-ALL caps,
// seccomp RuntimeDefault), even on workloads whose builder set a partial SecurityContext
// (e.g. the OPA sidecar / init containers that set only ReadOnlyRootFilesystem). This
// guards the hardenContainer "fill-don't-replace" contract end-to-end.
func TestRenderPlatformAllContainersSCCHardened(t *testing.T) {
	// Enable utils + an admin bootstrap so the rendered set includes the maximum
	// container surface (Core's OPA sidecar + wait-for-auth / provision-instance-
	// queue init containers, scheduler's wait-for-messaging init container, etc.).
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{APIURL: "https://prov.example.com"}
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}

	var containersChecked int
	for _, o := range RenderPlatform(p) {
		dep, ok := o.(*appsv1.Deployment)
		if !ok {
			continue
		}
		ps := dep.Spec.Template.Spec
		all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
		for _, c := range all {
			containersChecked++
			sc := c.SecurityContext
			require.NotNilf(t, sc, "container %q in Deployment %q must carry a SecurityContext", c.Name, dep.Name)
			require.NotNilf(t, sc.RunAsNonRoot, "container %q (%q): RunAsNonRoot must be set", c.Name, dep.Name)
			assert.Truef(t, *sc.RunAsNonRoot, "container %q (%q) must run as non-root", c.Name, dep.Name)
			require.NotNilf(t, sc.AllowPrivilegeEscalation, "container %q (%q): AllowPrivilegeEscalation must be set", c.Name, dep.Name)
			assert.Falsef(t, *sc.AllowPrivilegeEscalation, "container %q (%q) must not allow privilege escalation", c.Name, dep.Name)
			require.NotNilf(t, sc.Capabilities, "container %q (%q): Capabilities must be set", c.Name, dep.Name)
			assert.Equalf(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "container %q (%q) must drop ALL caps", c.Name, dep.Name)
			require.NotNilf(t, sc.SeccompProfile, "container %q (%q): SeccompProfile must be set", c.Name, dep.Name)
			assert.Equalf(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "container %q (%q) seccomp must be RuntimeDefault", c.Name, dep.Name)
			assert.Nilf(t, sc.RunAsUser, "container %q (%q) must not pin a UID", c.Name, dep.Name)
		}
	}
	// Sanity: we actually exercised a meaningful number of containers (mains + sidecar +
	// several init containers across the enabled components), not zero.
	assert.GreaterOrEqual(t, containersChecked, 10, "expected the full platform to render many containers")
}

// TestRenderPlatformReadOnlyRootFilesystem documents which components/containers get a
// read-only root filesystem. ALL workloads — every init container, sidecar, AND every
// JVM/.NET main container — now run with a read-only root, validated end-to-end against
// the live ILM images on Kind (the managed-platform e2e). It is the executable record
// of the read-only-root audit.
func TestRenderPlatformReadOnlyRootFilesystem(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true

	deps := map[string]*appsv1.Deployment{}
	for _, o := range RenderPlatform(p) {
		if dep, ok := o.(*appsv1.Deployment); ok {
			deps[dep.Name] = dep
		}
	}

	// rootRO returns the named container's readOnlyRootFilesystem (nil when unset).
	rootRO := func(dep *appsv1.Deployment, ctr string) *bool {
		require.NotNilf(t, dep, "Deployment must exist")
		for _, c := range append(append([]corev1.Container{}, dep.Spec.Template.Spec.InitContainers...), dep.Spec.Template.Spec.Containers...) {
			if c.Name == ctr {
				if c.SecurityContext == nil {
					return nil
				}
				return c.SecurityContext.ReadOnlyRootFilesystem
			}
		}
		t.Fatalf("container %q not found in Deployment %q", ctr, dep.Name)
		return nil
	}
	enabled := func(dep, ctr string) {
		ro := rootRO(deps[dep], ctr)
		require.NotNilf(t, ro, "%s/%s: readOnlyRootFilesystem must be set (ENABLED)", dep, ctr)
		assert.Truef(t, *ro, "%s/%s: readOnlyRootFilesystem must be true (ENABLED)", dep, ctr)
	}

	// ENABLED (init containers + sidecars) — every writable path is backed by a volume.
	enabled(testOPAPolicies, testOPAPolicies) // nginx: /var/cache/nginx + /tmp
	enabled(testFeAdmin, testFeAdmin)         // static nginx: cache + /tmp
	enabled("api-gateway", "api-gateway")     // Kong: KONG_PREFIX=/tmp
	enabled("core", "auth-opa")               // OPA sidecar: in-memory bundle
	enabled("core", "wait-for-auth")          // init: nc poll loop
	enabled("scheduler", "wait-for-messaging-service")

	// ENABLED (JVM / .NET main containers) — their only writable path is the in-memory
	// /tmp ephemeral volume (auth additionally gets TMPDIR=/tmp for the .NET
	// runtime). Validated end-to-end against the live images on Kind.
	enabled("core", "core")
	enabled("scheduler", "scheduler")
	enabled("utils", "utils")
	enabled("auth", "auth") // auth's container is named "auth"
}

// allPlatformComponents is every component role RenderPlatformBase renders a
// Deployment for (with utils enabled), used to assert spec.additionalEnv
// reaches EVERY component uniformly.
var allPlatformComponents = []string{
	"core", "scheduler", testOPAPolicies, "auth",
	testFeAdmin, "utils", gatewayName,
}

// mainContainerEnvValue returns the value of env var `name` on the named
// Deployment's MAIN container (index 0), and whether it is present as an inline value.
// mainContainer returns the deployment's MAIN application container — the one whose name matches
// the deployment (component) name. Core renders its OPA sidecar BEFORE the main container
// (Component.SidecarsFirst, to avoid the postStart deadlock), so the main container is NOT
// necessarily Containers[0]; find it by name.
func mainContainer(d *appsv1.Deployment) corev1.Container {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == d.Name {
			return c
		}
	}
	return d.Spec.Template.Spec.Containers[0]
}

func mainContainerEnvValue(deps map[string]*appsv1.Deployment, dep, name string) (string, bool) {
	d, ok := deps[dep]
	if !ok {
		return "", false
	}
	for _, e := range mainContainer(d).Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestRenderPlatformAdditionalEnvOnEveryComponent asserts that spec.additionalEnv
// (platform-wide extra env, e.g. OTEL_SDK_DISABLED) is applied to the main container
// of EVERY platform component.
func TestRenderPlatformAdditionalEnvOnEveryComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true // so all seven components render
	p.Spec.AdditionalEnv = []otilmv1alpha1.EnvVar{
		{Name: "OTEL_SDK_DISABLED", Value: "false"},
		{Name: "GLOBAL_EXTRA", Value: "g"},
	}

	deps := map[string]*appsv1.Deployment{}
	for _, o := range RenderPlatform(p) {
		if dep, ok := o.(*appsv1.Deployment); ok {
			deps[dep.Name] = dep
		}
	}

	for _, comp := range allPlatformComponents {
		otel, ok := mainContainerEnvValue(deps, comp, "OTEL_SDK_DISABLED")
		require.Truef(t, ok, "%s must carry the global OTEL_SDK_DISABLED env", comp)
		assert.Equalf(t, "false", otel, "%s OTEL_SDK_DISABLED value", comp)
		extra, ok := mainContainerEnvValue(deps, comp, "GLOBAL_EXTRA")
		require.Truef(t, ok, "%s must carry the global GLOBAL_EXTRA env", comp)
		assert.Equalf(t, "g", extra, "%s GLOBAL_EXTRA value", comp)
	}
}

// TestRenderPlatformAdditionalEnvOmittedWhenUnset asserts that when spec.additionalEnv
// is empty no extra env is injected (the global passthrough is a no-op): a sentinel
// name never appears on any component.
func TestRenderPlatformAdditionalEnvOmittedWhenUnset(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	deps := map[string]*appsv1.Deployment{}
	for _, o := range RenderPlatform(p) {
		if dep, ok := o.(*appsv1.Deployment); ok {
			deps[dep.Name] = dep
		}
	}
	for _, comp := range allPlatformComponents {
		_, ok := mainContainerEnvValue(deps, comp, "OTEL_SDK_DISABLED")
		assert.Falsef(t, ok, "%s must NOT carry OTEL_SDK_DISABLED when additionalEnv is unset", comp)
	}
}

// TestRenderPlatformComponentEnvOverridesGlobal asserts the documented precedence:
// the global additionalEnv is merged BEFORE each component's own/derived env, so a
// component-specific variable of the SAME name overrides the global one (Kubernetes
// container-env last-wins). Core sets HEADER_ENABLED="true"; a global HEADER_ENABLED
// ="global-loses" must be shadowed by Core's own value on the resolved container.
func TestRenderPlatformComponentEnvOverridesGlobal(t *testing.T) {
	// JAVA_OPTS is plain per-component env (the operator wires no JAVA_OPTS of its own).
	const javaOptsEnv = "JAVA_OPTS"
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.AdditionalEnv = []otilmv1alpha1.EnvVar{
		// Collides with Core's own HEADER_ENABLED (always "true") and with a per-component
		// JAVA_OPTS set on core.env — both must win over the fleet-wide global env.
		{Name: w.HeaderEnabledEnv, Value: "global-loses"},
		{Name: javaOptsEnv, Value: "-Dglobal=loses"},
		// A non-colliding global var still passes through.
		{Name: "OTEL_SDK_DISABLED", Value: "false"},
	}
	// A core.env JAVA_OPTS must still override the fleet-wide one.
	p.Spec.Core.Env = []otilmv1alpha1.EnvVar{{Name: javaOptsEnv, Value: "-Xmx512m"}}

	var core *appsv1.Deployment
	for _, o := range RenderPlatform(p) {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == "core" {
			core = dep
		}
	}
	require.NotNil(t, core)

	env := mainContainer(core).Env
	// effectiveValue returns the LAST value bound to name (k8s container-env semantics:
	// the last duplicate wins), proving the component-specific value overrides the global.
	effectiveValue := func(name string) (string, bool) {
		val, ok := "", false
		for _, e := range env {
			if e.Name == name {
				val, ok = e.Value, true
			}
		}
		return val, ok
	}

	hdr, ok := effectiveValue(w.HeaderEnabledEnv)
	require.True(t, ok)
	assert.Equal(t, "true", hdr, "Core's own HEADER_ENABLED must override the global one (global merged first)")

	jopts, ok := effectiveValue(javaOptsEnv)
	require.True(t, ok)
	assert.Equal(t, "-Xmx512m", jopts, "Core's own JAVA_OPTS must override the global one")

	// The non-colliding global var still lands on Core.
	otel, ok := effectiveValue("OTEL_SDK_DISABLED")
	require.True(t, ok)
	assert.Equal(t, "false", otel)
}

func TestRenderPlatformUtilsEnabled(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	objs := RenderPlatform(p)

	type objKey struct{ kind, name string }
	got := map[objKey]client.Object{}
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.ServiceAccount:
			got[objKey{"ServiceAccount", v.Name}] = o
		case *appsv1.Deployment:
			got[objKey{"Deployment", v.Name}] = o
		case *corev1.Service:
			got[objKey{"Service", v.Name}] = o
		}
	}

	for _, want := range []objKey{
		{"ServiceAccount", "utils"},
		{"Deployment", "utils"},
		{"Service", "utils"},
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("RenderPlatform missing %s/%s when utils enabled", want.kind, want.name)
		}
	}
}

// renderedDeployments renders the platform (utils enabled) and returns its Deployments
// keyed by name, the shared setup for the spec.global passthrough tests.
func renderedDeployments(p *otilmv1alpha1.Platform) map[string]*appsv1.Deployment {
	deps := map[string]*appsv1.Deployment{}
	for _, o := range RenderPlatform(p) {
		if dep, ok := o.(*appsv1.Deployment); ok {
			deps[dep.Name] = dep
		}
	}
	return deps
}

// findContainer returns the named container from a Deployment's pod template (searching
// init containers then regular containers), and whether it was found.
func findContainer(dep *appsv1.Deployment, name string) (corev1.Container, bool) {
	ps := dep.Spec.Template.Spec
	for _, c := range append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...) {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

// TestRenderPlatformGlobalContainersOnEveryComponent asserts that a global init container
// AND a global sidecar (spec.global.initContainers / spec.global.sidecars) appear on the
// pod of EVERY platform component, and that both are SCC-hardened (fill-AND-force) even
// though the CR set the most hostile possible security posture (Privileged:true,
// RunAsNonRoot:false, capabilities.Add, no seccomp). It is the executable proof that the
// fleet-wide customization passthrough cannot weaken OpenShift restricted-v2.
func TestRenderPlatformGlobalContainersOnEveryComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true // so all seven components render
	truthy := true
	p.Spec.Common.InitContainers = []corev1.Container{{
		Name:  "global-init",
		Image: "ca-injector:latest",
		// Hostile posture the hardening MUST override:
		SecurityContext: &corev1.SecurityContext{
			Privileged:   &truthy,
			RunAsNonRoot: &[]bool{false}[0],
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
		},
	}}
	p.Spec.Common.Sidecars = []corev1.Container{{
		Name:  "vault-agent",
		Image: "vault:latest",
		SecurityContext: &corev1.SecurityContext{
			Privileged: &truthy, // hostile: must be forced to false
		},
	}}

	deps := renderedDeployments(p)

	assertHardened := func(t *testing.T, comp, ctr string) {
		t.Helper()
		dep := deps[comp]
		require.NotNilf(t, dep, testDeploymentMustRender, comp)
		c, ok := findContainer(dep, ctr)
		require.Truef(t, ok, "%s must carry the global container %q", comp, ctr)
		sc := c.SecurityContext
		require.NotNilf(t, sc, "%s/%s must have a SecurityContext", comp, ctr)
		require.NotNil(t, sc.RunAsNonRoot)
		assert.Truef(t, *sc.RunAsNonRoot, "%s/%s: RunAsNonRoot must be forced true", comp, ctr)
		require.NotNil(t, sc.Privileged)
		assert.Falsef(t, *sc.Privileged, "%s/%s: Privileged must be forced false", comp, ctr)
		require.NotNil(t, sc.AllowPrivilegeEscalation)
		assert.Falsef(t, *sc.AllowPrivilegeEscalation, "%s/%s: privilege escalation must be forced off", comp, ctr)
		require.NotNil(t, sc.Capabilities)
		assert.Equalf(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "%s/%s must drop ALL caps", comp, ctr)
		assert.Emptyf(t, sc.Capabilities.Add, "%s/%s: caller-added caps must be stripped", comp, ctr)
		require.NotNil(t, sc.SeccompProfile)
		assert.Equalf(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "%s/%s seccomp must be RuntimeDefault", comp, ctr)
	}

	// The global init container + sidecar appear on, and are hardened in, EVERY component
	// (explicitly including core + scheduler + the gateway).
	for _, comp := range allPlatformComponents {
		assertHardened(t, comp, "global-init")
		assertHardened(t, comp, "vault-agent")
	}
}

// TestRenderPlatformGlobalVolumesPortsEnvFromOnEveryComponent asserts that the global
// volumes / volumeMounts / additionalPorts / additionalEnvFrom (spec.global.*) land on
// EVERY component's pod and main container.
func TestRenderPlatformGlobalVolumesPortsEnvFromOnEveryComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	p.Spec.Common.Volumes = []corev1.Volume{{
		Name:         testGlobalVol,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	p.Spec.Common.VolumeMounts = []corev1.VolumeMount{{Name: testGlobalVol, MountPath: "/global"}}
	p.Spec.Common.AdditionalPorts = []corev1.ContainerPort{{
		Name: "metrics-extra", ContainerPort: 9999, Protocol: corev1.ProtocolTCP,
	}}
	p.Spec.Common.AdditionalEnvFrom = &otilmv1alpha1.EnvFromSources{
		Secrets:    []string{"global-secret"},
		ConfigMaps: []string{"global-config"},
	}

	deps := renderedDeployments(p)

	for _, comp := range allPlatformComponents {
		dep := deps[comp]
		require.NotNilf(t, dep, testDeploymentMustRender, comp)
		main := mainContainer(dep)

		assertGlobalPodVolume(t, comp, dep)
		assertGlobalVolumeMount(t, comp, main)
		assertGlobalAdditionalPort(t, comp, main)
		assertGlobalEnvFrom(t, comp, main)
	}
}

// assertGlobalPodVolume asserts the global pod-level volume (spec.common.volumes) lands on
// the component's pod.
func assertGlobalPodVolume(t *testing.T, comp string, dep *appsv1.Deployment) {
	t.Helper()
	hasVol := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == testGlobalVol {
			hasVol = true
		}
	}
	assert.Truef(t, hasVol, "%s must carry the global pod volume", comp)
}

// assertGlobalVolumeMount asserts the global volumeMount (spec.common.volumeMounts) lands on
// the component's main container.
func assertGlobalVolumeMount(t *testing.T, comp string, main corev1.Container) {
	t.Helper()
	hasMount := false
	for _, m := range main.VolumeMounts {
		if m.Name == testGlobalVol && m.MountPath == "/global" {
			hasMount = true
		}
	}
	assert.Truef(t, hasMount, "%s main container must carry the global volumeMount", comp)
}

// assertGlobalAdditionalPort asserts the global additionalPort (spec.common.additionalPorts)
// lands on the component's main container.
func assertGlobalAdditionalPort(t *testing.T, comp string, main corev1.Container) {
	t.Helper()
	hasPort := false
	for _, port := range main.Ports {
		if port.Name == "metrics-extra" && port.ContainerPort == 9999 {
			hasPort = true
		}
	}
	assert.Truef(t, hasPort, "%s main container must carry the global additionalPort", comp)
}

// assertGlobalEnvFrom asserts the global envFrom secretRef + configMapRef
// (spec.common.additionalEnvFrom) land on the component's main container.
func assertGlobalEnvFrom(t *testing.T, comp string, main corev1.Container) {
	t.Helper()
	hasSecretFrom, hasConfigFrom := false, false
	for _, ef := range main.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == "global-secret" {
			hasSecretFrom = true
		}
		if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == "global-config" {
			hasConfigFrom = true
		}
	}
	assert.Truef(t, hasSecretFrom, "%s must carry the global envFrom.secretRef", comp)
	assert.Truef(t, hasConfigFrom, "%s must carry the global envFrom.configMapRef", comp)
}

// TestRenderPlatformGlobalAndPerComponentSidecarCoexist asserts that a per-component
// sidecar (set via the component's own ComponentSpec) coexists with a global sidecar
// (spec.global.sidecars) on that component — the global one layers underneath, the
// per-component one is also present, and both are hardened.
func TestRenderPlatformGlobalAndPerComponentSidecarCoexist(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.Sidecars = []corev1.Container{{Name: testGlobalSidecar, Image: "global:latest"}}
	// Core gets its OWN extra sidecar via its per-component ComponentSpec.
	p.Spec.Core.Sidecars = []corev1.Container{{Name: "core-only-sidecar", Image: "core:latest"}}

	deps := renderedDeployments(p)
	core := deps["core"]
	require.NotNil(t, core)

	names := map[string]bool{}
	for _, c := range core.Spec.Template.Spec.Containers {
		names[c.Name] = true
		// Every container (incl. both injected sidecars) is SCC-hardened.
		require.NotNilf(t, c.SecurityContext, "core container %q must be hardened", c.Name)
		require.NotNil(t, c.SecurityContext.RunAsNonRoot)
		assert.Truef(t, *c.SecurityContext.RunAsNonRoot, "core container %q must run non-root", c.Name)
	}
	assert.Truef(t, names[testGlobalSidecar], "the global sidecar must be present on core")
	assert.Truef(t, names["core-only-sidecar"], "the per-component core sidecar must coexist")
	// The component's own pre-existing OPA sidecar is untouched too.
	assert.Truef(t, names["auth-opa"], "core's operator-derived OPA sidecar must be preserved")
}

// TestRenderPlatformGlobalOmittedWhenEmpty asserts that an unset spec.common passthrough
// is a no-op: no sentinel global container/volume/port/envFrom appears on any component,
// so the out-of-the-box render is unchanged (additive, non-breaking).
func TestRenderPlatformGlobalOmittedWhenEmpty(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	// basePlatform sets only spec.common.image; the fleet-wide passthrough is empty.
	require.Empty(t, p.Spec.Common.InitContainers)
	require.Empty(t, p.Spec.Common.Sidecars)
	require.Empty(t, p.Spec.Common.Volumes)

	deps := renderedDeployments(p)
	for _, comp := range allPlatformComponents {
		dep := deps[comp]
		require.NotNilf(t, dep, testDeploymentMustRender, comp)
		_, ok := findContainer(dep, testGlobalSidecar)
		assert.Falsef(t, ok, "%s must not carry a global container when spec.common passthrough is empty", comp)
		for _, v := range dep.Spec.Template.Spec.Volumes {
			assert.NotEqualf(t, testGlobalVol, v.Name, "%s must not carry a global volume when spec.common passthrough is empty", comp)
		}
	}
}
