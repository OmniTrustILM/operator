/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	pinMigrationInputs(p)
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
		declared.SetGeneration(1)
		// Ready is only Ready FOR THE CURRENT SPEC: the Topology Operator stamps the generation
		// it reconciled, and the cutover refuses a Ready that belongs to an older one.
		_ = unstructured.SetNestedField(declared.Object, int64(1), "status", "observedGeneration")
		_ = unstructured.SetNestedSlice(declared.Object, []interface{}{
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionTrue)},
		}, "status", "conditions")
		out = append(out, declared)
	}
	return out
}

// cutoverWorkload is a workload carrying the given image and the given VERSION's pod-template
// stamp, at the given desired replica count, with the rollout status the caller asks for:
// rolled=true reports the workload controller finished rolling every pod onto the current
// template.
//
// The version and the image are separate arguments because they are separate facts, and the
// cutover leans on the version: several components carry the SAME image across neighbouring
// bundles, so only the stamp says which bundle the template was rendered from.
func cutoverWorkload(name, image, version string, replicas int32, rolled bool) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: migrationTestNS, Generation: 4},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{platformbuilder.PlatformVersionAnnotation: version},
				},
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

// schedulerImageFor resolves the image a given platform VERSION renders for the scheduler.
func schedulerImageFor(p *otilmv1alpha1.Platform, version string) string {
	render := p.DeepCopy()
	render.Spec.Version = version
	return platformbuilder.ResolveScheduler(render).Image
}

// gatewayImageOf resolves the image the render pins for the API gateway. Fixtures use it rather
// than a literal tag because the cutover measures a live pod template against exactly this
// value before it lifts the fence from the workload.
func gatewayImageOf(p *otilmv1alpha1.Platform) string {
	return platformbuilder.ResolveGateway(p).Image
}

// retargetWorkload puts the TARGET version's pod template on a live workload — what the
// reconcile's own Server-Side Apply does to every workload of this phase, fenced or not, since
// the fence holds .spec.replicas alone.
func retargetWorkload(t *testing.T, r *Reconciler, name, image, version string) {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: name, Namespace: migrationTestNS}, &dep))
	dep.Spec.Template.Annotations[platformbuilder.PlatformVersionAnnotation] = version
	dep.Spec.Template.Spec.Containers[0].Image = image
	require.NoError(t, r.Update(context.Background(), &dep))
}

// fencedProvisioning / fencedGateway / fencedScheduler are the fence records the drain hands the
// cutover: the three producers, at the counts restore must write back.
func fencedProvisioning() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: provisioningWorkloadName, Kind: kindDeployment, Replicas: 2}
}

func fencedGateway() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: gatewayWorkloadName, Kind: kindDeployment, Replicas: 3}
}

func fencedScheduler() otilmv1alpha1.FencedWorkload {
	return otilmv1alpha1.FencedWorkload{Name: schedulerWorkloadName, Kind: kindDeployment, Replicas: 1}
}

// rolledOutScheduler is the scheduler as stage 2 leaves it: released, on the target template and
// serving. Every fixture that is past stage 2 needs it — Core's wait-for-auth init container
// polls the scheduler's Service, so the cutover measures its rollout exactly as it measures the
// provisioning service's.
func rolledOutScheduler(p *otilmv1alpha1.Platform) *appsv1.Deployment {
	return cutoverWorkload(schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219, 1, true)
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
		assertNoBrokerCoordinates(t, migrationCutoverMessage(cutoverPlatform().Status.Upgrade, cutoverMeasurement{stage: s}))
		assertNoBrokerCoordinates(t, migrationCutoverMessage(cutoverPlatform().Status.Upgrade,
			cutoverMeasurement{stage: s, blockedOn: schedulerWorkloadName}))
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
	assert.Contains(t, names, schedulerWorkloadName)
	assert.NotContains(t, names, coreDeploymentName, "Core is the consumer, never a fence target")
}

