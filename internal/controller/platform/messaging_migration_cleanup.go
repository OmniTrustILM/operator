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

// messaging_migration_cleanup.go is the CLEANINGUP phase of the messaging-migration engine:
// with the platform serving from the target virtual host, it reclaims the topology it moved
// away from and then finishes the migration.
//
// THIS IS THE ONE PLACE THE OPERATOR DELETES rabbitmq.com OBJECTS. Everywhere else they are
// excluded from the prune and reclaimed only by an explicit deletionPolicy=Delete teardown,
// precisely because deleting one destroys data no reconcile can put back. The gates below are
// therefore not defensive garnish — they are the whole of this file's reason to exist.
//
// THE FINAL BARRIER. Three clean drain polls are a SAMPLE, not a guarantee: Core keeps running
// throughout the migration and keeps write permission on the source exchanges, so a message can
// arrive after the last clean sample. Nothing here is ever deleted on the strength of that
// earlier result. Before EVERY delete the phase re-establishes, from the broker itself, that
//
//  1. NO client connection is open on the source virtual host — a still-attached remote proxy or
//     time-quality monitor is a client the platform would strand, and
//  2. a FINAL, freshly taken queue snapshot shows nothing outstanding — every queue the broker
//     reports, not merely the ones a bundle happens to list, so a queue no version knows about
//     blocks too.
//
// The single exemption is the bundle's latest-only retention queues: they are bounded to one
// message their publisher keeps refreshing, so they are DESIGNED never to empty (requiring them
// would make every cleanup block forever) and their retained value is re-published on the target
// virtual host. The exemption is derived from the bundle's declared queue arguments, exactly as
// the drain derives it — never from a list of names.
//
// FAIL CLOSED. A broker that does not answer, credentials that cannot be read, a listing that
// did not parse: none of them is evidence that anything is safe to delete, so every one of them
// HOLDS the cleanup. The phase's deadline is what ends the wait, and it ends it by saying so.
//
// ONE CLASS AT A TIME, COMPLETING BEFORE THE NEXT. Deletion runs bindings → queues → exchanges →
// permissions → virtual host, and each class must be GONE before the next is touched: issuing
// them all in one pass leaves every class terminating concurrently behind the Messaging Topology
// Operator's finalizers, and a virtual host deleted out from under them strands the rest. The
// progress state is the OBJECTS THEMSELVES — the first class with anything left is the class this
// pass works on — so a restarted operator resumes exactly where it was without a counter to
// disagree with the cluster.
//
// SECURITY: the deletion targets are the source render's exact object identities, and those names
// encode the virtual host they are scoped to. So no name, kind-qualified or not, ever reaches a
// condition, an Event or an error — progress is reported as the class being reclaimed, in prose.

