/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// The SOURCE-STATE guards, against a fake client: both answer a question about the LIVE
// cluster that the spec alone cannot, so each test states the cluster it is answering against.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// testPlatformUID is the Platform UID the owner references in these fixtures point at, so
// controllerOwnedBy has something real to match.
const testPlatformUID types.UID = "11111111-2222-3333-4444-555555555555"

// targetDefaultVirtualHost is 2.19.0's own default virtual host — the value a user reaches for
// when pinning "the new one" in the same update as the version move, which is the combined edit
// the pin guard exists to catch.
const targetDefaultVirtualHost = "/"

// sourceStateReconciler builds a reconciler over a fake client that knows the rabbitmq.com
// kinds (so a Vhost CR can be stored and read back) and holds the Platform plus the given
// cluster objects.
func sourceStateReconciler(t *testing.T, p *otilmv1alpha1.Platform, funcs interceptor.Funcs, objs ...client.Object) *Reconciler {
	t.Helper()
	s := cutoverScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(append([]client.Object{p}, objs...)...).WithInterceptorFuncs(funcs).Build()
	return &Reconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(32)}
}

// liveVhostCR is the Vhost topology CR the operator renders for p, declaring the given virtual
// host — i.e. the object that records where the platform's live topology actually is.
func liveVhostCR(p *otilmv1alpha1.Platform, vhost string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK())
	u.SetNamespace(p.Namespace)
	u.SetName(platformbuilder.ManagedMessagingVhostName(p))
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{"name": vhost}, "spec")
	return u
}

// coreWorkloadAt is the platform's core workload, stamped with the platform-version annotation
// a render of `version` leaves on its pod template, and controller-owned by the Platform unless
// owned is false.
func coreWorkloadAt(p *otilmv1alpha1.Platform, version string, owned bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: platformbuilder.ResolveCore(p).ResourceName(), Namespace: p.Namespace,
		},
	}
	d.Spec.Template.Annotations = map[string]string{platformbuilder.PlatformVersionAnnotation: version}
	if owned {
		d.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: otilmv1alpha1.GroupVersion.String(), Kind: "Platform",
			Name: p.Name, UID: p.UID, Controller: ptr(true),
		}}
	}
	return d
}

// TestVirtualHostPinSuppressesRename pins the PRECONDITION of the verify-pin decision: only a
// pin standing between two bundles that default to DIFFERENT virtual hosts needs verifying.
func TestVirtualHostPinSuppressesRename(t *testing.T) {
	from, to := bundleFor(t, platformVersion218), bundleFor(t, platformVersion219)
	same := bundleFor(t, platformVersion217)

	unpinned := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
	assert.False(t, virtualHostPinSuppressesRename(unpinned, from, to),
		"no pin: the rename is visible, so the trigger decides it on its own")

	pinned := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
	pinned.Spec.Messaging.VirtualHost = pinnedTestVirtualHost
	assert.True(t, virtualHostPinSuppressesRename(pinned, from, to),
		"a pin across bundles with different default virtual hosts is the case that needs verifying")

	assert.False(t, virtualHostPinSuppressesRename(pinned, same, from),
		"2.17.0 and 2.18.0 default to the SAME virtual host: the pin suppresses nothing")
}

// TestGateMigrationVirtualHostPinRefusesACombinedEdit is the MAJOR-1 regression: a live
// platform that pins a virtual host in the SAME update as the version move must not slip past
// the migration on the strength of a pin describing a place it is not.
func TestGateMigrationVirtualHostPinRefusesACombinedEdit(t *testing.T) {
	p := migrationGatePlatform()
	p.UID = testPlatformUID
	// The combined edit: the version bump AND the pin arrive together. The platform's live
	// topology is still on 2.18.0's default virtual host.
	p.Spec.Messaging.VirtualHost = targetDefaultVirtualHost
	live := liveVhostCR(p, bom.LegacyUnscopedVirtualHost)

	r := sourceStateReconciler(t, p, interceptor.Funcs{}, live)
	handled, _, err := r.guardMigrationVirtualHostPin(context.Background(), p, platformVersion219)
	require.NoError(t, err)
	require.True(t, handled, "a pin the live topology is not on must stop the pass")

	c := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reasonMigrationVirtualHostPinned, c.Reason)
	assert.Contains(t, c.Message, "spec.messaging.virtualHost")
	assert.Contains(t, c.Message, platformVersion218, "the refusal names the version the platform stays on")
	assertNoBrokerCoordinates(t, c.Message)
}

