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
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// applyComponentSpec LAYERS the user-supplied per-component overrides (the shared
// ComponentSpec embedded by every platform component) onto a base Component that each
// Resolve* function has already built from the BOM + the operator's hardcoded wiring.
// Overrides never replace the operator-derived wiring wholesale: they extend it so the
// out-of-the-box render is unchanged when the spec is empty, and a set field augments
// (env appended last-wins; secret/configmap refs and volumes appended; init/sidecar
// containers appended) or overrides (replicas/resources/probes/service) the base.
//
// The image is resolved BY THE CALLER (each Resolve* already calls common.ResolveImage
// with the component name so the BOM lookup uses the right key) — this helper does not
// re-resolve the image. It is the single chokepoint every platform component shares, so the
// override semantics are identical everywhere.
//
// HA PROFILE: before applying the user's overrides, the platform-wide spec.highAvailability
// defaults are FOLDED INTO the spec (applyHADefaults) for the stateless components — but
// only into fields the user left unset, so an explicit per-component replicas/affinity/PDB
// always wins. The component's effective ComponentSpec (HA defaults + user overrides) is
// what drives both this Deployment shaping and the PDB/HPA render in RenderPlatformBase.
//
// SCC SAFETY: a user-supplied SecurityContext flows into the main container only via the
// fill-don't-replace path in common.BuildDeployment (the four SCC-critical fields stay
// hardened), and user init/sidecar containers are appended to the slices BuildDeployment
// hardens — so neither a CR securityContext nor a CR sidecar/init can weaken OpenShift
// restricted-v2.
func applyComponentSpec(p *otilmv1alpha1.Platform, c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	// spec.common is the fleet-wide base for this shared override surface. Fold common.Affinity
	// in FIRST (only when the component set none) so the precedence is per-component > common >
	// HA default: applyHADefaults below fills the HA anti-affinity ONLY when Affinity is still nil.
	if spec.Affinity == nil {
		spec.Affinity = p.Spec.Common.Affinity
	}

	// Fold in the HA-profile defaults next (override-safe: only fills fields the user left
	// unset), so the per-component overrides below still win. c.Name is the component role.
	spec = applyHADefaults(p, c.Name, spec)

	applyScalingOverrides(p, c, spec)

	// Env: APPEND user env AFTER whatever the operator has already put in c.Env.
	// buildContainerEnv renders c.Env, then c.SecretEnv, then c.ConfigMapEnv, then
	// c.ExtraEnv, then c.FieldRefEnv — that FIXED slice order, not Go append order, decides
	// who wins a duplicate name (Kubernetes container-env semantics: last one in the
	// rendered list wins). That is THREE precedence tiers, not one:
	//   - a PLAIN-VALUE operator default (one the operator itself appended to c.Env, e.g.
	//     Core's explicit PLATFORM_INSTANCE_ID) loses to a user spec.env entry of the same
	//     name, because the user's entry lands in the SAME slice, after it.
	//   - a user's KEYED Secret/ConfigMap ref (c.ExtraEnv, appended by
	//     applyRefAndVolumeOverrides below) beats both of those and every reference-derived
	//     operator var the operator sourced via c.SecretEnv or c.ConfigMapEnv — overriding a
	//     wired default is what that surface exists for.
	//   - a DOWNWARD-API var (c.FieldRefEnv, e.g. the pod-index-derived
	//     PLATFORM_INSTANCE_ID) beats EVERYTHING, user sources included, because it is the
	//     pod's own identity rather than configuration: a StatefulSet Core's instance id IS
	//     its ordinal, and one shared value across replicas would collide their certificate
	//     serial numbers. spec.core.instanceId is the supported way to supply one, and it is
	//     validated against a multi-replica core.
	for _, e := range spec.Env {
		c.Env = append(c.Env, common.EnvPair{Name: e.Name, Value: e.Value})
	}

	applyRefAndVolumeOverrides(c, spec)

	// Probes: override the operator defaults when the user supplies a probe block.
	if spec.Probes != nil {
		applyProbeOverrides(c, spec.Probes)
	}

	applySecurityContextOverride(c, spec)
	applyPodMetadataAndScheduling(p, c, spec)

	// Init containers / sidecars: APPEND the user's containers; BuildDeployment hardens
	// every init/sidecar container (fill-don't-replace), so a user container is still
	// SCC-clean.
	c.InitContainers = append(c.InitContainers, spec.InitContainers...)
	c.Sidecars = append(c.Sidecars, spec.Sidecars...)

	applyServiceAccountAndServiceOverrides(c, spec)
}

