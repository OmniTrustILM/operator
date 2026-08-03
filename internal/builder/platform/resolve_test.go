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
	"strconv"
	"strings"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// envValue returns the inline env value for name, and whether it was found.
func envValue(env []common.EnvPair, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// secretEnvFor returns the SecretEnvRef whose EnvVar matches name, and whether it was found.
func secretEnvFor(refs []common.SecretEnvRef, name string) (common.SecretEnvRef, bool) {
	for _, r := range refs {
		if r.EnvVar == name {
			return r, true
		}
	}
	return common.SecretEnvRef{}, false
}

func basePlatform() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: testNamespace},
		Spec: otilmv1alpha1.PlatformSpec{
			Common: otilmv1alpha1.CommonSpec{
				Image: otilmv1alpha1.ImageSpec{Registry: "hub.omnitrustregistry.com", Repository: "ilm"},
			},
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "db.example.com", Port: 5432, Name: "ilmdb",
				Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testDBCreds},
			},
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", BrokerType: "rabbitmq", Host: testMQHost, Port: 5672,
				VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testMQCreds},
			},
		},
	}
}

func TestResolveCoreBasics(t *testing.T) {
	c := ResolveCore(basePlatform())
	assert.Equal(t, "core", c.Name)
	assert.Equal(t, testNamespace, c.Namespace, "namespace should propagate from the CR")
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas, "default replicas when spec.core.replicas is nil")
}

// TestDBMigratingComponentsUseRecreate locks that Core and auth — both run DB schema
// migrations at startup — use the Recreate Deployment strategy, so a roll never runs two
// migrating pods concurrently (which would race through the transaction pooler + corrupt the
// schema).
func TestDBMigratingComponentsUseRecreate(t *testing.T) {
	assert.True(t, ResolveCore(basePlatform()).Recreate, "core runs DB migrations -> Recreate")
	assert.True(t, ResolveAuth(basePlatform()).Recreate, "auth runs DB migrations -> Recreate")
}

func TestResolveCoreDatabaseURLFromWiringProfile(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()
	got, ok := envValue(c.Env, w.DatabaseURLEnv)
	require.True(t, ok, "database URL env (%s) must be present", w.DatabaseURLEnv)
	assert.Equal(t, "JDBC_URL", w.DatabaseURLEnv, "guards against silent wiring-profile drift")
	assert.Equal(t, "jdbc:postgresql://db.example.com:5432/ilmdb?characterEncoding=UTF-8", got)
}

func TestResolveCoreMessagingVHostInline(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()
	vhost, ok := envValue(c.Env, w.MessagingVHostEnv)
	require.True(t, ok)
	assert.Equal(t, "ilm", vhost)
}

// configMapEnvFor returns the ConfigMapEnvRef whose EnvVar matches name, and whether it was found.
func configMapEnvFor(refs []common.ConfigMapEnvRef, name string) (common.ConfigMapEnvRef, bool) {
	for _, r := range refs {
		if r.EnvVar == name {
			return r, true
		}
	}
	return common.ConfigMapEnvRef{}, false
}

func TestResolveCoreBrokerHostPortFromConfigMap(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()

	host, ok := configMapEnvFor(c.ConfigMapEnv, w.MessagingHostEnv)
	require.True(t, ok, "BROKER_HOST must be configmap-backed")
	assert.Equal(t, w.MessagingConfigMapName, host.ConfigMapName)
	assert.Equal(t, w.MessagingHostKey, host.ConfigMapKey)

	port, ok := configMapEnvFor(c.ConfigMapEnv, w.MessagingPortEnv)
	require.True(t, ok, "BROKER_PORT must be configmap-backed")
	assert.Equal(t, w.MessagingConfigMapName, port.ConfigMapName)
	assert.Equal(t, w.MessagingPortKey, port.ConfigMapKey)

	// The broker host/port must NOT also appear as inline env values.
	_, inlineHost := envValue(c.Env, w.MessagingHostEnv)
	_, inlinePort := envValue(c.Env, w.MessagingPortEnv)
	assert.False(t, inlineHost, "BROKER_HOST must not be inline")
	assert.False(t, inlinePort, "BROKER_PORT must not be inline")
}

func TestResolveCoreDatabaseCredentialsAreSecretBacked(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()

	user, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.UsernameEnv)
	require.True(t, ok, "db username env must be secret-backed")
	assert.Equal(t, testDBCreds, user.SecretName)
	assert.Equal(t, w.DatabaseCred.UsernameKey, user.SecretKey)

	pass, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.PasswordEnv)
	require.True(t, ok, "db password env must be secret-backed")
	assert.Equal(t, testDBCreds, pass.SecretName)
	assert.Equal(t, w.DatabaseCred.PasswordKey, pass.SecretKey)

	// Credentials must NEVER appear as inline env values.
	_, inlineUser := envValue(c.Env, w.DatabaseCred.UsernameEnv)
	_, inlinePass := envValue(c.Env, w.DatabaseCred.PasswordEnv)
	assert.False(t, inlineUser, "db username must not be inline")
	assert.False(t, inlinePass, "db password must not be inline")
}

func TestResolveCoreMessagingCredentialsAreSecretBacked(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()

	user, ok := secretEnvFor(c.SecretEnv, w.MessagingCred.UsernameEnv)
	require.True(t, ok, "messaging username env must be secret-backed")
	assert.Equal(t, testMQCreds, user.SecretName)
	assert.Equal(t, w.MessagingCred.UsernameKey, user.SecretKey)

	pass, ok := secretEnvFor(c.SecretEnv, w.MessagingCred.PasswordEnv)
	require.True(t, ok, "messaging password env must be secret-backed")
	assert.Equal(t, testMQCreds, pass.SecretName)
	assert.Equal(t, w.MessagingCred.PasswordKey, pass.SecretKey)

	_, inlineUser := envValue(c.Env, w.MessagingCred.UsernameEnv)
	_, inlinePass := envValue(c.Env, w.MessagingCred.PasswordEnv)
	assert.False(t, inlineUser, "messaging username must not be inline")
	assert.False(t, inlinePass, "messaging password must not be inline")
}

func TestResolveCoreNoSecretRefsMeansNoSecretEnv(t *testing.T) {
	p := basePlatform()
	p.Spec.Database.Credentials = nil
	p.Spec.Messaging.Credentials = nil
	c := ResolveCore(p)
	assert.Empty(t, c.SecretEnv, "no credentials.secretRef => no secret-backed env vars")
}

func TestResolveCoreReplicasOverride(t *testing.T) {
	p := basePlatform()
	three := int32(3)
	p.Spec.Core.Replicas = &three
	c := ResolveCore(p)
	assert.Equal(t, int32(3), c.Replicas)
}

func TestResolveCoreEnvOverridesAppendedLast(t *testing.T) {
	p := basePlatform()
	p.Spec.Core.Env = []otilmv1alpha1.EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
	}
	c := ResolveCore(p)
	v, ok := envValue(c.Env, "LOG_LEVEL")
	require.True(t, ok, "core.env entries must be applied")
	assert.Equal(t, "debug", v)
}

func TestResolveCoreImageResolvedFromBOM(t *testing.T) {
	// Shared registry/repository + bundle-provided core name/tag.
	c := ResolveCore(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.18.0", c.Image)
	assert.Equal(t, "IfNotPresent", string(c.PullPolicy), "default pull policy when unset")
}

func TestResolveCorePullSecretsPropagate(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.Image.PullSecrets = []string{"regcred"}
	c := ResolveCore(p)
	assert.Equal(t, []string{"regcred"}, c.PullSecrets)
}

func TestResolveCoreImageComponentOverride(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Common:    otilmv1alpha1.CommonSpec{Image: otilmv1alpha1.ImageSpec{Registry: "hub.example.com", Repository: "ilm"}},
			Core:      otilmv1alpha1.CoreSpec{ComponentSpec: otilmv1alpha1.ComponentSpec{Image: otilmv1alpha1.ImageSpec{Repository: "team", Tag: "9.9.9"}}},
			Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb"},
			Messaging: otilmv1alpha1.MessagingSpec{Host: "rabbit", VirtualHost: "ilm"},
		},
	}
	c := ResolveCore(p)
	// registry from shared, repository from component, name "core" from the bundle, tag from component
	assert.Equal(t, "hub.example.com/team/core:9.9.9", c.Image)
}

func TestDefaultImageRegistryUnsetResolvesToPublicRegistry(t *testing.T) {
	// An out-of-the-box CR with NO spec.image set at all: after DefaultImageRegistry,
	// only the shared registry is defaulted onto the CR (the repository resolves
	// lazily in ResolveImage and is never persisted), so Core resolves to the full
	// hub.omnitrustregistry.com/ilm/core:<tag> reference (name + tag from the bundle)
	// — not the bare "core:<tag>" Docker would treat as docker.io.
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb"},
			Messaging: otilmv1alpha1.MessagingSpec{Host: "rabbit", VirtualHost: "ilm"},
		},
	}
	// Before defaulting, the repository already resolves lazily from the bundle/bom
	// default — only the registry segment is missing.
	assert.Equal(t, "ilm/core:"+bom.DefaultVersion, ResolveCore(p).Image,
		"sanity: an undefaulted image resolves repository lazily, registry absent")

	DefaultImageRegistry(p)
	assert.Equal(t, bom.DefaultImageRegistry, p.Spec.Common.Image.Registry)
	assert.Equal(t, "", p.Spec.Common.Image.Repository,
		"DefaultImageRegistry no longer touches the repository (defaults are lazy, never persisted)")
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:"+bom.DefaultVersion, ResolveCore(p).Image,
		"unset spec.image must resolve Core to the public registry after defaulting")
}

func TestDefaultImageRegistryUserOverrideWins(t *testing.T) {
	// A user-set spec.image.registry/repository must NOT be clobbered by the default.
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Common:    otilmv1alpha1.CommonSpec{Image: otilmv1alpha1.ImageSpec{Registry: testRegistry, Repository: "myteam"}},
			Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb"},
			Messaging: otilmv1alpha1.MessagingSpec{Host: "rabbit", VirtualHost: "ilm"},
		},
	}
	DefaultImageRegistry(p)
	assert.Equal(t, testRegistry, p.Spec.Common.Image.Registry, "user-set registry must win")
	assert.Equal(t, "myteam", p.Spec.Common.Image.Repository, "user-set repository must win")
	assert.Equal(t, "registry.example.com/myteam/core:"+bom.DefaultVersion, ResolveCore(p).Image)
}

func TestDefaultImageRegistryFillsOnlyRegistry(t *testing.T) {
	// DefaultImageRegistry only ever fills the registry — the repository defaults
	// lazily in ResolveImage and the CR field stays untouched.
	p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{
		Common:    otilmv1alpha1.CommonSpec{Image: otilmv1alpha1.ImageSpec{Registry: testRegistry}},
		Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb"},
		Messaging: otilmv1alpha1.MessagingSpec{Host: "rabbit", VirtualHost: "ilm"},
	}}
	DefaultImageRegistry(p)
	assert.Equal(t, testRegistry, p.Spec.Common.Image.Registry)
	assert.Equal(t, "", p.Spec.Common.Image.Repository, "repository stays unset on the CR")
	assert.Contains(t, ResolveCore(p).Image, "/ilm/", "resolved reference still gets the default repository")
}

func TestResolveCoreEnvOverrideShadowsProfile(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Database:  otilmv1alpha1.DatabaseSpec{Host: "pg", Port: 5432, Name: "ilmdb"},
			Messaging: otilmv1alpha1.MessagingSpec{Host: "rabbit", VirtualHost: "ilm"},
			Core:      otilmv1alpha1.CoreSpec{ComponentSpec: otilmv1alpha1.ComponentSpec{Env: []otilmv1alpha1.EnvVar{{Name: "JDBC_URL", Value: "jdbc:custom"}}}},
		},
	}
	c := ResolveCore(p)
	// the wiring profile sets JDBC_URL; the CR override is appended after it, so the
	// last JDBC_URL entry carries the override (k8s uses the last value for duplicate env names).
	var last string
	for _, e := range c.Env {
		if e.Name == "JDBC_URL" {
			last = e.Value
		}
	}
	assert.Equal(t, "jdbc:custom", last)
}

