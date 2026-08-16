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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveMessagingConnectionExternalUnchanged(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", Host: testExternalMQHost, Port: 5673, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testILMMQ},
			},
		},
	}
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, testExternalMQHost, conn.Host, "external host comes straight from the spec")
	assert.Equal(t, int32(5673), conn.Port)
	assert.Equal(t, "ilm", conn.VirtualHost)
	assert.Equal(t, testILMMQ, conn.CredentialsSecretName)
	assert.Empty(t, conn.ProvisioningCredentialsSecretName, "external mode has no managed provisioner Secret")
}

func TestResolveMessagingConnectionManagedResolvesToRabbitMQ(t *testing.T) {
	p := managedMQPlatform(nil) // name "ilm", no virtualHost → the pinned 2.18.0 bundle's czertainly
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "ilm-messaging", conn.Host, "managed host is the RabbitMQ client Service <cluster>")
	assert.Equal(t, int32(5672), conn.Port)
	assert.Equal(t, "czertainly", conn.VirtualHost, "the 2.18.0 managed vhost is czertainly")
	assert.Equal(t, "ilm-messaging-core-user-credentials", conn.CredentialsSecretName,
		"managed creds come from the Topology-generated Core-user Secret")
	assert.Equal(t, "ilm-messaging-provisioner-user-credentials", conn.ProvisioningCredentialsSecretName,
		"managed provisioning creds come from the provisioner-user Secret")
}

func TestResolveMessagingConnectionManagedHonoursVirtualHost(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Messaging.VirtualHost = "custom-vh" })
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "custom-vh", conn.VirtualHost, "a configured virtualHost is honoured in managed mode")
}

// TestManagedMessagingWiresCoreThroughGeneratedSecret proves the mode-agnostic readback
// threads the managed Core-user Secret into Core's BROKER_* secretKeyRef wiring — exactly
// the same code path external mode uses, only the Secret source differs. The credential is
// referenced (secretKeyRef), never inlined.
func TestManagedMessagingWiresCoreThroughGeneratedSecret(t *testing.T) {
	p := managedMQPlatform(nil)
	core := ResolveCore(p)

	want := "ilm-messaging-core-user-credentials"
	var userRef, passRef string
	for _, ref := range core.SecretEnv {
		switch ref.EnvVar {
		case "BROKER_USERNAME":
			userRef = ref.SecretName
		case "BROKER_PASSWORD":
			passRef = ref.SecretName
		}
	}
	assert.Equal(t, want, userRef, "Core's BROKER_USERNAME is sourced from the managed Core-user Secret")
	assert.Equal(t, want, passRef, "Core's BROKER_PASSWORD is sourced from the managed Core-user Secret")

	// The managed broker host/vhost flow through Core's ConfigMap-backed + inline env too.
	cm := BuildMessagingConfigMap(p)
	assert.Equal(t, "ilm-messaging", cm.Data["messaging.host"], "the ConfigMap publishes the managed broker Service host")
	assert.Equal(t, "5672", cm.Data["messaging.amqp.port"])
}

// TestExternalMessagingWiringUnchanged proves external-mode Core wiring is unchanged by the
// managed work: BROKER_* still come from the caller's Secret.
func TestExternalMessagingWiringUnchanged(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", Host: "mq.example.com", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testILMMQ},
			},
		},
	}
	core := ResolveCore(p)
	for _, ref := range core.SecretEnv {
		if ref.EnvVar == "BROKER_USERNAME" || ref.EnvVar == "BROKER_PASSWORD" {
			assert.Equal(t, testILMMQ, ref.SecretName, "external Core still uses the caller's broker Secret")
		}
	}
	cm := BuildMessagingConfigMap(p)
	assert.Equal(t, "mq.example.com", cm.Data["messaging.host"], "external host comes straight from the spec")
}

// TestResolveMessagingConnectionDefaultKeysWhenUnmapped proves an external broker Secret
// without key overrides resolves to the BOM default keys (username/password).
func TestResolveMessagingConnectionDefaultKeysWhenUnmapped(t *testing.T) {
	p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Messaging: otilmv1alpha1.MessagingSpec{
		Mode: "external", Host: "mq", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"},
	}}}
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "username", conn.UsernameKey)
	assert.Equal(t, "password", conn.PasswordKey)
}

