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

// The staged cutover's contract is an ORDER, and the order is only observable when every input
// it reads is controllable: a topology that does or does not report Ready, a fence that does or
// does not still hold the provisioning service, a rollout that has or has not finished. A fake
// client gives all three exactly, which envtest cannot — it serves no rabbitmq.com CRDs and
// runs no workload controller.
//
// The claim under test is one-directional, exactly as the drain's is: nothing may let Core roll
// before the topology is declared AND the provisioning service is back up, because Core's
// proxy-path init container blocks on that service and a Core that can never become Ready is a
// migration that can never finish.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
)

// provisioningBootstrapSecret is the Secret the deployed provisioning service reads its API key
// and JWT signing key from. Only its NAME matters here (the spec requires one).
const provisioningBootstrapSecret = "provisioning-bootstrap" //nolint:gosec // G101: a Secret name, not a credential

// cutoverScheme is the reconcile scheme plus the rabbitmq.com kinds registered as unstructured,
// so the fake client can store and GET the topology CRs the cutover gates on.
func cutoverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := reconcileScheme(t)
	gv := platformbuilder.ManagedMessagingClusterGVK().GroupVersion()
	for _, kind := range []string{"RabbitmqCluster", "Vhost", "User", "Permission", "Exchange", "Queue", "Binding"} {
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// cutoverPlatform is a managed-messaging platform mid-CUTOVER: running 2.18.0, spec asking for
// 2.19.0, the bundled provisioning service rendered, and the given workloads fenced. The image
// registry is defaulted onto the spec so the images the builders resolve here are the ones the
// reconciler resolves in production (Reconcile defaults it on its own fetched copy).
func cutoverPlatform(fenced ...otilmv1alpha1.FencedWorkload) *otilmv1alpha1.Platform {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseCuttingOver, fenced...)
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		Mode:   "deploy",
		Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: provisioningBootstrapSecret},
	}
	platformbuilder.DefaultImageRegistry(p)
	return p
}

// cutoverReconciler builds a reconciler over a fake client holding the Platform (with the status
// subresource, as the real apiserver has it) plus the given objects.
func cutoverReconciler(t *testing.T, p *otilmv1alpha1.Platform, funcs interceptor.Funcs, objs ...client.Object) (*Reconciler, *record.FakeRecorder) {
	t.Helper()
	s := cutoverScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(append([]client.Object{p}, objs...)...).WithInterceptorFuncs(funcs).Build()
	rec := record.NewFakeRecorder(32)
	return &Reconciler{Client: c, Scheme: s, Recorder: rec}, rec
}

// cutoverPass runs one whole gate pass from a FRESHLY READ Platform, as Reconcile does.
func cutoverPass(t *testing.T, r *Reconciler) (migrationRender, bool, ctrl.Result, error) {
	t.Helper()
	_, to := migrationBundles(t)
	return r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
}

// declaredTargetTopology returns the TARGET bundle's topology CRs, each carrying the Messaging
// Topology Operator's Ready condition — the broker having accepted and declared every one of
// them, which is what stage 1 waits for. The RabbitmqCluster is left out: it is the broker, not
// a topology object, and the cutover does not gate on it.
func declaredTargetTopology(p *otilmv1alpha1.Platform) []client.Object {
	clusterKind := platformbuilder.ManagedMessagingClusterGVK().Kind
	var out []client.Object
	for _, obj := range platformbuilder.ResolveManagedMessaging(p) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() == clusterKind {
			continue
		}
		declared := u.DeepCopy()
		_ = unstructured.SetNestedSlice(declared.Object, []interface{}{
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionTrue)},
		}, "status", "conditions")
		out = append(out, declared)
	}
	return out
}

// cutoverWorkload is a workload carrying the given image, at the given desired replica count,
// with the rollout status the caller asks for: rolled=true reports the workload controller
// finished rolling every pod onto the current template.
func cutoverWorkload(name, image string, replicas int32, rolled bool) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: migrationTestNS, Generation: 4},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 4},
	}
	if rolled {
		dep.Status.Replicas = replicas
		dep.Status.UpdatedReplicas = replicas
		dep.Status.AvailableReplicas = replicas
	}
	return dep
}

