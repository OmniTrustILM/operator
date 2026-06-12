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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/bom"
)

const testEgressProxyURL = "http://egress:3128"

func findEnv(envs []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i]
		}
	}
	return nil
}

func TestBuildDeploymentTokenEnvWiring(t *testing.T) {
	px := newProxy()
	d := BuildDeployment(px, "abc123")
	require.Len(t, d.Spec.Template.Spec.Containers, 1)
	c := d.Spec.Template.Spec.Containers[0]

	tok := findEnv(c.Env, EnvConfigToken)
	require.NotNil(t, tok, "PROXY_CONFIG_TOKEN must be set")
	require.NotNil(t, tok.ValueFrom)
	require.NotNil(t, tok.ValueFrom.SecretKeyRef)
	assert.Equal(t, "dc-east-config", tok.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "configToken", tok.ValueFrom.SecretKeyRef.Key)
	assert.Nil(t, tok.ValueFrom.SecretKeyRef.Optional, "token key is required")

	sig := findEnv(c.Env, EnvTokenSigningKey)
	require.NotNil(t, sig, "PROXY_TOKEN_SIGNING_KEY must be set (optional ref)")
	require.NotNil(t, sig.ValueFrom.SecretKeyRef.Optional)
	assert.True(t, *sig.ValueFrom.SecretKeyRef.Optional)
	assert.Equal(t, "tokenSigningKey", sig.ValueFrom.SecretKeyRef.Key)
}

func TestBuildDeploymentCustomKeysAndUserEnv(t *testing.T) {
	px := newProxy()
	px.Spec.ConfigTokenSecretRef.TokenKey = "tok"
	px.Spec.Env = []otilmv1alpha1.EnvVar{{Name: "HTTPS_PROXY", Value: testEgressProxyURL}}
	c := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0]

	assert.Equal(t, "tok", findEnv(c.Env, EnvConfigToken).ValueFrom.SecretKeyRef.Key)
	hp := findEnv(c.Env, "HTTPS_PROXY")
	require.NotNil(t, hp)
	assert.Equal(t, testEgressProxyURL, hp.Value)
}

func TestBuildDeploymentImageFromBOM(t *testing.T) {
	px := newProxy()
	c := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0]

	b, ok := bom.BundleFor(bom.DefaultVersion)
	require.True(t, ok)
	img, ok := b.Lookup(bom.ComponentProxy)
	require.True(t, ok)
	assert.Contains(t, c.Image, "proxy:"+img.Tag)
	assert.Contains(t, c.Image, bom.DefaultImageRegistry)
}

func TestBuildDeploymentImageOverride(t *testing.T) {
	px := newProxy()
	px.Spec.Image = &otilmv1alpha1.ImageSpec{Registry: "example.com", Repository: "custom", Name: "proxy", Tag: "9.9.9"}
	c := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0]
	assert.Equal(t, "example.com/custom/proxy:9.9.9", c.Image)
}

func TestResolvedVersion(t *testing.T) {
	px := newProxy()
	b, _ := bom.BundleFor(bom.DefaultVersion)
	img, _ := b.Lookup(bom.ComponentProxy)
	assert.Equal(t, img.Tag, ResolvedVersion(px))

	px.Spec.Image = &otilmv1alpha1.ImageSpec{Tag: "9.9.9"}
	assert.Equal(t, "9.9.9", ResolvedVersion(px))
}

func TestBuildDeploymentProbeDefaults(t *testing.T) {
	c := BuildDeployment(newProxy(), "x").Spec.Template.Spec.Containers[0]
	require.NotNil(t, c.LivenessProbe)
	assert.Equal(t, "/health", c.LivenessProbe.HTTPGet.Path)
	require.NotNil(t, c.ReadinessProbe)
	assert.Equal(t, "/ready", c.ReadinessProbe.HTTPGet.Path)
	require.NotNil(t, c.StartupProbe)
	assert.Equal(t, "/health", c.StartupProbe.HTTPGet.Path)
	assert.Equal(t, HTTPPort, c.ReadinessProbe.HTTPGet.Port.IntVal)
}