// containerByName returns the container with the given name from a slice, and whether found.
func containerByName(cs []corev1.Container, name string) (corev1.Container, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

// initEnvValue returns a container's inline env value for name (Value, not ValueFrom),
// and whether an entry with that name exists.
func initEnvValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// initSecretEnv returns the corev1.EnvVar for name on a container, and whether found.
// Used to assert secretKeyRef wiring on raw container env (as opposed to the
// component render model's SecretEnv list).
func initSecretEnv(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestResolveCoreStaticServiceURLEnv(t *testing.T) {
	c := ResolveCore(basePlatform())
	w := bom.Wiring()
	cases := map[string]string{
		w.HeaderEnabledEnv: "true",
		w.HeaderNameEnv:    "ssl-client-cert",
		w.OPABaseURLEnv:    "http://localhost:8181",
		w.AuthURLEnv:       "http://auth:8080",
		w.SchedulerURLEnv:  "http://scheduler:8080",
		w.ProxyEnabledEnv:  "false",
	}
	for env, want := range cases {
		got, ok := envValue(c.Env, env)
		require.True(t, ok, "env %s must be present", env)
		assert.Equal(t, want, got, "env %s", env)
	}
}

func TestResolveCoreHeaderNameDefaultsAndOverrides(t *testing.T) {
	w := bom.Wiring()

	// Default when auth.header.certificate is empty.
	c := ResolveCore(basePlatform())
	got, ok := envValue(c.Env, w.HeaderNameEnv)
	require.True(t, ok)
	assert.Equal(t, "ssl-client-cert", got)

	// CR override wins.
	p := basePlatform()
	p.Spec.Core.ClientCertHeader = "x-client-cert"
	c = ResolveCore(p)
	got, ok = envValue(c.Env, w.HeaderNameEnv)
	require.True(t, ok)
	assert.Equal(t, "x-client-cert", got)
}

func TestResolveCoreLoggingLevelDefaultsAndOverrides(t *testing.T) {
	w := bom.Wiring()

	c := ResolveCore(basePlatform())
	got, ok := envValue(c.Env, w.LoggingLevelEnv)
	require.True(t, ok)
	assert.Equal(t, "INFO", got, "logging level defaults to INFO")

	p := basePlatform()
	p.Spec.Common.Logging.Level = "DEBUG"
	c = ResolveCore(p)
	got, _ = envValue(c.Env, w.LoggingLevelEnv)
	assert.Equal(t, "DEBUG", got)
}

// TestResolveCoreJavaOptsViaEnv proves JAVA_OPTS is now expressed as plain per-component
// env (the special-case root spec.javaOpts is gone): a core.env JAVA_OPTS entry renders
// verbatim on the Core container, and the operator injects NO JAVA_OPTS of its own when
// the user sets none. This is the fast-tier proof that removing the auto JAVA_OPTS is
// non-lossy — users keep full control via env.
func TestResolveCoreJavaOptsViaEnv(t *testing.T) {
	// JAVA_OPTS is plain per-component env now (the operator wires no JAVA_OPTS of its own).
	const javaOptsEnv = "JAVA_OPTS"

	// No JAVA_OPTS is auto-injected when the user sets none.
	bare := ResolveCore(basePlatform())
	_, hasAuto := envValue(bare.Env, javaOptsEnv)
	assert.False(t, hasAuto, "the operator must not auto-inject JAVA_OPTS; it is plain env now")

	// A core.env JAVA_OPTS entry renders verbatim on Core.
	p := basePlatform()
	p.Spec.Core.Env = []otilmv1alpha1.EnvVar{{Name: javaOptsEnv, Value: "-Xmx1g"}}
	c := ResolveCore(p)
	got, ok := envValue(c.Env, javaOptsEnv)
	require.True(t, ok, "core.env JAVA_OPTS must render on Core")
	assert.Equal(t, "-Xmx1g", got)
}

func TestResolveCoreProxyDisabledOmitsURLs(t *testing.T) {
	w := bom.Wiring()
	c := ResolveCore(basePlatform())
	enabled, ok := envValue(c.Env, w.ProxyEnabledEnv)
	require.True(t, ok)
	assert.Equal(t, "false", enabled)
	_, hasHTTP := envValue(c.Env, w.HTTPProxyEnv)
	_, hasHTTPS := envValue(c.Env, w.HTTPSProxyEnv)
	_, hasNo := envValue(c.Env, w.NoProxyEnv)
	assert.False(t, hasHTTP, "HTTP_PROXY omitted when unset")
	assert.False(t, hasHTTPS, "HTTPS_PROXY omitted when unset")
	assert.False(t, hasNo, "NO_PROXY omitted when unset")
}

func TestResolveCoreProxyEnabledInjectsURLs(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{
		Enabled: true, HTTP: "http://proxy:3128", HTTPS: "http://proxy:3129", NoProxy: "localhost,.svc",
	}
	c := ResolveCore(p)
	enabled, _ := envValue(c.Env, w.ProxyEnabledEnv)
	assert.Equal(t, "true", enabled)
	httpv, ok := envValue(c.Env, w.HTTPProxyEnv)
	require.True(t, ok)
	assert.Equal(t, "http://proxy:3128", httpv)
	httpsv, ok := envValue(c.Env, w.HTTPSProxyEnv)
	require.True(t, ok)
	assert.Equal(t, "http://proxy:3129", httpsv)
	nov, ok := envValue(c.Env, w.NoProxyEnv)
	require.True(t, ok)
	assert.Equal(t, "localhost,.svc", nov)
}

// fieldRefPath returns the downward-API fieldPath bound to the given env var in a
// component's FieldRefEnv slice (and whether it is present).
func fieldRefPath(refs []common.FieldRefEnv, name string) (string, bool) {
	for _, r := range refs {
		if r.EnvVar == name {
			return r.FieldPath, true
		}
	}
	return "", false
}

// TestResolveCoreProxyInstanceIDGatedOnProxy asserts Core gets PROXY_INSTANCE_ID
// from the pod name via the downward API (fieldRef metadata.name) when proxy is
// enabled (gated on spec.proxy.enabled) — and that it is absent (no fieldRef env
// at all) when proxy is disabled.
func TestResolveCoreProxyInstanceIDGatedOnProxy(t *testing.T) {
	w := bom.Wiring()

	// Proxy disabled (the base default): no PROXY_INSTANCE_ID, no fieldRef env.
	c := ResolveCore(basePlatform())
	_, has := fieldRefPath(c.FieldRefEnv, w.ProxyInstanceIDEnv)
	assert.False(t, has, "PROXY_INSTANCE_ID must be absent when proxy is disabled")
	assert.Empty(t, c.FieldRefEnv, "no downward-API env when proxy is disabled")
	// And it must not leak in as an inline env either.
	_, hasInline := envValue(c.Env, w.ProxyInstanceIDEnv)
	assert.False(t, hasInline, "PROXY_INSTANCE_ID is never an inline env")

	// Proxy enabled: PROXY_INSTANCE_ID present via fieldRef metadata.name.
	p := basePlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	c = ResolveCore(p)
	path, has := fieldRefPath(c.FieldRefEnv, w.ProxyInstanceIDEnv)
	require.True(t, has, "PROXY_INSTANCE_ID must be present when proxy is enabled")
	assert.Equal(t, "metadata.name", path, "PROXY_INSTANCE_ID sourced from the pod name")
	// It is downward-API only — never inlined as a value.
	_, hasInline = envValue(c.Env, w.ProxyInstanceIDEnv)
	assert.False(t, hasInline, "PROXY_INSTANCE_ID is sourced via fieldRef, not inline")
}

func TestResolveCoreProvisioning(t *testing.T) {
	w := bom.Wiring()

	// URL inline, key secret-backed when both set.
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		APIURL: testProvURL, APIKeySecretRef: testProvSecret,
	}
	c := ResolveCore(p)
	url, ok := envValue(c.Env, w.ProvisioningURLEnv)
	require.True(t, ok)
	assert.Equal(t, testProvURL, url)
	key, ok := secretEnvFor(c.SecretEnv, w.ProvisioningAPIKey.Env)
	require.True(t, ok, "PROVISIONING_API_KEY must be secret-backed when apiKeySecretRef is set")
	assert.Equal(t, testProvSecret, key.SecretName)
	assert.Equal(t, w.ProvisioningAPIKey.Key, key.SecretKey)

	// No secret-backed key when apiKeySecretRef is empty (URL still present, empty by default).
	c = ResolveCore(basePlatform())
	_, ok = secretEnvFor(c.SecretEnv, w.ProvisioningAPIKey.Env)
	assert.False(t, ok, "no PROVISIONING_API_KEY without apiKeySecretRef")
	url, ok = envValue(c.Env, w.ProvisioningURLEnv)
	require.True(t, ok, "PROVISIONING_API_URL is always present (empty when unset)")
	assert.Empty(t, url)
}

func TestResolveCoreTrustedCertificatesSecretBacked(t *testing.T) {
	w := bom.Wiring()

	// Present only when secretRef is set; sourced via secretKeyRef to ca.crt.
	c := ResolveCore(basePlatform())
	_, ok := secretEnvFor(c.SecretEnv, w.TrustedCertificates.Env)
	assert.False(t, ok, "no TRUSTED_CERTIFICATES without trustedCertificates.secretRef")

	p := basePlatform()
	p.Spec.Common.TrustedCertificates.SecretRef = "ilm-trusted-ca"
	c = ResolveCore(p)
	tc, ok := secretEnvFor(c.SecretEnv, w.TrustedCertificates.Env)
	require.True(t, ok, "TRUSTED_CERTIFICATES must be secret-backed when set")
	assert.Equal(t, "ilm-trusted-ca", tc.SecretName)
	assert.Equal(t, "ca.crt", tc.SecretKey)
	assert.True(t, tc.Optional,
		"TRUSTED_CERTIFICATES secretKeyRef must be Optional so a not-yet-present bundle does not wedge Core")
}

func TestResolveCoreAdminCertDisabledOmitsEnv(t *testing.T) {
	w := bom.Wiring()
	// No registerAdmin → no ADMIN_CERT.
	c := ResolveCore(basePlatform())
	_, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	assert.False(t, ok, "no ADMIN_CERT without registerAdmin")

	// registerAdmin disabled → still no ADMIN_CERT.
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: false}
	c = ResolveCore(p)
	_, ok = secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	assert.False(t, ok, "no ADMIN_CERT when registerAdmin.enabled=false")
}

func TestResolveCoreAdminCertProvidedByRef(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr(testAdminCert)},
	}
	c := ResolveCore(p)
	ac, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	require.True(t, ok, "ADMIN_CERT must be secret-backed for source=provided")
	assert.Equal(t, testAdminCert, ac.SecretName, "ADMIN_CERT must reference the caller's SecretRef")
	assert.Equal(t, testTLSCrt, ac.SecretKey)
	assert.True(t, ac.Optional,
		"ADMIN_CERT secretKeyRef must be Optional so a not-yet-issued cert does not wedge Core")

	// The cert value must never be inline.
	_, inline := envValue(c.Env, w.AdminCert.Env)
	assert.False(t, inline, "ADMIN_CERT must not be inline")
}

func TestResolveCoreAdminCertGeneratedByRef(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}
	c := ResolveCore(p)
	ac, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	require.True(t, ok, "ADMIN_CERT must be secret-backed for source=generated")
	assert.Equal(t, adminCertSecretName, ac.SecretName,
		"ADMIN_CERT must reference the cert-manager-populated admin-certificate-secret")
	assert.Equal(t, testTLSCrt, ac.SecretKey)
	assert.True(t, ac.Optional,
		"ADMIN_CERT secretKeyRef must be Optional for source=generated so the cert-manager race does not wedge Core")
}

