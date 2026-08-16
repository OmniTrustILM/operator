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
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonPrometheusNotInstalled means a component requested a ServiceMonitor but the
// Prometheus operator CRD (monitoring.coreos.com) is not served by the cluster.
const ReasonPrometheusNotInstalled = "PrometheusOperatorNotInstalled"

// ServiceMonitorDependencies returns the upstream-CRD prerequisite for rendering
// per-component ServiceMonitors: the Prometheus operator's ServiceMonitor CRD
// (monitoring.coreos.com/ServiceMonitor). The controller gates the ServiceMonitors on
// this so a cluster without the Prometheus operator never fails the apply; the message
// names only the remedy. Returns nil when no component requests a ServiceMonitor (no
// dependency to assert).
func ServiceMonitorDependencies(p *otilmv1alpha1.Platform) []CRDDependency {
	if len(ResolvePlatformServiceMonitors(p)) == 0 {
		return nil
	}
	return []CRDDependency{
		{
			GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"},
			Versions:  []string{"v1"},
			Reason:    ReasonPrometheusNotInstalled,
			Message: "a component requested metrics.serviceMonitor but the Prometheus operator " +
				"(monitoring.coreos.com) is not installed; install the Prometheus operator or " +
				"disable metrics.serviceMonitor on the affected components",
		},
	}
}

// RenderPlatform returns every Kubernetes object the operator would create for
// the given Platform CR, in a stable order. It is the single source of truth for
// the operator's rendered output, shared by the controller's apply path and the tests.
//
// It is the base platform (RenderPlatformBase) followed by the gated objects: the
// managed database (ResolveManagedDatabase), the edge (ResolveEdge), and the admin
// bootstrap (ResolveAdminCertObjects). The controller applies each gated set separately
// so it can gate them on their upstream-CRD prerequisites (CloudNativePG / cert-manager /
// Gateway API) without affecting the rest of the platform; this function returns the
// combined set.
func RenderPlatform(p *otilmv1alpha1.Platform) []client.Object {
	// Default the effective shared image registry/repository (hub.omnitrustregistry.com
	// /ilm) before rendering, so an out-of-the-box CR resolves the ILM component images
	// to the public registry. The controller's Reconcile applies the same default on its
	// fetched copy; doing it here too keeps RenderPlatform (used by tests and other render
	// callers) self-contained. Idempotent — only fills empty fields.
	DefaultImageRegistry(p)
	objs := RenderPlatformBase(p)

	// Managed database (optional): the CloudNativePG Cluster (+ optional Pooler) when
	// database.mode=managed, gated on the CloudNativePG CRDs (DatabaseReady). nil for an
	// external database. The controller applies these via gateDatabase, after confirming
	// the CNPG CRDs are served, and WITHOUT a controller owner reference (deletion safety).
	objs = append(objs, ResolveManagedDatabase(p)...)

	// Managed messaging (optional): the RabbitmqCluster + the full messaging topology
	// (Vhost/Users/Permissions/Exchanges/Queues/Bindings) when messaging.mode=managed,
	// gated on the rabbitmq.com CRDs (MessagingReady). nil for an external broker. The
	// controller applies these via gateMessaging, after confirming the RabbitMQ Cluster +
	// Topology CRDs are served, and WITHOUT a controller owner reference (deletion safety).
	objs = append(objs, ResolveManagedMessaging(p)...)

	// Edge: the external Ingress (-> api-gateway consumer port) plus its cert-manager
	// TLS objects (self-signed CA chain, ACME issuer, or none for bring-your-own), or
	// the Gateway API HTTPRoute/Gateway, gated by spec.edge.enabled. ResolveEdge
	// returns nil when the edge is absent. The controller applies the edge only after
	// EdgeDependencies are confirmed served by the cluster.
	objs = append(objs, ResolveEdge(p)...)

	// Admin bootstrap (optional): the cert-manager Certificate (and possibly an admin
	// CA chain) that issues the admin client cert into admin-certificate-secret, gated
	// by spec.registerAdmin.enabled && source=generated. ResolveAdminCertObjects
	// returns nil otherwise. The controller applies these only after
	// AdminCertDependencies are confirmed served (same gating as the edge).
	objs = append(objs, ResolveAdminCertObjects(p)...)

	// Per-component ServiceMonitors (optional): one ServiceMonitor per component whose
	// spec.<component>.metrics.serviceMonitor is enabled, gated on the Prometheus operator
	// CRD (monitoring.coreos.com). nil when no component opts in. The controller applies
	// these via a dedicated gate (ServiceMonitorsReady) so a cluster without the Prometheus
	// operator never fails the apply.
	objs = append(objs, ResolvePlatformServiceMonitors(p)...)

	return objs
}

