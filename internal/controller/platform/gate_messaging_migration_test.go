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

// The phase machine's contract is "what is written down, and when". These tests exercise it
// against a fake client, where the exact number of passes and the exact moment a write fails
// are both controllable — the two things a live apiserver cannot give cheaply. The render-side
// consequences of the same machine (the source bundle really being what gets applied, and a
// restart really resuming) are proved against envtest in
// gate_messaging_migration_envtest_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

const migrationTestNS = "ns"

// unknownPlatformVersion is a version no bundle in this operator build carries — the shape of
// an operator rolled back past a version one of its platforms is still using.
const unknownPlatformVersion = "9.9.9"

// migrationGatePlatform is a managed-messaging Platform RUNNING 2.18.0 whose spec now asks for
// 2.19.0 — the move whose effective virtual host changes, i.e. the one that needs a migration.
// spec.version carries the target because resolvePlatformVersion has already pinned it there
// by the time the gate runs.
func migrationGatePlatform() *otilmv1alpha1.Platform {
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: migrationTestNS, Generation: 3}}
	p.Spec.Messaging.Mode = modeManaged
	p.Spec.Messaging.BrokerType = "rabbitmq"
	p.Spec.Messaging.Managed = &otilmv1alpha1.ManagedMessagingSpec{}
	p.Spec.Version = platformVersion219
	p.Status.ObservedVersion = platformVersion218
	return p
}

// migratingGatePlatform is migrationGatePlatform with a migration already recorded at the
// given phase and fenced set — the state a restarted operator reads back.
func migratingGatePlatform(phase otilmv1alpha1.MigrationPhase, fenced ...otilmv1alpha1.FencedWorkload) *otilmv1alpha1.Platform {
	p := migrationGatePlatform()
	now := metav1.Now()
	p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
		FromVersion: platformVersion218, ToVersion: platformVersion219, Phase: phase,
		Fenced: fenced, StartedAt: now, PhaseStartedAt: now,
	}
	return p
}

// producerWorkloads are the two fence targets a platform without a deployed provisioning
// service renders, at the counts the fence must record.
func producerWorkloads(gatewayReplicas, schedulerReplicas int32) []client.Object {
	return []client.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: migrationTestNS},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(gatewayReplicas)},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: migrationTestNS},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(schedulerReplicas)},
		},
	}
}

// migrationReconciler builds a reconciler over a fake client holding the Platform (status
// subresource enabled, as the real apiserver has it) plus the given objects, with a recording
// event sink. Optional interceptors let a test fail a specific write.
func migrationReconciler(t *testing.T, p *otilmv1alpha1.Platform, funcs interceptor.Funcs, objs ...client.Object) (*Reconciler, *record.FakeRecorder) {
	t.Helper()
	s := reconcileScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(append([]client.Object{p}, objs...)...).WithInterceptorFuncs(funcs).Build()
	rec := record.NewFakeRecorder(32)
	return &Reconciler{Client: c, Scheme: s, Recorder: rec}, rec
}

// migrationBundles resolves the source and target bundles the gate is called with.
func migrationBundles(t *testing.T) (from, to bom.Bundle) {
	t.Helper()
	return bundleFor(t, platformVersion218), bundleFor(t, platformVersion219)
}

// storedPlatform re-reads the Platform through the client, so an assertion sees what was
// PERSISTED rather than what the in-memory copy happens to hold.
func storedPlatform(t *testing.T, r *Reconciler) *otilmv1alpha1.Platform {
	t.Helper()
	var got otilmv1alpha1.Platform
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: "ilm", Namespace: migrationTestNS}, &got))
	return &got
}

// migrationCondition returns the platform's MessagingMigration condition (nil when unset).
func migrationCondition(p *otilmv1alpha1.Platform) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
}

// drainEvents collects every Event recorded so far.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// replicasOf reads a workload's .spec.replicas from the client, returning -1 when unset.
func replicasOf(t *testing.T, r *Reconciler, name string) int32 {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: name, Namespace: migrationTestNS}, &dep))
	if dep.Spec.Replicas == nil {
		return -1
	}
	return *dep.Spec.Replicas
}

