/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// findConnectorsForSecret returns reconcile requests for all Connectors
// that reference the given Secret in their secretRefs.
func (r *ConnectorReconciler) findConnectorsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	var connectorList otilmv1alpha1.ConnectorList
	if err := r.List(ctx, &connectorList, client.InNamespace(obj.GetNamespace())); err != nil {
		logger.Error(err, "failed to list Connectors for Secret watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range connectorList.Items {
		conn := &connectorList.Items[i]
		for _, sr := range conn.Spec.SecretRefs {
			if sr.Name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(conn),
				})
				break
			}
		}
	}

	if len(requests) > 0 {
		logger.Info("Secret change triggered reconcile", "secret", obj.GetName(), "connectors", len(requests))
	}

	return requests
}

// findConnectorsForConfigMap returns reconcile requests for all Connectors
// that reference the given ConfigMap in their configMapRefs.
func (r *ConnectorReconciler) findConnectorsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	var connectorList otilmv1alpha1.ConnectorList
	if err := r.List(ctx, &connectorList, client.InNamespace(obj.GetNamespace())); err != nil {
		logger.Error(err, "failed to list Connectors for ConfigMap watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range connectorList.Items {
		conn := &connectorList.Items[i]
		for _, cmr := range conn.Spec.ConfigMapRefs {
			if cmr.Name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(conn),
				})
				break
			}
		}
	}

	if len(requests) > 0 {
		logger.Info("ConfigMap change triggered reconcile", "configmap", obj.GetName(), "connectors", len(requests))
	}

	return requests
}