// TestNoCoreInitDependencyIsStillFencedWhenCoreRolls is the CLASS invariant behind stage 2, and
// the one an envtest cannot express: envtest runs no kubelet, so no init container ever blocks
// there and the deadlock this guards against is invisible until a real cluster runs it.
//
// The claim: of the workloads the fence holds, the ones still held when stage 3 rolls Core —
// i.e. the fence targets that cutoverPreCoreReleases does not release — must have NOTHING in
// common with the workloads Core's init containers block on. A fenced init dependency sits at
// zero replicas, so Core's pod waits on a Service with no endpoints, never becomes Ready, and
// the cutover (which has no deadline behind it, being past the point of no return) waits forever.
//
// Both sides are DERIVED, not restated: the dependencies come from the builder that renders the
// init containers, the held set from the fence's own targets minus the phase's own release list.
// Poll a new Service in wait-for-auth without teaching the cutover to release it and this fails.
//
// MEMBERSHIP IS NECESSARY AND NOT SUFFICIENT, so it is only the first of three claims. A listed
// workload still has to be released at the right MOMENT (the two tests below: not while it
// carries the source template, and not vacuously while it is configured for no pods at all), or
// the list is satisfied by a release that either publishes to the topology being reclaimed or
// answers nothing.
func TestNoCoreInitDependencyIsStillFencedWhenCoreRolls(t *testing.T) {
	p := cutoverPlatform()
	// The proxy path is on, so BOTH sources of an init dependency are in play: the Services
	// wait-for-auth polls and the provisioning service provision-instance-queue POSTs to.
	p.Spec.Common.Proxy.Enabled = true
	released := make(map[string]struct{}, len(cutoverPreCoreReleases))
	for _, name := range cutoverPreCoreReleases {
		released[name] = struct{}{}
	}

	var heldWhenCoreRolls []string
	for _, target := range platformbuilder.MigrationFenceTargets(p) {
		if _, isReleased := released[target.Name]; !isReleased {
			heldWhenCoreRolls = append(heldWhenCoreRolls, target.Name)
		}
	}

	dependencies := platformbuilder.CoreInitServiceDependencies(p)
	require.NotEmpty(t, dependencies)
	for _, dep := range dependencies {
		assert.NotContains(t, heldWhenCoreRolls, dep,
			"%q is a workload Core's init containers block on, so the cutover must release it before it rolls Core", dep)
	}

	// The assertion above is only worth anything while the two sets CAN overlap: this fixture
	// fences workloads Core depends on, and something is still held back when Core rolls.
	assert.NotEmpty(t, heldWhenCoreRolls, "the fence still holds the door shut when Core rolls")
	overlapping := 0
	for _, dep := range dependencies {
		if _, isReleased := released[dep]; isReleased {
			overlapping++
		}
	}
	assert.NotZero(t, overlapping, "this platform fences workloads Core's init containers depend on")
}

// TestCutoverHoldsAFencedWorkloadUntilTheTargetRenderReachesIt is the RELEASE half of the class
// invariant above, and the half membership of cutoverPreCoreReleases cannot express: not WHICH
// workloads come back up early, but WHEN one of them may.
//
// The fixture is what makes the ordering observable. The target topology is already declared on
// the FIRST CuttingOver pass — an orphaned target topology left behind by an earlier attempt —
// so the stage measures "release the services Core starts behind" before this phase's own render
// has been applied to anything. Every fenced workload therefore still carries the SOURCE pod
// template, and a producer released there starts pods that publish into the virtual host the
// drain has just emptied and the cleanup is about to delete.
func TestCutoverHoldsAFencedWorkloadUntilTheTargetRenderReachesIt(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageFor(p, platformVersion218), platformVersion218, 0, true),
		cutoverWorkload(schedulerWorkloadName, schedulerImageFor(p, platformVersion218), platformVersion218, 0, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion218, 0, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled, "the reconcile must continue: its own apply is what puts the target template on them")
	assert.True(t, render.holdCore)
	assert.True(t, render.requeue, "the release is deferred to the next pass, not abandoned")

	stored := storedPlatform(t, r)
	assert.Equal(t, int32(0), replicasOf(t, r, provisioningWorkloadName),
		"a producer on the source template must not be started back into the virtual host the drain emptied")
	assert.Equal(t, int32(0), replicasOf(t, r, schedulerWorkloadName))
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler(), fencedProvisioning()},
		stored.Status.Upgrade.Fenced, "nothing is de-listed either — the fence goes on holding what it did not release")

	// What the rest of the reconcile does after this phase hands back: the base apply puts the
	// TARGET template on every workload, fenced or not, because the fence holds .spec.replicas
	// alone. The same stage now finds what it was waiting for.
	retargetWorkload(t, r, provisioningWorkloadName, provisioningImageOf(p), platformVersion219)
	retargetWorkload(t, r, schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219)

	render, _, _, err = cutoverPass(t, r)
	require.NoError(t, err)
	assert.True(t, render.holdCore, "released is not the same as serving")
	assert.Equal(t, int32(2), replicasOf(t, r, provisioningWorkloadName),
		"on the target template it publishes only to the target topology, so it may come back up")
	assert.Equal(t, int32(1), replicasOf(t, r, schedulerWorkloadName))
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway()}, storedPlatform(t, r).Status.Upgrade.Fenced)
}

