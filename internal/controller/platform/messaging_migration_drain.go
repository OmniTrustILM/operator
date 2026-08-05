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

// messaging_migration_drain.go is the DRAINING phase of the messaging-migration engine: with
// the platform's producers fenced, it polls the source virtual host's management API until
// every DRAINABLE queue reports empty, and only then lets the migration cut over.
//
// FAIL CLOSED, ALWAYS. Anything short of a complete, affirmative "these queues are empty" —
// an unreachable broker, credentials it cannot read, a listing that did not parse — resets the
// progress counter and waits. An error may never be read as "drained": the whole point of the
// phase is that what follows it DELETES the source topology.
//
// THREE QUEUE CLASSES, and the two that are not obvious:
//
//  1. WORK queues — the source bundle's own queues. Drained means both depths at zero: a
//     message that has been delivered but not acknowledged is still in flight, and a consumer
//     that dies takes it back to ready.
//
//  2. LATEST-ONLY RETENTION queues (bom.MessagingQueue.IsLatestOnlyRetention) — bounded to a
//     single message that their publisher keeps refreshing, so they are DESIGNED to sit
//     non-empty. Requiring them to empty would hang every migration forever, so they are
//     EXEMPT. The exemption is read off the bundle's declared queue arguments, never a name
//     list, so a future bundle's retention queue inherits it.
//
//  3. DYNAMIC per-proxy queues — created at runtime as remote proxies enrol, and therefore
//     discovered from the source proxy exchange's BINDINGS rather than guessed from a name
//     pattern. They require zero DEPTH only: a healthy remote proxy is itself a consumer of
//     its queue, so requiring zero consumers here would deadlock the drain against the very
//     clients the migration exists to keep serving. Consumers and open connections gate the
//     CLEANUP that deletes the source topology — not this phase.
//
// A SAMPLE, NOT A PROOF: the drain advances on migrationCleanDrainPolls CONSECUTIVE clean
// polls, and any error or any non-empty queue puts the counter back to zero. The repetition is
// what makes an instantaneously-empty queue distinguishable from a quiet one.
//
// SECURITY: the poll's inputs — the management endpoint, the virtual host, the administrator
// credentials — are read in memory and never written anywhere. Nothing this file puts on a
// condition, an Event or a log carries a broker coordinate, a queue name or a credential:
// progress is reported as counts.