// failingStatusUpdate makes every status write fail, so a test can observe what the engine
// does when the record it depends on cannot be persisted.
func failingStatusUpdate(err error) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			return err
		},
	}
}

// failingPatch makes every object patch fail, so a test can observe what the engine does when
// it cannot move a producer.
func failingPatch(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return err
		},
	}
}

// --- the crash-safety rule ---------------------------------------------------

// TestBeginMigrationPersistsBeforeAnySideEffect is the rule the whole engine rests on: the
// record exists in the cluster before the first producer is touched. It is asserted from the
// STORED object, not the in-memory one, because only the stored one survives a crash.
func TestBeginMigrationPersistsBeforeAnySideEffect(t *testing.T) {
	p := migrationGatePlatform()
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(2, 3)...)

	require.NoError(t, r.beginMigration(context.Background(), p, platformVersion219))

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the migration must be recorded in the cluster before anything is fenced")
	assert.Equal(t, platformVersion218, stored.Status.Upgrade.FromVersion)
	assert.Equal(t, platformVersion219, stored.Status.Upgrade.ToVersion)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseFencing, stored.Status.Upgrade.Phase)
	assert.False(t, stored.Status.Upgrade.StartedAt.IsZero())
	assert.Empty(t, stored.Status.Upgrade.Fenced, "nothing is fenced yet at the moment the record lands")

	// The producers are untouched: the record is written BEFORE, not alongside.
	assert.Equal(t, int32(2), replicasOf(t, r, "api-gateway"))
	assert.Equal(t, int32(3), replicasOf(t, r, "scheduler"))

	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, string(otilmv1alpha1.MigrationPhaseFencing), cond.Reason)
	assert.Equal(t, p.Generation, cond.ObservedGeneration, "conditions carry the observed generation")

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventMigrationStarted)
	assertNoBrokerCoordinates(t, cond.Message)
}

// TestBeginMigrationDropsRecordWhenTheWriteFails: a state write that fails must abort the
// reconcile with NOTHING started — neither in the cluster nor in the copy the rest of the pass
// would read.
func TestBeginMigrationDropsRecordWhenTheWriteFails(t *testing.T) {
	p := migrationGatePlatform()
	r, _ := migrationReconciler(t, p, failingStatusUpdate(errors.New("status write rejected")))

	err := r.beginMigration(context.Background(), p, platformVersion219)
	require.Error(t, err)
	assert.Nil(t, p.Status.Upgrade, "an unpersisted migration must not survive in memory either")
	assert.Nil(t, storedPlatform(t, r).Status.Upgrade)
}

// TestTransitionMigrationPhaseRollsBackWhenTheWriteFails: same rule one level down. A phase
// the cluster did not accept must not be the phase the reconcile goes on to act at.
func TestTransitionMigrationPhaseRollsBackWhenTheWriteFails(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing)
	started := p.Status.Upgrade.PhaseStartedAt
	r, rec := migrationReconciler(t, p, failingStatusUpdate(errors.New("status write rejected")))

	err := r.transitionMigrationPhase(context.Background(), p, otilmv1alpha1.MigrationPhaseDraining)
	require.Error(t, err)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseFencing, p.Status.Upgrade.Phase, "the phase must roll back")
	assert.Equal(t, started, p.Status.Upgrade.PhaseStartedAt, "so must the phase's deadline clock")
	assert.Empty(t, drainEvents(rec), "a transition that did not happen announces nothing")
}

// TestTransitionMigrationPhaseResetsThePhaseClock: each phase gets its own full budget and a
// zeroed progress counter, so a slow predecessor never eats the successor's window.
func TestTransitionMigrationPhaseResetsThePhaseClock(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	p.Status.Upgrade.CleanDrainPolls = 2
	r, rec := migrationReconciler(t, p, interceptor.Funcs{})

	require.NoError(t, r.transitionMigrationPhase(context.Background(), p, otilmv1alpha1.MigrationPhaseDraining))

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, stored.Status.Upgrade.Phase)
	assert.Zero(t, stored.Status.Upgrade.CleanDrainPolls)
	assert.WithinDuration(t, time.Now(), stored.Status.Upgrade.PhaseStartedAt.Time, time.Minute)
	assert.Equal(t, string(otilmv1alpha1.MigrationPhaseDraining), migrationCondition(stored).Reason)

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventMigrationPhase)
}

