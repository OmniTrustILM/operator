/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	networkingv1 "k8s.io/api/networking/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildNetworkPolicy constructs the Proxy's NetworkPolicy via the shared Component builder: the
// proxy's http and api ports accept traffic only from pods in its namespace and from the
// operator's pods in operatorNamespace. Returns nil when spec.networkPolicy.enabled is false.
func BuildNetworkPolicy(px *otilmv1alpha1.Proxy, operatorNamespace string) *networkingv1.NetworkPolicy {
	if !common.WorkloadNetworkPolicyEnabled(px.Spec.NetworkPolicy) {
		return nil
	}
	return common.BuildNetworkPolicy(component(px, ""), operatorNamespace)
}
