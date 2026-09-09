/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
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
