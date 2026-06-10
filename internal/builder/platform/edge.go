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
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Edge object names, sources, and cert-manager coordinates.
const (
	// edgeIngressRole is the component label the operator's Ingress carries (the
	// platform Core's edge identity).
	edgeIngressRole = "ilm-platform-core"
	// selfSignedIssuerName / caIssuerName / caCertificateName / caKeypairSecret are
	// the fixed cert-manager object names the operator renders for the internal
	// self-signed CA chain.
	selfSignedIssuerName = "selfsigned-issuer"
	caIssuerName         = "ca-issuer"
	caCertificateName    = "ca-certificate"
	caKeypairSecret      = "ca-keypair"
	// defaultEdgeTLSSecret is the default leaf TLS Secret cert-manager's ingress-shim
	// populates (internal/letsEncrypt) or the Ingress references directly
	// (bring-your-own). This is a Secret *name*, not a credential value.
	defaultEdgeTLSSecret = "ilm-ingress-tls" //nolint:gosec // G101: Secret name, not a hardcoded credential

	// Edge TLS sources (spec.edge.tls.source). "issuerRef" points the cert-manager
	// shim at a caller-provided Issuer/ClusterIssuer.
	edgeSourceInternal    = "internal"
	edgeSourceLetsEncrypt = "letsEncrypt"
	edgeSourceSecret      = "secret"
	edgeSourceIssuerRef   = "issuerRef"

	// ACME directory endpoints selected by letsEncrypt.environment.
	acmeProductionURL  = "https://acme-v02.api.letsencrypt.org/directory"
	acmeStagingURL     = "https://acme-staging-v02.api.letsencrypt.org/directory"
	letsEncryptEnvProd = "production"

	// cert-manager API coordinates. The operator renders cert-manager objects as
	// unstructured with this apiVersion preset (see ResolveEdge's package doc).
	certManagerAPIVersion = "cert-manager.io/v1"
	kindIssuer            = "Issuer"
	kindClusterIssuer     = "ClusterIssuer"
	kindCertificate       = "Certificate"
	// certManagerDefaultGroup is the default API group of a cert-manager
	// Issuer/ClusterIssuer; the issuer-group shim annotation is only emitted when the
	// caller's group differs (e.g. an external issuer like awspca.cert-manager.io).
	certManagerDefaultGroup = "cert-manager.io"

	// Gateway API coordinates (the GA, non-deprecated Ingress successor). The
	// operator renders Gateway/HTTPRoute as unstructured with this apiVersion preset
	// (same rationale as cert-manager: no typed Gateway API dependency, which would
	// pin an incompatible k8s.io graph; the SSA loop resolves the preset GVK). v1 is
	// the GA channel — Gateway and HTTPRoute both graduated to v1 in Gateway API
	// v1.0+, so the operator targets v1 rather than the older v1beta1.
	gatewayAPIVersion = "gateway.networking.k8s.io/v1"
	kindGateway       = "Gateway"
	kindHTTPRoute     = "HTTPRoute"
	// ownedGatewayName is the fixed name of the Gateway the operator renders when it
	// owns the edge (gatewayAPI with gatewayClassName set). Clean, edge-scoped name.
	ownedGatewayName = "platform-edge"
	// edgeGatewayRole is the component label the operator's Gateway/HTTPRoute carry.
	edgeGatewayRole = "platform-edge"
	// Gateway listener names/protocols/ports for the operator-owned Gateway.
	gatewayListenerHTTP      = "http"
	gatewayListenerHTTPS     = "https"
	gatewayProtocolHTTP      = "HTTP"
	gatewayProtocolHTTPS     = "HTTPS"
	gatewayPortHTTP          = int64(80)
	gatewayPortHTTPS         = int64(443)
	gatewayTLSModeTerminate  = "Terminate"
	gatewayRoutesFromSame    = "Same"
	gatewayCertRefKindSecret = "Secret"
	httpRoutePathPrefix      = "PathPrefix"

	// caCommonName / caPrivateKeyAlgorithm / caPrivateKeySize shape the internal
	// ca-certificate (isCA, commonName "ca", RSA 4096).
	caCommonName          = "ca"
	caPrivateKeyAlgorithm = "RSA"
	caPrivateKeySize      = int64(4096)

	// certManagerIssuerAnnotation / certManagerIssuerKindAnnotation are the
	// ingress-shim annotations stamped for internal/letsEncrypt so cert-manager
	// provisions the leaf cert into the Ingress tls Secret. The shim also honors
	// certManagerClusterIssuerAnnotation (for a ClusterIssuer instead of a namespaced
	// Issuer) and certManagerIssuerGroupAnnotation (for an external issuer group);
	// these are used by the issuerRef source. See the cert-manager ingress-shim docs:
	// https://cert-manager.io/docs/usage/ingress/#supported-annotations
	certManagerIssuerAnnotation        = "cert-manager.io/issuer"
	certManagerClusterIssuerAnnotation = "cert-manager.io/cluster-issuer"
	certManagerIssuerKindAnnotation    = "cert-manager.io/issuer-kind"
	certManagerIssuerGroupAnnotation   = "cert-manager.io/issuer-group"
)

