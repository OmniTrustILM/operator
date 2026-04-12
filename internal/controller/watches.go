/*
Copyright 2026 OmniTrust ILM.

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
