/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildServiceExposesHTTPAndAPIPorts(t *testing.T) {
	svc := BuildService(newProxy())
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, SelectorLabels(newProxy()), svc.Spec.Selector)
	require.Len(t, svc.Spec.Ports, 2)

	byName := map[string]corev1.ServicePort{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p
	}
	assert.Equal(t, HTTPPort, byName["http"].Port)
	assert.Equal(t, HTTPPort, byName["http"].TargetPort.IntVal)
	assert.Equal(t, APIPort, byName["api"].Port)
	assert.Equal(t, APIPort, byName["api"].TargetPort.IntVal)
}
