/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func TestBuildPDBNilWhenUnsetOrDisabled(t *testing.T) {
	assert.Nil(t, BuildPDB(newProxy()))
	px := newProxy()
	px.Spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: false}
	assert.Nil(t, BuildPDB(px))
}

func TestBuildPDBDefaultsMinAvailable(t *testing.T) {
	px := newProxy()
	px.Spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true}
	pdb := BuildPDB(px)
	require.NotNil(t, pdb)
	assert.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable)
	assert.Equal(t, SelectorLabels(px), pdb.Spec.Selector.MatchLabels)
}

func TestBuildPDBHonoursMaxUnavailable(t *testing.T) {
	px := newProxy()
	maxUnavail := intstr.FromInt32(1)
	px.Spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{Enabled: true, MaxUnavailable: &maxUnavail}
	pdb := BuildPDB(px)
	require.NotNil(t, pdb)
	assert.Nil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, maxUnavail, *pdb.Spec.MaxUnavailable)
}

func TestBuildPDBMinAvailableWinsOverMaxUnavailable(t *testing.T) {
	px := newProxy()
	minAvail := intstr.FromInt32(2)
	maxUnavail := intstr.FromInt32(1)
	px.Spec.PodDisruptionBudget = &otilmv1alpha1.PDBSpec{
		Enabled: true, MinAvailable: &minAvail, MaxUnavailable: &maxUnavail,
	}
	pdb := BuildPDB(px)
	require.NotNil(t, pdb)
	assert.Equal(t, minAvail, *pdb.Spec.MinAvailable)
	assert.Nil(t, pdb.Spec.MaxUnavailable, "mutually exclusive: MinAvailable takes precedence")
}
