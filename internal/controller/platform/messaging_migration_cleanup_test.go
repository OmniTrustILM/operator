/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// This is the only file in the operator that tests DELETION of rabbitmq.com objects, and the
// claims it makes are one-directional: nothing may be deleted unless the broker has just said,
// affirmatively and completely, that the source virtual host is idle — and even then only one
// class at a time, in dependency order.
//
// Every test drives the phase through the whole migration gate from a freshly read Platform, as
// Reconcile does, with the source topology seeded into the cluster exactly as the source render
// produced it and a scripted broker answering for it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/rabbitmq"
)

// cleanupPlatform is a platform in the CLEANINGUP phase: serving 2.19.0, with the scheduler still
// fenced. A cutover that ran to completion hands over an empty fence, so this is the SAFETY-NET
// shape — an interrupted or forced cutover — and the one worth testing: the completion must bring
// back whatever is still held, whichever way the migration got here.
func cleanupPlatform(fenced ...otilmv1alpha1.FencedWorkload) *otilmv1alpha1.Platform {
	if len(fenced) == 0 {
		fenced = []otilmv1alpha1.FencedWorkload{fencedScheduler()}
	}
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseCleaningUp, fenced...)
	p.Status.ObservedVersion = platformVersion219 // the cutover reports the target before handing over
	return p
}

// sourceTopology renders the objects the platform's SOURCE version put in the cluster — the
// whole set, broker and users included, so a test can prove what the cleanup leaves alone as
// well as what it removes.
func sourceTopology(p *otilmv1alpha1.Platform) []client.Object {
	src := p.DeepCopy()
	src.Spec.Version = platformVersion218
	return platformbuilder.ResolveManagedMessaging(src)
}

// cleanupReconciler builds a reconciler over a fake client that can hold the rabbitmq.com kinds,
// seeded with the given objects, the administrator credentials Secret the barrier authenticates
// with, and a still-fenced scheduler for the completion to restore.
func cleanupReconciler(t *testing.T, p *otilmv1alpha1.Platform, admin rabbitmq.BrokerAdmin, funcs interceptor.Funcs, objs ...client.Object) (*Reconciler, *record.FakeRecorder) {
	t.Helper()
	pinMigrationInputs(p)
	s := cutoverScheme(t)
	seeded := append([]client.Object{p, administratorSecret(), fencedSchedulerWorkload()}, objs...)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(seeded...).WithInterceptorFuncs(funcs).Build()
	rec := record.NewFakeRecorder(32)
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}
	r.BrokerAdmins = func(_, _, _ string) rabbitmq.BrokerAdmin { return admin }
	return r, rec
}

// fencedSchedulerWorkload is a workload the fence is still holding at zero — the one the
// completion writes the recorded count back onto.
func fencedSchedulerWorkload() client.Object {
	return cutoverWorkload(schedulerWorkloadName, "scheduler:2.19.0", platformVersion219, 0, true)
}

// cleanupPass runs one whole gate pass from a FRESHLY READ Platform, as Reconcile does.
func cleanupPass(t *testing.T, r *Reconciler) (migrationRender, bool, ctrl.Result, error) {
	t.Helper()
	_, to := migrationBundles(t)
	return r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
}

// idleSourceListing is what the source virtual host reports once everything has moved: every
// work queue empty, and the two latest-only retention queues holding the single message they
// are DESIGNED to hold and that the target virtual host has already been given again.
func idleSourceListing() []rabbitmq.QueueState {
	return sourceQueueListing()
}

// idleBroker answers for a source virtual host nothing is attached to and nothing is left in.
func idleBroker() *fakeBrokerAdmin {
	return &fakeBrokerAdmin{queues: idleSourceListing()}
}

// extantOf reports how many of the given rendered objects are still in the cluster.
func extantOf(t *testing.T, r *Reconciler, objs []client.Object) int {
	t.Helper()
	found := 0
	for _, obj := range objs {
		var probe unstructured.Unstructured
		probe.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &probe)
		if err == nil {
			found++
			continue
		}
		require.True(t, apierrors.IsNotFound(err), "unexpected read failure: %v", err)
	}
	return found
}

