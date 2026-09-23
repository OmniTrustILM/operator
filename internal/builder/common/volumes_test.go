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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func TestBuildVolumeEmptyDir(t *testing.T) {
	tests := []struct {
		name          string
		spec          otilmv1alpha1.VolumeSpec
		wantMedium    corev1.StorageMedium
		wantSizeLimit *resource.Quantity
	}{
		{
			name: "no source named renders an emptyDir",
			spec: otilmv1alpha1.VolumeSpec{Name: "scratch", MountPath: "/tmp"},
		},
		{
			name: "medium and size limit are carried over",
			spec: otilmv1alpha1.VolumeSpec{
				Name: "scratch", MountPath: "/tmp",
				EmptyDir: &otilmv1alpha1.EmptyDirSpec{Medium: ptr.To("Memory"), SizeLimit: ptr.To("64Mi")},
			},
			wantMedium:    corev1.StorageMediumMemory,
			wantSizeLimit: ptr.To(resource.MustParse("64Mi")),
		},
		{
			name: "an unparsable size limit is dropped, not fatal",
			spec: otilmv1alpha1.VolumeSpec{
				Name: "scratch", MountPath: "/tmp",
				EmptyDir: &otilmv1alpha1.EmptyDirSpec{SizeLimit: ptr.To("not-a-quantity")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vol, mount := BuildVolume(tt.spec)

			require.NotNil(t, vol.EmptyDir)
			assert.Nil(t, vol.PersistentVolumeClaim)
			assert.Equal(t, tt.wantMedium, vol.EmptyDir.Medium)
			assert.Equal(t, tt.wantSizeLimit, vol.EmptyDir.SizeLimit)
			assert.Equal(t, "scratch", vol.Name)
			assert.Equal(t, "scratch", mount.Name)
			assert.Equal(t, "/tmp", mount.MountPath)
			assert.False(t, mount.ReadOnly)
		})
	}
}

func TestBuildVolumePersistentVolumeClaim(t *testing.T) {
	t.Run("a claim is mounted by name", func(t *testing.T) {
		vol, mount := BuildVolume(otilmv1alpha1.VolumeSpec{
			Name: "hsm-state", MountPath: "/var/lib/hsm-state",
			PersistentVolumeClaim: &otilmv1alpha1.PVCSpec{ClaimName: "hsm-state"},
		})

		require.NotNil(t, vol.PersistentVolumeClaim)
		assert.Nil(t, vol.EmptyDir, "a claim replaces the emptyDir source")
		assert.Equal(t, "hsm-state", vol.PersistentVolumeClaim.ClaimName)
		assert.False(t, vol.PersistentVolumeClaim.ReadOnly)
		assert.Equal(t, "hsm-state", mount.Name, "a sidecar mounts it by this name")
		assert.Equal(t, "/var/lib/hsm-state", mount.MountPath)
		assert.False(t, mount.ReadOnly)
	})

	t.Run("readOnly reaches both the volume and the mount", func(t *testing.T) {
		vol, mount := BuildVolume(otilmv1alpha1.VolumeSpec{
			Name: "reference-data", MountPath: "/var/lib/reference",
			PersistentVolumeClaim: &otilmv1alpha1.PVCSpec{ClaimName: "reference", ReadOnly: ptr.To(true)},
		})

		require.NotNil(t, vol.PersistentVolumeClaim)
		assert.True(t, vol.PersistentVolumeClaim.ReadOnly)
		assert.True(t, mount.ReadOnly)
	})
}
