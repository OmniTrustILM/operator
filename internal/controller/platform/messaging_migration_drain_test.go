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

// The drain is the one phase whose decision comes from OUTSIDE the cluster, so these tests
// script the broker instead: a fake BrokerAdmin makes "the queue is empty", "the queue is not",
// and "the broker did not answer" three separate, repeatable inputs, and every pass runs from a
// freshly re-read Platform exactly as Reconcile does.
//
// The safety-critical claim under test is one-directional: NOTHING except a complete,
// affirmative, repeated "empty" may move the migration forward.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/rabbitmq"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// The source (2.18.0) coordinates the drain must address. Stated literally so a change to the
// bundle data, the vhost precedence or the exchange-by-type lookup fails this test rather than
// silently moving the poll to another virtual host.
const (
	sourceVirtualHost   = "czertainly"
	sourceProxyExchange = "czertainly-proxy"
	adminSecretName     = "ilm-messaging-administrator-user-credentials" //nolint:gosec // G101: a Secret name, not a credential
	adminUsername       = "admin-user"
	adminPassword       = "admin-pass" //nolint:gosec // G101: a fixture value for a fake broker, not a real credential
	managementEndpoint  = "http://ilm-messaging.ns.svc:15672"
)

// brokerCall records one management-API call the drain made.
type brokerCall struct {
	method   string
	vhost    string
	exchange string
}

// fakeBrokerAdmin is a scripted rabbitmq.BrokerAdmin: a fixed queue listing, a fixed set of
// exchange bindings, and optional failures, recording every call. It is goroutine-safe because
// the envtest suite shares one with a manager reconciling in the background.
type fakeBrokerAdmin struct {
	mu        sync.Mutex
	queues    []rabbitmq.QueueState
	queuesErr error
	bound     map[string][]string
	boundErr  error
	// connections / connectionsErr and closeErr script the two calls only the CLEANUP makes:
	// the connection count its final barrier requires to be zero, and the force-close its
	// forced path issues.
	connections    int
	connectionsErr error
	closeErr       error
	calls          []brokerCall
}

// Queues returns the scripted queue listing (or the scripted failure).
func (f *fakeBrokerAdmin) Queues(_ context.Context, vhost string) ([]rabbitmq.QueueState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, brokerCall{method: "Queues", vhost: vhost})
	if f.queuesErr != nil {
		return nil, f.queuesErr
	}
	return f.queues, nil
}

// BoundQueues returns the queues scripted as bound to the exchange (or the scripted failure).
func (f *fakeBrokerAdmin) BoundQueues(_ context.Context, vhost, exchange string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, brokerCall{method: "BoundQueues", vhost: vhost, exchange: exchange})
	if f.boundErr != nil {
		return nil, f.boundErr
	}
	return f.bound[exchange], nil
}

// Connections returns the scripted open-connection count (or the scripted failure). The DRAIN
// never asks — consumers and connections gate the cleanup, not the drain — which the call log
// proves; the cleanup's final barrier does.
func (f *fakeBrokerAdmin) Connections(_ context.Context, vhost string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, brokerCall{method: "Connections", vhost: vhost})
	if f.connectionsErr != nil {
		return 0, f.connectionsErr
	}
	return f.connections, nil
}

// CloseConnections records the force-close the cleanup's forced path issues (or the scripted
// failure). The drain never closes anything.
func (f *fakeBrokerAdmin) CloseConnections(_ context.Context, vhost string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, brokerCall{method: "CloseConnections", vhost: vhost})
	return f.closeErr
}

// callLog returns a copy of the recorded calls.
func (f *fakeBrokerAdmin) callLog() []brokerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]brokerCall(nil), f.calls...)
}

// setQueues replaces the scripted listing and clears any scripted failure.
func (f *fakeBrokerAdmin) setQueues(queues []rabbitmq.QueueState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues, f.queuesErr = queues, nil
}

// emptyQueue is a queue the broker reports as holding nothing.
func emptyQueue(name string) rabbitmq.QueueState {
	return rabbitmq.QueueState{Name: name}
}

// sourceQueueListing is what the 2.18.0 source virtual host reports once the fenced producers
// have stopped: every work queue empty, and both latest-only retention queues holding the
// single message they are DESIGNED to hold. Extra queues can be appended per test.
func sourceQueueListing(extra ...rabbitmq.QueueState) []rabbitmq.QueueState {
	listing := []rabbitmq.QueueState{
		emptyQueue("core"),
		emptyQueue("core.audit-logs"),
		emptyQueue("core.notifications"),
		emptyQueue("core.scheduler"),
		emptyQueue("core.actions"),
		emptyQueue("core.validation"),
		emptyQueue("core.events"),
		emptyQueue("time-quality.results"),
		{Name: "time-quality.config", MessagesReady: 1},
		{Name: "time-quality.config-request", MessagesReady: 1},
	}
	return append(listing, extra...)
}

// administratorSecret is the Topology-generated administrator credentials Secret the drain
// authenticates with.
func administratorSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: migrationTestNS},
		Data:       map[string][]byte{"username": []byte(adminUsername), "password": []byte(adminPassword)},
	}
}