import (
	"context"
	"errors"
	"fmt"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/internal/rabbitmq"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// migrationCleanDrainPolls is how many CONSECUTIVE polls must observe every drainable queue
// empty before the migration may cut over. One poll proves only that a queue was empty at one
// instant — between two fenced producers' last messages, say — so the drain asks for the same
// answer several times in a row, at migrationDrainPollInterval apart.
const migrationCleanDrainPolls = 3

// migrationDrainPollInterval is the MINIMUM time between two drain samples, enforced from a
// timestamp on status.upgrade rather than from the requeue the phase asks for.
//
// The requeue is not a floor. Every status write the drain makes — the clean-poll count above
// all — updates the Platform, which the operator's own watch turns into an immediate
// re-enqueue; that reconcile arrives in milliseconds and would take the next sample straight
// away. Three "consecutive" polls inside the same second say nothing at all about a virtual
// host that has stopped receiving traffic, which is the entire claim the barrier rests on. So
// the drain records when it may next look, and a pass arriving early simply waits.
const migrationDrainPollInterval = migrationRequeueAfter

// eventMigrationForcedCutover announces that the operator authorised this migration to proceed
// without a clean drain, accepting the loss of whatever the source virtual host still holds.
const eventMigrationForcedCutover = "MessagingMigrationForcedCutover"

// brokerAdminFactory builds a RabbitMQ management-API client for one broker: its management
// endpoint plus the administrator credentials to authenticate with. It exists so the drain
// takes its client from the reconciler rather than constructing one inline — tests inject a
// factory that returns a scripted BrokerAdmin and never opens a socket.
type brokerAdminFactory func(endpoint, username, password string) rabbitmq.BrokerAdmin

// newBrokerAdmin returns the factory this reconciler builds management-API clients with,
// falling back to the package's real HTTP client when none was injected.
func (r *Reconciler) newBrokerAdmin() brokerAdminFactory {
	if r.BrokerAdmins != nil {
		return r.BrokerAdmins
	}
	return func(endpoint, username, password string) rabbitmq.BrokerAdmin {
		return rabbitmq.NewClient(endpoint, username, password)
	}
}

// migrationDrainingPhase waits for the source virtual host to empty, and advances the
// migration once it has stayed empty across migrationCleanDrainPolls consecutive polls.
//
// The source re-pin happens FIRST, before anything is read: the management endpoint, the
// administrator credentials Secret and the virtual host are all resolved from the platform's
// version, and this phase must address the version the platform is moving AWAY from — not the
// one version resolution pinned in memory a few steps earlier.
func (r *Reconciler) migrationDrainingPhase(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	src, handled, res, err := r.holdOnSourceVersion(ctx, p, render)
	if handled || err != nil {
		return src, handled, res, err
	}

	if migrationPhaseDeadlineExceeded(p, time.Now()) {
		return r.migrationDeadlineExit(ctx, p, src)
	}

	// SPACING IS PART OF THE BARRIER. A pass that arrives before the recorded floor — which is
	// most of them, because every status write re-enqueues the Platform — takes no sample and
	// changes nothing, so the count can only ever be built from polls that are genuinely
	// spread across time.
	if !migrationDrainPollDue(p, time.Now()) {
		return src, false, ctrl.Result{}, nil
	}

	outstanding, perr := r.pollSourceDrain(ctx, p, src.bundle)
	if perr != nil {
		// FAIL CLOSED: a poll that did not complete says nothing about the virtual host, so it
		// counts as no progress at all. The phase's own deadline is what ends the wait if the
		// failure persists, and the drain heals itself the moment the broker answers again.
		log.FromContext(ctx).V(1).Info("messaging migration drain poll did not complete; the clean-poll count resets to zero",
			"phase", otilmv1alpha1.MigrationPhaseDraining, "cause", perr.Error())
		return r.recordDrainProgress(ctx, p, src, render, 0)
	}
	if outstanding > 0 {
		return r.recordDrainProgress(ctx, p, src, render, 0)
	}

	polls := p.Status.Upgrade.CleanDrainPolls + 1
	if polls < migrationCleanDrainPolls {
		return r.recordDrainProgress(ctx, p, src, render, polls)
	}

	if terr := r.transitionMigrationPhase(ctx, p, otilmv1alpha1.MigrationPhaseCuttingOver); terr != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, terr)
		return render, true, res, aerr
	}
	// The migration is past the point of no return now, so this pass renders nothing further:
	// the cutover's staged work belongs to the next one.
	return src, true, ctrl.Result{RequeueAfter: migrationRequeueAfter}, nil
}

// recordDrainProgress persists the consecutive-clean-poll count and hands the reconcile back
// the source render, so the platform goes on serving its source version while it waits.
func (r *Reconciler) recordDrainProgress(ctx context.Context, p *otilmv1alpha1.Platform, src, render migrationRender, polls int32) (migrationRender, bool, ctrl.Result, error) {
	if err := r.writeCleanDrainPolls(ctx, p, polls); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return render, true, res, aerr
	}
	return src, false, ctrl.Result{}, nil
}

// writeCleanDrainPolls records the sample the drain has just taken: how many consecutive clean
// polls it has now seen, and the earliest time it may take the next one.
//
// Both move in ONE write because both describe that sample. The floor in particular must be
// persisted even when the count does not change (the common case is a drain that is not yet
// clean, whose counter is already zero): the floor is the only thing that stops the very
// re-enqueue this write causes from taking another sample immediately. A failed write rolls
// both back, keeping status and the copy the rest of the pass reads in agreement.
func (r *Reconciler) writeCleanDrainPolls(ctx context.Context, p *otilmv1alpha1.Platform, polls int32) error {
	u := p.Status.Upgrade
	previousPolls, previousNext := u.CleanDrainPolls, u.NextDrainPollAt

	next := metav1.NewTime(time.Now().Add(migrationDrainPollInterval))
	u.CleanDrainPolls, u.NextDrainPollAt = polls, &next
	if err := r.writeMigrationState(ctx, p, metav1.ConditionTrue,
		string(otilmv1alpha1.MigrationPhaseDraining), migrationDrainMessage(u)); err != nil {
		u.CleanDrainPolls, u.NextDrainPollAt = previousPolls, previousNext
		return err
	}
	return nil
}