// kindOf returns the rendered objects of one rabbitmq.com Kind.
func kindOf(objs []client.Object, kind string) []client.Object {
	var out []client.Object
	for _, obj := range objs {
		if obj.GetObjectKind().GroupVersionKind().Kind == kind {
			out = append(out, obj)
		}
	}
	return out
}

// --- the final barrier -------------------------------------------------------

// TestCleanupBlocksWhileClientsAreStillAttached is the first half of the barrier. A drained
// virtual host with a client on it is a client the delete would strand — a remote proxy still
// enrolled against the old topology, or the time-quality monitor still pointed at it.
func TestCleanupBlocksWhileClientsAreStillAttached(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.connections = 1
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	render, handled, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled, "the platform keeps converging while the reclaim waits")
	assert.True(t, render.requeue)

	assert.Equal(t, len(topology), extantOf(t, r, topology), "not one object may be deleted while a client is attached")
	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the migration is not finished")
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, stored.Status.Upgrade.Phase)

	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "waiting for the previous messaging topology to fall idle")
	assertNoBrokerCoordinates(t, cond.Message)
}

// TestCleanupBlocksOnALateMessage is the second half, and the reason the barrier exists at all:
// Core keeps running through the whole migration and keeps write permission on the source
// exchanges, so a message can land AFTER the drain's last clean sample. The cleanup re-snapshots
// rather than trusting that sample.
func TestCleanupBlocksOnALateMessage(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.queues = append(idleSourceListing(), rabbitmq.QueueState{Name: "core.events", MessagesUnacked: 1})
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, len(topology), extantOf(t, r, topology),
		"three clean drain polls are a sample; a message that arrived after them still blocks")

	// The snapshot was taken, and it was taken on the SOURCE virtual host.
	for _, c := range admin.callLog() {
		assert.Equal(t, sourceVirtualHost, c.vhost)
	}
}

// TestCleanupBlocksOnAQueueNoBundleDeclares: the snapshot is FULL. The drain only counts the
// queues it can name from the bundle plus the proxy bindings; the cleanup is about to destroy a
// virtual host, so anything on it holding messages blocks — including a queue no version of the
// platform has ever declared.
func TestCleanupBlocksOnAQueueNoBundleDeclares(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.queues = append(idleSourceListing(), rabbitmq.QueueState{Name: "instance-7a3f", MessagesReady: 2})
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, len(topology), extantOf(t, r, topology))
}

// TestCleanupBlocksWhenTheBrokerDoesNotAnswer: FAIL CLOSED. An error is never evidence that
// anything is safe to delete — neither on the connection count nor on the snapshot.
func TestCleanupBlocksWhenTheBrokerDoesNotAnswer(t *testing.T) {
	tests := []struct {
		name  string
		admin func() *fakeBrokerAdmin
	}{
		{
			name: "the connection count fails",
			admin: func() *fakeBrokerAdmin {
				a := idleBroker()
				a.connectionsErr = errors.New("broker error: connection listing")
				return a
			},
		},
		{
			name: "the final snapshot fails",
			admin: func() *fakeBrokerAdmin {
				a := idleBroker()
				a.queuesErr = errors.New("broker error: queue listing")
				return a
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cleanupPlatform()
			topology := sourceTopology(p)
			r, _ := cleanupReconciler(t, p, tc.admin(), interceptor.Funcs{}, topology...)

			_, handled, _, err := cleanupPass(t, r)
			require.NoError(t, err, "an unanswered broker is a wait, not a platform failure")
			assert.False(t, handled)
			assert.Equal(t, len(topology), extantOf(t, r, topology))
			assert.NotNil(t, storedPlatform(t, r).Status.Upgrade)
		})
	}
}

