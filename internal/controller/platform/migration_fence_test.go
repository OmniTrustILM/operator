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

// The fence's ownership behaviour is proved against a real apiserver
// (migration_fence_envtest_test.go). These unit tests cover the edges that envtest cannot
// reach cheaply: the states a crashed or half-written migration leaves behind, and the
// values the fence refuses to act on.

import (
	"context"
	"errors"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const fenceTestNS = "ns"

// fencedMigrationPlatform is a platform with a messaging migration recorded on status.upgrade and
// the given workloads already fenced.
func fencedMigrationPlatform(fenced ...otilmv1alpha1.FencedWorkload) *otilmv1alpha1.Platform {
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: fenceTestNS}}
	now := metav1.Now()
	p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
		FromVersion: platformVersion218, ToVersion: platformVersion219,
		Phase: otilmv1alpha1.MigrationPhaseFencing, StartedAt: now, PhaseStartedAt: now,
		Fenced: fenced,
	}
	return p
}

// fenceReconcilerFor builds a reconciler over a fake client holding the given objects (the
// Platform's status subresource enabled, as the real apiserver has it).
func fenceReconcilerFor(t *testing.T, p *otilmv1alpha1.Platform, objs ...client.Object) *Reconciler {
	t.Helper()
	pinMigrationInputs(p)
	s := reconcileScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(append([]client.Object{p}, objs...)...).Build()
	return &Reconciler{Client: c, Scheme: s}
}

// TestFenceWorkloadsWithoutMigration: fencing is meaningful only for a recorded migration.
// Asked to fence without one, the fence refuses rather than inventing a record — a caller
// that reached it in that state has a bug the reconcile must surface.
func TestFenceWorkloadsWithoutMigration(t *testing.T) {
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: fenceTestNS}}
	err := fenceReconcilerFor(t, p).fenceWorkloads(context.Background(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to fence")
}

// TestFenceWorkloadsRecordsEveryIntendedTarget: the fence records the COMPLETE set of producers
// it means to hold down, absent ones included.
//
// This test previously asserted the opposite — that an absent target is skipped — which is the
// hole it now covers. A skipped target is a target permanently OUTSIDE the fence: the ordinary
// render creates it on the next apply, at its configured replica count, publishing to the very
// virtual host the migration is draining, while a fence that only ever inspects its own list
// sees nothing wrong and advances. Recorded (with no count to restore, because there was no
// running workload to remember), it is fenced the moment it appears.
func TestFenceWorkloadsRecordsEveryIntendedTarget(t *testing.T) {
	p := fencedMigrationPlatform()
	sched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(4))},
	}
	r := fenceReconcilerFor(t, p, sched)

	require.NoError(t, r.fenceWorkloads(context.Background(), p))
	assert.ElementsMatch(t, []otilmv1alpha1.FencedWorkload{
		{Name: gatewayWorkloadName, Kind: kindDeployment, Absent: true},
		{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 4},
	}, p.Status.Upgrade.Fenced, "the absent api-gateway is recorded too, so it cannot escape the fence")
}

// TestFenceHoldsDownAWorkloadCreatedMidMigration is the escape the recording closes, end to end:
// a producer that did not exist when the fence was recorded is created by the ordinary render
// while the migration waits. It must be patched to zero like any other fenced workload, and the
// Fencing phase must go on waiting until it is observed stopped.
func TestFenceHoldsDownAWorkloadCreatedMidMigration(t *testing.T) {
	p := fencedMigrationPlatform()
	sched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
	}
	r := fenceReconcilerFor(t, p, sched)
	require.NoError(t, r.fenceWorkloads(context.Background(), p))

	stopped, err := r.fencedProducersStopped(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, stopped, "an absent target that is still absent has no producer to wait for")

	// The render brings the gateway up: replicas omitted (it is on the fenced list), so the
	// apiserver's default of one applies, and its pod is running.
	gateway := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
		Status:     appsv1.DeploymentStatus{Replicas: 1},
	}
	require.NoError(t, r.Create(context.Background(), gateway))

	stopped, err = r.fencedProducersStopped(context.Background(), p)
	require.NoError(t, err)
	assert.False(t, stopped, "a recreated producer holds the fence until it is observed stopped")

	require.NoError(t, r.fenceWorkloads(context.Background(), p))
	assert.Zero(t, replicasOfIn(t, r, fenceTestNS, gatewayWorkloadName),
		"the per-reconcile re-assert patches the newly created workload to zero")
}

