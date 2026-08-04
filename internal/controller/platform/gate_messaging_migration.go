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

// gate_messaging_migration.go is the PHASE MACHINE of the messaging-migration engine: it
// carries out the decisions messaging_migration_trigger.go makes, persists where the
// migration has got to, and resumes there after a restart. The trigger layer decides; this
// file executes and remembers.
//
// PLACEMENT. The gate runs immediately after version resolution and BEFORE
// gateManagedDependencies, so nothing belonging to the target version — no topology object, no
// workload image — is applied until the engine allows it.
//
// THE RE-PIN. resolvePlatformVersion has already mutated the in-memory spec.version to the
// TARGET, and every builder resolves its bundle off that field. While the migration is still
// on the source virtual host (Fencing and Draining) the gate therefore re-pins that
// already-mutated copy BACK to the migration's source version and hands the rest of the
// reconcile the source bundle. Without it the very next apply would render the target
// topology beside a source vhost that has not drained — exactly what the migration exists to
// sequence. The pin is in-memory only, as resolvePlatformVersion's is: no spec is ever
// persisted.
//
// THE CADENCE. Every phase that WAITS re-checks on migrationRequeueAfter. The gate must
// supply that requeue itself: it runs ahead of the messaging gate, so on a pass it
// short-circuits no later gate's requeue is reached, and on a pass it lets through the
// migration's own cadence is the only one that reflects how fast the engine needs to look
// again.
//
// SECURITY: every condition message and Event produced here carries version strings, phase
// names, workload names and spec field paths ONLY — never a virtual host, a hostname, a
// broker coordinate or a credential.