// TestCleanupBlocksWhenTheAdministratorCredentialsAreUnreadable: the barrier cannot be
// established without a broker to ask, so a missing credentials Secret holds the reclaim too.
func TestCleanupBlocksWhenTheAdministratorCredentialsAreUnreadable(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	s := cutoverScheme(t)
	seeded := append([]client.Object{p, fencedSchedulerWorkload()}, topology...) // no administrator Secret
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).WithObjects(seeded...).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(32)}
	r.BrokerAdmins = func(_, _, _ string) rabbitmq.BrokerAdmin { return idleBroker() }

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, len(topology), extantOf(t, r, topology))
}

// --- one class at a time -----------------------------------------------------

// TestCleanupDeletesOneClassAtATime is the ordering contract, asserted the only way that means
// anything: at every step, the classes that come LATER are still fully present. Issuing all the
// deletes at once would leave them terminating together behind the Topology Operator's
// finalizers, and a virtual host removed early strands the rest.
func TestCleanupDeletesOneClassAtATime(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	admin := idleBroker()
	r, rec := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	order := platformbuilder.ManagedMessagingReclaimKinds()
	require.Equal(t, []string{"Binding", "Queue", "Exchange", "Permission", "Vhost"}, order,
		"the deletion order is a dependency contract")

	for i, kind := range order {
		reclaimed := kindOf(topology, kind)
		require.NotEmpty(t, reclaimed, "the 2.18.0 source render must contain a %s to reclaim", kind)

		_, handled, _, err := cleanupPass(t, r)
		require.NoError(t, err)
		assert.False(t, handled)

		assert.Zero(t, extantOf(t, r, reclaimed), "pass %d must reclaim the whole %s class", i+1, kind)
		for _, later := range order[i+1:] {
			assert.Equal(t, len(kindOf(topology, later)), extantOf(t, r, kindOf(topology, later)),
				"%s may not be touched while %s is being reclaimed", later, kind)
		}

		// The class being reclaimed is on the condition BEFORE it is deleted, and it names the
		// class rather than any object of it.
		cond := migrationCondition(storedPlatform(t, r))
		require.NotNil(t, cond)
		assert.Contains(t, cond.Message, migrationReclaimStages[kind])
		assertNoBrokerCoordinates(t, cond.Message)
	}

	// The broker and its users are NOT the migration's to reclaim: the cluster goes on serving
	// the target topology, and the per-user credentials Secrets generated from those Users are
	// wired into every component that is running right now.
	survivors := append(kindOf(topology, "RabbitmqCluster"), kindOf(topology, "User")...)
	assert.Equal(t, len(survivors), extantOf(t, r, survivors),
		"the broker and the broker-global users must survive the reclaim")

	// Nothing left to reclaim: the migration finishes on the next pass.
	render, handled, res, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.False(t, render.requeue, "a finished migration has nothing left to re-check")
	assert.Equal(t, ctrl.Result{}, res)

	stored := storedPlatform(t, r)
	assert.Nil(t, stored.Status.Upgrade, "the record is discarded once the reclaim is done")
	assert.Equal(t, platformVersion219, stored.Status.ObservedVersion)
	assert.Equal(t, int32(1), replicasOf(t, r, schedulerWorkloadName),
		"whatever the fence still held is restored at its recorded count")

	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMigrationCompleted, cond.Reason)
	assertNoBrokerCoordinates(t, cond.Message)
	assert.Contains(t, strings.Join(drainEvents(rec), " "), eventMigrationCompleted)
}

// TestCleanupRechecksTheBarrierBeforeEveryClass: the barrier is not a gate the cleanup passes
// once. A client that attaches between two classes stops the next one.
func TestCleanupRechecksTheBarrierBeforeEveryClass(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	admin := idleBroker()
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	require.Zero(t, extantOf(t, r, kindOf(topology, "Binding")))

	admin.mu.Lock()
	admin.connections = 3
	admin.mu.Unlock()

	_, _, _, err = cleanupPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, len(kindOf(topology, "Queue")), extantOf(t, r, kindOf(topology, "Queue")),
		"a client that arrived after the first class stops the second")
}

