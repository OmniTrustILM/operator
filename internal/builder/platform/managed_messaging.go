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

// managed_messaging.go renders the operator-provisioned RabbitMQ infrastructure for a
// Platform whose messaging.mode=managed: a RabbitmqCluster (RabbitMQ Cluster Operator)
// plus the full messaging topology — Vhost, the platform Users, their Permissions, the
// Exchanges, the Queues, and the Bindings defined by the selected version bundle's
// messaging topology (Messaging Topology Operator) — emitted as preset-GVK
// *unstructured.Unstructured. The topology is BOM-versioned: the default 2.19.0 bundle
// renders 5 Users, 5 Permissions, 2 Exchanges, 11 Queues, and 10 Bindings (the 2.18.0
// bundle has 10 Queues and 9 Bindings; the legacy 2.17.0 bundle has a single User).
//
// It is the SECOND managed-infrastructure component and reuses, verbatim, the seams the
// managed PostgreSQL work (managed_database.go) factored out: managedLabels,
// newManagedUnstructured (the preset-GVK CRD-neutral constructor), and the RFC 7396
// JSON-merge-patch helpers (mergePatchMap / patchHasPath / joinPath) behind the
// per-component `overrides` escape hatch. Only the protected-path SET and the render-time
// error annotation are messaging-specific.
//
// WHY UNSTRUCTURED (preset GVK): exactly the rationale used for CloudNativePG and the
// edge's cert-manager / Gateway API objects. Adding a typed rabbitmq.com dependency would
// pin a k8s.io graph; an unstructured object with its apiVersion/kind preset apply-loops
// cleanly through SSA because Scheme.ObjectKinds returns the preset GVK even for an
// unregistered type.
//
// RABBITMQ SCHEMA: the rabbitmq.com apiVersion/Kinds and the field paths below are validated
// end-to-end against the RabbitMQ Cluster Operator (v2.21.0) and Messaging Topology Operator
// (v1.19.2) by the managed-messaging e2e: the Cluster Operator accepts the rendered
// RabbitmqCluster, round-trips its spec fields, and reconciles it to Ready; the Topology
// Operator accepts the full topology (Vhost + the bundle's Users + Permissions + Exchanges +
// Queues + Bindings — for the default 2.19.0 bundle 5 Users, 5 Permissions, 2 Exchanges,
// 11 Queues and 10 Bindings; the 2.18.0 bundle has 10 Queues and 9 Bindings, and the legacy
// 2.17.0 bundle has a single User) and reconciles EVERY CR to its Ready condition; and it
// generates the per-user "<user>-user-credentials" Secrets the readback consumes.
// spec.resources and the overrides protected-path set (spec.rabbitmq.additionalConfig) are
// not exercised end-to-end; their field paths are taken from the CRDs. NO credential is ever
// placed in these objects — the Topology Operator generates the per-user credentials Secrets
// and the operator reads them back by reference (never inlined).

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonRabbitMQNotInstalled is the MessagingReady=False reason when messaging is managed
// but the cluster does not serve the RabbitMQ Cluster Operator CRD (RabbitmqCluster).
// ReasonTopologyOperatorNotInstalled is the same for the Messaging Topology Operator
// CRDs (Vhost/User/Permission/Exchange/Queue/Binding). Both are exported so the
// controller and its tests share one definition (mirrors ReasonCloudNativePGNotInstalled).
const (
	ReasonRabbitMQNotInstalled         = "RabbitMQNotInstalled"
	ReasonTopologyOperatorNotInstalled = "TopologyOperatorNotInstalled"
)

