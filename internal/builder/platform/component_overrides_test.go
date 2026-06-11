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
	"github.com/OmniTrustILM/operator/internal/bom"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// i32Ptr / boolPtr are small pointer helpers for the override fixtures (strPtr is
// shared from edge_test.go in this package).
func i32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool  { return &b }

// envVarByName returns the rendered container EnvVar with the given name, and whether
// it was found (the LAST match wins, mirroring Kubernetes container-env semantics).
func envVarByName(env []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	var out corev1.EnvVar
	found := false
	for _, e := range env {
		if e.Name == name {
			out = e
			found = true
		}
	}
	return out, found
}

// volumeByName / mountByName look up a rendered pod volume / container mount by name.
func volumeByName(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func mountByName(mounts []corev1.VolumeMount, name string) (corev1.VolumeMount, bool) {
	for _, m := range mounts {
		if m.Name == name {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

// ---------------------------------------------------------------------------
// Resources / replicas / env (append + override)
// ---------------------------------------------------------------------------

func TestComponentSpecResourcesOverride(t *testing.T) {
	p := basePlatform()
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	p.Spec.Core.Resources = &want
	c := ResolveCore(p)
	assert.Equal(t, want, c.Resources)
}

func TestComponentSpecReplicasOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.Replicas = i32Ptr(4)
	c := ResolveScheduler(p)
	assert.Equal(t, int32(4), c.Replicas)
}

func TestComponentSpecEnvAppendedLastWins(t *testing.T) {
	p := basePlatform()
	// LOG_LEVEL-style override on scheduler: the operator sets LOGGING_LEVEL_COM_CZERTAINLY
	// already, and a NEW var plus an OVERRIDE of an existing one both apply, override last.
	p.Spec.Scheduler.Env = []otilmv1alpha1.EnvVar{
		{Name: "EXTRA_FLAG", Value: "on"},
		{Name: "PORT", Value: "9090"}, // overrides the operator-derived PORT=8080
	}
	c := ResolveScheduler(p)

	// New var present.
	v, ok := envValue(c.Env, "EXTRA_FLAG")
	require.True(t, ok)
	assert.Equal(t, "on", v)

	// The operator-derived PORT and the override PORT are both in the slice; the LAST wins.
	// Render through BuildDeployment to assert the realized container env (last-wins).
	dep := common.BuildDeployment(c)
	got, found := envVarByName(dep.Spec.Template.Spec.Containers[0].Env, "PORT")
	require.True(t, found)
	assert.Equal(t, "9090", got.Value, "user PORT override must win (appended last)")
}

// ---------------------------------------------------------------------------
// Secret / ConfigMap refs with key mapping → secretKeyRef / configMapKeyRef
// ---------------------------------------------------------------------------

func TestComponentSpecSecretRefKeyMapping(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.SecretRefs = []otilmv1alpha1.SecretRef{
		{
			Name: "extra-secret",
			Type: otilmv1alpha1.RefTypeEnv,
			Keys: []otilmv1alpha1.RefKeyMapping{
				{SecretKey: "api-token", EnvVar: strPtr("API_TOKEN")},
			},
		},
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	got, found := envVarByName(mainContainer(dep).Env, "API_TOKEN")
	require.True(t, found, "the mapped env var name must appear")
	require.NotNil(t, got.ValueFrom)
	require.NotNil(t, got.ValueFrom.SecretKeyRef, "must be a secretKeyRef, never an inline value")
	assert.Equal(t, "extra-secret", got.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "api-token", got.ValueFrom.SecretKeyRef.Key)
	assert.Empty(t, got.Value, "the value must never be inlined")
}

func TestComponentSpecSecretRefWholeEnvFrom(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.SecretRefs = []otilmv1alpha1.SecretRef{
		{Name: "bulk-secret", Type: otilmv1alpha1.RefTypeEnv}, // no keys => envFrom whole secret
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	envFrom := mainContainer(dep).EnvFrom
	require.Len(t, envFrom, 1)
	require.NotNil(t, envFrom[0].SecretRef)
	assert.Equal(t, "bulk-secret", envFrom[0].SecretRef.Name)
}

func TestComponentSpecConfigMapRefKeyMapping(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
		{
			Name: "tuning",
			Type: otilmv1alpha1.RefTypeEnv,
			Keys: []otilmv1alpha1.ConfigMapKeyMapping{
				{ConfigMapKey: "threads", EnvVar: strPtr("WORKER_THREADS")},
			},
		},
	}
	c := ResolveScheduler(p)
	dep := common.BuildDeployment(c)
	got, found := envVarByName(dep.Spec.Template.Spec.Containers[0].Env, "WORKER_THREADS")
	require.True(t, found)
	require.NotNil(t, got.ValueFrom)
	require.NotNil(t, got.ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, "tuning", got.ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "threads", got.ValueFrom.ConfigMapKeyRef.Key)
}

func TestComponentSpecSecretRefVolumeMount(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.SecretRefs = []otilmv1alpha1.SecretRef{
		{
			Name:      "tls-secret",
			Type:      otilmv1alpha1.RefTypeVolume,
			MountPath: strPtr("/etc/extra-tls"),
			Keys: []otilmv1alpha1.RefKeyMapping{
				{SecretKey: "tls.crt", Path: strPtr("server.crt")},
			},
		},
	}
	c := ResolveCore(p)
	vol, ok := volumeByName(c.Volumes, "secret-tls-secret")
	require.True(t, ok, "a type=volume secretRef must add a pod volume")
	require.NotNil(t, vol.Secret)
	assert.Equal(t, "tls-secret", vol.Secret.SecretName)
	require.Len(t, vol.Secret.Items, 1)
	assert.Equal(t, "tls.crt", vol.Secret.Items[0].Key)
	assert.Equal(t, "server.crt", vol.Secret.Items[0].Path)

	mount, ok := mountByName(c.VolumeMounts, "secret-tls-secret")
	require.True(t, ok)
	assert.Equal(t, "/etc/extra-tls", mount.MountPath)
	assert.True(t, mount.ReadOnly, "a mounted secret must be read-only")
}

// ---------------------------------------------------------------------------
// Volumes / probes / pod metadata
// ---------------------------------------------------------------------------

func TestComponentSpecVolumeOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.Volumes = []otilmv1alpha1.VolumeSpec{
		{Name: "scratch", MountPath: "/scratch", EmptyDir: &otilmv1alpha1.EmptyDirSpec{SizeLimit: strPtr("64Mi")}},
	}
	c := ResolveScheduler(p)
	vol, ok := volumeByName(c.Volumes, "scratch")
	require.True(t, ok)
	require.NotNil(t, vol.EmptyDir)
	require.NotNil(t, vol.EmptyDir.SizeLimit)
	assert.Equal(t, resource.MustParse("64Mi"), *vol.EmptyDir.SizeLimit)
	mount, ok := mountByName(c.VolumeMounts, "scratch")
	require.True(t, ok)
	assert.Equal(t, "/scratch", mount.MountPath)
}

func TestComponentSpecProbesOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.Probes = &otilmv1alpha1.ProbeSpec{
		Readiness: &otilmv1alpha1.ProbeConfig{Path: "/custom/ready", InitialDelaySeconds: 7, PeriodSeconds: 11, FailureThreshold: 2},
	}
	c := ResolveScheduler(p)
	require.NotNil(t, c.Probes.Readiness)
	require.NotNil(t, c.Probes.Readiness.HTTPGet)
	assert.Equal(t, "/custom/ready", c.Probes.Readiness.HTTPGet.Path)
	assert.Equal(t, int32(7), c.Probes.Readiness.InitialDelaySeconds)
	assert.Equal(t, 8080, c.Probes.Readiness.HTTPGet.Port.IntValue(), "probe targets the component service port")
}

func TestComponentSpecPodAnnotationsAndLabels(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.PodAnnotations = map[string]string{"vault.hashicorp.com/agent-inject": "true"}
	p.Spec.Core.PodLabels = map[string]string{"team": "platform"}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	tmpl := dep.Spec.Template
	assert.Equal(t, "true", tmpl.Annotations["vault.hashicorp.com/agent-inject"])
	assert.Equal(t, "platform", tmpl.Labels["team"])
	// Operator-managed labels still present and win as the immutable selector.
	assert.Equal(t, "core", tmpl.Labels[common.NameLabel])
}

func TestComponentSpecPodLabelsCannotOverrideSelector(t *testing.T) {
	p := basePlatform()
	// A malicious/clumsy attempt to override the immutable selector label must NOT win.
	p.Spec.Core.PodLabels = map[string]string{common.NameLabel: "evil"}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	assert.Equal(t, "core", dep.Spec.Template.Labels[common.NameLabel], "operator selector label wins")
}

// ---------------------------------------------------------------------------
// Scheduling: nodeSelector / affinity / tolerations
// ---------------------------------------------------------------------------

func TestComponentSpecSchedulingPassthrough(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.NodeSelector = map[string]string{"disktype": "ssd"}
	p.Spec.Core.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "ilm", Effect: corev1.TaintEffectNoSchedule}}
	p.Spec.Core.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
					}},
				}},
			},
		},
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	ps := dep.Spec.Template.Spec
	assert.Equal(t, "ssd", ps.NodeSelector["disktype"])
	require.Len(t, ps.Tolerations, 1)
	assert.Equal(t, "dedicated", ps.Tolerations[0].Key)
	require.NotNil(t, ps.Affinity)
	require.NotNil(t, ps.Affinity.NodeAffinity)
}

