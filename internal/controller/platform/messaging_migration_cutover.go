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
// THE ORDER IS NOT COSMETIC — IT IS A DEADLOCK. Core's pod starts only when its init containers
// let it: wait-for-auth polls auth, auth-opa-policies, the broker and the SCHEDULER, and on the
// proxy path provision-instance-queue retries against the provisioning API until it answers.
// Several of those are workloads the fence holds at zero replicas — a Service with no endpoints,
// which an nc-poll waits on forever. Roll Core while one of them is fenced and Core can never
// become Ready, so a cutover that waits for Core waits forever, with no deadline (past the drain,
// the migration is forward-only) and no way back. Hence:
//
//  1. TOPOLOGY — every target topology CR must report Ready. Not the broker plus its
//     credentials Secret, which is all the ordinary messaging gate asks: the cutover rolls Core
//     onto these exchanges, queues and bindings, so each one has to exist in the broker first.
//  2. CORE'S DEPENDENCIES — the target render is applied, the fence is lifted from EVERY fenced
//     workload Core's init containers block on (cutoverPreCoreReleases, which the invariant test
//     checks against the builder's own CoreInitServiceDependencies), and each of them must
//     finish rolling out. This is the stage that makes Core's init containers answerable.
//  3. CORE — only now may Core be rolled onto the target bundle, and the cutover waits until it
//     is Ready ON THAT ROLLOUT, never on the source pod template it was already Ready with.
//  4. GATEWAY — the door is reopened and the migration hands over to the cleanup.
//
// RELEASING A PRODUCER AT STAGE 2 DOES NOT PUT IT BACK ON THE SOURCE TOPOLOGY, which is what
// makes stage 2 safe for producers as well as for the provisioner. The ordinary reconcile
// applies the TARGET render to every workload this phase does not hold back — including a fenced
// one, whose pod template is updated even at zero replicas, because the fence holds
// .spec.replicas alone and under its own field manager. A workload released here therefore
// starts pods on the TARGET template and publishes only to the target virtual host, which the
// cleanup never touches.
//
// THAT IS A PRECONDITION, NOT AN ASSUMPTION, so every release CHECKS it. This phase runs ahead
// of the reconcile's own apply and the phases before it applied the SOURCE render, so a fenced
// workload is still on the source template until the first CuttingOver apply lands — and a
// target topology left declared by an abandoned earlier attempt makes the very first pass
// measure stage 2. releaseFencedWorkload therefore reads the live pod template and lifts the
// fence only once it is the target's.
//
// So the invariant is NOT "no producer runs until the source topology has been reclaimed" — it
// never was: stage 4 reopens the gateway to real user traffic, producing real messages, before
// the cleanup runs at all. The invariant is that NO PRODUCER MAY RUN ON THE SOURCE TEMPLATE ONCE
// THE DRAIN HAS COMPLETED, and releasing only after the target render has been applied is
// exactly what enforces it.
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

// cutoverPreCoreReleases names the fenced workloads the cutover releases at stage 2, before it
// is allowed to roll Core: every one of them is a workload Core's init containers block on, so
// leaving any of them at zero replicas past this stage is the deadlock described at the top of
// this file.
//
// It is the ONE place that decides which producers come back up early, and everything else in
// the phase is derived from it: the restore, the stage measurement, and the invariant test that
// checks it against the builder's CoreInitServiceDependencies.
var cutoverPreCoreReleases = []string{provisioningWorkloadName, schedulerWorkloadName}

// cutoverStage is how far the CuttingOver phase has got. The stages are ordered, and each one
// names the work that is still OUTSTANDING — so cutoverStageCore means "everything Core depends
// on is in place, roll it", not "Core is done".
type cutoverStage int

