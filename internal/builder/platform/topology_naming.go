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

// topology_naming.go derives the vhost SCOPE that makes a managed messaging topology's
// Kubernetes object names a function of the vhost it lives on.
//
// WHY: the Messaging Topology Operator treats a CR's spec.vhost / spec.name as immutable,
// so a topology cannot be re-pointed at a different vhost in place — the source and the
// target topology must COEXIST as distinct objects for the duration of a migration. That
// only works if their object names are disjoint, hence the scope.
//
// THE LEGACY PIN: the legacy vhost (bom.LegacyUnscopedVirtualHost) maps to an EMPTY scope,
// unconditionally. Every live 2.17.0/2.18.0 platform already carries the unscoped names in
// its cluster, and rabbitmq.com kinds are deliberately excluded from pruning — so a
// blanket rename would re-identify every applied CR the moment the operator upgrades and
// leave the originals orphaned forever, holding finalizers over live queues. The empty
// scope is what makes an upgraded operator re-render those platforms byte-identically
// (TestLegacyTopologyNamesAreFrozen is the safety net).
//
// THE USER-PINNED VHOST PIN: a vhost the platform pinned itself, via
// spec.messaging.virtualHost, ALSO maps to an EMPTY scope — for exactly the same reason as
// the legacy pin, one vhost over. Scoping exists solely so a migration's source and target
// topology can be disjoint and coexist while the cutover is in flight; a user override always
// wins over the bundle default (see managedVirtualHost in managed_messaging.go), so a pinned
// vhost resolves to the SAME value under every bundle. That platform therefore never
// experiences the vhost RENAME a version upgrade causes — it never migrates, and so it never
// needs a disjoint name. Without this pin, a live 2.17.0/2.18.0 platform with a custom
// virtualHost (a supported, documented field, unscoped today for the same reason the legacy
// vhost is) would re-render SCOPED names the moment the operator picked up vhost-scoped
// naming, orphaning its own already-applied CRs exactly like an unpinned legacy-vhost platform
// would without its pin (TestUserPinnedVhostTopologyNamesAreFrozen is the safety net for this
// one).
//
// The broker Users and the credentials Secrets the Topology Operator generates from them
// are NOT scoped: users are broker-global, their specs are identical across the bundles,
// and their "<user>-user-credentials" Secret names are wired into every component's
// secretKeyRef — scoping those would re-key every component's credentials at cutover.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/OmniTrustILM/operator/pkg/bom"
)

const (
	// rabbitMQDefaultVirtualHost is RabbitMQ's own default vhost — the vhost 2.19.0 moves
	// the platform onto. It is not expressible as a DNS-safe name, so it slugs to the
	// spelled-out vhostSlugDefault.
	rabbitMQDefaultVirtualHost = "/"

	// vhostSlugDefault is the slug of RabbitMQ's default "/" vhost, and the base slug for
	// any vhost that sanitizes to nothing (those also carry a hash suffix, so they never
	// collide with "/").
	vhostSlugDefault = "default"

	// vhostSlugBaseMaxLen bounds the readable part of a slug; a longer vhost is truncated
	// to it and disambiguated by the hash suffix. Base + "-" + 8 hex digits stays well
	// inside a Kubernetes name component.
	vhostSlugBaseMaxLen = 24

	// vhostSlugHashLen is the number of hex digits of the vhost's SHA-256 appended when the
	// sanitized form is not a faithful rendering of the vhost.
	vhostSlugHashLen = 8
)

// topologyScope returns the name infix that scopes a managed topology's Kubernetes object
// names to one vhost: the empty string for the legacy vhost (pinning the names live
// platforms already carry) or when userPinned is true (pinning the names a user-pinned
// vhost already carries, for the same reason — see the USER-PINNED VHOST PIN note above),
// and "-<vhostSlug>" for every other vhost.
func topologyScope(vhost string, userPinned bool) string {
	if vhost == bom.LegacyUnscopedVirtualHost || userPinned {
		return ""
	}
	return "-" + vhostSlug(vhost)
}

// vhostSlug maps a RabbitMQ vhost name to a non-empty, DNS-safe, bounded, deterministic
// slug. RabbitMQ vhost names are near-arbitrary strings (and case-sensitive), so the
// sanitized form alone would be ambiguous: whenever sanitizing CHANGES the vhost — a
// different case, an illegal character, an empty result — or the vhost is longer than the
// readable bound, a short deterministic hash of the RAW vhost is appended, so two distinct
// vhosts can never share a slug.
func vhostSlug(vhost string) string {
	if vhost == rabbitMQDefaultVirtualHost {
		return vhostSlugDefault
	}

	// A vhost spelled exactly like the "/" slug is treated as unfaithful too, so it takes a
	// hash suffix rather than colliding with the default vhost's slug.
	base := strings.Trim(sanitizeName(vhost), "-")
	faithful := base != "" && base == vhost && base != vhostSlugDefault
	if base == "" {
		base = vhostSlugDefault
	}
	if len(base) > vhostSlugBaseMaxLen {
		base = strings.Trim(base[:vhostSlugBaseMaxLen], "-")
		faithful = false
	}
	if faithful {
		return base
	}

	sum := sha256.Sum256([]byte(vhost))
	return base + "-" + hex.EncodeToString(sum[:])[:vhostSlugHashLen]
}
