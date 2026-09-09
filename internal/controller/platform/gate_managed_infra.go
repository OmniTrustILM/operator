/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// gate_managed_infra.go factors out the ONE reconcile/teardown shape every managed-
// infrastructure component shares — the managed database (CloudNativePG) and the managed
// broker (RabbitMQ) today, the managed Keycloak later. It is the managed-infra analogue of
// applyGatedObjects/gatedObjects (which unifies the edge + admin-cert gates): each
// component supplies a small managedInfraGate describing its predicate, render objects,
// dependencies, readiness probe, and condition vocabulary, and this one path drives the
// gating (CRD-absent → False/<reason> + requeue; applied-but-not-ready → False/Waiting +
// requeue; ready → True) and the deletion (Retain leaves, Delete cascades) identically for
// all of them — so the per-component gate/handler functions stay thin and never drift.
//
// SECURITY: condition/event messages name only the spec field + remedy — never a secret
// value or connection coordinate (the component supplies leak-free text).

import (
	"context"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// conditionTypeReady is the status condition TYPE the upstream operators (CloudNativePG, the
// RabbitMQ Messaging Topology Operator, the Keycloak Operator) set to "True" on their CRs once
// the resource is reconciled in the backing system. The managed-infra readiness probes
// (managedDatabaseReady / managedVhostReady / managedKeycloakReady) all key off it.
const conditionTypeReady = "Ready"

// managedInfraGate bundles everything that varies between one managed-infrastructure
// component and another, so gateManagedInfra (and handleManagedInfraDeletion) can drive
// them uniformly. All the funcs are pure/builder calls; the reconciler-bound readiness
// probe is the one closure each component provides.
type managedInfraGate struct {
	// managed reports whether this component is in managed mode for the Platform.
	managed bool
	// kind is the human label used in apply-error wrapping and Events (e.g. "database",
	// "broker"). Names/labels only — never secret material.
	kind string
	// name is the operator-owned managed object name (for deletion Events/logs).
	name string
	// objects are the rendered managed CRs (cluster + any topology), each carrying a
	// possible render-time error retrievable via renderError.
	objects []client.Object
	// renderError returns the render-time error stashed on a rendered object (a rejected
	// override / malformed patch), or nil.
	renderError func(client.Object) error
	// dependencies are the upstream-CRD prerequisites to probe before applying.
	dependencies []platformbuilder.CRDDependency
	// ready probes whether the applied managed component is fully Ready (its cluster
	// reports Ready AND its generated credentials Secret exists). It is called only after
	// a successful apply.
	ready func(ctx context.Context) bool
	// versionGuard, when non-nil, is the MAJOR-version upgrade guard for this managed
	// component (see gate_managed_upgrade.go). It runs after the CRDs are confirmed present
	// and BEFORE apply: it blocks a major engine bump of the running cluster (re-pinning the
	// running version on the rendered objects so the apply leaves the engine unchanged) until
	// spec.<infra>.managed.upgradeAcknowledged=true, as an adjunct (never degrades). nil for
	// a component that pins no engine version (none today; all three set it).
	versionGuard *infraVersionGuard
	// conditionType is the adjunct status condition this gate manages (e.g. "DatabaseReady").
	conditionType string
	// waitingReason / readyReason are the condition reasons for the not-yet-ready and ready
	// states; waitingMessage / readyMessage are their leak-free messages.
	waitingReason  string
	waitingMessage string
	readyReason    string
	readyMessage   string
	// detectionLabel names the component in dependency-detection log lines (e.g. "database").
	detectionLabel string
	// isPrereq, when non-nil, marks the subset of objects that must be applied AND become
	// ready (prereqReady) BEFORE the remaining objects are applied — a two-phase apply that
	// orders the RabbitMQ topology so the Vhost exists in the broker before its dependent
	// exchanges/queues/bindings/permissions are declared. The Messaging Topology Operator
	// otherwise declares a dependent against a not-yet-created vhost, fails it with
	// vhost_not_found, and converges only after a slow per-object backoff. nil for the
	// database/Keycloak, which apply every object in a single pass.
	isPrereq func(client.Object) bool
	// prereqReady probes whether the prerequisite objects are ready (e.g. the Vhost CR reports
	// Ready). Consulted only when isPrereq is non-nil; while it reports false the dependents are
	// not applied and the gate waits (conditionType=False/waitingReason + requeue).
	prereqReady func(ctx context.Context) bool
	// prereqWaitingMessage is the (leak-free) condition message while waiting on the
	// prerequisite; falls back to waitingMessage when empty.
	prereqWaitingMessage string
}

// managedReadyProbe composes the readiness predicate every managed-infra component shares:
// the cluster reports Ready (clusterReady, with a transient read error tolerated as
// not-ready so the requeue retries) AND the generated credentials Secret exists
// (secretPresent). It collapses each component's readiness closure to a single call so the
// per-component gate descriptors carry only their distinct probe functions.
func managedReadyProbe(ctx context.Context, clusterReady func(context.Context) (bool, error), secretPresent func(context.Context) bool) bool {
	ready, err := clusterReady(ctx)
	if err != nil {
		ready = false
	}
	return ready && secretPresent(ctx)
}

// gateManagedInfra reconciles one managed-infrastructure component and reflects its state
// on its adjunct condition, returning ready/requeue/err with exactly the semantics the
// per-component gates document:
//
//   - not managed → drop any stale condition; ready=true (nothing to provision/probe).
//   - render-time error (rejected override) → hard error (degrade).
//   - a dep absent (or transient detection error) → condition False/<dep.Reason> + Event,
//     mark objects desired (prune-preservation across a flap), ready=false, requeue.
//   - deps present → apply every object (NO owner ref — applyManaged), then probe ready:
//     not ready → False/<waitingReason>, ready=false, requeue; ready → True, ready=true.
func (r *Reconciler) gateManagedInfra(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, g managedInfraGate) (ready bool, requeue bool, err error) {
	if !g.managed {
		// External: nothing to provision or probe; drop any stale condition from a previous
		// managed generation. The platform is free to converge.
		meta.RemoveStatusCondition(&p.Status.Conditions, g.conditionType)
		return true, false, nil
	}

	// A rejected override / malformed patch is a deterministic user error, not a transient
	// state: surface it as a hard error so the Platform degrades with an actionable,
	// leak-free message (the builder embedded the field path).
	for _, obj := range g.objects {
		if rerr := g.renderError(obj); rerr != nil {
			return false, false, rerr
		}
	}

	// Probe the upstream CRD prerequisites. A transient detector error is folded into the
	// "not yet available" path (recorded as the dep's reason), never a hard failure.
	if missing, ok := r.firstMissingDependency(ctx, p, g); !ok {
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: g.conditionType, Status: metav1.ConditionFalse, Reason: missing.Reason,
			Message: missing.Message, ObservedGeneration: p.Generation, // no secret/coordinate leakage
		})
		// Preserve already-applied managed objects across a CRD flap: the component is
		// still managed, so mark its rendered objects desired even though we are not
		// applying them this reconcile. (The prune EXCLUDES the managed kinds anyway —
		// this keeps the desired-set bookkeeping consistent with the edge's gate.)
		for _, obj := range g.objects {
			desired.add(r, obj)
		}
		r.event(p, corev1.EventTypeWarning, missing.Reason, missing.Message)
		return false, true, nil
	}

	// MAJOR-version upgrade guard: before applying, block a major engine bump of the running
	// cluster unless acknowledged (re-pinning the running version onto the rendered objects so
	// the apply below leaves the engine unchanged). It is an ADJUNCT — when it blocks we set
	// its own UpgradeBlocked condition + Warning and request a requeue, but STILL apply the
	// (re-pinned) objects and keep evaluating readiness, so the platform stays converged on the
	// healthy running version and never degrades. First creation / a patch-minor jump / an
	// acknowledged jump all fall through and apply the desired version normally.
	upgradeBlocked := false
	if g.versionGuard != nil {
		upgradeBlocked = r.guardMajorUpgrade(ctx, p, g.objects, *g.versionGuard)
	}

	// CRDs present: apply the managed objects. They carry NO controller owner reference
	// (deletion safety) — applyManaged stamps only the managed-by/instance labels and never
	// SetControllerReference, so a transient de-render never deletes the infrastructure, and
	// they are EXCLUDED from the prune. When a prerequisite gate is set (the RabbitMQ vhost),
	// the prerequisite objects are applied FIRST and the dependents wait until the prerequisite
	// is ready — so a dependent (exchange/queue/binding/permission) is never declared against a
	// not-yet-created vhost. Without a prereq gate (database/Keycloak) the first pass is a no-op
	// and every object applies in the second.
	if waitingPrereq, aerr := r.applyManagedObjects(ctx, p, desired, g); aerr != nil || waitingPrereq {
		return false, waitingPrereq, aerr
	}

	// Probe readiness: the cluster's Ready status AND its generated credentials Secret.
	// Until both hold, the platform's dependents would fail to connect, so this is a
	// waiting state.
	if !g.ready(ctx) {
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: g.conditionType, Status: metav1.ConditionFalse, Reason: g.waitingReason,
			Message: g.waitingMessage, ObservedGeneration: p.Generation,
		})
		return false, true, nil
	}

	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: g.conditionType, Status: metav1.ConditionTrue, Reason: g.readyReason,
		Message: g.readyMessage, ObservedGeneration: p.Generation,
	})
	// The running cluster is Ready (its current engine version is healthy). When a major
	// upgrade is blocked pending acknowledgement we still requeue so the operator re-checks
	// (and lets the upgrade through once acknowledged) without the cluster ever being marked
	// not-ready — the bump is suppressed, not the running broker/database.
	return true, upgradeBlocked, nil
}

