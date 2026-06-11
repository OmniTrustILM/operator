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

// gate_managed_upgrade.go is the MAJOR-version upgrade guard for managed infrastructure
// (the CloudNativePG database, the RabbitMQ broker, and the managed Keycloak). It protects
// an already-running managed cluster from a one-way, prerequisite-bearing MAJOR engine
// upgrade that the operator would otherwise pass straight through to the upstream operator
// (e.g. RabbitMQ 3.x → 4.x, which needs feature flags + all-quorum-queues first; a CNPG
// PostgreSQL major bump; a Keycloak realm-migrating major bump).
//
// Contract (per component, folded into gateManagedInfra after the CRDs are confirmed
// present and BEFORE the rendered managed CR is applied):
//
//   - First creation (no running cluster / no readable running version) is NOT an upgrade:
//     apply the desired version freely.
//   - A patch/minor change, or a same/lower major, applies freely.
//   - A MAJOR increase of the running version with spec.<infra>.managed.upgradeAcknowledged
//     != true is BLOCKED: the operator re-pins the CURRENTLY-RUNNING image on the rendered
//     CR (so the upstream CR is left unchanged — the engine is NOT bumped), sets the adjunct
//     <Infra>UpgradeBlocked=True condition with reason MajorUpgradeNeedsAck, emits a Warning
//     Event with an actionable message, and requests a requeue. It is an ADJUNCT (like the
//     gate's other waiting states): it NEVER flips the whole Platform to Degraded — the rest
//     of the platform keeps converging on the healthy running version.
//   - With upgradeAcknowledged=true the desired (new) major applies normally.
//
// The reference version (when the CR pins no spec.<infra>.managed.version) is the version
// bundle's managed-infra default (RabbitMQVersion/CNPGVersion/KeycloakVersion), supplied by
// the reconciler — so the guard reasons over per-bundle DATA, not Go literals.
//
// SECURITY: the condition/event/log messages carry only the two version strings and the
// remedy field path — never a secret value or a connection coordinate.

