package builder_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestConnector() *otilmv1alpha1.Connector {
	return &otilmv1alpha1.Connector{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-connector",
			Namespace: "default",
		},
		Spec: otilmv1alpha1.ConnectorSpec{
			Image: otilmv1alpha1.ImageSpec{
				Repository: "docker.io/czertainly/czertainly-x509-compliance-provider",
				Tag:        "2.0.0",
			},
			Service: otilmv1alpha1.ServiceSpec{
				Port: 8080,
				Type: "ClusterIP",
			},
		},
	}
}

func TestLabels(t *testing.T) {
	conn := newTestConnector()
	labels := builder.Labels(conn)

	assert.Equal(t, "test-connector", labels["app.kubernetes.io/name"])
	assert.Equal(t, "ilm-operator", labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "connector", labels["app.kubernetes.io/component"])
	assert.Equal(t, conn.Name, labels["otilm.com/connector"])
}

func TestSelectorLabels(t *testing.T) {
	conn := newTestConnector()
	labels := builder.SelectorLabels(conn)

	assert.Equal(t, "test-connector", labels["app.kubernetes.io/name"])
	assert.Equal(t, "connector", labels["app.kubernetes.io/component"])
	assert.Equal(t, conn.Name, labels["otilm.com/connector"])
	_, exists := labels["app.kubernetes.io/managed-by"]
	assert.False(t, exists)
}

func TestChildResourceName(t *testing.T) {
	conn := newTestConnector()
	assert.Equal(t, "test-connector", builder.ChildResourceName(conn))
}
