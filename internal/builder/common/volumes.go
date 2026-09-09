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

// BuildEphemeralVolume renders one shared VolumeSpec (emptyDir) into a pod Volume and
// its main-container VolumeMount. An invalid sizeLimit is logged and skipped rather
// than failing the render (builders are pure and total).
func BuildEphemeralVolume(v otilmv1alpha1.VolumeSpec) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{Name: v.Name}
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
	vm := corev1.VolumeMount{
		Name:      v.Name,
		MountPath: v.MountPath,
	}
	return vol, vm
}