// drainReconciler builds a reconciler whose broker client is the given fake, recording the
// arguments the factory was handed so a test can prove WHICH broker and WHICH credentials the
// drain used. The administrator Secret is always present unless a test replaces it.
func drainReconciler(t *testing.T, p *otilmv1alpha1.Platform, admin rabbitmq.BrokerAdmin, funcs interceptor.Funcs, objs ...client.Object) (*Reconciler, *record.FakeRecorder, *brokerCall) {
	t.Helper()
	r, rec := migrationReconciler(t, p, funcs, append([]client.Object{administratorSecret()}, objs...)...)
	built := &brokerCall{}
	r.BrokerAdmins = func(endpoint, username, password string) rabbitmq.BrokerAdmin {
		built.method, built.vhost, built.exchange = endpoint, username, password
		return admin
	}
	return r, rec, built
}

// drainPass runs one whole gate pass from a FRESHLY READ Platform, as Reconcile does. Re-reading
// is not a detail: the drain re-pins spec.version to the source in memory, and a second pass on
// that same copy would read as a revert.
func drainPass(t *testing.T, r *Reconciler) (migrationRender, bool, ctrl.Result, error) {
	t.Helper()
	_, to := migrationBundles(t)
	return r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
}

// drainingPlatform is a platform mid-drain, with the given consecutive-clean-poll count and one
// producer already fenced.
func drainingPlatform(polls int32) *otilmv1alpha1.Platform {
	p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining,
		otilmv1alpha1.FencedWorkload{Name: "scheduler", Kind: "Deployment", Replicas: 3})
	p.Status.Upgrade.CleanDrainPolls = polls
	return p
}

// advanceDrainPollClock back-dates the recorded next-poll floor, standing in for the migration
// cadence a real reconcile waits out between two samples. Every looping drain spec calls it
// BETWEEN passes rather than skipping the floor, because the floor is the thing under test: a
// loop that could poll without it would be proving nothing about "consecutive".
func advanceDrainPollClock(t *testing.T, r *Reconciler) {
	t.Helper()
	p := storedPlatform(t, r)
	require.NotNil(t, p.Status.Upgrade)
	past := metav1.NewTime(time.Now().Add(-time.Second))
	p.Status.Upgrade.NextDrainPollAt = &past
	require.NoError(t, r.Status().Update(context.Background(), p))
}

// storedPolls returns the persisted consecutive-clean-poll count.
func storedPolls(t *testing.T, r *Reconciler) int32 {
	t.Helper()
	u := storedPlatform(t, r).Status.Upgrade
	require.NotNil(t, u)
	return u.CleanDrainPolls
}

// storedPhase returns the persisted migration phase.
func storedPhase(t *testing.T, r *Reconciler) otilmv1alpha1.MigrationPhase {
	t.Helper()
	u := storedPlatform(t, r).Status.Upgrade
	require.NotNil(t, u)
	return u.Phase
}

// --- what advances the drain, and what does not ------------------------------

// TestDrainAdvancesOnThreeConsecutiveCleanPolls is the whole contract in one walk: an empty
// source virtual host, sampled three times, hands the migration to the cutover — and not one
// poll earlier.
func TestDrainAdvancesOnThreeConsecutiveCleanPolls(t *testing.T) {
	p := drainingPlatform(0)
	admin := &fakeBrokerAdmin{queues: sourceQueueListing()}
	r, rec, built := drainReconciler(t, p, admin, interceptor.Funcs{})

	for poll := int32(1); poll < migrationCleanDrainPolls; poll++ {
		render, handled, _, err := drainPass(t, r)
		require.NoError(t, err)
		assert.False(t, handled, "a drain still counting keeps the platform reconciling its source version")
		assert.Equal(t, platformVersion218, render.version)
		assert.True(t, render.requeue, "the drain asks for the migration cadence")
		assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r))
		assert.Equal(t, poll, storedPolls(t, r), "each clean poll is persisted, so a restart resumes the count")
		advanceDrainPollClock(t, r)
	}

	_, handled, res, err := drainPass(t, r)
	require.NoError(t, err)
	assert.True(t, handled, "past the drain the pass renders nothing further")
	assert.Equal(t, ctrl.Result{RequeueAfter: migrationRequeueAfter}, res)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r))
	assert.Zero(t, storedPolls(t, r), "the counter belongs to the phase that used it")

	// The poll addressed the SOURCE virtual host and the SOURCE proxy exchange, authenticated
	// as the administrator user read out of the Secret — never the target's.
	assert.Equal(t, managementEndpoint, built.method)
	assert.Equal(t, adminUsername, built.vhost)
	assert.Equal(t, adminPassword, built.exchange)
	for _, c := range admin.callLog() {
		assert.Equal(t, sourceVirtualHost, c.vhost)
		assert.Contains(t, []string{"Queues", "BoundQueues"}, c.method,
			"the drain neither counts nor closes connections; that gates the cleanup")
		if c.method == "BoundQueues" {
			assert.Equal(t, sourceProxyExchange, c.exchange)
		}
	}

	cond := migrationCondition(storedPlatform(t, r))
	require.NotNil(t, cond)
	assertNoBrokerCoordinates(t, cond.Message)
	assert.Contains(t, strings.Join(drainEvents(rec), " "), eventMigrationPhase)
}

