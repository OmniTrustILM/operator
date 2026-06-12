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

package proxy

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/bom"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildDeployment constructs a Deployment for the given Proxy by composing the shared
// Component render model. The container's only operator-wired configuration is the
// config token (PROXY_CONFIG_TOKEN via secretKeyRef) and the optional signing key —
// all proxy configuration travels inside the token, so the builder never renders
// broker settings.
func BuildDeployment(px *otilmv1alpha1.Proxy, configChecksum string) *appsv1.Deployment {
	return common.BuildDeployment(component(px, configChecksum))
}

// component assembles the render-ready Component for a Proxy. The per-kind
// label/selector scheme rides the override fields (a deployed Proxy's .spec.selector
// is immutable), and the two-port Service shape is passed verbatim.
func component(px *otilmv1alpha1.Proxy, configChecksum string) common.Component {
	image, pullPolicy := resolveImage(px)

	// User env is process-level only (HTTPS_PROXY etc.); the operator-reserved token
	// names are filtered out — Kubernetes env is last-duplicate-wins, and the token
	// SecretEnv refs render AFTER inline env, so the secretKeyRef wiring always wins.
	inlineEnv := make([]common.EnvPair, 0, len(px.Spec.Env))
	for _, e := range px.Spec.Env {
		if e.Name == EnvConfigToken || e.Name == EnvTokenSigningKey {
			continue // reserved operator wiring; an inline value must never replace it
		}
		inlineEnv = append(inlineEnv, common.EnvPair{Name: e.Name, Value: e.Value})
	}

	extraEnv, envFrom, volumes, volumeMounts := refProjections(px)

	// Read-only root is a per-workload runtime decision: default true, user override honored.
	roRoot := true
	if px.Spec.SecurityContext != nil && px.Spec.SecurityContext.ReadOnlyRootFilesystem != nil {
		roRoot = *px.Spec.SecurityContext.ReadOnlyRootFilesystem
	}

	secretName := px.Spec.ConfigTokenSecretRef.Name
	replicas := int32(1)
	if px.Spec.Replicas != nil {
		replicas = *px.Spec.Replicas
	}

	podAnnotations := make(map[string]string, len(px.Spec.PodAnnotations)+1)
	for k, v := range px.Spec.PodAnnotations {
		podAnnotations[k] = v
	}
	podAnnotations[ChecksumAnnotation] = configChecksum

	c := common.Component{
		Name:          ChildResourceName(px),
		ContainerName: "proxy",
		Instance:      px.Name,
		Namespace:     px.Namespace,
		Image:         image,
		PullPolicy:    pullPolicy,
		Replicas:      replicas,
		Port:          HTTPPort,
		AdditionalPorts: []corev1.ContainerPort{
			{Name: "api", ContainerPort: APIPort},
		},
		ServicePorts: []corev1.ServicePort{
			{Name: "http", Port: HTTPPort, TargetPort: intstr.FromInt32(HTTPPort), Protocol: corev1.ProtocolTCP},
			{Name: "api", Port: APIPort, TargetPort: intstr.FromInt32(APIPort), Protocol: corev1.ProtocolTCP},
		},
		Env:      inlineEnv,
		ExtraEnv: extraEnv,
		EnvFrom:  envFrom,
		SecretEnv: []common.SecretEnvRef{
			{EnvVar: EnvConfigToken, SecretName: secretName, SecretKey: TokenKey(px)},
			// Optional: present only when the platform signs tokens.
			{EnvVar: EnvTokenSigningKey, SecretName: secretName, SecretKey: SigningKeyKey(px), Optional: true},
		},
		Probes: common.Probes{
			Liveness:  buildProbe(px, livenessProbe),
			Readiness: buildProbe(px, readinessProbe),
			Startup:   buildProbe(px, startupProbe),
		},
		Volumes:                       volumes,
		VolumeMounts:                  volumeMounts,
		ReadOnlyRootFilesystem:        &roRoot,
		TerminationGracePeriodSeconds: px.Spec.TerminationGracePeriodSeconds,
		NodeSelector:                  px.Spec.NodeSelector,
		Tolerations:                   px.Spec.Tolerations,
		PodAnnotations:                podAnnotations,
		PodLabels:                     px.Spec.PodLabels,
		LabelsOverride:                Labels(px),
		SelectorLabelsOverride:        SelectorLabels(px),
	}
	if px.Spec.Resources != nil {
		c.Resources = *px.Spec.Resources
	}
	if px.Spec.Image != nil {
		c.PullSecrets = px.Spec.Image.PullSecrets
		c.Command = px.Spec.Image.Command
		c.Args = px.Spec.Image.Args
	}
	c.InitContainers = px.Spec.InitContainers
	c.Sidecars = px.Spec.Sidecars
	c.Affinity = px.Spec.Affinity
	if px.Spec.ServiceAccount != nil {
		if px.Spec.ServiceAccount.Name != nil {
			c.ServiceAccountName = *px.Spec.ServiceAccount.Name
		}
		c.ServiceAccountAnnotations = px.Spec.ServiceAccount.Annotations
	}
	return c
}

