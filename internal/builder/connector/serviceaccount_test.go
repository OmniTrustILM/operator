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

package connector_test

import (
	"testing"

	"github.com/OmniTrustILM/operator/internal/builder/connector"
	"github.com/stretchr/testify/assert"
)

func TestBuildServiceAccount(t *testing.T) {
	conn := newTestConnector()
	sa := connector.BuildServiceAccount(conn)

	assert.Equal(t, testConnectorName, sa.Name)
	assert.Equal(t, "default", sa.Namespace)
	assert.Equal(t, testConnectorName, sa.Labels[connector.NameLabel])
	assert.Equal(t, "ilm-operator", sa.Labels[connector.ManagedByLabel])
}
