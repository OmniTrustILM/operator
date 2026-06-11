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
	"github.com/OmniTrustILM/operator/internal/bom"
	"github.com/OmniTrustILM/operator/internal/checksum"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestComposesTrustedCertsAndSecretName covers when the operator composes its own
// trusted-certificates Secret and which name Core/auth reference.
func TestComposesTrustedCertsAndSecretName(t *testing.T) {
	t.Run("no admin bootstrap → no composition, reference the user's SecretRef", func(t *testing.T) {
		p := basePlatform()
		p.Spec.Common.TrustedCertificates.SecretRef = testMyTrust
		assert.False(t, ComposesTrustedCerts(p))
		assert.Equal(t, testMyTrust, TrustedCertsSecretName(p))
	})
	t.Run("unset trustedCertificates + no bootstrap → empty name (render nothing)", func(t *testing.T) {
		assert.Equal(t, "", TrustedCertsSecretName(basePlatform()))
	})
	t.Run("source=provided → no composition (caller supplies the cert Secret)", func(t *testing.T) {
		p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("c")}})
		p.Spec.Common.TrustedCertificates.SecretRef = testMyTrust
		assert.False(t, ComposesTrustedCerts(p))
		assert.Equal(t, testMyTrust, TrustedCertsSecretName(p))
	})
	t.Run("source=generated standalone → compose into trusted-certificates", func(t *testing.T) {
		p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}})
		assert.True(t, ComposesTrustedCerts(p))
		assert.Equal(t, composedTrustedCertsSecretName, TrustedCertsSecretName(p))
	})
}

// TestBuildTrustedCertificatesSecret verifies the composed Secret's shape: the
// well-known name, the ca.crt key holding the bundle, the Opaque type, and labels.
func TestBuildTrustedCertificatesSecret(t *testing.T) {
	p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}})
	bundle := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	s := BuildTrustedCertificatesSecret(p, bundle)

	assert.Equal(t, composedTrustedCertsSecretName, s.Name)
	assert.Equal(t, p.Namespace, s.Namespace)
	assert.Equal(t, corev1.SecretTypeOpaque, s.Type)
	assert.Equal(t, bundle, s.Data[TrustedCertsBundleKey()])
	assert.Equal(t, "trusted-certificates", s.Labels["app.kubernetes.io/component"])
}

// TestTrustedCertsBundleKeyMatchesWiring guards that the composed-bundle key equals
// the key Core's TRUSTED_CERTIFICATES env reads, so the env resolves against the
// composed Secret.
func TestTrustedCertsBundleKeyMatchesWiring(t *testing.T) {
	assert.Equal(t, bom.Wiring().TrustedCertificates.Key, TrustedCertsBundleKey())
}

// coreDeployment renders the Core Deployment for a platform.
func coreDeployment(p *otilmv1alpha1.Platform) *appsv1.Deployment {
	objs := RenderPlatformBase(p)
	for _, o := range objs {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == coreComponentName {
			return dep
		}
	}
	return nil
}

// gatewayDeployment renders the api-gateway (Kong) Deployment for a platform.
func gatewayDeployment(p *otilmv1alpha1.Platform) *appsv1.Deployment {
	objs := RenderPlatformBase(p)
	for _, o := range objs {
		if dep, ok := o.(*appsv1.Deployment); ok && dep.Name == gatewayName {
			return dep
		}
	}
	return nil
}

// TestStampConfigChecksumOnlyCore verifies the checksum annotation is stamped on the
// Core Deployment's pod template and nowhere else, and is a no-op for an empty sum.
func TestStampConfigChecksumOnlyCore(t *testing.T) {
	p := basePlatform()

	t.Run("stamps Core's pod template", func(t *testing.T) {
		dep := coreDeployment(p)
		require.NotNil(t, dep)
		ok := StampConfigChecksum(dep, "abc123")
		assert.True(t, ok)
		assert.Equal(t, "abc123", dep.Spec.Template.Annotations[ConfigChecksumAnnotation])
	})

	t.Run("empty checksum is a no-op", func(t *testing.T) {
		dep := coreDeployment(p)
		require.NotNil(t, dep)
		assert.False(t, StampConfigChecksum(dep, ""))
		_, present := dep.Spec.Template.Annotations[ConfigChecksumAnnotation]
		assert.False(t, present)
	})

	t.Run("no-op on a non-Core Deployment", func(t *testing.T) {
		other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "auth"}}
		assert.False(t, StampConfigChecksum(other, "abc123"))
		assert.Nil(t, other.Spec.Template.Annotations)
	})

	t.Run("no-op on a non-Deployment object", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: coreComponentName}}
		assert.False(t, StampConfigChecksum(svc, "abc123"))
	})
}

