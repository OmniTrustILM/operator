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

// The unit tests prove what the drain DECIDES. This one proves it is actually wired: driven
// through the whole Reconcile, with a broker that answers, the phase reads the administrator
// credentials the platform's own topology names, polls the SOURCE virtual host, and moves the
// migration on — none of which a fake client can establish, because the wiring is exactly the
// part that resolves builder output against live objects.
//
// Both this spec's hand-driven passes and the suite manager's background ones go through the
// same scripted broker (keyed by the administrator username), so whichever actor runs a pass,
// it sees the same answer.

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/rabbitmq"
)

// fakeBrokerAdmins is the suite's broker-client factory: it routes the drain to a scripted
// broker registered for the administrator USERNAME the drain authenticates with — the one input
// a spec fully controls, since it writes the credentials Secret the drain reads. Every other
// namespace gets an unreachable broker, which is what envtest genuinely offers, so the migration
// specs that have no business draining simply fail closed and hold their phase.
var fakeBrokerAdmins = &brokerAdminRegistry{admins: map[string]rabbitmq.BrokerAdmin{}}

// brokerAdminRegistry maps an administrator username to the broker that answers for it.
type brokerAdminRegistry struct {
	mu     sync.Mutex
	admins map[string]rabbitmq.BrokerAdmin
}

// register makes admin the broker every drain authenticating as username will see.
func (b *brokerAdminRegistry) register(username string, admin rabbitmq.BrokerAdmin) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.admins[username] = admin
}

// factory is the reconciler's brokerAdminFactory: the registered broker, or an unreachable one.
func (b *brokerAdminRegistry) factory(_, username, _ string) rabbitmq.BrokerAdmin {
	b.mu.Lock()
	defer b.mu.Unlock()
	if admin, registered := b.admins[username]; registered {
		return admin
	}
	return unreachableBroker{}
}

// unreachableBroker is a broker nothing can be learned from — every call fails, so a drain
// pointed at it can only ever fail closed.
type unreachableBroker struct{}

func (unreachableBroker) Queues(context.Context, string) ([]rabbitmq.QueueState, error) {
	return nil, errors.New("no broker is reachable")
}

func (unreachableBroker) BoundQueues(context.Context, string, string) ([]string, error) {
	return nil, errors.New("no broker is reachable")
}

func (unreachableBroker) Connections(context.Context, string) (int, error) {
	return 0, errors.New("no broker is reachable")
}

func (unreachableBroker) CloseConnections(context.Context, string) error {
	return errors.New("no broker is reachable")
}

// advanceDrainPollClock back-dates the recorded next-poll floor so the following reconcile is
// allowed to take a sample. The drain deliberately refuses to sample twice inside one interval
// — that spacing is what makes three "consecutive" clean polls mean anything — so a spec that
// drives the reconciler in a tight loop has to stand in for the time that would have passed.
func advanceDrainPollClockFor(ns string) {
	var p otilmv1alpha1.Platform
	if err := k8sClient.Get(ctx, platformKey(ns), &p); err != nil || p.Status.Upgrade == nil {
		return
	}
	past := metav1.NewTime(time.Now().Add(-time.Second))
	p.Status.Upgrade.NextDrainPollAt = &past
	_ = k8sClient.Status().Update(ctx, &p)
}

// queuesPolled returns how many times the drain has asked this broker for the queue listing.
func queuesPolled(admin *fakeBrokerAdmin) int {
	polled := 0
	for _, c := range admin.callLog() {
		if c.method == "Queues" {
			polled++
		}
	}
	return polled
}

// managedMessagingPlatform switches a fixture Platform onto an operator-managed broker with no
// pinned virtual host, so the source virtual host is the source bundle's own default — the
// rename a messaging migration exists for.
func managedMessagingPlatform(p *otilmv1alpha1.Platform) {
	p.Spec.Messaging.Mode = "managed"
	p.Spec.Messaging.Host = ""
	p.Spec.Messaging.VirtualHost = ""
	p.Spec.Messaging.Credentials = nil
	p.Spec.Messaging.Managed = &otilmv1alpha1.ManagedMessagingSpec{
		Replicas: 1,
		Storage:  otilmv1alpha1.StorageSpec{Size: "1Gi"},
	}
}

