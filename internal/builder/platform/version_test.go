/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// TestResolveBundleEmptyVersionIsDefault proves that an unset spec.version resolves the
// operator's default (DefaultVersion) bundle, so the out-of-the-box render is unchanged.
func TestResolveBundleEmptyVersionIsDefault(t *testing.T) {
	p := basePlatform() // sets no spec.version
	def, _ := bom.BundleFor("")
	assert.Equal(t, def.Wiring, resolveBundle(p).Wiring)
	assert.Equal(t, def.Components, resolveBundle(p).Components)
}

// TestResolveBundleKnownVersion proves an explicit, KNOWN spec.version resolves that
// version's bundle (selecting the current default explicitly renders identically to leaving
// the version unset).
func TestResolveBundleKnownVersion(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = testVersion218
	b := resolveBundle(p)
	img, ok := b.Lookup("core")
	assert.True(t, ok)
	assert.Equal(t, testVersion218, img.Tag)
}

// TestResolveBundleUnknownVersionFallsBackDefensively proves the builder-side resolver is
// TOTAL: an unknown spec.version (which the controller rejects BEFORE rendering) falls back
// to the default bundle rather than panicking, so the pure builders never crash if called
// directly with an out-of-range version.
func TestResolveBundleUnknownVersionFallsBackDefensively(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = "9.9.9"
	def, _ := bom.BundleFor("")
	assert.Equal(t, def.Wiring, resolveBundle(p).Wiring, "unknown version falls back to the default bundle")
}

// TestResolveCoreImageUsesBundleTag proves the bundle's core tag flows into the rendered
// Core image when no per-component override is set (the default-version render).
func TestResolveCoreImageUsesBundleTag(t *testing.T) {
	c := ResolveCore(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.19.0", c.Image)
}

// TestResolveCoreComponentTagOverridesBundleTag proves a per-component image.tag override
// WINS over the bundle's coordinates: the tag becomes the override, while the name still
// falls back to the bundle (the override layers ON TOP of the bundle, it does not replace
// the whole image).
func TestResolveCoreComponentTagOverridesBundleTag(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.Image = otilmv1alpha1.ImageSpec{Tag: "9.9.9-canary"}
	c := ResolveCore(p)
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:9.9.9-canary", c.Image,
		"per-component image.tag overrides the bundle tag; name still from the bundle")
}

// TestRender2170UsesPreRebrandContract proves the 2.17.0 bundle renders the CZERTAINLY-era
// contract: the core:2.17.0 image under the SAME registry as 2.18.0, RABBITMQ_* broker env
// (not BROKER_*), and NO provisioning/proxy/broker-vhost env (the empty-name skip drops the
// env vars 2.17.0's wiring leaves unnamed).
func TestRender2170UsesPreRebrandContract(t *testing.T) {
	p := basePlatform()
	p.Spec.Version = "2.17.0"

	c := ResolveCore(p)
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.17.0", c.Image,
		"2.17.0 images are republished under the same registry/repository, only the tag differs")

	// Render to the actual container env: the empty-name skip applies at build time, so this
	// is where 2.17.0's unnamed (provisioning/proxy/vhost) env vars are dropped.
	d := common.BuildDeployment(c)
	var coreEnv []corev1.EnvVar
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.Name == "core" {
			coreEnv = ctr.Env
		}
	}
	require.NotNil(t, coreEnv, "core container must be present")
	names := map[string]bool{}
	for _, e := range coreEnv {
		assert.NotEmpty(t, e.Name, "no container env var may have an empty name")
		names[e.Name] = true
	}
	assert.True(t, names["RABBITMQ_HOST"], "2.17.0 Core uses RABBITMQ_HOST")
	assert.True(t, names["RABBITMQ_USERNAME"], "2.17.0 Core uses RABBITMQ_USERNAME")
	assert.False(t, names["BROKER_HOST"], "2.17.0 Core does NOT use the 2.18.0 BROKER_HOST")
	assert.False(t, names["BROKER_VIRTUAL_HOST"], "2.17.0 Core has no broker-vhost env")
	assert.False(t, names["PROVISIONING_API_URL"], "provisioning is a 2.18.0 feature")
	assert.False(t, names["PROXY_ENABLED"], "native proxy wiring is a 2.18.0 feature")
}

// TestProvisioningDeployGatedByVersion proves provisioning.mode=deploy renders the
// provisioning workload on 2.18.0 but is a NO-OP on 2.17.0 (that release has no provisioning
// component), so the same CR is portable across versions.
func TestProvisioningDeployGatedByVersion(t *testing.T) {
	mk := func(version string) *otilmv1alpha1.Platform {
		p := basePlatform()
		p.Spec.Version = version
		p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
			Mode:   "deploy",
			Deploy: &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: "prov-bootstrap"},
		}
		return p
	}
	assert.False(t, ProvisioningDeploy(mk("2.17.0")), "2.17.0 has no provisioning component")
	assert.True(t, ProvisioningDeploy(mk(testVersion218)), "2.18.0 ships the provisioning component")
}

// TestResolveAuthComponentTagOverridesBundleTag proves the override-wins rule holds
// for a component whose PUBLISHED image name differs from its operator identity (auth
// → "auth"): the bundle still supplies the published name, the override supplies the tag.
func TestResolveAuthComponentTagOverridesBundleTag(t *testing.T) {
	p := basePlatform()
	p.Spec.Auth.Image = otilmv1alpha1.ImageSpec{Tag: "7.7.7"}
	c := ResolveAuth(p)
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/auth:7.7.7", c.Image,
		"override tag wins; the published name 'auth' still comes from the bundle")
}
