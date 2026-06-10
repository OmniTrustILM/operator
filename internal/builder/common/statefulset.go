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

package common

import (
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildStatefulSet renders a StatefulSet (apps/v1) for a component, with the SAME
// SCC-clean (OpenShift restricted-v2) hardened pod template the Deployment path uses:
// it consumes the shared buildPodTemplateSpec, so the container set, env, scheduling,
// volumes, and per-container hardening are byte-for-byte identical to BuildDeployment —
// only the enclosing workload kind differs.
//
// StatefulSet specifics:
//   - spec.serviceName is the component's Service name (its ResourceName, the same name
//     BuildService renders), so the StatefulSet's pods get a stable per-ordinal DNS
//     identity under that Service.
//   - spec.replicas follows the SAME OmitReplicas rule as BuildDeployment: it is left
//     UNSET for an HPA-owned component so the operator's Server-Side-Apply field manager
//     never clobbers the count the HorizontalPodAutoscaler writes each reconcile.
//   - spec.selector and spec.template come from the component's selector labels and the
//     shared pod template.
//
// It intentionally renders NO volumeClaimTemplates: the platform components are stateless
// (they keep their state in the database/broker, not on per-pod volumes), so a StatefulSet
// here buys stable identity / ordered rollout, not persistence. Persistent per-pod storage
// (volumeClaimTemplates) is a deliberate future knob.
func BuildStatefulSet(c Component) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: c.ResourceName(), Namespace: c.Namespace, Labels: c.Labels()},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: c.ResourceName(),
			Selector:    &metav1.LabelSelector{MatchLabels: c.SelectorLabels()},
			Template:    buildPodTemplateSpec(c),
		},
	}
	// Set .spec.replicas only when the component is NOT HPA-owned (same rule as the
	// Deployment path): for an HPA-owned component leave it unset so SSA never sends — and
	// so never clobbers — the replica count the HorizontalPodAutoscaler writes.
	if !c.OmitReplicas {
		replicas := c.Replicas
		sts.Spec.Replicas = &replicas
	}
	return sts
}