// coreImageFor resolves the image a given platform VERSION renders for Core — the visible
// difference between a Core still on the source bundle and one that has been cut over.
func coreImageFor(p *otilmv1alpha1.Platform, version string) string {
	render := p.DeepCopy()
	render.Spec.Version = version
	return platformbuilder.ResolveCore(render).Image
}

// provisioningImageOf resolves the image the target bundle renders for the provisioning service.
func provisioningImageOf(p *otilmv1alpha1.Platform) string {
	return platformbuilder.ResolveProvisioning(p).Image
}

// fencedProvisioning / fencedGateway / fencedScheduler are the fence records the drain hands the
// cutover: the three producers, at the counts restore must write back.
func fencedProvisioning() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: provisioningWorkloadName, Kind: "Deployment", Replicas: 2}
}

func fencedGateway() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: "Deployment", Replicas: 3}
}

func fencedScheduler() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 1}
}

// --- the pure predicates -----------------------------------------------------

// TestRolloutCompleteMatrix pins the rollout predicate, including the case the whole staged
// cutover turns on: a workload at ZERO replicas has no ready replica to measure, so the naive
// "ready >= desired" reads 0/0 as success — which would let the cutover step straight over a
// dependency the fence is still holding down.
func TestRolloutCompleteMatrix(t *testing.T) {
	tests := []struct {
		name  string
		state rolloutState
		want  bool
	}{
		{
			name:  "the controller has not observed the applied template yet",
			state: rolloutState{generation: 5, observedGeneration: 4, desired: 2, replicas: 2, updated: 2, available: 2},
		},
		{
			name:  "old pods are still around",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 2, replicas: 3, updated: 2, available: 2},
		},
		{
			name:  "not every pod is on the new template",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 2, replicas: 1, updated: 1, available: 1},
		},
		{
			name:  "the new pods are not serving yet",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 2, replicas: 2, updated: 2, available: 1},
		},
		{
			name:  "fully rolled out",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 2, replicas: 2, updated: 2, available: 2},
			want:  true,
		},
		{
			name:  "at zero replicas with pods still winding down",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 0, replicas: 1},
		},
		{
			name:  "at zero replicas, generation not observed — 0/0 is NOT readiness",
			state: rolloutState{generation: 5, observedGeneration: 4, desired: 0, replicas: 0},
		},
		{
			name:  "deliberately at zero, template in place and no pod left",
			state: rolloutState{generation: 5, observedGeneration: 5, desired: 0, replicas: 0},
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rolloutComplete(tc.state))
		})
	}
}

// TestPodTemplateRuns proves the target-revision marker: the template has to actually pull the
// image the target bundle renders, and an unresolvable image is never evidence of anything.
func TestPodTemplateRuns(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "opa", Image: "opa:1"}, {Name: "core", Image: "core:2.19.0"},
	}}}
	assert.True(t, podTemplateRuns(tpl, "core:2.19.0"))
	assert.False(t, podTemplateRuns(tpl, "core:2.18.0"))
	assert.False(t, podTemplateRuns(tpl, ""), "an empty image can never be matched")
}

// TestRolloutStateProjection proves both workload kinds project onto the same shape, with an
// unset .spec.replicas reading as the apiserver's default of one.
func TestRolloutStateProjection(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	assert.Equal(t, rolloutState{generation: 2, observedGeneration: 2, desired: 1, replicas: 1, updated: 1, available: 1},
		deploymentRolloutState(dep))

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr(int32(2))},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2},
	}
	assert.Equal(t, rolloutState{generation: 3, observedGeneration: 3, desired: 2, replicas: 2, updated: 2, available: 2},
		statefulSetRolloutState(sts))
}

// TestFencedWorkloadFor proves membership is read from the record alone.
func TestFencedWorkloadFor(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	w, fenced := fencedWorkloadFor(p, gatewayWorkloadName)
	assert.True(t, fenced)
	assert.Equal(t, int32(3), w.Replicas, "the record carries the count restore must write back")

	_, fenced = fencedWorkloadFor(p, provisioningWorkloadName)
	assert.False(t, fenced, "a workload the fence never listed is not held")

	_, fenced = fencedWorkloadFor(&otilmv1alpha1.Platform{}, gatewayWorkloadName)
	assert.False(t, fenced, "no migration means no fence")
}

