/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	networkingv1 "k8s.io/api/networking/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildNetworkPolicy constructs the Connector's NetworkPolicy via the shared Component builder: the
// connector's Service port accepts traffic only from pods in its namespace and from the operator's
// pods in operatorNamespace. Returns nil when spec.networkPolicy.enabled is false.
func BuildNetworkPolicy(conn *otilmv1alpha1.Connector, operatorNamespace string) *networkingv1.NetworkPolicy {
	if !common.WorkloadNetworkPolicyEnabled(conn.Spec.NetworkPolicy) {
		return nil
	}
	return common.BuildNetworkPolicy(component(conn, ""), operatorNamespace)
}
