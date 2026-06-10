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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// npByName fishes a typed NetworkPolicy out of a render set by name (or nil when absent).
func npByName(objs []client.Object, name string) *networkingv1.NetworkPolicy {
	for _, o := range objs {
		if np, ok := o.(*networkingv1.NetworkPolicy); ok && np.Name == name {
			return np
		}
	}
	return nil
}

// countNetworkPolicies counts the NetworkPolicy objects in a render set.
func countNetworkPolicies(objs []client.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := o.(*networkingv1.NetworkPolicy); ok {
			n++
		}
	}
	return n
}

// withNetworkPolicy returns a base platform with spec.networkPolicy set to np.
func withNetworkPolicy(np *otilmv1alpha1.NetworkPolicySpec) *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.NetworkPolicy = np
	return p
}

// TestNetworkPolicyDefaultOn asserts the default-deny set renders by default (opt-out):
// nil spec, a nil Enabled pointer, and an explicit enabled=true all render all three
// policies.
func TestNetworkPolicyDefaultOn(t *testing.T) {
	cases := []struct {
		name string
		spec *otilmv1alpha1.NetworkPolicySpec
	}{
		{"nil spec (default on)", nil},
		{"nil Enabled pointer (default on)", &otilmv1alpha1.NetworkPolicySpec{}},
		{"explicit enabled=true", &otilmv1alpha1.NetworkPolicySpec{Enabled: boolPtr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := ResolveNetworkPolicies(withNetworkPolicy(tc.spec))
			require.Equal(t, 3, countNetworkPolicies(objs),
				"default-on must render the ingress default-deny + edge-allow + egress policies")
			assert.NotNil(t, npByName(objs, npDefaultDenyIngressName))
			assert.NotNil(t, npByName(objs, npAllowEdgeName))
			assert.NotNil(t, npByName(objs, npAllowEgressName))
		})
	}
}

// TestNetworkPolicyDisabledRendersNone asserts that an explicit enabled=false renders no
// policies — the opt-out the prune then reclaims.
func TestNetworkPolicyDisabledRendersNone(t *testing.T) {
	objs := ResolveNetworkPolicies(withNetworkPolicy(&otilmv1alpha1.NetworkPolicySpec{Enabled: boolPtr(false)}))
	assert.Nil(t, objs, "disabled networkPolicy must render no policies")
	assert.Equal(t, 0, countNetworkPolicies(objs))
}

// TestNetworkPolicyDisabledPrunedFromRenderBase asserts the policies are absent from the
// full base render when disabled (so the post-apply prune reclaims any that exist) and
// present when enabled — the enable/disable transition the prune relies on.
func TestNetworkPolicyDisabledPrunedFromRenderBase(t *testing.T) {
	on := RenderPlatformBase(withNetworkPolicy(&otilmv1alpha1.NetworkPolicySpec{Enabled: boolPtr(true)}))
	assert.Equal(t, 3, countNetworkPolicies(on), "enabled => policies present in the base render")

	off := RenderPlatformBase(withNetworkPolicy(&otilmv1alpha1.NetworkPolicySpec{Enabled: boolPtr(false)}))
	assert.Equal(t, 0, countNetworkPolicies(off),
		"disabled => no policies in the base render, so the prune reclaims any that exist")
}

// TestNetworkPolicyLabels asserts every policy carries the standard recommended labels with
// the operator-only network-policy role and the platform instance — the selectors the prune
// (managed-by + instance) and role-based lookups (component role) key on.
func TestNetworkPolicyLabels(t *testing.T) {
	objs := ResolveNetworkPolicies(basePlatform())
	require.Equal(t, 3, len(objs))
	for _, o := range objs {
		np, ok := o.(*networkingv1.NetworkPolicy)
		require.True(t, ok)
		labels := np.GetLabels()
		assert.Equal(t, networkPolicyRole, labels[common.NameLabel], np.Name)
		assert.Equal(t, networkPolicyRole, labels[common.ComponentLabel], np.Name)
		assert.Equal(t, "ilm", labels[common.InstanceLabel], np.Name)
		assert.Equal(t, common.PartOfValue, labels[common.PartOfLabel], np.Name)
		assert.Equal(t, common.ManagedByValue, labels[common.ManagedByLabel], np.Name)
		assert.Equal(t, "ilm-system", np.Namespace, np.Name)
	}
}

