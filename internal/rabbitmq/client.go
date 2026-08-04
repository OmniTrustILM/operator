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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultRequestTimeout bounds a single management API call end to end.
	defaultRequestTimeout = 15 * time.Second
	// defaultConnectTimeout bounds establishing the TCP connection (and the TLS handshake)
	// so an unreachable broker fails fast instead of consuming the whole request budget.
	defaultConnectTimeout = 5 * time.Second
	// defaultMaxResponseBytes bounds how much of a response body is read before decoding.
	// A vhost with tens of thousands of queues stays well inside it; a runaway or hostile
	// body is rejected rather than buffered.
	defaultMaxResponseBytes int64 = 8 << 20
)

// contentTypeJSON is the media type requested from (and expected of) the management API.
const contentTypeJSON = "application/json"

// destinationTypeQueue is the binding destination type that denotes a queue (as opposed to
// an exchange-to-exchange binding).
const destinationTypeQueue = "queue"

// Operation labels used to build generic error messages. They name the STEP that failed and
// nothing about the broker being talked to.
const (
	opQueues          = "queue listing"
	opBindings        = "exchange binding listing"
	opConnections     = "connection listing"
	opCloseConnection = "connection close"
)

// transportErrContactingBroker is the generic, leak-free message for any HTTP transport
// failure. It deliberately omits err.Error(), which embeds the request URL (and thus the
// broker host and the vhost).
const transportErrContactingBroker = "transport error contacting the broker"

// Client is the production BrokerAdmin: it talks to RabbitMQ's HTTP management API using
// administrator credentials supplied by the caller. It holds no per-call state, so one
// instance may be shared.
type Client struct {
	// baseURL is the management API root, e.g. http://broker.namespace.svc:15672.
	baseURL string
	// username and password are the administrator credentials sent as HTTP basic auth.
	// SECURITY: they are used only to sign requests and are never logged or returned.
	username string
	password string
	// httpClient performs the requests. When nil a bounded-timeout default is used.
	httpClient *http.Client
	// maxResponseBytes bounds the body read for a single call. Zero means the default.
	maxResponseBytes int64
}

// Client implements the interface the migration consumes.
var _ BrokerAdmin = (*Client)(nil)

// NewClient returns a Client for the management API at baseURL, authenticating as
// username/password, with connect and overall timeouts applied.
func NewClient(baseURL, username, password string) *Client {
	return NewClientWithHTTPClient(baseURL, username, password, nil)
}

// NewClientWithHTTPClient returns a Client using the supplied HTTP client, so a caller (or a
// test) can control timeouts, transport and TLS. A nil httpClient selects the bounded default.
func NewClientWithHTTPClient(baseURL, username, password string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &Client{
		baseURL:          baseURL,
		username:         username,
		password:         password,
		httpClient:       httpClient,
		maxResponseBytes: defaultMaxResponseBytes,
	}
}

// defaultHTTPClient returns an HTTP client with both a connect timeout and an overall
// timeout, so neither a black-holed SYN nor a stalled response can hang a reconcile.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultRequestTimeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: defaultConnectTimeout}).DialContext,
			TLSHandshakeTimeout:   defaultConnectTimeout,
			ResponseHeaderTimeout: defaultRequestTimeout,
		},
	}
}

// client returns the configured HTTP client, or a bounded default for a zero-valued Client.
func (c *Client) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return defaultHTTPClient()
}

// limit returns the configured response size bound, or the default.
func (c *Client) limit() int64 {
	if c.maxResponseBytes > 0 {
		return c.maxResponseBytes
	}
	return defaultMaxResponseBytes
}

// queueRepr, bindingRepr and connectionRepr are the subsets of the management API responses
// the client decodes. The numeric queue fields are POINTERS on purpose: the broker omits them
// for a queue it cannot currently report on, and a missing depth must fail closed rather than
// decode to a very convincing zero.
type queueRepr struct {
	Name            string `json:"name"`
	MessagesReady   *int64 `json:"messages_ready"`
	MessagesUnacked *int64 `json:"messages_unacknowledged"`
}

type bindingRepr struct {
	Destination     string `json:"destination"`
	DestinationType string `json:"destination_type"`
}

type connectionRepr struct {
	Name string `json:"name"`
}

// Queues implements BrokerAdmin. It reads GET /api/queues/<vhost> and returns one QueueState
// per queue. An entry missing its name or either depth is rejected — an unreportable queue
// must never be mistaken for an empty one.
func (c *Client) Queues(ctx context.Context, vhost string) ([]QueueState, error) {
	var reprs []queueRepr
	if err := c.do(ctx, http.MethodGet, managementPath("queues", vhost), opQueues, &reprs); err != nil {
		return nil, err
	}

	queues := make([]QueueState, 0, len(reprs))
	for _, r := range reprs {
		if r.Name == "" || r.MessagesReady == nil || r.MessagesUnacked == nil {
			return nil, incompleteErr(opQueues)
		}
		queues = append(queues, QueueState{
			Name:            r.Name,
			MessagesReady:   *r.MessagesReady,
			MessagesUnacked: *r.MessagesUnacked,
		})
	}
	return queues, nil
}

