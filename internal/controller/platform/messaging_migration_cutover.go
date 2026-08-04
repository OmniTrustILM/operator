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

// messaging_migration_cutover.go is the CUTTINGOVER phase of the messaging-migration engine:
// with the source virtual host drained and the producers fenced, it moves the platform onto
// the TARGET topology in dependency order and reopens it.
//
// NO SOURCE RE-PIN. Fencing and Draining hold the in-memory spec.version at the version the
// platform is moving AWAY from, so nothing belonging to the target can be applied while the
// source virtual host still holds traffic. The cutover is the step that ends that: it leaves
// the version resolution's TARGET pin exactly as it found it, so the ordinary reconcile — the
// managed-messaging gate, the base Server-Side Apply, the fence re-assert — renders the target
// bundle. There is no second applier here; the phase only decides WHEN each part is allowed to
// move.
//
// THE ORDER IS NOT COSMETIC — IT IS A DEADLOCK. A proxy-enabled Core runs the
// provision-instance-queue init container, which retries against the provisioning API until it
// answers. The provisioning service is one of the workloads the fence holds at zero replicas.
// Roll Core while provisioning is still fenced and Core can never become Ready, so a cutover
// that waits for Core would wait forever, with no deadline (past the drain, the migration is
// forward-only) and no way back. Hence:
//
//  1. TOPOLOGY — every target topology CR must report Ready. Not the broker plus its
//     credentials Secret, which is all the ordinary messaging gate asks: the cutover rolls Core
//     onto these exchanges, queues and bindings, so each one has to exist in the broker first.
//  2. PROVISIONING — the target render is applied, the fence is lifted from the provisioning
//     service, and it must finish rolling out. This is the stage that makes Core's init
//     container answerable.
//  3. CORE — only now may Core be rolled onto the target bundle, and the cutover waits until it
//     is Ready ON THAT ROLLOUT, never on the source pod template it was already Ready with.
//  4. GATEWAY — the door is reopened and the migration hands over to the cleanup, with the
//     scheduler still fenced (its timed jobs stay stopped until the source topology is gone).
//
// THE STAGE IS MEASURED, NOT COUNTED. Every stage's completion is an observable fact about the
// cluster, so a restarted operator re-derives exactly where it was from the same reads, and a
// step that already ran is a no-op rather than a repeat. What IS written down first is the
// stage itself: the condition records it before the restore or hand-over it authorises runs.
//
// SECURITY: the condition messages carry version strings, phase and stage names only — never a
// virtual host, a topology object name (whose scope encodes the vhost), a broker coordinate or
// a credential.