// TestCutoverSurfacesADependencyScaledToZeroWithoutStoppingTheReconcile is the OTHER way a stage
// can be satisfied without anything having happened.
//
// A component configured for zero replicas is admissible on an ordinary platform, and the rollout
// predicate deliberately calls it rolled out — correct for the fence's checks, where zero is the
// state the fence itself imposed. In the cutover it is a wedge: a Service with no endpoints never
// answers, so Core's init containers (wait-for-auth polls the scheduler, provision-instance-queue
// polls the provisioning API) can never pass, and the phase has no deadline to end the wait. Core
// at zero is the same vacuum one step later.
//
// SO IT IS REPORTED, AND THE PASS GOES ON. The remedy is the operator raising the component's
// replica count in the CR, and the only thing that carries that edit down to the child workload is
// the base Server-Side Apply that runs AFTER this gate. A pass that short-circuited here would
// skip the very apply the remedy travels through: the reconcile would read the same zero forever,
// however the CR was corrected. So the measurement names the workload, the condition carries the
// action, and handled stays false.
func TestCutoverSurfacesADependencyScaledToZeroWithoutStoppingTheReconcile(t *testing.T) {
	tests := []struct {
		name string
		// workload is the component the platform configures for no pods at all.
		workload string
		// wantStage is the stage that cannot complete while it is at zero, and wantHold whether
		// Core may be rendered at that stage.
		wantStage cutoverStage
		wantHold  bool
	}{
		{
			name: "the provisioning service Core's proxy path retries against", workload: provisioningWorkloadName,
			wantStage: cutoverStageProvisioning, wantHold: true,
		},
		{
			name: "the scheduler Core's wait-for-auth polls", workload: schedulerWorkloadName,
			wantStage: cutoverStageProvisioning, wantHold: true,
		},
		{
			name: "core itself, whose readiness stage 4 turns on", workload: coreDeploymentName,
			wantStage: cutoverStageCore,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			replicas := func(name string) int32 {
				if name == tc.workload {
					return 0
				}
				return 1
			}
			p := cutoverPlatform(fencedGateway())
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219,
					replicas(provisioningWorkloadName), true),
				cutoverWorkload(schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219,
					replicas(schedulerWorkloadName), true),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), platformVersion219,
					replicas(coreDeploymentName), true),
				cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
			)
			r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

			render, handled, _, err := cutoverPass(t, r)
			require.NoError(t, err, "a component the operator can scale is not a failure of this operator")
			assert.False(t, handled,
				"the reconcile must continue: its own apply is the only way the corrected replica count reaches the workload")
			assert.True(t, render.requeue)
			assert.Equal(t, tc.wantHold, render.holdCore)

			stored := storedPlatform(t, r)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
				"the migration stays where it is until the operator scales the component up")
			assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway()}, stored.Status.Upgrade.Fenced,
				"the door is not reopened onto a platform that cannot finish cutting over")
			assert.Equal(t, int32(0), replicasOf(t, r, gatewayWorkloadName))

			cond := migrationCondition(stored)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status,
				"a platform waiting on a replica count of its own is waiting, not broken")
			assert.Contains(t, cond.Message, tc.wantStage.String())
			assert.Contains(t, cond.Message, tc.workload, "the operator is told which workload to scale")
			assert.Contains(t, cond.Message, "1 replica", "and the one action that unblocks the migration")
			assertNoBrokerCoordinates(t, cond.Message)
			assert.Nil(t, meta.FindStatusCondition(stored.Status.Conditions, conditionDegraded),
				"a component nobody asked for pods from is the platform's own configuration, not a degraded operator")

			// The remedy: spec.<component>.replicas goes back up, and the base apply — the one
			// this pass did NOT skip — lands it on the child workload. The migration resumes with
			// no further intervention. Every OTHER component in this fixture is already serving
			// the target, so the single blocked one coming up carries the cutover all the way to
			// the hand-over.
			scaleWorkload(t, r, tc.workload, 1)

			_, handled, _, err = cutoverPass(t, r)
			require.NoError(t, err)
			assert.False(t, handled)

			stored = storedPlatform(t, r)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, stored.Status.Upgrade.Phase,
				"the migration carries on by itself once the workload the condition named is serving")
			assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName), "the door is reopened at its recorded count")
			assert.NotContains(t, migrationCondition(stored).Message, "0 replicas",
				"the remedy stops being advertised once it has been applied")
		})
	}
}

