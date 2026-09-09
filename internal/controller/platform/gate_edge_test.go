/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// programmableDetector returns the configured availability per API group. It lets
// gateEdge unit tests model cert-manager / Gateway API as present or absent without
// a live cluster, including a transient error.
type programmableDetector struct {
	available map[string]bool
	err       error
}

func (d programmableDetector) Available(gk schema.GroupKind, _ ...string) (bool, error) {
	if d.err != nil {
		return false, d.err
	}
	return d.available[gk.Group], nil
}

// gateEdgeScheme is a scheme with the otilm types plus the core/networking types the
// edge renders, so Reconciler.apply can resolve typed GVKs. (cert-manager / Gateway
// API objects are unstructured with a preset GVK, so they need no registration.)
func gateEdgeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	return s
}

// applyRecorder is a thread-safe sink for the GVKs Reconciler.apply submits via SSA.
// It intercepts Patch (the apply call) and returns nil so the fake client never has
// to actually persist unstructured cert-manager / Gateway API objects.
type applyRecorder struct {
	mu   sync.Mutex
	gvks []schema.GroupVersionKind
}

func (a *applyRecorder) record(obj client.Object) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gvks = append(a.gvks, obj.GetObjectKind().GroupVersionKind())
}

func (a *applyRecorder) groups() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]bool{}
	for _, gvk := range a.gvks {
		out[gvk.Group] = true
	}
	return out
}

// newGateEdgeReconciler builds a Reconciler whose client records (and swallows) SSA
// applies, with the given detector.
func newGateEdgeReconciler(t *testing.T, det capabilityDetector, p *otilmv1alpha1.Platform) (*Reconciler, *applyRecorder) {
	t.Helper()
	rec := &applyRecorder{}
	s := gateEdgeScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(p).
		WithStatusSubresource(p).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				rec.record(obj)
				return nil // swallow the SSA apply; we only assert which objects were submitted
			},
		}).
		Build()
	return &Reconciler{Client: c, Scheme: s, Capabilities: det}, rec
}

// ingressLetsEncryptPlatform is an enabled Ingress edge with letsEncrypt TLS (needs
// cert-manager), in a namespace so apply can stamp an owner ref.
func ingressLetsEncryptPlatform() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Edge: &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "ingress", Host: testEdgeHost,
				TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "letsEncrypt",
					LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "a@b.c", Environment: "staging"}},
			},
		},
	}
}

func edgeReadyCondition(t *testing.T, p *otilmv1alpha1.Platform) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, "EdgeReady")
}

func TestGateEdgeCertManagerPresentAppliesEdge(t *testing.T) {
	p := ingressLetsEncryptPlatform()
	det := programmableDetector{available: map[string]bool{certManagerGroup: true}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateEdge(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue, "with cert-manager present the edge applies and needs no requeue")

	// The edge objects — Ingress (networking) AND the ACME Issuer (cert-manager) — were
	// submitted to apply.
	groups := rec.groups()
	assert.True(t, groups["networking.k8s.io"], "the Ingress must be applied")
	assert.True(t, groups[certManagerGroup], "the cert-manager Issuer must be applied")

	cond := edgeReadyCondition(t, p)
	require.NotNil(t, cond, "EdgeReady condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Reconciled", cond.Reason)
}

func TestGateEdgeCertManagerAbsentSkipsEdge(t *testing.T) {
	p := ingressLetsEncryptPlatform()
	det := programmableDetector{available: map[string]bool{certManagerGroup: false}}
	r, rec := newGateEdgeReconciler(t, det, p)

	desired := newDesiredSet()
	requeue, err := r.gateEdge(context.Background(), p, desired)
	require.NoError(t, err, "a missing dependency is non-fatal; never a hard error")
	assert.True(t, requeue, "a missing dependency requests a requeue to self-heal")

	assert.Empty(t, rec.gvks, "NO edge object may be applied when cert-manager is absent (the Ingress would reference a never-populated Secret)")

	// The gate is still ACTIVE (the edge is enabled): its rendered objects must be
	// marked desired so the post-apply prune PRESERVES any already-applied copies across
	// a cert-manager flap — a transient outage must not delete a healthy Ingress.
	assert.NotEmpty(t, desired, "an active-but-waiting gate must keep its objects desired (prune-preservation)")

	cond := edgeReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "CertManagerNotInstalled", cond.Reason)
	assert.Contains(t, cond.Message, "spec.edge.tls.source=secret", "message must be actionable")
}

