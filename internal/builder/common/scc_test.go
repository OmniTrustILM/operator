/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// requireRestrictedV2 asserts that a Deployment's pod template satisfies the
// OpenShift "restricted-v2" SCC contract, so the workload is admitted without a
// custom SCC. It is reusable across component-builder tests as more platform
// components are added.
func requireRestrictedV2(t *testing.T, d *appsv1.Deployment) {
	t.Helper()
	requirePodTemplateRestrictedV2(t, d.Spec.Template)
}

// requirePodTemplateRestrictedV2 asserts the OpenShift "restricted-v2" SCC contract on a
// pod template directly, so the SAME hardening can be verified for either workload kind
// (Deployment or StatefulSet) — they share buildPodTemplateSpec, so the contract must hold
// identically across both.
func requirePodTemplateRestrictedV2(t *testing.T, tmpl corev1.PodTemplateSpec) {
	t.Helper()
	ps := tmpl.Spec

	require.NotNil(t, ps.SecurityContext, "pod securityContext must be set")
	require.NotNil(t, ps.SecurityContext.RunAsNonRoot)
	assert.True(t, *ps.SecurityContext.RunAsNonRoot, "pod must run as non-root")
	assert.Nil(t, ps.SecurityContext.RunAsUser, "must not pin a UID; the SCC assigns it from the namespace range")
	require.NotNil(t, ps.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, ps.SecurityContext.SeccompProfile.Type)
	assert.False(t, ps.HostNetwork, "hostNetwork is forbidden")
	assert.False(t, ps.HostPID, "hostPID is forbidden")
	assert.False(t, ps.HostIPC, "hostIPC is forbidden")
	for _, v := range ps.Volumes {
		assert.Nil(t, v.HostPath, "hostPath volumes are forbidden under restricted-v2")
	}

	require.NotEmpty(t, ps.Containers)
	all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	for _, c := range all {
		sc := c.SecurityContext
		require.NotNil(t, sc, "container %q securityContext must be set", c.Name)
		require.NotNil(t, sc.RunAsNonRoot)
		assert.True(t, *sc.RunAsNonRoot, "container %q must run as non-root", c.Name)
		assert.Nil(t, sc.RunAsUser, "container %q must not pin a UID", c.Name)
		require.NotNil(t, sc.AllowPrivilegeEscalation)
		assert.False(t, *sc.AllowPrivilegeEscalation, "container %q must not allow privilege escalation", c.Name)
		if sc.Privileged != nil {
			assert.False(t, *sc.Privileged, "container %q must not be privileged", c.Name)
		}
		require.NotNil(t, sc.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "container %q must drop ALL capabilities", c.Name)
		assert.Empty(t, sc.Capabilities.Add, "container %q must not add capabilities", c.Name)
		require.NotNil(t, sc.SeccompProfile)
		assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
		for _, port := range c.Ports {
			assert.Zero(t, port.HostPort, "container %q must not use hostPort", c.Name)
		}
	}
}

func TestBuildDeploymentRestrictedV2Compliant(t *testing.T) {
	d := BuildDeployment(Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Env:       []EnvPair{{Name: "FOO", Value: "bar"}},
		SecretEnv: []SecretEnvRef{{EnvVar: "PW", SecretName: "s", SecretKey: "password"}},
	})
	requireRestrictedV2(t, d)
}

// TestBuildDeploymentHardensInitAndSidecarContainers verifies the SCC contract is
// applied to init containers and sidecars (not only the main container) when they
// carry no SecurityContext of their own. This is the path Core relies on for its
// wait-for-auth / provision-instance-queue init containers and OPA sidecar.
func TestBuildDeploymentHardensInitAndSidecarContainers(t *testing.T) {
	d := BuildDeployment(Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		InitContainers: []corev1.Container{
			{Name: "wait", Image: "curl:1", Command: []string{"/bin/sh", "-c", "true"}},
			{Name: "provision", Image: "curl:1", Command: []string{"/bin/sh", "-c", "true"}},
		},
		Sidecars: []corev1.Container{{Name: "opa", Image: "opa:1"}},
	})

	require.Len(t, d.Spec.Template.Spec.InitContainers, 2)
	requireRestrictedV2(t, d) // iterates BOTH init and main/sidecar containers
}

// TestBuildDeploymentPreservesCallerSecurityContext ensures a caller-supplied
// container SecurityContext is left untouched (hardenContainer only fills the gap).
func TestBuildDeploymentPreservesCallerSecurityContext(t *testing.T) {
	custom := &corev1.SecurityContext{RunAsNonRoot: boolPtr(true)}
	d := BuildDeployment(Component{
		Name: "x", Namespace: "ilm", Image: "x:1", Port: 8080,
		InitContainers: []corev1.Container{{Name: "i", Image: "i:1", SecurityContext: custom}},
	})
	got := d.Spec.Template.Spec.InitContainers[0].SecurityContext
	assert.Same(t, custom, got, "a caller-supplied init-container SecurityContext must be preserved")
}

func boolPtr(b bool) *bool { return &b }
