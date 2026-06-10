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

package connector_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/connector"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testConnectorName = "test-connector"

func newTestConnector() *otilmv1alpha1.Connector {
	return &otilmv1alpha1.Connector{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testConnectorName,
			Namespace: "default",
		},
		Spec: otilmv1alpha1.ConnectorSpec{
			Image: otilmv1alpha1.ImageSpec{
				Repository: "hub.omnitrustregistry.com/ilm/x509-compliance-provider",
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
	labels := connector.Labels(conn)

	assert.Equal(t, testConnectorName, labels["app.kubernetes.io/name"])
	assert.Equal(t, "ilm-operator", labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "connector", labels["app.kubernetes.io/component"])
	assert.Equal(t, conn.Name, labels["otilm.com/connector"])
}

func TestSelectorLabels(t *testing.T) {
	conn := newTestConnector()
	labels := connector.SelectorLabels(conn)

	assert.Equal(t, testConnectorName, labels["app.kubernetes.io/name"])
	assert.Equal(t, "connector", labels["app.kubernetes.io/component"])
	assert.Equal(t, conn.Name, labels["otilm.com/connector"])
	_, exists := labels["app.kubernetes.io/managed-by"]
	assert.False(t, exists)
}

func TestChildResourceName(t *testing.T) {
	conn := newTestConnector()
	assert.Equal(t, testConnectorName, connector.ChildResourceName(conn))
}
