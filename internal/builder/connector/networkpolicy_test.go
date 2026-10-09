/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/connector"
)

// testOperatorNamespace is the namespace the operator runs in for these tests.
const testOperatorNamespace = "ilm-operator-system"

func TestBuildNetworkPolicy(t *testing.T) {
	conn := newTestConnector()
	np := connector.BuildNetworkPolicy(conn, testOperatorNamespace)
	require.NotNil(t, np)

	assert.Equal(t, testConnectorName, np.Name)
	assert.Equal(t, "default", np.Namespace)
	assert.Equal(t, connector.Labels(conn), np.Labels)
	assert.Equal(t, connector.SelectorLabels(conn), np.Spec.PodSelector.MatchLabels)

	require.Len(t, np.Spec.Ingress, 1)
	rule := np.Spec.Ingress[0]
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, ptr.To(corev1.ProtocolTCP), rule.Ports[0].Protocol)
	assert.Equal(t, ptr.To(intstr.FromInt32(8080)), rule.Ports[0].Port)

	require.Len(t, rule.From, 2)
	operator := rule.From[1]
	require.NotNil(t, operator.NamespaceSelector)
	require.NotNil(t, operator.PodSelector)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": testOperatorNamespace}, operator.NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "ilm-operator"}, operator.PodSelector.MatchLabels)
}

func TestBuildNetworkPolicyFollowsServicePort(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Service.Port = 9443

	np := connector.BuildNetworkPolicy(conn, testOperatorNamespace)
	require.NotNil(t, np)
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	assert.Equal(t, ptr.To(intstr.FromInt32(9443)), np.Spec.Ingress[0].Ports[0].Port)
}

func TestBuildNetworkPolicySwitch(t *testing.T) {
	cases := []struct {
		name     string
		spec     *otilmv1alpha1.WorkloadNetworkPolicySpec
		rendered bool
	}{
		{"absent block", nil, true},
		{"block without enabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{}, true},
		{"enabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(true)}, true},
		{"disabled", &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newTestConnector()
			conn.Spec.NetworkPolicy = tc.spec
			assert.Equal(t, tc.rendered, connector.BuildNetworkPolicy(conn, testOperatorNamespace) != nil)
		})
	}
}
