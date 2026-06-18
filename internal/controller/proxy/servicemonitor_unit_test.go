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

package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/capabilities"
)

// newSMReconciler builds a Reconciler over a fake client whose RESTMapper does or
// does not serve the monitoring.coreos.com ServiceMonitor kind — the two sides of
// the capability gate, unit-testable without envtest.
func newSMReconciler(t *testing.T, smServed bool, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, otilmv1alpha1.AddToScheme(scheme))
	require.NoError(t, monitoringv1.AddToScheme(scheme))

	mapper := apimeta.NewDefaultRESTMapper(nil)
	if smServed {
		mapper.Add(schema.GroupVersionKind{
			Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor",
		}, apimeta.RESTScopeNamespace)
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Reconciler{Client: cl, Scheme: scheme, Capabilities: capabilities.New(mapper)}
}

const (
	smTestName      = "dc-east"
	smTestNamespace = "ilm-edge"
)

func proxyWithServiceMonitor() *otilmv1alpha1.Proxy {
	return &otilmv1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Name: smTestName, Namespace: smTestNamespace},
		Spec: otilmv1alpha1.ProxySpec{
			ConfigTokenSecretRef: otilmv1alpha1.ConfigTokenRef{Name: "cfg"},
			Metrics: &otilmv1alpha1.ProxyMetricsSpec{
				Enabled:        true,
				ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
			},
		},
	}
}

func TestReconcileServiceMonitorCreatesWhenServed(t *testing.T) {
	px := proxyWithServiceMonitor()
	r := newSMReconciler(t, true)

	requeue, err := r.reconcileServiceMonitor(context.Background(), px)
	require.NoError(t, err)
	assert.False(t, requeue)

	var sm monitoringv1.ServiceMonitor
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: smTestName, Namespace: smTestNamespace}, &sm))
	assert.Equal(t, "/metrics", sm.Spec.Endpoints[0].Path)

	cond := apimeta.FindStatusCondition(px.Status.Conditions, condServiceMonitorReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestReconcileServiceMonitorPrunesWhenDisabled(t *testing.T) {
	existing := &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: smTestName, Namespace: smTestNamespace},
	}
	px := proxyWithServiceMonitor()
	px.Spec.Metrics = nil // ServiceMonitor no longer desired
	// Simulate a condition left over from when the ServiceMonitor was rendered.
	apimeta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type: condServiceMonitorReady, Status: metav1.ConditionTrue,
		Reason: "ServiceMonitorCreated", Message: "ServiceMonitor is rendered",
	})
	r := newSMReconciler(t, true, existing)

	requeue, err := r.reconcileServiceMonitor(context.Background(), px)
	require.NoError(t, err)
	assert.False(t, requeue)

	var sm monitoringv1.ServiceMonitor
	err = r.Get(context.Background(),
		types.NamespacedName{Name: smTestName, Namespace: smTestNamespace}, &sm)
	assert.True(t, apierrors.IsNotFound(err), "stale ServiceMonitor must be pruned")
	assert.Nil(t, apimeta.FindStatusCondition(px.Status.Conditions, condServiceMonitorReady),
		"stale ServiceMonitorReady condition must be removed with the object")
}

func TestReconcileServiceMonitorGateAbsent(t *testing.T) {
	px := proxyWithServiceMonitor()
	r := newSMReconciler(t, false)

	requeue, err := r.reconcileServiceMonitor(context.Background(), px)
	require.NoError(t, err)
	assert.True(t, requeue, "desired-but-gated ServiceMonitor must request a self-heal requeue")

	cond := apimeta.FindStatusCondition(px.Status.Conditions, condServiceMonitorReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "ServiceMonitorCRDNotInstalled", cond.Reason)
}

func TestReconcileServiceMonitorGateAbsentNotDesired(t *testing.T) {
	px := proxyWithServiceMonitor()
	px.Spec.Metrics = nil
	// A leftover condition must be dropped even when the CRD is no longer served.
	apimeta.SetStatusCondition(&px.Status.Conditions, metav1.Condition{
		Type: condServiceMonitorReady, Status: metav1.ConditionTrue,
		Reason: "ServiceMonitorCreated", Message: "ServiceMonitor is rendered",
	})
	r := newSMReconciler(t, false)

	requeue, err := r.reconcileServiceMonitor(context.Background(), px)
	require.NoError(t, err)
	assert.False(t, requeue, "nothing desired and nothing served — no requeue")
	assert.Nil(t, apimeta.FindStatusCondition(px.Status.Conditions, condServiceMonitorReady))
}