// componentMetrics pairs a resolved Component with the user MetricsSpec from its CR
// block, for the per-component ServiceMonitor builder.
type componentMetrics struct {
	component common.Component
	metrics   *otilmv1alpha1.MetricsSpec
}

// ResolvePlatformServiceMonitors returns one Prometheus ServiceMonitor per platform
// component whose metrics ServiceMonitor is enabled (spec.<component>.metrics.
// serviceMonitor.enabled). The operator emits a precise per-component ServiceMonitor
// selecting that component's pods on its "http" port. The utils ServiceMonitor is
// rendered only when utils itself is enabled (same gate as the workload). Returns
// nil when no component opts in.
func ResolvePlatformServiceMonitors(p *otilmv1alpha1.Platform) []client.Object {
	candidates := []componentMetrics{
		{ResolveCore(p), p.Spec.Core.Metrics},
		{ResolveScheduler(p), p.Spec.Scheduler.Metrics},
		{ResolveAuthOpaPolicies(p), p.Spec.AuthOpaPolicies.Metrics},
		{ResolveAuth(p), p.Spec.Auth.Metrics},
		{ResolveFeAdministrator(p), p.Spec.FeAdministrator.Metrics},
		{ResolveGateway(p), p.Spec.Gateway.Metrics},
	}
	if p.Spec.Utils.Enabled {
		candidates = append(candidates, componentMetrics{ResolveUtils(p), p.Spec.Utils.Metrics})
	}
	// provisioning-rabbitmq's ServiceMonitor is rendered only when the component itself is
	// (provisioning.mode=deploy), same gate as the workload.
	if ProvisioningDeploy(p) {
		candidates = append(candidates, componentMetrics{ResolveProvisioning(p), p.Spec.Provisioning.Deploy.Metrics})
	}

	var objs []client.Object
	for _, cm := range candidates {
		if sm := common.BuildServiceMonitor(cm.component, cm.metrics); sm != nil {
			objs = append(objs, sm)
		}
	}
	return objs
}

// buildWorkload renders a component's workload object, selecting the kind from the
// resolved component's WorkloadType: a StatefulSet when WorkloadType==StatefulSet, else a
// Deployment (the default for the platform's stateless components). Both kinds enclose the
// SAME hardened pod template (buildPodTemplateSpec) and carry the same name/labels/owner
// refs, so the only difference is the apps/v1 kind.
//
// A kind switch on an existing component is orchestrated STOP-BEFORE-START by the controller:
// the superseded object is deleted with foreground propagation and the new kind is withheld
// until its pods are gone, so the component is never running as two kinds at once. Expect a
// brief outage across the switch. The switch is REFUSED outright while a messaging migration is
// recorded — the fence and the switch move the same workloads and cannot see each other.
func buildWorkload(c common.Component) client.Object {
	if c.WorkloadType == otilmv1alpha1.WorkloadKindStatefulSet {
		return common.BuildStatefulSet(c)
	}
	return common.BuildDeployment(c)
}