// TestConfigChecksumChangesCorePodHash is the load-bearing proof: a change to the
// composed trusted-certs bundle changes the checksum, and stamping that checksum
// changes Core's pod-template annotation — so the apiserver rolls Core. It exercises
// the exact path the reconciler uses (ComputeSecretChecksum -> StampConfigChecksum).
func TestConfigChecksumChangesCorePodHash(t *testing.T) {
	p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}})

	bundleV1 := []byte("-----BEGIN CERTIFICATE-----\nVERSION-ONE\n-----END CERTIFICATE-----\n")
	bundleV2 := []byte("-----BEGIN CERTIFICATE-----\nVERSION-ONE\n-----END CERTIFICATE-----\n" +
		"-----BEGIN CERTIFICATE-----\nADMIN-CA-ADDED\n-----END CERTIFICATE-----\n")

	sumV1 := checksum.ComputeSecretChecksum(BuildTrustedCertificatesSecret(p, bundleV1))
	sumV2 := checksum.ComputeSecretChecksum(BuildTrustedCertificatesSecret(p, bundleV2))
	require.NotEqual(t, sumV1, sumV2, "a bundle change must change the checksum")

	depV1 := coreDeployment(p)
	require.True(t, StampConfigChecksum(depV1, sumV1))
	depV2 := coreDeployment(p)
	require.True(t, StampConfigChecksum(depV2, sumV2))

	assert.NotEqual(t,
		depV1.Spec.Template.Annotations[ConfigChecksumAnnotation],
		depV2.Spec.Template.Annotations[ConfigChecksumAnnotation],
		"a bundle change must change Core's checksum/config annotation (rolling Core)")

	// Same bundle → same checksum → stable annotation (no churn across reconciles).
	sumV1Again := checksum.ComputeSecretChecksum(BuildTrustedCertificatesSecret(p, bundleV1))
	assert.Equal(t, sumV1, sumV1Again, "an unchanged bundle must yield a stable checksum")
}

// TestStampGatewayConfigChecksum verifies the checksum annotation is stamped on the api-gateway
// Deployment's pod template and nowhere else, is a no-op for an empty sum, and does NOT collide
// with Core's stamper (the two share an inner helper but must stay name-scoped).
func TestStampGatewayConfigChecksum(t *testing.T) {
	p := basePlatform()

	t.Run("stamps the gateway's pod template", func(t *testing.T) {
		dep := gatewayDeployment(p)
		require.NotNil(t, dep)
		assert.True(t, StampGatewayConfigChecksum(dep, testKongABC))
		assert.Equal(t, testKongABC, dep.Spec.Template.Annotations[ConfigChecksumAnnotation])
	})

	t.Run("empty checksum is a no-op", func(t *testing.T) {
		dep := gatewayDeployment(p)
		require.NotNil(t, dep)
		assert.False(t, StampGatewayConfigChecksum(dep, ""))
		_, present := dep.Spec.Template.Annotations[ConfigChecksumAnnotation]
		assert.False(t, present)
	})

	t.Run("no-op on a non-gateway Deployment", func(t *testing.T) {
		other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "auth"}}
		assert.False(t, StampGatewayConfigChecksum(other, testKongABC))
		assert.Nil(t, other.Spec.Template.Annotations)
	})

	t.Run("Core and gateway stampers stay name-scoped (no cross-contamination)", func(t *testing.T) {
		gw := gatewayDeployment(p)
		require.NotNil(t, gw)
		// Core's stamper must NOT touch the gateway workload...
		assert.False(t, StampConfigChecksum(gw, "core-sum"))
		_, present := gw.Spec.Template.Annotations[ConfigChecksumAnnotation]
		assert.False(t, present)
		// ...and the gateway stamper does, with its own value.
		assert.True(t, StampGatewayConfigChecksum(gw, "kong-sum"))
		assert.Equal(t, "kong-sum", gw.Spec.Template.Annotations[ConfigChecksumAnnotation])
	})
}