const (
	// cutoverStageTopology: the target topology is not fully declared in the broker yet.
	cutoverStageTopology cutoverStage = iota
	// cutoverStageProvisioning: the topology is declared; the workloads Core's init containers
	// block on still have to be released from the fence and finish rolling out onto the target
	// bundle.
	cutoverStageProvisioning
	// cutoverStageCore: every service Core starts behind answers, so Core may be rolled onto the
	// target bundle; the cutover waits until that rollout is complete.
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
		return "restoring the services core depends on onto the target topology"
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
		// Releasing these is the stage's own step: they are fenced workloads, so nothing else
		// will ever scale them back up, and Core's init containers cannot finish without them.
		// The target render has already been applied to each — including while it was fenced —
		// so what comes back up publishes to the target virtual host, never the source one.
		for _, name := range cutoverPreCoreReleases {
			if rerr := r.releaseFencedWorkload(ctx, p, name); rerr != nil {
				res, aerr := r.applyOrDegrade(ctx, p, reasonMigrationFenceError, rerr)
				return render, true, res, aerr
			}
		}
	}

	if stage == cutoverStageGateway {
		// The gateway is restored BEFORE the phase moves on, so the cleanup can never inherit a
		// platform whose door is still shut with nothing left that would open it. It is the last
		// workload the fence holds: everything Core starts behind came back at stage 2.
		if rerr := r.releaseFencedWorkload(ctx, p, gatewayWorkloadName); rerr != nil {
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
	declared, err := r.targetTopologyDeclared(ctx, p)
	if err != nil {
		return cutoverStageTopology, err
	}
	if !declared {
		return cutoverStageTopology, nil
	}

	// Still fenced means the release has not run yet, whatever the workload's own status says:
	// a fenced workload sits at zero replicas, which a naive readiness check would happily call
	// "rolled out" — the exact reading that wedges the migration.
	for _, name := range cutoverPreCoreReleases {
		if _, fenced := fencedWorkloadFor(p, name); fenced {
			return cutoverStageProvisioning, nil
		}
	}
	rolled, err := r.provisioningRolledOut(ctx, p)
	if err != nil {
		return cutoverStageTopology, err
	}
	if !rolled {
		return cutoverStageProvisioning, nil
	}
	rolled, err = r.schedulerRolledOut(ctx, p)
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

// migrationTargetVersion is the version the cutover is moving the platform ONTO, read from the
// migration record rather than from spec.version: the record is what every other part of the
// phase is measured against, and the spec is a live field.
func migrationTargetVersion(p *otilmv1alpha1.Platform) string {
	if p.Status.Upgrade == nil {
		return ""
	}
	return p.Status.Upgrade.ToVersion
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
func (r *Reconciler) targetTopologyDeclared(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	clusterKind := platformbuilder.ManagedMessagingClusterGVK().Kind
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind == clusterKind {
			continue
		}
		ready, err := r.topologyObjectDeclared(ctx, p.Namespace, gvk, obj.GetName())
		if err != nil {
			return false, err
		}
		if !ready {
			// Kind only: a topology object's NAME carries the vhost scope, which is a broker
			// coordinate.
			log.FromContext(ctx).V(1).Info("messaging migration cutover: a target topology object is not declared yet",
				"phase", otilmv1alpha1.MigrationPhaseCuttingOver, "kind", gvk.Kind)
			return false, nil
		}
	}
	return true, nil
}

// topologyObjectDeclared reports whether ONE topology CR is declared in the broker FOR ITS
// CURRENT SPEC — the question the cutover has to ask before it rolls the platform onto that
// topology, and a stricter one than the ordinary messaging gate's.
//
// The extra requirement is the GENERATION. A migration retains objects whose identities the two
// versions share, and a retained object carries the Ready its PREVIOUS spec earned until the
// Topology Operator has reconciled the new one — a yes that would authorise the cutover to
// publish to a topology the broker has not been told about.
func (r *Reconciler) topologyObjectDeclared(ctx context.Context, namespace string, gvk schema.GroupVersionKind, name string) (bool, error) {
	state, err := r.topologyObjectState(ctx, namespace, gvk, name)
	if err != nil {
		return false, err
	}
	return state.found && state.current && state.ready, nil
}

// topologyObjectState is what one topology CR says about itself: whether it exists, whether the
// Messaging Topology Operator has reconciled the spec it currently carries, and whether that
// reconcile succeeded.
type topologyObjectState struct {
	found, current, ready bool
}

// topologyObjectState reads one topology CR's self-report.
//
// A FAILURE IS AN ERROR, not a "no". Folding an unreadable object into "not ready" turns every
// apiserver blip and every unexpected status shape into an indefinite, silent wait — which the
// cutover, a phase with no deadline behind it, could sit in forever. Callers that DO have a
// retry story of their own (the ordinary messaging gate re-checks on its own cadence) are free
// to treat the error as a no; the cutover surfaces it. Only a genuine absence — including a Kind
// the cluster does not serve, which is positive proof — is a plain, error-free no.
func (r *Reconciler) topologyObjectState(ctx context.Context, namespace string, gvk schema.GroupVersionKind, name string) (topologyObjectState, error) {
	var state topologyObjectState
	var u unstructured.Unstructured
	u.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &u); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return state, nil
		}
		// Kind and API reason only: the object's NAME carries the vhost scope, and so does the
		// apiserver's own error text.
		return state, safeErrorf(err, "reading a messaging topology %s to check it is declared failed (%s)",
			gvk.Kind, apiFailureReason(err))
	}
	state.found = true

	observed, observedFound, oerr := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	if oerr != nil {
		return state, safeErrorf(oerr, "reading a messaging topology %s's observed generation failed", gvk.Kind)
	}
	state.current = observedFound && observed >= u.GetGeneration()

	conds, found, cerr := unstructured.NestedSlice(u.Object, "status", "conditions")
	if cerr != nil {
		return state, safeErrorf(cerr, "reading a messaging topology %s's conditions failed", gvk.Kind)
	}
	if !found {
		return state, nil
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			return state, fmt.Errorf("a messaging topology %s reports a condition this operator cannot read", gvk.Kind)
		}
		if cm["type"] == conditionTypeReady && cm["status"] == string(metav1.ConditionTrue) {
			state.ready = true
			return state, nil
		}
	}
	return state, nil
}