// TestDrainResetsTheCountOnAnError is the fail-closed rule: a poll that did not complete is not
// evidence of anything, least of all emptiness.
func TestDrainResetsTheCountOnAnError(t *testing.T) {
	cases := []struct {
		name  string
		admin *fakeBrokerAdmin
	}{
		{
			name:  "the queue listing failed",
			admin: &fakeBrokerAdmin{queuesErr: &rabbitmq.Error{StatusCode: 503, Message: "listing queues", Retryable: true}},
		},
		{
			name:  "the bindings listing failed, so a dynamic queue may be unaccounted for",
			admin: &fakeBrokerAdmin{queues: sourceQueueListing(), boundErr: errors.New("bindings unavailable")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := drainingPlatform(migrationCleanDrainPolls - 1)
			r, _, _ := drainReconciler(t, p, tc.admin, interceptor.Funcs{})

			render, handled, _, err := drainPass(t, r)
			require.NoError(t, err, "an unreachable broker is a wait, not a reconcile failure")
			assert.False(t, handled)
			assert.True(t, render.requeue)
			assert.Equal(t, platformVersion218, render.version, "the platform keeps running its source version")
			assert.Zero(t, storedPolls(t, r), "an error can never be counted as progress")
			assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r))
		})
	}
}

// TestDrainResetsTheCountOnADirtyQueue: a count of consecutive clean polls means consecutive.
func TestDrainResetsTheCountOnADirtyQueue(t *testing.T) {
	cases := []struct {
		name  string
		queue rabbitmq.QueueState
	}{
		{name: "messages still ready", queue: rabbitmq.QueueState{Name: "core.events", MessagesReady: 4}},
		{
			name: "messages delivered but unacknowledged",
			// A consumer that dies hands these straight back to ready, so they are still in the
			// virtual host the migration is about to reclaim.
			queue: rabbitmq.QueueState{Name: "core.events", MessagesUnacked: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := drainingPlatform(migrationCleanDrainPolls - 1)
			listing := sourceQueueListing()
			listing[6] = tc.queue // core.events
			r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{queues: listing}, interceptor.Funcs{})

			_, handled, _, err := drainPass(t, r)
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Zero(t, storedPolls(t, r))
			assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r),
				"one dirty queue holds the whole migration")
		})
	}
}

// TestDrainIgnoresLatestOnlyRetentionQueues: the time-quality config queues are bounded to a
// single message their publishers keep refreshing. Waiting for them would hang every migration
// forever, so a drain must complete WITH them non-empty.
func TestDrainIgnoresLatestOnlyRetentionQueues(t *testing.T) {
	p := drainingPlatform(migrationCleanDrainPolls - 1)
	listing := sourceQueueListing()
	listing[8] = rabbitmq.QueueState{Name: "time-quality.config", MessagesReady: 1}
	listing[9] = rabbitmq.QueueState{Name: "time-quality.config-request", MessagesReady: 1}
	r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{queues: listing}, interceptor.Funcs{})

	_, handled, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r),
		"a queue designed never to empty cannot be allowed to block the drain")
}

// TestDrainDiscoversDynamicQueuesByBindingNotByName: per-proxy queues exist only once a proxy
// has enrolled, so the drain asks the broker which queues the source proxy exchange feeds. A
// queue that merely LOOKS like one is not the migration's business.
func TestDrainDiscoversDynamicQueuesByBindingNotByName(t *testing.T) {
	t.Run("a bound queue still holding messages blocks the drain", func(t *testing.T) {
		p := drainingPlatform(migrationCleanDrainPolls - 1)
		admin := &fakeBrokerAdmin{
			queues: sourceQueueListing(rabbitmq.QueueState{Name: "instance-7a3f", MessagesReady: 2}),
			bound:  map[string][]string{sourceProxyExchange: {"instance-7a3f"}},
		}
		r, _, _ := drainReconciler(t, p, admin, interceptor.Funcs{})

		_, _, _, err := drainPass(t, r)
		require.NoError(t, err)
		assert.Zero(t, storedPolls(t, r), "a queue with no recognisable name still counts, because it is BOUND")
		assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r))
	})

	t.Run("an unbound queue is not the migration's to drain, whatever it is called", func(t *testing.T) {
		p := drainingPlatform(migrationCleanDrainPolls - 1)
		admin := &fakeBrokerAdmin{
			// Named exactly like a per-proxy queue, but bound to nothing this platform owns.
			queues: sourceQueueListing(rabbitmq.QueueState{Name: "proxy.someone-elses", MessagesReady: 9}),
			bound:  map[string][]string{},
		}
		r, _, _ := drainReconciler(t, p, admin, interceptor.Funcs{})

		_, _, _, err := drainPass(t, r)
		require.NoError(t, err)
		assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r),
			"membership comes from the bindings, never from a name pattern")
	})
}

