/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// gate_migration_source_state.go guards the engine's SOURCE STATE — its belief about where the
// platform actually is before a version move.
//
// Every decision the migration engine makes rests on two facts it reads from the edited spec
// and from status: the version the platform is RUNNING, and the virtual host its messaging
// topology LIVES ON. Both are inferences, and a single update can invalidate either one in the
// same breath as it requests the move:
//
//   - status.observedVersion is written AFTER the pass applies the children, so an interrupted
//     reconcile — or a restore that carries spec but not status — leaves a live platform whose
//     running version is unrecorded. An empty value reads as a FRESH INSTALL, and a fresh
//     install renders the target directly, with no migration and nothing drained.
//   - the effective virtual host is spec.messaging.virtualHost when the platform pins one, so a
//     pin set in the SAME update as a version bump makes the source and target vhosts resolve
//     identically — the trigger sees no rename and takes the ordinary additive apply path,
//     while the topology the platform is really running on is left holding its messages.
//
// Both guards therefore REFUSE rather than guess: the platform keeps running what it is
// running, and the message names the ordering that makes the move safe. Neither invents state —
// each asks the cluster the one question the spec cannot answer, and both are read-only.
//
// SECURITY: the conditions and Events produced here carry version strings, workload names and
// spec field paths ONLY. The virtual host these guards READ is a broker coordinate and is
// compared in memory — it is never written to status, a condition, an Event or a log.

import (
	"context"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Condition/Event reasons the source-state guards produce. They are identity-free labels.
const (
	// reasonMigrationVirtualHostPinned: spec.messaging.virtualHost pins a virtual host the
	// platform's live topology is NOT on, in the same update as a version move that would
	// otherwise migrate — see guardMigrationVirtualHostPin.
	reasonMigrationVirtualHostPinned = "MigrationVirtualHostPinned"
	// reasonMigrationRunningVersionUnrecorded: status.observedVersion is empty while an owned
	// core workload is demonstrably running a version whose messaging topology differs from the
	// requested one — see guardMigrationUnrecordedRunningVersion.
	reasonMigrationRunningVersionUnrecorded = "MigrationRunningVersionUnrecorded"
)

// guardMigrationVirtualHostPin decides the migrationActionVerifyPin case: the requested move
// WOULD rename the managed virtual host, and only spec.messaging.virtualHost is suppressing
// that rename.
//
// The trigger layer cannot tell a pin that PREDATES the move from one made in the same update,
// because both produce the identical spec. The cluster can: the Vhost CR the operator already
// renders carries the virtual host the live topology is declared on. If it matches the pin, the
// platform has always been on that one vhost, the move renames nothing, and the documented
// no-migration path is correct. If it does not — or if there is no such object at all — then
// the pin is describing a place the platform is not, and taking the additive path would move
// every producer onto the target's topology while the one they are actually publishing to is
// never drained.
//
// A cluster whose rabbitmq.com CRDs are not served answers nothing, so the guard steps aside:
// gateManagedDependencies reports the missing operator in its own words, and there is no
// topology to strand yet either way.
//
// handled=true stops the pass. Refusing is a DETERMINISTIC, user-correctable steady state, the
// same shape guardMigrationWorkloadKind and guardMigrationTimeQualityMonitor use.
func (r *Reconciler) guardMigrationVirtualHostPin(ctx context.Context, p *otilmv1alpha1.Platform, targetVersion string) (bool, ctrl.Result, error) {
	live, served, err := r.liveManagedVirtualHost(ctx, p)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return true, res, aerr
	}
	if !served || live == p.Spec.Messaging.VirtualHost {
		return false, ctrl.Result{}, nil
	}

	from := migrationSourceVersion(p)
	message := fmt.Sprintf(
		"the move from platform version %s to %s cannot proceed: spec.messaging.virtualHost pins a virtual host the platform's "+
			"live messaging topology is not declared on. The two versions provision different virtual hosts by default, so the "+
			"pin is the only reason this move looks like it renames nothing — taking it would put every component on the target "+
			"version's topology while the one they are publishing to now is never drained, and its messages would be stranded. "+
			"Restore spec.version to %s and spec.messaging.virtualHost to the value the platform is running with, then request "+
			"%s on its own so the operator migrates the messaging topology; pin a virtual host before a version move, never in "+
			"the same update as one",
		from, targetVersion, from, targetVersion)
	setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationVirtualHostPinned, message)
	res, serr := r.steadyState(ctx, p, reasonMigrationVirtualHostPinned, message)
	return true, res, serr
}

// liveManagedVirtualHost reads the virtual host the platform's LIVE managed topology is
// declared on, from the Vhost CR's spec.name.
//
// served=false means the rabbitmq.com CRDs are not present on this cluster, which is not an
// answer — the caller steps aside rather than refusing on a cluster that could not have a
// topology yet. A missing Vhost CR IS an answer ("", served): whatever the platform is running,
// it is not this vhost.
//
// SECURITY: the returned value is a broker coordinate. It is compared in memory and must never
// reach status, a condition, an Event or a log.
func (r *Reconciler) liveManagedVirtualHost(ctx context.Context, p *otilmv1alpha1.Platform) (vhost string, served bool, err error) {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK())
	key := client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ManagedMessagingVhostName(p)}
	if gerr := r.Get(ctx, key, &u); gerr != nil {
		if meta.IsNoMatchError(gerr) {
			return "", false, nil
		}
		if apierrors.IsNotFound(gerr) {
			return "", true, nil
		}
		// Kind and API reason only: the object's NAME carries the vhost scope, and so does the
		// apiserver's own error text.
		return "", false, safeErrorf(gerr, "reading the managed messaging %s to check which virtual host the platform is running on failed (%s)",
			platformbuilder.ManagedMessagingVhostGVK().Kind, apiFailureReason(gerr))
	}
	name, _, nerr := unstructured.NestedString(u.Object, "spec", "name")
	if nerr != nil {
		return "", false, safeErrorf(nerr, "reading the managed messaging %s's virtual host failed",
			platformbuilder.ManagedMessagingVhostGVK().Kind)
	}
	return name, true, nil
}