// provisioningRolledOut reports whether the bundled provisioning service is running the TARGET
// version's pod template and has finished rolling out onto it. A platform that does not render
// the service at all (provisioning.mode=external) has nothing to wait for: somebody else's
// provisioner is somebody else's to restart.
func (r *Reconciler) provisioningRolledOut(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	if !platformbuilder.ProvisioningDeploy(p) {
		return true, nil
	}
	return r.workloadRolledOut(ctx, p.Namespace, provisioningWorkloadName,
		platformbuilder.ResolveProvisioning(p).Image, migrationTargetVersion(p))
}

// schedulerRolledOut reports whether the scheduler is running the TARGET version's pod template
// and has finished rolling out onto it.
//
// It is the second half of stage 2, symmetrical with provisioningRolledOut, and for the same
// reason: Core's wait-for-auth init container polls the scheduler's Service, so a scheduler that
// has been released but is not serving yet still holds Core's pod in its init phase. There is no
// mode gate to make here — the scheduler is always rendered, and MigrationFenceTargets lists it
// unconditionally.
func (r *Reconciler) schedulerRolledOut(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	return r.workloadRolledOut(ctx, p.Namespace, schedulerWorkloadName,
		platformbuilder.ResolveScheduler(p).Image, migrationTargetVersion(p))
}

