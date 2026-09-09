/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Admin-bootstrap object names, sources, and cert-manager coordinates for the
// optional first-admin certificate (spec.registerAdmin). Core's ADMIN_CERT
// secretKeyRef reads tls.crt from the admin-certificate-secret.
const (
	// adminCertSecretName is the Secret cert-manager populates with the generated
	// admin client certificate (tls.crt/tls.key) — the Secret Core's ADMIN_CERT env
	// resolves from.
	adminCertSecretName = "admin-certificate-secret" //nolint:gosec // G101: Secret name, not a hardcoded credential
	// adminCertName / adminCertRole are the Certificate object name and the component
	// role label the admin cert-manager objects carry.
	adminCertName = "admin-certificate"
	adminCertRole = "register-admin"

	// adminCertCommonName is the default Subject CommonName when registerAdmin.username
	// is unset (the default admin identity "Administrator").
	adminCertCommonName = "Administrator"

	// adminCertDuration / adminCertRenewBefore are sane cert-manager defaults for the
	// admin client cert: a one-year (8760h) lifetime renewed 30 days (720h) before
	// expiry. These are durations, never secret material.
	adminCertDuration    = "8760h"
	adminCertRenewBefore = "720h"

	// Admin private-key parameters (cert-manager generates the key; the operator
	// generates nothing). RSA 2048 is sufficient for a client auth certificate.
	adminPrivateKeyAlgorithm = "RSA"
	adminPrivateKeySize      = int64(2048)

	// Dedicated admin internal-CA chain object names, used ONLY when source=generated
	// has no issuerRef AND the edge does not already provision the internal CA
	// (ca-issuer). They are admin-scoped (distinct from the edge's selfsigned-issuer /
	// ca-certificate / ca-issuer) so generated works standalone without emitting a
	// second ca-issuer that would collide with the edge's.
	adminSelfSignedIssuerName = "admin-selfsigned-issuer"
	adminCACertificateName    = "admin-ca-certificate"
	adminCAKeypairSecret      = "admin-ca-keypair" //nolint:gosec // G101: Secret name, not a hardcoded credential
	adminCAIssuerName         = "admin-ca-issuer"

	// adminCertSourceProvided / adminCertSourceGenerated are the registerAdmin.source
	// values. "provided" = caller supplies the Secret; "generated" = cert-manager
	// issues the cert.
	adminCertSourceProvided  = "provided"
	adminCertSourceGenerated = "generated"
)

// adminCertUsages are the X.509 key usages cert-manager stamps on the admin client
// certificate: client authentication plus the digital-signature and key-encipherment
// usages a TLS client key needs.
var adminCertUsages = []interface{}{"client auth", "digital signature", "key encipherment"}

// registerAdminEnabled reports whether the optional admin bootstrap is on.
func registerAdminEnabled(p *otilmv1alpha1.Platform) bool {
	ra := p.Spec.RegisterAdmin
	return ra != nil && ra.Enabled
}

// registerAdminCertEnabled reports whether the CERTIFICATE admin method is active: the
// bootstrap is enabled AND the certificate sub-block is enabled. The certificate method
// defaults to ON (certificate.enabled defaults true, and an omitted certificate block is
// treated as enabled), so an enabled registerAdmin with no certificate block keeps the
// historical single-method (certificate) behaviour. It is false when the bootstrap is
// disabled or certificate.enabled is explicitly false (password-only).
func registerAdminCertEnabled(p *otilmv1alpha1.Platform) bool {
	if !registerAdminEnabled(p) {
		return false
	}
	cert := p.Spec.RegisterAdmin.Certificate
	// Default ON: an omitted block, or a block with Enabled unset (nil), is enabled. It is
	// OFF only when explicitly set to false (Enabled is a *bool so false ≠ unset).
	return cert == nil || cert.Enabled == nil || *cert.Enabled
}

// adminCertSource returns the configured admin-cert source, defaulting to "provided"
// (the CRD default) when the certificate block is absent or its source is empty.
func adminCertSource(p *otilmv1alpha1.Platform) string {
	if ra := p.Spec.RegisterAdmin; ra != nil && ra.Certificate != nil && ra.Certificate.Source != "" {
		return ra.Certificate.Source
	}
	return adminCertSourceProvided
}

