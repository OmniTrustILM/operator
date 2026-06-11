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
	"encoding/json"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// adminPlatform returns the base platform with registerAdmin set as given.
func adminPlatform(ra *otilmv1alpha1.RegisterAdminSpec) *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.RegisterAdmin = ra
	return p
}

// certManagerObjs returns the cert-manager.io unstructured objects from a rendered set.
func certManagerObjs(objs []client.Object) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GroupVersionKind().Group == certManagerDefaultGroup {
			out = append(out, u)
		}
	}
	return out
}

func TestResolveAdminCertObjectsDisabled(t *testing.T) {
	t.Run("nil registerAdmin → no objects", func(t *testing.T) {
		assert.Nil(t, ResolveAdminCertObjects(basePlatform()))
	})
	t.Run("enabled=false → no objects", func(t *testing.T) {
		assert.Nil(t, ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: false})))
	})
	t.Run("provided source → no cert-manager objects (caller supplies the Secret)", func(t *testing.T) {
		objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("my-admin-cert")},
		}))
		assert.Nil(t, objs, "provided source renders no cert-manager objects")
	})
}

// adminCertificate returns the single admin Certificate from a rendered set.
func adminCertificate(t *testing.T, objs []client.Object) *unstructured.Unstructured {
	t.Helper()
	return findUnstructured(t, objs, kindCertificate, adminCertName)
}

func TestResolveAdminCertGeneratedDefaultStandaloneCAChain(t *testing.T) {
	// generated, no issuerRef, no edge → the operator provisions a dedicated admin CA
	// chain so it works standalone.
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled: true, Username: "admin@ilm",
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
	}))
	require.NotEmpty(t, objs)

	// The full chain: admin self-signed issuer -> admin CA cert -> admin CA issuer -> leaf.
	selfSigned := findUnstructured(t, objs, kindIssuer, adminSelfSignedIssuerName)
	assert.Contains(t, selfSigned.Object["spec"].(map[string]interface{}), "selfSigned")

	caCert := findUnstructured(t, objs, kindCertificate, adminCACertificateName)
	caSpec := caCert.Object["spec"].(map[string]interface{})
	assert.Equal(t, true, caSpec["isCA"])
	assert.Equal(t, adminCAKeypairSecret, caSpec["secretName"])

	caIssuer := findUnstructured(t, objs, kindIssuer, adminCAIssuerName)
	assert.Equal(t, adminCAKeypairSecret,
		caIssuer.Object["spec"].(map[string]interface{})["ca"].(map[string]interface{})["secretName"])

	// The leaf admin Certificate.
	cert := adminCertificate(t, objs)
	spec := cert.Object["spec"].(map[string]interface{})
	assert.Equal(t, adminCertSecretName, spec["secretName"], "secretName must match the name Core's ADMIN_CERT reads")
	assert.Equal(t, "admin@ilm", spec["commonName"], "commonName comes from registerAdmin.username")
	assert.ElementsMatch(t, []interface{}{"client auth", "digital signature", "key encipherment"}, spec["usages"])
	assert.NotEmpty(t, spec["duration"])
	assert.NotEmpty(t, spec["renewBefore"])

	// The leaf points at the operator's admin CA issuer.
	issuerRef := spec["issuerRef"].(map[string]interface{})
	assert.Equal(t, adminCAIssuerName, issuerRef["name"])
	assert.Equal(t, kindIssuer, issuerRef["kind"])
}

func TestResolveAdminCertGeneratedDefaultCommonName(t *testing.T) {
	// generated, no username → default Subject CN.
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
	}))
	cert := adminCertificate(t, objs)
	assert.Equal(t, adminCertCommonName, cert.Object["spec"].(map[string]interface{})["commonName"])
}

func TestResolveAdminCertGeneratedReusesEdgeInternalCA(t *testing.T) {
	// generated, no issuerRef, BUT the edge already provisions the internal CA
	// (edge.tls.source=internal) → REUSE ca-issuer; emit NO duplicate issuer chain.
	p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}

	// Render only the admin objects (not the edge's) so we can assert the admin set is
	// the single leaf Certificate referencing the edge's ca-issuer.
	objs := ResolveAdminCertObjects(p)
	require.Len(t, objs, 1, "reusing the edge CA must render only the leaf Certificate, no duplicate issuers")

	cert := adminCertificate(t, objs)
	issuerRef := cert.Object["spec"].(map[string]interface{})["issuerRef"].(map[string]interface{})
	assert.Equal(t, caIssuerName, issuerRef["name"], "must reuse the edge's ca-issuer")
	assert.Equal(t, kindIssuer, issuerRef["kind"])

	// And the admin set must NOT contain a second ca-issuer / selfsigned-issuer.
	for _, o := range objs {
		u := o.(*unstructured.Unstructured)
		assert.NotEqual(t, caIssuerName, u.GetName(), "no duplicate ca-issuer in the admin set")
		assert.NotEqual(t, selfSignedIssuerName, u.GetName(), "no duplicate selfsigned-issuer in the admin set")
	}
}

