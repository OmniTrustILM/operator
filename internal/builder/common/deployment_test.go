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

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Shared literals used across the deployment builder tests.
const (
	testDBSecretName     = "ilm-db"
	testCoreConfigCMName = "core-config"
)

func TestBuildDeploymentRecreateStrategy(t *testing.T) {
	base := Component{Name: "core", Namespace: "ilm", Image: "img", Replicas: 1, Port: 8080}

	// Default: no explicit strategy (apps/v1 defaults to RollingUpdate).
	def := BuildDeployment(base)
	assert.Empty(t, string(def.Spec.Strategy.Type), "default leaves the strategy unset (RollingUpdate)")

	// Recreate: a DB-migrating component must never run two pods at once (the old pod is fully
	// terminated before the new one starts).
	base.Recreate = true
	rec := BuildDeployment(base)
	assert.Equal(t, "Recreate", string(rec.Spec.Strategy.Type),
		"Recreate so a roll never runs two schema-migrating pods concurrently")
}

func TestBuildDeployment(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm",
		Image: "hub.omnitrustregistry.com/ilm/core:2.18.0", PullPolicy: "IfNotPresent",
		Replicas: 1, Port: 8080,
		Env: []EnvPair{{Name: "PORT", Value: "8080"}},
	}
	d := BuildDeployment(c)
	require.NotNil(t, d)
	assert.Equal(t, "core", d.Name)
	assert.Equal(t, "ilm", d.Namespace)
	require.Len(t, d.Spec.Template.Spec.Containers, 1)
	ctr := d.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.18.0", ctr.Image)
	assert.Equal(t, int32(1), *d.Spec.Replicas)
	// SCC-clean defaults (OpenShift restricted-v2)
	require.NotNil(t, ctr.SecurityContext)
	assert.True(t, *ctr.SecurityContext.RunAsNonRoot)
	assert.Nil(t, ctr.SecurityContext.RunAsUser) // never pin a UID
	assert.Equal(t, "core", d.Spec.Selector.MatchLabels[NameLabel])
	assert.False(t, *ctr.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, ctr.SecurityContext.Capabilities.Drop)
	require.NotNil(t, ctr.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, ctr.SecurityContext.SeccompProfile.Type)
	require.NotNil(t, d.Spec.Template.Spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, d.Spec.Template.Spec.SecurityContext.SeccompProfile.Type)
	require.Len(t, ctr.Ports, 1)
	assert.Equal(t, "http", ctr.Ports[0].Name)
	assert.Equal(t, int32(8080), ctr.Ports[0].ContainerPort)
	assert.Equal(t, "core", d.Spec.Template.Spec.ServiceAccountName)
	assert.Equal(t, "ilm", d.Spec.Template.Labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "ilm-operator", d.Spec.Template.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "core", d.Spec.Template.Labels["app.kubernetes.io/component"])
}

func TestBuildDeploymentSetsReplicasByDefault(t *testing.T) {
	c := Component{Name: "core", Namespace: "ilm", Port: 8080, Replicas: 3}
	d := BuildDeployment(c)
	require.NotNil(t, d.Spec.Replicas, "replicas set when the component is not HPA-owned")
	assert.Equal(t, int32(3), *d.Spec.Replicas)
}

func TestBuildDeploymentOmitsReplicasWhenHPAOwned(t *testing.T) {
	// An HPA-owned component (OmitReplicas) must leave .spec.replicas UNSET so the
	// operator's SSA field manager never clobbers the count the HPA writes.
	c := Component{Name: "core", Namespace: "ilm", Port: 8080, Replicas: 3, OmitReplicas: true}
	d := BuildDeployment(c)
	assert.Nil(t, d.Spec.Replicas, "replicas must be omitted when the HPA owns scaling")
}

func TestBuildDeploymentWiresEnvAndPullSecrets(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Port: 8080,
		Env:         []EnvPair{{Name: "FOO", Value: "bar"}},
		PullSecrets: []string{"regcred"},
	}
	d := BuildDeployment(c)
	ctr := d.Spec.Template.Spec.Containers[0]
	require.Len(t, ctr.Env, 1)
	assert.Equal(t, "FOO", ctr.Env[0].Name)
	assert.Equal(t, "bar", ctr.Env[0].Value)
	require.Len(t, d.Spec.Template.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "regcred", d.Spec.Template.Spec.ImagePullSecrets[0].Name)
}