// edgeTLSSecretName returns the TLS Secret name the Ingress references: the
// caller's edge.tls.secretRef when set, otherwise defaultEdgeTLSSecret.
func edgeTLSSecretName(tls *otilmv1alpha1.EdgeTLSSpec) string {
	if tls != nil && tls.SecretRef != nil && *tls.SecretRef != "" {
		return *tls.SecretRef
	}
	return defaultEdgeTLSSecret
}

// edgeSource returns the configured TLS source, defaulting to "internal" when TLS
// is unset or the source is empty.
func edgeSource(tls *otilmv1alpha1.EdgeTLSSpec) string {
	if tls == nil || tls.Source == "" {
		return edgeSourceInternal
	}
	return tls.Source
}

// isCertManagedSource reports whether the TLS source relies on cert-manager (via
// the ingress/gateway shim) to provision the leaf cert: internal and letsEncrypt
// (operator-created issuers) and issuerRef (the caller's issuer). The bring-your-own
// "secret" source does not. This drives EdgeDependencies' cert-manager gating and is
// distinct from whether the operator RENDERS a cert-manager object (issuerRef does
// not — see certManagerObjectsForSource).
func isCertManagedSource(source string) bool {
	switch source {
	case edgeSourceInternal, edgeSourceLetsEncrypt, edgeSourceIssuerRef:
		return true
	default:
		return false
	}
}

// certManagerShimAnnotations returns the cert-manager ingress/gateway-shim
// annotations to stamp on the Ingress / Gateway for the given edge TLS spec, so
// cert-manager provisions the leaf cert into the edge TLS Secret. The keys are
// operator-owned (reserved): a caller cannot override the issuer wiring (see
// reservedEdgeAnnotations / buildIngress's merge).
//
//   - internal / letsEncrypt: the operator-created CA/ACME issuer named ca-issuer
//     (Issuer kind).
//   - issuerRef: the caller's existing Issuer/ClusterIssuer. A namespaced Issuer
//     uses the cert-manager.io/issuer key; a ClusterIssuer uses the
//     cert-manager.io/cluster-issuer key. issuer-kind is always set; issuer-group
//     is set only when the group is non-default (e.g. an external issuer), since
//     the shim defaults the group to cert-manager.io.
//   - secret (bring-your-own) and any unknown source: no shim annotations — the
//     edge references the caller-provided Secret directly.
func certManagerShimAnnotations(tls *otilmv1alpha1.EdgeTLSSpec) map[string]string {
	switch edgeSource(tls) {
	case edgeSourceInternal, edgeSourceLetsEncrypt:
		// The operator-created ca-issuer (a namespaced Issuer) mints the leaf.
		return map[string]string{
			certManagerIssuerAnnotation:     caIssuerName,
			certManagerIssuerKindAnnotation: kindIssuer,
		}
	case edgeSourceIssuerRef:
		ref := tls.IssuerRef
		if ref == nil || ref.Name == "" {
			// Misconfigured (the webhook/CRD requires issuerRef.name for this
			// source); render no shim rather than a dangling annotation.
			return nil
		}
		kind := ref.Kind
		if kind == "" {
			kind = kindIssuer
		}
		ann := map[string]string{certManagerIssuerKindAnnotation: kind}
		// A ClusterIssuer is named via cert-manager.io/cluster-issuer; a namespaced
		// Issuer via cert-manager.io/issuer.
		if kind == kindClusterIssuer {
			ann[certManagerClusterIssuerAnnotation] = ref.Name
		} else {
			ann[certManagerIssuerAnnotation] = ref.Name
		}
		// Only emit issuer-group for a non-default (external) group; the shim
		// defaults it to cert-manager.io.
		if ref.Group != "" && ref.Group != certManagerDefaultGroup {
			ann[certManagerIssuerGroupAnnotation] = ref.Group
		}
		return ann
	default:
		// secret (BYO) and unknown sources carry no cert-manager wiring.
		return nil
	}
}

