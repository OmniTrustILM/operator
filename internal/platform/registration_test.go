package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func TestRegisterSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v2/connector/register", r.URL.Path)

		var req RegistrationRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)
		assert.Equal(t, "my-connector", req.Name)
		assert.Equal(t, "v2", req.Version)
		assert.Equal(t, "http://my-connector.default.svc.cluster.local:8080", req.URL)
		assert.Equal(t, "basic", req.AuthType)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(RegistrationResponse{
			UUID:   "abc-123",
			Name:   "my-connector",
			Status: "waitingForApproval",
		}))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	resp, err := Register(context.Background(), client, &RegistrationRequest{
		Name:     "my-connector",
		Version:  "v2",
		URL:      "http://my-connector.default.svc.cluster.local:8080",
		AuthType: "basic",
	})
	require.NoError(t, err)
	assert.Equal(t, "abc-123", resp.UUID)
	assert.Equal(t, "my-connector", resp.Name)
	assert.Equal(t, "waitingForApproval", resp.Status)
}

func TestRegisterErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		retryable  bool
	}{
		{
			name:       "already exists returns non-retryable error",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"connector already registered"}`,
			retryable:  false,
		},
		{
			name:       "server error returns retryable error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"internal error"}`,
			retryable:  true,
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
			resp, err := Register(context.Background(), client, &RegistrationRequest{
				Name:    "my-connector",
				Version: "v2",
			})
			require.Error(t, err)
			assert.Nil(t, resp)

			var pErr *PlatformError
			require.ErrorAs(t, err, &pErr)
			assert.Equal(t, tt.statusCode, pErr.StatusCode)
			assert.Equal(t, tt.retryable, pErr.Retryable)
		})
	}
}

func TestBuildRegistrationRequest(t *testing.T) {
	reg := &otilmv1alpha1.RegistrationSpec{
		Name:     "my-connector",
		AuthType: otilmv1alpha1.AuthTypeBasic,
		AuthAttributes: []otilmv1alpha1.RegistrationAttribute{
			{
				Name:    "username",
				Content: apiextensionsv1.JSON{Raw: []byte(`"admin"`)},
			},
		},
		CustomAttributes: []otilmv1alpha1.RegistrationAttribute{
			{
				Name:    "region",
				Content: apiextensionsv1.JSON{Raw: []byte(`"eu-west-1"`)},
			},
		},
	}

	req := BuildRegistrationRequest("test-connector", "http://my-connector.default.svc.cluster.local:8080", reg)
	assert.Equal(t, "my-connector", req.Name)
	assert.Equal(t, "v2", req.Version)
	assert.Equal(t, "http://my-connector.default.svc.cluster.local:8080", req.URL)
	assert.Equal(t, "basic", req.AuthType)

	require.Len(t, req.AuthAttributes, 1)
	assert.Equal(t, "username", req.AuthAttributes[0].Name)
	assert.Equal(t, "admin", req.AuthAttributes[0].Content)

	require.Len(t, req.CustomAttributes, 1)
	assert.Equal(t, "region", req.CustomAttributes[0].Name)
	assert.Equal(t, "eu-west-1", req.CustomAttributes[0].Content)
}

func TestBuildRegistrationRequestAuthNone(t *testing.T) {
	reg := &otilmv1alpha1.RegistrationSpec{
		Name:     "simple-connector",
		AuthType: otilmv1alpha1.AuthTypeNone,
	}

	req := BuildRegistrationRequest("simple-connector", "http://simple.default.svc.cluster.local:8080", reg)
	assert.Equal(t, "simple-connector", req.Name)
	assert.Equal(t, "v2", req.Version)
	assert.Equal(t, "http://simple.default.svc.cluster.local:8080", req.URL)
	assert.Equal(t, "none", req.AuthType)
	assert.Empty(t, req.AuthAttributes)
	assert.Empty(t, req.CustomAttributes)
}
