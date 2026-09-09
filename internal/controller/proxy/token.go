/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package proxy implements the Kubernetes reconciler for Proxy custom resources.
package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// tokenExpiry extracts the registered exp claim from a JWT-shaped config token
// WITHOUT verifying it and WITHOUT reading any other claim. The token's config claim
// embeds broker credentials, so nothing beyond exp is ever decoded into a value the
// reconciler holds, and the token is never logged. Returns found=false for anything
// that is not a parseable JWT with a numeric exp — that is not an error condition
// (unsigned/exp-less tokens are valid; the proxy binary does its own validation).
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(*claims.Exp, 0), true
}