func TestBuildDeploymentSCCClean(t *testing.T) {
	sc := BuildDeployment(newProxy(), "x").Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	assert.True(t, *sc.RunAsNonRoot)
	assert.Nil(t, sc.RunAsUser, "no hard-coded runAsUser (SCC-clean)")
	assert.False(t, *sc.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

func TestBuildDeploymentChecksumAndPodMeta(t *testing.T) {
	px := newProxy()
	px.Spec.PodAnnotations = map[string]string{"vault.example/inject": "true"}
	px.Spec.PodLabels = map[string]string{"team": "edge", NameLabel: "spoofed"}
	d := BuildDeployment(px, "deadbeef")
	tpl := d.Spec.Template
	assert.Equal(t, "deadbeef", tpl.Annotations[ChecksumAnnotation])
	assert.Equal(t, "true", tpl.Annotations["vault.example/inject"])
	assert.Equal(t, "edge", tpl.Labels["team"])
	assert.Equal(t, "dc-east", tpl.Labels[NameLabel], "operator labels take precedence")
	assert.Equal(t, ChildResourceName(px), tpl.Spec.ServiceAccountName)
}

func TestBuildDeploymentReservedEnvCannotBeOverridden(t *testing.T) {
	px := newProxy()
	px.Spec.Env = []otilmv1alpha1.EnvVar{
		{Name: EnvConfigToken, Value: "attacker-supplied"},
		{Name: EnvTokenSigningKey, Value: "attacker-supplied"},
		{Name: "HTTPS_PROXY", Value: testEgressProxyURL},
	}
	c := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0]

	// Kubernetes env is last-duplicate-wins: reserved names must appear exactly once,
	// as secretKeyRef wiring, with no trailing inline duplicate.
	tokenCount := 0
	for _, e := range c.Env {
		if e.Name == EnvConfigToken || e.Name == EnvTokenSigningKey {
			tokenCount++
			assert.Empty(t, e.Value, "reserved env must never carry an inline value")
			assert.NotNil(t, e.ValueFrom, "reserved env must stay secretKeyRef-wired")
		}
	}
	assert.Equal(t, 2, tokenCount)
	assert.NotNil(t, findEnv(c.Env, "HTTPS_PROXY"), "non-reserved user env passes through")
}

func TestBuildDeploymentPodLevelSecurityContext(t *testing.T) {
	podSC := BuildDeployment(newProxy(), "x").Spec.Template.Spec.SecurityContext
	require.NotNil(t, podSC, "pod-level hardening must be present (mirrors common builders)")
	assert.True(t, *podSC.RunAsNonRoot)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, podSC.SeccompProfile.Type)
}

func TestBuildDeploymentExposesBothPorts(t *testing.T) {
	c := BuildDeployment(newProxy(), "x").Spec.Template.Spec.Containers[0]
	require.Len(t, c.Ports, 2)
	assert.Equal(t, HTTPPort, c.Ports[0].ContainerPort)
	assert.Equal(t, APIPort, c.Ports[1].ContainerPort)
}

func TestBuildDeploymentSecretRefEnvAndVolume(t *testing.T) {
	px := newProxy()
	mountPath := "/etc/ssl/corp"
	envName := "BROKER_CA_INFO"
	px.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
		{Name: "ca-bundle", Type: otilmv1alpha1.RefTypeVolume, MountPath: &mountPath},
		{Name: "extra", Type: otilmv1alpha1.RefTypeEnv, Keys: []otilmv1alpha1.RefKeyMapping{
			{SecretKey: "info", EnvVar: &envName},
		}},
	}
	d := BuildDeployment(px, "x")
	c := d.Spec.Template.Spec.Containers[0]

	e := findEnv(c.Env, envName)
	require.NotNil(t, e, "keyed secret ref must project an env var")
	assert.Equal(t, "extra", e.ValueFrom.SecretKeyRef.Name)

	require.Len(t, d.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "ca-bundle", d.Spec.Template.Spec.Volumes[0].Secret.SecretName)
	require.Len(t, c.VolumeMounts, 1)
	assert.Equal(t, mountPath, c.VolumeMounts[0].MountPath)
}