// ---------------------------------------------------------------------------
// Fleet-wide scheduling + pod metadata (spec.common) — fan-out + per-component precedence
// ---------------------------------------------------------------------------

// nodeAffinityFor builds a minimal nodeAffinity whose single match value is the given marker,
// so a test can tell which source (common vs component) won.
func nodeAffinityFor(value string) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "marker", Operator: corev1.NodeSelectorOpIn, Values: []string{value},
				}},
			}},
		},
	}}
}

func TestCommonSchedulingFansOutToComponents(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.NodeSelector = map[string]string{"pool": "platform"}
	p.Spec.Common.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "ilm", Effect: corev1.TaintEffectNoSchedule}}
	p.Spec.Common.Affinity = nodeAffinityFor("common")
	// Components that set none of their own inherit the fleet-wide scheduling.
	for name, c := range map[string]common.Component{"core": ResolveCore(p), "scheduler": ResolveScheduler(p)} {
		ps := common.BuildDeployment(c).Spec.Template.Spec
		assert.Equal(t, "platform", ps.NodeSelector["pool"], "%s inherits common.nodeSelector", name)
		require.Len(t, ps.Tolerations, 1, "%s inherits common.tolerations", name)
		assert.Equal(t, "dedicated", ps.Tolerations[0].Key, "%s", name)
		require.NotNil(t, ps.Affinity, "%s inherits common.affinity", name)
		require.NotNil(t, ps.Affinity.NodeAffinity, "%s", name)
	}
}

