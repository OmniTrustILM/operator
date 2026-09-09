/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// provisioning.go renders the bundled provisioning-rabbitmq service as a native, operator-
// managed component when provisioning.mode=deploy. The service dynamically manages
// per-proxy AMQP topology for REMOTE proxies/connectors (outside the cluster, talking over
// the broker); Core calls it via a REST API. It is a pure builder over the version bundle's
// wiring data.
//
// In external mode the operator renders NOTHING here (Core points at the caller's own
// provisioner via spec.provisioning.apiURL, today's consume-only behaviour). The
// switch lives in ProvisioningDeploy.
//
// SECURITY: the JWT signing key and the API key are consumed via secretKeyRef ONLY (from
// the deploy bootstrap Secret) — never inlined into the rendered objects, status,
// conditions, or logs. The broker provisioner/proxy credentials are likewise injected by
// reference only.

import (
	"fmt"
	"strconv"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
	corev1 "k8s.io/api/core/v1"
)

const (
	// provisioningName is the operator's clean component name for the bundled
	// provisioning-rabbitmq service (clean, unsuffixed like every other component).
	provisioningName = "provisioning-rabbitmq"
	// provisioningPort is the service's HTTP listening port (8077).
	provisioningPort int32 = 8077
)

// ProvisioningDeploy reports whether the platform renders the bundled provisioning-rabbitmq
// service: provisioning.mode=deploy with the deploy block present, a RabbitMQ broker (the
// service is RabbitMQ-specific), AND a platform version whose bundle SHIPS provisioning
// (HasProvisioning). It is the single predicate the builder, the render set, and Core's
// rewiring share so they never drift.
//
// The version gate makes the same CR portable across versions: provisioning-rabbitmq is a
// 2.18.0 component, so a 2.17.0 platform that sets provisioning.mode=deploy simply does not
// render it (the operator surfaces a non-fatal condition rather than referencing a component
// that release never had). The brokerType gate is belt-and-suspenders: a PlatformSpec CEL rule
// already rejects mode=deploy with a non-rabbitmq broker at admission, but gating the render
// here too means a stale object (or a direct builder call in tests) can never produce a
// broker-incompatible service.
func ProvisioningDeploy(p *otilmv1alpha1.Platform) bool {
	pr := p.Spec.Provisioning
	return pr != nil && pr.Mode == provisioningModeDeploy && pr.Deploy != nil &&
		brokerIsRabbitMQ(p) && resolveBundle(p).HasProvisioning
}

// provisioningModeDeploy / provisioningModeExternal are the spec.provisioning.mode
// literals.
const (
	provisioningModeDeploy   = "deploy"
	provisioningModeExternal = "external"
)

// brokerIsRabbitMQ reports whether the platform's messaging broker is RabbitMQ (the empty
// brokerType defaults to rabbitmq, matching the CRD default), so an out-of-the-box deploy is
// accepted.
func brokerIsRabbitMQ(p *otilmv1alpha1.Platform) bool {
	bt := p.Spec.Messaging.BrokerType
	return bt == "" || bt == "rabbitmq"
}