// refProjections renders spec.secretRefs / spec.configMapRefs / spec.volumes into
// the container env, envFrom, and volume shapes via the shared projection. Keyed-ref
// env rides in ExtraEnv, which renders AFTER the token SecretEnv (last-duplicate-wins)
// — so the operator-reserved names are filtered here too: a keyed ref must not be
// able to redirect the token wiring to a different Secret. Whole-ref envFrom needs no
// filter: explicit env entries always take precedence over envFrom in Kubernetes.
func refProjections(px *otilmv1alpha1.Proxy) (extraEnv []corev1.EnvVar, envFrom []corev1.EnvFromSource, volumes []corev1.Volume, volumeMounts []corev1.VolumeMount) {
	appendRef := func(env []corev1.EnvVar, ef []corev1.EnvFromSource, vols []corev1.Volume, vms []corev1.VolumeMount) {
		for _, e := range env {
			if e.Name == EnvConfigToken || e.Name == EnvTokenSigningKey {
				continue // reserved operator wiring
			}
			extraEnv = append(extraEnv, e)
		}
		envFrom = append(envFrom, ef...)
		volumes = append(volumes, vols...)
		volumeMounts = append(volumeMounts, vms...)
	}
	for i := range px.Spec.SecretRefs {
		b := common.BuildSecretRef(&px.Spec.SecretRefs[i])
		appendRef(b.Env, b.EnvFrom, b.Volumes, b.VolumeMounts)
	}
	for i := range px.Spec.ConfigMapRefs {
		b := common.BuildConfigMapRef(&px.Spec.ConfigMapRefs[i])
		appendRef(b.Env, b.EnvFrom, b.Volumes, b.VolumeMounts)
	}
	for _, v := range px.Spec.Volumes {
		vol, vm := common.BuildEphemeralVolume(v)
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, vm)
	}
	return extraEnv, envFrom, volumes, volumeMounts
}

// resolveImage resolves the proxy image: spec.image override fields win, missing
// name/tag fill from the operator's default BOM bundle, and registry/repository
// default to the published coordinates.
func resolveImage(px *otilmv1alpha1.Proxy) (string, corev1.PullPolicy) {
	override := otilmv1alpha1.ImageSpec{}
	if px.Spec.Image != nil {
		override = *px.Spec.Image
	}
	shared := otilmv1alpha1.ImageSpec{
		Registry:   bom.DefaultImageRegistry,
		Repository: bom.DefaultImageRepository,
	}
	bundle, _ := bom.BundleFor(bom.DefaultVersion)
	return common.ResolveImage(bundle.Lookup, bom.ComponentProxy, shared, override)
}

// ResolvedVersion returns the proxy image tag the builder will render — the
// spec.image override when set, otherwise the default bundle's proxy tag. The
// controller surfaces it as status.observedVersion.
func ResolvedVersion(px *otilmv1alpha1.Proxy) string {
	if px.Spec.Image != nil && px.Spec.Image.Tag != "" {
		return px.Spec.Image.Tag
	}
	bundle, _ := bom.BundleFor(bom.DefaultVersion)
	if img, ok := bundle.Lookup(bom.ComponentProxy); ok {
		return img.Tag
	}
	return ""
}

type probeType int

const (
	livenessProbe probeType = iota
	readinessProbe
	startupProbe
)

// buildProbe renders one probe; defaults match the proxy binary's endpoints
// (/health, /ready on the HTTP port). An explicit empty path disables the probe.
func buildProbe(px *otilmv1alpha1.Proxy, pt probeType) *corev1.Probe {
	var cfg *otilmv1alpha1.ProbeConfig
	if px.Spec.Probes != nil {
		switch pt {
		case livenessProbe:
			cfg = px.Spec.Probes.Liveness
		case readinessProbe:
			cfg = px.Spec.Probes.Readiness
		case startupProbe:
			cfg = px.Spec.Probes.Startup
		}
	}
	if cfg == nil {
		cfg = defaultProbeConfig(pt)
	}
	if cfg.Path == "" {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: cfg.Path, Port: intstr.FromInt32(HTTPPort)},
		},
		InitialDelaySeconds: cfg.InitialDelaySeconds,
		PeriodSeconds:       cfg.PeriodSeconds,
		FailureThreshold:    cfg.FailureThreshold,
	}
}

func defaultProbeConfig(pt probeType) *otilmv1alpha1.ProbeConfig {
	switch pt {
	case livenessProbe:
		return &otilmv1alpha1.ProbeConfig{Path: "/health", InitialDelaySeconds: 10, PeriodSeconds: 30, FailureThreshold: 3}
	case readinessProbe:
		return &otilmv1alpha1.ProbeConfig{Path: "/ready", InitialDelaySeconds: 5, PeriodSeconds: 10, FailureThreshold: 3}
	case startupProbe:
		return &otilmv1alpha1.ProbeConfig{Path: "/health", PeriodSeconds: 10, FailureThreshold: 30}
	default:
		return &otilmv1alpha1.ProbeConfig{}
	}
}