// RabbitMQ API coordinates and the operator-owned naming/contract constants. The objects
// are rendered as unstructured with this apiVersion preset (see the package doc); the
// names and the topology shape are operator-owned so the readback (managedMessaging
// connection) is a fixed contract independent of the CR.
const (
	// rabbitmqGroup / rabbitmqVersion / rabbitmqAPIVersion are the rabbitmq.com API
	// coordinates. Both the Cluster Operator (RabbitmqCluster) and the Messaging Topology
	// Operator (Vhost/User/Permission/Exchange/Queue/Binding) live at rabbitmq.com/v1beta1.
	rabbitmqGroup      = "rabbitmq.com"
	rabbitmqVersion    = "v1beta1"
	rabbitmqAPIVersion = rabbitmqGroup + "/" + rabbitmqVersion

	// The RabbitMQ Kinds the operator renders. Cluster from the Cluster Operator; the
	// rest from the Messaging Topology Operator.
	rmqKindCluster    = "RabbitmqCluster"
	rmqKindVhost      = "Vhost"
	rmqKindUser       = "User"
	rmqKindPermission = "Permission"
	rmqKindExchange   = "Exchange"
	rmqKindQueue      = "Queue"
	rmqKindBinding    = "Binding"

	// managedMessagingRole is the component label the managed RabbitmqCluster carries;
	// managedTopologyRole is the component label every topology CR carries.
	managedMessagingRole = "messaging"
	managedTopologyRole  = "messaging-topology"

	// managedBrokerPort is the AMQP port the managed RabbitMQ cluster serves on.
	managedBrokerPort int32 = 5672

	// managedManagementPort is the RabbitMQ HTTP management API port the managed cluster's
	// client Service exposes alongside AMQP and Prometheus (verified on the generated
	// Service: amqp:5672, management:15672, prometheus:15692).
	managedManagementPort int32 = 15672

	// userCredentialsSecretSuffix is the Messaging Topology Operator's generated-Secret
	// naming convention for a User CR named <user>: it creates a Secret <user>-user-credentials
	// holding the user's username/password (Topology Operator source: Name = user.Name +
	// "-user-credentials", keys "username"/"password" — the keys the readback wires).
	userCredentialsSecretSuffix = "-user-credentials" //nolint:gosec // G101: a Secret-name suffix, not a credential value
)

// ManagedMessagingName returns the stable name of the RabbitmqCluster the operator
// renders for a Platform: "<platform>-messaging". It is operator-owned (a protected
// override path) so the generated broker Service name and the per-user credentials Secret
// names — which the readback resolves — are a fixed function of the Platform name.
func ManagedMessagingName(p *otilmv1alpha1.Platform) string {
	return p.Name + "-messaging"
}

// managedUserName returns the stable name of the User CR (and the prefix of its generated
// credentials Secret) for a given platform broker role: "<platform>-messaging-<role>"
// (e.g. "ilm-messaging-core"). The role-suffix keeps every topology object's name a fixed
// function of the operator-owned cluster name.
//
// It is deliberately NOT vhost-scoped: broker users are global to the cluster, their specs
// are identical across the version bundles, and the "<user>-user-credentials" Secret the
// Topology Operator generates from each one is wired into every component's secretKeyRef —
// so scoping this would re-key every component's credentials on a vhost migration.
func managedUserName(p *otilmv1alpha1.Platform, role bom.MessagingUserRole) string {
	return ManagedMessagingName(p) + "-" + string(role)
}

// managedUserCredentialsSecretName returns the name of the credentials Secret the
// Messaging Topology Operator generates for the given user role: "<user-cr-name>-user-credentials"
// (see userCredentialsSecretSuffix).
func managedUserCredentialsSecretName(p *otilmv1alpha1.Platform, role bom.MessagingUserRole) string {
	return managedUserName(p, role) + userCredentialsSecretSuffix
}

// ManagedMessagingClusterGVK returns the preset GroupVersionKind of the RabbitmqCluster
// the operator renders. The reconciler uses it to GET the cluster (unstructured) when
// probing readiness.
func ManagedMessagingClusterGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: rabbitmqGroup, Version: rabbitmqVersion, Kind: rmqKindCluster}
}

// ManagedMessagingVhostGVK returns the preset GroupVersionKind of the Vhost topology CR the
// operator renders. The reconciler GETs the Vhost (unstructured) to gate the vhost-dependent
// topology (exchanges/queues/bindings/permissions) on the vhost having been created, so a
// dependent is never declared against a not-yet-existing vhost (which the Messaging Topology
// Operator fails as vhost_not_found and only retries on a slow backoff).
func ManagedMessagingVhostGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: rabbitmqGroup, Version: rabbitmqVersion, Kind: rmqKindVhost}
}

