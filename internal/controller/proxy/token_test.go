/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeToken builds an unsigned JWT-shaped string with the given claims.
func makeToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestTokenExpiry(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).Unix()

	tests := []struct {
		name      string
		token     string
		wantFound bool
		wantUnix  int64
	}{
		{"expired token", makeToken(t, map[string]any{"exp": past}), true, past},
		{"valid token", makeToken(t, map[string]any{"exp": future}), true, future},
		{"no exp claim", makeToken(t, map[string]any{"v": 1}), false, 0},
		{"not a jwt", "just-a-string", false, 0},
		{"bad base64 payload", "aGVhZGVy.!!!.sig", false, 0},
		{"empty", "", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp, found := tokenExpiry(tt.token)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.wantUnix, exp.Unix())
			}
		})
	}
}
