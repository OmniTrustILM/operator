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
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

const (
	// testBootstrapSecretName is the provisioning bootstrap Secret name these CEL cases
	// reference; the Secret itself is never read, only the reference is validated.
	testBootstrapSecretName = "prov-bootstrap"
	// testPatternRejection is the fragment the apiserver includes when a field fails its
	// charset Pattern, as opposed to a cross-field XValidation rule.
	testPatternRejection = "should match"
	// testDuplicateRejection is the fragment the apiserver includes when a list-map receives
	// two entries sharing a merge key.
	testDuplicateRejection = "Duplicate value"
	// testQueueArgExpires is the standard queue-argument name for message expiration.
	testQueueArgExpires = "x-expires"
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
				Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: testBootstrapSecretName},
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
				Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: testBootstrapSecretName},
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

	// The exchange name is rendered into the queue-registration request Core issues at startup,
	// so the field is charset-constrained at ADMISSION as defence in depth: a shell/JSON
	// metacharacter can never be stored, on top of the builder composing the request body with
	// encoding/json inside a quoted heredoc.
	Context("provisioning deploy exchange charset", func() {
		provisioningWith := func(exchange string) *otilmv1alpha1.ProvisioningSpec {
			return &otilmv1alpha1.ProvisioningSpec{
				Mode: "deploy",
				Deploy: &otilmv1alpha1.ProvisioningDeploySpec{
					BootstrapSecretRef: testBootstrapSecretName, Exchange: exchange,
				},
			}
		}
		for i, bad := range []string{"$(id)", "`id`", `a"b`, "a\nb", "a$VAR", "a b", "a;b"} {
			exchange := bad
			It("rejects a shell/JSON metacharacter in the exchange name: "+exchange, func() {
				ns := freshNS(fmt.Sprintf("cel-prov-exchange-bad-%d", i))
				p := platformIn(ns)
				p.Spec.Provisioning = provisioningWith(exchange)
				err := k8sClient.Create(ctx, p)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(testPatternRejection))
			})
		}
		It("accepts a conventional exchange name", func() {
			ns := freshNS("cel-prov-exchange-good")
			p := platformIn(ns)
			p.Spec.Provisioning = provisioningWith("ilm-proxy_2.v1:alt")
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	// The routing key completes the configurable queue-request trio (exchange / routingKey /
	// queueArguments) and lands in the same rendered request, so it carries the same charset
	// discipline — widened by exactly two things a binding key legitimately needs: the AMQP
	// topic wildcards * and #, and the three characters of the literal ${HOSTNAME} token the
	// init container substitutes with the pod name at runtime. Shell metacharacters, quotes,
	// whitespace and newlines stay rejected.
	Context("provisioning deploy routingKey charset", func() {
		deployWith := func(apply func(*otilmv1alpha1.ProvisioningDeploySpec)) *otilmv1alpha1.ProvisioningSpec {
			d := &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: testBootstrapSecretName}
			apply(d)
			return &otilmv1alpha1.ProvisioningSpec{Mode: "deploy", Deploy: d}
		}
		routingKeyWith := func(key string) *otilmv1alpha1.ProvisioningSpec {
			return deployWith(func(d *otilmv1alpha1.ProvisioningDeploySpec) { d.RoutingKey = key })
		}

		for i, good := range []string{
			"proxymessage.*.${HOSTNAME}", // the shipped default: both a wildcard and the token
			"proxymessage.#",             // an AMQP multi-word topic wildcard
			"events.*.eu-west_1:v2",      // the plain binding-key charset
			"${HOSTNAME}",                // the token alone
		} {
			key := good
			It("accepts a legitimate binding key: "+key, func() {
				ns := freshNS(fmt.Sprintf("cel-prov-rk-good-%d", i))
				p := platformIn(ns)
				p.Spec.Provisioning = routingKeyWith(key)
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
			})
		}

		for i, bad := range []string{"$(id)", "`id`", `a"b`, "a\nb", "a b", "a;b", "a|b", "a&b"} {
			key := bad
			It("rejects a shell/JSON metacharacter in the routing key: "+key, func() {
				ns := freshNS(fmt.Sprintf("cel-prov-rk-bad-%d", i))
				p := platformIn(ns)
				p.Spec.Provisioning = routingKeyWith(key)
				err := k8sClient.Create(ctx, p)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(testPatternRejection))
			})
		}

		// queueArguments replaces the operator's default argument set outright, so a repeated
		// name is a configuration mistake with a silently-wins outcome; the field is a
		// list-map keyed by name, so the apiserver rejects it at the door instead.
		It("accepts distinct queue-argument names with arbitrary JSON values", func() {
			ns := freshNS("cel-prov-qargs-good")
			p := platformIn(ns)
			p.Spec.Provisioning = deployWith(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
				d.QueueArguments = []otilmv1alpha1.QueueArgument{
					{Name: testQueueArgExpires, Value: apiextensionsv1.JSON{Raw: []byte("1800000")}},
					{Name: "x-queue-type", Value: apiextensionsv1.JSON{Raw: []byte(`"quorum"`)}},
					{Name: "x-overflow", Value: apiextensionsv1.JSON{Raw: []byte(`{"strategy":"drop-head"}`)}},
				}
			})
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects a repeated queue-argument name", func() {
			ns := freshNS("cel-prov-qargs-dup")
			p := platformIn(ns)
			p.Spec.Provisioning = deployWith(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
				d.QueueArguments = []otilmv1alpha1.QueueArgument{
					{Name: testQueueArgExpires, Value: apiextensionsv1.JSON{Raw: []byte("1")}},
					{Name: testQueueArgExpires, Value: apiextensionsv1.JSON{Raw: []byte("2")}},
				}
			})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testDuplicateRejection))
		})
		It("rejects a shell/JSON metacharacter in a queue-argument name", func() {
			ns := freshNS("cel-prov-qargs-bad-name")
			p := platformIn(ns)
			p.Spec.Provisioning = deployWith(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
				d.QueueArguments = []otilmv1alpha1.QueueArgument{
					{Name: "$(id)", Value: apiextensionsv1.JSON{Raw: []byte("1")}},
				}
			})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testPatternRejection))
		})
	})

	// The broker/database hosts, the Keycloak realm and the platform/edge hostnames are all
	// rendered into generated wiring (the broker-reachability wait loops, the in-pod OIDC
	// registration request), so each is charset-constrained at ADMISSION as defence in depth:
	// a shell/JSON metacharacter can never be STORED. The structural fixes in the builders —
	// env-var indirection for the wait loops, encoding/json plus quoted heredocs for the request
	// bodies — are what make those renders safe regardless; this layer just stops the value at
	// the door. spec.registerAdmin.{username,name,lastName,email} deliberately carry NO pattern:
	// apostrophes and unicode are legitimate in human names and addresses, and the JSON encoding
	// is what makes them safe.
	Context("host / realm charsets", func() {
		badValues := []string{"$(id)", "`id`", `a"b`, "a'b", "a\nb", "a$VAR", "a;b", "a|b", "a b"}
		fields := []struct {
			name string
			set  func(*otilmv1alpha1.Platform, string)
			good string
		}{
			{
				name: "messaging.host",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Messaging.Host = v
				},
				good: "rabbitmq.example.com",
			},
			{
				name: "database.host",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Database.Host = v
				},
				good: "postgres.example.com",
			},
			{
				name: "keycloak.realm",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: v}
				},
				good: "ilm_realm-2.0",
			},
			{
				name: "common.hostName",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Common.HostName = v
				},
				good: "ilm.example.com",
			},
			{
				name: "edge.host",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Host: v}
				},
				good: "ilm.example.com",
			},
		}
		for fi, f := range fields {
			field := f
			for vi, bad := range badValues {
				value := bad
				It("rejects a shell metacharacter in "+field.name+": "+value, func() {
					ns := freshNS(fmt.Sprintf("cel-charset-%d-%d", fi, vi))
					p := platformIn(ns)
					field.set(p, value)
					err := k8sClient.Create(ctx, p)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring(testPatternRejection))
				})
			}
			It("accepts a conventional value for "+field.name, func() {
				ns := freshNS(fmt.Sprintf("cel-charset-good-%d", fi))
				p := platformIn(ns)
				field.set(p, field.good)
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
			})
		}
		It("accepts an IPv6 literal as the broker and database host", func() {
			ns := freshNS("cel-charset-ipv6")
			p := platformIn(ns)
			p.Spec.Messaging.Host = "[fd00::1]"
			p.Spec.Database.Host = "[fd00::2]"
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a wildcard public host on the edge and common hostName", func() {
			// Ingress rules and Gateway API listeners both accept a leading "*." label, so the
			// charset guard must not reject the wildcard form a real edge may legitimately serve.
			ns := freshNS("cel-charset-wildcard")
			p := platformIn(ns)
			p.Spec.Common.HostName = "*.example.com"
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress", Host: "*.apps.example.com"}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects a wildcard star outside the leading label", func() {
			ns := freshNS("cel-charset-wildcard-bad")
			p := platformIn(ns)
			p.Spec.Common.HostName = "app.*.example.com"
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testPatternRejection))
		})
		It("accepts an admin identity carrying an apostrophe and unicode (no pattern applies)", func() {
			ns := freshNS("cel-charset-admin-apostrophe")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:  true,
				Username: "o'brien",
				Name:     "Séamus",
				LastName: "O'Brien",
				Email:    "seamus.o'brien@example.com",
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	// managedMessaging satisfies messaging.mode=managed's required managed block, used by
	// the migration-control field cases below (drainTimeout and forceCutoverForVersion both
	// live under messaging.managed).
	managedMessaging := func() otilmv1alpha1.MessagingSpec {
		return otilmv1alpha1.MessagingSpec{
			Mode: "managed", BrokerType: "rabbitmq",
			Managed: &otilmv1alpha1.ManagedMessagingSpec{
				Replicas: 1, Version: "4.0", Storage: otilmv1alpha1.StorageSpec{Size: "20Gi"},
			},
		}
	}

	// messaging.managed.drainTimeout is a *metav1.Duration: the typed Go client can never
	// construct an invalid raw value for it (metav1.Duration only ever holds an
	// already-parsed time.Duration), so its charset is exercised at the unstructured/raw
	// level — exactly what the apiserver sees on the wire — by converting a valid typed
	// Platform and overwriting the field with a raw string.
	Context("messaging managed drainTimeout duration charset", func() {
		withDrainTimeout := func(ns, drainTimeout string) *unstructured.Unstructured {
			p := platformIn(ns)
			p.Spec.Messaging = managedMessaging()
			m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(m, drainTimeout, "spec", "messaging", "managed", "drainTimeout")).To(Succeed())
			u := &unstructured.Unstructured{Object: m}
			u.SetGroupVersionKind(otilmv1alpha1.GroupVersion.WithKind("Platform"))
			return u
		}
		It("accepts a conventional Go duration string", func() {
			ns := freshNS("cel-drain-good")
			Expect(k8sClient.Create(ctx, withDrainTimeout(ns, "15m"))).To(Succeed())
		})
		It("rejects a non-duration string", func() {
			ns := freshNS("cel-drain-bad")
			err := k8sClient.Create(ctx, withDrainTimeout(ns, "abc"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("drainTimeout must be a Go duration string"))
		})
	})

	// forceCutoverForVersion and migrationAcknowledgedForVersion are both target-scoped
	// version strings rendered into migration bookkeeping, so they carry the same charset
	// discipline as the exchange/routingKey fields above: a shell metacharacter can never
	// be STORED.
	Context("messaging migration version-shaped fields charset", func() {
		fields := []struct {
			name string
			set  func(*otilmv1alpha1.Platform, string)
		}{
			{
				name: "messaging.migrationAcknowledgedForVersion",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Messaging.MigrationAcknowledgedForVersion = v
				},
			},
			{
				name: "messaging.managed.forceCutoverForVersion",
				set: func(p *otilmv1alpha1.Platform, v string) {
					p.Spec.Messaging = managedMessaging()
					p.Spec.Messaging.Managed.ForceCutoverForVersion = v
				},
			},
		}
		badValues := []string{"$(id)", "`id`", `a"b`, "a\nb", "a$VAR", "a;b", "a|b", "a b"}
		for fi, f := range fields {
			field := f
			for vi, bad := range badValues {
				value := bad
				It("rejects a shell metacharacter in "+field.name+": "+value, func() {
					ns := freshNS(fmt.Sprintf("cel-migver-bad-%d-%d", fi, vi))
					p := platformIn(ns)
					field.set(p, value)
					err := k8sClient.Create(ctx, p)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring(testPatternRejection))
				})
			}
			It("accepts a conventional version value for "+field.name, func() {
				ns := freshNS(fmt.Sprintf("cel-migver-good-%d", fi))
				p := platformIn(ns)
				field.set(p, "2.19.0")
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
			})
		}
	})

	// status.upgrade.phase is a status-subresource field, so it is only reachable through
	// the /status subresource — a plain Create ignores status content entirely.
	Context("status.upgrade phase enum", func() {
		newUpgrade := func(phase otilmv1alpha1.MigrationPhase) *otilmv1alpha1.UpgradeStatus {
			now := metav1.Now()
			return &otilmv1alpha1.UpgradeStatus{
				FromVersion: "2.18.0", ToVersion: "2.19.0",
				Phase: phase, StartedAt: now, PhaseStartedAt: now,
			}
		}
		It("rejects a bogus migration phase on the status subresource", func() {
			ns := freshNS("cel-upgrade-phase-bad")
			p := platformIn(ns)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Upgrade = newUpgrade("Bogus")
			err := k8sClient.Status().Update(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})
		It("accepts a valid migration phase on the status subresource", func() {
			ns := freshNS("cel-upgrade-phase-good")
			p := platformIn(ns)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Upgrade = newUpgrade(otilmv1alpha1.MigrationPhaseFencing)
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		})
	})
})