// TestDrainAdvancesWithLiveConsumersOnDynamicQueues is the anti-deadlock case, and the reason
// the drain looks at DEPTH only: a healthy remote proxy is permanently attached to its own
// queue and holds an open connection to the virtual host, so a drain that waited for either to
// go away would never finish while the platform was still serving the clients the migration
// exists to keep. The call log is the proof — the drain never asks about connections at all;
// that question belongs to the cleanup, which is allowed to wait for it.
func TestDrainAdvancesWithLiveConsumersOnDynamicQueues(t *testing.T) {
	p := drainingPlatform(0)
	admin := &fakeBrokerAdmin{
		queues:      sourceQueueListing(rabbitmq.QueueState{Name: "instance-7a3f"}),
		bound:       map[string][]string{sourceProxyExchange: {"instance-7a3f"}},
		connections: 3,
	}
	r, _, _ := drainReconciler(t, p, admin, interceptor.Funcs{})

	for poll := int32(1); poll < migrationCleanDrainPolls; poll++ {
		_, _, _, err := drainPass(t, r)
		require.NoError(t, err)
		assert.Equal(t, poll, storedPolls(t, r))
		advanceDrainPollClock(t, r)
	}
	_, _, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r),
		"an attached client on an empty queue must never stall the migration")
	for _, c := range admin.callLog() {
		assert.NotEqual(t, "Connections", c.method, "the drain never consults the connection count")
	}
}

// --- the deadline and its authorised exit ------------------------------------

// TestDrainTimeoutRestoresTheFence: past its budget the drain stops WAITING — the producers
// come back, the platform runs its source version at full strength, and the reason says how to
// get out.
func TestDrainTimeoutRestoresTheFence(t *testing.T) {
	p := drainingPlatform(migrationCleanDrainPolls - 1)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	admin := &fakeBrokerAdmin{queues: sourceQueueListing()}
	r, rec, _ := drainReconciler(t, p, admin, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	render, handled, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled, "the platform keeps reconciling — on its source version")
	assert.Equal(t, platformVersion218, render.version)
	assert.False(t, render.requeue, "nothing will change until the user chooses an exit")

	assert.Equal(t, int32(3), replicasOf(t, r, "scheduler"), "the fence is lifted at the deadline")
	assert.Empty(t, admin.callLog(), "an expired phase does not poll: it has already stopped waiting")

	stored := storedPlatform(t, r)
	require.NotNil(t, stored.Status.Upgrade, "the record survives so the migration cannot restart itself")
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, stored.Status.Upgrade.Phase)
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMigrationDrainTimeout, cond.Reason)
	assertNoBrokerCoordinates(t, cond.Message)
	assert.Contains(t, strings.Join(drainEvents(rec), " "), eventMigrationBlocked)
}

// TestDrainTimeoutHonoursAnAuthorisedForcedCutover: the blocked state's message offers exactly
// one way forward, and setting it must actually take it — including re-fencing the producers
// the block itself let back up.
func TestDrainTimeoutHonoursAnAuthorisedForcedCutover(t *testing.T) {
	t.Run("the fence is still held", func(t *testing.T) {
		p := drainingPlatform(0)
		p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
		p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
		admin := &fakeBrokerAdmin{queues: sourceQueueListing(rabbitmq.QueueState{Name: "core", MessagesReady: 12})}
		r, rec, _ := drainReconciler(t, p, admin, interceptor.Funcs{}, producerWorkloads(0, 0)...)

		_, handled, res, err := drainPass(t, r)
		require.NoError(t, err)
		assert.True(t, handled)
		assert.Equal(t, ctrl.Result{RequeueAfter: migrationRequeueAfter}, res)
		assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r),
			"the authorisation proceeds past a virtual host that never drained")
		assert.Equal(t, []otilmv1alpha1.FencedWorkload{{Name: "scheduler", Kind: "Deployment", Replicas: 3}},
			fenced(storedPlatform(t, r)), "an already-held fence is left exactly as it was")

		events := strings.Join(drainEvents(rec), " ")
		assert.Contains(t, events, eventMigrationForcedCutover)
		assertNoBrokerCoordinates(t, events)
	})

	t.Run("the fence was already lifted by an earlier block", func(t *testing.T) {
		p := migratingGatePlatform(otilmv1alpha1.MigrationPhaseDraining) // nothing fenced any more
		p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
		p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
		r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{}, interceptor.Funcs{}, producerWorkloads(2, 3)...)

		_, handled, _, err := drainPass(t, r)
		require.NoError(t, err)
		assert.True(t, handled)
		assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, storedPhase(t, r))
		assert.ElementsMatch(t, []otilmv1alpha1.FencedWorkload{
			{Name: "api-gateway", Kind: "Deployment", Replicas: 2},
			{Name: "scheduler", Kind: "Deployment", Replicas: 3},
		}, fenced(storedPlatform(t, r)), "the producers the block released are stopped again before the cutover")
		assert.Zero(t, replicasOf(t, r, "api-gateway"))
		assert.Zero(t, replicasOf(t, r, "scheduler"))
	})
}

// TestDrainTimeoutWithAForceForAnotherVersionStaysBlocked: the authorisation names the version
// it authorises at the deadline too, so a value left behind by an older migration takes the
// blocked exit rather than the forced one — nothing is cut over and nothing is discarded.
func TestDrainTimeoutWithAForceForAnotherVersionStaysBlocked(t *testing.T) {
	p := drainingPlatform(0)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion217
	admin := &fakeBrokerAdmin{queues: sourceQueueListing(rabbitmq.QueueState{Name: "core", MessagesReady: 12})}
	r, rec, _ := drainReconciler(t, p, admin, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	_, handled, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r),
		"a stale authorisation may not move the migration on")
	assert.Equal(t, int32(3), replicasOf(t, r, "scheduler"), "the fence is lifted, as it is with no authorisation at all")

	cond := migrationCondition(storedPlatform(t, r))
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationDrainTimeout, cond.Reason)
	assert.NotContains(t, strings.Join(drainEvents(rec), " "), eventMigrationForcedCutover)
}