// TestCleanupWaitsForATerminatingClass: an object deleted but still held by the Topology
// Operator's finalizer still EXISTS, and that is exactly what keeps the cleanup on its class
// instead of advancing over a dependency the broker has not actually let go of.
func TestCleanupWaitsForATerminatingClass(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	bindings := kindOf(topology, "Binding")
	// The fake client keeps a deleted object that carries a finalizer, exactly as the apiserver
	// does while the Topology Operator is still removing it from the broker.
	bindings[0].SetFinalizers([]string{"deletion.finalizers.bindings.rabbitmq.com"})

	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{}, topology...)
	for i := 0; i < 3; i++ {
		_, _, _, err := cleanupPass(t, r)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, extantOf(t, r, bindings[:1]), "the held binding is still there")
	assert.Equal(t, len(kindOf(topology, "Queue")), extantOf(t, r, kindOf(topology, "Queue")),
		"the next class must wait for it, however many passes that takes")
}

// TestCleanupDoesNotDeleteOnAReadFailure: a topology object the apiserver could not be asked
// about is not an absent one. Reading it as absent would advance the cleanup past a class that
// is still there.
func TestCleanupDoesNotDeleteOnAReadFailure(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	failRead := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if u, isUnstructured := obj.(*unstructured.Unstructured); isUnstructured && u.GetKind() == "Binding" {
				return errors.New("the apiserver could not be reached")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r, _ := cleanupReconciler(t, p, idleBroker(), failRead, topology...)

	_, handled, _, err := cleanupPass(t, r)
	require.Error(t, err, "an unreadable topology surfaces rather than being read as reclaimed")
	assert.True(t, handled)
	assert.Equal(t, len(kindOf(topology, "Queue")), extantOf(t, r, kindOf(topology, "Queue")))
	assert.NotNil(t, storedPlatform(t, r).Status.Upgrade)
}

// TestCleanupStopsWhenTheClassCannotBePersisted keeps the engine's rule at the one place it
// matters most: nothing is destroyed that the platform has not first recorded it was about to
// destroy.
func TestCleanupStopsWhenTheClassCannotBePersisted(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	r, _ := cleanupReconciler(t, p, idleBroker(), failingStatusUpdate(errors.New(errStatusWriteRejected)), topology...)

	_, handled, _, err := cleanupPass(t, r)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, len(topology), extantOf(t, r, topology), "a refused status write deletes nothing")
}

// TestCleanupSurfacesADeleteFailure: a delete that fails must not be mistaken for progress —
// and what it surfaces may not name the object, because a topology object's name encodes the
// virtual host it is scoped to.
//
// The failure the interceptor returns is a real Kubernetes StatusError, deliberately: a
// StatusError's OWN text quotes the object it is about, so an error wrapped with %w republishes
// the coordinate however carefully the wrapper is worded. The degraded condition and the Warning
// Event are written from exactly this text.
func TestCleanupSurfacesADeleteFailure(t *testing.T) {
	p := cleanupPlatform()
	topology := sourceTopology(p)
	failDelete := interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			return apierrors.NewForbidden(
				schema.GroupResource{Group: "rabbitmq.com", Resource: "bindings"}, obj.GetName(), errors.New("denied"))
		},
	}
	r, rec := cleanupReconciler(t, p, idleBroker(), failDelete, topology...)

	_, handled, _, err := cleanupPass(t, r)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, len(topology), extantOf(t, r, topology))
	assertNoBrokerCoordinates(t, err.Error())

	stored := storedPlatform(t, r)
	for _, name := range namesOf(topology) {
		assert.NotContains(t, err.Error(), name, "the error text may not name a topology object")
		assert.NotContains(t, degradedMessageOf(stored), name, "and neither may the condition it becomes")
		assert.NotContains(t, strings.Join(drainEvents(rec), " "), name)
	}
	assertNoBrokerCoordinates(t, degradedMessageOf(stored))
}

// namesOf returns the object names of a rendered set, which are exactly the strings no
// condition, Event or log line may carry.
func namesOf(objs []client.Object) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

