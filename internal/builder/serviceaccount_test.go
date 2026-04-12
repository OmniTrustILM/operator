package builder_test

import (
	"testing"

	"github.com/OmniTrustILM/operator/internal/builder"
	"github.com/stretchr/testify/assert"
)

func TestBuildServiceAccount(t *testing.T) {
	conn := newTestConnector()
	sa := builder.BuildServiceAccount(conn)

	assert.Equal(t, "test-connector", sa.Name)
	assert.Equal(t, "default", sa.Namespace)
	assert.Equal(t, "test-connector", sa.Labels[builder.NameLabel])
	assert.Equal(t, "ilm-operator", sa.Labels[builder.ManagedByLabel])
}
