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

// These specs cover the guard that stops a live migration from being STEERED: almost
// everything the engine acts on is re-derived from the current spec each pass, so a mid-flight
// edit of the messaging configuration would silently change which broker is addressed and which
// objects count as the source.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// TestMigrationInputDriftBlocksTheMigration: each of these edits changes what "the source
// topology" and "the target topology" MEAN — the virtual hosts the two renders scope their
// object names to, the broker the drain polls, or whether there is an operator-managed broker at
// all. Following one mid-flight can migrate the wrong topology, make the two sets overlap, or
// finish "successfully" while the original topology is left behind holding messages. So the
// engine refuses to advance and says which fields to put back.
func TestMigrationInputDriftBlocksTheMigration(t *testing.T) {
	cases := []struct {
		name  string
		drift func(*otilmv1alpha1.Platform)
	}{
		{
			name:  "the virtual host is pinned mid-flight, renaming both topologies",
			drift: func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = "somewhere-else" },
		},
		{
			name: "the broker stops being operator-managed",
			drift: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Mode = "external"
				p.Spec.Messaging.Host = "broker.example.com"
			},
		},
		{
			name: "the provisioning service changes hands",
			drift: func(p *otilmv1alpha1.Platform) {
				p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
					Mode: "external", APIURL: "http://provisioner.example.com",
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining,
				otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3})
			r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(0, 0)...)

			// The edit lands after the migration was recorded — a GitOps push mid-flight.
			drifted := storedPlatform(t, r)
			tc.drift(drifted)
			require.NoError(t, r.Update(context.Background(), drifted))

			_, handled, res, err := r.gateMessagingMigration(context.Background(),
				storedPlatform(t, r), bundleFor(t, platformVersion219), platformVersion219)
			require.NoError(t, err, "a refused spec change is a steady state, not a reconcile failure")
			assert.True(t, handled, "nothing may render while the record and the spec disagree")
			assert.Equal(t, ctrl.Result{}, res)

			stored := storedPlatform(t, r)
			require.NotNil(t, stored.Status.Upgrade, "the migration it is protecting is not discarded")
			assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, stored.Status.Upgrade.Phase,
				"a migration whose inputs moved may not advance a phase")
			assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, stored.Status.Phase)
			assert.Equal(t, int32(0), replicasOf(t, r, "scheduler"), "and the fence stays exactly as it was")

			cond := migrationCondition(stored)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, reasonMigrationInputsChanged, cond.Reason)
			assert.Contains(t, cond.Message, "spec.messaging.mode")
			assertNoBrokerCoordinates(t, cond.Message)
			assert.Contains(t, strings.Join(drainEvents(rec), " "), reasonMigrationInputsChanged)
		})
	}
}

// TestMigrationInputsAllowTheSteeringControls: the two fields an operator needs in order to get
// a stuck migration UNSTUCK are deliberately outside the fingerprint. Pinning them would leave
// a blocked migration with no exit at all — spec.version is how a reversible one is aborted, and
// the force authorisation is useless if it cannot be set after the migration has blocked.
func TestMigrationInputsAllowTheSteeringControls(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	pinMigrationInputs(p)
	pinned := p.Status.Upgrade.InputsHash
	require.NotEmpty(t, pinned)

	p.Spec.Version = platformVersion218
	after, ok := migrationInputFingerprint(p)
	require.True(t, ok)
	assert.Equal(t, pinned, after, "reverting spec.version is how a reversible migration is aborted")

	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	after, ok = migrationInputFingerprint(p)
	require.True(t, ok)
	assert.Equal(t, pinned, after, "the destructive escape hatch is set AFTER a migration blocks, by design")
}

// TestMigrationInputFingerprintKeepsCoordinatesOut: the fingerprint is published on
// status.upgrade, so it may carry no broker coordinate — which is why it is a hash of the
// inputs rather than the inputs themselves.
func TestMigrationInputFingerprintKeepsCoordinatesOut(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	p.Spec.Messaging.VirtualHost = sourceVirtualHost
	fingerprint, ok := migrationInputFingerprint(p)
	require.True(t, ok)
	assert.NotContains(t, fingerprint, sourceVirtualHost)
	assertNoBrokerCoordinates(t, fingerprint)

	// It is still a fingerprint: a different virtual host is a different value.
	p.Spec.Messaging.VirtualHost = "another-vhost"
	other, ok := migrationInputFingerprint(p)
	require.True(t, ok)
	assert.NotEqual(t, fingerprint, other)
}

// TestMigrationInputsAreAdoptedWhenUnrecorded: a migration recorded before the fingerprint
// existed carries none, and there is nothing to compare it against. The engine records what the
// inputs are NOW and guards every pass from there, rather than blocking a migration on a value
// it was never given.
func TestMigrationInputsAreAdoptedWhenUnrecorded(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining,
		otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3})
	r, _ := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	adopted := storedPlatform(t, r)
	adopted.Status.Upgrade.InputsHash = ""
	require.NoError(t, r.Status().Update(context.Background(), adopted))

	_, handled, _, err := r.gateMessagingMigration(context.Background(),
		storedPlatform(t, r), bundleFor(t, platformVersion219), platformVersion219)
	require.NoError(t, err)
	assert.False(t, handled, "an unfingerprinted migration is adopted, not blocked")

	stored := storedPlatform(t, r)
	expected, ok := migrationInputFingerprint(stored)
	require.True(t, ok)
	assert.Equal(t, expected, stored.Status.Upgrade.InputsHash, "and the inputs are pinned from here on")
}

// TestMigrationInputFingerprintNeedsBothBundles: with a version this build does not carry there
// is nothing meaningful to fingerprint, and the phase's own source-bundle check is what reports
// that — in its own words, rather than as spec drift.
func TestMigrationInputFingerprintNeedsBothBundles(t *testing.T) {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining)
	p.Status.Upgrade.FromVersion = unknownPlatformVersion
	_, ok := migrationInputFingerprint(p)
	assert.False(t, ok)

	plain := migrationGatePlatform()
	_, ok = migrationInputFingerprint(plain)
	assert.False(t, ok, "no migration, nothing to fingerprint")
}