// TestGatewayConfigChecksumRollsGateway is the load-bearing proof for the gateway auto-roll:
// enabling the managed-Keycloak /kc route changes kong.yml, so the global ConfigMap's checksum
// changes, and stamping it changes the gateway Deployment's pod-template annotation — so the
// apiserver rolls Kong to pick up the new declarative config (it reads kong.yml only at boot).
// It exercises the exact path the reconciler uses (ComputeConfigMapChecksum(BuildGlobalConfigMap)
// -> StampGatewayConfigChecksum).
func TestGatewayConfigChecksumRollsGateway(t *testing.T) {
	managed := managedKCPlatform(nil)
	external := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Mode = "external" })

	sumManaged := checksum.ComputeConfigMapChecksum(BuildGlobalConfigMap(managed))
	sumExternal := checksum.ComputeConfigMapChecksum(BuildGlobalConfigMap(external))
	require.NotEqual(t, sumManaged, sumExternal,
		"the managed-Keycloak /kc route must change kong.yml's checksum")

	depManaged := gatewayDeployment(managed)
	require.True(t, StampGatewayConfigChecksum(depManaged, sumManaged))
	depExternal := gatewayDeployment(external)
	require.True(t, StampGatewayConfigChecksum(depExternal, sumExternal))
	assert.NotEqual(t,
		depManaged.Spec.Template.Annotations[ConfigChecksumAnnotation],
		depExternal.Spec.Template.Annotations[ConfigChecksumAnnotation],
		"a kong.yml change must change the gateway's checksum/config annotation (rolling Kong)")

	// Same config → same checksum → stable annotation (no churn across reconciles).
	assert.Equal(t, sumManaged, checksum.ComputeConfigMapChecksum(BuildGlobalConfigMap(managed)),
		"an unchanged kong.yml must yield a stable checksum")
}

// TestCoreScriptsChecksumRollsCore proves Core auto-rolls when its in-pod script changes: a
// hostname change rewrites the browser-facing /kc OIDC URLs in register-internal-keycloak.sh, so
// the scripts ConfigMap's checksum changes — folded into Core's config-checksum, that rolls Core
// to re-run the postStart with the new script (a lazily-synced ConfigMap volume would not, since
// postStart runs only at container start). External Keycloak renders no scripts ConfigMap, so it
// contributes no roll-trigger.
func TestCoreScriptsChecksumRollsCore(t *testing.T) {
	p1 := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Common.HostName = "ilm-1.example.com" })
	p2 := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Common.HostName = "ilm-2.example.com" })

	cm1 := BuildCoreScriptsConfigMap(p1)
	cm2 := BuildCoreScriptsConfigMap(p2)
	require.NotNil(t, cm1)
	require.NotNil(t, cm2)
	assert.NotEqual(t,
		checksum.ComputeConfigMapChecksum(cm1), checksum.ComputeConfigMapChecksum(cm2),
		"a hostname change rewrites the script's browser /kc URLs → the scripts checksum must change")

	assert.Nil(t, BuildCoreScriptsConfigMap(basePlatform()),
		"no scripts ConfigMap is rendered when Keycloak is not managed (no scripts roll-trigger)")
}

// TestComposedTrustedCertsRewireCoreEnv verifies Core's TRUSTED_CERTIFICATES env and
// auth's trusted-cert volume reference the operator-composed Secret name when
// the admin bootstrap composes the bundle (source=generated).
func TestComposedTrustedCertsRewireCoreEnv(t *testing.T) {
	p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}})
	p.Spec.Common.TrustedCertificates.SecretRef = "user-supplied-trust"

	w := bom.Wiring()
	core := ResolveCore(p)
	var found bool
	for _, ref := range core.SecretEnv {
		if ref.EnvVar == w.TrustedCertificates.Env {
			found = true
			assert.Equal(t, composedTrustedCertsSecretName, ref.SecretName,
				"Core must read TRUSTED_CERTIFICATES from the composed Secret when composing")
			assert.Equal(t, w.TrustedCertificates.Key, ref.SecretKey)
		}
	}
	assert.True(t, found, "Core must wire TRUSTED_CERTIFICATES")

	auth := ResolveAuth(p)
	var volFound bool
	for _, v := range auth.Volumes {
		if v.Secret != nil && v.Secret.SecretName == composedTrustedCertsSecretName {
			volFound = true
		}
	}
	assert.True(t, volFound, "auth must mount the composed trusted-certs Secret")
}
