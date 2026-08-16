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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

func hpaComponent() Component {
	return Component{Name: "core", Instance: "ilm", Namespace: "ilm-system"}
}

func TestBuildHorizontalPodAutoscalerNil(t *testing.T) {
	assert.Nil(t, BuildHorizontalPodAutoscaler(hpaComponent(), nil), "nil spec renders no HPA")
}

func TestBuildHorizontalPodAutoscalerScaleTargetAndBounds(t *testing.T) {
	c := hpaComponent()
	minR := int32(2)
	cpu := int32(75)
	hpa := BuildHorizontalPodAutoscaler(c, &otilmv1alpha1.AutoscalingSpec{
		MinReplicas: &minR, MaxReplicas: 6, TargetCPUUtilization: &cpu,
	})
	require.NotNil(t, hpa)
	assert.Equal(t, "core", hpa.Name)
	assert.Equal(t, "ilm-system", hpa.Namespace)
	assert.Equal(t, "core", hpa.Labels[ComponentLabel])

	// scaleTargetRef points at the component's apps/v1 Deployment.
	assert.Equal(t, testAppsV1APIVersion, hpa.Spec.ScaleTargetRef.APIVersion)
	assert.Equal(t, "Deployment", hpa.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, "core", hpa.Spec.ScaleTargetRef.Name)

	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(6), hpa.Spec.MaxReplicas)

	// One CPU resource metric on average utilization.
	require.Len(t, hpa.Spec.Metrics, 1)
	m := hpa.Spec.Metrics[0]
	assert.Equal(t, autoscalingv2.ResourceMetricSourceType, m.Type)
	require.NotNil(t, m.Resource)
	assert.Equal(t, corev1.ResourceCPU, m.Resource.Name)
	assert.Equal(t, autoscalingv2.UtilizationMetricType, m.Resource.Target.Type)
	require.NotNil(t, m.Resource.Target.AverageUtilization)
	assert.Equal(t, int32(75), *m.Resource.Target.AverageUtilization)
}

func TestBuildHorizontalPodAutoscalerCPUAndMemoryMetrics(t *testing.T) {
	c := hpaComponent()
	cpu := int32(80)
	mem := int32(70)
	hpa := BuildHorizontalPodAutoscaler(c, &otilmv1alpha1.AutoscalingSpec{
		MaxReplicas: 4, TargetCPUUtilization: &cpu, TargetMemoryUtilization: &mem,
	})
	require.NotNil(t, hpa)
	// MinReplicas left to the autoscaling/v2 default (nil) when unset.
	assert.Nil(t, hpa.Spec.MinReplicas)
	require.Len(t, hpa.Spec.Metrics, 2, "both CPU and memory metrics render")

	byResource := map[corev1.ResourceName]int32{}
	for _, m := range hpa.Spec.Metrics {
		require.NotNil(t, m.Resource)
		require.NotNil(t, m.Resource.Target.AverageUtilization)
		byResource[m.Resource.Name] = *m.Resource.Target.AverageUtilization
	}
	assert.Equal(t, int32(80), byResource[corev1.ResourceCPU])
	assert.Equal(t, int32(70), byResource[corev1.ResourceMemory])
}

func TestBuildHorizontalPodAutoscalerNoMetricsWhenNoTargets(t *testing.T) {
	c := hpaComponent()
	hpa := BuildHorizontalPodAutoscaler(c, &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 3})
	require.NotNil(t, hpa)
	assert.Empty(t, hpa.Spec.Metrics, "no target utilization => no metrics")
}

// TestBuildHPAScaleTargetFollowsWorkloadType pins the scale target to the kind the component
// is actually rendered as. An HPA aimed at a Deployment that does not exist (because the
// component renders as a StatefulSet) never scales and reports FailedGetScale — silently.
func TestBuildHPAScaleTargetFollowsWorkloadType(t *testing.T) {
	spec := &otilmv1alpha1.AutoscalingSpec{MaxReplicas: 5}

	dep := BuildHorizontalPodAutoscaler(Component{Name: "core", Namespace: "ilm"}, spec)
	require.NotNil(t, dep)
	assert.Equal(t, "Deployment", dep.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, testAppsV1APIVersion, dep.Spec.ScaleTargetRef.APIVersion)
	assert.Equal(t, "core", dep.Spec.ScaleTargetRef.Name)

	sts := BuildHorizontalPodAutoscaler(Component{
		Name: "core", Namespace: "ilm", WorkloadType: otilmv1alpha1.WorkloadKindStatefulSet,
	}, spec)
	require.NotNil(t, sts)
	assert.Equal(t, "StatefulSet", sts.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, testAppsV1APIVersion, sts.Spec.ScaleTargetRef.APIVersion)
	assert.Equal(t, "core", sts.Spec.ScaleTargetRef.Name)
}
