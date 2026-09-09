/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	corev1 "k8s.io/api/core/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// BuildServiceAccount constructs a dedicated ServiceAccount for the given Proxy via
// the shared Component builder.
func BuildServiceAccount(px *otilmv1alpha1.Proxy) *corev1.ServiceAccount {
	return common.BuildServiceAccount(component(px, ""))
}