// scaleWorkload raises a live workload's desired replica count and reports it fully rolled out at
// that count — what the operator editing spec.<component>.replicas, the reconcile's own apply and
// the workload controller between them leave behind.
func scaleWorkload(t *testing.T, r *Reconciler, name string, replicas int32) {
	t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: name, Namespace: migrationTestNS}, &dep))
	dep.Spec.Replicas = ptr(replicas)
	require.NoError(t, r.Update(ctx, &dep))

	// The workload controller catching up is a SECOND write, on the status subresource and after
	// the spec one bumped the generation: a rollout is complete only once the controller has
	// observed the very template the operator applied.
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: name, Namespace: migrationTestNS}, &dep))
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.AvailableReplicas = replicas, replicas, replicas
	require.NoError(t, r.Status().Update(ctx, &dep))
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

// TestCutoverHoldsCoreWhileADependencyIsStillFenced is THE deadlock guard.
//
// The topology is fully declared, so stage 1 is behind us — but the provisioning service and the
// scheduler are still on the fence's list, sitting at zero replicas with the target template
// already applied to them. Such a workload reports 0/0 ready, which is precisely the shape a
// naive readiness check calls "ready". If the cutover believed it, it would roll Core, whose init
// containers poll the scheduler's Service and retry against the provisioning API forever, and the
// migration — which is past the point of no return and has no deadline — would never finish.
//
// So: Core is withheld, and the pass's own job is to RELEASE both of them. Neither goes back onto
// the source topology by coming up here: the target render has been applied to their pod
// templates since the first pass of this phase, fenced or not.
func TestCutoverHoldsCoreWhileADependencyIsStillFenced(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		// The trap: fenced at zero, already carrying the target template.
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 0, true),
		cutoverWorkload(schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219, 0, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.True(t, render.holdCore,
		"a workload the fence still holds can never answer Core's init containers")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"the cutover cannot hand over while a dependency is still fenced")
	assert.Equal(t, int32(2), replicasOf(t, r, provisioningWorkloadName),
		"the pass releases the provisioning service at the count the fence recorded")
	assert.Equal(t, int32(1), replicasOf(t, r, schedulerWorkloadName),
		"and the scheduler too — Core's wait-for-auth init container polls its Service")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway()}, stored.Status.Upgrade.Fenced,
		"only what Core starts behind is released; the door stays shut")
	assert.Equal(t, int32(0), replicasOf(t, r, gatewayWorkloadName), "the gateway is not reopened at this stage")
	assert.Contains(t, migrationCondition(stored).Message, cutoverStageProvisioning.String())
}

// TestCutoverHoldsCoreUntilItsDependenciesHaveRolledOut is the second half of stage 2: released
// from the fence is not the same as answering. Until EACH service Core starts behind has finished
// rolling onto the target template, Core stays where it is — and the two are measured separately,
// so neither one's rollout can stand in for the other's.
func TestCutoverHoldsCoreUntilItsDependenciesHaveRolledOut(t *testing.T) {
	tests := []struct {
		name                    string
		provRolled, schedRolled bool
	}{
		{name: "the provisioning service is up but not serving yet", schedRolled: true},
		{name: "the scheduler is up but not serving yet", provRolled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cutoverPlatform(fencedGateway())
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, tc.provRolled),
				cutoverWorkload(schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219, 1, tc.schedRolled),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
			)
			r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

			render, _, _, err := cutoverPass(t, r)
			require.NoError(t, err)
			assert.True(t, render.holdCore, "Core's pod would block in its init containers")
			stored := storedPlatform(t, r)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase)
			assert.Contains(t, migrationCondition(stored).Message, cutoverStageProvisioning.String())
		})
	}
}