// firstMissingDependency probes the gate's upstream-CRD prerequisites in order and returns
// the first one that is not available (ok=false). A transient detector error is folded into
// the "not yet available" path (logged, treated as unavailable), never a hard failure. When
// every dependency is available it returns ok=true and a zero CRDDependency.
func (r *Reconciler) firstMissingDependency(ctx context.Context, _ *otilmv1alpha1.Platform, g managedInfraGate) (platformbuilder.CRDDependency, bool) {
	for _, dep := range g.dependencies {
		available, derr := r.Capabilities.Available(dep.GroupKind, dep.Versions...)
		if derr != nil {
			log.FromContext(ctx).Info(g.detectionLabel+" dependency detection failed; treating as not yet available",
				"groupKind", dep.GroupKind.String(), "err", derr.Error())
			available = false
		}
		if !available {
			return dep, false
		}
	}
	return platformbuilder.CRDDependency{}, true
}

// applyManagedObjects server-side-applies the gate's managed objects in two phases —
// prerequisites first (e.g. the RabbitmqCluster + its Vhost), then dependents — gating the
// dependents on the prerequisite's readiness. When the prerequisite is not yet ready it sets
// the waiting condition, marks every object desired (prune-preservation across the wait), and
// returns waitingPrereq=true so the caller requeues without applying the dependents. The
// managed objects carry NO controller owner reference (deletion safety). Without a prereq gate
// (database/Keycloak) the first phase is a no-op and every object applies in the second.
func (r *Reconciler) applyManagedObjects(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, g managedInfraGate) (waitingPrereq bool, err error) {
	// Phase 1: prerequisites (e.g. the RabbitmqCluster + its Vhost).
	if perr := r.applyManagedPhase(ctx, p, desired, g, true); perr != nil {
		return false, perr
	}
	// Gate the dependents on the prerequisite's readiness (e.g. the Vhost CR Ready). While it
	// is not ready, set the waiting condition + requeue and DO NOT apply the dependents; mark
	// every object desired so the prune preserves any already-applied copies across the wait.
	if r.prereqNotReady(ctx, p, desired, g) {
		return true, nil
	}
	// Phase 2: dependents (exchanges/queues/bindings/permissions/users).
	if perr := r.applyManagedPhase(ctx, p, desired, g, false); perr != nil {
		return false, perr
	}
	return false, nil
}

