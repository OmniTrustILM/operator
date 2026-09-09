/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	corev1 "k8s.io/api/core/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildService constructs a Service for the given Connector via the shared Component
// builder. The port shape (int targetPort, TCP) is passed verbatim so live Services
// stay byte-identical to the pre-Component rendering.
func BuildService(conn *otilmv1alpha1.Connector) *corev1.Service {
	return common.BuildService(component(conn, ""))
}