// TestGateMigrationVirtualHostPinAllowsAPreExistingPin is the other half, and the one that must
// NOT regress: a platform that has always run on its pinned virtual host renames nothing when it
// moves version, which is the documented no-migration path the upgrade e2e asserts.
func TestGateMigrationVirtualHostPinAllowsAPreExistingPin(t *testing.T) {
	p := migrationGatePlatform()
	p.UID = testPlatformUID
	p.Spec.Messaging.VirtualHost = pinnedTestVirtualHost
	live := liveVhostCR(p, pinnedTestVirtualHost) // the pin was already in effect

	r := sourceStateReconciler(t, p, interceptor.Funcs{}, live)
	handled, _, err := r.guardMigrationVirtualHostPin(context.Background(), p, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled, "the platform is already on the pinned virtual host: the move renames nothing")
	assert.Nil(t, meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration),
		"nothing to report: the guard is silent when it has nothing to refuse")
}

// TestGateMigrationVirtualHostPinStepsAsideWithoutTheCRDs proves the guard never refuses on a
// cluster that could not answer: no rabbitmq.com CRDs means no topology to strand, and the
// managed-infrastructure gate reports the missing operator in its own words.
func TestGateMigrationVirtualHostPinStepsAsideWithoutTheCRDs(t *testing.T) {
	p := migrationGatePlatform()
	p.UID = testPlatformUID
	p.Spec.Messaging.VirtualHost = targetDefaultVirtualHost

	noMatch := interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			if _, isTopology := obj.(*unstructured.Unstructured); isTopology {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "rabbitmq.com", Kind: "Vhost"}}
			}
			return nil
		},
	}
	r := sourceStateReconciler(t, p, noMatch)
	handled, _, err := r.guardMigrationVirtualHostPin(context.Background(), p, platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled, "an unserved CRD is not an answer, so it is not a refusal either")
}

// TestGateMigrationVirtualHostPinRefusesWithNoLiveTopology covers the other way the platform can
// fail to be on the pinned virtual host: the Vhost CR is not there at all, so nothing supports
// the claim that this move renames nothing.
func TestGateMigrationVirtualHostPinRefusesWithNoLiveTopology(t *testing.T) {
	p := migrationGatePlatform()
	p.UID = testPlatformUID
	p.Spec.Messaging.VirtualHost = pinnedTestVirtualHost

	r := sourceStateReconciler(t, p, interceptor.Funcs{})
	handled, _, err := r.guardMigrationVirtualHostPin(context.Background(), p, platformVersion219)
	require.NoError(t, err)
	assert.True(t, handled, "no live Vhost object means the pin is unverified, which is a refusal")
}

