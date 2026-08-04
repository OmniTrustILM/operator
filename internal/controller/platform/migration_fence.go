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

// migration_fence.go is the CONTROLLER side of the messaging-migration fence: the code that
// actually scales the platform's message producers to zero, holds them there, and lets them
// back up. The render side (internal/builder/platform/fence.go) only decides which
// components stop sending .spec.replicas.
//
// THREE MECHANICS CARRY THE WHOLE THING:
//
//  1. THE RECORD COMES FIRST, AND IT IS THE COMPLETE INTENDED SET. status.upgrade.fenced is
//     written with EVERY producer the migration means to hold down — name, kind and the
//     replica count each one carried — BEFORE the first workload is patched. A crash
//     mid-fencing then leaves a recoverable SUPERSET (some entries may name workloads that
//     were never actually patched, which restore handles idempotently) rather than an unknown
//     partial state with no record of what to restore.
//
//     A target that does not exist yet is recorded TOO, marked Absent. Skipping it would put
//     the workload permanently outside the fence: the ordinary render creates it on the very
//     next apply, at the configured replica count, publishing to the virtual host the
//     migration is draining — while a fence that only inspects what it recorded sees nothing
//     wrong. Recorded, it is re-fenced the instant it appears (the render omits its replicas
//     and enforceMigrationFence re-writes the zero behind every apply) and the Fencing phase
//     keeps waiting until it is observed stopped.
//
//  2. THE PATCH RUNS AFTER THE OPERATOR'S APPLY, WITHIN THE SAME RECONCILE, EVERY RECONCILE.
//     The reconciler's Server-Side Apply force-owns every field it sends, so the fence cannot
//     rely on a claim it staked once: enforceMigrationFence re-writes the zero behind each
//     apply for as long as the workload appears in the fenced list. The patch uses a DISTINCT
//     field manager, so managedFields shows unambiguously which actor holds the workload
//     down, and a merge patch rather than an apply, so it never disturbs the field set the
//     reconciler owns.
//
//  3. RESTORE IS THE MIRROR IMAGE, WITH THE SAME FIELD MANAGER. It writes the recorded count
//     back and only THEN removes the entry from status.upgrade.fenced, so the fence's own
//     invariants (render omits replicas, re-assert keeps the value) hold right up to the
//     moment the workload is released — and a crash between the two halves simply repeats a
//     patch that is already idempotent.
//
// SECURITY: nothing here touches messaging coordinates or credentials. It reads and writes
// replica counts and object names only, and surfaces neither in status beyond the workload
// names the CRD already records.

import (
	"context"
	"errors"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fenceFieldManager is the field manager the migration fence writes .spec.replicas with
// ("ilm-operator-migration-fence"). It is deliberately DISTINCT from the reconciler's
// Server-Side Apply owner: the fence and the ordinary render are two different actors
// competing for one field, and keeping them apart is what makes the zero attributable in
// managedFields and re-claimable behind every apply. Restore writes through the SAME manager,
// so the field's ownership history stays a single, coherent story.
const fenceFieldManager = fieldManager + "-migration-fence"

// reasonMigrationFenceError is the condition/Event reason for a failure to hold the fence.
// A producer the operator cannot scale to zero would keep publishing to the source virtual
// host, so the drain would never finish — the reconcile must surface that, not swallow it.
const reasonMigrationFenceError = "MigrationFenceError"

// fenceWorkloads stops the platform's message producers for the migration recorded on
// status.upgrade: it records the complete set of workloads it is about to fence (name, kind
// and current replica count) and then patches each of them to zero.
//
// It is IDEMPOTENT and re-enterable. The recording step runs only while status.upgrade.fenced
// is still empty, so a second call after a crash never overwrites the counts recorded by the
// first (which are the only surviving evidence of what the platform was running); it goes
// straight to re-asserting the zeros. A workload that does not exist yet is recorded as
// Absent rather than skipped, so it is fenced if the render brings it up.
func (r *Reconciler) fenceWorkloads(ctx context.Context, p *otilmv1alpha1.Platform) error {
	if p.Status.Upgrade == nil {
		return errors.New("no messaging migration is recorded on the platform, so there is nothing to fence")
	}

	if len(p.Status.Upgrade.Fenced) == 0 {
		recorded, err := r.recordFenceTargets(ctx, p)
		if err != nil {
			return err
		}
		if len(recorded) == 0 {
			return nil
		}
		// Persist the WHOLE list before the first patch: from here on, every entry is
		// restorable even if the process dies between two patches.
		p.Status.Upgrade.Fenced = recorded
		if err := r.Status().Update(ctx, p); err != nil {
			return err
		}
	}

	return r.enforceMigrationFence(ctx, p)
}

// recordFenceTargets returns the entries to record on status.upgrade.fenced: one for EVERY
// fence target the platform renders, whether or not it currently exists.
//
// For a workload that exists the count is read from the LIVE object rather than derived from
// the spec, so what restore writes back is what the platform was actually running (including a
// count an HPA had scaled it to). A workload that is absent is recorded with Absent=true and
// no count: there is nothing to restore into, but there IS something to hold down if the
// render creates it, and the fence's list is the only place that knowledge lives.
func (r *Reconciler) recordFenceTargets(ctx context.Context, p *otilmv1alpha1.Platform) ([]otilmv1alpha1.FencedWorkload, error) {
	targets := platformbuilder.MigrationFenceTargets(p)
	recorded := make([]otilmv1alpha1.FencedWorkload, 0, len(targets))
	for _, t := range targets {
		obj, err := workloadObject(t.Kind)
		if err != nil {
			return nil, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: t.Name, Namespace: p.Namespace}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				recorded = append(recorded, otilmv1alpha1.FencedWorkload{Name: t.Name, Kind: t.Kind, Absent: true})
				continue
			}
			return nil, fmt.Errorf("reading %s %q to fence it: %w", t.Kind, t.Name, err)
		}
		recorded = append(recorded, otilmv1alpha1.FencedWorkload{
			Name: t.Name, Kind: t.Kind, Replicas: workloadReplicaCount(obj),
		})
	}
	return recorded, nil
}

