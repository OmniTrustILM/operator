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

package rabbitmq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// defaultVhost is RabbitMQ's default virtual host: the literal "/" that must travel as
	// %2F when it is used as a management API path segment.
	defaultVhost = "/"
	// sensitiveVhost is a distinctive vhost name used by the no-leak assertions so a match
	// in an error string can only mean the client actually echoed the vhost.
	sensitiveVhost = "tenant-vhost-9f3c"
	testExchange   = "ilm.exchange"
	testUser       = "broker-admin-9f3c"
	testPassword   = "s3cr3t-p4ss-9f3c"

	// connectionName / escapedConnectionName are a realistic RabbitMQ connection name and
	// its percent-encoded path-segment form (spaces and ">" must not reach the request line
	// unencoded).
	connectionName        = "10.0.0.1:5672 -> 10.0.0.9:41234"
	escapedConnectionName = "10.0.0.1:5672%20-%3E%2010.0.0.9:41234"

	contentTypeHeader = "Content-Type"

	queuesBody = `[{"name":"ilm.q","messages_ready":3,"messages_unacknowledged":2,"consumers":1},
	               {"name":"ilm.dlq","messages_ready":0,"messages_unacknowledged":0,"consumers":0}]`
	connectionsBody = `[{"name":"` + connectionName + `"}]`
	bindingsBody    = `[{"destination":"ilm.q","destination_type":"queue"}]`
)

// brokerCall names one BrokerAdmin method invocation so every error-path table can be run
// against ALL of them — the fail-closed contract holds for the whole interface, not one method.
type brokerCall struct {
	name string
	call func(ctx context.Context, admin BrokerAdmin) error
}

func callsForVhost(vhost string) []brokerCall {
	return []brokerCall{
		{
			name: "Queues",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.Queues(ctx, vhost)
				return err
			},
		},
		{
			name: "BoundQueues",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.BoundQueues(ctx, vhost, testExchange)
				return err
			},
		},
		{
			name: "Connections",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.Connections(ctx, vhost)
				return err
			},
		},
		{
			name: "CloseConnections",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				return admin.CloseConnections(ctx, vhost)
			},
		},
	}
}

// happyHandler answers every management endpoint the client calls with a well-formed body.
func happyHandler(record func(string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			record(r.Method + " " + r.RequestURI)
		}
		w.Header().Set(contentTypeHeader, contentTypeJSON)
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.EscapedPath(), "/connections"):
			_, _ = w.Write([]byte(connectionsBody))
		case strings.HasSuffix(r.URL.EscapedPath(), "/bindings/source"):
			_, _ = w.Write([]byte(bindingsBody))
		default:
			_, _ = w.Write([]byte(queuesBody))
		}
	}
}

// recorder returns a handler that appends every request line to a slice, plus the accessor.
func recorder() (http.HandlerFunc, func() []string) {
	var (
		mu  sync.Mutex
		got []string
	)
	handler := happyHandler(func(line string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, line)
	})
	return handler, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func serveJSON(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, contentTypeJSON)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestManagementRequestsPercentEncodeVhostSegments is the %2F wire test: it asserts on
// r.RequestURI — the RAW request target as it arrived on the wire — that the default vhost "/"
// (and any vhost containing a slash) travels as a single percent-encoded path segment.
func TestManagementRequestsPercentEncodeVhostSegments(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, admin BrokerAdmin) error
		want []string
	}{
		{
			name: "queues on the default vhost",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.Queues(ctx, defaultVhost)
				return err
			},
			want: []string{"GET /api/queues/%2F"},
		},
		{
			name: "queues on a vhost containing a slash",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.Queues(ctx, "ilm/prod")
				return err
			},
			want: []string{"GET /api/queues/ilm%2Fprod"},
		},
		{
			name: "bound queues on the default vhost",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.BoundQueues(ctx, defaultVhost, testExchange)
				return err
			},
			want: []string{"GET /api/exchanges/%2F/ilm.exchange/bindings/source"},
		},
		{
			name: "connections on the default vhost",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				_, err := admin.Connections(ctx, defaultVhost)
				return err
			},
			want: []string{"GET /api/vhosts/%2F/connections"},
		},
		{
			name: "close connections on the default vhost",
			call: func(ctx context.Context, admin BrokerAdmin) error {
				return admin.CloseConnections(ctx, defaultVhost)
			},
			want: []string{
				"GET /api/vhosts/%2F/connections",
				"DELETE /api/connections/" + escapedConnectionName,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, requests := recorder()
			srv := httptest.NewServer(handler)
			defer srv.Close()

			require.NoError(t, tt.call(context.Background(), NewClient(srv.URL, testUser, testPassword)))
			assert.Equal(t, tt.want, requests(), "the vhost must reach the wire percent-encoded")
			for _, line := range requests() {
				assert.NotContains(t, line, "//", "an unencoded vhost would produce a double slash")
			}
		})
	}
}

