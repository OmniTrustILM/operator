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

package connector

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildDeployment constructs a Deployment for the given Connector by composing the
// shared Component render model (the CLAUDE.md per-kind pattern): pod+container SCC
// hardening, env/volume projection, and the workload shape all come from
// internal/builder/common, so hardening fixes land on every Kind at once.
func BuildDeployment(conn *otilmv1alpha1.Connector, configChecksum string) *appsv1.Deployment {
	return common.BuildDeployment(component(conn, configChecksum))
}

// component assembles the render-ready Component for a Connector. The historical
// per-kind label/selector scheme is preserved via the override fields — a deployed
// Connector's .spec.selector is immutable — and the Service port shape is passed
// verbatim so live Services stay byte-identical.
//
// SCC note: the user SecurityContextSpec's ReadOnlyRootFilesystem is honored (a
// runtime decision), but RunAsNonRoot is FORCED true by the shared hardening — a CR
// can no longer weaken the restricted-v2 contract, matching the platform components.
func component(conn *otilmv1alpha1.Connector, configChecksum string) common.Component {
	// nil bundleLookup: Connector is version-agnostic (no BOM bundle); its image comes
	// wholly from conn.Spec.Image, so there is no bundle default to fall back to.
	image, pullPolicy := common.ResolveImage(nil, "", otilmv1alpha1.ImageSpec{}, conn.Spec.Image)
	port := conn.Spec.Service.Port

	inlineEnv := make([]common.EnvPair, 0, len(conn.Spec.Env))
	for _, e := range conn.Spec.Env {
		inlineEnv = append(inlineEnv, common.EnvPair{Name: e.Name, Value: e.Value})
	}

	// Keyed secret/configmap refs render via the shared CRD-agnostic projection and
	// ride in ExtraEnv (appended last, so a user keyed ref wins on a duplicate name —
	// Kubernetes container env is last-duplicate-wins).
	var extraEnv []corev1.EnvVar
	var envFrom []corev1.EnvFromSource
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	for i := range conn.Spec.SecretRefs {
		b := common.BuildSecretRef(&conn.Spec.SecretRefs[i])
		extraEnv = append(extraEnv, b.Env...)
		envFrom = append(envFrom, b.EnvFrom...)
		volumes = append(volumes, b.Volumes...)
		volumeMounts = append(volumeMounts, b.VolumeMounts...)
	}
	for i := range conn.Spec.ConfigMapRefs {
		b := common.BuildConfigMapRef(&conn.Spec.ConfigMapRefs[i])
		extraEnv = append(extraEnv, b.Env...)
		envFrom = append(envFrom, b.EnvFrom...)
		volumes = append(volumes, b.Volumes...)
		volumeMounts = append(volumeMounts, b.VolumeMounts...)
	}
	for _, v := range conn.Spec.Volumes {
		vol, vm := common.BuildEphemeralVolume(v)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, vm)
	}

	replicas := int32(1)
	if conn.Spec.Replicas != nil {
		replicas = *conn.Spec.Replicas
	}

	// Merge user-provided pod annotations with the checksum annotation.
	// The checksum annotation takes precedence over user-provided values.
	podAnnotations := make(map[string]string, len(conn.Spec.PodAnnotations)+1)
	for k, v := range conn.Spec.PodAnnotations {
		podAnnotations[k] = v
	}
	podAnnotations[ChecksumAnnotation] = configChecksum

	// Read-only root is a per-workload runtime decision: default true, user override honored.
	roRoot := true
	if conn.Spec.SecurityContext != nil && conn.Spec.SecurityContext.ReadOnlyRootFilesystem != nil {
		roRoot = *conn.Spec.SecurityContext.ReadOnlyRootFilesystem
	}

	c := common.Component{
		Name:          ChildResourceName(conn),
		ContainerName: "connector",
		Instance:      conn.Name,
		Namespace:     conn.Namespace,
		Image:         image,
		PullPolicy:    pullPolicy,
		PullSecrets:   conn.Spec.Image.PullSecrets,
		Replicas:      replicas,
		Port:          port,
		ServiceType:   corev1.ServiceType(conn.Spec.Service.Type),
		ServicePorts: []corev1.ServicePort{{
			Name:       "http",
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		}},
		Env:      inlineEnv,
		ExtraEnv: extraEnv,
		EnvFrom:  envFrom,
		Probes: common.Probes{
			Liveness:  buildProbe(conn, livenessProbe, port),
			Readiness: buildProbe(conn, readinessProbe, port),
			Startup:   buildProbe(conn, startupProbe, port),
		},
		Volumes:                volumes,
		VolumeMounts:           volumeMounts,
		Command:                conn.Spec.Image.Command,
		Args:                   conn.Spec.Image.Args,
		ReadOnlyRootFilesystem: &roRoot,
		NodeSelector:           conn.Spec.NodeSelector,
		Tolerations:            conn.Spec.Tolerations,
		PodAnnotations:         podAnnotations,
		PodLabels:              conn.Spec.PodLabels,
		LabelsOverride:         Labels(conn),
		SelectorLabelsOverride: SelectorLabels(conn),
	}
	if conn.Spec.Resources != nil {
		c.Resources = *conn.Spec.Resources
	}
	if conn.Spec.Lifecycle != nil {
		c.TerminationGracePeriodSeconds = conn.Spec.Lifecycle.TerminationGracePeriodSeconds
	}
	c.InitContainers = conn.Spec.InitContainers
	c.Sidecars = conn.Spec.Sidecars
	c.Affinity = conn.Spec.Affinity
	if conn.Spec.ServiceAccount != nil {
		if conn.Spec.ServiceAccount.Name != nil {
			c.ServiceAccountName = *conn.Spec.ServiceAccount.Name
		}
		c.ServiceAccountAnnotations = conn.Spec.ServiceAccount.Annotations
	}
	return c
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

	// Skip the probe if the path is empty
	if cfg.Path == "" {
		return nil
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