// --- the dispatch skeleton ---------------------------------------------------

// TestGateMessagingMigrationNoMigration: a move that needs no migration hands the reconcile
// straight back the version resolution's own answer, and records nothing.
func TestGateMessagingMigrationNoMigration(t *testing.T) {
	p := migrationGatePlatform()
	p.Spec.Version = platformVersion218 // no version change at all
	from, _ := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{})

	render, handled, res, err := r.gateMessagingMigration(context.Background(), p, from, platformVersion218)
	require.NoError(t, err)
	assert.False(t, handled, "the ordinary apply path must not be short-circuited")
	assert.Equal(t, platformVersion218, render.version)
	assert.False(t, render.requeue)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Nil(t, p.Status.Upgrade)
	assert.Empty(t, drainEvents(rec))
}

// TestGateMessagingMigrationStartFencesAndHoldsTheSourceVersion walks the whole entry path in
// one pass: no record → record → fence → advance to the drain → keep rendering the source.
func TestGateMessagingMigrationStartFencesAndHoldsTheSourceVersion(t *testing.T) {
	p := migrationGatePlatform()
	from, to := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(2, 3)...)

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	require.False(t, handled, "a migration in a waiting phase keeps reconciling the SOURCE version")

	assert.Equal(t, platformVersion218, render.version, "the reconcile reports the version it is still running")
	assert.Equal(t, from, render.bundle)
	assert.True(t, render.requeue, "a waiting phase asks for the migration cadence")
	assert.Equal(t, platformVersion218, p.Spec.Version,
		"the in-memory pin resolvePlatformVersion put on the TARGET must be re-pinned to the source")

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, stored.Status.Upgrade.Phase,
		"with the producers observed stopped, fencing hands over to the drain")
	assert.ElementsMatch(t, []otilmv1alpha1.FencedWorkload{
		{Name: "api-gateway", Kind: "Deployment", Replicas: 2},
		{Name: "scheduler", Kind: "Deployment", Replicas: 3},
	}, stored.Status.Upgrade.Fenced, "the counts to restore are recorded from the live workloads")

	assert.Zero(t, replicasOf(t, r, "api-gateway"))
	assert.Zero(t, replicasOf(t, r, "scheduler"))

	reasons := strings.Join(drainEvents(rec), " ")
	assert.Contains(t, reasons, eventMigrationStarted)
	assert.Contains(t, reasons, eventMigrationPhase)
	assertNoBrokerCoordinates(t, reasons)
}

// TestGateMessagingMigrationHoldsFencingWhileProducersRun: the fence patch is instantaneous,
// the producers are not. Fencing must not hand over to the drain while pods are still up —
// the drain would start counting an "empty" queue that is still being written to.
func TestGateMessagingMigrationHoldsFencingWhileProducersRun(t *testing.T) {
	p := migrationGatePlatform()
	_, to := migrationBundles(t)
	running := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: migrationTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(3))},
		Status:     appsv1.DeploymentStatus{Replicas: 3},
	}
	gateway := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: migrationTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
	}
	r, _ := migrationReconciler(t, p, interceptor.Funcs{}, running, gateway)

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.True(t, render.requeue)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseFencing, stored.Status.Upgrade.Phase,
		"the phase holds until every fenced producer's last pod is gone")
	assert.Zero(t, replicasOf(t, r, "scheduler"), "the fence still applies while the phase waits")
}

// TestGateMessagingMigrationResumesDraining: a restarted operator reads the recorded phase and
// re-enters it, still on the source version and without re-recording the fence.
func TestGateMessagingMigrationResumesDraining(t *testing.T) {
	fenced := []otilmv1alpha1.FencedWorkload{{Name: "scheduler", Kind: "Deployment", Replicas: 3}}
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fenced...)
	from, to := migrationBundles(t)
	// The workload is already at zero, as the earlier pass left it.
	sched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: migrationTestNS},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
	}
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, sched)

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, from, render.bundle, "a resumed drain still renders the source bundle")
	assert.True(t, render.requeue)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, stored.Status.Upgrade.Phase)
	assert.Equal(t, fenced, stored.Status.Upgrade.Fenced,
		"resuming must not re-record the fence — the counts are the only evidence of what was running")
	assert.Empty(t, drainEvents(rec), "re-entering a phase is not a transition and announces nothing")
}

