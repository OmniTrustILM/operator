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
	"encoding/json"
	"sort"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Keycloak gating condition type and reasons. KeycloakReady is an ADJUNCT signal (like
// DatabaseReady / MessagingReady / EdgeReady): it reports the managed Keycloak's
// provisioning state without ever blocking the platform's Available condition. The OIDC
// wiring reads back the generated initial-admin Secret to drive Keycloak's admin API;
// provisioning itself only needs the Keycloak Operator CRDs + the shared platform database.
// Reasons carry no identity material.
const (
	// conditionKeycloakReady reports whether the operator-provisioned (managed) Keycloak is
	// provisioned and its Keycloak CR reports Ready. It is set only for a managed Keycloak; an
	// external Keycloak drops it (nothing to report).
	conditionKeycloakReady = "KeycloakReady"
	// reasonWaitingForKeycloak: the k8s.keycloak.org CRDs are served and the Keycloak CR is
	// applied, but it is not yet Ready. A transient, self-healing waiting state (requeue).
	reasonWaitingForKeycloak = "WaitingForKeycloak"
	// reasonKeycloakReady: the managed Keycloak CR reports Ready.
	reasonKeycloakReady = "Reconciled"
	// reasonRealmImportConfigMapMissing: a realm import is configured but its ConfigMap is not
	// found. Non-fatal: the Keycloak itself still provisions; the import is deferred and the
	// reconcile requeues so it converges once the ConfigMap is created.
	reasonRealmImportConfigMapMissing = "RealmImportConfigMapMissing"
)

// keycloakRequeueAfter is how long the reconciler waits before re-checking a managed
// Keycloak that is applied but not yet Ready (the Keycloak CR is still provisioning), or
// whose realm-import ConfigMap is not yet present. No direct Watch on the Keycloak types is
// possible (it would break the cache when the CRD is absent), so this requeue backstop —
// together with the Secret watch — is how the managed Keycloak converges, mirroring
// databaseRequeueAfter / messagingRequeueAfter.
const keycloakRequeueAfter = 15 * time.Second

// gateKeycloak reconciles the managed Keycloak (a Keycloak CR sharing the platform database)
// and reflects its state on the KeycloakReady condition. It is the Keycloak sibling of
// gateDatabase / gateMessaging built on the SAME shared gateManagedInfra machinery: the
// Keycloak CR is provisioned + readiness-probed through the shared path, and the optional
// create-only realm import is handled as an adjunct step afterwards (it reads a user
// ConfigMap and inlines the realm representation ONCE — it does not churn every reconcile).
//
// Returns ready=true only when external (nothing to provision) OR a managed Keycloak whose
// CR reports Ready. requeue=true asks the caller to re-check soon. An error is returned only
// for an actual apply failure or a rejected override (a missing CRD or a not-yet-Ready
// Keycloak is non-fatal, like the database/messaging/edge).
//
// SECURITY: the condition message names only the spec field and the remedy — never a secret
// value or a connection coordinate.
func (r *Reconciler) gateKeycloak(ctx context.Context, p *otilmv1alpha1.Platform, bundle bom.Bundle, desired desiredSet, dbReady bool) (ready bool, requeue bool, err error) {
	// The managed Keycloak shares the platform database. Defer provisioning its Keycloak CR until
	// the database is ready, so the Keycloak Operator's StatefulSet does not start and crash-loop
	// against a Postgres that is not yet accepting connections. dbReady is true for an external
	// database (nothing to wait on) and for a Ready managed CNPG cluster; external Keycloak is not
	// managed and falls through (the shared gate simply drops the condition). The rendered objects
	// are marked desired so the prune preserves an already-applied Keycloak across a DB flap.
	if platformbuilder.KeycloakManaged(p) && !dbReady {
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: conditionKeycloakReady, Status: metav1.ConditionFalse, Reason: reasonWaitingForDatabase,
			Message:            "waiting for the platform database to become ready before provisioning the managed Keycloak",
			ObservedGeneration: p.Generation,
		})
		for _, obj := range platformbuilder.ResolveManagedKeycloak(p) {
			desired.add(r, obj)
		}
		return false, true, nil
	}

	// Provision + probe the Keycloak CR through the shared machinery (CRD-absent → False/
	// KeycloakOperatorNotInstalled + requeue; applied-but-not-Ready → False/WaitingForKeycloak
	// + requeue; Ready → True). The gating descriptor applies ONLY the Keycloak CR — the realm
	// import is create-only and handled below, never re-applied each reconcile. The major-
	// version upgrade guard is attached here (the deletion path's descriptor needs none).
	gate := r.keycloakProvisionGate(p)
	gate.versionGuard = keycloakVersionGuard(p, bundle)
	ready, requeue, err = r.gateManagedInfra(ctx, p, desired, gate)
	if err != nil || !ready {
		return ready, requeue, err
	}

	// Keycloak CR is Ready: import the realm ONCE (create-only) from the user ConfigMap. A
	// missing ConfigMap is non-fatal (a clear KeycloakReady-adjacent reason + requeue), and a
	// successful import is never re-applied (so a user editing the realm in Keycloak is not
	// clobbered). This is the one Keycloak-specific step beyond the shared gate.
	importRequeue, ierr := r.reconcileKeycloakRealmImport(ctx, p)
	if ierr != nil {
		return false, false, ierr
	}
	return true, requeue || importRequeue, nil
}

