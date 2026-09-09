/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pdbByName / hpaByName / deploymentByName fish a typed render object out of the full
// RenderPlatform output by name (or nil when absent).
func pdbByName(objs []client.Object, name string) *policyv1.PodDisruptionBudget {
	for _, o := range objs {
		if pdb, ok := o.(*policyv1.PodDisruptionBudget); ok && pdb.Name == name {
			return pdb
		}
	}
	return nil
}

func hpaByName(objs []client.Object, name string) *autoscalingv2.HorizontalPodAutoscaler {
	for _, o := range objs {
		if hpa, ok := o.(*autoscalingv2.HorizontalPodAutoscaler); ok && hpa.Name == name {
			return hpa
		}
	}
	return nil
}

func deploymentByName(objs []client.Object, name string) *appsv1.Deployment {
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == name {
			return d
		}
	}
	return nil
}

// countKind counts how many objects of the given typed kind are in the render set.
func countPDBs(objs []client.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := o.(*policyv1.PodDisruptionBudget); ok {
			n++
		}
	}
	return n
}

func countHPAs(objs []client.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := o.(*autoscalingv2.HorizontalPodAutoscaler); ok {
			n++
		}
	}
	return n
}

// --- baseline: nothing rendered when no availability primitives are configured ---

func TestRenderPlatformNoAvailabilityObjectsByDefault(t *testing.T) {
	objs := RenderPlatform(basePlatform())
	assert.Zero(t, countPDBs(objs), "no PDB rendered when HA off and no component PDB set")
	assert.Zero(t, countHPAs(objs), "no HPA rendered when no component autoscaling set")
	// And every Deployment carries an explicit replica count (none HPA-owned).
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok {
			assert.NotNil(t, d.Spec.Replicas, "%s Deployment should set replicas when not HPA-owned", d.Name)
		}
	}
}

// --- per-component PodDisruptionBudget ---

func TestRenderPlatformComponentPDB(t *testing.T) {
	p := basePlatform()
	minA := intstr.FromInt32(2)
	p.Spec.Core.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true, MinAvailable: &minA}

	objs := RenderPlatform(p)
	pdb := pdbByName(objs, "core")
	require.NotNil(t, pdb, "a component with podDisruptionBudget must render a PDB")
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromInt32(2), *pdb.Spec.MinAvailable)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "core", "app.kubernetes.io/instance": "ilm"},
		pdb.Spec.Selector.MatchLabels)
	// Only Core opts in: no other component PDB.
	assert.Equal(t, 1, countPDBs(objs))
}

func TestRenderPlatformComponentPDBDisabledRendersNothing(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: false}
	objs := RenderPlatform(p)
	assert.Nil(t, pdbByName(objs, "core"), "a disabled PDB renders nothing")
	assert.Zero(t, countPDBs(objs))
}

// --- per-component HorizontalPodAutoscaler + replica omission ---

func TestRenderPlatformComponentHPAOmitsDeploymentReplicas(t *testing.T) {
	p := basePlatform()
	minR := int32(2)
	cpu := int32(75)
	mem := int32(80)
	p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{
		MinReplicas: &minR, MaxReplicas: 5, TargetCPUUtilization: &cpu, TargetMemoryUtilization: &mem,
	}

	objs := RenderPlatform(p)

	// The HPA renders with the correct bounds, targets, and scaleTargetRef.
	hpa := hpaByName(objs, "core")
	require.NotNil(t, hpa, "a component with autoscaling must render an HPA")
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(5), hpa.Spec.MaxReplicas)
	assert.Equal(t, "apps/v1", hpa.Spec.ScaleTargetRef.APIVersion)
	assert.Equal(t, "Deployment", hpa.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, "core", hpa.Spec.ScaleTargetRef.Name)
	require.Len(t, hpa.Spec.Metrics, 2)

	// CRITICAL: Core's Deployment must NOT set .spec.replicas (HPA owns scaling under SSA).
	core := deploymentByName(objs, "core")
	require.NotNil(t, core)
	assert.Nil(t, core.Spec.Replicas, "HPA-owned Deployment must omit .spec.replicas")

	// A non-autoscaled component still sets replicas.
	auth := deploymentByName(objs, "auth")
	require.NotNil(t, auth)
	assert.NotNil(t, auth.Spec.Replicas, "non-HPA component keeps its replica count")

	assert.Equal(t, 1, countHPAs(objs), "only Core opts into autoscaling")
}

// --- HA profile: defaults on the stateless components ---

func TestRenderPlatformHADefaultsOnStatelessComponents(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	p.Spec.Utils.Enabled = true // include utils so its HA defaults are asserted too

	objs := RenderPlatform(p)

	// Every stateless component gets: HA replicas (2), a PDB (minAvailable 1), and pod
	// anti-affinity by app.kubernetes.io/name across kubernetes.io/hostname.
	for _, name := range []string{
		"core", "auth", "scheduler", "fe-administrator",
		"auth-opa-policies", "utils", gatewayName,
	} {
		d := deploymentByName(objs, name)
		require.NotNil(t, d, "%s Deployment must exist", name)
		require.NotNil(t, d.Spec.Replicas, "%s should have an HA replica count", name)
		assert.Equal(t, int32(2), *d.Spec.Replicas, "%s HA default replicas", name)

		pdb := pdbByName(objs, name)
		require.NotNil(t, pdb, "%s should get a default HA PDB", name)
		require.NotNil(t, pdb.Spec.MinAvailable)
		assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable, "%s HA PDB minAvailable", name)

		// Anti-affinity spreads replicas by the component label across nodes.
		require.NotNil(t, d.Spec.Template.Spec.Affinity, "%s should get anti-affinity", name)
		paa := d.Spec.Template.Spec.Affinity.PodAntiAffinity
		require.NotNil(t, paa)
		require.Len(t, paa.PreferredDuringSchedulingIgnoredDuringExecution, 1)
		term := paa.PreferredDuringSchedulingIgnoredDuringExecution[0]
		assert.Equal(t, "kubernetes.io/hostname", term.PodAffinityTerm.TopologyKey)
		assert.Equal(t, map[string]string{"app.kubernetes.io/name": name},
			term.PodAffinityTerm.LabelSelector.MatchLabels)
	}

	// One PDB per stateless component (7 with utils enabled); no HPA (HA does not autoscale).
	assert.Equal(t, 7, countPDBs(objs))
	assert.Zero(t, countHPAs(objs), "the HA profile renders no HPAs")
}