// reservedEdgeAnnotations is the set of cert-manager shim annotation keys the
// operator owns: a caller's edge.annotations may never override them, regardless of
// the TLS source. Listing every shim key (not just the ones the current source
// emits) keeps the issuer wiring tamper-proof — e.g. a caller cannot inject a
// cluster-issuer key to redirect an internal-source leaf.
var reservedEdgeAnnotations = map[string]struct{}{
	certManagerIssuerAnnotation:        {},
	certManagerClusterIssuerAnnotation: {},
	certManagerIssuerKindAnnotation:    {},
	certManagerIssuerGroupAnnotation:   {},
}

// edgeLabels returns the standard recommended labels for an edge object with the
// given component role. The instance is the Platform name.
func edgeLabels(p *otilmv1alpha1.Platform, role string) map[string]string {
	return map[string]string{
		common.NameLabel:      role,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: role,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
}

// ResolveEdge returns every Kubernetes object the platform's edge requires for the
// given Platform CR, or nil when the edge is absent/disabled. It branches on
// spec.edge.type:
//
//   - "ingress" (default/empty): a networking.k8s.io/v1 Ingress plus cert-manager
//     TLS objects (see resolveIngressEdge).
//   - "gatewayAPI": a gateway.networking.k8s.io/v1 HTTPRoute, optionally with an
//     operator-owned Gateway and cert-manager TLS objects (see resolveGatewayAPIEdge).
//
// Both paths route the configured host to the api-gateway Service consumer port and
// reuse the same TLS-source switch (internal self-signed CA chain / letsEncrypt ACME
// issuer / bring-your-own secret).
//
// The cert-manager and Gateway API objects are rendered as *unstructured.Unstructured
// with their apiVersion/kind preset. This avoids adding the heavy cert-manager and
// Gateway API Go modules as dependencies (they pin older k8s.io than this project)
// while remaining fully compatible with the controller's SSA apply loop:
// Scheme.ObjectKinds returns the preset GVK for an unstructured object even when the
// type is not registered.
//
// SECURITY: no certificate or private-key material is ever placed in these
// objects. cert-manager populates the TLS Secret; the operator only references
// Secrets by name. The Let's Encrypt account key lives in a cert-manager-managed
// Secret (letsencrypt-<env>) the operator never reads.
func ResolveEdge(p *otilmv1alpha1.Platform) []client.Object {
	edge := p.Spec.Edge
	if edge == nil || !edge.Enabled {
		return nil
	}
	if edge.Type == otilmv1alpha1EdgeTypeGatewayAPI {
		return resolveGatewayAPIEdge(p)
	}
	// Default/empty type and "ingress" both render the Ingress edge.
	return resolveIngressEdge(p)
}

// otilmv1alpha1EdgeTypeGatewayAPI is the spec.edge.type value selecting the Gateway
// API edge. It matches the CR enum literal "gatewayAPI".
const otilmv1alpha1EdgeTypeGatewayAPI = "gatewayAPI"

// Condition reasons surfaced by the reconciler when an edge dependency (an
// upstream operator / CRD bundle) is not served by the cluster. They are exported
// so the controller and its tests share one definition.
const (
	// ReasonCertManagerNotInstalled means the edge's TLS mode needs cert-manager
	// (cert-manager.io Certificate/Issuer) but the cluster does not serve it.
	ReasonCertManagerNotInstalled = "CertManagerNotInstalled"
	// ReasonGatewayAPINotInstalled means the edge type is gatewayAPI but the
	// cluster does not serve the Gateway API CRDs (gateway.networking.k8s.io).
	ReasonGatewayAPINotInstalled = "GatewayAPINotInstalled"
)

// CRDDependency is one upstream-operator / CRD-bundle prerequisite a gated feature
// needs in its current configuration: the API GroupKind to probe, the version(s) that
// satisfy it, and the condition Reason + actionable Message the reconciler surfaces
// when it is absent. Messages name only the spec field and the remedy — never any
// secret value or connection coordinate.
//
// It is mode-neutral: the edge (cert-manager / Gateway API), the admin bootstrap
// (cert-manager), and the managed database (CloudNativePG) all express their upstream
// prerequisites with it, and the controller's one gating path (applyGatedObjects)
// consumes it for all of them. The EdgeDependency alias preserves the original name used
// by the edge code and its tests.
type CRDDependency struct {
	// GroupKind is the API GroupKind to detect (e.g. cert-manager.io/Certificate).
	GroupKind schema.GroupKind
	// Versions are the acceptable served versions (e.g. "v1").
	Versions []string
	// Reason is the condition reason when this dep is absent.
	Reason string
	// Message is the actionable condition message when this dep is absent.
	Message string
}

// EdgeDependency is the original name of CRDDependency, kept as an alias so the edge
// builders, the admin builders, and their tests read unchanged. New (mode-neutral)
// code should prefer CRDDependency.
type EdgeDependency = CRDDependency

// certManagerEdgeDeps are the cert-manager prerequisites for the cert-managed TLS
// sources (internal / letsEncrypt / issuerRef): cert-manager must be served for the
// ingress/gateway shim to honor the issuer annotation and mint the leaf cert. For
// internal/letsEncrypt the operator also renders Issuer/Certificate objects only
// cert-manager reconciles; for issuerRef the operator renders no cert-manager object
// but still depends on cert-manager (the shim + the caller's issuer). The actionable
// message points at the bring-your-own escape hatch (tls.source=secret needs no
// cert-manager).
func certManagerEdgeDeps(source string) []EdgeDependency {
	msg := "edge tls.source=" + source + " requires cert-manager; install cert-manager " +
		"or set spec.edge.tls.source=secret to bring your own TLS Secret"
	return []EdgeDependency{
		{
			GroupKind: schema.GroupKind{Group: certManagerDefaultGroup, Kind: kindCertificate},
			Versions:  []string{"v1"},
			Reason:    ReasonCertManagerNotInstalled,
			Message:   msg,
		},
		{
			GroupKind: schema.GroupKind{Group: certManagerDefaultGroup, Kind: kindIssuer},
			Versions:  []string{"v1"},
			Reason:    ReasonCertManagerNotInstalled,
			Message:   msg,
		},
	}
}

// gatewayAPIEdgeDeps are the Gateway API CRD prerequisites for the gatewayAPI edge
// type: the operator renders Gateway and/or HTTPRoute objects served only when the
// Gateway API CRDs are installed.
func gatewayAPIEdgeDeps() []EdgeDependency {
	msg := "edge type=gatewayAPI requires the Gateway API CRDs (gateway.networking.k8s.io); " +
		"install the Gateway API CRDs or set spec.edge.type=ingress"
	return []EdgeDependency{
		{
			GroupKind: schema.GroupKind{Group: "gateway.networking.k8s.io", Kind: kindGateway},
			Versions:  []string{"v1"},
			Reason:    ReasonGatewayAPINotInstalled,
			Message:   msg,
		},
		{
			GroupKind: schema.GroupKind{Group: "gateway.networking.k8s.io", Kind: kindHTTPRoute},
			Versions:  []string{"v1"},
			Reason:    ReasonGatewayAPINotInstalled,
			Message:   msg,
		},
	}
}

// EdgeDependencies returns the upstream-operator / CRD-bundle prerequisites the
// edge needs given the Platform CR, or nil when the edge is absent/disabled or
// needs none. The reconciler probes each via the capability detector and gates the
// edge objects on their presence; the result mirrors exactly what ResolveEdge
// renders so the two never drift:
//
//   - ingress (default/empty) + tls.source internal|letsEncrypt|issuerRef →
//     cert-manager (issuerRef renders no operator cert object but still needs
//     cert-manager: the shim + the caller's Issuer/ClusterIssuer resolve it).
//   - ingress + tls.source secret (BYO) → none (the Ingress references the caller's
//     Secret; nothing beyond core networking is required).
//   - gatewayAPI → the Gateway API CRDs; AND, when the operator renders its OWN
//     Gateway (no parentRef) with a cert-managed TLS source → ALSO cert-manager
//     (the gateway-shim provisions the leaf cert). With a parentRef the operator
//     renders only the HTTPRoute, so TLS — and cert-manager — are the existing
//     Gateway's concern.
func EdgeDependencies(p *otilmv1alpha1.Platform) []EdgeDependency {
	edge := p.Spec.Edge
	if edge == nil || !edge.Enabled {
		return nil
	}
	source := edgeSource(edge.TLS)
	certManaged := isCertManagedSource(source)

	if edge.Type == otilmv1alpha1EdgeTypeGatewayAPI {
		deps := gatewayAPIEdgeDeps()
		// Only an operator-OWNED Gateway (no parentRef) renders cert-manager objects.
		gw := gatewayAPISpec(p)
		ownsGateway := gw == nil || gw.ParentRef == nil
		if ownsGateway && certManaged {
			deps = append(deps, certManagerEdgeDeps(source)...)
		}
		return deps
	}

	// Default/empty type and "ingress".
	if certManaged {
		return certManagerEdgeDeps(source)
	}
	return nil
}

// resolveIngressEdge renders the Ingress edge: a networking.k8s.io/v1 Ingress plus
// cert-manager objects matched to the TLS source:
//
//   - "internal": selfsigned-issuer (Issuer, selfSigned) + ca-certificate
//     (Certificate, isCA, into ca-keypair) + ca-issuer (Issuer, ca);
//   - "letsEncrypt": ca-issuer (Issuer, ACME) — the ingress-shim mints the leaf;
//   - "issuerRef": none — the caller's Issuer/ClusterIssuer mints the leaf via the
//     shim annotation; the operator creates no issuer object;
//   - "secret" (bring-your-own): none — the Ingress references the caller's Secret.
func resolveIngressEdge(p *otilmv1alpha1.Platform) []client.Object {
	objs := []client.Object{buildIngress(p)}
	return append(objs, certManagerObjectsForSource(p)...)
}

// certManagerObjectsForSource returns the cert-manager objects for the edge's TLS
// source, shared by the Ingress and Gateway API paths (the gateway-shim honors the
// same Issuer/issuer-kind annotations the ingress-shim does, so the issuer objects
// are identical). Returns nil for the bring-your-own ("secret") source.
func certManagerObjectsForSource(p *otilmv1alpha1.Platform) []client.Object {
	switch edgeSource(p.Spec.Edge.TLS) {
	case edgeSourceInternal:
		// Self-signed CA chain: selfsigned-issuer -> ca-certificate -> ca-issuer.
		// The shim then issues the leaf cert into the edge's TLS Secret.
		return []client.Object{
			buildSelfSignedIssuer(p),
			buildCACertificate(p),
			buildCAIssuer(p),
		}
	case edgeSourceLetsEncrypt:
		// ACME issuer only; the shim issues the leaf cert into the TLS Secret.
		return []client.Object{buildACMEIssuer(p)}
	case edgeSourceIssuerRef:
		// The caller owns the Issuer/ClusterIssuer: the operator creates no
		// cert-manager objects, it only stamps the shim annotation naming that
		// issuer (see certManagerShimAnnotations). cert-manager still provisions the
		// leaf into the edge TLS Secret. The cluster must serve cert-manager (gated
		// by EdgeDependencies) so the shim and the issuer resolve.
		return nil
	case edgeSourceSecret:
		// Bring-your-own: no cert-manager objects; the edge references the
		// caller-provided TLS Secret directly.
		return nil
	default:
		return nil
	}
}

// buildIngress renders the platform edge Ingress. The class is set as spec.ingressClassName
// (the modern, non-deprecated selector; the legacy kubernetes.io/ingress.class annotation is no
// longer emitted); the cert-manager ingress-shim annotations are added for the internal/
// letsEncrypt sources; the caller's annotations are merged last. The single rule routes all
// paths to the api-gateway Service consumer port.
func buildIngress(p *otilmv1alpha1.Platform) *networkingv1.Ingress {
	edge := p.Spec.Edge
	host := PlatformHost(p)

	// IngressClassName (spec.ingressClassName) is the modern, non-deprecated way to select the
	// ingress controller — it references an IngressClass of this name. Unset (nil) when no class
	// is configured, so the cluster's default IngressClass applies. The deprecated
	// kubernetes.io/ingress.class annotation is no longer emitted (the API server warns on it).
	var ingressClassName *string
	if edge.ClassName != nil && *edge.ClassName != "" {
		ingressClassName = edge.ClassName
	}

	annotations := map[string]string{}
	// cert-manager ingress-shim: internal/letsEncrypt point at the operator's
	// ca-issuer; issuerRef points at the caller's Issuer/ClusterIssuer; bring-your-own
	// (secret) carries no issuer annotation.
	for k, v := range certManagerShimAnnotations(edge.TLS) {
		annotations[k] = v
	}
	// Caller annotations merged last (after the class/cert-manager keys); a caller may
	// not override the cert-manager wiring.
	for k, v := range edge.Annotations {
		if _, reserved := reservedEdgeAnnotations[k]; reserved {
			continue
		}
		annotations[k] = v
	}

	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        p.Name + "-platform-core",
			Namespace:   p.Namespace,
			Labels:      edgeLabels(p, edgeIngressRole),
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ingressClassName,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: gatewayName,
									Port: networkingv1.ServiceBackendPort{Number: gatewayConsumerPort},
								},
							},
						}},
					},
				},
			}},
		},
	}

	// TLS block: the host (the cert SAN) plus the serving Secret name (cert-manager-populated
	// for internal/letsEncrypt, caller-provided for secret). Never any cert material. The SAN
	// is the platform's public FQDN (PlatformHost), so common.hostName supplies it when
	// edge.host is unset.
	ing.Spec.TLS = []networkingv1.IngressTLS{{
		Hosts:      []string{host},
		SecretName: edgeTLSSecretName(edge.TLS),
	}}
	return ing
}

