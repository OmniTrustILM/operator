/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayAPIPlatform returns the base platform with an enabled gatewayAPI edge
// whose TLS source and GatewayAPI sub-spec are set by the caller. Host matches the
// other edge tests so assertions are realistic.
func gatewayAPIPlatform(tls *otilmv1alpha1.EdgeTLSSpec, gw *otilmv1alpha1.GatewayAPISpec) *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.Edge = &otilmv1alpha1.EdgeSpec{
		Enabled:    true,
		Type:       "gatewayAPI",
		Host:       testEdgeHost,
		TLS:        tls,
		GatewayAPI: gw,
	}
	return p
}

// ownedGatewayPlatform is the common "operator owns the Gateway" setup: a
// gatewayClassName is set and no parentRef.
func ownedGatewayPlatform(tls *otilmv1alpha1.EdgeTLSSpec) *otilmv1alpha1.Platform {
	return gatewayAPIPlatform(tls, &otilmv1alpha1.GatewayAPISpec{
		GatewayClassName: strPtr("istio"),
	})
}

// firstParentRef returns the single parentRef map from an HTTPRoute's spec.
func firstParentRef(t *testing.T, route *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	refs, ok, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	require.True(t, ok, "HTTPRoute must have spec.parentRefs")
	require.Len(t, refs, 1, "expected exactly one parentRef")
	m, ok := refs[0].(map[string]interface{})
	require.True(t, ok, "parentRef must be a map")
	return m
}

// firstListener returns the listener with the given name from a Gateway's spec.
func firstListener(t *testing.T, gw *unstructured.Unstructured, name string) map[string]interface{} {
	t.Helper()
	listeners, ok, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	require.True(t, ok, "Gateway must have spec.listeners")
	for _, l := range listeners {
		m, ok := l.(map[string]interface{})
		require.True(t, ok)
		if m["name"] == name {
			return m
		}
	}
	require.Failf(t, "listener not found", "no listener named %q", name)
	return nil
}

// noIngress asserts the rendered set contains no networking.k8s.io Ingress (the
// Gateway API path renders an HTTPRoute, never an Ingress).
func noIngress(t *testing.T, objs []client.Object) {
	t.Helper()
	for _, o := range objs {
		_, ok := o.(*networkingv1.Ingress)
		assert.False(t, ok, "Gateway API edge must not render an Ingress")
	}
}

