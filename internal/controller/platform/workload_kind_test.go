package platform

import (
	"context"
	"fmt"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// kindSwitchPlatform returns a PURE Platform fixture for the workload-kind guards: external
// dependencies (so RenderPlatformBase produces the full workload set with no cluster facts)
// and a UID, so controller-ownership is assertable. It deliberately does not use
// lifecyclePlatform, which creates objects through the envtest client.
func kindSwitchPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ilm", UID: "platform-uid"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "db", Port: 5432, Name: "ilm",
				Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "db-secret"},
			},
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", BrokerType: "rabbitmq", Host: "mq", Port: 5672,
				VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "mq-secret"},
			},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// ownedByPlatform returns the controller owner reference the operator stamps on every child
// it applies, so a fixture object is indistinguishable from a really-applied one.
func ownedByPlatform(p *otilmv1alpha1.Platform) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: otilmv1alpha1.GroupVersion.String(), Kind: "Platform",
		Name: p.Name, UID: p.UID, Controller: ptr(true),
	}
}

// statefulCore is the mutation every guard case shares: Core's workloadType flipped to
// StatefulSet while a Deployment of the same name is what the cluster actually has.
func statefulCore(p *otilmv1alpha1.Platform) {
	p.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet
}

// liveCoreDeployment returns the controller-owned Core Deployment a platform is running
// before its workloadType is flipped.
func liveCoreDeployment(p *otilmv1alpha1.Platform) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "core", Namespace: p.Namespace,
		OwnerReferences: []metav1.OwnerReference{ownedByPlatform(p)},
	}}
}

// kindSwitchReconciler builds a reconciler over a fake client holding the Platform and the
// given objects. The Platform's STATUS SUBRESOURCE is enabled because the durable kind-switch
// marker is persisted through it (a bare fake client silently drops that write), and a fake
// recorder is wired because the switch records an Event.
func kindSwitchReconciler(t *testing.T, p *otilmv1alpha1.Platform, objs ...client.Object) *Reconciler {
	t.Helper()
	s := reconcileScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(append([]client.Object{p}, objs...)...).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}
}

// markedSwitching stamps the DURABLE marker on a fixture the way markWorkloadKindSwitch does,
// for the gap cases where neither kind's object exists and the marker is the only signal.
//
//nolint:unparam // name is intentionally a parameter for a reusable marker-stamping helper, even though every current fixture switches "core"
func markedSwitching(p *otilmv1alpha1.Platform, name, live, rendered string) {
	setWorkloadKindSwitchCondition(p, metav1.ConditionTrue, reasonWorkloadKindSwitch,
		fmt.Sprintf("workload %q is being switched from a %s to a %s", name, live, rendered))
}

// renderedPlatformWorkloads materialises EVERY workload the platform renders as a
// controller-owned, non-terminating object — a converged cluster.
//
// Settlement is an all-or-nothing question over the WHOLE render, so a fixture that stood up
// only core could never reach it and a "not settled" assertion would pass for the wrong reason.
// Callers that want one workload terminating mutate that entry and leave the rest alone, which
// makes the deletionTimestamp the only difference between the two cases.
func renderedPlatformWorkloads(p *otilmv1alpha1.Platform) []client.Object {
	var objs []client.Object
	for _, obj := range platformbuilder.RenderPlatformBase(p) {
		if workloadKindOf(obj) == "" {
			continue
		}
		obj.SetOwnerReferences([]metav1.OwnerReference{ownedByPlatform(p)})
		objs = append(objs, obj)
	}
	return objs
}

// terminating marks the named workload in a converged set as foreground-deleting: a
// deletionTimestamp AND the foregroundDeletion finalizer, which is what actually keeps such an
// object alive (and what the fake client requires before it will hold a deletionTimestamp).
func terminating(objs []client.Object, name string) []client.Object {
	now := metav1.Now()
	for _, o := range objs {
		if o.GetName() == name {
			o.SetDeletionTimestamp(&now)
			o.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
		}
	}
	return objs
}