// newEdgeUnstructured returns an unstructured edge object with apiVersion, kind,
// name, namespace, and labels populated for the given group apiVersion, role, and
// spec. It is the shared constructor behind both the cert-manager objects and the
// Gateway API objects: the controller's SSA apply loop resolves the preset GVK via
// Scheme.ObjectKinds even though neither type is registered in the scheme.
func newEdgeUnstructured(p *otilmv1alpha1.Platform, apiVersion, kind, name, role string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	u.SetNamespace(p.Namespace)
	u.SetLabels(edgeLabels(p, role))
	_ = unstructured.SetNestedMap(u.Object, spec, "spec")
	return u
}

// newCertManagerObject returns a cert-manager unstructured object with apiVersion,
// kind, name, namespace, and labels populated for the given role and spec.
func newCertManagerObject(p *otilmv1alpha1.Platform, kind, name, role string, spec map[string]interface{}) *unstructured.Unstructured {
	return newEdgeUnstructured(p, certManagerAPIVersion, kind, name, role, spec)
}

// buildSelfSignedIssuer renders the selfsigned-issuer (Issuer, selfSigned: {}),
// the root of the internal-CA chain (chart templates/selfsigned-issuer.yaml).
func buildSelfSignedIssuer(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindIssuer, selfSignedIssuerName, selfSignedIssuerName, map[string]interface{}{
		"selfSigned": map[string]interface{}{},
	})
}

