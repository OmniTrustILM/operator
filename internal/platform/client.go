// Package platform provides an HTTP client for the ILM Core platform API.
package platform

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
		return &Error{
			StatusCode: 0,
			Message:    err.Error(),
			Retryable:  true,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("reading response body: %v", err),
			Retryable:  true,
		}
	}

	if resp.StatusCode >= 400 {
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    string(respBody),
			Retryable:  resp.StatusCode >= 500,
		}
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decoding response body: %w", err)
		}
	}

	return nil
}