func TestResolveCoreAdminCertProvidedWithoutRefOmitsEnv(t *testing.T) {
	w := bom.Wiring()
	// source=provided but SecretRef unset (a misconfiguration the webhook rejects):
	// the operator renders no dangling ADMIN_CERT reference.
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided"}}
	c := ResolveCore(p)
	_, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	assert.False(t, ok, "no ADMIN_CERT when source=provided but SecretRef is unset")
}

func TestResolveCoreOPASidecar(t *testing.T) {
	c := ResolveCore(basePlatform())
	opa, ok := containerByName(c.Sidecars, "auth-opa")
	require.True(t, ok, "auth-opa sidecar must be present")
	assert.True(t, c.SidecarsFirst,
		"OPA must render BEFORE core: core's OIDC postStart blocks on the OPA sidecar's :8181, and the "+
			"kubelet starts containers in order with synchronous postStart, so OPA-after-core deadlocks")
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/opa:1.10.0-static", opa.Image)
	require.Len(t, opa.Ports, 1)
	assert.Equal(t, int32(8181), opa.Ports[0].ContainerPort)
	require.NotNil(t, opa.ReadinessProbe)
	require.NotNil(t, opa.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/health?bundle=true", opa.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(8181), opa.ReadinessProbe.HTTPGet.Port.IntVal)
	require.NotNil(t, opa.StartupProbe, "OPA startup probe present")
	// args run the OPA server and point its bundle service at the clean Service name.
	joined := strings.Join(opa.Args, " ")
	assert.Contains(t, joined, "run")
	assert.Contains(t, joined, "--server")
	assert.Contains(t, joined, "--addr=0.0.0.0:8181")
	assert.Contains(t, joined, "--set=services.nginx.url=http://auth-opa-policies:8080")
	assert.Contains(t, joined, "--set=bundles.nginx.resource=bundles/bundle.tar.gz")
}

func TestResolveCoreWaitForAuthInitContainer(t *testing.T) {
	c := ResolveCore(basePlatform())
	init, ok := containerByName(c.InitContainers, testWaitForAuth)
	require.True(t, ok, "wait-for-auth init container must be present")
	assert.Equal(t, testCurlImage, init.Image)
	require.Len(t, init.Command, 3)
	script := init.Command[2]
	// The three operator-owned dependencies are waited on by their clean Service names
	// (compile-time constants, safe to interpolate).
	assert.Contains(t, script, "nc -z auth 8080")
	assert.Contains(t, script, "nc -z auth-opa-policies 8080")
	assert.Contains(t, script, "nc -z scheduler 8080")
	// The broker loop reads the CR-supplied coordinates from QUOTED env vars instead.
	assert.Contains(t, script, testMQWaitLoop)
	host, ok := initEnvValue(init, mqWaitHostEnv)
	require.True(t, ok, "the broker host must be passed as an env value")
	assert.Equal(t, testMQHost, host)
	port, ok := initEnvValue(init, mqWaitPortEnv)
	require.True(t, ok, "the broker port must be passed as an env value")
	assert.Equal(t, "5672", port)
}

// hostileHosts is the shared hostile-value table for the broker wait loops: shell
// metacharacters that, in the earlier shape (the host interpolated UNQUOTED into `nc -z %s`),
// would have run a command in the waiting container on every poll iteration.
func hostileHosts() []struct {
	name string
	host string
} {
	return []struct {
		name string
		host string
	}{
		{name: "command substitution", host: "$(id)"},
		{name: "backtick command substitution", host: "`id`"},
		{name: "embedded double quote", host: `a"b`},
		{name: "embedded single quote", host: `a'b`},
		{name: "embedded newline", host: "a\nb"},
		{name: "variable reference", host: "a$VAR"},
		{name: "command separator", host: "a;b"},
		{name: "pipeline", host: "a|b"},
		{name: "the original report's payload", host: "x; touch /tmp/pwned"},
	}
}

// TestWaitLoopsNeverInterpolateTheBrokerHost is the structural regression guard for the
// broker-reachability wait loops on BOTH Core (wait-for-auth) and scheduler
// (wait-for-messaging-service).
//
// The property under test: an external broker's spec.messaging.host NEVER appears in the
// generated script text at all. It reaches the container only as a Kubernetes env VALUE (which
// kubelet sets verbatim — no shell involved) and is read back as a QUOTED parameter expansion,
// which the shell never re-parses as syntax. So no host value, however hostile, can reach a
// shell command position.
func TestWaitLoopsNeverInterpolateTheBrokerHost(t *testing.T) {
	waiters := []struct {
		name      string
		container func(*otilmv1alpha1.Platform) (corev1.Container, bool)
	}{
		{
			name: testWaitForAuth,
			container: func(p *otilmv1alpha1.Platform) (corev1.Container, bool) {
				return containerByName(ResolveCore(p).InitContainers, testWaitForAuth)
			},
		},
		{
			name: testWaitForMessaging,
			container: func(p *otilmv1alpha1.Platform) (corev1.Container, bool) {
				return containerByName(ResolveScheduler(p).InitContainers, testWaitForMessaging)
			},
		},
	}
	for _, w := range waiters {
		for _, tc := range hostileHosts() {
			t.Run(w.name+"/"+tc.name, func(t *testing.T) {
				p := basePlatform()
				p.Spec.Messaging.Host = tc.host
				init, ok := w.container(p)
				require.True(t, ok, "%s init container must be present", w.name)
				require.Len(t, init.Command, 3)
				script := init.Command[2]

				// STRUCTURE: the loop reads the coordinates from quoted expansions, and the
				// host value appears nowhere in the script the shell executes.
				assert.Contains(t, script, testMQWaitLoop,
					"the broker loop must read the coordinates from QUOTED env vars")
				assert.NotContains(t, script, tc.host,
					"the broker host must never be interpolated into shell source")

				// DELIVERY: the raw value reaches the container as an env value, unmangled.
				host, ok := initEnvValue(init, mqWaitHostEnv)
				require.True(t, ok, "%s must carry the broker host as an env value", mqWaitHostEnv)
				assert.Equal(t, tc.host, host, "the env value carries the host verbatim")
				port, ok := initEnvValue(init, mqWaitPortEnv)
				require.True(t, ok, "%s must carry the broker port as an env value", mqWaitPortEnv)
				assert.Equal(t, "5672", port)

				// The wait-loop env is script-local and must never carry a credential.
				for _, e := range init.Env {
					assert.Nil(t, e.ValueFrom, "the wait-loop env must be plain values only")
				}
			})
		}
	}
}

// TestWaitLoopsCarryManagedBrokerCoordinates locks that the env-var indirection is
// mode-agnostic: a MANAGED broker's generated Service name and port travel the same path.
func TestWaitLoopsCarryManagedBrokerCoordinates(t *testing.T) {
	p := basePlatform()
	p.Spec.Messaging.Mode = "managed"
	p.Spec.Messaging.Managed = &otilmv1alpha1.ManagedMessagingSpec{Replicas: 1}

	mq := ResolveMessagingConnection(p)
	require.NotEmpty(t, mq.Host, "a managed broker resolves to the generated Service name")

	for name, init := range map[string]corev1.Container{
		testWaitForAuth:      mustContainer(t, ResolveCore(p).InitContainers, testWaitForAuth),
		testWaitForMessaging: mustContainer(t, ResolveScheduler(p).InitContainers, testWaitForMessaging),
	} {
		host, ok := initEnvValue(init, mqWaitHostEnv)
		require.True(t, ok, "%s must carry the broker host", name)
		assert.Equal(t, mq.Host, host, "%s waits on the managed broker Service", name)
		port, ok := initEnvValue(init, mqWaitPortEnv)
		require.True(t, ok, "%s must carry the broker port", name)
		assert.Equal(t, strconv.Itoa(int(mq.Port)), port)
	}
}

// mustContainer returns the named container or fails the test.
func mustContainer(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	c, ok := containerByName(cs, name)
	require.True(t, ok, "container %q must be present", name)
	return c
}

// ---- provision-instance-queue init container (proxy path) ------------------

// proxyProvisioningPlatform returns a base platform with the proxy enabled and an
// external provisioning API configured (key by Secret reference) — the gate that
// turns on the provision-instance-queue init container.
func proxyProvisioningPlatform() *otilmv1alpha1.Platform {
	p := basePlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		APIURL: testProvURL, APIKeySecretRef: testProvSecret,
	}
	return p
}

func TestResolveCoreProvisionQueueInitRenderedWhenProxyAndProvisioning(t *testing.T) {
	w := bom.Wiring()
	c := ResolveCore(proxyProvisioningPlatform())

	init, ok := containerByName(c.InitContainers, testProvInstanceQueue)
	require.True(t, ok, "provision-instance-queue must be present on the proxy+provisioning path")
	assert.Equal(t, testCurlImage, init.Image,
		"provision-queue uses the curl image from the BOM")

	// wait-for-auth still runs, and runs first (init-container ordering).
	require.NotEmpty(t, c.InitContainers)
	assert.Equal(t, testWaitForAuth, c.InitContainers[0].Name, "wait-for-auth runs first")

	require.Len(t, init.Command, 3)
	script := init.Command[2]
	// Per-pod queue named after the pod hostname; self-idempotent retry on transient failures.
	assert.Contains(t, script, "HOSTNAME=$(hostname)", "queue is named per-pod from the hostname")
	assert.Contains(t, script, "/api/v1/queues", "POSTs to the provisioning queues endpoint")
	assert.Contains(t, script, "-X POST", "registers the queue via POST")
	assert.Contains(t, script, "while true; do", "the request is issued in a retry loop")
	assert.Contains(t, script, "sleep 5", "retry loop backs off between attempts")
	assert.Contains(t, script, `"name":"${HOSTNAME}"`, "queue name is the pod hostname placeholder")
	assert.Contains(t, script, `sed "s/\${HOSTNAME}/${HOSTNAME}/g"`,
		"the placeholder is substituted at runtime from the pod hostname")
	assert.Contains(t, script, "X-API-Key: ${PROVISIONING_API_KEY}", "sends the API key header when present")
	// The endpoint is CR-derived (spec.provisioning.apiURL), so it is passed with curl's --url
	// flag: an apiURL beginning with "-" is then taken as a URL, not parsed as a curl option.
	assert.Equal(t, 2, strings.Count(script, `--url "${PROVISIONING_API_URL}/api/v1/queues"`),
		"both curl branches must pass the CR-derived endpoint via --url")
	assert.NotContains(t, script, `-X POST "${PROVISIONING_API_URL}`,
		"a CR-derived URL must never sit in curl's bare-argument position")

	// PROVISIONING_API_URL is inline; the API key is secretKeyRef'd (never inline).
	url, urlInline := initEnvValue(init, w.ProvisioningURLEnv)
	require.True(t, urlInline, "PROVISIONING_API_URL must be set on the init container")
	assert.Equal(t, testProvURL, url)

	keyRef, ok := initSecretEnv(init, w.ProvisioningAPIKey.Env)
	require.True(t, ok, "PROVISIONING_API_KEY must be present and secret-backed")
	require.NotNil(t, keyRef.ValueFrom)
	require.NotNil(t, keyRef.ValueFrom.SecretKeyRef)
	assert.Equal(t, testProvSecret, keyRef.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, w.ProvisioningAPIKey.Key, keyRef.ValueFrom.SecretKeyRef.Key)
	assert.Empty(t, keyRef.Value, "API key must not be inlined; only the reference is set")
}

