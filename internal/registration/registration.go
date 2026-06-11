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

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Request represents a connector registration request to the platform.
type Request struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	URL              string `json:"url"`
	AuthType         string `json:"authType"`
	AuthAttributes   []Attr `json:"authAttributes"`
	CustomAttributes []Attr `json:"customAttributes"`
}

// Attr represents a name/content attribute pair for registration.
type Attr struct {
	Name    string `json:"name"`
	Content any    `json:"content"`
}

// Response represents the platform's response to a registration request.
type Response struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

const registrationPath = "/v2/connector/register"

// Register calls POST /v2/connector/register on the platform.
func Register(ctx context.Context, client *Client, req *Request) (*Response, error) {
	var resp Response
	if err := client.Post(ctx, registrationPath, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// BuildRequest creates a Request from a Connector spec and service endpoint.
func BuildRequest(serviceEndpoint string, reg *otilmv1alpha1.RegistrationSpec) *Request {
	return &Request{
		Name:             reg.Name,
		Version:          "v2",
		URL:              serviceEndpoint,
		AuthType:         string(reg.AuthType),
		AuthAttributes:   convertAttributes(reg.AuthAttributes),
		CustomAttributes: convertAttributes(reg.CustomAttributes),
	}
}

// convertAttributes converts CRD RegistrationAttribute slices to registration Attr slices.
func convertAttributes(attrs []otilmv1alpha1.RegistrationAttribute) []Attr {
	if len(attrs) == 0 {
		return nil
	}
	result := make([]Attr, 0, len(attrs))
	for _, a := range attrs {
		var content any
		// Unmarshal the raw JSON to get a native Go value
		if a.Content.Raw != nil {
			if err := json.Unmarshal(a.Content.Raw, &content); err != nil {
				log.Log.Info("failed to unmarshal registration attribute content, using raw value",
					"name", a.Name, "error", err)
				content = string(a.Content.Raw)
			}
		}
		result = append(result, Attr{
			Name:    a.Name,
			Content: content,
		})
	}
	return result
}
