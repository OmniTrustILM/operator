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
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Prune-related event reasons (names/reasons only — never secret material).
const (
	// reasonPruned is the Event reason recorded for each orphaned child the reconciler
	// garbage-collects (a child no longer in the desired render set).
	reasonPruned = "Pruned"
	// reasonPruneFailed is the Event reason recorded when deleting an orphaned child
	// fails (the reconcile requeues to retry).
	reasonPruneFailed = "PruneFailed"
)

// Upstream-CRD object Kinds the operator renders as unstructured and may prune. They
// mirror the builder-package kind constants (cert-manager.io Issuer/Certificate,
// gateway.networking.k8s.io Gateway/HTTPRoute); duplicated here so the prune's managed
// GVK list is self-contained (the builder's constants are unexported).
const (
	pruneKindIssuer         = "Issuer"
	pruneKindCertificate    = "Certificate"
	pruneKindGateway        = "Gateway"
	pruneKindHTTPRoute      = "HTTPRoute"
	pruneKindServiceMonitor = "ServiceMonitor"
)

// objectKey identifies one rendered object by GVK + namespace + name. It is the
// membership key of the desired set: an existing managed child whose key is NOT in the
// desired set (and which this Platform controller-owns) is an orphan to be pruned.
type objectKey struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

// desiredSet accumulates the objectKeys of every object the reconciler applies in a
// single reconcile. After a successful apply of the full desired state, pruneOrphans
// deletes managed children absent from this set.
type desiredSet map[objectKey]struct{}

// newDesiredSet returns an empty desired set.
func newDesiredSet() desiredSet { return desiredSet{} }

// add records an applied object's key in the desired set. The object MUST already have
// its GVK populated (Reconciler.apply sets it before SSA); when it is empty add falls
// back to the scheme so a typed object without a populated TypeMeta is still tracked.
// It is nil-safe (a no-op on a nil set) so callers need no guard.
func (d desiredSet) add(r *Reconciler, obj client.Object) {
	if d == nil {
		return
	}
	d[r.keyForObject(obj)] = struct{}{}
}

// has reports whether the given key is part of the desired set.
func (d desiredSet) has(k objectKey) bool {
	_, ok := d[k]
	return ok
}

// keyForObject resolves an object's GVK (preferring a populated TypeMeta, falling back
// to the scheme for a typed object whose TypeMeta is empty) and returns its objectKey.
func (r *Reconciler) keyForObject(obj client.Object) objectKey {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		if gvks, _, err := r.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
			gvk = gvks[0]
		}
	}
	return objectKey{gvk: gvk, namespace: obj.GetNamespace(), name: obj.GetName()}
}

// typedManagedLists returns one empty List per built-in (scheme-registered) kind the
// Platform controller owns and may therefore prune. The prune lists each in the
// Platform's namespace with the operator's label selector and deletes controller-owned
// members absent from the desired set. delete on each of these is granted by the
// controller's RBAC markers.
func typedManagedLists() []client.ObjectList {
	return []client.ObjectList{
		&appsv1.DeploymentList{},
		// StatefulSets: a component rendered as a StatefulSet (workloadType=StatefulSet) is
		// pruned like a Deployment. A workloadType FLIP (Deployment<->StatefulSet, same name,
		// different GVK — they do not collide) is NOT reclaimed here: the controller stops the
		// superseded kind itself, stop-before-start (see stopSupersededWorkload), and keeps it in
		// the desired set for as long as the switch runs — so this list only ever prunes a
		// workload that is genuinely de-rendered (the component removed, not merely re-kinded).
		&appsv1.StatefulSetList{},
		&corev1.ServiceList{},
		&corev1.ServiceAccountList{},
		&corev1.ConfigMapList{},
		&corev1.SecretList{},
		&networkingv1.IngressList{},
		// NetworkPolicies (networking.k8s.io/v1 is an always-served core API): the
		// platform's default-deny policies are pruned when spec.networkPolicy is disabled
		// (or the policy set changes). delete is granted by the controller's RBAC marker.
		&networkingv1.NetworkPolicyList{},
		// Availability children (policy/v1 + autoscaling/v2 are always-served core APIs):
		// a per-component PDB/HPA is pruned when a component de-configures it (or HA is
		// turned off). delete on both is granted by the controller's RBAC markers.
		&policyv1.PodDisruptionBudgetList{},
		&autoscalingv2.HorizontalPodAutoscalerList{},
	}
}

