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

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestTimeQualityMonitorIsBundleGated proves the sidecar renders only when the CR asks for it
// AND the selected bundle carries the monitor image — the same version-gating by DATA the
// bundled provisioning service uses, so the same CR stays portable across platform versions.
func TestTimeQualityMonitorIsBundleGated(t *testing.T) {
	cases := []struct {
		name    string
		version string
		enabled bool
		want    bool
	}{
		{"2.19.0 enabled", testVersion219, true, true},
		{"2.19.0 disabled", testVersion219, false, false},
		{"2.18.0 has no monitor image", testVersion218, true, false},
		{"2.17.0 has no monitor image", testVersion217, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := basePlatform()
			p.Spec.Version = c.version
			p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
				Enabled:     c.enabled,
				Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
			}
			assert.Equal(t, c.want, TimeQualityMonitorEnabled(p))
			found := false
			for _, s := range ResolveCore(p).Sidecars {
				if s.Name == "time-quality-monitor" {
					found = true
				}
			}
			assert.Equal(t, c.want, found)
		})
	}
}

// TestTimeQualityMonitorSidecarShape pins the container contract against the chart's:
// the private image, port 8082, /health liveness+readiness, and the broker wiring.
func TestTimeQualityMonitorSidecarShape(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = testVersion219
	p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled:     true,
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
	}
	c := timeQualityMonitorSidecar(p)

	assert.Equal(t, "time-quality-monitor", c.Name)
	assert.Equal(t, "hub.omnitrustregistry.com/ilm-private/time-quality-monitor:1.0.0", c.Image)
	require.Len(t, c.Ports, 1)
	assert.Equal(t, int32(8082), c.Ports[0].ContainerPort)
	require.NotNil(t, c.LivenessProbe)
	require.NotNil(t, c.ReadinessProbe)
	assert.Equal(t, "/health", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, "/health", c.ReadinessProbe.HTTPGet.Path)

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	assert.Equal(t, "8082", env["LISTEN_PORT"].Value)
	assert.Equal(t, "INFO", env["LOG_LEVEL"].Value)
	assert.Equal(t, "messaging-configmap", env["BROKER_HOST"].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "messaging.host", env["BROKER_HOST"].ValueFrom.ConfigMapKeyRef.Key)
	assert.Equal(t, "messaging.amqp.port", env["BROKER_PORT"].ValueFrom.ConfigMapKeyRef.Key)
	assert.Equal(t, testTQMonitorSecretRef, env["BROKER_USERNAME"].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "username", env["BROKER_USERNAME"].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, testTQMonitorSecretRef, env["BROKER_PASSWORD"].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", env["BROKER_PASSWORD"].ValueFrom.SecretKeyRef.Key)
	assert.NotEmpty(t, env["BROKER_VIRTUAL_HOST"].Value)
}

// TestTimeQualityMonitorManagedCredentials proves managed messaging wires the
// TOPOLOGY-GENERATED monitor-user Secret, with no CR-supplied reference at all.
func TestTimeQualityMonitorManagedCredentials(t *testing.T) {
	p := managedMQPlatform(nil)
	p.Spec.Version = testVersion219
	p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{Enabled: true}
	secret, userKey, passKey := timeQualityMonitorCredentials(p)
	assert.Equal(t, p.Name+"-messaging-monitor-user-credentials", secret)
	assert.Equal(t, "username", userKey)
	assert.Equal(t, "password", passKey)
}

// TestTimeQualityMonitorExternalCredentialKeyOverrides proves EXTERNAL messaging honors the
// CR's own usernameKey/passwordKey overrides rather than silently falling back to the bom
// default keys. Every other external-mode fixture in this file omits usernameKey/passwordKey,
// so a regression that read the bom default directly instead of the CR override would still
// pass without this case.
func TestTimeQualityMonitorExternalCredentialKeyOverrides(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = testVersion219
	p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled: true,
		Credentials: &otilmv1alpha1.CredentialsRef{
			SecretRef:   testTQMonitorSecretRef,
			UsernameKey: testTQMonitorUsernameKey,
			PasswordKey: testTQMonitorPasswordKey,
		},
	}
	secret, userKey, passKey := timeQualityMonitorCredentials(p)
	assert.Equal(t, testTQMonitorSecretRef, secret)
	assert.Equal(t, testTQMonitorUsernameKey, userKey)
	assert.Equal(t, testTQMonitorPasswordKey, passKey)

	// The override reaches the rendered container's secretKeyRef, not just the resolver.
	c := timeQualityMonitorSidecar(p)
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	assert.Equal(t, testTQMonitorUsernameKey, env["BROKER_USERNAME"].ValueFrom.SecretKeyRef.Key)
	assert.Equal(t, testTQMonitorPasswordKey, env["BROKER_PASSWORD"].ValueFrom.SecretKeyRef.Key)
}