// TestForceBeforeTheDeadlineStillWaitsForACleanDrain pins what the authorisation is FOR: it
// overrides the stop at spec.messaging.managed.drainTimeout, not the drain itself. Set while the
// phase still has budget, it changes nothing — the source virtual host is still given its full
// window to empty cleanly, and only a deadline it does not meet spends the authorisation.
func TestForceBeforeTheDeadlineStillWaitsForACleanDrain(t *testing.T) {
	p := drainingPlatform(0)
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	admin := &fakeBrokerAdmin{queues: sourceQueueListing(rabbitmq.QueueState{Name: "core", MessagesReady: 12})}
	r, rec, _ := drainReconciler(t, p, admin, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	render, handled, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, platformVersion218, render.version, "the platform still serves its source version")
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r))
	assert.Zero(t, storedPolls(t, r), "the queue that still holds messages is not counted as clean")
	assert.Zero(t, replicasOf(t, r, "scheduler"), "the producers stay fenced")
	assert.NotContains(t, strings.Join(drainEvents(rec), " "), eventMigrationForcedCutover)
}

// TestMigrationForceCutoverAuthorized: the authorisation is bound to ONE ATTEMPT, so neither a
// value naming another version nor one this attempt inherited can authorise data loss here.
func TestMigrationForceCutoverAuthorized(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		want   bool
	}{
		{name: "unset", mutate: func(*otilmv1alpha1.Platform) {}},
		{
			name:   "naming this migration's target",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219 },
			want:   true,
		},
		{
			name:   "left over from an older migration",
			mutate: func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion217 },
		},
		{
			name: "naming this target, but already present when this attempt began",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
				p.Status.Upgrade.ForceCarriedOver = true
			},
		},
		{
			name: "already consumed by this attempt, and since cleared from the spec",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Managed.ForceCutoverForVersion = ""
				p.Status.Upgrade.ForceAuthorized = true
			},
			want: true,
		},
		{
			name: "no migration to authorise",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
				p.Status.Upgrade = nil
			},
		},
		{
			name: "no managed messaging block at all",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Managed = nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := drainingPlatform(0)
			tc.mutate(p)
			assert.Equal(t, tc.want, migrationForceCutoverAuthorized(p))
		})
	}
}

// TestForceLeftBehindByAnAbortedAttemptAuthorizesNothing is the destructive-authorisation hole
// the attempt binding closes. The field is a plain version string, so a value an operator set
// for a migration that was then aborted (or blocked and reverted) sits in the spec naming the
// same target — and target-scoping ALONE would let it silently authorise every later migration
// to that version to discard whatever the source virtual host still holds. It must not: the
// attempt that inherited it records that, and takes the blocked exit instead.
func TestForceLeftBehindByAnAbortedAttemptAuthorizesNothing(t *testing.T) {
	p := migrationGatePlatform()
	// The leftover: set for an earlier attempt at this very version, never cleared.
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	_, to := migrationBundles(t)
	// The producers never wind down, so the attempt stays in Fencing and reaches that phase's
	// deadline — the exit an inherited authorisation must not be able to take.
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, runningProducerWorkloads()...)

	// The new attempt begins, and inherits the value.
	_, _, _, err := r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
	require.NoError(t, err)
	begun := storedPlatform(t, r)
	require.NotNil(t, begun.Status.Upgrade)
	assert.True(t, begun.Status.Upgrade.ForceCarriedOver, "a value the attempt did not ask for is recorded as inherited")
	assert.False(t, migrationForceCutoverAuthorized(begun))

	// Its fencing phase now outlives the deadline: with a genuine authorisation this would cut
	// over and discard. With an inherited one it must block instead.
	begun.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	require.NoError(t, r.Status().Update(context.Background(), begun))
	drainEvents(rec)

	_, _, _, err = r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
	require.NoError(t, err)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseFencing, stored.Status.Upgrade.Phase,
		"an inherited authorisation may not move the migration on")
	cond := migrationCondition(stored)
	require.NotNil(t, cond)
	assert.Equal(t, reasonMigrationDrainTimeout, cond.Reason)
	assert.NotContains(t, strings.Join(drainEvents(rec), " "), eventMigrationForcedCutover)
}

