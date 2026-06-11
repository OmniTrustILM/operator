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

import (
	"context"
	"errors"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reconcileScheme is a scheme with the otilm + core/apps types the reconcile helpers
// touch (Platform, Secret, Deployment) plus the availability children the prune lists
// (policy/v1 PodDisruptionBudget, autoscaling/v2 HorizontalPodAutoscaler).
func reconcileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	return s
}

// genAdminPlatform is a source=generated Platform in namespace ns (composition active).
func genAdminPlatform(ns string) *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
		Spec: otilmv1alpha1.PlatformSpec{
			Database:      otilmv1alpha1.DatabaseSpec{Host: "db", Port: 5432, Name: "ilm"},
			Messaging:     otilmv1alpha1.MessagingSpec{Host: "mq", Port: 5672, VirtualHost: "ilm"},
			RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"}},
		},
	}
}

func TestAppendPEM(t *testing.T) {
	t.Run("nil + blob → blob", func(t *testing.T) {
		assert.Equal(t, []byte("X"), appendPEM(nil, []byte("X")))
	})
	t.Run("empty blob is ignored", func(t *testing.T) {
		assert.Equal(t, []byte("A"), appendPEM([]byte("A"), nil))
	})
	t.Run("inserts a separating newline when the bundle lacks a trailing one", func(t *testing.T) {
		assert.Equal(t, []byte("A\nB"), appendPEM([]byte("A"), []byte("B")))
	})
	t.Run("does not double the newline when one is already present", func(t *testing.T) {
		assert.Equal(t, []byte("A\nB"), appendPEM([]byte("A\n"), []byte("B")))
	})
}

// TestReconcileTrustedCertsNoComposition: when the operator does not compose (no admin
// CA / source=provided), it renders nothing and returns an empty checksum.
func TestReconcileTrustedCertsNoComposition(t *testing.T) {
	s := reconcileScheme(t)
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Common: otilmv1alpha1.CommonSpec{TrustedCertificates: otilmv1alpha1.TrustedCertificatesSpec{SecretRef: userTrustSecretRef}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s}

	sum, err := r.reconcileTrustedCerts(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.Empty(t, sum, "no composition → empty checksum")
}

// TestReconcileTrustedCertsComposesUserPlusAdminCA: with source=generated the bundle
// concatenates the user CA and the admin CA (read from admin-ca-keypair), SSA-applies
// the composed Secret, and returns a non-empty checksum.
func TestReconcileTrustedCertsComposesUserPlusAdminCA(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	p := genAdminPlatform(ns)
	p.Spec.Common.TrustedCertificates.SecretRef = userTrustSecretRef

	userTrust := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userTrustSecretRef, Namespace: ns},
		Data:       map[string][]byte{caCrtKey: []byte(userCAValue)},
	}
	// admin-ca-keypair is the dedicated admin CA the operator provisions for generated
	// standalone; cert-manager would populate ca.crt — model it as present here.
	adminCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-ca-keypair", Namespace: ns},
		Data:       map[string][]byte{caCrtKey: []byte("ADMIN-CA"), "tls.crt": []byte("ADMIN-LEAF")},
	}

	var applied *corev1.Secret
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(p, userTrust, adminCA).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if sec, ok := obj.(*corev1.Secret); ok {
					applied = sec.DeepCopy()
				}
				return nil // swallow the SSA apply
			},
		}).
		Build()
	r := &Reconciler{Client: c, Scheme: s}

	sum, err := r.reconcileTrustedCerts(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.NotEmpty(t, sum)
	require.NotNil(t, applied, "the composed Secret must be applied")
	bundle := string(applied.Data[caCrtKey])
	assert.Contains(t, bundle, userCAValue)
	assert.Contains(t, bundle, "ADMIN-CA", "the admin CA cert (ca.crt) must be folded in")
	assert.NotContains(t, bundle, "ADMIN-LEAF", "the admin leaf cert (tls.crt) must NOT be used as the CA")
}