func TestCommonPodMetadataFansOut(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.PodAnnotations = map[string]string{"sidecar.istio.io/inject": "true"}
	p.Spec.Common.PodLabels = map[string]string{"cost-center": "pki"}
	tmpl := common.BuildDeployment(ResolveCore(p)).Spec.Template
	assert.Equal(t, "true", tmpl.Annotations["sidecar.istio.io/inject"])
	assert.Equal(t, "pki", tmpl.Labels["cost-center"])
	assert.Equal(t, "core", tmpl.Labels[common.NameLabel], "operator selector label still wins")
}

func TestComponentSchedulingOverridesAndAugmentsCommon(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.NodeSelector = map[string]string{"pool": "platform", "tier": "base"}
	p.Spec.Common.PodAnnotations = map[string]string{"team": "infra", "common-only": "yes"}
	p.Spec.Common.Tolerations = []corev1.Toleration{{Key: "common", Operator: corev1.TolerationOpExists}}
	// Core overrides the colliding keys and appends its own toleration.
	p.Spec.Core.NodeSelector = map[string]string{"tier": "core"}
	p.Spec.Core.PodAnnotations = map[string]string{"team": "core"}
	p.Spec.Core.Tolerations = []corev1.Toleration{{Key: "core", Operator: corev1.TolerationOpExists}}
	tmpl := common.BuildDeployment(ResolveCore(p)).Spec.Template
	// nodeSelector: merged; component key wins on collision, common-only key survives.
	assert.Equal(t, "platform", tmpl.Spec.NodeSelector["pool"], "common-only nodeSelector key survives")
	assert.Equal(t, "core", tmpl.Spec.NodeSelector["tier"], "component nodeSelector key wins on collision")
	// podAnnotations: merged; component key wins, common-only key survives.
	assert.Equal(t, "yes", tmpl.Annotations["common-only"])
	assert.Equal(t, "core", tmpl.Annotations["team"], "component annotation wins on collision")
	// tolerations: union of common + component.
	require.Len(t, tmpl.Spec.Tolerations, 2, "common + component tolerations both apply")
}

