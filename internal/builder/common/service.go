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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BuildService renders a Service for a component (defaults to ClusterIP). When the
// component sets ServicePorts, those are emitted verbatim (multi-port workloads such
// as the Kong gateway); otherwise a single "http" port targeting the named container
// port is used.
func BuildService(c Component) *corev1.Service {
	t := c.ServiceType
	if t == "" {
		t = corev1.ServiceTypeClusterIP
	}
	ports := c.ServicePorts
	if len(ports) == 0 {
		ports = []corev1.ServicePort{{Name: "http", Port: c.Port, TargetPort: intstr.FromString("http")}}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: c.ResourceName(), Namespace: c.Namespace, Labels: c.Labels()},
		Spec: corev1.ServiceSpec{
			Type:     t,
			Selector: c.SelectorLabels(),
			Ports:    ports,
		},
	}
}

// BuildServiceAccount renders a ServiceAccount for a component. Its name is the
// component's SAName (the ServiceAccountName override when set, otherwise the component
// name), and any ServiceAccountAnnotations are stamped on (e.g. a cloud
// workload-identity binding). No credentials are ever placed here.
func BuildServiceAccount(c Component) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        c.SAName(),
			Namespace:   c.Namespace,
			Labels:      c.Labels(),
			Annotations: c.ServiceAccountAnnotations,
		},
	}
}
