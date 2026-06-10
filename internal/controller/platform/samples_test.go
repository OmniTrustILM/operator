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

package platform

import (
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

// This spec is the correctness gate for the shipped Platform samples: it loads every
// config/samples/*platform*.yaml, applies it to the envtest apiserver, and asserts the
// apiserver ACCEPTS it. Acceptance proves the sample uses only fields that exist in the
// CRD schema AND passes every CEL (XValidation) rule — the same admission path a real
// `kubectl apply` takes. A rejection means a sample drifted from the shipped surface and
// must be fixed (never relax the rule to fit a bad sample).
//
// The reconciler is irrelevant here (admission happens before any reconcile), so no
// readiness is awaited. Each sample is created in its own fresh namespace so name reuse
// across samples (they all use name "ilm") never collides.
var _ = Describe("Platform samples", func() {
	It("the apiserver accepts every config/samples/*platform*.yaml", func() {
		samples := platformSampleFiles()
		Expect(samples).NotTo(BeEmpty(), "expected to find Platform sample files under config/samples")

		for i, path := range samples {
			obj, err := decodePlatformSample(path)
			Expect(err).NotTo(HaveOccurred(), "failed to decode sample %s", path)
			Expect(obj.GetAPIVersion()).To(Equal("otilm.com/v1alpha1"), "sample %s has unexpected apiVersion", path)
			Expect(obj.GetKind()).To(Equal("Platform"), "sample %s is not a Platform", path)

			// Isolate each sample in its own namespace (samples share the name "ilm").
			ns := freshSampleNS(i)
			obj.SetNamespace(ns)
			obj.SetResourceVersion("")

			Expect(k8sClient.Create(ctx, obj)).To(Succeed(),
				"apiserver REJECTED sample %s — fix the sample to match the shipped CRD schema/CEL", path)
		}
	})
})

// platformSampleFiles returns the sorted absolute paths of every Platform sample under
// config/samples (matched by the "platform" filename token), so the set tracks new
// samples automatically.
func platformSampleFiles() []string {
	dir := filepath.Join(samplesRepoRoot(), "config", "samples")
	matches, err := filepath.Glob(filepath.Join(dir, "*platform*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	sort.Strings(matches)
	return matches
}

// decodePlatformSample reads a sample file and decodes its YAML into an unstructured
// object, preserving exactly the fields the author wrote (so admission validates the
// real document, not a round-tripped typed struct).
func decodePlatformSample(path string) (*unstructured.Unstructured, error) {
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

// freshSampleNS creates and returns a unique namespace for one sample.
func freshSampleNS(i int) string {
	name := "sample-" + itoa(i)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

// itoa is a tiny non-negative int-to-string helper (avoids pulling strconv into the
// dot-imported Ginkgo file just for one call).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// samplesRepoRoot derives the module root from this file's location
// (internal/controller/platform/samples_test.go → up 3).
func samplesRepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}