func TestBuildResourceNameAndInstanceSelector(t *testing.T) {
	// A Platform is a per-namespace singleton: resource names are unscoped (just
	// the component role), while the instance label/selector still records the
	// owning CR so the workload is uniquely identified within the namespace.
	c := Component{
		Name: "core", Instance: "ilm", Namespace: "ilm",
		Image: "x:1", Port: 8080,
	}

	d := BuildDeployment(c)
	assert.Equal(t, "core", d.Name, "Deployment name must be the component name (unscoped)")
	assert.Equal(t, "core", d.Spec.Template.Spec.ServiceAccountName, "pod must use the unscoped component SA")
	assert.Equal(t, map[string]string{
		NameLabel:     "core",
		InstanceLabel: "ilm",
	}, d.Spec.Selector.MatchLabels, "selector must carry the name+instance subset")
	// The pod-template labels are a superset of the selector.
	assert.Equal(t, "core", d.Spec.Template.Labels[NameLabel])
	assert.Equal(t, "ilm", d.Spec.Template.Labels[InstanceLabel])
	assert.Equal(t, "core", d.Spec.Template.Labels["app.kubernetes.io/component"])
	// Unscoped resource name does not regress SCC compliance.
	requireRestrictedV2(t, d)

	svc := BuildService(c)
	assert.Equal(t, "core", svc.Name, "Service name must be the component name (unscoped)")
	assert.Equal(t, map[string]string{
		NameLabel:     "core",
		InstanceLabel: "ilm",
	}, svc.Spec.Selector, "Service selector must carry the name+instance subset")

	sa := BuildServiceAccount(c)
	assert.Equal(t, "core", sa.Name, "ServiceAccount name must be the component name (unscoped)")
	assert.Equal(t, "ilm", sa.Labels[InstanceLabel])
}

func TestBuildDeploymentWiresSecretEnv(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		SecretEnv: []SecretEnvRef{
			{EnvVar: "JDBC_USERNAME", SecretName: testDBSecretName, SecretKey: "username"},
			{EnvVar: "JDBC_PASSWORD", SecretName: testDBSecretName, SecretKey: "password"},
		},
	}
	d := BuildDeployment(c)
	envs := d.Spec.Template.Spec.Containers[0].Env
	var found *corev1.EnvVar
	for i := range envs {
		if envs[i].Name == "JDBC_PASSWORD" {
			found = &envs[i]
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.ValueFrom)
	require.NotNil(t, found.ValueFrom.SecretKeyRef)
	assert.Equal(t, testDBSecretName, found.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", found.ValueFrom.SecretKeyRef.Key)
	assert.Empty(t, found.Value) // never inline the secret value
}

// TestBuildDeploymentSecretEnvOptional asserts the SecretEnvRef.Optional flag is
// wired through to secretKeyRef.optional: an Optional ref renders optional=true (so a
// missing Secret does not wedge the container with CreateContainerConfigError), while a
// non-Optional ref leaves optional unset (the default required behaviour).
func TestBuildDeploymentSecretEnvOptional(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		SecretEnv: []SecretEnvRef{
			{EnvVar: "REQUIRED_VAR", SecretName: "s", SecretKey: "k"},
			{EnvVar: "OPTIONAL_VAR", SecretName: "s", SecretKey: "k", Optional: true},
		},
	}
	envs := BuildDeployment(c).Spec.Template.Spec.Containers[0].Env
	byName := func(name string) *corev1.EnvVar {
		for i := range envs {
			if envs[i].Name == name {
				return &envs[i]
			}
		}
		return nil
	}

	req := byName("REQUIRED_VAR")
	require.NotNil(t, req)
	require.NotNil(t, req.ValueFrom.SecretKeyRef)
	assert.Nil(t, req.ValueFrom.SecretKeyRef.Optional, "a non-Optional ref must leave optional unset (required)")

	opt := byName("OPTIONAL_VAR")
	require.NotNil(t, opt)
	require.NotNil(t, opt.ValueFrom.SecretKeyRef)
	require.NotNil(t, opt.ValueFrom.SecretKeyRef.Optional, "an Optional ref must set optional")
	assert.True(t, *opt.ValueFrom.SecretKeyRef.Optional, "an Optional ref must set optional=true")
}