// TestRequestsCarryBasicAuthAndAcceptJSON verifies the administrator credentials are sent as
// HTTP basic auth on every call and that JSON is requested.
func TestRequestsCarryBasicAuthAndAcceptJSON(t *testing.T) {
	for _, c := range callsForVhost(defaultVhost) {
		t.Run(c.name, func(t *testing.T) {
			var seen int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				assert.True(t, ok, "requests must carry basic auth")
				assert.Equal(t, testUser, user)
				assert.Equal(t, testPassword, pass)
				assert.Equal(t, contentTypeJSON, r.Header.Get("Accept"))
				seen++
				happyHandler(nil)(w, r)
			}))
			defer srv.Close()

			require.NoError(t, c.call(context.Background(), NewClient(srv.URL, testUser, testPassword)))
			assert.Positive(t, seen)
		})
	}
}

func TestQueuesDecodesDepthsAndConsumers(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, queuesBody)

	queues, err := NewClient(srv.URL, testUser, testPassword).Queues(context.Background(), defaultVhost)
	require.NoError(t, err)
	assert.Equal(t, []QueueState{
		{Name: "ilm.q", MessagesReady: 3, MessagesUnacked: 2, Consumers: 1},
		{Name: "ilm.dlq", MessagesReady: 0, MessagesUnacked: 0, Consumers: 0},
	}, queues)
}

func TestQueuesEmptyListing(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, `[]`)

	queues, err := NewClient(srv.URL, testUser, testPassword).Queues(context.Background(), defaultVhost)
	require.NoError(t, err)
	assert.Empty(t, queues)
}

// TestQueuesFailClosedOnIncompleteEntry is the core safety property: a queue whose depth or
// consumer fields the broker did not report (an unavailable queue) must NEVER be decoded as an
// empty queue — it must be an error.
func TestQueuesFailClosedOnIncompleteEntry(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing name", body: `[{"messages_ready":0,"messages_unacknowledged":0,"consumers":0}]`},
		{name: "missing ready depth", body: `[{"name":"ilm.q","messages_unacknowledged":0,"consumers":0}]`},
		{name: "missing unacked depth", body: `[{"name":"ilm.q","messages_ready":0,"consumers":0}]`},
		{name: "missing consumers", body: `[{"name":"ilm.q","messages_ready":0,"messages_unacknowledged":0}]`},
		{name: "queue reported as down", body: `[{"name":"ilm.q","state":"down"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serveJSON(t, http.StatusOK, tt.body)

			queues, err := NewClient(srv.URL, testUser, testPassword).Queues(context.Background(), defaultVhost)
			require.Error(t, err, "an incomplete queue listing must never read as drained")
			assert.Nil(t, queues)
		})
	}
}

func TestBoundQueuesFiltersDestinationsAndDeduplicates(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, `[
	  {"destination":"ilm.q1","destination_type":"queue","routing_key":"a"},
	  {"destination":"ilm.q1","destination_type":"queue","routing_key":"b"},
	  {"destination":"ilm.fanout","destination_type":"exchange"},
	  {"destination":"ilm.q2","destination_type":"queue","routing_key":"c"}
	]`)

	bound, err := NewClient(srv.URL, testUser, testPassword).
		BoundQueues(context.Background(), defaultVhost, testExchange)
	require.NoError(t, err)
	assert.Equal(t, []string{"ilm.q1", "ilm.q2"}, bound)
}

func TestBoundQueuesEmptyListing(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, `[]`)

	bound, err := NewClient(srv.URL, testUser, testPassword).
		BoundQueues(context.Background(), defaultVhost, testExchange)
	require.NoError(t, err)
	assert.Empty(t, bound)
}

// TestBoundQueuesFailClosedOnIncompleteBinding asserts a binding missing its destination or
// destination type is an error — silently skipping it would hide a queue that must be drained.
func TestBoundQueuesFailClosedOnIncompleteBinding(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing destination", body: `[{"destination_type":"queue"}]`},
		{name: "missing destination type", body: `[{"destination":"ilm.q1"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serveJSON(t, http.StatusOK, tt.body)

			bound, err := NewClient(srv.URL, testUser, testPassword).
				BoundQueues(context.Background(), defaultVhost, testExchange)
			require.Error(t, err)
			assert.Nil(t, bound)
		})
	}
}