func TestComponentAffinityReplacesCommon(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.Affinity = nodeAffinityFor("common")
	p.Spec.Core.Affinity = nodeAffinityFor("component")
	ps := common.BuildDeployment(ResolveCore(p)).Spec.Template.Spec
	require.NotNil(t, ps.Affinity)
	got := ps.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values
	assert.Equal(t, []string{"component"}, got, "component affinity replaces common (not merged)")
}

func TestCommonAffinityBeatsHADefault(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	p.Spec.Common.Affinity = nodeAffinityFor("common")
	// HA on + no per-component affinity → common.affinity wins over the HA anti-affinity default.
	ps := common.BuildDeployment(ResolveCore(p)).Spec.Template.Spec
	require.NotNil(t, ps.Affinity)
	require.NotNil(t, ps.Affinity.NodeAffinity, "common nodeAffinity applied")
	assert.Nil(t, ps.Affinity.PodAntiAffinity, "the HA anti-affinity default is overridden by common.affinity")
}

// ---------------------------------------------------------------------------
// ServiceAccount name + annotations
// ---------------------------------------------------------------------------

func TestComponentSpecServiceAccountOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.ServiceAccount = &otilmv1alpha1.ServiceAccountSpec{
		Name:        strPtr(testCoreIRSA),
		Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123:role/core"},
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	assert.Equal(t, testCoreIRSA, dep.Spec.Template.Spec.ServiceAccountName, "pod must reference the overridden SA")

	sa := common.BuildServiceAccount(c)
	assert.Equal(t, testCoreIRSA, sa.Name, "the rendered SA is named after the override")
	assert.Equal(t, "arn:aws:iam::123:role/core", sa.Annotations["eks.amazonaws.com/role-arn"])
}

func TestComponentSpecServiceAccountDefaultsToComponentName(t *testing.T) {
	c := ResolveCore(basePlatform())
	sa := common.BuildServiceAccount(c)
	assert.Equal(t, "core", sa.Name)
	assert.Nil(t, sa.Annotations)
}

// ---------------------------------------------------------------------------
// Service port/type override
// ---------------------------------------------------------------------------

func TestComponentSpecServiceOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.Service = &otilmv1alpha1.ServiceSpec{Port: 9000, Type: "NodePort"}
	c := ResolveScheduler(p)
	assert.Equal(t, int32(9000), c.Port)
	assert.Equal(t, corev1.ServiceTypeNodePort, c.ServiceType)
	svc := common.BuildService(c)
	assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)
	assert.Equal(t, int32(9000), svc.Spec.Ports[0].Port)
}

// ---------------------------------------------------------------------------
// Image: digest (name@digest), command/args
// ---------------------------------------------------------------------------

