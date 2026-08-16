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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// Messaging modes used by the trigger fixtures.
const (
	modeManaged  = "managed"
	modeExternal = "external"
)

// pinnedTestVirtualHost is a user-pinned spec.messaging.virtualHost: a vhost override always
// wins over the bundle default, so it resolves identically under every bundle.
const pinnedTestVirtualHost = "ilm-messages"

// migrationPlatform builds the minimal Platform the trigger layer reads: the messaging mode
// (with the managed block present, which MessagingManaged requires), the RUNNING version
// (status.observedVersion) and the REQUESTED one (spec.version).
func migrationPlatform(mode, observed, requested string) *otilmv1alpha1.Platform {
	p := &otilmv1alpha1.Platform{}
	p.Spec.Messaging.Mode = mode
	if mode == modeManaged {
		p.Spec.Messaging.Managed = &otilmv1alpha1.ManagedMessagingSpec{}
	}
	p.Spec.Version = requested
	p.Status.ObservedVersion = observed
	return p
}

// migratingPlatform builds a managed platform RUNNING 2.18.0 with a migration to 2.19.0 in
// flight at the given phase, whose spec.version now asks for `requested`. A migration's
// fromVersion is always the running version, which is what the engine records when it starts.
func migratingPlatform(requested string, phase otilmv1alpha1.MigrationPhase) *otilmv1alpha1.Platform {
	p := migrationPlatform(modeManaged, platformVersion218, requested)
	p.Status.Upgrade = &otilmv1alpha1.UpgradeStatus{
		FromVersion: p.Status.ObservedVersion, ToVersion: platformVersion219, Phase: phase,
	}
	return p
}

// bundleFor resolves a bundle for a fixture version, failing the test if the operator build
// does not carry it.
func bundleFor(t *testing.T, version string) bom.Bundle {
	t.Helper()
	b, ok := bom.BundleFor(version)
	require.True(t, ok, "fixture version %s must resolve to a bundle", version)
	return b
}