func TestConnectionsCounts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "no connections", body: `[]`, want: 0},
		{name: "two connections", body: `[{"name":"a"},{"name":"b"}]`, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serveJSON(t, http.StatusOK, tt.body)

			count, err := NewClient(srv.URL, testUser, testPassword).
				Connections(context.Background(), defaultVhost)
			require.NoError(t, err)
			assert.Equal(t, tt.want, count)
		})
	}
}

// TestConnectionsFailClosedOnIncompleteEntry asserts a nameless connection entry is an error:
// it cannot be closed, so it must not be counted away either.
func TestConnectionsFailClosedOnIncompleteEntry(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, `[{"peer_host":"10.0.0.9"}]`)

	count, err := NewClient(srv.URL, testUser, testPassword).
		Connections(context.Background(), defaultVhost)
	require.Error(t, err)
	assert.Zero(t, count)
}

// TestCloseConnectionsDeletesEveryConnection also covers the benign race where a connection
// closes itself between the listing and the delete (404 → already gone → success).
func TestCloseConnectionsDeletesEveryConnection(t *testing.T) {
	var (
		mu      sync.Mutex
		deleted []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.URL.EscapedPath())
			first := len(deleted) == 1
			mu.Unlock()
			if first {
				// The first connection vanished on its own — a benign race, not a failure.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set(contentTypeHeader, contentTypeJSON)
		_, _ = w.Write([]byte(`[{"name":"conn-a"},{"name":"conn-b"}]`))
	}))
	defer srv.Close()

	require.NoError(t, NewClient(srv.URL, testUser, testPassword).
		CloseConnections(context.Background(), defaultVhost))
	assert.Equal(t, []string{"/api/connections/conn-a", "/api/connections/conn-b"}, deleted)
}

func TestCloseConnectionsFailsOnDeleteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set(contentTypeHeader, contentTypeJSON)
		_, _ = w.Write([]byte(connectionsBody))
	}))
	defer srv.Close()

	err := NewClient(srv.URL, testUser, testPassword).CloseConnections(context.Background(), defaultVhost)
	require.Error(t, err)
}

// brokerFailure is one way the broker (or the network) can misbehave. Every one of them must
// make every BrokerAdmin method return an error.
type brokerFailure struct {
	name    string
	handler http.HandlerFunc
	tune    func(*Client)
}

func brokerFailures() []brokerFailure {
	return []brokerFailure{
		{
			name: "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"` + leakyBody + `"}`))
			},
		},
		{
			name: "unauthorized",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"` + leakyBody + `"}`))
			},
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(contentTypeHeader, contentTypeJSON)
				_, _ = w.Write([]byte(`{not json ` + leakyBody))
			},
		},
		{
			name: "empty body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(contentTypeHeader, contentTypeJSON)
			},
		},
		{
			name: "html error page",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html><body>` + leakyBody + `</body></html>`))
			},
		},
		{
			name: "oversized body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(contentTypeHeader, contentTypeJSON)
				_, _ = w.Write([]byte(`[` + strings.Repeat(`{"name":"`+leakyBody+`"},`, 64) + `{"name":"x"}]`))
			},
			tune: func(c *Client) { c.maxResponseBytes = 32 },
		},
	}
}

const leakyBody = "leaky-stacktrace-token-9f3c"

// TestEveryCallFailsClosedOnBrokerFailure is the fail-closed matrix: every method × every
// broker failure must return an error, never a zero-valued "everything is drained" answer.
func TestEveryCallFailsClosedOnBrokerFailure(t *testing.T) {
	for _, failure := range brokerFailures() {
		for _, c := range callsForVhost(defaultVhost) {
			t.Run(failure.name+"/"+c.name, func(t *testing.T) {
				srv := httptest.NewServer(failure.handler)
				defer srv.Close()

				client := NewClient(srv.URL, testUser, testPassword)
				if failure.tune != nil {
					failure.tune(client)
				}
				require.Error(t, c.call(context.Background(), client))
			})
		}
	}
}