// coreRolledOut reports whether Core is Ready ON THE TARGET ROLLOUT.
//
// The distinction is the whole reason stage 3 exists. Core has been Ready throughout the
// migration — on the SOURCE pod template, consuming from the source virtual host — so plain
// readiness would report success before the cutover had moved anything, and the gateway would
// be reopened onto a platform still talking to the topology the cleanup is about to reclaim.
func (r *Reconciler) coreRolledOut(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	return r.workloadRolledOut(ctx, p.Namespace, coreDeploymentName,
		platformbuilder.ResolveCore(p).Image, migrationTargetVersion(p))
}

// workloadRolledOut reports whether the named workload carries the TARGET VERSION's pod
// template — proved by the version annotation the render stamps AND by the expected image —
// and has finished rolling out onto it. A workload that has not been applied yet is
// not-rolled-out rather than an error.
//
// A WORKLOAD CONFIGURED FOR ZERO REPLICAS IS REFUSED HERE, not answered. rolloutComplete calls
// a zero-replica workload rolled out, which is the right reading where the zero is the FENCE's
// own — but this check is the cutover measuring progress toward SERVING, and there the zero is
// the platform's own configuration: a Service that will never have an endpoint, an init
// container that can never finish polling it, and a phase with no deadline waiting on the
// result. Fenced workloads never reach this check: cutoverStage answers from fence membership
// before it measures anything.
func (r *Reconciler) workloadRolledOut(ctx context.Context, namespace, name, image, version string) (bool, error) {
	live, err := r.readWorkload(ctx, namespace, name)
	if err != nil || !live.found {
		return false, err
	}
	if live.state.desired == 0 {
		return false, workloadScaledToZero(name)
	}
	return podTemplateIsTarget(live.template, image, version) && rolloutComplete(live.state), nil
}

// workloadScaledToZero is what the cutover says about a component it needs SERVING that the
// platform configures for no pods at all. It names the workload and the single action that
// unblocks the migration — workload names and replica counts only, never a coordinate — and it
// is deliberately a stop rather than a wait: the engine cannot know when, or whether, the
// operator means to scale the component up.
func workloadScaledToZero(name string) error {
	return fmt.Errorf("the messaging migration's cutover needs workload %q to be serving before it can continue, but it is "+
		"configured for 0 replicas, so it never will; scale that component to at least 1 replica and the migration resumes "+
		"on its own", name)
}

// liveWorkload is what one read tells the cutover about a workload: the pod template the
// operator's apply left on it, and the counts its controller reports. found=false means the
// workload has not been applied yet — neither an error nor evidence of anything.
type liveWorkload struct {
	template *corev1.PodTemplateSpec
	state    rolloutState
	found    bool
}

// readWorkload reads one workload of EITHER kind (a component is rendered as a Deployment by
// default, or a StatefulSet when its workloadType says so) and projects it onto the single shape
// this phase measures. Both questions the cutover asks of a live workload — is the target
// template on it, has it finished rolling onto it — are answered from this one read, so neither
// can drift into handling the two kinds differently.
func (r *Reconciler) readWorkload(ctx context.Context, namespace, name string) (liveWorkload, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &dep)
	if err == nil {
		return liveWorkload{template: &dep.Spec.Template, state: deploymentRolloutState(&dep), found: true}, nil
	}
	if !apierrors.IsNotFound(err) {
		return liveWorkload{}, fmt.Errorf("reading Deployment %q for the cutover: %w", name, err)
	}

	var sts appsv1.StatefulSet
	if serr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sts); serr != nil {
		if apierrors.IsNotFound(serr) {
			return liveWorkload{}, nil
		}
		return liveWorkload{}, fmt.Errorf("reading StatefulSet %q for the cutover: %w", name, serr)
	}
	return liveWorkload{template: &sts.Spec.Template, state: statefulSetRolloutState(&sts), found: true}, nil
}

