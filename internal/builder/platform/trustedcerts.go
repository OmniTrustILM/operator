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
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// coreComponentName is the Core Deployment's name, used to identify Core in the
// reconciler's generic render-apply loop when stamping the config checksum.
const coreComponentName = "core"

const (
	// composedTrustedCertsSecretName is the well-known name of the operator-managed
	// trusted-certificates Secret the operator COMPOSES (bundle = user CA + admin CA)
	// when source=generated, the Secret Core's TRUSTED_CERTIFICATES secretKeyRef and
	// auth's volume read.
	composedTrustedCertsSecretName = "trusted-certificates" //nolint:gosec // G101: Secret name, not a credential
	// trustedCertsBundleKey is the in-Secret key holding the concatenated CA bundle
	// (PEM); it matches the wiring profile's TrustedCertificates.Key.
	trustedCertsBundleKey = "ca.crt"

	// ConfigChecksumAnnotation is the pod-template annotation key carrying a checksum
	// of the config the workload depends on (here, the trusted-certificates bundle).
	// A change to the checksum changes the pod template hash, so the apiserver rolls
	// the workload to pick up the new bundle.
	ConfigChecksumAnnotation = "checksum/config"
)

// ComposesTrustedCerts reports whether the operator composes its OWN
// trusted-certificates Secret (rather than referencing the user's verbatim). This is
// true only when the admin bootstrap requires Core to additionally trust a CA the
// operator knows (source=generated with an operator-owned/edge-reused admin CA):
// Core must trust the issuer of the admin client cert it validates, so that CA cert
// is folded into the bundle. For source=provided or a caller-supplied admin issuerRef
// the operator does not compose — it references the user's Secret unchanged.
func ComposesTrustedCerts(p *otilmv1alpha1.Platform) bool {
	return AdminCABundle(p).Known
}

// TrustedCertsSecretName returns the name of the Secret that holds the trusted-CA
// bundle Core and auth reference, accounting for operator composition:
//
//   - composition active (source=generated + a known admin CA) → the operator-managed
//     "trusted-certificates" Secret (composed read-only at reconcile time).
//   - otherwise → the caller-provided spec.trustedCertificates.secretRef verbatim
//     (which may be empty, in which case no trusted-cert wiring is rendered).
//
// This is the single source of truth for the trusted-cert Secret name on both the
// env (Core) and volume (auth) wiring paths.
func TrustedCertsSecretName(p *otilmv1alpha1.Platform) string {
	if ComposesTrustedCerts(p) {
		return composedTrustedCertsSecretName
	}
	return p.Spec.Common.TrustedCertificates.SecretRef
}

// BuildTrustedCertificatesSecret builds the operator-managed trusted-certificates
// Secret carrying the composed CA bundle under the ca.crt key, labelled and ready for
// a controller owner ref + server-side apply by the reconciler. The bundle bytes are
// composed by the controller (read read-only from the referenced Secrets); this
// builder only shapes the object. SECURITY: CA certs are public, but the discipline
// holds — the bundle lives only in this Secret, never in status/conditions/logs.
func BuildTrustedCertificatesSecret(p *otilmv1alpha1.Platform, bundle []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      composedTrustedCertsSecretName,
			Namespace: p.Namespace,
			Labels: map[string]string{
				common.NameLabel:      composedTrustedCertsSecretName,
				common.InstanceLabel:  p.Name,
				common.ComponentLabel: "trusted-certificates",
				common.PartOfLabel:    common.PartOfValue,
				common.ManagedByLabel: common.ManagedByValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{trustedCertsBundleKey: bundle},
	}
}

// TrustedCertsBundleKey returns the in-Secret key the operator-COMPOSED CA bundle is
// stored under (the OUTPUT key, fixed to "ca.crt"), so the controller and tests
// reference one constant. This is NOT the user-mappable input key — see
// TrustedCertsInputKey.
func TrustedCertsBundleKey() string { return trustedCertsBundleKey }

// TrustedCertsInputKey resolves the effective key the CA bundle is READ from the user's
// spec.trustedCertificates Secret: the spec.trustedCertificates.caKey override when set,
// else the wiring-profile default ("ca.crt"). It maps only the INPUT read; the composed
// bundle's OUTPUT key (TrustedCertsBundleKey) stays the fixed chart contract.
func TrustedCertsInputKey(p *otilmv1alpha1.Platform) string {
	if k := p.Spec.Common.TrustedCertificates.CAKey; k != "" {
		return k
	}
	return wiringFor(p).TrustedCertificates.Key
}