// applyManagedPhase server-side-applies the gate's objects whose prerequisite classification
// matches prereq (true = prerequisites, false = dependents). Objects without a prereq
// classifier are all treated as dependents (applied in the prereq=false phase). The objects
// carry NO controller owner reference (deletion safety, via applyManaged).
func (r *Reconciler) applyManagedPhase(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, g managedInfraGate, prereq bool) error {
	for _, obj := range g.objects {
		if (g.isPrereq != nil && g.isPrereq(obj)) != prereq {
			continue
		}
		if aerr := r.applyManaged(ctx, p, obj, desired); aerr != nil {
			// KIND AND API REASON ONLY. A managed object's NAME is a coordinate (the messaging
			// topology's names encode the virtual host they are scoped to), and so is the
			// apiserver's own error text, which quotes the object it refused. safeErrorf
			// publishes neither while keeping the API error reachable for the transience
			// classification applyOrDegrade makes.
			return safeErrorf(aerr, "applying a managed %s %s failed (%s)",
				g.kind, obj.GetObjectKind().GroupVersionKind().Kind, apiFailureReason(aerr))
		}
	}
	return nil
}

// prereqNotReady reports whether the gate has a configured prerequisite that is not yet ready.
// When so it records the waiting condition and marks every object desired (prune-preservation
// across the wait) as a side effect, so the caller requeues without applying the dependents.
// A gate with no prereq classifier / readiness probe is never "not ready" here.
func (r *Reconciler) prereqNotReady(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet, g managedInfraGate) bool {
	if g.isPrereq == nil || g.prereqReady == nil || g.prereqReady(ctx) {
		return false
	}
	waitMsg := g.prereqWaitingMessage
	if waitMsg == "" {
		waitMsg = g.waitingMessage
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: g.conditionType, Status: metav1.ConditionFalse, Reason: g.waitingReason,
		Message: waitMsg, ObservedGeneration: p.Generation,
	})
	for _, obj := range g.objects {
		desired.add(r, obj)
	}
	return true
}