// unstructuredManagedGVKs are the upstream-CRD (cert-manager / Gateway API) kinds the
// Platform controller renders as unstructured and may prune. They are listed via an
// unstructured list with the GVK set; a GVK whose CRD is not served is skipped (the
// capability detector + meta.IsNoMatchError), so a cluster without cert-manager /
// Gateway API does not error the prune. The List GVKs carry the "List" kind suffix.
//
// DELETION SAFETY — the managed-infrastructure kinds (CloudNativePG database, the RabbitMQ
// broker, AND the managed Keycloak: postgresql.cnpg.io, rabbitmq.com and k8s.keycloak.org)
// are DELIBERATELY EXCLUDED: those CRs carry NO controller owner reference and are tracked
// only by the managed-by/instance labels, so a transient de-render (e.g. a momentary read of
// the CR mid-edit) must NEVER cause the prune to delete the database/broker/Keycloak and its
// data. Managed CRs are torn down ONLY by handleDeletion under spec.deletionPolicy=Delete —
// never here. Do NOT add postgresql.cnpg.io, rabbitmq.com or k8s.keycloak.org kinds to this
// list.
func unstructuredManagedGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{Group: "cert-manager.io", Version: "v1", Kind: pruneKindIssuer},
		{Group: "cert-manager.io", Version: "v1", Kind: pruneKindCertificate},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: pruneKindGateway},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: pruneKindHTTPRoute},
		// ServiceMonitor (Prometheus operator): per-component ServiceMonitors are pruned
		// when a component disables metrics.serviceMonitor. Listed via the unstructured
		// path so a cluster WITHOUT the Prometheus operator CRD is skipped (never errors),
		// exactly like cert-manager / Gateway API.
		{Group: "monitoring.coreos.com", Version: "v1", Kind: pruneKindServiceMonitor},
	}
}

// pruneOrphans garbage-collects managed children that are no longer desired. For every
// managed kind it lists objects in the Platform's namespace matching the operator's
// label selector ({managed-by: ilm-operator, instance: <platform>}), and deletes each
// listed object that (a) is controller-owned by THIS Platform and (b) is NOT in the
// desired set. The owner-ref check is mandatory defense-in-depth: a label match alone
// never authorizes a delete.
//
// It is called ONLY after a successful apply of the full desired state, so a child
// absent from the desired set is genuinely de-rendered (a disabled component/edge or a
// changed edge type/source), not merely an object this reconcile failed to apply.
//
// Behaviour:
//   - typed kinds (Deployment/Service/ServiceAccount/ConfigMap/Secret/Ingress): always
//     listed (scheme-registered, always served).
//   - unstructured upstream-CRD kinds (cert-manager Issuer/Certificate, Gateway API
//     Gateway/HTTPRoute): listed only when the CRD is served; a missing CRD is skipped
//     (never an error) via the capability detector.
//   - a prune Delete failure requeues (returned as an error) rather than crashing; an
//     already-gone object (NotFound) is ignored.
//
// SECURITY: scoped strictly to the controlled kind list + owner-ref + label selector;
// it never lists or deletes cluster-scoped or arbitrary kinds. Events carry only the
// pruned object's kind/name (no secret material).
func (r *Reconciler) pruneOrphans(ctx context.Context, p *otilmv1alpha1.Platform, desired desiredSet) error {
	selector := client.MatchingLabels{
		common.ManagedByLabel: common.ManagedByValue,
		common.InstanceLabel:  p.Name,
	}

	for _, list := range typedManagedLists() {
		if err := r.pruneTypedList(ctx, p, list, selector, desired); err != nil {
			return err
		}
	}

	for _, gvk := range unstructuredManagedGVKs() {
		if err := r.pruneUnstructuredGVK(ctx, p, gvk, selector, desired); err != nil {
			return err
		}
	}
	return nil
}

// pruneTypedList lists one typed managed kind and prunes its orphans.
func (r *Reconciler) pruneTypedList(ctx context.Context, p *otilmv1alpha1.Platform, list client.ObjectList, selector client.MatchingLabels, desired desiredSet) error {
	if err := r.List(ctx, list, client.InNamespace(p.Namespace), selector); err != nil {
		return fmt.Errorf("listing %T for prune: %w", list, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return fmt.Errorf("extracting %T for prune: %w", list, err)
	}
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}
		if err := r.pruneIfOrphan(ctx, p, obj, desired); err != nil {
			return err
		}
	}
	return nil
}

