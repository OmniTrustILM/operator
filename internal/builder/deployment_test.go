package builder_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	testChecksum   = "chk"
	testSecretName = "my-secret"
	testCMName     = "app-config"
	testMemory128  = "128Mi"
	testVolName    = "tmp-data"
)

func TestBuildDeploymentBasic(t *testing.T) {
	conn := newTestConnector()
	replicas := int32(3)
	conn.Spec.Replicas = &replicas
	conn.Spec.Image.PullPolicy = "Always"

	dep := builder.BuildDeployment(conn, "abc123")

	assert.Equal(t, testConnectorName, dep.Name)
	assert.Equal(t, "default", dep.Namespace)
	assert.Equal(t, builder.Labels(conn), dep.Labels)
	assert.Equal(t, int32(3), *dep.Spec.Replicas)
	assert.Equal(t, builder.SelectorLabels(conn), dep.Spec.Selector.MatchLabels)

	// Pod template labels
	assert.Equal(t, builder.Labels(conn), dep.Spec.Template.Labels)

	// Checksum annotation on pod template
	assert.Equal(t, "abc123", dep.Spec.Template.Annotations[builder.ChecksumAnnotation])

	// ServiceAccountName
	assert.Equal(t, testConnectorName, dep.Spec.Template.Spec.ServiceAccountName)

	// Container
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "connector", c.Name)
	assert.Equal(t, "docker.io/czertainly/czertainly-x509-compliance-provider:2.0.0", c.Image)
	assert.Equal(t, corev1.PullAlways, c.ImagePullPolicy)

	// Container port
	require.Len(t, c.Ports, 1)
	assert.Equal(t, int32(8080), c.Ports[0].ContainerPort)
	assert.Equal(t, corev1.ProtocolTCP, c.Ports[0].Protocol)
}

func TestBuildDeploymentProbes(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Probes = &otilmv1alpha1.ProbeSpec{
		Liveness: &otilmv1alpha1.ProbeConfig{
			Path:                "/healthz",
			InitialDelaySeconds: 30,
			PeriodSeconds:       20,
			FailureThreshold:    5,
		},
		Readiness: &otilmv1alpha1.ProbeConfig{
			Path:                "/ready",
			InitialDelaySeconds: 10,
			PeriodSeconds:       15,
			FailureThreshold:    2,
		},
		Startup: &otilmv1alpha1.ProbeConfig{
			Path:             "/started",
			PeriodSeconds:    5,
			FailureThreshold: 30,
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Liveness
	require.NotNil(t, c.LivenessProbe)
	assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.LivenessProbe.HTTPGet.Port)
	assert.Equal(t, int32(30), c.LivenessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(20), c.LivenessProbe.PeriodSeconds)
	assert.Equal(t, int32(5), c.LivenessProbe.FailureThreshold)

	// Readiness
	require.NotNil(t, c.ReadinessProbe)
	assert.Equal(t, "/ready", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.ReadinessProbe.HTTPGet.Port)
	assert.Equal(t, int32(10), c.ReadinessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(15), c.ReadinessProbe.PeriodSeconds)
	assert.Equal(t, int32(2), c.ReadinessProbe.FailureThreshold)

	// Startup
	require.NotNil(t, c.StartupProbe)
	assert.Equal(t, "/started", c.StartupProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.StartupProbe.HTTPGet.Port)
	assert.Equal(t, int32(5), c.StartupProbe.PeriodSeconds)
	assert.Equal(t, int32(30), c.StartupProbe.FailureThreshold)
}

func TestBuildDeploymentDefaultProbes(t *testing.T) {
	conn := newTestConnector()
	// Probes is nil by default

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Default liveness
	require.NotNil(t, c.LivenessProbe)
	assert.Equal(t, "/v2/health/liveness", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.LivenessProbe.HTTPGet.Port)
	assert.Equal(t, int32(15), c.LivenessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(10), c.LivenessProbe.PeriodSeconds)
	assert.Equal(t, int32(3), c.LivenessProbe.FailureThreshold)

	// Default readiness
	require.NotNil(t, c.ReadinessProbe)
	assert.Equal(t, "/v2/health/readiness", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.ReadinessProbe.HTTPGet.Port)
	assert.Equal(t, int32(5), c.ReadinessProbe.InitialDelaySeconds)
	assert.Equal(t, int32(10), c.ReadinessProbe.PeriodSeconds)
	assert.Equal(t, int32(3), c.ReadinessProbe.FailureThreshold)

	// Default startup
	require.NotNil(t, c.StartupProbe)
	assert.Equal(t, "/v2/health/liveness", c.StartupProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), c.StartupProbe.HTTPGet.Port)
	assert.Equal(t, int32(0), c.StartupProbe.InitialDelaySeconds)
	assert.Equal(t, int32(10), c.StartupProbe.PeriodSeconds)
	assert.Equal(t, int32(45), c.StartupProbe.FailureThreshold)
}