import (
	"context"
	"fmt"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/rabbitmq"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Condition and Event vocabulary the cleanup adds.
const (
	// reasonMigrationCleanupBlocked: the cleanup could not establish that the source virtual
	// host is idle within the phase's budget. The platform is fully functional on the target
	// topology; only the reclaim of the old one is held.
	reasonMigrationCleanupBlocked = "CleanupBlocked"
	// reasonMigrationCleanupError: the cleanup could not READ the state it gates on (the
	// topology objects it would delete). Nothing was deleted — the engine simply could not
	// establish what is still there.
	reasonMigrationCleanupError = "MigrationCleanupError"
	// reasonMigrationCompleted: the migration finished — the source topology is reclaimed, the
	// fence is fully lifted and the record is discarded.
	reasonMigrationCompleted = "MigrationCompleted"
	// eventMigrationCompleted announces a finished migration.
	eventMigrationCompleted = "MessagingMigrationCompleted"
)

// reclaimClass is one class of source topology object — every Binding, say — together with the
// Kind that names it. The classes are deleted in dependency order, one whole class per pass.
type reclaimClass struct {
	// kind is the rabbitmq.com Kind of every object in the class.
	kind string
	// objects are the class's source-rendered identities.
	objects []client.Object
}

// migrationReclaimStages describes each reclaimable class for the migration condition's message.
// A topology object's NAME encodes the virtual host it is scoped to, so the condition names the
// CLASS in prose and never an object.
var migrationReclaimStages = map[string]string{
	"Binding":    "reclaiming the previous messaging topology's bindings",
	"Queue":      "reclaiming the previous messaging topology's queues",
	"Exchange":   "reclaiming the previous messaging topology's exchanges",
	"Permission": "reclaiming the previous messaging topology's permissions",
	"Vhost":      "reclaiming the previous messaging topology's virtual host",
}

// migrationCleaningUpPhase reclaims the source topology one class per pass and finishes the
// migration once nothing of it is left.
//
// It returns handled=FALSE throughout, exactly as the cutover does. The platform is live on the
// target topology for the whole of this phase, so it must go on converging as one — its children
// applied, its readiness measured, its status written, the last fenced workload held down behind
// every apply. Short-circuiting the reconcile here would freeze all of that for as long as the
// reclaim takes, which on the blocked path is indefinitely.
func (r *Reconciler) migrationCleaningUpPhase(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	render.requeue = true

	classes, err := migrationReclaimClasses(p)
	if err != nil {
		res, derr := r.degraded(ctx, p, reasonMigrationStateError, err)
		return render, true, res, derr
	}

	// WHAT IS LEFT is measured before anything else, and it is also the termination condition:
	// once the virtual host is gone the management API can no longer answer for it, so a phase
	// that consulted the broker first would fail closed forever on the very state that means
	// success.
	class, extant, err := r.firstExtantReclaimClass(ctx, p, classes)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationCleanupError, err)
		return render, true, res, aerr
	}
	if len(extant) == 0 {
		return r.finishMigration(ctx, p, render)
	}

	forced := migrationForceCutoverAuthorized(p)
	if !forced && !r.sourceVirtualHostIdle(ctx, p) {
		return r.holdCleanup(ctx, p, render)
	}
	if forced {
		// Record the authorisation as consumed BEFORE acting on it, so a reclaim that starts
		// discarding cannot be turned back into a waiting one by a later spec edit.
		if werr := r.consumeMigrationForce(ctx, p); werr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, werr)
			return render, true, res, aerr
		}
		// The authorisation is explicit and attempt-scoped: whatever is still attached to the
		// source virtual host is disconnected, and whatever it still holds is discarded with it.
		r.closeSourceConnections(ctx, p)
	}

	// Write the class down BEFORE deleting it. The class is re-measured from the cluster on every
	// pass, so this is not how the cleanup resumes — that is what object existence is for — but
	// it keeps the engine's rule intact: nothing is destroyed that the platform has not first
	// said it was about to destroy, and a status write that fails stops the pass.
	if werr := r.recordCleanupProgress(ctx, p, migrationCleanupMessage(p.Status.Upgrade, class.kind, forced)); werr != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, werr)
		return render, true, res, aerr
	}

	if derr := r.deleteReclaimClass(ctx, extant); derr != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationCleanupError, derr)
		return render, true, res, aerr
	}
	// The next pass re-measures: this class is advanced past only once the broker's operator has
	// actually finished removing it, finalizers included.
	return render, false, ctrl.Result{}, nil
}

// holdCleanup keeps the source topology while the barrier is not satisfied, and turns the wait
// into an explicit, actionable terminal state once the phase's budget is spent.
func (r *Reconciler) holdCleanup(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if migrationCleanupDeadlineExceeded(p, time.Now()) {
		return r.blockCleanup(ctx, p, render)
	}
	if werr := r.recordCleanupProgress(ctx, p, migrationCleanupWaitingMessage(p.Status.Upgrade)); werr != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, werr)
		return render, true, res, aerr
	}
	return render, false, ctrl.Result{}, nil
}