// applyScalingOverrides layers the user's scaling-related overrides (replicas, workload
// kind, autoscaling, resources) onto the component, and applies the two cases where an
// EXTERNAL actor owns .spec.replicas and the operator must therefore stop sending it: a
// configured HPA, and the messaging-migration fence.
func applyScalingOverrides(p *otilmv1alpha1.Platform, c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	// Replicas: override when set. Ignored when Autoscaling owns scaling (handled below),
	// where the HPA owns the replica count and the Deployment omits .spec.replicas.
	if spec.Replicas != nil {
		c.Replicas = *spec.Replicas
	}

	// WorkloadType: select the workload kind the render layer builds (Deployment by
	// default, StatefulSet when requested). Empty means Deployment, so an unset spec is
	// unchanged. The render layer (RenderPlatformBase) consults c.WorkloadType to pick
	// BuildDeployment vs BuildStatefulSet; both enclose the same hardened pod template.
	if spec.WorkloadType != "" {
		c.WorkloadType = spec.WorkloadType
	}

	// Autoscaling: when an HPA is configured for this component, the Deployment MUST NOT
	// set .spec.replicas — the operator's SSA field manager would otherwise clobber the
	// count the HPA writes each reconcile. The HPA object itself is rendered by the
	// render layer (RenderPlatformBase) from this same spec; here we only flip the
	// Deployment's omit-replicas flag.
	if spec.Autoscaling != nil {
		c.OmitReplicas = true
	}

	// Migration fence: while this component is listed in status.upgrade.fenced, the
	// controller holds its workload at .spec.replicas=0 under its own field manager, so the
	// render must not send replicas either — an apply carrying the configured count would
	// restart a message producer the migration has deliberately stopped. Same
	// omit-so-another-actor-owns-it mechanic as the HPA case above, keyed on MEMBERSHIP of
	// the fenced list (never on the migration phase) so it lifts the moment the controller
	// removes the entry, having first patched the recorded count back.
	if FenceOmitsReplicas(p, c.Name) {
		c.OmitReplicas = true
	}

	// Resources: override the whole block when set.
	if spec.Resources != nil {
		c.Resources = *spec.Resources
	}
}

// applyRefAndVolumeOverrides appends the user's Secret/ConfigMap references and emptyDir
// volumes onto the component.
func applyRefAndVolumeOverrides(c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	// Secret / ConfigMap references: render via the shared key-mapping logic and append.
	// type=env keyed refs become ExtraEnv (appended last so they win), whole-source refs
	// become EnvFrom; type=volume refs add a pod volume + a read-only container mount.
	for i := range spec.SecretRefs {
		b := common.BuildSecretRef(&spec.SecretRefs[i])
		appendRefBindings(c, b)
	}
	for i := range spec.ConfigMapRefs {
		b := common.BuildConfigMapRef(&spec.ConfigMapRefs[i])
		appendRefBindings(c, b)
	}

	// Volumes (emptyDir): append the user's volumes + their container mounts.
	for _, v := range spec.Volumes {
		vol, mount := buildSpecVolume(v)
		c.Volumes = append(c.Volumes, vol)
		c.VolumeMounts = append(c.VolumeMounts, mount)
	}
}

// applySecurityContextOverride feeds the user's SecurityContext into the main container,
// ALWAYS through the fill-don't-replace hardening in BuildDeployment (it merges
// c.SecurityContext and re-hardens). Translate the typed SecurityContextSpec into a
// corev1.SecurityContext; the operator's per-component ReadOnlyRootFilesystem decision
// still wins (it reflects the component's validated writable-path reality), so we do NOT
// let the CR flip it.
func applySecurityContextOverride(c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	if spec.SecurityContext == nil {
		return
	}
	sc := &corev1.SecurityContext{}
	if spec.SecurityContext.RunAsNonRoot != nil {
		sc.RunAsNonRoot = spec.SecurityContext.RunAsNonRoot
	}
	// ReadOnlyRootFilesystem from the CR is intentionally NOT applied here: c.ReadOnlyRootFilesystem
	// (set by the Resolve* function) is the validated per-component value and wins in BuildDeployment.
	c.SecurityContext = sc
}