// provisionQueueBody is the parsed shape of the request body the provision-instance-queue
// script POSTs. Declared here (not reused from production) so the JSON contract is asserted
// independently of the builder's own struct.
type provisionQueueBody struct {
	Name       string           `json:"name"`
	Exchange   string           `json:"exchange"`
	RoutingKey string           `json:"routingKey"`
	Properties map[string]int64 `json:"properties"`
}

// deployOverride returns a platform mutation switching provisioning to deploy mode and
// applying apply to the deploy block, so the queue-request trio
// (exchange / routingKey / queueArguments) is exercised on its real spec path.
func deployOverride(apply func(*otilmv1alpha1.ProvisioningDeploySpec)) func(*otilmv1alpha1.Platform) {
	return func(p *otilmv1alpha1.Platform) {
		d := &otilmv1alpha1.ProvisioningDeploySpec{BootstrapSecretRef: testProvBootstrap}
		apply(d)
		p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{Mode: "deploy", Deploy: d}
	}
}

// exchangeOverride returns a platform mutation pinning spec.provisioning.deploy.exchange to v
// (deploy mode, so the override path is the one under test).
func exchangeOverride(v string) func(*otilmv1alpha1.Platform) {
	return deployOverride(func(d *otilmv1alpha1.ProvisioningDeploySpec) { d.Exchange = v })
}

// provisionQueueScript renders p and returns the provision-instance-queue init container's
// script, failing the test when the proxy path did not render the container at all.
func provisionQueueScript(t *testing.T, p *otilmv1alpha1.Platform) string {
	t.Helper()
	init, ok := containerByName(ResolveCore(p).InitContainers, testProvInstanceQueue)
	require.True(t, ok, "provision-instance-queue must render on the proxy+provisioning path")
	require.Len(t, init.Command, 3)
	return init.Command[2]
}

// heredocParts splits a rendered init-container script at its QUOTED heredoc, returning the
// heredoc body (inert data the shell never expands) and the surrounding text (everything the
// shell actually executes). It fails the test unless the script uses a quoted heredoc, which
// is the property that makes a hostile value harmless.
func heredocParts(t *testing.T, script string) (body, outside string) {
	t.Helper()
	const openTok, closeTok = "<<'EOF'\n", "\nEOF\n"
	i := strings.Index(script, openTok)
	require.GreaterOrEqual(t, i, 0, "the request body must be emitted inside a QUOTED heredoc (<<'EOF')")
	rest := script[i+len(openTok):]
	j := strings.Index(rest, closeTok)
	require.GreaterOrEqual(t, j, 0, "the quoted heredoc must be terminated")
	return rest[:j], script[:i+len(openTok)] + rest[j:]
}

// TestResolveCoreProvisionQueueInitExchangeIsVersionResolved proves two things about the
// exchange the provision-instance-queue script binds the pod's queue to.
//
// RESOLUTION: it is the SAME proxy exchange the provisioning builder declares — the selected
// bundle's default (2.18.0: czertainly-proxy; 2.19.0: ilm-proxy) or the
// spec.provisioning.deploy.exchange override. A hard-coded exchange would make Core's
// registration retry forever on a platform whose bundle renamed it.
//
// SAFETY: whatever the value, it reaches the container as JSON DATA inside a quoted heredoc —
// never as shell command text. The hostile cases below (command substitution, backticks, a
// quote, a newline, a variable reference) are the regression guard for the earlier shape, which
// interpolated the value into a DOUBLE-quoted shell assignment (%q) and an unencoded JSON body:
// there, `$(id)` would have executed in an init container holding the provisioning API key.
func TestResolveCoreProvisionQueueInitExchangeIsVersionResolved(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*otilmv1alpha1.Platform)
		want    string
		notWant string
	}{
		{
			name: "default version (2.18.0) uses the czertainly proxy exchange",
			mutate: func(_ *otilmv1alpha1.Platform) {
				// no extra spec setup: default version resolves without explicit mutation
			},
			want: testExchangeCzertainlyProxy,
		},
		{
			name:    "explicit 2.18.0 uses the czertainly proxy exchange",
			mutate:  func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion218 },
			want:    testExchangeCzertainlyProxy,
			notWant: testExchangeIlmProxy,
		},
		{
			name:    "explicit 2.19.0 uses the renamed ilm proxy exchange",
			mutate:  func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion219 },
			want:    testExchangeIlmProxy,
			notWant: testExchangeCzertainlyProxy,
		},
		{
			name:    "spec.provisioning.deploy.exchange overrides the bundle default",
			mutate:  exchangeOverride(testCustomProxyExchange),
			want:    testCustomProxyExchange,
			notWant: testExchangeCzertainlyProxy,
		},
		// Hostile values. None can be STORED any more (the CRD constrains the field's charset),
		// so these prove the CONSTRUCTION is safe regardless — the value stays inert JSON data
		// even if one somehow reaches the builder.
		{
			name:    "command substitution stays inert data",
			mutate:  exchangeOverride("$(id)"),
			want:    "$(id)",
			notWant: testExchangeCzertainlyProxy,
		},
		{
			name:    "backtick command substitution stays inert data",
			mutate:  exchangeOverride("`id`"),
			want:    "`id`",
			notWant: testExchangeCzertainlyProxy,
		},
		{
			name:    "an embedded double quote cannot reshape the JSON",
			mutate:  exchangeOverride(`a"b`),
			want:    `a"b`,
			notWant: testExchangeCzertainlyProxy,
		},
		{
			name:    "an embedded newline cannot break out of the heredoc",
			mutate:  exchangeOverride("a\nb"),
			want:    "a\nb",
			notWant: testExchangeCzertainlyProxy,
		},
		{
			name:    "a variable reference is not expanded",
			mutate:  exchangeOverride("a$VAR"),
			want:    "a$VAR",
			notWant: testExchangeCzertainlyProxy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := proxyProvisioningPlatform()
			tc.mutate(p)
			c := ResolveCore(p)

			init, ok := containerByName(c.InitContainers, testProvInstanceQueue)
			require.True(t, ok, "provision-instance-queue must render on the proxy+provisioning path")
			require.Len(t, init.Command, 3)
			script := init.Command[2]

			// STRUCTURE: the body is emitted inside a QUOTED heredoc (the shell expands nothing
			// in it), and the old double-quoted shell assignment is gone for good.
			body, outside := heredocParts(t, script)
			assert.Contains(t, script, "<<'EOF'", "the heredoc delimiter must be QUOTED")
			assert.NotContains(t, script, "PROXY_EXCHANGE=",
				"the value must never be assigned into a double-quoted shell variable again")
			assert.NotContains(t, body, "\n",
				"the JSON body is one line, so no value can forge the heredoc terminator")

			// ROUND-TRIP: the heredoc body is valid JSON and carries the resolved exchange EXACTLY.
			var got provisionQueueBody
			require.NoError(t, json.Unmarshal([]byte(body), &got),
				"the heredoc body must be valid JSON (encoding/json composed it)")
			assert.Equal(t, tc.want, got.Exchange, "the exchange must round-trip through JSON unchanged")
			assert.Equal(t, "${HOSTNAME}", got.Name, "the queue name is the runtime hostname placeholder")
			assert.Equal(t, "proxymessage.*.${HOSTNAME}", got.RoutingKey,
				"the routing key keeps its hostname placeholder")
			assert.Equal(t, map[string]int64{testQueueArgExpires: 1800000}, got.Properties)

			// INERTNESS: the value appears ONLY in the heredoc body — never in text the shell
			// executes, so command substitution, backticks and quotes have nothing to escape.
			assert.NotContains(t, outside, tc.want,
				"the exchange value must not appear anywhere the shell evaluates it")

			// Both curl branches send the one composed body.
			assert.Equal(t, 2, strings.Count(script, `-d "${BODY}"`),
				"both curl branches must send the same composed JSON body")
			if tc.notWant != "" {
				assert.NotContains(t, script, tc.notWant,
					"no other exchange name may appear in the script")
			}
		})
	}
}

// ---- provision-instance-queue: failure classification + timeouts -----------

// caseBlock returns the body of the shell `case` statement the rendered script dispatches on
// expr (e.g. `"$RC"`) — the text between `case <expr> in` and its `esac`. Asserting on the
// two classifier BLOCKS keeps these tests structural: they check that each outcome has a
// branch and that the branch does the right thing, without pasting the whole script.
func caseBlock(t *testing.T, script, expr string) string {
	t.Helper()
	open := "case " + expr + " in\n"
	i := strings.Index(script, open)
	require.GreaterOrEqual(t, i, 0, "the script must classify outcomes with `case %s in`", expr)
	rest := script[i+len(open):]
	j := strings.Index(rest, "esac")
	require.GreaterOrEqual(t, j, 0, "the `case %s` statement must be terminated with esac", expr)
	return rest[:j]
}

// caseBranch returns the body of the branch a case block dispatches on for pattern — the text
// between "<pattern>)" and the branch terminator ";;". It fails the test when the block has no
// such branch, so a classifier that silently drops an outcome is caught rather than passing
// vacuously.
func caseBranch(t *testing.T, block, pattern string) string {
	t.Helper()
	open := pattern + ")\n"
	i := strings.Index(block, open)
	require.GreaterOrEqual(t, i, 0, "the classifier must have a %q branch", pattern)
	rest := block[i+len(open):]
	j := strings.Index(rest, ";;")
	require.GreaterOrEqual(t, j, 0, "the %q branch must be terminated with ;;", pattern)
	return rest[:j]
}

// exitStatus returns the status a shell branch terminates the init container with, and
// whether it terminates at all. It matches an `exit` STATEMENT (a line of its own), so a
// diagnostic message that merely mentions the word "exit" is not mistaken for one.
func exitStatus(branch string) (string, bool) {
	for _, line := range strings.Split(branch, "\n") {
		if code, ok := strings.CutPrefix(strings.TrimSpace(line), "exit "); ok {
			return code, true
		}
	}
	return "", false
}

// TestResolveCoreProvisionQueueInitClassifiesOutcomes is the regression guard for the
// init container's central robustness property: it must RETRY only transient outcomes and
// FAIL FAST on permanent ones.
//
// The previous shape wrapped `curl -sf` in an until-loop, so `-f`'s single non-zero exit made
// 400/401/404 indistinguishable from "API not up yet": a wrong API key or URL retried forever
// behind a misleading "Waiting for provisioning API" message, an init container that never
// terminates and blocks Core's startup silently. The script now classifies the curl exit code
// and the HTTP status separately, mirroring the released platform chart's
// ilm.initContainer.provisionQueue.
func TestResolveCoreProvisionQueueInitClassifiesOutcomes(t *testing.T) {
	script := provisionQueueScript(t, proxyProvisioningPlatform())
	transport := caseBlock(t, script, `"$RC"`)
	status := caseBlock(t, script, `"$CODE"`)

	tests := []struct {
		name string
		// block is the classifier the outcome is dispatched by, pattern its case label.
		block, pattern string
		// permanent outcomes exit non-zero without sleeping; transient ones sleep and loop.
		permanent bool
		// diagnostics are the fragments the branch must print, so the failure is actionable
		// in `kubectl logs` rather than a bare exit code.
		diagnostics []string
	}{
		{
			name:  "permanent transport failures (unsupported protocol, malformed URL, TLS) fail fast",
			block: transport, pattern: "1|3|60", permanent: true,
			diagnostics: []string{"failed permanently", "curl exit ${RC}", "not retrying"},
		},
		{
			name:  "any other transport failure is transient and retried",
			block: transport, pattern: "*", permanent: false,
			diagnostics: []string{"curl exit ${RC}", "retrying in 5s"},
		},
		{
			name:  "408, 429 and 5xx are transient and retried",
			block: status, pattern: "408|429|5??", permanent: false,
			diagnostics: []string{"not ready (HTTP ${CODE})", "retrying in 5s"},
		},
		{
			name:  "any other 4xx is permanent and prints the status AND the response body",
			block: status, pattern: "*", permanent: true,
			diagnostics: []string{"Queue provisioning failed (HTTP ${CODE})", "${RESPONSE}"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			branch := caseBranch(t, tc.block, tc.pattern)
			for _, d := range tc.diagnostics {
				assert.Contains(t, branch, d, "the branch must print an actionable diagnostic")
			}
			code, terminates := exitStatus(branch)
			if tc.permanent {
				require.True(t, terminates, "a permanent failure must terminate the init container")
				assert.Equal(t, "1", code, "a permanent failure must exit non-zero")
				assert.NotContains(t, branch, "sleep", "a permanent failure must not sleep and retry")
				return
			}
			assert.False(t, terminates, "a transient failure must not terminate the init container")
			assert.Contains(t, branch, "sleep 5", "a transient failure must back off before retrying")
		})
	}

	// The success branch keeps its exit status and its original message.
	success := caseBranch(t, status, "2??")
	assert.Contains(t, success, `echo "Instance queue provisioned for ${HOSTNAME}"`)
	code, terminates := exitStatus(success)
	require.True(t, terminates, "a 2xx must terminate the init container")
	assert.Equal(t, "0", code, "a 2xx must succeed the init container")

	// The status and body are captured from one response: -w appends the code on its own
	// line, which the script then splits off the body.
	assert.Contains(t, script, `CODE=$(printf '%s' "$OUT" | tail -n 1)`, "the HTTP status is the last line")
	assert.Contains(t, script, `RESPONSE=$(printf '%s' "$OUT" | sed '$d')`, "the response body is everything before it")
}

