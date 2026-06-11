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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// depGroupKinds collects the GroupKinds from a dependency slice for set assertions.
func depGroupKinds(deps []EdgeDependency) []schema.GroupKind {
	gks := make([]schema.GroupKind, 0, len(deps))
	for _, d := range deps {
		gks = append(gks, d.GroupKind)
	}
	return gks
}

var (
	gkCertificate = schema.GroupKind{Group: certManagerDefaultGroup, Kind: "Certificate"}
	gkIssuer      = schema.GroupKind{Group: certManagerDefaultGroup, Kind: "Issuer"}
	gkGateway     = schema.GroupKind{Group: testGatewayAPIGroup, Kind: "Gateway"}
	gkHTTPRoute   = schema.GroupKind{Group: testGatewayAPIGroup, Kind: "HTTPRoute"}
)

func TestEdgeDependenciesNilOrDisabled(t *testing.T) {
	t.Run("nil edge → no dependencies", func(t *testing.T) {
		assert.Nil(t, EdgeDependencies(basePlatform()))
	})

	t.Run("disabled edge → no dependencies", func(t *testing.T) {
		p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceLetsEncrypt})
		p.Spec.Edge.Enabled = false
		assert.Nil(t, EdgeDependencies(p))
	})
}

func TestEdgeDependenciesIngress(t *testing.T) {
	t.Run("internal → cert-manager Certificate + Issuer (v1)", func(t *testing.T) {
		deps := EdgeDependencies(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}))
		assert.ElementsMatch(t, []schema.GroupKind{gkCertificate, gkIssuer}, depGroupKinds(deps))
		for _, d := range deps {
			assert.Equal(t, ReasonCertManagerNotInstalled, d.Reason)
			assert.Equal(t, []string{"v1"}, d.Versions)
		}
	})

	t.Run("letsEncrypt → cert-manager Certificate + Issuer", func(t *testing.T) {
		deps := EdgeDependencies(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceLetsEncrypt}))
		assert.ElementsMatch(t, []schema.GroupKind{gkCertificate, gkIssuer}, depGroupKinds(deps))
	})

	t.Run("issuerRef → cert-manager Certificate + Issuer (same gating as internal/letsEncrypt)", func(t *testing.T) {
		// issuerRef renders no operator cert object but still needs cert-manager: the
		// shim + the caller's Issuer/ClusterIssuer must be resolvable.
		deps := EdgeDependencies(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer},
		}))
		assert.ElementsMatch(t, []schema.GroupKind{gkCertificate, gkIssuer}, depGroupKinds(deps))
		for _, d := range deps {
			assert.Equal(t, ReasonCertManagerNotInstalled, d.Reason)
		}
	})

	t.Run("default (nil TLS) defaults to internal → cert-manager", func(t *testing.T) {
		// edgeSource() defaults a nil/empty TLS source to "internal".
		deps := EdgeDependencies(edgePlatform(nil))
		assert.ElementsMatch(t, []schema.GroupKind{gkCertificate, gkIssuer}, depGroupKinds(deps))
	})

	t.Run("secret (BYO) → NO dependencies", func(t *testing.T) {
		deps := EdgeDependencies(edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceSecret, SecretRef: strPtr("my-tls"),
		}))
		assert.Empty(t, deps, "bring-your-own TLS needs neither cert-manager nor anything beyond core networking")
	})
}

func TestEdgeDependenciesGatewayAPI(t *testing.T) {
	t.Run("owned Gateway + internal → Gateway API CRDs AND cert-manager", func(t *testing.T) {
		deps := EdgeDependencies(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}))
		assert.ElementsMatch(t,
			[]schema.GroupKind{gkGateway, gkHTTPRoute, gkCertificate, gkIssuer},
			depGroupKinds(deps),
			"an operator-owned Gateway with a cert-managed source needs both the Gateway API CRDs and cert-manager")
	})

	t.Run("owned Gateway + secret (BYO) → Gateway API CRDs only", func(t *testing.T) {
		deps := EdgeDependencies(ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceSecret, SecretRef: strPtr("my-tls"),
		}))
		assert.ElementsMatch(t, []schema.GroupKind{gkGateway, gkHTTPRoute}, depGroupKinds(deps),
			"a BYO-TLS owned Gateway renders no cert-manager objects, so it needs only the Gateway API CRDs")
	})

	t.Run("parentRef + internal → Gateway API CRDs only (TLS is the existing Gateway's concern)", func(t *testing.T) {
		p := gatewayAPIPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal},
			&otilmv1alpha1.GatewayAPISpec{ParentRef: &otilmv1alpha1.GatewayParentRef{Name: "shared-gw"}})
		deps := EdgeDependencies(p)
		assert.ElementsMatch(t, []schema.GroupKind{gkGateway, gkHTTPRoute}, depGroupKinds(deps),
			"with a parentRef the operator renders only the HTTPRoute; cert-manager is not its concern")
		for _, d := range deps {
			assert.Equal(t, ReasonGatewayAPINotInstalled, d.Reason)
		}
	})
}

// objAnnotations returns a client.Object's annotations regardless of whether it is
// a typed object (Ingress) or unstructured (Gateway), for the drift-guard's
// shim-annotation scan.
func objAnnotations(o client.Object) map[string]string {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u.GetAnnotations()
	}
	return o.GetAnnotations()
}