// keycloakProvisionGate describes the managed Keycloak CR (Keycloak Operator) for the shared
// gateManagedInfra path: its predicate, the Keycloak CR to render+apply, the render-error
// accessor, the Keycloak CRD dependency, the Keycloak-CR Ready probe, and the KeycloakReady
// condition vocabulary. It is the deliberate sibling of databaseGate / messagingGate (the
// shared shape is the point). Note its `objects` carries ONLY the Keycloak CR (NOT the realm
// import) — the import is create-only and handled by reconcileKeycloakRealmImport; the
// deletion path uses the FULL ResolveManagedKeycloak set via keycloakDeletionGate.
//
//nolint:dupl // deliberate per-component descriptor sibling of databaseGate/messagingGate
func (r *Reconciler) keycloakProvisionGate(p *otilmv1alpha1.Platform) managedInfraGate {
	return managedInfraGate{
		managed:      platformbuilder.KeycloakManaged(p),
		kind:         "keycloak",
		name:         platformbuilder.ManagedKeycloakName(p),
		objects:      []client.Object{keycloakCRObject(p)},
		renderError:  platformbuilder.ManagedKeycloakRenderError,
		dependencies: platformbuilder.KeycloakDependencies(p),
		// Ready when the Keycloak CR reports Ready. There is no operator-generated Secret to
		// gate provisioning on (the initial-admin Secret is consumed by the OIDC wiring, not by
		// provisioning), so the readiness probe is the cluster signal alone (secretPresent is a
		// constant true).
		ready: func(ctx context.Context) bool {
			return managedReadyProbe(ctx,
				func(ctx context.Context) (bool, error) { return r.managedKeycloakReady(ctx, p) },
				func(context.Context) bool { return true })
		},
		conditionType: conditionKeycloakReady,
		waitingReason: reasonWaitingForKeycloak,
		// Actionable: the most common reason a managed Keycloak never becomes ready is that the
		// Keycloak Operator (namespace-scoped by default, unlike CloudNativePG/RabbitMQ which are
		// cluster-scoped) is not watching this namespace, so its Keycloak CR is never reconciled.
		// The hint names the remedy without leaking any coordinate. (No namespace value is
		// interpolated — the message must stay leak-free and the namespace is the Platform's own.)
		waitingMessage: "waiting for the managed Keycloak to become ready; if it never provisions, " +
			"ensure a Keycloak Operator watches this namespace (it is namespace-scoped by default, " +
			"unlike CloudNativePG/RabbitMQ)",
		readyReason:    reasonKeycloakReady,
		readyMessage:   "managed Keycloak reconciled",
		detectionLabel: "keycloak",
	}
}

