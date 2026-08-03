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
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

const (
	platformTimeout  = 30 * time.Second
	platformInterval = 250 * time.Millisecond

	// dbHost is the external Postgres hostname used in all assertions.
	dbHost = "postgres.example.com"
	// dbName is the database name used in all assertions.
	dbName = "ilmdb"
	// brokerHost is the external RabbitMQ hostname used in all assertions.
	brokerHost = "rabbitmq.example.com"
	// dbSecretRef is the name of the Secret holding DB credentials.
	dbSecretRef = "ilm-db"
	// messagingSecretRef is the name of the Secret holding broker credentials.
	messagingSecretRef = "ilm-messaging"
)

var _ = Describe("Platform Controller", func() {

	// ---------------------------------------------------------------
	// Test 1: Core reconcile — SA / Deployment / Service + status
	// Each spec runs in its own dedicated namespace to avoid singleton collisions.
	// ---------------------------------------------------------------
	Context("TestCoreReconcile", func() {
		const platformName = "ilm"
		const ns = "ilm-happy"

		It("should create owned SA/Deployment/Service, wire DB credentials as secretKeyRef, and set Running status", func() {
			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			By("creating credential Secrets so the preflight check passes")
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}

			By("creating a Platform CR with external database and messaging")
			platform := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{
					Name:      platformName,
					Namespace: ns,
				},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode:        "external",
						Host:        dbHost,
						Port:        5432,
						Name:        dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode:        "external",
						BrokerType:  "rabbitmq",
						Host:        brokerHost,
						Port:        5672,
						VirtualHost: "ilm",
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, platform)).To(Succeed())

			// Platform is a per-namespace singleton: the component name is unscoped.
			// Resources are named "core" (not "ilm-core").
			key := types.NamespacedName{Name: "core", Namespace: ns}

			By("verifying ServiceAccount named 'core' is created")
			Eventually(func(g Gomega) {
				var sa corev1.ServiceAccount
				g.Expect(k8sClient.Get(ctx, key, &sa)).To(Succeed())
				g.Expect(sa.OwnerReferences).NotTo(BeEmpty())
				g.Expect(sa.OwnerReferences[0].Name).To(Equal(platformName))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Deployment named 'core' is created")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				g.Expect(dep.OwnerReferences).NotTo(BeEmpty())
				g.Expect(dep.OwnerReferences[0].Name).To(Equal(platformName))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Service named 'core' is created")
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
				g.Expect(svc.OwnerReferences).NotTo(BeEmpty())
				g.Expect(svc.OwnerReferences[0].Name).To(Equal(platformName))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Deployment has a controller owner reference to the Platform")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())

				var controlled bool
				for _, ref := range dep.OwnerReferences {
					if ref.Controller != nil && *ref.Controller && ref.Name == platformName {
						controlled = true
						break
					}
				}
				g.Expect(controlled).To(BeTrue(), "Deployment should have a controller owner reference to the Platform")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying JDBC_PASSWORD env is sourced from secretKeyRef (never inlined)")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				// Core renders the main "core" container plus the auth-opa sidecar;
				// assert against the main container located by name.
				container := coreContainer(g, dep)

				var found bool
				for _, e := range container.Env {
					if e.Name == "JDBC_PASSWORD" {
						g.Expect(e.Value).To(BeEmpty(), "JDBC_PASSWORD must not have an inline Value; it must use secretKeyRef")
						g.Expect(e.ValueFrom).NotTo(BeNil(), "JDBC_PASSWORD must have a ValueFrom")
						g.Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil(), "JDBC_PASSWORD must use SecretKeyRef")
						g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(dbSecretRef))
						g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal("password"))
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "container env must contain JDBC_PASSWORD")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying JDBC_URL is an inline env var containing the rendered connection string")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
				container := coreContainer(g, dep)

				var found bool
				for _, e := range container.Env {
					if e.Name == "JDBC_URL" {
						g.Expect(e.Value).To(ContainSubstring("jdbc:"), "JDBC_URL must be a rendered JDBC connection string")
						g.Expect(e.Value).To(ContainSubstring(dbHost))
						g.Expect(e.ValueFrom).To(BeNil(), "JDBC_URL must be an inline value, not a secretKeyRef")
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "container env must contain JDBC_URL")
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingCoreAuthReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform status Phase=Running with Available=True condition")
			Eventually(func(g Gomega) {
				var p otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: platformName, Namespace: ns}, &p)).To(Succeed())
				g.Expect(p.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(p.Status.ObservedGeneration).To(BeNumerically(">", 0))

				var availFound bool
				for _, cond := range p.Status.Conditions {
					if cond.Type == "Available" && cond.Status == metav1.ConditionTrue {
						g.Expect(cond.ObservedGeneration).To(BeNumerically(">", 0))
						availFound = true
						break
					}
				}
				g.Expect(availFound).To(BeTrue(), "status must have Available=True condition")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("security guard: no secret value or connection coordinate leaks into status conditions")
			Eventually(func(g Gomega) {
				var p otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: platformName, Namespace: ns}, &p)).To(Succeed())

				// Marshal conditions to JSON and inspect as plain text.
				condBytes, err := json.Marshal(p.Status.Conditions)
				g.Expect(err).NotTo(HaveOccurred())
				condStr := string(condBytes)

				g.Expect(condStr).NotTo(ContainSubstring(dbHost),
					"database host must not appear in status conditions")
				g.Expect(condStr).NotTo(ContainSubstring("jdbc:"),
					"JDBC URI must not appear in status conditions")
				g.Expect(condStr).NotTo(ContainSubstring(brokerHost),
					"broker host must not appear in status conditions")
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 1a: Full stateless platform — every component deploys
	// A Platform with utils enabled must render-then-apply (via SSA) the
	// whole stateless platform: Core + auth + scheduler +
	// fe-administrator + utils + auth-opa-policies (Deployments + Services
	// + ServiceAccounts), the fe-administrator + messaging ConfigMaps, and the
	// operator-composed auth-db Secret — each owned by the Platform.
	// ---------------------------------------------------------------
	Context("TestFullPlatformReconcile", func() {
		const platformName = "ilm"
		const ns = "ilm-full"

		It("deploys every stateless component (with owner refs) and reaches Running", func() {
			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			By("creating credential Secrets so the preflight check passes")
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}

			By("creating a Platform CR with utils enabled")
			platform := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: platformName, Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
					Utils: otilmv1alpha1.UtilsSpec{Enabled: true},
				},
			}
			Expect(k8sClient.Create(ctx, platform)).To(Succeed())

			// Every component renders a Deployment, Service, and ServiceAccount under
			// its unscoped component name (per-namespace singleton).
			components := []string{
				"core", "auth", "scheduler",
				"fe-administrator", "utils", "auth-opa-policies",
			}

			By("verifying every component's Deployment is applied with a Platform owner ref")
			for _, name := range components {
				key := types.NamespacedName{Name: name, Namespace: ns}
				Eventually(func(g Gomega) {
					var dep appsv1.Deployment
					g.Expect(k8sClient.Get(ctx, key, &dep)).To(Succeed())
					g.Expect(controlledBy(dep.OwnerReferences, platformName)).To(BeTrue(),
						"Deployment %q must be controller-owned by the Platform", name)
				}, platformTimeout, platformInterval).Should(Succeed(), "Deployment "+name)
			}

			By("verifying every component's Service is applied with a Platform owner ref")
			for _, name := range components {
				key := types.NamespacedName{Name: name, Namespace: ns}
				Eventually(func(g Gomega) {
					var svc corev1.Service
					g.Expect(k8sClient.Get(ctx, key, &svc)).To(Succeed())
					g.Expect(controlledBy(svc.OwnerReferences, platformName)).To(BeTrue(),
						"Service %q must be controller-owned by the Platform", name)
				}, platformTimeout, platformInterval).Should(Succeed(), "Service "+name)
			}

			By("verifying every component's ServiceAccount is applied with a Platform owner ref")
			for _, name := range components {
				key := types.NamespacedName{Name: name, Namespace: ns}
				Eventually(func(g Gomega) {
					var sa corev1.ServiceAccount
					g.Expect(k8sClient.Get(ctx, key, &sa)).To(Succeed())
					g.Expect(controlledBy(sa.OwnerReferences, platformName)).To(BeTrue(),
						"ServiceAccount %q must be controller-owned by the Platform", name)
				}, platformTimeout, platformInterval).Should(Succeed(), "ServiceAccount "+name)
			}

			By("verifying the fe-administrator and messaging ConfigMaps are applied with owner refs")
			for _, name := range []string{"fe-administrator-configmap", "messaging-configmap"} {
				key := types.NamespacedName{Name: name, Namespace: ns}
				Eventually(func(g Gomega) {
					var cm corev1.ConfigMap
					g.Expect(k8sClient.Get(ctx, key, &cm)).To(Succeed())
					g.Expect(controlledBy(cm.OwnerReferences, platformName)).To(BeTrue(),
						"ConfigMap %q must be controller-owned by the Platform", name)
				}, platformTimeout, platformInterval).Should(Succeed(), "ConfigMap "+name)
			}

			By("verifying the operator-composed auth-db Secret is applied with an owner ref")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authDBSecretName, Namespace: ns}, &s)).To(Succeed())
				g.Expect(controlledBy(s.OwnerReferences, platformName)).To(BeTrue(),
					"auth-db Secret must be controller-owned by the Platform")
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingCoreAuthReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform status Phase=Running with Available=True condition")
			Eventually(func(g Gomega) {
				var p otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: platformName, Namespace: ns}, &p)).To(Succeed())
				g.Expect(p.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(meta.IsStatusConditionTrue(p.Status.Conditions, "Available")).To(BeTrue(),
					"status must have Available=True condition")
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 1b: auth DB connection-string Secret composition
	// The operator composes the .NET connection string from the DB-creds Secret
	// into an owner-referenced Secret (auth-db); the composed value must
	// never leak into the Platform status/conditions.
	// ---------------------------------------------------------------
	Context("AuthDBSecret", func() {
		const platformName = "ilm"
		const ns = "ilm-authdb"
		const dbPassword = "s3cr3t-db-pw"
		const dbUsername = "ilmuser"

		It("composes auth-db with an owner ref and never leaks the credential into status", func() {
			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			By("creating DB + messaging credential Secrets (DB creds carry the real username/password)")
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dbSecretRef, Namespace: ns},
				Type:       corev1.SecretTypeBasicAuth,
				Data:       map[string][]byte{"username": []byte(dbUsername), "password": []byte(dbPassword)},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: messagingSecretRef, Namespace: ns},
				Type:       corev1.SecretTypeBasicAuth,
				Data:       map[string][]byte{"username": []byte("mq"), "password": []byte("mq-pw")},
			})).To(Succeed())

			By("creating a Platform CR")
			platform := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: platformName, Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, platform)).To(Succeed())

			authSecretKey := types.NamespacedName{Name: authDBSecretName, Namespace: ns}

			By("verifying the operator-managed auth-db Secret is composed correctly")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, authSecretKey, &s)).To(Succeed())

				// The composed connection string lives in StringData on create
				// (envtest preserves it; in a real apiserver it would move to Data).
				conn := s.StringData[connectionStringKey]
				if conn == "" {
					conn = string(s.Data[connectionStringKey])
				}
				g.Expect(conn).NotTo(BeEmpty(), "connection-string key must be present")
				g.Expect(conn).To(ContainSubstring("Host=" + dbHost))
				g.Expect(conn).To(ContainSubstring("Port=5432"))
				g.Expect(conn).To(ContainSubstring("Database=" + dbName))
				g.Expect(conn).To(ContainSubstring("Username=" + dbUsername))
				g.Expect(conn).To(ContainSubstring("Password=" + dbPassword))
				g.Expect(conn).To(ContainSubstring("Pooling=true"))
				// Exact ilm-lib netUrl ordering: Host;Port;Username;Password;Database;Pooling.
				g.Expect(conn).To(Equal(
					"Host=" + dbHost + ";Port=5432;Username=" + dbUsername +
						";Password=" + dbPassword + ";Database=" + dbName + ";Pooling=true"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying auth-db has a controller owner reference to the Platform")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, authSecretKey, &s)).To(Succeed())
				var controlled bool
				for _, ref := range s.OwnerReferences {
					if ref.Controller != nil && *ref.Controller && ref.Name == platformName {
						controlled = true
						break
					}
				}
				g.Expect(controlled).To(BeTrue(), "auth-db must be owned by the Platform")
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingCoreAuthReady)
			markRequiredDeploymentsReady(ns)

			By("security guard: the composed connection string / password never reaches status or conditions")
			Eventually(func(g Gomega) {
				var p otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: platformName, Namespace: ns}, &p)).To(Succeed())
				g.Expect(p.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))

				statusBytes, err := json.Marshal(p.Status)
				g.Expect(err).NotTo(HaveOccurred())
				statusStr := string(statusBytes)
				g.Expect(statusStr).NotTo(ContainSubstring(dbPassword), "DB password must not appear in status")
				g.Expect(statusStr).NotTo(ContainSubstring("Host="), "composed connection string must not appear in status")
				g.Expect(statusStr).NotTo(ContainSubstring(connectionStringKey), "no connection-string material in status")
				g.Expect(statusStr).NotTo(ContainSubstring(dbHost), "DB host must not appear in status")
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("composes auth-db reading the user-MAPPED in-Secret keys (External-Secrets/Vault/CNPG-shaped Secret)", func() {
			const nsMapped = "ilm-authdb-mapped"
			const mappedDBSecret = "eso-db"
			const mappedUsername = "ilm-mapped-user"
			const mappedPassword = "s3cr3t-mapped-pw"

			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsMapped}})).To(Succeed())

			By("creating a DB Secret whose keys are POSTGRES_USER/POSTGRES_PASSWORD (NOT username/password)")
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: mappedDBSecret, Namespace: nsMapped},
				Type:       corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"POSTGRES_USER":     []byte(mappedUsername),
					"POSTGRES_PASSWORD": []byte(mappedPassword),
				},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: messagingSecretRef, Namespace: nsMapped},
				Type:       corev1.SecretTypeBasicAuth,
				Data:       map[string][]byte{"username": []byte("mq"), "password": []byte("mq-pw")},
			})).To(Succeed())

			By("creating a Platform whose database.credentials maps usernameKey/passwordKey to those keys")
			platform := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: nsMapped},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{
							SecretRef: mappedDBSecret, UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD",
						},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, platform)).To(Succeed())

			authSecretKey := types.NamespacedName{Name: authDBSecretName, Namespace: nsMapped}

			By("verifying the composed connection string contains the values read from the MAPPED keys")
			Eventually(func(g Gomega) {
				var s corev1.Secret
				g.Expect(k8sClient.Get(ctx, authSecretKey, &s)).To(Succeed())
				conn := s.StringData[connectionStringKey]
				if conn == "" {
					conn = string(s.Data[connectionStringKey])
				}
				g.Expect(conn).To(Equal(
					"Host=" + dbHost + ";Port=5432;Username=" + mappedUsername +
						";Password=" + mappedPassword + ";Database=" + dbName + ";Pooling=true"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Core's JDBC_USERNAME/JDBC_PASSWORD secretKeyRef points at the MAPPED keys (same mapping feeds both)")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: nsMapped}, &dep)).To(Succeed())
				var userKey, passKey string
				for _, c := range dep.Spec.Template.Spec.Containers {
					if c.Name != "core" {
						continue
					}
					for _, e := range c.Env {
						if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
							continue
						}
						switch e.Name {
						case "JDBC_USERNAME":
							g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal(mappedDBSecret))
							userKey = e.ValueFrom.SecretKeyRef.Key
						case "JDBC_PASSWORD":
							passKey = e.ValueFrom.SecretKeyRef.Key
						}
					}
				}
				g.Expect(userKey).To(Equal("POSTGRES_USER"))
				g.Expect(passKey).To(Equal("POSTGRES_PASSWORD"))
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 2: Missing credential Secret → Degraded (no leak)
	// ---------------------------------------------------------------
	Context("MissingSecret", func() {
		It("sets Degraded (not Available) when a referenced credential Secret is missing", func() {
			ctx := context.Background()
			const ns = "ilm-missing"

			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			p := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "absent-db"}},
					Messaging: otilmv1alpha1.MessagingSpec{Host: "r", VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "absent-mq"}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			Eventually(func() otilmv1alpha1.PlatformPhase {
				got := &otilmv1alpha1.Platform{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, got)
				return got.Status.Phase
			}, 10*time.Second, 250*time.Millisecond).Should(Equal(otilmv1alpha1.PlatformPhaseDegraded))
		})
	})

	// ---------------------------------------------------------------
	// Test 3: Singleton-per-namespace guard
	// ---------------------------------------------------------------
	Context("SingletonPerNamespace", func() {
		It("degrades the newer Platform when two Platforms exist in the same namespace", func() {
			ctx := context.Background()
			const ns = "ilm-singleton"

			By(stepCreatingNamespace)
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			By("creating credential Secrets so the older Platform can reconcile to Running")
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}

			By("creating the first (older) Platform")
			first := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, first)).To(Succeed())

			By("simulating kubelet: marking the first Platform's required Deployments ready")
			markRequiredDeploymentsReady(ns)

			By("waiting for the first Platform to reach Running")
			Eventually(func() otilmv1alpha1.PlatformPhase {
				got := &otilmv1alpha1.Platform{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "first", Namespace: ns}, got)
				return got.Status.Phase
			}, platformTimeout, platformInterval).Should(Equal(otilmv1alpha1.PlatformPhaseRunning))

			By("creating the second (newer) Platform in the same namespace")
			second := &otilmv1alpha1.Platform{
				ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: ns},
				Spec: otilmv1alpha1.PlatformSpec{
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
			Expect(k8sClient.Create(ctx, second)).To(Succeed())

			By("verifying the newer Platform becomes Degraded with reason AnotherPlatformExists")
			Eventually(func(g Gomega) {
				got := &otilmv1alpha1.Platform{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "second", Namespace: ns}, got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseDegraded))

				var degradedFound bool
				for _, cond := range got.Status.Conditions {
					if cond.Type == "Degraded" && cond.Reason == "AnotherPlatformExists" {
						degradedFound = true
						break
					}
				}
				g.Expect(degradedFound).To(BeTrue(), "status must have Degraded condition with reason AnotherPlatformExists")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the first Platform remains Running")
			Consistently(func() otilmv1alpha1.PlatformPhase {
				got := &otilmv1alpha1.Platform{}
				_ = k8sClient.Get(ctx, types.NamespacedName{Name: "first", Namespace: ns}, got)
				return got.Status.Phase
			}, 3*time.Second, 250*time.Millisecond).Should(Equal(otilmv1alpha1.PlatformPhaseRunning))
		})
	})

	// ---------------------------------------------------------------
	// Test 4: Edge gated on upstream-CRD prerequisites (cert-manager / Gateway API)
	// The reconciler must converge the rest of the platform and surface a non-fatal
	// EdgeReady=False when an edge prerequisite is absent. envtest loads only the
	// operator's own CRDs, so this also exercises the real "absent" case end-to-end.
	// Each spec drives the injected fake detector (fakeCaps) per its own namespace.
	// ---------------------------------------------------------------
	Context("EdgeGating", func() {
		// edgeGatingPlatform builds a Platform with credential Secrets and an enabled
		// edge of the caller's shape, in its own namespace.
		newEdgeGatingPlatform := func(ns string, edge *otilmv1alpha1.EdgeSpec) *otilmv1alpha1.Platform {
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
					Edge: edge,
				},
			}
		}

		It("(b) skips the edge but converges the platform when cert-manager is absent (letsEncrypt)", func() {
			const ns = "ilm-edge-nocm"
			By("marking cert-manager absent for this run")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newEdgeGatingPlatform(ns, &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "ingress", Host: testEdgeHost,
				TLS: &otilmv1alpha1.EdgeTLSSpec{
					Source:      "letsEncrypt",
					LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "ops@example.com", Environment: "staging"},
				},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying the rest of the platform still converges (core Deployment applied)")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the Ingress is NOT created (the edge is skipped)")
			Consistently(func() error {
				var ing networkingv1.Ingress
				return k8sClient.Get(ctx, types.NamespacedName{Name: ilmPlatformCoreName, Namespace: ns}, &ing)
			}, 2*time.Second, 250*time.Millisecond).ShouldNot(Succeed())

			By(stepMarkingDeploymentsReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform stays Running/Available with EdgeReady=False/CertManagerNotInstalled")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning),
					"a missing edge dependency must NOT flip the whole Platform to Degraded")
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Available")).To(BeTrue())

				cond := meta.FindStatusCondition(got.Status.Conditions, "EdgeReady")
				g.Expect(cond).NotTo(BeNil(), "EdgeReady condition must be present")
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("CertManagerNotInstalled"))
				g.Expect(cond.Message).To(ContainSubstring("spec.edge.tls.source=secret"), "message must be actionable")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("security guard: no edge host leaks into status conditions")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				condBytes, err := json.Marshal(got.Status.Conditions)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(condBytes)).NotTo(ContainSubstring(testEdgeHost))
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("(c) skips the edge with GatewayAPINotInstalled when the Gateway API CRDs are absent", func() {
			const ns = "ilm-edge-nogw"
			By("marking the Gateway API CRDs absent for this run")
			fakeCaps.setGroup("gateway.networking.k8s.io", false)

			p := newEdgeGatingPlatform(ns, &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "gatewayAPI", Host: testEdgeHost,
				GatewayAPI: &otilmv1alpha1.GatewayAPISpec{GatewayClassName: stringPtr("istio")},
				TLS:        &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: stringPtr("tls")},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By(stepMarkingDeploymentsReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform reaches Running with EdgeReady=False/GatewayAPINotInstalled")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				cond := meta.FindStatusCondition(got.Status.Conditions, "EdgeReady")
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("GatewayAPINotInstalled"))
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("(d) applies a BYO-secret Ingress edge with no cert-manager dependency, setting EdgeReady=True", func() {
			const ns = "ilm-edge-byo"
			By("marking cert-manager absent — a BYO edge must apply anyway")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newEdgeGatingPlatform(ns, &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "ingress", Host: testEdgeHost, ClassName: stringPtr("nginx"),
				TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: stringPtr("my-tls")},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying the Ingress IS created (BYO edge depends on nothing)")
			Eventually(func(g Gomega) {
				var ing networkingv1.Ingress
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ilmPlatformCoreName, Namespace: ns}, &ing)).To(Succeed())
				g.Expect(controlledBy(ing.OwnerReferences, "ilm")).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingDeploymentsReady)
			markRequiredDeploymentsReady(ns)

			By("verifying EdgeReady=True and Platform Running")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "EdgeReady")).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 5: Admin-bootstrap cert gated on cert-manager (source=generated)
	// The admin Certificate is gated exactly like the edge: a missing cert-manager is
	// a non-fatal AdminCertReady=False; the rest of the platform converges and Core's
	// ADMIN_CERT env is wired regardless of the cert-manager gate. source=provided
	// needs no cert-manager. Reuses fakeCaps as the EdgeGating context does.
	// ---------------------------------------------------------------
	Context("AdminCertGating", func() {
		newAdminPlatform := func(ns string, ra *otilmv1alpha1.RegisterAdminSpec) *otilmv1alpha1.Platform {
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
					RegisterAdmin: ra,
				},
			}
		}

		// adminCertEnv returns Core's ADMIN_CERT env var, or nil if absent.
		adminCertEnv := func(g Gomega, ns string) *corev1.EnvVar {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			ctr := coreContainer(g, dep)
			for i := range ctr.Env {
				if ctr.Env[i].Name == "ADMIN_CERT" {
					return &ctr.Env[i]
				}
			}
			return nil
		}

		It("(a) generated + cert-manager absent → AdminCertReady=False/CertManagerNotInstalled, platform still converges, ADMIN_CERT wired", func() {
			const ns = "ilm-admin-nocm"
			By("marking cert-manager absent for this run")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newAdminPlatform(ns, &otilmv1alpha1.RegisterAdminSpec{
				Enabled:  true,
				Username: "root-operator", Name: "Platform Root", Email: "root@secret.example.com",
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying the rest of the platform still converges (core Deployment applied)")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Core's ADMIN_CERT env is wired via secretKeyRef to the generated admin cert Secret (gating-independent)")
			Eventually(func(g Gomega) {
				e := adminCertEnv(g, ns)
				g.Expect(e).NotTo(BeNil(), "ADMIN_CERT must be present when registerAdmin is enabled")
				g.Expect(e.Value).To(BeEmpty(), "ADMIN_CERT must not be inline")
				g.Expect(e.ValueFrom).NotTo(BeNil())
				g.Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
				g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("admin-certificate-secret"))
				g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal("tls.crt"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingDeploymentsReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform stays Running/Available with AdminCertReady=False/CertManagerNotInstalled")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning),
					"a missing admin-cert dependency must NOT flip the whole Platform to Degraded")
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "Available")).To(BeTrue())

				cond := meta.FindStatusCondition(got.Status.Conditions, "AdminCertReady")
				g.Expect(cond).NotTo(BeNil(), "AdminCertReady condition must be present")
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("CertManagerNotInstalled"))
				g.Expect(cond.Message).To(ContainSubstring("spec.registerAdmin.source=provided"), "message must be actionable")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("security guard: no admin identity (username/name/email) leaks into status conditions")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				condBytes, err := json.Marshal(got.Status.Conditions)
				g.Expect(err).NotTo(HaveOccurred())
				condStr := string(condBytes)
				g.Expect(condStr).NotTo(ContainSubstring("root-operator"), "admin username must not appear in conditions")
				g.Expect(condStr).NotTo(ContainSubstring("Platform Root"), "admin name must not appear in conditions")
				g.Expect(condStr).NotTo(ContainSubstring("root@secret.example.com"), "admin email must not appear in conditions")
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("(b) provided source needs no cert-manager: ADMIN_CERT references the SecretRef and no AdminCertReady condition is set", func() {
			const ns = "ilm-admin-provided"
			By("marking cert-manager absent — provided source must not depend on it")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newAdminPlatform(ns, &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided", SecretRef: stringPtr("my-admin-cert")},
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			By("verifying Core's ADMIN_CERT env references the caller-provided Secret")
			Eventually(func(g Gomega) {
				e := adminCertEnv(g, ns)
				g.Expect(e).NotTo(BeNil())
				g.Expect(e.ValueFrom).NotTo(BeNil())
				g.Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
				g.Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("my-admin-cert"))
				g.Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal("tls.crt"))
			}, platformTimeout, platformInterval).Should(Succeed())

			By(stepMarkingDeploymentsReady)
			markRequiredDeploymentsReady(ns)

			By("verifying Platform reaches Running with NO AdminCertReady condition (provided needs no gating)")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(meta.FindStatusCondition(got.Status.Conditions, "AdminCertReady")).To(BeNil(),
					"provided source renders no cert-manager objects, so no AdminCertReady gate")
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 6: Prune de-rendered children
	// After a successful apply, the reconciler garbage-collects owned children no
	// longer in the desired set: disabling utils prunes its Deployment/Service/SA;
	// disabling the edge prunes the Ingress; a composed Secret is pruned when its
	// feature turns off. Each spec runs in its own namespace.
	// ---------------------------------------------------------------
	Context("Prune", func() {
		// newPlatformWith builds a credentialled Platform in ns and applies a mutator.
		newPlatformWith := func(ns string, mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			for _, n := range []string{dbSecretRef, messagingSecretRef} {
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
					Type:       corev1.SecretTypeBasicAuth,
					Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
				})).To(Succeed())
			}
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
			mutate(p)
			return p
		}

		// updatePlatform re-fetches and mutates the Platform under a conflict retry.
		updatePlatform := func(ns string, mutate func(*otilmv1alpha1.Platform)) {
			Eventually(func() error {
				var p otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &p); err != nil {
					return err
				}
				mutate(&p)
				return k8sClient.Update(ctx, &p)
			}, platformTimeout, platformInterval).Should(Succeed())
		}

		It("(a) prunes the utils Deployment/Service/SA when utils is disabled", func() {
			const ns = "ilm-prune-utils"
			p := newPlatformWith(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Utils = otilmv1alpha1.UtilsSpec{Enabled: true}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			utilsKey := types.NamespacedName{Name: "utils", Namespace: ns}
			By("verifying the utils children are created while enabled")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, utilsKey, &dep)).To(Succeed())
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, utilsKey, &svc)).To(Succeed())
				var sa corev1.ServiceAccount
				g.Expect(k8sClient.Get(ctx, utilsKey, &sa)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("disabling utils")
			updatePlatform(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Utils = otilmv1alpha1.UtilsSpec{Enabled: false}
			})

			By("verifying the utils Deployment/Service/SA are pruned")
			Eventually(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, utilsKey, &dep))).To(BeTrue(), "Deployment pruned")
				var svc corev1.Service
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, utilsKey, &svc))).To(BeTrue(), "Service pruned")
				var sa corev1.ServiceAccount
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, utilsKey, &sa))).To(BeTrue(), "ServiceAccount pruned")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying Core (still desired) is NOT pruned")
			Consistently(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("(b) prunes the Ingress when the edge is disabled", func() {
			const ns = "ilm-prune-edge"
			By("marking cert-manager absent — a BYO edge needs none")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newPlatformWith(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.Edge = &otilmv1alpha1.EdgeSpec{
					Enabled: true, Type: "ingress", Host: testEdgeHost,
					TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: stringPtr("my-tls")},
				}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			ingKey := types.NamespacedName{Name: ilmPlatformCoreName, Namespace: ns}
			By("verifying the Ingress is created while the edge is enabled")
			Eventually(func(g Gomega) {
				var ing networkingv1.Ingress
				g.Expect(k8sClient.Get(ctx, ingKey, &ing)).To(Succeed())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("disabling the edge")
			updatePlatform(ns, func(p *otilmv1alpha1.Platform) { p.Spec.Edge.Enabled = false })

			By("verifying the now-stale Ingress is pruned")
			Eventually(func(g Gomega) {
				var ing networkingv1.Ingress
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, ingKey, &ing))).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("(d) keeps the composed trusted-certificates Secret while registerAdmin=generated, prunes it when disabled", func() {
			const ns = "ilm-prune-secret"
			By("marking cert-manager absent (composition is independent of the cert-manager gate)")
			fakeCaps.setGroup(certManagerGroup, false)

			p := newPlatformWith(ns, func(p *otilmv1alpha1.Platform) {
				p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
					Enabled:  true,
					Username: "admin", Name: "Admin", Email: "a@example.com",
					Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
				}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			secretKey := types.NamespacedName{Name: "trusted-certificates", Namespace: ns}
			By("verifying the composed trusted-certificates Secret exists while the feature is on")
			Eventually(func(g Gomega) {
				var sec corev1.Secret
				g.Expect(k8sClient.Get(ctx, secretKey, &sec)).To(Succeed())
				g.Expect(controlledBy(sec.OwnerReferences, "ilm")).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("disabling registerAdmin (composition turns off)")
			updatePlatform(ns, func(p *otilmv1alpha1.Platform) { p.Spec.RegisterAdmin.Enabled = false })

			By("verifying the composed Secret is pruned when its feature turns off")
			Eventually(func(g Gomega) {
				var sec corev1.Secret
				g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &sec))).To(BeTrue())
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the operator-managed auth-db Secret (feature still on) is NOT pruned")
			Consistently(func(g Gomega) {
				var sec corev1.Secret
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authDBSecretName, Namespace: ns}, &sec)).To(Succeed())
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// Test 7: spec.version selection + status.observedVersion (operator/platform decoupling)
	// An unset/known spec.version reconciles normally and reports the resolved version on
	// status.observedVersion; an UNKNOWN spec.version is a deterministic user mistake that
	// degrades with an actionable supported-versions message (NOT a crash, NOT a frozen
	// CEL enum) and renders no children. Each spec runs in its own namespace.
	// ---------------------------------------------------------------
	Context("PlatformVersion", func() {
		newVersionedPlatform := func(ns, version string) *otilmv1alpha1.Platform {
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
					Version: version,
					Database: otilmv1alpha1.DatabaseSpec{
						Mode: "external", Host: dbHost, Port: 5432, Name: dbName,
						Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: dbSecretRef},
					},
					Messaging: otilmv1alpha1.MessagingSpec{
						Mode: "external", BrokerType: "rabbitmq", Host: brokerHost,
						Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: messagingSecretRef},
					},
				},
			}
		}

		It("reports status.observedVersion = the operator's newest when spec.version is unset", func() {
			const ns = "ilm-version-default"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, ""))).To(Succeed())

			By("marking the required Deployments ready so the platform reaches Running")
			markRequiredDeploymentsReady(ns)

			By("verifying status.observedVersion is the operator's DefaultVersion")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion),
					"an unset spec.version resolves the operator's newest version")
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("reports status.observedVersion = spec.version when a KNOWN version is selected", func() {
			const ns = "ilm-version-known"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, bom.DefaultVersion))).To(Succeed())

			By("marking the required Deployments ready so the platform reaches Running")
			markRequiredDeploymentsReady(ns)

			By("verifying status.observedVersion echoes the explicitly-selected version")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion))
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("degrades with an actionable supported-versions message when spec.version is UNKNOWN", func() {
			const ns = "ilm-version-unknown"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, "9.9.9"))).To(Succeed())

			By("verifying the Platform is Degraded with reason UnsupportedVersion listing the supported versions")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseDegraded))

				cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
				g.Expect(cond).NotTo(BeNil(), "an unknown version must set a Degraded condition")
				g.Expect(cond.Reason).To(Equal("UnsupportedVersion"))
				// The message is actionable: it names the supported versions this build carries.
				g.Expect(cond.Message).To(ContainSubstring("9.9.9"))
				for _, v := range bom.SupportedVersions() {
					g.Expect(cond.Message).To(ContainSubstring(v),
						"the degraded message must list every supported version")
				}
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying an unknown version renders NO children (the reconcile stops before rendering)")
			Consistently(func() error {
				var dep appsv1.Deployment
				return k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)
			}, 2*time.Second, 250*time.Millisecond).ShouldNot(Succeed())
		})

		It("recovers to Running once an unknown spec.version is corrected to a supported one", func() {
			const ns = "ilm-version-recover"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, "8.8.8"))).To(Succeed())

			By("verifying it first degrades on the unknown version")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseDegraded))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("correcting spec.version to a supported version")
			Eventually(func() error {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got); err != nil {
					return err
				}
				got.Spec.Version = "" // unset → the operator's newest
				return k8sClient.Update(ctx, &got)
			}, platformTimeout, platformInterval).Should(Succeed())

			By("marking the required Deployments ready and verifying recovery to Running with observedVersion set")
			markRequiredDeploymentsReady(ns)
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion))
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("refuses an explicit downgrade (DowngradeForbidden) and keeps the running version", func() {
			const ns = "ilm-version-downgrade"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, ""))).To(Succeed())

			By("reaching Running with observedVersion pinned to the operator's newest")
			markRequiredDeploymentsReady(ns)
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("requesting an OLDER spec.version than the running version")
			Eventually(func() error {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got); err != nil {
					return err
				}
				got.Spec.Version = "2.0.0" // strictly older than the running DefaultVersion
				return k8sClient.Update(ctx, &got)
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying it goes Degraded/DowngradeForbidden and the running version is NOT rolled back")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
				g.Expect(cond).NotTo(BeNil(), "a downgrade must set a Degraded condition")
				g.Expect(cond.Reason).To(Equal(reasonDowngradeForbidden))
				g.Expect(cond.Message).To(ContainSubstring("2.0.0"))
				// status.observedVersion stays at the running version — the platform is NOT downgraded.
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion),
					"the running version must remain in effect when a downgrade is refused")
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("refuses upgrading a live platform onto a preview bundle, but allows a fresh preview install", func() {
			By("reaching Running on the operator's newest (released) version")
			const ns = "ilm-version-preview-upgrade"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, ""))).To(Succeed())
			markRequiredDeploymentsReady(ns)
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("requesting an upgrade to the unreleased 2.19.0 preview bundle")
			Eventually(func() error {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got); err != nil {
					return err
				}
				got.Spec.Version = platformVersion219
				return k8sClient.Update(ctx, &got)
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying it goes Degraded/PreviewVersionUpgradeBlocked and the running version is NOT moved")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
				g.Expect(cond).NotTo(BeNil(), "an upgrade onto a preview bundle must set a Degraded condition")
				g.Expect(cond.Reason).To(Equal(reasonPreviewVersionUpgradeBlocked))
				g.Expect(cond.Message).To(ContainSubstring(platformVersion219))
				// status.observedVersion stays at the running (released) version — the platform is
				// NOT upgraded onto the preview bundle.
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion),
					"the running version must remain in effect when a preview upgrade is refused")
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the core Deployment is NOT re-rendered/rolled onto the blocked preview image")
			Consistently(func(g Gomega) {
				var dep appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "core", Namespace: ns}, &dep)).To(Succeed())
				g.Expect(coreContainer(g, dep).Image).To(HaveSuffix(":"+bom.DefaultVersion),
					"a blocked preview upgrade must not roll the core Deployment onto the preview version's image")
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())

			By("creating a FRESH platform pinned directly to the same preview version")
			const freshNS = "ilm-version-preview-fresh"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(freshNS, platformVersion219))).To(Succeed())
			markRequiredDeploymentsReady(freshNS)

			By("verifying the fresh install proceeds to Running, pinned to the preview version")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: freshNS}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				g.Expect(got.Status.ObservedVersion).To(Equal(platformVersion219))
				cond := meta.FindStatusCondition(got.Status.Conditions, conditionDegraded)
				if cond != nil {
					g.Expect(cond.Reason).NotTo(Equal(reasonPreviewVersionUpgradeBlocked),
						"a fresh preview install must not be blocked by the upgrade guard")
				}
			}, platformTimeout, platformInterval).Should(Succeed())
		})

		It("clears the stale Degraded condition once a refused spec.version is corrected", func() {
			const ns = "ilm-version-degraded-cleared"
			Expect(k8sClient.Create(ctx, newVersionedPlatform(ns, ""))).To(Succeed())
			markRequiredDeploymentsReady(ns)

			By("reaching Running on the operator's newest (released) version")
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.ObservedVersion).To(Equal(bom.DefaultVersion))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("requesting the blocked preview upgrade so the platform goes Degraded")
			Eventually(func() error {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got); err != nil {
					return err
				}
				got.Spec.Version = platformVersion219
				return k8sClient.Update(ctx, &got)
			}, platformTimeout, platformInterval).Should(Succeed())

			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionDegraded)).To(BeTrue())
				g.Expect(meta.FindStatusCondition(got.Status.Conditions, conditionDegraded).Reason).
					To(Equal(reasonPreviewVersionUpgradeBlocked))
			}, platformTimeout, platformInterval).Should(Succeed())

			By("reverting spec.version to the version actually running")
			Eventually(func() error {
				var got otilmv1alpha1.Platform
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got); err != nil {
					return err
				}
				got.Spec.Version = bom.DefaultVersion
				return k8sClient.Update(ctx, &got)
			}, platformTimeout, platformInterval).Should(Succeed())

			By("verifying the stale Degraded is cleared (not left True forever) and the platform is Running again")
			markRequiredDeploymentsReady(ns)
			Eventually(func(g Gomega) {
				var got otilmv1alpha1.Platform
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ilm", Namespace: ns}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(otilmv1alpha1.PlatformPhaseRunning))
				// Literal type/reason: the published contract, asserted independently of the
				// production constants so a rename cannot silently keep this green.
				cond := meta.FindStatusCondition(got.Status.Conditions, "Degraded")
				g.Expect(cond).NotTo(BeNil(), "the condition stays visible, flipped to False")
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
					"a corrected platform must not advertise Degraded=True forever")
				g.Expect(cond.Reason).To(Equal("Reconciled"))
			}, platformTimeout, platformInterval).Should(Succeed())
		})
	})
})