// buildCACertificate renders the ca-certificate (Certificate, isCA, commonName
// "ca", RSA 4096, into the ca-keypair Secret, issued by selfsigned-issuer).
func buildCACertificate(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindCertificate, caCertificateName, "ca", map[string]interface{}{
		"isCA":       true,
		"commonName": caCommonName,
		"secretName": caKeypairSecret,
		"privateKey": map[string]interface{}{
			"algorithm": caPrivateKeyAlgorithm,
			"size":      caPrivateKeySize,
		},
		"issuerRef": map[string]interface{}{
			"name": selfSignedIssuerName,
			"kind": kindIssuer,
		},
	})
}

// buildCAIssuer renders the ca-issuer (Issuer, ca: {secretName: ca-keypair}) for
// the internal source — the issuer the ingress-shim uses to mint the leaf cert.
func buildCAIssuer(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindIssuer, caIssuerName, caIssuerName, map[string]interface{}{
		"ca": map[string]interface{}{
			"secretName": caKeypairSecret,
		},
	})
}

// buildACMEIssuer renders the ca-issuer (Issuer, acme: {...}) for the letsEncrypt
// source: the ACME directory selected by environment, the registered email, the
// account-key Secret name (letsencrypt-<env>, cert-manager-managed), and an http01
// solver referencing the ingress class. Matches templates/ingress/ca-issuer.yaml
// (letsencrypt branch).
func buildACMEIssuer(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	le := p.Spec.Edge.TLS.LetsEncrypt
	env := letsEncryptEnvProd
	server := acmeProductionURL
	email := ""
	if le != nil {
		if le.Environment != "" {
			env = le.Environment
		}
		if env != letsEncryptEnvProd {
			server = acmeStagingURL
		}
		email = le.Email
	}

	className := ""
	if p.Spec.Edge.ClassName != nil {
		className = *p.Spec.Edge.ClassName
	}

	return newCertManagerObject(p, kindIssuer, caIssuerName, caIssuerName, map[string]interface{}{
		"acme": map[string]interface{}{
			"server": server,
			"email":  email,
			"privateKeySecretRef": map[string]interface{}{
				"name": "letsencrypt-" + env,
			},
			"solvers": []interface{}{
				map[string]interface{}{
					"http01": map[string]interface{}{
						"ingress": map[string]interface{}{
							"class": className,
						},
					},
				},
			},
		},
	})
}

