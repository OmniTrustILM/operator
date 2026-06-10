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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkPolicy object names + the component-label role they carry. The role is a
// single shared value (networkPolicyRole) for all three policies — these are
// platform-WIDE objects (they do not belong to one workload component), so they use a
// dedicated role distinct from every workload role.
const (
	// networkPolicyRole is the app.kubernetes.io/component value (and label-selector key)
	// for all platform NetworkPolicies. Platform-wide, not per-workload.
	networkPolicyRole = "network-policy"

	// npDefaultDenyIngressName denies cross-namespace/external ingress to the platform's
	// pods while allowing intra-namespace ingress (the platform's own components).
	npDefaultDenyIngressName = "ilm-default-deny-ingress"
	// npAllowEdgeName allows ingress to the api-gateway from the ingress-controller /
	// Gateway namespace so the edge entrypoint keeps working.
	npAllowEdgeName = "ilm-allow-edge-to-gateway"
	// npAllowEgressName allows all egress from the platform's pods (DNS + intra-namespace +
	// DB/messaging/Keycloak). Permissive by default; tighter egress is a future knob.
	npAllowEgressName = "ilm-allow-egress"

	// defaultIngressNamespace is the well-known namespace the ingress-nginx controller
	// runs in; the edge-allow policy defaults to it when spec.networkPolicy.ingressNamespace
	// is empty. Overridable for a different ingress controller / Gateway implementation.
	defaultIngressNamespace = "ingress-nginx"

	// namespaceNameLabel is the immutable label every namespace carries
	// (kubernetes.io/metadata.name == the namespace name), set by the apiserver. The
	// edge-allow policy matches the ingress-controller namespace by it.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// networkPolicyEnabled reports whether the platform's default-deny NetworkPolicies are
// on. It is OPT-OUT: a nil spec.networkPolicy or a nil/true Enabled pointer means
// enabled (so an out-of-the-box platform is network-isolated); only an explicit
// enabled=false turns it off. Defaulting here (not relying solely on the CRD default)
// keeps RenderPlatform self-contained for tests and other render callers, which do not
// run apiserver defaulting.
func networkPolicyEnabled(p *otilmv1alpha1.Platform) bool {
	np := p.Spec.NetworkPolicy
	if np == nil {
		return true
	}
	return np.Enabled == nil || *np.Enabled
}

// ingressControllerNamespace returns the namespace the edge-allow policy permits ingress
// to the gateway from: spec.networkPolicy.ingressNamespace when set, otherwise the
// well-known ingress-nginx namespace.
func ingressControllerNamespace(p *otilmv1alpha1.Platform) string {
	if np := p.Spec.NetworkPolicy; np != nil && np.IngressNamespace != "" {
		return np.IngressNamespace
	}
	return defaultIngressNamespace
}

// networkPolicyLabels returns the standard recommended labels for a platform-wide
// NetworkPolicy. The instance is the Platform name; the component role is the shared
// networkPolicyRole (these are platform-wide, not per-workload, objects).
func networkPolicyLabels(p *otilmv1alpha1.Platform) map[string]string {
	return map[string]string{
		common.NameLabel:      networkPolicyRole,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: networkPolicyRole,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
}

// platformPodSelectorLabels is the label set that selects EVERY pod the platform owns:
// part-of=ilm scoped to this Platform instance. It matches the workload pod labels every
// component carries (see common.Component.Labels), so a NetworkPolicy using it applies to
// Core, the stateless services, and the gateway alike — and only to THIS platform's pods
// (instance-scoped, clean for the per-namespace singleton). It is workload-kind-agnostic
// (Deployments and StatefulSets share the same pod labels).
func platformPodSelectorLabels(p *otilmv1alpha1.Platform) map[string]string {
	return map[string]string{
		common.PartOfLabel:   common.PartOfValue,
		common.InstanceLabel: p.Name,
	}
}

// ResolveNetworkPolicies returns the platform's default-deny NetworkPolicies, or nil
// when spec.networkPolicy.enabled is false. The three policies are a SAFE default-deny —
// secure but unable to break intra-platform traffic (see NetworkPolicySpec):
//
//  1. default-deny ingress + intra-namespace allow (buildDefaultDenyIngress): the
//     high-value isolation — external/cross-namespace ingress is denied, the platform's
//     own components talk freely.
//  2. edge -> api-gateway allow (buildAllowEdgeToGateway): keeps the edge entrypoint
//     working (ingress to the gateway's consumer port from the ingress-controller
//     namespace).
//  3. permissive egress (buildAllowEgress): all egress allowed so managed-infra /
//     external DB/messaging/Keycloak connectivity is never broken; tighter egress is a
//     future hardening knob.
//
// All three are networking.k8s.io/v1 (a core, always-served API — no capability gating),
// owner-referenced + labeled by the controller's apply, and pruned like every other
// rendered child (networkpolicies are in the prune list + RBAC). They carry NO connection
// coordinates — only label selectors and the (non-secret) ingress-controller namespace.
func ResolveNetworkPolicies(p *otilmv1alpha1.Platform) []client.Object {
	if !networkPolicyEnabled(p) {
		return nil
	}
	return []client.Object{
		buildDefaultDenyIngress(p),
		buildAllowEdgeToGateway(p),
		buildAllowEgress(p),
	}
}

// networkPolicyMeta returns the ObjectMeta (name/namespace/labels) shared by every
// platform NetworkPolicy.
func networkPolicyMeta(p *otilmv1alpha1.Platform, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: p.Namespace,
		Labels:    networkPolicyLabels(p),
	}
}

// buildDefaultDenyIngress renders the ingress default-deny: it selects every platform pod
// and, because policyTypes includes Ingress, denies all ingress EXCEPT the single allowed
// source — pods in the SAME namespace (an empty podSelector under `from` matches all pods
// in the policy's namespace). So the platform's own components reach each other freely
// while cross-namespace / external ingress is denied. NetworkPolicies are additive, so the
// edge-allow policy layers the gateway's external entrypoint on top of this.
func buildDefaultDenyIngress(p *otilmv1alpha1.Platform) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: networkPolicyMeta(p, npDefaultDenyIngressName),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: platformPodSelectorLabels(p)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// Empty podSelector => all pods in THIS namespace (intra-platform). No
				// namespaceSelector, so cross-namespace traffic is NOT matched and is
				// therefore denied by the default-deny.
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{},
				}},
			}},
		},
	}
}