// teardownGate builds the managed-infra gate the DELETION path acts on: build is the
// per-component gate constructor (databaseGate / messagingGate / keycloakDeletionGate), and it
// is called once per teardown-render platform (teardownRenderPlatforms) with the object sets
// merged into their DEDUPLICATED UNION.
//
// The union is what makes a partially applied upgrade — or a legacy-scoped custom vhost —
// safe to delete: sweeping every bundle version (plus the legacy-scope variant) leaves objects
// from every rendered topology live, and a single render would orphan the rest. Every
// non-object field (managed, kind, name, the Event labels) comes from renders[0]'s gate; none
// of them vary with the pinned spec.version or spec.messaging.virtualHost (they are a fixed
// function of the Platform name and its managed/mode spec), so which render is first does not
// matter.
func (r *Reconciler) teardownGate(p *otilmv1alpha1.Platform, build func(*otilmv1alpha1.Platform) managedInfraGate) managedInfraGate {
	renders := teardownRenderPlatforms(p)
	g := build(renders[0])
	for _, extra := range renders[1:] {
		g.objects = mergeManagedObjects(g.objects, build(extra).objects)
	}
	return g
}

// mergeManagedObjects concatenates two rendered managed-object sets, dropping duplicates —
// objects the two renders have in common (same GVK, namespace and name), which is most of a
// topology when a version bump renames only part of it. Order is preserved (primary set first)
// so teardown still deletes in the rendered order.
func mergeManagedObjects(primary, extra []client.Object) []client.Object {
	out := make([]client.Object, 0, len(primary)+len(extra))
	seen := make(map[string]struct{}, len(primary)+len(extra))
	for _, set := range [][]client.Object{primary, extra} {
		for _, obj := range set {
			// The Go type is part of the identity so a typed object whose TypeMeta is empty
			// (only the unstructured renders carry a GVK) can never collide with another kind.
			key := fmt.Sprintf("%s|%T|%s/%s", obj.GetObjectKind().GroupVersionKind(), obj,
				obj.GetNamespace(), obj.GetName())
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, obj)
		}
	}
	return out
}

// handleManagedInfraDeletion enforces the deletion-safety contract for one managed-
// infrastructure component on Platform deletion, shared by the database/broker handlers:
//
//   - not managed → no-op (the operator provisions nothing to tear down).
//   - Retain (default) → leave the managed CRs + their data intact; record a Warning Event
//     naming the retained component (object name only — no coordinate). Deletes nothing.
//   - Delete → delete every managed CR (the upstream operator GCs its PVCs); a NotFound is
//     ignored (already gone); any other delete error is returned so the finalizer keeps the
//     Platform and the teardown is retried.
//
// retainReason/deleteReason are the Event reasons (e.g. "RetainedDatabase"); kind is the
// human label in the messages/logs. The managed CRs carry NO owner reference and are
// prune-excluded, so this handler is the ONLY thing that deletes them, and only under Delete.
func (r *Reconciler) handleManagedInfraDeletion(ctx context.Context, p *otilmv1alpha1.Platform, policy otilmv1alpha1.PlatformDeletionPolicy, g managedInfraGate, retainReason, deleteReason string) error {
	if !g.managed {
		return nil
	}

	if policy != otilmv1alpha1.PlatformDeletionPolicyDelete {
		// Retain: protect the component and its data. Name the retained object (a non-secret
		// object name) but never a connection coordinate.
		r.eventf(p, corev1.EventTypeWarning, retainReason,
			"deletionPolicy=Retain: leaving managed %s %q and its data intact", g.kind, g.name)
		log.FromContext(ctx).Info("retaining managed "+g.kind+" on Platform deletion", "name", p.Name, g.kind, g.name)
		return nil
	}

	// Delete: reclaim every managed CR. The upstream operator GCs the PVCs when the cluster
	// is deleted, so the operator does not touch storage directly.
	for _, obj := range g.objects {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting managed %s %s %q: %w", g.kind,
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	r.eventf(p, corev1.EventTypeNormal, deleteReason,
		"deletionPolicy=Delete: reclaimed managed %s %q", g.kind, g.name)
	log.FromContext(ctx).Info("reclaimed managed "+g.kind+" on Platform deletion", "name", p.Name, g.kind, g.name)
	return nil
}
