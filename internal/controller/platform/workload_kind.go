package platform

import (
	"context"
	"fmt"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// conditionWorkloadKindSwitch is the DURABLE marker that a component's workloadType switch is
// mid-orchestration.
//
// It exists because OBJECT PRESENCE is not enough. Between the superseded workload's foreground
// delete completing and the new kind being applied, NEITHER object exists — and in that window a
// messaging migration would start, record the requested producer as Absent, and read that as
// "this producer is stopped" while the switch is still under way. The marker is written BEFORE
// the delete (the migration engine's own write-status-before-act discipline) and cleared only
// once the cluster actually carries the new kind, so the window is never invisible.
const conditionWorkloadKindSwitch = "WorkloadKindSwitch"

// Condition/Event reasons the workload-kind switch produces. They are identity-free labels.
const (
	// reasonWorkloadKindSwitch: a switch is in flight — the marker condition's reason while it
	// runs, and the Event reason when the superseded workload is stopped.
	reasonWorkloadKindSwitch = "WorkloadKindSwitch"
	// reasonWorkloadKindSwitchSettled: the cluster carries the rendered kind for every
	// component and nothing of the previous kind is left, so the marker is retired.
	reasonWorkloadKindSwitchSettled = "WorkloadKindSwitchSettled"
	// reasonWorkloadKindSwitchError: a superseded workload could not be read, recorded or
	// stopped. Routed through applyOrDegrade so a transient API failure requeues.
	reasonWorkloadKindSwitchError = "WorkloadKindSwitchError"
)

// workloadSwitchRequeueAfter is how soon a reconcile that stopped a superseded workload looks
// again. The Owns watch already re-triggers when the old object finally disappears; this is the
// backstop that keeps the switch moving if that event is ever missed.
//
// It reaches nextRequeueResult only on a pass that runs the full route through to
// finalizeReconcile. A pass that short-circuits EARLIER — enforceMigrationFence's own error
// return, or a gated apply failure in gateEdgeAdminAndMonitors — reports that step's own
// requeue/backoff instead, and this cadence is skipped for that one pass. That is not a stuck
// switch: the Owns watch still re-triggers on the old object's deletion regardless of which
// requeue fired, and the step that short-circuited already asks for its own retry, which reaches
// composeAndApplyBase again and re-evaluates the switch exactly as this backstop would have.
const workloadSwitchRequeueAfter = 5 * time.Second

// workloadKindChange names an outstanding workloadType switch: the workload's object name (also
// the component's role name), the apps/v1 Kind LIVE in the cluster, and the Kind the render
// now produces for it.
type workloadKindChange struct {
	// Name is the workload's object name.
	Name string
	// Live is the apps/v1 Kind of the controller-owned object currently in the cluster.
	Live string
	// Rendered is the apps/v1 Kind this pass renders for the same component.
	Rendered string
	// Marker holds the sentence the DURABLE marker recorded, and is set ONLY when the switch is
	// known from that marker alone — the window in which neither kind's object exists, so there
	// is nothing live to compare the render against.
	Marker string
}

// describe returns the one-clause description of an outstanding switch: read from the live
// cluster when both kinds can be compared, and from the durable marker otherwise. Names and
// kinds only — never a coordinate.
func (c workloadKindChange) describe() string {
	if c.Marker != "" {
		return c.Marker
	}
	return fmt.Sprintf("workload %q is rendered as a %s but the cluster is still running it as a %s",
		c.Name, c.Rendered, c.Live)
}

// workloadKindOf returns the apps/v1 Kind of a rendered workload object, and "" for anything
// that is not a workload.
func workloadKindOf(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return string(otilmv1alpha1.WorkloadKindDeployment)
	case *appsv1.StatefulSet:
		return string(otilmv1alpha1.WorkloadKindStatefulSet)
	default:
		return ""
	}
}