func TestRenderPlatformHAOffRendersNoAvailabilityObjects(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: false}
	objs := RenderPlatform(p)
	assert.Zero(t, countPDBs(objs), "HA disabled renders no PDB")
	assert.Zero(t, countHPAs(objs))
	core := deploymentByName(objs, "core")
	require.NotNil(t, core)
	assert.Equal(t, int32(1), *core.Spec.Replicas, "HA off keeps the single-replica default")
	assert.Nil(t, core.Spec.Template.Spec.Affinity, "HA off adds no anti-affinity")
}

// --- HA defaults are OVERRIDABLE per component ---

func TestRenderPlatformHAReplicasOverriddenByComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	five := int32(5)
	p.Spec.Core.Replicas = &five // explicit replicas wins over the HA default (2)

	objs := RenderPlatform(p)
	core := deploymentByName(objs, "core")
	require.NotNil(t, core)
	require.NotNil(t, core.Spec.Replicas)
	assert.Equal(t, int32(5), *core.Spec.Replicas, "explicit per-component replicas overrides the HA default")

	// auth still gets the HA default since it set nothing.
	auth := deploymentByName(objs, "auth")
	require.NotNil(t, auth)
	assert.Equal(t, int32(2), *auth.Spec.Replicas)
}

func TestRenderPlatformHAAffinityOverriddenByComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	// A user-supplied affinity is taken wholesale (the HA default does not merge into it).
	userAffinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "disktype", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"},
					}},
				}},
			},
		},
	}
	p.Spec.Core.Affinity = userAffinity

	objs := RenderPlatform(p)
	core := deploymentByName(objs, "core")
	require.NotNil(t, core)
	require.NotNil(t, core.Spec.Template.Spec.Affinity)
	// The user's node affinity is present and the HA anti-affinity was NOT injected.
	assert.NotNil(t, core.Spec.Template.Spec.Affinity.NodeAffinity, "explicit affinity is honoured")
	assert.Nil(t, core.Spec.Template.Spec.Affinity.PodAntiAffinity,
		"explicit affinity wins; HA anti-affinity is not merged in")
}

func TestRenderPlatformHAPDBOverriddenByComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	minA := intstr.FromString("60%")
	p.Spec.Core.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true, MinAvailable: &minA}

	objs := RenderPlatform(p)
	pdb := pdbByName(objs, "core")
	require.NotNil(t, pdb)
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromString("60%"), *pdb.Spec.MinAvailable,
		"explicit per-component PDB overrides the HA default minAvailable=1")
}

func TestRenderPlatformHAComponentDisablesPDB(t *testing.T) {
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	// An explicit disabled PDB on a component opts that component OUT of the HA default PDB.
	p.Spec.Core.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: false}

	objs := RenderPlatform(p)
	assert.Nil(t, pdbByName(objs, "core"), "an explicit disabled PDB suppresses the HA default")
	// Other stateless components still get the HA default PDB.
	assert.NotNil(t, pdbByName(objs, "auth"))
}

func TestRenderPlatformHAAutoscaledComponentOmitsReplicasAndHADefaultPDB(t *testing.T) {
	// When a component sets autoscaling AND the HA profile is on, the HPA owns scaling:
	// the Deployment omits replicas (no HA static count injected), but the HA default PDB
	// still applies (a PDB is meaningful alongside an HPA).
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	cpu := int32(75)
	p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 4, TargetCPUUtilization: &cpu}

	objs := RenderPlatform(p)
	core := deploymentByName(objs, "core")
	require.NotNil(t, core)
	assert.Nil(t, core.Spec.Replicas, "autoscaling omits replicas even under the HA profile")
	assert.NotNil(t, hpaByName(objs, "core"), "the HPA renders")
	assert.NotNil(t, pdbByName(objs, "core"), "the HA default PDB still applies alongside the HPA")
}

// --- statefulness boundary: HA must not touch the managed-infra render ---

func TestRenderPlatformHADoesNotRenderManagedInfraPDBOrHPA(t *testing.T) {
	// HA only touches the stateless component set. The managed-infra builders (CNPG /
	// RabbitMQ / Keycloak) are delegated to upstream operators, so HA renders no PDB/HPA
	// for them. This is implicitly the case (those kinds aren't in the stateless set); the
	// PDB count == stateless count assertion in the HA-defaults test guards it, and here we
	// confirm the names are exactly the stateless components.
	p := basePlatform()
	p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
	objs := RenderPlatform(p)

	pdbNames := map[string]bool{}
	for _, o := range objs {
		if pdb, ok := o.(*policyv1.PodDisruptionBudget); ok {
			pdbNames[pdb.Name] = true
		}
	}
	// utils is disabled by default, so 6 PDBs.
	assert.Len(t, pdbNames, 6)
	for name := range pdbNames {
		assert.True(t, statelessComponents[name], "PDB %q must be a stateless component", name)
	}
}
