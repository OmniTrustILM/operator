/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	policyv1 "k8s.io/api/policy/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildPDB constructs a PodDisruptionBudget for the given Proxy via the shared
// Component builder (MinAvailable wins over MaxUnavailable; neither set defaults to
// minAvailable=1). Returns nil when no PDB is configured or it is disabled.
func BuildPDB(px *otilmv1alpha1.Proxy) *policyv1.PodDisruptionBudget {
	return common.BuildPodDisruptionBudget(component(px, ""), px.Spec.PodDisruptionBudget)
}
