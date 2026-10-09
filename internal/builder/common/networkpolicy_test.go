/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// testOperatorNamespace is the namespace the operator runs in for these tests.
const testOperatorNamespace = "ilm-operator-system"

func TestOperatorPeers(t *testing.T) {
	assert.Nil(t, OperatorPeers(""), "an operator outside a cluster has no namespace to admit")

	peers := OperatorPeers(testOperatorNamespace)
	require.Len(t, peers, 1)
	require.NotNil(t, peers[0].NamespaceSelector)
	require.NotNil(t, peers[0].PodSelector)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": testOperatorNamespace},
		peers[0].NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "ilm-operator"}, peers[0].PodSelector.MatchLabels)
	assert.Nil(t, peers[0].IPBlock)
}

func TestWorkloadNetworkPolicyEnabled(t *testing.T) {
	cases := []struct {
		name string
		spec *otilmv1alpha1.WorkloadNetworkPolicySpec
		want bool
	}{
		{"absent block", nil, true},
		{"block without enabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{}, true},
		{"enabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(true)}, true},
		{"disabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, WorkloadNetworkPolicyEnabled(tc.spec))
		})
	}
}

func TestBuildNetworkPolicy(t *testing.T) {
	c := Component{
		Name:                   "x509",
		Namespace:              "ilm",
		Port:                   8080,
		LabelsOverride:         map[string]string{NameLabel: "x509", ManagedByLabel: ManagedByValue},
		SelectorLabelsOverride: map[string]string{NameLabel: "x509"},
	}
	np := BuildNetworkPolicy(c, testOperatorNamespace)

	assert.Equal(t, "x509", np.Name)
	assert.Equal(t, "ilm", np.Namespace)
	assert.Equal(t, c.Labels(), np.Labels)
	assert.Equal(t, c.SelectorLabels(), np.Spec.PodSelector.MatchLabels)
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, np.Spec.PolicyTypes)
	assert.Empty(t, np.Spec.Egress, "egress is not restricted")

	require.Len(t, np.Spec.Ingress, 1)
	from := np.Spec.Ingress[0].From
	require.Len(t, from, 2)
	require.NotNil(t, from[0].PodSelector)
	assert.Empty(t, from[0].PodSelector.MatchLabels, "an empty podSelector admits every pod in the namespace")
	assert.Nil(t, from[0].NamespaceSelector, "without a namespaceSelector the first peer stays in the namespace")
	assert.Equal(t, OperatorPeers(testOperatorNamespace), from[1:])
}

func TestBuildNetworkPolicyWithoutOperatorNamespace(t *testing.T) {
	np := BuildNetworkPolicy(Component{Name: "x509", Namespace: "ilm", Port: 8080}, "")

	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].From, 1, "only the namespace is admitted")
	assert.Nil(t, np.Spec.Ingress[0].From[0].NamespaceSelector)
}

func TestBuildNetworkPolicyPorts(t *testing.T) {
	tcp := ptr.To(corev1.ProtocolTCP)
	cases := []struct {
		name  string
		ports []corev1.ServicePort
		want  []networkingv1.NetworkPolicyPort
	}{
		{
			name: "the default Service port targets the named http container port",
			want: []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: ptr.To(intstr.FromString("http"))}},
		},
		{
			name:  "a numeric target port",
			ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP}},
			want:  []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: ptr.To(intstr.FromInt32(8080))}},
		},
		{
			name:  "an unset target port is the Service port and an unset protocol is TCP",
			ports: []corev1.ServicePort{{Name: "https", Port: 8443}},
			want:  []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: ptr.To(intstr.FromInt32(8443))}},
		},
		{
			name: "every port of a multi-port Service, each with its protocol",
			ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
				{Name: "dns", Port: 53, TargetPort: intstr.FromInt32(5353), Protocol: corev1.ProtocolUDP},
			},
			want: []networkingv1.NetworkPolicyPort{
				{Protocol: tcp, Port: ptr.To(intstr.FromInt32(8080))},
				{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(5353))},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Component{Name: "x509", Namespace: "ilm", Port: 8080, ServicePorts: tc.ports}
			np := BuildNetworkPolicy(c, "")
			require.Len(t, np.Spec.Ingress, 1)
			assert.Equal(t, tc.want, np.Spec.Ingress[0].Ports)
			assert.Len(t, np.Spec.Ingress[0].Ports, len(BuildService(c).Spec.Ports),
				"the policy opens exactly the ports the Service targets")
		})
	}
}
