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

// messaging_connection.go is the mode-agnostic readback seam for the broker: it resolves
// the messaging connection facts (host/port/vhost + the credentials Secret names) the
// platform's builders and the controller consume, REGARDLESS of whether the broker is
// external (coordinates from the spec) or managed (coordinates from the RabbitMQ Cluster
// Operator Service + the Messaging Topology Operator-generated per-user Secrets).
//
// It is the messaging twin of db_connection.go's DatabaseConnection: by funnelling both
// modes through one MessagingConnection, every downstream consumer (BuildMessagingConfigMap
// host/port, sharedCredSecretEnv's BROKER_* creds, the provisioning init container) stays
// mode-agnostic — a managed broker is wired EXACTLY like an external one (a host/port + a
// secretKeyRef into a credentials Secret), with only the source of those facts differing.

import (
	"fmt"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// MessagingConnection is the resolved, mode-agnostic broker connection the platform's
// components are wired to. For external mode it is the CR coordinates verbatim; for
// managed mode it is the RabbitMQ Cluster Operator-generated broker Service, port 5672,
// the provisioned vhost, and the Messaging Topology Operator-generated per-user
// credentials Secrets (Core's for the main BROKER_* creds, the provisioner's for the
// provisioning flow).
//
// SECURITY: this struct holds only NON-secret coordinates plus the NAMES of credentials
// Secrets — never a secret value. It must never be placed (in whole or in part) into
// status, conditions, events, or logs; the credentials are consumed by reference
// (secretKeyRef) only.
type MessagingConnection struct {
	// Host is the broker hostname the platform connects to (an in-cluster Service name for
	// managed mode, the caller's host for external mode).
	Host string
	// Port is the broker AMQP port.
	Port int32
	// VirtualHost is the broker virtual host.
	VirtualHost string
	// CredentialsSecretName names the Secret holding the username/password the platform's
	// Core and scheduler authenticate with (the caller's Secret for external mode,
	// the Topology-generated Core-user Secret for managed mode). The Core-user Secret carries
	// both the username and password keys the wiring profile reads, and Core's BROKER_* env
	// is wired to it by reference.
	CredentialsSecretName string
	// UsernameKey / PasswordKey are the EFFECTIVE in-Secret keys the broker username/password
	// live under. For external mode they are the user's spec.messaging.credentials key
	// overrides when set, else the wiring-profile defaults (username/password). For managed
	// mode they are ALWAYS the wiring-profile defaults — the keys are the upstream Messaging
	// Topology Operator's generated-Secret convention (which matches username/password), NOT
	// user-mappable. The secretKeyRef wiring (sharedCredSecretEnv) sources these keys.
	UsernameKey string
	PasswordKey string
	// ProvisioningCredentialsSecretName names the Secret holding the provisioner user's
	// credentials, for the remote-proxy provisioning flow. For external mode it is empty
	// (the caller wires provisioning via spec.provisioning); for managed mode it is the
	// Topology-generated provisioner-user Secret. Consumed by reference only.
	ProvisioningCredentialsSecretName string
	// ProxyCredentialsSecretName names the Secret holding the proxy user's credentials, which
	// the bundled provisioning service (core.provisioning.mode=deploy) embeds into the
	// per-proxy JWT tokens it issues. For external mode it is empty (the caller wires it via
	// spec.core.provisioning.deploy.proxyCredentials); for managed mode it is the Topology-
	// generated proxy-user Secret. Consumed by reference only.
	ProxyCredentialsSecretName string
	// AdministratorCredentialsSecretName names the Secret holding the full-access
	// administrator user's credentials — the user a RabbitMQ management-API client (e.g. a
	// migration's queue-depth poll) authenticates as. For external mode it is empty (the
	// operator does not manage a foreign broker); for managed mode it is the Topology-
	// generated administrator-user Secret, EXCEPT on the 2.17.0 bundle, whose single-user
	// topology has no administrator ROLE (its lone user is Core, merely TAGGED
	// administrator) — so it is empty there too. Consumed by reference only.
	AdministratorCredentialsSecretName string
}

// ResolveMessagingConnection resolves the mode-agnostic MessagingConnection for a Platform:
//
//   - external → the spec coordinates (host/port/virtualHost) and credentials.secretRef
//     verbatim, with the in-Secret keys resolved as pick(spec override, bom default); no
//     managed provisioner Secret.
//   - managed  → Host is the RabbitMQ broker Service "<cluster>" (the Cluster Operator
//     names the client Service after the RabbitmqCluster), Port is 5672, VirtualHost is
//     the provisioned vhost (spec.messaging.virtualHost or the default), and
//     CredentialsSecretName / ProvisioningCredentialsSecretName are the Topology-generated
//     Core- and provisioner-user Secrets.
//
// The generated-resource names are a fixed function of the operator-owned cluster name
// (ManagedMessagingName), which is why those override paths are protected — so this
// readback can never be invalidated by a caller's Overrides.
func ResolveMessagingConnection(p *otilmv1alpha1.Platform) MessagingConnection {
	w := wiringFor(p)
	if MessagingManaged(p) {
		return MessagingConnection{
			// The RabbitMQ Cluster Operator exposes the cluster's client (AMQP) Service under
			// the RabbitmqCluster's own name in the same namespace (ServiceSuffix=""), which is
			// the host the readback wires dependents to.
			Host:                               ManagedMessagingName(p),
			Port:                               managedBrokerPort,
			VirtualHost:                        managedVirtualHost(p),
			CredentialsSecretName:              managedUserCredentialsSecretName(p, bom.MessagingUserCore),
			ProvisioningCredentialsSecretName:  managedUserCredentialsSecretName(p, bom.MessagingUserProvisioner),
			ProxyCredentialsSecretName:         managedUserCredentialsSecretName(p, bom.MessagingUserProxy),
			AdministratorCredentialsSecretName: administratorCredentialsSecretName(p),
			// Managed: the keys are the Topology Operator's generated-Secret convention (which
			// matches the wiring defaults), NOT user-mappable.
			UsernameKey: w.MessagingCred.UsernameKey,
			PasswordKey: w.MessagingCred.PasswordKey,
		}
	}
	creds := p.Spec.Messaging.Credentials
	return MessagingConnection{
		Host:        p.Spec.Messaging.Host,
		Port:        p.Spec.Messaging.Port,
		VirtualHost: p.Spec.Messaging.VirtualHost,
		// External: the user's mapping wins; the wiring profile supplies the default key.
		CredentialsSecretName: credentialsSecretRef(creds),
		UsernameKey:           msgCredUsernameKey(creds, w),
		PasswordKey:           msgCredPasswordKey(creds, w),
		// External provisioning credentials are configured via spec.provisioning, not here.
	}
}

// msgCredUsernameKey resolves the effective broker username key as pick(spec override, bom
// default), keeping the wiring profile as the single source of the default messaging key.
func msgCredUsernameKey(c *otilmv1alpha1.CredentialsRef, w bom.WiringProfile) string {
	if c != nil && c.UsernameKey != "" {
		return c.UsernameKey
	}
	return w.MessagingCred.UsernameKey
}

// msgCredPasswordKey resolves the effective broker password key as pick(spec override, bom
// default).
func msgCredPasswordKey(c *otilmv1alpha1.CredentialsRef, w bom.WiringProfile) string {
	if c != nil && c.PasswordKey != "" {
		return c.PasswordKey
	}
	return w.MessagingCred.PasswordKey
}

// administratorCredentialsSecretName resolves the managed broker's administrator-user
// credentials Secret name, or empty when the selected bundle's messaging topology carries no
// administrator ROLE. The 2.17.0 bundle's sole broker user is Core (merely TAGGED
// administrator), so it has no distinct administrator-user Secret to reference.
func administratorCredentialsSecretName(p *otilmv1alpha1.Platform) string {
	if !messagingTopologyHasRole(resolveBundle(p).Messaging, bom.MessagingUserAdministrator) {
		return ""
	}
	return managedUserCredentialsSecretName(p, bom.MessagingUserAdministrator)
}

// messagingTopologyHasRole reports whether a messaging topology provisions a user with the
// given role.
func messagingTopologyHasRole(topo bom.MessagingTopology, role bom.MessagingUserRole) bool {
	for _, u := range topo.Users {
		if u.Role == role {
			return true
		}
	}
	return false
}

// ManagedMessagingManagementEndpoint returns the managed broker's RabbitMQ HTTP management
// API base URL — http://<cluster-service>.<namespace>.svc:<managedManagementPort> — for a
// managed broker; empty for external mode, since the operator does not manage a foreign
// broker's management API.
//
// SECURITY: like MessagingConnection, this is builder output consumed in memory only — it
// must never be placed (in whole or in part) into status, conditions, events, or logs.
func ManagedMessagingManagementEndpoint(p *otilmv1alpha1.Platform) string {
	if !MessagingManaged(p) {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", ManagedMessagingName(p), p.Namespace, managedManagementPort)
}