func TestGateEdgeGatewayAPIAbsentSkipsEdge(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Edge: &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "gatewayAPI", Host: testEdgeHost,
				GatewayAPI: &otilmv1alpha1.GatewayAPISpec{GatewayClassName: ptr("istio")},
				TLS:        &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: ptr("tls")},
			},
		},
	}
	// Gateway API absent (cert-manager irrelevant for BYO TLS).
	det := programmableDetector{available: map[string]bool{"gateway.networking.k8s.io": false}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateEdge(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Empty(t, rec.gvks, "no edge object may be applied when the Gateway API CRDs are absent")

	cond := edgeReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "GatewayAPINotInstalled", cond.Reason)
}

func TestGateEdgeSecretSourceNeedsNoCertManager(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Edge: &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "ingress", Host: testEdgeHost,
				TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: ptr("my-tls")},
			},
		},
	}
	// Everything absent: a BYO edge must still apply because it depends on nothing.
	det := programmableDetector{available: map[string]bool{}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateEdge(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue, "BYO TLS needs no upstream CRD, so the edge applies with no requeue")

	groups := rec.groups()
	assert.True(t, groups["networking.k8s.io"], "the Ingress must be applied")
	assert.False(t, groups[certManagerGroup], "no cert-manager object is rendered for BYO TLS")

	cond := edgeReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestGateEdgeDisabledClearsCondition(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec:       otilmv1alpha1.PlatformSpec{Edge: &otilmv1alpha1.EdgeSpec{Enabled: false}},
		Status: otilmv1alpha1.PlatformStatus{Conditions: []metav1.Condition{{
			Type: "EdgeReady", Status: metav1.ConditionFalse, Reason: "CertManagerNotInstalled",
			Message: "stale", LastTransitionTime: metav1.Now(),
		}}},
	}
	det := programmableDetector{available: map[string]bool{}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateEdge(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, rec.gvks, "a disabled edge applies nothing")
	assert.Nil(t, edgeReadyCondition(t, p), "a disabled edge drops any stale EdgeReady condition")
}

func TestGateEdgeTransientDetectionErrorRequeues(t *testing.T) {
	p := ingressLetsEncryptPlatform()
	det := programmableDetector{err: assertAnError()}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateEdge(context.Background(), p, newDesiredSet())
	require.NoError(t, err, "a transient detector error must NOT become a hard reconcile error")
	assert.True(t, requeue, "a transient detector error is treated as 'not yet available' and requeued")
	assert.Empty(t, rec.gvks, "no edge object is applied while detection is failing")

	cond := edgeReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "CertManagerNotInstalled", cond.Reason)
}

// TestGateEdgeNoLeak asserts the EdgeReady message carries no secret value or
// connection coordinate, for both reason codes.
func TestGateEdgeNoLeak(t *testing.T) {
	cases := []*otilmv1alpha1.Platform{
		ingressLetsEncryptPlatform(),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
			Spec: otilmv1alpha1.PlatformSpec{Edge: &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "gatewayAPI", Host: "secret-host.example.com",
				GatewayAPI: &otilmv1alpha1.GatewayAPISpec{GatewayClassName: ptr("istio")},
				TLS:        &otilmv1alpha1.EdgeTLSSpec{Source: "internal"},
			}},
		},
	}
	for _, p := range cases {
		det := programmableDetector{available: map[string]bool{}} // all absent
		r, _ := newGateEdgeReconciler(t, det, p)
		_, err := r.gateEdge(context.Background(), p, newDesiredSet())
		require.NoError(t, err)

		condBytes, err := json.Marshal(p.Status.Conditions)
		require.NoError(t, err)
		condStr := string(condBytes)
		assert.NotContains(t, condStr, p.Spec.Edge.Host, "edge host must not appear in conditions")
		if p.Spec.Edge.TLS != nil && p.Spec.Edge.TLS.LetsEncrypt != nil {
			assert.NotContains(t, condStr, p.Spec.Edge.TLS.LetsEncrypt.Email, "ACME email must not appear in conditions")
		}
	}
}