func TestComponentSpecImageDigest(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.Image = otilmv1alpha1.ImageSpec{
		Digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	c := ResolveCore(p)
	// registry/repository from shared, name "core" from the bundle, pinned by digest (tag ignored).
	assert.Equal(t,
		"hub.omnitrustregistry.com/ilm/core@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		c.Image)
}

func TestComponentSpecImageDigestTakesPrecedenceOverTag(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.Image = otilmv1alpha1.ImageSpec{Tag: "1.2.3", Digest: "sha256:abc"}
	c := ResolveCore(p)
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core@sha256:abc", c.Image, "digest wins over tag")
}

func TestComponentSpecImageCommandArgs(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.Image = otilmv1alpha1.ImageSpec{
		Command: []string{testCustomEntry},
		Args:    []string{testFlagArg, "value"},
	}
	c := ResolveScheduler(p)
	assert.Equal(t, []string{testCustomEntry}, c.Command)
	assert.Equal(t, []string{testFlagArg, "value"}, c.Args)
	dep := common.BuildDeployment(c)
	assert.Equal(t, []string{testCustomEntry}, dep.Spec.Template.Spec.Containers[0].Command)
	assert.Equal(t, []string{testFlagArg, "value"}, dep.Spec.Template.Spec.Containers[0].Args)
}

func TestComponentSpecImageCommandPerComponentWinsOverShared(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.Image.Command = []string{"/shared"}
	p.Spec.Core.Image = otilmv1alpha1.ImageSpec{Command: []string{"/core-specific"}}
	c := ResolveCore(p)
	assert.Equal(t, []string{"/core-specific"}, c.Command)
}

// ---------------------------------------------------------------------------
// SCC hardening: a user-supplied securityContext / sidecar / init container is
// STILL hardened (drop-ALL / runAsNonRoot / seccomp) — SCC cannot be weakened.
// ---------------------------------------------------------------------------

func TestComponentSpecUserSecurityContextCannotWeakenSCC(t *testing.T) {
	p := basePlatform()
	// A user tries to weaken pod security: run as root, allow privilege escalation,
	// keep all capabilities. The fill-don't-replace hardening must override the unsafe
	// fields it owns (the four SCC-critical fields are guaranteed present and safe).
	p.Spec.Core.SecurityContext = &otilmv1alpha1.SecurityContextSpec{
		RunAsNonRoot: boolPtr(false),
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	// hardenContainer fills the four SCC fields; a nil RunAsNonRoot would be filled true,
	// but the user set it false — assert it is forced back to a hardened value.
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot, "RunAsNonRoot must stay true even when the CR sets false")
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

func TestComponentSpecUserSidecarIsHardened(t *testing.T) {
	p := basePlatform()
	// A user-supplied sidecar with NO security context (and another with a deliberately
	// unsafe partial one): BuildDeployment must harden BOTH.
	p.Spec.Core.Sidecars = []corev1.Container{
		{Name: "user-metrics", Image: "metrics:latest"},
		{Name: "user-unsafe", Image: "x:latest", SecurityContext: &corev1.SecurityContext{
			Privileged: boolPtr(true), // user attempt; the four SCC fields are still filled around it
		}},
	}
	c := ResolveCore(p)
	dep := common.BuildDeployment(c)

	metrics, ok := containerByName(dep.Spec.Template.Spec.Containers, "user-metrics")
	require.True(t, ok, "user sidecar must be appended to the pod")
	requireHardened(t, metrics.SecurityContext)

	unsafe, ok := containerByName(dep.Spec.Template.Spec.Containers, "user-unsafe")
	require.True(t, ok)
	requireHardened(t, unsafe.SecurityContext)
}

func TestComponentSpecUserInitContainerIsHardened(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler.InitContainers = []corev1.Container{
		{Name: "user-migrate", Image: "migrate:latest"},
	}
	c := ResolveScheduler(p)
	dep := common.BuildDeployment(c)
	mig, ok := containerByName(dep.Spec.Template.Spec.InitContainers, "user-migrate")
	require.True(t, ok, "user init container must be appended")
	requireHardened(t, mig.SecurityContext)
	// The operator's own init container (wait-for-messaging-service) is still present.
	_, ok = containerByName(dep.Spec.Template.Spec.InitContainers, "wait-for-messaging-service")
	assert.True(t, ok, "operator init containers are preserved alongside user ones")
}

// requireHardened asserts the four SCC-critical fields are present and safe on a
// container security context.
func requireHardened(t *testing.T, sc *corev1.SecurityContext) {
	t.Helper()
	require.NotNil(t, sc, "every container must carry a security context")
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot, "runAsNonRoot must be true")
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation, "privilege escalation must be disabled")
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "all capabilities must be dropped")
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

// ---------------------------------------------------------------------------
// Defaults unchanged when the spec is empty (the floor: out-of-the-box render).
// ---------------------------------------------------------------------------

func TestComponentSpecEmptyLeavesDefaultsIntact(t *testing.T) {
	p := basePlatform()
	c := ResolveCore(p)
	// No overrides => the operator's defaults stand.
	assert.Equal(t, int32(1), c.Replicas)
	assert.Empty(t, c.ExtraEnv, "no user refs => no ExtraEnv")
	assert.Empty(t, c.EnvFrom, "no whole-secret refs => no EnvFrom")
	assert.Empty(t, c.NodeSelector)
	assert.Nil(t, c.Affinity)
	assert.Empty(t, c.Tolerations)
	assert.Empty(t, c.ServiceAccountName, "no SA override => empty (BuildServiceAccount uses the component name)")
	// Core's operator-derived OPA sidecar is still the only sidecar.
	require.Len(t, c.Sidecars, 1)
	assert.Equal(t, "auth-opa", c.Sidecars[0].Name)
}

// ---------------------------------------------------------------------------
// Per-component ServiceMonitor
// ---------------------------------------------------------------------------

func TestComponentSpecServiceMonitorRendered(t *testing.T) {
	p := basePlatform()
	interval := "30s"
	p.Spec.Core.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled: true,
		Path:    strPtr("/api/v1/metrics"),
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{
			Enabled:  true,
			Interval: &interval,
			Labels:   map[string]string{"release": "prometheus"},
		},
	}
	objs := ResolvePlatformServiceMonitors(p)
	require.Len(t, objs, 1, "exactly one ServiceMonitor for the one component that opted in")

	sm := objs[0]
	assert.Equal(t, "core", sm.GetName())
	assert.Equal(t, "ilm-system", sm.GetNamespace())
	assert.Equal(t, "prometheus", sm.GetLabels()["release"], "user serviceMonitor labels are merged on")

	// Dependency surfaces the Prometheus operator CRD requirement.
	deps := ServiceMonitorDependencies(p)
	require.Len(t, deps, 1)
	assert.Equal(t, "monitoring.coreos.com", deps[0].GroupKind.Group)
	assert.Equal(t, "ServiceMonitor", deps[0].GroupKind.Kind)
}