// TestResolveMessagingConnectionMappedKeysFlowToBrokerSecretKeyRef proves a user's broker
// in-Secret key overrides land on the resolved connection AND on Core's BROKER_USERNAME/
// BROKER_PASSWORD secretKeyRef SecretKey — while the OUTPUT env-var names stay BOM contracts.
func TestResolveMessagingConnectionMappedKeysFlowToBrokerSecretKeyRef(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{Messaging: otilmv1alpha1.MessagingSpec{
			Mode: "external", Host: "mq", Port: 5672, VirtualHost: "ilm",
			Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "rmq", UsernameKey: "RABBITMQ_USER", PasswordKey: "RABBITMQ_PASS"},
		}},
	}
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "RABBITMQ_USER", conn.UsernameKey)
	assert.Equal(t, "RABBITMQ_PASS", conn.PasswordKey)

	w := bom.Wiring()
	core := ResolveCore(p)
	var userKey, passKey string
	for _, ref := range core.SecretEnv {
		switch ref.EnvVar {
		case w.MessagingCred.UsernameEnv:
			userKey = ref.SecretKey
		case w.MessagingCred.PasswordEnv:
			passKey = ref.SecretKey
		}
	}
	assert.Equal(t, "RABBITMQ_USER", userKey, "the mapped INPUT key drives Core's BROKER_USERNAME secretKeyRef")
	assert.Equal(t, "RABBITMQ_PASS", passKey, "the mapped INPUT key drives Core's BROKER_PASSWORD secretKeyRef")
}

// TestResolveMessagingConnectionManagedIgnoresUserKeys proves a managed broker ALWAYS uses
// the Topology Operator's generated-Secret keys (BOM defaults), even with a stray user mapping.
func TestResolveMessagingConnectionManagedIgnoresUserKeys(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Messaging.Credentials = &otilmv1alpha1.CredentialsRef{UsernameKey: "RABBITMQ_USER", PasswordKey: "RABBITMQ_PASS"}
	})
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "username", conn.UsernameKey, "managed keeps the Topology-generated username key")
	assert.Equal(t, "password", conn.PasswordKey, "managed keeps the Topology-generated password key")
}

// TestResolveMessagingConnectionManagedAdministratorCredentials proves a managed broker on the
// pinned 2.18.0 bundle exposes the Topology-generated administrator-user Secret name and the
// management API endpoint the queue-depth poll authenticates against.
func TestResolveMessagingConnectionManagedAdministratorCredentials(t *testing.T) {
	p := managedMQPlatform(nil)
	conn := ResolveMessagingConnection(p)
	assert.Equal(t, "ilm-messaging-administrator-user-credentials", conn.AdministratorCredentialsSecretName,
		"managed administrator creds come from the Topology-generated administrator-user Secret")
	assert.Equal(t, "http://ilm-messaging.ns.svc:15672", ManagedMessagingManagementEndpoint(p),
		"the management endpoint targets the broker client Service's management port")
}

// TestResolveMessagingConnectionExternalHasNoAdministratorOrEndpoint proves external mode
// exposes neither an administrator Secret nor a management endpoint — the operator does not
// manage a foreign broker.
func TestResolveMessagingConnectionExternalHasNoAdministratorOrEndpoint(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode: "external", Host: testExternalMQHost, Port: 5673, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: testILMMQ},
			},
		},
	}
	conn := ResolveMessagingConnection(p)
	assert.Empty(t, conn.AdministratorCredentialsSecretName, "external mode has no managed administrator Secret")
	assert.Empty(t, ManagedMessagingManagementEndpoint(p), "external mode has no managed management endpoint")
}

// TestResolveMessagingConnection217HasNoAdministratorCredentials proves the 2.17.0 bundle —
// whose single-user topology's lone user is Core, merely TAGGED administrator, not a distinct
// administrator ROLE — resolves an empty administrator Secret name.
func TestResolveMessagingConnection217HasNoAdministratorCredentials(t *testing.T) {
	p := managedMQPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Version = testVersion217 })
	conn := ResolveMessagingConnection(p)
	assert.Empty(t, conn.AdministratorCredentialsSecretName, "2.17.0 has no administrator role in its topology")
}
