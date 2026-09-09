/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"fmt"
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Database gating condition type and reasons. DatabaseReady is an ADJUNCT signal (like
// EdgeReady): it reports the managed database's provisioning state without ever blocking
// the platform's Available condition — Core/scheduler wait on the generated app Secret
// via their secretKeyRef, exactly as they would wait on an external Secret. Reasons carry
// no identity material (no host/port/URI, no credentials).
const (
	// conditionDatabaseReady reports whether the operator-provisioned (managed) database
	// is provisioned and its generated credentials Secret is available. It is set only
	// for a managed database; an external database drops it (nothing to report).
	conditionDatabaseReady = "DatabaseReady"
	// reasonWaitingForDatabase: the CloudNativePG CRDs are served and the Cluster is
	// applied, but the Cluster is not yet Ready or its generated app Secret is not yet
	// present. A transient, self-healing waiting state (requeue).
	reasonWaitingForDatabase = "WaitingForDatabase"
	// reasonDatabaseReady: the managed Cluster is Ready and its app Secret is present.
	reasonDatabaseReady = "Reconciled"
)

// databaseRequeueAfter is how long the reconciler waits before re-checking a managed
// database that is applied but not yet Ready (the CloudNativePG Cluster is still
// provisioning, or its generated app Secret has not appeared yet). The CNPG Cluster
// Watch (SetupWithManager) re-enqueues on the Cluster's status change and the Secret
// Watch on the generated Secret's creation, so this requeue is a backstop only.
const databaseRequeueAfter = 15 * time.Second

// gateDatabase reconciles the managed database (CloudNativePG Cluster + optional Pooler)
// and reflects its state on the DatabaseReady condition. It is the managed-infra analogue
// of gateEdge, with one extra state: a present CRD does not imply a Ready database, so
// after applying the CNPG objects it probes the Cluster's readiness and the generated
// app Secret.
//
// Returns ready=true only when an external database (nothing to provision — always
// "ready" from the platform's perspective) OR a managed database whose Cluster is Ready
// and whose app Secret exists. requeue=true asks the caller to re-check soon. An error is
// returned only for an actual apply failure or a rejected override (a missing CRD or a
// not-yet-Ready Cluster is non-fatal, like the edge).
//
// Behaviour (managed database):
//   - render-time error (rejected override / malformed patch) → hard error (degrade).
//   - CRD absent → DatabaseReady=False/CloudNativePGNotInstalled, apply nothing, mark the
//     CNPG objects desired (prune-preservation across a flap), requeue.
//   - CRD present → apply the Cluster (+Pooler); then probe readiness:
//     Cluster not Ready or app Secret missing → DatabaseReady=False/WaitingForDatabase,
//     ready=false, requeue;
//     both present → DatabaseReady=True, ready=true.
//
// SECURITY: the condition message names only the spec field and the remedy — never a
// secret value or a connection coordinate.
func (r *Reconciler) gateDatabase(ctx context.Context, p *otilmv1alpha1.Platform, bundle bom.Bundle, desired desiredSet) (ready bool, requeue bool, err error) {
	gate := r.databaseGate(p)
	gate.versionGuard = databaseVersionGuard(p, bundle)
	return r.gateManagedInfra(ctx, p, desired, gate)
}

// databaseVersionGuard builds the major-version upgrade guard descriptor for the managed
// database (nil when external — nothing to guard). The running version is read from the live
// CNPG Cluster's spec.imageName; the reference version is spec.database.managed.version, or
// the bundle's CNPGVersion when the CR pins none.
func databaseVersionGuard(p *otilmv1alpha1.Platform, bundle bom.Bundle) *infraVersionGuard {
	if !platformbuilder.DatabaseManaged(p) {
		return nil
	}
	return &infraVersionGuard{
		clusterGVK:       platformbuilder.ManagedDatabaseClusterGVK(),
		clusterName:      platformbuilder.ManagedDatabaseName(p),
		imageFieldPath:   platformbuilder.ManagedDatabaseImageFieldPath(),
		versionFromImage: platformbuilder.ManagedDatabaseVersionFromImage,
		desiredVersion:   platformbuilder.ManagedDatabaseDesiredVersion(p),
		bundleVersion:    bundle.CNPGVersion,
		acknowledged:     p.Spec.Database.Managed.UpgradeAcknowledged,
		conditionPrefix:  "Database",
		upstreamLabel:    "CloudNativePG",
		ackFieldPath:     "spec.database.managed.upgradeAcknowledged",
	}
}

// databaseGate describes the managed database (CloudNativePG) for the shared
// gateManagedInfra / handleManagedInfraDeletion paths: its predicate, rendered CRs, render-
// error accessor, CRD dependencies, the cluster+app-Secret readiness probe, and the
// DatabaseReady condition vocabulary. messagingGate is its deliberate sibling: both
// populate the same managedInfraGate shape with their distinct values (the shared shape is
// the point), so the structural similarity dupl flags is intentional.
//
//nolint:dupl // deliberate per-component descriptor sibling of messagingGate; see doc above
func (r *Reconciler) databaseGate(p *otilmv1alpha1.Platform) managedInfraGate {
	return managedInfraGate{
		managed:      platformbuilder.DatabaseManaged(p),
		kind:         "database",
		name:         platformbuilder.ManagedDatabaseName(p),
		objects:      platformbuilder.ResolveManagedDatabase(p),
		renderError:  platformbuilder.ManagedDatabaseRenderError,
		dependencies: platformbuilder.DatabaseDependencies(p),
		// Ready when the Cluster reports Ready AND the CNPG-generated app Secret exists.
		ready: func(ctx context.Context) bool {
			return managedReadyProbe(ctx,
				func(ctx context.Context) (bool, error) { return r.managedClusterReady(ctx, p) },
				func(ctx context.Context) bool { return r.managedAppSecretPresent(ctx, p) })
		},
		conditionType:  conditionDatabaseReady,
		waitingReason:  reasonWaitingForDatabase,
		waitingMessage: "waiting for the managed database to become ready",
		readyReason:    reasonDatabaseReady,
		readyMessage:   "managed database reconciled",
		detectionLabel: "database",
	}
}

// managedClusterReady reports whether the managed CloudNativePG Cluster is Ready. It
// GETs the Cluster as unstructured (its GVK is preset; the type is not registered in the
// scheme) and checks two CNPG-documented signals, treating EITHER as ready:
//   - status.conditions[type=Ready].status == "True", and/or
//   - status.phase == "Cluster in healthy state".
//
// A NotFound (the Cluster was just applied and has no status yet, or detection is
// momentarily behind) is "not ready". The managed-database e2e drives a CNPG Cluster to its
// healthy steady state and the operator's DatabaseReady condition converges to True via
// exactly these two signals — the "Ready" condition type and the "Cluster in healthy state"
// phase string below (CNPG's PhaseHealthy). The exact phase text may vary across major CNPG
// versions, so re-check when bumping the pinned CNPG.
func (r *Reconciler) managedClusterReady(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ManagedDatabaseName(p)}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// status.conditions[type=Ready].status == True
	if cnpgReadyConditionTrue(u.Object) {
		return true, nil
	}

	// status.phase == healthy. CNPG reports a human-readable phase; the healthy steady
	// state is "Cluster in healthy state" (CNPG's PhaseHealthy constant).
	phase, found, _ := unstructured.NestedString(u.Object, "status", "phase")
	if found && phase == "Cluster in healthy state" {
		return true, nil
	}
	return false, nil
}