import (
	"context"
	"strconv"
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// reasonMajorUpgradeNeedsAck is the condition reason / Event reason when a managed-infra
// MAJOR version upgrade is blocked pending acknowledgement. It carries no identity material.
const reasonMajorUpgradeNeedsAck = "MajorUpgradeNeedsAck"

// upgradeBlockedConditionSuffix is appended to a component's gate condition prefix to name
// its upgrade-blocked condition, e.g. "Database" + suffix = "DatabaseUpgradeBlocked".
const upgradeBlockedConditionSuffix = "UpgradeBlocked"

// infraVersionGuard bundles everything the major-version guard needs for ONE managed-infra
// component. It is the version-guard analogue of managedInfraGate: each component supplies a
// small descriptor and the shared guardMajorUpgrade path drives the detection + pin-back +
// condition identically for all of them. All functions are pure builder calls.
type infraVersionGuard struct {
	// clusterGVK / clusterName identify the live upstream CR to read the running version off.
	clusterGVK  schema.GroupVersionKind
	clusterName string
	// imageFieldPath is the unstructured spec path of the engine image on the upstream CR
	// (where the operator writes it AND where the running image is read back).
	imageFieldPath []string
	// versionFromImage parses the running image ref back to its version string (the inverse
	// of the builder's *ImageForVersion composer). "" means "unknown running version".
	versionFromImage func(image string) string
	// desiredVersion is the version the CR requests (spec.<infra>.managed.version), or "" to
	// fall back to bundleVersion.
	desiredVersion string
	// bundleVersion is the version bundle's managed-infra default for this component, the
	// reference version when the CR pins none.
	bundleVersion string
	// acknowledged reports whether spec.<infra>.managed.upgradeAcknowledged is true.
	acknowledged bool
	// conditionPrefix is the component's gate condition prefix ("Database"/"Messaging"/
	// "Keycloak"); the upgrade-blocked condition is conditionPrefix + "UpgradeBlocked".
	conditionPrefix string
	// upstreamLabel names the upstream operator in the actionable Warning message (e.g.
	// "CloudNativePG", "RabbitMQ", "Keycloak"). Non-secret label only.
	upstreamLabel string
	// ackFieldPath is the spec field the user sets to acknowledge (e.g.
	// "spec.database.managed.upgradeAcknowledged"), named verbatim in the remedy message.
	ackFieldPath string
}

// upgradeBlockedCondition returns the guard's condition type, e.g. "DatabaseUpgradeBlocked".
func (g infraVersionGuard) upgradeBlockedCondition() string {
	return g.conditionPrefix + upgradeBlockedConditionSuffix
}

// effectiveDesiredVersion is the version the operator wants to run: the CR's pinned version,
// or the bundle default when the CR pins none (mirroring the builders, which omit the image
// — letting the upstream operator pick its default — only when BOTH are empty).
func (g infraVersionGuard) effectiveDesiredVersion() string {
	if g.desiredVersion != "" {
		return g.desiredVersion
	}
	return g.bundleVersion
}

// guardMajorUpgrade enforces the major-version upgrade guard for one managed-infra component.
// It is called from gateManagedInfra after the CRDs are confirmed present and BEFORE the
// rendered objects are applied, and mutates objs IN PLACE when it blocks (re-pinning the
// running image so the apply leaves the engine version unchanged). It returns blocked=true
// when it set the UpgradeBlocked condition + Event and the caller should requeue; on the
// not-blocked path it removes any stale UpgradeBlocked condition.
//
// blocked=true never degrades the platform: the caller treats it like the gate's other
// waiting states (condition + requeue), so the rest of the platform keeps converging on the
// healthy running version.
func (r *Reconciler) guardMajorUpgrade(ctx context.Context, p *otilmv1alpha1.Platform, objs []client.Object, g infraVersionGuard) bool {
	running := r.runningManagedVersion(ctx, p, g)
	desired := g.effectiveDesiredVersion()

	// First creation (no readable running version) is not an upgrade; a non-major change is
	// allowed. Either way nothing is blocked — drop any stale condition and apply freely.
	if !isMajorUpgrade(running, desired) || g.acknowledged {
		meta.RemoveStatusCondition(&p.Status.Conditions, g.upgradeBlockedCondition())
		return false
	}

	// A MAJOR increase without acknowledgement: re-pin the running image on every rendered
	// object so the apply does NOT bump the engine, set the adjunct condition + Warning, and
	// ask the caller to requeue. The running version stays in effect until acknowledged.
	r.pinRunningImage(ctx, p, objs, g)

	msg := "major upgrade " + running + "→" + desired + " requires " + g.ackFieldPath +
		"=true; review " + g.upstreamLabel + " upgrade prerequisites before acknowledging"
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: g.upgradeBlockedCondition(), Status: metav1.ConditionTrue, Reason: reasonMajorUpgradeNeedsAck,
		Message: msg, ObservedGeneration: p.Generation, // version strings + remedy only — no secret/coordinate
	})
	r.event(p, corev1.EventTypeWarning, reasonMajorUpgradeNeedsAck, msg)
	log.FromContext(ctx).Info("managed-infra major upgrade blocked pending acknowledgement",
		"component", g.conditionPrefix, "running", running, "desired", desired)
	return true
}

