/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package incluster

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
)

const (
	// mountPath is where Kubernetes mounts a pod's ServiceAccount namespace, relative to the root.
	mountPath = "var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// podNamespace is the namespace a test pod runs in.
	podNamespace = "ilm-system"
)

func TestNamespaceIn(t *testing.T) {
	cases := []struct {
		name string
		root fstest.MapFS
		want string
	}{
		{"the mounted namespace", fstest.MapFS{mountPath: {Data: []byte(podNamespace)}}, podNamespace},
		{"a trailing newline is dropped", fstest.MapFS{mountPath: {Data: []byte(podNamespace + "\n")}}, podNamespace},
		{"no mount outside a cluster", fstest.MapFS{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, namespaceIn(tc.root))
		})
	}
}

func TestNamespaceReadsTheServiceAccountMount(t *testing.T) {
	want := "" // outside a cluster there is no mount, and no namespace is guessed
	if data, err := os.ReadFile("/" + mountPath); err == nil {
		want = strings.TrimSpace(string(data))
	}
	assert.Equal(t, want, Namespace())
}
