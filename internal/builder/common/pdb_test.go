/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pdbComponent() Component {
	return Component{Name: "core", Instance: "ilm", Namespace: "ilm-system"}
}

func TestBuildPodDisruptionBudgetNilOrDisabled(t *testing.T) {
	c := pdbComponent()
	assert.Nil(t, BuildPodDisruptionBudget(c, nil), "nil spec renders no PDB")
	assert.Nil(t, BuildPodDisruptionBudget(c, &otilmv1alpha1.PDBSpec{Enabled: false}),
		"disabled spec renders no PDB")
}

func TestBuildPodDisruptionBudgetDefaultMinAvailable(t *testing.T) {
	c := pdbComponent()
	pdb := BuildPodDisruptionBudget(c, &otilmv1alpha1.PDBSpec{Enabled: true})
	require.NotNil(t, pdb)
	assert.Equal(t, "core", pdb.Name)
	assert.Equal(t, "ilm-system", pdb.Namespace)
	// Default minAvailable=1 when neither minAvailable nor maxUnavailable is set.
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable)
	assert.Nil(t, pdb.Spec.MaxUnavailable)
	// Selector targets the component's immutable selector labels.
	assert.Equal(t, c.SelectorLabels(), pdb.Spec.Selector.MatchLabels)
	// Carries the standard recommended labels.
	assert.Equal(t, "core", pdb.Labels[ComponentLabel])
}

func TestBuildPodDisruptionBudgetExplicitMinAvailable(t *testing.T) {
	c := pdbComponent()
	minA := intstr.FromString("50%")
	pdb := BuildPodDisruptionBudget(c, &otilmv1alpha1.PDBSpec{Enabled: true, MinAvailable: &minA})
	require.NotNil(t, pdb)
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromString("50%"), *pdb.Spec.MinAvailable)
	assert.Nil(t, pdb.Spec.MaxUnavailable)
}

func TestBuildPodDisruptionBudgetMaxUnavailable(t *testing.T) {
	c := pdbComponent()
	maxU := intstr.FromInt32(1)
	pdb := BuildPodDisruptionBudget(c, &otilmv1alpha1.PDBSpec{Enabled: true, MaxUnavailable: &maxU})
	require.NotNil(t, pdb)
	// maxUnavailable honoured when minAvailable is unset.
	require.NotNil(t, pdb.Spec.MaxUnavailable)
	assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MaxUnavailable)
	assert.Nil(t, pdb.Spec.MinAvailable)
}

func TestBuildPodDisruptionBudgetMinAvailableWinsOverMaxUnavailable(t *testing.T) {
	c := pdbComponent()
	minA := intstr.FromInt32(2)
	maxU := intstr.FromInt32(1)
	pdb := BuildPodDisruptionBudget(c, &otilmv1alpha1.PDBSpec{Enabled: true, MinAvailable: &minA, MaxUnavailable: &maxU})
	require.NotNil(t, pdb)
	// A PDB may carry at most one; minAvailable takes precedence and maxUnavailable is dropped.
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, intstr.FromInt32(2), *pdb.Spec.MinAvailable)
	assert.Nil(t, pdb.Spec.MaxUnavailable)
}
