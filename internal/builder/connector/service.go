/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"fmt"

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

// ServiceEndpoint is the in-cluster URL of the connector's Service. Registration and the
// health check both use it.
func ServiceEndpoint(conn *otilmv1alpha1.Connector) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ChildResourceName(conn), conn.Namespace, conn.Spec.Service.Port)
}
