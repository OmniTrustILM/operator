// Package checksum provides utilities for computing SHA-256 checksums of
// Kubernetes Secrets and ConfigMaps to detect configuration changes and
// trigger rolling updates.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// ComputeSecretChecksum computes a SHA-256 checksum of a Secret's data.
func ComputeSecretChecksum(secret *corev1.Secret) string {
	h := sha256.New()
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write(secret.Data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeConfigMapChecksum computes a SHA-256 checksum of a ConfigMap's data.
func ComputeConfigMapChecksum(cm *corev1.ConfigMap) string {
	h := sha256.New()
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(cm.Data[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CombineChecksums combines multiple named checksums into a single deterministic hash.
func CombineChecksums(checksums map[string]string) string {
	h := sha256.New()
	keys := make([]string, 0, len(checksums))
	for k := range checksums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k + "=" + checksums[k] + ";"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
