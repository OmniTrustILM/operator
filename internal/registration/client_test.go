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

package registration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testEndpoint    = "/test"
	contentTypeJSON = "application/json"
	contentTypeKey  = "Content-Type"
)

func TestPostSuccess(t *testing.T) {
	type response struct {
		Message string `json:"message"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, contentTypeJSON, r.Header.Get(contentTypeKey))

		var body map[string]string
		err := json.NewDecoder(r.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "hello", body["key"])

		w.Header().Set(contentTypeKey, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(response{Message: "ok"}))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var result response
	err := client.Post(context.Background(), testEndpoint, map[string]string{"key": "hello"}, &result)
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Message)
}

func TestPostHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		retryable  bool
	}{
		{
			name:       "4xx not retryable",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"bad request"}`,
			retryable:  false,
		},
		{
			name:       "5xx retryable",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"internal server error"}`,
			retryable:  true,
		},
		{
			name:       "502 retryable",
			statusCode: http.StatusBadGateway,
			body:       `{"error":"bad gateway"}`,
			retryable:  true,
		},
		{
			name:       "403 not retryable",
			statusCode: http.StatusForbidden,
			body:       `{"error":"forbidden"}`,
			retryable:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(server.URL)
			err := client.Post(context.Background(), testEndpoint, map[string]string{}, nil)
			require.Error(t, err)

			var pErr *Error
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, tt.statusCode, pErr.StatusCode)
			assert.Equal(t, tt.retryable, pErr.Retryable)
			// SECURITY: the error carries only the status code — never the response body
			// (which flows into the Connector status condition, a Warning event, and logs).
			assert.NotContains(t, pErr.Message, tt.body, "response body must not leak into the error")
			assert.Contains(t, pErr.Message, "registration failed with status")
		})
	}
}

// TestPostErrorBodyNoLeak verifies that a sensitive-looking error response body is
// NEVER surfaced anywhere on the returned *Error: not the message, not the full
// Error() string. Mirrors the admin registrar's no-leak test. The error must carry
// only the HTTP status code (the "never in status/conditions/events/logs" invariant).
func TestPostErrorBodyNoLeak(t *testing.T) {
	const leakySecretInBody = "super-secret-stacktrace-token-abc123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"` + leakySecretInBody + `"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	err := client.Post(context.Background(), testEndpoint, map[string]string{}, nil)
	require.Error(t, err)

	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.Equal(t, http.StatusInternalServerError, pErr.StatusCode)
	assert.True(t, pErr.Retryable, "5xx must be retryable")
	assert.NotContains(t, pErr.Message, leakySecretInBody, "response body must not leak into Message")
	assert.NotContains(t, pErr.Error(), leakySecretInBody, "response body must not leak into Error()")
}

func TestPostNetworkError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1") // port 1 — nothing listening
	err := client.Post(context.Background(), testEndpoint, map[string]string{}, nil)
	require.Error(t, err)

	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable)
}

func TestPostTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.httpClient.Timeout = 50 * time.Millisecond

	err := client.Post(context.Background(), testEndpoint, map[string]string{}, nil)
	require.Error(t, err)

	var pErr *Error
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable)
}

func TestErrorString(t *testing.T) {
	t.Run("retryable error", func(t *testing.T) {
		err := &Error{StatusCode: 500, Message: "internal error", Retryable: true}
		assert.Equal(t, "platform error 500: internal error (retryable)", err.Error())
	})

	t.Run("non-retryable error", func(t *testing.T) {
		err := &Error{StatusCode: 400, Message: "bad request", Retryable: false}
		assert.Equal(t, "platform error 400: bad request", err.Error())
	})
}

func TestPostMarshalError(t *testing.T) {
	client := NewClient("http://localhost")
	// channels cannot be marshalled to JSON
	err := client.Post(context.Background(), testEndpoint, make(chan int), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshalling request body")
}

func TestPostInvalidURL(t *testing.T) {
	client := NewClient("://invalid-url")
	err := client.Post(context.Background(), testEndpoint, map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating request")
}

func TestPostInvalidResponseJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeKey, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-valid-json"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var result map[string]string
	err := client.Post(context.Background(), testEndpoint, map[string]string{}, &result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding response body")
}