// ManagedMessagingReclaimKinds returns the vhost-scoped topology Kinds a messaging migration
// reclaims from the virtual host it moved away from, in the order they must be deleted:
// bindings → queues → exchanges → permissions → virtual host. A dependent deleted after the
// object it depends on is stranded behind the Messaging Topology Operator's finalizer, so the
// order is a correctness contract, not a preference.
//
// Two rendered Kinds are DELIBERATELY absent. The RabbitmqCluster is the broker itself — the
// same one that goes on serving the target topology. The Users are broker-global rather than
// vhost-scoped (see managedUserName): both renders produce the identical objects, and the
// per-user credentials Secrets the Topology Operator generates from them are wired into every
// component's secretKeyRef, so deleting one would take the running platform's credentials with
// it.
func ManagedMessagingReclaimKinds() []string {
	return []string{rmqKindBinding, rmqKindQueue, rmqKindExchange, rmqKindPermission, rmqKindVhost}
}

// ManagedMessagingVhostName returns the stable k8s object name of the Vhost CR the operator
// renders: "<platform>-messaging<scope>-vhost" (NOT the vhost's spec.name, which is the
// broker vhost the components connect to). It is a fixed function of the operator-owned
// cluster name and the vhost scope, so a legacy-vhost or user-pinned-vhost platform keeps the
// exact name it already carries while another vhost gets its own Vhost CR.
func ManagedMessagingVhostName(p *otilmv1alpha1.Platform) string {
	return scopedTopologyName(p, managedVirtualHost(p), "vhost")
}

// MessagingManaged reports whether the Platform's messaging is operator-provisioned
// (mode=managed with the managed block present). It is the single predicate the builder,
// the readback, the gating, and the deletion path share so they never drift.
func MessagingManaged(p *otilmv1alpha1.Platform) bool {
	return p.Spec.Messaging.Mode == messagingModeManaged && p.Spec.Messaging.Managed != nil
}

// messagingModeManaged / messagingModeExternal are the spec.messaging.mode literals.
const (
	messagingModeManaged  = "managed"
	messagingModeExternal = "external"
)

// managedVirtualHost returns the vhost name the operator provisions for a managed
// broker: the configured spec.messaging.virtualHost, or the selected bundle's
// per-version default when empty. The exchanges/queues/bindings all bind to this vhost.
func managedVirtualHost(p *otilmv1alpha1.Platform) string {
	return ManagedVirtualHostFor(p, resolveBundle(p))
}

// ManagedVirtualHostFor returns the vhost a managed broker's topology lives on under the
// GIVEN version bundle, applying the same precedence managedVirtualHost applies to the
// platform's own selected bundle: spec.messaging.virtualHost when set, else that bundle's
// per-version default. It is the per-bundle form so a caller comparing two bundles (a version
// upgrade's source and target) derives both answers from the ONE precedence rule.
//
// A user override wins over every bundle default, so a pinned vhost resolves identically under
// every bundle — which is exactly why such a platform never experiences the vhost rename a
// version upgrade otherwise causes.
//
// MANAGED mode only: external mode passes spec.messaging.virtualHost through verbatim and
// applies no version default, so this is not the external-mode answer.
func ManagedVirtualHostFor(p *otilmv1alpha1.Platform, b bom.Bundle) string {
	if v := p.Spec.Messaging.VirtualHost; v != "" {
		return v
	}
	return b.Messaging.DefaultVirtualHost
}

// vhostIsUserPinned reports whether managedVirtualHost's result came from the platform's own
// spec.messaging.virtualHost rather than the bundle default. scopedTopologyName feeds this
// into topologyScope: a user-pinned vhost resolves to the SAME value under every bundle (the
// override always wins over the default), so that platform never experiences the vhost RENAME
// a version upgrade causes and its topology names must stay unscoped forever — see
// topology_naming.go.
func vhostIsUserPinned(p *otilmv1alpha1.Platform) bool {
	return p.Spec.Messaging.VirtualHost != ""
}

