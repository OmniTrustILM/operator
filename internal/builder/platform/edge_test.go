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
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// strPtr returns a pointer to s, for setting *string CR fields in tests.
func strPtr(s string) *string { return &s }

// edgePlatform returns the base platform with an enabled edge whose TLS source
// and class are set by the caller; host and a couple of nginx annotations use
// representative defaults so the assertions exercise realistic input.
func edgePlatform(tls *otilmv1alpha1.EdgeTLSSpec) *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled:   true,
		Type:      "ingress",
		ClassName: strPtr("nginx"),
		Host:      testEdgeHost,
		Annotations: map[string]string{
			"nginx.ingress.kubernetes.io/backend-protocol":  "HTTP",
			"nginx.ingress.kubernetes.io/proxy-buffer-size": "256k",
		},
		TLS: tls,
	}
	return p
}

// findIngress returns the single typed Ingress from the rendered set, failing if
// zero or more than one is present. The builder returns a typed *networkingv1.Ingress
// (its TypeMeta is left empty, so we match on Go type).
func findIngress(t *testing.T, objs []client.Object) *networkingv1.Ingress {
	t.Helper()
	var found []*networkingv1.Ingress
	for _, o := range objs {
		if ing, ok := o.(*networkingv1.Ingress); ok {
			found = append(found, ing)
		}
	}
	require.Len(t, found, 1, "expected exactly one Ingress object")
	return found[0]
}

// findUnstructured returns the single unstructured object with the given kind and
// name, failing if absent.
func findUnstructured(t *testing.T, objs []client.Object, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetKind() == kind && u.GetName() == name {
			return u
		}
	}
	require.Failf(t, "object not found", "no %s/%s in rendered edge objects", kind, name)
	return nil
}

// countUnstructured counts unstructured objects of the given kind.
func countUnstructured(objs []client.Object, kind string) int {
	n := 0
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == kind {
			n++
		}
	}
	return n
}

func TestResolveEdgeDisabled(t *testing.T) {
	// nil edge -> no objects.
	assert.Nil(t, ResolveEdge(basePlatform()), "nil edge must render no objects")

	// enabled=false -> no objects.
	p := basePlatform()
	p.Spec.Edge = &otilmv1alpha1.EdgeSpec{Enabled: false, Host: testEdgeHost}
	assert.Nil(t, ResolveEdge(p), "disabled edge must render no objects")
}

func TestResolveEdgeIngressFields(t *testing.T) {
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}))
	ing := findIngress(t, objs)

	// Name follows the operator's edge naming: "<instance>-platform-core".
	assert.Equal(t, "ilm-platform-core", ing.Name)
	assert.Equal(t, "ilm-system", ing.Namespace)

	// The component label carries the Ingress role so the object is selectable by role.
	assert.Equal(t, edgeIngressRole, ing.Labels[common.ComponentLabel])

	// Class is set as spec.ingressClassName (the modern selector); the deprecated
	// kubernetes.io/ingress.class annotation is no longer emitted.
	require.NotNil(t, ing.Spec.IngressClassName, "operator must set spec.ingressClassName from edge.className")
	assert.Equal(t, "nginx", *ing.Spec.IngressClassName)
	assert.NotContains(t, ing.Annotations, "kubernetes.io/ingress.class",
		"the deprecated class annotation must not be emitted")

	// Single rule: host + path "/" Prefix -> api-gateway Service consumer port 8000.
	require.Len(t, ing.Spec.Rules, 1)
	rule := ing.Spec.Rules[0]
	assert.Equal(t, testEdgeHost, rule.Host)
	require.NotNil(t, rule.HTTP)
	require.Len(t, rule.HTTP.Paths, 1)
	path := rule.HTTP.Paths[0]
	assert.Equal(t, "/", path.Path)
	require.NotNil(t, path.PathType)
	assert.Equal(t, networkingv1.PathTypePrefix, *path.PathType)
	require.NotNil(t, path.Backend.Service)
	assert.Equal(t, gatewayName, path.Backend.Service.Name, "backend must be the api-gateway Service")
	assert.Equal(t, int32(8000), path.Backend.Service.Port.Number)

	// TLS: host + the default serving Secret name.
	require.Len(t, ing.Spec.TLS, 1)
	assert.Equal(t, []string{testEdgeHost}, ing.Spec.TLS[0].Hosts)
	assert.Equal(t, defaultEdgeTLSSecret, ing.Spec.TLS[0].SecretName)

	// Caller annotations are merged through.
	assert.Equal(t, "HTTP", ing.Annotations["nginx.ingress.kubernetes.io/backend-protocol"])
	assert.Equal(t, "256k", ing.Annotations["nginx.ingress.kubernetes.io/proxy-buffer-size"])
}