// TestGateMessagingMigrationCleaningUpDoesNotRender: the last phase has nothing left to render —
// the platform is already serving the target — so it holds its state and re-checks on the
// migration's own cadence. (Its forward-only sibling, CuttingOver, is the phase that DOES render
// the target; messaging_migration_cutover_test.go drives it.)
func TestGateMessagingMigrationCleaningUpDoesNotRender(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseCleaningUp)
	_, to := migrationBundles(t)
	r, _ := migrationReconciler(t, p, interceptor.Funcs{})

	_, handled, res, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.True(t, handled, "the reconcile stops at the gate")
	assert.Equal(t, ctrl.Result{RequeueAfter: migrationRequeueAfter}, res)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, storedPlatform(t, r).Status.Upgrade.Phase,
		"the recorded phase is unchanged")
}

// TestGateMessagingMigrationCuttingOverKeepsTheTargetPin is the counterpart of the re-pin the
// waiting phases apply: past the drain there is nothing to hold back for, so the gate must hand
// the reconcile the TARGET bundle — and must NOT put spec.version back to the source, which
// would make every builder render the version the migration is leaving.
func TestGateMessagingMigrationCuttingOverKeepsTheTargetPin(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseCuttingOver)
	_, to := migrationBundles(t)
	r, _ := migrationReconciler(t, p, interceptor.Funcs{})

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled, "the reconcile continues — applying the target is what the cutover is for")
	assert.Equal(t, to, render.bundle, "the cutover renders the TARGET bundle")
	assert.Equal(t, platformVersion219, render.version)
	assert.Equal(t, platformVersion219, p.Spec.Version, "the source re-pin must not reach the cutover")
	assert.True(t, render.holdCore, "with no target topology declared, Core is withheld from the apply")
}

// TestGateMessagingMigrationRefusesAnUnrelatedVersion: a third version asked for mid-migration
// is a deterministic, user-correctable state — degrade and stop, with the remedy spelled out.
func TestGateMessagingMigrationRefusesAnUnrelatedVersion(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	p.Spec.Version = platformVersion217
	_, to := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{})

	_, handled, res, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion217)
	require.NoError(t, err, "a refusal is a steady state, not a reconcile failure")
	assert.True(t, handled)
	assert.Equal(t, ctrl.Result{}, res, "terminal until a spec edit re-enqueues the platform")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, stored.Status.Phase)
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMigrationInProgress, cond.Reason)
	assertNoBrokerCoordinates(t, cond.Message)
	assert.NotNil(t, stored.Status.Upgrade, "a refusal must not discard the migration it is protecting")
	assert.Contains(t, strings.Join(drainEvents(rec), " "), reasonMigrationInProgress)
}

// TestGateMessagingMigrationAbortRestoresAndClears: reverting spec.version while the migration
// is still reversible puts every producer back at its recorded count and discards the record,
// leaving the platform exactly where it started.
func TestGateMessagingMigrationAbortRestoresAndClears(t *testing.T) {
	fenced := []otilmv1alpha1.FencedWorkload{
		{Name: "api-gateway", Kind: "Deployment", Replicas: 2},
		{Name: "scheduler", Kind: "Deployment", Replicas: 3},
	}
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fenced...)
	p.Spec.Version = platformVersion218 // the user reverted
	from, _ := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, from, platformVersion218)
	require.NoError(t, err)
	assert.False(t, handled, "with the migration unwound the reconcile continues down the ordinary path")
	assert.Equal(t, platformVersion218, render.version)
	assert.False(t, render.requeue, "nothing is waiting any more")

	assert.Equal(t, int32(2), replicasOf(t, r, "api-gateway"))
	assert.Equal(t, int32(3), replicasOf(t, r, "scheduler"))

	stored := storedPlatform(t, r)
	assert.Nil(t, stored.Status.Upgrade, "the record is discarded only after every producer is back")
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationAborted, cond.Reason)
	assertNoBrokerCoordinates(t, cond.Message)
	assert.Contains(t, strings.Join(drainEvents(rec), " "), eventMigrationAborted)
}