// adminCertSecretRef returns the name of the Secret holding the admin client
// certificate Core reads its ADMIN_CERT from: the caller-provided SecretRef for
// source=provided, or the fixed cert-manager-populated admin-certificate-secret for
// source=generated. It returns "" only when registerAdmin is disabled or a
// source=provided spec omits SecretRef (admission/webhook rejects the latter; the
// caller then renders no ADMIN_CERT rather than a dangling reference).
func adminCertSecretRef(p *otilmv1alpha1.Platform) string {
	if !registerAdminCertEnabled(p) {
		return ""
	}
	if adminCertSource(p) == adminCertSourceGenerated {
		return adminCertSecretName
	}
	if cert := p.Spec.RegisterAdmin.Certificate; cert != nil && cert.SecretRef != nil && *cert.SecretRef != "" {
		return *cert.SecretRef
	}
	return ""
}

// AdminCertSecretRef is the exported accessor for adminCertSecretRef, used by the
// reconciler to read the admin client certificate for the registration action. It
// returns the same value as the internal resolver (the caller's SecretRef for
// source=provided, the generated admin-certificate-secret for source=generated, or "" when
// the certificate method is disabled).
func AdminCertSecretRef(p *otilmv1alpha1.Platform) string {
	return adminCertSecretRef(p)
}

// RegisterAdminCertEnabled is the exported accessor for registerAdminCertEnabled: it
// reports whether the CERTIFICATE admin method is active (the bootstrap is enabled AND the
// certificate sub-block is enabled — the latter defaulting to ON). The reconciler uses it to
// gate the Core registration action, which is the certificate method's terminal step.
func RegisterAdminCertEnabled(p *otilmv1alpha1.Platform) bool {
	return registerAdminCertEnabled(p)
}

// AdminCertKey resolves the effective in-Secret key the admin client certificate is read
// from. For source=provided it is the spec.registerAdmin.certKey override when set, else
// the wiring-profile default ("tls.crt"). For source=generated it is ALWAYS the default —
// cert-manager populates the fixed Secret with the standard kubernetes.io/tls keys, NOT
// user-mappable. The OUTPUT env var (ADMIN_CERT) stays a BOM contract.
func AdminCertKey(p *otilmv1alpha1.Platform) string {
	w := wiringFor(p)
	if adminCertSource(p) == adminCertSourceProvided {
		if cert := certificateSpec(p); cert != nil && cert.CertKey != "" {
			return cert.CertKey
		}
	}
	return w.AdminCert.Key
}

// AdminPrivateKeyKey resolves the effective in-Secret key the admin client private key is
// read from (the same source=provided-only mapping rule as AdminCertKey; default "tls.key").
// It is shipped for the registration follow-up that consumes the keypair; today the operator
// reads only the certificate, so this has no current read consumer but keeps the provided-
// Secret mapping complete and BOM-defaulted.
func AdminPrivateKeyKey(p *otilmv1alpha1.Platform) string {
	w := wiringFor(p)
	if adminCertSource(p) == adminCertSourceProvided {
		if cert := certificateSpec(p); cert != nil && cert.PrivateKeyKey != "" {
			return cert.PrivateKeyKey
		}
	}
	return w.AdminPrivateKeyKey
}

// certificateSpec returns the registerAdmin.certificate sub-block, or nil when the
// bootstrap (or the sub-block) is unset. A nil return means the certificate method runs
// with all defaults (source=provided, the BOM keys), exactly as an explicit empty block.
func certificateSpec(p *otilmv1alpha1.Platform) *otilmv1alpha1.AdminCertificateSpec {
	if ra := p.Spec.RegisterAdmin; ra != nil {
		return ra.Certificate
	}
	return nil
}

// certIssuerRef returns the certificate block's issuerRef, or nil when the block is unset.
func certIssuerRef(cert *otilmv1alpha1.AdminCertificateSpec) *otilmv1alpha1.CertManagerIssuerRef {
	if cert == nil {
		return nil
	}
	return cert.IssuerRef
}