// migrationDrainPollDue reports whether the drain may take a sample now. An unrecorded floor
// (a drain that has not polled yet, or one resumed from a record written before the floor
// existed) is due immediately — the floor bounds the SPACING of samples, and there is nothing
// to space the first one from.
func migrationDrainPollDue(p *otilmv1alpha1.Platform, now time.Time) bool {
	u := p.Status.Upgrade
	if u == nil || u.NextDrainPollAt == nil || u.NextDrainPollAt.IsZero() {
		return true
	}
	return !now.Before(u.NextDrainPollAt.Time)
}

// migrationDrainMessage is the draining condition's text: the phase message plus how far the
// consecutive-clean-poll count has got. Counts only — no queue name and no coordinate.
func migrationDrainMessage(u *otilmv1alpha1.UpgradeStatus) string {
	return fmt.Sprintf("%s (%d of %d consecutive clean drain polls)",
		migrationPhaseMessage(u), u.CleanDrainPolls, migrationCleanDrainPolls)
}

// migrationForceCutoverAuthorized reports whether the platform authorises THIS ATTEMPT to
// finish without a clean drain, discarding whatever the source virtual host still holds.
//
// Target-scoping alone is not enough. The value is a plain version string, so one left behind
// by an attempt that was aborted or blocked would go on authorising every later migration to
// that same version — silently, and for the most destructive step the engine has. The
// authorisation is therefore bound to the ATTEMPT, in three parts:
//
//   - it must name the version this migration is cutting over TO;
//   - it must NOT be the value the field already carried when this attempt began
//     (status.upgrade.forceCarriedOver, cleared as soon as the field is cleared or changed —
//     so re-setting it deliberately for this attempt counts, and a leftover never does); and
//   - once an attempt has ACTED on it (status.upgrade.forceAuthorized), it stays authorised
//     for the rest of that attempt, so the cleanup honours a force the cutover already used
//     even if the field is cleared in between.
func migrationForceCutoverAuthorized(p *otilmv1alpha1.Platform) bool {
	u := p.Status.Upgrade
	if u == nil {
		return false
	}
	if u.ForceAuthorized {
		return true
	}
	if u.ForceCarriedOver {
		return false
	}
	m := p.Spec.Messaging.Managed
	return m != nil && m.ForceCutoverForVersion != "" && m.ForceCutoverForVersion == u.ToVersion
}

// migrationForceCarriedOver reports whether spec.messaging.managed.forceCutoverForVersion
// currently names the given target version — the check beginMigration makes to decide whether
// the value predates the attempt it is about to start.
func migrationForceCarriedOver(p *otilmv1alpha1.Platform, toVersion string) bool {
	m := p.Spec.Messaging.Managed
	return m != nil && m.ForceCutoverForVersion != "" && m.ForceCutoverForVersion == toVersion
}

// refreshForceAuthorization clears a carried-over marker once the spec value it refers to is
// gone. That is what makes the authorisation re-armable: the operator clears
// forceCutoverForVersion (or points it elsewhere), this pass records that the leftover is no
// longer in play, and setting it again on a later pass is a fresh, deliberate authorisation
// for THIS attempt.
//
// It writes only on the transition, so the steady state costs nothing.
func (r *Reconciler) refreshForceAuthorization(ctx context.Context, p *otilmv1alpha1.Platform) error {
	u := p.Status.Upgrade
	if u == nil || !u.ForceCarriedOver || migrationForceCarriedOver(p, u.ToVersion) {
		return nil
	}
	u.ForceCarriedOver = false
	if err := r.writeStatus(ctx, p); err != nil {
		u.ForceCarriedOver = true
		return err
	}
	return nil
}