// blockCleanup reports a reclaim that has not been able to start within the phase's budget.
//
// Nothing is unwound: the migration is long past the point where there was anything to go back
// to, and the platform is serving the target topology at full strength. What is held is only the
// deletion of the previous one — which is why this state is safe to sit in, and why the message
// has to say so and name every way out of it.
//
// The engine keeps re-checking on its own cadence rather than stopping: the two remedies are
// changes OUTSIDE the cluster (a proxy re-enrolling, a monitor re-pointed), and nothing about
// either would re-enqueue this Platform. A cleanup that stopped looking would sit blocked
// forever after the user had already done exactly what it asked.
func (r *Reconciler) blockCleanup(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if migrationBlockedFor(p, reasonMigrationCleanupBlocked) {
		return render, false, ctrl.Result{}, nil
	}

	message := migrationCleanupBlockedMessage(p)
	if err := r.writeMigrationState(ctx, p, metav1.ConditionFalse, reasonMigrationCleanupBlocked, message); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return render, true, res, aerr
	}
	r.event(p, corev1.EventTypeWarning, eventMigrationBlocked, message)
	return render, false, ctrl.Result{}, nil
}

// migrationCleanupBlockedMessage is the terminal state's text. It names BOTH remedies — the
// remote proxies the platform can re-enrol, and the time-quality monitor, an external component
// that republishes to whichever broker it is pointed at and which the operator neither manages
// nor migrates — plus the loss-accepting exit, and it states plainly that the platform itself is
// fine. Versions and spec field paths only: no virtual host, no host, no credential.
func migrationCleanupBlockedMessage(p *otilmv1alpha1.Platform) string {
	u := p.Status.Upgrade
	return fmt.Sprintf(
		"the messaging migration to platform version %s has cut over, but the messaging topology it moved away from still has clients "+
			"attached or messages outstanding, so it could not be reclaimed within %s; the platform is fully functional on the new "+
			"messaging topology throughout this state — re-enrol the remote proxies so they reconnect through the new topology, and "+
			"re-point the time-quality monitor (an external component the operator does not manage and cannot migrate) at it, or set "+
			"spec.messaging.managed.forceCutoverForVersion to %q to reclaim the previous topology now and discard whatever it still holds",
		u.ToVersion, migrationDrainTimeout(p), u.ToVersion)
}

// migrationCleanupWaitingMessage is the condition's text while the barrier is not yet satisfied.
func migrationCleanupWaitingMessage(u *otilmv1alpha1.UpgradeStatus) string {
	return fmt.Sprintf("%s (waiting for the previous messaging topology to fall idle before reclaiming it)",
		migrationPhaseMessage(u))
}

// migrationCleanupMessage is the condition's text while a class is being reclaimed. A forced
// reclaim says so: it is discarding whatever the source virtual host still holds.
func migrationCleanupMessage(u *otilmv1alpha1.UpgradeStatus, kind string, forced bool) string {
	stage, described := migrationReclaimStages[kind]
	if !described {
		stage = "reclaiming the previous messaging topology"
	}
	if forced {
		stage += ", discarding whatever it still holds as spec.messaging.managed.forceCutoverForVersion authorises"
	}
	return fmt.Sprintf("%s (%s)", migrationPhaseMessage(u), stage)
}

// recordCleanupProgress persists what the cleanup is doing. An UNCHANGED message writes nothing,
// so a long wait costs one status write per change of state rather than one per pass.
func (r *Reconciler) recordCleanupProgress(ctx context.Context, p *otilmv1alpha1.Platform, message string) error {
	reason := string(otilmv1alpha1.MigrationPhaseCleaningUp)
	c := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	if c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason && c.Message == message {
		return nil
	}
	return r.writeMigrationState(ctx, p, metav1.ConditionTrue, reason, message)
}