// TestCutoverRollsCoreOnceItsDependenciesAnswer is stage 3: with the topology declared and every
// service Core starts behind serving, Core is finally allowed to roll — but the cutover does NOT
// hand over yet, because Core is still Ready on the SOURCE pod template it never left.
func TestCutoverRollsCoreOnceItsDependenciesAnswer(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
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
// with nothing left fenced at all.
func TestCutoverReopensTheGatewayAndHandsOver(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), platformVersion219, 1, true),
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
	)
	r, rec := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.False(t, render.holdCore)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, stored.Status.Upgrade.Phase)
	assert.Empty(t, stored.Status.Upgrade.Fenced,
		"the cleanup inherits a platform that is open, serving from the target topology")
	assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName), "the gateway is reopened at its recorded count")
	assert.Equal(t, int32(1), replicasOf(t, r, schedulerWorkloadName), "the scheduler has been up since stage 2")

	var handOver string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, eventMigrationPhase) && strings.Contains(e, string(otilmv1alpha1.MigrationPhaseCleaningUp)) {
			handOver = e
		}
	}
	require.NotEmpty(t, handOver, "the hand-over is announced")
	assertNoBrokerCoordinates(t, handOver)
}

// TestCutoverDoesNotHandOverWhileTheGatewayIsStillFenced is stage 4's own version of the release
// contract: a release that DEFERS is not a release, and the hand-over may not step over it.
//
// The gateway's release is template-guarded exactly as the pre-Core ones are — a door reopened
// while it still carries the SOURCE pod template would publish into the virtual host the drain has
// emptied. But stage 4 is the last stage: unlike stage 2, nothing downstream re-measures fence
// membership, and the cleanup's own final restore is not template-guarded. So a deferred release
// that the phase handed over anyway would leave the platform shut with the cleanup running, and
// reopen it later with no check at all. The stage therefore hands over only once the gateway is
// actually clear of the fence.
func TestCutoverDoesNotHandOverWhileTheGatewayIsStillFenced(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), platformVersion219, 1, true),
		// The door still carries the SOURCE template: this phase runs ahead of the reconcile's own
		// apply, so the first pass to reach stage 4 can find it not yet retargeted.
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion218, 0, true),
	)
	r, rec := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, handled, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.True(t, render.requeue, "the release is deferred to the next pass, not abandoned")

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
		"the cleanup may not inherit a platform whose door is still shut")
	assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway()}, stored.Status.Upgrade.Fenced)
	assert.Equal(t, int32(0), replicasOf(t, r, gatewayWorkloadName))
	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, string(otilmv1alpha1.MigrationPhaseCleaningUp),
			"a hand-over that has not happened must not be announced")
	}

	// What the rest of the reconcile does after this phase hands back: the base apply puts the
	// target template on the gateway too. The same stage now releases it and hands over.
	retargetWorkload(t, r, gatewayWorkloadName, gatewayImageOf(p), platformVersion219)

	_, handled, _, err = cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)

	stored = storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCleaningUp, stored.Status.Upgrade.Phase)
	assert.Empty(t, stored.Status.Upgrade.Fenced)
	assert.Equal(t, int32(3), replicasOf(t, r, gatewayWorkloadName), "the door is reopened at its recorded count")
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
			wantFenced: []otilmv1alpha1.FencedWorkload{fencedGateway()},
			wantHold:   true,
		},
		{
			name:       "re-entering the core stage leaves the fence alone",
			fenced:     []otilmv1alpha1.FencedWorkload{fencedGateway()},
			coreImage:  platformVersion218,
			provRolled: true,
			wantPhase:  otilmv1alpha1.MigrationPhaseCuttingOver,
			wantFenced: []otilmv1alpha1.FencedWorkload{fencedGateway()},
		},
		{
			name:           "re-entering past the hand-over does not re-open or hand over twice",
			fenced:         []otilmv1alpha1.FencedWorkload{fencedGateway()},
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
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, tc.provRolled),
				rolledOutScheduler(p),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, tc.coreImage), tc.coreImage, 1, true),
				cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
			)
			r, rec := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

			render := repeatedCutoverPasses(t, r, 3)

			stored := storedPlatform(t, r)
			if tc.wantHandedOver {
				assertHandedOverExactlyOnce(t, r, rec, stored)
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

// repeatedCutoverPasses runs the gate n times on a reconciler that carries nothing from the pass
// before — the operator restarting, over and over, at the same stage boundary — and returns the
// last render.
func repeatedCutoverPasses(t *testing.T, r *Reconciler, n int) migrationRender {
	t.Helper()
	var render migrationRender
	for i := 0; i < n; i++ {
		var err error
		render, _, _, err = cutoverPass(t, r)
		require.NoError(t, err)
	}
	return render
}

// assertHandedOverExactlyOnce is what the repeated passes must leave behind once the cutover is
// already past: a finished migration, a door reopened at the recorded count, and ONE hand-over
// announcement however many times the pass ran.
func assertHandedOverExactlyOnce(t *testing.T, r *Reconciler, rec *record.FakeRecorder, stored *otilmv1alpha1.Platform) {
	t.Helper()
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
}

// --- the failure paths -------------------------------------------------------

// TestCutoverStopsWhenItCannotReadTheRolloutState proves an unreadable cluster stops the pass
// rather than being folded into "not done yet". Past the drain the migration is forward-only and
// has no deadline, so a read failure that sat silently in a stage would be invisible forever.
func TestCutoverStopsWhenItCannotReadTheRolloutState(t *testing.T) {
	for _, unreadable := range []string{provisioningWorkloadName, schedulerWorkloadName, coreDeploymentName} {
		t.Run(unreadable, func(t *testing.T) {
			p := cutoverPlatform(fencedGateway())
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
				rolledOutScheduler(p),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), platformVersion219, 1, true),
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
			assert.Equal(t, []otilmv1alpha1.FencedWorkload{fencedGateway()},
				stored.Status.Upgrade.Fenced, "an unanswered read releases nothing")
		})
	}
}