// TestAbortMigrationKeepsTheRecordWhenTheWriteFails: the record is the only thing that says
// the producers must come back up, so a failed clear must leave it in place for the retry.
func TestAbortMigrationKeepsTheRecordWhenTheWriteFails(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing)
	r, _ := migrationReconciler(t, p, failingStatusUpdate(errors.New("status write rejected")))

	require.Error(t, r.abortMigration(context.Background(), p))
	require.NotNil(t, p.Status.Upgrade, "an unpersisted clear must not lose the record in memory")
	assert.Equal(t, platformVersion219, p.Status.Upgrade.ToVersion)
}

// --- the deadline ------------------------------------------------------------

// TestBlockMigrationLiftsTheFenceAndKeepsTheRecord: a phase that outlives its budget stops
// WAITING, not the migration. The producers come back so the platform runs its source version
// at full strength, and the record survives so the trigger cannot silently start it all again.
func TestBlockMigrationLiftsTheFenceAndKeepsTheRecord(t *testing.T) {
	fenced := []otilmv1alpha1.FencedWorkload{{Name: "scheduler", Kind: "Deployment", Replicas: 3}}
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fenced...)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	_, to := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled, "the platform keeps reconciling — on its source version")
	assert.Equal(t, platformVersion218, render.version)
	assert.False(t, render.requeue, "nothing will change until the user chooses an exit")

	assert.Equal(t, int32(3), replicasOf(t, r, "scheduler"), "the fence is lifted")

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the record survives so the migration cannot restart itself")
	assert.Empty(t, stored.Status.Upgrade.Fenced)
	assert.NotEqual(t, otilmv1alpha1.PlatformPhaseDegraded, stored.Status.Phase,
		"a platform serving its source version normally is not Degraded")
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMigrationDrainTimeout, cond.Reason)
	assert.Contains(t, cond.Message, "spec.messaging.managed.forceCutoverForVersion")
	assertNoBrokerCoordinates(t, cond.Message)

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventMigrationBlocked)

	// Re-entering the blocked state repeats neither the Event nor the restore. The next pass
	// starts from a fresh read, as Reconcile does — the source re-pin lives only in the copy
	// the previous pass held.
	_, _, _, err = r.gateMessagingMigration(context.Background(), stored, to, platformVersion219)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec), "a state already reported is not reported again on every reconcile")
}

// TestMigrationPhaseDeadlineExceeded pins which phases the deadline governs and where it is
// measured from.
func TestMigrationPhaseDeadlineExceeded(t *testing.T) {
	long := metav1.NewTime(time.Now().Add(-time.Hour))
	cases := []struct {
		name    string
		mutate  func(*otilmv1alpha1.Platform)
		want    bool
		noStart bool
	}{
		{
			name:   "a fresh phase is well inside its budget",
			mutate: func(*otilmv1alpha1.Platform) {},
		},
		{
			name:   "fencing past the budget",
			mutate: func(p *otilmv1alpha1.Platform) { p.Status.Upgrade.PhaseStartedAt = long },
			want:   true,
		},
		{
			name: "draining past the budget",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Status.Upgrade.Phase = otilmv1alpha1.MigrationPhaseDraining
				p.Status.Upgrade.PhaseStartedAt = long
			},
			want: true,
		},
		{
			name: "cutting over is forward-only, so no deadline governs it",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Status.Upgrade.Phase = otilmv1alpha1.MigrationPhaseCuttingOver
				p.Status.Upgrade.PhaseStartedAt = long
			},
		},
		{
			name: "cleaning up is forward-only too",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Status.Upgrade.Phase = otilmv1alpha1.MigrationPhaseCleaningUp
				p.Status.Upgrade.PhaseStartedAt = long
			},
		},
		{
			name: "a pinned drainTimeout shortens the window",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-2 * time.Minute))
				p.Spec.Messaging.Managed.DrainTimeout = &metav1.Duration{Duration: time.Minute}
			},
			want: true,
		},
		{
			name:   "an unset phase clock never expires",
			mutate: func(p *otilmv1alpha1.Platform) { p.Status.Upgrade.PhaseStartedAt = metav1.Time{} },
		},
		{
			name:    "no migration, no deadline",
			mutate:  func(*otilmv1alpha1.Platform) {},
			noStart: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing)
			if tc.noStart {
				p.Status.Upgrade = nil
			}
			tc.mutate(p)
			assert.Equal(t, tc.want, migrationPhaseDeadlineExceeded(p, time.Now()))
		})
	}
}