func TestResolveEdgeInternalCertManagerChain(t *testing.T) {
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}))

	// internal source: ingress-shim annotations point at the CA issuer.
	ing := findIngress(t, objs)
	assert.Equal(t, caIssuerName, ing.Annotations[certManagerIssuerAnnotation])
	assert.Equal(t, kindIssuer, ing.Annotations[certManagerIssuerKindAnnotation])

	// selfsigned-issuer: selfSigned: {}.
	ssi := findUnstructured(t, objs, kindIssuer, selfSignedIssuerName)
	selfSigned, ok, _ := unstructured.NestedMap(ssi.Object, "spec", "selfSigned")
	assert.True(t, ok, "selfsigned-issuer must have spec.selfSigned")
	assert.Empty(t, selfSigned)

	// ca-certificate: isCA, commonName ca, into ca-keypair, RSA 4096, issued by
	// selfsigned-issuer.
	cert := findUnstructured(t, objs, kindCertificate, caCertificateName)
	assert.Equal(t, "ca", cert.GetLabels()[common.ComponentLabel], "Certificate role must be 'ca'")
	isCA, _, _ := unstructured.NestedBool(cert.Object, "spec", "isCA")
	assert.True(t, isCA)
	cn, _, _ := unstructured.NestedString(cert.Object, "spec", "commonName")
	assert.Equal(t, caCommonName, cn)
	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	assert.Equal(t, caKeypairSecret, secretName)
	alg, _, _ := unstructured.NestedString(cert.Object, "spec", "privateKey", "algorithm")
	assert.Equal(t, caPrivateKeyAlgorithm, alg)
	size, _, _ := unstructured.NestedInt64(cert.Object, "spec", "privateKey", "size")
	assert.Equal(t, caPrivateKeySize, size)
	issuerName, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	assert.Equal(t, selfSignedIssuerName, issuerName)
	issuerKind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
	assert.Equal(t, kindIssuer, issuerKind)

	// ca-issuer: ca: {secretName: ca-keypair}; no acme block.
	caIssuer := findUnstructured(t, objs, kindIssuer, caIssuerName)
	caSecret, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "ca", "secretName")
	assert.Equal(t, caKeypairSecret, caSecret)
	_, hasACME, _ := unstructured.NestedMap(caIssuer.Object, "spec", "acme")
	assert.False(t, hasACME, "internal ca-issuer must not have an acme block")

	// Exactly two Issuers (selfsigned + ca) and one Certificate.
	assert.Equal(t, 2, countUnstructured(objs, kindIssuer))
	assert.Equal(t, 1, countUnstructured(objs, kindCertificate))
}

func TestResolveEdgeLetsEncryptProduction(t *testing.T) {
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:      edgeSourceLetsEncrypt,
		LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: testACMEEmail, Environment: "production"},
	}))

	// No Certificate (the ingress-shim mints the leaf), exactly one Issuer (ca-issuer).
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate), "letsEncrypt must not render a Certificate")
	assert.Equal(t, 1, countUnstructured(objs, kindIssuer))

	// Ingress still carries the ingress-shim annotations.
	ing := findIngress(t, objs)
	assert.Equal(t, caIssuerName, ing.Annotations[certManagerIssuerAnnotation])

	caIssuer := findUnstructured(t, objs, kindIssuer, caIssuerName)
	server, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "server")
	assert.Equal(t, acmeProductionURL, server, "production must use the prod ACME directory")
	email, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "email")
	assert.Equal(t, testACMEEmail, email)
	keyRef, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "privateKeySecretRef", "name")
	assert.Equal(t, "letsencrypt-production", keyRef)

	// http01 solver references the ingress class.
	solvers, _, _ := unstructured.NestedSlice(caIssuer.Object, "spec", "acme", "solvers")
	require.Len(t, solvers, 1)
	solver := solvers[0].(map[string]interface{})
	class := solver["http01"].(map[string]interface{})["ingress"].(map[string]interface{})["class"]
	assert.Equal(t, "nginx", class)
}

func TestResolveEdgeLetsEncryptStaging(t *testing.T) {
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:      edgeSourceLetsEncrypt,
		LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "ops@example.com", Environment: "staging"},
	}))
	caIssuer := findUnstructured(t, objs, kindIssuer, caIssuerName)
	server, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "server")
	assert.Equal(t, acmeStagingURL, server, "staging must use the staging ACME directory")
	keyRef, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "privateKeySecretRef", "name")
	assert.Equal(t, "letsencrypt-staging", keyRef)
}

