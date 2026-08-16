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
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// deployProvisioningPlatform returns a base platform with provisioning.mode=deploy and
// the bootstrap Secret referenced (the minimal valid deploy config).
func deployProvisioningPlatform() *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		Mode: "deploy",
		Deploy: &otilmv1alpha1.ProvisioningDeploySpec{
			BootstrapSecretRef: testProvBootstrap,
		},
	}
	return p
}

// renderDeployments returns the rendered Deployments keyed by name, and the rendered
// Services keyed by name.
func renderDeployments(p *otilmv1alpha1.Platform) (map[string]*appsv1.Deployment, map[string]*corev1.Service) {
	deps := map[string]*appsv1.Deployment{}
	svcs := map[string]*corev1.Service{}
	for _, o := range RenderPlatform(p) {
		switch v := o.(type) {
		case *appsv1.Deployment:
			deps[v.Name] = v
		case *corev1.Service:
			svcs[v.Name] = v
		}
	}
	return deps, svcs
}

// TestProvisioningExternalRendersNoComponent asserts that the default (external) mode renders
// NO provisioning component, and Core's PROVISIONING_API_URL is the configured external URL
// (today's consume-only behaviour, unchanged).
func TestProvisioningExternalRendersNoComponent(t *testing.T) {
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		// Mode defaults to external (the CRD default); set explicitly for clarity.
		Mode: "external", APIURL: testProvURL, APIKeySecretRef: "ext-prov-secret",
	}

	assert.False(t, ProvisioningDeploy(p), "external mode must not deploy the provisioning service")

	deps, svcs := renderDeployments(p)
	_, hasDep := deps[provisioningName]
	_, hasSvc := svcs[provisioningName]
	assert.False(t, hasDep, "external mode must render no provisioning Deployment")
	assert.False(t, hasSvc, "external mode must render no provisioning Service")

	// Core points at the EXTERNAL apiURL, unchanged.
	core := ResolveCore(p)
	w := bom.Wiring()
	url, ok := envValue(core.Env, w.ProvisioningURLEnv)
	require.True(t, ok, "Core must carry PROVISIONING_API_URL")
	assert.Equal(t, testProvURL, url, "external mode: Core uses the configured apiURL")
	// And Core's API key comes from the caller's Secret with the external default key.
	ref, ok := secretEnvFor(core.SecretEnv, w.ProvisioningAPIKey.Env)
	require.True(t, ok, "Core must carry PROVISIONING_API_KEY by reference")
	assert.Equal(t, "ext-prov-secret", ref.SecretName)
	assert.Equal(t, w.ProvisioningAPIKey.Key, ref.SecretKey, "external default in-Secret key")
}

// TestProvisioningExternalUnsetUnchanged asserts the out-of-the-box CR (no provisioning block
// at all) renders no provisioning component and leaves Core's PROVISIONING_API_URL empty.
func TestProvisioningExternalUnsetUnchanged(t *testing.T) {
	p := basePlatform() // no Provisioning block
	assert.False(t, ProvisioningDeploy(p))
	deps, _ := renderDeployments(p)
	_, hasDep := deps[provisioningName]
	assert.False(t, hasDep, "no provisioning block must render no provisioning Deployment")

	url, _ := envValue(ResolveCore(p).Env, bom.Wiring().ProvisioningURLEnv)
	assert.Empty(t, url, "no provisioning configured: PROVISIONING_API_URL is empty")
}

// TestProvisioningDeployRendersDeploymentAndService asserts mode=deploy renders the
// provisioning Deployment + Service + ServiceAccount with the expected name/port/image.
func TestProvisioningDeployRendersDeploymentAndService(t *testing.T) {
	p := deployProvisioningPlatform()
	require.True(t, ProvisioningDeploy(p), "deploy mode with rabbitmq broker must deploy the service")

	deps, svcs := renderDeployments(p)
	dep, ok := deps[provisioningName]
	require.True(t, ok, "deploy mode must render the provisioning Deployment")
	svc, ok := svcs[provisioningName]
	require.True(t, ok, "deploy mode must render the provisioning Service")

	// Image resolves from the bundle (registry/repository/name:tag). Derive the expected ref
	// from the BOM so this stays correct across the provisioning tag flip (develop-latest ->
	// 1.0.0 on release) — it asserts the wiring resolves the bundle's coordinates, not a frozen
	// literal tag.
	bImg, ok := bom.Lookup("provisioning")
	require.True(t, ok, "the bundle must carry the provisioning image coordinates")
	img := dep.Spec.Template.Spec.Containers[0].Image
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/"+bImg.Name+":"+bImg.Tag, img,
		"provisioning image must resolve from the bundle")

	// Service exposes the 8077 port.
	require.NotEmpty(t, svc.Spec.Ports)
	assert.Equal(t, provisioningPort, svc.Spec.Ports[0].Port, "Service port must be 8077")

	// Container port matches.
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers[0].Ports)
	assert.Equal(t, provisioningPort, dep.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
}

