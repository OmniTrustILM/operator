/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// testExternalIssuer* reference an external cert-manager issuer (AWS Private CA) whose
	// kind is its own rather than Issuer or ClusterIssuer.
	testExternalIssuerName  = "aws-pca"
	testExternalIssuerKind  = "AWSPCAClusterIssuer"
	testExternalIssuerGroup = "awspca.cert-manager.io"
	// testIssuerKindRejection is the issuerRef kind/group rule's message.
	testIssuerKindRejection = "issuerRef.kind must be Issuer or ClusterIssuer unless issuerRef.group names an external issuer"
	// testMCP*Rejection are the messages of the three mcp rules on PlatformSpec.
	testMCPKeycloakRejection = "mcp.enabled requires keycloak.mode=managed"
	testMCPHostRejection     = "mcp.enabled needs a public host: set common.hostName, or edge.host on an enabled edge"
	testMCPVersionRejection  = "mcp.enabled requires keycloak.managed.version 26.8.1 or later"
	// testMCPEnumRejection is the fragment the apiserver includes when mcp.access is not one of
	// its enumerated levels.
	testMCPEnumRejection = `Unsupported value: "write"`
	// testMCPIdentity*Rejection are the messages of the mcp.identity rules: the settings external mode
	// requires, the settings only external mode takes, the URL shapes, the delegation group only managed
	// mode takes, the platform's own Keycloak, which only managed mode serves, and the name and key of
	// the client Secret, which the MCP Deployment's secretKeyRef takes only in Kubernetes' own form.
	testMCPIdentityRequiredRejection = "identity.mode external requires issuer, jwkSetUri, tokenUri, clientId, clientSecretRef and exchangeMode"
	testMCPIdentityManagedRejection  = "identity settings other than mode are used only with identity.mode external"
	testMCPIssuerRejection           = "issuer must be an https URL without userinfo, query or fragment"
	testMCPBackchannelRejection      = "jwkSetUri and tokenUri may use http only for a Kubernetes Service host (.svc), and take no userinfo, fragment or whitespace"
	testMCPGroupRejection            = "mcp.delegation.group is used only with identity.mode managed"
	testMCPOwnKeycloakRejection      = "identity.mode external needs a provider the platform does not manage; use identity.mode managed for the platform's Keycloak"
	testMCPSecretNameRejection       = "clientSecretRef must be a Secret name: lowercase letters, digits, '-' and '.', each part starting and ending with a letter or digit"
	testMCPSecretKeyRejection        = "clientSecretKey must be a Secret key: letters, digits, '-', '_' and '.', never '.' alone or starting with '..'"
	// testMCPOwnedSecretRejection is the message of the rule on Platform itself that keeps an external
	// identity off <platform>-mcp-client, the Secret the operator owns for the managed identity. The specs
	// create a platform named ilm, whose Secret is testMCPOwnedSecretName. testMCPOtherPlatform is another
	// platform's name, and testMCPOtherSecretName its Secret.
	testMCPOwnedSecretRejection = "identity.clientSecretRef must not name <platform>-mcp-client, the Secret the operator owns for the managed identity"
	testMCPOwnedSecretName      = "ilm-mcp-client"
	testMCPOtherPlatform        = "other"
	testMCPOtherSecretName      = testMCPOtherPlatform + "-mcp-client"
	// testMCPUnsupportedRejection, testMCPTooLongRejection and testMCPTooManyRejection are the fragments
	// the apiserver includes when a value is not in its enum, a string outgrows its length or a list
	// outgrows its item count.
	testMCPUnsupportedRejection = "Unsupported value"
	testMCPTooLongRejection     = "Too long"
	testMCPTooManyRejection     = "Too many"
	// testMCPIdentity* are the settings of a valid external identity: a provider whose public issuer is on
	// another host than the platform's, and whose key-set and token endpoints MCP reaches over plain http
	// at an in-cluster Service. testMCPIdentityEdgeHost is an edge host other than the platform's.
	testMCPIdentityIssuer    = "https://idp.example.com/realms/corp"
	testMCPIdentityInCluster = "http://kc.idp.svc:8080/realms/corp"
	testMCPIdentityCertsPath = "/protocol/openid-connect/certs"
	testMCPIdentityTokenPath = "/protocol/openid-connect/token"
	testMCPIdentityClientID  = "ilm-mcp"
	testMCPIdentitySecretRef = "mcp-idp-client"
	testMCPIdentityEdgeHost  = "edge.example.com"
	// testHTTPSScheme starts the issuer URLs the specs build, and testMCPOwnRealmPath is a realm under
	// the path the platform serves its managed Keycloak from. It is not the platform's own realm, ilm:
	// every realm under that path is the platform's.
	testHTTPSScheme     = "https://"
	testMCPOwnRealmPath = "/kc/realms/ILM"
	// testMCPOldKeycloakVersion is a pinned managed Keycloak version below the 26.8.1 floor that the
	// managed identity needs.
	testMCPOldKeycloakVersion = "26.6.3"
	// testRuleEvaluationError is the fragment the apiserver includes when a CEL rule fails by
	// erroring at runtime, such as on a field that is absent, rather than by evaluating to false.
	testRuleEvaluationError = "evaluating rule"
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

	Context("issuerRef kind", func() {
		issuerRefEdge := func(ref *otilmv1alpha1.CertManagerIssuerRef) *otilmv1alpha1.EdgeSpec {
			return &otilmv1alpha1.EdgeSpec{
				Enabled: true, Type: "ingress", Host: testEdgeHost,
				TLS: &otilmv1alpha1.EdgeTLSSpec{Source: "issuerRef", IssuerRef: ref},
			}
		}
		externalIssuerRef := func() *otilmv1alpha1.CertManagerIssuerRef {
			return &otilmv1alpha1.CertManagerIssuerRef{Name: testExternalIssuerName, Kind: testExternalIssuerKind, Group: testExternalIssuerGroup}
		}
		// withEmptyIssuerRefField sends the edge issuerRef with one field explicitly empty on
		// the wire: the typed client omits an empty string, and the CRD default would fill it.
		withEmptyIssuerRefField := func(ns string, ref *otilmv1alpha1.CertManagerIssuerRef, field string) *unstructured.Unstructured {
			p := platformIn(ns)
			p.Spec.Edge = issuerRefEdge(ref)
			m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedField(m, "", "spec", "edge", "tls", "issuerRef", field)).To(Succeed())
			u := &unstructured.Unstructured{Object: m}
			u.SetGroupVersionKind(otilmv1alpha1.GroupVersion.WithKind("Platform"))
			return u
		}
		It("accepts an external issuer's own kind on the edge", func() {
			ns := freshNS("cel-ir-external")
			p := platformIn(ns)
			p.Spec.Edge = issuerRefEdge(externalIssuerRef())
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts an external issuer's own kind for a generated admin certificate", func() {
			ns := freshNS("cel-ir-admin-external")
			p := platformIn(ns)
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
				Enabled:     true,
				Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: ptr(true), Source: "generated", IssuerRef: externalIssuerRef()},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects a kind other than Issuer or ClusterIssuer in the cert-manager.io group", func() {
			ns := freshNS("cel-ir-bad-kind")
			p := platformIn(ns)
			p.Spec.Edge = issuerRefEdge(&otilmv1alpha1.CertManagerIssuerRef{Name: testExternalIssuerName, Kind: testExternalIssuerKind})
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testIssuerKindRejection))
		})
		It("rejects an external kind with an empty group", func() {
			ns := freshNS("cel-ir-empty-group")
			ref := &otilmv1alpha1.CertManagerIssuerRef{Name: testExternalIssuerName, Kind: testExternalIssuerKind}
			err := k8sClient.Create(ctx, withEmptyIssuerRefField(ns, ref, "group"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(testIssuerKindRejection))
		})
		It("rejects an empty kind for an external group", func() {
			ns := freshNS("cel-ir-empty-kind")
			err := k8sClient.Create(ctx, withEmptyIssuerRefField(ns, externalIssuerRef(), "kind"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("should be at least 1 chars long"))
		})
		It("accepts a cert-manager ClusterIssuer", func() {
			ns := freshNS("cel-ir-cluster")
			p := platformIn(ns)
			p.Spec.Edge = issuerRefEdge(&otilmv1alpha1.CertManagerIssuerRef{Name: "corp-ca", Kind: "ClusterIssuer"})
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
				// %q keeps the spec name single-line: badValues includes a literal newline.
				It(fmt.Sprintf("rejects a shell metacharacter in %s: %q", field.name, value), func() {
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
				// %q keeps the spec name single-line: badValues includes a literal newline.
				It(fmt.Sprintf("rejects a shell metacharacter in %s: %q", field.name, value), func() {
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

	Context("core instanceId guards", func() {
		It("rejects instanceId together with multi-replica core", func() {
			ns := freshNS("cel-instanceid-replicas")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(7))
			p.Spec.Core.Replicas = ptr(int32(2))
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("core.instanceId requires a single-replica core"))
		})
		It("rejects instanceId together with core autoscaling", func() {
			ns := freshNS("cel-instanceid-hpa")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(7))
			p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 3}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("core.instanceId cannot be combined with core.autoscaling"))
		})
		It("rejects instanceId under the HA profile without an explicit single replica", func() {
			ns := freshNS("cel-instanceid-ha")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(7))
			p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("core.instanceId with highAvailability.enabled"))
		})
		It("accepts instanceId under the HA profile when core is pinned to one replica", func() {
			ns := freshNS("cel-instanceid-ha-pinned")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(7))
			p.Spec.Core.Replicas = ptr(int32(1))
			p.Spec.HighAvailability = &otilmv1alpha1.HighAvailabilitySpec{Enabled: true}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a single-replica core with an instanceId", func() {
			ns := freshNS("cel-instanceid-good")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(65535))
			p.Spec.Core.Replicas = ptr(int32(1))
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("rejects an instanceId outside 0-65535", func() {
			ns := freshNS("cel-instanceid-range")
			p := platformIn(ns)
			p.Spec.Core.InstanceID = ptr(int32(65536))
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("should be less than or equal to 65535"))
		})
		It("accepts a CR that sets no instanceId at all (existing CRs are never wedged)", func() {
			ns := freshNS("cel-instanceid-absent")
			p := platformIn(ns)
			p.Spec.Core.Replicas = ptr(int32(3))
			p.Spec.Core.Autoscaling = &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 5}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	Context("time-quality monitor credentials", func() {
		It("rejects an enabled monitor on an external broker without a credentials secretRef", func() {
			ns := freshNS("cel-tqm-external-bad")
			p := platformIn(ns)
			p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{Enabled: true}
			err := k8sClient.Create(ctx, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("core.timeQualityMonitor.credentials.secretRef is required"))
		})
		It("accepts an enabled monitor on an external broker with a credentials secretRef", func() {
			ns := freshNS("cel-tqm-external-good")
			p := platformIn(ns)
			p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
				Enabled:     true,
				Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "tq-monitor"},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts an enabled monitor on a managed broker with no secretRef", func() {
			ns := freshNS("cel-tqm-managed")
			p := platformIn(ns)
			p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
				Mode: "managed", BrokerType: "rabbitmq",
				Managed: &otilmv1alpha1.ManagedMessagingSpec{
					Replicas: 1,
					Storage:  otilmv1alpha1.StorageSpec{Size: "1Gi"},
				},
			}
			p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a DISABLED monitor on an external broker with no secretRef", func() {
			ns := freshNS("cel-tqm-off")
			p := platformIn(ns)
			p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{Enabled: false}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
	})

	// mcp.enabled depends on three facts that live in OTHER sections of the spec, so its rules sit
	// on PlatformSpec: a managed Keycloak (the operator provisions MCP's Keycloak objects through
	// the admin API), a public host (MCP's resource URL is built from it) and, when the managed
	// Keycloak is pinned, a version of 26.8.1 or later. The Keycloak and its version matter only
	// while spec.mcp.identity is managed, which an absent identity is. mcpPlatform satisfies all
	// three and each spec breaks exactly one, so a rejection is attributable to that rule.
	Context("mcp", func() {
		mcpPlatform := func(ns string) *otilmv1alpha1.Platform {
			p := platformIn(ns)
			p.Spec.Keycloak = managedKeycloak()
			p.Spec.Common.HostName = testEdgeHost
			p.Spec.MCP = otilmv1alpha1.MCPSpec{Enabled: true}
			return p
		}
		// pinnedTo is mcpPlatform with the managed Keycloak pinned to version.
		pinnedTo := func(ns, version string) *otilmv1alpha1.Platform {
			p := mcpPlatform(ns)
			p.Spec.Keycloak.Managed.Version = version
			return p
		}
		// rejects asserts that the apiserver refuses obj with a message containing fragment, and that
		// no rule failed by erroring: a rule that dereferences an absent field reports "evaluating
		// rule" instead of passing or failing cleanly.
		rejects := func(obj client.Object, fragment string) {
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(fragment))
			Expect(err.Error()).NotTo(ContainSubstring(testRuleEvaluationError))
		}
		// onTheWire returns p as the apiserver sees it once edit has changed it. The typed client
		// always sends the common and mcp blocks and omits an empty string, so a field that must be
		// absent or explicitly empty on the wire can only be set here.
		onTheWire := func(p *otilmv1alpha1.Platform, edit func(obj map[string]interface{})) *unstructured.Unstructured {
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
			Expect(err).NotTo(HaveOccurred())
			edit(obj)
			u := &unstructured.Unstructured{Object: obj}
			u.SetGroupVersionKind(otilmv1alpha1.GroupVersion.WithKind("Platform"))
			return u
		}

		It("rejects MCP with an external Keycloak", func() {
			ns := freshNS("cel-mcp-extkc")
			p := mcpPlatform(ns)
			p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
			rejects(p, testMCPKeycloakRejection)
		})
		It("rejects MCP with no keycloak block (external by default)", func() {
			ns := freshNS("cel-mcp-nokc")
			p := mcpPlatform(ns)
			p.Spec.Keycloak = nil
			rejects(p, testMCPKeycloakRejection)
		})
		It("rejects MCP with no public host", func() {
			ns := freshNS("cel-mcp-nohost")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			rejects(p, testMCPHostRejection)
		})
		It("rejects MCP with no common block and no edge on the wire", func() {
			ns := freshNS("cel-mcp-nocommon")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			rejects(onTheWire(p, func(obj map[string]interface{}) {
				unstructured.RemoveNestedField(obj, "spec", "common")
			}), testMCPHostRejection)
		})
		// The public host is the one the platform resolves: the host of an ENABLED edge, otherwise
		// common.hostName. The host of a disabled edge is ignored by that resolution, so it must not
		// satisfy the rule, whether enabled is false or never said.
		It("rejects MCP when only a disabled edge names a host", func() {
			ns := freshNS("cel-mcp-edgeoff")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Type: "ingress", Host: testEdgeHost}
			rejects(onTheWire(p, func(obj map[string]interface{}) {
				Expect(unstructured.SetNestedField(obj, false, "spec", "edge", "enabled")).To(Succeed())
			}), testMCPHostRejection)
		})
		It("rejects MCP when only an edge that never says enabled names a host", func() {
			ns := freshNS("cel-mcp-edgeunset")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Type: "ingress", Host: testEdgeHost} // enabled is omitted on the wire
			rejects(p, testMCPHostRejection)
		})
		It("rejects MCP when an enabled edge names no host and there is no common.hostName", func() {
			ns := freshNS("cel-mcp-edgenohost")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress"} // the edge's own rule rejects it too
			rejects(p, testMCPHostRejection)
		})
		It("accepts a public host that comes from an enabled edge (no common.hostName)", func() {
			ns := freshNS("cel-mcp-edgehost")
			p := mcpPlatform(ns)
			p.Spec.Common.HostName = ""
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: true, Type: "ingress", Host: testEdgeHost}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts common.hostName behind a disabled edge (the platform's own ingress)", func() {
			ns := freshNS("cel-mcp-ownedge")
			p := mcpPlatform(ns) // common.hostName is set
			p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})

		// 26.8.0 and 26.6.3 are older 26.x releases, 25.0.1 an older major. 26.8 names no patch, so it
		// cannot be shown to be 26.8.1 or later, and a Keycloak image build suffix does not lift a
		// patch below 26.8.1. A tag is a bare version: a leading v is not one, and 270 is not 27.
		for i, version := range []string{"26.8.0", testMCPOldKeycloakVersion, "25.0.1", "26.8", "26.8.0-0", "v26.8.1", "270.0.0"} {
			It("rejects a managed Keycloak pinned to "+version, func() {
				rejects(pinnedTo(freshNS(fmt.Sprintf("cel-mcp-oldkc-%d", i)), version), testMCPVersionRejection)
			})
		}
		// 26.8.1 is the floor and 26.8.99 the top of its patch range. 26.9 names no patch but is past
		// the floor, and 26.8.1-0 is the build-suffixed tag form Keycloak publishes.
		for i, version := range []string{"", "26.8.1", "26.8.99", "26.8.1-0", "26.9", "26.10.0", "27.0.0"} {
			name := "pinned to " + version
			if version == "" {
				name = "left unpinned"
			}
			It("accepts MCP with a managed Keycloak and a host, "+name, func() {
				Expect(k8sClient.Create(ctx, pinnedTo(freshNS(fmt.Sprintf("cel-mcp-newkc-%d", i)), version))).To(Succeed())
			})
		}
		It("accepts a managed Keycloak whose version is explicitly empty", func() {
			ns := freshNS("cel-mcp-emptyversion")
			u := onTheWire(mcpPlatform(ns), func(obj map[string]interface{}) {
				Expect(unstructured.SetNestedField(obj, "", "spec", "keycloak", "managed", "version")).To(Succeed())
			})
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
		})

		It("accepts MCP off with an external Keycloak and no host", func() {
			ns := freshNS("cel-mcp-off")
			p := platformIn(ns)
			p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
			p.Spec.MCP = otilmv1alpha1.MCPSpec{Enabled: false}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts MCP off with a managed Keycloak pinned below 26.8.1", func() {
			ns := freshNS("cel-mcp-off-oldkc")
			p := pinnedTo(ns, "25.0.1")
			p.Spec.MCP.Enabled = false
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts a platform with no mcp block at all", func() {
			ns := freshNS("cel-mcp-absent")
			u := onTheWire(platformIn(ns), func(obj map[string]interface{}) {
				unstructured.RemoveNestedField(obj, "spec", "mcp")
			})
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
		})
		// The version rule is a disjunction, and CEL lets one true operand decide it even when another cannot be
		// evaluated. Its guard against an absent mcp block therefore matters only when every other operand is false,
		// which a managed Keycloak pinned below 26.8.1 does: an absent or unpinned Keycloak would pass without it.
		It("accepts a managed Keycloak pinned below 26.8.1 when there is no mcp block", func() {
			ns := freshNS("cel-mcp-absent-oldkc")
			p := platformIn(ns)
			p.Spec.Keycloak = managedKeycloak()
			p.Spec.Keycloak.Managed.Version = testMCPOldKeycloakVersion
			u := onTheWire(p, func(obj map[string]interface{}) {
				unstructured.RemoveNestedField(obj, "spec", "mcp")
			})
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
		})

		It("rejects access: write", func() {
			ns := freshNS("cel-mcp-access-write")
			p := mcpPlatform(ns)
			p.Spec.MCP.Access = "write"
			rejects(p, testMCPEnumRejection)
		})
		for i, level := range []string{"read-only", "read-write", "full"} {
			It("accepts the "+level+" access level", func() {
				ns := freshNS(fmt.Sprintf("cel-mcp-access-%d", i))
				p := mcpPlatform(ns)
				p.Spec.MCP.Access = level
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
			})
		}
		It("rejects a delegation group written as a path", func() {
			ns := freshNS("cel-mcp-grouppath")
			p := mcpPlatform(ns)
			p.Spec.MCP.Delegation.Group = "/ilm/mcp-users"
			rejects(p, testPatternRejection)
		})
		It("rejects a delegation group name longer than 255 characters", func() {
			ns := freshNS("cel-mcp-grouplong")
			p := mcpPlatform(ns)
			p.Spec.MCP.Delegation.Group = strings.Repeat("g", 256)
			rejects(p, testPatternRejection)
		})
		It("accepts a delegation group name of 255 characters", func() {
			ns := freshNS("cel-mcp-groupmax")
			p := mcpPlatform(ns)
			p.Spec.MCP.Delegation.Group = strings.Repeat("g", 255)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("accepts an explicitly empty delegation group", func() {
			ns := freshNS("cel-mcp-groupempty")
			u := onTheWire(mcpPlatform(ns), func(obj map[string]interface{}) {
				Expect(unstructured.SetNestedField(obj, "", "spec", "mcp", "delegation", "group")).To(Succeed())
			})
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
		})
		It("accepts a delegation group name and CIMD switched off", func() {
			ns := freshNS("cel-mcp-group")
			p := mcpPlatform(ns)
			p.Spec.MCP.CIMD.Enabled = ptr(false)
			p.Spec.MCP.Delegation.Group = "mcp users"
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
		})
		It("defaults MCP to off, read-only and CIMD on", func() {
			ns := freshNS("cel-mcp-defaults")
			Expect(k8sClient.Create(ctx, platformIn(ns))).To(Succeed())
			got := &otilmv1alpha1.Platform{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ilm"}, got)).To(Succeed())
			Expect(got.Spec.MCP.Enabled).To(BeFalse())
			Expect(got.Spec.MCP.Access).To(Equal("read-only"))
			Expect(got.Spec.MCP.CIMD.Enabled).To(HaveValue(BeTrue()))
		})

		// spec.mcp.identity selects where MCP signs its users in. Its rules read the identity block and
		// the delegation group, Keycloak and host around it. Each case starts from externalPlatform, an
		// external identity that satisfies every rule on mcpPlatform's managed Keycloak and host, and
		// edits it. reject is the message the apiserver must answer with, and is empty for a Platform it
		// must admit; rejects also fails a rule that errors on an absent field instead of failing cleanly.
		Context("identity", func() {
			type identityCase struct {
				name   string
				edit   func(p *otilmv1alpha1.Platform)
				reject string
			}
			// urlSetting is a URL setting and the message of the rule that owns it.
			type urlSetting struct {
				name   string
				set    func(id *otilmv1alpha1.MCPIdentitySpec, url string)
				reject string
			}
			// lengthCase is a string setting and its declared length. url marks a setting that holds a URL.
			type lengthCase struct {
				name  string
				set   func(id *otilmv1alpha1.MCPIdentitySpec, value string)
				limit int
				url   bool
			}
			// valueCase is a value of a setting, and the case that names it.
			type valueCase struct{ name, value string }
			externalPlatform := func(ns string) *otilmv1alpha1.Platform {
				p := mcpPlatform(ns)
				p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{
					Mode:            "external",
					Issuer:          testMCPIdentityIssuer,
					JWKSetURI:       testMCPIdentityInCluster + testMCPIdentityCertsPath,
					TokenURI:        testMCPIdentityInCluster + testMCPIdentityTokenPath,
					ClientID:        testMCPIdentityClientID,
					ClientSecretRef: testMCPIdentitySecretRef,
					ExchangeMode:    "delegation",
				}
				return p
			}
			// onIdentity edits the identity block of an external identity.
			onIdentity := func(edit func(id *otilmv1alpha1.MCPIdentitySpec)) func(p *otilmv1alpha1.Platform) {
				return func(p *otilmv1alpha1.Platform) { edit(p.Spec.MCP.Identity) }
			}
			// managed replaces the identity with managed mode, edited by edit.
			managed := func(edit func(id *otilmv1alpha1.MCPIdentitySpec)) func(p *otilmv1alpha1.Platform) {
				return func(p *otilmv1alpha1.Platform) {
					p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{Mode: "managed"}
					edit(p.Spec.MCP.Identity)
				}
			}
			// modeOnly is the edit of a managed identity that sets nothing besides its mode.
			modeOnly := func(*otilmv1alpha1.MCPIdentitySpec) {
				// The identity block holds the mode managed sets and nothing else.
			}
			// externalKeycloak makes the platform's Keycloak one the operator does not provision.
			externalKeycloak := func(p *otilmv1alpha1.Platform) {
				p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"}
			}
			// edgeOn is an edge that names host, enabled or not.
			edgeOn := func(host string, enabled bool) func(p *otilmv1alpha1.Platform) {
				return func(p *otilmv1alpha1.Platform) {
					p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: enabled, Type: "ingress", Host: host}
				}
			}
			// issuerUnder points the issuer under host's /kc/ path, where the platform serves its managed
			// Keycloak, after the edits have changed the Platform. The realm is not the Platform's ilm:
			// every realm under that path is the platform's.
			issuerUnder := func(host string, edits ...func(p *otilmv1alpha1.Platform)) func(p *otilmv1alpha1.Platform) {
				return func(p *otilmv1alpha1.Platform) {
					for _, edit := range edits {
						edit(p)
					}
					p.Spec.MCP.Identity.Issuer = testHTTPSScheme + host + testMCPOwnRealmPath
				}
			}
			// longURL is an https URL of n characters.
			longURL := func(n int) string {
				prefix := testMCPIdentityIssuer + "/"
				return prefix + strings.Repeat("a", n-len(prefix))
			}

			// The settings external mode requires, and those that only external mode takes.
			required := []struct {
				name  string
				clear func(id *otilmv1alpha1.MCPIdentitySpec)
			}{
				{"issuer", func(id *otilmv1alpha1.MCPIdentitySpec) { id.Issuer = "" }},
				{"jwkSetUri", func(id *otilmv1alpha1.MCPIdentitySpec) { id.JWKSetURI = "" }},
				{"tokenUri", func(id *otilmv1alpha1.MCPIdentitySpec) { id.TokenURI = "" }},
				{"clientId", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientID = "" }},
				{"clientSecretRef", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretRef = "" }},
				{"exchangeMode", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ExchangeMode = "" }},
			}
			externalOnly := []struct {
				name string
				set  func(id *otilmv1alpha1.MCPIdentitySpec)
			}{
				{"issuer", func(id *otilmv1alpha1.MCPIdentitySpec) { id.Issuer = testMCPIdentityIssuer }},
				{"jwkSetUri", func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.JWKSetURI = testMCPIdentityIssuer + testMCPIdentityCertsPath
				}},
				{"tokenUri", func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.TokenURI = testMCPIdentityIssuer + testMCPIdentityTokenPath
				}},
				{"clientId", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientID = testMCPIdentityClientID }},
				{"clientSecretRef", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretRef = testMCPIdentitySecretRef }},
				{"clientSecretKey", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretKey = "client-secret" }},
				{"exchangeMode", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ExchangeMode = "standard" }},
				{"exchangeScope", func(id *otilmv1alpha1.MCPIdentitySpec) { id.ExchangeScope = "openid ilm-mcp-upstream" }},
				{"requiredScope", func(id *otilmv1alpha1.MCPIdentitySpec) { id.RequiredScope = "mcp:access" }},
				{"authorizationScopes", func(id *otilmv1alpha1.MCPIdentitySpec) { id.AuthorizationScopes = []string{"openid"} }},
				{"apiAudience", func(id *otilmv1alpha1.MCPIdentitySpec) { id.APIAudience = "ilm" }},
			}
			issuer := urlSetting{"issuer", func(id *otilmv1alpha1.MCPIdentitySpec, url string) { id.Issuer = url }, testMCPIssuerRejection}
			backchannel := []urlSetting{
				{"jwkSetUri", func(id *otilmv1alpha1.MCPIdentitySpec, url string) { id.JWKSetURI = url }, testMCPBackchannelRejection},
				{"tokenUri", func(id *otilmv1alpha1.MCPIdentitySpec, url string) { id.TokenURI = url }, testMCPBackchannelRejection},
			}
			// Every URL setting refuses these.
			malformed := []struct{ why, url string }{
				{"userinfo", "https://u@idp.example.com/x"},
				{"a fragment", "https://idp.example.com/x#f"},
				{"a scheme other than http and https", "ftp://idp.example.com/x"},
				{"no scheme", "idp.example.com/x"},
				{"no host", "https:///x"},
				{"a space in the host", "https://idp example.com/x"},
				{"a space in the path", "https://idp.example.com/a b"},
			}
			// A key-set or token URL may use http only for a Service host, <service>.<namespace>.svc
			// optionally followed by the cluster domain. The issuer may not use http at all.
			plainHTTP := []struct{ why, url string }{
				{"a public host", "http://idp.example.com/certs"},
				{"a host that is no Service", "http://kc.idp:8080/certs"},
				{"userinfo on a Service host", "http://u@kc.idp.svc:8080/certs"},
				{"a fragment on a Service host", "http://kc.idp.svc:8080/certs#f"},
				{"a svc label under a public domain", "http://a.svc.example.com/certs"},
				{"a Service name with no namespace", "http://a.svc/certs"},
				{"svc as the Service name, with no namespace", "http://svc.svc/c"},
			}
			// The Service hosts a key-set or token URL may use over http.
			serviceHosts := []struct{ why, url string }{
				{"a Service host with no port or path", "http://kc.idp.svc"},
				{"a Service host with the default cluster domain", "http://kc.idp.svc.cluster.local:8080/t"},
				{"a Service host with a custom cluster domain", "http://kc.idp.svc.k8s.corp.example.com/c"},
				{"a pod of a headless Service", "http://pod-0.kc.idp.svc.cluster.local/c"},
			}
			// The client Secret is read through the MCP Deployment's secretKeyRef. Kubernetes refuses that
			// reference unless the name is a DNS-1123 subdomain and the key is made of letters, digits, '-',
			// '_' and '.', and is neither '.' nor starts with '..'. The Deployment's apply then fails and
			// degrades the whole platform, so admission refuses what Kubernetes would.
			badSecretNames := []valueCase{
				{"a clientSecretRef with an uppercase letter and an underscore", "MCP_Client"},
				{"an uppercase clientSecretRef", "Mcp-Idp-Client"},
				{"a clientSecretRef with a slash", "a/b"},
				{"a clientSecretRef with a space", "mcp idp client"},
				{"a clientSecretRef that starts with a hyphen", "-mcp-idp-client"},
				{"a clientSecretRef that ends with a hyphen", "mcp-idp-client-"},
				{"a clientSecretRef that starts with a dot", ".mcp-idp-client"},
				{"a clientSecretRef that ends with a dot", "mcp-idp-client."},
				{"a clientSecretRef with an empty part", "mcp..idp-client"},
				{"a clientSecretRef with a part that starts with a hyphen", "mcp.-idp-client"},
				{"a clientSecretRef with a part that ends with a hyphen", "mcp-.idp-client"},
			}
			goodSecretNames := []valueCase{
				{"a clientSecretRef of lowercase words joined by hyphens", "mcp-idp-client"},
				{"a clientSecretRef of dotted parts", "idp.client-1"},
				{"a clientSecretRef of one character", "a"},
				{"a clientSecretRef that starts with a digit", "1mcp"},
			}
			badSecretKeys := []valueCase{
				{"a clientSecretKey with a space", "client secret"},
				{"a clientSecretKey with a slash", "a/b"},
				{"a clientSecretKey with a colon", "client:secret"},
				{"a clientSecretKey of one dot", "."},
				{"a clientSecretKey of two dots", ".."},
				{"a clientSecretKey that starts with two dots", "..data"},
			}
			goodSecretKeys := []valueCase{
				{"a clientSecretKey with a hyphen", "client-secret"},
				{"a clientSecretKey with an underscore", "client_secret"},
				{"a clientSecretKey with uppercase letters and a dot", "CLIENT.secret"},
				{"a clientSecretKey that starts with one dot", ".secret"},
				{"a clientSecretKey with two dots inside", "a..b"},
			}
			// secretCases is a case for each value of a client Secret setting that admission refuses with
			// message, and for each that it takes.
			secretCases := func(set func(id *otilmv1alpha1.MCPIdentitySpec, value string), message string, refused, taken []valueCase) []identityCase {
				var out []identityCase
				add := func(v valueCase, reject string) {
					out = append(out, identityCase{
						name:   v.name,
						edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { set(id, v.value) }),
						reject: reject,
					})
				}
				for _, v := range refused {
					add(v, message)
				}
				for _, v := range taken {
					add(v, "")
				}
				return out
			}
			// Every string setting stops at its declared length, which the longest value fills.
			lengths := []lengthCase{
				{"issuer", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.Issuer = v }, 2048, true},
				{"jwkSetUri", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.JWKSetURI = v }, 2048, true},
				{"tokenUri", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.TokenURI = v }, 2048, true},
				{"clientId", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ClientID = v }, 255, false},
				{"clientSecretRef", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ClientSecretRef = v }, 253, false},
				{"clientSecretKey", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ClientSecretKey = v }, 253, false},
				{"exchangeScope", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ExchangeScope = v }, 1024, false},
				{"requiredScope", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.RequiredScope = v }, 255, false},
				{"apiAudience", func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.APIAudience = v }, 255, false},
			}
			valueOf := func(l lengthCase, n int) string {
				if l.url {
					return longURL(n)
				}
				return strings.Repeat("a", n)
			}
			// scopes is n authorization scopes of size characters.
			scopes := func(n, size int) []string {
				out := make([]string, n)
				for i := range out {
					out[i] = strings.Repeat("a", size)
				}
				return out
			}

			cases := []identityCase{
				// External identity is admitted whatever the Keycloak is, and whatever its version: the
				// operator provisions nothing at the provider.
				{name: "external identity on a managed Keycloak, with in-cluster http key-set and token URLs"},
				{name: "external identity on an external Keycloak, with https URLs", edit: func(p *otilmv1alpha1.Platform) {
					externalKeycloak(p)
					p.Spec.MCP.Identity.JWKSetURI = testMCPIdentityIssuer + testMCPIdentityCertsPath
					p.Spec.MCP.Identity.TokenURI = testMCPIdentityIssuer + testMCPIdentityTokenPath
				}},
				{name: "external identity with no keycloak block", edit: func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak = nil }},
				{name: "external identity on a managed Keycloak pinned below 26.8.1", edit: func(p *otilmv1alpha1.Platform) {
					p.Spec.Keycloak.Managed.Version = testMCPOldKeycloakVersion
				}},
				{name: "external identity with every setting", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					for _, s := range externalOnly {
						s.set(id)
					}
				})},
				{name: "external identity with the standard exchange mode", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.ExchangeMode = "standard"
				})},
				{name: "external identity without a public host", edit: func(p *otilmv1alpha1.Platform) { p.Spec.Common.HostName = "" },
					reject: testMCPHostRejection},

				// Managed identity, stated or implied, needs a managed Keycloak, at 26.8.1 or later when pinned.
				{name: "managed identity stated alone", edit: managed(modeOnly)},
				{name: "managed identity on an external Keycloak", edit: func(p *otilmv1alpha1.Platform) {
					managed(modeOnly)(p)
					externalKeycloak(p)
				}, reject: testMCPKeycloakRejection},
				{name: "an identity block that names no mode, on an external Keycloak", edit: func(p *otilmv1alpha1.Platform) {
					p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{}
					externalKeycloak(p)
				}, reject: testMCPKeycloakRejection},
				{name: "managed identity on a managed Keycloak pinned below 26.8.1", edit: func(p *otilmv1alpha1.Platform) {
					managed(modeOnly)(p)
					p.Spec.Keycloak.Managed.Version = testMCPOldKeycloakVersion
				}, reject: testMCPVersionRejection},

				// The delegation group belongs to managed mode: the operator creates it in Keycloak.
				{name: "external identity with a delegation group", edit: func(p *otilmv1alpha1.Platform) {
					p.Spec.MCP.Delegation.Group = testMCPDelegationGroup
				}, reject: testMCPGroupRejection},
				{name: "managed identity with a delegation group", edit: func(p *otilmv1alpha1.Platform) {
					managed(modeOnly)(p)
					p.Spec.MCP.Delegation.Group = testMCPDelegationGroup
				}},
				{name: "an identity block that names no mode, with a delegation group", edit: func(p *otilmv1alpha1.Platform) {
					p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{}
					p.Spec.MCP.Delegation.Group = testMCPDelegationGroup
				}},

				// The platform's own Keycloak is not a provider the platform does not manage. It is served
				// under /kc/ on the platform's host: the host of an enabled edge that names one, otherwise
				// common.hostName.
				{name: "an issuer under the platform's Keycloak path, on common.hostName",
					edit: issuerUnder(testEdgeHost), reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, on the host of an enabled edge",
					edit: issuerUnder(testMCPIdentityEdgeHost, edgeOn(testMCPIdentityEdgeHost, true)), reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, on the host of an enabled edge with no common.hostName",
					edit: issuerUnder(testMCPIdentityEdgeHost, edgeOn(testMCPIdentityEdgeHost, true),
						func(p *otilmv1alpha1.Platform) { p.Spec.Common.HostName = "" }),
					reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, on common.hostName behind an enabled edge that names no host",
					edit: issuerUnder(testEdgeHost, edgeOn("", true)), reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, on common.hostName behind a disabled edge",
					edit: issuerUnder(testEdgeHost, edgeOn(testMCPIdentityEdgeHost, false)), reject: testMCPOwnKeycloakRejection},
				// A host is the same in any case, and https has the port 443 whether the URL spells it or not.
				{name: "an issuer under the platform's Keycloak path, with common.hostName in another case",
					edit: issuerUnder(strings.ToUpper(testEdgeHost)), reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, with the port 443 spelled out",
					edit: issuerUnder(testEdgeHost + ":443"), reject: testMCPOwnKeycloakRejection},
				{name: "an issuer in lower case under the Keycloak path of a common.hostName in capitals",
					edit:   issuerUnder(testEdgeHost, func(p *otilmv1alpha1.Platform) { p.Spec.Common.HostName = strings.ToUpper(testEdgeHost) }),
					reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path, on the host of an enabled edge in another case and with the port 443",
					edit:   issuerUnder(strings.ToUpper(testMCPIdentityEdgeHost)+":443", edgeOn(testMCPIdentityEdgeHost, true)),
					reject: testMCPOwnKeycloakRejection},
				{name: "an issuer under the platform's Keycloak path on another port of common.hostName",
					edit: issuerUnder(testEdgeHost + ":8443")},
				{name: "an issuer under the platform's Keycloak path on a port that only starts with 443",
					edit: issuerUnder(testEdgeHost + ":4430")},
				{name: "an issuer under common.hostName's Keycloak path while an enabled edge names another host",
					edit: issuerUnder(testEdgeHost, edgeOn(testMCPIdentityEdgeHost, true))},
				{name: "an issuer under a disabled edge's host",
					edit: issuerUnder(testMCPIdentityEdgeHost, edgeOn(testMCPIdentityEdgeHost, false))},
				{name: "an issuer under the Keycloak path of the platform's host, on an external Keycloak",
					edit: issuerUnder(testEdgeHost, externalKeycloak)},
				{name: "an issuer under the Keycloak path of the platform's host, with no keycloak block",
					edit: issuerUnder(testEdgeHost, func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak = nil })},
				{name: "an issuer under the platform's Keycloak path while MCP is off",
					edit: issuerUnder(testEdgeHost, func(p *otilmv1alpha1.Platform) { p.Spec.MCP.Enabled = false })},
				{name: "an issuer on common.hostName outside its Keycloak path", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.Issuer = testHTTPSScheme + testEdgeHost + "/realms/corp"
				})},
				{name: "an issuer on a host that only begins with common.hostName", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.Issuer = testHTTPSScheme + testEdgeHost + ".idp.test/kc/realms/corp"
				})},
				{name: "an issuer on the host of an enabled edge outside its Keycloak path", edit: func(p *otilmv1alpha1.Platform) {
					edgeOn(testMCPIdentityEdgeHost, true)(p)
					p.Spec.MCP.Identity.Issuer = testHTTPSScheme + testMCPIdentityEdgeHost + "/realms/corp"
				}},
				{name: "an issuer on a host that only begins with the host of an enabled edge", edit: func(p *otilmv1alpha1.Platform) {
					edgeOn(testMCPIdentityEdgeHost, true)(p)
					p.Spec.MCP.Identity.Issuer = testHTTPSScheme + testMCPIdentityEdgeHost + ".idp.test/kc/realms/corp"
				}},

				// The operator relays <platform>-mcp-client for the managed identity and prunes it once the
				// platform leaves that identity, so an external identity may not read it. Only that exact name
				// is the operator's: a longer one, and the Secret of another platform, are not.
				{name: "an external identity that reads the Secret the operator owns for the managed identity",
					edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretRef = testMCPOwnedSecretName }),
					reject: testMCPOwnedSecretRejection},
				{name: "an external identity that reads the Secret the operator owns, on a platform that is not named ilm",
					edit: func(p *otilmv1alpha1.Platform) {
						p.Name = testMCPOtherPlatform
						p.Spec.MCP.Identity.ClientSecretRef = testMCPOtherSecretName
					},
					reject: testMCPOwnedSecretRejection},
				{name: "an external identity that reads a Secret whose name only begins with the operator's",
					edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretRef = testMCPOwnedSecretName + "-x" })},
				{name: "an external identity that reads the Secret the operator owns for another platform",
					edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { id.ClientSecretRef = testMCPOtherSecretName })},

				// Schema bounds beyond the lengths: the enums and the one list.
				{name: "an unknown identity mode", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { id.Mode = "other" }),
					reject: testMCPUnsupportedRejection},
				{name: "an unknown exchange mode", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { id.ExchangeMode = "other" }),
					reject: testMCPUnsupportedRejection},
				{name: "33 authorization scopes", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.AuthorizationScopes = scopes(33, 1)
				}), reject: testMCPTooManyRejection},
				{name: "an authorization scope of 256 characters", edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.AuthorizationScopes = scopes(1, 256)
				}), reject: testMCPTooLongRejection},
				{name: "32 authorization scopes of 255 characters and every other setting at its longest",
					edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) {
						id.AuthorizationScopes = scopes(32, 255)
						for _, l := range lengths {
							l.set(id, valueOf(l, l.limit))
						}
					})},
			}
			for _, s := range required {
				cases = append(cases, identityCase{
					name:   "external identity without " + s.name,
					edit:   onIdentity(s.clear),
					reject: testMCPIdentityRequiredRejection,
				})
			}
			for _, s := range externalOnly {
				cases = append(cases, identityCase{
					name:   "managed identity with " + s.name,
					edit:   managed(s.set),
					reject: testMCPIdentityManagedRejection,
				})
			}
			for _, u := range append([]urlSetting{issuer}, backchannel...) {
				for _, m := range malformed {
					cases = append(cases, identityCase{
						name:   u.name + " with " + m.why,
						edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { u.set(id, m.url) }),
						reject: u.reject,
					})
				}
			}
			for _, url := range []string{"http://idp.example.com/x", "http://kc.idp.svc:8080/x"} {
				cases = append(cases, identityCase{
					name:   "an issuer over http, " + url,
					edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { issuer.set(id, url) }),
					reject: issuer.reject,
				})
			}
			// An issuer is the identifier the provider puts in its tokens, so it carries no query either.
			for _, q := range []struct{ why, url string }{
				{"a query", "https://idp.example.com/realms/x?x=1"},
				{"a query and no path", "https://idp.example.com?x=1"},
				{"a fragment and no path", "https://idp.example.com#f"},
			} {
				cases = append(cases, identityCase{
					name:   "an issuer with " + q.why,
					edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { issuer.set(id, q.url) }),
					reject: issuer.reject,
				})
			}
			for _, u := range backchannel {
				for _, h := range plainHTTP {
					cases = append(cases, identityCase{
						name:   u.name + " over http to " + h.why,
						edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { u.set(id, h.url) }),
						reject: u.reject,
					})
				}
				for _, h := range serviceHosts {
					cases = append(cases, identityCase{
						name: u.name + " over http to " + h.why,
						edit: onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { u.set(id, h.url) }),
					})
				}
			}
			for _, l := range lengths {
				cases = append(cases, identityCase{
					name:   l.name + " of one character more than its limit",
					edit:   onIdentity(func(id *otilmv1alpha1.MCPIdentitySpec) { l.set(id, valueOf(l, l.limit+1)) }),
					reject: testMCPTooLongRejection,
				})
			}
			cases = append(cases, secretCases(func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ClientSecretRef = v },
				testMCPSecretNameRejection, badSecretNames, goodSecretNames)...)
			cases = append(cases, secretCases(func(id *otilmv1alpha1.MCPIdentitySpec, v string) { id.ClientSecretKey = v },
				testMCPSecretKeyRejection, badSecretKeys, goodSecretKeys)...)

			for i, tc := range cases {
				title := "accepts " + tc.name
				if tc.reject != "" {
					title = "rejects " + tc.name
				}
				It(title, func() {
					p := externalPlatform(freshNS(fmt.Sprintf("cel-mcp-id-%d", i)))
					if tc.edit != nil {
						tc.edit(p)
					}
					if tc.reject == "" {
						Expect(k8sClient.Create(ctx, p)).To(Succeed())
						return
					}
					rejects(p, tc.reject)
				})
			}
			It("rejects managed identity with an issuer under the platform's Keycloak path without naming that Keycloak", func() {
				p := externalPlatform(freshNS("cel-mcp-id-managedissuer"))
				managed(func(id *otilmv1alpha1.MCPIdentitySpec) {
					id.Issuer = testHTTPSScheme + testEdgeHost + testMCPOwnRealmPath
				})(p)
				err := k8sClient.Create(ctx, p)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(testMCPIdentityManagedRejection))
				Expect(err.Error()).NotTo(ContainSubstring(testMCPOwnKeycloakRejection))
			})

			// The switch from the managed identity is where the Secret would be lost: the operator has relayed
			// <platform>-mcp-client for that identity, and an external one that read it would find it pruned.
			// Admission refuses the switch on update as on create. A merge patch carries no resourceVersion,
			// so the reconciler's own writes never turn the refusal into a conflict.
			It("rejects switching a managed identity to an external one that reads the Secret the operator owns", func() {
				ns := freshNS("cel-mcp-id-switch")
				Expect(k8sClient.Create(ctx, mcpPlatform(ns))).To(Succeed())
				managedPlatform := &otilmv1alpha1.Platform{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ilm"}, managedPlatform)).To(Succeed())
				switchTo := func(secretRef string) error {
					p := managedPlatform.DeepCopy()
					p.Spec.MCP.Identity = externalPlatform(ns).Spec.MCP.Identity
					p.Spec.MCP.Identity.ClientSecretRef = secretRef
					return k8sClient.Patch(ctx, p, client.MergeFrom(managedPlatform))
				}

				err := switchTo(testMCPOwnedSecretName)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(testMCPOwnedSecretRejection))
				Expect(switchTo(testMCPIdentitySecretRef)).To(Succeed())
			})
			// That rule sits on the Platform itself, where spec is optional, so it must not fail by erroring on a
			// Platform that has none.
			It("accepts a Platform that has no spec", func() {
				u := onTheWire(externalPlatform(freshNS("cel-mcp-id-nospec")), func(obj map[string]interface{}) {
					unstructured.RemoveNestedField(obj, "spec")
				})
				Expect(k8sClient.Create(ctx, u, client.DryRunAll)).To(Succeed())
			})

			// The typed client omits an empty string and always sends the common and mcp blocks, so a setting
			// that is present and empty, or a block that is absent, can only be sent unstructured. A setting
			// that is present and empty counts as unset, as an empty delegation group does.
			It("accepts managed identity whose settings are all explicitly empty", func() {
				ns := freshNS("cel-mcp-id-empty")
				p := mcpPlatform(ns)
				p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{Mode: "managed"}
				u := onTheWire(p, func(obj map[string]interface{}) {
					for _, name := range []string{
						"issuer", "jwkSetUri", "tokenUri", "clientId", "clientSecretRef", "clientSecretKey",
						"exchangeScope", "requiredScope", "apiAudience",
					} {
						Expect(unstructured.SetNestedField(obj, "", "spec", "mcp", "identity", name)).To(Succeed())
					}
					Expect(unstructured.SetNestedStringSlice(obj, []string{}, "spec", "mcp", "identity", "authorizationScopes")).To(Succeed())
				})
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
			})
			// exchangeMode has no such case: its enum refuses an empty string outright.
			for i, name := range []string{"issuer", "jwkSetUri", "tokenUri", "clientId", "clientSecretRef"} {
				It("rejects external identity whose "+name+" is explicitly empty", func() {
					ns := freshNS(fmt.Sprintf("cel-mcp-id-empty-%d", i))
					u := onTheWire(externalPlatform(ns), func(obj map[string]interface{}) {
						Expect(unstructured.SetNestedField(obj, "", "spec", "mcp", "identity", name)).To(Succeed())
					})
					rejects(u, testMCPIdentityRequiredRejection)
				})
			}
			// clientSecretKey is optional with external identity, where an empty value takes its default.
			It("accepts external identity whose clientSecretKey is explicitly empty", func() {
				ns := freshNS("cel-mcp-id-emptykey")
				u := onTheWire(externalPlatform(ns), func(obj map[string]interface{}) {
					Expect(unstructured.SetNestedField(obj, "", "spec", "mcp", "identity", "clientSecretKey")).To(Succeed())
				})
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
			})
			It("accepts external identity whose delegation group is explicitly empty", func() {
				ns := freshNS("cel-mcp-id-emptygroup")
				u := onTheWire(externalPlatform(ns), func(obj map[string]interface{}) {
					Expect(unstructured.SetNestedField(obj, "", "spec", "mcp", "delegation", "group")).To(Succeed())
				})
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
			})
			It("accepts external identity with no delegation block on the wire", func() {
				ns := freshNS("cel-mcp-id-nodelegation")
				u := onTheWire(externalPlatform(ns), func(obj map[string]interface{}) {
					unstructured.RemoveNestedField(obj, "spec", "mcp", "delegation")
				})
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
			})
			It("rejects external identity with no common block and no edge on the wire", func() {
				ns := freshNS("cel-mcp-id-nocommon")
				p := externalPlatform(ns)
				p.Spec.Common.HostName = ""
				rejects(onTheWire(p, func(obj map[string]interface{}) {
					unstructured.RemoveNestedField(obj, "spec", "common")
				}), testMCPHostRejection)
			})
			// An enabled edge whose host is present but empty names no host, so the platform's host is
			// common.hostName and an issuer under its Keycloak path is the platform's own.
			It("rejects an issuer under common.hostName's Keycloak path behind an enabled edge whose host is explicitly empty", func() {
				ns := freshNS("cel-mcp-id-emptyedgehost")
				p := externalPlatform(ns)
				edgeOn(testMCPIdentityEdgeHost, true)(p)
				p.Spec.MCP.Identity.Issuer = testHTTPSScheme + testEdgeHost + testMCPOwnRealmPath
				rejects(onTheWire(p, func(obj map[string]interface{}) {
					Expect(unstructured.SetNestedField(obj, "", "spec", "edge", "host")).To(Succeed())
				}), testMCPOwnKeycloakRejection)
			})

			It("stores no identity block for a Platform that sets none", func() {
				ns := freshNS("cel-mcp-id-none")
				Expect(k8sClient.Create(ctx, mcpPlatform(ns))).To(Succeed())
				got := &otilmv1alpha1.Platform{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ilm"}, got)).To(Succeed())
				Expect(got.Spec.MCP.Identity).To(BeNil())
			})
			It("defaults an empty identity block to managed mode and nothing more", func() {
				ns := freshNS("cel-mcp-id-default")
				p := mcpPlatform(ns)
				p.Spec.MCP.Identity = &otilmv1alpha1.MCPIdentitySpec{}
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
				got := &otilmv1alpha1.Platform{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ilm"}, got)).To(Succeed())
				Expect(got.Spec.MCP.Identity).To(Equal(&otilmv1alpha1.MCPIdentitySpec{Mode: "managed"}))
			})
		})
	})
})