// keycloakVersionGuard builds the major-version upgrade guard descriptor for the managed
// Keycloak (nil when external). The running version is read from the live Keycloak CR's
// spec.image; the reference version is spec.keycloak.managed.version, or the bundle's
// KeycloakVersion when the CR pins none. A Keycloak major bump runs a one-way realm/DB
// migration and must stay aligned with the Keycloak Operator, so it is guarded too.
func keycloakVersionGuard(p *otilmv1alpha1.Platform, bundle bom.Bundle) *infraVersionGuard {
	if !platformbuilder.KeycloakManaged(p) {
		return nil
	}
	return &infraVersionGuard{
		clusterGVK:       platformbuilder.ManagedKeycloakGVK(),
		clusterName:      platformbuilder.ManagedKeycloakName(p),
		imageFieldPath:   platformbuilder.ManagedKeycloakImageFieldPath(),
		versionFromImage: platformbuilder.ManagedKeycloakVersionFromImage,
		desiredVersion:   platformbuilder.ManagedKeycloakDesiredVersion(p),
		bundleVersion:    bundle.KeycloakVersion,
		acknowledged:     p.Spec.Keycloak.Managed.UpgradeAcknowledged,
		conditionPrefix:  "Keycloak",
		upstreamLabel:    "Keycloak",
		ackFieldPath:     "spec.keycloak.managed.upgradeAcknowledged",
	}
}

// keycloakDeletionGate describes the managed Keycloak for the shared
// handleManagedInfraDeletion path. Unlike keycloakProvisionGate it carries the FULL rendered
// set (the Keycloak CR AND the realm import) so Delete reclaims both and Retain leaves both.
func (r *Reconciler) keycloakDeletionGate(p *otilmv1alpha1.Platform) managedInfraGate {
	return managedInfraGate{
		managed: platformbuilder.KeycloakManaged(p),
		kind:    "keycloak",
		name:    platformbuilder.ManagedKeycloakName(p),
		objects: platformbuilder.ResolveManagedKeycloak(p),
	}
}

// keycloakCRObject returns just the Keycloak CR from the rendered set (the first object;
// ResolveManagedKeycloak always renders the Keycloak CR first, then the optional import).
func keycloakCRObject(p *otilmv1alpha1.Platform) client.Object {
	objs := platformbuilder.ResolveManagedKeycloak(p)
	if len(objs) == 0 {
		return nil
	}
	return objs[0]
}

// managedKeycloakReady reports whether the managed Keycloak CR is Ready. It GETs the
// Keycloak CR as unstructured (its GVK is preset; the type is not registered in the scheme)
// and checks the Keycloak Operator's documented readiness signal:
//   - status.conditions[type=Ready].status == "True".
//
// A NotFound (the Keycloak CR was just applied and has no status yet, or detection is
// momentarily behind) is "not ready". The Keycloak CR exposes a
// status condition of type "Ready" whose status is the string "True"/"False" (the e2e observed
// it transition False→True as Keycloak booted against the shared CNPG DB).
func (r *Reconciler) managedKeycloakReady(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ManagedKeycloakName(p)}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return false, nil
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == conditionTypeReady && keycloakConditionStatusTrue(cm["status"]) {
			return true, nil
		}
	}
	return false, nil
}

// keycloakConditionStatusTrue reports whether a Keycloak status-condition "status" value
// represents True. The v2beta1 API (like v2alpha1) uses a STRING status
// ("True"/"False"/"Unknown", per the kstatus-conformance change in keycloak#13074) — the
// string-"True" match is the live path (confirmed against the served v2beta1 status). The
// boolean fallback is kept defensively for older releases that emitted a boolean status.
func keycloakConditionStatusTrue(status interface{}) bool {
	switch s := status.(type) {
	case string:
		return s == string(metav1.ConditionTrue)
	case bool:
		return s
	default:
		return false
	}
}