// guardMigrationUnrecordedRunningVersion refuses to treat a LIVE platform as a fresh install.
//
// status.observedVersion is the engine's only record of the version a platform is running, and
// it is written at the END of a successful pass — after the children of that version are
// already applied. An interrupted reconcile, or a backup restored without its status subresource,
// therefore leaves a fully-running platform reporting nothing. decideMigrationStart reads that
// empty value as "there is nothing running to migrate FROM" and returns migrationActionNone, so
// the requested version's topology is rendered straight over a live one and whatever the running
// topology still holds is stranded — silently, because no migration was ever considered.
//
// The pod template's own version annotation is the honest answer the status lost: every rendered
// component carries platformbuilder.PlatformVersionAnnotation, stamped from the same resolution
// that produced its images and wiring (an image tag would not do — several components are pinned
// to the same image across neighbouring bundles). Core is the component to ask because it exists
// in every render.
//
// The refusal is narrow on purpose, so nothing that is genuinely a fresh install is ever
// blocked:
//
//   - no owned core workload, or one carrying no version annotation ⇒ proceed. The first is a
//     real fresh install; the second is a platform rendered by an operator build that predates
//     the annotation, and refusing there would wedge it with no way to answer.
//   - the annotation names the version being requested ⇒ proceed. Nothing to migrate from.
//   - the annotation names a version whose messaging topology matches the target's ⇒ proceed.
//     That is the ordinary additive apply the engine deliberately does not migrate.
//
// Otherwise the pass stops: the running version is named, and the remedy is to request THAT
// version first so the operator records it, after which the move is an ordinary, fully-guarded
// migration. A version this build does not carry is refused too — the topologies cannot be
// compared, and rendering the target on that ignorance is the unsafe half of the guess (the same
// reasoning as reasonMigrationSourceVersionUnknown).
//
// handled=true stops the pass.
func (r *Reconciler) guardMigrationUnrecordedRunningVersion(ctx context.Context, p *otilmv1alpha1.Platform, target bom.Bundle, targetVersion string) (bool, ctrl.Result, error) {
	running, err := r.coreWorkloadPlatformVersion(ctx, p)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, err)
		return true, res, aerr
	}
	if running == "" || running == targetVersion {
		return false, ctrl.Result{}, nil
	}
	source, known := bom.BundleFor(running)
	if known && !messagingTopologyDiffers(p, source, target) {
		return false, ctrl.Result{}, nil
	}

	message := fmt.Sprintf(
		"this platform reports no running version (status.observedVersion is empty) but its core workload is running platform "+
			"version %s, whose messaging topology differs from %s's; treating it as a fresh install would render %s over a live "+
			"%s and strand whatever the running messaging topology still holds — set spec.version to %s and let the platform "+
			"reconcile so the operator records the version it is actually running, then request %s to move, which the operator "+
			"then migrates",
		running, targetVersion, targetVersion, running, running, targetVersion)
	setMigrationCondition(p, metav1.ConditionFalse, reasonMigrationRunningVersionUnrecorded, message)
	res, serr := r.steadyState(ctx, p, reasonMigrationRunningVersionUnrecorded, message)
	return true, res, serr
}

// coreWorkloadPlatformVersion returns the platform version stamped on the LIVE core workload's
// pod template, or "" when this Platform controller-owns no core workload of either kind (or the
// one it owns carries no version annotation).
//
// Both apps/v1 kinds are probed because spec.core.workloadType selects between them, and an
// object without THIS Platform's controller owner reference is ignored — it is somebody else's,
// exactly as the prune and the workload-kind switch treat it.
func (r *Reconciler) coreWorkloadPlatformVersion(ctx context.Context, p *otilmv1alpha1.Platform) (string, error) {
	key := client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ResolveCore(p).ResourceName()}
	for _, probe := range []struct {
		obj      client.Object
		template func(client.Object) *corev1.PodTemplateSpec
	}{
		{obj: &appsv1.Deployment{}, template: func(o client.Object) *corev1.PodTemplateSpec {
			return &o.(*appsv1.Deployment).Spec.Template
		}},
		{obj: &appsv1.StatefulSet{}, template: func(o client.Object) *corev1.PodTemplateSpec {
			return &o.(*appsv1.StatefulSet).Spec.Template
		}},
	} {
		if err := r.Get(ctx, key, probe.obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("reading the core workload to read the version it is running: %w", err)
		}
		if !controllerOwnedBy(probe.obj, p.UID) {
			continue
		}
		return probe.template(probe.obj).Annotations[platformbuilder.PlatformVersionAnnotation], nil
	}
	return "", nil
}
