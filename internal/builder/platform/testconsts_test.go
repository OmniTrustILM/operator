/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// Shared string-literal constants used across the platform builder test files. Centralizing
// these de-duplicates literals the tests repeat (avoiding go:S1192) while keeping the value a
// single source of truth.
const (
	// Cert-manager / Gateway API coordinates referenced in the edge tests.
	testVaultIssuer         = "vault-issuer"
	testCorpCA              = "corp-ca"
	testExternalIssuerGroup = "issuer.example.com"
	testExternalIssuerKind  = "ExampleClusterIssuer"
	testGatewayAPIGroup     = "gateway.networking.k8s.io"

	// Common identifiers/hosts shared by multiple test files.
	testEdgeHost     = "ilm.example.com"
	testBYOTLSSecret = "my-byo-tls"
	testClientCrt    = "client.crt"
	testILMDB        = "ilm-db"
	testProvURL      = "https://prov.example.com"

	// testOperatorNamespace is the namespace the operator renders from; the ingress default-deny
	// admits the operator's pods from it.
	testOperatorNamespace = "ilm-operator-system"

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
	// testDefaultClientCertHeader is the header the version bundle's wiring names for the client
	// certificate the edge forwards; testCustomClientCertHeader is a spec.core.clientCertHeader that
	// replaces it.
	testDefaultClientCertHeader = "ssl-client-cert"
	testCustomClientCertHeader  = "x-client-cert"
	// testMCPMetadataPath is the OAuth protected-resource metadata path the MCP route serves, and
	// testRequestTransformer the Kong plugin that strips the client-certificate header from it.
	testMCPMetadataPath    = "/.well-known/oauth-protected-resource"
	testRequestTransformer = "request-transformer"

	// managed_database_test.go.
	testDefaultKeptMsg = "default kept when not overridden"
	testKindDatabase   = "Database"

	// managed_keycloak_test.go.
	testPGHost       = "pg.example.com"
	testKeycloakName = "ilm-keycloak"
	testRealmName    = "ilm-realm"
	testLogLevel     = "log-level"
	testThemeTag     = "develop-latest"

	// managed_messaging_test.go.
	testMessagingName    = "ilm-messaging"
	testMsgVhost         = "ilm-messaging-vhost"
	testMsgUserCore      = "ilm-messaging-core"
	testMsgUserMonitor   = "ilm-messaging-monitor"
	testMsgUserProv      = "ilm-messaging-provisioner"
	testMsgUserProxy     = "ilm-messaging-proxy"
	testMsgCorePerm      = "ilm-messaging-core-permission"
	testMsgQueueAudit    = "ilm-messaging-queue-core-audit-logs"
	testQueueAuditLogs   = "core.audit-logs"
	testQueueTQResults   = "time-quality.results"
	testQueueTQConfigReq = "time-quality.config-request"
	testQueueTQConfig    = "time-quality.config"
	testQueueDiscovery   = "provider.discovery-work"
	// The administrator User and its Permission, referenced by the per-user expectation tables
	// (the frozen name lists spell every name out on purpose and are left literal).
	testMessagingAdminUser = "ilm-messaging-administrator"
	testMessagingAdminPerm = "ilm-messaging-administrator-permission"

	// messaging_connection_test.go.
	testILMMQ = "ilm-mq"
	// testExternalMQHost is the broker host an external-messaging spec carries.
	testExternalMQHost = "rabbitmq.example.com"

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
	testNamespace        = "ilm-system"
	testDBCreds          = "db-creds"
	testMQCreds          = "mq-creds"
	testRegistry         = "registry.example.com"
	testMQHost           = "mq.example.com"
	testProvSecret       = "prov-secret"
	testAdminCert        = "my-admin-cert"
	testTLSCrt           = "tls.crt"
	testWaitForAuth      = "wait-for-auth"
	testWaitForMessaging = "wait-for-messaging-service"
	// testMQWaitLoop is the broker-reachability poll the wait loops must render: the
	// coordinates come from QUOTED env expansions, never from interpolated script text.
	testMQWaitLoop        = `while ! nc -z "$MQ_WAIT_HOST" "$MQ_WAIT_PORT"; do sleep 1; done`
	testCurlImage         = "hub.omnitrustregistry.com/ilm/curl:8.16.0"
	testProvInstanceQueue = "provision-instance-queue"
	// testProvInstanceQueueMsg is the assertion message every proxy+provisioning render case
	// shares.
	testProvInstanceQueueMsg = "provision-instance-queue must render on the proxy+provisioning path"
	testTrustedCerts         = "trusted-certificates"
	testEphemeralMsg         = "ephemeral volume must be present"
	testLivenessMsg          = "liveness probe disabled by default"
	testConfigJS             = "config.js"
	testCABundle             = "ca-bundle"
	testTLSCA                = "tls-ca"
	// testJDBCURL is the database URL the wiring profile composes for basePlatform's external database.
	testJDBCURL = "jdbc:postgresql://db.example.com:5432/ilmdb?characterEncoding=UTF-8"
	// The outbound proxy settings a spec carries and the variables render back, shared by the Core and
	// MCP tests.
	testProxyHTTP   = "http://proxy:3128"
	testProxyHTTPS  = "http://proxy:3129"
	testNoProxyList = "localhost,.svc"
	// testNoProxyEntries is a noProxy value the MCP tests extend with MCP's own hosts.
	testNoProxyEntries = "localhost,*.svc"

	// trustedcerts_test.go.
	testMyTrust           = "my-trust"
	testKongABC           = "kong-abc"
	testMCPChecksum       = "mcp-abc"
	testEmptyChecksumNoOp = "empty checksum is a no-op"

	// version_test.go.
	testVersion217 = "2.17.0"
	testVersion218 = "2.18.0"
	testVersion219 = "2.19.0"
	testVersion220 = "2.20.0"
	// testVersion221 is the preview bundle that carries the MCP server image.
	testVersion221 = "2.21.0"

	// mcp_test.go.
	testMCPName            = "mcp-server"
	testMCPImage           = "hub.omnitrustregistry.com/ilm/mcp-server:1.0.0"
	testMCPTag             = "develop-latest"
	testMCPClientSecret    = "ilm-mcp-client"
	testMCPClientSecretKey = "clientSecret"
	testMCPClientSecretEnv = "ILM_MCP_SECURITY_CLIENT_SECRET"
	testMCPAccessReadOnly  = "read-only"
	testMCPAccessReadWrite = "read-write"
	testMCPAccessFull      = "full"
	testMCPIssuerEnv       = "ILM_MCP_SECURITY_ISSUER"
	testMCPResourceEnv     = "ILM_MCP_SECURITY_RESOURCE"
	testMCPJWKSEnv         = "ILM_MCP_SECURITY_JWK_SET_URI"
	testMCPTokenEnv        = "ILM_MCP_SECURITY_TOKEN_URI"
	testMCPReadinessPath   = "/actuator/health/readiness"
	testMCPLivenessPath    = "/actuator/health/liveness"
	// testMCPResourceURL is the resource MCP derives from the platform's public host, ilm.example.com
	// in these tests; testMustBeSet is the failure message of an assertion that a variable is rendered.
	testMCPResourceURL = "https://ilm.example.com/mcp"
	testMustBeSet      = "%s must be set"
	// The settings of the external identity provider the MCP tests configure, and the variables MCP
	// reads them from. The client id differs from managed mode's, so a value that came from the spec
	// cannot be mistaken for the one managed mode derives.
	testMCPClientIDEnv        = "ILM_MCP_SECURITY_CLIENT_ID"
	testMCPExchangeModeEnv    = "ILM_MCP_SECURITY_EXCHANGE_MODE"
	testMCPExchangeScopeEnv   = "ILM_MCP_SECURITY_EXCHANGE_SCOPE"
	testMCPAuthScopesEnv      = "ILM_MCP_SECURITY_AUTHORIZATION_SCOPES"
	testMCPRequiredScopeEnv   = "ILM_MCP_SECURITY_REQUIRED_SCOPE"
	testMCPAPIAudienceEnv     = "ILM_MCP_SECURITY_API_AUDIENCE"
	testMCPBackchannelEnv     = "ILM_MCP_SECURITY_ALLOW_HTTP_BACKCHANNEL"
	testMCPIdPIssuer          = "https://idp.example.com/realms/corp"
	testMCPIdPJWKS            = "http://kc.idp.svc:8080/realms/corp/protocol/openid-connect/certs"
	testMCPIdPToken           = "http://kc.idp.svc:8080/realms/corp/protocol/openid-connect/token"
	testMCPIdPClientID        = "corp-mcp"
	testMCPIdPClientSecret    = "mcp-idp-client"
	testMCPIdPClientSecretKey = "client-secret"
	testMCPIdentityManaged    = "managed"
	testMCPIdentityExternal   = "external"
	testMCPExchangeDelegation = "delegation"
	testMCPExchangeStandard   = "standard"
	testMCPIdPCustomSecretKey = "idp-secret"

	// The variables Core and MCP both read for the trusted certificates and the outbound proxy, and
	// the Secret the tests reference for the certificates.
	testTrustedCertsEnv = "TRUSTED_CERTIFICATES"
	testHTTPProxyEnv    = "HTTP_PROXY"
	testHTTPSProxyEnv   = "HTTPS_PROXY"
	testNoProxyEnv      = "NO_PROXY"
	testProxyEnabledEnv = "PROXY_ENABLED"
	testTrustSecret     = "ilm-trusted-ca"

	// resolve_test.go — the per-bundle proxy exchange names (2.19.0 renamed them).
	testExchangeCzertainlyProxy = "czertainly-proxy"
	testExchangeIlmProxy        = "ilm-proxy"
	testCustomProxyExchange     = "custom-proxy"
	testQueueArgExpires         = "x-expires"
	// testQueueArgHostile is the argument name the hostile queueArguments values are stored
	// under, so the round-trip assertions read one key.
	testQueueArgHostile = "x-hostile"
	// testVerifyInstanceID is the name of Core's StatefulSet-derived-instance-id fail-fast init
	// container, asserted both present (the derivation case) and absent (every other shape).
	testVerifyInstanceID = "verify-instance-id"

	// time_quality_test.go.
	testTQMonitorSecretRef = "tq-monitor"
	// testSharedCommand/testSharedArg are the shared image.command/image.args override the
	// per-component override must beat, and the value that applies when the component sets none.
	testSharedCommand = "/bin/shared"
	testSharedArg     = "--shared"
	// testTQMonitorUsernameKey/testTQMonitorPasswordKey are the CR-supplied credential-key
	// overrides an external time-quality-monitor Credentials ref must honour.
	testTQMonitorUsernameKey = "svc-user"
	testTQMonitorPasswordKey = "svc-pass"
	// testSharedPullSecret/testPrivatePullSecret are the shared and per-sidecar pull secrets the
	// time-quality-monitor's pod-level imagePullSecrets union must carry.
	testSharedPullSecret  = "shared-pull"
	testPrivatePullSecret = "private-pull"
)
