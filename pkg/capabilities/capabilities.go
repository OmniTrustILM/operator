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

// Package capabilities provides a small, reusable detector for whether the
// cluster currently serves a given API GroupKind — the operator's check for
// upstream-operator / upstream-CRD availability.
//
// The platform reconciler depends on resources owned by other operators and
// CRD bundles that are NOT installed by this operator (they are cluster-singleton
// infrastructure): cert-manager (cert-manager.io Certificate/Issuer) for the
// cert-managed edge modes, and the Gateway API CRDs (gateway.networking.k8s.io
// Gateway/HTTPRoute) for the Gateway API edge. Rather than assume those CRDs are
// served — which makes a Server-Side Apply fail with a cryptic "no matches for
// kind" error — the operator detects them first and gates the dependent objects.
//
// The detector is deliberately generic (it takes any GroupKind) so the
// managed-infra milestone can reuse it for the CloudNativePG, RabbitMQ, and
// Keycloak operator CRDs as well.
package capabilities

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Detector reports whether the cluster serves a given API GroupKind. It is backed
// by a controller-runtime RESTMapper. The manager's default mapper is a dynamic
// mapper that re-discovers on a miss, so a GroupKind that becomes available after
// the operator started (e.g. cert-manager is installed later) is picked up on a
// subsequent detection without restarting the operator.
type Detector struct {
	mapper meta.RESTMapper
}

// New returns a Detector backed by the given RESTMapper (typically
// mgr.GetRESTMapper()).
func New(m meta.RESTMapper) *Detector {
	return &Detector{mapper: m}
}

// Available reports whether the cluster serves the given GroupKind at some
// version. When one or more versions are supplied, the mapping is restricted to
// those versions; with none supplied the preferred mapping for the kind is used.
//
// A "no matches for kind" outcome (meta.IsNoMatchError — i.e. the CRD/operator is
// not installed) is reported as (false, nil), so callers can gracefully gate the
// dependent objects. Any other error (e.g. a transient discovery failure) is
// propagated unchanged so the caller can decide whether to retry.
func (d *Detector) Available(gk schema.GroupKind, versions ...string) (bool, error) {
	if _, err := d.mapper.RESTMapping(gk, versions...); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
