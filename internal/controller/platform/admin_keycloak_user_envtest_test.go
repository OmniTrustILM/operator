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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// These specs exercise the password-admin reconcile action (reconcileAdminKeycloakUser) through
// the LIVE reconciler. envtest serves no Keycloak Operator, so a managed Keycloak CR never goes
// Ready — the action therefore defers (WaitingForKeycloak) and never calls the registrar, which
// is exactly the adjunct, non-fatal behaviour to prove here. The success/idempotency/no-leak of
// the EnsureRealmUser call itself are covered deterministically by the direct-call unit tests in
// admin_keycloak_user_test.go (and the registrar's own tests).
var _ = Describe("Password admin reconcile action", func() {

	// ---------------------------------------------------------------
	// External Keycloak (password method invalid → no-op): the action sets no
	// AdminUserReady condition and never calls EnsureRealmUser. The CR is built
	// password-disabled so it satisfies the CEL (which forbids password+external).
	// ---------------------------------------------------------------
	Context("ExternalKeycloak", func() {
		It("never calls EnsureRealmUser and sets no AdminUserReady condition", func() {
			const ns = "ilm-adminuser-external"
			fakeOIDC.setUserOutcome(nil)
			before := fakeOIDC.userCallCount()

			p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			markRequiredDeploymentsReady(ns)

			Eventually(func() otilmv1alpha1.PlatformPhase {
				var got otilmv1alpha1.Platform
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)
				return got.Status.Phase
			}, platformTimeout, platformInterval).Should(Equal(otilmv1alpha1.PlatformPhaseRunning))

			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionAdminUserReady)).To(BeNil())

			Consistently(func() int { return fakeOIDC.userCallCount() }, 2*time.Second, 250*time.Millisecond).
				Should(Equal(before))
		})
	})

	// ---------------------------------------------------------------
	// Managed Keycloak (Operator absent in envtest) + password method enabled →
	// AdminUserReady=False/WaitingForKeycloak, the platform is NOT Degraded, the
	// registrar is never called, and the password never leaks into status.
	// ---------------------------------------------------------------
	Context("ManagedKeycloakNotReady", func() {
		It("defers to WaitingForKeycloak, stays non-Degraded, and never leaks the password", func() {
			const ns = "ilm-adminuser-kc-notready"
			const adminPWSecret = "admin-pw"
			const theLivePassword = "live-secret-admin-pw-4c1d"
			fakeOIDC.setUserOutcome(nil)
			before := fakeOIDC.userCallCount()
			fakeCaps.setGroup(keycloakGroup, false) // Keycloak Operator absent

			p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{
				Mode: "managed", Realm: "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.0"},
			})
			// Password-only admin, with the password Secret present (so the gate that fails is
			// KeycloakReady, not the missing password — proving the ordering).
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: adminPWSecret, Namespace: ns},
				Data:       map[string][]byte{"password": []byte(theLivePassword)},
			})).To(Succeed())
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Username:    "root-operator",
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(false)},
				Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: true, SecretRef: adminPWSecret},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			markRequiredDeploymentsReady(ns)

			By("verifying AdminUserReady=False/WaitingForKeycloak")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionAdminUserReady)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(reasonAdminUserWaitingForKeycloak))
			}, platformTimeout, platformInterval).Should(Succeed())

			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())

			By("verifying the platform is NOT Degraded (the password admin is an adjunct)")
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Degraded")).To(BeFalse())

			By("verifying the registrar was never called (Keycloak not ready)")
			Consistently(func() int { return fakeOIDC.userCallCount() }, 2*time.Second, 250*time.Millisecond).
				Should(Equal(before))

			By("verifying the admin password never appears in the Platform status")
			b, err := json.Marshal(got.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).NotTo(ContainSubstring(theLivePassword))
		})
	})
})