// cnpgReadyConditionTrue reports whether the CloudNativePG Cluster's
// status.conditions[type=Ready].status equals "True". It returns false when the
// conditions slice is absent or carries no True Ready condition.
func cnpgReadyConditionTrue(obj map[string]interface{}) bool {
	conds, found, _ := unstructured.NestedSlice(obj, "status", "conditions")
	if !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == conditionTypeReady && cm["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

// managedAppSecretPresent reports whether the CloudNativePG-generated application Secret
// (<cluster>-app) exists yet. CNPG creates it once the Cluster bootstraps; until then a
// dependent's secretKeyRef would not resolve. A read error other than NotFound is treated
// as "not present" (the requeue retries) so a transient API blip never degrades the
// platform. SECURITY: only the Secret's existence is checked — its content is never read.
func (r *Reconciler) managedAppSecretPresent(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	name := platformbuilder.ResolveDatabaseConnection(p).CredentialsSecretName
	if name == "" {
		return false
	}
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, &s); err != nil {
		return false
	}
	return true
}

// applyManaged server-side-applies a managed-infrastructure object (a CloudNativePG
// Cluster/Pooler today) WITHOUT a controller owner reference — the deletion-safety
// contract: managed CRs are tracked only by the managed-by/instance labels, are EXCLUDED
// from the prune, and are torn down (or retained) explicitly by handleDeletion per
// spec.deletionPolicy. This is deliberately distinct from Reconciler.apply, which stamps
// the owner reference for the operator's own children.
//
// It populates the object's GVK on the wire (SSA requires it; for the preset-GVK
// unstructured object that GVK is already set) and patches with the stable field manager
// and ForceOwnership. It does NOT add the object to the desired set used by the prune —
// managed CRs are never pruned (the prune's GVK list excludes CNPG kinds), so recording
// them there would be meaningless; the caller records them in the desired set only on the
// CRD-absent path, for consistency with the edge's prune-preservation bookkeeping.
func (r *Reconciler) applyManaged(ctx context.Context, _ *otilmv1alpha1.Platform, obj client.Object, _ desiredSet) error {
	// GVK is preset on the unstructured object; ensure it is non-empty for SSA.
	if obj.GetObjectKind().GroupVersionKind().Empty() {
		return fmt.Errorf("managed object %q has no GVK", obj.GetName())
	}
	if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil { //nolint:staticcheck // SA1019: patch-based SSA is intentional for preset-GVK unstructured render objects; ApplyConfiguration path is unsafe for these
		return err
	}
	return nil
}