func TestComponentSpecServiceMonitorAbsentWhenDisabled(t *testing.T) {
	p := basePlatform()
	// Metrics enabled but ServiceMonitor not => no ServiceMonitor object, no dependency.
	p.Spec.Core.Metrics = &otilmv1alpha1.MetricsSpec{Enabled: true}
	assert.Empty(t, ResolvePlatformServiceMonitors(p))
	assert.Nil(t, ServiceMonitorDependencies(p))
}

func TestComponentSpecServiceMonitorUtilsGatedOnEnabled(t *testing.T) {
	p := basePlatform()
	// utils opts into a ServiceMonitor but is itself disabled => no SM rendered.
	p.Spec.Utils.Metrics = &otilmv1alpha1.MetricsSpec{
		Enabled:        true,
		ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
	}
	assert.Empty(t, ResolvePlatformServiceMonitors(p), "utils SM only when utils is enabled")

	p.Spec.Utils.Enabled = true
	objs := ResolvePlatformServiceMonitors(p)
	require.Len(t, objs, 1)
	assert.Equal(t, "utils", objs[0].GetName())
}

// ---------------------------------------------------------------------------
// A newly-enabled component (scheduler) gains the full override surface.
// ---------------------------------------------------------------------------

func TestSchedulerFullOverrideSurface(t *testing.T) {
	p := basePlatform()
	p.Spec.Scheduler = otilmv1alpha1.SchedulerSpec{
		ComponentSpec: otilmv1alpha1.ComponentSpec{
			Replicas:     i32Ptr(2),
			Env:          []otilmv1alpha1.EnvVar{{Name: "FEATURE_X", Value: "1"}},
			NodeSelector: map[string]string{"zone": "a"},
			PodLabels:    map[string]string{"tier": "backend"},
			ServiceAccount: &otilmv1alpha1.ServiceAccountSpec{
				Name: strPtr(testSchedSA),
			},
		},
	}
	c := ResolveScheduler(p)
	assert.Equal(t, int32(2), c.Replicas)
	v, ok := envValue(c.Env, "FEATURE_X")
	require.True(t, ok)
	assert.Equal(t, "1", v)
	assert.Equal(t, testSchedSA, c.SAName())

	dep := common.BuildDeployment(c)
	assert.Equal(t, "a", dep.Spec.Template.Spec.NodeSelector["zone"])
	assert.Equal(t, "backend", dep.Spec.Template.Labels["tier"])
	assert.Equal(t, testSchedSA, dep.Spec.Template.Spec.ServiceAccountName)
	// The operator-derived scheduler env/wiring is preserved (DB URL still present).
	_, ok = envValue(c.Env, bom.Wiring().DatabaseURLEnv)
	assert.True(t, ok, "operator-derived wiring is preserved under overrides")
}