// ResolveProvisioning resolves the provisioning-rabbitmq component's render model when
// provisioning.mode=deploy, or the zero Component when provisioning is external (the
// render set never appends it then — see RenderPlatformBase). Env-var names, the bootstrap
// exchange/response-queue defaults, and the bootstrap Secret-key defaults come from the
// version bundle's provisioning wiring; the broker connection (host/port/vhost + the
// provisioner/proxy credentials) comes from the mode-agnostic messaging connection.
//
// The render wires the broker host/port from the messaging ConfigMap, the provisioner
// credentials for the admin AMQP connection, the proxy credentials embedded into issued
// JWTs, the bootstrap exchange/queue, and the API key + JWT signing key by secretKeyRef.
// It is SCC-clean (the common
// BuildDeployment path hardens it) with a read-only root (Spring Boot service whose only
// writable path is the in-memory /tmp ephemeral volume), and honours the full ComponentSpec
// override surface via applyComponentSpec.
func ResolveProvisioning(p *otilmv1alpha1.Platform) common.Component {
	b := resolveBundle(p)
	w := b.Wiring
	pw := w.Provisioning
	d := p.Spec.Provisioning.Deploy

	image, policy := common.ResolveImage(b.Lookup, "provisioning", p.Spec.Common.Image, d.Image)

	logLevel := p.Spec.Common.Logging.Level
	if logLevel == "" {
		logLevel = defaultLogLevel
	}

	mq := ResolveMessagingConnection(p)

	// Inline (non-secret) env: the port, log level, broker vhost, the proxy AMQP
	// URL proxies connect with, the bootstrap exchange/response-queue, and the X-API-Key
	// toggle (always enabled — the operator always wires the API key by reference).
	env := []common.EnvPair{
		{Name: pw.PortEnv, Value: fmt.Sprintf("%d", provisioningPort)},
		{Name: pw.LoggingLevelEnv, Value: logLevel},
		{Name: pw.BrokerVHostEnv, Value: mq.VirtualHost},
		{Name: pw.ProxyAMQPURLEnv, Value: fmt.Sprintf("amqp://%s:%d", mq.Host, mq.Port)},
		{Name: pw.ProxyExchangeEnv, Value: provisioningExchange(p, pw)},
		{Name: pw.ResponseQueueEnv, Value: provisioningResponseQueue(p, pw)},
		{Name: pw.SecurityEnabledEnv, Value: strconv.FormatBool(true)},
	}

	c := common.Component{
		Name: provisioningName, Instance: p.Name, Namespace: p.Namespace,
		Image: image, PullPolicy: policy, PullSecrets: common.MergePullSecrets(p.Spec.Common.Image.PullSecrets, d.Image.PullSecrets),
		Replicas: 1, Port: provisioningPort, ServiceType: corev1.ServiceTypeClusterIP,
		Command: imageCommand(p.Spec.Common.Image, d.Image),
		Args:    imageArgs(p.Spec.Common.Image, d.Image),
		Env:     env,
		// Broker host/port come from the shared messaging ConfigMap (configMapKeyRef), exactly
		// like Core/scheduler — so a managed broker's published host/port flow through too.
		ConfigMapEnv: []common.ConfigMapEnvRef{
			{EnvVar: pw.BrokerHostEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingHostKey},
			{EnvVar: pw.BrokerPortEnv, ConfigMapName: w.MessagingConfigMapName, ConfigMapKey: w.MessagingPortKey},
		},
		SecretEnv:      provisioningSecretEnv(p, pw, mq),
		InitContainers: []corev1.Container{waitForMessagingInitContainer(p)},
		Volumes:        []corev1.Volume{ephemeralVolume()},
		VolumeMounts:   []corev1.VolumeMount{{Name: ephemeralVolumeName, MountPath: tmpMountPath}},
		Probes:         provisioningProbes(),
		// Read-only root filesystem: ENABLED. provisioning-rabbitmq is a Spring Boot (JVM)
		// service; its only writable path is the in-memory /tmp ephemeral volume. This is
		// asserted from the image's expected security posture, not yet observed on Kind.
		ReadOnlyRootFilesystem: readOnlyRootFS(),
	}

	applyComponentSpec(p, &c, d.ComponentSpec)
	return c
}

// provisioningSecretEnv lists the provisioning service's secret-backed env vars, all
// sourced via secretKeyRef from user-provided Secrets — the operator never copies the
// values into the rendered objects:
//
//   - the broker provisioner credentials (RABBITMQ_USERNAME/PASSWORD): the admin user the
//     service authenticates as to manage queues/exchanges;
//   - the proxy credentials (PROXY_RABBITMQ_USERNAME/PASSWORD): embedded into the per-proxy
//     JWTs the service issues;
//   - the API key (SECURITY_API_KEY) and the JWT signing key (TOKEN_SIGNING_KEY): from the
//     deploy bootstrap Secret.
//
// The broker/proxy credentials default to the platform messaging connection (managed: the
// Topology-generated provisioner/proxy Secrets; external: spec.messaging.credentials) when
// the deploy block does not override them. A credential ref is omitted only when no Secret
// name resolves at all (an admission-validated misconfiguration, kept render-total).
func provisioningSecretEnv(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring, mq MessagingConnection) []common.SecretEnvRef {
	d := p.Spec.Provisioning.Deploy
	var refs []common.SecretEnvRef

	// Broker (provisioner) credentials: the deploy override wins; else the platform's
	// provisioner Secret (managed) or messaging credentials (external).
	if name, userKey, passKey := provisionerBrokerCreds(p, pw, mq); name != "" {
		refs = append(refs,
			common.SecretEnvRef{EnvVar: pw.BrokerCred.UsernameEnv, SecretName: name, SecretKey: userKey},
			common.SecretEnvRef{EnvVar: pw.BrokerCred.PasswordEnv, SecretName: name, SecretKey: passKey},
		)
	}
	// Proxy credentials embedded into issued JWTs.
	if name, userKey, passKey := proxyBrokerCreds(p, pw, mq); name != "" {
		refs = append(refs,
			common.SecretEnvRef{EnvVar: pw.ProxyCred.UsernameEnv, SecretName: name, SecretKey: userKey},
			common.SecretEnvRef{EnvVar: pw.ProxyCred.PasswordEnv, SecretName: name, SecretKey: passKey},
		)
	}
	// Bootstrap API key + JWT signing key from the deploy bootstrap Secret.
	refs = append(refs,
		common.SecretEnvRef{EnvVar: pw.APIKey.Env, SecretName: d.BootstrapSecretRef, SecretKey: provisioningAPIKeyKey(p, pw)},
		common.SecretEnvRef{EnvVar: pw.TokenSigningKey.Env, SecretName: d.BootstrapSecretRef, SecretKey: provisioningTokenSigningKeyKey(p, pw)},
	)
	return refs
}