func TestResolveAdminCertGeneratedExplicitIssuerRef(t *testing.T) {
	// generated with an explicit issuerRef → use it verbatim; render NO CA chain.
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer, Kind: "ClusterIssuer", Group: certManagerDefaultGroup}},
	}))
	require.Len(t, objs, 1, "an explicit issuerRef renders only the leaf Certificate")

	cert := adminCertificate(t, objs)
	issuerRef := cert.Object["spec"].(map[string]interface{})["issuerRef"].(map[string]interface{})
	assert.Equal(t, testVaultIssuer, issuerRef["name"])
	assert.Equal(t, "ClusterIssuer", issuerRef["kind"])
	_, hasGroup := issuerRef["group"]
	assert.False(t, hasGroup, "default group cert-manager.io is omitted")
}

func TestResolveAdminCertGeneratedExternalIssuerGroup(t *testing.T) {
	// A non-default issuer group (external issuer) must be emitted on the issuerRef.
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "aws-pca", Kind: "ClusterIssuer", Group: "awspca.cert-manager.io"}},
	}))
	cert := adminCertificate(t, objs)
	issuerRef := cert.Object["spec"].(map[string]interface{})["issuerRef"].(map[string]interface{})
	assert.Equal(t, "awspca.cert-manager.io", issuerRef["group"])
}

func TestResolveAdminCertGeneratedIssuerRefDefaultsKind(t *testing.T) {
	// issuerRef with no kind defaults to Issuer.
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "ns-issuer"}},
	}))
	cert := adminCertificate(t, objs)
	issuerRef := cert.Object["spec"].(map[string]interface{})["issuerRef"].(map[string]interface{})
	assert.Equal(t, kindIssuer, issuerRef["kind"])
}

// TestAdminCABundle verifies the resolution of the CA Secret whose cert must be
// folded into Core's trust bundle so Core trusts the admin client cert's issuer.
func TestAdminCABundle(t *testing.T) {
	t.Run("disabled → not known", func(t *testing.T) {
		assert.Equal(t, AdminCABundleSource{}, AdminCABundle(basePlatform()))
	})
	t.Run("provided source → not known (caller supplies the cert Secret)", func(t *testing.T) {
		p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("c")}})
		assert.False(t, AdminCABundle(p).Known)
	})
	t.Run("generated standalone → dedicated admin-ca-keypair", func(t *testing.T) {
		p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}})
		got := AdminCABundle(p)
		assert.True(t, got.Known)
		assert.Equal(t, adminCAKeypairSecret, got.SecretName)
	})
	t.Run("generated reusing edge internal CA → ca-keypair", func(t *testing.T) {
		p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
		p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}
		got := AdminCABundle(p)
		assert.True(t, got.Known)
		assert.Equal(t, caKeypairSecret, got.SecretName)
	})
	t.Run("generated with caller issuerRef → not known", func(t *testing.T) {
		p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: testVaultIssuer}},
		})
		assert.False(t, AdminCABundle(p).Known, "operator does not own a caller-supplied issuer's CA Secret")
	})
}

func TestResolveAdminCertObjectsMetadata(t *testing.T) {
	objs := ResolveAdminCertObjects(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
	}))
	for _, o := range certManagerObjs(objs) {
		assert.Equal(t, "cert-manager.io/v1", o.GetAPIVersion())
		assert.Equal(t, "ilm-system", o.GetNamespace(), "namespace propagates from the CR")
		assert.Equal(t, adminCertRole, o.GetLabels()["app.kubernetes.io/component"])
	}
}

// TestResolveAdminCertNoLeak asserts no certificate or key material appears anywhere
// in the rendered admin objects — only Secret/issuer names are referenced.
func TestResolveAdminCertNoLeak(t *testing.T) {
	cases := map[string]*otilmv1alpha1.Platform{
		"standalone": adminPlatform(&otilmv1alpha1.RegisterAdminSpec{Enabled: true, Username: "admin", Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}),
		"reuse-edge-ca": func() *otilmv1alpha1.Platform {
			p := edgePlatform(&otilmv1alpha1.EdgeTLSSpec{Source: edgeSourceInternal})
			p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}
			return p
		}(),
		"explicit-issuer": adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "issuer"}},
		}),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			objs := ResolveAdminCertObjects(p)
			b, err := json.Marshal(toRaw(objs))
			require.NoError(t, err)
			s := string(b)
			assert.NotContains(t, s, "BEGIN CERTIFICATE", "no PEM cert material in rendered admin objects")
			assert.NotContains(t, s, "PRIVATE KEY", "no private-key material in rendered admin objects")
			// No "data"/"stringData" blocks (Secrets are never rendered by the operator here).
			assert.NotContains(t, s, "\"data\"")
			assert.NotContains(t, s, "stringData")
		})
	}
}

// toRaw extracts the Object maps from unstructured objects for marshaling.
func toRaw(objs []client.Object) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u.Object)
		}
	}
	return out
}