// ResolveManagedMessaging returns the RabbitMQ objects the operator provisions for a
// managed broker, or nil when messaging is external (or managed but mis-specified). It
// renders, in a stable order, the RabbitmqCluster, the Vhost, the Users, their
// Permissions, the Exchanges, the Queues, and the Bindings from the selected bundle's
// messaging topology (the default 2.19.0 bundle: 5 Users, 5 Permissions, 2 Exchanges,
// 11 Queues and 10 Bindings; the 2.18.0 bundle has 10 Queues and 9 Bindings, and the legacy
// 2.17.0 bundle has a single User) — all as preset-GVK unstructured objects with NO owner
// reference (the reconciler decides ownership per the deletion-safety contract: managed CRs
// carry no controller ownerRef and are prune-excluded, so a transient de-render never
// deletes the broker).
//
// SECURITY: no credential is placed in these objects. The User CRs let the Messaging
// Topology Operator GENERATE each user's credentials Secret; the operator reads those
// back by reference (never inlined). Only non-secret sizing/topology data is rendered.
func ResolveManagedMessaging(p *otilmv1alpha1.Platform) []client.Object {
	if !MessagingManaged(p) {
		return nil
	}
	topo := resolveBundle(p).Messaging
	vhost := managedVirtualHost(p)

	objs := []client.Object{buildRabbitmqCluster(p)}
	objs = append(objs, buildVhost(p, vhost))
	for _, u := range topo.Users {
		objs = append(objs, buildUser(p, u))
	}
	for _, u := range topo.Users {
		objs = append(objs, buildPermission(p, vhost, u))
	}
	for _, e := range topo.Exchanges {
		objs = append(objs, buildExchange(p, vhost, e))
	}
	for _, q := range topo.Queues {
		objs = append(objs, buildQueue(p, vhost, q))
	}
	for _, b := range topo.Bindings {
		objs = append(objs, buildBinding(p, vhost, b))
	}
	return objs
}

// buildRabbitmqCluster renders the RabbitmqCluster for the managed broker. The spec is
// built from the typed surface (replicas/version/storage/resources) and then the caller's
// Overrides JSON-merge patch is applied onto it, with the operator-owned paths protected.
// A protected-path or malformed override surfaces as an error embedded in the returned
// object's annotation so the reconciler can degrade with a clear message.
func buildRabbitmqCluster(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	m := p.Spec.Messaging.Managed

	// spec.persistence.{storage,storageClassName}: storage is a Quantity string;
	// storageClassName is the RabbitmqClusterPersistenceSpec field path (not exercised
	// end-to-end, which uses the cluster default).
	persistence := map[string]interface{}{
		"storage": m.Storage.Size,
	}
	if m.Storage.StorageClass != nil && *m.Storage.StorageClass != "" {
		persistence["storageClassName"] = *m.Storage.StorageClass
	}

	spec := map[string]interface{}{
		"replicas":    int64(managedReplicas(m)),
		"persistence": persistence,
	}

	// RabbitMQ version → image. The RabbitMQ Cluster Operator selects the engine version
	// via spec.image (a fully-qualified image ref). The operator composes the community
	// image "rabbitmq:<version>-management" from the requested version; when the version is
	// empty the Cluster Operator applies its own default image (image omitted).
	if m.Version != "" {
		spec["image"] = rabbitmqImageForVersion(m.Version)
	}

	if m.Resources != nil {
		// The Cluster Operator passes spec.resources straight to the node pods' container
		// resources (a core/v1 ResourceRequirements).
		if res, err := toUnstructured(m.Resources); err == nil {
			spec["resources"] = res
		}
	}

	// spec.rabbitmq.additionalConfig: when the management UI is published through the gateway
	// (messaging.management.expose), tell RabbitMQ it is served under the gateway's /mq prefix via
	// management.path_prefix. This is the RabbitMQ-documented way to run the management UI behind a
	// path-prefixed reverse proxy: RabbitMQ then prefixes its OWN asset/API URLs with /mq AND issues
	// the /mq -> /mq/ redirect itself, so the UI loads with OR without the trailing slash (the
	// gateway forwards /mq UNSTRIPPED — see the /mq route in gateway.go). It affects ONLY the
	// management HTTP listener (15672); AMQP (5672) carries no HTTP path, so the broker clients
	// (Core/proxy/scheduler) are unaffected. The prefix MUST equal the gateway route path, so both
	// read the same gatewayMqPath constant. additionalConfig is an operator-owned protected path.
	if p.Spec.Messaging.Management.Expose {
		spec["rabbitmq"] = map[string]interface{}{
			"additionalConfig": "management.path_prefix = " + gatewayMqPath + "\n",
		}
	}

	u := newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindCluster, ManagedMessagingName(p), managedMessagingRole, spec)

	// Apply the caller's Overrides JSON-merge patch (RFC 7396) onto the rendered spec,
	// rejecting the operator-owned protected paths. On error, stamp the object so the
	// reconciler degrades with an actionable, leak-free message.
	if err := applyManagedMessagingOverrides(u, m.Overrides); err != nil {
		setManagedMessagingError(u, err)
	}
	return u
}

