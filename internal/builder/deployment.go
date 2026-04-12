package builder

import (
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BuildDeployment constructs a Deployment for the given Connector.
func BuildDeployment(conn *otilmv1alpha1.Connector, configChecksum string) *appsv1.Deployment {
	name := ChildResourceName(conn)
	labels := Labels(conn)
	port := conn.Spec.Service.Port

	envVars, envFrom := buildEnvVarsAndSources(conn)
	volumes, volumeMounts := buildVolumes(conn)

	container := corev1.Container{
		Name:            "connector",
		Image:           fmt.Sprintf("%s:%s", conn.Spec.Image.Repository, conn.Spec.Image.Tag),
		ImagePullPolicy: corev1.PullPolicy(conn.Spec.Image.PullPolicy),
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env:             envVars,
		EnvFrom:         envFrom,
		VolumeMounts:    volumeMounts,
		LivenessProbe:   buildProbe(conn, livenessProbe, port),
		ReadinessProbe:  buildProbe(conn, readinessProbe, port),
		StartupProbe:    buildProbe(conn, startupProbe, port),
		SecurityContext: buildSecurityContext(conn),
	}

	// Resources
	if conn.Spec.Resources != nil {
		container.Resources = *conn.Spec.Resources
	}

	podSpec := corev1.PodSpec{
		ServiceAccountName: name,
		Containers:         []corev1.Container{container},
		Volumes:            volumes,
		ImagePullSecrets:   buildImagePullSecrets(conn),
	}

	// Termination grace period
	if conn.Spec.Lifecycle != nil && conn.Spec.Lifecycle.TerminationGracePeriodSeconds != nil {
		podSpec.TerminationGracePeriodSeconds = conn.Spec.Lifecycle.TerminationGracePeriodSeconds
	}

	// Merge user-provided pod annotations with the checksum annotation.
	// The checksum annotation takes precedence over user-provided values.
	podAnnotations := make(map[string]string, len(conn.Spec.PodAnnotations)+1)
	for k, v := range conn.Spec.PodAnnotations {
		podAnnotations[k] = v
	}
	podAnnotations[ChecksumAnnotation] = configChecksum

	// Merge user-provided pod labels with operator-managed labels.
	// Operator labels take precedence (they are immutable selectors).
	podLabels := make(map[string]string, len(conn.Spec.PodLabels)+len(labels))
	for k, v := range conn.Spec.PodLabels {
		podLabels[k] = v
	}
	for k, v := range labels {
		podLabels[k] = v
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: conn.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: conn.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: SelectorLabels(conn),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: podSpec,
			},
		},
	}
}

// buildEnvVarsAndSources assembles inline env vars and EnvFrom sources from
// the Connector spec, including inline env, SecretRefs, and ConfigMapRefs.
func buildEnvVarsAndSources(conn *otilmv1alpha1.Connector) ([]corev1.EnvVar, []corev1.EnvFromSource) {
	var envVars []corev1.EnvVar
	var envFrom []corev1.EnvFromSource

	for _, e := range conn.Spec.Env {
		envVars = append(envVars, corev1.EnvVar{
			Name:  e.Name,
			Value: e.Value,
		})
	}

	for i := range conn.Spec.SecretRefs {
		sr := &conn.Spec.SecretRefs[i]
		e, ef, _, _ := buildSecretRef(sr)
		envVars = append(envVars, e...)
		envFrom = append(envFrom, ef...)
	}

	for i := range conn.Spec.ConfigMapRefs {
		cmr := &conn.Spec.ConfigMapRefs[i]
		e, ef, _, _ := buildConfigMapRef(cmr)
		envVars = append(envVars, e...)
		envFrom = append(envFrom, ef...)
	}

	return envVars, envFrom
}