// applyPodMetadataAndScheduling layers the user's pod metadata (annotations/labels) and
// scheduling overrides (nodeSelector/affinity/tolerations) onto the component.
func applyPodMetadataAndScheduling(p *otilmv1alpha1.Platform, c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	// Pod metadata: fleet-wide spec.common first, then the per-component block (component keys
	// win on collision). The operator-managed selector labels + config checksum are layered on
	// top later in BuildDeployment, so they always win.
	c.PodAnnotations = mergeStringMaps(c.PodAnnotations, p.Spec.Common.PodAnnotations)
	c.PodAnnotations = mergeStringMaps(c.PodAnnotations, spec.PodAnnotations)
	c.PodLabels = mergeStringMaps(c.PodLabels, p.Spec.Common.PodLabels)
	c.PodLabels = mergeStringMaps(c.PodLabels, spec.PodLabels)

	// Scheduling (passthrough to the pod spec). nodeSelector: common base MERGED with the
	// per-component map (component keys win). affinity: already resolved into spec.Affinity above
	// (per-component > common > HA default). tolerations: the UNION of common + per-component.
	c.NodeSelector = mergeStringMaps(c.NodeSelector, p.Spec.Common.NodeSelector)
	c.NodeSelector = mergeStringMaps(c.NodeSelector, spec.NodeSelector)
	if spec.Affinity != nil {
		c.Affinity = spec.Affinity
	}
	c.Tolerations = append(c.Tolerations, p.Spec.Common.Tolerations...)
	c.Tolerations = append(c.Tolerations, spec.Tolerations...)
}

// applyServiceAccountAndServiceOverrides layers the user's ServiceAccount and Service
// overrides onto the component.
func applyServiceAccountAndServiceOverrides(c *common.Component, spec otilmv1alpha1.ComponentSpec) {
	// ServiceAccount: override name and/or stamp annotations.
	if spec.ServiceAccount != nil {
		if spec.ServiceAccount.Name != nil {
			c.ServiceAccountName = *spec.ServiceAccount.Name
		}
		if len(spec.ServiceAccount.Annotations) > 0 {
			c.ServiceAccountAnnotations = spec.ServiceAccount.Annotations
		}
	}

	// Service: override port/type when set. The component's ServicePorts (multi-port
	// workloads such as the gateway) take precedence in BuildService, so a single-port
	// override of Port is ignored for those — by design.
	if spec.Service != nil {
		if spec.Service.Port != 0 {
			c.Port = spec.Service.Port
		}
		if spec.Service.Type != "" {
			c.ServiceType = corev1.ServiceType(spec.Service.Type)
		}
	}
}

// appendRefBindings appends one RefBindings result onto the component: keyed env into
// ExtraEnv (which renders after the operator's own env sources, so user refs win over
// those — but not over the downward-API entries in FieldRefEnv), whole-source env into
// EnvFrom, and volumes + their mounts.
func appendRefBindings(c *common.Component, b common.RefBindings) {
	c.ExtraEnv = append(c.ExtraEnv, b.Env...)
	c.EnvFrom = append(c.EnvFrom, b.EnvFrom...)
	c.Volumes = append(c.Volumes, b.Volumes...)
	c.VolumeMounts = append(c.VolumeMounts, b.VolumeMounts...)
}

// applyProbeOverrides translates the typed ProbeSpec into corev1 probes against the
// component's service port, overriding the operator defaults for any probe the user
// supplies (a nil sub-probe leaves the operator default in place). An empty path
// disables that probe (mirrors the Connector probe semantics).
func applyProbeOverrides(c *common.Component, spec *otilmv1alpha1.ProbeSpec) {
	port := c.Port
	build := func(cfg *otilmv1alpha1.ProbeConfig) *corev1.Probe {
		if cfg == nil {
			return nil
		}
		if cfg.Path == "" {
			return nil
		}
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: cfg.Path, Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: cfg.InitialDelaySeconds,
			PeriodSeconds:       cfg.PeriodSeconds,
			FailureThreshold:    cfg.FailureThreshold,
		}
	}
	if spec.Liveness != nil {
		c.Probes.Liveness = build(spec.Liveness)
	}
	if spec.Readiness != nil {
		c.Probes.Readiness = build(spec.Readiness)
	}
	if spec.Startup != nil {
		c.Probes.Startup = build(spec.Startup)
	}
}

// buildSpecVolume renders one VolumeSpec (emptyDir) into a pod volume + its container
// mount, mirroring the Connector's ephemeral-volume rendering.
func buildSpecVolume(v otilmv1alpha1.VolumeSpec) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{Name: v.Name}
	emptyDir := &corev1.EmptyDirVolumeSource{}
	if v.EmptyDir != nil {
		if v.EmptyDir.Medium != nil {
			emptyDir.Medium = corev1.StorageMedium(*v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit != nil {
			if qty, err := resource.ParseQuantity(*v.EmptyDir.SizeLimit); err == nil {
				emptyDir.SizeLimit = &qty
			}
		}
	}
	vol.VolumeSource = corev1.VolumeSource{EmptyDir: emptyDir}
	return vol, corev1.VolumeMount{Name: v.Name, MountPath: v.MountPath}
}

// mergeStringMaps returns base overlaid by extra (extra wins on conflict). A nil/empty
// extra returns base unchanged; a nil base with a non-empty extra returns a fresh copy.
func mergeStringMaps(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