// podTemplateIsTarget reports whether a pod template is the one the TARGET version renders.
//
// THE IMAGE ALONE CANNOT ANSWER THIS. Neighbouring bundles legitimately pin a component to the
// same image — provisioning-rabbitmq:1.0.0 is identical in 2.18.0 and 2.19.0 — so a template
// rendered entirely from the SOURCE bundle, still configured for the source virtual host,
// satisfies an image check on the target. The cutover acts on that answer by releasing Core,
// which then rolls onto a topology its provisioner has not moved to. So the version the render
// STAMPED on the template (PlatformVersionAnnotation) is the load-bearing half, and the image
// is kept as the corroborating one. An empty image or version answers no: neither absence is
// evidence of anything.
func podTemplateIsTarget(tpl *corev1.PodTemplateSpec, image, version string) bool {
	if version == "" || tpl.Annotations[platformbuilder.PlatformVersionAnnotation] != version {
		return false
	}
	return podTemplateRuns(tpl, image)
}

// podTemplateRuns reports whether a pod template's containers pull the given image. An empty
// image answers no: an unresolvable image is not evidence of anything.
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

// releaseFencedWorkload lifts the fence from ONE named workload, leaving every other fenced
// workload exactly where it is — but only once the TARGET render has actually reached that
// workload's pod template. A workload the fence no longer holds is a no-op, so a stage
// re-entered after a crash repeats nothing.
//
// THE TEMPLATE CHECK IS THE POINT. This phase runs BEFORE the reconcile's own Server-Side Apply,
// and every phase before it applied the SOURCE render, so a fenced workload carries the source
// pod template until the first CuttingOver apply lands. Normally that first pass measures stage
// 1 and the question never arises — but a target topology left declared by an abandoned earlier
// attempt makes stage 2 the very first measurement, and a producer released there would start
// pods on the SOURCE template and publish into the virtual host the drain has just emptied and
// the cleanup is about to delete.
//
// So the live template is compared against the target's, on exactly the evidence the rollout
// checks use, and a workload that is not on it yet stays fenced for this pass. Nothing is
// counted or recorded: the base apply later in this same reconcile renders the target, and the
// next pass re-measures the same stage and releases it.
//
// A workload that does not EXIST is released: there is no pod to start on the wrong template,
// and the render creates it from the target bundle at the count the spec asks for.
func (r *Reconciler) releaseFencedWorkload(ctx context.Context, p *otilmv1alpha1.Platform, name string) error {
	w, fenced := fencedWorkloadFor(p, name)
	if !fenced {
		return nil
	}

	image, err := cutoverReleaseImage(p, name)
	if err != nil {
		return err
	}
	live, err := r.readWorkload(ctx, p.Namespace, name)
	if err != nil {
		return err
	}
	if live.found && !podTemplateIsTarget(live.template, image, migrationTargetVersion(p)) {
		// Workload name and phase only: neither is a broker coordinate.
		log.FromContext(ctx).V(1).Info("messaging migration cutover: a fenced workload stays held until the target render reaches it",
			"phase", otilmv1alpha1.MigrationPhaseCuttingOver, "workload", name)
		return nil
	}
	return r.restoreWorkload(ctx, p, w)
}

// cutoverReleaseImage resolves the image the render pins for a workload the cutover releases —
// the corroborating half of "the target render has reached this workload".
//
// An unknown name is an ERROR rather than an empty answer. An empty image can never match a
// template, so a workload nobody taught this function about would be one the cutover held fenced
// forever, silently; adding a name to cutoverPreCoreReleases has to be paired with a case here.
func cutoverReleaseImage(p *otilmv1alpha1.Platform, name string) (string, error) {
	switch name {
	case provisioningWorkloadName:
		return platformbuilder.ResolveProvisioning(p).Image, nil
	case schedulerWorkloadName:
		return platformbuilder.ResolveScheduler(p).Image, nil
	case gatewayWorkloadName:
		return platformbuilder.ResolveGateway(p).Image, nil
	default:
		return "", fmt.Errorf("the messaging migration's cutover cannot resolve the rendered image for workload %q, "+
			"so it cannot tell whether the target render has reached it", name)
	}
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