// BoundQueues implements BrokerAdmin. It reads
// GET /api/exchanges/<vhost>/<exchange>/bindings/source and returns the distinct queue
// destinations, in the order the broker reported them. Bindings to other exchanges are
// skipped; a binding missing either field is rejected rather than skipped, since silently
// dropping it would hide a queue that still has to drain.
func (c *Client) BoundQueues(ctx context.Context, vhost, exchange string) ([]string, error) {
	var reprs []bindingRepr
	path := managementPath("exchanges", vhost, exchange, "bindings", "source")
	if err := c.do(ctx, http.MethodGet, path, opBindings, &reprs); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(reprs))
	queues := make([]string, 0, len(reprs))
	for _, r := range reprs {
		if r.Destination == "" || r.DestinationType == "" {
			return nil, incompleteErr(opBindings)
		}
		if r.DestinationType != destinationTypeQueue {
			continue
		}
		if _, dup := seen[r.Destination]; dup {
			continue
		}
		seen[r.Destination] = struct{}{}
		queues = append(queues, r.Destination)
	}
	return queues, nil
}

// Connections implements BrokerAdmin by counting the entries of
// GET /api/vhosts/<vhost>/connections.
func (c *Client) Connections(ctx context.Context, vhost string) (int, error) {
	names, err := c.connectionNames(ctx, vhost)
	if err != nil {
		return 0, err
	}
	return len(names), nil
}

// CloseConnections implements BrokerAdmin: it lists the vhost's connections and issues
// DELETE /api/connections/<name> for each. A connection that has already gone away (404) is
// success; any other failure is returned, so the caller never concludes the vhost is quiet.
func (c *Client) CloseConnections(ctx context.Context, vhost string) error {
	names, err := c.connectionNames(ctx, vhost)
	if err != nil {
		return err
	}

	for _, name := range names {
		err := c.do(ctx, http.MethodDelete, managementPath("connections", name), opCloseConnection, nil)
		if err == nil {
			continue
		}
		var bErr *Error
		if errors.As(err, &bErr) && bErr.StatusCode == http.StatusNotFound {
			// The connection closed itself between the listing and the delete.
			continue
		}
		return err
	}
	return nil
}

// connectionNames reads the vhost's open connections. A nameless entry is rejected: it cannot
// be closed, so it must not be counted away either.
func (c *Client) connectionNames(ctx context.Context, vhost string) ([]string, error) {
	var reprs []connectionRepr
	if err := c.do(ctx, http.MethodGet, managementPath("vhosts", vhost, "connections"), opConnections, &reprs); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(reprs))
	for _, r := range reprs {
		if r.Name == "" {
			return nil, incompleteErr(opConnections)
		}
		names = append(names, r.Name)
	}
	return names, nil
}

// do issues one management API request and, when out is non-nil, decodes the bounded response
// body into it. Every failure becomes a leak-free *Error.
//
// SECURITY: an error response body is drained but NEVER inspected — RabbitMQ echoes the
// request (vhost, user) in its error bodies, and this error travels into status conditions,
// events and logs. Only the status code survives.
func (c *Client) do(ctx context.Context, method, escapedPath, op string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(escapedPath), nil)
	if err != nil {
		// The construction error embeds the URL; report only the step.
		return &Error{Message: "building the broker " + op + " request"}
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", contentTypeJSON)

	resp, err := c.client().Do(req)
	if err != nil {
		return &Error{Message: transportErrContactingBroker, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.drain(resp.Body)
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("broker %s failed with status %d", op, resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}

	if out == nil {
		c.drain(resp.Body)
		return nil
	}
	return c.decode(resp, op, out)
}

// decode reads at most the configured limit from the response body and unmarshals it. A body
// that exceeds the limit, or that does not parse, is an error — never a partial answer.
func (c *Client) decode(resp *http.Response, op string, out any) error {
	limit := c.limit()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "reading the broker " + op + " response", Retryable: true}
	}
	if int64(len(body)) > limit {
		return &Error{StatusCode: resp.StatusCode, Message: "broker " + op + " response exceeds the size limit"}
	}
	// Never wrap the unmarshal error: it quotes the offending bytes of the body.
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "decoding the broker " + op + " response", Retryable: true}
	}
	return nil
}

// drain discards a bounded amount of the body so the connection can be reused without letting
// an unbounded response occupy the reconcile.
func (c *Client) drain(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, c.limit()))
}

// incompleteErr reports a response the broker returned successfully but did not fully
// populate. It is retryable: the missing state is usually a queue or node that is briefly
// unavailable.
func incompleteErr(op string) *Error {
	return &Error{Message: "broker " + op + " response is incomplete", Retryable: true}
}

// endpoint joins the base URL with an ALREADY-ESCAPED path.
func (c *Client) endpoint(escapedPath string) string {
	return strings.TrimRight(c.baseURL, "/") + escapedPath
}

// managementPath assembles an /api path from decoded segments, percent-encoding each one
// individually.
//
// This is the load-bearing detail of the whole client: RabbitMQ takes the vhost as a PATH
// SEGMENT, and the default vhost is literally "/", which must travel as %2F. url.PathEscape
// escapes "/" because it escapes for a single segment; the resulting string is then preserved
// on the wire because url.Parse keeps it in URL.RawPath (URL.Path holds the decoded form, and
// the request line is written from the escaped one).
func managementPath(segments ...string) string {
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}
	return "/api/" + strings.Join(escaped, "/")
}