// TestProvisioningDeployBrokerWiring asserts the provisioning service is wired to the broker:
// the host/port via the messaging ConfigMap, the vhost + proxy AMQP URL inline, the bootstrap
// exchange/response-queue, and the broker/proxy credentials by secretKeyRef.
func TestProvisioningDeployBrokerWiring(t *testing.T) {
	p := deployProvisioningPlatform()
	c := ResolveProvisioning(p)
	w := bom.Wiring()
	pw := w.Provisioning

	// Broker host/port come from the shared messaging ConfigMap (configMapKeyRef), not inline.
	cmHost, ok := configMapEnvFor(c.ConfigMapEnv, pw.BrokerHostEnv)
	require.True(t, ok, "RABBITMQ_HOST must be a configMapKeyRef")
	assert.Equal(t, w.MessagingConfigMapName, cmHost.ConfigMapName)
	assert.Equal(t, w.MessagingHostKey, cmHost.ConfigMapKey)
	cmPort, ok := configMapEnvFor(c.ConfigMapEnv, pw.BrokerPortEnv)
	require.True(t, ok, "RABBITMQ_PORT must be a configMapKeyRef")
	assert.Equal(t, w.MessagingPortKey, cmPort.ConfigMapKey)

	// vhost + proxy AMQP URL inline, from the external messaging coordinates.
	vhost, ok := envValue(c.Env, pw.BrokerVHostEnv)
	require.True(t, ok)
	assert.Equal(t, "ilm", vhost, "vhost from spec.messaging.virtualHost")
	amqp, ok := envValue(c.Env, pw.ProxyAMQPURLEnv)
	require.True(t, ok)
	assert.Equal(t, "amqp://mq.example.com:5672", amqp, "proxy AMQP URL from the broker host/port")

	// Bootstrap exchange/response-queue default from the bundle.
	exch, _ := envValue(c.Env, pw.ProxyExchangeEnv)
	assert.Equal(t, testExchangeIlmProxy, exch)
	rq, _ := envValue(c.Env, pw.ResponseQueueEnv)
	assert.Equal(t, "core", rq)

	// X-API-Key feature toggle is on (the operator always wires the key by reference).
	sec, _ := envValue(c.Env, pw.SecurityEnabledEnv)
	assert.Equal(t, "true", sec)

	// Broker provisioner + proxy credentials: by secretKeyRef from the platform messaging
	// Secret (external default), never inline.
	for _, env := range []string{pw.BrokerCred.UsernameEnv, pw.BrokerCred.PasswordEnv, pw.ProxyCred.UsernameEnv, pw.ProxyCred.PasswordEnv} {
		ref, ok := secretEnvFor(c.SecretEnv, env)
		require.Truef(t, ok, "%s must be wired by secretKeyRef", env)
		assert.Equalf(t, "mq-creds", ref.SecretName, "%s sources the platform messaging Secret by default", env)
		_, inline := envValue(c.Env, env)
		assert.Falsef(t, inline, "%s must NOT be an inline value", env)
	}
}

// TestProvisioningDeployBootstrapSecretsByRef asserts the API key and JWT signing key are
// sourced via secretKeyRef from the deploy bootstrap Secret — never inlined.
func TestProvisioningDeployBootstrapSecretsByRef(t *testing.T) {
	p := deployProvisioningPlatform()
	c := ResolveProvisioning(p)
	pw := bom.Wiring().Provisioning

	apiKey, ok := secretEnvFor(c.SecretEnv, pw.APIKey.Env)
	require.True(t, ok, "SECURITY_API_KEY must be a secretKeyRef")
	assert.Equal(t, testProvBootstrap, apiKey.SecretName)
	assert.Equal(t, "securityApiKey", apiKey.SecretKey, "default API-key in-Secret key")

	signing, ok := secretEnvFor(c.SecretEnv, pw.TokenSigningKey.Env)
	require.True(t, ok, "TOKEN_SIGNING_KEY must be a secretKeyRef")
	assert.Equal(t, testProvBootstrap, signing.SecretName)
	assert.Equal(t, "tokenSigningKey", signing.SecretKey, "default signing-key in-Secret key")
}