// runningManagedVersion reads the version of the CURRENTLY-RUNNING managed cluster by GETting
// the live upstream CR (unstructured; its GVK is preset, the type is not in the scheme) and
// parsing the engine image off its spec image-field path. It returns "" when the cluster does
// not exist yet (first creation), when the image field is unset (the operator omitted it so
// the upstream operator's own default is running — an unknown-to-us version we do not block
// on), or on any read error (tolerated as unknown so a transient API blip never blocks).
//
// DESIGN NOTE (running-version source): the running version is read from the live CR's SPEC
// image field — which reflects what is CURRENTLY applied/running because the guard runs BEFORE
// this reconcile re-applies — rather than a status field, because the upstream status image
// paths differ and shift across versions (CNPG records prior image/version under
// .status.pgDataImageInfo and historically .status.image; the RabbitMQ Cluster Operator does
// not surface a running-version status field; the Keycloak CR status carries no engine-image
// field). Reading the applied spec image is uniform across all three and is the value the
// operator itself last set. A future bump that wants the status image instead should prefer
// .status.image (CNPG) / the operand-image status the operator adds, re-checked per operator.
func (r *Reconciler) runningManagedVersion(ctx context.Context, p *otilmv1alpha1.Platform, g infraVersionGuard) string {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(g.clusterGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: g.clusterName}, &u); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("reading running managed-infra version failed; treating as unknown (not an upgrade)",
				"component", g.conditionPrefix, "err", err.Error())
		}
		return "" // not yet created, or unreadable → unknown running version → not an upgrade
	}
	image, found, err := unstructured.NestedString(u.Object, g.imageFieldPath...)
	if err != nil || !found || image == "" {
		return "" // image omitted (upstream default running) → unknown → not a detectable upgrade
	}
	return g.versionFromImage(image)
}

// pinRunningImage re-pins the running engine image onto every rendered managed object that
// carries the image field, so the subsequent SSA apply leaves the upstream CR's engine
// version UNCHANGED (the major bump is suppressed until acknowledged). It reads the running
// image once from the live cluster CR and copies it onto the rendered objects' image-field
// path; objects without that field (topology CRs, the Pooler, a realm import) are left as-is.
//
// When the running image cannot be read (it should be readable here, since guardMajorUpgrade
// only blocks when a running version was parsed) the rendered image field is REMOVED so the
// apply does not push the new version — a conservative fallback that still suppresses the bump.
func (r *Reconciler) pinRunningImage(ctx context.Context, p *otilmv1alpha1.Platform, objs []client.Object, g infraVersionGuard) {
	var live unstructured.Unstructured
	live.SetGroupVersionKind(g.clusterGVK)
	runningImage := ""
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: g.clusterName}, &live); err == nil {
		if img, found, _ := unstructured.NestedString(live.Object, g.imageFieldPath...); found {
			runningImage = img
		}
	}

	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		// Only touch the cluster CR (the one carrying the image field); other rendered CRs
		// (topology, pooler, realm import) have no image and are left untouched.
		if _, found, _ := unstructured.NestedString(u.Object, g.imageFieldPath...); !found {
			continue
		}
		if runningImage == "" {
			unstructured.RemoveNestedField(u.Object, g.imageFieldPath...)
			continue
		}
		// SetNestedField copies a string scalar; it cannot error for a []string leaf.
		_ = unstructured.SetNestedField(u.Object, runningImage, g.imageFieldPath...)
	}
}

// isMajorUpgrade reports whether moving from the running version to the desired version is a
// MAJOR increase. It is false (apply freely) when:
//   - running is "" (first creation / unknown running version — not an upgrade), OR
//   - desired is "" (no concrete target — should not happen once the bundle default is
//     applied, but guarded for safety), OR
//   - either major is unparseable, OR
//   - desired's major is <= running's major (same major, a patch/minor change, or a
//     downgrade — none of which this guard blocks; a downgrade is its own concern).
//
// Only a strictly-greater desired major is an upgrade this guard gates.
func isMajorUpgrade(running, desired string) bool {
	if running == "" || desired == "" {
		return false
	}
	rMajor, rOK := majorOf(running)
	dMajor, dOK := majorOf(desired)
	if !rOK || !dOK {
		return false // an unparseable version is not a detectable major jump
	}
	return dMajor > rMajor
}

// majorOf extracts the leading numeric MAJOR component of a version string: the digits before
// the first "." or "-" (so "16" → 16, "4.0" → 4, "26.4.0" → 26, "16-standard-bookworm" → 16).
// ok is false when the leading component is not a non-negative integer (e.g. "latest").
func majorOf(version string) (int, bool) {
	// Cut at the first separator so "4.0-management" / "16-bookworm" reduce to the major.
	major := version
	if i := strings.IndexAny(version, ".-"); i >= 0 {
		major = version[:i]
	}
	n, err := strconv.Atoi(major)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