// TestBuildDeploymentProbes asserts that liveness, readiness, and startup probes
// from Component.Probes are wired onto the main container.
func TestBuildDeploymentProbes(t *testing.T) {
	httpGet := func(path string, port int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: path,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		}
	}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Probes: Probes{
			Liveness:  httpGet("/healthz", 8080),
			Readiness: httpGet("/ready", 8080),
			Startup:   httpGet("/startup", 8080),
		},
	}
	d := BuildDeployment(c)
	ctr := d.Spec.Template.Spec.Containers[0]
	require.NotNil(t, ctr.LivenessProbe, "main container must have liveness probe")
	require.NotNil(t, ctr.ReadinessProbe, "main container must have readiness probe")
	require.NotNil(t, ctr.StartupProbe, "main container must have startup probe")
	assert.Equal(t, "/healthz", ctr.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, "/ready", ctr.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, "/startup", ctr.StartupProbe.HTTPGet.Path)
}

// TestBuildDeploymentInitAndSidecars asserts that init containers and sidecars
// land in the right positions in the pod spec, and that the builder hardens any
// container that arrives without a SecurityContext (SCC-clean).
func TestBuildDeploymentInitAndSidecars(t *testing.T) {
	// init + sidecar provided WITHOUT a SecurityContext — builder must harden them.
	initCtr := corev1.Container{
		Name:  "wait-for-db",
		Image: "busybox:1.36",
	}
	sidecar := corev1.Container{
		Name:  "opa",
		Image: "openpolicyagent/opa:0.63",
		Ports: []corev1.ContainerPort{{Name: "grpc", ContainerPort: 8181}},
	}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		InitContainers: []corev1.Container{initCtr},
		Sidecars:       []corev1.Container{sidecar},
	}
	d := BuildDeployment(c)
	ps := d.Spec.Template.Spec

	// Init containers are present.
	require.Len(t, ps.InitContainers, 1)
	assert.Equal(t, "wait-for-db", ps.InitContainers[0].Name)

	// Container list is [main, ...sidecars].
	require.Len(t, ps.Containers, 2)
	assert.Equal(t, "core", ps.Containers[0].Name, "main container must be first")
	assert.Equal(t, "opa", ps.Containers[1].Name, "sidecar must follow main")

	// Every container (init, main, sidecar) must be SCC-clean.
	requireRestrictedV2(t, d)
}

// TestBuildDeploymentSidecarsFirst asserts SidecarsFirst renders the sidecars BEFORE the main
// container — the ordering Core needs so its postStart hook (which blocks on the OPA sidecar's
// :8181) does not deadlock the kubelet's in-order, synchronous-postStart container startup.
func TestBuildDeploymentSidecarsFirst(t *testing.T) {
	sidecar := corev1.Container{Name: "opa", Image: "openpolicyagent/opa:0.63"}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Sidecars:      []corev1.Container{sidecar},
		SidecarsFirst: true,
	}
	ps := BuildDeployment(c).Spec.Template.Spec
	require.Len(t, ps.Containers, 2)
	assert.Equal(t, "opa", ps.Containers[0].Name, "SidecarsFirst: the sidecar must be first")
	assert.Equal(t, "core", ps.Containers[1].Name, "SidecarsFirst: the main container must follow the sidecars")
}

// TestBuildDeploymentInitSidecarWithExistingSecurityContext asserts that a
// caller-supplied SecurityContext is preserved and not overwritten.
func TestBuildDeploymentInitSidecarWithExistingSecurityContext(t *testing.T) {
	runAsUser := int64(1001)
	initCtr := corev1.Container{
		Name:  "custom-init",
		Image: "custom:1",
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: &runAsUser, // caller explicitly set this — builder must not erase it
		},
	}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		InitContainers: []corev1.Container{initCtr},
	}
	d := BuildDeployment(c)
	sc := d.Spec.Template.Spec.InitContainers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.RunAsUser)
	assert.Equal(t, int64(1001), *sc.RunAsUser, "caller-supplied RunAsUser must be preserved")
}