// replicasOfIn reads a workload's .spec.replicas in the given namespace, returning -1 when unset.
func replicasOfIn(t *testing.T, r *Reconciler, namespace, name string) int32 {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: name, Namespace: namespace}, &dep))
	if dep.Spec.Replicas == nil {
		return -1
	}
	return *dep.Spec.Replicas
}

// TestRestoreWorkloadOfAnAbsentTargetWritesNoCount: an entry the fence recorded as absent has no
// replica count to write back — the render's own configured count is the truth the moment the
// entry is dropped, and inventing a zero here would fight it.
func TestRestoreWorkloadOfAnAbsentTargetWritesNoCount(t *testing.T) {
	entry := otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: kindDeployment, Absent: true}
	p := fencedMigrationPlatform(entry, otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3})
	gateway := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
	}
	r := fenceReconcilerFor(t, p, gateway)

	require.NoError(t, r.restoreWorkload(context.Background(), p, entry))
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3}},
		p.Status.Upgrade.Fenced, "the entry is dropped, which is the whole of the restore")
	assert.Equal(t, int32(2), replicasOfIn(t, r, fenceTestNS, gatewayWorkloadName),
		"whatever the render put there stands: the fence writes no count of its own")
}

// TestFenceWorkloadsPreservesRecordedCounts: re-entering after a crash must NOT re-record.
// The recorded counts are the only surviving evidence of what the platform was running, and
// by then the workloads are at zero — a second recording pass would overwrite them with
// zeroes and restore would bring nothing back up.
func TestFenceWorkloadsPreservesRecordedCounts(t *testing.T) {
	p := fencedMigrationPlatform(otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 4})
	sched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
	}
	r := fenceReconcilerFor(t, p, sched)

	require.NoError(t, r.fenceWorkloads(context.Background(), p))
	assert.Equal(t, int32(4), p.Status.Upgrade.Fenced[0].Replicas,
		"a re-entered fence keeps the count recorded by the first pass")
}

// TestEnforceMigrationFenceToleratesDeletedWorkload: a workload deleted underneath the fence
// has no producer left to hold down, so the reconcile must not fail on it.
func TestEnforceMigrationFenceToleratesDeletedWorkload(t *testing.T) {
	p := fencedMigrationPlatform(otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 2})
	r := fenceReconcilerFor(t, p)
	assert.NoError(t, r.enforceMigrationFence(context.Background(), p))
}

// TestEnforceMigrationFenceRejectsUnknownKind: the kind comes off the Platform's status, so an
// unrecognised value is refused outright — silently treating it as a Deployment could leave
// the real producer running while the migration believed it was stopped.
func TestEnforceMigrationFenceRejectsUnknownKind(t *testing.T) {
	p := fencedMigrationPlatform(otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: "CronJob", Replicas: 2})
	err := fenceReconcilerFor(t, p).enforceMigrationFence(context.Background(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CronJob")
}

// TestEnforceMigrationFenceInactive: with nothing fenced (and with no migration at all) the
// per-reconcile re-assert is a no-op — the steady state of every platform.
func TestEnforceMigrationFenceInactive(t *testing.T) {
	plain := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: fenceTestNS}}
	assert.NoError(t, fenceReconcilerFor(t, plain).enforceMigrationFence(context.Background(), plain))

	empty := fencedMigrationPlatform()
	assert.NoError(t, fenceReconcilerFor(t, empty).enforceMigrationFence(context.Background(), empty))
}