// rendersCertManagerShim reports whether any rendered edge object carries a
// cert-manager ingress/gateway-shim annotation (issuer / cluster-issuer). This is
// the issuerRef justification for a cert-manager dependency that renders no
// cert-manager OBJECT — the shim still needs cert-manager to act on it.
func rendersCertManagerShim(objs []client.Object) bool {
	for _, o := range objs {
		ann := objAnnotations(o)
		if _, ok := ann[certManagerIssuerAnnotation]; ok {
			return true
		}
		if _, ok := ann[certManagerClusterIssuerAnnotation]; ok {
			return true
		}
	}
	return false
}

// renderedEdgeGroups reports whether the rendered edge objects include a cert-manager
// object and/or a Gateway API object (by API group).
func renderedEdgeGroups(objs []client.Object) (rendersCertManager, rendersGatewayAPI bool) {
	for _, o := range objs {
		switch o.GetObjectKind().GroupVersionKind().Group {
		case certManagerDefaultGroup:
			rendersCertManager = true
		case testGatewayAPIGroup:
			rendersGatewayAPI = true
		}
	}
	return rendersCertManager, rendersGatewayAPI
}

// requiredEdgeGroups reports whether the dependency GroupKinds require cert-manager and/or
// Gateway API (by API group).
func requiredEdgeGroups(gks []schema.GroupKind) (requiresCertManager, requiresGatewayAPI bool) {
	for _, gk := range gks {
		switch gk.Group {
		case certManagerDefaultGroup:
			requiresCertManager = true
		case testGatewayAPIGroup:
			requiresGatewayAPI = true
		}
	}
	return requiresCertManager, requiresGatewayAPI
}

// TestEdgeDependenciesMatchResolveEdge guards the invariant that the cert-manager /
// Gateway API dependency set never drifts from what ResolveEdge actually renders.
// The guard distinguishes "renders a cert-manager OBJECT" from "needs cert-manager":
// the issuerRef source renders NO cert-manager object yet still requires cert-manager
// (the shim + the caller's Issuer/ClusterIssuer must resolve). So the cert-manager
// dependency is checked in both directions:
//
//   - Soundness: if the edge renders a cert-manager object, cert-manager MUST be a
//     dependency (an object whose CRD is absent could never be applied).
//   - Justification: if cert-manager is a dependency, it MUST be backed by either a
//     rendered cert-manager object OR a cert-manager shim annotation on the edge —
//     never a spurious requirement nothing in the output needs.
//
// Gateway API stays a strict iff (the operator only renders Gateway/HTTPRoute, never
// just an annotation referencing them).
func TestEdgeDependenciesMatchResolveEdge(t *testing.T) {
	cases := map[string]*otilmv1alpha1.Platform{
		"ingress/internal":    edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}),
		"ingress/letsEncrypt": edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceLetsEncrypt}),
		"ingress/issuerRef": edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer},
		}),
		"ingress/issuerRef-cluster": edgePlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "corp-ca", Kind: kindClusterIssuer},
		}),
		"ingress/secret":    edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceSecret, SecretRef: strPtr("tls")}),
		"gw/owned/internal": ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}),
		"gw/owned/issuerRef": ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{
			Source: edgeSourceIssuerRef, IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer},
		}),
		"gw/owned/secret": ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceSecret, SecretRef: strPtr("tls")}),
		"gw/parentRef/internal": gatewayAPIPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal},
			&otilmv1alpha1.GatewayAPISpec{ParentRef: &otilmv1alpha1.GatewayParentRef{Name: "shared-gw"}}),
	}

	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			objs := ResolveEdge(p)
			deps := EdgeDependencies(p)

			rendersCertManager, rendersGatewayAPI := renderedEdgeGroups(objs)
			requiresCertManager, requiresGatewayAPI := requiredEdgeGroups(depGroupKinds(deps))

			// Soundness: a rendered cert-manager object implies the dependency.
			if rendersCertManager {
				assert.True(t, requiresCertManager,
					"a rendered cert-manager object must be backed by a cert-manager dependency")
			}
			// Justification: a cert-manager dependency must be backed by a rendered
			// object OR a shim annotation (the issuerRef case renders neither object).
			if requiresCertManager {
				assert.True(t, rendersCertManager || rendersCertManagerShim(objs),
					"a cert-manager dependency must be justified by a rendered cert-manager object or a shim annotation")
			}
			assert.Equal(t, rendersGatewayAPI, requiresGatewayAPI,
				"Gateway API dependency must be present iff a Gateway API object is rendered")
		})
	}
}

// TestEdgeDependencyMessagesNoLeak asserts the actionable messages name only the
// spec field and remedy — never a secret value or a connection coordinate.
func TestEdgeDependencyMessagesNoLeak(t *testing.T) {
	platforms := []*otilmv1alpha1.Platform{
		edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal}),
		edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceLetsEncrypt}),
		ownedGatewayPlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceLetsEncrypt}),
	}
	// edgePlatform sets Host "ilm.example.com" and db/mq hosts come from basePlatform.
	forbidden := []string{"ilm.example.com", "db.example.com", "mq.example.com", "db-creds", "mq-creds"}

	for _, p := range platforms {
		for _, d := range EdgeDependencies(p) {
			require.NotEmpty(t, d.Message, "every dependency must carry an actionable message")
			for _, s := range forbidden {
				assert.NotContains(t, d.Message, s,
					"dependency message must not leak a host/coordinate/secret name")
			}
			// The message should mention the offending field and the BYO/ingress remedy.
			assert.True(t,
				strings.Contains(d.Message, "spec.edge.tls.source=secret") ||
					strings.Contains(d.Message, "spec.edge.type=ingress"),
				"message must point at an actionable remedy: %q", d.Message)
		}
	}
}
