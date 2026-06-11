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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestBuildStatefulSetBasics asserts the StatefulSet-specific wiring: name/namespace/
// labels, the headless serviceName (== the component's Service name), the selector, and
// the default replica count.
func TestBuildStatefulSetBasics(t *testing.T) {
	c := Component{
		Name: "core", Instance: "ilm", Namespace: "ilm",
		Image: "hub.omnitrustregistry.com/ilm/core:2.18.0", PullPolicy: "IfNotPresent",
		Replicas: 2, Port: 8080,
		Env: []EnvPair{{Name: "PORT", Value: "8080"}},
	}
	sts := BuildStatefulSet(c)
	require.NotNil(t, sts)
	assert.Equal(t, "core", sts.Name)
	assert.Equal(t, "ilm", sts.Namespace)
	assert.Equal(t, "core", sts.Spec.ServiceName, "serviceName must be the component's headless Service name")
	require.NotNil(t, sts.Spec.Replicas)
	assert.Equal(t, int32(2), *sts.Spec.Replicas)
	assert.Equal(t, map[string]string{
		"app.kubernetes.io/name":     "core",
		"app.kubernetes.io/instance": "ilm",
	}, sts.Spec.Selector.MatchLabels, "selector must carry the name+instance subset")
	// Pod template labels are a superset of the selector.
	assert.Equal(t, "core", sts.Spec.Template.Labels["app.kubernetes.io/name"])
	assert.Equal(t, "ilm", sts.Spec.Template.Labels["app.kubernetes.io/instance"])
	assert.Equal(t, "core", sts.Spec.Template.Labels["app.kubernetes.io/component"])
	// No volumeClaimTemplates: the platform components are stateless (state lives in
	// PG/broker); persistent per-pod storage is a deliberate future knob.
	assert.Empty(t, sts.Spec.VolumeClaimTemplates)
}

// TestBuildStatefulSetRestrictedV2Compliant asserts a StatefulSet's pod template is
// SCC-clean (OpenShift restricted-v2) — the same hardening the Deployment path carries.
func TestBuildStatefulSetRestrictedV2Compliant(t *testing.T) {
	sts := BuildStatefulSet(Component{
		Name: "core", Namespace: "ilm", Image: "x:1", Port: 8080,
		Env:       []EnvPair{{Name: "FOO", Value: "bar"}},
		SecretEnv: []SecretEnvRef{{EnvVar: "PW", SecretName: "s", SecretKey: "password"}},
		InitContainers: []corev1.Container{
			{Name: "wait", Image: "curl:1", Command: []string{"/bin/sh", "-c", "true"}},
		},
		Sidecars: []corev1.Container{{Name: "opa", Image: "opa:1"}},
	})
	requirePodTemplateRestrictedV2(t, sts.Spec.Template)
}

// TestBuildStatefulSetSharesPodTemplateWithDeployment is the load-bearing assertion for
// the shared-pod-template factoring: for the SAME component, BuildDeployment and
// BuildStatefulSet must produce a byte-for-byte identical pod template (the same
// container, env, volumes, scheduling, and SCC hardening) — only the enclosing workload
// kind differs.
func TestBuildStatefulSetSharesPodTemplateWithDeployment(t *testing.T) {
	roRoot := true
	custom := &corev1.SecurityContext{}
	c := Component{
		Name: "core", Instance: "ilm", Namespace: "ilm",
		Image: "x:1", PullPolicy: "IfNotPresent", PullSecrets: []string{"regcred"},
		Replicas: 3, Port: 8080,
		Env:          []EnvPair{{Name: "FOO", Value: "bar"}},
		SecretEnv:    []SecretEnvRef{{EnvVar: "PW", SecretName: "s", SecretKey: "password"}},
		ConfigMapEnv: []ConfigMapEnvRef{{EnvVar: "CM", ConfigMapName: "cm", ConfigMapKey: "k"}},
		FieldRefEnv:  []FieldRefEnv{{EnvVar: "POD", FieldPath: "metadata.name"}},
		InitContainers: []corev1.Container{
			{Name: "wait", Image: "curl:1"},
		},
		Sidecars:               []corev1.Container{{Name: "opa", Image: "opa:1"}},
		Volumes:                []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		VolumeMounts:           []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
		NodeSelector:           map[string]string{"disktype": "ssd"},
		Tolerations:            []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpExists}},
		PodAnnotations:         map[string]string{"a": "b"},
		PodLabels:              map[string]string{"extra": "label"},
		ReadOnlyRootFilesystem: &roRoot,
		SecurityContext:        custom,
	}

	dep := BuildDeployment(c)
	sts := BuildStatefulSet(c)
	assert.Equal(t, dep.Spec.Template, sts.Spec.Template,
		"the Deployment and StatefulSet must enclose an IDENTICAL hardened pod template")
}

// TestBuildStatefulSetOmitsReplicasWhenHPAOwned asserts the SAME OmitReplicas rule as the
// Deployment path: an HPA-owned StatefulSet leaves .spec.replicas UNSET so SSA never
// clobbers the count the HorizontalPodAutoscaler writes.
func TestBuildStatefulSetOmitsReplicasWhenHPAOwned(t *testing.T) {
	sts := BuildStatefulSet(Component{
		Name: "core", Namespace: "ilm", Port: 8080, Replicas: 3, OmitReplicas: true,
	})
	assert.Nil(t, sts.Spec.Replicas, "replicas must be omitted when the HPA owns scaling")
}

// TestBuildStatefulSetSetsReplicasByDefault asserts replicas are set when the component is
// not HPA-owned (mirrors the Deployment path).
func TestBuildStatefulSetSetsReplicasByDefault(t *testing.T) {
	sts := BuildStatefulSet(Component{Name: "core", Namespace: "ilm", Port: 8080, Replicas: 4})
	require.NotNil(t, sts.Spec.Replicas)
	assert.Equal(t, int32(4), *sts.Spec.Replicas)
}