func TestBuildDeploymentProbeEmptyPath(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Probes = &otilmv1alpha1.ProbeSpec{
		Liveness: &otilmv1alpha1.ProbeConfig{
			Path:             "",
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Empty path should result in nil probe
	assert.Nil(t, c.LivenessProbe, "expected nil liveness probe when path is empty")
	// Readiness and startup should still get defaults
	require.NotNil(t, c.ReadinessProbe)
	require.NotNil(t, c.StartupProbe)
}

func TestBuildDeploymentEnvVars(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Env = []otilmv1alpha1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{Name: "DB_HOST", Value: "localhost"},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Find the inline env vars
	envMap := make(map[string]string)
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			envMap[e.Name] = e.Value
		}
	}
	assert.Equal(t, "bar", envMap["FOO"])
	assert.Equal(t, "localhost", envMap["DB_HOST"])
}

func TestBuildDeploymentSecretRefEnv(t *testing.T) {
	conn := newTestConnector()
	envVarName := "DB_PASSWORD"
	conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
		{
			Name: testSecretName,
			Type: otilmv1alpha1.RefTypeEnv,
			Keys: []otilmv1alpha1.RefKeyMapping{
				{
					SecretKey: "password",
					EnvVar:    &envVarName,
				},
			},
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Find env var with ValueFrom
	var found *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "DB_PASSWORD" {
			found = &c.Env[i]
			break
		}
	}
	require.NotNil(t, found, "expected env var DB_PASSWORD")
	require.NotNil(t, found.ValueFrom)
	require.NotNil(t, found.ValueFrom.SecretKeyRef)
	assert.Equal(t, testSecretName, found.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", found.ValueFrom.SecretKeyRef.Key)
}

func TestBuildDeploymentSecretRefEnvAllKeys(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
		{
			Name: testSecretName,
			Type: otilmv1alpha1.RefTypeEnv,
			// No keys → envFrom
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	require.Len(t, c.EnvFrom, 1)
	require.NotNil(t, c.EnvFrom[0].SecretRef)
	assert.Equal(t, testSecretName, c.EnvFrom[0].SecretRef.Name)
}

func TestBuildDeploymentSecretRefVolume(t *testing.T) {
	conn := newTestConnector()
	mountPath := "/etc/secret"
	path1 := "cert.pem"
	conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
		{
			Name:      "tls-secret",
			Type:      otilmv1alpha1.RefTypeVolume,
			MountPath: &mountPath,
			Keys: []otilmv1alpha1.RefKeyMapping{
				{
					SecretKey: "tls.crt",
					Path:      &path1,
				},
			},
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	podSpec := dep.Spec.Template.Spec
	c := podSpec.Containers[0]

	// Volume
	var vol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "secret-tls-secret" {
			vol = &podSpec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "expected volume secret-tls-secret")
	require.NotNil(t, vol.Secret)
	assert.Equal(t, "tls-secret", vol.Secret.SecretName)
	require.Len(t, vol.Secret.Items, 1)
	assert.Equal(t, "tls.crt", vol.Secret.Items[0].Key)
	assert.Equal(t, "cert.pem", vol.Secret.Items[0].Path)

	// VolumeMount
	var vm *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "secret-tls-secret" {
			vm = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, vm, "expected volume mount secret-tls-secret")
	assert.Equal(t, "/etc/secret", vm.MountPath)
	assert.True(t, vm.ReadOnly)
}

func TestBuildDeploymentConfigMapRefEnv(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
		{
			Name: testCMName,
			Type: otilmv1alpha1.RefTypeEnv,
			// No keys → envFrom
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	// Should have an envFrom with configMapRef
	var found bool
	for _, ef := range c.EnvFrom {
		if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == testCMName {
			found = true
			break
		}
	}
	assert.True(t, found, "expected envFrom with configMapRef app-config")
}

func TestBuildDeploymentConfigMapRefVolume(t *testing.T) {
	conn := newTestConnector()
	mountPath := "/etc/config"
	conn.Spec.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
		{
			Name:      testCMName,
			Type:      otilmv1alpha1.RefTypeVolume,
			MountPath: &mountPath,
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	podSpec := dep.Spec.Template.Spec
	c := podSpec.Containers[0]

	// Volume
	var vol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "configmap-app-config" {
			vol = &podSpec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "expected volume configmap-app-config")
	require.NotNil(t, vol.ConfigMap)
	assert.Equal(t, testCMName, vol.ConfigMap.Name)

	// VolumeMount
	var vm *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "configmap-app-config" {
			vm = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, vm, "expected volume mount configmap-app-config")
	assert.Equal(t, "/etc/config", vm.MountPath)
	assert.True(t, vm.ReadOnly)
}

func TestBuildDeploymentEphemeralVolumes(t *testing.T) {
	conn := newTestConnector()
	medium := "Memory"
	sizeLimit := testMemory128
	conn.Spec.Volumes = []otilmv1alpha1.VolumeSpec{
		{
			Name:      testVolName,
			MountPath: "/tmp",
			EmptyDir: &otilmv1alpha1.EmptyDirSpec{
				Medium:    &medium,
				SizeLimit: &sizeLimit,
			},
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	podSpec := dep.Spec.Template.Spec
	c := podSpec.Containers[0]

	// Volume
	var vol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == testVolName {
			vol = &podSpec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "expected volume tmp-data")
	require.NotNil(t, vol.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, vol.EmptyDir.Medium)
	expectedSize := resource.MustParse(testMemory128)
	assert.True(t, expectedSize.Equal(*vol.EmptyDir.SizeLimit))

	// VolumeMount
	var vm *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == testVolName {
			vm = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, vm, "expected volume mount tmp-data")
	assert.Equal(t, "/tmp", vm.MountPath)
}

func TestBuildDeploymentSecurityContext(t *testing.T) {
	conn := newTestConnector()
	runAsNonRoot := false
	readOnly := false
	conn.Spec.SecurityContext = &otilmv1alpha1.SecurityContextSpec{
		RunAsNonRoot:           &runAsNonRoot,
		ReadOnlyRootFilesystem: &readOnly,
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	require.NotNil(t, c.SecurityContext)
	assert.Equal(t, false, *c.SecurityContext.RunAsNonRoot)
	assert.Equal(t, false, *c.SecurityContext.ReadOnlyRootFilesystem)
	// Hardened fields are always set regardless of spec overrides.
	require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, false, *c.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, c.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
	require.NotNil(t, c.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, c.SecurityContext.SeccompProfile.Type)
}

func TestBuildDeploymentDefaultSecurityContext(t *testing.T) {
	conn := newTestConnector()
	// SecurityContext is nil

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	require.NotNil(t, c.SecurityContext)
	assert.Equal(t, true, *c.SecurityContext.RunAsNonRoot)
	assert.Equal(t, true, *c.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, false, *c.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, c.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
	require.NotNil(t, c.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, c.SecurityContext.SeccompProfile.Type)
}

func TestBuildDeploymentResources(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse(testMemory128),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	c := dep.Spec.Template.Spec.Containers[0]

	assert.True(t, c.Resources.Requests.Cpu().Equal(resource.MustParse("100m")))
	assert.True(t, c.Resources.Requests.Memory().Equal(resource.MustParse(testMemory128)))
	assert.True(t, c.Resources.Limits.Cpu().Equal(resource.MustParse("500m")))
	assert.True(t, c.Resources.Limits.Memory().Equal(resource.MustParse("512Mi")))
}

func TestBuildDeploymentImagePullSecrets(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Image.PullSecrets = []string{"regcred", "another-secret"}

	dep := builder.BuildDeployment(conn, testChecksum)
	podSpec := dep.Spec.Template.Spec

	require.Len(t, podSpec.ImagePullSecrets, 2)
	assert.Equal(t, "regcred", podSpec.ImagePullSecrets[0].Name)
	assert.Equal(t, "another-secret", podSpec.ImagePullSecrets[1].Name)
}

func TestBuildDeploymentGracePeriod(t *testing.T) {
	conn := newTestConnector()
	grace := int64(60)
	conn.Spec.Lifecycle = &otilmv1alpha1.LifecycleSpec{
		TerminationGracePeriodSeconds: &grace,
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	podSpec := dep.Spec.Template.Spec

	require.NotNil(t, podSpec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(60), *podSpec.TerminationGracePeriodSeconds)
}

func TestBuildDeploymentPodAnnotations(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.PodAnnotations = map[string]string{
		"vault.hashicorp.com/agent-inject": "true",
		"custom":                           "value",
	}

	dep := builder.BuildDeployment(conn, "abc123")
	annotations := dep.Spec.Template.Annotations

	assert.Equal(t, "true", annotations["vault.hashicorp.com/agent-inject"])
	assert.Equal(t, "value", annotations["custom"])
	assert.Equal(t, "abc123", annotations[builder.ChecksumAnnotation])
}

func TestBuildDeploymentPodAnnotationsChecksumPrecedence(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.PodAnnotations = map[string]string{
		builder.ChecksumAnnotation: "user-value",
	}

	dep := builder.BuildDeployment(conn, "real-checksum")
	annotations := dep.Spec.Template.Annotations

	assert.Equal(t, "real-checksum", annotations[builder.ChecksumAnnotation])
}

func TestBuildDeploymentPodLabels(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.PodLabels = map[string]string{
		"team":                   "platform",
		"app.kubernetes.io/name": "override-attempt",
	}

	dep := builder.BuildDeployment(conn, testChecksum)
	podLabels := dep.Spec.Template.Labels

	// User label should appear
	assert.Equal(t, "platform", podLabels["team"])
	// Operator label takes precedence over user-provided override
	assert.Equal(t, testConnectorName, podLabels["app.kubernetes.io/name"])
}
