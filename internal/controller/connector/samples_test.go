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

package connector

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

// This spec is the correctness gate for the shipped Connector samples: it loads every
// curated config/samples/connector_*.yaml, applies it to the envtest apiserver, and
// asserts the apiserver ACCEPTS it — proving the sample uses only fields that exist
// in the CRD schema and passes its validation rules. A rejection means a sample
// drifted from the shipped surface and must be fixed (never relax the schema to fit
// a bad sample). The kustomize scaffolding stub (v1alpha1_connector.yaml) is excluded
// by the glob: its spec is intentionally empty.
var _ = Describe("Connector samples", func() {
	It("the apiserver accepts every config/samples/connector_*.yaml", func() {
		samples := connectorSampleFiles()
		Expect(samples).NotTo(BeEmpty(), "expected to find Connector sample files under config/samples")

		for i, path := range samples {
			obj, err := decodeConnectorSample(path)
			Expect(err).NotTo(HaveOccurred(), "failed to decode sample %s", path)
			Expect(obj.GetAPIVersion()).To(Equal("otilm.com/v1alpha1"), "sample %s has unexpected apiVersion", path)
			Expect(obj.GetKind()).To(Equal("Connector"), "sample %s is not a Connector", path)

			ns := freshConnectorSampleNS(i)
			obj.SetNamespace(ns)
			obj.SetResourceVersion("")

			Expect(k8sClient.Create(ctx, obj)).To(Succeed(),
				"apiserver REJECTED sample %s — fix the sample to match the shipped CRD schema", path)
		}
	})
})

// connectorSampleFiles returns the sorted absolute paths of every curated Connector
// sample under config/samples (the connector_ filename prefix).
func connectorSampleFiles() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue())
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	matches, err := filepath.Glob(filepath.Join(root, "config", "samples", "connector_*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	sort.Strings(matches)
	return matches
}

// decodeConnectorSample reads a sample file and decodes its YAML into an unstructured
// object, preserving exactly the fields the author wrote (so admission validates the
// real document, not a round-tripped typed struct).
func decodeConnectorSample(path string) (*unstructured.Unstructured, error) {
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

// freshConnectorSampleNS creates and returns a unique namespace for one sample.
func freshConnectorSampleNS(i int) string {
	name := fmt.Sprintf("connector-sample-%d", i)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}
