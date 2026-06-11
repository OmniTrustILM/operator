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

// Shared string constants for the platform controller test suite. These deduplicate literals
// that recur across the package's tests (upstream-operator API groups, well-known object
// names/keys, fixture versions, and repeated Ginkgo step descriptions) so a single source of
// truth stays in sync with the production code under test.
const (
	// Upstream-operator API groups gated by the managed-infra detector.
	cnpgGroup        = "postgresql.cnpg.io"
	rabbitmqGroup    = "rabbitmq.com"
	keycloakGroup    = "k8s.keycloak.org"
	certManagerGroup = "cert-manager.io"
	// otilmGroup is the operator's own API group, used in error-classification fixtures.
	otilmGroup = "otilm.com"

	// testEdgeHost is the canonical platform FQDN used across edge/validation assertions.
	testEdgeHost = "ilm.example.com"

	// Well-known operator-managed object names and Secret keys.
	authDBSecretName       = "auth-db"
	connectionStringKey    = "connection-string"
	ilmPlatformCoreName    = "ilm-platform-core"
	caCrtKey               = "ca.crt"
	trustedCertsSecretName = "trusted-certificates"

	// Managed-Keycloak fixture names and realm-import coordinates.
	keycloakName           = "ilm-keycloak"
	keycloakRealmImportRes = "ilm-keycloak-realm"
	realmConfigMapRef      = "ilm-realm"
	realmConfigMapKey      = "ilm_realm.json"

	// Checksum fixture inputs used by combinedCoreChecksum tests.
	checksumTrustedInput = "trusted-abc"
	checksumOIDCInput    = "oidc-xyz"
	checksumScriptsInput = "scripts-123"

	// Credential fixture references/values.
	kcAdminPassword        = "kc-admin-pw"
	adminPasswordSecretRef = "admin-pw"
	usesDBName             = "uses-db"
	userTrustSecretRef     = "user-trust"
	userCAValue            = "USER-CA"

	// Managed-infra fixture versions.
	pgImage16          = "ghcr.io/cloudnative-pg/postgresql:16"
	rabbitVersion313   = "3.13.7"
	keycloakVersion    = "26.4.0"
	platformVersion217 = "2.17.0"
	platformVersion218 = "2.18.0"

	// Repeated Ginkgo step descriptions.
	stepCreatingNamespace       = "creating the dedicated namespace"
	stepMarkingCoreAuthReady    = "simulating kubelet: marking the required Deployments (core, auth) ready"
	stepMarkingDeploymentsReady = "simulating kubelet: marking the required Deployments ready"

	// rejectsManagedWithoutBlock is a repeated CEL-validation spec description.
	rejectsManagedWithoutBlock = "rejects mode=managed without the managed block"
)