// TestForceReArmedForThisAttemptAuthorizes is the other half: an inherited value stops being
// inherited the moment the operator CLEARS it, so setting it again — deliberately, for the
// attempt they can now see blocked in the platform's status — is a real authorisation.
func TestForceReArmedForThisAttemptAuthorizes(t *testing.T) {
	p := drainingPlatform(0)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	p.Status.Upgrade.ForceCarriedOver = true
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	_, to := migrationBundles(t)
	r, rec := migrationReconciler(t, p, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	// Pass one: the operator clears the leftover. The engine records that it is out of play.
	cleared := storedPlatform(t, r)
	cleared.Spec.Messaging.Managed.ForceCutoverForVersion = ""
	require.NoError(t, r.Update(context.Background(), cleared))
	_, _, _, err := r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
	require.NoError(t, err)
	assert.False(t, storedPlatform(t, r).Status.Upgrade.ForceCarriedOver)

	// Pass two: they set it again, for this attempt.
	rearmed := storedPlatform(t, r)
	rearmed.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	require.NoError(t, r.Update(context.Background(), rearmed))
	drainEvents(rec)

	_, handled, _, err := r.gateMessagingMigration(context.Background(), storedPlatform(t, r), to, platformVersion219)
	require.NoError(t, err)
	assert.True(t, handled)

	stored := storedPlatform(t, r)
	assert.Equal(t, otilmv1alpha1.MigrationPhaseCuttingOver, stored.Status.Upgrade.Phase)
	assert.True(t, stored.Status.Upgrade.ForceAuthorized,
		"the authorisation is recorded as consumed before the step it permits")
	assert.Contains(t, strings.Join(drainEvents(rec), " "), eventMigrationForcedCutover)
}

// TestForcedCutoverRecordsItsAuthorizationBeforeActing: the consumed flag is what carries the
// operator's decision forward to the CLEANUP, which is the phase that actually discards. A
// forced cutover that recorded nothing would let a later spec edit quietly turn the reclaim back
// into a waiting one — a different outcome from the one that was authorised.
func TestForcedCutoverRecordsItsAuthorizationBeforeActing(t *testing.T) {
	p := drainingPlatform(0)
	p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
	r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{}, interceptor.Funcs{}, producerWorkloads(0, 0)...)

	_, _, _, err := drainPass(t, r)
	require.NoError(t, err)
	require.True(t, storedPlatform(t, r).Status.Upgrade.ForceAuthorized)

	// The operator clears the field after the cutover has acted on it.
	cleared := storedPlatform(t, r)
	cleared.Spec.Messaging.Managed.ForceCutoverForVersion = ""
	require.NoError(t, r.Update(context.Background(), cleared))
	assert.True(t, migrationForceCutoverAuthorized(storedPlatform(t, r)),
		"the reclaim finishes the migration the operator authorised, not a different one")
}

// --- the poll's own inputs ---------------------------------------------------

// TestSourceBrokerAdminFailsClosed: every way the client cannot be built is an ERROR, never a
// silently skipped poll that the drain would then count as clean.
func TestSourceBrokerAdminFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*otilmv1alpha1.Platform)
		objects []client.Object
	}{
		{
			name: "an external broker is not the operator's to poll",
			mutate: func(p *otilmv1alpha1.Platform) {
				p.Spec.Messaging.Mode = "external"
				p.Spec.Messaging.Managed = nil
			},
			objects: []client.Object{administratorSecret()},
		},
		{
			name: "the source version's topology has no administrator user to poll as",
			mutate: func(p *otilmv1alpha1.Platform) {
				// The pre-2.18.0 single-user layout: the drain refuses rather than reaching for
				// another user's credentials.
				p.Spec.Version = platformVersion217
			},
			objects: []client.Object{administratorSecret()},
		},
		{
			name:   "the administrator credentials Secret does not exist",
			mutate: func(*otilmv1alpha1.Platform) {},
		},
		{
			name:   "the Secret carries no password",
			mutate: func(*otilmv1alpha1.Platform) {},
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: migrationTestNS},
				Data:       map[string][]byte{"username": []byte(adminUsername)},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := drainingPlatform(0)
			tc.mutate(p)
			r, _ := migrationReconciler(t, p, interceptor.Funcs{}, tc.objects...)
			r.BrokerAdmins = func(string, string, string) rabbitmq.BrokerAdmin { return &fakeBrokerAdmin{} }

			admin, err := r.sourceBrokerAdmin(context.Background(), p)
			require.Error(t, err)
			assert.Nil(t, admin)
		})
	}
}

// TestDrainWithoutABrokerToPollHoldsTheMigration: the same failure seen from the phase — the
// drain waits on its deadline rather than advancing on a poll it could not make.
func TestDrainWithoutABrokerToPollHoldsTheMigration(t *testing.T) {
	p := drainingPlatform(migrationCleanDrainPolls - 1)
	r, _ := migrationReconciler(t, p, interceptor.Funcs{}) // no administrator Secret, no injected factory

	_, handled, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, storedPolls(t, r))
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r))
}

// TestNewBrokerAdminDefaultsToTheRealClient: with nothing injected the reconciler still has a
// client — the production path — rather than a nil interface the drain would panic on.
func TestNewBrokerAdminDefaultsToTheRealClient(t *testing.T) {
	r := &Reconciler{}
	assert.NotNil(t, r.newBrokerAdmin()(managementEndpoint, adminUsername, adminPassword))

	injected := &fakeBrokerAdmin{}
	r.BrokerAdmins = func(string, string, string) rabbitmq.BrokerAdmin { return injected }
	assert.Same(t, injected, r.newBrokerAdmin()(managementEndpoint, adminUsername, adminPassword))
}

// --- the classification, on its own ------------------------------------------

