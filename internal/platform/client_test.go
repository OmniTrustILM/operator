package platform

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

func TestPostSuccess(t *testing.T) {
	type response struct {
		Message string `json:"message"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body map[string]string
		err := json.NewDecoder(r.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "hello", body["key"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(response{Message: "ok"}))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var result response
	err := client.Post(context.Background(), "/test", map[string]string{"key": "hello"}, &result)
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(server.URL)
			err := client.Post(context.Background(), "/test", map[string]string{}, nil)
			require.Error(t, err)

			var pErr *PlatformError
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, tt.statusCode, pErr.StatusCode)
			assert.Equal(t, tt.retryable, pErr.Retryable)
			assert.Contains(t, pErr.Message, tt.body)
		})
	}
}

func TestPostNetworkError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1") // port 1 — nothing listening
	err := client.Post(context.Background(), "/test", map[string]string{}, nil)
	require.Error(t, err)

	var pErr *PlatformError
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable)
}

func TestPostTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	client.httpClient.Timeout = 50 * time.Millisecond

	err := client.Post(context.Background(), "/test", map[string]string{}, nil)
	require.Error(t, err)

	var pErr *PlatformError
	require.ErrorAs(t, err, &pErr)
	assert.True(t, pErr.Retryable)
}

func TestPlatformErrorString(t *testing.T) {
	t.Run("retryable error", func(t *testing.T) {
		err := &PlatformError{StatusCode: 500, Message: "internal error", Retryable: true}
		assert.Equal(t, "platform error 500: internal error (retryable)", err.Error())
	})

	t.Run("non-retryable error", func(t *testing.T) {
		err := &PlatformError{StatusCode: 400, Message: "bad request", Retryable: false}
		assert.Equal(t, "platform error 400: bad request", err.Error())
	})
}

func TestPostMarshalError(t *testing.T) {
	client := NewClient("http://localhost")
	// channels cannot be marshalled to JSON
	err := client.Post(context.Background(), "/test", make(chan int), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshalling request body")
}

func TestPostInvalidURL(t *testing.T) {
	client := NewClient("://invalid-url")
	err := client.Post(context.Background(), "/test", map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating request")
}

func TestPostInvalidResponseJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-valid-json"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var result map[string]string
	err := client.Post(context.Background(), "/test", map[string]string{}, &result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding response body")
}