// pruneUnstructuredGVK lists one upstream-CRD managed kind (unstructured) and prunes its
// orphans, skipping the kind entirely when its CRD is not served by the cluster.
func (r *Reconciler) pruneUnstructuredGVK(ctx context.Context, p *otilmv1alpha1.Platform, gvk schema.GroupVersionKind, selector client.MatchingLabels, desired desiredSet) error {
	// Skip a kind whose CRD is absent: the capability detector reports it as not
	// available (meta.IsNoMatchError folded to false), so a cluster without
	// cert-manager / Gateway API does not error the prune.
	available, derr := r.Capabilities.Available(gvk.GroupKind(), gvk.Version)
	if derr != nil {
		log.FromContext(ctx).Info("prune: CRD detection failed; skipping kind this reconcile",
			"groupVersionKind", gvk.String(), "err", derr.Error())
		return nil
	}
	if !available {
		return nil
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(gvk)
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace), selector); err != nil {
		// A CRD removed between detection and List surfaces as a NoMatch — treat it as
		// "absent now" rather than failing the prune.
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("listing %s for prune: %w", gvk.String(), err)
	}
	for i := range list.Items {
		if err := r.pruneIfOrphan(ctx, p, &list.Items[i], desired); err != nil {
			return err
		}
	}
	return nil
}

// pruneIfOrphan deletes obj when it is controller-owned by THIS Platform AND its key is
// absent from the desired set. A label match alone is never sufficient — the
// controller-owner check is mandatory defense-in-depth so the operator never deletes an
// object it does not own. A NotFound on delete is ignored (already gone); any other
// delete error is returned so the reconcile requeues.
func (r *Reconciler) pruneIfOrphan(ctx context.Context, p *otilmv1alpha1.Platform, obj client.Object, desired desiredSet) error {
	if !controllerOwnedBy(obj, p.UID) {
		return nil // not ours — never delete (defense-in-depth)
	}
	if obj.GetDeletionTimestamp() != nil {
		// Already terminating. Re-issuing DELETE here would re-evaluate its propagation policy
		// and can strip a foregroundDeletion finalizer the deleter relies on to keep the object
		// alive until its pods are gone. NOTE this is a BEST-EFFORT guard: the listed object
		// comes from the manager's cache, so a very recent delete may not show a timestamp yet.
		// The workload-kind switch does not depend on it — it keeps the superseded object in the
		// desired set instead, which this function honours before it gets here.
		return nil
	}
	key := r.keyForObject(obj)
	if desired.has(key) {
		return nil // still desired
	}

	// Resolve the Kind via keyForObject's GVK (which falls back to the scheme) so the
	// Event/log message carries a real Kind even when a listed typed object has empty
	// TypeMeta. Names/Kinds only — never any secret material.
	kind := key.gvk.Kind
	if err := r.Delete(ctx, obj, pruneDeleteOptions(obj)...); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		r.eventf(p, corev1.EventTypeWarning, reasonPruneFailed, "failed to prune %s %q", kind, obj.GetName())
		return fmt.Errorf("pruning %s %q: %w", kind, obj.GetName(), err)
	}
	r.eventf(p, corev1.EventTypeNormal, reasonPruned, "pruned %s %q (no longer in desired state)", kind, obj.GetName())
	log.FromContext(ctx).Info("pruned orphaned child", "kind", kind, "name", obj.GetName())
	return nil
}

// pruneDeleteOptions returns the delete options for one orphan: FOREGROUND propagation for the
// two apps/v1 WORKLOAD kinds, and the apiserver's own default for everything else.
//
// The default for a Deployment or a StatefulSet is BACKGROUND — the workload object disappears
// at once and its ReplicaSet and pods are reclaimed afterwards, on the garbage collector's own
// schedule. For a plain de-render that is harmless. It is not harmless when the component comes
// BACK: stopSupersededWorkload decides there is nothing to stop by asking whether the other
// kind's object exists, so a component disabled and immediately re-enabled AS THE OTHER KIND
// finds no object to wait on and starts the new workload beside pods the old one has not
// finished terminating — two schedulers publishing the same jobs, two Cores against one
// database. Foreground propagation keeps the object alive until its pods are gone, which is the
// same guarantee stop-before-start already chose for itself, so both routes to a stopped
// workload mean the same thing.
//
// It is scoped to workloads because they are the kinds that OWN pods: a Service or a ConfigMap
// has no dependents whose lifetime the propagation policy could change.
func pruneDeleteOptions(obj client.Object) []client.DeleteOption {
	if workloadKindOf(obj) == "" {
		return nil
	}
	return []client.DeleteOption{client.PropagationPolicy(metav1.DeletePropagationForeground)}
}

// controllerOwnedBy reports whether obj has a controller owner reference whose UID
// matches the given Platform UID — i.e. THIS Platform controls it. Matching on UID
// (not name) is precise across recreation of a same-named Platform.
func controllerOwnedBy(obj metav1.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}
