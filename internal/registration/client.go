/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
	"net/url"
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

// requestURL composes the absolute request URL for a path relative to the client's base URL.
//
// spec.registration.platformUrl is the platform's BASE API URL — it already carries the /api
// prefix (see RegistrationSpec.PlatformURL) — so this deliberately does NOT inject, rewrite or
// de-duplicate any path segment. It only JOINS. url.URL.JoinPath keeps the base's own path
// exactly once and cleans the result, so a trailing slash on the base can no longer produce the
// "//v2/..." that Core does not route.
//
// A base URL that does not PARSE falls back to concatenation, so an invalid platformUrl still
// fails inside http.NewRequestWithContext with the same "creating request" error it has always
// produced (TestPostInvalidURL) rather than a new, differently-worded one.
//
// SECURITY: the URL is never placed in an error, condition or log line — Core's host/port must
// not leak out of this package (the same reason Post's transport error is a fixed phrase).
func (c *Client) requestURL(path string) string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + path
	}
	return u.JoinPath(path).String()
}

// Post sends a JSON POST request to the given path. The body is JSON-encoded
// and the result (if non-nil) is decoded from the response body.
// Returns a *Error for HTTP errors. 5xx and network errors are retryable; 4xx are not.
func (c *Client) Post(ctx context.Context, path string, body any, result any) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshalling request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.requestURL(path), bytes.NewReader(jsonBody))
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