// TestMigrationDrainTimeoutFallsBackToTheCRDDefault: an object built without the CRD's
// defaulting still gets the documented window rather than a zero one.
func TestMigrationDrainTimeoutFallsBackToTheCRDDefault(t *testing.T) {
	p := migrationGatePlatform()
	assert.Equal(t, defaultMigrationDrainTimeout, migrationDrainTimeout(p))

	p.Spec.Messaging.Managed.DrainTimeout = &metav1.Duration{Duration: 90 * time.Second}
	assert.Equal(t, 90*time.Second, migrationDrainTimeout(p))

	p.Spec.Messaging.Managed = nil
	assert.Equal(t, defaultMigrationDrainTimeout, migrationDrainTimeout(p))
}

// --- the workload-kind guard -------------------------------------------------

// TestMigrationWorkloadKindFlip: flipping a fenced component's workloadType would bring the
// other kind up outside the fence, at the apiserver's default of one replica, publishing to
// the very virtual host the migration is draining. The engine must SEE that.
func TestMigrationWorkloadKindFlip(t *testing.T) {
	fenced := otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "Deployment", Replicas: 2}

	t.Run("no migration", func(t *testing.T) {
		p := migrationGatePlatform()
		_, _, _, flipped := migrationWorkloadKindFlip(p)
		assert.False(t, flipped)
	})

	t.Run("kinds agree", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fenced)
		_, _, _, flipped := migrationWorkloadKindFlip(p)
		assert.False(t, flipped)
	})

	t.Run("the fenced component is now rendered as the other kind", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fenced)
		p.Spec.Gateway.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
		name, recorded, requested, flipped := migrationWorkloadKindFlip(p)
		assert.True(t, flipped)
		assert.Equal(t, "api-gateway", name)
		assert.Equal(t, string(otilmv1alpha1.WorkloadKindDeployment), recorded)
		assert.Equal(t, string(otilmv1alpha1.WorkloadKindStatefulSet), requested)
	})

	t.Run("an entry that is no longer a fence target is left alone", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining,
			otilmv1alpha1.FencedWorkload{Name: "provisioning-rabbitmq", Kind: "StatefulSet", Replicas: 1})
		_, _, _, flipped := migrationWorkloadKindFlip(p)
		assert.False(t, flipped, "a component the spec no longer deploys cannot be flipped")
	})
}

// TestGateMessagingMigrationRefusesAWorkloadKindFlip: the guard refuses the flip outright and
// stops the pass, so the other-kind workload is never rendered in the first place.
func TestGateMessagingMigrationRefusesAWorkloadKindFlip(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining,
		otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3})
	p.Spec.Scheduler.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
	_, to := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{})

	_, handled, res, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err, "a refused spec change is a steady state, not a reconcile failure")
	assert.True(t, handled, "nothing may render while the fence and the spec disagree")
	assert.Equal(t, ctrl.Result{}, res)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, stored.Status.Phase)
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationWorkloadKindChanged, cond.Reason)
	assert.Contains(t, cond.Message, "scheduler")
	assert.Contains(t, cond.Message, string(otilmv1alpha1.WorkloadKindStatefulSet))
	assertNoBrokerCoordinates(t, cond.Message)
	assert.Equal(t, fenced(stored), []otilmv1alpha1.FencedWorkload{
		{Name: "scheduler", Kind: "Deployment", Replicas: 3},
	}, "the refusal changes nothing about the fence")
	assert.Contains(t, strings.Join(drainEvents(rec), " "), reasonMigrationWorkloadKindChanged)
}