// TestPendingWorkloadKindChange pins the ONE predicate the two migration refusals and the
// stop-before-start orchestration all read: a component whose RENDERED kind differs from the
// controller-owned workload actually in the cluster.
//
// It deliberately cannot tell a switch just REQUESTED from one mid-orchestration — from
// outside, both are exactly "the live kind is not the rendered kind" — and that is correct,
// because both are equally unsafe to combine with a messaging migration.
func TestPendingWorkloadKindChange(t *testing.T) {
	s := reconcileScheme(t)

	t.Run("no other-kind object is the steady state", func(t *testing.T) {
		p := kindSwitchPlatform(nil)
		r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build(), Scheme: s}
		_, pending, err := r.pendingWorkloadKindChange(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, pending, "the overwhelmingly common case, on every reconcile of every platform")
	})

	t.Run("a live Deployment under a StatefulSet render is a pending switch", func(t *testing.T) {
		p := kindSwitchPlatform(statefulCore)
		r := &Reconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(p, liveCoreDeployment(p)).Build(),
			Scheme: s,
		}
		change, pending, err := r.pendingWorkloadKindChange(context.Background(), p)
		require.NoError(t, err)
		require.True(t, pending)
		assert.Equal(t, "core", change.Name)
		assert.Equal(t, "Deployment", change.Live)
		assert.Equal(t, "StatefulSet", change.Rendered)
	})

	t.Run("a foreign object of the other kind is never a pending switch", func(t *testing.T) {
		p := kindSwitchPlatform(statefulCore)
		foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: p.Namespace}}
		r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(p, foreign).Build(), Scheme: s}
		_, pending, err := r.pendingWorkloadKindChange(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, pending, "an object the operator does not control is somebody else's")
	})
}

// TestMigrationRefusesToStartOnAnOutstandingKindSwitch is the SIMULTANEOUS case: one edit
// bumps spec.version AND flips core.workloadType. The migration must not be recorded at all —
// beginMigration would persist a migration, the fence would then record kinds, and the
// newly-requested kind would be recorded Absent (i.e. "stopped") while the old producer still
// ran. Refused BEFORE anything is written, so there is nothing to unwind.
//
// It is ALSO the nil-safety proof for the start path. On this path status.upgrade does not
// exist yet (beginMigration has not run), so the refusal must derive the would-be SOURCE
// version the way beginMigration does — from status.observedVersion — and must not touch
// status.upgrade at all. An earlier draft read p.Status.Upgrade.FromVersion here and panicked.
func TestMigrationRefusesToStartOnAnOutstandingKindSwitch(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		statefulCore(p)
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
			Mode: "managed", BrokerType: "rabbitmq", Managed: &otilmv1alpha1.ManagedMessagingSpec{},
		}
		p.Spec.Version = platformVersion219
		p.Status.ObservedVersion = platformVersion218
	})
	require.Nil(t, p.Status.Upgrade, "the fixture's premise: nothing is recorded yet")
	target, ok := bom.BundleFor(platformVersion219)
	require.True(t, ok)
	r := fenceReconcilerFor(t, p, liveCoreDeployment(p))

	_, handled, _, err := r.gateMessagingMigration(context.Background(), p, target, platformVersion219)
	require.NoError(t, err)
	assert.True(t, handled, "the pass must stop rather than record a migration")
	assert.Nil(t, p.Status.Upgrade, "NO migration may be recorded on top of an outstanding kind switch")

	cond := meta.FindStatusCondition(p.Status.Conditions, conditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationWorkloadKindPending, cond.Reason)
	assert.Contains(t, cond.Message, "core")
	assert.Contains(t, cond.Message, platformVersion218,
		"the would-be source version comes from status.observedVersion, exactly as beginMigration reads it")
	assert.Contains(t, cond.Message, "revert spec.version to "+platformVersion218,
		"reverting spec.version to the source is the ONE remedy that actually works here: it is what lets this "+
			"pass proceed instead of trying to start the migration, which is what lets the switch complete")
	assert.NotContains(t, cond.Message, "let the workloadType switch finish",
		"this refusal itself short-circuits the pass that would let the switch finish, so that suggestion never works")
	assert.NotContains(t, cond.Message, "revert that component's workloadType",
		"reverting the workloadType alone does not un-request the target version, so the guard would keep firing")
}

