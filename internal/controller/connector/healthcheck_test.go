/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/monitoring"
	"github.com/OmniTrustILM/operator/internal/registration"
)

const (
	v2HealthPath   = "/v2/health"
	v1HealthPath   = "/v1/health"
	v2Up           = `{"status":"UP","components":{"liveness":{"status":"UP"},"readiness":{"status":"UP"}}}`
	v2KeystoreDown = `{"status":"DOWN","components":{"liveness":{"status":"UP"},"keystore":{"status":"DOWN",` +
		`"details":{"error":"Timeout connecting to hsm.internal:1792"}}}}`
)

// setReplicasReady reports every desired replica of the connector's Deployment ready.
func setReplicasReady(key types.NamespacedName) {
	EventuallyWithOffset(1, func(g Gomega) {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		g.Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

// poke changes an annotation to trigger a reconcile now.
func poke(key types.NamespacedName, value string) {
	EventuallyWithOffset(1, func(g Gomega) {
		var latest otilmv1alpha1.Connector
		g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[triggerAnnotation] = value
		g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

// createRunning creates the connector and brings it to phase Running.
func createRunning(conn *otilmv1alpha1.Connector) types.NamespacedName {
	key := types.NamespacedName{Name: conn.Name, Namespace: conn.Namespace}
	ExpectWithOffset(1, k8sClient.Create(ctx, conn)).To(Succeed())
	EventuallyWithOffset(1, func(g Gomega) {
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
	}, timeout, interval).Should(Succeed())
	setReplicasReady(key)
	poke(key, "running")
	EventuallyWithOffset(1, func(g Gomega) {
		var c otilmv1alpha1.Connector
		g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
		g.Expect(c.Status.Phase).To(Equal(otilmv1alpha1.ConnectorPhaseRunning))
	}, timeout, interval).Should(Succeed())
	return key
}

// expectHealthy waits for the connector's Healthy condition to reach status and reason, then
// returns it.
func expectHealthy(key types.NamespacedName, status metav1.ConditionStatus, reason string) metav1.Condition {
	var found metav1.Condition
	EventuallyWithOffset(1, func(g Gomega) {
		var c otilmv1alpha1.Connector
		g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
		cond := meta.FindStatusCondition(c.Status.Conditions, condHealthy)
		g.Expect(cond).NotTo(BeNil(), "Healthy condition is missing")
		g.Expect(cond.Status).To(Equal(status))
		g.Expect(cond.Reason).To(Equal(reason))
		found = *cond
	}, timeout, interval).Should(Succeed())
	return found
}

var _ = Describe("Connector health check", func() {
	Context("TestHealthyFollowsTheReport", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-follows")
		})

		It("should set Healthy from the report and follow it, leaving phase and Available alone", func() {
			conn := newConnector("health-follows", ns)
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			key := createRunning(conn)

			By("reading an UP report")
			expectHealthy(key, metav1.ConditionTrue, "Up")

			By("following the connector to DOWN")
			connectors.answer(conn, v2HealthPath, http.StatusServiceUnavailable, v2KeystoreDown)
			poke(key, "down")
			down := expectHealthy(key, metav1.ConditionFalse, "Down")
			Expect(down.Message).To(ContainSubstring("keystore"))
			Expect(down.Message).NotTo(ContainSubstring("hsm.internal"), "component details stay out of the condition")
			Expect(down.ObservedGeneration).To(BeNumerically(">", 0))

			var c otilmv1alpha1.Connector
			Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
			Expect(c.Status.Phase).To(Equal(otilmv1alpha1.ConnectorPhaseRunning))
			Expect(meta.IsStatusConditionTrue(c.Status.Conditions, condAvailable)).To(BeTrue())

			By("following the connector back to UP")
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			poke(key, "up")
			expectHealthy(key, metav1.ConditionTrue, "Up")
		})
	})

	Context("TestRegistrationIgnoresHealth", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-registration")
		})

		It("should register a Running connector whose report is DOWN", func() {
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(registration.Response{UUID: "health-down-uuid", Status: "waitingForApproval"})
			}))
			defer platform.Close()

			conn := newConnector("health-registration", ns)
			conn.Spec.Registration = &otilmv1alpha1.RegistrationSpec{
				PlatformURL: platform.URL,
				Name:        testConnectorName,
				AuthType:    otilmv1alpha1.AuthTypeNone,
			}
			connectors.answer(conn, v2HealthPath, http.StatusServiceUnavailable, v2KeystoreDown)
			key := createRunning(conn)

			expectHealthy(key, metav1.ConditionFalse, "Down")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(c.Status.Registration).NotTo(BeNil())
				g.Expect(c.Status.Registration.UUID).To(Equal("health-down-uuid"))
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("TestRejectedRegistrationWaitsForASpecChange", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-rejected-registration")
		})

		It("should post a rejected registration again only after a spec change", func() {
			var posts atomic.Int32
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer platform.Close()

			conn := newConnector("health-rejected-registration", ns)
			conn.Spec.Registration = &otilmv1alpha1.RegistrationSpec{
				PlatformURL: platform.URL,
				Name:        testConnectorName,
				AuthType:    otilmv1alpha1.AuthTypeNone,
			}
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			key := createRunning(conn)

			By("waiting for the rejection to settle")
			var rejected int32
			Eventually(func(g Gomega) {
				before := posts.Load()
				g.Expect(before).To(BeNumerically(">", 0))
				g.Consistently(posts.Load, time.Second, interval).Should(Equal(before))
				rejected = before
			}, timeout, interval).Should(Succeed())

			By("reconciling twice more, each time checking health")
			for _, value := range []string{"rejected-1", "rejected-2"} {
				asked := len(connectors.askedPaths(conn))
				poke(key, value)
				Eventually(func() int { return len(connectors.askedPaths(conn)) }, timeout, interval).
					Should(BeNumerically(">", asked))
			}
			Consistently(posts.Load, time.Second, interval).Should(Equal(rejected),
				"a rejected registration waits for a spec change")
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				degraded := meta.FindStatusCondition(c.Status.Conditions, condDegraded)
				g.Expect(degraded).NotTo(BeNil())
				g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(degraded.Reason).To(Equal(monitoring.ReasonRegistrationFailed))
			}, timeout, interval).Should(Succeed())

			By("changing the registration")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				latest.Spec.Registration.Name = "renamed-connector"
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			Eventually(posts.Load, timeout, interval).Should(BeNumerically(">", rejected))
		})
	})

	Context("TestHealthCheckExplicitPath", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-explicit-path")
		})

		It("should check only the path the CR names", func() {
			conn := newConnector("health-explicit-path", ns)
			conn.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{Path: v1HealthPath}
			connectors.answer(conn, v1HealthPath, http.StatusOK,
				`{"status":"nok","description":null,"parts":{"database":{"status":"nok","description":"Connection refused"}}}`)
			key := createRunning(conn)

			nok := expectHealthy(key, metav1.ConditionFalse, "Down")
			Expect(nok.Message).To(ContainSubstring("database"))
			Expect(connectors.askedPaths(conn)).NotTo(ContainElement(v2HealthPath))
		})
	})

	Context("TestHealthCheckFallsBackToV1", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-fallback")
		})

		It("should read the v1 report when the v2 path has none", func() {
			conn := newConnector("health-fallback", ns)
			connectors.answer(conn, v2HealthPath, http.StatusInternalServerError, `{"message":"Internal server error."}`)
			connectors.answer(conn, v1HealthPath, http.StatusOK, `{"status":"ok","description":null,"parts":null}`)
			key := createRunning(conn)

			expectHealthy(key, metav1.ConditionTrue, "Up")
			Expect(connectors.askedPaths(conn)).To(ContainElements(v2HealthPath, v1HealthPath))
		})
	})

	Context("TestHealthyWhileNotRunning", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-not-running")
		})

		It("should mark a connector without ready replicas NotRunning and leave it unasked", func() {
			conn := newConnector("health-not-running", ns)
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
			key := types.NamespacedName{Name: conn.Name, Namespace: ns}

			expectHealthy(key, metav1.ConditionUnknown, reasonNotRunning)
			Expect(connectors.askedPaths(conn)).To(BeEmpty())
		})
	})

	Context("TestHealthyWhenAReferenceIsMissing", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-missing-ref")
		})

		It("should mark a connector Failed by a missing Secret as NotRunning", func() {
			conn := newConnector("health-missing-ref", ns)
			conn.Spec.SecretRefs = []otilmv1alpha1.SecretRef{{Name: "absent-secret", Type: otilmv1alpha1.RefTypeEnv}}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
			key := types.NamespacedName{Name: conn.Name, Namespace: ns}

			expectHealthy(key, metav1.ConditionUnknown, reasonNotRunning)
		})
	})

	Context("TestHealthCheckSwitchedOff", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-off")
		})

		It("should drop the Healthy condition and stop asking", func() {
			conn := newConnector("health-off", ns)
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			key := createRunning(conn)
			expectHealthy(key, metav1.ConditionTrue, "Up")

			By("switching the check off")
			Eventually(func(g Gomega) {
				var latest otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &latest)).To(Succeed())
				latest.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{Enabled: ptr.To(false)}
				g.Expect(k8sClient.Update(ctx, &latest)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			Eventually(func(g Gomega) {
				var c otilmv1alpha1.Connector
				g.Expect(k8sClient.Get(ctx, key, &c)).To(Succeed())
				g.Expect(meta.FindStatusCondition(c.Status.Conditions, condHealthy)).To(BeNil())
			}, timeout, interval).Should(Succeed())

			By("reconciling again without a request to the connector")
			asked := len(connectors.askedPaths(conn))
			poke(key, "off")
			Consistently(func() int { return len(connectors.askedPaths(conn)) }, 2*time.Second, interval).Should(Equal(asked))
		})
	})

	Context("TestSteadyHealthWaitsForThePeriod", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-steady")
		})

		It("should ask a connector with an unchanged report again only after its period", func() {
			conn := newConnector("health-steady", ns)
			connectors.answer(conn, v2HealthPath, http.StatusServiceUnavailable, v2KeystoreDown)
			key := createRunning(conn)
			expectHealthy(key, metav1.ConditionFalse, "Down")

			By("waiting for the checks to settle")
			var settled int
			Eventually(func(g Gomega) {
				before := len(connectors.askedPaths(conn))
				g.Consistently(func() int { return len(connectors.askedPaths(conn)) }, time.Second, interval).
					Should(Equal(before))
				settled = before
			}, timeout, interval).Should(Succeed())

			Consistently(func() int { return len(connectors.askedPaths(conn)) }, 3*time.Second, interval).
				Should(Equal(settled), "an unchanged report leaves the status as it is, so the next check waits for the period")
		})
	})

	Context("TestHangingCheckLeavesOtherConnectorsReconciling", func() {
		var ns string

		BeforeEach(func() {
			ns = createTestNamespace("test-health-hanging")
		})

		It("should reconcile another connector while one check waits out its timeout", func() {
			hanging := newConnector("health-hanging", ns)
			hanging.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 60, TimeoutSeconds: 30}
			connectors.hang(hanging, v2HealthPath)
			Expect(k8sClient.Create(ctx, hanging)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hanging)).To(Succeed()) })
			key := types.NamespacedName{Name: hanging.Name, Namespace: ns}
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			setReplicasReady(key)
			Eventually(func() []string { return connectors.askedPaths(hanging) }, timeout, interval).
				Should(ContainElement(v2HealthPath))

			By("creating another connector while the check hangs")
			other := newConnector("health-other", ns)
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: other.Name, Namespace: ns}, &dep)).To(Succeed())
			}, 5*time.Second, interval).Should(Succeed(), "the other connector waits behind the hanging check")
		})
	})

	Context("TestHealthCheckRequeue", func() {
		reconcile := func(key types.NamespacedName) ctrl.Result {
			r := &Reconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Recorder:     record.NewFakeRecorder(100),
				HealthClient: connectors.client(),
			}
			var result ctrl.Result
			EventuallyWithOffset(1, func(g Gomega) {
				var err error
				result, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				g.Expect(err).NotTo(HaveOccurred())
			}, timeout, interval).Should(Succeed())
			return result
		}

		It("should check a Running connector again after its period", func() {
			conn := newConnector("health-requeue", createTestNamespace("test-health-requeue"))
			conn.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{PeriodSeconds: 15, TimeoutSeconds: 5}
			connectors.answer(conn, v2HealthPath, http.StatusOK, v2Up)
			key := createRunning(conn)

			Expect(reconcile(key).RequeueAfter).To(Equal(15 * time.Second))
		})

		It("should leave a switched-off check unscheduled", func() {
			conn := newConnector("health-requeue-off", createTestNamespace("test-health-requeue-off"))
			conn.Spec.HealthCheck = &otilmv1alpha1.HealthCheckSpec{Enabled: ptr.To(false)}
			key := createRunning(conn)

			Expect(reconcile(key).RequeueAfter).To(BeZero())
		})
	})
})