// stringPtr returns a pointer to s, for setting *string CR fields in specs.
func stringPtr(s string) *string { return &s }

// controlledBy reports whether refs contains a controller owner reference whose
// Name matches owner — i.e. the object is controller-owned by that owner. The owner
// name is a parameter so the helper reads as a general owner-reference assertion,
// even though current specs all assert ownership by the "ilm" Platform.
//
//nolint:unparam // owner is intentionally a parameter for a reusable ownership assertion
func controlledBy(refs []metav1.OwnerReference, owner string) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.Name == owner {
			return true
		}
	}
	return false
}

// coreContainer returns the main "core" container from a rendered Core Deployment.
// Core's pod also runs the auth-opa sidecar, so assertions must target the main
// container by name rather than assuming a single-container pod.
func coreContainer(g Gomega, dep appsv1.Deployment) corev1.Container {
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		if ctr.Name == "core" {
			return ctr
		}
	}
	g.Expect(false).To(BeTrue(), "Deployment must contain a container named 'core'")
	return corev1.Container{}
}

// requiredReadyDeployments are the Deployments the reconciler gates Available on
// (Core + the auth provider). Tests simulate readiness on exactly these.
var requiredReadyDeployments = []string{"core", "auth"}

// markRequiredDeploymentsReady simulates kubelet by setting the required Deployments'
// status to ready (AvailableReplicas == desired + Available=True). envtest runs no
// kubelet, so the operator's MEASURED readiness would otherwise stay Progressing
// forever; this lets specs drive the Platform to Running. It waits for each Deployment
// to be applied first, then patches its /status subresource.
func markRequiredDeploymentsReady(ns string) {
	for _, name := range requiredReadyDeployments {
		key := types.NamespacedName{Name: name, Namespace: ns}
		Eventually(func() error {
			var dep appsv1.Deployment
			if err := k8sClient.Get(ctx, key, &dep); err != nil {
				return err
			}
			desired := int32(1)
			if dep.Spec.Replicas != nil {
				desired = *dep.Spec.Replicas
			}
			dep.Status.Replicas = desired
			dep.Status.ReadyReplicas = desired
			dep.Status.AvailableReplicas = desired
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Conditions = []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
				Reason: "MinimumReplicasAvailable", LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now(),
			}}
			return k8sClient.Status().Update(ctx, &dep)
		}, platformTimeout, platformInterval).Should(Succeed())
	}
}

// markDeploymentNotReady sets the named Deployment's status to 0 ready replicas with
// Available=False, simulating a not-yet-rolled-out workload.
func markDeploymentNotReady(ns, name string) {
	key := types.NamespacedName{Name: name, Namespace: ns}
	Eventually(func() error {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &dep); err != nil {
			return err
		}
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 0
		dep.Status.AvailableReplicas = 0
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
			Reason: "MinimumReplicasUnavailable", LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now(),
		}}
		return k8sClient.Status().Update(ctx, &dep)
	}, platformTimeout, platformInterval).Should(Succeed())
}
