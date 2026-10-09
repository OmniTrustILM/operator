/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// operatorPodName is the app.kubernetes.io/name label of the operator's own pod. The Helm chart,
// the kustomize manifests and the OLM bundle all set it.
const operatorPodName = "ilm-operator"

// OperatorPeers returns the NetworkPolicy peer that admits the operator's pods: pods labeled
// app.kubernetes.io/name=ilm-operator in operatorNamespace, matched by the namespace's
// kubernetes.io/metadata.name label. An empty operatorNamespace (an operator running outside a
// cluster) returns nil, so no operator is admitted.
func OperatorPeers(operatorNamespace string) []networkingv1.NetworkPolicyPeer {
	if operatorNamespace == "" {
		return nil
	}
	return []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: operatorNamespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{NameLabel: operatorPodName}},
	}}
}

// WorkloadNetworkPolicyEnabled reports whether a Connector or a Proxy renders its NetworkPolicy. A
// missing block or a missing enabled counts as true, the value the CRD defaults a present block to.
func WorkloadNetworkPolicyEnabled(np *otilmv1alpha1.WorkloadNetworkPolicySpec) bool {
	return np == nil || np.Enabled == nil || *np.Enabled
}

// BuildNetworkPolicy renders the ingress NetworkPolicy of a component: its pods accept traffic on
// the ports their Service targets, from pods in the same namespace and from the operator's pods.
// Egress is not restricted.
func BuildNetworkPolicy(c Component, operatorNamespace string) *networkingv1.NetworkPolicy {
	// An empty podSelector without a namespaceSelector matches every pod in the policy's namespace.
	from := append([]networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}, OperatorPeers(operatorNamespace)...)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: c.ResourceName(), Namespace: c.Namespace, Labels: c.Labels()},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: c.SelectorLabels()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: networkPolicyPorts(c.servicePorts()),
				From:  from,
			}},
		},
	}
}

// networkPolicyPorts maps Service ports to the pod ports they target. A missing target port is the
// Service port and a missing protocol is TCP, as the apiserver defaults them; setting both keeps the
// live policy equal to the rendered one, so the reconciler never rewrites it.
func networkPolicyPorts(servicePorts []corev1.ServicePort) []networkingv1.NetworkPolicyPort {
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(servicePorts))
	for _, sp := range servicePorts {
		target := sp.TargetPort
		if target.Type == intstr.Int && target.IntVal == 0 {
			target = intstr.FromInt32(sp.Port)
		}
		protocol := sp.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &target})
	}
	return ports
}