// generatesAdminCert reports whether the operator renders cert-manager objects for
// the admin certificate: only when the certificate method is enabled with source=generated.
// For source=provided the caller supplies the Secret and the operator renders no
// cert-manager objects.
func generatesAdminCert(p *otilmv1alpha1.Platform) bool {
	return registerAdminCertEnabled(p) && adminCertSource(p) == adminCertSourceGenerated
}

// edgeProvisionsInternalCA reports whether the edge already provisions the internal
// CA chain (selfsigned-issuer -> ca-certificate -> ca-issuer) — i.e. the edge is
// enabled with tls.source=internal. When true, the admin cert reuses that existing
// ca-issuer rather than emitting a duplicate one.
func edgeProvisionsInternalCA(p *otilmv1alpha1.Platform) bool {
	edge := p.Spec.Edge
	if edge == nil || !edge.Enabled {
		return false
	}
	// gatewayAPI with a parentRef renders no cert-manager objects (TLS is the existing
	// Gateway's concern), so the internal CA is NOT provisioned in that case.
	if edge.Type == otilmv1alpha1EdgeTypeGatewayAPI {
		if gw := gatewayAPISpec(p); gw != nil && gw.ParentRef != nil {
			return false
		}
	}
	return edgeSource(edge.TLS) == edgeSourceInternal
}

// adminIssuerResolution captures how the admin certificate's issuerRef is resolved
// and whether the operator must also render a dedicated admin CA chain to back it.
type adminIssuerResolution struct {
	// issuerRef is the cert-manager issuerRef block stamped on the admin Certificate.
	issuerRef map[string]interface{}
	// renderAdminCAChain is true when the operator must provision its OWN admin
	// self-signed CA chain (admin-selfsigned-issuer -> admin-ca-certificate ->
	// admin-ca-issuer) to satisfy the resolved issuerRef.
	renderAdminCAChain bool
}

// resolveAdminIssuer determines the admin certificate's issuer, in priority order:
//
//  1. registerAdmin.issuerRef set → use it verbatim (the caller's Issuer/ClusterIssuer,
//     honoring kind/group). No CA chain is rendered (the caller owns the issuer).
//  2. issuerRef unset + the edge already provisions the internal CA
//     (edge.tls.source=internal) → REUSE the edge's ca-issuer (a namespaced Issuer).
//     This avoids emitting a second ca-issuer that would collide with the edge's.
//  3. issuerRef unset + no edge internal CA → render a DEDICATED admin self-signed CA
//     chain (admin-scoped names) and point the admin cert at admin-ca-issuer, so
//     generated works standalone independent of the edge config.
//
// SECURITY: only issuer NAMES/kinds/groups are referenced here; no key material.
func resolveAdminIssuer(p *otilmv1alpha1.Platform) adminIssuerResolution {
	cert := certificateSpec(p)
	if ref := certIssuerRef(cert); ref != nil && ref.Name != "" {
		kind := ref.Kind
		if kind == "" {
			kind = kindIssuer
		}
		issuerRef := map[string]interface{}{
			"name": ref.Name,
			"kind": kind,
		}
		// Emit group only when non-default (e.g. an external issuer); cert-manager
		// defaults the group to cert-manager.io.
		if ref.Group != "" && ref.Group != certManagerDefaultGroup {
			issuerRef["group"] = ref.Group
		}
		return adminIssuerResolution{issuerRef: issuerRef}
	}

	// Default to the platform internal CA. Reuse the edge's ca-issuer when present.
	if edgeProvisionsInternalCA(p) {
		return adminIssuerResolution{
			issuerRef: map[string]interface{}{"name": caIssuerName, "kind": kindIssuer},
		}
	}

	// Standalone: the operator renders its own admin CA chain.
	return adminIssuerResolution{
		issuerRef:          map[string]interface{}{"name": adminCAIssuerName, "kind": kindIssuer},
		renderAdminCAChain: true,
	}
}