// TestResolveCoreProvisionQueueInitCurlOptions locks the per-request curl options both
// branches must carry: the chart's connect/total timeouts (so a black-holed endpoint cannot
// wedge an attempt indefinitely), the status write-out the classifier reads, and the ABSENCE
// of -f, whose collapse of every HTTP error into one exit code is exactly what made a
// permanent failure indistinguishable from a transient one.
func TestResolveCoreProvisionQueueInitCurlOptions(t *testing.T) {
	script := provisionQueueScript(t, proxyProvisioningPlatform())
	assert.Equal(t, 2, strings.Count(script, "--connect-timeout 5 --max-time 30"),
		"both curl branches must bound connect and total time")
	assert.Equal(t, 2, strings.Count(script, `-w '\n%{http_code}'`),
		"both curl branches must write out the HTTP status for the classifier")
	assert.Equal(t, 2, strings.Count(script, "curl -sS "),
		"both curl branches must stay quiet about progress but loud about errors")
	assert.NotContains(t, script, "curl -sf",
		"-f collapses every HTTP error into one exit code and defeats the classifier")
}

// ---- provision-instance-queue: configurable request body -------------------

// testDefaultQueueBody is the request body the operator composes when
// spec.provisioning.deploy leaves the queue trio unset. It is asserted VERBATIM so an
// unintended change to the default contract (which existing platforms depend on) cannot slip
// through as an incidental diff.
const testDefaultQueueBody = `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
	`"routingKey":"proxymessage.*.${HOSTNAME}","properties":{"x-expires":1800000}}`

// jsonValue wraps a raw JSON literal as a queue-argument value, the way the apiserver
// delivers spec.provisioning.deploy.queueArguments[].value.
func jsonValue(raw string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(raw)} }

// queueArgs builds the deploy-block mutation setting queueArguments to the given name/raw-JSON
// pairs in order.
func queueArgs(pairs ...[2]string) func(*otilmv1alpha1.Platform) {
	return deployOverride(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
		for _, kv := range pairs {
			d.QueueArguments = append(d.QueueArguments,
				otilmv1alpha1.QueueArgument{Name: kv[0], Value: jsonValue(kv[1])})
		}
	})
}

// TestResolveCoreProvisionQueueBodyIsConfigurable proves the operator's half of the released
// chart's global.provisioning.queue.{exchange,routingKey,properties} contract: each field is
// overridable at spec.provisioning.deploy, an UNSET field renders exactly the bytes it
// rendered before the field existed, and setting queueArguments REPLACES the default set
// outright (chart semantics — a provisioning service ignores arguments it does not recognise,
// so a deployment running a different service states its own full set).
func TestResolveCoreProvisionQueueBodyIsConfigurable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*otilmv1alpha1.Platform)
		want   string
	}{
		{
			name: "defaults render the pre-existing bytes verbatim",
			mutate: deployOverride(func(*otilmv1alpha1.ProvisioningDeploySpec) {
				// intentionally empty: leave the deploy spec untouched to test default behavior
			}),
			want: testDefaultQueueBody,
		},
		{
			name: "routingKey overrides the default binding key",
			mutate: deployOverride(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
				d.RoutingKey = "events.#"
			}),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"events.#","properties":{"x-expires":1800000}}`,
		},
		{
			name: "a routingKey may carry the ${HOSTNAME} token itself",
			mutate: deployOverride(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
				d.RoutingKey = "proxymessage.custom.${HOSTNAME}"
			}),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.custom.${HOSTNAME}","properties":{"x-expires":1800000}}`,
		},
		{
			name:   "a single queueArgument REPLACES the default x-expires",
			mutate: queueArgs([2]string{"x-message-ttl", "60000"}),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}","properties":{"x-message-ttl":60000}}`,
		},
		{
			name: "multiple queueArguments render in a stable, spec-order-independent order",
			mutate: queueArgs(
				[2]string{"x-queue-type", `"quorum"`},
				[2]string{testQueueArgExpires, "900000"},
				[2]string{"x-max-length", "5000"},
			),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}",` +
				`"properties":{"x-expires":900000,"x-max-length":5000,"x-queue-type":"quorum"}}`,
		},
		{
			name: "non-string JSON values (boolean, object) are forwarded verbatim",
			mutate: queueArgs(
				[2]string{"x-single-active-consumer", "true"},
				[2]string{"x-overflow", `{"strategy":"reject-publish","limit":10}`},
			),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}",` +
				`"properties":{"x-overflow":{"strategy":"reject-publish","limit":10},` +
				`"x-single-active-consumer":true}}`,
		},
		{
			name:   "an unusable value degrades to JSON null rather than corrupting the body",
			mutate: queueArgs([2]string{"x-broken", ""}),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}","properties":{"x-broken":null}}`,
		},
		{
			name: "a repeated name (rejected at admission) still renders deterministically",
			mutate: queueArgs(
				[2]string{testQueueArgExpires, "1"},
				[2]string{testQueueArgExpires, "2"},
			),
			want: `{"name":"${HOSTNAME}","exchange":"czertainly-proxy",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}","properties":{"` + testQueueArgExpires + `":2}}`,
		},
		{
			name: "the whole trio can be overridden together",
			mutate: func(p *otilmv1alpha1.Platform) {
				deployOverride(func(d *otilmv1alpha1.ProvisioningDeploySpec) {
					d.Exchange = testCustomProxyExchange
					d.RoutingKey = "proxymessage.*.${HOSTNAME}.eu"
					d.QueueArguments = []otilmv1alpha1.QueueArgument{
						{Name: testQueueArgExpires, Value: jsonValue("60000")},
					}
				})(p)
			},
			want: `{"name":"${HOSTNAME}","exchange":"` + testCustomProxyExchange + `",` +
				`"routingKey":"proxymessage.*.${HOSTNAME}.eu","properties":{"` + testQueueArgExpires + `":60000}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := proxyProvisioningPlatform()
			tc.mutate(p)
			body, outside := heredocParts(t, provisionQueueScript(t, p))

			assert.Equal(t, tc.want, body, "the composed request body must match byte-for-byte")
			assert.NotContains(t, body, "\n",
				"the body stays one line so no value can forge the heredoc terminator")
			require.True(t, json.Valid([]byte(body)), "the composed body must be valid JSON")
			assert.NotContains(t, outside, "\"properties\"",
				"the body must live only in the heredoc, never in text the shell evaluates")
		})
	}
}

// TestResolveCoreProvisionQueueArgumentValuesStayInertData is the hostile-value guard for
// spec.provisioning.deploy.queueArguments — the ONE input to this script the operator does NOT
// constrain (a queue argument is arbitrary JSON defined by the provisioning service, so unlike
// the exchange it carries no CRD charset pattern).
//
// It pins the two properties the delivery shape depends on:
//
//   - STRUCTURE: the body is read by a single `read -r` from a QUOTED, UN-NESTED heredoc. The
//     older nested form ($(cat <<'EOF' ... EOF)) is mis-parsed by bash 3.2 when the body holds
//     an unbalanced quote — and a value like a'b produces exactly that — so the nested form must
//     not come back.
//   - ROUND-TRIP: every value reaches the container byte-for-byte. A single quote survives
//     because the heredoc is quoted (the shell never re-parses it); a real newline survives
//     because encoding/json escapes it, keeping the body one line so it can neither forge the
//     heredoc terminator nor outrun the single `read`.
func TestResolveCoreProvisionQueueArgumentValuesStayInertData(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "an embedded single quote survives the heredoc delivery", want: `a'b`},
		{name: "an unbalanced quote does not derail the shell parse", want: `it's a 'value`},
		{name: "a real newline is escaped and cannot forge the terminator", want: "a\nb"},
		{name: "a newline next to a quote is still inert", want: "a\n'b"},
		{name: "command substitution stays inert data", want: "$(id)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.want)
			require.NoError(t, err, "the hostile value must be encodable as a JSON string")

			p := proxyProvisioningPlatform()
			queueArgs([2]string{testQueueArgHostile, string(raw)})(p)
			script := provisionQueueScript(t, p)
			body, outside := heredocParts(t, script)

			// STRUCTURE: one `read -r` from a quoted heredoc, and never the nested form again.
			assert.Contains(t, script, "read -r BODY <<'EOF'",
				"the body must be delivered by a single `read -r` from a QUOTED heredoc")
			assert.NotContains(t, script, "$(cat <<'EOF'",
				"the nested command-substitution heredoc is mis-parsed by bash 3.2 and must not return")
			assert.NotContains(t, body, "\n",
				"the body stays one line, so no value can forge the heredoc terminator")

			// ROUND-TRIP: the value arrives exactly as stored.
			var got struct {
				Properties map[string]string `json:"properties"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &got),
				"the heredoc body must be valid JSON (encoding/json composed it)")
			assert.Equal(t, tc.want, got.Properties[testQueueArgHostile],
				"the queue argument value must round-trip through JSON unchanged")

			// INERTNESS: the value lives only in the heredoc, never in text the shell evaluates.
			assert.NotContains(t, outside, tc.want,
				"the queue argument value must not appear anywhere the shell evaluates it")
		})
	}
}

func TestResolveCoreProvisionQueueInitOmittedWithoutProxy(t *testing.T) {
	// Provisioning configured but proxy disabled => no provision-queue init container.
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{APIURL: testProvURL}
	c := ResolveCore(p)
	_, ok := containerByName(c.InitContainers, testProvInstanceQueue)
	assert.False(t, ok, "no provision-queue init container when proxy is disabled")
}

func TestResolveCoreProvisionQueueInitOmittedWithoutProvisioning(t *testing.T) {
	// Proxy enabled but no provisioning API => no provision-queue init container.
	p := basePlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	c := ResolveCore(p)
	_, ok := containerByName(c.InitContainers, testProvInstanceQueue)
	assert.False(t, ok, "no provision-queue init container without a provisioning API")
}

// sccClean asserts a container carries the restricted-v2 SecurityContext: non-root,
// no pinned UID, drop ALL caps, seccomp RuntimeDefault, no privilege escalation.
func sccClean(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	require.NotNil(t, sc, "container %q must have a SecurityContext", c.Name)
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot, "container %q must run as non-root", c.Name)
	assert.Nil(t, sc.RunAsUser, "container %q must not pin a UID", c.Name)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation, "container %q must not allow privilege escalation", c.Name)
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "container %q must drop ALL caps", c.Name)
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "container %q seccomp", c.Name)
}

// TestBuildCoreDeploymentInitContainersAreSCCClean is the security-critical check
// for item 4: BOTH Core init containers (wait-for-auth AND the proxy-path
// provision-instance-queue) must come out restricted-v2 hardened from the common
// BuildDeployment path — no fixed UID, all caps dropped, seccomp RuntimeDefault.
func TestBuildCoreDeploymentInitContainersAreSCCClean(t *testing.T) {
	d := common.BuildDeployment(ResolveCore(proxyProvisioningPlatform()))
	inits := d.Spec.Template.Spec.InitContainers
	require.Len(t, inits, 2, "both wait-for-auth and provision-instance-queue render on the proxy path")

	wait, ok := containerByName(inits, testWaitForAuth)
	require.True(t, ok)
	sccClean(t, wait)

	provision, ok := containerByName(inits, testProvInstanceQueue)
	require.True(t, ok)
	sccClean(t, provision)
}

// TestBuildCoreDeploymentNoSecretLeakage scans the fully rendered Core Deployment
// (proxy + provisioning + trusted-cert + DB/messaging creds all set, by reference)
// and asserts NO secret material is inlined anywhere: every sensitive value reaches
// a container only via secretKeyRef, never as a literal env value or command arg.
func TestBuildCoreDeploymentNoSecretLeakage(t *testing.T) {
	p := proxyProvisioningPlatform()
	p.Spec.Common.TrustedCertificates.SecretRef = testTrustedCerts
	d := common.BuildDeployment(ResolveCore(p))

	// Walk every container (init, main, sidecar) and assert no inline env value or
	// command/arg token embeds a credential fragment. Secret-backed env carry only a
	// ValueFrom.SecretKeyRef, so their Value is empty by construction.
	ps := d.Spec.Template.Spec
	all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	forbidden := []string{"Password=", "Host=", "placeholder", "prov-secret-value"}
	for _, c := range all {
		for _, e := range c.Env {
			for _, frag := range forbidden {
				assert.NotContains(t, e.Value, frag,
					"container %q env %q must not inline secret material", c.Name, e.Name)
			}
		}
		for _, arg := range append(append([]string{}, c.Command...), c.Args...) {
			for _, frag := range forbidden {
				assert.NotContains(t, arg, frag,
					"container %q command/arg must not inline secret material", c.Name)
			}
		}
	}

	// Positive control: the API key reaches provision-instance-queue ONLY by reference.
	w := bom.Wiring()
	provision, ok := containerByName(all, testProvInstanceQueue)
	require.True(t, ok)
	keyRef, ok := initSecretEnv(provision, w.ProvisioningAPIKey.Env)
	require.True(t, ok)
	require.NotNil(t, keyRef.ValueFrom, "PROVISIONING_API_KEY must be a secretKeyRef, not an inline value")
	assert.Empty(t, keyRef.Value)
}

func TestResolveCoreProvisionQueueInitNoAPIKeyHeaderWhenKeyUnset(t *testing.T) {
	// Proxy + provisioning URL but no apiKeySecretRef: the init container still
	// renders, with no PROVISIONING_API_KEY env (the script falls back to the
	// no-header branch at runtime).
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Common.Proxy = otilmv1alpha1.OutboundProxySpec{Enabled: true}
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{APIURL: testProvURL}
	c := ResolveCore(p)

	init, ok := containerByName(c.InitContainers, testProvInstanceQueue)
	require.True(t, ok, "provision-queue init renders even without an API key")
	_, hasKey := initSecretEnv(init, w.ProvisioningAPIKey.Env)
	assert.False(t, hasKey, "no PROVISIONING_API_KEY env when apiKeySecretRef is unset")
}

func TestResolveCoreEphemeralVolumeAndMount(t *testing.T) {
	c := ResolveCore(basePlatform())
	var vol *corev1.Volume
	for i := range c.Volumes {
		if c.Volumes[i].Name == ephemeralVolumeName {
			vol = &c.Volumes[i]
		}
	}
	require.NotNil(t, vol, testEphemeralMsg)
	require.NotNil(t, vol.EmptyDir, "ephemeral volume is an emptyDir")
	assert.Equal(t, corev1.StorageMediumMemory, vol.EmptyDir.Medium)

	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == ephemeralVolumeName {
			mount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, mount, "ephemeral volume mount must be present")
	assert.Equal(t, "/tmp", mount.MountPath)
}

func TestResolveCoreProbes(t *testing.T) {
	c := ResolveCore(basePlatform())
	// Core has NO liveness probe — consistent with every other component (httpProbes
	// omits it) and with the chart (image.probes.liveness.enabled=false). A liveness
	// probe would risk killing Core mid-migration; readiness + startup are sufficient.
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)

	require.NotNil(t, c.Probes.Readiness)
	require.NotNil(t, c.Probes.Readiness.HTTPGet)
	assert.Equal(t, "/api/v1/health/readiness", c.Probes.Readiness.HTTPGet.Path)

	require.NotNil(t, c.Probes.Startup)
	require.NotNil(t, c.Probes.Startup.HTTPGet)
	assert.Equal(t, "/api/v1/health/liveness", c.Probes.Startup.HTTPGet.Path, "chart uses liveness path for startup")
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

func TestResolveCoreNoSecretRefsStillNoSecretEnv(t *testing.T) {
	// Extends the existing no-secret-refs test: with all optional secret refs
	// cleared, SecretEnv stays empty.
	p := basePlatform()
	p.Spec.Database.Credentials = nil
	p.Spec.Messaging.Credentials = nil
	p.Spec.Common.TrustedCertificates.SecretRef = ""
	p.Spec.Provisioning = nil
	c := ResolveCore(p)
	assert.Empty(t, c.SecretEnv)
}

// ---- scheduler ----

func TestResolveSchedulerBasics(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	assert.Equal(t, schedulerName, c.Name)
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
}

func TestResolveSchedulerImage(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/scheduler:1.1.0", c.Image)
	assert.Equal(t, "IfNotPresent", string(c.PullPolicy))
}

func TestResolveSchedulerInlineEnv(t *testing.T) {
	p := basePlatform()
	c := ResolveScheduler(p)

	port, ok := envValue(c.Env, "PORT")
	require.True(t, ok)
	assert.Equal(t, "8080", port)

	logLevel, ok := envValue(c.Env, "LOGGING_LEVEL_COM_CZERTAINLY")
	require.True(t, ok)
	assert.Equal(t, "INFO", logLevel)

	// JAVA_OPTS is no longer auto-injected — it is plain per-component env now.
	_, ok = envValue(c.Env, "JAVA_OPTS")
	assert.False(t, ok, "scheduler must not auto-inject JAVA_OPTS")

	// DB URL is inline.
	dbURL, ok := envValue(c.Env, "JDBC_URL")
	require.True(t, ok)
	assert.Equal(t, "jdbc:postgresql://db.example.com:5432/ilmdb?characterEncoding=UTF-8", dbURL)

	// Messaging vhost is inline.
	w := bom.Wiring()
	vhost, ok := envValue(c.Env, w.MessagingVHostEnv)
	require.True(t, ok)
	assert.Equal(t, "ilm", vhost)
}

func TestResolveSchedulerBrokerFromConfigMap(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	w := bom.Wiring()

	host, ok := configMapEnvFor(c.ConfigMapEnv, w.MessagingHostEnv)
	require.True(t, ok, "BROKER_HOST must be configmap-backed")
	assert.Equal(t, w.MessagingConfigMapName, host.ConfigMapName)
	assert.Equal(t, w.MessagingHostKey, host.ConfigMapKey)

	port, ok := configMapEnvFor(c.ConfigMapEnv, w.MessagingPortEnv)
	require.True(t, ok, "BROKER_PORT must be configmap-backed")
	assert.Equal(t, w.MessagingConfigMapName, port.ConfigMapName)
	assert.Equal(t, w.MessagingPortKey, port.ConfigMapKey)
}

func TestResolveSchedulerCredentialsAreSecretBacked(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	w := bom.Wiring()

	dbUser, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.UsernameEnv)
	require.True(t, ok, "db username must be secret-backed")
	assert.Equal(t, testDBCreds, dbUser.SecretName)

	dbPass, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.PasswordEnv)
	require.True(t, ok, "db password must be secret-backed")
	assert.Equal(t, testDBCreds, dbPass.SecretName)

	mqUser, ok := secretEnvFor(c.SecretEnv, w.MessagingCred.UsernameEnv)
	require.True(t, ok, "messaging username must be secret-backed")
	assert.Equal(t, testMQCreds, mqUser.SecretName)

	mqPass, ok := secretEnvFor(c.SecretEnv, w.MessagingCred.PasswordEnv)
	require.True(t, ok, "messaging password must be secret-backed")
	assert.Equal(t, testMQCreds, mqPass.SecretName)

	// Credentials must never appear inline.
	for _, name := range []string{w.DatabaseCred.UsernameEnv, w.DatabaseCred.PasswordEnv,
		w.MessagingCred.UsernameEnv, w.MessagingCred.PasswordEnv} {
		_, inline := envValue(c.Env, name)
		assert.False(t, inline, "%s must not be inline", name)
	}
}

func TestResolveSchedulerInitContainer(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	init, ok := containerByName(c.InitContainers, testWaitForMessaging)
	require.True(t, ok, "wait-for-messaging-service init container must be present")
	assert.Equal(t, testCurlImage, init.Image)
	require.Len(t, init.Command, 3)
	script := init.Command[2]
	// The broker coordinates arrive as env values and are read back QUOTED.
	assert.Contains(t, script, testMQWaitLoop)
	host, ok := initEnvValue(init, mqWaitHostEnv)
	require.True(t, ok, "the broker host must be passed as an env value")
	assert.Equal(t, testMQHost, host)
	port, ok := initEnvValue(init, mqWaitPortEnv)
	require.True(t, ok, "the broker port must be passed as an env value")
	assert.Equal(t, "5672", port)
}

func TestResolveSchedulerProbes(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)
	require.NotNil(t, c.Probes.Readiness)
	assert.Equal(t, "/health/readiness", c.Probes.Readiness.HTTPGet.Path)
	require.NotNil(t, c.Probes.Startup)
	assert.Equal(t, "/health/liveness", c.Probes.Startup.HTTPGet.Path)
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

func TestResolveSchedulerEphemeralVolume(t *testing.T) {
	c := ResolveScheduler(basePlatform())
	var found bool
	for _, v := range c.Volumes {
		if v.Name == ephemeralVolumeName {
			found = true
			assert.NotNil(t, v.EmptyDir)
		}
	}
	assert.True(t, found, testEphemeralMsg)

	var mountFound bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" {
			mountFound = true
		}
	}
	assert.True(t, mountFound, "/tmp volume mount must be present")
}

// ---- utils ----

func TestResolveUtilsBasics(t *testing.T) {
	c := ResolveUtils(basePlatform())
	assert.Equal(t, "utils", c.Name)
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
}

func TestResolveUtilsImage(t *testing.T) {
	c := ResolveUtils(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/utils-service:1.0.2", c.Image)
}

func TestResolveUtilsEnv(t *testing.T) {
	p := basePlatform()
	c := ResolveUtils(p)

	port, ok := envValue(c.Env, "PORT")
	require.True(t, ok)
	assert.Equal(t, "8080", port)

	logLevel, ok := envValue(c.Env, "LOG_LEVEL")
	require.True(t, ok)
	assert.Equal(t, "INFO", logLevel)

	// JAVA_OPTS is no longer auto-injected — it is plain per-component env now.
	_, ok = envValue(c.Env, "JAVA_OPTS")
	assert.False(t, ok, "utils must not auto-inject JAVA_OPTS")

	// No DB / messaging env.
	assert.Empty(t, c.SecretEnv, "utils has no DB/messaging secrets")
	assert.Empty(t, c.ConfigMapEnv, "utils has no configmap env")
}

func TestResolveUtilsProbes(t *testing.T) {
	c := ResolveUtils(basePlatform())
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)
	require.NotNil(t, c.Probes.Readiness)
	assert.Equal(t, "/health/readiness", c.Probes.Readiness.HTTPGet.Path)
	require.NotNil(t, c.Probes.Startup)
	assert.Equal(t, "/health/liveness", c.Probes.Startup.HTTPGet.Path)
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

func TestResolveUtilsEphemeralVolume(t *testing.T) {
	c := ResolveUtils(basePlatform())
	var found bool
	for _, v := range c.Volumes {
		if v.Name == ephemeralVolumeName {
			found = true
		}
	}
	assert.True(t, found, testEphemeralMsg)
	assert.Empty(t, c.InitContainers, "utils has no init containers")
}

// ---- auth-opa-policies ----

func TestResolveAuthOpaPoliciesBasics(t *testing.T) {
	c := ResolveAuthOpaPolicies(basePlatform())
	assert.Equal(t, authOPAPoliciesName, c.Name)
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
}

func TestResolveAuthOpaPoliciesImage(t *testing.T) {
	c := ResolveAuthOpaPolicies(basePlatform())
	// auth-opa-policies uses the configured policies-server image (auth-opa-policies),
	// distinct from the bare OPA-engine sidecar (opa) that runs alongside Core.
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/auth-opa-policies:1.4.1", c.Image)
}

func TestResolveAuthOpaPoliciesNoEnv(t *testing.T) {
	c := ResolveAuthOpaPolicies(basePlatform())
	assert.Empty(t, c.Env, "auth-opa-policies has no env vars (nginx, static bundle)")
	assert.Empty(t, c.SecretEnv)
	assert.Empty(t, c.ConfigMapEnv)
	assert.Empty(t, c.InitContainers)
}

func TestResolveAuthOpaPoliciesVolumes(t *testing.T) {
	c := ResolveAuthOpaPolicies(basePlatform())

	// Must have an ephemeral volume.
	var found bool
	for _, v := range c.Volumes {
		if v.Name == ephemeralVolumeName {
			found = true
			assert.NotNil(t, v.EmptyDir)
		}
	}
	require.True(t, found, testEphemeralMsg)

	// Must have both /var/cache/nginx and /tmp mounts.
	mountPaths := make(map[string]bool)
	for _, m := range c.VolumeMounts {
		mountPaths[m.MountPath] = true
	}
	assert.True(t, mountPaths["/var/cache/nginx"], "/var/cache/nginx mount must be present")
	assert.True(t, mountPaths["/tmp"], "/tmp mount must be present")
}

func TestResolveAuthOpaPoliciesProbes(t *testing.T) {
	c := ResolveAuthOpaPolicies(basePlatform())
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)
	require.NotNil(t, c.Probes.Readiness)
	assert.Equal(t, "/index.html", c.Probes.Readiness.HTTPGet.Path)
	require.NotNil(t, c.Probes.Startup)
	assert.Equal(t, "/index.html", c.Probes.Startup.HTTPGet.Path)
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

// ---- auth ----------------------------------------------------------

func TestResolveAuthBasics(t *testing.T) {
	c := ResolveAuth(basePlatform())
	assert.Equal(t, authName, c.Name, "the workload/Service role is auth")
	assert.Equal(t, "auth", c.MainContainerName(), "the main container is named 'auth'")
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
}

func TestResolveAuthImage(t *testing.T) {
	c := ResolveAuth(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/auth:1.6.3", c.Image)
}

func TestResolveAuthInlineEnv(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Auth.Create.CreateUnknownUsers = true
	p.Spec.Auth.Create.CreateUnknownRoles = true
	p.Spec.Auth.SyncPolicy = "sync-data"
	c := ResolveAuth(p)

	users, ok := envValue(c.Env, w.AuthCreateUsersEnv)
	require.True(t, ok)
	assert.Equal(t, "true", users)
	roles, ok := envValue(c.Env, w.AuthCreateRolesEnv)
	require.True(t, ok)
	assert.Equal(t, "true", roles)
	sync, ok := envValue(c.Env, w.AuthSyncPolicyEnv)
	require.True(t, ok)
	assert.Equal(t, "sync-data", sync)
	urls, ok := envValue(c.Env, w.AuthAspNetURLsEnv)
	require.True(t, ok)
	assert.Equal(t, "http://+:8080", urls, "ASPNETCORE_URLS binds Kestrel to the HTTP port")
}

func TestResolveAuthEnvDefaults(t *testing.T) {
	w := bom.Wiring()
	c := ResolveAuth(basePlatform())

	users, _ := envValue(c.Env, w.AuthCreateUsersEnv)
	roles, _ := envValue(c.Env, w.AuthCreateRolesEnv)
	sync, _ := envValue(c.Env, w.AuthSyncPolicyEnv)
	assert.Equal(t, "false", users, "createUnknownUsers defaults to false")
	assert.Equal(t, "false", roles, "createUnknownRoles defaults to false")
	assert.Equal(t, "create-only", sync, "syncPolicy defaults to create-only")
}

// TestResolveAuthConnectionStringIsSecretBacked is the security-critical
// assertion: the DB connection string is referenced via secretKeyRef from the
// operator-managed Secret (auth-db) and never appears inline anywhere in
// the rendered component.
func TestResolveAuthConnectionStringIsSecretBacked(t *testing.T) {
	w := bom.Wiring()
	c := ResolveAuth(basePlatform())

	ref, ok := secretEnvFor(c.SecretEnv, w.AuthDBConnEnv)
	require.True(t, ok, "AUTH_DB_CONNECTION_STRING must be secret-backed")
	assert.Equal(t, "auth-db", ref.SecretName, "must reference the operator-managed Secret")
	assert.Equal(t, w.AuthDBSecretName, ref.SecretName)
	assert.Equal(t, "connection-string", ref.SecretKey)
	assert.Equal(t, w.AuthDBSecretKey, ref.SecretKey)

	// The connection string must NEVER appear as an inline env value, and no inline
	// env value may contain a connection-string fragment (Host=/Password=).
	_, inline := envValue(c.Env, w.AuthDBConnEnv)
	assert.False(t, inline, "AUTH_DB_CONNECTION_STRING must not be inline")
	for _, e := range c.Env {
		assert.NotContains(t, e.Value, "Host=", "no inline env may embed a connection string (%s)", e.Name)
		assert.NotContains(t, e.Value, "Password=", "no inline env may embed a password (%s)", e.Name)
	}
}

func TestResolveAuthProbes(t *testing.T) {
	c := ResolveAuth(basePlatform())
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)
	require.NotNil(t, c.Probes.Readiness)
	assert.Equal(t, "/health", c.Probes.Readiness.HTTPGet.Path)
	require.NotNil(t, c.Probes.Startup)
	assert.Equal(t, "/health", c.Probes.Startup.HTTPGet.Path)
	assert.Equal(t, int32(45), c.Probes.Startup.FailureThreshold)
}

func TestResolveAuthTrustedCertificatesVolumeWhenSet(t *testing.T) {
	p := basePlatform()
	p.Spec.Common.TrustedCertificates.SecretRef = testTrustedCerts
	c := ResolveAuth(p)

	var vol *corev1.Volume
	for i := range c.Volumes {
		if c.Volumes[i].Name == trustedCertsVolume {
			vol = &c.Volumes[i]
		}
	}
	require.NotNil(t, vol, "trusted-certificates volume must be present when secretRef set")
	require.NotNil(t, vol.Secret)
	assert.Equal(t, testTrustedCerts, vol.Secret.SecretName, "mounts the user-provided Secret")

	var mountPath string
	var readOnly bool
	for _, m := range c.VolumeMounts {
		if m.Name == trustedCertsVolume {
			mountPath = m.MountPath
			readOnly = m.ReadOnly
		}
	}
	assert.Equal(t, "/etc/ssl/certs", mountPath)
	assert.True(t, readOnly, "trusted-certificates must be mounted read-only")
}

func TestResolveAuthNoTrustedCertificatesByDefault(t *testing.T) {
	c := ResolveAuth(basePlatform())
	for _, v := range c.Volumes {
		assert.NotEqual(t, trustedCertsVolume, v.Name, "no trusted-certs volume without secretRef")
	}
	// Without a trusted-certs secretRef the only volume/mount is the /tmp ephemeral
	// volume that backs the read-only root (added unconditionally for .NET).
	for _, m := range c.VolumeMounts {
		assert.NotEqual(t, trustedCertsVolume, m.Name, "no trusted-certs mount without secretRef")
	}
	require.Len(t, c.VolumeMounts, 1, "only the /tmp ephemeral mount without a trusted-certs secretRef")
	assert.Equal(t, ephemeralVolumeName, c.VolumeMounts[0].Name)
	assert.Equal(t, tmpMountPath, c.VolumeMounts[0].MountPath)
}

// TestResolveAuthReadOnlyRootWithTmp asserts auth (.NET) enables a
// read-only root and carries the writable in-memory /tmp ephemeral volume + the
// TMPDIR=/tmp env that pins the runtime's temp dir there (residual b, closed e2e).
func TestResolveAuthReadOnlyRootWithTmp(t *testing.T) {
	c := ResolveAuth(basePlatform())

	require.NotNil(t, c.ReadOnlyRootFilesystem, "auth must opt into a read-only root")
	assert.True(t, *c.ReadOnlyRootFilesystem)

	var ephemeral *corev1.Volume
	for i := range c.Volumes {
		if c.Volumes[i].Name == ephemeralVolumeName {
			ephemeral = &c.Volumes[i]
		}
	}
	require.NotNil(t, ephemeral, "the /tmp ephemeral volume must be present")
	require.NotNil(t, ephemeral.EmptyDir, "the /tmp volume is an in-memory emptyDir")
	assert.Equal(t, corev1.StorageMediumMemory, ephemeral.EmptyDir.Medium)

	var tmpMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == ephemeralVolumeName {
			tmpMount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, tmpMount, "the /tmp ephemeral volume must be mounted")
	assert.Equal(t, tmpMountPath, tmpMount.MountPath)

	tmpdir, ok := envValue(c.Env, "TMPDIR")
	require.True(t, ok, "TMPDIR must be set so the .NET runtime writes to the writable /tmp")
	assert.Equal(t, tmpMountPath, tmpdir)
}

// TestJVMMainContainersEnableReadOnlyRoot asserts the JVM app components (core,
// scheduler, utils) opt into a read-only root filesystem and each
// carries the in-memory /tmp ephemeral volume + mount that backs it (residual b,
// validated end-to-end on Kind). auth is covered separately above (it needs
// an added /tmp + TMPDIR for the .NET runtime).
func TestJVMMainContainersEnableReadOnlyRoot(t *testing.T) {
	p := basePlatform()
	p.Spec.Utils.Enabled = true
	for _, tc := range []struct {
		name string
		c    common.Component
	}{
		{"core", ResolveCore(p)},
		{"scheduler", ResolveScheduler(p)},
		{"utils", ResolveUtils(p)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.c.ReadOnlyRootFilesystem, "%s must opt into a read-only root", tc.name)
			assert.True(t, *tc.c.ReadOnlyRootFilesystem)

			var tmpMount *corev1.VolumeMount
			for i := range tc.c.VolumeMounts {
				if tc.c.VolumeMounts[i].MountPath == tmpMountPath {
					tmpMount = &tc.c.VolumeMounts[i]
				}
			}
			require.NotNil(t, tmpMount, "%s must mount a writable /tmp under a read-only root", tc.name)
			assert.Equal(t, ephemeralVolumeName, tmpMount.Name)
		})
	}
}

// ---- fe-administrator ------------------------------------------------------

func TestResolveFeAdministratorBasics(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())
	assert.Equal(t, feAdministratorName, c.Name)
	assert.Equal(t, feAdministratorName, c.MainContainerName(), "the container is named fe-administrator")
	assert.Equal(t, testNamespace, c.Namespace)
	assert.Equal(t, int32(8080), c.Port)
	assert.Equal(t, int32(1), c.Replicas)
}

func TestResolveFeAdministratorImage(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/frontend-administrator:2.18.0", c.Image)
}

func TestResolveFeAdministratorNoEnv(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())
	assert.Empty(t, c.Env, "fe-administrator config is served from config.js, not env")
	assert.Empty(t, c.SecretEnv)
	assert.Empty(t, c.ConfigMapEnv)
	assert.Empty(t, c.InitContainers)
}

func TestResolveFeAdministratorConfigMount(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())

	// config.js mounted via subPath from the config volume.
	var configMount *corev1.VolumeMount
	mountPaths := map[string]bool{}
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		mountPaths[m.MountPath] = true
		if m.Name == feConfigVolume {
			configMount = m
		}
	}
	require.NotNil(t, configMount, "config volume mount must be present")
	assert.Equal(t, "/usr/share/nginx/html/config.js", configMount.MountPath)
	assert.Equal(t, testConfigJS, configMount.SubPath, "config.js mounts as a single file via subPath")

	// nginx writable paths backed by the ephemeral volume.
	assert.True(t, mountPaths["/var/cache/nginx"], "/var/cache/nginx must be mounted")
	assert.True(t, mountPaths["/tmp"], "/tmp must be mounted")
}

func TestResolveFeAdministratorVolumes(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())

	var configVol, ephemeral *corev1.Volume
	for i := range c.Volumes {
		switch c.Volumes[i].Name {
		case feConfigVolume:
			configVol = &c.Volumes[i]
		case ephemeralVolumeName:
			ephemeral = &c.Volumes[i]
		}
	}
	require.NotNil(t, configVol, "config volume must be present")
	require.NotNil(t, configVol.ConfigMap, "config volume must be ConfigMap-backed")
	assert.Equal(t, feConfigMapName, configVol.ConfigMap.Name)
	require.Len(t, configVol.ConfigMap.Items, 1, "config volume projects config.js as a single item")
	assert.Equal(t, testConfigJS, configVol.ConfigMap.Items[0].Key)
	assert.Equal(t, testConfigJS, configVol.ConfigMap.Items[0].Path)

	require.NotNil(t, ephemeral, testEphemeralMsg)
	require.NotNil(t, ephemeral.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, ephemeral.EmptyDir.Medium)
}

func TestResolveFeAdministratorProbes(t *testing.T) {
	c := ResolveFeAdministrator(basePlatform())
	assert.Nil(t, c.Probes.Liveness, testLivenessMsg)
	require.NotNil(t, c.Probes.Readiness)
	assert.Equal(t, "/", c.Probes.Readiness.HTTPGet.Path)
	require.NotNil(t, c.Probes.Startup)
	assert.Equal(t, "/", c.Probes.Startup.HTTPGet.Path)
}

// --- Typed-infra Secret key-mapping (the maintainer's Q2 concern) ----------------
//
// These tests prove a user's in-Secret key overrides flow onto the rendered secretKeyRef
// SecretKey while the OUTPUT env-var names stay BOM application contracts; and that
// unmapped fields fall back to the BOM default keys.

// TestResolveCoreDatabaseCredentialsMappedKeys: a custom usernameKey/passwordKey on the
// external database credentials flows to Core's JDBC_USERNAME/JDBC_PASSWORD secretKeyRef
// SecretKey (the env-var names stay the BOM contract).
func TestResolveCoreDatabaseCredentialsMappedKeys(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Database.Credentials = &otilmv1alpha1.CredentialsRef{
		SecretRef: testDBCreds, UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD",
	}
	c := ResolveCore(p)

	user, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.UsernameEnv)
	require.True(t, ok)
	assert.Equal(t, "JDBC_USERNAME", w.DatabaseCred.UsernameEnv, "output env name stays the BOM contract")
	assert.Equal(t, "POSTGRES_USER", user.SecretKey, "input key is the user's mapping")

	pass, ok := secretEnvFor(c.SecretEnv, w.DatabaseCred.PasswordEnv)
	require.True(t, ok)
	assert.Equal(t, "POSTGRES_PASSWORD", pass.SecretKey)
}

// TestResolveCoreTrustedCertsMappedKey: spec.trustedCertificates.caKey maps Core's
// TRUSTED_CERTIFICATES secretKeyRef SecretKey (no admin composition active).
func TestResolveCoreTrustedCertsMappedKey(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: testCABundle, CAKey: testTLSCA}
	c := ResolveCore(p)
	tc, ok := secretEnvFor(c.SecretEnv, w.TrustedCertificates.Env)
	require.True(t, ok, "TRUSTED_CERTIFICATES must be secret-backed when set")
	assert.Equal(t, testCABundle, tc.SecretName)
	assert.Equal(t, testTLSCA, tc.SecretKey, "the mapped caKey drives the secretKeyRef input key")
}

// TestResolveCoreTrustedCertsDefaultKey: an unmapped trustedCertificates falls back to the
// BOM default key (ca.crt).
func TestResolveCoreTrustedCertsDefaultKey(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: testCABundle}
	c := ResolveCore(p)
	tc, ok := secretEnvFor(c.SecretEnv, w.TrustedCertificates.Env)
	require.True(t, ok)
	assert.Equal(t, "ca.crt", tc.SecretKey)
}

// TestResolveCoreProvisioningAPIKeyMappedKey: spec.provisioning.apiKey maps Core's
// PROVISIONING_API_KEY secretKeyRef SecretKey.
func TestResolveCoreProvisioningAPIKeyMappedKey(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
		APIURL: testProvURL, APIKeySecretRef: testProvSecret, APIKey: "x-api-key",
	}
	c := ResolveCore(p)
	key, ok := secretEnvFor(c.SecretEnv, w.ProvisioningAPIKey.Env)
	require.True(t, ok)
	assert.Equal(t, "x-api-key", key.SecretKey, "the mapped apiKey drives the secretKeyRef input key")
}

// TestResolveCoreAdminCertProvidedMappedKey: spec.registerAdmin.certKey maps Core's
// ADMIN_CERT secretKeyRef SecretKey for source=provided.
func TestResolveCoreAdminCertProvidedMappedKey(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{
		Enabled:     true,
		Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "provided", SecretRef: strPtr(testAdminCert), CertKey: testClientCrt},
	}
	c := ResolveCore(p)
	ac, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	require.True(t, ok)
	assert.Equal(t, testClientCrt, ac.SecretKey, "the mapped certKey drives ADMIN_CERT's input key")
}

// TestResolveCoreAdminCertGeneratedIgnoresMappedKey: for source=generated the cert-manager
// Secret uses the standard tls.crt key — a stray user certKey override must NOT apply.
func TestResolveCoreAdminCertGeneratedIgnoresMappedKey(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated", CertKey: testClientCrt}}
	c := ResolveCore(p)
	ac, ok := secretEnvFor(c.SecretEnv, w.AdminCert.Env)
	require.True(t, ok)
	assert.Equal(t, testTLSCrt, ac.SecretKey, "source=generated keeps the cert-manager kubernetes.io/tls key")
}

// TestTrustedCertsSecretKeyComposedUsesOutputKey: when the operator composes its own bundle
// Secret (admin source=generated), Core reads the fixed OUTPUT key regardless of any user
// input mapping — so the env name+key still line up with the composed Secret.
func TestTrustedCertsSecretKeyComposedUsesOutputKey(t *testing.T) {
	p := basePlatform()
	// caKey set on the user input, but composition is active (generated admin CA), so Core
	// must read the composed Secret's OUTPUT key (ca.crt), not the user's input mapping.
	p.Spec.Common.TrustedCertificates = otilmv1alpha1.TrustedCertificatesSpec{SecretRef: testCABundle, CAKey: testTLSCA}
	p.Spec.RegisterAdmin = &otilmv1alpha1.RegisterAdminSpec{Enabled: true, Certificate: &otilmv1alpha1.AdminCertificateSpec{Enabled: boolPtr(true), Source: "generated"}}
	require.True(t, ComposesTrustedCerts(p), "precondition: admin source=generated composes the bundle")
	assert.Equal(t, TrustedCertsBundleKey(), TrustedCertsSecretKey(p),
		"composed bundle is read under the fixed OUTPUT key, not the user input mapping")
	// The user's input mapping still governs the READ of the user Secret during composition.
	assert.Equal(t, testTLSCA, TrustedCertsInputKey(p))
}

// TestComponentPullSecretsUnion proves per-component pull secrets reach the pod alongside the
// shared ones (they were silently DROPPED before the union fix) for EVERY component whose
// resolver merges them — one case per resolver, so a newly added component that forgets the
// union is not covered by a core-only test. The shared list comes first (order-preserving
// union), and duplicates collapse.
func TestComponentPullSecretsUnion(t *testing.T) {
	const shared, own = "shared-cred", "own-cred"
	tests := []struct {
		name    string
		set     func(*otilmv1alpha1.Platform, []string)
		resolve func(*otilmv1alpha1.Platform) common.Component
	}{
		{
			name:    "core",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.Core.Image.PullSecrets = s },
			resolve: ResolveCore,
		},
		{
			name:    "scheduler",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.Scheduler.Image.PullSecrets = s },
			resolve: ResolveScheduler,
		},
		{
			name:    "utils",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.Utils.Image.PullSecrets = s },
			resolve: ResolveUtils,
		},
		{
			name:    "auth-opa-policies",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.AuthOpaPolicies.Image.PullSecrets = s },
			resolve: ResolveAuthOpaPolicies,
		},
		{
			name:    "auth",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.Auth.Image.PullSecrets = s },
			resolve: ResolveAuth,
		},
		{
			name:    "fe-administrator",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.FeAdministrator.Image.PullSecrets = s },
			resolve: ResolveFeAdministrator,
		},
		{
			name:    "gateway",
			set:     func(p *otilmv1alpha1.Platform, s []string) { p.Spec.Gateway.Image.PullSecrets = s },
			resolve: ResolveGateway,
		},
		{
			name: "provisioning",
			set: func(p *otilmv1alpha1.Platform, s []string) {
				p.Spec.Provisioning = &otilmv1alpha1.ProvisioningSpec{
					Mode: "deploy",
					Deploy: &otilmv1alpha1.ProvisioningDeploySpec{
						BootstrapSecretRef: "prov-bootstrap",
						ComponentSpec: otilmv1alpha1.ComponentSpec{
							Image: otilmv1alpha1.ImageSpec{PullSecrets: s},
						},
					},
				}
			},
			resolve: ResolveProvisioning,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Shared + per-component: the union carries both, shared first.
			p := basePlatform()
			p.Spec.Common.Image.PullSecrets = []string{shared}
			tc.set(p, []string{own})
			assert.Equal(t, []string{shared, own}, tc.resolve(p).PullSecrets,
				"%s must union the shared and per-component pull secrets", tc.name)

			// Per-component only: it still reaches the pod.
			only := basePlatform()
			tc.set(only, []string{own})
			assert.Equal(t, []string{own}, tc.resolve(only).PullSecrets,
				"%s must carry a per-component-only pull secret", tc.name)

			// A duplicate collapses to one entry.
			dup := basePlatform()
			dup.Spec.Common.Image.PullSecrets = []string{shared}
			tc.set(dup, []string{shared})
			assert.Equal(t, []string{shared}, tc.resolve(dup).PullSecrets,
				"%s must not duplicate a pull secret listed twice", tc.name)

			// Neither set: nil, so rendered pods stay byte-identical when none are configured.
			// set(nil) still installs whatever scaffolding the resolver requires (the
			// provisioning deploy block), just with no pull secrets.
			none := basePlatform()
			tc.set(none, nil)
			assert.Nil(t, tc.resolve(none).PullSecrets,
				"%s must render no pull secrets when none are configured", tc.name)
		})
	}
}