// provisionerBrokerCreds resolves the Secret name + username/password keys for the broker
// provisioner (admin) connection: the deploy.provisionerCredentials override when set, else
// the messaging connection's provisioner Secret (managed) or main credentials Secret
// (external). The in-Secret keys are pick(override key, wiring default).
func provisionerBrokerCreds(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring, mq MessagingConnection) (name, userKey, passKey string) {
	d := p.Spec.Provisioning.Deploy
	fallback := mq.ProvisioningCredentialsSecretName // managed provisioner Secret
	if fallback == "" {
		fallback = mq.CredentialsSecretName // external: the platform messaging Secret
	}
	return resolveCredsRef(d.ProvisionerCredentials, fallback, pw.BrokerCred)
}

// proxyBrokerCreds resolves the Secret name + username/password keys for the proxy user the
// service embeds into issued JWTs: the deploy.proxyCredentials override when set, else the
// messaging connection's proxy Secret (managed) or main credentials Secret (external).
func proxyBrokerCreds(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring, mq MessagingConnection) (name, userKey, passKey string) {
	d := p.Spec.Provisioning.Deploy
	fallback := mq.ProxyCredentialsSecretName // managed proxy Secret
	if fallback == "" {
		fallback = mq.CredentialsSecretName // external: the platform messaging Secret
	}
	return resolveCredsRef(d.ProxyCredentials, fallback, pw.ProxyCred)
}

// resolveCredsRef picks the effective credentials Secret name and in-Secret keys: the
// caller-supplied CredentialsRef overrides both the name (when its SecretRef is set) and the
// keys (when set), falling back to the given default name and the wiring-profile default
// keys. It keeps the override semantics identical for the provisioner and proxy refs.
func resolveCredsRef(ref *otilmv1alpha1.CredentialsRef, fallbackName string, def bom.CredentialEnv) (name, userKey, passKey string) {
	name = fallbackName
	userKey = def.UsernameKey
	passKey = def.PasswordKey
	if ref != nil {
		if ref.SecretRef != "" {
			name = ref.SecretRef
		}
		if ref.UsernameKey != "" {
			userKey = ref.UsernameKey
		}
		if ref.PasswordKey != "" {
			passKey = ref.PasswordKey
		}
	}
	return name, userKey, passKey
}

// provisioningAPIKeyKey resolves the in-Secret key for the deploy bootstrap API key:
// deploy.apiKeyKey when set, else the wiring default ("securityApiKey").
func provisioningAPIKeyKey(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring) string {
	if d := p.Spec.Provisioning.Deploy; d != nil && d.APIKeyKey != "" {
		return d.APIKeyKey
	}
	return pw.APIKey.Key
}

// provisioningTokenSigningKeyKey resolves the in-Secret key for the deploy bootstrap JWT
// signing key: deploy.tokenSigningKeyKey when set, else the wiring default ("tokenSigningKey").
func provisioningTokenSigningKeyKey(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring) string {
	if d := p.Spec.Provisioning.Deploy; d != nil && d.TokenSigningKeyKey != "" {
		return d.TokenSigningKeyKey
	}
	return pw.TokenSigningKey.Key
}

// provisioningExchange resolves the bootstrap proxy exchange: deploy.exchange when set, else
// the bundle default ("czertainly-proxy").
func provisioningExchange(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring) string {
	if d := p.Spec.Provisioning.Deploy; d != nil && d.Exchange != "" {
		return d.Exchange
	}
	return pw.DefaultExchange
}

// provisioningResponseQueue resolves the bootstrap response queue: deploy.responseQueue when
// set, else the bundle default ("core").
func provisioningResponseQueue(p *otilmv1alpha1.Platform, pw bom.ProvisioningWiring) string {
	if d := p.Spec.Provisioning.Deploy; d != nil && d.ResponseQueue != "" {
		return d.ResponseQueue
	}
	return pw.DefaultResponseQueue
}

// provisioningProbes returns the provisioning service's readiness and startup probes against
// /actuator/health, with the standard probe parameters. Liveness is intentionally omitted.
func provisioningProbes() common.Probes {
	return httpProbesOn("/actuator/health", 15, "/actuator/health", provisioningPort)
}