// TestCutoverStageOrder pins the one thing the stage ordering exists to express: Core may not be
// rolled until the topology and the provisioning service are both behind it.
func TestCutoverStageOrder(t *testing.T) {
	assert.False(t, cutoverStageTopology.rollsCore(), "nothing may roll onto a topology that is not declared")
	assert.False(t, cutoverStageProvisioning.rollsCore(), "Core's init container blocks on the provisioning service")
	assert.True(t, cutoverStageCore.rollsCore())
	assert.True(t, cutoverStageGateway.rollsCore())

	for _, s := range []cutoverStage{cutoverStageTopology, cutoverStageProvisioning, cutoverStageCore, cutoverStageGateway} {
		assert.NotEmpty(t, s.String())
		assertNoBrokerCoordinates(t, migrationCutoverMessage(cutoverPlatform().Status.Upgrade, s))
	}
}

// TestCutoverWorkloadNamesAreTheFenceTargets keeps the names the cutover addresses by hand in
// step with the ones the fence actually records: a builder-side rename would otherwise leave the
// cutover restoring a workload that does not exist, silently.
func TestCutoverWorkloadNamesAreTheFenceTargets(t *testing.T) {
	var names []string
	for _, target := range platformbuilder.MigrationFenceTargets(cutoverPlatform()) {
		names = append(names, target.Name)
	}
	assert.Contains(t, names, gatewayWorkloadName)
	assert.Contains(t, names, provisioningWorkloadName)
	assert.NotContains(t, names, coreDeploymentName, "Core is the consumer, never a fence target")
}

// --- the staged sequence -----------------------------------------------------

// TestCutoverHoldsCoreUntilTheTargetTopologyIsDeclared is stage 1: with the target topology not
// yet declared in the broker, nothing moves — Core is withheld from the apply, the fence is left
// whole, and the platform goes on reporting the version it is still serving.
func TestCutoverHoldsCoreUntilTheTargetTopologyIsDeclared(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{})

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled, "the reconcile has to continue — applying the target topology is what stage 1 waits for")
	assert.True(t, render.holdCore, "Core must not roll onto a topology the broker has not declared")
	assert.True(t, render.requeue)
	core, known := render.bundle.Lookup("core")
	require.True(t, known)
	assert.Equal(t, platformVersion219, core.Tag,
		"the TARGET bundle is what renders — the source re-pin the waiting phases apply must NOT reach the cutover")
	assert.Equal(t, platformVersion219, render.version,
		"the cutover renders the target, so that is what the platform reports it is reconciled against")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase)
	assert.Len(t, stored.Status.Upgrade.Fenced, 3, "no producer is released before the topology exists")
	assert.Contains(t, migrationCondition(stored).Message, cutoverStageTopology.String())
	assertNoBrokerCoordinates(t, migrationCondition(stored).Message)
}

// TestCutoverHoldsCoreWhileProvisioningIsStillFenced is THE deadlock guard.
//
// The topology is fully declared, so stage 1 is behind us — but the provisioning service is
// still on the fence's list, sitting at zero replicas with the target template already applied
// to it. That workload reports 0/0 ready, which is precisely the shape a naive readiness check
// calls "ready". If the cutover believed it, it would roll Core, whose proxy-path init container
// retries against the provisioning API forever, and the migration — which is past the point of
// no return and has no deadline — would never finish.
//
// So: Core is withheld, and the pass's own job is to RELEASE the provisioning service.
func TestCutoverHoldsCoreWhileProvisioningIsStillFenced(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		// The trap: fenced at zero, already carrying the target template.
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 0, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), 1, true),
		cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.True(t, render.holdCore,
		"a provisioning service the fence still holds can never answer Core's init container")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"the cutover cannot hand over while a dependency is still fenced")
	assert.Equal(t, int32(2), replicasOf(t, r, provisioningWorkloadName),
		"the pass releases the provisioning service at the count the fence recorded")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()}, stored.Status.Upgrade.Fenced,
		"only the provisioning service is released; the door stays shut and the scheduler stays stopped")
	assert.Equal(t, int32(0), replicasOf(t, r, gatewayWorkloadName), "the gateway is not reopened at this stage")
	assert.Contains(t, migrationCondition(stored).Message, cutoverStageProvisioning.String())
}

