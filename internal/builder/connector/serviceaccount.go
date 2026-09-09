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

// BuildServiceAccount constructs a ServiceAccount for the given Connector via the
// shared Component builder.
func BuildServiceAccount(conn *otilmv1alpha1.Connector) *corev1.ServiceAccount {
	return common.BuildServiceAccount(component(conn, ""))
}