// degradedMessageOf returns the platform's Degraded condition message (empty when unset).
func degradedMessageOf(p *otilmv1alpha1.Platform) string {
	if c := meta.FindStatusCondition(p.Status.Conditions, conditionDegraded); c != nil {
		return c.Message
	}
	return ""
}

// --- the forced reclaim ------------------------------------------------------

// TestForcedCleanupDiscardsAndClosesConnections: the loss-accepting exit, and the only path that
// deletes without the barrier. It is target-scoped, so it can never be a flag left behind by an
// earlier migration.
func TestForcedCleanupDiscardsAndClosesConnections(t *testing.T) {
	p := cleanupPlatform()
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.connections = 4
	admin.queues = append(idleSourceListing(), rabbitmq.QueueState{Name: "core.events", MessagesReady: 12})
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)

	assert.Zero(t, extantOf(t, r, kindOf(topology, "Binding")), "force discards whatever the source still holds")

	closed := 0
	for _, c := range admin.callLog() {
		if c.method == "CloseConnections" {
			closed++
			assert.Equal(t, sourceVirtualHost, c.vhost, "only the SOURCE virtual host's clients are cut off")
		}
	}
	assert.Equal(t, 1, closed, "the lingering clients are force-closed")

	cond := migrationCondition(storedPlatform(t, r))
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, forceCutoverField)
	assertNoBrokerCoordinates(t, cond.Message)
}

// TestForcedCleanupProceedsWhenTheCloseFails: force is an instruction to proceed and discard, so
// a broker that will not close its connections cannot hold the reclaim it authorises (deleting
// the virtual host closes them anyway).
func TestForcedCleanupProceedsWhenTheCloseFails(t *testing.T) {
	p := cleanupPlatform()
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.closeErr = errors.New("broker error: connection close")
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Zero(t, extantOf(t, r, kindOf(topology, "Binding")))
}

// TestForceForAnotherVersionDoesNotAuthorizeThisReclaim: the authorisation names the version it
// authorises, so a value left behind by a past migration is inert.
func TestForceForAnotherVersionDoesNotAuthorizeThisReclaim(t *testing.T) {
	p := cleanupPlatform()
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion218
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.connections = 2
	r, _ := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, len(topology), extantOf(t, r, topology))
}

// --- the blocked terminal ----------------------------------------------------

// TestCleanupBlockedTerminal is the state the whole phase is designed to end in rather than sit
// in silently: the reclaim could not start within its budget, so the platform says exactly what
// is holding it, that it is itself fine, and every way out.
func TestCleanupBlockedTerminal(t *testing.T) {
	p := cleanupPlatform()
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(metav1.Now().Add(-2 * defaultMigrationDrainTimeout))
	topology := sourceTopology(p)
	admin := idleBroker()
	admin.connections = 2
	r, rec := cleanupReconciler(t, p, admin, interceptor.Funcs{}, topology...)

	render, handled, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled, "a blocked reclaim is a steady state, not a failure: the platform keeps converging")
	assert.True(t, render.requeue, "it keeps looking, because both remedies happen outside the cluster")
	assert.Equal(t, len(topology), extantOf(t, r, topology))

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the migration record survives so the exits stay explicit")
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMigrationCleanupBlocked, cond.Reason)

	// Both remedies, and the loss-accepting exit. The time-quality monitor is named because the
	// operator does not migrate it: it goes on republishing to whichever broker it is pointed at.
	assert.Contains(t, cond.Message, "re-enrol the remote proxies")
	assert.Contains(t, cond.Message, "time-quality monitor")
	assert.Contains(t, cond.Message, forceCutoverField)
	assert.Contains(t, cond.Message, "fully functional")
	assertNoBrokerCoordinates(t, cond.Message)

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], eventMigrationBlocked)

	// Re-entering the terminal state repeats neither the write nor the Event.
	_, _, _, err = cleanupPass(t, r)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec))
	assert.Equal(t, len(topology), extantOf(t, r, topology))
}

