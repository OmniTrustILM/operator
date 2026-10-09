/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// testOperatorNamespace is the namespace the operator runs in for these tests.
const testOperatorNamespace = "ilm-operator-system"

func TestBuildNetworkPolicyOpensHTTPAndAPIPorts(t *testing.T) {
	px := newProxy()
	np := BuildNetworkPolicy(px, testOperatorNamespace)
	require.NotNil(t, np)

	assert.Equal(t, testProxyName, np.Name)
	assert.Equal(t, testProxyNamespace, np.Namespace)
	assert.Equal(t, Labels(px), np.Labels)
	assert.Equal(t, SelectorLabels(px), np.Spec.PodSelector.MatchLabels)

	require.Len(t, np.Spec.Ingress, 1)
	var ports []int32
	for _, p := range np.Spec.Ingress[0].Ports {
		ports = append(ports, p.Port.IntVal)
	}
	assert.Equal(t, []int32{8080, 8081}, ports)

	require.Len(t, np.Spec.Ingress[0].From, 2)
	operator := np.Spec.Ingress[0].From[1]
	require.NotNil(t, operator.NamespaceSelector)
	require.NotNil(t, operator.PodSelector)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": testOperatorNamespace}, operator.NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "ilm-operator"}, operator.PodSelector.MatchLabels)
}

func TestBuildNetworkPolicyNilWhenDisabled(t *testing.T) {
	px := newProxy()
	px.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{Enabled: ptr.To(false)}
	assert.Nil(t, BuildNetworkPolicy(px, testOperatorNamespace))

	px.Spec.NetworkPolicy = &otilmv1alpha1.WorkloadNetworkPolicySpec{}
	assert.NotNil(t, BuildNetworkPolicy(px, testOperatorNamespace), "a block without enabled renders the policy")
}
