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

// Shared string-literal constants used across the platform builder test files. Centralizing
// these de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a
// single source of truth.
const (
	// Cert-manager / Gateway API coordinates referenced in the edge tests.
	testVaultIssuer     = "vault-issuer"
	testCorpCA          = "corp-ca"
	testGatewayAPIGroup = "gateway.networking.k8s.io"

	// Common identifiers/hosts shared by multiple test files.
	testEdgeHost     = "ilm.example.com"
	testBYOTLSSecret = "my-byo-tls"
	testClientCrt    = "client.crt"
	testILMDB        = "ilm-db"
	testProvURL      = "https://prov.example.com"

	// admin_test.go.
	testClientKey = "client.key"

	// component_overrides_test.go.
	testCoreIRSA    = "core-irsa"
	testCustomEntry = "/bin/custom-entry"
	testFlagArg     = "--flag"
	testSchedSA     = "sched-sa"
	testIngressRole = "ilm-platform-core"
	testSharedGW    = "shared-gw"
	testACMEEmail   = "info@example.com"

	// gateway_test.go.
	testMessagingService = "messaging-service"

	// managed_database_test.go.
	testDefaultKeptMsg = "default kept when not overridden"

	// managed_keycloak_test.go.
	testPGHost       = "pg.example.com"
	testKeycloakName = "ilm-keycloak"
	testRealmName    = "ilm-realm"
	testLogLevel     = "log-level"

	// managed_messaging_test.go.
	testMessagingName    = "ilm-messaging"
	testQueueAuditLogs   = "core.audit-logs"
	testQueueTQResults   = "time-quality.results"
	testQueueTQConfigReq = "time-quality.config-request"
	testQueueTQConfig    = "time-quality.config"

	// messaging_connection_test.go.
	testILMMQ = "ilm-mq"

	// platform_host_test.go.
	testHTTPSScheme  = "https://"
	testRedirectPath = "/api/login/oauth2/code/internal"
	testAdminPath    = "/administrator/"

	// provisioning_test.go.
	testProvBootstrap = "provisioning-bootstrap"
	testAPIKey        = "my-api-key"

	// render_test.go.
	testOPAPolicies          = "auth-opa-policies"
	testFeAdmin              = "fe-administrator"
	testDeploymentMustRender = "Deployment %q must render"
	testGlobalVol            = "global-vol"
	testGlobalSidecar        = "global-sidecar"

	// resolve_test.go.
	testNamespace         = "ilm-system"
	testDBCreds           = "db-creds"
	testMQCreds           = "mq-creds"
	testRegistry          = "registry.example.com"
	testProvSecret        = "prov-secret"
	testAdminCert         = "my-admin-cert"
	testTLSCrt            = "tls.crt"
	testWaitForAuth       = "wait-for-auth"
	testCurlImage         = "hub.omnitrustregistry.com/ilm/curl:8.16.0"
	testProvInstanceQueue = "provision-instance-queue"
	testTrustedCerts      = "trusted-certificates"
	testEphemeralMsg      = "ephemeral volume must be present"
	testLivenessMsg       = "liveness probe disabled by default"
	testConfigJS          = "config.js"
	testCABundle          = "ca-bundle"
	testTLSCA             = "tls-ca"

	// trustedcerts_test.go.
	testMyTrust = "my-trust"
	testKongABC = "kong-abc"

	// version_test.go.
	testVersion218 = "2.18.0"
)
