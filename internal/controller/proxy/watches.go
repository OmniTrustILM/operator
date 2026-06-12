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

package proxy

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// findProxiesForSecret returns reconcile requests for all Proxies in the Secret's
// namespace that reference the watched Secret — via configTokenSecretRef (the
// trigger that turns a credential rotation into an automatic rollout) or via
// spec.secretRefs (e.g. a rotated CA bundle).
func (r *Reconciler) findProxiesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.findProxiesForRef(ctx, obj, "secret", func(px *otilmv1alpha1.Proxy) []string {
		names := make([]string, 0, 1+len(px.Spec.SecretRefs))
		names = append(names, px.Spec.ConfigTokenSecretRef.Name)
		for _, sr := range px.Spec.SecretRefs {
			names = append(names, sr.Name)
		}
		return names
	})
}

// findProxiesForConfigMap returns reconcile requests for all Proxies that reference
// the watched ConfigMap in their configMapRefs.
func (r *Reconciler) findProxiesForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.findProxiesForRef(ctx, obj, "configmap", func(px *otilmv1alpha1.Proxy) []string {
		names := make([]string, len(px.Spec.ConfigMapRefs))
		for i, cmr := range px.Spec.ConfigMapRefs {
			names[i] = cmr.Name
		}
		return names
	})
}

// findProxiesForRef returns reconcile requests for all Proxies whose refs (extracted
// by getRefNames) include the watched object's name.
func (r *Reconciler) findProxiesForRef(ctx context.Context, obj client.Object, kind string, getRefNames func(*otilmv1alpha1.Proxy) []string) []reconcile.Request {
	logger := log.FromContext(ctx)

	var proxies otilmv1alpha1.ProxyList
	if err := r.List(ctx, &proxies, client.InNamespace(obj.GetNamespace())); err != nil {
		logger.Error(err, "failed to list Proxies for "+kind+" watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range proxies.Items {
		for _, name := range getRefNames(&proxies.Items[i]) {
			if name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&proxies.Items[i]),
				})
				break
			}
		}
	}

	if len(requests) > 0 {
		logger.Info(kind+" change triggered reconcile", kind, obj.GetName(), "proxies", len(requests))
	}

	return requests
}