// consumeMigrationForce records that this attempt has ACTED on its force authorisation, and
// persists that BEFORE the step it permits — the engine's ordinary rule that nothing
// destructive happens against a state the cluster has not accepted. Recording it is also what
// lets the cleanup finish a forced migration the cutover started, whatever the spec says by
// then.
func (r *Reconciler) consumeMigrationForce(ctx context.Context, p *otilmv1alpha1.Platform) error {
	u := p.Status.Upgrade
	if u == nil || u.ForceAuthorized {
		return nil
	}
	u.ForceAuthorized = true
	if err := r.writeStatus(ctx, p); err != nil {
		u.ForceAuthorized = false
		return err
	}
	return nil
}

// forceMigrationCutover takes the exit an expired reversible phase's own message offers: cut over
// anyway, accepting the loss of whatever the source virtual host still holds. Either reversible
// phase can reach it — a fence whose producers never wound down and a drain that never emptied
// are the same situation from the operator's side, and the message they produce is the same one.
//
// It re-asserts the fence first. The deadline may already have been reached once and blocked,
// which LIFTS the fence — so the producers can be back up by the time the authorisation
// arrives, and cutting over with them running would publish onto a virtual host that is about
// to be reclaimed. fenceWorkloads is idempotent either way: it re-records the live counts when
// nothing is fenced, and merely re-asserts the zeros when the fence still holds.
func (r *Reconciler) forceMigrationCutover(ctx context.Context, p *otilmv1alpha1.Platform, src migrationRender) (migrationRender, bool, ctrl.Result, error) {
	// Record the authorisation as consumed FIRST: from here the migration proceeds on it, and
	// the cleanup that finishes the job must honour the same decision even if the spec field
	// is cleared before it gets there.
	if err := r.consumeMigrationForce(ctx, p); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return src, true, res, aerr
	}

	if err := r.fenceWorkloads(ctx, p); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, err)
		return src, true, res, aerr
	}

	if err := r.transitionMigrationPhase(ctx, p, otilmv1alpha1.MigrationPhaseCuttingOver); err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return src, true, res, aerr
	}
	r.eventf(p, corev1.EventTypeWarning, eventMigrationForcedCutover,
		"spec.messaging.managed.forceCutoverForVersion authorises the messaging migration to platform version %s to cut over "+
			"without a clean drain; whatever has not drained will be discarded", p.Status.Upgrade.ToVersion)
	return src, true, ctrl.Result{RequeueAfter: migrationRequeueAfter}, nil
}

// pollSourceDrain asks the broker how many DRAINABLE queues on the migration's source virtual
// host still hold messages. Zero means this poll was clean; an error means the poll answered
// nothing at all, which is never the same thing.
func (r *Reconciler) pollSourceDrain(ctx context.Context, p *otilmv1alpha1.Platform, source bom.Bundle) (int, error) {
	admin, err := r.sourceBrokerAdmin(ctx, p)
	if err != nil {
		return 0, err
	}
	// The virtual host is derived from the SOURCE bundle rather than from the resolved
	// connection, so the poll addresses the right one no matter what has been pinned in memory.
	vhost := platformbuilder.ManagedVirtualHostFor(p, source)

	states, err := admin.Queues(ctx, vhost)
	if err != nil {
		return 0, fmt.Errorf("listing the source virtual host's queues: %w", err)
	}
	bound, err := boundProxyQueues(ctx, admin, source, vhost)
	if err != nil {
		return 0, err
	}
	return outstandingQueues(migrationDrainableQueues(source, bound), states), nil
}

