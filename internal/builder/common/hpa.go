/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildHorizontalPodAutoscaler renders an autoscaling/v2 HorizontalPodAutoscaler for the
// component, scaling its workload (scaleTargetRef → apps/v1 kind named after the
// component) between MinReplicas and MaxReplicas on the configured CPU/memory utilization
// targets. autoscaling/v2 is a core, always-served API, so no capability gating is needed.
//
// It returns nil when the spec is nil, so callers can append the result unconditionally.
// When the component is HPA-owned its workload MUST omit .spec.replicas (see
// Component.OmitReplicas) so the operator's Server-Side-Apply field manager never clobbers
// the replica count the HPA writes. A target utilization is emitted as a Resource metric
// only when set; a spec with neither CPU nor memory target renders an HPA with no metrics
// (the autoscaler will not scale until a metric is added).
func BuildHorizontalPodAutoscaler(c Component, spec *otilmv1alpha1.AutoscalingSpec) *autoscalingv2.HorizontalPodAutoscaler {
	if spec == nil {
		return nil
	}

	var metrics []autoscalingv2.MetricSpec
	if spec.TargetCPUUtilization != nil {
		metrics = append(metrics, resourceUtilizationMetric(corev1.ResourceCPU, *spec.TargetCPUUtilization))
	}
	if spec.TargetMemoryUtilization != nil {
		metrics = append(metrics, resourceUtilizationMetric(corev1.ResourceMemory, *spec.TargetMemoryUtilization))
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.ResourceName(),
			Namespace: c.Namespace,
			Labels:    c.Labels(),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       scaleTargetKind(c),
				Name:       c.ResourceName(),
			},
			MinReplicas: spec.MinReplicas,
			MaxReplicas: spec.MaxReplicas,
			Metrics:     metrics,
		},
	}
}

// scaleTargetKind returns the apps/v1 Kind the component is actually rendered as, so the
// HPA's scaleTargetRef addresses the object that exists. Anything other than an explicit
// StatefulSet is a Deployment (the default), matching the render layer's own choice.
func scaleTargetKind(c Component) string {
	if c.WorkloadType == otilmv1alpha1.WorkloadKindStatefulSet {
		return string(otilmv1alpha1.WorkloadKindStatefulSet)
	}
	return string(otilmv1alpha1.WorkloadKindDeployment)
}

// resourceUtilizationMetric builds one autoscaling/v2 Resource metric on the given
// container resource, targeting the given average-utilization percentage.
func resourceUtilizationMetric(name corev1.ResourceName, target int32) autoscalingv2.MetricSpec {
	utilization := target
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: &utilization,
			},
		},
	}
}