// TestDecideMigration is the whole trigger + refusal matrix, one row per semantic case. The
// decision layer is pure, so every row is a plain function call: no client, no envtest.
//
// The TRIGGER is a change of the EFFECTIVE messaging vhost (the bundle default overridden by
// spec.messaging.virtualHost) between the running and the requested bundle. Equal effective
// vhosts mean the ordinary additive apply path, never a migration.
func TestDecideMigration(t *testing.T) {
	cases := []struct {
		name         string
		platform     *otilmv1alpha1.Platform
		from, to     string
		wantAction   migrationAction
		wantReason   string
		messageParts []string
	}{
		{
			name:       "managed: the effective vhost is unchanged across the version bump",
			platform:   migrationPlatform(modeManaged, platformVersion217, platformVersion218),
			from:       platformVersion217,
			to:         platformVersion218,
			wantAction: migrationActionNone,
		},
		{
			// A pin resolves to the same vhost under both bundles, so the trigger sees no
			// rename — but only the CLUSTER can say whether the platform was already on that
			// vhost or is being moved onto it by this very update, so the decision is deferred
			// rather than waved through. See Reconciler.guardMigrationVirtualHostPin.
			name: "managed: a user-pinned virtualHost across a bundle-default rename is verified, not assumed",
			platform: func() *otilmv1alpha1.Platform {
				p := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
				p.Spec.Messaging.VirtualHost = pinnedTestVirtualHost
				return p
			}(),
			from:       platformVersion218,
			to:         platformVersion219,
			wantAction: migrationActionVerifyPin,
		},
		{
			// The same pin between two bundles that already share a default vhost suppresses
			// nothing, so there is nothing to verify: this is the ordinary additive path.
			name: "managed: a user-pinned virtualHost between bundles with the same default is simply no migration",
			platform: func() *otilmv1alpha1.Platform {
				p := migrationPlatform(modeManaged, platformVersion217, platformVersion218)
				p.Spec.Messaging.VirtualHost = pinnedTestVirtualHost
				return p
			}(),
			from:       platformVersion217,
			to:         platformVersion218,
			wantAction: migrationActionNone,
		},
		{
			name:       "managed: the bundle default vhost changes → start",
			platform:   migrationPlatform(modeManaged, platformVersion218, platformVersion219),
			from:       platformVersion218,
			to:         platformVersion219,
			wantAction: migrationActionStart,
		},
		{
			name:       "external: the topology is renamed and nothing acknowledges it",
			platform:   migrationPlatform(modeExternal, platformVersion218, platformVersion219),
			from:       platformVersion218,
			to:         platformVersion219,
			wantAction: migrationActionRefuse,
			wantReason: reasonExternalMessagingMigrationRequired,
			messageParts: []string{
				"spec.messaging.migrationAcknowledgedForVersion",
				platformVersion219,
			},
		},
		{
			name: "external: the acknowledgement names the target version",
			platform: func() *otilmv1alpha1.Platform {
				p := migrationPlatform(modeExternal, platformVersion218, platformVersion219)
				p.Spec.Messaging.MigrationAcknowledgedForVersion = platformVersion219
				return p
			}(),
			from:       platformVersion218,
			to:         platformVersion219,
			wantAction: migrationActionNone,
		},
		{
			name: "external: a stale acknowledgement from a past upgrade does not authorize this one",
			platform: func() *otilmv1alpha1.Platform {
				p := migrationPlatform(modeExternal, platformVersion218, platformVersion219)
				p.Spec.Messaging.MigrationAcknowledgedForVersion = platformVersion218
				return p
			}(),
			from:         platformVersion218,
			to:           platformVersion219,
			wantAction:   migrationActionRefuse,
			wantReason:   reasonExternalMessagingMigrationRequired,
			messageParts: []string{"spec.messaging.migrationAcknowledgedForVersion"},
		},
		{
			name:       "external: a purely additive topology change needs no acknowledgement",
			platform:   migrationPlatform(modeExternal, platformVersion217, platformVersion218),
			from:       platformVersion217,
			to:         platformVersion218,
			wantAction: migrationActionNone,
		},
		{
			name:         "managed: a 2.17.0 source must pass through the stepping stone",
			platform:     migrationPlatform(modeManaged, platformVersion217, platformVersion219),
			from:         platformVersion217,
			to:           platformVersion219,
			wantAction:   migrationActionRefuse,
			wantReason:   reasonSteppingStoneRequired,
			messageParts: []string{platformVersion217, "upgrade to " + platformVersion218 + " first", platformVersion219},
		},
		{
			name:       "no version change at all",
			platform:   migrationPlatform(modeManaged, platformVersion219, platformVersion219),
			from:       platformVersion219,
			to:         platformVersion219,
			wantAction: migrationActionNone,
		},
		{
			name:       "fresh install: nothing is running yet, so nothing can be migrated",
			platform:   migrationPlatform(modeManaged, "", platformVersion219),
			from:       platformVersion219,
			to:         platformVersion219,
			wantAction: migrationActionNone,
		},
		{
			name:       "in flight: this exact target resumes at the recorded phase",
			platform:   migratingPlatform(platformVersion219, otilmv1alpha1.MigrationPhaseDraining),
			from:       platformVersion218,
			to:         platformVersion219,
			wantAction: migrationActionResume,
		},
		{
			name:       "in flight: reverting to the source version while fencing aborts",
			platform:   migratingPlatform(platformVersion218, otilmv1alpha1.MigrationPhaseFencing),
			from:       platformVersion218,
			to:         platformVersion218,
			wantAction: migrationActionAbort,
		},
		{
			name:       "in flight: reverting to the source version while draining aborts",
			platform:   migratingPlatform(platformVersion218, otilmv1alpha1.MigrationPhaseDraining),
			from:       platformVersion218,
			to:         platformVersion218,
			wantAction: migrationActionAbort,
		},
		{
			name:         "in flight: reverting once the cutover began is refused (forward only)",
			platform:     migratingPlatform(platformVersion218, otilmv1alpha1.MigrationPhaseCuttingOver),
			from:         platformVersion218,
			to:           platformVersion218,
			wantAction:   migrationActionRefuse,
			wantReason:   reasonMigrationForwardOnly,
			messageParts: []string{platformVersion219, string(otilmv1alpha1.MigrationPhaseCuttingOver)},
		},
		{
			name:         "in flight: reverting during cleanup is refused (forward only)",
			platform:     migratingPlatform(platformVersion218, otilmv1alpha1.MigrationPhaseCleaningUp),
			from:         platformVersion218,
			to:           platformVersion218,
			wantAction:   migrationActionRefuse,
			wantReason:   reasonMigrationForwardOnly,
			messageParts: []string{platformVersion219},
		},
		{
			name:         "in flight: a third version cannot be requested mid-migration",
			platform:     migratingPlatform(platformVersion217, otilmv1alpha1.MigrationPhaseDraining),
			from:         platformVersion218,
			to:           platformVersion217,
			wantAction:   migrationActionRefuse,
			wantReason:   reasonMigrationInProgress,
			messageParts: []string{platformVersion219, platformVersion218},
		},
		{
			name:       "in flight: clearing spec.version follows the pin, which is the source version",
			platform:   migratingPlatform("", otilmv1alpha1.MigrationPhaseFencing),
			from:       platformVersion218,
			to:         platformVersion218,
			wantAction: migrationActionAbort,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideMigration(c.platform, bundleFor(t, c.from), bundleFor(t, c.to))

			assert.Equal(t, c.wantAction, got.Action)
			assert.Equal(t, c.wantReason, got.Reason)

			if c.wantAction == migrationActionRefuse {
				assert.NotEmpty(t, got.Message, "a refusal must carry an actionable message")
			} else {
				assert.Empty(t, got.Message, "only a refusal carries a message")
			}
			for _, part := range c.messageParts {
				assert.Contains(t, got.Message, part)
			}
			assertNoBrokerCoordinates(t, got.Message)
		})
	}
}

