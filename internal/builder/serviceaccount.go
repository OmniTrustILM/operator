package builder

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildServiceAccount constructs a ServiceAccount for the given Connector.
func BuildServiceAccount(conn *otilmv1alpha1.Connector) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildResourceName(conn),
			Namespace: conn.Namespace,
			Labels:    Labels(conn),
		},
	}
}
