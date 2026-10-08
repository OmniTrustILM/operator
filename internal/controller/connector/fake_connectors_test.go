/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// fakeConnectors stands in for every connector Service in the suite and answers by the
// request's host. Its client routes the operator's real Service URLs to the fake.
type fakeConnectors struct {
	server *httptest.Server

	mu      sync.Mutex
	answers map[string]fakeAnswer
	asked   map[string][]string
}

// fakeAnswer is one scripted response.
type fakeAnswer struct {
	status int
	body   string
	hang   bool
}

func newFakeConnectors() *fakeConnectors {
	f := &fakeConnectors{answers: map[string]fakeAnswer{}, asked: map[string][]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeConnectors) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked[r.Host] = append(f.asked[r.Host], r.URL.Path)
	a, ok := f.answers[r.Host+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.hang {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	_, _ = w.Write([]byte(a.body))
}

// client sends every request to the fake, keeping the Host the operator named.
func (f *fakeConnectors) client() *http.Client {
	addr := f.server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}
}

// answer scripts the connector Service's answer at path.
func (f *fakeConnectors) answer(conn *otilmv1alpha1.Connector, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[serviceHost(conn)+path] = fakeAnswer{status: status, body: body}
}

// hang leaves the connector Service's requests at path unanswered until the operator gives up.
func (f *fakeConnectors) hang(conn *otilmv1alpha1.Connector, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[serviceHost(conn)+path] = fakeAnswer{hang: true}
}

// askedPaths lists the paths asked of the connector's Service, oldest first.
func (f *fakeConnectors) askedPaths(conn *otilmv1alpha1.Connector) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked[serviceHost(conn)]...)
}

func (f *fakeConnectors) close() {
	f.server.Close()
}

// serviceHost is the Host header of a request to the connector's in-cluster Service.
func serviceHost(conn *otilmv1alpha1.Connector) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", conn.Name, conn.Namespace, conn.Spec.Service.Port)
}