// enforceMigrationFence re-claims .spec.replicas=0 for every workload listed in
// status.upgrade.fenced.
//
// It MUST run AFTER the reconcile's Server-Side Apply and within the SAME pass: the apply is
// the one thing that can un-fence a producer, so the zero is re-written behind it on every
// reconcile, for exactly as long as the workload appears in the list. Nothing is fenced ⇒ no
// work at all, which is the steady state of every platform that is not mid-migration.
func (r *Reconciler) enforceMigrationFence(ctx context.Context, p *otilmv1alpha1.Platform) error {
	if !platformbuilder.MigrationFenceActive(p) {
		return nil
	}
	for _, w := range p.Status.Upgrade.Fenced {
		if err := r.patchWorkloadReplicas(ctx, p.Namespace, w, 0); err != nil {
			return err
		}
	}
	return nil
}

// restoreWorkload lifts the fence from ONE workload: it writes the recorded replica count
// back under the SAME field manager that wrote the zero, and only THEN removes the entry
// from status.upgrade.fenced.
//
// That order is the crash contract. While the entry is present the render keeps omitting
// .spec.replicas and enforceMigrationFence keeps re-asserting whatever the fence last wrote,
// so re-entering after a crash mid-restore simply repeats an idempotent patch. Removing the
// entry first would do the opposite: the very next apply would re-assert the configured count
// on a workload the engine has not yet restored, and the fence would have released a producer
// it never actually scaled back up. An entry that is already gone is a no-op, so a caller may
// retry freely.
func (r *Reconciler) restoreWorkload(ctx context.Context, p *otilmv1alpha1.Platform, w otilmv1alpha1.FencedWorkload) error {
	if p.Status.Upgrade == nil {
		return nil
	}
	// An entry the fence recorded as ABSENT has no count to write back — there was no running
	// workload to remember. Dropping its entry is the whole restore: the render stops omitting
	// .spec.replicas the moment the entry is gone, so the next apply gives the workload the
	// count the spec asks for rather than a zero the fence invented.
	if !w.Absent {
		if err := r.patchWorkloadReplicas(ctx, p.Namespace, w, w.Replicas); err != nil {
			return err
		}
	}

	remaining := make([]otilmv1alpha1.FencedWorkload, 0, len(p.Status.Upgrade.Fenced))
	for _, f := range p.Status.Upgrade.Fenced {
		if f.Name != w.Name {
			remaining = append(remaining, f)
		}
	}
	if len(remaining) == len(p.Status.Upgrade.Fenced) {
		return nil // already restored on an earlier pass
	}
	p.Status.Upgrade.Fenced = remaining
	return r.Status().Update(ctx, p)
}

// patchWorkloadReplicas writes .spec.replicas on one fenced workload under the fence's own
// field manager.
//
// It is a MERGE patch, deliberately not a Server-Side Apply: an apply would have to describe
// the whole workload to own one field of it, whereas a merge patch touches replicas and
// nothing else, leaving the field set the reconciler owns exactly as the render left it. The
// body is the literal {"spec":{"replicas":N}} rather than a marshalled typed object because a
// Deployment/StatefulSet spec marshals its required selector and template as null, and JSON
// merge patch reads a null as DELETE — which the apiserver would (rightly) reject.
//
// A workload that has since been deleted is not an error: there is no producer left to hold
// down, and nothing to restore into.
func (r *Reconciler) patchWorkloadReplicas(ctx context.Context, namespace string, w otilmv1alpha1.FencedWorkload, replicas int32) error {
	obj, err := workloadObject(w.Kind)
	if err != nil {
		return err
	}
	obj.SetName(w.Name)
	obj.SetNamespace(namespace)

	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas))
	if err := r.Patch(ctx, obj, patch, client.FieldOwner(fenceFieldManager)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("patching %s %q replicas: %w", w.Kind, w.Name, err))
	}
	return nil
}

// workloadObject returns an empty typed object for a recorded workload kind. The kind lives
// on the Platform's status, so an unrecognised value is rejected explicitly rather than
// silently addressed as a Deployment — patching the wrong kind would leave the real producer
// running while the migration believed it was stopped.
func workloadObject(kind string) (client.Object, error) {
	switch kind {
	case string(otilmv1alpha1.WorkloadKindDeployment):
		return &appsv1.Deployment{}, nil
	case string(otilmv1alpha1.WorkloadKindStatefulSet):
		return &appsv1.StatefulSet{}, nil
	default:
		return nil, fmt.Errorf("unsupported fenced workload kind %q", kind)
	}
}

// workloadReplicaCount reads .spec.replicas off a Deployment or StatefulSet, falling back to
// the apiserver's own default of 1 when the field is unset (which is what an unset field
// means for both kinds).
func workloadReplicaCount(obj client.Object) int32 {
	switch w := obj.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
	}
	return 1
}