// ResolveAdminCertObjects returns the cert-manager objects the operator renders for
// the optional admin bootstrap, or nil when registerAdmin is disabled or its source
// is "provided" (the caller supplies the Secret and the operator renders nothing).
//
// For source=generated it returns a cert-manager Certificate that issues the admin
// CLIENT certificate into the admin-certificate-secret Secret (the Secret Core's
// ADMIN_CERT resolves from). The Certificate's issuerRef is
// resolved by resolveAdminIssuer; when that requires a dedicated admin self-signed CA
// chain (no issuerRef and no edge internal CA) the chain objects are prepended so they
// reconcile before the leaf. cert-manager owns the keypair — the operator generates
// NOTHING (no openssl, no kubectl, no operator-minted password).
//
// SECURITY: no certificate or private-key material is ever placed in these objects;
// cert-manager populates the Secret and the operator references it by name only.
//
// FOLLOW-UP: a PKCS12 keystore is intentionally not emitted — cert-manager's
// keystores.pkcs12 needs a password Secret, and the operator mints no password. If a
// PKCS12 keystore is needed later it must reference a caller-provided password Secret.
func ResolveAdminCertObjects(p *otilmv1alpha1.Platform) []client.Object {
	if !generatesAdminCert(p) {
		return nil
	}

	res := resolveAdminIssuer(p)

	var objs []client.Object
	// Render the dedicated admin CA chain first (when required) so it reconciles
	// before the leaf Certificate that references admin-ca-issuer.
	if res.renderAdminCAChain {
		objs = append(objs,
			buildAdminSelfSignedIssuer(p),
			buildAdminCACertificate(p),
			buildAdminCAIssuer(p),
		)
	}
	objs = append(objs, buildAdminCertificate(p, res.issuerRef))
	return objs
}

// AdminCABundleSource describes where the admin certificate's issuing CA cert lives,
// so the trusted-certificate composition can fold it into Core's trust bundle (Core
// must trust the issuer of the admin client cert it validates). Only populated when
// the operator itself provisions or reuses a CA whose Secret it knows; for a
// caller-supplied issuerRef the operator does not own the CA Secret and Known is false.
type AdminCABundleSource struct {
	// SecretName is the name of the cert-manager CA keypair Secret holding the issuing
	// CA certificate (its tls.crt / ca.crt). Empty when Known is false.
	SecretName string
	// Known reports whether the operator knows the CA Secret to bundle. False when the
	// admin cert is signed by a caller-supplied issuerRef (the operator does not own
	// that CA Secret, so it cannot read it — the caller's trust store is their concern).
	Known bool
}

// AdminCABundle resolves the CA Secret whose certificate must be added to Core's
// trusted bundle so Core trusts the issuer of the generated admin client cert. It
// mirrors resolveAdminIssuer: a dedicated admin CA chain → admin-ca-keypair; reuse of
// the edge internal CA → ca-keypair; a caller-supplied issuerRef → not known (the
// operator does not own that CA Secret). It returns Known=false when the admin
// bootstrap is disabled or source=provided (nothing for the operator to bundle).
func AdminCABundle(p *otilmv1alpha1.Platform) AdminCABundleSource {
	if !generatesAdminCert(p) {
		return AdminCABundleSource{}
	}
	res := resolveAdminIssuer(p)
	if res.renderAdminCAChain {
		// The operator provisions a dedicated admin self-signed CA into admin-ca-keypair.
		return AdminCABundleSource{SecretName: adminCAKeypairSecret, Known: true}
	}
	// resolveAdminIssuer points the leaf at the edge's ca-issuer (ca-keypair) only when
	// the edge already provisions the internal CA; otherwise it set renderAdminCAChain.
	// So a non-chain resolution backed by the platform internal CA means the edge CA.
	if edgeProvisionsInternalCA(p) {
		return AdminCABundleSource{SecretName: caKeypairSecret, Known: true}
	}
	// A caller-supplied issuerRef: the operator does not own the CA Secret.
	return AdminCABundleSource{}
}

