/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BuildPodDisruptionBudget renders a policy/v1 PodDisruptionBudget for the component,
// selecting the component's pods by its immutable selector labels. policy/v1 is a core,
// always-served API, so no capability gating is needed.
//
// It returns nil when the spec is nil or disabled, so callers can append the result
// unconditionally. MinAvailable and MaxUnavailable are mutually exclusive (a PDB carries
// at most one): MinAvailable wins when both are set; when neither is set the budget
// defaults to minAvailable=1 (tolerate losing all but one pod), the safe single-pod-
// guarding default that also mirrors the Connector builder.
func BuildPodDisruptionBudget(c Component, spec *otilmv1alpha1.PDBSpec) *policyv1.PodDisruptionBudget {
	if spec == nil || !spec.Enabled {
		return nil
	}

	pdbSpec := policyv1.PodDisruptionBudgetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: c.SelectorLabels()},
	}
	switch {
	case spec.MinAvailable != nil:
		pdbSpec.MinAvailable = spec.MinAvailable
	case spec.MaxUnavailable != nil:
		pdbSpec.MaxUnavailable = spec.MaxUnavailable
	default:
		defaultMin := intstr.FromInt32(1)
		pdbSpec.MinAvailable = &defaultMin
	}

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.ResourceName(),
			Namespace: c.Namespace,
			Labels:    c.Labels(),
		},
		Spec: pdbSpec,
	}
}