// TestNetworkPolicyDefaultDenyIngressSelectsPlatformPods asserts the ingress default-deny
// selects EVERY platform pod (part-of=ilm scoped to the instance), is an Ingress-type
// policy, and allows ONLY intra-namespace ingress (an empty podSelector under `from`, with
// no namespaceSelector) so cross-namespace/external ingress is denied.
func TestNetworkPolicyDefaultDenyIngressSelectsPlatformPods(t *testing.T) {
	np := npByName(ResolveNetworkPolicies(basePlatform()), npDefaultDenyIngressName)
	require.NotNil(t, np)

	// Selects all platform pods, instance-scoped.
	assert.Equal(t, common.PartOfValue, np.Spec.PodSelector.MatchLabels[common.PartOfLabel])
	assert.Equal(t, "ilm", np.Spec.PodSelector.MatchLabels[common.InstanceLabel])
	// Must NOT scope to a single component (it covers the whole platform).
	assert.NotContains(t, np.Spec.PodSelector.MatchLabels, common.NameLabel,
		"the default-deny must select ALL platform pods, not one component")

	// Ingress-type only (egress is a separate, permissive policy).
	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, np.Spec.PolicyTypes)

	// Exactly one allowed source: same-namespace pods (empty podSelector, no
	// namespaceSelector). This is what makes it a SAFE default-deny.
	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].From, 1)
	peer := np.Spec.Ingress[0].From[0]
	require.NotNil(t, peer.PodSelector, "intra-namespace allow uses an (empty) podSelector")
	assert.Empty(t, peer.PodSelector.MatchLabels, "an empty podSelector matches all pods in THIS namespace")
	assert.Nil(t, peer.NamespaceSelector, "no namespaceSelector => cross-namespace ingress is denied")
	// No port restriction on the intra-namespace allow (components talk freely).
	assert.Empty(t, np.Spec.Ingress[0].Ports)
}

// TestNetworkPolicyAllowEdgeSelectsGateway asserts the edge-allow policy selects the
// api-gateway pods and allows ingress on the gateway consumer port from the ingress
// controller namespace (defaulting to ingress-nginx).
func TestNetworkPolicyAllowEdgeSelectsGateway(t *testing.T) {
	np := npByName(ResolveNetworkPolicies(basePlatform()), npAllowEdgeName)
	require.NotNil(t, np)

	// Selects the api-gateway workload only, instance-scoped.
	assert.Equal(t, gatewayName, np.Spec.PodSelector.MatchLabels[common.NameLabel])
	assert.Equal(t, "ilm", np.Spec.PodSelector.MatchLabels[common.InstanceLabel])

	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Ingress, 1)

	// Port: the gateway consumer (proxy) port over TCP.
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	port := np.Spec.Ingress[0].Ports[0]
	require.NotNil(t, port.Port)
	assert.Equal(t, int32(gatewayConsumerPort), port.Port.IntVal)

	// From: the ingress controller namespace by its immutable name label (default).
	require.Len(t, np.Spec.Ingress[0].From, 1)
	from := np.Spec.Ingress[0].From[0]
	require.NotNil(t, from.NamespaceSelector)
	assert.Equal(t, defaultIngressNamespace, from.NamespaceSelector.MatchLabels[namespaceNameLabel])
	assert.Nil(t, from.PodSelector, "the edge allow is scoped by namespace, not a podSelector")
}

// TestNetworkPolicyAllowEdgeRespectsIngressNamespaceOverride asserts the edge-allow policy
// honors spec.networkPolicy.ingressNamespace for a non-default ingress controller / Gateway.
func TestNetworkPolicyAllowEdgeRespectsIngressNamespaceOverride(t *testing.T) {
	p := withNetworkPolicy(&otilmv1alpha1.NetworkPolicySpec{IngressNamespace: "istio-system"})
	np := npByName(ResolveNetworkPolicies(p), npAllowEdgeName)
	require.NotNil(t, np)
	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].From, 1)
	require.NotNil(t, np.Spec.Ingress[0].From[0].NamespaceSelector)
	assert.Equal(t, "istio-system",
		np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels[namespaceNameLabel])
}

// TestNetworkPolicyEgressPermissive asserts the egress policy selects every platform pod,
// is an Egress-type policy, and allows ALL egress (a single empty egress rule) so
// managed-infra / external connectivity is never broken.
func TestNetworkPolicyEgressPermissive(t *testing.T) {
	np := npByName(ResolveNetworkPolicies(basePlatform()), npAllowEgressName)
	require.NotNil(t, np)

	assert.Equal(t, common.PartOfValue, np.Spec.PodSelector.MatchLabels[common.PartOfLabel])
	assert.Equal(t, "ilm", np.Spec.PodSelector.MatchLabels[common.InstanceLabel])

	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	// A single empty egress rule = allow all egress (no to/ports restriction).
	require.Len(t, np.Spec.Egress, 1)
	assert.Empty(t, np.Spec.Egress[0].To, "permissive egress: no destination restriction")
	assert.Empty(t, np.Spec.Egress[0].Ports, "permissive egress: no port restriction")
}

// TestNetworkPolicyAPIVersion guards the rendered GVK (networking.k8s.io/v1) is what the
// controller's Owns/prune/RBAC target.
func TestNetworkPolicyAPIVersion(t *testing.T) {
	for _, o := range ResolveNetworkPolicies(basePlatform()) {
		np, ok := o.(*networkingv1.NetworkPolicy)
		require.True(t, ok)
		// The typed object's scheme GVK is networking.k8s.io/v1 NetworkPolicy; the
		// controller's apply stamps TypeMeta before SSA. Assert the Go type is the v1 one.
		assert.IsType(t, &networkingv1.NetworkPolicy{}, np)
	}
}