// TestCutoverStopsWhenTheHandOverCannotBePersisted covers the last write of the phase: the door
// is already open (the restore's own de-listing landed) but the hand-over does not, so the
// migration stays in CuttingOver and the next pass — which re-measures the same stage and finds
// the gateway already restored — repeats only the write that failed.
func TestCutoverStopsWhenTheHandOverCannotBePersisted(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion219), platformVersion219, 1, true),
		cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
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
	assert.Empty(t, stored.Status.Upgrade.Fenced,
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
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{platformbuilder.PlatformVersionAnnotation: platformVersion219},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kong", Image: image}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
	r, _ := cutoverReconciler(t, cutoverPlatform(), interceptor.Funcs{}, sts)
	ctx := context.Background()

	progress, err := r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, image, platformVersion219)
	require.NoError(t, err)
	assert.Equal(t, workloadProgress{rolled: true}, progress,
		"a StatefulSet-typed component rolls out like a Deployment-typed one")

	progress, err = r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, "kong:3.8.0", platformVersion219)
	require.NoError(t, err)
	assert.Equal(t, workloadProgress{}, progress, "the target image is corroborating evidence, and it has to match too")

	progress, err = r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, image, platformVersion218)
	require.NoError(t, err)
	assert.Equal(t, workloadProgress{}, progress, "a template stamped with another version is not the target rollout")

	progress, err = r.workloadRolledOut(ctx, migrationTestNS, "nothing-of-this-name", image, platformVersion219)
	require.NoError(t, err)
	assert.Equal(t, workloadProgress{}, progress, "a workload that has not been applied yet is not an error")

	// A StatefulSet-typed component the platform configures for no pods is named rather than
	// answered, exactly as a Deployment-typed one is.
	sts.Spec.Replicas = ptr(int32(0))
	require.NoError(t, r.Update(ctx, sts))
	progress, err = r.workloadRolledOut(ctx, migrationTestNS, gatewayWorkloadName, image, platformVersion219)
	require.NoError(t, err)
	assert.Equal(t, workloadProgress{blockedOn: gatewayWorkloadName}, progress,
		"a component with no pods is a wait the operator can end, not a rollout")
}