// RenderPlatformBase returns every Kubernetes object the operator owns EXCEPT the
// edge (Ingress / Gateway API / cert-manager) objects, in a stable order. The
// controller applies this set unconditionally; the edge (ResolveEdge) is applied
// separately and only when its upstream-CRD prerequisites are present, so a missing
// cert-manager / Gateway API never blocks the rest of the platform from converging.
func RenderPlatformBase(p *otilmv1alpha1.Platform) []client.Object {
	var objs []client.Object

	// Shared config: the messaging ConfigMap publishes the broker's non-secret
	// host/port, consumed by Core and scheduler via configMapKeyRef.
	objs = append(objs, BuildMessagingConfigMap(p))

	// Core scripts ConfigMap (managed Keycloak only): holds register-internal-keycloak.sh,
	// mounted on Core and run via a lifecycle.postStart hook to wire Core's internal OIDC
	// provider IN-POD (Core's settings API is localhost-only). nil for external/absent
	// Keycloak — appended only when present so the out-of-the-box render is unchanged. A flip
	// back to external de-renders it (the controller prunes the now-stale ConfigMap).
	if scripts := BuildCoreScriptsConfigMap(p); scripts != nil {
		objs = append(objs, scripts)
	}

	// Core: ServiceAccount + Deployment + Service.
	core := withPlatformGlobals(p, ResolveCore(p))
	objs = append(objs,
		common.BuildServiceAccount(core),
		buildWorkload(core),
		common.BuildService(core),
	)

	// scheduler: ServiceAccount + Deployment + Service.
	sched := withPlatformGlobals(p, ResolveScheduler(p))
	objs = append(objs,
		common.BuildServiceAccount(sched),
		buildWorkload(sched),
		common.BuildService(sched),
	)

	// auth-opa-policies: ServiceAccount + Deployment + Service.
	// Serves the OPA bundle consumed by Core's auth-opa sidecar.
	opa := withPlatformGlobals(p, ResolveAuthOpaPolicies(p))
	objs = append(objs,
		common.BuildServiceAccount(opa),
		buildWorkload(opa),
		common.BuildService(opa),
	)

	// auth: ServiceAccount + Deployment + Service. The Deployment reads its
	// DB connection string via secretKeyRef from the operator-managed Secret the
	// controller composes (auth-db); that Secret is NOT part of the render
	// model — it is created at reconcile time from the platform DB-creds Secret.
	auth := withPlatformGlobals(p, ResolveAuth(p))
	objs = append(objs,
		common.BuildServiceAccount(auth),
		buildWorkload(auth),
		common.BuildService(auth),
	)

	// fe-administrator: ConfigMap (config.js) + ServiceAccount + Deployment + Service.
	objs = append(objs, BuildFeAdministratorConfigMap(p))
	fe := withPlatformGlobals(p, ResolveFeAdministrator(p))
	objs = append(objs,
		common.BuildServiceAccount(fe),
		buildWorkload(fe),
		common.BuildService(fe),
	)

	// utils: ServiceAccount + Deployment + Service — only when enabled.
	if p.Spec.Utils.Enabled {
		utils := withPlatformGlobals(p, ResolveUtils(p))
		objs = append(objs,
			common.BuildServiceAccount(utils),
			buildWorkload(utils),
			common.BuildService(utils),
		)
	}

	// api-gateway (Kong, DB-less): the global ConfigMap holding the declarative
	// kong.yml + ServiceAccount + Deployment + Service. The gateway reads its routing
	// and plugins from the mounted kong.yml at boot.
	//
	// RBAC: the operator's Kong is DB-less and reads its full config from the mounted
	// ConfigMap at boot, so it needs no Kubernetes API access — no Role/RoleBinding is
	// rendered for the gateway (a Kong-Ingress-Controller-style config-reload hook would
	// need one, but this gateway has no such hook).
	objs = append(objs, BuildGlobalConfigMap(p))
	gw := withPlatformGlobals(p, ResolveGateway(p))
	objs = append(objs,
		common.BuildServiceAccount(gw),
		buildWorkload(gw),
		common.BuildService(gw),
	)

	// provisioning-rabbitmq (optional): the bundled provisioning service — ServiceAccount +
	// Deployment + Service — rendered only when core.provisioning.mode=deploy (and the broker
	// is RabbitMQ). It is a native stateless ILM component (not delegated to an upstream
	// operator); Core's PROVISIONING_API_URL is pointed at this Service by the same render.
	// In external mode nothing is appended, so the out-of-the-box render is unchanged, and a
	// flip back to external de-renders these three children (the controller prunes them).
	if ProvisioningDeploy(p) {
		prov := withPlatformGlobals(p, ResolveProvisioning(p))
		objs = append(objs,
			common.BuildServiceAccount(prov),
			buildWorkload(prov),
			common.BuildService(prov),
		)
	}

	// Availability children: a per-component PodDisruptionBudget (when configured, or
	// defaulted by the HA profile) and a per-component HorizontalPodAutoscaler (when
	// autoscaling is configured). Appended last so the workloads they target are already in
	// the render set. Both are core APIs (policy/v1, autoscaling/v2) — no capability gating.
	// They are operator-owned children pruned like the rest (poddisruptionbudgets +
	// horizontalpodautoscalers are in the prune list + RBAC markers).
	objs = append(objs, platformAvailabilityObjects(p)...)

	// Network isolation: the default-deny NetworkPolicies (default ON, opt-out via
	// spec.networkPolicy.enabled=false). They deny cross-namespace/external ingress to the
	// platform's pods while leaving intra-platform traffic and the edge -> api-gateway path
	// open (egress stays permissive). networking.k8s.io/v1 is a core, always-served API —
	// no capability gating. Operator-owned children pruned like the rest (networkpolicies
	// are in the prune list + RBAC markers).
	objs = append(objs, ResolveNetworkPolicies(p)...)

	// NOTE: As the operator implements more of the platform (the shared config/RBAC
	// objects), append their builders here.

	return objs
}