// fenced returns the platform's recorded fence entries (nil when no migration is recorded).
func fenced(p *otilmv1alpha1.Platform) []otilmv1alpha1.FencedWorkload {
	if p.Status.Upgrade == nil {
		return nil
	}
	return p.Status.Upgrade.Fenced
}

// --- the pieces the phases lean on -------------------------------------------

// TestFencedProducersStopped reads the OBSERVED replica count, not the zero the fence wrote:
// the two differ for exactly as long as a producer's last pod is still publishing.
func TestFencedProducersStopped(t *testing.T) {
	entry := otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3}

	t.Run("pods still running", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
		sched := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: migrationTestNS},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
			Status:     appsv1.DeploymentStatus{Replicas: 1},
		}
		r, _ := migrationReconciler(t, p, interceptor.Funcs{}, sched)
		stopped, err := r.fencedProducersStopped(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, stopped)
	})

	t.Run("the last pod is gone", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
		sched := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: migrationTestNS},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(0))},
		}
		r, _ := migrationReconciler(t, p, interceptor.Funcs{}, sched)
		stopped, err := r.fencedProducersStopped(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, stopped)
	})

	t.Run("a workload deleted underneath the fence has no producer left", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
		r, _ := migrationReconciler(t, p, interceptor.Funcs{})
		stopped, err := r.fencedProducersStopped(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, stopped)
	})

	t.Run("a StatefulSet is read the same way", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing,
			otilmv1alpha1.FencedWorkload{Name: "api-gateway", Kind: "StatefulSet", Replicas: 2})
		gw := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: migrationTestNS},
			Status:     appsv1.StatefulSetStatus{Replicas: 2},
		}
		r, _ := migrationReconciler(t, p, interceptor.Funcs{}, gw)
		stopped, err := r.fencedProducersStopped(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, stopped)
	})

	t.Run("an unrecognised recorded kind is an explicit error", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing,
			otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "CronJob", Replicas: 1})
		r, _ := migrationReconciler(t, p, interceptor.Funcs{})
		_, err := r.fencedProducersStopped(context.Background(), p)
		assert.Error(t, err)
	})

	t.Run("no migration recorded", func(t *testing.T) {
		p := migrationGatePlatform()
		r, _ := migrationReconciler(t, p, interceptor.Funcs{})
		stopped, err := r.fencedProducersStopped(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, stopped)
	})
}

// TestMigrationSourceRenderRejectsAnUnknownSourceVersion: an operator rolled back to a build
// that no longer carries the version a migration started from cannot render it, and must say
// so rather than silently fall back to its own default bundle.
func TestMigrationSourceRenderRejectsAnUnknownSourceVersion(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	p.Status.Upgrade.FromVersion = unknownPlatformVersion
	_, err := migrationSourceRender(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), unknownPlatformVersion)

	p.Status.Upgrade.FromVersion = ""
	_, err = migrationSourceRender(p)
	assert.Error(t, err, "an empty version must not resolve to the operator's default bundle")
}

// TestGateMessagingMigrationDegradesOnAnUnknownSourceVersion: the same case seen from the
// gate — the reconcile stops rather than render a bundle nobody asked for.
func TestGateMessagingMigrationDegradesOnAnUnknownSourceVersion(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	p.Status.Upgrade.FromVersion = unknownPlatformVersion
	_, to := migrationBundles(t)
	r, _ := migrationReconciler(t, p, interceptor.Funcs{})

	_, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, storedPlatform(t, r).Status.Phase)
}

// TestGateMessagingMigrationIgnoresAnUnknownRunningVersion: with no migration recorded, a
// running version this build does not carry simply cannot start one — the gate steps aside
// rather than inventing a decision from a bundle it had to guess.
func TestGateMessagingMigrationIgnoresAnUnknownRunningVersion(t *testing.T) {
	p := migrationGatePlatform()
	p.Status.ObservedVersion = unknownPlatformVersion
	_, to := migrationBundles(t)
	r, _ := migrationReconciler(t, p, interceptor.Funcs{})

	render, handled, _, err := r.gateMessagingMigration(context.Background(), p, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, platformVersion219, render.version)
	assert.Nil(t, p.Status.Upgrade)
}

// --- the failure paths -------------------------------------------------------