// TestTopologyObjectDeclaredFailsClosed pins the one-directional answer stage 1 depends on: only an
// explicit Ready=True, for the CURRENT spec, is a yes. Everything else — absent, no status, an
// explicit False, or a Ready the object earned before its latest spec was reconciled — holds the
// cutover, and a condition list the operator cannot parse is an ERROR rather than a silent wait.
func TestTopologyObjectDeclaredFailsClosed(t *testing.T) {
	gvk := platformbuilder.ManagedMessagingVhostGVK()
	topologyObject := func(name string, generation, observed int64, conditions []interface{}) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		u.SetName(name)
		u.SetNamespace(migrationTestNS)
		u.SetGeneration(generation)
		if observed > 0 {
			_ = unstructured.SetNestedField(u.Object, observed, "status", "observedGeneration")
		}
		if conditions != nil {
			_ = unstructured.SetNestedSlice(u.Object, conditions, "status", "conditions")
		}
		return u
	}
	ready := []interface{}{
		map[string]interface{}{"type": "SomethingElse", "status": string(metav1.ConditionTrue)},
		map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionTrue)},
	}
	seed := []client.Object{
		topologyObject("no-status", 1, 0, nil),
		topologyObject("unparseable", 1, 1, []interface{}{"not-a-condition"}),
		topologyObject("not-ready", 1, 1, []interface{}{
			map[string]interface{}{"type": conditionTypeReady, "status": string(metav1.ConditionFalse)},
		}),
		// The stale case: Ready=True, but earned by the spec BEFORE the one the operator just
		// applied. Accepting it would let the cutover roll Core onto a topology the broker has
		// not been told about yet.
		topologyObject("stale-ready", 4, 3, ready),
		topologyObject("declared", 4, 4, ready),
	}
	r, _ := cutoverReconciler(t, cutoverPlatform(), interceptor.Funcs{}, seed...)
	ctx := context.Background()

	for _, name := range []string{"absent", "no-status", "not-ready", "stale-ready"} {
		got, err := r.topologyObjectDeclared(ctx, migrationTestNS, gvk, name)
		require.NoError(t, err, name)
		assert.False(t, got, name)
	}

	_, err := r.topologyObjectDeclared(ctx, migrationTestNS, gvk, "unparseable")
	require.Error(t, err, "an unreadable status is surfaced, not turned into an endless wait")

	got, err := r.topologyObjectDeclared(ctx, migrationTestNS, gvk, "declared")
	require.NoError(t, err)
	assert.True(t, got)
}