func TestResolveEdgeSecretBYO(t *testing.T) {
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceSecret,
		SecretRef: strPtr(testBYOTLSSecret),
	}))

	// No cert-manager objects at all.
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate))
	assert.Equal(t, 0, countUnstructured(objs, kindIssuer))
	assert.Len(t, objs, 1, "bring-your-own renders only the Ingress")

	ing := findIngress(t, objs)
	// The Ingress references the caller's Secret and carries NO cert-manager
	// ingress-shim annotation.
	assert.Equal(t, testBYOTLSSecret, ing.Spec.TLS[0].SecretName)
	_, hasIssuer := ing.Annotations[certManagerIssuerAnnotation]
	assert.False(t, hasIssuer, "bring-your-own Ingress must not carry a cert-manager issuer annotation")
}

func TestResolveEdgeIssuerRefNamespacedIssuer(t *testing.T) {
	// source=issuerRef with the default Kind (Issuer): the Ingress carries the
	// cert-manager.io/issuer annotation naming the caller's Issuer, plus issuer-kind
	// Issuer. No operator-created cert-manager objects are rendered.
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer, Kind: kindIssuer, Group: certManagerDefaultGroup},
	}))

	// Only the Ingress is rendered — the user's issuer mints the cert via the shim.
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate), "issuerRef renders no operator Certificate")
	assert.Equal(t, 0, countUnstructured(objs, kindIssuer), "issuerRef renders no operator Issuer")
	assert.Len(t, objs, 1, "issuerRef renders only the Ingress (the caller owns the issuer)")

	ing := findIngress(t, objs)
	// Namespaced Issuer -> cert-manager.io/issuer naming the caller's issuer.
	assert.Equal(t, testVaultIssuer, ing.Annotations[certManagerIssuerAnnotation])
	assert.Equal(t, kindIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
	// No cluster-issuer key for a namespaced Issuer.
	_, hasCluster := ing.Annotations[certManagerClusterIssuerAnnotation]
	assert.False(t, hasCluster, "namespaced Issuer must not carry the cluster-issuer annotation")
	// Default group is implicit — no issuer-group annotation.
	_, hasGroup := ing.Annotations[certManagerIssuerGroupAnnotation]
	assert.False(t, hasGroup, "default cert-manager.io group must not emit issuer-group")
	// TLS Secret defaults to the same name cert-manager populates.
	assert.Equal(t, defaultEdgeTLSSecret, ing.Spec.TLS[0].SecretName)
}

func TestResolveEdgeIssuerRefClusterIssuer(t *testing.T) {
	// source=issuerRef with Kind=ClusterIssuer: the Ingress carries the
	// cert-manager.io/cluster-issuer annotation (NOT the namespaced issuer key).
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testCorpCA, Kind: kindClusterIssuer},
	}))

	assert.Equal(t, 0, countUnstructured(objs, kindCertificate))
	assert.Equal(t, 0, countUnstructured(objs, kindIssuer))

	ing := findIngress(t, objs)
	assert.Equal(t, testCorpCA, ing.Annotations[certManagerClusterIssuerAnnotation])
	assert.Equal(t, kindClusterIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
	// The namespaced issuer key must be absent for a ClusterIssuer.
	_, hasNamespaced := ing.Annotations[certManagerIssuerAnnotation]
	assert.False(t, hasNamespaced, "ClusterIssuer must use cluster-issuer, not the namespaced issuer key")
}

func TestResolveEdgeIssuerRefExternalGroup(t *testing.T) {
	// A non-default issuer group (an external issuer, e.g. awspca.cert-manager.io)
	// emits the issuer-group annotation; the default group does not (tested above).
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source: edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{
			Name: "aws-pca", Kind: kindClusterIssuer, Group: "awspca.cert-manager.io",
		},
	}))
	ing := findIngress(t, objs)
	assert.Equal(t, "awspca.cert-manager.io", ing.Annotations[certManagerIssuerGroupAnnotation])
	assert.Equal(t, "aws-pca", ing.Annotations[certManagerClusterIssuerAnnotation])
	assert.Equal(t, kindClusterIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
}

func TestResolveEdgeIssuerRefDefaultKindIsIssuer(t *testing.T) {
	// Kind unset behaves as the CRD default (Issuer): the namespaced issuer key.
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer},
	}))
	ing := findIngress(t, objs)
	assert.Equal(t, testVaultIssuer, ing.Annotations[certManagerIssuerAnnotation])
	assert.Equal(t, kindIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
}

func TestResolveEdgeIssuerRefSecretRefOverride(t *testing.T) {
	// issuerRef uses the default serving Secret name unless secretRef overrides it
	// (cert-manager still populates whatever name the Ingress references).
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer},
		SecretRef: strPtr("custom-issuerref-tls"),
	}))
	ing := findIngress(t, objs)
	assert.Equal(t, "custom-issuerref-tls", ing.Spec.TLS[0].SecretName)
}

