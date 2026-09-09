/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// oidcPlatform builds a Platform with credential Secrets and the given keycloak spec, in its
// own namespace, for the live-reconcile OIDC specs.
func oidcPlatform(ns string, kc *otilmv1alpha1.KeycloakSpec) *otilmv1alpha1.Platform {
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	for _, n := range []string{dbSecretRef, messagingSecretRef} {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
		})).To(Succeed())
	}
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: dbHost, Port: 5432, Name: dbName, Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
			},
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", BrokerType: "rabbitmq", Host: brokerHost, Port: 5672,
				VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
			},
			Keycloak: kc,
		},
	}
}

var _ = Describe("OIDC provider reconcile action", func() {

	// ---------------------------------------------------------------
	// External/unmanaged Keycloak → no call, no OIDCConfigured condition.
	// ---------------------------------------------------------------
	Context("ExternalKeycloak", func() {
		It("never calls the registrar and sets no OIDCConfigured condition", func() {
			const ns = "ilm-oidc-external"
			fakeOIDC.setOutcome(nil)
			before := fakeOIDC.callCount()

			p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("marking the required Deployments ready so the platform reaches Running")
			markRequiredDeploymentsReady(ns)

			Eventually(func() otilmv1alpha1.PlatformPhase {
				var got otilmv1alpha1.Platform
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)
				return got.Status.Phase
			}, platformTimeout, platformInterval).Should(Equal(otilmv1alpha1.PlatformPhaseRunning))

			By("verifying no OIDCConfigured condition is set")
			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
			Expect(meta.FindStatusCondition(got.Status.Conditions, "OIDCConfigured")).To(BeNil())

			By("verifying the registrar was never called")
			Consistently(func() int { return fakeOIDC.callCount() }, 2*time.Second, 250*time.Millisecond).
				Should(Equal(before))
		})
	})

	// ---------------------------------------------------------------
	// Managed Keycloak but the Keycloak Operator is absent (envtest serves
	// none) → KeycloakReady is False, so OIDC defers WaitingForKeycloak with
	// no call, and the platform is NOT Degraded (adjunct).
	// ---------------------------------------------------------------
	Context("ManagedKeycloakNotReady", func() {
		It("defers to WaitingForKeycloak and does NOT call the registrar", func() {
			const ns = "ilm-oidc-kc-notready"
			fakeOIDC.setOutcome(nil)
			before := fakeOIDC.callCount()
			// The Keycloak Operator group is absent (the suite's fakeCaps reports nothing served).
			fakeCaps.setGroup(keycloakGroup, false)

			p := oidcPlatform(ns, &otilmv1alpha1.KeycloakSpec{
				Mode: "managed", Realm: "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.0"},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			markRequiredDeploymentsReady(ns)

			By("verifying OIDCConfigured=False/WaitingForKeycloak")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				cond := meta.FindStatusCondition(got.Status.Conditions, "OIDCConfigured")
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("WaitingForKeycloak"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the platform is NOT Degraded (OIDC wiring is an adjunct)")
			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Degraded")).To(BeFalse())

			By("verifying the registrar was never called (Keycloak not ready)")
			Consistently(func() int { return fakeOIDC.callCount() }, 2*time.Second, 250*time.Millisecond).
				Should(Equal(before))
		})
	})
})