// resolveGatewayAPIEdge renders the Gateway API edge (gateway.networking.k8s.io/v1).
// It always renders an HTTPRoute. When edge.gatewayAPI.parentRef is set the operator
// attaches that single HTTPRoute to the existing Gateway and renders nothing else
// (TLS is the existing Gateway's concern — separation of concerns). Otherwise the
// operator owns the edge: it renders its own Gateway of the configured
// gatewayClassName (HTTP + HTTPS listeners, the cert-manager gateway-shim annotation
// for internal/letsEncrypt, and an HTTPS certificateRef to the edge TLS Secret) plus
// the same cert-manager objects the Ingress path uses for the TLS source.
func resolveGatewayAPIEdge(p *otilmv1alpha1.Platform) []client.Object {
	gw := gatewayAPISpec(p)

	// ParentRef path: only the HTTPRoute, attached to the existing Gateway. No
	// Gateway, no cert-manager objects.
	if gw != nil && gw.ParentRef != nil {
		return []client.Object{buildHTTPRoute(p, gw.ParentRef)}
	}

	// Owned-Gateway path: HTTPRoute (parentRef -> our Gateway) + Gateway + the TLS
	// source's cert-manager objects (gateway-shim provisions the leaf into the
	// certificateRef Secret for internal/letsEncrypt).
	objs := []client.Object{
		buildHTTPRoute(p, nil),
		buildOwnedGateway(p),
	}
	return append(objs, certManagerObjectsForSource(p)...)
}