// supersededWorkloadKind returns the apps/v1 Kind a workloadType flip leaves behind — the OTHER
// kind — and "" for anything that is not a workload at all.
func supersededWorkloadKind(obj client.Object) string {
	switch workloadKindOf(obj) {
	case string(otilmv1alpha1.WorkloadKindDeployment):
		return string(otilmv1alpha1.WorkloadKindStatefulSet)
	case string(otilmv1alpha1.WorkloadKindStatefulSet):
		return string(otilmv1alpha1.WorkloadKindDeployment)
	default:
		return ""
	}
}

// supersededWorkload returns an empty typed object of the OTHER apps/v1 kind for a rendered
// workload — the object a workloadType flip leaves behind — and false for anything that is not
// a workload at all. The empty object comes from workloadObject (migration_fence.go), which is
// this package's single kind → empty-object constructor.
func supersededWorkload(obj client.Object) (client.Object, bool) {
	kind := supersededWorkloadKind(obj)
	if kind == "" {
		return nil, false
	}
	old, err := workloadObject(kind)
	return old, err == nil
}

// workloadKindSwitchInFlight reads the DURABLE marker: it reports whether a workloadType switch
// is mid-orchestration and returns the sentence recorded with it (which names the workload and
// both kinds).
func workloadKindSwitchInFlight(p *otilmv1alpha1.Platform) (string, bool) {
	c := meta.FindStatusCondition(p.Status.Conditions, conditionWorkloadKindSwitch)
	if c == nil || c.Status != metav1.ConditionTrue {
		return "", false
	}
	return c.Message, true
}

// setWorkloadKindSwitchCondition records the marker, following the package's convention of
// stamping observedGeneration on every condition it sets. Workload names and apps/v1 kinds
// only — never a coordinate.
func setWorkloadKindSwitchCondition(p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionWorkloadKindSwitch, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation,
	})
}

// markWorkloadKindSwitch persists the marker BEFORE the superseded workload is deleted.
//
// That order is the crash/visibility contract, and it is the same one writeMigrationState
// argues for: an effect must never run against a state the cluster does not know about. Once
// the object is gone, object presence can no longer tell anyone a switch is under way — so if
// the marker were written afterwards, a crash (or simply a migration trigger firing in the
// interval) would see a clean platform mid-switch. The opposite order is safe: a recorded marker
// whose delete has not happened yet is re-derived and re-issued on the next pass, because the
// delete is idempotent. Already recorded ⇒ no second write.
func (r *Reconciler) markWorkloadKindSwitch(ctx context.Context, p *otilmv1alpha1.Platform, change workloadKindChange) error {
	if _, inFlight := workloadKindSwitchInFlight(p); inFlight {
		return nil
	}
	setWorkloadKindSwitchCondition(p, metav1.ConditionTrue, reasonWorkloadKindSwitch,
		fmt.Sprintf("workload %q is being switched from a %s to a %s", change.Name, change.Live, change.Rendered))
	return r.writeStatus(ctx, p)
}

// renderedWorkloadsSettled reports whether every workload this pass renders is present in the
// cluster AND NOT TERMINATING — half of "the switch has settled".
//
// THE deletionTimestamp CHECK IS LOAD-BEARING, not tidiness. Consider a user who flips
// core.workloadType to StatefulSet (marker written, the Deployment foreground-deleted) and then
// REVERTS it to Deployment while that delete is still draining pods:
//
//   - pendingWorkloadKindChange probes only the SUPERSEDED kind, which is now the StatefulSet —
//     and that was never created, so the cluster half reports nothing;
//   - the Deployment is still THERE, finalizer and all, so a bare existence check calls this
//     settled.
//
// The marker would then be retired and the switch requeue dropped while the deletion still had
// the workload to remove — and the gap that opens seconds later is UNMARKED, so a messaging
// migration could start into it and nothing but a watch event would bring the workload back.
// A terminating object has not settled: it is going away, and the render has to re-create it.
func (r *Reconciler) renderedWorkloadsSettled(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	for _, obj := range platformbuilder.RenderPlatformBase(p) {
		kind := workloadKindOf(obj)
		if kind == "" {
			continue
		}
		live, err := workloadObject(kind)
		if err != nil {
			return false, err
		}
		key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		if err := r.Get(ctx, key, live); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("reading the rendered workload %q: %w", key.Name, err)
		}
		if live.GetDeletionTimestamp() != nil {
			return false, nil // present, but on its way out — see the doc above
		}
	}
	return true, nil
}

