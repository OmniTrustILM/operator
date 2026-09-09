/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	policyv1 "k8s.io/api/policy/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildPDB constructs a PodDisruptionBudget for the given Connector via the shared
// Component builder, which honours the MinAvailable/MaxUnavailable mutual exclusion
// (MinAvailable wins; neither set defaults to minAvailable=1). Returns nil when no
// PDB is configured or it is disabled.
func BuildPDB(conn *otilmv1alpha1.Connector) *policyv1.PodDisruptionBudget {
	if conn.Spec.Lifecycle == nil {
		return nil
	}
	return common.BuildPodDisruptionBudget(component(conn, ""), conn.Spec.Lifecycle.PodDisruptionBudget)
}