// adminCertCommonNameValue returns the Subject CommonName for the admin certificate:
// registerAdmin.username when set, otherwise the default ("Administrator"). This is
// the admin identity the certificate binds, not a credential.
func adminCertCommonNameValue(p *otilmv1alpha1.Platform) string {
	if ra := p.Spec.RegisterAdmin; ra != nil && ra.Username != "" {
		return ra.Username
	}
	return adminCertCommonName
}

// buildAdminCertificate renders the admin client Certificate (cert-manager.io/v1):
// the resolved issuerRef signs a client-auth leaf with the admin Subject CommonName
// into the admin-certificate-secret Secret. cert-manager generates and stores the
// keypair; the operator places no cert/key material here.
func buildAdminCertificate(p *otilmv1alpha1.Platform, issuerRef map[string]interface{}) *unstructured.Unstructured {
	return newCertManagerObject(p, kindCertificate, adminCertName, adminCertRole, map[string]interface{}{
		"secretName":  adminCertSecretName,
		"commonName":  adminCertCommonNameValue(p),
		"duration":    adminCertDuration,
		"renewBefore": adminCertRenewBefore,
		"usages":      adminCertUsages,
		"privateKey": map[string]interface{}{
			"algorithm": adminPrivateKeyAlgorithm,
			"size":      adminPrivateKeySize,
		},
		"issuerRef": issuerRef,
	})
}

// buildAdminSelfSignedIssuer renders the admin self-signed Issuer (the root of the
// dedicated admin CA chain), used only when generated has no issuerRef and the edge
// does not already provision the internal CA.
func buildAdminSelfSignedIssuer(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindIssuer, adminSelfSignedIssuerName, adminCertRole, map[string]interface{}{
		"selfSigned": map[string]interface{}{},
	})
}

// buildAdminCACertificate renders the admin CA Certificate (isCA, RSA 4096, into the
// admin-ca-keypair Secret, signed by admin-selfsigned-issuer) — the intermediate of
// the dedicated admin CA chain.
func buildAdminCACertificate(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindCertificate, adminCACertificateName, adminCertRole, map[string]interface{}{
		"isCA":       true,
		"commonName": "admin-ca",
		"secretName": adminCAKeypairSecret,
		"privateKey": map[string]interface{}{
			"algorithm": caPrivateKeyAlgorithm,
			"size":      caPrivateKeySize,
		},
		"issuerRef": map[string]interface{}{
			"name": adminSelfSignedIssuerName,
			"kind": kindIssuer,
		},
	})
}

// buildAdminCAIssuer renders the admin CA Issuer (ca: {secretName: admin-ca-keypair})
// — the issuer the admin leaf Certificate references when the operator provisions its
// own admin CA chain.
func buildAdminCAIssuer(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	return newCertManagerObject(p, kindIssuer, adminCAIssuerName, adminCertRole, map[string]interface{}{
		"ca": map[string]interface{}{
			"secretName": adminCAKeypairSecret,
		},
	})
}

// AdminCertDependencies returns the cert-manager prerequisites for the admin
// certificate, or nil when nothing the operator renders needs cert-manager (admin
// bootstrap disabled, or source=provided where the caller supplies the Secret).
// source=generated needs cert-manager: the operator renders a Certificate (and
// possibly an Issuer chain) only cert-manager reconciles. The actionable message
// points at the provided escape hatch. It mirrors the edge's dependency model
// (EdgeDependency) so the reconciler gates both through the same machinery.
func AdminCertDependencies(p *otilmv1alpha1.Platform) []EdgeDependency {
	if !generatesAdminCert(p) {
		return nil
	}
	msg := "spec.registerAdmin.source=generated requires cert-manager; install cert-manager " +
		"or set spec.registerAdmin.source=provided to supply the admin certificate Secret"
	return []EdgeDependency{
		{
			GroupKind: schema.GroupKind{Group: "cert-manager.io", Kind: kindCertificate},
			Versions:  []string{"v1"},
			Reason:    ReasonCertManagerNotInstalled,
			Message:   msg,
		},
		{
			GroupKind: schema.GroupKind{Group: "cert-manager.io", Kind: kindIssuer},
			Versions:  []string{"v1"},
			Reason:    ReasonCertManagerNotInstalled,
			Message:   msg,
		},
	}
}