import (
	"context"
	"fmt"
	"slices"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// migrationRequeueAfter is how long the reconciler waits before looking at an in-flight
// messaging migration again. Every WAITING phase returns it, so the engine's own progress —
// producers winding down, the source virtual host emptying — is re-checked on a cadence the
// migration owns rather than on whatever another gate happened to ask for.
const migrationRequeueAfter = 15 * time.Second

// defaultMigrationDrainTimeout bounds a reversible migration phase when the platform pins no
// spec.messaging.managed.drainTimeout. The CRD defaults that field, so this fallback matters
// only for a Platform built in memory (a unit test, or an object created before the field
// existed) — it mirrors the CRD default rather than inventing a second policy.
const defaultMigrationDrainTimeout = 15 * time.Minute

// conditionMessagingMigration reports an in-flight messaging migration. True means one is
// running and progressing, with the phase as the reason; False means it has stopped — aborted
// by the user, refused by the trigger layer, or held at a deadline it did not meet.
//
// Like MessagingReady / DatabaseReady it is an ADJUNCT: a migration that stops does not by
// itself make the platform Degraded, because the platform goes on running its source version
// at full strength. The refusals are the exception — those come from a spec the operator
// cannot act on, so they degrade.
const conditionMessagingMigration = "MessagingMigration"

// Condition reasons the phase machine produces (the phase names double as the progressing
// reasons, so the vocabulary is not duplicated). They are identity-free labels.
const (
	// reasonMigrationAborted: the user reverted spec.version while the migration was still
	// reversible, so the fence was lifted and the recorded state discarded.
	reasonMigrationAborted = "MigrationAborted"
	// reasonMigrationDrainTimeout: a reversible phase outlived
	// spec.messaging.managed.drainTimeout. The fence is lifted and the platform holds on its
	// source version; the migration record survives so it cannot silently restart.
	reasonMigrationDrainTimeout = "DrainTimeout"
	// reasonMigrationStateError: the migration state could not be written, or names a version
	// this operator build does not carry. The reconcile stops rather than act blindly.
	reasonMigrationStateError = "MigrationStateError"
	// reasonMigrationWorkloadKindChanged: a fenced component's workloadType was changed while
	// the migration holds it at zero replicas. Refused — see migrationWorkloadKindFlip.
	reasonMigrationWorkloadKindChanged = "MigrationWorkloadKindChanged"
)

// Event reasons for the migration's lifecycle transitions.
const (
	eventMigrationStarted = "MessagingMigrationStarted"
	eventMigrationPhase   = "MessagingMigrationPhase"
	eventMigrationAborted = "MessagingMigrationAborted"
	eventMigrationBlocked = "MessagingMigrationBlocked"
)

// migrationRender is what the migration gate leaves the rest of the reconcile to render and
// report: the version whose bundle every builder must resolve against, that bundle, and
// whether the engine needs another look soon.
//
// With no migration involved it is simply the version resolution's own answer. While a
// migration holds the platform on its source version it is the SOURCE bundle — which is also
// what finalizeReconcile records on status.observedVersion, so the pinned running version
// never claims a target the platform has not reached.
type migrationRender struct {
	// version is the platform version the reconcile renders and reports.
	version string
	// bundle is that version's bundle.
	bundle bom.Bundle
	// requeue asks for the migration cadence at the end of the pass.
	requeue bool
}

// gateMessagingMigration decides and carries out this reconcile's messaging-migration work.
//
// It is called with the TARGET bundle and version the version resolution produced, and
// returns the bundle and version the rest of the reconcile must actually use — the same ones
// when no migration is in the way, the SOURCE ones while a migration holds the platform back.
// handled=true means the gate short-circuited the reconcile (a refusal, a failure, or a phase
// that must not render at all).
func (r *Reconciler) gateMessagingMigration(ctx context.Context, p *otilmv1alpha1.Platform, target bom.Bundle, targetVersion string) (migrationRender, bool, ctrl.Result, error) {
	render := migrationRender{version: targetVersion, bundle: target}

	// The SOURCE bundle is read only to decide whether a move needs a migration at all; an
	// in-flight one is decided entirely from status.upgrade. So a running version this
	// operator build no longer carries simply cannot START a migration — it does not silence
	// one that is already recorded.
	source, sourceKnown := bom.BundleFor(p.Status.ObservedVersion)
	if !sourceKnown && !migrationInFlight(p) {
		return render, false, ctrl.Result{}, nil
	}

	switch decision := decideMigration(p, source, target); decision.Action {
	case migrationActionNone:
		return render, false, ctrl.Result{}, nil

	case migrationActionRefuse:
		res, err := r.refuseMigration(ctx, p, decision)
		return render, true, res, err

	case migrationActionAbort:
		if err := r.abortMigration(ctx, p); err != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
			return render, true, res, aerr
		}
		// The abort restored exactly the prior state, and spec.version already names the
		// version the platform is running: the reconcile proceeds down the ordinary path.
		return render, false, ctrl.Result{}, nil

	case migrationActionStart:
		if err := r.beginMigration(ctx, p, targetVersion); err != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
			return render, true, res, aerr
		}
		return r.advanceMigration(ctx, p, render)

	default: // migrationActionResume
		return r.advanceMigration(ctx, p, render)
	}
}

// advanceMigration runs the recorded phase of a live migration.
//
// The workload-kind guard runs first, for EVERY phase: it protects a fence that is already
// holding workloads down, and the fence outlives the phase that raised it.
func (r *Reconciler) advanceMigration(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if name, recorded, requested, flipped := migrationWorkloadKindFlip(p); flipped {
		message := fmt.Sprintf(
			"workload %q is rendered as a %s but the messaging migration to platform version %s holds it fenced as a %s; "+
				"a workload of the other kind would start outside the fence and publish while the platform drains — "+
				"restore its workloadType until the migration finishes, or revert spec.version to %s to abort it",
			name, requested, p.Status.Upgrade.ToVersion, recorded, p.Status.Upgrade.FromVersion)
		setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationWorkloadKindChanged, message)
		res, err := r.steadyState(ctx, p, reasonMigrationWorkloadKindChanged, message)
		return render, true, res, err
	}

	switch p.Status.Upgrade.Phase {
	case otilmv1alpha1.MigrationPhaseFencing:
		return r.migrationFencingPhase(ctx, p, render)

	case otilmv1alpha1.MigrationPhaseDraining:
		return r.migrationDrainingPhase(ctx, p, render)

	default:
		// CuttingOver and CleaningUp are past the point of no return, so neither the deadline
		// nor the source re-pin applies to them. Their broker-side work — repointing the
		// platform at the target topology and reclaiming the source one — lands with the
		// later steps of the engine; until then the phase simply holds its recorded state and
		// re-checks, which is what keeps the persisted state and the resume path honest.
		return render, true, ctrl.Result{RequeueAfter: migrationRequeueAfter}, nil
	}
}

