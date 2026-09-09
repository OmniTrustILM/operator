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

// BuildService constructs a ClusterIP Service for the given Proxy via the shared
// Component builder. It exposes the HTTP port (health/metrics) and the
// connector-facing API port — the latter is the in-cluster target a future Connector
// spec.registration.proxyRef registers through.
func BuildService(px *otilmv1alpha1.Proxy) *corev1.Service {
	return common.BuildService(component(px, ""))
}