// Every one of these is the same rule seen from a different angle: when the engine cannot
// complete a step it must STOP the pass, not carry on to the next step with a state the
// cluster has not accepted.
func TestGateMessagingMigrationStopsOnAFailedStep(t *testing.T) {
	entry := otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3}
	overdue := metav1.NewTime(time.Now().Add(-time.Hour))

	cases := []struct {
		name     string
		platform func() *otilmv1alpha1.Platform
		funcs    interceptor.Funcs
	}{
		{
			name:     "the migration cannot be recorded, so nothing is fenced",
			platform: migrationGatePlatform,
			funcs:    failingStatusUpdate(errors.New("status write rejected")),
		},
		{
			name: "a producer cannot be held down, so the drain is never started",
			platform: func() *otilmv1alpha1.Platform {
				return migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
			},
			funcs: failingPatch(errors.New("patch rejected")),
		},
		{
			name: "the fence's effect cannot be read, so the phase does not advance on a guess",
			platform: func() *otilmv1alpha1.Platform {
				return migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
			},
			funcs: interceptor.Funcs{
				Get: func(_ context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isWorkload := obj.(*appsv1.Deployment); isWorkload {
						return errors.New("read rejected")
					}
					return c.Get(context.Background(), key, obj, opts...)
				},
			},
		},
		{
			name: "the hand-over to the drain cannot be recorded",
			platform: func() *otilmv1alpha1.Platform {
				// Already fenced, so nothing re-records: the only write left is the transition.
				return migratingGatePlatform(otilmv1alpha1.MigrationPhaseFencing, entry)
			},
			funcs: failingStatusUpdate(errors.New("status write rejected")),
		},
		{
			name: "the revert cannot be completed, so the record it needs is kept",
			platform: func() *otilmv1alpha1.Platform {
				p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, entry)
				p.Spec.Version = platformVersion218
				return p
			},
			funcs: failingStatusUpdate(errors.New("status write rejected")),
		},
		{
			name: "the fence cannot be lifted at the deadline",
			platform: func() *otilmv1alpha1.Platform {
				p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, entry)
				p.Status.Upgrade.PhaseStartedAt = overdue
				return p
			},
			funcs: failingPatch(errors.New("patch rejected")),
		},
		{
			name: "the deadline's outcome cannot be recorded",
			platform: func() *otilmv1alpha1.Platform {
				p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
				p.Status.Upgrade.PhaseStartedAt = overdue
				return p
			},
			funcs: failingStatusUpdate(errors.New("status write rejected")),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.platform()
			requested := p.Spec.Version
			r, _ := migrationReconciler(t, p, tc.funcs, producerWorkloads(1, 3)...)
			target := bundleFor(t, requested)

			_, handled, _, err := r.gateMessagingMigration(context.Background(), p, target, requested)
			require.Error(t, err, "a step the cluster refused must surface, not be swallowed")
			assert.True(t, handled, "and it must stop the pass")
		})
	}
}

// TestWorkloadObservedReplicasIgnoresOtherKinds: the reader is asked only about the two kinds
// the fence can address, and anything else reports nothing running rather than guessing.
func TestWorkloadObservedReplicasIgnoresOtherKinds(t *testing.T) {
	assert.Equal(t, int32(2), workloadObservedReplicas(&appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: 2}}))
	assert.Equal(t, int32(5), workloadObservedReplicas(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{Replicas: 5}}))
	assert.Zero(t, workloadObservedReplicas(&otilmv1alpha1.Platform{}))
}

// TestNextRequeueResultPrefersTheMigration: the migration's cadence drives a sequence forward,
// so it outranks every dependency re-check — including the ones whose gates never ran.
func TestNextRequeueResultPrefersTheMigration(t *testing.T) {
	assert.Equal(t, ctrl.Result{RequeueAfter: migrationRequeueAfter},
		nextRequeueResult(requeueSignals{migration: true, adminUser: true, database: true, edge: true}))
	assert.Equal(t, ctrl.Result{RequeueAfter: adminRegisterRequeueAfter},
		nextRequeueResult(requeueSignals{adminUser: true}))
	assert.Equal(t, ctrl.Result{}, nextRequeueResult(requeueSignals{ready: true}))
}