// reconcileKeycloakRealmImport imports the platform realm into the managed Keycloak ONCE
// (create-only). The operator ALWAYS imports a realm for a managed Keycloak so OIDC works out
// of the box: by default the operator's VERSION-BUNDLED realm (DefaultKeycloakRealm), which
// DEFINES the confidential "ilm" OIDC client Core uses — Keycloak generates that client's
// secret, which reconcileOIDCProvider later reads back via the admin API and wires to Core.
// When the caller supplies their own realm ConfigMap (keycloak.managed.realmImport), the
// user's representation REPLACES the default. Either way the import is applied only if it does
// not already exist — so a realm a user later edits in Keycloak is never clobbered by a
// re-import.
//
// Returns requeue=true (non-fatal) when a CONFIGURED user ConfigMap is not yet present: a
// missing ConfigMap is surfaced as a clear KeycloakReady-adjacent reason + requeue (it
// converges once the ConfigMap is created). It returns an error only for a non-NotFound API
// failure or a malformed realm JSON (a deterministic user mistake). With NO ConfigMap
// configured the default realm imports immediately (no requeue).
//
// SECURITY: the realm representation is data only (no client secret — Keycloak generates the
// "ilm" client secret); it is applied to the import object's spec and never logged or placed
// in status/conditions/events. Errors name only the ConfigMap / key, never its contents.
func (r *Reconciler) reconcileKeycloakRealmImport(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	// Only a MANAGED Keycloak has a realm to import. External Keycloak (gateKeycloak treats it
	// as ready with nothing to provision and falls through to here) configures OIDC in the
	// application database and has no KeycloakRealmImport — so bail before touching that GVK
	// (otherwise the GET below fails with "no matches for kind KeycloakRealmImport" on a cluster
	// that does not serve the Keycloak Operator CRDs, which an external-Keycloak platform never
	// requires).
	if !platformbuilder.KeycloakManaged(p) {
		return false, nil
	}

	importName := platformbuilder.ManagedKeycloakName(p) + "-realm"

	// Create-only: if the import already exists, do nothing (do not re-import / churn). The
	// import object's GVK is preset on the unstructured GET.
	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: importName}, &existing); err == nil {
		return false, nil // already imported; create-only — leave it
	} else if !apierrors.IsNotFound(err) {
		return false, err // transient API error: requeue via the returned error/backoff
	}

	// Resolve the realm representation to import. With a user ConfigMap it is the user's realm
	// (read + parsed below); without one it is the operator's default bundled realm (with the
	// "ilm" client). A missing-but-configured ConfigMap is non-fatal (defer + requeue).
	realm, requeue, err := r.resolveRealmRepresentation(ctx, p)
	if err != nil || requeue || realm == nil {
		return requeue, err
	}

	// Build the import with the realm representation inlined. The operator stamps the realm
	// NAME so the import is self-consistent even if the user's JSON omits/differs on it.
	imp := &unstructured.Unstructured{}
	imp.SetGroupVersionKind(platformbuilder.ManagedKeycloakRealmImportGVK())
	imp.SetName(importName)
	imp.SetNamespace(p.Namespace)
	imp.SetLabels(platformbuilder.ManagedKeycloakLabels(p))
	if _, hasRealmName := realm["realm"]; !hasRealmName {
		realm["realm"] = platformbuilder.KeycloakRealmName(p)
	}
	// Set the import's spec fields, FAILING (not swallowing) on a structural error — a
	// SetNested* error means the realm representation is not deep-copyable into the object and
	// the import would silently lose data. Surfacing it as a reconcile error is the safe choice.
	if serr := unstructured.SetNestedField(imp.Object, platformbuilder.ManagedKeycloakName(p), "spec", "keycloakCRName"); serr != nil {
		return false, serr
	}
	if serr := unstructured.SetNestedMap(imp.Object, realm, "spec", "realm"); serr != nil {
		return false, serr
	}

	// Diagnostic (leak-free): log which realm keys + how many clients we are ABOUT to apply, so
	// a "realm imported but OIDC never closes" symptom is debuggable from operator logs without
	// dumping the object. The realm representation carries NO secret (the ilm client's secret is
	// omitted so Keycloak generates it), so the key set + client count are safe to log.
	log.FromContext(ctx).V(1).Info("applying keycloak realm import (create-only)",
		"name", importName, "realmKeys", sortedRealmKeys(realm), "clients", realmClientCount(realm))

	// Create-only apply (no owner reference — managed CRs follow the deletion-safety
	// contract): a plain Create so a later user edit is never overwritten. A concurrent
	// create racing in is treated as "already imported".
	if err := r.Create(ctx, imp); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	log.FromContext(ctx).Info("imported keycloak realm (create-only)", "name", p.Name)
	return false, nil
}

