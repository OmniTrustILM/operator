/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// RefBindings is the rendered result of projecting one SecretRef / ConfigMapRef into a
// workload: container env vars (keyed valueFrom references), whole-source envFrom
// sources, pod volumes, and the matching container volume mounts. The operator projects
// every value BY REFERENCE — it never copies a Secret/ConfigMap value into the rendered
// objects. It is the single, CRD-agnostic key-mapping renderer shared by the Connector
// reconciler and the Platform component override surface, so both render identical
// secretKeyRef / configMapKeyRef / volume shapes from the same spec types.
type RefBindings struct {
	// Env are keyed container env vars (valueFrom secretKeyRef / configMapKeyRef).
	Env []corev1.EnvVar
	// EnvFrom are whole-Secret / whole-ConfigMap env sources (type=env with no keys).
	EnvFrom []corev1.EnvFromSource
	// Volumes are the pod volumes for a type=volume ref.
	Volumes []corev1.Volume
	// VolumeMounts are the container volume mounts paired with Volumes (read-only).
	VolumeMounts []corev1.VolumeMount
}

// derefString returns the pointed-to string, or "" when the pointer is nil.
func derefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

// BuildSecretRef renders one SecretRef into its env / envFrom / volume bindings. For
// type=env it projects every key via envFrom (no key mapping) or, when keys are listed,
// one keyed secretKeyRef per key (the env var name is the mapped EnvVar or the key
// itself). For type=volume it mounts the Secret read-only at MountPath, projecting the
// listed keys to their mapped paths (or the key name). Values are referenced, never copied.
//
//nolint:dupl // SecretRef and ConfigMapRef handle different K8s types; deduplication would hurt clarity.
func BuildSecretRef(sr *otilmv1alpha1.SecretRef) RefBindings {
	var b RefBindings
	switch sr.Type {
	case otilmv1alpha1.RefTypeEnv:
		buildSecretEnvBindings(sr, &b)
	case otilmv1alpha1.RefTypeVolume:
		buildSecretVolumeBindings(sr, &b)
	}
	return b
}

// buildSecretEnvBindings projects a type=env SecretRef: every key via envFrom (no key
// mapping) or, when keys are listed, one keyed secretKeyRef per key (the env var name is the
// mapped EnvVar or the key itself).
//
//nolint:dupl // Secret and ConfigMap env bindings handle different K8s types; deduplication would hurt clarity.
func buildSecretEnvBindings(sr *otilmv1alpha1.SecretRef, b *RefBindings) {
	if len(sr.Keys) == 0 {
		b.EnvFrom = append(b.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: sr.Name},
			},
		})
		return
	}
	for _, k := range sr.Keys {
		envName := k.SecretKey
		if k.EnvVar != nil {
			envName = *k.EnvVar
		}
		b.Env = append(b.Env, corev1.EnvVar{
			Name: envName,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sr.Name},
					Key:                  k.SecretKey,
				},
			},
		})
	}
}

// buildSecretVolumeBindings mounts a type=volume SecretRef read-only at MountPath,
// projecting the listed keys to their mapped paths (or the key name).
func buildSecretVolumeBindings(sr *otilmv1alpha1.SecretRef, b *RefBindings) {
	volName := fmt.Sprintf("secret-%s", sr.Name)
	secretVol := &corev1.SecretVolumeSource{SecretName: sr.Name}
	for _, k := range sr.Keys {
		path := k.SecretKey
		if k.Path != nil {
			path = *k.Path
		}
		secretVol.Items = append(secretVol.Items, corev1.KeyToPath{Key: k.SecretKey, Path: path})
	}
	b.Volumes = append(b.Volumes, corev1.Volume{
		Name:         volName,
		VolumeSource: corev1.VolumeSource{Secret: secretVol},
	})
	b.VolumeMounts = append(b.VolumeMounts, corev1.VolumeMount{
		Name:      volName,
		MountPath: derefString(sr.MountPath),
		ReadOnly:  true,
	})
}

// BuildConfigMapRef renders one ConfigMapRef into its env / envFrom / volume bindings,
// mirroring BuildSecretRef for ConfigMaps (configMapKeyRef / whole-ConfigMap envFrom /
// read-only volume mount). Values are referenced, never copied.
//
//nolint:dupl // ConfigMapRef and SecretRef handle different K8s types; deduplication would hurt clarity.
func BuildConfigMapRef(cmr *otilmv1alpha1.ConfigMapRef) RefBindings {
	var b RefBindings
	switch cmr.Type {
	case otilmv1alpha1.RefTypeEnv:
		buildConfigMapEnvBindings(cmr, &b)
	case otilmv1alpha1.RefTypeVolume:
		buildConfigMapVolumeBindings(cmr, &b)
	}
	return b
}

// buildConfigMapEnvBindings projects a type=env ConfigMapRef: every key via envFrom (no key
// mapping) or, when keys are listed, one keyed configMapKeyRef per key (the env var name is
// the mapped EnvVar or the key itself).
//
//nolint:dupl // ConfigMap and Secret env bindings handle different K8s types; deduplication would hurt clarity.
func buildConfigMapEnvBindings(cmr *otilmv1alpha1.ConfigMapRef, b *RefBindings) {
	if len(cmr.Keys) == 0 {
		b.EnvFrom = append(b.EnvFrom, corev1.EnvFromSource{
			ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
			},
		})
		return
	}
	for _, k := range cmr.Keys {
		envName := k.ConfigMapKey
		if k.EnvVar != nil {
			envName = *k.EnvVar
		}
		b.Env = append(b.Env, corev1.EnvVar{
			Name: envName,
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
					Key:                  k.ConfigMapKey,
				},
			},
		})
	}
}

// buildConfigMapVolumeBindings mounts a type=volume ConfigMapRef read-only at MountPath,
// projecting the listed keys to their mapped paths (or the key name).
func buildConfigMapVolumeBindings(cmr *otilmv1alpha1.ConfigMapRef, b *RefBindings) {
	volName := fmt.Sprintf("configmap-%s", cmr.Name)
	cmVol := &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: cmr.Name},
	}
	for _, k := range cmr.Keys {
		path := k.ConfigMapKey
		if k.Path != nil {
			path = *k.Path
		}
		cmVol.Items = append(cmVol.Items, corev1.KeyToPath{Key: k.ConfigMapKey, Path: path})
	}
	b.Volumes = append(b.Volumes, corev1.Volume{
		Name:         volName,
		VolumeSource: corev1.VolumeSource{ConfigMap: cmVol},
	})
	b.VolumeMounts = append(b.VolumeMounts, corev1.VolumeMount{
		Name:      volName,
		MountPath: derefString(cmr.MountPath),
		ReadOnly:  true,
	})
}