// TestCleanupStopsWhenTheWaitCannotBePersisted: a wait the cluster did not record is not a wait
// the platform can be left in silently — on either side of the deadline.
func TestCleanupStopsWhenTheWaitCannotBePersisted(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "still within the phase's budget"
		if blocked {
			name = "past the phase's budget"
		}
		t.Run(name, func(t *testing.T) {
			p := cleanupPlatform()
			if blocked {
				p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(metav1.Now().Add(-2 * defaultMigrationDrainTimeout))
			}
			topology := sourceTopology(p)
			admin := idleBroker()
			admin.connections = 1
			r, rec := cleanupReconciler(t, p, admin, failingStatusUpdate(errors.New(errStatusWriteRejected)), topology...)

			_, handled, _, err := cleanupPass(t, r)
			require.Error(t, err)
			assert.True(t, handled)
			assert.Equal(t, len(topology), extantOf(t, r, topology))
			assert.NotContains(t, strings.Join(drainEvents(rec), " "), eventMigrationBlocked,
				"a terminal state the cluster did not record is not announced as one")
		})
	}
}

// TestMigrationCleanupMessageNamesAnUndescribedClass: the condition still says what is happening
// if a class is ever added to the builder's order without a description here.
func TestMigrationCleanupMessageNamesAnUndescribedClass(t *testing.T) {
	u := cleanupPlatform().Status.Upgrade

	assert.Contains(t, migrationCleanupMessage(u, "SomethingNew", false), "reclaiming the previous messaging topology")
	forced := migrationCleanupMessage(u, "SomethingNew", true)
	assert.Contains(t, forced, forceCutoverField)
	assertNoBrokerCoordinates(t, forced)
}

// TestCleanupDeadlineDoesNotBlockAReclaimThatCanRun: the deadline ends a WAIT. A cleanup whose
// barrier is satisfied is not blocked by having taken a long time to get there.
func TestCleanupDeadlineDoesNotBlockAReclaimThatCanRun(t *testing.T) {
	p := cleanupPlatform()
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(metav1.Now().Add(-2 * defaultMigrationDrainTimeout))
	topology := sourceTopology(p)
	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{}, topology...)

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.Zero(t, extantOf(t, r, kindOf(topology, "Binding")))
}

// TestCleanupDeadlineIsScopedToItsOwnPhase proves the cleanup's deadline reads the phase it
// belongs to, so a long fence or drain cannot spend it.
func TestCleanupDeadlineIsScopedToItsOwnPhase(t *testing.T) {
	stale := metav1.NewTime(metav1.Now().Add(-2 * defaultMigrationDrainTimeout))

	cleaning := cleanupPlatform()
	cleaning.Status.Upgrade.PhaseStartedAt = stale
	assert.True(t, migrationCleanupDeadlineExceeded(cleaning, metav1.Now().Time))

	for _, phase := range []otilmv1alpha1.MigrationPhase{
		otilmv1alpha1.MigrationPhaseFencing,
		otilmv1alpha1.MigrationPhaseDraining,
		otilmv1alpha1.MigrationPhaseCuttingOver,
	} {
		other := migratingGatePlatform(phase)
		other.Status.Upgrade.PhaseStartedAt = stale
		assert.False(t, migrationCleanupDeadlineExceeded(other, metav1.Now().Time),
			"%s has its own budget, spent by its own rules", phase)
	}

	fresh := cleanupPlatform()
	assert.False(t, migrationCleanupDeadlineExceeded(fresh, metav1.Now().Time))
	assert.False(t, migrationCleanupDeadlineExceeded(&otilmv1alpha1.Platform{}, metav1.Now().Time),
		"no migration, no deadline")
}

// --- what the reclaim is aimed at --------------------------------------------