// terminatingOwnedWorkload reports whether ANY workload object this Platform controls is still
// terminating — of EITHER apps/v1 kind, and whether or not this pass still renders it.
//
// It is the half of "settled" that cannot be asked of the render, and the reason is that a
// component can leave the render WHILE its objects are still being torn down. Flip
// provisioning to external mid-switch and its Deployment vanishes from RenderPlatformBase, so
// pendingWorkloadKindChange has no object to compare and renderedWorkloadsSettled has nothing
// to look for — both report "settled" while the Deployment is still foreground-deleting with
// pods attached. Retiring the marker there re-opens exactly the window the marker exists to
// cover: a messaging migration would start, record the absent producer as stopped, and drain
// against one that is still publishing.
//
// Object-based, so it holds regardless of what the current render happens to contain. The
// membership rule is the prune's: the operator's own label selector, plus a controller owner
// reference for THIS Platform — a label match alone is never this operator's object.
func (r *Reconciler) terminatingOwnedWorkload(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	selector := client.MatchingLabels{
		common.ManagedByLabel: common.ManagedByValue,
		common.InstanceLabel:  p.Name,
	}
	for _, list := range []client.ObjectList{&appsv1.DeploymentList{}, &appsv1.StatefulSetList{}} {
		if err := r.List(ctx, list, client.InNamespace(p.Namespace), selector); err != nil {
			return false, fmt.Errorf("listing %T to check the workload-kind switch has settled: %w", list, err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return false, fmt.Errorf("extracting %T to check the workload-kind switch has settled: %w", list, err)
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			if obj.GetDeletionTimestamp() != nil && controllerOwnedBy(obj, p.UID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// clearWorkloadKindSwitchIfSettled retires the durable marker once the cluster has converged on
// the rendered kinds, and reports whether a switch is still outstanding.
//
// SETTLED is all three together: no component still has a controller-owned workload of the
// OTHER kind, every workload this pass renders is present and NOT TERMINATING, and no workload
// object this Platform owns is terminating at all. The second condition makes the marker cover
// the two windows the first cannot see — the object gone and the new kind not yet applied, and
// (see renderedWorkloadsSettled) a revert to the ORIGINAL kind while that kind's own delete is
// still draining. The third covers the window neither can, because both read the RENDER: a
// component DE-RENDERED mid-switch (see terminatingOwnedWorkload). In all of them, only the
// marker stands between the window and a migration that would record the requested producer as
// "absent, therefore stopped".
//
// It runs AFTER this pass's applies, so the object the loop has just applied counts.
func (r *Reconciler) clearWorkloadKindSwitchIfSettled(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	if _, inFlight := workloadKindSwitchInFlight(p); !inFlight {
		return false, nil
	}
	if _, pending, err := r.pendingWorkloadKindChange(ctx, p); err != nil || pending {
		return true, err
	}
	settled, err := r.renderedWorkloadsSettled(ctx, p)
	if err != nil || !settled {
		return true, err
	}
	terminating, terr := r.terminatingOwnedWorkload(ctx, p)
	if terr != nil || terminating {
		return true, terr
	}
	setWorkloadKindSwitchCondition(p, metav1.ConditionFalse, reasonWorkloadKindSwitchSettled,
		"every component is running as the workload kind it renders as")
	if werr := r.writeStatus(ctx, p); werr != nil {
		return true, werr
	}
	return false, nil
}

// pendingWorkloadKindChange reports the FIRST component whose rendered workload kind differs
// from the controller-owned workload actually in the cluster.
//
// It is the CLUSTER half of the outstanding-switch signal (the other half is the durable
// marker — see outstandingWorkloadKindSwitch, which is what every caller should use). Both the
// migration refusals and the stop-before-start orchestration read the same predicate, so they
// can never disagree about whether a switch is in progress.
//
// The candidate set is the RENDER's own workload objects, so it cannot drift from what the
// apply loop produces. The kind a component renders as comes from spec.<component>.workloadType
// and does not depend on the selected bundle (see buildWorkload), so this answer is the same
// whichever version the pass is rendering — which is what makes it safe to call from the
// migration gate, where the render version is the SOURCE one.
//
// An object without THIS Platform's controller owner reference is ignored: it is somebody
// else's, and the operator neither blocks on it nor touches it.
func (r *Reconciler) pendingWorkloadKindChange(ctx context.Context, p *otilmv1alpha1.Platform) (workloadKindChange, bool, error) {
	for _, obj := range platformbuilder.RenderPlatformBase(p) {
		old, ok := supersededWorkload(obj)
		if !ok {
			continue
		}
		key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		if err := r.Get(ctx, key, old); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return workloadKindChange{}, false, fmt.Errorf("reading the superseded workload %q: %w", key.Name, err)
		}
		if !controllerOwnedBy(old, p.UID) {
			continue
		}
		return workloadKindChange{
			Name: key.Name, Live: workloadKindOf(old), Rendered: workloadKindOf(obj),
		}, true, nil
	}
	return workloadKindChange{}, false, nil
}

// outstandingWorkloadKindSwitch is the ONE signal every migration refusal reads: a switch is
// outstanding when the live cluster disagrees with the render, OR when the durable marker says
// one is mid-orchestration. The second half covers the window the first cannot see — after the
// superseded object is gone and before the new kind is applied, neither exists to compare.
func (r *Reconciler) outstandingWorkloadKindSwitch(ctx context.Context, p *otilmv1alpha1.Platform) (workloadKindChange, bool, error) {
	change, pending, err := r.pendingWorkloadKindChange(ctx, p)
	if err != nil || pending {
		return change, pending, err
	}
	if marker, inFlight := workloadKindSwitchInFlight(p); inFlight {
		return workloadKindChange{Marker: marker}, true, nil
	}
	return workloadKindChange{}, false, nil
}

// restoreMigrationConditionAfterKindSwitch rewrites a STALE kind-switch refusal back to the
// migration's current phase, and is the CORRECTION half of the refusal.
//
// The refusal is written with setMigrationCondition + steadyState, and NOTHING later undoes it:
// once the user restores the component's workloadType the guard simply stops firing, and the
// phase that resumes need not write migration state at all — migrationFencingPhase persists
// nothing while it is still waiting for producers to stop, because fenceWorkloads only writes
// while status.upgrade.fenced is empty and transitionMigrationPhase only runs once they ARE
// stopped. The Platform would sit with an active status.upgrade beside a False condition
// claiming the migration is refused — for as long as the fence takes.
//
// So the restore is unconditional for exactly that one reason, through the same
// writeMigrationState path every phase uses. Scope is deliberately narrow:
//
//   - only when a migration is actually RECORDED (the start refusal happens with no record, and
//     beginMigration's own ConditionTrue write overwrites it the moment the user corrects);
//   - only for reasonMigrationWorkloadKindChanged (the abort refusal leaves no record either, and
//     every other False reason — a drain timeout, an input-drift refusal — belongs to the guard
//     that wrote it and must not be cleared from here).
func (r *Reconciler) restoreMigrationConditionAfterKindSwitch(ctx context.Context, p *otilmv1alpha1.Platform) error {
	if p.Status.Upgrade == nil || !migrationBlockedFor(p, reasonMigrationWorkloadKindChanged) {
		return nil
	}
	return r.writeMigrationState(ctx, p, metav1.ConditionTrue,
		string(p.Status.Upgrade.Phase), migrationPhaseMessage(p.Status.Upgrade))
}

// migrationKindGuardStage names WHY the workload-kind guard is running. It decides the wording
// and — critically — whether status.upgrade may be read at all: on the START path the record
// does not exist yet, so nothing on that path may dereference it.
type migrationKindGuardStage int

const (
	// kindGuardStart: no migration is recorded and one is about to be.
	kindGuardStart migrationKindGuardStage = iota
	// kindGuardAbort: a migration was just unwound in this pass.
	kindGuardAbort
	// kindGuardInFlight: a migration is recorded and running.
	kindGuardInFlight
)

// migrationSourceVersion is the version a migration is moving away FROM: the recorded one while
// a migration is in flight and, before one exists, the version beginMigration WOULD record —
// read exactly as beginMigration reads it, from status.observedVersion.
//
// It exists so the START refusal can name the version the platform stays on WITHOUT touching
// status.upgrade, which is nil on that path.
func migrationSourceVersion(p *otilmv1alpha1.Platform) string {
	if p.Status.Upgrade != nil {
		return p.Status.Upgrade.FromVersion
	}
	return p.Status.ObservedVersion
}

// guardMigrationWorkloadKind refuses to run — or to START, or to ride along with an ABORT — a
// messaging migration while a workloadType switch is outstanding on ANY component.
//
// WHY REFUSED RATHER THAN ORCHESTRATED. The two move the same workloads for incompatible
// reasons and neither can see the other:
//
//   - The fence holds each producer at .spec.replicas=0 addressed by the kind it RECORDED, and
//     CORE IS DELIBERATELY NOT FENCED (it is the consumer that empties the queues the fence
//     stops filling). So a Core kind change is invisible to migrationWorkloadKindFlip, which
//     inspects only recorded fenced workloads.
//   - Starting a migration on top of an outstanding switch records the newly-requested, absent
//     kind as Absent=true — which the drain reads as "this producer is stopped" — while the
//     old producer of the other kind is still running and publishing into the very virtual
//     host being drained.
//   - A switch DELETES the superseded workload. During the staged cutover Core's workload is
//     withheld from the apply entirely, so the switch would stop Core without starting it.
//
// A refusal is the only answer that keeps the fence's record and the cluster in agreement. It
// is a DETERMINISTIC, user-correctable steady state — the same shape guardMigrationInputs uses
// for mid-flight input drift — and it names the ways out. handled=true stops the pass.
func (r *Reconciler) guardMigrationWorkloadKind(ctx context.Context, p *otilmv1alpha1.Platform, version string, stage migrationKindGuardStage) (bool, ctrl.Result, error) {
	change, outstanding, err := r.outstandingWorkloadKindSwitch(ctx, p)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonWorkloadKindSwitchError, err)
		return true, res, aerr
	}
	if !outstanding {
		// The switch is gone — which means a refusal THIS guard wrote on an earlier pass is now
		// stale, and no phase is obliged to rewrite it. Correct it here, before the pass proceeds.
		if rerr := r.restoreMigrationConditionAfterKindSwitch(ctx, p); rerr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, rerr)
			return true, res, aerr
		}
		return false, ctrl.Result{}, nil
	}

	// Read the source version through migrationSourceVersion, NEVER off status.upgrade: on the
	// start path there is no record yet, and the earlier draft of this guard panicked there.
	from := migrationSourceVersion(p)
	var reason, message string
	switch stage {
	case kindGuardStart:
		reason = reasonMigrationWorkloadKindPending
		// This refusal itself short-circuits the pass that would complete the switch — the
		// pending case never reaches the apply loop that would settle it, and the gap case has
		// no live object to revert a workloadType edit against — so neither "let it finish" nor
		// "revert the workloadType" is a working remedy here. Reverting spec.version to the
		// source IS: the guard is only reached because a migration was about to start, so
		// un-requesting the target lets THIS pass proceed, which lets the switch complete on its
		// own, after which the target version can be requested again.
		message = fmt.Sprintf(
			"%s, so the messaging migration to platform version %s cannot start: the fence would record a kind the "+
				"cluster has not settled on, and the newly requested kind would be recorded as stopped while the old one "+
				"was still publishing — revert spec.version to %s until the workloadType switch settles, then request "+
				"%s again",
			change.describe(), version, from, version)
	case kindGuardAbort:
		reason = reasonMigrationAbortWorkloadKind
		message = fmt.Sprintf(
			"the messaging migration to platform version %s was aborted in this pass, and %s; the workloadType switch was "+
				"deliberately NOT carried out alongside the abort — the fence had to be lifted first, and a delete issued "+
				"in the same pass would race it — so it is left to the next reconcile, which orchestrates it "+
				"stop-before-start with no migration recorded; revert that component's workloadType if you no longer want it",
			version, change.describe())
	default: // kindGuardInFlight
		reason = reasonMigrationWorkloadKindChanged
		message = fmt.Sprintf(
			"%s, and the messaging migration to platform version %s is in flight; the migration fence tracks producers by "+
				"the kind it recorded and never fences core at all, so completing the switch now could leave a producer of "+
				"the other kind publishing into the virtual host being drained — restore that component's workloadType "+
				"until the migration finishes, or revert spec.version to %s to abort it while it is still fencing or draining",
			change.describe(), version, from)
	}
	setMigrationCondition(p, metav1.ConditionFalse, reason, message)
	res, serr := r.steadyState(ctx, p, reason, message)
	return true, res, serr
}