func TestBuildDeploymentSecretRefCannotRemapReservedEnv(t *testing.T) {
	px := newProxy()
	reserved := EnvConfigToken
	px.Spec.SecretRefs = []otilmv1alpha1.SecretRef{
		{Name: "evil", Type: otilmv1alpha1.RefTypeEnv, Keys: []otilmv1alpha1.RefKeyMapping{
			{SecretKey: "tok", EnvVar: &reserved},
		}},
	}
	c := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0]

	count := 0
	for _, e := range c.Env {
		if e.Name == EnvConfigToken {
			count++
			assert.Equal(t, "dc-east-config", e.ValueFrom.SecretKeyRef.Name,
				"the config-token wiring must keep pointing at configTokenSecretRef")
		}
	}
	assert.Equal(t, 1, count, "a keyed ref must not be able to redirect the token wiring")
}

func TestBuildDeploymentConfigMapRefAndEphemeralVolume(t *testing.T) {
	px := newProxy()
	cmMount := "/etc/proxy/extra"
	px.Spec.ConfigMapRefs = []otilmv1alpha1.ConfigMapRef{
		{Name: "extra-cm", Type: otilmv1alpha1.RefTypeVolume, MountPath: &cmMount},
	}
	px.Spec.Volumes = []otilmv1alpha1.VolumeSpec{{Name: "scratch", MountPath: "/tmp"}}
	d := BuildDeployment(px, "x")
	c := d.Spec.Template.Spec.Containers[0]

	require.Len(t, d.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, "extra-cm", d.Spec.Template.Spec.Volumes[0].ConfigMap.Name)
	assert.NotNil(t, d.Spec.Template.Spec.Volumes[1].EmptyDir)
	require.Len(t, c.VolumeMounts, 2)
	assert.Equal(t, "/tmp", c.VolumeMounts[1].MountPath)
}

func TestBuildDeploymentSecurityContextReadOnlyRootOverride(t *testing.T) {
	px := newProxy()
	readOnly := false
	px.Spec.SecurityContext = &otilmv1alpha1.SecurityContextSpec{ReadOnlyRootFilesystem: &readOnly}
	sc := BuildDeployment(px, "x").Spec.Template.Spec.Containers[0].SecurityContext
	assert.False(t, *sc.ReadOnlyRootFilesystem, "readOnlyRootFilesystem is a honored runtime decision")
	assert.True(t, *sc.RunAsNonRoot, "SCC-critical fields stay hardened")
}

func TestBuildDeploymentSchedulingAndGracePeriod(t *testing.T) {
	px := newProxy()
	grace := int64(120)
	px.Spec.TerminationGracePeriodSeconds = &grace
	px.Spec.NodeSelector = map[string]string{"network-zone": "egress"}
	px.Spec.Tolerations = []corev1.Toleration{{Key: "egress", Operator: corev1.TolerationOpExists}}
	podSpec := BuildDeployment(px, "x").Spec.Template.Spec

	require.NotNil(t, podSpec.TerminationGracePeriodSeconds)
	assert.Equal(t, grace, *podSpec.TerminationGracePeriodSeconds)
	assert.Equal(t, "egress", podSpec.NodeSelector["network-zone"])
	require.Len(t, podSpec.Tolerations, 1)
}

func TestBuildDeploymentSidecarsInitAffinityAndSA(t *testing.T) {
	px := newProxy()
	px.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}
	px.Spec.InitContainers = []corev1.Container{{Name: "wait-broker-dns", Image: "busybox"}}
	px.Spec.Sidecars = []corev1.Container{{
		Name:            "log-shipper",
		Image:           "fluent-bit:3",
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
	}}
	saName := "egress-identity"
	px.Spec.ServiceAccount = &otilmv1alpha1.ServiceAccountSpec{
		Name:        &saName,
		Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/egress"},
	}

	dep := BuildDeployment(px, "x")
	podSpec := dep.Spec.Template.Spec

	assert.NotNil(t, podSpec.Affinity.PodAntiAffinity)
	assert.Equal(t, saName, podSpec.ServiceAccountName)
	require.Len(t, podSpec.InitContainers, 1)
	assert.True(t, *podSpec.InitContainers[0].SecurityContext.RunAsNonRoot)

	require.Len(t, podSpec.Containers, 2)
	sidecar := podSpec.Containers[1]
	assert.False(t, *sidecar.SecurityContext.Privileged, "a privileged sidecar must be forced back off")

	sa := BuildServiceAccount(px)
	assert.Equal(t, saName, sa.Name)
	assert.Equal(t, "arn:aws:iam::1:role/egress", sa.Annotations["eks.amazonaws.com/role-arn"])
}