// TrustedCertsSecretKey resolves the effective in-Secret key Core's TRUSTED_CERTIFICATES
// secretKeyRef points at, accounting for operator composition:
//
//   - composition active (source=generated + a known admin CA) → the operator writes the
//     composed bundle under the fixed OUTPUT key (TrustedCertsBundleKey, "ca.crt"), so Core
//     reads THAT key regardless of any user input mapping.
//   - otherwise → Core reads the user's Secret verbatim under the mapped input key
//     (TrustedCertsInputKey).
//
// This pairs with TrustedCertsSecretName (which picks the composed vs. user Secret name)
// so the env wiring's name+key always line up.
func TrustedCertsSecretKey(p *otilmv1alpha1.Platform) string {
	if ComposesTrustedCerts(p) {
		return trustedCertsBundleKey
	}
	return TrustedCertsInputKey(p)
}

// ProvisioningAPIKeyKey resolves the effective in-Secret key Core's PROVISIONING_API_KEY is
// read from. It tracks the same Secret provisioningAPIKeySecretRef resolves:
//
//   - mode=deploy → the deploy bootstrap Secret's API-key key (deploy.apiKeyKey when set,
//     else the wiring default "securityApiKey"), so Core reads the SAME key the deployed
//     service validates against;
//   - mode=external → the spec.provisioning.apiKey override when set, else the
//     wiring-profile default ("provisioningApiKey").
//
// The OUTPUT env var (PROVISIONING_API_KEY) stays a BOM contract.
func ProvisioningAPIKeyKey(p *otilmv1alpha1.Platform) string {
	if ProvisioningDeploy(p) {
		return provisioningAPIKeyKey(p, wiringFor(p).Provisioning)
	}
	if pr := p.Spec.Provisioning; pr != nil && pr.APIKey != "" {
		return pr.APIKey
	}
	return wiringFor(p).ProvisioningAPIKey.Key
}

// StampConfigChecksum sets the checksum/config pod-template annotation on the Core
// workload so a change in Core's reconcile-time config that lives OUTSIDE its pod
// template rolls Core's pods: the composed trusted-certificates bundle, the relayed OIDC
// client Secret, and the in-pod scripts ConfigMap (register-internal-keycloak.sh). All
// three are mounted/sourced by reference, so a change to their content does NOT alter
// Core's pod template on its own — and the postStart hook in particular reads the mounted
// script only at container start while a ConfigMap volume updates lazily, so without this
// roll a script change (e.g. the OIDC URLs) would not take effect until an unrelated
// restart. It handles Core whether it is rendered as a Deployment (the default) or a
// StatefulSet (spec.core.workloadType=StatefulSet). No-op for any other object or an
// empty checksum; returns true when it stamped.
//
// Threading the reconcile-time checksum here (rather than into the pure render) keeps
// RenderPlatform deterministic and free of cluster reads, while still rolling Core on
// a config change. SSA owns only the annotation key the operator sends, so a stable
// checksum does not churn the pod template across reconciles.
func StampConfigChecksum(obj client.Object, checksum string) bool {
	return stampWorkloadConfigChecksum(obj, coreComponentName, checksum)
}

// StampGatewayConfigChecksum sets the checksum/config pod-template annotation on the
// api-gateway workload so a change to the Kong declarative config (kong.yml, in the
// global ConfigMap) rolls the gateway's pods. Kong reads KONG_DECLARATIVE_CONFIG only at
// boot and the ConfigMap volume updates lazily, so without this roll a kong.yml change
// (e.g. the managed-Keycloak /kc route) would not take effect until an unrelated restart.
// No-op for any other object or an empty checksum; returns true when it stamped. Threaded
// in by the reconciler exactly like Core's checksum (kept out of the pure render), so
// RenderPlatform stays deterministic.
func StampGatewayConfigChecksum(obj client.Object, checksum string) bool {
	return stampWorkloadConfigChecksum(obj, gatewayName, checksum)
}

// stampWorkloadConfigChecksum sets the checksum/config annotation on the pod template of
// the workload named `name`, handling both the Deployment and StatefulSet kinds and
// allocating the annotation map if needed. It is a no-op (returns false) for an empty
// checksum, an object of another kind, or a workload with a different name. Shared by the
// per-component stampers (Core, gateway) so the kind switch lives in one place.
func stampWorkloadConfigChecksum(obj client.Object, name, checksum string) bool {
	if checksum == "" {
		return false
	}
	var tmpl *corev1.PodTemplateSpec
	switch w := obj.(type) {
	case *appsv1.Deployment:
		if w.Name != name {
			return false
		}
		tmpl = &w.Spec.Template
	case *appsv1.StatefulSet:
		if w.Name != name {
			return false
		}
		tmpl = &w.Spec.Template
	default:
		return false
	}
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[ConfigChecksumAnnotation] = checksum
	return true
}
