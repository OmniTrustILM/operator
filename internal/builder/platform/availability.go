/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// haDefaultReplicas is the replica count the HA profile assigns to a stateless component
// that sets neither replicas nor autoscaling. Two is the smallest count that survives a
// single-pod voluntary disruption (the default PDB minAvailable=1 then still permits one
// node to drain), which is the point of the profile.
const haDefaultReplicas int32 = 2

// hostnameTopologyKey is the node-identity topology key the HA anti-affinity spreads
// replicas across (one well-known label every node carries).
const hostnameTopologyKey = "kubernetes.io/hostname"

// statelessComponents is the set of platform component roles the HA profile applies its
// defaults to. They are the stateless workloads (Deployments) the operator owns; stateful
// MANAGED infrastructure (CloudNativePG / RabbitMQ / Keycloak) is deliberately absent —
// its availability is the upstream operators' concern, configured via each managed block's
// own replica/instance count.
var statelessComponents = map[string]bool{
	"core":              true,
	"auth":              true,
	"scheduler":         true,
	"fe-administrator":  true,
	"auth-opa-policies": true,
	"utils":             true,
	gatewayName:         true,
}

// highAvailabilityEnabled reports whether the platform's HA profile is on.
func highAvailabilityEnabled(p *otilmv1alpha1.Platform) bool {
	return p.Spec.HighAvailability != nil && p.Spec.HighAvailability.Enabled
}

// applyHADefaults fills the HA-profile defaults into a component's effective ComponentSpec
// when the platform's spec.highAvailability is enabled AND the component is one of the
// stateless workloads the profile covers. It is OVERRIDE-SAFE: it only fills fields the
// user left unset, so an explicit per-component replicas / podDisruptionBudget / affinity
// always wins. When HA is off, the component is stateful/managed, or every field is already
// set, the spec is returned unchanged.
//
// The HA defaults, each applied only when the corresponding field is unset:
//   - replicas → a sane multi-replica count, but ONLY when the component also has no
//     autoscaling (an HPA owns scaling, so a static replica default would be meaningless
//     and would be omitted from the Deployment anyway).
//   - podDisruptionBudget → enabled with minAvailable=1 (tolerate losing all but one pod).
//   - affinity → preferred pod anti-affinity spreading the component's replicas across
//     nodes (by app.kubernetes.io/name, topology kubernetes.io/hostname).
func applyHADefaults(p *otilmv1alpha1.Platform, componentName string, spec otilmv1alpha1.ComponentSpec) otilmv1alpha1.ComponentSpec {
	if !highAvailabilityEnabled(p) || !statelessComponents[componentName] {
		return spec
	}

	// Replicas: default to the HA count only when the user set neither replicas nor
	// autoscaling. Under autoscaling the HPA owns the count (and the Deployment omits
	// .spec.replicas), so a static default must not be injected.
	if spec.Replicas == nil && spec.Autoscaling == nil {
		r := haDefaultReplicas
		spec.Replicas = &r
	}

	// PodDisruptionBudget: default to a minAvailable=1 budget when the user set none.
	if spec.PodDisruptionBudget == nil {
		minAvailable := intstr.FromInt32(1)
		spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true, MinAvailable: &minAvailable}
	}

	// Affinity: default to spread anti-affinity when the user set no affinity at all. We do
	// not merge into a user-supplied Affinity — an explicit affinity is taken as the user
	// owning the component's scheduling wholesale (the design says the component's explicit
	// affinity wins).
	if spec.Affinity == nil {
		spec.Affinity = haAntiAffinity(componentName)
	}

	return spec
}

// haAntiAffinity returns a preferred (soft) pod anti-affinity that spreads a component's
// replicas across nodes: it prefers scheduling a pod away from other pods carrying the
// same app.kubernetes.io/name, across the node-identity topology (kubernetes.io/hostname).
// Preferred (not required) so a single-node / capacity-constrained cluster still schedules.
func haAntiAffinity(componentName string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: hostnameTopologyKey,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{common.NameLabel: componentName},
						},
					},
				},
			},
		},
	}
}

// componentAvailability is a resolved component paired with the per-component
// ComponentSpec from its CR block, for the PDB/HPA render below.
type componentAvailability struct {
	component common.Component
	spec      otilmv1alpha1.ComponentSpec
}

// platformAvailabilityObjects renders the operator-owned availability children — a
// PodDisruptionBudget and/or a HorizontalPodAutoscaler per component — across the platform.
// For each component it folds in the HA-profile defaults (override-safe) onto the
// component's CR ComponentSpec, then appends a PDB when one is configured (explicitly or by
// the HA default) and an HPA when autoscaling is set. utils is included only when
// it is enabled (same gate as its workload). Both kinds are core, always-served APIs
// (policy/v1, autoscaling/v2), so neither needs capability gating.
//
// These are operator-owned children: the reconciler stamps the Platform owner reference +
// the managed-by/instance labels on apply, and adds their kinds to the prune list + RBAC,
// so a de-configured PDB/HPA is reclaimed like every other rendered child.
func platformAvailabilityObjects(p *otilmv1alpha1.Platform) []client.Object {
	candidates := []componentAvailability{
		{ResolveCore(p), p.Spec.Core.ComponentSpec},
		{ResolveScheduler(p), p.Spec.Scheduler.ComponentSpec},
		{ResolveAuthOpaPolicies(p), p.Spec.AuthOpaPolicies.ComponentSpec},
		{ResolveAuth(p), p.Spec.Auth.ComponentSpec},
		{ResolveFeAdministrator(p), p.Spec.FeAdministrator.ComponentSpec},
		{ResolveGateway(p), p.Spec.Gateway.ComponentSpec},
	}
	if p.Spec.Utils.Enabled {
		candidates = append(candidates, componentAvailability{ResolveUtils(p), p.Spec.Utils.ComponentSpec})
	}

	var objs []client.Object
	for _, ca := range candidates {
		spec := applyHADefaults(p, ca.component.Name, ca.spec)
		if pdb := common.BuildPodDisruptionBudget(ca.component, spec.PodDisruptionBudget); pdb != nil {
			objs = append(objs, pdb)
		}
		if hpa := common.BuildHorizontalPodAutoscaler(ca.component, spec.Autoscaling); hpa != nil {
			objs = append(objs, hpa)
		}
	}
	return objs
}
