/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package incluster reads what the operator learns about itself from the pod it runs in.
package incluster

import (
	"io/fs"
	"os"
	"strings"
)

// serviceAccountNamespace is the file, relative to the filesystem root, in which Kubernetes mounts
// the namespace of the pod's ServiceAccount. controller-runtime reads the same file to find its
// leader-election namespace.
const serviceAccountNamespace = "var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Namespace returns the namespace the operator's pod runs in, or "" when the operator runs outside
// a cluster.
func Namespace() string {
	return namespaceIn(os.DirFS("/"))
}

// namespaceIn reads the ServiceAccount namespace below root.
func namespaceIn(root fs.FS) string {
	data, err := fs.ReadFile(root, serviceAccountNamespace)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