// TestReconcileTrustedCertsMissingUserSecret: a failed read of the user trusted Secret
// returns an error that names only the Secret (no content).
func TestReconcileTrustedCertsMissingUserSecret(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	p := genAdminPlatform(ns)
	p.Spec.Common.TrustedCertificates.SecretRef = "absent-trust"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s}

	_, err := r.reconcileTrustedCerts(context.Background(), p, newDesiredSet())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent-trust", "error names the Secret")
	assert.NotContains(t, err.Error(), userCAValue)
}

// TestReconcileTrustedCertsAdminCANotYetIssued: when the admin CA keypair is absent
// (cert-manager race), composition tolerates it and composes the user CA only.
func TestReconcileTrustedCertsAdminCANotYetIssued(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	p := genAdminPlatform(ns)
	p.Spec.Common.TrustedCertificates.SecretRef = userTrustSecretRef
	userTrust := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userTrustSecretRef, Namespace: ns},
		Data:       map[string][]byte{caCrtKey: []byte(userCAValue)},
	}

	var applied *corev1.Secret
	c := fake.NewClientBuilder().
		WithScheme(s).WithObjects(p, userTrust).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if sec, ok := obj.(*corev1.Secret); ok {
					applied = sec.DeepCopy()
				}
				return nil
			},
		}).Build()
	r := &Reconciler{Client: c, Scheme: s}

	sum, err := r.reconcileTrustedCerts(context.Background(), p, newDesiredSet())
	require.NoError(t, err, "a not-yet-issued admin CA must not fail composition")
	assert.NotEmpty(t, sum)
	require.NotNil(t, applied)
	assert.Equal(t, userCAValue, string(applied.Data[caCrtKey]))
}

// TestDeploymentReady covers the measured-readiness primitive (workloadReady) on the
// Deployment path: AvailableReplicas vs the desired count, an explicit Available=False
// veto, and the absent/error cases. The StatefulSet fallback path is covered separately
// (TestWorkloadReadyStatefulSet).
func TestDeploymentReady(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	two := int32(2)
	newR := func(objs ...client.Object) *Reconciler {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return &Reconciler{Client: c, Scheme: s}
	}

	t.Run("absent Deployment → not ready, no error", func(t *testing.T) {
		ready, err := newR().workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("AvailableReplicas meets desired → ready", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Spec:       appsv1.DeploymentSpec{Replicas: &two},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 2},
		}
		ready, err := newR(dep).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.True(t, ready)
	})

	t.Run("AvailableReplicas below desired → not ready", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Spec:       appsv1.DeploymentSpec{Replicas: &two},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
		ready, err := newR(dep).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("replicas meet desired but Available=False vetoes → not ready", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Status: appsv1.DeploymentStatus{
				AvailableReplicas: 1, // desired defaults to 1
				Conditions:        []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse}},
			},
		}
		ready, err := newR(dep).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("a non-NotFound Get error is propagated", func(t *testing.T) {
		boom := errors.New("apiserver down")
		c := fake.NewClientBuilder().WithScheme(s).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return boom
				},
			}).Build()
		_, err := (&Reconciler{Client: c, Scheme: s}).workloadReady(context.Background(), ns, "core")
		require.Error(t, err)
	})
}

// TestWorkloadReadyStatefulSet covers the StatefulSet fallback path of workloadReady: when
// a component is rendered as a StatefulSet (no Deployment of that name), readiness is keyed
// off the StatefulSet's ReadyReplicas vs its desired count — so a StatefulSet-typed
// component gates Available exactly as a Deployment-typed one does.
func TestWorkloadReadyStatefulSet(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	two := int32(2)
	newR := func(objs ...client.Object) *Reconciler {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return &Reconciler{Client: c, Scheme: s}
	}

	t.Run("StatefulSet ReadyReplicas meets desired → ready", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Spec:       appsv1.StatefulSetSpec{Replicas: &two},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 2},
		}
		ready, err := newR(sts).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.True(t, ready, "a StatefulSet with ReadyReplicas>=desired must be ready")
	})

	t.Run("StatefulSet ReadyReplicas below desired → not ready", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Spec:       appsv1.StatefulSetSpec{Replicas: &two},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}
		ready, err := newR(sts).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("StatefulSet with default (unset) replicas needs 1 ready", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "core", Namespace: ns},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}
		ready, err := newR(sts).workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.True(t, ready)
	})

	t.Run("neither Deployment nor StatefulSet → not ready, no error", func(t *testing.T) {
		ready, err := newR().workloadReady(context.Background(), ns, "core")
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("requiredDeploymentsReady is satisfied by ready StatefulSets", func(t *testing.T) {
		p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns}}
		readySTS := func(name string) *appsv1.StatefulSet {
			return &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
			}
		}
		ready, err := newR(p, readySTS("core"), readySTS("auth")).requiredDeploymentsReady(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, ready, "the readiness gate must accept StatefulSet-typed required components")
	})
}