// TestCutoverHoldsCoreUntilProvisioningHasRolledOut is the second half of stage 2: released from
// the fence is not the same as answering. Until the service has finished rolling onto the target
// template, Core stays where it is.
func TestCutoverHoldsCoreUntilProvisioningHasRolledOut(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, false),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), 1, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, _, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.True(t, render.holdCore, "the service is up but not serving yet")
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPlatform(t, r).Status.Upgrade.Phase)
}

// TestCutoverRollsCoreOnceProvisioningAnswers is stage 3: with the topology declared and the
// provisioning service serving, Core is finally allowed to roll — but the cutover does NOT hand
// over yet, because Core is still Ready on the SOURCE pod template it never left.
func TestCutoverRollsCoreOnceProvisioningAnswers(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), 1, true),
		cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.False(t, render.holdCore, "everything Core's pod needs to become Ready is in place")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"Ready on the SOURCE template is not Ready on the target rollout")
	assert.Equal(t, int32(0), replicasOf(t, r, gatewayWorkloadName), "the door stays shut until Core is on the target")
	assert.Contains(t, migrationCondition(stored).Message, cutoverStageCore.String())
}

// TestCutoverReopensTheGatewayAndHandsOver is stage 4: Core is Ready ON THE TARGET ROLLOUT, so
// the door is reopened at the count the fence recorded and the migration moves to the cleanup —
// with the scheduler, and only the scheduler, still stopped.
func TestCutoverReopensTheGatewayAndHandsOver(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), 1, true),
		cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
		cutoverWorkload("scheduler", "scheduler:1.1.1", 0, true),
	)
	r, rec := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.False(t, render.holdCore)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, stored.Status.Upgrade.Phase)
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedScheduler()}, stored.Status.Upgrade.Fenced,
		"the cleanup inherits a platform that is open, with only the scheduler's timed jobs held back")
	assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName), "the gateway is reopened at its recorded count")
	assert.Equal(t, int32(0), replicasOf(t, r, "scheduler"))

	var handOver string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, eventMigrationPhase) && strings.Contains(e, string(otilmv1alpha1.MigrationPhaseCleaningUp)) {
			handOver = e
		}
	}
	require.NotEmpty(t, handOver, "the hand-over is announced")
	assertNoBrokerCoordinates(t, handOver)
}

// TestCutoverResumesAtEveryStageBoundary is the crash contract. Each stage is re-entered from
// the state the cluster holds — never from a counter — so a pass repeated after a restart
// re-measures the same stage and repeats nothing: no second restore, no second hand-over.
func TestCutoverResumesAtEveryStageBoundary(t *testing.T) {
	tests := []struct {
		name       string
		fenced     []otilmv1alpha1.FencedWorkload
		coreImage  string
		provRolled bool
		wantPhase  otilmv1alpha1.MigrationPhase
		wantFenced []otilmv1alpha1.FencedWorkload
		wantHold   bool
		// wantHandedOver marks the row that has passed the cutover altogether: the repeated
		// passes run the CLEANUP, which finds nothing of the source topology left in this fixture
		// and finishes the migration. What the row still proves is the same thing — the repeats
		// neither re-open a workload at a stale count nor hand over a second time.
		wantHandedOver bool
	}{
		{
			name:       "re-entering the provisioning stage does not restore twice",
			fenced:     []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler(), fencedProvisioning()},
			coreImage:  platformVersion218,
			provRolled: false,
			wantPhase:  otilmv1alpha1.MigrationPhaseCuttingOver,
			wantFenced: []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()},
			wantHold:   true,
		},
		{
			name:       "re-entering the core stage leaves the fence alone",
			fenced:     []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()},
			coreImage:  platformVersion218,
			provRolled: true,
			wantPhase:  otilmv1alpha1.MigrationPhaseCuttingOver,
			wantFenced: []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()},
		},
		{
			name:           "re-entering past the hand-over does not re-open or hand over twice",
			fenced:         []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()},
			coreImage:      platformVersion219,
			provRolled:     true,
			wantHandedOver: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cutoverPlatform(tc.fenced...)
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, tc.provRolled),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, tc.coreImage), 1, true),
				cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
			)
			r, rec := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

			// Three passes on a reconciler that carries nothing from the one before: the
			// operator restarting, over and over, at the same stage boundary.
			var render migrationRender
			for i := 0; i < 3; i++ {
				var err error
				render, _, _, err = cutoverPass(t, r)
				require.NoError(t, err)
			}

			stored := storedPlatform(t, r)
			if tc.wantHandedOver {
				assert.Nil(t, stored.Status.Upgrade, "the hand-over led on to a cleanup with nothing to reclaim")
				assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName),
					"the door was reopened once, at the count the fence recorded")
				handOvers := 0
				for _, e := range drainEvents(rec) {
					if strings.Contains(e, eventMigrationPhase) {
						handOvers++
					}
				}
				assert.Equal(t, 1, handOvers, "a repeated pass announces no second hand-over")
			} else {
				assert.Equal(t, tc.wantPhase, stored.Status.Upgrade.Phase)
				assert.Equal(t, tc.wantFenced, stored.Status.Upgrade.Fenced)
			}
			assert.Equal(t, tc.wantHold, render.holdCore)
			// Whatever the fence wrote back stays written back: a repeat must not re-record a
			// restored workload, nor re-patch it to a stale count.
			assert.Equal(t, int32(2), replicasOf(t, r, provisioningWorkloadName))
		})
	}
}

