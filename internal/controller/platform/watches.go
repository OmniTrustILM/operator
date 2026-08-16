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
	"context"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// platformsForSecret maps a watched Secret to the Platforms in its namespace that
// reference it, so a credential / CA-bundle rotation triggers a reconcile. A Platform
// is enqueued when the Secret's name matches any of its referenced-Secret fields:
// database.credentials.secretRef, messaging.credentials.secretRef,
// trustedCertificates.secretRef, provisioning.apiKeySecretRef,
// registerAdmin.certificate.secretRef, registerAdmin.password.secretRef,
// or edge.tls.secretRef. The reconcile then re-composes the operator-managed
// auth-db / trusted-certificates Secrets (whose checksum annotation rolls the
// consumers) and re-reads the rotated material.
//
// SECURITY: only Secret NAMES are compared and logged here — never the Secret content.
func (r *Reconciler) platformsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	var platforms otilmv1alpha1.PlatformList
	if err := r.List(ctx, &platforms, client.InNamespace(obj.GetNamespace())); err != nil {
		logger.Error(err, "failed to list Platforms for Secret watch")
		return nil
	}

	name := obj.GetName()
	var requests []reconcile.Request
	for i := range platforms.Items {
		p := &platforms.Items[i]
		for _, ref := range referencedSecretNames(p) {
			if ref == name {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
				break
			}
		}
	}

	if len(requests) > 0 {
		logger.Info("Secret change triggered Platform reconcile", "secret", name, "platforms", len(requests))
	}
	return requests
}

// referencedSecretNames returns every Secret name the Platform references (names only;
// never values). It is the single source of truth for the WATCH mapping
// (platformsForSecret) ONLY — it is NOT consumed by the credential preflight
// (preflightCredentialSecrets resolves the DB/messaging refs itself, directly from
// ResolveDatabaseConnection/ResolveMessagingConnection; see the deliberate-exclusion note
// below for why some of this function's refs must never be added there). The database
// and messaging credentials Secrets are resolved mode-agnostically: the caller's Secret
// for an external dependency, or the CloudNativePG-generated <cluster>-app Secret /
// Messaging-Topology-generated per-user Secrets for a managed one — so the Secret watch
// re-enqueues the Platform the instant the upstream operator creates the generated Secret
// (this is what lets the managed-database / managed-broker readbacks converge WITHOUT
// watching the CNPG Cluster or the RabbitMQ types directly, which would break the cache
// when the CRD is absent). For a managed broker the provisioner-user Secret is included
// too (the provisioning flow's credential). A managed Keycloak needs NO new Secret here for
// provisioning — it consumes the SAME platform DB-credentials Secret (already included
// above) via spec.db.usernameSecret/passwordSecret, so the existing Secret watch already
// re-enqueues when that Secret appears/rotates; the Keycloak Operator-generated initial-admin
// Secret is consumed by the OIDC wiring (which reads it directly), not by the provisioning
// watch set. Empty refs (an unset optional field) are skipped.
func referencedSecretNames(p *otilmv1alpha1.Platform) []string {
	var names []string
	add := func(ref string) {
		if ref != "" {
			names = append(names, ref)
		}
	}
	add(platformbuilder.ResolveDatabaseConnection(p).CredentialsSecretName)
	mq := platformbuilder.ResolveMessagingConnection(p)
	add(mq.CredentialsSecretName)
	add(mq.ProvisioningCredentialsSecretName)
	// The time-quality-monitor sidecar's broker credentials. What the watch buys is CONVERGENCE
	// ON APPEARANCE: in MANAGED messaging this Secret is generated by the Messaging Topology
	// Operator, so the watch is the only thing that re-enqueues the Platform when it finally
	// exists, and the reference is wired on that pass. Empty when no sidecar renders.
	//
	// DELIBERATELY NOT PREFLIGHTED. preflightCredentialSecrets guards only the Secrets the
	// PLATFORM cannot start without (the DB and messaging credentials). The monitor is an
	// opt-in SIDECAR: a missing credentials Secret must degrade that sidecar alone (its
	// container fails to start; Core and every other component are unaffected), never the
	// whole platform. Adding this ref to the preflight would put a platform with an
	// otherwise-healthy Core into MissingSecret over an optional sidecar's Secret — so this
	// entry is watched for convergence but must never be added to preflightCredentialSecrets.
	add(platformbuilder.TimeQualityMonitorCredentialsSecretName(p))
	add(p.Spec.Common.TrustedCertificates.SecretRef)
	if pr := p.Spec.Provisioning; pr != nil {
		add(pr.APIKeySecretRef)
	}
	if ra := p.Spec.RegisterAdmin; ra != nil {
		// Certificate method (source=provided): the caller-supplied admin cert Secret.
		if cert := ra.Certificate; cert != nil && cert.SecretRef != nil {
			add(*cert.SecretRef)
		}
		// Password method: the caller-supplied admin password Secret. A rotation
		// re-enqueues, but the realm-user creation is idempotent (the operator does NOT
		// reset the password on a re-reconcile — see reconcileAdminKeycloakUser).
		if pw := ra.Password; pw != nil {
			add(pw.SecretRef)
		}
	}
	if e := p.Spec.Edge; e != nil && e.TLS != nil && e.TLS.SecretRef != nil {
		add(*e.TLS.SecretRef)
	}
	return names
}
