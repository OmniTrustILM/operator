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

// STOPPING a workload is one contract with two routes into it — the prune, when a component is
// de-rendered, and stop-before-start, when it changes kind. These tests hold the two routes to
// the same meaning: the object outlives its pods, and nothing calls the switch settled while
// any workload this Platform owns is still going away.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// terminationTestFinalizer keeps a deleted object alive in the fake client, which runs no
// garbage collector — the stand-in for the foregroundDeletion finalizer a real apiserver adds.
const terminationTestFinalizer = "test.otilm.com/hold"

// ownedWorkloadLabels are the operator's own selector labels, the membership rule every teardown
// path shares.
func ownedWorkloadLabels(p *otilmv1alpha1.Platform) map[string]string {
	return map[string]string{
		common.ManagedByLabel: common.ManagedByValue,
		common.InstanceLabel:  p.Name,
	}
}

// deRenderedWorkloadName is the component these fixtures tear down: provisioning is the one
// platform component a spec flip (mode=external) removes from the render entirely, which is the
// shape both teardown routes have to survive.
const deRenderedWorkloadName = "provisioning"

// ownedWorkload is a labelled, controller-owned workload of the given kind.
func ownedWorkload(p *otilmv1alpha1.Platform, kind otilmv1alpha1.WorkloadKind) client.Object {
	om := metav1.ObjectMeta{
		Name: deRenderedWorkloadName, Namespace: p.Namespace, Labels: ownedWorkloadLabels(p),
		Finalizers: []string{terminationTestFinalizer},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: otilmv1alpha1.GroupVersion.String(), Kind: "Platform",
			Name: p.Name, UID: p.UID, Controller: ptr(true),
		}},
	}
	if kind == otilmv1alpha1.WorkloadKindStatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: om}
	}
	return &appsv1.Deployment{ObjectMeta: om}
}

// TestPruneStopsWorkloadsForeground is the MAJOR-3 regression: an orphaned Deployment or
// StatefulSet must be deleted with FOREGROUND propagation, so its pods are gone before the
// object is and a re-enabled component of the other kind cannot start beside them.
func TestPruneStopsWorkloadsForeground(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns", UID: testPlatformUID},
	}

	cases := []struct {
		name           string
		obj            client.Object
		wantForeground bool
	}{
		{name: "Deployment", obj: ownedWorkload(p, otilmv1alpha1.WorkloadKindDeployment), wantForeground: true},
		{name: "StatefulSet", obj: ownedWorkload(p, otilmv1alpha1.WorkloadKindStatefulSet), wantForeground: true},
		{
			name: "Service (owns no pods)",
			obj: &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: deRenderedWorkloadName, Namespace: p.Namespace, Labels: ownedWorkloadLabels(p),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: otilmv1alpha1.GroupVersion.String(), Kind: "Platform",
					Name: p.Name, UID: p.UID, Controller: ptr(true),
				}},
			}},
			wantForeground: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var policy *metav1.DeletionPropagation
			capture := interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					var o client.DeleteOptions
					o.ApplyOptions(opts)
					policy = o.PropagationPolicy
					return cl.Delete(ctx, obj, opts...)
				},
			}
			s := reconcileScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, c.obj).
				WithInterceptorFuncs(capture).Build()
			r := &Reconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(8)}

			require.NoError(t, r.pruneIfOrphan(context.Background(), p, c.obj, newDesiredSet()))
			if !c.wantForeground {
				assert.Nil(t, policy, "a kind that owns no pods takes the apiserver's own default")
				return
			}
			require.NotNil(t, policy, "a workload must not be reclaimed on the default background policy")
			assert.Equal(t, metav1.DeletePropagationForeground, *policy,
				"the object must outlive its pods, exactly as stop-before-start requires")
		})
	}
}

// TestTerminatingOwnedWorkloadIsObjectBased is the MAJOR-4 regression: settlement must see a
// workload that has left the RENDER while its object is still terminating.
func TestTerminatingOwnedWorkloadIsObjectBased(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns", UID: testPlatformUID},
	}

	cases := []struct {
		name      string
		build     func() client.Object
		terminate bool
		want      bool
	}{
		{
			name:      "a de-rendered Deployment still terminating holds settlement open",
			build:     func() client.Object { return ownedWorkload(p, otilmv1alpha1.WorkloadKindDeployment) },
			terminate: true, want: true,
		},
		{
			name:      "a de-rendered StatefulSet still terminating holds settlement open",
			build:     func() client.Object { return ownedWorkload(p, otilmv1alpha1.WorkloadKindStatefulSet) },
			terminate: true, want: true,
		},
		{
			name:      "a live workload of either kind settles",
			build:     func() client.Object { return ownedWorkload(p, otilmv1alpha1.WorkloadKindDeployment) },
			terminate: false, want: false,
		},
		{
			name: "a terminating workload this Platform does not control is somebody else's",
			build: func() client.Object {
				d := ownedWorkload(p, otilmv1alpha1.WorkloadKindDeployment)
				d.SetOwnerReferences(nil)
				return d
			},
			terminate: true, want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := c.build()
			s := reconcileScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, obj).Build()
			r := &Reconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(8)}
			ctx := context.Background()
			if c.terminate {
				require.NoError(t, cl.Delete(ctx, obj))
			}

			got, err := r.terminatingOwnedWorkload(ctx, p)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestKindSwitchMarkerSurvivesADeRenderedTerminatingWorkload proves the whole point of the
// object-based half: the marker must NOT retire while a component that left the render is still
// being torn down, because the migration engine reads that marker to know a producer is
// mid-orchestration rather than genuinely stopped.
func TestKindSwitchMarkerSurvivesADeRenderedTerminatingWorkload(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns", UID: testPlatformUID},
	}
	setWorkloadKindSwitchCondition(p, metav1.ConditionTrue, reasonWorkloadKindSwitch,
		`workload "`+deRenderedWorkloadName+`" is being switched from a Deployment to a StatefulSet`)

	// The component is GONE from the render (provisioning flipped to external), so neither
	// render-based check can see it — but its Deployment is still terminating.
	gone := ownedWorkload(p, otilmv1alpha1.WorkloadKindDeployment)
	s := reconcileScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).
		WithObjects(p, gone).Build()
	r := &Reconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(8)}
	ctx := context.Background()
	require.NoError(t, cl.Delete(ctx, gone))

	outstanding, err := r.terminatingOwnedWorkload(ctx, p)
	require.NoError(t, err)
	require.True(t, outstanding, "the de-rendered workload is still terminating")

	assert.NotNil(t, meta.FindStatusCondition(p.Status.Conditions, conditionWorkloadKindSwitch))
}