// TestGateMigrationUnrecordedRunningVersion is the MAJOR-2 regression matrix: an empty
// status.observedVersion must not be read as a fresh install while an owned core workload
// demonstrably runs a version with a different messaging topology.
func TestGateMigrationUnrecordedRunningVersion(t *testing.T) {
	target := bundleFor(t, platformVersion219)

	cases := []struct {
		name        string
		annotated   string
		owned       bool
		absent      bool
		pinnedVhost string
		wantHandled bool
	}{
		{
			name:        "a live 2.18.0 core with no recorded version is refused, not adopted",
			annotated:   platformVersion218,
			owned:       true,
			wantHandled: true,
		},
		{
			name:        "a version this build does not carry is refused too: the topologies cannot be compared",
			annotated:   unknownPlatformVersion,
			owned:       true,
			wantHandled: true,
		},
		{
			name:        "no core workload at all is a genuine fresh install",
			absent:      true,
			wantHandled: false,
		},
		{
			name:        "a core already running the requested version has nothing to migrate from",
			annotated:   platformVersion219,
			owned:       true,
			wantHandled: false,
		},
		{
			name:        "an unannotated core (an older operator's render) cannot answer, so it is not refused",
			annotated:   "",
			owned:       true,
			wantHandled: false,
		},
		{
			name:        "a core this Platform does not control is somebody else's",
			annotated:   platformVersion218,
			owned:       false,
			wantHandled: false,
		},
		{
			name:        "a pinned virtual host keeps the topology identical across the move",
			annotated:   platformVersion218,
			owned:       true,
			pinnedVhost: pinnedTestVirtualHost,
			wantHandled: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := migrationGatePlatform()
			p.UID = testPlatformUID
			p.Status.ObservedVersion = "" // the interrupted-reconcile / status-less-restore state
			p.Spec.Messaging.VirtualHost = c.pinnedVhost

			var objs []client.Object
			if !c.absent {
				objs = append(objs, coreWorkloadAt(p, c.annotated, c.owned))
			}
			r := sourceStateReconciler(t, p, interceptor.Funcs{}, objs...)

			handled, _, err := r.guardMigrationUnrecordedRunningVersion(context.Background(), p, target, platformVersion219)
			require.NoError(t, err)
			require.Equal(t, c.wantHandled, handled)

			cond := meta.FindStatusCondition(p.Status.Conditions, conditionMessagingMigration)
			if !c.wantHandled {
				assert.Nil(t, cond, "a pass the guard permits reports nothing")
				return
			}
			require.NotNil(t, cond)
			assert.Equal(t, reasonMigrationRunningVersionUnrecorded, cond.Reason)
			assert.Contains(t, cond.Message, "status.observedVersion")
			assert.Contains(t, cond.Message, c.annotated, "the refusal names the version actually running")
			assertNoBrokerCoordinates(t, cond.Message)
		})
	}
}

// TestCoreWorkloadPlatformVersionReadsBothKinds proves the probe follows
// spec.core.workloadType: a StatefulSet core answers exactly as a Deployment core does.
func TestCoreWorkloadPlatformVersionReadsBothKinds(t *testing.T) {
	p := migrationGatePlatform()
	p.UID = testPlatformUID
	p.Spec.Core.WorkloadType = otilmv1alpha1.WorkloadKindStatefulSet

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: platformbuilder.ResolveCore(p).ResourceName(), Namespace: p.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: otilmv1alpha1.GroupVersion.String(), Kind: "Platform",
				Name: p.Name, UID: p.UID, Controller: ptr(true),
			}},
		},
	}
	sts.Spec.Template.Annotations = map[string]string{
		platformbuilder.PlatformVersionAnnotation: platformVersion218,
	}

	r := sourceStateReconciler(t, p, interceptor.Funcs{}, sts)
	got, err := r.coreWorkloadPlatformVersion(context.Background(), p)
	require.NoError(t, err)
	assert.Equal(t, platformVersion218, got)
}

// TestMessagingTopologyDiffers pins the shared predicate both the trigger and the
// unrecorded-running-version guard read, so the two can never disagree about what a topology
// change IS.
func TestMessagingTopologyDiffers(t *testing.T) {
	from, to := bundleFor(t, platformVersion218), bundleFor(t, platformVersion219)

	managed := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
	assert.True(t, messagingTopologyDiffers(managed, from, to),
		"managed: the effective virtual host changes across this move")

	pinned := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
	pinned.Spec.Messaging.VirtualHost = pinnedTestVirtualHost
	assert.False(t, messagingTopologyDiffers(pinned, from, to),
		"managed + pinned: one virtual host under both bundles")

	external := migrationPlatform(modeExternal, platformVersion218, platformVersion219)
	assert.True(t, messagingTopologyDiffers(external, from, to),
		"external: the target stops publishing to an exchange the source used")

	assert.False(t, messagingTopologyDiffers(external, bundleFor(t, platformVersion217), from),
		"external: a purely additive change renames nothing")
}
