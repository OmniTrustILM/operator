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
