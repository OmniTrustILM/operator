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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
)

// coreConfigChecksum returns Core's checksum/config pod-template annotation (or "").
func coreConfigChecksum(g Gomega, ns string) string {
	var dep appsv1.Deployment
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
	return dep.Spec.Template.Annotations[platformbuilder.ConfigChecksumAnnotation]
}

var _ = Describe("Trusted-certificate SSA composition", func() {

	// ---------------------------------------------------------------
	// source=generated → compose user CA (+ admin CA when issued) into an
	// operator-owned trusted-certificates Secret via SSA, and stamp Core's
	// checksum/config so a bundle change rolls Core.
	// ---------------------------------------------------------------
	Context("Composed (source=generated)", func() {
		const ns = "ilm-trustedcerts"
		const userTrustSecret = "user-trust"

		It("composes the trusted-certificates Secret (owner-ref'd) and rolls Core on a bundle change", func() {
			By("creating the namespace, credential Secrets, and the user trusted-CA Secret")
			fakeCaps.setGroup(certManagerGroup, false) // admin Certificate is gated off; composition still runs
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}
			userCAV1 := []byte("-----BEGIN CERTIFICATE-----\nVVNFUi1DQS1WMQ==\n-----END CERTIFICATE-----\n")
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: userTrustSecret, Namespace: ns},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{caCrtKey: userCAV1},
			})).To(Succeed())

			By("creating a Platform with registerAdmin source=generated and a user trusted-CA ref")
			p := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName, Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost, Port: 5672,
						VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
					Common:        otilmv1alpha1.CommonSpec{TrustedCertificates: otilmv1alpha1.TrustedCertificatesSpec{SecretRef: userTrustSecret}},
					RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			secretKey := types.NamespacedName{Name: trustedCertsSecretName, Namespace: ns}

			By("verifying the operator-composed trusted-certificates Secret is owner-ref'd and holds the user CA")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, secretKey, &s)).To(Succeed())
				g.Expect(controlledBy(s.OwnerReferences, "ilm")).To(BeTrue(),
					"trusted-certificates must be controller-owned by the Platform")
				g.Expect(s.Data[caCrtKey]).To(ContainSubstring("VVNFUi1DQS1WMQ=="),
					"composed bundle must contain the user CA")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("capturing Core's initial checksum/config annotation")
			var checksumV1 string
			Eventually(func(g Gomega) {
				checksumV1 = coreConfigChecksum(g, ns)
				g.Expect(checksumV1).NotTo(BeEmpty(), "Core must carry a checksum/config annotation when composing")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("marking Core Ready so its config-checksum unfreezes (the auto-roll is a steady-state behavior; during the first migration the checksum is frozen so a config change cannot roll Core mid-migration)")
			markRequiredDeploymentsReady(ns)

			By("verifying Core's TRUSTED_CERTIFICATES env references the composed Secret")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
				ctr := coreContainer(g, dep)
				var found bool
				for _, e := range ctr.Env {
					if e.Name == "TRUSTED_CERTIFICATES" {
						found = true
						g.Expect(e.ValueFrom).NotTo(BeNil())
						g.Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
						g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(trustedCertsSecretName))
					}
				}
				g.Expect(found).To(BeTrue(), "Core must wire TRUSTED_CERTIFICATES from the composed Secret")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("changing the user trusted-CA bundle")
			var userTrust corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userTrustSecret, Namespace: ns}, &userTrust)).To(Succeed())
			userTrust.Data[caCrtKey] = []byte("-----BEGIN CERTIFICATE-----\nVVNFUi1DQS1WMg==\n-----END CERTIFICATE-----\n")
			Expect(k8sClient.Update(ctx, &userTrust)).To(Succeed())

			By("forcing a reconcile (the user Secret is not watched) by bumping a spec field")
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				cur.Spec.Common.Logging.Level = "DEBUG" // any spec change bumps generation -> reconcile
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the composed Secret picks up the new bundle")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, secretKey, &s)).To(Succeed())
				g.Expect(s.Data[caCrtKey]).To(ContainSubstring("VVNFUi1DQS1WMg=="),
					"composed bundle must reflect the updated user CA")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Core's checksum/config annotation changed (rolling Core)")
			Eventually(func(g Gomega) {
				g.Expect(coreConfigChecksum(g, ns)).NotTo(Equal(checksumV1),
					"a trusted-certs bundle change must change Core's checksum/config annotation")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("security guard: no cert bundle material leaks into status conditions")
			var got otilmv1alpha1.Platform
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
			condBytes, err := json.Marshal(got.Status.Conditions)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(condBytes)).NotTo(ContainSubstring("VVNFUi1DQS"))
		})
	})

	// FREEZE regression: while Core is bringing up its first pod (NOT Ready — running its initial
	// DB migration), its config-checksum must NOT change even when a config input changes, or the
	// resulting roll would kill the migrating pod and corrupt the schema. Once Core is Ready the
	// checksum unfreezes and a config change rolls it.
	Context("Freeze during first migration (source=generated)", func() {
		const ns = "ilm-trustedcerts-freeze"
		const userTrustSecret = "user-trust"

		It("does NOT roll Core's checksum while Core is not Ready, then rolls once Ready", func() {
			fakeCaps.setGroup(certManagerGroup, false)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: userTrustSecret, Namespace: ns},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{caCrtKey: []byte("-----BEGIN CERTIFICATE-----\nVjE=\n-----END CERTIFICATE-----\n")},
			})).To(Succeed())

			p := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database:      otilmv1alpha1.DatabaseSpec{Mode: "external", Host: dbHost, Port: 5432, Name: dbName, Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef}},
					Messaging:     otilmv1alpha1.MessagingSpec{Mode: "external", BrokerType: "rabbitmq", Host: brokerHost, Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef}},
					Common:        otilmv1alpha1.CommonSpec{TrustedCertificates: otilmv1alpha1.TrustedCertificatesSpec{SecretRef: userTrustSecret}},
					RegisterAdmin: &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("Core is created with an initial checksum while NOT Ready")
			var checksumV1 string
			Eventually(func(g Gomega) {
				checksumV1 = coreConfigChecksum(g, ns)
				g.Expect(checksumV1).NotTo(BeEmpty())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("changing the trusted-CA bundle + forcing a reconcile while Core stays NOT Ready")
			var userTrust corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userTrustSecret, Namespace: ns}, &userTrust)).To(Succeed())
			userTrust.Data[caCrtKey] = []byte("-----BEGIN CERTIFICATE-----\nVjI=\n-----END CERTIFICATE-----\n")
			Expect(k8sClient.Update(ctx, &userTrust)).To(Succeed())
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				cur.Spec.Common.Logging.Level = "DEBUG" // bump generation -> reconcile
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("FREEZE: Core's checksum stays frozen (no mid-migration roll) while Core is not Ready")
			Consistently(func(g Gomega) {
				g.Expect(coreConfigChecksum(g, ns)).To(Equal(checksumV1),
					"a not-yet-Ready Core must NOT roll when its config changes")
			}, "4s", "500ms").Should(Succeed())

			By("marking Core Ready -> the checksum unfreezes and a config change rolls Core")
			markRequiredDeploymentsReady(ns)
			Eventually(func(g Gomega) {
				var cur otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &cur)).To(Succeed())
				cur.Spec.Common.Logging.Level = "INFO" // bump generation -> reconcile
				g.Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(coreConfigChecksum(g, ns)).NotTo(Equal(checksumV1),
					"once Ready, a config change must roll Core (unfrozen)")
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// No composition (no admin CA + no trustedCertificates) → no Secret rendered,
	// Core has no checksum/config annotation.
	// ---------------------------------------------------------------
	Context("Absent (no composition)", func() {
		const ns = "ilm-trustedcerts-none"

		It("renders no trusted-certificates Secret and no checksum/config annotation", func() {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}

			// No trustedCertificates, no registerAdmin → ComposesTrustedCerts is false.
			p := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName, Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost, Port: 5672,
						VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("waiting for Core to be applied")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying no operator-managed trusted-certificates Secret is rendered")
			Consistently(func() bool {
				var s corev1.Secret
				err := k8sClient.Get(ctx, types.NamespacedName{Name: trustedCertsSecretName, Namespace: ns}, &s)
				return apierrors.IsNotFound(err)
			}, 2*time.Second, 250*time.Millisecond).Should(BeTrue(),
				"no trusted-certificates Secret should exist when nothing is composed")

			By("verifying Core has no checksum/config annotation")
			Eventually(func(g Gomega) {
				g.Expect(coreConfigChecksum(g, ns)).To(BeEmpty())
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})
})
