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