// TestOutstandingSourceQueues pins the snapshot's rule: every queue the broker reports counts,
// except the latest-only retention queues the bundle DECLARES as such — the exemption is derived
// from the queue's arguments, so a future bundle's retention queue inherits it.
func TestOutstandingSourceQueues(t *testing.T) {
	source := bundleFor(t, platformVersion218)

	assert.Zero(t, outstandingSourceQueues(source, idleSourceListing()),
		"a virtual host holding nothing but its retained config messages is idle")
	assert.Equal(t, 1, outstandingSourceQueues(source,
		append(idleSourceListing(), rabbitmq.QueueState{Name: "core", MessagesReady: 1})))
	assert.Equal(t, 1, outstandingSourceQueues(source,
		append(idleSourceListing(), rabbitmq.QueueState{Name: "core", MessagesUnacked: 1})),
		"a message delivered but not acknowledged can still come back")
	assert.Equal(t, 1, outstandingSourceQueues(source,
		append(idleSourceListing(), rabbitmq.QueueState{Name: "nobody-declared-me", MessagesReady: 1})),
		"a queue no bundle knows about is exactly the case a full snapshot exists to catch")
	assert.Zero(t, outstandingSourceQueues(source, []rabbitmq.QueueState{{Name: "core"}}),
		"an empty queue does not block, however many clients are attached to it — that is the connection check's business")
}

// TestMigrationReclaimClassesTargetTheSourceRender proves what the deletion is aimed at: the
// SOURCE render's exact identities, grouped by class, with the broker and the broker-global
// users excluded — and nothing the TARGET render also produces.
func TestMigrationReclaimClassesTargetTheSourceRender(t *testing.T) {
	p := cleanupPlatform()
	classes, err := migrationReclaimClasses(p)
	require.NoError(t, err)

	var kinds []string
	targets := map[string]struct{}{}
	for _, class := range classes {
		kinds = append(kinds, class.kind)
		for _, obj := range class.objects {
			assert.Equal(t, class.kind, obj.GetObjectKind().GroupVersionKind().Kind)
			targets[obj.GetName()] = struct{}{}
		}
	}
	assert.Equal(t, platformbuilder.ManagedMessagingReclaimKinds(), kinds)

	source := sourceTopology(p)
	for _, obj := range kindOf(source, "Binding") {
		assert.Contains(t, targets, obj.GetName(), "every source binding is a target")
	}
	for _, kind := range []string{"RabbitmqCluster", "User"} {
		for _, obj := range kindOf(source, kind) {
			assert.NotContains(t, targets, obj.GetName(), "the %s is never the migration's to reclaim", kind)
		}
	}
	// The live topology is untouchable, whichever way its names are derived.
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		assert.NotContains(t, targets, obj.GetName(), "an object the running platform uses may never be a target")
	}
}

// TestMigrationReclaimClassesRefuseAnUnknownSourceVersion: the renderer falls back to the
// default bundle for a version it cannot resolve, which would aim the deletion at a topology
// this platform never had. It has to be an error.
func TestMigrationReclaimClassesRefuseAnUnknownSourceVersion(t *testing.T) {
	p := cleanupPlatform()
	p.Status.Upgrade.FromVersion = unknownPlatformVersion

	_, err := migrationReclaimClasses(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), unknownPlatformVersion)
}

// TestCleanupDegradesOnAnUnknownSourceVersion: and the phase surfaces that rather than deleting
// something else.
func TestCleanupDegradesOnAnUnknownSourceVersion(t *testing.T) {
	p := cleanupPlatform()
	p.Status.Upgrade.FromVersion = unknownPlatformVersion
	topology := sourceTopology(p)
	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{}, topology...)

	_, handled, _, err := cleanupPass(t, r)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, len(topology), extantOf(t, r, topology))
}

// TestEveryReclaimKindHasAStage ties the condition's vocabulary to the builder's deletion order,
// so a class added or renamed there fails a test instead of silently reporting nothing.
func TestEveryReclaimKindHasAStage(t *testing.T) {
	for _, kind := range platformbuilder.ManagedMessagingReclaimKinds() {
		assert.Contains(t, migrationReclaimStages, kind)
	}
	assert.Len(t, migrationReclaimStages, len(platformbuilder.ManagedMessagingReclaimKinds()))
}

// --- finishing the migration -------------------------------------------------