// TestMigrationRefusesToStartInsideTheKindSwitchGap is the case object presence CANNOT see:
// the superseded workload's foreground delete has completed and the new kind has not been
// applied yet, so NEITHER object exists. Without the durable marker the trigger sees a clean
// platform, starts the migration, and the fence records the requested producer as Absent —
// which fencedProducersStopped reads as "stopped" while the switch is still mid-flight.
func TestMigrationRefusesToStartInsideTheKindSwitchGap(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		statefulCore(p)
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
			Mode: "managed", BrokerType: "rabbitmq", Managed: &otilmv1alpha1.ManagedMessagingSpec{},
		}
		p.Spec.Version = platformVersion219
		p.Status.ObservedVersion = platformVersion218
		markedSwitching(p, "core", "Deployment", "StatefulSet")
	})
	target, ok := bom.BundleFor(platformVersion219)
	require.True(t, ok)
	r := fenceReconcilerFor(t, p) // NO workload of either kind exists

	change, pending, err := r.pendingWorkloadKindChange(context.Background(), p)
	require.NoError(t, err)
	require.False(t, pending, "object presence alone sees nothing here — that is the whole point")
	assert.Empty(t, change.Name)

	_, outstanding, err := r.outstandingWorkloadKindSwitch(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, outstanding, "the durable marker must still report the switch")

	_, handled, _, err := r.gateMessagingMigration(context.Background(), p, target, platformVersion219)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Nil(t, p.Status.Upgrade, "no migration may be recorded inside the switch gap")
	cond := meta.FindStatusCondition(p.Status.Conditions, conditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationWorkloadKindPending, cond.Reason)
	assert.Contains(t, cond.Message, "revert spec.version to "+platformVersion218,
		"in the gap state NEITHER object exists, so there is nothing to revert a workloadType edit against — "+
			"reverting spec.version to the source is the only remedy that actually retires this refusal")
	assert.NotContains(t, cond.Message, "let the workloadType switch finish",
		"the switch cannot complete while this refusal keeps short-circuiting the pass that would apply it")
	assert.NotContains(t, cond.Message, "revert that component's workloadType",
		"there is no live object of either kind here for a workloadType revert to act on")
}

// TestMigrationAbortProceedsButRefusesTheKindChange is the third way in, and the one the two
// guards above do not cover: migrationActionAbort is handled BEFORE the start branch and never
// enters advanceMigration, so one edit reverting spec.version AND flipping workloadType walks
// past both.
//
// The abort itself must PROCEED — it is the user's revert and it lifts the fence — so the
// assertion is asymmetric: status.upgrade is discarded (the abort ran) AND the pass is still
// stopped with the kind-change refusal (the switch did not run alongside it).
func TestMigrationAbortProceedsButRefusesTheKindChange(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		statefulCore(p)
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "managed", BrokerType: "rabbitmq"}
		p.Spec.Version = platformVersion218 // reverted in the same edit that flipped the workload kind
		p.Status.ObservedVersion = platformVersion218
		p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
			FromVersion: platformVersion218, ToVersion: platformVersion219,
			Phase: otilmv1alpha1.MigrationPhaseFencing,
		}
	})
	source, ok := bom.BundleFor(platformVersion218)
	require.True(t, ok)
	live := liveCoreDeployment(p)
	r := fenceReconcilerFor(t, p, live)

	_, handled, _, err := r.gateMessagingMigration(context.Background(), p, source, platformVersion218)
	require.NoError(t, err)
	assert.Nil(t, p.Status.Upgrade, "the abort must PROCEED — the fence is lifted and the record discarded")
	assert.True(t, handled, "but the pass must still stop: the kind change may not ride along with the abort")

	cond := meta.FindStatusCondition(p.Status.Conditions, conditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationAbortWorkloadKind, cond.Reason)

	var still appsv1.Deployment
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "core", Namespace: p.Namespace}, &still))
	assert.Nil(t, still.DeletionTimestamp, "nothing may be deleted in the abort's own pass")
}

