package builder

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BuildService constructs a Service for the given Connector.
func BuildService(conn *otilmv1alpha1.Connector) *corev1.Service {
	port := conn.Spec.Service.Port

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildResourceName(conn),
			Namespace: conn.Namespace,
			Labels:    Labels(conn),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceType(conn.Spec.Service.Type),
			Selector: SelectorLabels(conn),
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
