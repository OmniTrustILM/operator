/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildService(t *testing.T) {
	c := Component{Name: "core", Namespace: "ilm", Port: 8080, ServiceType: "ClusterIP"}
	s := BuildService(c)
	assert.Equal(t, "core", s.Name)
	assert.Equal(t, int32(8080), s.Spec.Ports[0].Port)
	assert.Equal(t, "core", s.Spec.Selector["app.kubernetes.io/name"])
}

func TestBuildServiceAccount(t *testing.T) {
	c := Component{Name: "core", Namespace: "ilm"}
	sa := BuildServiceAccount(c)
	assert.Equal(t, "core", sa.Name)
	assert.Equal(t, "ilm", sa.Namespace)
	assert.Equal(t, "core", sa.Labels["app.kubernetes.io/name"])
}

func TestBuildServiceExplicitPorts(t *testing.T) {
	// When ServicePorts is set, BuildService emits them verbatim (multi-port Kong).
	c := Component{
		Name: "api-gateway", Namespace: "ilm",
		ServicePorts: []corev1.ServicePort{
			{Name: "admin", Port: 8001, Protocol: corev1.ProtocolTCP},
			{Name: "consumer", Port: 8000, Protocol: corev1.ProtocolTCP},
			{Name: "status", Port: 8100, Protocol: corev1.ProtocolTCP},
		},
	}
	s := BuildService(c)
	require.Len(t, s.Spec.Ports, 3)
	got := map[string]int32{}
	for _, p := range s.Spec.Ports {
		got[p.Name] = p.Port
	}
	assert.Equal(t, map[string]int32{"admin": 8001, "consumer": 8000, "status": 8100}, got)
}

func TestBuildServiceTargetPortAndDefault(t *testing.T) {
	// TargetPort references the named container port "http".
	s := BuildService(Component{Name: "core", Namespace: "ilm", Port: 8080, ServiceType: "ClusterIP"})
	assert.Equal(t, intstr.FromString("http"), s.Spec.Ports[0].TargetPort)
	// Empty ServiceType defaults to ClusterIP.
	s2 := BuildService(Component{Name: "core", Namespace: "ilm", Port: 8080})
	assert.Equal(t, corev1.ServiceTypeClusterIP, s2.Spec.Type)
}