// migrationCleanupDeadlineExceeded reports whether the reclaim has outlived its budget, measured
// from the phase's own start. spec.messaging.managed.drainTimeout bounds this phase as it bounds
// the reversible ones, but it means something different here: not "unwind", which is no longer
// possible, but "stop waiting silently and say what is holding it".
func migrationCleanupDeadlineExceeded(p *otilmv1alpha1.Platform, now time.Time) bool {
	u := p.Status.Upgrade
	if u == nil || u.PhaseStartedAt.IsZero() || u.Phase != otilmv1alpha1.MigrationPhaseCleaningUp {
		return false
	}
	return now.Sub(u.PhaseStartedAt.Time) > migrationDrainTimeout(p)
}

// finishMigration ends the migration: whatever the fence still holds is restored, the version the
// platform has actually reached is pinned, and the record is discarded.
//
// A cutover that ran to completion leaves nothing fenced — it releases Core's dependencies at its
// stage 2 and the gateway at its stage 4 — so the restore here is the SAFETY NET rather than the
// ordinary path: a migration forced through, or one whose cutover was interrupted between the two,
// must never end with a workload parked at zero replicas and no record left to bring it back.
//
// The version and the record move in ONE write, deliberately. A cleared record beside a status
// still naming the source version reads, to the very next reconcile, as a fresh request to
// upgrade — and the engine would start the whole migration again, fencing the producers to drain
// a virtual host that has just been reclaimed.
func (r *Reconciler) finishMigration(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	u := p.Status.Upgrade
	message := fmt.Sprintf(
		"the messaging migration from platform version %s to %s is complete; the platform is running on the new messaging topology and the previous one has been reclaimed",
		u.FromVersion, u.ToVersion)

	if err := r.concludeMigration(ctx, p, u.ToVersion, reasonMigrationCompleted, message, eventMigrationCompleted); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return render, true, res, aerr
	}
	// Nothing is left to wait for: the platform reconciles on its ordinary cadence from here.
	render.requeue = false
	return render, false, ctrl.Result{}, nil
}

// migrationReclaimClasses returns the SOURCE topology objects this cleanup may delete, grouped by
// class and ordered by dependency.
//
// The targets are the source render's EXACT identities. The renderer is deterministic and its
// object names are scoped by virtual host, so re-rendering the version the migration is moving
// away from reproduces precisely the objects that were applied for it — and a migration only ever
// runs when the two virtual hosts differ, which is what makes the two sets disjoint.
//
// Anything the TARGET render also produces is subtracted regardless. That is belt and braces on
// top of the disjointness — the broker-global Users are the one set both renders share, and they
// are excluded by class anyway — but it is the guard that matters: this file may never delete an
// object the running platform still depends on.
func migrationReclaimClasses(p *otilmv1alpha1.Platform) ([]reclaimClass, error) {
	source, err := migrationSourceMessaging(p)
	if err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(source))
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		live[managedObjectIdentity(obj)] = struct{}{}
	}

	byKind := make(map[string][]client.Object, len(source))
	for _, obj := range source {
		if _, kept := live[managedObjectIdentity(obj)]; kept {
			continue
		}
		byKind[obj.GetObjectKind().GroupVersionKind().Kind] = append(
			byKind[obj.GetObjectKind().GroupVersionKind().Kind], obj)
	}

	kinds := platformbuilder.ManagedMessagingReclaimKinds()
	classes := make([]reclaimClass, 0, len(kinds))
	for _, kind := range kinds {
		if objs := byKind[kind]; len(objs) > 0 {
			classes = append(classes, reclaimClass{kind: kind, objects: objs})
		}
	}
	return classes, nil
}

// migrationSourceMessaging renders the managed topology of the version the migration is moving
// away from, on a DEEP COPY: the reconcile that follows this gate renders the TARGET, and pinning
// the source onto the live object would take it with them.
func migrationSourceMessaging(p *otilmv1alpha1.Platform) ([]client.Object, error) {
	// A source version this operator build no longer carries has to be an ERROR: the renderer
	// falls back to the default bundle for a version it cannot resolve, and that render would
	// name a topology this platform never had.
	if _, err := migrationSourceBundle(p); err != nil {
		return nil, err
	}
	src := p.DeepCopy()
	src.Spec.Version = p.Status.Upgrade.FromVersion
	return platformbuilder.ResolveManagedMessaging(src), nil
}

