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

import otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"

// Shared string constants for the platform controller test suite. These deduplicate literals
// that recur across the package's tests (upstream-operator API groups, well-known object
// names/keys, fixture versions, and repeated Ginkgo step descriptions) so a single source of
// truth stays in sync with the production code under test.
//
// Workload NAMES are not repeated here: the production constants (coreDeploymentName,
// gatewayWorkloadName, schedulerWorkloadName, provisioningWorkloadName) are what the reconciler
// addresses, so the tests address them through the same names.
const (
	// Workload kinds, exactly as the migration fence records them on status.upgrade.fenced and
	// as the render produces them.
	kindDeployment  = string(otilmv1alpha1.WorkloadKindDeployment)
	kindStatefulSet = string(otilmv1alpha1.WorkloadKindStatefulSet)

	// errStatusWriteRejected is the refusal the migration suites inject to prove a state write
	// that does not land stops the pass.
	errStatusWriteRejected = "status write rejected"
	// errNoBrokerReachable is what every call on an unreachable broker answers, so a drain
	// pointed at one can only fail closed.
	errNoBrokerReachable = "no broker is reachable"

	// Source-topology queue names the drain fixtures use: one Core queue, one time-quality
	// queue (retained across the migration, so never drainable) and two proxy instance queues.
	testQueueCoreEvents  = "core.events"
	testQueueTQConfig    = "time-quality.config"
	testQueueInstanceOne = "instance-1"
	testQueueInstanceHex = "instance-7a3f"

	// forceCutoverField is the spec field a blocked migration's message must offer as an exit.
	forceCutoverField = "spec.messaging.managed.forceCutoverForVersion"

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
	// platformVersion219 is the 2.19.0 fixture version: the operator's newest RELEASED bundle
	// and its DefaultVersion — used to exercise the messaging migration engine and the
	// running-vs-requested version split on deletion.
	platformVersion219 = "2.19.0"

	// Repeated Ginkgo step descriptions.
	stepCreatingNamespace       = "creating the dedicated namespace"
	stepMarkingCoreAuthReady    = "simulating kubelet: marking the required Deployments (core, auth) ready"
	stepMarkingDeploymentsReady = "simulating kubelet: marking the required Deployments ready"

	// rejectsManagedWithoutBlock is a repeated CEL-validation spec description.
	rejectsManagedWithoutBlock = "rejects mode=managed without the managed block"

	// steadyStateNotFailure is the assertion message the migration suites repeat wherever a
	// refusal/steady-state path must not be reported as a reconcile error.
	steadyStateNotFailure = "a refusal is a steady state, not a reconcile failure"
)