// TestMigrationDrainableQueues pins the queue set the 2.18.0 source virtual host must empty:
// the work queues, no retention queue, and every dynamically bound one the bundle does not
// already list.
func TestMigrationDrainableQueues(t *testing.T) {
	source := bundleFor(t, platformVersion218)

	assert.ElementsMatch(t, []string{
		"core", "core.audit-logs", "core.notifications", "core.scheduler",
		"core.actions", "core.validation", "core.events", "time-quality.results",
	}, migrationDrainableQueues(source, nil))

	t.Run("dynamic queues are added, and never duplicated", func(t *testing.T) {
		got := migrationDrainableQueues(source, []string{"instance-1", "instance-1", "core.events"})
		assert.Contains(t, got, "instance-1")
		assert.Len(t, got, 9, "the repeat and the queue the bundle already lists add nothing")
	})

	t.Run("a bound retention queue stays exempt", func(t *testing.T) {
		assert.NotContains(t, migrationDrainableQueues(source, []string{"time-quality.config"}), "time-quality.config",
			"the exemption is a property of the queue, not of how it was discovered")
	})
}

// TestOutstandingQueues pins what counts as still holding messages.
func TestOutstandingQueues(t *testing.T) {
	states := []rabbitmq.QueueState{
		{Name: "a"},
		{Name: "b", MessagesReady: 3},
		{Name: "c", MessagesUnacked: 1},
		{Name: "d"},
	}
	assert.Zero(t, outstandingQueues([]string{"a", "d"}, states), "an attached consumer is not a message")
	assert.Equal(t, 1, outstandingQueues([]string{"b"}, states))
	assert.Equal(t, 2, outstandingQueues([]string{"a", "b", "c"}, states))
	assert.Zero(t, outstandingQueues([]string{"gone"}, states), "a queue the broker does not list holds nothing")
	assert.Zero(t, outstandingQueues(nil, states))
}

// TestMigrationProxyExchangeIsFoundByType: the proxy exchange is the TOPIC one, in every
// bundle — 2.19.0 renamed both exchanges, and the drain follows without a code change.
func TestMigrationProxyExchangeIsFoundByType(t *testing.T) {
	cases := map[string]string{
		platformVersion218: sourceProxyExchange,
		platformVersion219: "ilm-proxy",
	}
	for version, want := range cases {
		t.Run(version, func(t *testing.T) {
			admin := &fakeBrokerAdmin{bound: map[string][]string{want: {"instance-1"}}}
			bound, err := boundProxyQueues(context.Background(), admin, bundleFor(t, version), sourceVirtualHost)
			require.NoError(t, err)
			assert.Equal(t, []string{"instance-1"}, bound)
		})
	}

	t.Run("a topology with no proxy exchange asks the broker nothing", func(t *testing.T) {
		admin := &fakeBrokerAdmin{}
		bound, err := boundProxyQueues(context.Background(), admin, bundleFor(t, platformVersion217), sourceVirtualHost)
		require.NoError(t, err)
		assert.Empty(t, bound)
		assert.Empty(t, admin.callLog())
	})
}

// TestIsLatestOnlyRetention pins the data-level rule the exemption is derived from, so a future
// bundle's retention queue inherits it and a plain queue never does.
func TestIsLatestOnlyRetention(t *testing.T) {
	assert.False(t, bom.MessagingQueue{Name: "core"}.IsLatestOnlyRetention())
	assert.True(t, bom.MessagingQueue{
		Name: "latest", Arguments: map[string]interface{}{"x-max-length": int64(1), "x-overflow": "drop-head"},
	}.IsLatestOnlyRetention())
	assert.False(t, bom.MessagingQueue{
		Name: "bounded", Arguments: map[string]interface{}{"x-max-length": int64(5000)},
	}.IsLatestOnlyRetention(), "a merely bounded queue is still expected to empty")
	assert.False(t, bom.MessagingQueue{
		Name: "other", Arguments: map[string]interface{}{"x-expires": int64(1)},
	}.IsLatestOnlyRetention())
}

// --- the writes the drain depends on -----------------------------------------

// TestDrainStopsWhenItsProgressCannotBeRecorded: the counter is state the next pass acts on, so
// a pass that cannot persist it must stop rather than carry an unrecorded count forward.
func TestDrainStopsWhenItsProgressCannotBeRecorded(t *testing.T) {
	cases := []struct {
		name    string
		polls   int32
		listing []rabbitmq.QueueState
	}{
		{
			name:    "the reset after a dirty poll",
			polls:   migrationCleanDrainPolls - 1,
			listing: sourceQueueListing(rabbitmq.QueueState{Name: "core", MessagesReady: 1}),
		},
		{name: "the increment after a clean one", polls: 0, listing: sourceQueueListing()},
		{name: "the hand-over to the cutover", polls: migrationCleanDrainPolls - 1, listing: sourceQueueListing()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := drainingPlatform(tc.polls)
			r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{queues: tc.listing},
				failingStatusUpdate(errors.New("status write rejected")))

			_, handled, _, err := r.gateMessagingMigration(context.Background(), p, bundleFor(t, platformVersion219), platformVersion219)
			require.Error(t, err, "a write the cluster refused must surface")
			assert.True(t, handled, "and it must stop the pass")
		})
	}
}