// stopSupersededWorkload enforces STOP-BEFORE-START on a workloadType change, for a platform
// with NO messaging migration recorded (the migration case is refused outright — see
// guardMigrationWorkloadKind).
//
// A Deployment and a StatefulSet of the same name are different objects, so applying the new
// kind while the old one still runs briefly doubles the component — two Cores against one
// database, two schedulers publishing the same timed jobs. The prune reclaims the old object,
// but only AFTER the apply, which is exactly the wrong order.
//
// So: before the new kind is applied, delete the superseded one — with FOREGROUND propagation,
// so the object itself survives until its dependent pods are gone — and report withhold=true so
// the caller WITHHOLDS the new kind this pass. The next reconcile (triggered by the Owns watch,
// with workloadSwitchRequeueAfter as the backstop) finds the old object gone and applies the
// new kind against a component that is genuinely stopped.
//
// TWO THINGS HAPPEN BEFORE THE DELETE, in this order, and neither is optional:
//
//  1. The superseded object's key goes into the KEEP SET. pruneOrphans runs later in the SAME
//     reconcile and deletes every controller-owned child absent from that set. It lists through
//     the manager's CACHED client, so the object it sees can still carry no deletionTimestamp
//     however recently this delete was issued — and a second Delete re-evaluates the propagation
//     policy and can strip the foregroundDeletion finalizer the switch depends on. Keeping it in
//     the desired set removes the race without depending on read freshness at all.
//  2. The DURABLE marker is persisted. Once the object is gone, object presence can no longer
//     tell the migration trigger that a switch is under way — see markWorkloadKindSwitch.
//
// Two safety rails: an object without THIS Platform's controller owner reference is never
// touched, kept or marked (defense in depth, the same rule the prune applies), and an object
// already terminating is reported without re-issuing a delete.
func (r *Reconciler) stopSupersededWorkload(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, rendered client.Object) (bool, error) {
	old, ok := supersededWorkload(rendered)
	if !ok {
		return false, nil
	}
	key := types.NamespacedName{Name: rendered.GetName(), Namespace: rendered.GetNamespace()}
	if err := r.Get(ctx, key, old); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // the common case: no other-kind object exists
		}
		return false, fmt.Errorf("reading the superseded workload %q: %w", key.Name, err)
	}
	if !controllerOwnedBy(old, p.UID) {
		return false, nil // not ours — never delete, never keep, never block on it
	}

	desired.add(r, old) // (1) hold the prune off, for as long as the switch runs

	// (2) record the switch BEFORE the effect that makes it invisible
	if err := r.markWorkloadKindSwitch(ctx, p, workloadKindChange{
		Name: key.Name, Live: workloadKindOf(old), Rendered: workloadKindOf(rendered),
	}); err != nil {
		return false, fmt.Errorf("recording the workload-kind switch for %q: %w", key.Name, err)
	}

	if old.GetDeletionTimestamp() != nil {
		return true, nil // already stopping; wait for its pods to finish terminating
	}
	if err := r.Delete(ctx, old, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("stopping the superseded workload %q: %w", key.Name, err)
	}
	r.eventf(p, corev1.EventTypeNormal, reasonWorkloadKindSwitch,
		"stopping the superseded workload %q before applying its new workload kind", key.Name)
	return true, nil
}