// buildVolumes assembles all volumes and volume mounts from SecretRefs,
// ConfigMapRefs, and ephemeral volumes.
func buildVolumes(conn *otilmv1alpha1.Connector) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	for i := range conn.Spec.SecretRefs {
		sr := &conn.Spec.SecretRefs[i]
		_, _, v, vm := buildSecretRef(sr)
		volumes = append(volumes, v...)
		volumeMounts = append(volumeMounts, vm...)
	}

	for i := range conn.Spec.ConfigMapRefs {
		cmr := &conn.Spec.ConfigMapRefs[i]
		_, _, v, vm := buildConfigMapRef(cmr)
		volumes = append(volumes, v...)
		volumeMounts = append(volumeMounts, vm...)
	}

	for _, v := range conn.Spec.Volumes {
		vol, vm := buildEphemeralVolume(v)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, vm)
	}

	return volumes, volumeMounts
}

// buildEphemeralVolume constructs a single ephemeral Volume and its VolumeMount.
func buildEphemeralVolume(v otilmv1alpha1.VolumeSpec) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{Name: v.Name}
	if v.EmptyDir != nil {
		emptyDir := &corev1.EmptyDirVolumeSource{}
		if v.EmptyDir.Medium != nil {
			emptyDir.Medium = corev1.StorageMedium(*v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit != nil {
			qty, err := resource.ParseQuantity(*v.EmptyDir.SizeLimit)
			if err != nil {
				log.Log.Info("invalid sizeLimit value, skipping", "volume", v.Name, "sizeLimit", *v.EmptyDir.SizeLimit, "error", err)
			} else {
				emptyDir.SizeLimit = &qty
			}
		}
		vol.VolumeSource = corev1.VolumeSource{EmptyDir: emptyDir}
	}
	vm := corev1.VolumeMount{
		Name:      v.Name,
		MountPath: v.MountPath,
	}
	return vol, vm
}

// buildImagePullSecrets converts the image pull secret names into LocalObjectReferences.
func buildImagePullSecrets(conn *otilmv1alpha1.Connector) []corev1.LocalObjectReference {
	var secrets []corev1.LocalObjectReference
	for _, s := range conn.Spec.Image.PullSecrets {
		secrets = append(secrets, corev1.LocalObjectReference{Name: s})
	}
	return secrets
}

// buildSecretEnvVars builds env vars for a SecretRef with explicit keys.
func buildSecretEnvVars(sr *otilmv1alpha1.SecretRef) []corev1.EnvVar {
	var envVars []corev1.EnvVar
	for _, k := range sr.Keys {
		envName := k.SecretKey
		if k.EnvVar != nil {
			envName = *k.EnvVar
		}
		envVars = append(envVars, corev1.EnvVar{
			Name: envName,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sr.Name},
					Key:                  k.SecretKey,
				},
			},
		})
	}
	return envVars
}

// buildConfigMapEnvVars builds env vars for a ConfigMapRef with explicit keys.
func buildConfigMapEnvVars(cmr *otilmv1alpha1.ConfigMapRef) []corev1.EnvVar {
	var envVars []corev1.EnvVar
	for _, k := range cmr.Keys {
		envName := k.ConfigMapKey
		if k.EnvVar != nil {
			envName = *k.EnvVar
		}
		envVars = append(envVars, corev1.EnvVar{
			Name: envName,
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
					Key:                  k.ConfigMapKey,
				},
			},
		})
	}
	return envVars
}

//nolint:dupl // SecretRef and ConfigMapRef handle different K8s types; deduplication would hurt clarity.
func buildSecretRef(sr *otilmv1alpha1.SecretRef) (
	envVars []corev1.EnvVar,
	envFrom []corev1.EnvFromSource,
	volumes []corev1.Volume,
	volumeMounts []corev1.VolumeMount,
) {
	switch sr.Type {
	case otilmv1alpha1.RefTypeEnv:
		if len(sr.Keys) == 0 {
			envFrom = append(envFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: sr.Name},
				},
			})
		} else {
			envVars = buildSecretEnvVars(sr)
		}
	case otilmv1alpha1.RefTypeVolume:
		volName := fmt.Sprintf("secret-%s", sr.Name)
		secretVol := &corev1.SecretVolumeSource{SecretName: sr.Name}
		for _, k := range sr.Keys {
			path := k.SecretKey
			if k.Path != nil {
				path = *k.Path
			}
			secretVol.Items = append(secretVol.Items, corev1.KeyToPath{Key: k.SecretKey, Path: path})
		}
		volumes = append(volumes, corev1.Volume{
			Name:         volName,
			VolumeSource: corev1.VolumeSource{Secret: secretVol},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: derefString(sr.MountPath),
			ReadOnly:  true,
		})
	}
	return
}