// migrationFencingPhase stops the platform's message producers and, once they are actually
// down, hands over to the drain.
//
// fenceWorkloads is idempotent and re-enterable: it records the complete set before the first
// patch and never re-records afterwards, so this runs unchanged whether it is the first pass
// or the first pass after a crash.
func (r *Reconciler) migrationFencingPhase(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if migrationPhaseDeadlineExceeded(p, time.Now()) {
		return r.blockMigration(ctx, p, render)
	}

	if err := r.fenceWorkloads(ctx, p); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, err)
		return render, true, res, aerr
	}

	stopped, err := r.fencedProducersStopped(ctx, p)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, err)
		return render, true, res, aerr
	}
	if stopped {
		if terr := r.transitionMigrationPhase(ctx, p, otilmv1alpha1.MigrationPhaseDraining); terr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, terr)
			return render, true, res, aerr
		}
	}

	return r.holdOnSourceVersion(ctx, p, render)
}

// migrationDrainingPhase waits for the source virtual host to empty.
//
// The poll itself — asking the broker's management API whether every drainable queue has
// gone quiet — is the broker-side step that lands with the rest of the engine. Until then the
// phase only HOLDS: the producers stay fenced, the platform keeps rendering and serving its
// source version, and the deadline below is what ends the wait.
func (r *Reconciler) migrationDrainingPhase(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if migrationPhaseDeadlineExceeded(p, time.Now()) {
		return r.blockMigration(ctx, p, render)
	}
	return r.holdOnSourceVersion(ctx, p, render)
}

// holdOnSourceVersion re-pins the in-memory spec.version back to the migration's source
// version and lets the reconcile CONTINUE against the source bundle.
//
// Continuing is the point: while the migration waits, the platform is a fully functioning
// deployment of its source version and must keep converging as one — its children reconciled,
// its readiness measured, its fence re-asserted behind every apply. What it must NOT do is
// render anything belonging to the target, which is precisely what the re-pin prevents.
func (r *Reconciler) holdOnSourceVersion(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	src, err := migrationSourceRender(p)
	if err != nil {
		res, derr := r.degraded(ctx, p, reasonMigrationStateError, err)
		return render, true, res, derr
	}
	src.requeue = true
	return src, false, ctrl.Result{}, nil
}

// migrationSourceRender re-pins the ALREADY-MUTATED in-memory spec.version back to the
// version the migration is moving away from, and returns that version's bundle.
//
// resolvePlatformVersion pins the TARGET onto spec.version so the builders resolve the bundle
// the reconciler gated on; for the phases that still live on the source virtual host that pin
// is the wrong one, and undoing it here is the single point where the target stops being
// rendered. In-memory only, exactly like the pin it overrides.
func migrationSourceRender(p *otilmv1alpha1.Platform) (migrationRender, error) {
	from := p.Status.Upgrade.FromVersion
	bundle, ok := bom.BundleFor(from)
	if from == "" || !ok {
		return migrationRender{}, fmt.Errorf(
			"the in-flight messaging migration is moving away from platform version %q, which this operator build does not carry", from)
	}
	p.Spec.Version = from
	return migrationRender{version: from, bundle: bundle}, nil
}

// refuseMigration reports a move the trigger layer will not make. It is a DETERMINISTIC,
// user-correctable state — the requested version is one the engine cannot get to from here —
// so it degrades and stops, terminal until a spec edit re-enqueues the Platform.
func (r *Reconciler) refuseMigration(ctx context.Context, p *otilmv1alpha1.Platform, d migrationDecision) (ctrl.Result, error) {
	setMigrationCondition(p, metav1.ConditionFalse, d.Reason, d.Message)
	return r.steadyState(ctx, p, d.Reason, d.Message)
}