// TestMigrationRefusesAKindSwitchWhileHoldingCore is the CuttingOver case, and the one the
// fence cannot see: Core is never a fence target, so migrationWorkloadKindFlip reports
// nothing, and mig.holdCore withholds Core's workload from the apply while the prune would
// still reclaim the old kind. The pass must stop at the gate — before any apply, before any
// prune — leaving the running Deployment exactly as it is.
func TestMigrationRefusesAKindSwitchWhileHoldingCore(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		statefulCore(p)
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "managed", BrokerType: "rabbitmq"}
		p.Spec.Version = platformVersion219
		p.Status.ObservedVersion = platformVersion218
		p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
			FromVersion: platformVersion218, ToVersion: platformVersion219,
			Phase: otilmv1alpha1.MigrationPhaseCuttingOver,
		}
	})
	live := liveCoreDeployment(p)
	r := fenceReconcilerFor(t, p, live)

	_, handled, _, err := r.advanceMigration(context.Background(), p, migrationRender{version: platformVersion218})
	require.NoError(t, err)
	assert.True(t, handled, "the migration pass must stop")

	cond := meta.FindStatusCondition(p.Status.Conditions, conditionDegraded)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationWorkloadKindChanged, cond.Reason)

	var still appsv1.Deployment
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "core", Namespace: p.Namespace}, &still),
		"the running Core Deployment must be untouched — refusing is the whole point")
	assert.Nil(t, still.DeletionTimestamp)
}

// TestStopSupersededWorkloadDeletesTheOtherKind proves the ordering fix on the NON-migration
// path: when a component's rendered kind changes, the OLD kind is deleted before the new one
// is applied, and the caller is told to withhold the new object this pass. Applying first
// (today's order) briefly runs two schedulers publishing the same timed jobs.
//
// It also pins the two things the delete alone does not buy:
//
//   - the superseded object's key is added to the KEEP SET handed to the prune. The prune runs
//     later in the SAME reconcile, through the manager's CACHED client, so the object it lists
//     may still carry no deletionTimestamp — and a second Delete re-evaluates the propagation
//     policy and can strip the foregroundDeletion finalizer this switch depends on. Keep-set
//     membership removes the race without depending on read freshness at all, which is why it is
//     asserted here directly rather than through a timing-dependent envtest.
//   - the DURABLE marker is recorded, so the window in which neither kind's object exists is
//     still visible to the migration trigger.
func TestStopSupersededWorkloadDeletesTheOtherKind(t *testing.T) {
	p := kindSwitchPlatform(nil)
	old := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "scheduler", Namespace: p.Namespace,
		OwnerReferences: []metav1.OwnerReference{ownedByPlatform(p)},
	}}
	r := kindSwitchReconciler(t, p, old)

	keep := newDesiredSet()
	rendered := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: p.Namespace}}
	withhold, err := r.stopSupersededWorkload(context.Background(), p, keep, rendered)
	require.NoError(t, err)
	assert.True(t, withhold, "the new kind must be withheld while the old one is being stopped")

	assert.True(t, keep.has(r.keyForObject(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "scheduler", Namespace: p.Namespace,
	}})), "the superseded Deployment must be in the keep set, or the post-apply prune re-deletes it")

	_, inFlight := workloadKindSwitchInFlight(p)
	assert.True(t, inFlight, "the durable marker must be recorded BEFORE the delete")

	var gone appsv1.Deployment
	err = r.Get(context.Background(), types.NamespacedName{Name: "scheduler", Namespace: p.Namespace}, &gone)
	assert.True(t, apierrors.IsNotFound(err), "the superseded Deployment must have been deleted")
}

