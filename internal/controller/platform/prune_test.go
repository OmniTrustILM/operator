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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pruneScheme registers the otilm + typed kinds the prune lists (core/apps/networking),
// plus the unstructured cert-manager / Gateway API GVKs (singular + List) so the fake
// client can serve UnstructuredList for them in the unstructured-prune test.
func pruneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	for _, gvk := range unstructuredManagedGVKs() {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		s.AddKnownTypeWithName(gvk, u)
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), list)
	}
	return s
}

// allCapsDetector reports every GroupKind as served, so prune unit tests exercise the
// unstructured list path too (the fake client serves them once registered). For the
// missing-CRD case a noCapsDetector is used instead.
type allCapsDetector struct{}

func (allCapsDetector) Available(_ schema.GroupKind, _ ...string) (bool, error) { return true, nil }

// noCapsDetector reports every GroupKind as absent (no cert-manager / Gateway API),
// modelling a cluster where those CRDs are not served.
type noCapsDetector struct{}

func (noCapsDetector) Available(_ schema.GroupKind, _ ...string) (bool, error) { return false, nil }

// ownedLabels are the operator's selector labels for a child of platform p.
func ownedLabels(instance string) map[string]string {
	return map[string]string{
		common.NameLabel:      "x",
		common.InstanceLabel:  instance,
		common.ComponentLabel: "x",
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
}

// ownerRefTo builds a controller owner reference to platform p (UID-matched).
func ownerRefTo(p *otilmv1alpha1.Platform) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion: "otilm.com/v1alpha1", Kind: "Platform",
		Name: p.Name, UID: p.UID, Controller: &yes,
	}
}

// newPrunePlatform returns a Platform with a stable UID for owner-ref matching. ns is a
// parameter so each test names its own namespace, even though all currently use "ns".
//
//nolint:unparam // ns is intentionally a parameter for per-test namespacing
func newPrunePlatform(ns string) *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns, UID: types.UID("platform-uid")},
	}
}

// TestPruneDeletesOrphanedOwnedChild: a managed, owner-referenced Deployment absent from
// the desired set is deleted; one present in the desired set is kept.
func TestPruneDeletesOrphanedOwnedChild(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := newPrunePlatform(ns)

	keep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "core", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "utils", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, keep, orphan).Build()
	r := &Reconciler{Client: c, Scheme: s, Capabilities: allCapsDetector{}}

	// Desired set contains only "core" — utils was de-rendered.
	desired := newDesiredSet()
	keep.GetObjectKind().SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	desired.add(r, keep)

	require.NoError(t, r.pruneOrphans(context.Background(), p, desired))

	var got appsv1.Deployment
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Name: "utils", Namespace: ns}, &got)),
		"the de-rendered Deployment must be pruned")
	assert.NoError(t,
		c.Get(context.Background(), types.NamespacedName{Name: "core", Namespace: ns}, &got),
		"the still-desired Deployment must be kept")
}

// TestPruneDeletesOrphanedAvailabilityChildren: a de-configured PodDisruptionBudget and
// HorizontalPodAutoscaler (owner-referenced, absent from the desired set) are pruned, while
// ones still in the desired set are kept. This guards the availability-primitive teardown:
// disabling a component's PDB/HPA (or turning the HA profile off) reclaims them.
func TestPruneDeletesOrphanedAvailabilityChildren(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := newPrunePlatform(ns)

	keepPDB := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name: "core", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}
	orphanPDB := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name: "auth", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}
	orphanHPA := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
		Name: "core", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, keepPDB, orphanPDB, orphanHPA).Build()
	r := &Reconciler{Client: c, Scheme: s, Capabilities: allCapsDetector{}}

	// Desired set contains only the core PDB; the auth PDB and the core HPA were
	// de-configured this reconcile.
	desired := newDesiredSet()
	keepPDB.GetObjectKind().SetGroupVersionKind(policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"))
	desired.add(r, keepPDB)

	require.NoError(t, r.pruneOrphans(context.Background(), p, desired))

	var gotPDB policyv1.PodDisruptionBudget
	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "core", Namespace: ns}, &gotPDB),
		"the still-desired PDB must be kept")
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Name: "auth", Namespace: ns}, &gotPDB)),
		"the de-configured PDB must be pruned")
	var gotHPA autoscalingv2.HorizontalPodAutoscaler
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Name: "core", Namespace: ns}, &gotHPA)),
		"the de-configured HPA must be pruned")
}