// TestTopologyObjectDeclaredSurfacesAReadFailure: stage 1 has no deadline behind it, so an
// apiserver that will not answer must reach the platform's status rather than sit in a stage
// that looks like ordinary waiting — and it must do so without naming the object, whose name
// carries the virtual-host scope.
func TestTopologyObjectDeclaredSurfacesAReadFailure(t *testing.T) {
	gvk := platformbuilder.ManagedMessagingVhostGVK()
	r, _ := cutoverReconciler(t, cutoverPlatform(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isTopology := obj.(*unstructured.Unstructured); isTopology {
				return apierrors.NewInternalError(errors.New("etcd is unavailable"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, err := r.topologyObjectDeclared(context.Background(), migrationTestNS, gvk, "ilm-2-19-0-vhost")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ilm-2-19-0-vhost", "a topology object's name is a coordinate")
	assertNoBrokerCoordinates(t, err.Error())
}

// TestCutoverRefusesAProvisioningServiceStillOnTheSourceTemplate is the false-yes the image
// check alone cannot catch: 2.18.0 and 2.19.0 pin provisioning-rabbitmq to the SAME image, so a
// provisioning service still configured entirely from the source bundle — pointing at the
// virtual host the migration is leaving — satisfies "runs the target's image" perfectly. Releasing
// Core on that answer rolls it onto a topology its provisioner has not moved to.
func TestCutoverRefusesAProvisioningServiceStillOnTheSourceTemplate(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	sourceImage := provisioningImageFor(p, platformVersion218)
	require.Equal(t, provisioningImageOf(p), sourceImage,
		"the whole point of this case: the two bundles pin the same provisioning image")

	seed := declaredTargetTopology(p)
	seed = append(seed,
		// Target image, fully rolled out — and stamped with the SOURCE version.
		cutoverWorkload(provisioningWorkloadName, sourceImage, platformVersion218, 2, true),
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, _, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.True(t, render.holdCore,
		"a provisioning service still on the source template may not release Core, whatever image it runs")
	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase)
	assert.Contains(t, migrationCondition(stored).Message, cutoverStageProvisioning.String())
}

// provisioningImageFor resolves the image a given platform VERSION renders for the provisioning
// service.
func provisioningImageFor(p *otilmv1alpha1.Platform, version string) string {
	render := p.DeepCopy()
	render.Spec.Version = version
	return platformbuilder.ResolveProvisioning(render).Image
}

// TestCutoverStopsWhenTheStageCannotBePersisted proves the stage is written down BEFORE the step
// it authorises: a failed write means the step does not run at all, so a restore can never
// happen against a state the cluster has no record of.
func TestCutoverStopsWhenTheStageCannotBePersisted(t *testing.T) {
	p := cutoverPlatform(fencedGateway(), fencedScheduler(), fencedProvisioning())
	seed := declaredTargetTopology(p)
	seed = append(seed,
		cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 0, true),
		cutoverWorkload(schedulerWorkloadName, platformbuilder.ResolveScheduler(p).Image, platformVersion219, 0, true),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
	)
	r, _ := cutoverReconciler(t, p, failingStatusUpdate(errors.New("status write refused")), seed...)

	_, handled, _, err := cutoverPass(t, r)
	assert.True(t, handled)
	require.Error(t, err)
	assert.Equal(t, int32(0), replicasOf(t, r, provisioningWorkloadName),
		"nothing is released while the cluster has no record of the stage that releases it")
	assert.Equal(t, int32(0), replicasOf(t, r, schedulerWorkloadName))
}

// TestCutoverStopsWhenAWorkloadCannotBeRestored proves a fence the engine cannot lift surfaces
// instead of being stepped over, at BOTH stages that lift one: a workload that stays at zero is
// visible, a cutover that pretended it had released it would not be.
func TestCutoverStopsWhenAWorkloadCannotBeRestored(t *testing.T) {
	tests := []struct {
		name      string
		fenced    []otilmv1alpha1.FencedWorkload
		coreImage string
	}{
		{
			name:      "the services Core starts behind, at stage 2",
			fenced:    []otilmv1alpha1.FencedWorkload{fencedGateway(), fencedScheduler(), fencedProvisioning()},
			coreImage: platformVersion218,
		},
		{
			name:      "the gateway, at stage 4",
			fenced:    []otilmv1alpha1.FencedWorkload{fencedGateway()},
			coreImage: platformVersion219,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cutoverPlatform(tc.fenced...)
			seed := declaredTargetTopology(p)
			seed = append(seed,
				cutoverWorkload(provisioningWorkloadName, provisioningImageOf(p), platformVersion219, 2, true),
				rolledOutScheduler(p),
				cutoverWorkload(coreDeploymentName, coreImageFor(p, tc.coreImage), tc.coreImage, 1, true),
				cutoverWorkload(gatewayWorkloadName, gatewayImageOf(p), platformVersion219, 0, true),
			)
			r, _ := cutoverReconciler(t, p, failingPatch(errors.New("patch refused")), seed...)

			_, handled, _, err := cutoverPass(t, r)
			assert.True(t, handled)
			require.Error(t, err)

			stored := storedPlatform(t, r)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase,
				"the migration does not move on from a fence it failed to lift")
			assert.Equal(t, tc.fenced, stored.Status.Upgrade.Fenced)
		})
	}
}

// TestCutoverWithoutADeployedProvisioningServiceSkipsThatWait proves the provisioning half of
// stage 2 is about a DEPENDENCY, not a component: a platform whose provisioning API is somebody
// else's has nothing to restore and nothing to wait for. The scheduler half is unaffected —
// Core's wait-for-auth init container polls it whatever the provisioning mode is.
func TestCutoverWithoutADeployedProvisioningServiceSkipsThatWait(t *testing.T) {
	p := cutoverPlatform(fencedGateway())
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{Mode: "external", APIURL: "http://provisioner.example.com"}
	seed := declaredTargetTopology(p)
	seed = append(seed,
		rolledOutScheduler(p),
		cutoverWorkload(coreDeploymentName, coreImageFor(p, platformVersion218), platformVersion218, 1, true),
	)
	r, _ := cutoverReconciler(t, p, interceptor.Funcs{}, seed...)

	render, _, _, err := cutoverPass(t, r)
	require.NoError(t, err)
	assert.False(t, render.holdCore, "there is no operator-managed provisioning service to wait for")
	assert.Contains(t, migrationCondition(storedPlatform(t, r)).Message, cutoverStageCore.String())
}