// TestStopSupersededWorkloadIgnoresForeignAndAbsentObjects proves the two safety rails: an
// object this Platform does not control is NEVER deleted, never kept, and never marked; and no
// other-kind object at all is a plain no-op (the overwhelmingly common case, on every reconcile
// of every platform).
func TestStopSupersededWorkloadIgnoresForeignAndAbsentObjects(t *testing.T) {
	p := kindSwitchPlatform(nil)
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: p.Namespace}}
	r := kindSwitchReconciler(t, p, foreign)
	keep := newDesiredSet()

	withhold, err := r.stopSupersededWorkload(context.Background(), p, keep,
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: p.Namespace}})
	require.NoError(t, err)
	assert.False(t, withhold, "a workload the operator does not control must never block or be deleted")
	var still appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "core", Namespace: p.Namespace}, &still))

	withhold, err = r.stopSupersededWorkload(context.Background(), p, keep,
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: p.Namespace}})
	require.NoError(t, err)
	assert.False(t, withhold)

	assert.Empty(t, keep, "neither a foreign nor an absent object may enter the keep set")
	_, inFlight := workloadKindSwitchInFlight(p)
	assert.False(t, inFlight, "no marker may be recorded when no switch is ours to make")
}

// TestClearWorkloadKindSwitchIfSettledNeedsTheNewKindPresent proves the marker's retirement
// rule, which is the other half of what makes it durable: it is cleared ONLY once nothing of
// the previous kind is left AND every rendered workload actually exists. Clearing on the first
// condition alone would re-open exactly the gap the marker was added to close.
func TestClearWorkloadKindSwitchIfSettledNeedsTheNewKindPresent(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		markedSwitching(p, "core", "Deployment", "StatefulSet")
	})
	r := kindSwitchReconciler(t, p) // nothing exists yet: the gap

	stillSwitching, err := r.clearWorkloadKindSwitchIfSettled(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, stillSwitching, "the marker must survive while a rendered workload is missing")
	_, inFlight := workloadKindSwitchInFlight(p)
	assert.True(t, inFlight)
}

// TestClearWorkloadKindSwitchIfSettledIgnoresATerminatingWorkload is the REVERT-MID-DELETE case,
// and the one a bare existence check gets wrong.
//
// Sequence: core.workloadType is flipped Deployment → StatefulSet (marker written, the Deployment
// foreground-deleted), and the user then REVERTS it to Deployment while that delete is still
// draining pods. The render now produces a Deployment again, so pendingWorkloadKindChange probes
// only the StatefulSet — never created — and reports nothing. The Deployment is still THERE, so a
// bare existence check would call this settled, retire the marker and drop the 5-second switch
// requeue, while the deletion still had the workload to remove. The gap that opens next would be
// UNMARKED, and a messaging migration could start into it.
//
// Every other rendered workload is present and healthy, so the deletionTimestamp on core's is the
// only thing that can make this "not settled" — the sibling test below is the control.
func TestClearWorkloadKindSwitchIfSettledIgnoresATerminatingWorkload(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		markedSwitching(p, "core", "Deployment", "StatefulSet") // the switch that was started
	})
	r := kindSwitchReconciler(t, p, terminating(renderedPlatformWorkloads(p), "core")...)

	_, pending, err := r.pendingWorkloadKindChange(context.Background(), p)
	require.NoError(t, err)
	require.False(t, pending,
		"the reverted-to kind is what renders now, so the superseded probe finds nothing — that is the trap")

	settled, err := r.renderedWorkloadsSettled(context.Background(), p)
	require.NoError(t, err)
	assert.False(t, settled, "a TERMINATING workload has not settled: it is still going away")

	stillSwitching, err := r.clearWorkloadKindSwitchIfSettled(context.Background(), p)
	require.NoError(t, err)
	assert.True(t, stillSwitching, "the marker AND the switch requeue must both survive the deletion")
	_, inFlight := workloadKindSwitchInFlight(p)
	assert.True(t, inFlight)
}

