/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildServiceAccount(t *testing.T) {
	sa := BuildServiceAccount(newProxy())
	assert.Equal(t, "dc-east", sa.Name)
	assert.Equal(t, "ilm-edge", sa.Namespace)
	assert.Equal(t, Labels(newProxy()), sa.Labels)
}
