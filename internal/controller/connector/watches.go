/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package connector

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// refNamesFunc extracts the reference names from a Connector that should be
// compared against a watched object's name.
type refNamesFunc func(conn *otilmv1alpha1.Connector) []string

// findConnectorsForRef returns reconcile requests for all Connectors whose
// refs (extracted by getRefNames) include the watched object's name.
func (r *Reconciler) findConnectorsForRef(ctx context.Context, obj client.Object, kind string, getRefNames refNamesFunc) []reconcile.Request {
	logger := log.FromContext(ctx)

	var connectorList otilmv1alpha1.ConnectorList
	if err := r.List(ctx, &connectorList, client.InNamespace(obj.GetNamespace())); err != nil {
		logger.Error(err, "failed to list Connectors for "+kind+" watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range connectorList.Items {
		conn := &connectorList.Items[i]
		for _, name := range getRefNames(conn) {
			if name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(conn),
				})
				break
			}
		}
	}

	if len(requests) > 0 {
		logger.Info(kind+" change triggered reconcile", kind, obj.GetName(), "connectors", len(requests))
	}

	return requests
}

// findConnectorsForSecret returns reconcile requests for all Connectors
// that reference the given Secret in their secretRefs.
func (r *Reconciler) findConnectorsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.findConnectorsForRef(ctx, obj, "secret", func(conn *otilmv1alpha1.Connector) []string {
		names := make([]string, len(conn.Spec.SecretRefs))
		for i, sr := range conn.Spec.SecretRefs {
			names[i] = sr.Name
		}
		return names
	})
}

// findConnectorsForConfigMap returns reconcile requests for all Connectors
// that reference the given ConfigMap in their configMapRefs.
func (r *Reconciler) findConnectorsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.findConnectorsForRef(ctx, obj, "configmap", func(conn *otilmv1alpha1.Connector) []string {
		names := make([]string, len(conn.Spec.ConfigMapRefs))
		for i, cmr := range conn.Spec.ConfigMapRefs {
			names[i] = cmr.Name
		}
		return names
	})
}