// --- the failure paths -------------------------------------------------------

// TestCutoverStopsWhenItCannotReadTheRolloutState proves an unreadable cluster stops the pass
// rather than being folded into "not done yet". Past the drain the migration is forward-only and
// has no deadline, so a read failure that sat silently in a stage would be invisible forever.
func TestCutoverStopsWhenItCannotReadTheRolloutState(t *testing.T) {
	for _, unreadable := range []string{provisioningWorkloadName, coreDeploymentName} {
		t.Run(unreadable, func(t *testing.T) {
			p := cutoverPlatform(fencedGateway(), fencedScheduler())
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, true),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), 1, true),
			)
			r, _ := cutoverReconciler(t, p, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == unreadable {
						return errors.New("the apiserver did not answer")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}, seed...)

			_, handled, _, err := cutoverPass(t, r)
			assert.True(t, handled, "the pass short-circuits rather than guessing")
			require.Error(t, err)

			stored := storedPlatform(t, r)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase)
			assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()},
				stored.Status.Upgrade.Fenced, "an unanswered read releases nothing")
		})
	}
}

// TestCutoverStopsWhenTheHandOverCannotBePersisted covers the last write of the phase: the door
// is already open (the restore's own de-listing landed) but the hand-over does not, so the
// migration stays in CuttingOver and the next pass — which re-measures the same stage and finds
// the gateway already restored — repeats only the write that failed.
func TestCutoverStopsWhenTheHandOverCannotBePersisted(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), 1, true),
		cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
	)
	// Writes in order: the stage record, the restore's de-listing, the hand-over. Only the last
	// one is refused.
	writes := 0
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			if writes >= 3 {
				return errors.New("status write refused")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, seed...)

	_, handled, _, err := cutoverPass(t, r)
	assert.True(t, handled)
	require.Error(t, err)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"the cleanup may not inherit a phase the cluster never recorded")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedScheduler()}, stored.Status.Upgrade.Fenced,
		"the restore that DID land stays landed — re-entry repeats nothing")
	assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName))
}

