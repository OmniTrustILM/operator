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