// beginMigration records a new migration and persists it BEFORE anything is fenced.
//
// On a failed write the in-memory record is dropped again, so the reconcile that reports the
// failure cannot leave a phase behind that the cluster never agreed to.
func (r *Reconciler) beginMigration(ctx context.Context, p *otilmv1alpha1.Platform, toVersion string) error {
	now := metav1.Now()
	p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
		FromVersion:    p.Status.ObservedVersion,
		ToVersion:      toVersion,
		Phase:          otilmv1alpha1.MigrationPhaseFencing,
		StartedAt:      now,
		PhaseStartedAt: now,
	}
	if err := r.writeMigrationState(ctx, p, metav1.ConditionTrue,
		string(otilmv1alpha1.MigrationPhaseFencing), migrationPhaseMessage(p.Status.Upgrade)); err != nil {
		p.Status.Upgrade = nil
		return err
	}
	r.eventf(p, corev1.EventTypeNormal, eventMigrationStarted,
		"messaging migration from platform version %s to %s started", p.Status.Upgrade.FromVersion, toVersion)
	return nil
}

// transitionMigrationPhase moves the migration to its next phase and persists that BEFORE the
// phase's own work begins. A failed write rolls the in-memory phase back, so nothing acts on
// a stage the cluster has not recorded.
func (r *Reconciler) transitionMigrationPhase(ctx context.Context, p *otilmv1alpha1.Platform, next otilmv1alpha1.MigrationPhase) error {
	u := p.Status.Upgrade
	previous, previousStart, previousPolls := u.Phase, u.PhaseStartedAt, u.CleanDrainPolls

	u.Phase = next
	u.PhaseStartedAt = metav1.Now()
	// Each phase's deadline and its progress counter are the previous phase's, not the new
	// one's: a phase begins with a full budget and nothing counted.
	u.CleanDrainPolls = 0

	if err := r.writeMigrationState(ctx, p, metav1.ConditionTrue, string(next), migrationPhaseMessage(u)); err != nil {
		u.Phase, u.PhaseStartedAt, u.CleanDrainPolls = previous, previousStart, previousPolls
		return err
	}
	r.eventf(p, corev1.EventTypeNormal, eventMigrationPhase,
		"messaging migration to platform version %s entered phase %s (from %s)", u.ToVersion, next, previous)
	return nil
}

// abortMigration unwinds a migration the user reverted while it was still reversible: the
// fence is lifted workload by workload (each restore persisted as it completes) and only then
// is the record discarded.
//
// That order is deliberate. While the record survives, a crash mid-abort resumes as another
// abort and finishes the job; discarding it first would leave producers at zero replicas with
// nothing left to say they should come back up.
func (r *Reconciler) abortMigration(ctx context.Context, p *otilmv1alpha1.Platform) error {
	if err := r.liftMigrationFence(ctx, p); err != nil {
		return err
	}

	recorded := p.Status.Upgrade
	message := fmt.Sprintf("the messaging migration to platform version %s was aborted at phase %s; the platform keeps running version %s",
		recorded.ToVersion, recorded.Phase, recorded.FromVersion)

	p.Status.Upgrade = nil
	if err := r.writeMigrationState(ctx, p, metav1.ConditionFalse, reasonMigrationAborted, message); err != nil {
		p.Status.Upgrade = recorded
		return err
	}
	r.event(p, corev1.EventTypeNormal, eventMigrationAborted, message)
	return nil
}