func TestResolveGatewayAPIOwnedInternal(t *testing.T) {
	objs := ResolveEdge(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}))
	noIngress(t, objs)

	// --- Gateway ---
	gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
	assert.Equal(t, gatewayAPIVersion, gw.GetAPIVersion(), "Gateway must carry the v1 apiVersion")
	assert.Equal(t, "ilm-system", gw.GetNamespace())
	assert.Equal(t, edgeGatewayRole, gw.GetLabels()[common.ComponentLabel])

	className, _, _ := unstructured.NestedString(gw.Object, "spec", "gatewayClassName")
	assert.Equal(t, "istio", className)

	// gateway-shim annotations present for the internal source.
	assert.Equal(t, caIssuerName, gw.GetAnnotations()[certManagerIssuerAnnotation])
	assert.Equal(t, kindIssuer, gw.GetAnnotations()[certManagerIssuerKindAnnotation])

	// HTTP listener: :80, Same-namespace routes, host pinned.
	http := firstListener(t, gw, gatewayListenerHTTP)
	assert.Equal(t, gatewayProtocolHTTP, http["protocol"])
	assert.Equal(t, gatewayPortHTTP, http["port"])
	assert.Equal(t, testEdgeHost, http["hostname"])
	httpFrom := http["allowedRoutes"].(map[string]interface{})["namespaces"].(map[string]interface{})["from"]
	assert.Equal(t, gatewayRoutesFromSame, httpFrom)

	// HTTPS listener: :443, Terminate, certificateRef -> default edge TLS Secret.
	https := firstListener(t, gw, gatewayListenerHTTPS)
	assert.Equal(t, gatewayProtocolHTTPS, https["protocol"])
	assert.Equal(t, gatewayPortHTTPS, https["port"])
	assert.Equal(t, testEdgeHost, https["hostname"])
	tls := https["tls"].(map[string]interface{})
	assert.Equal(t, gatewayTLSModeTerminate, tls["mode"])
	refs := tls["certificateRefs"].([]interface{})
	require.Len(t, refs, 1)
	ref := refs[0].(map[string]interface{})
	assert.Equal(t, gatewayCertRefKindSecret, ref["kind"])
	assert.Equal(t, defaultEdgeTLSSecret, ref["name"], "HTTPS listener must reference the edge TLS Secret by name")

	// --- HTTPRoute ---
	route := findUnstructured(t, objs, kindHTTPRoute, testIngressRole)
	assert.Equal(t, gatewayAPIVersion, route.GetAPIVersion())
	parent := firstParentRef(t, route)
	assert.Equal(t, ownedGatewayName, parent["name"], "owned-Gateway HTTPRoute must attach to platform-edge")
	_, hasNS := parent["namespace"]
	assert.False(t, hasNS, "owned-Gateway parentRef omits namespace (same ns)")

	hostnames, ok, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	require.True(t, ok)
	assert.Equal(t, []string{testEdgeHost}, hostnames)

	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	require.Len(t, rules, 1)
	rule := rules[0].(map[string]interface{})
	backend := rule["backendRefs"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, gatewayName, backend["name"], "backend must be the api-gateway Service")
	assert.Equal(t, int64(8000), backend["port"], "backend port must be the consumer port 8000")
	match := rule["matches"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, httpRoutePathPrefix, match["path"].(map[string]interface{})["type"])
	assert.Equal(t, "/", match["path"].(map[string]interface{})["value"])

	// --- cert-manager chain (same as the Ingress internal source) ---
	assert.Equal(t, 2, countUnstructured(objs, kindIssuer), "internal source renders selfsigned + ca issuers")
	assert.Equal(t, 1, countUnstructured(objs, kindCertificate))
	ssi := findUnstructured(t, objs, kindIssuer, selfSignedIssuerName)
	_, ok, _ = unstructured.NestedMap(ssi.Object, "spec", "selfSigned")
	assert.True(t, ok, "selfsigned-issuer present")
	caIssuer := findUnstructured(t, objs, kindIssuer, caIssuerName)
	caSecret, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "ca", "secretName")
	assert.Equal(t, caKeypairSecret, caSecret)
}

func TestResolveGatewayAPIOwnedLetsEncrypt(t *testing.T) {
	objs := ResolveEdge(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
		Source:      edgeSourceLetsEncrypt,
		LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "info@example.com", Environment: "production"},
	}))

	// gateway-shim annotations still present (letsEncrypt lets cert-manager mint).
	gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
	assert.Equal(t, caIssuerName, gw.GetAnnotations()[certManagerIssuerAnnotation])
	assert.Equal(t, kindIssuer, gw.GetAnnotations()[certManagerIssuerKindAnnotation])

	// Exactly the ACME issuer, no Certificate (the shim mints the leaf).
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate))
	assert.Equal(t, 1, countUnstructured(objs, kindIssuer))
	caIssuer := findUnstructured(t, objs, kindIssuer, caIssuerName)
	server, _, _ := unstructured.NestedString(caIssuer.Object, "spec", "acme", "server")
	assert.Equal(t, acmeProductionURL, server)
}