// withAdditionalEnv is the single chokepoint that merges the platform-wide
// spec.additionalEnv (e.g. OTEL_SDK_DISABLED) onto a resolved component, applied
// uniformly to every component as it is rendered in RenderPlatformBase.
//
// Precedence: the global env is PREPENDED, before the component's own/derived
// env. Because BuildDeployment emits env in slice order and Kubernetes
// container-env semantics make the LAST duplicate name win, a component-specific
// variable of the same name overrides the global one. additionalEnv is for
// non-secret config only — sensitive values use the dedicated Secret references.
func withAdditionalEnv(p *otilmv1alpha1.Platform, c common.Component) common.Component {
	if len(p.Spec.AdditionalEnv) == 0 {
		return c
	}
	global := make([]common.EnvPair, 0, len(p.Spec.AdditionalEnv))
	for _, e := range p.Spec.AdditionalEnv {
		global = append(global, common.EnvPair{Name: e.Name, Value: e.Value})
	}
	c.Env = append(global, c.Env...)
	return c
}

// withPlatformGlobals is the single per-component chokepoint that layers BOTH
// platform-wide passthroughs onto a resolved component: the structured spec.common
// customization (init/sidecars/volumes/mounts/ports/envFrom) and the inline
// spec.additionalEnv variables. It is applied uniformly to every component as it is
// rendered in RenderPlatformBase, so neither passthrough can be silently skipped for a
// component. The component arrives with its operator-derived base AND its own
// per-component ComponentSpec already applied (each Resolve* runs applyComponentSpec at
// its tail); these passthroughs are global, so they are merged BEFORE the per-component
// layer (see withGlobalCustomization / withAdditionalEnv for the precedence).
func withPlatformGlobals(p *otilmv1alpha1.Platform, c common.Component) common.Component {
	return withPlatformVersion(p, withGlobalCustomization(p, withAdditionalEnv(p, c)))
}

// PlatformVersionAnnotation is the pod-template annotation every rendered component carries,
// naming the platform version bundle its pod template was rendered from.
//
// It exists because an IMAGE TAG is not a version marker: several components are pinned to the
// same image across neighbouring bundles (provisioning-rabbitmq is identical in 2.18.0 and
// 2.19.0), so "this workload runs the target's image" can be true of a workload still
// configured entirely from the source bundle — a false yes the messaging migration's staged
// cutover would act on, releasing Core onto a topology its provisioner has not moved to. The
// annotation is stamped from the SAME resolution the rest of the render uses, so it changes
// exactly when the rendered pod template does, and the cutover can require it.
const PlatformVersionAnnotation = "otilm.com/platform-version"

// withPlatformVersion stamps PlatformVersionAnnotation on a resolved component, AFTER the
// user's own pod annotations (spec.common.podAnnotations / spec.<component>.podAnnotations)
// have been merged, so the operator's marker cannot be overwritten by the CR.
func withPlatformVersion(p *otilmv1alpha1.Platform, c common.Component) common.Component {
	annotations := make(map[string]string, len(c.PodAnnotations)+1)
	for k, v := range c.PodAnnotations {
		annotations[k] = v
	}
	annotations[PlatformVersionAnnotation] = RenderedPlatformVersion(p)
	c.PodAnnotations = annotations
	return c
}