// TestOversizedResponseIsRejected pins the bounded read: a body past the limit is refused
// outright rather than decoded from its truncated prefix.
func TestOversizedResponseIsRejected(t *testing.T) {
	const queueEntry = `{"name":"ilm.q","messages_ready":0,"messages_unacknowledged":0,"consumers":0}`
	srv := serveJSON(t, http.StatusOK, `[`+strings.Repeat(queueEntry+`,`, 8)+queueEntry+`]`)

	client := NewClient(srv.URL, testUser, testPassword)
	client.maxResponseBytes = 32

	queues, err := client.Queues(context.Background(), defaultVhost)
	require.Error(t, err)
	assert.Nil(t, queues)

	var bErr *Error
	require.ErrorAs(t, err, &bErr)
	assert.Contains(t, bErr.Message, "exceeds the size limit")
}

// TestTruncatedResponseFailsClosed covers a body that stops mid-flight: the read fails, and a
// half-received queue listing must never be reported as a shorter (or empty) one.
func TestTruncatedResponseFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, contentTypeJSON)
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(`[{"name":"ilm.q","messages_ready":0`))
		// Flush the partial body, then abort the connection: the client receives headers
		// and a prefix, and the read of the promised remainder fails.
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	queues, err := NewClient(srv.URL, testUser, testPassword).Queues(context.Background(), defaultVhost)
	require.Error(t, err)
	assert.Nil(t, queues)

	var bErr *Error
	require.ErrorAs(t, err, &bErr)
	assert.True(t, bErr.Retryable)
	for _, sensitive := range []string{strings.TrimPrefix(srv.URL, "http://"), testUser, testPassword} {
		assert.NotContains(t, bErr.Error(), sensitive)
	}
}

func TestEveryCallFailsClosedOnTransportError(t *testing.T) {
	for _, c := range callsForVhost(defaultVhost) {
		t.Run(c.name, func(t *testing.T) {
			// Port 1: nothing is listening, so the request fails at transport.
			err := c.call(context.Background(), NewClient("http://127.0.0.1:1", testUser, testPassword))
			require.Error(t, err)

			var bErr *Error
			require.ErrorAs(t, err, &bErr)
			assert.True(t, bErr.Retryable, "a transport failure is retryable")
		})
	}
}

func TestEveryCallFailsClosedOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		happyHandler(nil)(w, r)
	}))
	defer srv.Close()

	for _, c := range callsForVhost(defaultVhost) {
		t.Run(c.name, func(t *testing.T) {
			client := NewClientWithHTTPClient(srv.URL, testUser, testPassword,
				&http.Client{Timeout: 20 * time.Millisecond})
			require.Error(t, c.call(context.Background(), client))
		})
	}
}

func TestEveryCallFailsClosedOnCanceledContext(t *testing.T) {
	handler, _ := recorder()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range callsForVhost(defaultVhost) {
		t.Run(c.name, func(t *testing.T) {
			require.Error(t, c.call(ctx, NewClient(srv.URL, testUser, testPassword)))
		})
	}
}