// TestBuildDeploymentVolumesAndMounts asserts that pod volumes and main-container
// volume mounts are rendered correctly.
func TestBuildDeploymentVolumesAndMounts(t *testing.T) {
	vol := corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: testCoreConfigCMName},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      "config",
		MountPath: "/app/config",
		ReadOnly:  true,
	}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Volumes:      []corev1.Volume{vol},
		VolumeMounts: []corev1.VolumeMount{mount},
	}
	d := BuildDeployment(c)
	ps := d.Spec.Template.Spec
	require.Len(t, ps.Volumes, 1)
	assert.Equal(t, "config", ps.Volumes[0].Name)
	ctr := ps.Containers[0]
	require.Len(t, ctr.VolumeMounts, 1)
	assert.Equal(t, "/app/config", ctr.VolumeMounts[0].MountPath)
	assert.True(t, ctr.VolumeMounts[0].ReadOnly)
}

// TestBuildDeploymentConfigMapEnv asserts that ConfigMapEnv entries are rendered
// as configMapKeyRef env vars after Env and SecretEnv entries.
func TestBuildDeploymentConfigMapEnv(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Env:       []EnvPair{{Name: "INLINE", Value: "v"}},
		SecretEnv: []SecretEnvRef{{EnvVar: "SECRET_VAR", SecretName: "s", SecretKey: "k"}},
		ConfigMapEnv: []ConfigMapEnvRef{
			{EnvVar: "APP_CONFIG", ConfigMapName: testCoreConfigCMName, ConfigMapKey: "app.yaml"},
		},
	}
	d := BuildDeployment(c)
	envs := d.Spec.Template.Spec.Containers[0].Env
	require.Len(t, envs, 3, "must have inline + secret + configmap env vars")
	// Order: inline first, then secretKeyRef, then configMapKeyRef.
	assert.Equal(t, "INLINE", envs[0].Name)
	assert.Nil(t, envs[0].ValueFrom)
	assert.Equal(t, "SECRET_VAR", envs[1].Name)
	require.NotNil(t, envs[1].ValueFrom)
	require.NotNil(t, envs[1].ValueFrom.SecretKeyRef)
	assert.Equal(t, "APP_CONFIG", envs[2].Name)
	require.NotNil(t, envs[2].ValueFrom)
	require.NotNil(t, envs[2].ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, testCoreConfigCMName, envs[2].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "app.yaml", envs[2].ValueFrom.ConfigMapKeyRef.Key)
	assert.Empty(t, envs[2].Value, "configmap ref must not inline the value")
}

// TestBuildDeploymentFieldRefEnv asserts that FieldRefEnv entries are rendered as
// downward-API valueFrom.fieldRef env vars, last in the env list (after inline,
// secretKeyRef, and configMapKeyRef), with the value never inlined.
func TestBuildDeploymentFieldRefEnv(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Env:          []EnvPair{{Name: "INLINE", Value: "v"}},
		SecretEnv:    []SecretEnvRef{{EnvVar: "SECRET_VAR", SecretName: "s", SecretKey: "k"}},
		ConfigMapEnv: []ConfigMapEnvRef{{EnvVar: "CM_VAR", ConfigMapName: "cm", ConfigMapKey: "k"}},
		FieldRefEnv: []FieldRefEnv{
			{EnvVar: "PROXY_INSTANCE_ID", FieldPath: "metadata.name"},
		},
	}
	d := BuildDeployment(c)
	envs := d.Spec.Template.Spec.Containers[0].Env
	require.Len(t, envs, 4, "must have inline + secret + configmap + fieldRef env vars")
	// Order: inline, secretKeyRef, configMapKeyRef, fieldRef (fieldRef last).
	assert.Equal(t, "INLINE", envs[0].Name)
	assert.Equal(t, "SECRET_VAR", envs[1].Name)
	assert.Equal(t, "CM_VAR", envs[2].Name)
	fr := envs[3]
	assert.Equal(t, "PROXY_INSTANCE_ID", fr.Name)
	require.NotNil(t, fr.ValueFrom)
	require.NotNil(t, fr.ValueFrom.FieldRef)
	assert.Equal(t, "metadata.name", fr.ValueFrom.FieldRef.FieldPath)
	assert.Empty(t, fr.Value, "fieldRef must not inline a value")
}