// managedReplicas returns the configured node count, defaulting to 1 (the CRD also
// defaults this, but the builder is a pure function callable without apiserver defaulting
// in unit tests).
func managedReplicas(m *otilmv1alpha1.ManagedMessagingSpec) int32 {
	if m.Replicas < 1 {
		return 1
	}
	return m.Replicas
}

// rabbitmqImageForVersion composes the community RabbitMQ management image for a version:
// rabbitmq:<version>-management. Deployments that pin a minor/patch tag or a private mirror
// use the override escape hatch.
func rabbitmqImageForVersion(version string) string {
	return "rabbitmq:" + version + "-management"
}

// clusterReference returns the spec.rabbitmqClusterReference block every topology CR
// carries, pointing at the operator's managed RabbitmqCluster by name (same namespace), so
// the Topology Operator resolves the cluster and reconciles each CR.
func clusterReference(p *otilmv1alpha1.Platform) map[string]interface{} {
	return map[string]interface{}{"name": ManagedMessagingName(p)}
}

// buildVhost renders the Vhost topology CR for the managed broker's vhost.
func buildVhost(p *otilmv1alpha1.Platform, vhost string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"name":                     vhost, // Vhost spec.name
		"rabbitmqClusterReference": clusterReference(p),
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindVhost, ManagedMessagingVhostName(p), managedTopologyRole, spec)
}

// buildUser renders a User topology CR for one platform broker user. The Messaging
// Topology Operator GENERATES this user's credentials Secret (<user>-user-credentials);
// the operator NEVER mints credentials itself. The user's tags come from the versioned
// topology data. The operator relies on the default (operator-generated) Secret rather than
// spec.importCredentialsSecret.
func buildUser(p *otilmv1alpha1.Platform, user bom.MessagingUser) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"rabbitmqClusterReference": clusterReference(p),
	}
	if len(user.Tags) > 0 {
		tags := make([]interface{}, 0, len(user.Tags))
		for _, t := range user.Tags {
			tags = append(tags, t)
		}
		spec["tags"] = tags // User spec.tags (e.g. "administrator")
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindUser, managedUserName(p, user.Role), managedTopologyRole, spec)
}

// buildPermission renders a Permission topology CR binding one user to the vhost with the
// configure/write/read regexes from the versioned topology data. It references the user via
// spec.userReference (the User CR by name, not spec.user) so the permission tracks the
// generated user. spec.permissions.{configure,write,read} carry the regexes (an empty
// configure means no configure permission, used by the proxy/core users).
func buildPermission(p *otilmv1alpha1.Platform, vhost string, user bom.MessagingUser) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"vhost":         vhost,
		"userReference": map[string]interface{}{"name": managedUserName(p, user.Role)},
		"permissions": map[string]interface{}{
			"configure": user.Configure,
			"write":     user.Write,
			"read":      user.Read,
		},
		"rabbitmqClusterReference": clusterReference(p),
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindPermission,
		scopedTopologyName(p, vhost, string(user.Role)+"-permission"), managedTopologyRole, spec)
}

// buildExchange renders an Exchange topology CR with spec.{name,type,durable,vhost}.
func buildExchange(p *otilmv1alpha1.Platform, vhost string, ex bom.MessagingExchange) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"name":                     ex.Name,
		"type":                     ex.Type,
		"durable":                  ex.Durable,
		"vhost":                    vhost,
		"rabbitmqClusterReference": clusterReference(p),
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindExchange,
		topologyObjectName(p, vhost, "exchange", ex.Name), managedTopologyRole, spec)
}