func TestAdminCertDependencies(t *testing.T) {
	gkCert := schema.GroupKind{Group: certManagerDefaultGroup, Kind: kindCertificate}
	gkIss := schema.GroupKind{Group: certManagerDefaultGroup, Kind: kindIssuer}

	t.Run("disabled → no deps", func(t *testing.T) {
		assert.Nil(t, AdminCertDependencies(basePlatform()))
	})
	t.Run("provided → no deps (no cert-manager needed)", func(t *testing.T) {
		deps := AdminCertDependencies(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin-cert")},
		}))
		assert.Empty(t, deps)
	})
	t.Run("generated → cert-manager Certificate + Issuer (v1)", func(t *testing.T) {
		deps := AdminCertDependencies(adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
			Enabled:     true,
			Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"},
		}))
		gks := depGroupKinds(deps)
		assert.ElementsMatch(t, []schema.GroupKind{gkCert, gkIss}, gks)
		for _, d := range deps {
			assert.Equal(t, ReasonCertManagerNotInstalled, d.Reason)
			assert.Equal(t, []string{"v1"}, d.Versions)
			assert.Contains(t, d.Message, "spec.registerAdmin.source=provided", "message must be actionable")
			assert.NotContains(t, d.Message, "db.example.com", "no coordinate leakage")
		}
	})
}

// TestAdminCertDependenciesMatchRender guards that the cert-manager dependency is
// asserted iff the admin objects actually render a cert-manager object — the same
// soundness invariant the edge keeps.
func TestAdminCertDependenciesMatchRender(t *testing.T) {
	cases := map[string]*otilmv1alpha1.RegisterAdminSpec{
		"disabled":          nil,
		"provided":          {Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin")}},
		"generated/default": {Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}},
		"generated/issuer":  {Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", IssuerRef: &otilmv1alpha1.CertManagerIssuerRef{Name: "i"}}},
	}
	for name, ra := range cases {
		t.Run(name, func(t *testing.T) {
			p := adminPlatform(ra)
			rendersCertManager := len(certManagerObjs(ResolveAdminCertObjects(p))) > 0
			requiresCertManager := len(AdminCertDependencies(p)) > 0
			assert.Equal(t, rendersCertManager, requiresCertManager,
				"a cert-manager dependency must be present iff a cert-manager object is rendered")
		})
	}
}

// --- Admin cert/key in-Secret key resolvers (source=provided mapping) ------------

// TestAdminCertKeyProvidedDefaultAndOverride: source=provided defaults to tls.crt and
// honours a certKey override.
func TestAdminCertKeyProvidedDefaultAndOverride(t *testing.T) {
	// Default (no certKey set).
	pDef := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin")},
	})
	assert.Equal(t, "tls.crt", AdminCertKey(pDef), "provided defaults to the BOM tls.crt key")

	// Override.
	pOv := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin"), CertKey: testClientCrt},
	})
	assert.Equal(t, testClientCrt, AdminCertKey(pOv), "provided honours the certKey override")
}

// TestAdminPrivateKeyKeyProvidedDefaultAndOverride: source=provided defaults to tls.key and
// honours a privateKeyKey override.
func TestAdminPrivateKeyKeyProvidedDefaultAndOverride(t *testing.T) {
	pDef := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin")},
	})
	assert.Equal(t, "tls.key", AdminPrivateKeyKey(pDef), "provided defaults to the BOM tls.key key")

	pOv := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr("admin"), PrivateKeyKey: testClientKey},
	})
	assert.Equal(t, testClientKey, AdminPrivateKeyKey(pOv), "provided honours the privateKeyKey override")
}

// TestAdminCertKeysGeneratedIgnoreOverrides: source=generated always uses the cert-manager
// kubernetes.io/tls keys (tls.crt/tls.key) — a stray user override must NOT apply.
func TestAdminCertKeysGeneratedIgnoreOverrides(t *testing.T) {
	p := adminPlatform(&otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", CertKey: testClientCrt, PrivateKeyKey: testClientKey},
	})
	assert.Equal(t, "tls.crt", AdminCertKey(p), "generated keeps the cert-manager tls.crt key")
	assert.Equal(t, "tls.key", AdminPrivateKeyKey(p), "generated keeps the cert-manager tls.key key")
}

// TestProvisioningAPIKeyKeyDefaultAndOverride: defaults to provisioningApiKey and honours
// an apiKey override.
func TestProvisioningAPIKeyKeyDefaultAndOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{APIURL: "https://p", APIKeySecretRef: "prov"}
	assert.Equal(t, "provisioningApiKey", ProvisioningAPIKeyKey(p), "defaults to the BOM provisioningApiKey key")

	p.Spec.Provisioning.APIKey = "x-api-key"
	assert.Equal(t, "x-api-key", ProvisioningAPIKeyKey(p), "honours the apiKey override")
}

// TestTrustedCertsInputKeyDefaultAndOverride: defaults to ca.crt and honours a caKey override.
func TestTrustedCertsInputKeyDefaultAndOverride(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: "ca"}
	assert.Equal(t, "ca.crt", TrustedCertsInputKey(p), "defaults to the BOM ca.crt key")

	p.Spec.Common.TrustedCertificates.CAKey = "tls-ca"
	assert.Equal(t, "tls-ca", TrustedCertsInputKey(p), "honours the caKey override")
}
