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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildService(t *testing.T) {
	conn := newTestConnector()
	svc := connector.BuildService(conn)

	assert.Equal(t, testConnectorName, svc.Name)
	assert.Equal(t, "default", svc.Namespace)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)

	assert.Equal(t, connector.SelectorLabels(conn), svc.Spec.Selector)
	assert.Equal(t, connector.Labels(conn), svc.Labels)

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

	svc := connector.BuildService(conn)

	assert.Equal(t, corev1.ServiceTypeNodePort, svc.Spec.Type)

	assert.Len(t, svc.Spec.Ports, 1)
	port := svc.Spec.Ports[0]
	assert.Equal(t, "http", port.Name)
	assert.Equal(t, int32(9090), port.Port)
	assert.Equal(t, intstr.FromInt32(9090), port.TargetPort)
	assert.Equal(t, corev1.ProtocolTCP, port.Protocol)
}
