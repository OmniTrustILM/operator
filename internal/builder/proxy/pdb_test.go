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