// gatewayAPISpec returns the edge's GatewayAPI sub-spec, or nil when unset (the
// operator then defaults to owning a class-less Gateway, which a real cluster would
// reject — callers must set either gatewayClassName or parentRef).
func gatewayAPISpec(p *otilmv1alpha1.Platform) *otilmv1alpha1.GatewayAPISpec {
	return p.Spec.Edge.GatewayAPI
}

// buildHTTPRoute renders the HTTPRoute (gateway.networking.k8s.io/v1) routing the
// edge host to the api-gateway Service consumer port. When parent is nil the route
// attaches to the operator-owned Gateway (ownedGatewayName, same namespace); when
// set it attaches to the caller's existing Gateway (name + optional namespace +
// optional sectionName). The hostnames field is omitted when the platform host
// (PlatformHost — edge.host or common.hostName) is empty. The single rule matches
// PathPrefix "/" and forwards to api-gateway:8000.
func buildHTTPRoute(p *otilmv1alpha1.Platform, parent *otilmv1alpha1.GatewayParentRef) *unstructured.Unstructured {
	host := PlatformHost(p)

	var parentRef map[string]interface{}
	if parent == nil {
		// Owned Gateway: reference it by name in the same namespace.
		parentRef = map[string]interface{}{"name": ownedGatewayName}
	} else {
		parentRef = map[string]interface{}{"name": parent.Name}
		if parent.Namespace != nil && *parent.Namespace != "" {
			parentRef["namespace"] = *parent.Namespace
		}
		if parent.SectionName != nil && *parent.SectionName != "" {
			parentRef["sectionName"] = *parent.SectionName
		}
	}

	spec := map[string]interface{}{
		"parentRefs": []interface{}{parentRef},
		"rules": []interface{}{
			map[string]interface{}{
				"matches": []interface{}{
					map[string]interface{}{
						"path": map[string]interface{}{
							"type":  httpRoutePathPrefix,
							"value": "/",
						},
					},
				},
				"backendRefs": []interface{}{
					map[string]interface{}{
						"name": gatewayName,
						"port": int64(gatewayConsumerPort),
					},
				},
			},
		},
	}
	// Omit hostnames entirely when the platform host is empty (an empty hostname is invalid).
	if host != "" {
		spec["hostnames"] = []interface{}{host}
	}

	return newEdgeUnstructured(p, gatewayAPIVersion, kindHTTPRoute, p.Name+"-platform-core", edgeGatewayRole, spec)
}