// TestBrokerErrorsNoLeak is the security invariant: no error returned by any method, on any
// failure path, may contain the broker host, the vhost, the username, the password, or any
// byte of the broker's response body.
func TestBrokerErrorsNoLeak(t *testing.T) {
	assertNoLeak := func(t *testing.T, err error, host string) {
		t.Helper()
		require.Error(t, err)
		for _, sensitive := range []string{host, sensitiveVhost, testUser, testPassword, leakyBody, testExchange} {
			assert.NotContains(t, err.Error(), sensitive, "the error must not carry connection or credential material")
		}
		var bErr *Error
		if errors.As(err, &bErr) {
			for _, sensitive := range []string{host, sensitiveVhost, testUser, testPassword, leakyBody, testExchange} {
				assert.NotContains(t, bErr.Message, sensitive, "the error message must not carry connection or credential material")
			}
		}
	}

	for _, failure := range brokerFailures() {
		for _, c := range callsForVhost(sensitiveVhost) {
			t.Run(failure.name+"/"+c.name, func(t *testing.T) {
				srv := httptest.NewServer(failure.handler)
				defer srv.Close()

				client := NewClient(srv.URL, testUser, testPassword)
				if failure.tune != nil {
					failure.tune(client)
				}
				assertNoLeak(t, c.call(context.Background(), client), strings.TrimPrefix(srv.URL, "http://"))
			})
		}
	}

	t.Run("transport error", func(t *testing.T) {
		for _, c := range callsForVhost(sensitiveVhost) {
			assertNoLeak(t, c.call(context.Background(),
				NewClient("http://127.0.0.1:1", testUser, testPassword)), "127.0.0.1:1")
		}
	})

	t.Run("incomplete listing", func(t *testing.T) {
		srv := serveJSON(t, http.StatusOK, `[{"state":"down"}]`)
		for _, c := range callsForVhost(sensitiveVhost) {
			assertNoLeak(t, c.call(context.Background(),
				NewClient(srv.URL, testUser, testPassword)), strings.TrimPrefix(srv.URL, "http://"))
		}
	})
}

func TestErrorRetryability(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "server error is retryable", status: http.StatusInternalServerError, retryable: true},
		{name: "bad gateway is retryable", status: http.StatusBadGateway, retryable: true},
		{name: "unauthorized is not retryable", status: http.StatusUnauthorized, retryable: false},
		{name: "not found is not retryable", status: http.StatusNotFound, retryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serveJSON(t, tt.status, `{"error":"nope"}`)

			_, err := NewClient(srv.URL, testUser, testPassword).Queues(context.Background(), defaultVhost)
			require.Error(t, err)

			var bErr *Error
			require.ErrorAs(t, err, &bErr)
			assert.Equal(t, tt.status, bErr.StatusCode)
			assert.Equal(t, tt.retryable, bErr.Retryable)
		})
	}
}

func TestErrorMessageFormatting(t *testing.T) {
	assert.Equal(t, "broker error 500: boom (retryable)",
		(&Error{StatusCode: http.StatusInternalServerError, Message: "boom", Retryable: true}).Error())
	assert.Equal(t, "broker error 401: nope",
		(&Error{StatusCode: http.StatusUnauthorized, Message: "nope"}).Error())
	assert.Equal(t, "broker error: transport failed (retryable)",
		(&Error{Message: "transport failed", Retryable: true}).Error())
}

func TestClientDefaults(t *testing.T) {
	client := NewClient("http://broker.example:15672", testUser, testPassword)
	require.NotNil(t, client.httpClient)
	assert.Equal(t, defaultRequestTimeout, client.httpClient.Timeout, "an overall timeout must bound every call")
	assert.Equal(t, defaultMaxResponseBytes, client.maxResponseBytes)

	injected := &http.Client{Timeout: time.Second}
	assert.Same(t, injected, NewClientWithHTTPClient("http://broker.example:15672", testUser, testPassword, injected).httpClient)

	// A zero-valued Client still uses bounded defaults rather than http.DefaultClient.
	zero := &Client{}
	require.NotNil(t, zero.client())
	assert.NotSame(t, http.DefaultClient, zero.client())
	assert.Equal(t, defaultMaxResponseBytes, zero.limit())
}

// TestBaseURLTrailingSlash asserts the assembled path never doubles the separator.
func TestBaseURLTrailingSlash(t *testing.T) {
	handler, requests := recorder()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	_, err := NewClient(srv.URL+"/", testUser, testPassword).Queues(context.Background(), defaultVhost)
	require.NoError(t, err)
	assert.Equal(t, []string{"GET /api/queues/%2F"}, requests())
}

func TestInvalidBaseURLFailsClosed(t *testing.T) {
	for _, c := range callsForVhost(defaultVhost) {
		t.Run(c.name, func(t *testing.T) {
			require.Error(t, c.call(context.Background(), NewClient("://not a url", testUser, testPassword)))
		})
	}
}

// TestClientImplementsBrokerAdmin pins the production client to the interface the migration
// consumes.
func TestClientImplementsBrokerAdmin(t *testing.T) {
	var admin BrokerAdmin = NewClient("http://broker.example:15672", testUser, testPassword)
	assert.NotNil(t, admin)
}
