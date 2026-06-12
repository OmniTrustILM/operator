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

package monitoring

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func TestManagedCountCollectorCountsFromCache(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&otilmv1alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "a"}},
		&otilmv1alpha1.Connector{ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "b"}},
		&otilmv1alpha1.Proxy{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "a"}},
	).Build()

	expected := `
# HELP ilm_operator_connectors_managed Current number of Connector resources managed by the ILM operator.
# TYPE ilm_operator_connectors_managed gauge
ilm_operator_connectors_managed 2
# HELP ilm_operator_proxies_managed Current number of Proxy resources managed by the ILM operator.
# TYPE ilm_operator_proxies_managed gauge
ilm_operator_proxies_managed 1
`
	require.NoError(t, testutil.CollectAndCompare(NewManagedCountCollector(cl), strings.NewReader(expected)))
}

func TestManagedCountCollectorOmitsMetricsOnListError(t *testing.T) {
	// A scheme without the otilm.com types makes every List fail — the unsynced-cache
	// startup case. The scrape must omit the metrics, not report zero.
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	require.Equal(t, 0, testutil.CollectAndCount(NewManagedCountCollector(cl)))
}
