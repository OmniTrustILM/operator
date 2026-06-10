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
)

// reasonDowngradeForbidden is the Degraded condition reason when an explicit spec.version is
// older than the version already running (status.observedVersion).
const reasonDowngradeForbidden = "DowngradeForbidden"

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