// TestPruneSkipsObjectWithoutOwnerRef: (c) an object with matching labels but NO
// controller owner reference to this Platform is NEVER pruned, even when absent from the
// desired set — the owner-ref check is mandatory defense-in-depth.
func TestPruneSkipsObjectWithoutOwnerRef(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := newPrunePlatform(ns)

	// Matching labels, but no owner reference at all.
	unowned := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "stranger", Namespace: ns, Labels: ownedLabels(p.Name),
	}}
	// Matching labels, owner reference to a DIFFERENT Platform UID.
	other := newPrunePlatform(ns)
	other.UID = types.UID("a-different-uid")
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(other)},
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, unowned, foreign).Build()
	r := &Reconciler{Client: c, Scheme: s, Capabilities: allCapsDetector{}}

	// Empty desired set — nothing is desired, so only owner-ref scoping protects these.
	require.NoError(t, r.pruneOrphans(context.Background(), p, newDesiredSet()))

	var svc corev1.Service
	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "stranger", Namespace: ns}, &svc),
		"an object without a controller owner ref must NOT be pruned")
	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "foreign", Namespace: ns}, &svc),
		"an object owned by a DIFFERENT Platform must NOT be pruned")
}

// TestPruneSkipsMissingCRD: the unstructured cert-manager / Gateway API kinds are
// skipped (no error) when their CRDs are not served, so a cluster without them prunes
// the typed kinds normally and does not fail.
func TestPruneSkipsMissingCRD(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := newPrunePlatform(ns)

	orphan := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "stale-config", Namespace: ns, Labels: ownedLabels(p.Name),
		OwnerReferences: []metav1.OwnerReference{ownerRefTo(p)},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, orphan).Build()
	// noCapsDetector → cert-manager / Gateway API absent: those GVKs are skipped.
	r := &Reconciler{Client: c, Scheme: s, Capabilities: noCapsDetector{}}

	require.NoError(t, r.pruneOrphans(context.Background(), p, newDesiredSet()),
		"a missing upstream CRD must not error the prune")

	var cm corev1.ConfigMap
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Name: "stale-config", Namespace: ns}, &cm)),
		"the typed orphan is still pruned when upstream CRDs are absent")
}

// TestPruneDeletesOrphanedUnstructured: a managed, owner-referenced cert-manager
// Certificate absent from the desired set is pruned when its CRD is served (the
// unstructured list+delete path). This exercises the edge/admin-cert prune that fires
// when registerAdmin/edge change source.
func TestPruneDeletesOrphanedUnstructured(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := newPrunePlatform(ns)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{Group: certManagerGroup, Version: "v1", Kind: pruneKindCertificate})
	cert.SetName("admin-certificate")
	cert.SetNamespace(ns)
	cert.SetLabels(ownedLabels(p.Name))
	cert.SetOwnerReferences([]metav1.OwnerReference{ownerRefTo(p)})

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, cert).Build()
	r := &Reconciler{Client: c, Scheme: s, Capabilities: allCapsDetector{}}

	require.NoError(t, r.pruneOrphans(context.Background(), p, newDesiredSet()))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: certManagerGroup, Version: "v1", Kind: pruneKindCertificate})
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), types.NamespacedName{Name: "admin-certificate", Namespace: ns}, got)),
		"the de-rendered cert-manager Certificate must be pruned when its CRD is served")
}