// managedObjectIdentity is a rendered object's identity for set comparison: its GroupVersionKind
// and name (every managed object lives in the Platform's own namespace).
func managedObjectIdentity(obj client.Object) string {
	return obj.GetObjectKind().GroupVersionKind().String() + "/" + obj.GetName()
}

// firstExtantReclaimClass returns the first class, in deletion order, that still has objects in
// the cluster — the class this pass is allowed to work on.
//
// A read that fails is returned as an error rather than folded into "absent": concluding that an
// object is gone because the apiserver could not be asked would advance the cleanup to the next
// class over a dependency that is still there. A Kind the cluster no longer SERVES is different
// and is read as absent — the CRD being gone is positive proof that no object of it exists.
func (r *Reconciler) firstExtantReclaimClass(ctx context.Context, p *otilmv1alpha1.Platform, classes []reclaimClass) (reclaimClass, []client.Object, error) {
	for _, class := range classes {
		extant := make([]client.Object, 0, len(class.objects))
		for _, obj := range class.objects {
			declared, err := r.topologyObjectExists(ctx, p.Namespace, obj)
			if err != nil {
				return reclaimClass{}, nil, err
			}
			if declared {
				extant = append(extant, obj)
			}
		}
		if len(extant) > 0 {
			return class, extant, nil
		}
	}
	return reclaimClass{}, nil, nil
}

// topologyObjectExists reports whether one source topology object is still in the cluster. An
// object that is terminating — deleted, but still held by the Topology Operator's finalizer —
// still EXISTS, which is exactly what keeps the cleanup on this class until the broker has
// really let it go.
func (r *Reconciler) topologyObjectExists(ctx context.Context, namespace string, obj client.Object) (bool, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	var probe unstructured.Unstructured
	probe.SetGroupVersionKind(gvk)

	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: obj.GetName()}, &probe)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	default:
		// Kind and API reason only: the object's NAME carries the virtual-host scope, and so
		// does the apiserver's own error text — safeErrorf publishes neither while keeping the
		// API error reachable for the transience classification.
		return false, safeErrorf(err,
			"reading a %s of the previous messaging topology to check whether it is still declared failed (%s)",
			gvk.Kind, apiFailureReason(err))
	}
}

// deleteReclaimClass deletes every object of one class. An object already gone is not an error —
// a class part-way through its own deletion is simply re-measured on the next pass.
func (r *Reconciler) deleteReclaimClass(ctx context.Context, objs []client.Object) error {
	for _, obj := range objs {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			// Kind and API reason only, for the same reason topologyObjectExists gives.
			return safeErrorf(err, "deleting a %s of the previous messaging topology failed (%s)",
				obj.GetObjectKind().GroupVersionKind().Kind, apiFailureReason(err))
		}
	}
	return nil
}

// sourceVirtualHostIdle reports whether the source virtual host is quiet enough to reclaim: no
// client attached, and nothing outstanding in a freshly taken snapshot of every queue on it.
//
// It answers a single bool because every negative answer means the same thing — DO NOT DELETE —
// and folding the failures into it is the fail-closed rule made structural: there is no error
// return a caller could mistake for permission. The cause is logged, never surfaced, because it
// may name the broker.
func (r *Reconciler) sourceVirtualHostIdle(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	logger := log.FromContext(ctx)

	admin, vhost, source, err := r.sourceVirtualHostAdmin(ctx, p)
	if err != nil {
		logger.V(1).Info("messaging migration cleanup: the source virtual host could not be inspected, so nothing is reclaimed",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "cause", err.Error())
		return false
	}

	// ZERO CONNECTIONS FIRST. A client still attached is one that would be cut off by the delete,
	// and it is also one that may still be publishing — which is what makes the snapshot below
	// meaningful rather than a race against a live producer.
	connections, err := admin.Connections(ctx, vhost)
	if err != nil {
		logger.V(1).Info("messaging migration cleanup: the source virtual host's connections could not be counted",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "cause", err.Error())
		return false
	}
	if connections > 0 {
		logger.V(1).Info("messaging migration cleanup: clients are still attached to the source virtual host",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "connections", connections)
		return false
	}

	states, err := admin.Queues(ctx, vhost)
	if err != nil {
		logger.V(1).Info("messaging migration cleanup: the final queue snapshot could not be taken",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "cause", err.Error())
		return false
	}
	if outstanding := outstandingSourceQueues(source, states); outstanding > 0 {
		logger.V(1).Info("messaging migration cleanup: the source virtual host still holds messages",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "queues", outstanding)
		return false
	}
	return true
}

