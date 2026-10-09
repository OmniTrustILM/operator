/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: c.ResourceName(), Namespace: c.Namespace, Labels: c.Labels()},
		Spec: corev1.ServiceSpec{
			Type:     t,
			Selector: c.SelectorLabels(),
			Ports:    c.servicePorts(),
		},
	}
}

// servicePorts returns the ports of the component's Service: ServicePorts when set, otherwise a
// single "http" port targeting the named container port. BuildService and BuildNetworkPolicy both
// use it, so a NetworkPolicy opens exactly the ports its Service targets.
func (c Component) servicePorts() []corev1.ServicePort {
	if len(c.ServicePorts) > 0 {
		return c.ServicePorts
	}
	return []corev1.ServicePort{{Name: "http", Port: c.Port, TargetPort: intstr.FromString("http")}}
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