//nolint:dupl // ConfigMapRef and SecretRef handle different K8s types; deduplication would hurt clarity.
func buildConfigMapRef(cmr *otilmv1alpha1.ConfigMapRef) (
	envVars []corev1.EnvVar,
	envFrom []corev1.EnvFromSource,
	volumes []corev1.Volume,
	volumeMounts []corev1.VolumeMount,
) {
	switch cmr.Type {
	case otilmv1alpha1.RefTypeEnv:
		if len(cmr.Keys) == 0 {
			envFrom = append(envFrom, corev1.EnvFromSource{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
				},
			})
		} else {
			envVars = buildConfigMapEnvVars(cmr)
		}
	case otilmv1alpha1.RefTypeVolume:
		volName := fmt.Sprintf("configmap-%s", cmr.Name)
		cmVol := &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
		}
		for _, k := range cmr.Keys {
			path := k.ConfigMapKey
			if k.Path != nil {
				path = *k.Path
			}
			cmVol.Items = append(cmVol.Items, corev1.KeyToPath{Key: k.ConfigMapKey, Path: path})
		}
		volumes = append(volumes, corev1.Volume{
			Name:         volName,
			VolumeSource: corev1.VolumeSource{ConfigMap: cmVol},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: derefString(cmr.MountPath),
			ReadOnly:  true,
		})
	}
	return
}

func derefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

type probeType int

const (
	livenessProbe probeType = iota
	readinessProbe
	startupProbe
)

func buildProbe(conn *otilmv1alpha1.Connector, pt probeType, port int32) *corev1.Probe {
	var cfg *otilmv1alpha1.ProbeConfig

	if conn.Spec.Probes != nil {
		switch pt {
		case livenessProbe:
			cfg = conn.Spec.Probes.Liveness
		case readinessProbe:
			cfg = conn.Spec.Probes.Readiness
		case startupProbe:
			cfg = conn.Spec.Probes.Startup
		}
	}

	// Use defaults if no explicit config
	if cfg == nil {
		cfg = defaultProbeConfig(pt)
	}

	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: cfg.Path,
				Port: intstr.FromInt32(port),
			},
		},
		InitialDelaySeconds: cfg.InitialDelaySeconds,
		PeriodSeconds:       cfg.PeriodSeconds,
		FailureThreshold:    cfg.FailureThreshold,
	}
}

func defaultProbeConfig(pt probeType) *otilmv1alpha1.ProbeConfig {
	switch pt {
	case livenessProbe:
		return &otilmv1alpha1.ProbeConfig{
			Path:                "/v2/health/liveness",
			InitialDelaySeconds: 15,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		}
	case readinessProbe:
		return &otilmv1alpha1.ProbeConfig{
			Path:                "/v2/health/readiness",
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		}
	case startupProbe:
		return &otilmv1alpha1.ProbeConfig{
			Path:             "/v2/health/liveness",
			PeriodSeconds:    10,
			FailureThreshold: 45,
		}
	default:
		return &otilmv1alpha1.ProbeConfig{}
	}
}

func buildSecurityContext(conn *otilmv1alpha1.Connector) *corev1.SecurityContext {
	runAsNonRoot := true
	readOnlyRoot := true
	allowPrivilegeEscalation := false

	if conn.Spec.SecurityContext != nil {
		if conn.Spec.SecurityContext.RunAsNonRoot != nil {
			runAsNonRoot = *conn.Spec.SecurityContext.RunAsNonRoot
		}
		if conn.Spec.SecurityContext.ReadOnlyRootFilesystem != nil {
			readOnlyRoot = *conn.Spec.SecurityContext.ReadOnlyRootFilesystem
		}
	}

	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		ReadOnlyRootFilesystem:   &readOnlyRoot,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