// sortedRealmKeys returns the realm representation's top-level keys, sorted, for a stable
// leak-free diagnostic log. It names only the realm's STRUCTURE (e.g. realm, enabled,
// clients), never any value.
func sortedRealmKeys(realm map[string]interface{}) []string {
	keys := make([]string, 0, len(realm))
	for k := range realm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// realmClientCount returns how many entries the realm's clients array holds (0 when absent),
// for the leak-free import diagnostic. A count of 0 is the bug signature that the imported
// realm lacks the ilm OIDC client.
func realmClientCount(realm map[string]interface{}) int {
	clients, _ := realm["clients"].([]interface{})
	return len(clients)
}

// resolveRealmRepresentation returns the realm representation to import for the managed
// Keycloak. When the caller supplies a realm ConfigMap (keycloak.managed.realmImport) it reads
// + parses that representation; otherwise it returns the operator's default bundled realm
// (DefaultKeycloakRealm, with the confidential "ilm" client). A configured-but-missing
// ConfigMap returns requeue=true (non-fatal, surfaced as a clear condition + Warning). A
// missing key or malformed JSON returns a deterministic user error.
//
// SECURITY: errors name only the ConfigMap / key, never its contents; the returned realm is
// data only (no client secret — Keycloak generates the "ilm" client secret on import).
func (r *Reconciler) resolveRealmRepresentation(ctx context.Context, p *otilmv1alpha1.Platform) (realm map[string]interface{}, requeue bool, err error) {
	cmName, cmKey := platformbuilder.KeycloakRealmImportConfigMap(p)
	if cmName == "" {
		// No user ConfigMap: import the operator's default bundled realm (with the ilm client),
		// so managed-Keycloak OIDC works out of the box.
		return platformbuilder.DefaultKeycloakRealm(p), false, nil
	}

	// Read the realm representation from the user ConfigMap. A missing ConfigMap is non-fatal:
	// surface a clear reason + requeue so it converges once the user creates it.
	var configMap corev1.ConfigMap
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: cmName}, &configMap); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type: conditionKeycloakReady, Status: metav1.ConditionFalse, Reason: reasonRealmImportConfigMapMissing,
				Message:            "waiting for the realm-import ConfigMap referenced by keycloak.managed.realmImport.configMapRef",
				ObservedGeneration: p.Generation, // names the spec field only — no contents
			})
			r.event(p, corev1.EventTypeWarning, reasonRealmImportConfigMapMissing,
				"realm-import ConfigMap not found; the managed Keycloak is provisioned but the realm import is deferred")
			log.FromContext(ctx).Info("keycloak realm-import ConfigMap not yet present; deferring import", "name", p.Name)
			return nil, true, nil
		}
		return nil, false, getErr
	}

	realmJSON, ok := configMap.Data[cmKey]
	if !ok || realmJSON == "" {
		// The ConfigMap exists but lacks the configured key: a deterministic user mistake.
		// Surface it as a hard error so the Platform degrades with an actionable message
		// naming the field/key (never the contents).
		return nil, false, &realmImportKeyError{configMap: cmName, key: cmKey}
	}

	// Parse the realm representation. A malformed JSON is a deterministic user error.
	var parsed map[string]interface{}
	if json.Unmarshal([]byte(realmJSON), &parsed) != nil {
		return nil, false, &realmImportParseError{configMap: cmName, key: cmKey}
	}
	// The caller's realm REPLACES the operator's default — but the confidential "ilm" OIDC
	// client is operator-owned (reconcileOIDCProvider unconditionally fetches its generated
	// secret for any managed Keycloak). Ensure it is present so a caller realm that omits it
	// still yields a working OIDC wiring (a caller realm that DOES define "ilm" wins untouched).
	// Without this the import lands an ilm-less realm and OIDCConfigured never closes — the
	// exact failure the gated managed e2e caught.
	platformbuilder.EnsureILMClient(p, parsed)
	return parsed, false, nil
}

// realmImportKeyError / realmImportParseError are deterministic user errors surfaced as a
// Degraded condition; their messages name only the ConfigMap + key, never the contents.
type realmImportKeyError struct{ configMap, key string }

func (e *realmImportKeyError) Error() string {
	return "keycloak.managed.realmImport: ConfigMap " + e.configMap + " has no key " + e.key
}

type realmImportParseError struct{ configMap, key string }

func (e *realmImportParseError) Error() string {
	return "keycloak.managed.realmImport: ConfigMap " + e.configMap + " key " + e.key + " is not valid realm-representation JSON"
}