// TestBuildDeploymentLifecycleCommandArgsAdditionalPorts asserts that Lifecycle,
// Command, Args, and AdditionalPorts are wired onto the main container.
func TestBuildDeploymentLifecycleCommandArgsAdditionalPorts(t *testing.T) {
	lc := &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "sleep 5"}},
		},
	}
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Command: []string{"/app/server"},
		Args:    []string{"--config=/app/config/app.yaml"},
		AdditionalPorts: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: 9090},
		},
		Lifecycle: lc,
	}
	d := BuildDeployment(c)
	ctr := d.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{"/app/server"}, ctr.Command)
	assert.Equal(t, []string{"--config=/app/config/app.yaml"}, ctr.Args)
	require.Len(t, ctr.Ports, 2, "primary http port + additional metrics port")
	assert.Equal(t, "http", ctr.Ports[0].Name)
	assert.Equal(t, int32(8080), ctr.Ports[0].ContainerPort)
	assert.Equal(t, "metrics", ctr.Ports[1].Name)
	assert.Equal(t, int32(9090), ctr.Ports[1].ContainerPort)
	require.NotNil(t, ctr.Lifecycle)
	require.NotNil(t, ctr.Lifecycle.PreStop)
	assert.Equal(t, []string{"sh", "-c", "sleep 5"}, ctr.Lifecycle.PreStop.Exec.Command)
}

// TestBuildDeploymentPrimaryPortName asserts the primary container port is renamed
// when PrimaryPortName is set (e.g. Kong's consumer-http) and defaults to "http".
func TestBuildDeploymentPrimaryPortName(t *testing.T) {
	c := Component{
		Name: "api-gateway", Namespace: "ilm", Image: "kong:3", Port: 8000,
		PrimaryPortName: "consumer-http",
		AdditionalPorts: []corev1.ContainerPort{{Name: "admin-http", ContainerPort: 8001}},
	}
	ctr := BuildDeployment(c).Spec.Template.Spec.Containers[0]
	require.Len(t, ctr.Ports, 2)
	assert.Equal(t, "consumer-http", ctr.Ports[0].Name)
	assert.Equal(t, int32(8000), ctr.Ports[0].ContainerPort)
	assert.Equal(t, "admin-http", ctr.Ports[1].Name)

	// Default name remains "http" when PrimaryPortName is unset.
	def := BuildDeployment(Component{Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080})
	assert.Equal(t, "http", def.Spec.Template.Spec.Containers[0].Ports[0].Name)
}

// TestBuildDeploymentBareComponentNoNilDeref asserts that a Component with only
// the required fields set renders without nil dereferences or spurious fields.
func TestBuildDeploymentBareComponentNoNilDeref(t *testing.T) {
	c := Component{Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080}
	d := BuildDeployment(c)
	require.NotNil(t, d)
	ps := d.Spec.Template.Spec
	assert.Empty(t, ps.InitContainers, "no InitContainers when unset")
	require.Len(t, ps.Containers, 1, "exactly one main container")
	ctr := ps.Containers[0]
	assert.Empty(t, ctr.VolumeMounts)
	assert.Empty(t, ctr.Command)
	assert.Empty(t, ctr.Args)
	assert.Nil(t, ctr.Lifecycle)
	assert.Nil(t, ctr.LivenessProbe)
	assert.Nil(t, ctr.ReadinessProbe)
	assert.Nil(t, ctr.StartupProbe)
	require.Len(t, ctr.Ports, 1, "only primary http port")
	assert.Equal(t, "http", ctr.Ports[0].Name)
	assert.Empty(t, ps.Volumes)
	requireRestrictedV2(t, d)
}

// TestBuildDeploymentResourcesUnchanged asserts the Resources field still works
// correctly (no regression from new fields).
func TestBuildDeploymentResourcesUnchanged(t *testing.T) {
	c := Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
	d := BuildDeployment(c)
	ctr := d.Spec.Template.Spec.Containers[0]
	assert.Equal(t, resource.MustParse("500m"), ctr.Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), ctr.Resources.Limits[corev1.ResourceMemory])
}