// buildQueue renders a Queue topology CR with spec.{name,durable,vhost}.
func buildQueue(p *otilmv1alpha1.Platform, vhost string, q bom.MessagingQueue) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"name":                     q.Name,
		"durable":                  q.Durable,
		"vhost":                    vhost,
		"rabbitmqClusterReference": clusterReference(p),
	}
	// Optional RabbitMQ x-arguments (e.g. the time-quality queues' x-max-length / x-overflow).
	// Integer values are int64 in the BOM so the unstructured render accepts them.
	if len(q.Arguments) > 0 {
		spec["arguments"] = q.Arguments
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindQueue,
		topologyObjectName(p, vhost, "queue", q.Name), managedTopologyRole, spec)
}

// buildBinding renders a Binding topology CR (exchange→queue) with
// spec.{source,destination,destinationType,routingKey,vhost}. destinationType is fixed to
// "queue" (every platform binding targets a queue).
func buildBinding(p *otilmv1alpha1.Platform, vhost string, b bom.MessagingBinding) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"source":                   b.Source,
		"destination":              b.Destination,
		"destinationType":          "queue",
		"routingKey":               b.RoutingKey,
		"vhost":                    vhost,
		"rabbitmqClusterReference": clusterReference(p),
	}
	return newManagedUnstructured(p, rabbitmqAPIVersion, rmqKindBinding,
		topologyObjectName(p, vhost, "binding", b.Source+"-"+b.Destination), managedTopologyRole, spec)
}

// scopedTopologyName composes the object name of a vhost-bound topology CR from the
// operator-owned cluster name, the vhost scope, and a suffix. The scope is EMPTY for the
// legacy vhost, so a live 2.17.0/2.18.0 platform re-renders the exact names it already
// carries; it is ALSO empty whenever the platform pinned its own spec.messaging.virtualHost,
// since a pinned vhost resolves to the same value on every bundle and so never migrates (see
// topologyScope in topology_naming.go). Any OTHER vhost — one only a bundle default
// introduced — gets its own name space, which is what lets a source and a target topology
// coexist during a migration.
func scopedTopologyName(p *otilmv1alpha1.Platform, vhost, suffix string) string {
	return ManagedMessagingName(p) + topologyScope(vhost, vhostIsUserPinned(p)) + "-" + suffix
}

// topologyObjectName composes a stable, DNS-safe object name for a topology CR from the
// operator-owned cluster name, the vhost scope, the kind discriminator, and the app-level
// resource name. App-level names (e.g. "core.audit-logs") contain characters illegal in a
// k8s object name (".", "_"), so they are sanitized to "-"; the kind discriminator keeps an
// exchange and a queue of the same app name from colliding.
func topologyObjectName(p *otilmv1alpha1.Platform, vhost, kind, appName string) string {
	return scopedTopologyName(p, vhost, kind+"-"+sanitizeName(appName))
}

// sanitizeName lowercases and replaces every character illegal in a Kubernetes object
// name (anything other than [a-z0-9-]) with "-", so an app-level RabbitMQ resource name
// (which may contain ".", "_", uppercase) becomes a valid metadata.name. The
// spec.name/spec.source/etc. carry the ORIGINAL app name verbatim; only metadata.name is
// sanitized.
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// managedMessagingOverrideProtectedPaths are the operator-owned paths a caller's Overrides
// JSON-merge patch may NOT touch on the RabbitmqCluster. metadata.name/namespace/
// ownerReferences keep the object identity + ownership the operator controls; the
// default_user/default_pass and the imported-definitions wiring keep the topology +
// readback contract (the operator drives users/permissions via the Topology Operator and
// reads back the generated per-user Secrets — a patch seeding a default user or importing
// its own definitions would silently break that). A patch touching any of these is
// rejected with a clear error.
//
// RabbitMQ exposes the default user / imported definitions under spec.rabbitmq (the
// rendered rabbitmq.conf / definitions): spec.rabbitmq.additionalConfig (default_user/
// default_pass live here as config keys) and spec.rabbitmq.advancedConfig /
// spec.rabbitmq.envConfig. The whole spec.rabbitmq.additionalConfig and the load-definitions
// paths are protected (rather than the exact nested key) so a config change in the Cluster
// Operator schema cannot slip a default user or imported definitions past the guard.
var managedMessagingOverrideProtectedPaths = [][]string{
	{"metadata", "name"},
	{"metadata", "namespace"},
	{"metadata", "ownerReferences"},
	{"spec", "rabbitmq", "additionalConfig"},
	{"spec", "rabbitmq", "advancedConfig"},
}