import (
	"context"
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// reasonMigrationCutoverError is the condition/Event reason for a failure to READ the state the
// staged cutover gates on. It is distinct from the fence and state-write reasons because the
// remedy is different: nothing has been changed, the engine simply could not establish whether
// the next stage may start.
const reasonMigrationCutoverError = "MigrationCutoverError"

// cutoverStage is how far the CuttingOver phase has got. The stages are ordered, and each one
// names the work that is still OUTSTANDING — so cutoverStageCore means "everything Core depends
// on is in place, roll it", not "Core is done".
type cutoverStage int

const (
	// cutoverStageTopology: the target topology is not fully declared in the broker yet.
	cutoverStageTopology cutoverStage = iota
	// cutoverStageProvisioning: the topology is declared; the provisioning service still has to
	// be released from the fence and finish rolling out onto the target bundle.
	cutoverStageProvisioning
	// cutoverStageCore: provisioning answers, so Core may be rolled onto the target bundle; the
	// cutover waits until that rollout is complete.
	cutoverStageCore
	// cutoverStageGateway: everything the platform serves from is on the target; the gateway can
	// be reopened and the migration handed to the cleanup.
	cutoverStageGateway
)

// String names the stage for the migration condition's message. The wording describes the work
// the stage is waiting on, in the operator's own vocabulary — no coordinate, no object name.
func (s cutoverStage) String() string {
	switch s {
	case cutoverStageTopology:
		return "declaring the target messaging topology"
	case cutoverStageProvisioning:
		return "restoring the provisioning service onto the target topology"
	case cutoverStageCore:
		return "rolling core onto the target bundle"
	default:
		return "reopening the platform"
	}
}

// rollsCore reports whether the cutover has reached the point where Core may be rolled onto the
// target bundle — i.e. every dependency Core's pod needs in order to become Ready is in place.
// Until it does, the render must go on applying everything EXCEPT Core's workload, which is
// what migrationRender.holdCore carries out.
func (s cutoverStage) rollsCore() bool {
	return s >= cutoverStageCore
}

// migrationCuttingOverPhase moves the platform onto the target topology, one gated stage per
// reconcile pass, and hands over to the cleanup once the platform is serving from it.
//
// It returns handled=FALSE at every stage: the reconcile has to continue for the stage's work
// to happen at all — the managed-messaging gate is what declares the target topology, and the
// base Server-Side Apply is what rolls the components onto the target bundle. The phase's whole
// contribution is the render it hands back: the TARGET bundle throughout, with Core's workload
// withheld until stage 3.
func (r *Reconciler) migrationCuttingOverPhase(ctx context.Context, p *otilmv1alpha1.Platform, render migrationRender) (migrationRender, bool, ctrl.Result, error) {
	stage, err := r.cutoverStage(ctx, p)
	if err != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationCutoverError, err)
		return render, true, res, aerr
	}

	// Write the stage down BEFORE the step it authorises. A crash between the two re-enters
	// this phase, re-measures the same stage and repeats a step that is idempotent; the opposite
	// order would let a restore or a hand-over happen against a state the cluster never recorded.
	if werr := r.recordCutoverStage(ctx, p, stage); werr != nil {
		res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, werr)
		return render, true, res, aerr
	}

	if stage == cutoverStageProvisioning {
		// Releasing the provisioning service is the stage's own step: it is a fenced PRODUCER,
		// so nothing else will ever scale it back up, and Core cannot start without it.
		if rerr := r.restoreFencedWorkload(ctx, p, provisioningWorkloadName); rerr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, rerr)
			return render, true, res, aerr
		}
	}

	if stage == cutoverStageGateway {
		// The gateway is restored BEFORE the phase moves on, so the cleanup can never inherit a
		// platform whose door is still shut with nothing left that would open it. The scheduler
		// stays fenced deliberately — its timed jobs must not publish while the source topology
		// is still being reclaimed.
		if rerr := r.restoreFencedWorkload(ctx, p, gatewayWorkloadName); rerr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, rerr)
			return render, true, res, aerr
		}
		if terr := r.transitionMigrationPhase(ctx, p, otilmv1alpha1.MigrationPhaseCleaningUp); terr != nil {
			res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationStateError, terr)
			return render, true, res, aerr
		}
	}

	// The render is handed back UNTOUCHED apart from the two flags: the target version and its
	// bundle, exactly as version resolution produced them. Fencing and Draining report the
	// source because they render the source; the cutover renders the target — from this phase
	// on the platform is being moved onto it with no way back, so reporting anything else would
	// name a version nothing in the namespace is being reconciled against.
	render.holdCore = !stage.rollsCore()
	render.requeue = true
	return render, false, ctrl.Result{}, nil
}

// cutoverStage measures how far the cutover has got, from the cluster.
//
// It is deliberately a MEASUREMENT rather than a counter on status: a stage's completion is an
// observable fact (the topology reports Ready, the fence no longer lists a workload, a rollout
// finished), so a restarted operator arrives at the same answer from the same reads and can
// never skip a stage because a counter was written and the work was not.
//
// A read that FAILS is returned as an error rather than folded into "not done": unlike the
// drain, whose fail-closed answer is a wait it will retry, the cutover has no deadline, so a
// persistent read failure must surface on the platform rather than sit silently in a stage.
func (r *Reconciler) cutoverStage(ctx context.Context, p *otilmv1alpha1.Platform) (cutoverStage, error) {
	if !r.targetTopologyDeclared(ctx, p) {
		return cutoverStageTopology, nil
	}

	// Still fenced means the release has not run yet, whatever the workload's own status says:
	// a fenced provisioning service sits at zero replicas, which a naive readiness check would
	// happily call "rolled out" — the exact reading that wedges the migration.
	if _, fenced := fencedWorkloadFor(p, provisioningWorkloadName); fenced {
		return cutoverStageProvisioning, nil
	}
	rolled, err := r.provisioningRolledOut(ctx, p)
	if err != nil {
		return cutoverStageTopology, err
	}
	if !rolled {
		return cutoverStageProvisioning, nil
	}

	rolled, err = r.coreRolledOut(ctx, p)
	if err != nil {
		return cutoverStageTopology, err
	}
	if !rolled {
		return cutoverStageCore, nil
	}

	return cutoverStageGateway, nil
}

