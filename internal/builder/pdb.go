package builder

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BuildPDB constructs a PodDisruptionBudget for the given Connector.
// Returns nil if the lifecycle spec is not set, PDB is not configured, or PDB is disabled.
func BuildPDB(conn *otilmv1alpha1.Connector) *policyv1.PodDisruptionBudget {
	if conn.Spec.Lifecycle == nil {
		return nil
	}

	pdbSpec := conn.Spec.Lifecycle.PodDisruptionBudget
	if pdbSpec == nil {
		return nil
	}

	if !pdbSpec.Enabled {
		return nil
	}

	minAvailable := pdbSpec.MinAvailable
	if minAvailable == nil {
		defaultMin := intstr.FromInt32(1)
		minAvailable = &defaultMin
	}

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildResourceName(conn),
			Namespace: conn.Namespace,
			Labels:    Labels(conn),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: SelectorLabels(conn),
			},
			MinAvailable: minAvailable,
		},
	}
}