// RenderedPlatformVersion returns the platform version whose bundle this render resolves
// against: spec.version when this build carries it, else the operator's DefaultVersion — the
// same fallback resolveBundle makes, so the annotation can never name a bundle other than the
// one the images and wiring came from.
func RenderedPlatformVersion(p *otilmv1alpha1.Platform) string {
	if _, ok := bom.BundleFor(p.Spec.Version); ok && p.Spec.Version != "" {
		return p.Spec.Version
	}
	return bom.DefaultVersion
}

// withGlobalCustomization layers the fleet-wide spec.common passthrough onto a resolved
// component: the initContainers/sidecarContainers/additionalVolumes/additionalVolumeMounts/
// additionalPorts/additionalEnv.{secrets,configMaps} surface, applied to EVERY component,
// that the per-component ComponentSpec alone cannot express. An empty spec.common
// passthrough is a no-op (the out-of-the-box render is unchanged), so this is purely additive.
//
// PRECEDENCE: the global containers/volumes/mounts/ports/envFrom are PREPENDED, before the
// resolved component's own (operator-derived base + per-component ComponentSpec) entries —
// the same global-then-component ordering withAdditionalEnv uses — so a component-specific
// override layers on top of the global one. For these set-like slices (containers are a pod
// set, volumes/ports are keyed by unique name, envFrom sources have no last-wins-by-name
// semantics) the position is not load-bearing; the prepend keeps the precedence consistent
// with spec.additionalEnv.
//
// SCC SAFETY: global InitContainers and Sidecars are appended to the slices that
// common.BuildDeployment hardens (fill-AND-force restricted-v2), so a global container is
// re-hardened (RunAsNonRoot=true, drop ALL caps, no privilege escalation, seccomp
// RuntimeDefault) even if the CR set Privileged:true / RunAsNonRoot:false / capabilities.Add.
// No global path can weaken OpenShift restricted-v2.
//
// SECURITY: AdditionalEnvFrom holds Secret/ConfigMap NAMES only; they become
// envFrom.secretRef / envFrom.configMapRef on the main container — no secret value is ever
// copied into the rendered objects.
func withGlobalCustomization(p *otilmv1alpha1.Platform, c common.Component) common.Component {
	g := p.Spec.Common

	// Init / sidecar containers: PREPEND the global containers before the component's own,
	// so a per-component init/sidecar layers after the global one. BuildDeployment hardens
	// every init/sidecar container (fill-AND-force), so a global container is SCC-clean.
	c.InitContainers = append(append([]corev1.Container{}, g.InitContainers...), c.InitContainers...)
	c.Sidecars = append(append([]corev1.Container{}, g.Sidecars...), c.Sidecars...)

	// Volumes + their main-container mounts: PREPEND the global volumes/mounts.
	c.Volumes = append(append([]corev1.Volume{}, g.Volumes...), c.Volumes...)
	c.VolumeMounts = append(append([]corev1.VolumeMount{}, g.VolumeMounts...), c.VolumeMounts...)

	// Additional container ports: PREPEND the global ports.
	c.AdditionalPorts = append(append([]corev1.ContainerPort{}, g.AdditionalPorts...), c.AdditionalPorts...)

	// AdditionalEnvFrom: project each whole Secret/ConfigMap by NAME via envFrom (PREPENDED
	// before the component's own envFrom sources). Explicit container env still wins over any
	// envFrom value, so this never overrides an operator-derived or per-component env var.
	if g.AdditionalEnvFrom != nil {
		envFrom := make([]corev1.EnvFromSource, 0,
			len(g.AdditionalEnvFrom.Secrets)+len(g.AdditionalEnvFrom.ConfigMaps))
		for _, name := range g.AdditionalEnvFrom.Secrets {
			envFrom = append(envFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			})
		}
		for _, name := range g.AdditionalEnvFrom.ConfigMaps {
			envFrom = append(envFrom, corev1.EnvFromSource{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			})
		}
		c.EnvFrom = append(envFrom, c.EnvFrom...)
	}

	return c
}
