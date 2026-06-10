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
