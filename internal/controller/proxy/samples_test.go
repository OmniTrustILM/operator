/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// This spec is the correctness gate for the shipped Proxy samples: it loads every
// config/samples/*proxy*.yaml, applies it to the envtest apiserver, and asserts the
// apiserver ACCEPTS it — proving the sample uses only fields that exist in the CRD
// schema. A rejection means a sample drifted from the shipped surface and must be
// fixed (never relax the schema to fit a bad sample). Samples are single-document
// files by convention (the decoder reads one object per file).
var _ = Describe("Proxy samples", func() {
	It("the apiserver accepts every config/samples/*proxy*.yaml", func() {
		samples := proxySampleFiles()
		Expect(samples).NotTo(BeEmpty(), "expected to find Proxy sample files under config/samples")

		for i, path := range samples {
			obj, err := decodeProxySample(path)
			Expect(err).NotTo(HaveOccurred(), "failed to decode sample %s", path)
			Expect(obj.GetAPIVersion()).To(Equal("otilm.com/v1alpha1"), "sample %s has unexpected apiVersion", path)
			Expect(obj.GetKind()).To(Equal("Proxy"), "sample %s is not a Proxy", path)

			ns := freshProxySampleNS(i)
			obj.SetNamespace(ns)
			obj.SetResourceVersion("")

			Expect(k8sClient.Create(ctx, obj)).To(Succeed(),
				"apiserver REJECTED sample %s — fix the sample to match the shipped CRD schema", path)
		}
	})
})

// proxySampleFiles returns the sorted absolute paths of every Proxy sample under
// config/samples (matched by the "proxy" filename token).
func proxySampleFiles() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue())
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	matches, err := filepath.Glob(filepath.Join(root, "config", "samples", "*proxy*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	sort.Strings(matches)
	return matches
}

// decodeProxySample reads a sample file and decodes its YAML into an unstructured
// object, preserving exactly the fields the author wrote (so admission validates the
// real document, not a round-tripped typed struct).
func decodeProxySample(path string) (*unstructured.Unstructured, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // test reads a repo-local sample path
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
		return nil, err
	}
	return obj, nil
}

// freshProxySampleNS creates and returns a unique namespace for one sample.
func freshProxySampleNS(i int) string {
	name := fmt.Sprintf("proxy-sample-%d", i)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}