// TestForcedCutoverStopsOnAFailedStep: the forced path is still the engine, so a step the
// cluster refuses stops the pass rather than moving a migration on from a state nobody recorded.
func TestForcedCutoverStopsOnAFailedStep(t *testing.T) {
	cases := map[string]interceptor.Funcs{
		"the producers cannot be stopped again": failingPatch(errors.New("patch rejected")),
		"the hand-over cannot be recorded":      failingStatusUpdate(errors.New("status write rejected")),
	}
	for name, funcs := range cases {
		t.Run(name, func(t *testing.T) {
			p := drainingPlatform(0)
			p.Status.Upgrade.PhaseStartedAt = metav1.NewTime(time.Now().Add(-time.Hour))
			p.Spec.Messaging.Managed.ForceCutoverForVersion = platformVersion219
			r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{}, funcs, producerWorkloads(2, 3)...)

			_, handled, _, err := r.gateMessagingMigration(context.Background(), p, bundleFor(t, platformVersion219), platformVersion219)
			require.Error(t, err)
			assert.True(t, handled)
			assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r),
				"the migration stays where the cluster last agreed it was")
		})
	}
}

// TestDrainDegradesOnAnUnknownSourceVersion: an operator rolled back past the version a
// migration started from cannot even work out which virtual host to poll, and says so.
func TestDrainDegradesOnAnUnknownSourceVersion(t *testing.T) {
	p := drainingPlatform(0)
	p.Status.Upgrade.FromVersion = unknownPlatformVersion
	r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{}, interceptor.Funcs{})

	_, handled, _, err := r.gateMessagingMigration(context.Background(), p, bundleFor(t, platformVersion219), platformVersion219)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, storedPlatform(t, r).Status.Phase)
}

// TestWriteCleanDrainPollsRecordsTheSampleAndItsFloor: each sample persists BOTH what it
// counted and when the next one may be taken. The floor is written even when the count has not
// moved — it is the only thing that stops the re-enqueue this very write causes from sampling
// again immediately — and a refused write rolls both halves back.
func TestWriteCleanDrainPollsRecordsTheSampleAndItsFloor(t *testing.T) {
	p := drainingPlatform(0)
	r, _, _ := drainReconciler(t, p, &fakeBrokerAdmin{}, interceptor.Funcs{})

	require.NoError(t, r.writeCleanDrainPolls(context.Background(), p, 0))
	stored := storedPlatform(t, r).Status.Upgrade
	require.NotNil(t, stored.NextDrainPollAt, "an unchanged count still moves the floor on")
	assert.WithinDuration(t, time.Now().Add(migrationDrainPollInterval), stored.NextDrainPollAt.Time, time.Minute)
	assert.False(t, migrationDrainPollDue(storedPlatform(t, r), time.Now()), "the next pass must wait")

	refusing, _, _ := drainReconciler(t, drainingPlatform(0), &fakeBrokerAdmin{},
		failingStatusUpdate(errors.New("status write rejected")))
	q := storedPlatform(t, refusing)
	err := refusing.writeCleanDrainPolls(context.Background(), q, 1)
	require.Error(t, err)
	assert.Zero(t, q.Status.Upgrade.CleanDrainPolls, "a count the cluster refused must roll back in memory too")
	assert.Nil(t, q.Status.Upgrade.NextDrainPollAt, "and so must the floor it would have set")
}

// TestDrainPollsInQuickSuccessionCountAsOne is the barrier's real claim. Every status write the
// drain makes re-enqueues the Platform through the operator's own watch, so the passes that
// follow a sample arrive within milliseconds — not on the requeue cadence. Three of those in a
// row prove nothing about a virtual host that has stopped receiving traffic, so only the FIRST
// counts and the rest wait.
func TestDrainPollsInQuickSuccessionCountAsOne(t *testing.T) {
	p := drainingPlatform(0)
	admin := &fakeBrokerAdmin{queues: sourceQueueListing()}
	r, _, _ := drainReconciler(t, p, admin, interceptor.Funcs{})

	for i := 0; i < 4; i++ {
		_, _, _, err := drainPass(t, r)
		require.NoError(t, err)
	}

	assert.Equal(t, int32(1), storedPolls(t, r),
		"four passes inside one interval are one sample, not four")
	assert.Equal(t, otilmv1alpha1.MigrationPhaseDraining, storedPhase(t, r),
		"the migration may not cut over on samples that were never spread across time")
	assert.Equal(t, 1, queueListings(admin), "and the broker is polled once, not once per pass")

	// Time passing is the only thing that lets the count grow.
	advanceDrainPollClock(t, r)
	_, _, _, err := drainPass(t, r)
	require.NoError(t, err)
	assert.Equal(t, int32(2), storedPolls(t, r))
}

// queueListings returns how many times the drain asked this broker for the queue listing.
func queueListings(admin *fakeBrokerAdmin) int {
	listings := 0
	for _, c := range admin.callLog() {
		if c.method == "Queues" {
			listings++
		}
	}
	return listings
}

// TestMigrationDrainMessageReportsProgressWithoutCoordinates: the condition an operator reads
// while waiting says how far the drain has got, and nothing about the broker.
func TestMigrationDrainMessageReportsProgressWithoutCoordinates(t *testing.T) {
	p := drainingPlatform(2)
	message := migrationDrainMessage(p.Status.Upgrade)
	assert.Contains(t, message, "2 of 3")
	assert.Contains(t, message, string(otilmv1alpha1.MigrationPhaseDraining))
	assertNoBrokerCoordinates(t, message)
}