func TestResolveGatewayAPIOwnedIssuerRef(t *testing.T) {
	// source=issuerRef on an operator-owned Gateway: the Gateway carries the
	// gateway-shim annotation naming the caller's issuer, and NO operator-created
	// cert-manager objects are rendered (the caller owns the issuer).
	t.Run("namespaced Issuer", func(t *testing.T) {
		objs := ResolveEdge(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source:    edgeSourceIssuerRef,
			IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "vault-issuer", Kind: kindIssuer},
		}))

		assert.Equal(t, 0, countUnstructured(objs, kindCertificate), "issuerRef renders no operator Certificate")
		assert.Equal(t, 0, countUnstructured(objs, kindIssuer), "issuerRef renders no operator Issuer")
		assert.Len(t, objs, 2, "issuerRef owned-Gateway renders only the Gateway and HTTPRoute")

		gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
		assert.Equal(t, "vault-issuer", gw.GetAnnotations()[certManagerIssuerAnnotation])
		assert.Equal(t, kindIssuer, gw.GetAnnotations()[certManagerIssuerKindAnnotation])
		_, hasCluster := gw.GetAnnotations()[certManagerClusterIssuerAnnotation]
		assert.False(t, hasCluster, "namespaced Issuer must not carry the cluster-issuer annotation")
		// HTTPS listener still references the edge TLS Secret by name (cert-manager-populated).
		https := firstListener(t, gw, gatewayListenerHTTPS)
		ref := https["tls"].(map[string]interface{})["certificateRefs"].([]interface{})[0].(map[string]interface{})
		assert.Equal(t, defaultEdgeTLSSecret, ref["name"])
	})

	t.Run("ClusterIssuer", func(t *testing.T) {
		objs := ResolveEdge(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source:    edgeSourceIssuerRef,
			IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testCorpCA, Kind: kindClusterIssuer},
		}))
		gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
		assert.Equal(t, testCorpCA, gw.GetAnnotations()[certManagerClusterIssuerAnnotation])
		assert.Equal(t, kindClusterIssuer, gw.GetAnnotations()[certManagerIssuerKindAnnotation])
		_, hasNamespaced := gw.GetAnnotations()[certManagerIssuerAnnotation]
		assert.False(t, hasNamespaced, "ClusterIssuer must use cluster-issuer, not the namespaced issuer key")
	})
}

func TestResolveGatewayAPIOwnedSecretBYO(t *testing.T) {
	objs := ResolveEdge(gatewayAPIPlatform(
		&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceSecret, SecretRef: strPtr(testBYOTLSSecret)},
		&otilmv1alpha1.GatewayAPISpec{GatewayClassName: strPtr("istio")},
	))

	// Bring-your-own: a Gateway + HTTPRoute, but NO cert-manager objects.
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate))
	assert.Equal(t, 0, countUnstructured(objs, kindIssuer))
	assert.Len(t, objs, 2, "BYO owned-Gateway renders only the Gateway and HTTPRoute")

	gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
	// No gateway-shim annotation for bring-your-own.
	_, hasIssuer := gw.GetAnnotations()[certManagerIssuerAnnotation]
	assert.False(t, hasIssuer, "BYO Gateway must not carry a cert-manager issuer annotation")
	// HTTPS listener still references the caller's Secret by name.
	https := firstListener(t, gw, gatewayListenerHTTPS)
	ref := https["tls"].(map[string]interface{})["certificateRefs"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, testBYOTLSSecret, ref["name"])
}

func TestResolveGatewayAPIParentRefOnly(t *testing.T) {
	// With a parentRef set, the operator renders ONLY the HTTPRoute, even when a
	// TLS source is also configured (TLS is the existing Gateway's concern).
	objs := ResolveEdge(gatewayAPIPlatform(
		&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal},
		&otilmv1alpha1.GatewayAPISpec{
			ParentRef: &otilmv1alpha1.GatewayParentRef{
				Name:        testSharedGW,
				Namespace:   strPtr("gateways"),
				SectionName: strPtr("https"),
			},
		},
	))

	require.Len(t, objs, 1, "parentRef path renders only the HTTPRoute")
	assert.Equal(t, 0, countUnstructured(objs, kindGateway), "no operator-owned Gateway")
	assert.Equal(t, 0, countUnstructured(objs, kindIssuer), "no cert-manager Issuers")
	assert.Equal(t, 0, countUnstructured(objs, kindCertificate), "no cert-manager Certificate")

	route := findUnstructured(t, objs, kindHTTPRoute, testIngressRole)
	parent := firstParentRef(t, route)
	assert.Equal(t, testSharedGW, parent["name"])
	assert.Equal(t, "gateways", parent["namespace"])
	assert.Equal(t, "https", parent["sectionName"])

	// Backend still the api-gateway consumer port.
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	backend := rules[0].(map[string]interface{})["backendRefs"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, gatewayName, backend["name"])
	assert.Equal(t, int64(8000), backend["port"])
}