// targetTopologyDeclared reports whether EVERY topology CR the target bundle renders reports
// Ready — the Messaging Topology Operator's signal that it has actually declared the object in
// the broker.
//
// The ordinary messaging gate's readiness (the RabbitmqCluster plus the generated credentials
// Secret) is not enough here. That answers "is there a broker to talk to"; the cutover needs
// "does the topology Core is about to publish to exist", and a Queue or Binding that the
// Topology Operator has accepted but not yet declared would let Core roll onto a virtual host
// that cannot route it.
//
// The RabbitmqCluster itself is skipped: it is the broker, not a topology object, its readiness
// vocabulary is different, and it is the same cluster that has been serving the source topology
// all along.
func (r *Reconciler) targetTopologyDeclared(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	clusterKind := platformbuilder.ManagedMessagingClusterGVK().Kind
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind == clusterKind {
			continue
		}
		if !r.topologyObjectReady(ctx, p.Namespace, gvk, obj.GetName()) {
			// Kind only: a topology object's NAME carries the vhost scope, which is a broker
			// coordinate.
			log.FromContext(ctx).V(1).Info("messaging migration cutover: a target topology object is not declared yet",
				"phase", otilmv1alpha1.MigrationPhaseCuttingOver, "kind", gvk.Kind)
			return false
		}
	}
	return true
}

// topologyObjectReady reports whether ONE topology CR reports the Messaging Topology Operator's
// Ready=True condition. Anything short of an affirmative yes — the object absent, unreadable,
// or carrying no condition yet — is a no, because what follows a yes is rolling the platform
// onto that topology.
func (r *Reconciler) topologyObjectReady(ctx context.Context, namespace string, gvk schema.GroupVersionKind, name string) bool {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &u); err != nil {
		return false
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == conditionTypeReady && cm["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

// provisioningRolledOut reports whether the bundled provisioning service is running the target
// bundle's image and has finished rolling out onto it. A platform that does not render the
// service at all (provisioning.mode=external) has nothing to wait for: somebody else's
// provisioner is somebody else's to restart.
func (r *Reconciler) provisioningRolledOut(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	if !platformbuilder.ProvisioningDeploy(p) {
		return true, nil
	}
	return r.workloadRolledOut(ctx, p.Namespace, provisioningWorkloadName, platformbuilder.ResolveProvisioning(p).Image)
}

// coreRolledOut reports whether Core is Ready ON THE TARGET ROLLOUT.
//
// The distinction is the whole reason stage 3 exists. Core has been Ready throughout the
// migration — on the SOURCE pod template, consuming from the source virtual host — so plain
// readiness would report success before the cutover had moved anything, and the gateway would
// be reopened onto a platform still talking to the topology the cleanup is about to reclaim.
func (r *Reconciler) coreRolledOut(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	return r.workloadRolledOut(ctx, p.Namespace, coreDeploymentName, platformbuilder.ResolveCore(p).Image)
}

// workloadRolledOut reports whether the named workload BOTH carries the given image and has
// finished rolling out onto it, handling either workload kind (a component is rendered as a
// Deployment by default, or a StatefulSet when its workloadType says so). A workload of neither
// kind has not been applied yet, which is not-rolled-out rather than an error.
func (r *Reconciler) workloadRolledOut(ctx context.Context, namespace, name, image string) (bool, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &dep)
	if err == nil {
		return podTemplateRuns(&dep.Spec.Template, image) && rolloutComplete(deploymentRolloutState(&dep)), nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading Deployment %q to check the cutover rollout: %w", name, err)
	}

	var sts appsv1.StatefulSet
	if serr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sts); serr != nil {
		if apierrors.IsNotFound(serr) {
			return false, nil
		}
		return false, fmt.Errorf("reading StatefulSet %q to check the cutover rollout: %w", name, serr)
	}
	return podTemplateRuns(&sts.Spec.Template, image) && rolloutComplete(statefulSetRolloutState(&sts)), nil
}