// TestClearWorkloadKindSwitchIfSettledClearsOnANonTerminatingWorkload closes that same sequence
// and is the control for the test above: the delete completes, the ordinary render re-creates
// core as the reverted kind, and only THEN is the marker retired. The fixture differs from the
// previous one by exactly the deletionTimestamp.
func TestClearWorkloadKindSwitchIfSettledClearsOnANonTerminatingWorkload(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		markedSwitching(p, "core", "Deployment", "StatefulSet")
	})
	r := kindSwitchReconciler(t, p, renderedPlatformWorkloads(p)...)

	settled, err := r.renderedWorkloadsSettled(context.Background(), p)
	require.NoError(t, err)
	require.True(t, settled)

	stillSwitching, err := r.clearWorkloadKindSwitchIfSettled(context.Background(), p)
	require.NoError(t, err)
	assert.False(t, stillSwitching, "present and non-terminating: the switch is over")
	_, inFlight := workloadKindSwitchInFlight(p)
	assert.False(t, inFlight, "the marker must be retired, so the migration engine stops refusing")
}

// TestGuardRestoresAStaleKindSwitchRefusal closes the CORRECTION loop on the in-flight refusal.
//
// The refusal writes MessagingMigration=False/MigrationWorkloadKindChanged, and nothing later
// rewrites it: once the user restores the workloadType the guard simply stops firing, and the
// phase that resumes need not write migration state at all (migrationFencingPhase persists
// nothing while it is still waiting for producers to stop). Without the restore the Platform
// carries an ACTIVE status.upgrade beside a condition claiming the migration is refused, for as
// long as the fence takes.
func TestGuardRestoresAStaleKindSwitchRefusal(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "managed", BrokerType: "rabbitmq"}
		p.Spec.Version = platformVersion219
		p.Status.ObservedVersion = platformVersion218
		p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
			FromVersion: platformVersion218, ToVersion: platformVersion219,
			Phase: otilmv1alpha1.MigrationPhaseFencing,
		}
		// The refusal an earlier pass wrote, while core.workloadType was still flipped.
		setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationWorkloadKindChanged,
			`workload "core" is rendered as a StatefulSet but the cluster is still running it as a Deployment`)
	})
	// core.workloadType has been reverted, so render and cluster agree again and no marker is set.
	r := kindSwitchReconciler(t, p, renderedPlatformWorkloads(p)...)

	handled, _, err := r.guardMigrationWorkloadKind(context.Background(), p, platformVersion219, kindGuardInFlight)
	require.NoError(t, err)
	require.False(t, handled, "with the kind change reverted the guard must let the pass through")

	cond := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"the migration is running again, so its condition must say so rather than keep the refusal")
	assert.Equal(t, string(otilmv1alpha1.MigrationPhaseFencing), cond.Reason)
	assert.Contains(t, cond.Message, platformVersion219)
}

// TestGuardLeavesOtherMigrationRefusalsAlone pins the restore's NARROW scope: a False condition
// this guard did not write is somebody else's to clear, and rewriting it would erase a genuine
// drain-timeout or input-drift refusal on the very next pass.
func TestGuardLeavesOtherMigrationRefusalsAlone(t *testing.T) {
	p := kindSwitchPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "managed", BrokerType: "rabbitmq"}
		p.Spec.Version = platformVersion219
		p.Status.ObservedVersion = platformVersion218
		p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
			FromVersion: platformVersion218, ToVersion: platformVersion219,
			Phase: otilmv1alpha1.MigrationPhaseDraining,
		}
		setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationDrainTimeout,
			"the messaging migration did not complete phase Draining in time")
	})
	r := kindSwitchReconciler(t, p, renderedPlatformWorkloads(p)...)

	handled, _, err := r.guardMigrationWorkloadKind(context.Background(), p, platformVersion219, kindGuardInFlight)
	require.NoError(t, err)
	require.False(t, handled)

	cond := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationDrainTimeout, cond.Reason, "a foreign refusal must be left exactly as it was")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}
