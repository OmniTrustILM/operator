/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// BuildVolume renders one shared VolumeSpec into a pod Volume and the matching container
// VolumeMount, so every Kind that carries spec.volumes mounts the same shapes. A spec
// naming a PersistentVolumeClaim mounts that claim; any other spec renders an emptyDir.
//
// An unparsable emptyDir sizeLimit is logged and dropped rather than failing the render:
// the field is a cap, and losing it costs less than a workload that will not deploy.
func BuildVolume(v otilmv1alpha1.VolumeSpec) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{Name: v.Name}
	vm := corev1.VolumeMount{Name: v.Name, MountPath: v.MountPath}

	if v.PersistentVolumeClaim != nil {
		claim := &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.PersistentVolumeClaim.ClaimName}
		if v.PersistentVolumeClaim.ReadOnly != nil {
			claim.ReadOnly = *v.PersistentVolumeClaim.ReadOnly
			vm.ReadOnly = *v.PersistentVolumeClaim.ReadOnly
		}
		vol.VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: claim}
		return vol, vm
	}

	emptyDir := &corev1.EmptyDirVolumeSource{}
	if v.EmptyDir != nil {
		if v.EmptyDir.Medium != nil {
			emptyDir.Medium = corev1.StorageMedium(*v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit != nil {
			qty, err := resource.ParseQuantity(*v.EmptyDir.SizeLimit)
			if err != nil {
				log.Log.Info("invalid sizeLimit value, skipping", "volume", v.Name, "sizeLimit", *v.EmptyDir.SizeLimit, "error", err)
			} else {
				emptyDir.SizeLimit = &qty
			}
		}
	}
	vol.VolumeSource = corev1.VolumeSource{EmptyDir: emptyDir}

	return vol, vm
}