// blockMigration ends the WAIT — not the migration — when a reversible phase outlives its
// deadline. The fence is lifted so the platform runs its source version at full strength, and
// the reason is recorded.
//
// The migration RECORD deliberately survives. Clearing it would hand the platform straight
// back to the trigger, which would see the same unchanged spec, start the same migration and
// fence the same producers — a loop the user could not break. Keeping it recorded makes the
// exits explicit and the user's: revert spec.version (which aborts), or authorise the forced
// cutover. The platform is not Degraded here: it is serving its source version normally.
func (r *Reconciler) blockMigration(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	if !migrationBlockedFor(p, reasonMigrationDrainTimeout) {
		if err := r.liftMigrationFence(ctx, p); err != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, err)
			return render, true, res, aerr
		}

		u := p.Status.Upgrade
		message := fmt.Sprintf(
			"the messaging migration to platform version %s did not complete phase %s within %s; the fence has been lifted and the platform "+
				"keeps running version %s — revert spec.version to %s to abort the migration, or set "+
				"spec.messaging.managed.forceCutoverForVersion to %q to proceed and discard whatever has not drained",
			u.ToVersion, u.Phase, migrationDrainTimeout(p), u.FromVersion, u.FromVersion, u.ToVersion)

		if err := r.writeMigrationState(ctx, p, metav1.ConditionFalse, reasonMigrationDrainTimeout, message); err != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
			return render, true, res, aerr
		}
		r.event(p, corev1.EventTypeWarning, eventMigrationBlocked, message)
	}

	// Still the source version — the platform lives there until the user chooses an exit —
	// but with no migration cadence: nothing will change until they do.
	src, err := migrationSourceRender(p)
	if err != nil {
		res, derr := r.degraded(ctx, p, reasonMigrationStateError, err)
		return render, true, res, derr
	}
	return src, false, ctrl.Result{}, nil
}

// liftMigrationFence restores every workload the fence is still holding down, at the count it
// carried before. Each restore persists itself, and an entry already gone is a no-op, so the
// whole operation may be re-entered freely after a crash.
func (r *Reconciler) liftMigrationFence(ctx context.Context, p *otilmv1alpha1.Platform) error {
	if p.Status.Upgrade == nil {
		return nil
	}
	// Iterate a COPY: restoreWorkload removes each entry from the list it is given.
	for _, w := range slices.Clone(p.Status.Upgrade.Fenced) {
		if err := r.restoreWorkload(ctx, p, w); err != nil {
			return err
		}
	}
	return nil
}

// writeMigrationState persists status.upgrade together with the migration condition, and is a
// DELIBERATE DEVIATION from this package's "measure the world, then write status once in
// finalizeReconcile" pattern.
//
// Everywhere else that ordering is right: status is a MEASUREMENT taken after the pass's side
// effects, and a crash before the write loses only a measurement the next reconcile re-takes.
// The migration's state is not a measurement — it is the only record of side effects the next
// reconcile cannot re-derive. Fence the producers, crash before finalizeReconcile, and the
// restarted operator reads a Platform with no migration recorded: it re-applies the bundle's
// replica counts, silently un-fences the producers, and the migration is gone with messages
// already flowing back onto the source virtual host.
//
// So every phase transition is persisted BEFORE the side effect it authorises, and a failed
// write ABORTS the reconcile rather than proceeding: an effect must never run against a state
// the cluster does not know about. The opposite order is safe — a persisted phase whose effect
// has not run yet is simply performed on the next pass, because every phase's effect is
// idempotent.
func (r *Reconciler) writeMigrationState(ctx context.Context, p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) error {
	setMigrationCondition(p, status, reason, message)
	return r.Status().Update(ctx, p)
}

// setMigrationCondition records the migration's state on the Platform's conditions, following
// the package's convention of stamping observedGeneration on every condition it sets.
func setMigrationCondition(p *otilmv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: conditionMessagingMigration, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation, // versions/phases/field paths only — never a coordinate
	})
}

// migrationBlockedFor reports whether the migration is ALREADY recorded as stopped for this
// reason, so re-entering a terminal path on every reconcile repeats neither the Event nor the
// status write.
func migrationBlockedFor(p *otilmv1alpha1.Platform, reason string) bool {
	c := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reason
}