// buildOwnedGateway renders the operator-owned Gateway (gateway.networking.k8s.io/v1)
// for the gatewayAPI edge with no parentRef. It has an HTTP listener (:80) and an
// HTTPS listener (:443, mode Terminate, certificateRef -> the edge TLS Secret), both
// scoped to same-namespace routes and pinned to the platform host (PlatformHost) when set. For the
// internal/letsEncrypt TLS sources it carries cert-manager's gateway-shim annotation
// (the same cert-manager.io/issuer + issuer-kind keys the Ingress uses), so
// cert-manager provisions the leaf cert into the certificateRef Secret. The
// gatewayClassName comes from gatewayAPI.gatewayClassName.
//
// SECURITY: no cert/key material is placed here; the HTTPS listener references the
// serving Secret by name only (cert-manager-populated or caller-provided).
func buildOwnedGateway(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	edge := p.Spec.Edge

	className := ""
	if gw := gatewayAPISpec(p); gw != nil && gw.GatewayClassName != nil {
		className = *gw.GatewayClassName
	}

	sameNamespaces := map[string]interface{}{
		"namespaces": map[string]interface{}{"from": gatewayRoutesFromSame},
	}

	httpListener := map[string]interface{}{
		"name":          gatewayListenerHTTP,
		"protocol":      gatewayProtocolHTTP,
		"port":          gatewayPortHTTP,
		"allowedRoutes": sameNamespaces,
	}
	httpsListener := map[string]interface{}{
		"name":     gatewayListenerHTTPS,
		"protocol": gatewayProtocolHTTPS,
		"port":     gatewayPortHTTPS,
		"tls": map[string]interface{}{
			"mode": gatewayTLSModeTerminate,
			"certificateRefs": []interface{}{
				map[string]interface{}{
					"kind": gatewayCertRefKindSecret,
					"name": edgeTLSSecretName(edge.TLS),
				},
			},
		},
		"allowedRoutes": sameNamespaces,
	}
	// Pin both listeners to the platform host (PlatformHost — edge.host or common.hostName)
	// when set (omit otherwise so the listener accepts any hostname — an empty string is not a
	// valid Gateway hostname).
	if host := PlatformHost(p); host != "" {
		httpListener["hostname"] = host
		httpsListener["hostname"] = host
	}

	u := newEdgeUnstructured(p, gatewayAPIVersion, kindGateway, ownedGatewayName, edgeGatewayRole, map[string]interface{}{
		"gatewayClassName": className,
		"listeners":        []interface{}{httpListener, httpsListener},
	})

	// cert-manager gateway-shim: internal/letsEncrypt point at the operator's
	// ca-issuer; issuerRef points at the caller's Issuer/ClusterIssuer; bring-your-own
	// (secret) carries no issuer annotation. The shim reads these annotations off the
	// Gateway and provisions into the certificateRef Secret.
	if shim := certManagerShimAnnotations(edge.TLS); len(shim) > 0 {
		u.SetAnnotations(shim)
	}
	return u
}