// applyManagedMessagingOverrides applies a caller's RFC 7396 JSON-merge patch onto the
// rendered RabbitmqCluster, rejecting any patch that addresses a protected (operator-
// owned) path. A nil/empty patch is a no-op. It reuses the shared merge/patch helpers
// (mergePatchMap / patchHasPath / joinPath) defined alongside the managed-database
// builder; only the protected-path set differs.
func applyManagedMessagingOverrides(u *unstructured.Unstructured, overrides *runtime.RawExtension) error {
	return applyOverridesWithProtectedPaths(u, overrides, managedMessagingOverrideProtectedPaths,
		"messaging.managed.overrides")
}

// managedMessagingErrorAnnotation carries a render-time error (a rejected override) out of
// the pure builder to the reconciler, which surfaces it as a Degraded condition. It is
// operator-internal and stripped before apply. SECURITY: the error text names only a
// field path / parse failure — never a secret or coordinate.
const managedMessagingErrorAnnotation = "otilm.com/managed-messaging-error"

// setManagedMessagingError stamps a render-time error onto the object via an annotation so
// the reconciler can detect-and-degrade rather than applying an invalid object.
func setManagedMessagingError(u *unstructured.Unstructured, err error) {
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[managedMessagingErrorAnnotation] = err.Error()
	u.SetAnnotations(ann)
}

// ManagedMessagingRenderError returns the render-time error carried by a managed-messaging
// object (a rejected override or malformed patch), or nil when the object rendered
// cleanly. The reconciler calls this on each ResolveManagedMessaging object before apply.
func ManagedMessagingRenderError(obj client.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	if msg := u.GetAnnotations()[managedMessagingErrorAnnotation]; msg != "" {
		return errFromString(msg)
	}
	return nil
}

// MessagingDependencies returns the upstream-operator / CRD-bundle prerequisites a managed
// broker needs, or nil for an external broker (which needs none). For a managed broker it
// requires the RabbitMQ Cluster Operator (RabbitmqCluster) AND the Messaging Topology
// Operator (one representative Kind per CRD it renders). The reconciler probes each via
// the capability detector and gates the MessagingReady condition on their presence; the
// result mirrors exactly what ResolveManagedMessaging renders so the two never drift. The
// actionable message names the install remedy and the external escape hatch.
func MessagingDependencies(p *otilmv1alpha1.Platform) []CRDDependency {
	if !MessagingManaged(p) {
		return nil
	}
	clusterMsg := "messaging.mode=managed requires the RabbitMQ Cluster Operator (rabbitmq.com); " +
		"install the RabbitMQ Cluster Operator or set spec.messaging.mode=external to bring your own broker"
	topologyMsg := "messaging.mode=managed requires the RabbitMQ Messaging Topology Operator (rabbitmq.com); " +
		"install the Messaging Topology Operator or set spec.messaging.mode=external to bring your own broker"
	return []CRDDependency{
		{
			GroupKind: schema.GroupKind{Group: rabbitmqGroup, Kind: rmqKindCluster},
			Versions:  []string{rabbitmqVersion},
			Reason:    ReasonRabbitMQNotInstalled,
			Message:   clusterMsg,
		},
		// One representative Topology kind per the set the operator renders. The Topology
		// Operator ships all of these in one bundle, so probing Vhost (the first thing the
		// operator needs) is a sufficient, cheap signal; the message names the operator.
		{
			GroupKind: schema.GroupKind{Group: rabbitmqGroup, Kind: rmqKindVhost},
			Versions:  []string{rabbitmqVersion},
			Reason:    ReasonTopologyOperatorNotInstalled,
			Message:   topologyMsg,
		},
	}
}
