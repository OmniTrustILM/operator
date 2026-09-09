/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector_test

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/connector"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestBuildPDB(t *testing.T) {
	conn := newTestConnector()
	minAvailable := intstr.FromInt32(1)
	conn.Spec.Lifecycle = &otilmv1alpha1.LifecycleSpec{
		PodDisruptionBudget: &otilmv1alpha1.PDBSpec{
			Enabled:      true,
			MinAvailable: &minAvailable,
		},
	}

	pdb := connector.BuildPDB(conn)

	assert.NotNil(t, pdb)
	assert.Equal(t, testConnectorName, pdb.Name)
	assert.Equal(t, "default", pdb.Namespace)
	assert.Equal(t, connector.Labels(conn), pdb.Labels)
	assert.Equal(t, connector.SelectorLabels(conn), pdb.Spec.Selector.MatchLabels)
	assert.Equal(t, &minAvailable, pdb.Spec.MinAvailable)
}

func TestBuildPDBDisabled(t *testing.T) {
	conn := newTestConnector()
	// No lifecycle spec set

	pdb := connector.BuildPDB(conn)

	assert.Nil(t, pdb)
}

func TestBuildPDBExplicitlyDisabled(t *testing.T) {
	conn := newTestConnector()
	conn.Spec.Lifecycle = &otilmv1alpha1.LifecycleSpec{
		PodDisruptionBudget: &otilmv1alpha1.PDBSpec{
			Enabled: false,
		},
	}

	pdb := connector.BuildPDB(conn)

	assert.Nil(t, pdb)
}
