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
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// These specs exercise the field-level CEL (XValidation) on the Platform CRD at
// the apiserver admission layer: they assert the apiserver REJECTS each
// cross-field-invalid CR (with the rule's message) and ACCEPTS the corrected one.
// The CRD installing successfully in BeforeSuite already proves the CEL compiles;
// these specs prove the rules actually fire on the cases they target.
//
// Every spec uses a fresh namespace and a name that satisfies every rule except the
// one under test, so a rejection is unambiguously attributable to that rule. The
// reconciler is irrelevant here (admission happens before any reconcile), so no
// readiness is awaited — only Create's success/failure.
var _ = Describe("Platform CEL validation", func() {
	var nsCounter int

	// freshNS creates and returns a unique namespace name for one spec.
	freshNS := func(prefix string) string {
		nsCounter++
		name := prefix
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
		return name
	}

	// validDB / validMessaging satisfy the external-mode coordinate rules.
	validDB := func() otilmv1alpha1.DatabaseSpec {
		return otilmv1alpha1.DatabaseSpec{Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "db-secret"}}
	}
	validMessaging := func() otilmv1alpha1.MessagingSpec {
		return otilmv1alpha1.MessagingSpec{Mode: "external", BrokerType: "rabbitmq", Host: "mq", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "mq-secret"}}
	}
	platformIn := func(ns string) *otilmv1alpha1.Platform {
		return &otilmv1alpha1.Platform{
			ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: ns},
			Spec:       otilmv1alpha1.PlatformSpec{Database: validDB(), Messaging: validMessaging()},
		}
	}

	Context("database external-mode coordinates", func() {
		It("rejects mode=external with missing host/name/credentials.secretRef", func() {
			ns := freshNS("cel-db-bad")
			p := platformIn(ns)
			p.Spec.Database = otilmv1alpha1.DatabaseSpec{Mode: "external", Port: 5432} // no host/name/credentials
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("database.host, database.name and database.credentials.secretRef are required when mode=external"))
		})
		It("rejects mode=external with a credentials block but no secretRef (only key overrides)", func() {
			ns := freshNS("cel-db-nosecret")
			p := platformIn(ns)
			// host/name present, but credentials carries only key overrides (no secretRef): the
			// reshaped CEL's has(self.credentials.secretRef) clause must still reject it.
			p.Spec.Database = otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "db", Port: 5432, Name: "ilm",
				Credentials: &otilmv1alpha1.CredentialsRef{UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD"},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("database.host, database.name and database.credentials.secretRef are required when mode=external"))
		})
		It("accepts a complete external database", func() {
			ns := freshNS("cel-db-good")
			Expect(k8sClient.Create(ctx, platformIn(ns))).To(Succeed())
		})
		It("accepts an external database with custom credential key overrides", func() {
			ns := freshNS("cel-db-mapped")
			p := platformIn(ns)
			p.Spec.Database = otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "db", Port: 5432, Name: "ilm",
				Credentials: &otilmv1alpha1.CredentialsRef{
					SecretRef: "db-secret", UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It(rejectsManagedWithoutBlock, func() {
			ns := freshNS("cel-db-managed-bad")
			p := platformIn(ns)
			// Clear the external coordinates and select managed, but omit the managed block.
			p.Spec.Database = otilmv1alpha1.DatabaseSpec{Mode: "managed", Port: 5432}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("database.managed is required when mode=managed"))
		})
		It("accepts mode=managed with the managed block (external coordinates not required)", func() {
			ns := freshNS("cel-db-managed-good")
			p := platformIn(ns)
			// Managed mode needs only the managed block — no host/name/credentials.
			p.Spec.Database = otilmv1alpha1.DatabaseSpec{
				Mode: "managed",
				Managed: &otilmv1alpha1.ManagedDatabaseSpec{
					Instances: 1,
					Version:   "16",
					Storage:   otilmv1alpha1.StorageSpec{Size: "10Gi"},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("messaging external-mode coordinates", func() {
		It("rejects mode=external with missing host/credentials.secretRef", func() {
			ns := freshNS("cel-mq-bad")
			p := platformIn(ns)
			p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "external", BrokerType: "rabbitmq", Port: 5672} // no host/credentials
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("messaging.host and messaging.credentials.secretRef are required when mode=external"))
		})
		It(rejectsManagedWithoutBlock, func() {
			ns := freshNS("cel-mq-managed-bad")
			p := platformIn(ns)
			// Clear the external coordinates and select managed, but omit the managed block.
			p.Spec.Messaging = otilmv1alpha1.MessagingSpec{Mode: "managed", BrokerType: "rabbitmq", Port: 5672}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("messaging.managed is required when mode=managed"))
		})
		It("accepts mode=managed with the managed block (external coordinates not required)", func() {
			ns := freshNS("cel-mq-managed-good")
			p := platformIn(ns)
			// Managed mode needs only the managed block — no host/credentials.
			p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
				Mode:       "managed",
				BrokerType: "rabbitmq",
				Managed: &otilmv1alpha1.ManagedMessagingSpec{
					Replicas: 1,
					Version:  "4.0",
					Storage:  otilmv1alpha1.StorageSpec{Size: "20Gi"},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("keycloak managed-mode block", func() {
		It(rejectsManagedWithoutBlock, func() {
			ns := freshNS("cel-kc-managed-bad")
			p := platformIn(ns)
			// Select managed Keycloak but omit the managed block.
			p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "managed", Realm: "ilm"}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("keycloak.managed is required when mode=managed"))
		})
		It("accepts mode=managed with the managed block", func() {
			ns := freshNS("cel-kc-managed-good")
			p := platformIn(ns)
			p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{
				Mode:    "managed",
				Realm:   "ilm",
				Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.0"},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts mode=external with no managed block", func() {
			ns := freshNS("cel-kc-external")
			p := platformIn(ns)
			p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("edge", func() {
		It("rejects enabled edge with no host on either edge.host or common.hostName", func() {
			ns := freshNS("cel-edge-nohost")
			p := platformIn(ns)
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress"} // no edge.host, no common.hostName
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("an enabled edge needs a public host: set edge.host or common.hostName"))
		})
		It("accepts an enabled edge whose host comes from common.hostName (no edge.host)", func() {
			ns := freshNS("cel-edge-commonhost")
			p := platformIn(ns)
			p.Spec.Common.HostName = testEdgeHost // canonical FQDN satisfies the enabled edge
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress"}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a disabled edge with no host", func() {
			ns := freshNS("cel-edge-disabled")
			p := platformIn(ns)
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects type=gatewayAPI without gatewayClassName or parentRef", func() {
			ns := freshNS("cel-edge-gwapi-bad")
			p := platformIn(ns)
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "gatewayAPI", Host: testEdgeHost,
				GatewayAPI: &otilmv1alpha1.GatewayAPISpec{}, // neither gatewayClassName nor parentRef
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("edge.type=gatewayAPI requires gatewayAPI.gatewayClassName or gatewayAPI.parentRef"))
		})
		It("accepts type=gatewayAPI with gatewayClassName", func() {
			ns := freshNS("cel-edge-gwapi-good")
			p := platformIn(ns)
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "gatewayAPI", Host: testEdgeHost,
				GatewayAPI: &otilmv1alpha1.GatewayAPISpec{GatewayClassName: stringPtr("istio")},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("edge.tls source-specific blocks", func() {
		newEdge := func(tls *otilmv1alpha1.EdgeTLSSpec) *otilmv1alpha1.EdgeSpec {
			return &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress", Host: testEdgeHost, TLS: tls}
		}
		It("rejects source=letsEncrypt without an email", func() {
			ns := freshNS("cel-tls-le-bad")
			p := platformIn(ns)
			p.Spec.Edge = newEdge(&otilmv1alpha1.EdgeTLSSpec{Source: "letsEncrypt", LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Environment: "staging"}})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("edge.tls.letsEncrypt.email is required when tls.source=letsEncrypt"))
		})
		It("accepts source=letsEncrypt with an email", func() {
			ns := freshNS("cel-tls-le-good")
			p := platformIn(ns)
			p.Spec.Edge = newEdge(&otilmv1alpha1.EdgeTLSSpec{Source: "letsEncrypt", LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "ops@example.com", Environment: "staging"}})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects source=issuerRef without an issuerRef", func() {
			ns := freshNS("cel-tls-ir-bad")
			p := platformIn(ns)
			p.Spec.Edge = newEdge(&otilmv1alpha1.EdgeTLSSpec{Source: "issuerRef"})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("edge.tls.issuerRef is required when tls.source=issuerRef"))
		})
		It("rejects source=secret without a secretRef", func() {
			ns := freshNS("cel-tls-secret-bad")
			p := platformIn(ns)
			p.Spec.Edge = newEdge(&otilmv1alpha1.EdgeTLSSpec{Source: "secret"})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("edge.tls.secretRef is required when tls.source=secret"))
		})
		It("accepts source=secret with a secretRef", func() {
			ns := freshNS("cel-tls-secret-good")
			p := platformIn(ns)
			p.Spec.Edge = newEdge(&otilmv1alpha1.EdgeTLSSpec{Source: "secret", SecretRef: stringPtr("my-tls")})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	// managedKeycloak satisfies the password method's keycloak.mode=managed requirement.
	managedKeycloak := func() *otilmv1alpha1.KeycloakSpec {
		return &otilmv1alpha1.KeycloakSpec{
			Mode: "managed", Realm: "ilm",
			Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1},
		}
	}

	Context("registerAdmin certificate method", func() {
		It("rejects certificate enabled + source=provided without a secretRef", func() {
			ns := freshNS("cel-admin-bad")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided"},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("registerAdmin.certificate.secretRef is required when certificate.source=provided"))
		})
		It("accepts certificate enabled + source=provided with a secretRef", func() {
			ns := freshNS("cel-admin-good")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "provided", SecretRef: stringPtr("admin-cert")},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts certificate enabled + source=generated without a secretRef", func() {
			ns := freshNS("cel-admin-generated")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts an enabled registerAdmin with NO sub-blocks (certificate defaults ON)", func() {
			ns := freshNS("cel-admin-default-cert")
			p := platformIn(ns)
			// certificate.enabled defaults true and source defaults provided; but with no
			// secretRef and source=provided defaulted, the certificate.secretRef rule fires only
			// when the certificate block is PRESENT. An absent block is the historical default
			// (provided) — accepted at admission; the operator waits on the (absent) Secret at
			// runtime, mirroring the prior single-method behaviour.
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a disabled registerAdmin with no methods (Enabled optional, defaults false)", func() {
			ns := freshNS("cel-admin-disabled")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects an enabled registerAdmin with BOTH methods explicitly disabled", func() {
			ns := freshNS("cel-admin-nomethod")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(false)},
				Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: false},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("registerAdmin requires at least one method"))
		})
	})

	Context("registerAdmin password method", func() {
		It("rejects password.enabled without a secretRef", func() {
			ns := freshNS("cel-admin-pw-nosecret")
			p := platformIn(ns)
			p.Spec.Keycloak = managedKeycloak()
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:  true,
				Password: &otilmv1alpha1.AdminPasswordSpec{Enabled: true},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("registerAdmin.password.secretRef is required when password.enabled is true"))
		})
		It("rejects password.enabled when keycloak is not managed", func() {
			ns := freshNS("cel-admin-pw-extkc")
			p := platformIn(ns) // no keycloak block → external by default
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(false)},
				Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: true, SecretRef: adminPasswordSecretRef},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("registerAdmin.password requires keycloak.mode=managed"))
		})
		It("accepts password.enabled with a secretRef and managed Keycloak (password-only)", func() {
			ns := freshNS("cel-admin-pw-good")
			p := platformIn(ns)
			p.Spec.Keycloak = managedKeycloak()
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(false)},
				Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: true, SecretRef: adminPasswordSecretRef},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts BOTH methods enabled (certificate generated + password) with managed Keycloak", func() {
			ns := freshNS("cel-admin-both")
			p := platformIn(ns)
			p.Spec.Keycloak = managedKeycloak()
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated"},
				Password:    &otilmv1alpha1.AdminPasswordSpec{Enabled: true, SecretRef: adminPasswordSecretRef},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("provisioning deploy mode", func() {
		It("rejects mode=deploy without the deploy block", func() {
			ns := freshNS("cel-prov-nodeploy")
			p := platformIn(ns)
			p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{Mode: "deploy"}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("provisioning.deploy is required when mode=deploy"))
		})
		It("rejects mode=deploy with a non-rabbitmq broker (servicebus)", func() {
			ns := freshNS("cel-prov-servicebus")
			p := platformIn(ns)
			// A valid servicebus external broker, but deploy is RabbitMQ-specific.
			p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
				Mode: "external", BrokerType: "servicebus", Host: "sb", Port: 5671, VirtualHost: "ilm",
				Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "mq-secret"},
			}
			p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
				Mode:   "deploy",
				Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: "prov-bootstrap"},
			}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("provisioning.mode=deploy requires messaging.brokerType=rabbitmq"))
		})
		It("accepts mode=deploy with a rabbitmq broker and the deploy block", func() {
			ns := freshNS("cel-prov-good")
			p := platformIn(ns) // validMessaging is rabbitmq
			p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
				Mode:   "deploy",
				Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: "prov-bootstrap"},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts the default external provisioning (apiURL only, no deploy block)", func() {
			ns := freshNS("cel-prov-external")
			p := platformIn(ns)
			p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
				Mode: "external", APIURL: "https://prov.example.com", APIKeySecretRef: "prov-secret",
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})
})
