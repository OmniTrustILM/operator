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

package connector

import (
	policyv1 "k8s.io/api/policy/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildPDB constructs a PodDisruptionBudget for the given Connector via the shared
// Component builder, which honours the MinAvailable/MaxUnavailable mutual exclusion
// (MinAvailable wins; neither set defaults to minAvailable=1). Returns nil when no
// PDB is configured or it is disabled.
func BuildPDB(conn *otilmv1alpha1.Connector) *policyv1.PodDisruptionBudget {
	if conn.Spec.Lifecycle == nil {
		return nil
	}
	return common.BuildPodDisruptionBudget(component(conn, ""), conn.Spec.Lifecycle.PodDisruptionBudget)
}