func TestResolveEdgeIssuerRefCallerCannotOverrideShim(t *testing.T) {
	// A caller annotation must not hijack the issuerRef shim — every shim key is
	// operator-owned (issuer, cluster-issuer, issuer-kind, issuer-group).
	p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceIssuerRef,
		IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer, Kind: kindIssuer},
	})
	p.Spec.Edge.Annotations[certManagerIssuerAnnotation] = "evil-issuer"
	p.Spec.Edge.Annotations[certManagerClusterIssuerAnnotation] = "evil-cluster-issuer"
	p.Spec.Edge.Annotations[certManagerIssuerKindAnnotation] = "ClusterIssuer"
	p.Spec.Edge.Annotations[certManagerIssuerGroupAnnotation] = "evil.example.com"
	objs := ResolveEdge(p)
	ing := findIngress(t, objs)
	assert.Equal(t, testVaultIssuer, ing.Annotations[certManagerIssuerAnnotation], "issuer key is operator-owned")
	assert.Equal(t, kindIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
	_, hasCluster := ing.Annotations[certManagerClusterIssuerAnnotation]
	assert.False(t, hasCluster, "caller cluster-issuer must be dropped (namespaced Issuer)")
	_, hasGroup := ing.Annotations[certManagerIssuerGroupAnnotation]
	assert.False(t, hasGroup, "caller issuer-group must be dropped (default group)")
}

func TestResolveEdgeSecretRefOverridesDefaultTLSSecret(t *testing.T) {
	// For internal source, a caller secretRef overrides the default serving Secret
	// name on the Ingress (cert-manager still populates it).
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:    edgeSourceInternal,
		SecretRef: strPtr("custom-tls"),
	}))
	ing := findIngress(t, objs)
	assert.Equal(t, "custom-tls", ing.Spec.TLS[0].SecretName)
}

func TestResolveEdgeDefaultSourceIsInternal(t *testing.T) {
	// TLS set but source empty -> default to internal (renders the CA chain).
	objs := ResolveEdge(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{}))
	assert.Equal(t, 2, countUnstructured(objs, kindIssuer), "empty source must default to internal CA chain")
	assert.Equal(t, 1, countUnstructured(objs, kindCertificate))
}

func TestResolveEdgeCallerCannotOverrideCertManagerAnnotations(t *testing.T) {
	p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
	// A malicious/incorrect caller annotation must not redirect the issuer.
	p.Spec.Edge.Annotations[certManagerIssuerAnnotation] = "evil-issuer"
	p.Spec.Edge.Annotations[certManagerIssuerKindAnnotation] = "ClusterIssuer"
	objs := ResolveEdge(p)
	ing := findIngress(t, objs)
	assert.Equal(t, caIssuerName, ing.Annotations[certManagerIssuerAnnotation], "cert-manager issuer annotation is operator-owned")
	assert.Equal(t, kindIssuer, ing.Annotations[certManagerIssuerKindAnnotation])
}

// TestResolveEdgeNoSecretLeakage asserts that no rendered edge object embeds
// certificate or key material: cert-manager objects reference Secrets by name
// only, and the only Secret names present are the well-known references. This is
// a structural guard against accidentally inlining cert/key bytes.
func TestResolveEdgeNoSecretLeakage(t *testing.T) {
	for _, src := range []*otilmv1alpha1.EdgeTLSSpec{
		{Source: edgeSourceInternal},
		{Source: edgeSourceLetsEncrypt, LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: testACMEEmail, Environment: "production"}},
		{Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer}},
		{Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testCorpCA, Kind: kindClusterIssuer}},
		{Source: edgeSourceSecret, SecretRef: strPtr(testBYOTLSSecret)},
	} {
		objs := ResolveEdge(edgePlatform(src))
		for _, o := range objs {
			u, ok := o.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			// No PEM/cert/key data fields anywhere in a cert-manager object's spec.
			for _, forbidden := range []string{"tls.crt", "tls.key", "ca.crt", "-----BEGIN"} {
				assert.NotContains(t, fmtUnstructured(u), forbidden,
					"edge object %s/%s must not embed cert/key material", u.GetKind(), u.GetName())
			}
			// privateKey blocks may declare algorithm/size but never a literal key.
			if pk, ok, _ := unstructured.NestedMap(u.Object, "spec", "privateKey"); ok {
				_, hasInlineKey := pk["key"]
				assert.False(t, hasInlineKey, "Certificate privateKey must not carry an inline key")
			}
		}
	}
}

// fmtUnstructured returns a flat string of an unstructured object's content for
// substring scanning in the leakage test.
func fmtUnstructured(u *unstructured.Unstructured) string {
	var sb strings.Builder
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, vv := range t {
				sb.WriteString(k)
				sb.WriteString(" ")
				walk(vv)
			}
		case []interface{}:
			for _, vv := range t {
				walk(vv)
			}
		default:
			sb.WriteString(strings.TrimSpace(strings.ToLower(strings.ReplaceAll(toString(t), "\n", " "))))
			sb.WriteString(" ")
		}
	}
	walk(u.Object)
	return sb.String()
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