// sourceBrokerAdmin builds the management-API client the drain polls with: the managed
// broker's management endpoint, authenticated as the topology's administrator user.
//
// SECURITY: the credentials are read into memory for the length of the call and go nowhere
// else — not into a rendered object, not into status, not into an Event, not into a log.
func (r *Reconciler) sourceBrokerAdmin(ctx context.Context, p *otilmv1alpha1.Platform) (rabbitmq.BrokerAdmin, error) {
	if !platformbuilder.MessagingManaged(p) {
		return nil, errors.New("the drain polls an operator-managed broker only; this platform's messaging is external")
	}
	endpoint := platformbuilder.ManagedMessagingManagementEndpoint(p)
	mq := platformbuilder.ResolveMessagingConnection(p)
	if endpoint == "" || mq.AdministratorCredentialsSecretName == "" {
		return nil, errors.New("the managed broker has no administrator user to poll its management API as")
	}

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: mq.AdministratorCredentialsSecretName}, &secret); err != nil {
		return nil, fmt.Errorf("reading the managed broker's administrator credentials Secret %q: %w",
			mq.AdministratorCredentialsSecretName, err)
	}
	username := secret.Data[mq.UsernameKey]
	secretValue := secret.Data[mq.PasswordKey]
	if len(username) == 0 || len(secretValue) == 0 {
		return nil, fmt.Errorf("the managed broker's administrator credentials Secret %q is incomplete",
			mq.AdministratorCredentialsSecretName)
	}
	return r.newBrokerAdmin()(endpoint, string(username), string(secretValue)), nil
}

// boundProxyQueues returns the queues currently bound to the source topology's PROXY
// exchanges — the per-proxy queues remote proxies create as they enrol, which no bundle can
// list because they do not exist until a proxy asks for one.
//
// The exchange is identified by TYPE (the topic exchange is the proxy one) rather than by
// name, so a bundle that renames it — as 2.19.0 did — is followed without a code change.
func boundProxyQueues(ctx context.Context, admin rabbitmq.BrokerAdmin, source bom.Bundle, vhost string) ([]string, error) {
	var bound []string
	for _, e := range source.Messaging.Exchanges {
		if e.Type != bom.ExchangeTypeTopic {
			continue
		}
		queues, err := admin.BoundQueues(ctx, vhost, e.Name)
		if err != nil {
			return nil, fmt.Errorf("listing the queues bound to the source proxy exchange: %w", err)
		}
		bound = append(bound, queues...)
	}
	return bound, nil
}

// migrationDrainableQueues returns the names of the queues that must be empty before the
// source virtual host counts as drained: the source bundle's own queues MINUS the latest-only
// retention queues that are designed never to empty, plus every dynamically-bound proxy queue
// the bundle does not already list.
func migrationDrainableQueues(source bom.Bundle, bound []string) []string {
	seen := make(map[string]struct{}, len(source.Messaging.Queues)+len(bound))
	drainable := make([]string, 0, len(source.Messaging.Queues)+len(bound))

	for _, q := range source.Messaging.Queues {
		seen[q.Name] = struct{}{}
		if q.IsLatestOnlyRetention() {
			continue
		}
		drainable = append(drainable, q.Name)
	}
	for _, name := range bound {
		if _, known := seen[name]; known {
			continue
		}
		seen[name] = struct{}{}
		drainable = append(drainable, name)
	}
	return drainable
}

// outstandingQueues counts how many of the drainable queues the broker still holds messages
// for. A message that has been delivered but not acknowledged counts: its consumer may die and
// hand it straight back.
//
// CONSUMERS ARE NOT CONSULTED. A remote proxy attached to its own queue is a healthy client of
// a platform that is still serving, and the migration must not wait for it to disconnect —
// that requirement belongs to the cleanup that deletes the source topology. A queue the broker
// does not list cannot be holding anything, so it does not hold the drain up either.
func outstandingQueues(drainable []string, states []rabbitmq.QueueState) int {
	depth := make(map[string]int64, len(states))
	for _, s := range states {
		depth[s.Name] = s.MessagesReady + s.MessagesUnacked
	}
	outstanding := 0
	for _, name := range drainable {
		if depth[name] > 0 {
			outstanding++
		}
	}
	return outstanding
}