// TestWorkloadRolledOutHandlesBothKinds proves the rollout check follows a component rendered as
// a StatefulSet (spec.<component>.workloadType) exactly as it follows a Deployment, and reads a
// workload of neither kind as not-rolled-out rather than as a failure.
func TestWorkloadRolledOutHandlesBothKinds(t *testing.T) {
	const image = "kong:3.9.1"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayWorkloadName, Namespace: migrationTestNS, Generation: 2},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kong", Image: image}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
	r, _ := cutoverReconciler(t, cutoverPlatform(), interceptor.Funcs{}, sts)
	ctx := context.Background()

	rolled, err := r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, image)
	require.NoError(t, err)
	assert.True(t, rolled, "a StatefulSet-typed component rolls out like a Deployment-typed one")

	rolled, err = r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, "kong:3.8.0")
	require.NoError(t, err)
	assert.False(t, rolled, "the target image is what makes it the target rollout")

	rolled, err = r.workloadRolledOut(ctx, migrationTestNS, "nothing-of-this-name", image)
	require.NoError(t, err)
	assert.False(t, rolled, "a workload that has not been applied yet is not an error")
}

// TestTopologyObjectReadyFailsClosed pins the one-directional answer stage 1 depends on: only an
// explicit Ready=True is a yes. Everything else — absent, no status, a condition list the
// operator cannot parse, an explicit False — holds the cutover.
func TestTopologyObjectReadyFailsClosed(t *testing.T) {
	gvk := platformbuilder.ManagedMessagingVhostGVK()
	topologyObject := func(name string, conditions []interface{}) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetNamespace(migrationTestNS)
		if conditions != nil {
			_ = unstructured.SetNestedSlice(u.Object, conditions, "status", "conditions")
		}
		return u
	}
	seed := []client.Object{
		topologyObject("no-status", nil),
		topologyObject("unparseable", []interface{}{"not-a-condition"}),
		topologyObject("not-ready", []interface{}{
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionFalse)},
		}),
		topologyObject("declared", []interface{}{
			map[string]interface{}{"type": "SomethingElse", "status": string(metav1.ConditionTrue)},
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionTrue)},
		}),
	}
	r, _ := cutoverReconciler(t, cutoverPlatform(), interceptor.Funcs{}, seed...)
	ctx := context.Background()

	for _, name := range []string{"absent", "no-status", "unparseable", "not-ready"} {
		assert.False(t, r.topologyObjectReady(ctx, migrationTestNS, gvk, name), name)
	}
	assert.True(t, r.topologyObjectReady(ctx, migrationTestNS, gvk, "declared"))
}

// TestCutoverStopsWhenTheStageCannotBePersisted proves the stage is written down BEFORE the step
// it authorises: a failed write means the step does not run at all, so a restore can never
// happen against a state the cluster has no record of.
func TestCutoverStopsWhenTheStageCannotBePersisted(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 0, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), 1, true),
	)
	r, _ := cutoverReconciler(t, p, failingStatusUpdate(errors.New("status write refused")), seed...)

	_, handled, _, err := cutoverPass(t, r)
	assert.True(t, handled)
	require.Error(t, err)
	assert.Equal(t, int32(0), replicasOf(t, r, provisioningWorkloadName),
		"nothing is released while the cluster has no record of the stage that releases it")
}

// TestCutoverStopsWhenAProducerCannotBeRestored proves a fence the engine cannot lift surfaces
// instead of being stepped over: a gateway that stays shut is visible, a cutover that pretended
// it had reopened would not be.
func TestCutoverStopsWhenAProducerCannotBeRestored(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), 2, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), 1, true),
		cutoverWorkload(gatewayWorkloadName, "kong:3.9.1", 0, true),
	)
	r, _ := cutoverReconciler(t, p, failingPatch(errors.New("patch refused")), seed...)

	_, handled, _, err := cutoverPass(t, r)
	assert.True(t, handled)
	require.Error(t, err)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"the migration does not hand over a platform whose door it failed to open")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler()}, stored.Status.Upgrade.Fenced)
}

// TestCutoverWithoutADeployedProvisioningServiceSkipsThatStage proves the stage is about a
// DEPENDENCY, not a component: a platform whose provisioning API is somebody else's has nothing
// to restore and nothing to wait for.
func TestCutoverWithoutADeployedProvisioningServiceSkipsThatStage(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler())
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{Mode: "external", APIURL: "http://provisioner.example.com"}
	seed := declaredTargetTopology(p)
	seed = append(seed, cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), 1, true))
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, _, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, render.holdCore, "there is no operator-managed provisioning service to wait for")
	assert.Contains(t, migrationCondition(storedPlatform(t, r)).Message, cutoverStageCore.String())
}