// podTemplateRuns reports whether a pod template's containers pull the given image — the
// visible marker that the template the workload carries came from the TARGET bundle rather than
// the source one. An empty image answers no: an unresolvable image is not evidence of anything.
func podTemplateRuns(tpl *corev1.PodTemplateSpec, image string) bool {
	if image == "" {
		return false
	}
	for _, c := range tpl.Spec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

// rolloutState is the subset of a workload's identity and status a rollout check reads, so the
// decision itself is one pure function over both workload kinds.
type rolloutState struct {
	// generation / observedGeneration say whether the workload controller has even LOOKED at
	// the template the operator last applied.
	generation, observedGeneration int64
	// desired is .spec.replicas (the apiserver's default of 1 when unset).
	desired int32
	// replicas / updated / available are the workload controller's counts: total pods, pods on
	// the current template, and pods actually serving.
	replicas, updated, available int32
}

// rolloutComplete reports whether a workload has finished rolling onto the template the operator
// applied: the controller has observed that generation, every pod is on the new template, none
// of the old ones are left, and the desired number are serving.
//
// A workload deliberately at ZERO replicas is the case that needs stating. It will never report
// a ready replica, so "ready >= desired" is trivially satisfied and would call an untouched
// workload rolled out — which is exactly the false yes that would let the cutover step over a
// fenced dependency. At zero, the honest answer is the one this returns: the controller has
// observed the current generation (so the template really is in place) and no pod is left
// running from before.
func rolloutComplete(s rolloutState) bool {
	if s.observedGeneration < s.generation {
		return false
	}
	if s.desired == 0 {
		return s.replicas == 0
	}
	return s.updated >= s.desired && s.replicas == s.updated && s.available >= s.desired
}

// deploymentRolloutState projects a Deployment's status onto the shared rollout shape.
func deploymentRolloutState(dep *appsv1.Deployment) rolloutState {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	return rolloutState{
		generation: dep.Generation, observedGeneration: dep.Status.ObservedGeneration,
		desired: desired, replicas: dep.Status.Replicas,
		updated: dep.Status.UpdatedReplicas, available: dep.Status.AvailableReplicas,
	}
}

// statefulSetRolloutState projects a StatefulSet's status onto the shared rollout shape. A
// StatefulSet reports no AvailableReplicas on every supported apiserver, so ReadyReplicas is
// its "serving" count — the same substitution statefulSetReady makes.
func statefulSetRolloutState(sts *appsv1.StatefulSet) rolloutState {
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	return rolloutState{
		generation: sts.Generation, observedGeneration: sts.Status.ObservedGeneration,
		desired: desired, replicas: sts.Status.Replicas,
		updated: sts.Status.UpdatedReplicas, available: sts.Status.ReadyReplicas,
	}
}

// fencedWorkloadFor returns the fence's record for the named workload, and whether the fence
// still holds it. Membership is the authoritative answer to "has this been released": the entry
// is removed only after the recorded replica count has been written back.
func fencedWorkloadFor(p *otilmv1alpha1.Platform, name string) (otilmv1alpha1.FencedWorkload, bool) {
	if p.Status.Upgrade == nil {
		return otilmv1alpha1.FencedWorkload{}, false
	}
	for _, w := range p.Status.Upgrade.Fenced {
		if w.Name == name {
			return w, true
		}
	}
	return otilmv1alpha1.FencedWorkload{}, false
}

// restoreFencedWorkload lifts the fence from ONE named workload, leaving every other fenced
// workload exactly where it is. A workload the fence no longer holds is a no-op, so a stage
// re-entered after a crash repeats nothing.
func (r *Reconciler) restoreFencedWorkload(ctx context.Context, p *otilmv1alpha1.Platform, name string) error {
	w, fenced := fencedWorkloadFor(p, name)
	if !fenced {
		return nil
	}
	return r.restoreWorkload(ctx, p, w)
}

// recordCutoverStage persists the stage the cutover has reached, before the step that stage
// authorises runs.
//
// An UNCHANGED stage writes nothing: the cutover re-measures on every pass and most passes find
// the same answer, so a long rollout costs one status write per stage rather than one per
// reconcile.
func (r *Reconciler) recordCutoverStage(ctx context.Context, p *otilmv1alpha1.Platform, stage cutoverStage) error {
	reason := string(otilmv1alpha1.MigrationPhaseCuttingOver)
	message := migrationCutoverMessage(p.Status.Upgrade, stage)
	c := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	if c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason && c.Message == message {
		return nil
	}
	return r.writeMigrationState(ctx, p, metav1.ConditionTrue, reason, message)
}

// migrationCutoverMessage is the cutover's condition text: the phase message plus the stage it
// is working on. Versions, the phase and the stage wording only — no queue, virtual host or
// broker coordinate.
func migrationCutoverMessage(u *otilmv1alpha1.UpgradeStatus, stage cutoverStage) string {
	return fmt.Sprintf("%s (%s)", migrationPhaseMessage(u), stage)
}
