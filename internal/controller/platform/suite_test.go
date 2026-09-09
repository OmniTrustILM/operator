/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/OmniTrustILM/operator/internal/controller/internal/testenv"
	"github.com/OmniTrustILM/operator/internal/registration"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	mgr       ctrl.Manager
	// fakeCaps is the capability detector the suite injects into the reconciler so
	// edge-gating specs control which upstream CRDs (cert-manager / Gateway API)
	// appear "served" — envtest itself loads only the operator's own CRDs, so the
	// real RESTMapper would report all of them absent.
	fakeCaps *fakeDetector
	// fakeOIDC is the OIDC registrar the suite injects so OIDC-wiring specs drive the
	// outcome (success / transient error) and assert the captured config WITHOUT a live
	// Keycloak or Core. It records every call for assertions.
	fakeOIDC *fakeOIDCRegistrar
)

// oidcCall captures one FetchClientSecret invocation for assertions.
type oidcCall struct {
	keycloakBaseURL string
	realm           string
	adminUsername   string
	adminPassword   string
	clientID        string
}

// realmUserCall captures one EnsureRealmUser invocation for assertions.
type realmUserCall struct {
	keycloakBaseURL string
	realm           string
	adminUsername   string
	adminPassword   string
	user            registration.RealmUser
}

// fakeOIDCRegistrar is a goroutine-safe registration.OIDCRegistrar stand-in: specs configure
// the outcome under a lock before creating a Platform and
// read the recorded calls after. It returns a canned client secret on success (the value the
// reconciler relays into the operator-owned OIDC client Secret), and records EnsureRealmUser
// calls for the password-admin specs.
type fakeOIDCRegistrar struct {
	mu        sync.Mutex
	err       error
	secret    string
	calls     []oidcCall
	userErr   error
	userCalls []realmUserCall
}

func newFakeOIDCRegistrar() *fakeOIDCRegistrar {
	return &fakeOIDCRegistrar{secret: "fake-kc-client-secret"}
}

// FetchClientSecret records the call and returns the configured secret/outcome.
func (f *fakeOIDCRegistrar) FetchClientSecret(_ context.Context, keycloakBaseURL, realm, adminUsername, adminPassword, clientID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, oidcCall{keycloakBaseURL, realm, adminUsername, adminPassword, clientID})
	if f.err != nil {
		return "", f.err
	}
	return f.secret, nil
}

// EnsureRealmUser records the call and returns the configured user-outcome.
func (f *fakeOIDCRegistrar) EnsureRealmUser(_ context.Context, keycloakBaseURL, realm, adminUsername, adminPassword string, user registration.RealmUser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls = append(f.userCalls, realmUserCall{keycloakBaseURL, realm, adminUsername, adminPassword, user})
	return f.userErr
}

// setOutcome configures the next (and subsequent) FetchClientSecret return value.
func (f *fakeOIDCRegistrar) setOutcome(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// setUserOutcome configures the next (and subsequent) EnsureRealmUser return value.
func (f *fakeOIDCRegistrar) setUserOutcome(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userErr = err
}

// callCount returns how many times FetchClientSecret has been invoked.
func (f *fakeOIDCRegistrar) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// userCallCount returns how many times EnsureRealmUser has been invoked.
func (f *fakeOIDCRegistrar) userCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.userCalls)
}

// fakeDetector is a goroutine-safe capabilities.Detector stand-in keyed by API
// group. The manager runs the reconciler in a background goroutine, so specs mutate
// availability under a lock before creating a Platform. Unknown groups default to
// absent, matching envtest reality.
type fakeDetector struct {
	mu        sync.Mutex
	available map[string]bool
}

func newFakeDetector() *fakeDetector { return &fakeDetector{available: map[string]bool{}} }

// setGroup marks an API group available or absent for subsequent detections. The
// available flag is a parameter so the helper reads as a general availability setter,
// even though current specs only mark groups absent (envtest serves none of them).
//
//nolint:unparam // available is intentionally a parameter for a reusable availability setter
func (f *fakeDetector) setGroup(group string, available bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available[group] = available
}

// Available reports the configured availability for the GroupKind's group.
func (f *fakeDetector) Available(gk schema.GroupKind, _ ...string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available[gk.Group], nil
}

func TestPlatformController(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Platform Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	cfg, testEnv, err = testenv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Start a controller manager with the Platform Reconciler so watches and
	// reconciliation loops run automatically during tests.
	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics listener in tests
		},
	})
	Expect(err).NotTo(HaveOccurred())

	fakeCaps = newFakeDetector()
	fakeOIDC = newFakeOIDCRegistrar()
	err = (&Reconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Capabilities:  fakeCaps,                                // injected so SetupWithManager won't overwrite it with the real mapper
		OIDCRegistrar: fakeOIDC,                                // injected so OIDC-wiring specs drive the outcome
		BrokerAdmins:  fakeBrokerAdmins.factory,                // injected so no drain ever opens a socket from a test
		Recorder:      mgr.GetEventRecorderFor("ilm-operator"), //nolint:staticcheck // the controller-runtime record.EventRecorder API is intentionally retained (the newer events.EventRecorder is not adopted)
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})