// TestProvisioningDeployCoreRewired asserts Core's PROVISIONING_API_URL points at the
// in-cluster provisioning Service, and Core's PROVISIONING_API_KEY is sourced from the deploy
// bootstrap Secret with the bootstrap API-key key — not the external apiURL/secret.
func TestProvisioningDeployCoreRewired(t *testing.T) {
	p := deployProvisioningPlatform()
	// Even if a stale external apiURL/secret is also present, deploy mode must win.
	p.Spec.Provisioning.APIURL = "https://ignored-external.example.com"
	p.Spec.Provisioning.APIKeySecretRef = "ignored-external-secret"

	core := ResolveCore(p)
	w := bom.Wiring()

	url, ok := envValue(core.Env, w.ProvisioningURLEnv)
	require.True(t, ok)
	assert.Equal(t, "http://provisioning-rabbitmq:8077", url,
		"deploy mode: Core points at the deployed provisioning Service, not the external apiURL")

	ref, ok := secretEnvFor(core.SecretEnv, w.ProvisioningAPIKey.Env)
	require.True(t, ok, "Core must carry PROVISIONING_API_KEY by reference")
	assert.Equal(t, testProvBootstrap, ref.SecretName, "deploy mode: Core reads the bootstrap Secret")
	assert.Equal(t, "securityApiKey", ref.SecretKey, "deploy mode: Core reads the bootstrap API-key key")
}

// TestProvisioningDeployInitContainerRunsAgainstService asserts the proxy-path
// provision-instance-queue init container is wired against the deployed service (it runs when
// proxy is enabled + provisioning configured, which deploy mode satisfies).
func TestProvisioningDeployInitContainerRunsAgainstService(t *testing.T) {
	p := deployProvisioningPlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}

	core := ResolveCore(p)
	var found *corev1.Container
	for i := range core.InitContainers {
		if core.InitContainers[i].Name == "provision-instance-queue" {
			found = &core.InitContainers[i]
		}
	}
	require.NotNil(t, found, "deploy mode + proxy: the provision-instance-queue init container must run")

	// Its PROVISIONING_API_URL is the deployed Service; its API key is a secretKeyRef into
	// the bootstrap Secret (never inline).
	w := bom.Wiring()
	var urlVal string
	var keyRef *corev1.SecretKeySelector
	for _, e := range found.Env {
		switch e.Name {
		case w.ProvisioningURLEnv:
			urlVal = e.Value
		case w.ProvisioningAPIKey.Env:
			require.NotNil(t, e.ValueFrom, "API key must be valueFrom")
			keyRef = e.ValueFrom.SecretKeyRef
		}
	}
	assert.Equal(t, "http://provisioning-rabbitmq:8077", urlVal)
	require.NotNil(t, keyRef, "the init container's API key must be a secretKeyRef")
	assert.Equal(t, testProvBootstrap, keyRef.Name)
	assert.Equal(t, "securityApiKey", keyRef.Key)
}

// TestProvisioningDeploySCCClean asserts every provisioning container (init + main) is
// SCC-hardened and the main container runs with a read-only root.
func TestProvisioningDeploySCCClean(t *testing.T) {
	p := deployProvisioningPlatform()
	deps, _ := renderDeployments(p)
	dep := deps[provisioningName]
	require.NotNil(t, dep)

	ps := dep.Spec.Template.Spec
	all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	require.GreaterOrEqual(t, len(all), 2, "expect at least the wait-for-messaging init + main container")
	for _, c := range all {
		sc := c.SecurityContext
		require.NotNilf(t, sc, "container %q must carry a SecurityContext", c.Name)
		require.NotNil(t, sc.RunAsNonRoot)
		assert.Truef(t, *sc.RunAsNonRoot, "container %q must run as non-root", c.Name)
		require.NotNil(t, sc.AllowPrivilegeEscalation)
		assert.Falsef(t, *sc.AllowPrivilegeEscalation, "container %q must not allow privilege escalation", c.Name)
		require.NotNil(t, sc.Capabilities)
		assert.Equalf(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "container %q must drop ALL caps", c.Name)
		require.NotNil(t, sc.SeccompProfile)
		assert.Equalf(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "container %q seccomp", c.Name)
		assert.Nilf(t, sc.RunAsUser, "container %q must not pin a UID", c.Name)
	}
	// Main container read-only root.
	main := ps.Containers[0]
	require.NotNil(t, main.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *main.SecurityContext.ReadOnlyRootFilesystem, "provisioning main container must have a read-only root")
}