// outstandingSourceQueues counts the queues in a final snapshot that block the reclaim: EVERY
// queue the broker reports on the virtual host, minus the source bundle's latest-only retention
// queues.
//
// The snapshot is deliberately not filtered against a list of expected queues the way the drain's
// is. The drain asks "has what the platform publishes stopped arriving", and a queue it does not
// know about is not evidence against that; this asks "is it safe to destroy this virtual host",
// and a queue nobody expected holding messages is exactly the case that must block.
//
// The retention queues are the one exemption, on the same BOM-derived basis the drain uses: they
// are bounded to a single message their publisher keeps refreshing, so they never empty, and the
// value is re-published on the target virtual host — waiting for them would block every migration
// forever while protecting nothing.
func outstandingSourceQueues(source bom.Bundle, states []rabbitmq.QueueState) int {
	retained := make(map[string]struct{}, len(source.Messaging.Queues))
	for _, q := range source.Messaging.Queues {
		if q.IsLatestOnlyRetention() {
			retained[q.Name] = struct{}{}
		}
	}

	outstanding := 0
	for _, s := range states {
		if _, exempt := retained[s.Name]; exempt {
			continue
		}
		if s.MessagesReady+s.MessagesUnacked > 0 {
			outstanding++
		}
	}
	return outstanding
}

// closeSourceConnections force-closes whatever is still attached to the source virtual host, on
// the forced path only.
//
// A failure is logged and not returned: force is an explicit instruction to proceed and discard,
// so a broker that will not answer must not be able to hold the reclaim it authorises. Deleting
// the virtual host closes its connections anyway; this only makes that graceful when it can be.
func (r *Reconciler) closeSourceConnections(ctx context.Context, p *otilmv1alpha1.Platform) {
	admin, vhost, _, err := r.sourceVirtualHostAdmin(ctx, p)
	if err == nil {
		err = admin.CloseConnections(ctx, vhost)
	}
	if err != nil {
		log.FromContext(ctx).V(1).Info("messaging migration cleanup: the source virtual host's connections could not be closed; the forced reclaim proceeds",
			"phase", otilmv1alpha1.MigrationPhaseCleaningUp, "cause", err.Error())
	}
}

// sourceVirtualHostAdmin builds the management-API client for the migration's SOURCE virtual
// host, and returns that virtual host and the source bundle alongside it.
//
// SECURITY: the credentials are read into memory for the length of the call and go nowhere else.
func (r *Reconciler) sourceVirtualHostAdmin(ctx context.Context, p *otilmv1alpha1.Platform) (rabbitmq.BrokerAdmin, string, bom.Bundle, error) {
	source, err := migrationSourceBundle(p)
	if err != nil {
		return nil, "", bom.Bundle{}, err
	}
	admin, err := r.sourceBrokerAdmin(ctx, p)
	if err != nil {
		return nil, "", bom.Bundle{}, err
	}
	// Derived from the SOURCE bundle, not from the resolved connection: by this phase the
	// platform's own messaging points at the target virtual host.
	return admin, platformbuilder.ManagedVirtualHostFor(p, source), source, nil
}
