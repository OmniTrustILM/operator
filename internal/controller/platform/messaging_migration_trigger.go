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

// messaging_migration_trigger.go is the DECISION layer of the messaging-migration engine:
// given a Platform plus the source and target version bundles, it concludes whether this
// reconcile must start a migration, resume one, abort one, or refuse the requested move — and
// with what reason. It performs NO I/O and holds no state: every function here is a pure
// function of its arguments, so the entire semantic matrix is table-testable. Carrying a
// decision out is the gate's job, not this file's.
//
// THE TRIGGER is a change of the EFFECTIVE messaging vhost — each bundle's per-version default
// overridden by spec.messaging.virtualHost when the platform pins one — between the running
// version and the requested one. Equal effective vhosts mean the ordinary additive apply path:
// the topology objects keep their identities and a plain apply converges them. A CHANGED vhost
// means the source and target topologies are disjoint objects that must coexist while traffic
// moves from one to the other, which is what the migration engine exists to do.
//
// SECURITY: every Reason and Message produced here becomes a status condition and an Event.
// They carry version strings, phase names and spec field paths ONLY — never a vhost name, a
// hostname, a broker coordinate, or a credential.

import (
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// migrationAction is the operation the trigger layer concludes for one reconcile.
type migrationAction string

const (
	// migrationActionNone means no migration is involved: the reconcile proceeds down the
	// ordinary additive apply path.
	migrationActionNone migrationAction = "none"
	// migrationActionStart means a migration must begin at the first phase.
	migrationActionStart migrationAction = "start"
	// migrationActionResume means a recorded migration continues at its recorded phase.
	migrationActionResume migrationAction = "resume"
	// migrationActionRefuse means the requested move is not supported: the platform holds at
	// the running version and surfaces migrationDecision.Reason/Message.
	migrationActionRefuse migrationAction = "refuse"
	// migrationActionAbort means an in-flight migration must be unwound (the fence restored,
	// the recorded state cleared) because the user reverted to the version it started from
	// while that was still reversible.
	migrationActionAbort migrationAction = "abort"
	// migrationActionVerifyPin means the requested move WOULD rename the managed virtual host,
	// and the only thing suppressing that rename is spec.messaging.virtualHost — a pin that
	// resolves identically under both bundles. Whether that is safe depends on a fact this
	// layer cannot see: whether the platform's LIVE topology is already on the pinned vhost.
	// A pin that predates the move means it is (no migration, the documented path); a pin made
	// in the SAME update as the move means it is not, and skipping the migration would strand
	// the source topology. So the decision is handed to the gate, which asks the cluster — see
	// Reconciler.guardMigrationVirtualHostPin.
	migrationActionVerifyPin migrationAction = "verifyPin"
)

// Condition/Event reasons the trigger layer produces. They are identity-free labels.
const (
	// reasonExternalMessagingMigrationRequired refuses a topology-renaming upgrade of a
	// platform on an EXTERNAL broker: the operator does not own that broker and cannot
	// migrate it, so the move waits on the user's own migration plus the acknowledgement.
	reasonExternalMessagingMigrationRequired = "ExternalMessagingMigrationRequired"
	// reasonSteppingStoneRequired refuses a migration whose SOURCE version predates the
	// messaging topology the engine supports migrating from.
	reasonSteppingStoneRequired = "SteppingStoneRequired"
	// reasonMigrationInProgress refuses a version OTHER than the two an in-flight migration
	// is moving between.
	reasonMigrationInProgress = "MigrationInProgress"
	// reasonMigrationForwardOnly refuses a revert to the source version once the migration
	// has cut traffic over: past that point only completing it is safe.
	reasonMigrationForwardOnly = "MigrationForwardOnly"
)

// migrationSteppingStoneVersion is the platform version an older platform must pass THROUGH
// before a messaging migration is supported from it. It is named in the refusal's remedy only
// — the refusal itself is DETECTED from the source bundle's topology data, never from a
// version literal.
const migrationSteppingStoneVersion = "2.18.0"

// migrationDecision is what the trigger layer concludes for one reconcile.
type migrationDecision struct {
	// Action is the operation to carry out.
	//
	// The recorded PHASE is deliberately not carried alongside it: the gate reads
	// status.upgrade.phase directly when it resumes, so a copy here could only ever
	// disagree with the record the engine actually acts on.
	Action migrationAction
	// Reason is the condition/Event reason for a refusal; empty otherwise.
	Reason string
	// Message is the refusal's actionable, coordinate-free remedy; empty otherwise.
	Message string
}

// migrationInFlight reports whether a messaging migration is currently running. status.upgrade
// exists ONLY for the duration of a migration — the engine writes it before its first side
// effect and clears it when the migration finishes — so its presence is the whole answer.
func migrationInFlight(p *otilmv1alpha1.Platform) bool {
	return p.Status.Upgrade != nil
}

// decideMigration concludes what this reconcile must do about a messaging migration, from the
// platform's spec + status and the source (running) and target (requested) version bundles.
//
// A recorded migration is decided FIRST and entirely on its own terms: while one is in flight
// the only question is whether the user still wants it (resume), has reverted it (abort or, past
// the point of no return, refuse), or has asked for something else entirely (refuse). Only with
// no migration recorded does the requested move itself get evaluated.
func decideMigration(p *otilmv1alpha1.Platform, from, to bom.Bundle) migrationDecision {
	if migrationInFlight(p) {
		return decideInFlightMigration(p)
	}
	return decideMigrationStart(p, from, to)
}

// decideInFlightMigration decides the fate of the migration recorded on status.upgrade against
// the version the platform now asks for.
//
// The revert window is deliberately narrow. While FENCING or DRAINING nothing has moved: the
// source topology still holds everything and the workloads are merely scaled to zero, so
// unwinding restores exactly the prior state. From CUTTINGOVER onwards the platform is (or is
// becoming) live on the target topology, and reverting would strand whatever already moved —
// so the migration is forward-only from there and the refusal says so.
func decideInFlightMigration(p *otilmv1alpha1.Platform) migrationDecision {
	u := p.Status.Upgrade
	requested := effectivePlatformVersion(p)

	switch requested {
	case u.ToVersion:
		return migrationDecision{Action: migrationActionResume}

	case u.FromVersion:
		if u.Phase == otilmv1alpha1.MigrationPhaseFencing || u.Phase == otilmv1alpha1.MigrationPhaseDraining {
			return migrationDecision{Action: migrationActionAbort}
		}
		return migrationDecision{
			Action: migrationActionRefuse, Reason: reasonMigrationForwardOnly,
			Message: fmt.Sprintf("the messaging migration to %s has passed the point where it can be reverted (phase %s); "+
				"set spec.version back to %s to let it finish", u.ToVersion, u.Phase, u.ToVersion),
		}

	default:
		return migrationDecision{
			Action: migrationActionRefuse, Reason: reasonMigrationInProgress,
			Message: fmt.Sprintf("a messaging migration to %s is in progress (phase %s), so no other version can be requested yet; "+
				"set spec.version to %s to let it finish, or back to %s to abort it while it is still fencing or draining",
				u.ToVersion, u.Phase, u.ToVersion, u.FromVersion),
		}
	}
}

// decideMigrationStart decides whether the requested version move needs a migration at all, and
// whether the operator is able (and willing) to run it.
//
// Only a version CHANGE of a platform that is already running can rename the topology: a fresh
// install renders the requested version's topology directly, and a reconcile that keeps the
// version cannot move the vhost.
func decideMigrationStart(p *otilmv1alpha1.Platform, from, to bom.Bundle) migrationDecision {
	running := p.Status.ObservedVersion
	requested := effectivePlatformVersion(p)
	if running == "" || requested == "" || running == requested {
		return migrationDecision{Action: migrationActionNone}
	}

	if !platformbuilder.MessagingManaged(p) {
		return decideExternalMessagingMigration(p, from, to, running, requested)
	}

	// The effective vhost is the trigger: unchanged means the source and target topologies
	// share their object identities and an ordinary apply converges them additively.
	if platformbuilder.ManagedVirtualHostFor(p, from) == platformbuilder.ManagedVirtualHostFor(p, to) {
		if virtualHostPinSuppressesRename(p, from, to) {
			return migrationDecision{Action: migrationActionVerifyPin}
		}
		return migrationDecision{Action: migrationActionNone}
	}

	// POLICY, not impossibility: a source topology without the dedicated administrator user
	// is the marker of the single-user pre-2.18.0 messaging layout. Its lone user does carry
	// administrator tags and a known credentials Secret, so the management API the drain uses
	// would technically work against it — the operator simply declines to support and test a
	// migration from that layout, and directs the platform through the stepping stone instead.
	if !from.Messaging.HasUserRole(bom.MessagingUserAdministrator) {
		return migrationDecision{
			Action: migrationActionRefuse, Reason: reasonSteppingStoneRequired,
			Message: fmt.Sprintf("a messaging migration from platform version %s is not supported; upgrade to %s first, then to %s",
				running, migrationSteppingStoneVersion, requested),
		}
	}

	return migrationDecision{Action: migrationActionStart}
}

// virtualHostPinSuppressesRename reports whether the ONLY reason the effective virtual hosts
// match across a version move is the platform's own spec.messaging.virtualHost: the two
// bundles default to DIFFERENT vhosts, and the pin overrides both.
//
// It is the precondition of migrationActionVerifyPin and nothing more — it says a pin is
// standing between this move and a migration, NOT that the pin is illegitimate. A pin that was
// already in effect while the platform ran its source version is exactly the documented
// no-migration path (the topology has always lived on that one vhost, so the move renames
// nothing); a pin introduced in the same update as the version bump only LOOKS like it, and
// telling the two apart needs the cluster.
func virtualHostPinSuppressesRename(p *otilmv1alpha1.Platform, from, to bom.Bundle) bool {
	return p.Spec.Messaging.VirtualHost != "" &&
		from.Messaging.DefaultVirtualHost != to.Messaging.DefaultVirtualHost
}

// decideExternalMessagingMigration decides a version move for a platform on an EXTERNAL broker.
//
// The operator provisions no topology there and cannot migrate a broker it does not own, so the
// effective-vhost comparison is not the right question: an external platform's vhost is its own
// spec value, identical under both bundles. What matters is whether the target version stops
// publishing to a topology the running version uses — a RENAME the broker's owner must apply by
// hand. Until they attest to having done so, target-scoped, the move is refused.
func decideExternalMessagingMigration(p *otilmv1alpha1.Platform, from, to bom.Bundle, running, requested string) migrationDecision {
	if !messagingExchangesRenamed(from, to) {
		return migrationDecision{Action: migrationActionNone}
	}
	if p.Spec.Messaging.MigrationAcknowledgedForVersion == requested {
		return migrationDecision{Action: migrationActionNone}
	}
	return migrationDecision{
		Action: migrationActionRefuse, Reason: reasonExternalMessagingMigrationRequired,
		Message: fmt.Sprintf("upgrading from %s to %s renames the messaging topology the platform publishes to, and the operator does not "+
			"manage an external broker; on your broker create the target version's exchanges, queues and bindings, move or drain whatever "+
			"the current ones still hold, re-enrol remote proxies, then set spec.messaging.migrationAcknowledgedForVersion to %q to proceed",
			running, requested, requested),
	}
}

// messagingTopologyDiffers reports whether moving between two bundles changes the messaging
// topology the platform depends on, in whichever way matters for the platform's own broker
// mode: the effective virtual host for a MANAGED broker (the operator owns that topology and
// migrates it), and an exchange RENAME for an EXTERNAL one (the operator owns nothing there,
// so what matters is whether the target stops publishing to something the source used).
//
// It is the same question decideMigrationStart asks, factored out so a caller that has no
// migration to decide — the unrecorded-running-version guard, which must judge a move the
// engine would otherwise treat as a fresh install — asks it identically rather than growing a
// second, drifting definition.
func messagingTopologyDiffers(p *otilmv1alpha1.Platform, from, to bom.Bundle) bool {
	if !platformbuilder.MessagingManaged(p) {
		return messagingExchangesRenamed(from, to)
	}
	return platformbuilder.ManagedVirtualHostFor(p, from) != platformbuilder.ManagedVirtualHostFor(p, to)
}

// messagingExchangesRenamed reports whether the target bundle STOPS serving an exchange the
// source bundle's platform publishes to. That is the signature of a topology RENAME, which
// leaves the source exchanges behind holding whatever was on them. A purely ADDITIVE change —
// a bundle that keeps every exchange and adds one — needs no migration of what already exists,
// so it is deliberately not reported here.
func messagingExchangesRenamed(from, to bom.Bundle) bool {
	target := make(map[string]struct{}, len(to.Messaging.Exchanges))
	for _, e := range to.Messaging.Exchanges {
		target[e.Name] = struct{}{}
	}
	for _, e := range from.Messaging.Exchanges {
		if _, kept := target[e.Name]; !kept {
			return true
		}
	}
	return false
}