// adminGeneratedPlatform is an enabled registerAdmin with source=generated (needs
// cert-manager), in a namespace so apply can stamp an owner ref.
func adminGeneratedPlatform() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Username: "admin", Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"}},
		},
	}
}

func adminCertReadyCondition(t *testing.T, p *otilmv1alpha1.Platform) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, "AdminCertReady")
}

func TestGateAdminCertCertManagerPresentAppliesCert(t *testing.T) {
	p := adminGeneratedPlatform()
	det := programmableDetector{available: map[string]bool{certManagerGroup: true}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateAdminCert(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue, "with cert-manager present the admin cert applies and needs no requeue")

	// The admin cert-manager objects (Certificate + admin CA chain) were submitted.
	assert.True(t, rec.groups()[certManagerGroup], "the admin cert-manager objects must be applied")

	cond := adminCertReadyCondition(t, p)
	require.NotNil(t, cond, "AdminCertReady condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Reconciled", cond.Reason)
}

func TestGateAdminCertCertManagerAbsentSkipsCert(t *testing.T) {
	p := adminGeneratedPlatform()
	det := programmableDetector{available: map[string]bool{certManagerGroup: false}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateAdminCert(context.Background(), p, newDesiredSet())
	require.NoError(t, err, "a missing dependency is non-fatal; never a hard error")
	assert.True(t, requeue, "a missing dependency requests a requeue to self-heal")
	assert.Empty(t, rec.gvks, "NO admin cert object may be applied when cert-manager is absent")

	cond := adminCertReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "CertManagerNotInstalled", cond.Reason)
	assert.Contains(t, cond.Message, "spec.registerAdmin.source=provided", "message must be actionable")
}

func TestGateAdminCertProvidedNeedsNoCertManager(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided", SecretRef: ptr("admin-cert")}},
		},
	}
	// cert-manager absent: a provided admin cert renders nothing, so the gate is inactive.
	det := programmableDetector{available: map[string]bool{}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateAdminCert(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue, "source=provided renders no cert-manager objects, so no requeue")
	assert.Empty(t, rec.gvks, "source=provided applies no cert-manager objects")
	assert.Nil(t, adminCertReadyCondition(t, p), "source=provided sets no AdminCertReady condition")
}

func TestGateAdminCertDisabledClearsCondition(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec:       otilmv1alpha1.PlatformSpec{RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: false}},
		Status: otilmv1alpha1.PlatformStatus{Conditions: []metav1.Condition{{
			Type: "AdminCertReady", Status: metav1.ConditionFalse, Reason: "CertManagerNotInstalled",
			Message: "stale", LastTransitionTime: metav1.Now(),
		}}},
	}
	det := programmableDetector{available: map[string]bool{}}
	r, rec := newGateEdgeReconciler(t, det, p)

	requeue, err := r.gateAdminCert(context.Background(), p, newDesiredSet())
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Empty(t, rec.gvks, "a disabled admin bootstrap applies nothing")
	assert.Nil(t, adminCertReadyCondition(t, p), "a disabled admin bootstrap drops any stale AdminCertReady condition")
}

// ptr returns a pointer to v (for *string CR fields).
func ptr[T any](v T) *T { return &v }

// assertAnError returns a non-nil error sentinel for the transient-error path.
func assertAnError() error { return errSentinel }

var errSentinel = &simpleError{"discovery temporarily unavailable"}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