// TestFinishMigrationPinsTheVersionItReached is the invariant that keeps a finished migration
// finished. Clearing the record while status still named the SOURCE version would read, to the
// very next reconcile, as a fresh upgrade request — and the engine would fence the producers and
// start draining a virtual host it has just reclaimed.
func TestFinishMigrationPinsTheVersionItReached(t *testing.T) {
	p := cleanupPlatform()
	p.Status.ObservedVersion = platformVersion218 // a status write that lost its race during the cutover
	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{})

	_, handled, _, err := cleanupPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)

	stored := storedPlatform(t, r)
	assert.Nil(t, stored.Status.Upgrade)
	assert.Equal(t, platformVersion219, stored.Status.ObservedVersion,
		"the version and the record are discarded in the same write")

	// The proof of the invariant: the next pass starts nothing.
	_, to := migrationBundles(t)
	next := storedPlatform(t, r)
	_, handled, res, err := r.gateMessagingMigration(context.Background(), next, to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Nil(t, next.Status.Upgrade, "a finished migration does not start itself again")
}

// TestFinishMigrationDiscardsTheRecordAndPinsTheVersionInOneUpdate proves the invariant above
// where it actually lives — in the WRITES, not in the state they add up to. Every status update
// the completion makes is captured, and the intermediate the split version of this code would
// produce (the record already gone, the reported version still the source) must never be one of
// them: a reconcile that read it would see a platform running 2.18.0 with 2.19.0 requested and
// nothing recorded, and start the whole migration again.
func TestFinishMigrationDiscardsTheRecordAndPinsTheVersionInOneUpdate(t *testing.T) {
	p := cleanupPlatform()
	p.Status.ObservedVersion = platformVersion218

	var written []*otilmv1alpha1.Platform
	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if pl, isPlatform := obj.(*otilmv1alpha1.Platform); isPlatform {
				written = append(written, pl.DeepCopy())
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	_, _, _, err := cleanupPass(t, r)
	require.NoError(t, err)

	cleared := 0
	for i, w := range written {
		if w.Status.Upgrade != nil {
			assert.Equal(t, platformVersion218, w.Status.ObservedVersion,
				"write %d: the reported version moves only as the record is discarded", i)
			continue
		}
		cleared++
		assert.Equal(t, platformVersion219, w.Status.ObservedVersion,
			"write %d: the write that discards the record carries the version the platform reached", i)
	}
	assert.Equal(t, 1, cleared, "the record is discarded exactly once, and in a single write")

	stored := storedPlatform(t, r)
	assert.Nil(t, stored.Status.Upgrade)
	assert.Equal(t, platformVersion219, stored.Status.ObservedVersion)
}

// TestFinishMigrationRollsBackWhenTheWriteFails: a conclusion the cluster did not accept must
// not survive in the copy the rest of the pass reads either.
func TestFinishMigrationRollsBackWhenTheWriteFails(t *testing.T) {
	p := cleanupPlatform(otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 1})
	p.Status.ObservedVersion = platformVersion218
	r, _ := cleanupReconciler(t, p, idleBroker(), failingStatusUpdate(errors.New(errStatusWriteRejected)))

	_, handled, _, err := cleanupPass(t, r)
	require.Error(t, err)
	assert.True(t, handled)

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the migration is still recorded")
	assert.Equal(t, platformVersion218, stored.Status.ObservedVersion)
}

// TestAbortMigrationLeavesTheReportedVersionAlone is the other half of the shared conclusion: an
// abort keeps the platform on the version it never left.
func TestAbortMigrationLeavesTheReportedVersionAlone(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining, fencedScheduler())
	r, _ := cleanupReconciler(t, p, idleBroker(), interceptor.Funcs{})

	require.NoError(t, r.abortMigration(context.Background(), p))

	stored := storedPlatform(t, r)
	assert.Nil(t, stored.Status.Upgrade)
	assert.Equal(t, platformVersion218, stored.Status.ObservedVersion,
		"an abort reports the version the platform is still running")
	assert.Equal(t, int32(1), replicasOf(t, r, schedulerWorkloadName))
}
