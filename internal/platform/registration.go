package platform

import (
	"context"
	"encoding/json"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// RegistrationRequest represents a connector registration request to the platform.
type RegistrationRequest struct {
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	URL              string             `json:"url"`
	AuthType         string             `json:"authType"`
	AuthAttributes   []RegistrationAttr `json:"authAttributes"`
	CustomAttributes []RegistrationAttr `json:"customAttributes"`
}

// RegistrationAttr represents a name/content attribute pair for registration.
type RegistrationAttr struct {
	Name    string `json:"name"`
	Content any    `json:"content"`
}

// RegistrationResponse represents the platform's response to a registration request.
type RegistrationResponse struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

const registrationPath = "/v2/connector/register"

// Register calls POST /v2/connector/register on the platform.
func Register(ctx context.Context, client *Client, req *RegistrationRequest) (*RegistrationResponse, error) {
	var resp RegistrationResponse
	if err := client.Post(ctx, registrationPath, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BuildRegistrationRequest creates a RegistrationRequest from a Connector spec and service endpoint.
func BuildRegistrationRequest(connectorName, serviceEndpoint string, reg *otilmv1alpha1.RegistrationSpec) *RegistrationRequest {
	return &RegistrationRequest{
		Name:             reg.Name,
		Version:          "v2",
		URL:              serviceEndpoint,
		AuthType:         string(reg.AuthType),
		AuthAttributes:   convertAttributes(reg.AuthAttributes),
		CustomAttributes: convertAttributes(reg.CustomAttributes),
	}
}

// convertAttributes converts CRD RegistrationAttribute slices to platform RegistrationAttr slices.
func convertAttributes(attrs []otilmv1alpha1.RegistrationAttribute) []RegistrationAttr {
	if len(attrs) == 0 {
		return nil
	}
	result := make([]RegistrationAttr, 0, len(attrs))
	for _, a := range attrs {
		var content any
		// Unmarshal the raw JSON to get a native Go value
		if a.Content.Raw != nil {
			_ = json.Unmarshal(a.Content.Raw, &content)
		}
		result = append(result, RegistrationAttr{
			Name:    a.Name,
			Content: content,
		})
	}
	return result
}