// TestPlatformsForSecret: (e) a referenced-Secret change enqueues exactly the Platforms
// that reference it (by any of its referenced-Secret fields), and no others.
func TestPlatformsForSecret(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)

	refsDB := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: usesDBName, Namespace: ns},
		Spec:       otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "shared"}}},
	}
	refsAdmin := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "uses-admin", Namespace: ns},
		Spec: otilmv1alpha1.PlatformSpec{
			RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided", SecretRef: stringPtr("shared")}},
		},
	}
	unrelated := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: ns},
		Spec:       otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "other"}}},
	}
	otherNS := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: usesDBName, Namespace: "elsewhere"},
		Spec:       otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "shared"}}},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(refsDB, refsAdmin, unrelated, otherNS).Build()
	r := &Reconciler{Client: c, Scheme: s}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
	reqs := r.platformsForSecret(context.Background(), secret)

	names := map[string]bool{}
	for _, req := range reqs {
		assert.Equal(t, ns, req.Namespace, "only same-namespace Platforms are enqueued")
		names[req.Name] = true
	}
	assert.True(t, names[usesDBName], "a Platform referencing the Secret via database.credentials.secretRef must enqueue")
	assert.True(t, names["uses-admin"], "a Platform referencing the Secret via registerAdmin.secretRef must enqueue")
	assert.False(t, names["unrelated"], "a Platform referencing a different Secret must NOT enqueue")
	assert.Len(t, reqs, 2, "only the two same-namespace referencing Platforms are enqueued")
}

// TestReferencedSecretNames covers every referenced-Secret field the watch maps over.
func TestReferencedSecretNames(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Version:       platformVersion219,
			Common:        otilmv1alpha1.CommonSpec{TrustedCertificates: otilmv1alpha1.TrustedCertificatesSpec{SecretRef: "trust"}},
			Database:      otilmv1alpha1.DatabaseSpec{Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "db"}},
			Messaging:     otilmv1alpha1.MessagingSpec{Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "mq"}},
			Provisioning:  &otilmv1alpha1.ProvisioningSpec{APIKeySecretRef: "apikey"},
			RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Certificate: &otilmv1alpha1.AdminCertificateSpec{SecretRef: stringPtr("admincert")}},
			Edge:          &otilmv1alpha1.EdgeSpec{TLS: &otilmv1alpha1.EdgeTLSSpec{SecretRef: stringPtr("edgetls")}},
			Core: otilmv1alpha1.CoreSpec{
				TimeQualityMonitor: &otilmv1alpha1.TimeQualityMonitorSpec{
					Enabled:     true,
					Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "tqmonitor"},
				},
			},
		},
	}
	assert.ElementsMatch(t,
		[]string{"db", "mq", "trust", "apikey", "admincert", "edgetls", "tqmonitor"},
		referencedSecretNames(p),
		"the monitor sidecar's credentials Secret joins the watch when it renders")

	// Empty/unset refs are skipped, including a Platform whose monitor sidecar does not render.
	assert.Empty(t, referencedSecretNames(&otilmv1alpha1.Platform{}))
}

// TestReconcileMissingSecretReturnsNoError: (f) a missing referenced credential Secret
// yields phase Degraded with reason MissingSecret AND no returned error (a fixed
// requeue, not a backoff) — the deterministic steady state must not spam error logs.
// The condition message names only the Secret, never its content.
func TestReconcileMissingSecretReturnsNoError(t *testing.T) {
	const ns = "ns"
	s := pruneScheme(t)
	p := &otilmv1alpha1.Platform{
		// Pre-seed the finalizer so this single Reconcile call exercises the
		// missing-secret steady state directly: the first reconcile of a finalizer-less
		// Platform would otherwise just add the finalizer and return early.
		ObjectMeta: metav1.ObjectMeta{
			Name: "ilm", Namespace: ns, UID: types.UID("u"),
			Finalizers: []string{platformFinalizer},
		},
		Spec: otilmv1alpha1.PlatformSpec{
			Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "absent-db"}},
			Messaging: otilmv1alpha1.MessagingSpec{Host: "mq", VirtualHost: "ilm"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).
		WithStatusSubresource(&otilmv1alpha1.Platform{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Capabilities: allCapsDetector{}}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ilm", Namespace: ns}})
	require.NoError(t, err, "a missing referenced Secret is a deterministic steady state — NOT a returned error")
	assert.Positive(t, res.RequeueAfter, "the steady state requeues as a safety net")

	var got otilmv1alpha1.Platform
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "ilm", Namespace: ns}, &got))
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, got.Status.Phase)
	cond := metaFindDegraded(got.Status.Conditions)
	require.NotNil(t, cond)
	assert.Equal(t, "MissingSecret", cond.Reason)
	assert.Contains(t, cond.Message, "absent-db", "message names the missing Secret")
}

// metaFindDegraded returns the Degraded condition, or nil.
func metaFindDegraded(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == "Degraded" {
			return &conds[i]
		}
	}
	return nil
}