// buildAllowEdgeToGateway renders the edge -> api-gateway ingress allow: it selects the
// api-gateway pods and allows ingress to the gateway's consumer (proxy) port from pods in
// the ingress-controller / Gateway namespace (matched by the kubernetes.io/metadata.name
// namespace label). This keeps the external entrypoint working under the ingress
// default-deny. The namespace defaults to the well-known ingress-nginx namespace and is
// overridable via spec.networkPolicy.ingressNamespace. The TCP consumer port is pinned to
// the gateway's well-known consumer port.
func buildAllowEdgeToGateway(p *otilmv1alpha1.Platform) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	consumerPort := intstr.FromInt32(gatewayConsumerPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: networkPolicyMeta(p, npAllowEdgeName),
		Spec: networkingv1.NetworkPolicySpec{
			// Select the api-gateway pods only (name + instance — the gateway workload's
			// own selector labels), so this allow is scoped to the edge entrypoint.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				common.NameLabel:     gatewayName,
				common.InstanceLabel: p.Name,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp,
					Port:     &consumerPort,
				}},
				From: []networkingv1.NetworkPolicyPeer{{
					// Match the ingress-controller namespace by its immutable name label.
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						namespaceNameLabel: ingressControllerNamespace(p),
					}},
				}},
			}},
		},
	}
}

// buildAllowEgress renders the permissive egress policy: it selects every platform pod and
// allows ALL egress (an empty egress rule). This guarantees the ingress default-deny never
// breaks the platform's outbound connectivity — DNS, intra-namespace calls, and the
// managed-infra / external DB/messaging/Keycloak endpoints all keep working. The primary
// security win is the ingress isolation; tighter, allow-listed egress is a future
// hardening knob (it must not be turned on by default lest it break external connectivity).
func buildAllowEgress(p *otilmv1alpha1.Platform) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: networkPolicyMeta(p, npAllowEgressName),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: platformPodSelectorLabels(p)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			// A single empty egress rule allows all egress (to anywhere).
			Egress: []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
}
