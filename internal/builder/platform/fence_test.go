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
)

// fencedPlatform returns a base platform whose status records a migration in the given phase
// with the named workloads fenced. The phase is a parameter precisely so the membership tests
// can vary it independently of the fenced list.
func fencedPlatform(phase otilmv1alpha1.MigrationPhase, names ...string) *otilmv1alpha1.Platform {
	p := basePlatform()
	u := &otilmv1alpha1.UpgradeStatus{FromVersion: "2.18.0", ToVersion: "2.19.0", Phase: phase}
	for _, n := range names {
		u.Fenced = append(u.Fenced, otilmv1alpha1.FencedWorkload{
			Name: n, Kind: string(otilmv1alpha1.WorkloadKindDeployment), Replicas: 2,
		})
	}
	p.Status.Upgrade = u
	return p
}

// TestFenceOmitsReplicasKeysOnMembership is the executable statement of the fence's central
// rule: a component omits .spec.replicas for exactly as long as it appears in
// status.upgrade.fenced, whatever phase the migration is in — and stops the moment its entry
// is removed. A phase-keyed rule would fail the CleaningUp and removed-from-list cases below.
func TestFenceOmitsReplicasKeysOnMembership(t *testing.T) {
	tests := []struct {
		name      string
		platform  *otilmv1alpha1.Platform
		component string
		want      bool
	}{
		{
			name:      "no migration recorded → nothing is fenced",
			platform:  basePlatform(),
			component: schedulerName,
			want:      false,
		},
		{
			name:      "listed while fencing → omits replicas",
			platform:  fencedPlatform(otilmv1alpha1.MigrationPhaseFencing, schedulerName, gatewayName),
			component: schedulerName,
			want:      true,
		},
		{
			name:      "not listed while fencing → keeps its replicas (core is never fenced)",
			platform:  fencedPlatform(otilmv1alpha1.MigrationPhaseFencing, schedulerName, gatewayName),
			component: coreComponentName,
			want:      false,
		},
		{
			name:      "STILL listed in a late phase → still omits replicas (membership, not phase)",
			platform:  fencedPlatform(otilmv1alpha1.MigrationPhaseCleaningUp, schedulerName),
			component: schedulerName,
			want:      true,
		},
		{
			name:      "removed from the list mid-fencing → immediately un-fenced",
			platform:  fencedPlatform(otilmv1alpha1.MigrationPhaseFencing, gatewayName),
			component: schedulerName,
			want:      false,
		},
		{
			name:      "an empty list fences nothing",
			platform:  fencedPlatform(otilmv1alpha1.MigrationPhaseDraining),
			component: gatewayName,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, FenceOmitsReplicas(tc.platform, tc.component))
		})
	}
}

// TestMigrationFenceActive asserts the cheap early-out: only a non-empty fenced list is an
// active fence, so a platform with no migration (or one whose fence has been fully lifted)
// does no per-reconcile work.
func TestMigrationFenceActive(t *testing.T) {
	assert.False(t, MigrationFenceActive(basePlatform()), "no migration recorded")
	assert.False(t, MigrationFenceActive(fencedPlatform(otilmv1alpha1.MigrationPhaseCleaningUp)),
		"a migration whose fence is fully lifted holds nothing down")
	assert.True(t, MigrationFenceActive(fencedPlatform(otilmv1alpha1.MigrationPhaseFencing, schedulerName)))
}

// TestMigrationFenceTargets pins the fenced SET: the platform's message producers, and only
// those. Core is deliberately absent — it is the consumer that drains the queues.
func TestMigrationFenceTargets(t *testing.T) {
	t.Run("the producers, as Deployments by default", func(t *testing.T) {
		got := MigrationFenceTargets(basePlatform())
		assert.Equal(t, []FenceTarget{
			{Name: gatewayName, Kind: "Deployment"},
			{Name: schedulerName, Kind: "Deployment"},
		}, got)
		for _, target := range got {
			assert.NotEqual(t, coreComponentName, target.Name, "core must keep running to drain the queues")
		}
	})

	t.Run("the gateway's effective kind is recorded (StatefulSet)", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Gateway.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
		assert.Contains(t, MigrationFenceTargets(p), FenceTarget{Name: gatewayName, Kind: "StatefulSet"})
	})

	t.Run("the bundled provisioning service is fenced only when the operator renders it", func(t *testing.T) {
		names := func(p *otilmv1alpha1.Platform) []string {
			var out []string
			for _, target := range MigrationFenceTargets(p) {
				out = append(out, target.Name)
			}
			return out
		}
		assert.NotContains(t, names(basePlatform()), provisioningName, "an external provisioner is not ours to scale")
		assert.Contains(t, names(deployProvisioningPlatform()), provisioningName)
	})
}

// TestRenderFencedComponentOmitsReplicas is the render-level consequence: a fenced component's
// workload carries NO .spec.replicas, so the operator's Server-Side Apply never re-asserts a
// count over the zero the controller patched in — while an unfenced component of the same
// render keeps its configured count.
func TestRenderFencedComponentOmitsReplicas(t *testing.T) {
	p := fencedPlatform(otilmv1alpha1.MigrationPhaseDraining, schedulerName)
	p.Spec.Scheduler.Replicas = i32Ptr(3)
	p.Spec.Core.Replicas = i32Ptr(3)

	deps := renderedDeployments(p)
	sched := deps[schedulerName]
	require.NotNil(t, sched)
	assert.Nil(t, sched.Spec.Replicas, "a fenced component must not send .spec.replicas")

	core := deps[coreComponentName]
	require.NotNil(t, core)
	require.NotNil(t, core.Spec.Replicas, "an unfenced component still owns its replica count")
	assert.Equal(t, int32(3), *core.Spec.Replicas)
}

// TestRenderFencedStatefulSetOmitsReplicas covers the StatefulSet-typed gateway: the fence
// mechanism is the workload-kind-agnostic omit-replicas flag, so the StatefulSet path must
// behave exactly like the Deployment one.
func TestRenderFencedStatefulSetOmitsReplicas(t *testing.T) {
	p := fencedPlatform(otilmv1alpha1.MigrationPhaseFencing, gatewayName)
	p.Spec.Gateway.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
	p.Spec.Gateway.Replicas = i32Ptr(2)

	var gw *appsv1.StatefulSet
	for _, o := range RenderPlatform(p) {
		if s, ok := o.(*appsv1.StatefulSet); ok && s.Name == gatewayName {
			gw = s
		}
	}
	require.NotNil(t, gw, "the gateway must render as a StatefulSet")
	assert.Nil(t, gw.Spec.Replicas, "a fenced StatefulSet must not send .spec.replicas either")
}
