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
	"strings"
	"testing"

	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
)

// TestTopologyScopeLegacyIsEmpty proves the legacy vhost keeps today's unscoped names
// while any other vhost gets a deterministic, DNS-safe scope.
func TestTopologyScopeLegacyIsEmpty(t *testing.T) {
	assert.Equal(t, "", topologyScope(bom.LegacyUnscopedVirtualHost))
	assert.Equal(t, "-default", topologyScope("/"))
	assert.Equal(t, "-myvhost", topologyScope("myvhost"))
}

// TestVhostSlugIsTotal proves the slug is defined, DNS-safe and bounded for every input,
// and that inputs which sanitize identically are still distinguished by the hash suffix.
func TestVhostSlugIsTotal(t *testing.T) {
	seen := map[string]string{}
	for _, v := range []string{"/", "myvhost", "My.Vhost", "my_vhost", "a/b", strings.Repeat("x", 300), "", "///"} {
		s := vhostSlug(v)
		assert.NotEmpty(t, s, "slug must be total for %q", v)
		assert.Regexp(t, `^[a-z0-9-]+$`, s)
		assert.LessOrEqual(t, len(s), 40)
		if prev, dup := seen[s]; dup {
			t.Fatalf("slug collision: %q and %q both -> %q", prev, v, s)
		}
		seen[s] = v
	}
}

// TestVhostSlugIsDeterministic proves the slug is a pure function of the vhost: the same
// vhost renders the same object names on every reconcile, so an applied topology CR is
// never re-identified by a restart or a rebuild.
func TestVhostSlugIsDeterministic(t *testing.T) {
	for _, v := range []string{"/", "myvhost", "My.Vhost", strings.Repeat("x", 300), ""} {
		assert.Equal(t, vhostSlug(v), vhostSlug(v), "vhostSlug(%q) must be stable", v)
	}
}

// TestVhostSlugDefaultLiteralIsDistinguished proves a vhost literally named "default" does
// not collide with RabbitMQ's "/" vhost, whose slug is that same word.
func TestVhostSlugDefaultLiteralIsDistinguished(t *testing.T) {
	assert.Equal(t, vhostSlugDefault, vhostSlug(rabbitMQDefaultVirtualHost))
	assert.NotEqual(t, vhostSlug(rabbitMQDefaultVirtualHost), vhostSlug(vhostSlugDefault))
	assert.Regexp(t, `^default-[0-9a-f]{8}$`, vhostSlug(vhostSlugDefault))
}

// TestVhostSlugCaseIsDistinguished proves two vhosts that differ only in case do NOT share
// a slug: RabbitMQ vhost names are case-sensitive, so "MyVhost" and "myvhost" are different
// vhosts and their topologies must stay disjoint.
func TestVhostSlugCaseIsDistinguished(t *testing.T) {
	assert.NotEqual(t, vhostSlug("myvhost"), vhostSlug("MyVhost"))
	assert.Equal(t, "myvhost", vhostSlug("myvhost"), "an already-canonical vhost slugs to itself, unsuffixed")
}
