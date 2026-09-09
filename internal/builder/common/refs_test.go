/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func TestBuildSecretRefEnvKeyMapping(t *testing.T) {
	b := BuildSecretRef(&otilmv1alpha1.SecretRef{
		Name: "s", Type: otilmv1alpha1.RefTypeEnv,
		Keys: []otilmv1alpha1.RefKeyMapping{
			{SecretKey: "password", EnvVar: strPtr("DB_PASSWORD")},
			{SecretKey: "user"}, // no EnvVar => env name is the key
		},
	})
	require.Len(t, b.Env, 2)
	assert.Equal(t, "DB_PASSWORD", b.Env[0].Name)
	assert.Equal(t, "password", b.Env[0].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, "s", b.Env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "user", b.Env[1].Name, "missing EnvVar defaults to the key name")
	assert.Empty(t, b.EnvFrom)
	assert.Empty(t, b.Volumes)
}

func TestBuildSecretRefWholeEnvFrom(t *testing.T) {
	b := BuildSecretRef(&otilmv1alpha1.SecretRef{Name: "bulk", Type: otilmv1alpha1.RefTypeEnv})
	assert.Empty(t, b.Env)
	require.Len(t, b.EnvFrom, 1)
	assert.Equal(t, "bulk", b.EnvFrom[0].SecretRef.Name)
}

func TestBuildSecretRefVolume(t *testing.T) {
	b := BuildSecretRef(&otilmv1alpha1.SecretRef{
		Name: "tls", Type: otilmv1alpha1.RefTypeVolume, MountPath: strPtr("/etc/tls"),
		Keys: []otilmv1alpha1.RefKeyMapping{
			{SecretKey: "tls.crt", Path: strPtr("server.crt")},
			{SecretKey: "tls.key"}, // no Path => path is the key
		},
	})
	require.Len(t, b.Volumes, 1)
	assert.Equal(t, "secret-tls", b.Volumes[0].Name)
	require.NotNil(t, b.Volumes[0].Secret)
	assert.Equal(t, "tls", b.Volumes[0].Secret.SecretName)
	require.Len(t, b.Volumes[0].Secret.Items, 2)
	assert.Equal(t, "server.crt", b.Volumes[0].Secret.Items[0].Path)
	assert.Equal(t, "tls.key", b.Volumes[0].Secret.Items[1].Path, "missing Path defaults to the key")
	require.Len(t, b.VolumeMounts, 1)
	assert.Equal(t, "/etc/tls", b.VolumeMounts[0].MountPath)
	assert.True(t, b.VolumeMounts[0].ReadOnly)
}

func TestBuildConfigMapRefEnvKeyMapping(t *testing.T) {
	b := BuildConfigMapRef(&otilmv1alpha1.ConfigMapRef{
		Name: "cm", Type: otilmv1alpha1.RefTypeEnv,
		Keys: []otilmv1alpha1.ConfigMapKeyMapping{
			{ConfigMapKey: "level", EnvVar: strPtr("LOG_LEVEL")},
			{ConfigMapKey: "mode"},
		},
	})
	require.Len(t, b.Env, 2)
	assert.Equal(t, "LOG_LEVEL", b.Env[0].Name)
	assert.Equal(t, "level", b.Env[0].ValueFrom.ConfigMapKeyRef.Key)
	assert.Equal(t, "mode", b.Env[1].Name)
}

func TestBuildConfigMapRefWholeEnvFrom(t *testing.T) {
	b := BuildConfigMapRef(&otilmv1alpha1.ConfigMapRef{Name: "cm", Type: otilmv1alpha1.RefTypeEnv})
	require.Len(t, b.EnvFrom, 1)
	assert.Equal(t, "cm", b.EnvFrom[0].ConfigMapRef.Name)
}

func TestBuildConfigMapRefVolume(t *testing.T) {
	b := BuildConfigMapRef(&otilmv1alpha1.ConfigMapRef{
		Name: "cfg", Type: otilmv1alpha1.RefTypeVolume, MountPath: strPtr("/etc/cfg"),
		Keys: []otilmv1alpha1.ConfigMapKeyMapping{{ConfigMapKey: "app.conf"}},
	})
	require.Len(t, b.Volumes, 1)
	assert.Equal(t, "configmap-cfg", b.Volumes[0].Name)
	require.NotNil(t, b.Volumes[0].ConfigMap)
	require.Len(t, b.VolumeMounts, 1)
	assert.Equal(t, "/etc/cfg", b.VolumeMounts[0].MountPath)
	assert.True(t, b.VolumeMounts[0].ReadOnly)
}

func TestBuildSecretRefNilMountPath(t *testing.T) {
	// A type=volume ref with no MountPath renders an empty mount path (the caller's
	// validation enforces presence; the renderer must not panic on a nil pointer).
	b := BuildSecretRef(&otilmv1alpha1.SecretRef{Name: "s", Type: otilmv1alpha1.RefTypeVolume})
	require.Len(t, b.VolumeMounts, 1)
	assert.Empty(t, b.VolumeMounts[0].MountPath)
}