func TestResolveGatewayAPIParentRefMinimal(t *testing.T) {
	// A parentRef with only a name: namespace/sectionName must be omitted.
	objs := ResolveEdge(gatewayAPIPlatform(nil, &otilmv1alpha1.GatewayAPISpec{
		ParentRef: &otilmv1alpha1.GatewayParentRef{Name: testSharedGW},
	}))
	require.Len(t, objs, 1)
	route := findUnstructured(t, objs, kindHTTPRoute, testIngressRole)
	parent := firstParentRef(t, route)
	assert.Equal(t, testSharedGW, parent["name"])
	_, hasNS := parent["namespace"]
	assert.False(t, hasNS, "optional namespace must be omitted when unset")
	_, hasSection := parent["sectionName"]
	assert.False(t, hasSection, "optional sectionName must be omitted when unset")
}

func TestResolveGatewayAPIHostnameOmittedWhenEmpty(t *testing.T) {
	// No Host: the HTTPRoute omits hostnames and the Gateway listeners omit hostname.
	p := ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
	p.Spec.Edge.Host = ""
	objs := ResolveEdge(p)

	route := findUnstructured(t, objs, kindHTTPRoute, testIngressRole)
	_, hasHostnames, _ := unstructured.NestedSlice(route.Object, "spec", "hostnames")
	assert.False(t, hasHostnames, "HTTPRoute must omit hostnames when Host is empty")

	gw := findUnstructured(t, objs, kindGateway, ownedGatewayName)
	http := firstListener(t, gw, gatewayListenerHTTP)
	_, hasHostname := http["hostname"]
	assert.False(t, hasHostname, "Gateway listener must omit hostname when Host is empty")
}

// TestResolveEdgeIngressTypeStillRenders confirms the Ingress path is unchanged by
// the type switch: type="ingress" (and the default) renders the typed Ingress and
// the cert-manager chain, with no Gateway API objects.
func TestResolveEdgeIngressTypeStillRenders(t *testing.T) {
	for _, typ := range []string{"ingress", ""} {
		p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
		p.Spec.Edge.Type = typ
		objs := ResolveEdge(p)

		// Exactly one typed Ingress, no Gateway API objects.
		findIngress(t, objs) // fails if not exactly one
		assert.Equal(t, 0, countUnstructured(objs, kindGateway), "type=%q must not render a Gateway", typ)
		assert.Equal(t, 0, countUnstructured(objs, kindHTTPRoute), "type=%q must not render an HTTPRoute", typ)
		// Cert-manager chain unchanged.
		assert.Equal(t, 2, countUnstructured(objs, kindIssuer))
		assert.Equal(t, 1, countUnstructured(objs, kindCertificate))
	}
}

// TestResolveGatewayAPINoSecretLeakage asserts no rendered Gateway API or
// cert-manager object embeds cert/key material; Secrets are referenced by name only.
func TestResolveGatewayAPINoSecretLeakage(t *testing.T) {
	platforms := []*otilmv1alpha1.Platform{
		ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}),
		ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source:      edgeSourceLetsEncrypt,
			LetsEncrypt: &otilmv1alpha1.LetsEncryptSpec{Email: "info@example.com", Environment: "production"},
		}),
		ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source:    edgeSourceIssuerRef,
			IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testCorpCA, Kind: kindClusterIssuer},
		}),
		gatewayAPIPlatform(
			&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceSecret, SecretRef: strPtr(testBYOTLSSecret)},
			&otilmv1alpha1.GatewayAPISpec{GatewayClassName: strPtr("istio")},
		),
		gatewayAPIPlatform(nil, &otilmv1alpha1.GatewayAPISpec{
			ParentRef: &otilmv1alpha1.GatewayParentRef{Name: testSharedGW},
		}),
	}
	for _, p := range platforms {
		for _, o := range ResolveEdge(p) {
			u, ok := o.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			for _, forbidden := range []string{"tls.crt", "tls.key", "ca.crt", "-----BEGIN"} {
				assert.NotContains(t, fmtUnstructured(u), forbidden,
					"edge object %s/%s must not embed cert/key material", u.GetKind(), u.GetName())
			}
		}
	}
}