// assertNoBrokerCoordinates enforces the security invariant on everything the trigger layer
// produces: these strings become conditions and Events, so they may carry version strings and
// spec field paths only — never a vhost name, a host, a URL, or a credential.
func assertNoBrokerCoordinates(t *testing.T, message string) {
	t.Helper()
	forbidden := []string{
		bom.LegacyUnscopedVirtualHost, // the 2.17.0/2.18.0 vhost name
		"/",                           // the 2.19.0 vhost name, and any URL path
		pinnedTestVirtualHost,
		"amqp",
		"password",
		"@",
		// The queue-name class: "time-quality." covers every queue the messaging topology
		// carries under that prefix (time-quality.config, time-quality.config-request,
		// time-quality.results) — a literal queue name is a broker coordinate exactly like a
		// vhost or a host, and naming one in a user-facing message is the same leak this helper
		// exists to catch.
		"time-quality.",
	}
	for _, f := range forbidden {
		assert.NotContains(t, strings.ToLower(message), f,
			"trigger messages must never leak a broker coordinate")
	}
}

// TestMigrationInFlight proves the in-flight predicate reads exactly status.upgrade: its
// presence is what makes a migration in flight, its absence what makes one startable.
func TestMigrationInFlight(t *testing.T) {
	p := migrationPlatform(modeManaged, platformVersion218, platformVersion219)
	assert.False(t, migrationInFlight(p), "no status.upgrade means no migration is running")

	assert.True(t, migrationInFlight(migratingPlatform(platformVersion219, otilmv1alpha1.MigrationPhaseFencing)))
}

// TestMessagingExchangesRenamed distinguishes the two shapes of topology change an external
// broker's owner faces: a RENAME (the platform stops publishing to an exchange it used —
// 2.18.0's czertainly became 2.19.0's ilm), which needs manual work on a foreign broker, from
// a purely ADDITIVE change (2.17.0 → 2.18.0 only adds one), which does not.
func TestMessagingExchangesRenamed(t *testing.T) {
	b217, b218, b219 := bundleFor(t, platformVersion217), bundleFor(t, platformVersion218), bundleFor(t, platformVersion219)

	assert.True(t, messagingExchangesRenamed(b218, b219), "the 2.19.0 exchange rename must be detected")
	assert.False(t, messagingExchangesRenamed(b217, b218), "adding an exchange is not a rename")
	assert.False(t, messagingExchangesRenamed(b218, b218), "a bundle is never a rename of itself")
}
