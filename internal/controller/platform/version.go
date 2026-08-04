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
	"github.com/Masterminds/semver/v3"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// reasonDowngradeForbidden is the Degraded condition reason when an explicit spec.version is
// older than the version already running (status.observedVersion).
const reasonDowngradeForbidden = "DowngradeForbidden"

// reasonPreviewVersionUpgradeBlocked is the Degraded condition reason when an explicit
// spec.version resolves to an unreleased (preview) bundle while a different version is
// already running: a live platform cannot be upgraded onto a preview bundle.
const reasonPreviewVersionUpgradeBlocked = "PreviewVersionUpgradeBlocked"

// effectivePlatformVersion implements the PIN-ON-CREATE policy: the version the operator
// reconciles a Platform against, in precedence order, is
//
//  1. spec.version when the user set it explicitly — an explicit version is the ONLY upgrade
//     trigger;
//  2. otherwise status.observedVersion — the version the platform was first reconciled
//     against. An empty spec.version therefore FOLLOWS the pinned version, so upgrading the
//     operator (which changes the built-in DefaultVersion) never silently upgrades a running
//     platform;
//  3. otherwise "" — only on the very first reconcile of a version-less platform, which
//     bom.BundleFor resolves to the operator's newest (DefaultVersion); that resolved version
//     is then recorded on status.observedVersion, pinning it for every subsequent reconcile.
//
// It returns "" only in case 3 (BundleFor maps "" to DefaultVersion). The pin lives in
// status, never in spec, so the policy does NOT fight a GitOps actor that owns the spec.
func effectivePlatformVersion(p *otilmv1alpha1.Platform) string {
	if p.Spec.Version != "" {
		return p.Spec.Version
	}
	return p.Status.ObservedVersion
}

// teardownPlatformVersion resolves the version the DELETION teardown renders against, with the
// precedence DELIBERATELY REVERSED from effectivePlatformVersion: the RUNNING reality wins over
// the requested version, i.e. status.observedVersion when pinned, else spec.version.
//
// Teardown must reclaim the objects that actually EXIST, and those were rendered from the
// running version. Two cases make the requested version wrong:
//
//   - a blocked upgrade (spec.version names a bundle the version guards refused, e.g. an
//     unreleased preview) left the platform running the OLD topology, whose object names the
//     new bundle may have renamed;
//   - an empty spec.version on a pinned platform would resolve to the operator's built-in
//     default, which is not necessarily the running version.
//
// In both, rendering teardown from spec.version would delete nothing and ORPHAN the live
// upstream-operator CRs under deletionPolicy=Delete.
func teardownPlatformVersion(p *otilmv1alpha1.Platform) string {
	if p.Status.ObservedVersion != "" {
		return p.Status.ObservedVersion
	}
	return p.Spec.Version
}

// teardownRenderPlatforms returns the platform copies the DELETION teardown renders against —
// DEEP COPIES with spec.version (and, for one, spec.messaging.virtualHost) pinned for
// rendering only, so the finalizer-removal Update that follows never persists a spec change.
//
// One copy is rendered per bom.AllVersions() — every bundle this operator ships, released and
// preview. A released upgrade APPLIES the new version's managed objects BEFORE
// status.observedVersion is persisted, so a reconcile (or that status write) failing in
// between can leave a platform whose managed objects belong to a DIFFERENT bundle than either
// spec.version or status.observedVersion names; sweeping every known bundle reclaims that
// topology regardless of which version the failure happened at, without having to track which
// bundle was actually applied.
//
// A further LEGACY-SCOPE copy pins the RUNNING version (teardownPlatformVersion) but forces
// spec.messaging.virtualHost to bom.LegacyUnscopedVirtualHost, reproducing the UNSCOPED
// managed-messaging object names an operator predating vhost-scoped topology naming rendered.
// A platform with a CUSTOM virtualHost had those unscoped names before vhost scoping shipped;
// the same platform now renders vhost-scoped ones, so without this copy its original CRs would
// be orphaned — rabbitmq.com kinds are prune-excluded, so nothing else ever reclaims them.
//
// Rendering this whole set is safe: deleting an object that was never created is a no-op
// (handleManagedInfraDeletion tolerates NotFound), and mergeManagedObjects dedupes the objects
// the renders have in common.
func teardownRenderPlatforms(p *otilmv1alpha1.Platform) []*otilmv1alpha1.Platform {
	versions := bom.AllVersions()
	out := make([]*otilmv1alpha1.Platform, 0, len(versions)+1)
	for _, v := range versions {
		render := p.DeepCopy()
		render.Spec.Version = v
		out = append(out, render)
	}

	legacy := p.DeepCopy()
	legacy.Spec.Version = teardownPlatformVersion(p)
	legacy.Spec.Messaging.VirtualHost = bom.LegacyUnscopedVirtualHost
	return append(out, legacy)
}

// isPlatformDowngrade reports whether requested is strictly OLDER (by semver) than running.
// Both must parse as semver; an unparseable input is treated as "not a downgrade" — the CRD
// format guards the shape, and refusing to render on a parse quirk would be worse than
// allowing it (an unsupported version is already caught separately by bom.BundleFor). A
// downgrade of a stateful platform that has already self-migrated its schema is unsafe, so the
// reconciler refuses it.
func isPlatformDowngrade(requested, running string) bool {
	rv, err1 := semver.NewVersion(requested)
	cv, err2 := semver.NewVersion(running)
	if err1 != nil || err2 != nil {
		return false
	}
	return rv.LessThan(cv)
}