// TestRestoreWorkloadIsIdempotent: restoring an entry that is already gone from the list is a
// no-op, so a caller re-entering after a crash between the patch and the status write can
// simply call it again.
func TestRestoreWorkloadIsIdempotent(t *testing.T) {
	p := fencedMigrationPlatform(otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: kindDeployment, Replicas: 1})
	sched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerWorkloadName, Namespace: fenceTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
	}
	r := fenceReconcilerFor(t, p, sched)

	// scheduler is NOT in the list any more: a repeat of a completed restore.
	restored := otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3}
	require.NoError(t, r.restoreWorkload(context.Background(), p, restored))
	assert.Len(t, p.Status.Upgrade.Fenced, 1, "the remaining entry is untouched")

	// The patch still runs (it is the idempotent half), so the recorded count is re-asserted.
	var got appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sched), &got))
	require.NotNil(t, got.Spec.Replicas)
	assert.Equal(t, int32(3), *got.Spec.Replicas)
}

// TestRestoreWorkloadRemovesExactlyOneEntry: the fenced list is the migration's state, so a
// restore must account for precisely the workload it restored — one entry gone, every other
// entry and its recorded count untouched.
func TestRestoreWorkloadRemovesExactlyOneEntry(t *testing.T) {
	p := fencedMigrationPlatform(
		otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: kindDeployment, Replicas: 2},
		otilmv1alpha1.FencedWorkload{Name: "provisioning", Kind: kindDeployment, Replicas: 1},
		otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3},
	)
	r := fenceReconcilerFor(t, p, fencedWorkloadObjects()...)

	require.NoError(t, r.restoreWorkload(context.Background(), p, p.Status.Upgrade.Fenced[1]))
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{
		{Name: gatewayWorkloadName, Kind: kindDeployment, Replicas: 2},
		{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3},
	}, p.Status.Upgrade.Fenced)
	assert.Equal(t, int32(1), replicasOf(t, r, "provisioning"))
	assert.Zero(t, replicasOf(t, r, gatewayWorkloadName), "the others stay down")
	assert.Zero(t, replicasOf(t, r, schedulerWorkloadName))
}

// TestRestoreWorkloadReEntersAfterAFailedWrite is the crash contract of a single restore. The
// patch lands and the status write does not, so the entry is still listed — which is what keeps
// the render omitting replicas and the fence re-asserting behind it. Re-entering repeats the
// patch (idempotent) and finishes the job.
func TestRestoreWorkloadReEntersAfterAFailedWrite(t *testing.T) {
	p := fencedMigrationPlatform(otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3})
	crash := true
	r, _ := migrationReconciler(t, p, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if crash {
				return errors.New(errStatusWriteRejected)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, fencedWorkloadObjects()...)

	entry := p.Status.Upgrade.Fenced[0]
	require.Error(t, r.restoreWorkload(context.Background(), p, entry))
	assert.Equal(t, int32(3), replicasOf(t, r, schedulerWorkloadName), "the workload is back up")
	assert.Len(t, storedFenced(t, r), 1, "but the fence still lists it, so it is restored again if this pass is lost")

	crash = false
	require.NoError(t, r.restoreWorkload(context.Background(), storedPlatform(t, r), entry))
	assert.Empty(t, storedFenced(t, r))
	assert.Equal(t, int32(3), replicasOf(t, r, schedulerWorkloadName))
}

