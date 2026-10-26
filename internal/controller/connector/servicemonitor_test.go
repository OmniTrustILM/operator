/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

// The RESTMapper's discovery requests take no context. While the ServiceMonitor CRD is not
// served, every ServiceMonitor lookup makes the mapper ask for monitoring.coreos.com/v1 again, so
// a slow answer holds the reconcile, and the manager's shutdown, until the discovery client's own
// timeout. These specs interfere with that request.

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo pattern
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega pattern

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// faultyDiscovery interferes, once switched on, with the monitoring.coreos.com/v1 discovery
// request: it answers status when set, and otherwise holds the request until the request ends or
// the spec releases it.
type faultyDiscovery struct {
	next     http.RoundTripper
	on       atomic.Bool
	status   int
	entered  chan struct{}
	released chan struct{}
}

func (f *faultyDiscovery) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.on.Load() || req.URL.Path != "/apis/monitoring.coreos.com/v1" {
		return f.next.RoundTrip(req)
	}
	if f.status != 0 {
		return &http.Response{StatusCode: f.status, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
	}
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-f.released:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return f.next.RoundTrip(req)
}

// faultyClient returns a client that reaches the API server through f. Its RESTMapper has already
// discovered the cluster, so its next ServiceMonitor lookup asks for monitoring.coreos.com/v1
// itself, and f is switched on.
func faultyClient(f *faultyDiscovery) client.Client {
	faultCfg := rest.CopyConfig(cfg)
	faultCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		f.next = rt
		return f
	})
	c, err := client.New(faultCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	_, err = c.RESTMapper().RESTMapping(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "v1")
	Expect(err).NotTo(HaveOccurred())
	f.on.Store(true)
	return c
}

var _ = Describe("ServiceMonitor discovery", func() {
	It("leaves the reconcile when its context ends while discovery stalls", func() {
		f := &faultyDiscovery{entered: make(chan struct{}, 1), released: make(chan struct{})}
		DeferCleanup(func() { close(f.released) })
		c := faultyClient(f)
		r := &Reconciler{Client: c, Scheme: c.Scheme()}
		reconcileCtx, cancelReconcile := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- r.reconcileServiceMonitor(reconcileCtx, newConnector("sm-stalled", "default")) }()
		Eventually(f.entered, timeout).Should(Receive())

		cancelReconcile()
		Eventually(done, 2*time.Second).Should(Receive(MatchError(context.Canceled)))
	})

	It("skips the cleanup when discovery fails and no ServiceMonitor is wanted", func() {
		c := faultyClient(&faultyDiscovery{status: http.StatusInternalServerError})
		r := &Reconciler{Client: c, Scheme: c.Scheme()}
		Expect(r.reconcileServiceMonitor(ctx, newConnector("sm-unwanted", "default"))).To(Succeed())
	})

	It("fails the reconcile when discovery fails and a ServiceMonitor is wanted", func() {
		c := faultyClient(&faultyDiscovery{status: http.StatusInternalServerError})
		r := &Reconciler{Client: c, Scheme: c.Scheme()}
		conn := newConnector("sm-wanted", "default")
		conn.Spec.Metrics = &otilmv1alpha1.MetricsSpec{
			Enabled:        true,
			ServiceMonitor: &otilmv1alpha1.ServiceMonitorSpec{Enabled: true},
		}
		Expect(r.reconcileServiceMonitor(ctx, conn)).To(MatchError(ContainSubstring("detecting the ServiceMonitor CRD")))
	})
})