// createAdministratorSecret writes the credentials Secret the Messaging Topology Operator would
// generate for the topology's administrator user — the one the drain authenticates with.
func createAdministratorSecret(ns, username string) {
	ExpectWithOffset(1, k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecretName, Namespace: ns},
		Data:       map[string][]byte{"username": []byte(username), "password": []byte(adminPassword)},
	})).To(Succeed())
}

var _ = Describe("Messaging migration drain", func() {
	Context("A drain polling the source virtual host", func() {
		It("holds while the source still holds messages, and advances once it is repeatedly empty", func() {
			const ns = "ilm-migration-drain"
			const username = "drain-spec-administrator"

			admin := &fakeBrokerAdmin{
				queues: sourceQueueListing(rabbitmq.QueueState{Name: "core.events", MessagesReady: 5}),
				bound:  map[string][]string{sourceProxyExchange: {"instance-7a3f"}},
			}
			fakeBrokerAdmins.register(username, admin)

			beginMigrationFixture(ns, otilmv1alpha1.MigrationPhaseDraining, managedMessagingPlatform)
			createAdministratorSecret(ns, username)

			By("holding the migration while a work queue is still not empty")
			Eventually(func() int {
				return queuesPolled(admin)
			}, platformTimeout, platformInterval).ShouldNot(BeZero(), "the drain reads the credentials Secret and polls")
			Consistently(func(g Gomega) {
				u := getPlatform(ns).Status.Upgrade
				g.Expect(u).NotTo(BeNil())
				g.Expect(u.Phase).To(Equal(otilmv1alpha1.MigrationPhaseDraining))
				g.Expect(u.CleanDrainPolls).To(BeZero(), "a dirty poll can never be counted as progress")
			}, "2s", platformInterval).Should(Succeed())

			By("polling the SOURCE virtual host and the SOURCE proxy exchange")
			for _, c := range admin.callLog() {
				Expect(c.vhost).To(Equal(sourceVirtualHost),
					"the drain must address the virtual host the platform is moving away from")
				if c.method == "BoundQueues" {
					Expect(c.exchange).To(Equal(sourceProxyExchange))
				}
			}

			Expect(getPlatform(ns).Status.ObservedVersion).To(Equal(platformVersion218),
				"a draining platform reports the version it is still running")

			By("emptying the queue and letting the drain sample it")
			admin.setQueues(sourceQueueListing(rabbitmq.QueueState{Name: "instance-7a3f"}))
			polledBefore := queuesPolled(admin)

			Eventually(func() otilmv1alpha1.MigrationPhase {
				// Each pass stands for one whole migration cadence: the recorded floor is what
				// keeps two samples from landing in the same instant, so a spec that drove the
				// reconciler faster than real time has to move it on deliberately.
				advanceDrainPollClockFor(ns)
				reconcileOnce(ns)
				u := getPlatform(ns).Status.Upgrade
				if u == nil {
					return ""
				}
				return u.Phase
			}, platformTimeout, platformInterval).Should(Equal(otilmv1alpha1.MigrationPhaseCuttingOver),
				"a repeatedly empty source virtual host — the bound proxy queue included — hands over to the cutover")

			// Each pass reads the count back from the cluster, so reaching the threshold at all
			// proves it was persisted: a reconciler carries nothing from the pass before it.
			Expect(queuesPolled(admin)-polledBefore).To(BeNumerically(">=", migrationCleanDrainPolls),
				"the hand-over takes as many clean polls as the contract asks for")
			Expect(getPlatform(ns).Status.Upgrade.CleanDrainPolls).To(BeZero(),
				"the counter belongs to the phase that used it")

			By("reporting the migration it moved on without leaking a coordinate")
			cond := migrationConditionOf(ns)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(string(otilmv1alpha1.MigrationPhaseCuttingOver)))
			Expect(cond.Message).NotTo(ContainSubstring(sourceVirtualHost))
			Expect(cond.Message).NotTo(ContainSubstring(adminPassword))
		})
	})
})