// TestLiftMigrationFenceResumesAfterAPartialRestore is the re-entrancy claim the whole
// conclusion rests on: a crash between two restores leaves the workloads already released
// released, and the fenced list naming exactly the ones that are not. Re-entering finishes from
// there — nothing is restored twice to a stale count, and nothing is left at zero with no record
// that it should come back up.
func TestLiftMigrationFenceResumesAfterAPartialRestore(t *testing.T) {
	p := fencedMigrationPlatform(
		otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: kindDeployment, Replicas: 2},
		otilmv1alpha1.FencedWorkload{Name: "provisioning", Kind: kindDeployment, Replicas: 1},
		otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3},
	)
	writes := 0
	crash := true
	r, _ := migrationReconciler(t, p, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			if crash && writes == 2 {
				return errors.New(errStatusWriteRejected)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, fencedWorkloadObjects()...)

	require.Error(t, r.liftMigrationFence(context.Background(), p), "the pass stops at the write it could not make")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{
		{Name: "provisioning", Kind: kindDeployment, Replicas: 1},
		{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3},
	}, storedFenced(t, r), "only the restore that was persisted is accounted for")
	assert.Equal(t, int32(2), replicasOf(t, r, gatewayWorkloadName))
	assert.Zero(t, replicasOf(t, r, schedulerWorkloadName), "the restores past the failure never ran")

	// The restart: a fresh read of what survived, and the same call again.
	crash = false
	resumed := storedPlatform(t, r)
	require.NoError(t, r.liftMigrationFence(context.Background(), resumed))
	assert.Empty(t, storedFenced(t, r), "the fenced list is empty only once every workload is back")
	assert.Equal(t, int32(2), replicasOf(t, r, gatewayWorkloadName), "an already-restored workload keeps its count")
	assert.Equal(t, int32(1), replicasOf(t, r, "provisioning"))
	assert.Equal(t, int32(3), replicasOf(t, r, schedulerWorkloadName))
}

// TestLiftMigrationFenceIsANoOpWhenNothingIsFenced: the conclusion runs it unconditionally, so
// an empty list — and a platform with no migration at all — must simply cost nothing.
func TestLiftMigrationFenceIsANoOpWhenNothingIsFenced(t *testing.T) {
	empty := fencedMigrationPlatform()
	assert.NoError(t, fenceReconcilerFor(t, empty).liftMigrationFence(context.Background(), empty))

	plain := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: fenceTestNS}}
	assert.NoError(t, fenceReconcilerFor(t, plain).liftMigrationFence(context.Background(), plain))
}

// fencedWorkloadObjects are the three producers the restore tests hold at zero replicas.
func fencedWorkloadObjects() []client.Object {
	names := []string{gatewayWorkloadName, "provisioning", schedulerWorkloadName}
	objs := make([]client.Object, 0, len(names))
	for _, name := range names {
		objs = append(objs, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fenceTestNS},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
		})
	}
	return objs
}

// storedFenced returns the PERSISTED fence entries — what a resume would start from, rather than
// the in-memory copy a failed pass left behind.
func storedFenced(t *testing.T, r *Reconciler) []otilmv1alpha1.FencedWorkload {
	t.Helper()
	u := storedPlatform(t, r).Status.Upgrade
	require.NotNil(t, u)
	return u.Fenced
}

// TestRestoreWorkloadWithoutMigration: nothing is recorded, so there is nothing to restore.
func TestRestoreWorkloadWithoutMigration(t *testing.T) {
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: fenceTestNS}}
	r := fenceReconcilerFor(t, p)
	assert.NoError(t, r.restoreWorkload(context.Background(), p,
		otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 3}))
}

// TestWorkloadReplicaCountDefaults: an unset .spec.replicas means the apiserver's default of
// one for both workload kinds, so that is what the fence records and restore writes back.
func TestWorkloadReplicaCountDefaults(t *testing.T) {
	assert.Equal(t, int32(1), workloadReplicaCount(&appsv1.Deployment{}))
	assert.Equal(t, int32(1), workloadReplicaCount(&appsv1.StatefulSet{}))
	assert.Equal(t, int32(5), workloadReplicaCount(&appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(5))},
	}))
}

// TestWorkloadObjectKinds pins the kinds the fence can address, and that anything else is an
// explicit error rather than a silent default.
func TestWorkloadObjectKinds(t *testing.T) {
	dep, err := workloadObject(kindDeployment)
	require.NoError(t, err)
	assert.IsType(t, &appsv1.Deployment{}, dep)

	sts, err := workloadObject(kindStatefulSet)
	require.NoError(t, err)
	assert.IsType(t, &appsv1.StatefulSet{}, sts)

	_, err = workloadObject("")
	assert.Error(t, err, "an empty kind must not fall back to Deployment")
}