// migrationPhaseMessage is the progressing condition's text: the two versions and the stage,
// and nothing else.
func migrationPhaseMessage(u *otilmv1alpha1.UpgradeStatus) string {
	return fmt.Sprintf("messaging migration from platform version %s to %s is in phase %s",
		u.FromVersion, u.ToVersion, u.Phase)
}

// migrationDrainTimeout is how long one reversible phase may take, from the platform's own
// spec.messaging.managed.drainTimeout or the CRD's default when it pins none.
func migrationDrainTimeout(p *otilmv1alpha1.Platform) time.Duration {
	if m := p.Spec.Messaging.Managed; m != nil && m.DrainTimeout != nil && m.DrainTimeout.Duration > 0 {
		return m.DrainTimeout.Duration
	}
	return defaultMigrationDrainTimeout
}

// migrationPhaseDeadlineExceeded reports whether the CURRENT phase has outlived its budget,
// measured from the phase's own start rather than the migration's: a phase that took a long
// time to reach still gets its full window.
//
// It applies to the reversible phases only. Past the cutover there is no state to go back to,
// so a deadline could not do anything useful with the answer.
func migrationPhaseDeadlineExceeded(p *otilmv1alpha1.Platform, now time.Time) bool {
	u := p.Status.Upgrade
	if u == nil || u.PhaseStartedAt.IsZero() {
		return false
	}
	if u.Phase != otilmv1alpha1.MigrationPhaseFencing && u.Phase != otilmv1alpha1.MigrationPhaseDraining {
		return false
	}
	return now.Sub(u.PhaseStartedAt.Time) > migrationDrainTimeout(p)
}

// migrationWorkloadKindFlip reports a fenced component whose EFFECTIVE workload kind no longer
// matches the kind the fence recorded for it — i.e. spec.<component>.workloadType was changed
// while the migration holds that component at zero replicas.
//
// The fence addresses the kind it recorded, so it would go on holding an object the render no
// longer produces while the newly-rendered workload of the other kind came up at the
// apiserver's default of one replica: a producer publishing to the very virtual host the
// migration is draining, invisible to the fence. The engine refuses the flip rather than
// chasing it, so the fence's record and the cluster can never disagree about what is stopped.
func migrationWorkloadKindFlip(p *otilmv1alpha1.Platform) (name, recorded, requested string, flipped bool) {
	if p.Status.Upgrade == nil {
		return "", "", "", false
	}
	kinds := make(map[string]string, len(platformbuilder.MigrationFenceTargets(p)))
	for _, t := range platformbuilder.MigrationFenceTargets(p) {
		kinds[t.Name] = t.Kind
	}
	for _, w := range p.Status.Upgrade.Fenced {
		if k, targeted := kinds[w.Name]; targeted && k != w.Kind {
			return w.Name, w.Kind, k, true
		}
	}
	return "", "", "", false
}

// fencedProducersStopped reports whether every fenced workload has actually wound down, read
// from each one's OBSERVED replica count rather than the zero the fence wrote.
//
// The distinction is the whole point of the Fencing phase: patching .spec.replicas to zero is
// instantaneous, but a producer keeps publishing until its last pod is gone. Advancing to the
// drain on the patch alone would start counting an "empty" queue while messages were still
// arriving. A workload that no longer exists has no producer left and is stopped by
// definition.
func (r *Reconciler) fencedProducersStopped(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	if p.Status.Upgrade == nil {
		return false, nil
	}
	for _, w := range p.Status.Upgrade.Fenced {
		obj, err := workloadObject(w.Kind)
		if err != nil {
			return false, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: w.Name, Namespace: p.Namespace}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("reading %s %q to check the fence has taken effect: %w", w.Kind, w.Name, err)
		}
		if workloadObservedReplicas(obj) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// workloadObservedReplicas reads .status.replicas off a Deployment or StatefulSet — the pods
// the workload controller currently has, which only reaches zero once the last one is gone.
func workloadObservedReplicas(obj client.Object) int32 {
	switch w := obj.(type) {
	case *appsv1.Deployment:
		return w.Status.Replicas
	case *appsv1.StatefulSet:
		return w.Status.Replicas
	}
	return 0
}