// TestProvisioningDeployNoSecretLeakage asserts no secret VALUE is ever inlined anywhere in
// the rendered provisioning objects (all sensitive values are secretKeyRef).
func TestProvisioningDeployNoSecretLeakage(t *testing.T) {
	p := deployProvisioningPlatform()
	deps, _ := renderDeployments(p)
	dep := deps[provisioningName]
	require.NotNil(t, dep)

	ps := dep.Spec.Template.Spec
	all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	for _, c := range all {
		for _, e := range c.Env {
			// Every credential / key / api-key env must be valueFrom (secretKeyRef), never an
			// inline Value.
			name := strings.ToUpper(e.Name)
			if strings.Contains(name, "PASSWORD") || strings.Contains(name, "USERNAME") ||
				strings.Contains(name, "API_KEY") && name != "SECURITY_API_KEY_ENABLED" ||
				strings.Contains(name, "SIGNING_KEY") {
				assert.Emptyf(t, e.Value, "env %q must not carry an inline value (secret leakage)", e.Name)
			}
		}
	}
}

// TestProvisioningDeployComponentSpecOverrides asserts the deploy block's embedded
// ComponentSpec is honoured (replicas, an extra env var, an extra secret ref).
func TestProvisioningDeployComponentSpecOverrides(t *testing.T) {
	p := deployProvisioningPlatform()
	replicas := int32(3)
	p.Spec.Provisioning.Deploy.ComponentSpec = otilmv1alpha1.ComponentSpec{
		Replicas: &replicas,
		Env:      []otilmv1alpha1.EnvVar{{Name: "EXTRA_FLAG", Value: "on"}},
	}
	// Keep the required bootstrap ref (ComponentSpec is a separate embed).
	p.Spec.Provisioning.Deploy.BootstrapSecretRef = testProvBootstrap

	c := ResolveProvisioning(p)
	assert.Equal(t, int32(3), c.Replicas, "ComponentSpec.Replicas override must apply")
	v, ok := envValue(c.Env, "EXTRA_FLAG")
	require.True(t, ok, "ComponentSpec.Env override must apply")
	assert.Equal(t, "on", v)
}

// TestProvisioningDeployOverridesBootstrapKeysAndTopology asserts the deploy block can
// override the bootstrap in-Secret keys and the exchange/response-queue.
func TestProvisioningDeployOverridesBootstrapKeysAndTopology(t *testing.T) {
	p := deployProvisioningPlatform()
	d := p.Spec.Provisioning.Deploy
	d.APIKeyKey = testAPIKey
	d.TokenSigningKeyKey = "my-signing-key"
	d.Exchange = "custom-proxy-exchange"
	d.ResponseQueue = "custom-response"

	c := ResolveProvisioning(p)
	pw := bom.Wiring().Provisioning

	apiKey, _ := secretEnvFor(c.SecretEnv, pw.APIKey.Env)
	assert.Equal(t, testAPIKey, apiKey.SecretKey)
	signing, _ := secretEnvFor(c.SecretEnv, pw.TokenSigningKey.Env)
	assert.Equal(t, "my-signing-key", signing.SecretKey)
	exch, _ := envValue(c.Env, pw.ProxyExchangeEnv)
	assert.Equal(t, "custom-proxy-exchange", exch)
	rq, _ := envValue(c.Env, pw.ResponseQueueEnv)
	assert.Equal(t, "custom-response", rq)

	// And Core (deploy) reads the overridden API-key key from the bootstrap Secret.
	core := ResolveCore(p)
	ref, _ := secretEnvFor(core.SecretEnv, bom.Wiring().ProvisioningAPIKey.Env)
	assert.Equal(t, testAPIKey, ref.SecretKey, "Core's bootstrap API-key key tracks deploy.apiKeyKey")
}