// TestRequiredDeploymentsReady asserts BOTH Core AND auth must be ready
// (Available is gated on the auth provider): one ready alone is not enough.
func TestRequiredDeploymentsReady(t *testing.T) {
	const ns = "ns"
	s := reconcileScheme(t)
	p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns}}
	readyDep := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
	}
	newR := func(objs ...client.Object) *Reconciler {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return &Reconciler{Client: c, Scheme: s}
	}

	t.Run("only Core ready → not ready (auth provider gates Available)", func(t *testing.T) {
		ready, err := newR(p, readyDep("core")).requiredDeploymentsReady(context.Background(), p)
		require.NoError(t, err)
		assert.False(t, ready)
	})
	t.Run("Core + auth ready → ready", func(t *testing.T) {
		ready, err := newR(p, readyDep("core"), readyDep("auth")).requiredDeploymentsReady(context.Background(), p)
		require.NoError(t, err)
		assert.True(t, ready)
	})
}

// TestSetReadinessStatus asserts the measured-readiness conditions/phase mapping.
func TestSetReadinessStatus(t *testing.T) {
	r := &Reconciler{}

	t.Run("ready → Running, Available=True, Progressing=False", func(t *testing.T) {
		p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
		r.setReadinessStatus(p, true)
		assert.Equal(t, otilmv1alpha1.PlatformPhaseRunning, p.Status.Phase)
		assert.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, conditionAvailable))
		assert.True(t, meta.IsStatusConditionFalse(p.Status.Conditions, conditionProgressing))
		// observedGeneration is stamped on each condition.
		assert.Equal(t, int64(3), meta.FindStatusCondition(p.Status.Conditions, conditionAvailable).ObservedGeneration)
	})

	t.Run("not ready → Progressing, Available=False, Progressing=True", func(t *testing.T) {
		p := &otilmv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		r.setReadinessStatus(p, false)
		assert.Equal(t, otilmv1alpha1.PlatformPhaseProgressing, p.Status.Phase)
		assert.True(t, meta.IsStatusConditionFalse(p.Status.Conditions, conditionAvailable))
		assert.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, conditionProgressing))
	})
}

// TestHandleDeletion asserts the deletion handler honors deletionPolicy (defaulting to
// Retain), emits a leak-free Event, and never blocks deletion (returns nil).
func TestHandleDeletion(t *testing.T) {
	s := reconcileScheme(t)

	run := func(policy otilmv1alpha1.PlatformDeletionPolicy) string {
		p := &otilmv1alpha1.Platform{
			ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
			Spec: otilmv1alpha1.PlatformSpec{
				Database:       otilmv1alpha1.DatabaseSpec{Host: "secret-db.example.com", Port: 5432, Name: "ilm"},
				Messaging:      otilmv1alpha1.MessagingSpec{Host: "mq", Port: 5672, VirtualHost: "ilm"},
				DeletionPolicy: policy,
			},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
		rec := record.NewFakeRecorder(8)
		r := &Reconciler{Client: c, Scheme: s, Recorder: rec}
		require.NoError(t, r.handleDeletion(context.Background(), p), "deletion must never block (returns nil)")
		select {
		case e := <-rec.Events:
			return e
		default:
			return ""
		}
	}

	t.Run("default (empty) policy treated as Retain in the Event", func(t *testing.T) {
		e := run("")
		assert.Contains(t, e, "deletionPolicy=Retain")
		assert.NotContains(t, e, "secret-db.example.com", "no connection coordinate may leak into the Event")
	})
	t.Run("explicit Delete policy surfaced in the Event", func(t *testing.T) {
		assert.Contains(t, run(otilmv1alpha1.PlatformDeletionPolicyDelete), "deletionPolicy=Delete")
	})
}
