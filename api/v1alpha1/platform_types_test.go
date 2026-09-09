/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlatformObjectsConstruct(t *testing.T) {
	require.NotNil(t, &Platform{})
	require.NotNil(t, &PlatformList{})
}

func TestPlatformSpecFields(t *testing.T) {
	p := Platform{
		Spec: PlatformSpec{
			Database:  DatabaseSpec{Host: "pg", Name: "ilmdb", Credentials: &CredentialsRef{SecretRef: "ilm-db"}},
			Messaging: MessagingSpec{Host: "rabbit", VirtualHost: "ilm", Credentials: &CredentialsRef{SecretRef: "ilm-messaging"}},
		},
	}
	assert.Equal(t, "pg", p.Spec.Database.Host)
	assert.Equal(t, "ilm", p.Spec.Messaging.VirtualHost)
}
