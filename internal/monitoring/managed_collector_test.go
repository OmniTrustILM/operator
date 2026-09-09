/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
