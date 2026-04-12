package builder_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildService(t *testing.T) {
	conn := newTestConnector()
	svc := builder.BuildService(conn)

	assert.Equal(t, "test-connector", svc.Name)
	assert.Equal(t, "default", svc.Namespace)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)

	assert.Equal(t, builder.SelectorLabels(conn), svc.Spec.Selector)
	assert.Equal(t, builder.Labels(conn), svc.Labels)

	assert.Len(t, svc.Spec.Ports, 1)
	port := svc.Spec.Ports[0]
	assert.Equal(t, "http", port.Name)
	assert.Equal(t, int32(8080), port.Port)
	assert.Equal(t, intstr.FromInt32(8080), port.TargetPort)
	assert.Equal(t, corev1.ProtocolTCP, port.Protocol)
}

func TestBuildServiceCustomPort(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Service = otilmv1alpha1.ServiceSpec{
		Port: 9090,
		Type: "NodePort",
	}

	svc := builder.BuildService(conn)

	assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)

	assert.Len(t, svc.Spec.Ports, 1)
	port := svc.Spec.Ports[0]
	assert.Equal(t, "http", port.Name)
	assert.Equal(t, int32(9090), port.Port)
	assert.Equal(t, intstr.FromInt32(9090), port.TargetPort)
	assert.Equal(t, corev1.ProtocolTCP, port.Protocol)
}
