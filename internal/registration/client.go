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

// Package registration provides an HTTP client for the ILM Core platform registration API.
package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultTimeout = 30 * time.Second

// Client is an HTTP client for the ILM Core platform API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// Error represents an error returned by the platform API.
type Error struct {
	StatusCode int
	Message    string
	Retryable  bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	retryable := ""
	if e.Retryable {
		retryable = " (retryable)"
	}
	return fmt.Sprintf("platform error %d: %s%s", e.StatusCode, e.Message, retryable)
}

// NewClient creates a new platform API client with default settings.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// Post sends a JSON POST request to the given path. The body is JSON-encoded
// and the result (if non-nil) is decoded from the response body.
// Returns a *Error for HTTP errors. 5xx and network errors are retryable; 4xx are not.
func (c *Client) Post(ctx context.Context, path string, body any, result any) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshalling request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport error: retryable. Do not surface err.Error() — it may include the
		// request URL (and thus Core's host/coordinates); use a generic phrase instead.
		return &Error{
			StatusCode: 0,
			Message:    "transport error contacting platform",
			Retryable:  true,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		// SECURITY: never read or surface the response body — Core's error body may
		// echo request/identity material that would then flow into the Connector's
		// status condition, a Warning event, and logs (the "never in status/events/
		// logs" invariant). Drain the body so the connection can be reused, but keep
		// ONLY the status code on the error (5xx is retryable; other 4xx are not).
		_, _ = io.Copy(io.Discard, resp.Body)
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("registration failed with status %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
		}
	}

	// Success path: the body is the operator's own expected response shape (not error
	// material), so reading it to decode the result is safe.
	if result != nil {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return &Error{
				StatusCode: resp.StatusCode,
				Message:    "reading response body",
				Retryable:  true,
			}
		}
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("decoding response body: %w", err)
			}
		}
	}

	return nil
}
