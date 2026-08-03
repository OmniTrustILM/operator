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
// DEEP COPIES with spec.version pinned for rendering only, so the finalizer-removal Update that
// follows never persists a spec change.
//
// The RUNNING version (teardownPlatformVersion) always comes first: it names the objects that
// certainly exist. A second copy pinned to the REQUESTED spec.version is appended when an
// upgrade is in flight — spec.version is set, resolves to a known bundle, and differs from the
// running version — because a released upgrade APPLIES the new version's managed objects BEFORE
// status.observedVersion is persisted. If reconcile (or that status write) fails in between and
// the Platform is then deleted with deletionPolicy=Delete, rendering only the running version
// would ORPHAN the new-version-only upstream CRs, which are prune-excluded by design and so are
// reclaimed by nothing else.
//
// Rendering the UNION is safe in the other direction too: deleting an object that was never
// created is a no-op (handleManagedInfraDeletion tolerates NotFound), and an unresolvable or
// equal requested version falls back to the single effective render.
func teardownRenderPlatforms(p *otilmv1alpha1.Platform) []*otilmv1alpha1.Platform {
	running := p.DeepCopy()
	running.Spec.Version = teardownPlatformVersion(p)
	out := []*otilmv1alpha1.Platform{running}

	requested := p.Spec.Version
	if requested == "" || requested == running.Spec.Version {
		return out
	}
	if _, ok := bom.BundleFor(requested); !ok {
		// An unsupported version rendered nothing, so there is nothing extra to reclaim.
		return out
	}
	inFlight := p.DeepCopy()
	inFlight.Spec.Version = requested
	return append(out, inFlight)
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