// TestTimeQualityMonitorPullSecretsReachThePod proves the private image's pull secret lands on
// the POD (a sidecar's pull secret is not a container field), unioned with the shared and
// per-component lists and de-duplicated.
func TestTimeQualityMonitorPullSecretsReachThePod(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = testVersion219
	p.Spec.Common.Image.PullSecrets = []string{testSharedPullSecret}
	p.Spec.Core.Image.PullSecrets = []string{testSharedPullSecret, "core-pull"}
	p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled:     true,
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
		Image:       otilmv1alpha1.ImageSpec{PullSecrets: []string{testPrivatePullSecret}},
	}
	assert.Equal(t, []string{testSharedPullSecret, "core-pull", testPrivatePullSecret}, ResolveCore(p).PullSecrets)
}

// TestTimeQualityMonitorResourcesRender proves the CR's resources reach the container — the
// one field of the sidecar spec that is neither image nor credentials, and the one a
// silent-drop bug would be invisible in.
func TestTimeQualityMonitorResourcesRender(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = testVersion219
	p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
		Enabled:     true,
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("150M"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("300M")},
		},
	}
	c := timeQualityMonitorSidecar(p)
	assert.Equal(t, "50m", c.Resources.Requests.Cpu().String())
	assert.Equal(t, "150M", c.Resources.Requests.Memory().String())
	assert.Equal(t, "300M", c.Resources.Limits.Memory().String())
}

// TestTimeQualityMonitorCommandAndArgsRender proves the sidecar honours image.command /
// image.args like every other component with a user-facing ImageSpec (and like the chart's own
// monitor template), rather than exposing the fields through the CRD and dropping them.
func TestTimeQualityMonitorCommandAndArgsRender(t *testing.T) {
	base := func() *otilmv1alpha1.Platform {
		p := basePlatform()
		p.Spec.Version = testVersion219
		p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
			Enabled:     true,
			Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
		}
		return p
	}

	t.Run("unset leaves the image's own entrypoint", func(t *testing.T) {
		c := timeQualityMonitorSidecar(base())
		assert.Empty(t, c.Command)
		assert.Empty(t, c.Args)
	})

	t.Run("the per-component override wins over the shared one", func(t *testing.T) {
		p := base()
		p.Spec.Common.Image.Command = []string{testSharedCommand}
		p.Spec.Common.Image.Args = []string{testSharedArg}
		p.Spec.Core.TimeQualityMonitor.Image.Command = []string{testCustomEntry}
		p.Spec.Core.TimeQualityMonitor.Image.Args = []string{testFlagArg}
		c := timeQualityMonitorSidecar(p)
		assert.Equal(t, []string{testCustomEntry}, c.Command)
		assert.Equal(t, []string{testFlagArg}, c.Args)
	})

	t.Run("the shared override applies when the component sets none", func(t *testing.T) {
		p := base()
		p.Spec.Common.Image.Command = []string{testSharedCommand}
		p.Spec.Common.Image.Args = []string{testSharedArg}
		c := timeQualityMonitorSidecar(p)
		assert.Equal(t, []string{testSharedCommand}, c.Command)
		assert.Equal(t, []string{testSharedArg}, c.Args)
	})
}

// TestTimeQualityMonitorPullSecretsFollowTheGate proves a monitor that does NOT render never
// touches Core's pod. imagePullSecrets is a POD field, so an ungated helper would mutate Core's
// pod spec — a pod-template change, and therefore a Core ROLL — for a sidecar that is disabled,
// or for one whose bundle carries no monitor image at all.
func TestTimeQualityMonitorPullSecretsFollowTheGate(t *testing.T) {
	withMonitor := func(version string, enabled bool) []string {
		p := basePlatform()
		p.Spec.Version = version
		p.Spec.Common.Image.PullSecrets = []string{testSharedPullSecret}
		p.Spec.Core.TimeQualityMonitor = &otilmv1alpha1.TimeQualityMonitorSpec{
			Enabled:     enabled,
			Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testTQMonitorSecretRef},
			Image:       otilmv1alpha1.ImageSpec{PullSecrets: []string{testPrivatePullSecret}},
		}
		return ResolveCore(p).PullSecrets
	}
	assert.Equal(t, []string{testSharedPullSecret, testPrivatePullSecret}, withMonitor(testVersion219, true))
	assert.Equal(t, []string{testSharedPullSecret}, withMonitor(testVersion219, false),
		"a DISABLED monitor must not mutate Core's pod")
	assert.Equal(t, []string{testSharedPullSecret}, withMonitor(testVersion218, true),
		"a bundle without the monitor image renders no sidecar, so it contributes no pull secret")
}
