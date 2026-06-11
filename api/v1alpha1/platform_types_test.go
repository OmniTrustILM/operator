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