// TestProvisioningDeployOverridesBrokerProxyCreds asserts the deploy block can point the
// broker provisioner and proxy credentials at distinct Secrets/keys.
func TestProvisioningDeployOverridesBrokerProxyCreds(t *testing.T) {
	p := deployProvisioningPlatform()
	d := p.Spec.Provisioning.Deploy
	d.ProvisionerCredentials = &otilmv1alpha1.CredentialsRef{
		SecretRef: "broker-admin", UsernameKey: "u", PasswordKey: "pw",
	}
	d.ProxyCredentials = &otilmv1alpha1.CredentialsRef{SecretRef: "proxy-user"}

	c := ResolveProvisioning(p)
	pw := bom.Wiring().Provisioning

	bu, _ := secretEnvFor(c.SecretEnv, pw.BrokerCred.UsernameEnv)
	assert.Equal(t, "broker-admin", bu.SecretName)
	assert.Equal(t, "u", bu.SecretKey, "provisioner username key override")
	bp, _ := secretEnvFor(c.SecretEnv, pw.BrokerCred.PasswordEnv)
	assert.Equal(t, "pw", bp.SecretKey, "provisioner password key override")

	xu, _ := secretEnvFor(c.SecretEnv, pw.ProxyCred.UsernameEnv)
	assert.Equal(t, "proxy-user", xu.SecretName)
	assert.Equal(t, "username", xu.SecretKey, "proxy username key defaults when not overridden")
}

// TestProvisioningDeployManagedBrokerCreds asserts that with a MANAGED broker the provisioner
// and proxy credentials default to the Topology-generated per-user Secrets.
func TestProvisioningDeployManagedBrokerCreds(t *testing.T) {
	p := deployProvisioningPlatform()
	p.Spec.Messaging = otilmv1alpha1.MessagingSpec{
		Mode: "managed", BrokerType: "rabbitmq",
		Managed: &otilmv1alpha1.ManagedMessagingSpec{
			Replicas: 1,
			Storage:  otilmv1alpha1.StorageSpec{Size: "1Gi"},
		},
	}
	require.True(t, ProvisioningDeploy(p), "managed rabbitmq broker still allows deploy")

	c := ResolveProvisioning(p)
	pw := bom.Wiring().Provisioning

	bu, ok := secretEnvFor(c.SecretEnv, pw.BrokerCred.UsernameEnv)
	require.True(t, ok)
	assert.Equal(t, managedUserCredentialsSecretName(p, bom.MessagingUserProvisioner), bu.SecretName,
		"managed: provisioner creds from the Topology provisioner-user Secret")
	xu, ok := secretEnvFor(c.SecretEnv, pw.ProxyCred.UsernameEnv)
	require.True(t, ok)
	assert.Equal(t, managedUserCredentialsSecretName(p, bom.MessagingUserProxy), xu.SecretName,
		"managed: proxy creds from the Topology proxy-user Secret")
}

// TestProvisioningDeployPrunedOnFlipBackToExternal asserts the provisioning children are
// rendered under deploy and ABSENT when the mode flips back to external — so the controller's
// label+owner prune (which lists Deployment/Service/ServiceAccount) reclaims them.
func TestProvisioningDeployPrunedOnFlipBackToExternal(t *testing.T) {
	p := deployProvisioningPlatform()
	deps, svcs := renderDeployments(p)
	_, hasDep := deps[provisioningName]
	_, hasSvc := svcs[provisioningName]
	require.True(t, hasDep, "deploy: provisioning Deployment present")
	require.True(t, hasSvc, "deploy: provisioning Service present")

	// Flip back to external — the component must disappear from the desired render set.
	p.Spec.Provisioning.Mode = "external"
	p.Spec.Provisioning.APIURL = testProvURL
	deps2, svcs2 := renderDeployments(p)
	_, hasDep2 := deps2[provisioningName]
	_, hasSvc2 := svcs2[provisioningName]
	assert.False(t, hasDep2, "external: provisioning Deployment de-rendered (pruned)")
	assert.False(t, hasSvc2, "external: provisioning Service de-rendered (pruned)")
}

// TestProvisioningDeployGatedOnRabbitMQ asserts the render is gated on a RabbitMQ broker: a
// servicebus broker yields no provisioning component even with mode=deploy (the CEL rule
// rejects this at admission; the builder no-ops as belt-and-suspenders).
func TestProvisioningDeployGatedOnRabbitMQ(t *testing.T) {
	p := deployProvisioningPlatform()
	p.Spec.Messaging.BrokerType = "servicebus"
	assert.False(t, ProvisioningDeploy(p), "deploy must no-op for a non-rabbitmq broker")

	deps, _ := renderDeployments(p)
	_, hasDep := deps[provisioningName]
	assert.False(t, hasDep, "servicebus broker must render no provisioning Deployment")
}
