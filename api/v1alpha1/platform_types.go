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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DatabaseSpec is the database connection. It supports two modes: "external" (bring
// your own PostgreSQL — the platform connects to host/name with the referenced
// credentials) and "managed" (operator-provisioned via the CloudNativePG operator —
// the platform renders a CNPG Cluster and reads back its generated coordinates and app
// Secret).
//
// The XValidation rules enforce that the mode-specific block is actually present:
// external needs host/name and credentials.secretRef (without them the connection
// coordinates are incomplete and the platform would only surface a late Degraded), and
// managed needs the managed block (the CNPG Cluster sizing). Either omission would
// otherwise surface only as a late failure.
// +kubebuilder:validation:XValidation:rule="self.mode != 'external' || (has(self.host) && self.host.size() > 0 && has(self.name) && self.name.size() > 0 && has(self.credentials) && has(self.credentials.secretRef) && self.credentials.secretRef.size() > 0)",message="database.host, database.name and database.credentials.secretRef are required when mode=external"
// +kubebuilder:validation:XValidation:rule="self.mode != 'managed' || has(self.managed)",message="database.managed is required when mode=managed"
type DatabaseSpec struct {
	// Mode selects how the database is provided: "external" (bring your own) connects
	// to the host/name below with the referenced credentials; "managed" provisions a
	// PostgreSQL cluster via the CloudNativePG operator (which must be installed in the
	// cluster) and reads back its generated coordinates and credentials.
	// +kubebuilder:validation:Enum=external;managed
	// +kubebuilder:default=external
	Mode string `json:"mode,omitempty"`
	// Host is the database server hostname (external mode). Ignored when mode=managed
	// (the operator resolves the host from the generated CloudNativePG Service).
	// It is constrained to the hostname/IP charset (letters, digits, dot, underscore and
	// hyphen, plus colon and square brackets for IPv6 literals, up to 253 characters) —
	// defence in depth for the generated wiring the operator renders it into.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:\[\]-]{1,253}$`
	Host string `json:"host,omitempty"`
	// Port is the database server port.
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// Name is the database name (external mode). Ignored when mode=managed (the operator
	// uses the bootstrapped application database name).
	Name string `json:"name,omitempty"`
	// Credentials references the Secret holding the database username/password and lets the
	// user map the in-Secret keys (usernameKey/passwordKey, defaulting to username/password).
	// Required for external mode; ignored when mode=managed (the operator reads back the
	// CloudNativePG-generated application Secret, whose keys are the upstream operator's —
	// the user key mapping applies to external mode only).
	// +optional
	Credentials *CredentialsRef `json:"credentials,omitempty"`
	// Managed configures the operator-provisioned PostgreSQL cluster (CloudNativePG).
	// Required when mode=managed; ignored otherwise.
	// +optional
	Managed *ManagedDatabaseSpec `json:"managed,omitempty"`
	// PgBouncer optionally fronts the database with a connection pooler. For a managed
	// database the operator can provision a CloudNativePG Pooler; the platform then
	// connects through the pooler Service instead of the cluster's read-write Service.
	// +optional
	PgBouncer *PgBouncerSpec `json:"pgBouncer,omitempty"`
}

// ManagedDatabaseSpec configures the operator-provisioned PostgreSQL cluster rendered
// as a CloudNativePG Cluster when database.mode=managed. The operator owns the cluster
// name, namespace, owner references, and the bootstrapped application database/owner;
// everything else is configurable here (and, as an escape hatch, via Overrides). No
// credentials are ever placed here — CloudNativePG generates the application Secret and
// the operator reads it back by reference.
type ManagedDatabaseSpec struct {
	// Instances is the number of PostgreSQL instances (1 = single primary; >1 adds hot
	// standbys with synchronous/asynchronous replication managed by CloudNativePG).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Instances int32 `json:"instances,omitempty"`
	// Version is the major PostgreSQL version to run (e.g. "18"). It selects the
	// CloudNativePG container image; leave empty to use the operator's default.
	// +optional
	Version string `json:"version,omitempty"`
	// UpgradeAcknowledged opts in to a MAJOR PostgreSQL version upgrade of an
	// already-running managed cluster. A major bump (e.g. 17 → 18) is a one-way,
	// potentially data-affecting operation with upstream prerequisites; the operator
	// therefore BLOCKS a major increase of the running cluster's version until this is
	// set true, surfacing an actionable condition + Warning instead of passing the new
	// version straight through to CloudNativePG. Patch/minor changes and the FIRST
	// creation (no running version yet) apply freely regardless of this flag. Reset it
	// to false after the upgrade completes; leaving it true has no effect until the
	// next major bump.
	// +optional
	UpgradeAcknowledged bool `json:"upgradeAcknowledged,omitempty"`
	// Storage configures the persistent volume for each instance.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`
	// Resources is the per-instance container resource requirements (requests/limits)
	// passed through to the CloudNativePG Cluster. When unset the operator's default
	// applies.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Backup configures scheduled backups of the managed cluster. The field is shipped
	// now for forward compatibility; the deep backup/restore wiring (object-store
	// coordinates, end-to-end verification) is deferred to a later milestone.
	// +optional
	Backup *PGBackupSpec `json:"backup,omitempty"`
	// Overrides is a JSON-merge patch (RFC 7396) applied onto the rendered CloudNativePG
	// Cluster spec, an escape hatch for CNPG settings the typed surface does not expose.
	// Operator-owned fields are protected: metadata.name/namespace/ownerReferences and
	// bootstrap.initdb.database/owner may not be overridden (the operator rejects such a
	// patch), so the readback contract the platform relies on cannot be broken.
	// +optional
	Overrides *runtime.RawExtension `json:"overrides,omitempty"`
}

// PGBackupSpec configures scheduled backups for a managed PostgreSQL cluster. It is
// intentionally minimal today (schedule + retention); the object-store coordinates are
// referenced by Secret in a later milestone (never inlined), and end-to-end backup is
// deferred.
type PGBackupSpec struct {
	// Schedule is the backup schedule in six-field cron format (CloudNativePG's
	// ScheduledBackup schedule, e.g. "0 0 2 * * *").
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// Retention is the backup retention policy (e.g. "30d"), passed to CloudNativePG.
	// +optional
	Retention string `json:"retention,omitempty"`
}

// PgBouncerSpec configures the optional PgBouncer connection pooler in front of a managed
// database. Managed is the SOLE switch: for a managed database the operator renders a
// CloudNativePG Pooler and routes the platform's connection through its Service. The pooler
// is ON BY DEFAULT — omit the pgBouncer block to keep it, set managed=false to opt out. An
// external database brings its own pooling, so this block is ignored for it.
type PgBouncerSpec struct {
	// Managed makes the operator provision the pooler via a CloudNativePG Pooler
	// (managed-database mode only). For a managed database the pooler is ON BY DEFAULT — the ILM
	// fleet (core + auth + keycloak + scheduler) opens enough direct connection pools to
	// exhaust PostgreSQL's default max_connections, so the operator fronts the database with
	// PgBouncer (transaction mode) and wires every component through it. Omit the pgBouncer block
	// to keep the default-on pooler; set managed=false to explicitly opt out. NOTE: an empty
	// pgBouncer block leaves managed at its false default and disables the pooler — set
	// managed=true explicitly when customizing instances/parameters.
	// +kubebuilder:default=false
	Managed bool `json:"managed,omitempty"`
	// Instances is the number of pooler instances to run.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Instances int32 `json:"instances,omitempty"`
	// Parameters are extra PgBouncer settings passed through to the CloudNativePG Pooler
	// (e.g. max_client_conn). Non-sensitive only.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// MessagingSpec is the AMQP client connection. It supports two modes: "external"
// (bring your own broker — the platform connects to host/virtualHost with the referenced
// credentials) and "managed" (operator-provisioned via the RabbitMQ Cluster
// Operator + Messaging Topology Operator — the platform renders a RabbitmqCluster plus
// the full messaging topology and reads back the broker Service and the
// Topology-generated per-user credentials Secrets).
//
// The XValidation rules enforce that the mode-specific block is actually present:
// external needs host and credentials.secretRef (without them the connection coordinates
// are incomplete and the platform would only surface a late failure), and managed needs
// the managed block (the RabbitmqCluster sizing). Either omission would otherwise surface
// only as a late failure.
// +kubebuilder:validation:XValidation:rule="self.mode != 'external' || (has(self.host) && self.host.size() > 0 && has(self.credentials) && has(self.credentials.secretRef) && self.credentials.secretRef.size() > 0)",message="messaging.host and messaging.credentials.secretRef are required when mode=external"
// +kubebuilder:validation:XValidation:rule="self.mode != 'managed' || has(self.managed)",message="messaging.managed is required when mode=managed"
type MessagingSpec struct {
	// Mode selects how the broker is provided: "external" (bring your own) connects to
	// the host/virtualHost below with the referenced credentials; "managed" provisions a
	// RabbitMQ cluster via the RabbitMQ Cluster Operator (which, with the Messaging
	// Topology Operator, must be installed in the cluster) and reads back its generated
	// coordinates and per-user credentials.
	// +kubebuilder:validation:Enum=external;managed
	// +kubebuilder:default=external
	Mode string `json:"mode,omitempty"`
	// BrokerType selects the AMQP 1.0 broker dialect: rabbitmq or servicebus.
	// +kubebuilder:validation:Enum=rabbitmq;servicebus
	// +kubebuilder:default=rabbitmq
	BrokerType string `json:"brokerType,omitempty"`
	// Host is the message broker hostname (external mode). Ignored when mode=managed
	// (the operator resolves the host from the generated RabbitMQ Service).
	// It is constrained to the hostname/IP charset (letters, digits, dot, underscore and
	// hyphen, plus colon and square brackets for IPv6 literals, up to 253 characters) —
	// defence in depth for the broker-reachability wait loops the operator renders on Core
	// and scheduler, which receive it as an environment value.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:\[\]-]{1,253}$`
	Host string `json:"host,omitempty"`
	// Port is the message broker port.
	// +kubebuilder:default=5672
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// VirtualHost is the broker vhost. In MANAGED mode an empty value selects the platform
	// version's default vhost, which the operator then provisions and connects to (2.18.0:
	// "czertainly"; 2.19.0+: "/"). In EXTERNAL mode the value is passed through verbatim and
	// no version default is applied — an empty value stays empty, so set it explicitly to the
	// vhost your broker serves.
	VirtualHost string `json:"virtualHost,omitempty"`
	// Credentials references the Secret holding the broker username/password and lets the
	// user map the in-Secret keys (usernameKey/passwordKey, defaulting to username/password).
	// Required for external mode; ignored when mode=managed (the operator reads back the
	// Topology-generated per-user credentials Secrets, whose keys are the upstream operator's
	// — the user key mapping applies to external mode only).
	// +optional
	Credentials *CredentialsRef `json:"credentials,omitempty"`
	// Managed configures the operator-provisioned RabbitMQ cluster (RabbitMQ Cluster
	// Operator + Messaging Topology Operator). Required when mode=managed; ignored
	// otherwise.
	// +optional
	Managed *ManagedMessagingSpec `json:"managed,omitempty"`
	// Management controls exposure of the broker's management UI.
	// +optional
	Management MessagingManagementSpec `json:"management,omitempty"`
	// TimeQuality toggles Core's time-quality messaging integration. The monitor SIDECAR is a
	// separate, independent control (spec.core.timeQualityMonitor) — enabling one never
	// enables the other.
	// +optional
	TimeQuality TimeQualitySpec `json:"timeQuality,omitempty"`
	// MigrationAcknowledgedForVersion is the external-broker escape hatch for a messaging
	// version migration: when mode=external the operator does not own the broker and
	// cannot migrate a foreign topology itself, so a platform upgrade whose target version
	// changes the messaging topology (e.g. 2.18.0 -> 2.19.0) is REFUSED until this is set
	// to the target version, attesting that the operator has migrated their own broker
	// topology to match.
	//
	// The value is TARGET-SCOPED: it must equal the version being upgraded TO, so a stale
	// acknowledgement from a past upgrade can never silently authorize the next one.
	// Ignored when mode=managed (the operator migrates the managed broker itself).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	MigrationAcknowledgedForVersion string `json:"migrationAcknowledgedForVersion,omitempty"`
}

// MessagingManagementSpec controls exposure of the broker's management UI.
type MessagingManagementSpec struct {
	// Expose publishes the RabbitMQ management UI through the API gateway on /mq.
	// Honored only when messaging is operator-managed (mode=managed, brokerType=rabbitmq);
	// for an external broker the operator does not own the management endpoint and renders
	// no /mq route.
	// +optional
	Expose bool `json:"expose,omitempty"`
}

// TimeQualitySpec toggles Core's TIME-QUALITY MESSAGING INTEGRATION — the platform-side half
// of the time-quality feature pair. It is deliberately INDEPENDENT of the time-quality-monitor
// sidecar (spec.core.timeQualityMonitor): the Helm chart separates the two so an EXTERNAL
// monitor, or a staged rollout (integration first, monitor later), is possible, and the
// operator mirrors that split exactly.
//
// It renders the selected platform version's time-quality env var on Core (2.19.0+:
// MESSAGING_TIME_QUALITY_ENABLED). A bundle whose wiring profile does not name that variable
// renders NOTHING, so the same CR stays portable across platform versions.
type TimeQualitySpec struct {
	// Enabled turns on Core's time-quality messaging integration. Core then publishes its
	// time-quality configuration and consumes the monitor's request/result queues, which the
	// managed topology provisions from 2.18.0 onward.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
}

// ManagedMessagingSpec configures the operator-provisioned RabbitMQ cluster rendered as
// a RabbitmqCluster (RabbitMQ Cluster Operator) plus the full messaging topology
// (Vhost/User/Permission/Exchange/Queue/Binding via the Messaging Topology Operator)
// when messaging.mode=managed. The operator owns the cluster name, namespace, owner
// references, and the topology shape; everything else is configurable here (and, as an
// escape hatch, via Overrides). No credentials are ever placed here — the Topology
// Operator generates the per-user credentials Secrets and the operator reads them back
// by reference.
type ManagedMessagingSpec struct {
	// Replicas is the number of RabbitMQ nodes (1 = single node; >1 forms a clustered,
	// quorum-capable broker managed by the RabbitMQ Cluster Operator).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
	// Version is the RabbitMQ version to run (e.g. "4.3"). It selects the RabbitMQ
	// Cluster Operator container image; leave empty to use the operator's default.
	// +optional
	Version string `json:"version,omitempty"`
	// UpgradeAcknowledged opts in to a MAJOR RabbitMQ version upgrade of an
	// already-running managed cluster. A major bump (e.g. 3.x → 4.x) has hard upstream
	// prerequisites — required feature flags must be enabled and all classic-mirrored
	// queues migrated to quorum queues BEFORE the jump — and skipping them can wedge the
	// broker; the operator therefore BLOCKS a major increase of the running cluster's
	// version until this is set true, surfacing an actionable condition + Warning instead
	// of passing the new version straight through to the RabbitMQ Cluster Operator.
	// Patch/minor changes and the FIRST creation (no running version yet) apply freely
	// regardless of this flag. Reset it to false after the upgrade completes.
	// +optional
	UpgradeAcknowledged bool `json:"upgradeAcknowledged,omitempty"`
	// Storage configures the persistent volume for each node.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`
	// Resources is the per-node container resource requirements (requests/limits) passed
	// through to the RabbitmqCluster. When unset the operator's default applies.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Overrides is a JSON-merge patch (RFC 7396) applied onto the rendered RabbitmqCluster
	// spec, an escape hatch for RabbitMQ settings the typed surface does not expose.
	// Operator-owned fields are protected: metadata.name/namespace/ownerReferences and the
	// default_user/default_pass / imported-definitions wiring may not be overridden (the
	// operator rejects such a patch), so the topology and readback contract the platform
	// relies on cannot be broken.
	// +optional
	Overrides *runtime.RawExtension `json:"overrides,omitempty"`
	// DrainTimeout bounds EACH of the migration's waiting phases separately: how long
	// Fencing waits for the platform's message producers to actually wind down, how long
	// Draining waits for the source vhost to empty, and how long CleaningUp waits for the
	// source vhost to fall idle before it can be reclaimed. Each phase gets the full window,
	// measured from its own start. The migration engine polls the source vhost across the
	// Draining window for every drainable queue to report empty; if a deadline passes with
	// producers still running or messages still outstanding the migration stops in that
	// phase rather than proceeding — see ForceCutoverForVersion to override that stop.
	// It is constrained to the Go duration charset (digits, an optional fraction, and one
	// or more unit suffixes — ns, us, ms, s, m or h, e.g. "15m" or "1h30m") via a CEL rule
	// rather than a Pattern marker: controller-gen cannot apply +kubebuilder:validation:
	// Pattern directly to a metav1.Duration-typed field (it is a cross-package type
	// reference, not a plain string, at schema-generation time).
	// +optional
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:XValidation:rule="self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$')",message="drainTimeout must be a Go duration string, e.g. 15m or 1h30m"
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`
	// ForceCutoverForVersion authorizes the migration engine to cut traffic over to the
	// target version's vhost EVEN THOUGH the source vhost has not drained cleanly within
	// DrainTimeout, AND to run cleanup that DISCARDS whatever remains in the source vhost —
	// including force-closing any connections still open on it. This is a deliberately
	// destructive escape hatch for an operator who has confirmed the remaining messages
	// are safe to lose.
	//
	// The value is scoped to ONE ATTEMPT, in two parts. It must equal the version the
	// migration is cutting over TO (status.upgrade.toVersion); and it must be set — or
	// re-set — AFTER that attempt began. A value this field already carried when the
	// migration started is recorded as carried over (status.upgrade.forceCarriedOver) and
	// authorizes nothing until it is cleared and set again, so a value left behind by an
	// aborted attempt can never silently authorize a later migration to the same version.
	// Once an attempt acts on the authorization it is recorded as consumed
	// (status.upgrade.forceAuthorized), so the cleanup still honours a force the cutover
	// used even if this field is cleared in between.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	ForceCutoverForVersion string `json:"forceCutoverForVersion,omitempty"`
}

// KeycloakSpec configures the platform's OIDC identity provider (Keycloak). It supports
// two modes: "external" (the platform's OIDC providers are configured directly in the
// application database — the operator provisions nothing) and "managed" (operator-
// provisioned via the Keycloak Operator — the platform renders a Keycloak CR that shares
// the platform database, plus an optional realm import). Keycloak is its own top-level
// concern, separate from database/messaging.
//
// The XValidation rule enforces that the managed block is present when mode=managed (the
// Keycloak CR sizing). Its omission would otherwise surface only as a late failure.
//
// NOTE: for a managed Keycloak the operator also wires Core's internal OIDC provider — once
// the Keycloak CR is Ready it relays the generated client secret into an operator-owned
// <platform>-oidc-client Secret that Core self-registers from in-pod (surfaced on the adjunct
// OIDCConfigured condition).
// +kubebuilder:validation:XValidation:rule="self.mode != 'managed' || has(self.managed)",message="keycloak.managed is required when mode=managed"
type KeycloakSpec struct {
	// Mode selects how the OIDC provider is provided: "external" (configure OIDC providers
	// directly in the application database — the operator provisions no Keycloak) or
	// "managed" (provision a Keycloak instance via the Keycloak Operator, which must be
	// installed in the cluster).
	// +kubebuilder:validation:Enum=external;managed
	// +kubebuilder:default=external
	Mode string `json:"mode,omitempty"`
	// Realm is the realm name the platform uses (defaults to "ilm"). For a managed Keycloak
	// it names the realm the platform's clients live in; the optional realm import seeds it.
	// It is constrained to the safe realm-name charset (letters, digits, dot, underscore and
	// hyphen, up to 255 characters) because the operator renders it into the OIDC provider URLs
	// of the in-pod registration request Core issues at startup.
	// +kubebuilder:default=ilm
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]{1,255}$`
	Realm string `json:"realm,omitempty"`
	// Managed configures the operator-provisioned Keycloak instance (Keycloak Operator).
	// Required when mode=managed; ignored otherwise.
	// +optional
	Managed *ManagedKeycloakSpec `json:"managed,omitempty"`
}

// ManagedKeycloakSpec configures the operator-provisioned Keycloak instance rendered as a
// Keycloak CR (Keycloak Operator) when keycloak.mode=managed. The operator owns the
// instance name, namespace, owner references, and the database wiring (Keycloak shares the
// platform database via the mode-agnostic readback); everything else is configurable here
// (and, as an escape hatch, via Overrides). No credentials are ever placed here — the
// Keycloak Operator generates the initial-admin Secret and the platform database
// credentials are consumed by reference.
type ManagedKeycloakSpec struct {
	// Instances is the number of Keycloak instances (1 = single instance; >1 forms a
	// clustered, HA-capable deployment managed by the Keycloak Operator).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Instances int32 `json:"instances,omitempty"`
	// Version is the Keycloak version to run (e.g. "26.6"). It selects the Keycloak
	// Operator container image; leave empty to use the operator's default.
	// +optional
	Version string `json:"version,omitempty"`
	// UpgradeAcknowledged opts in to a MAJOR Keycloak version upgrade of an
	// already-running managed instance. A major bump (e.g. 25.x → 26.x) runs a one-way
	// realm/database migration and the operand image version must stay aligned with the
	// Keycloak Operator; the operator therefore BLOCKS a major increase of the running
	// instance's version until this is set true, surfacing an actionable condition +
	// Warning instead of passing the new version straight through to the Keycloak
	// Operator. Patch/minor changes and the FIRST creation (no running version yet) apply
	// freely regardless of this flag. Reset it to false after the upgrade completes.
	// +optional
	UpgradeAcknowledged bool `json:"upgradeAcknowledged,omitempty"`
	// LogLevel sets the managed Keycloak server's root log level (KC_LOG_LEVEL), e.g. "info"
	// (the Keycloak default), "debug", or "warn". Keycloak also accepts a comma-separated,
	// category-specific form (e.g. "info,org.keycloak:debug"). Leave empty to use Keycloak's
	// default (info). Applies only to a managed Keycloak.
	// +optional
	LogLevel string `json:"logLevel,omitempty"`
	// RealmImport optionally provisions the platform realm from a user-provided ConfigMap
	// (create-only: the realm is imported once and not re-imported on every reconcile).
	// +optional
	RealmImport *KeycloakRealmImportSpec `json:"realmImport,omitempty"`
	// Overrides is a JSON-merge patch (RFC 7396) applied onto the rendered Keycloak CR spec,
	// an escape hatch for Keycloak settings the typed surface does not expose. Operator-owned
	// fields are protected: metadata.name/namespace/ownerReferences, the spec.db database
	// wiring, spec.hostname, and spec.proxy may not be overridden (the operator rejects such a
	// patch), so the database-sharing contract and the gateway X-Forwarded trust the platform
	// relies on cannot be broken.
	// +optional
	Overrides *runtime.RawExtension `json:"overrides,omitempty"`
}

// KeycloakRealmImportSpec references a user-provided ConfigMap whose key holds the realm
// representation JSON the operator imports into the managed Keycloak (create-only). The
// representation is data only — never a credential: the OIDC client secret is wired by the
// operator via a Secret reference (managed-Keycloak readback), and SMTP credentials, if
// needed, are supplied by Secret reference (not yet wired by the operator).
type KeycloakRealmImportSpec struct {
	// ConfigMapRef names a ConfigMap (same namespace as the Platform) whose key holds the
	// realm representation JSON.
	// +kubebuilder:validation:Required
	ConfigMapRef string `json:"configMapRef"`
	// Key is the ConfigMap key holding the realm representation JSON (defaults to
	// "realm.json").
	// +kubebuilder:default=realm.json
	Key string `json:"key,omitempty"`
}

// TrustedCertificatesSpec references a Secret holding the CA bundle injected into
// platform components as TRUSTED_CERTIFICATES. The value is never inlined; only the
// Secret name (and the in-Secret key) is held here (the operator reads the CA bundle
// via secretKeyRef / volume).
type TrustedCertificatesSpec struct {
	// SecretRef names a Secret whose CAKey holds the trusted CA bundle (PEM).
	SecretRef string `json:"secretRef,omitempty"`
	// CAKey is the key in the referenced Secret holding the CA bundle. When empty it
	// defaults to the operator's wiring-profile key ("ca.crt"). Set it to read a
	// differently-named key (e.g. a Vault/ESO-shaped bundle under "tls-ca").
	// +optional
	CAKey string `json:"caKey,omitempty"`
}

// HighAvailabilitySpec is the platform-wide high-availability profile. When Enabled the
// operator applies HA DEFAULTS to the STATELESS components (core, auth,
// scheduler, fe-administrator, auth-opa-policies, utils, gateway) that a
// component has not already overridden: a sane multi-replica count (for a component that
// sets neither replicas nor autoscaling), a PodDisruptionBudget (minAvailable 1), and pod
// anti-affinity spreading replicas across nodes. Every default is OVERRIDABLE per
// component — an explicit replicas / podDisruptionBudget / affinity on the component wins.
//
// Stateful MANAGED infrastructure (the CloudNativePG database, the RabbitMQ broker, and
// Keycloak) is OUT OF SCOPE here: their availability is the upstream operators' concern,
// configured via each managed block's own replica/instance count (e.g.
// database.managed.instances, messaging.managed.replicas, keycloak.managed.instances), not
// this profile.
type HighAvailabilitySpec struct {
	// Enabled turns the HA profile on. When false (the default) no HA defaults are applied
	// and components render with their own (or the operator's single-replica) settings.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
}

// LoggingSpec configures platform logging.
type LoggingSpec struct {
	// Level is the log level for the com.czertainly logger (defaults to "INFO").
	// +kubebuilder:default=INFO
	Level string `json:"level,omitempty"`
}

// OutboundProxySpec configures outbound HTTP(S) proxy support for platform components.
// When Enabled is true, PROXY_ENABLED is set; the HTTP/HTTPS/NoProxy values are
// only injected when non-empty.
type OutboundProxySpec struct {
	// Enabled turns on proxy support (sets PROXY_ENABLED=true).
	Enabled bool `json:"enabled,omitempty"`
	// HTTP is the HTTP proxy URL (injected as HTTP_PROXY when set).
	HTTP string `json:"http,omitempty"`
	// HTTPS is the HTTPS proxy URL (injected as HTTPS_PROXY when set).
	HTTPS string `json:"https,omitempty"`
	// NoProxy is the comma-separated no-proxy list (injected as NO_PROXY when set).
	NoProxy string `json:"noProxy,omitempty"`
}

// ProvisioningSpec configures the remote-proxy provisioning used by Core (held under
// spec.provisioning). It supports two modes: "external" (the default — Core CALLS
// your own provisioner at APIURL, e.g. an Azure Service Bus provisioner) and "deploy"
// (the operator RENDERS the bundled provisioning-rabbitmq service as a native, operator-
// managed component and points Core at it). The provisioning-rabbitmq service is
// RabbitMQ-specific, so mode=deploy requires messaging.brokerType=rabbitmq (enforced by a
// PlatformSpec CEL rule). The API key and the JWT signing key are ALWAYS Secret references,
// never inlined.
//
// The XValidation rules enforce that the mode-specific block is actually present: deploy
// needs the deploy block (the bootstrap Secret reference). Its omission would otherwise
// surface only as a late failure.
// +kubebuilder:validation:XValidation:rule="self.mode != 'deploy' || has(self.deploy)",message="provisioning.deploy is required when mode=deploy"
type ProvisioningSpec struct {
	// Mode selects how provisioning is provided: "external" (the default) points Core at
	// your own provisioner via APIURL/APIKeySecretRef (consume-only, today's behaviour);
	// "deploy" renders the bundled provisioning-rabbitmq service as an operator-managed
	// component (broker-wired via the platform messaging connection) and points Core at the
	// deployed Service instead. mode=deploy requires messaging.brokerType=rabbitmq.
	// +kubebuilder:validation:Enum=external;deploy
	// +kubebuilder:default=external
	Mode string `json:"mode,omitempty"`
	// APIURL is the provisioning API base URL (injected as PROVISIONING_API_URL). It is the
	// caller's provisioner URL for mode=external; ignored for mode=deploy (the operator
	// derives Core's PROVISIONING_API_URL from the deployed provisioning Service).
	APIURL string `json:"apiURL,omitempty"`
	// APIKeySecretRef names a Secret whose APIKey key holds the API key (injected as
	// PROVISIONING_API_KEY via secretKeyRef when set). For mode=external it is the caller's
	// Secret; for mode=deploy it is ignored (Core reads the API key from the deploy
	// bootstrap Secret, deploy.bootstrapSecretRef).
	APIKeySecretRef string `json:"apiKeySecretRef,omitempty"`
	// APIKey is the key in the referenced Secret holding the provisioning API key. When
	// empty it defaults to the operator's wiring-profile key ("provisioningApiKey"). Set it
	// to read a differently-named key. (mode=external; for mode=deploy the in-Secret key is
	// deploy.apiKeyKey.)
	// +optional
	APIKey string `json:"apiKey,omitempty"`
	// Deploy configures the operator-managed provisioning-rabbitmq component. Required when
	// mode=deploy; ignored otherwise.
	// +optional
	Deploy *ProvisioningDeploySpec `json:"deploy,omitempty"`
}

// ProvisioningDeploySpec configures the operator-managed provisioning-rabbitmq component
// rendered when provisioning.mode=deploy. The service dynamically manages per-proxy
// AMQP topology on the broker; the operator wires it to the platform messaging connection
// (the provisioner broker user) and Core's PROVISIONING_API_URL at its in-cluster Service.
//
// SECURITY: the JWT signing key and the API key are ALWAYS Secret references
// (BootstrapSecretRef), never inlined here or anywhere in the rendered objects, status,
// conditions, or logs. The broker credentials are likewise consumed by reference only.
type ProvisioningDeploySpec struct {
	// ComponentSpec is the shared per-component override surface (image, replicas,
	// resources, env, secret/configmap refs, volumes, probes, security context, pod
	// metadata, scheduling, init/sidecar passthrough, serviceAccount, service, metrics),
	// inline so the CRD stays flat — exactly as every other platform component embeds it.
	ComponentSpec `json:",inline"`
	// BootstrapSecretRef names the Secret holding the provisioning service's JWT signing
	// key and API key. It is REQUIRED for mode=deploy (the operator has no key to wire the
	// service's SECURITY_API_KEY / TOKEN_SIGNING_KEY from, nor Core's PROVISIONING_API_KEY,
	// otherwise). The values are consumed via secretKeyRef only — never inlined.
	// +kubebuilder:validation:Required
	BootstrapSecretRef string `json:"bootstrapSecretRef"`
	// APIKeyKey is the key in BootstrapSecretRef holding the API key (the value Core sends
	// as X-API-Key and the service validates). When empty it defaults to the operator's
	// wiring-profile key ("securityApiKey"). The same Secret+key backs both the service's
	// SECURITY_API_KEY and Core's PROVISIONING_API_KEY.
	// +optional
	APIKeyKey string `json:"apiKeyKey,omitempty"`
	// TokenSigningKeyKey is the key in BootstrapSecretRef holding the JWT signing key the
	// service uses to sign per-proxy tokens (must be at least 32 characters). When empty it
	// defaults to the operator's wiring-profile key ("tokenSigningKey").
	// +optional
	TokenSigningKeyKey string `json:"tokenSigningKeyKey,omitempty"`
	// ProvisionerCredentials references the Secret holding the BROKER provisioner user's
	// username/password (the admin user the service authenticates as to manage queues/
	// exchanges), with the usual usernameKey/passwordKey mapping. When omitted, the operator
	// defaults it to the platform messaging credentials (managed mode: the Topology-generated
	// provisioner-user Secret; external mode: spec.messaging.credentials). Consumed by
	// reference only.
	// +optional
	ProvisionerCredentials *CredentialsRef `json:"provisionerCredentials,omitempty"`
	// ProxyCredentials references the Secret holding the PROXY user's username/password — the
	// credentials the service embeds into the per-proxy JWT tokens it issues — with the usual
	// usernameKey/passwordKey mapping. When omitted, the operator defaults it to the platform
	// messaging credentials (managed mode: the Topology-generated proxy-user Secret; external
	// mode: spec.messaging.credentials). Consumed by reference only.
	// +optional
	ProxyCredentials *CredentialsRef `json:"proxyCredentials,omitempty"`
	// Exchange overrides the proxy exchange name; when empty, the selected platform
	// version's default applies (2.18.0: czertainly-proxy; 2.19.0+: ilm-proxy).
	// It is constrained to the safe RabbitMQ exchange-name charset (letters, digits, dot,
	// underscore, colon and hyphen, up to 255 characters) because the operator renders it into
	// the queue-registration request Core issues at startup.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:-]{1,255}$`
	Exchange string `json:"exchange,omitempty"`
	// RoutingKey overrides the binding routing key of the per-instance queue Core registers
	// at startup; when empty the operator's default "proxymessage.*.${HOSTNAME}" applies.
	// The literal ${HOSTNAME} token is substituted with the POD NAME at runtime (the init
	// container resolves it from `hostname`), so each replica binds its own queue.
	//
	// The charset is the AMQP binding-key charset — letters, digits, dot, underscore,
	// hyphen, colon and the topic wildcards * and # — plus exactly the three characters the
	// ${HOSTNAME} token needs ($, { and }). Allowing those three in the Pattern is simpler
	// and stricter than a CEL rule that special-cases the token, and it stays safe because
	// the value is JSON-encoded into a QUOTED heredoc the shell never expands (shell
	// metacharacters, quotes and whitespace remain rejected outright).
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._:*#${}-]{1,255}$`
	RoutingKey string `json:"routingKey,omitempty"`
	// QueueArguments are the queue arguments forwarded verbatim to the provisioning API in
	// the per-instance queue-registration request. The operator does not interpret them; the
	// set of valid arguments is defined by the provisioning service implementation in use.
	//
	// When empty the operator's default (x-expires: 1800000) applies. Setting this REPLACES
	// the default outright rather than merging with it: a service ignores arguments it does
	// not recognise and falls back to its own defaults, so a deployment running a different
	// provisioning service should state its own full set here. The arguments are sent only
	// when the queue is created and are not reconciled afterwards.
	//
	// The list is KEYED BY NAME (a list-map), so the apiserver rejects a repeated argument
	// name outright — a duplicate is a configuration mistake whose outcome would otherwise be
	// a silent last-one-wins. A CEL uniqueness rule would express the same thing, but its
	// O(n²) comparison blows the CRD's rule-cost budget; the keyed list costs nothing and
	// additionally gives server-side apply a per-argument merge key.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	QueueArguments []QueueArgument `json:"queueArguments,omitempty"`
	// ResponseQueue is the response queue name the service bootstraps for proxy replies.
	// When empty it defaults to the platform version bundle's value ("core").
	// +optional
	ResponseQueue string `json:"responseQueue,omitempty"`
}

// QueueArgument is a name/value queue argument of the per-instance queue-registration
// request the operator renders into Core's provision-instance-queue init container. The
// value is arbitrary JSON (numbers, strings, booleans, objects) because the provisioning
// service — not the operator — defines what each argument means.
type QueueArgument struct {
	// Name is the argument name (for example "x-expires"). It is constrained to the
	// conventional AMQP argument charset because the operator renders it into the
	// queue-registration request Core issues at startup, and it is the list's merge key,
	// so it must be unique across queueArguments.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]{1,255}$`
	Name string `json:"name"`

	// Value is the arbitrary JSON value of the argument, forwarded verbatim.
	Value apiextensionsv1.JSON `json:"value"`
}

// TimeQualityMonitorSpec configures the OPTIONAL time-quality-monitor SIDECAR on the Core pod
// — the second, INDEPENDENT half of the time-quality feature pair (the first is
// spec.messaging.timeQuality, which toggles Core's own integration). The chart keeps them
// separate so an EXTERNAL monitor, or a staged rollout, is possible, and the operator mirrors
// that: enabling one never enables the other.
//
// The sidecar's image comes from the selected platform version's bundle (2.19.0+, published to
// a PRIVATE repository), so a bundle that does not carry that image renders no sidecar at all
// and the same CR stays portable across platform versions — the same version-gating the
// bundled provisioning service uses.
//
// CREDENTIALS. The monitor authenticates to the broker as the platform's dedicated monitor
// user. In MANAGED messaging the operator wires the topology-generated monitor-user Secret
// automatically and Credentials may be left unset. In EXTERNAL messaging the operator manages
// no broker users, so Credentials.secretRef is REQUIRED (enforced by a PlatformSpec CEL rule).
// No credential value is ever held here — only a Secret reference.
//
// TWO BEHAVIORAL COUPLINGS to know before enabling this:
//
//  1. Enabling the monitor BLOCKS a messaging migration from starting. The sidecar rides
//     Core's pod, which a migration's fence never stops, so an enabled monitor is an
//     unfenced producer the drain cannot account for — disable it for the migration window
//     (spec.version move), then re-enable it once the move completes.
//  2. The sidecar's readiness probe gates the CORE POD's readiness (chart parity): Kubernetes
//     requires every container in a pod to be Ready before the pod is, so a failing monitor
//     takes Core out of its Service's endpoints along with it. The escape hatch is disabling
//     the sidecar (Enabled: false).
type TimeQualityMonitorSpec struct {
	// Enabled deploys the time-quality-monitor sidecar on the Core pod.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Image overrides the sidecar's image settings per field. Unset fields fall back to the
	// shared spec.common.image and then the version bundle (whose per-component repository —
	// the private one this image ships from — wins over a stock shared repository). PullSecrets
	// set here are unioned into the Core POD's imagePullSecrets, which is how a private sidecar
	// image is pulled alongside public component images.
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// Resources overrides the sidecar container's resource requirements (requests/limits).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Credentials references the Secret holding the monitor's broker username/password, and
	// lets the user map the in-Secret keys. REQUIRED when messaging.mode=external; ignored when
	// messaging.mode=managed (the operator reads back the topology-generated monitor-user
	// Secret, whose keys are the upstream operator's).
	// +optional
	Credentials *CredentialsRef `json:"credentials,omitempty"`
}

// CoreSpec configures the ILM Core component. It embeds the shared ComponentSpec
// (image, replicas, resources, env, secret/configmap refs, volumes, probes, security
// context, pod metadata, scheduling, init/sidecar passthrough, serviceAccount, service,
// metrics) and adds the Core-only wiring field ClientCertHeader (the client-certificate
// forwarding header the design review found is consumed solely by Core). Provisioning is a
// platform-level concern and lives at spec.provisioning, not here.
//
// The XValidation rules mirror the Helm chart's platformInstanceId guards. An explicit
// instance id is ONE value shared by every replica, and Core folds it into the certificate
// serial numbers it issues — so a multi-replica Core configured with an explicit id would emit
// IDENTICAL serials from different pods. Both rules are SPEC-ONLY (they fire only when
// instanceId is set), so no CR that exists today can be wedged by them.
// +kubebuilder:validation:XValidation:rule="!has(self.instanceId) || !has(self.replicas) || self.replicas <= 1",message="core.instanceId requires a single-replica core: one explicit id is shared by every replica, which would emit identical certificate serial numbers — for multi-replica core use workloadType: StatefulSet and leave instanceId unset"
// +kubebuilder:validation:XValidation:rule="!has(self.instanceId) || !has(self.autoscaling)",message="core.instanceId cannot be combined with core.autoscaling: an autoscaled core is multi-replica by definition — use workloadType: StatefulSet and leave instanceId unset"
type CoreSpec struct {
	// ComponentSpec is the shared per-component override surface (inline, so the CRD
	// stays flat).
	ComponentSpec `json:",inline"`
	// ClientCertHeader is the header name carrying the client certificate the edge
	// forwards to Core (wired as HEADER_NAME; HEADER_ENABLED is always set). Defaults
	// to "ssl-client-cert" when empty. OIDC providers themselves are configured in the
	// application database — this only covers the client-certificate forwarding header.
	// +optional
	ClientCertHeader string `json:"clientCertHeader,omitempty"`
	// InstanceID is Core's explicit platform instance id (0–65535), rendered as the selected
	// platform version's instance-id env var (2.19.0+: PLATFORM_INSTANCE_ID). Core folds it
	// into the certificate serial numbers it issues, so the value must be UNIQUE per running
	// Core pod.
	//
	// Set it ONLY on a single-replica Core (the CEL rules on this type, and the HA-profile rule
	// on PlatformSpec, reject every multi-replica combination). Leave it UNSET for the derived
	// paths: workloadType=StatefulSet derives a unique id per pod from the pod ordinal, and a
	// Deployment lets Core derive one from its pod IP (two pods sharing the last two IPv4
	// octets then collide, which is why StatefulSet is the multi-replica shape).
	//
	// A bundle whose wiring does not name the instance-id variable (pre-2.19.0) renders
	// nothing, so the same CR stays portable across platform versions.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +optional
	InstanceID *int32 `json:"instanceId,omitempty"`
	// TimeQualityMonitor configures the optional time-quality-monitor sidecar on the Core pod.
	// It is INDEPENDENT of spec.messaging.timeQuality (Core's own integration): the two are
	// separate controls, and either can be used without the other.
	//
	// Two behavioral couplings to know before enabling it: (1) it BLOCKS a messaging
	// migration from starting — the sidecar rides Core's pod, which a migration's fence
	// never stops, so an enabled monitor is an unfenced producer the drain cannot account
	// for; disable it for the migration window (the spec.version move), then re-enable it
	// once the move completes. (2) its readiness probe gates the CORE POD's readiness
	// (chart parity) — Kubernetes requires every container in a pod to be Ready before the
	// pod is, so a failing monitor takes Core out of its Service's endpoints along with it;
	// disabling the sidecar is the escape hatch.
	// +optional
	TimeQualityMonitor *TimeQualityMonitorSpec `json:"timeQualityMonitor,omitempty"`
}

// SchedulerSpec configures the scheduler component. It embeds the shared
// ComponentSpec and adds no scheduler-specific extras (its DB/messaging wiring is derived
// from the platform-level database/messaging specs).
type SchedulerSpec struct {
	// ComponentSpec is the shared per-component override surface (inline).
	ComponentSpec `json:",inline"`
}

// AuthOpaPoliciesSpec configures the auth-opa-policies component (the nginx bundle server
// that serves the OPA policy bundle to Core's OPA sidecar). It embeds the shared
// ComponentSpec and adds no component-specific extras.
type AuthOpaPoliciesSpec struct {
	// ComponentSpec is the shared per-component override surface (inline).
	ComponentSpec `json:",inline"`
}

// UtilsSpec controls the optional utils component. It embeds the shared
// ComponentSpec and adds the Enabled toggle (utils is opt-in).
type UtilsSpec struct {
	// ComponentSpec is the shared per-component override surface (inline).
	ComponentSpec `json:",inline"`
	// Enabled controls whether utils is deployed. Defaults to false.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
}

// GatewayCorsSpec configures the Kong cors plugin. It maps to
// https://docs.konghq.com/hub/kong-inc/cors.
type GatewayCorsSpec struct {
	// Enabled turns on the cors plugin in the gateway's declarative config.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// Origins is the list of allowed origins for Access-Control-Allow-Origin
	// (when empty and cors is enabled, defaults to the platform's own origin,
	// https://<host> using the edge host or common.hostName, falling back to
	// ["*"] only when no host is known).
	Origins []string `json:"origins,omitempty"`
	// ExposedHeaders is the list of values for Access-Control-Expose-Headers
	// (defaults to ["X-Auth-Token"] when empty and cors is enabled).
	ExposedHeaders []string `json:"exposedHeaders,omitempty"`
}

// GatewayLoggingSpec configures the gateway's request logging.
type GatewayLoggingSpec struct {
	// Request enables the Kong file-log plugin writing request logs to /dev/stdout.
	// +kubebuilder:default=false
	Request bool `json:"request,omitempty"`
}

// GatewaySpec configures the Kong API gateway and its declarative (DB-less)
// configuration. The gateway routes external traffic to the platform's internal
// services; the routes and plugins it renders are gated by these fields. Sensitive
// values are never configured here.
//
// For a MANAGED Keycloak the gateway renders a /kc route that proxies Keycloak's OIDC
// endpoints AND its admin console to the in-cluster Keycloak Service, so the managed Keycloak
// is reachable through the edge at https://<host>/kc/... (Keycloak serves under
// KC_HTTP_RELATIVE_PATH=/kc, strip_path=false so the prefix reaches it; the admin console is at
// /kc/admin/). The /kc route is rendered ONLY for a managed Keycloak — a non-managed/external
// Keycloak is reached at its own URL and gets no gateway route (see renderKongYAML).
type GatewaySpec struct {
	// ComponentSpec is the shared per-component override surface (image, replicas,
	// resources, env, scheduling, serviceAccount, ...), inline so the CRD stays flat.
	ComponentSpec `json:",inline"`
	// Cors configures the Kong cors plugin.
	Cors GatewayCorsSpec `json:"cors,omitempty"`
	// Logging configures gateway request logging (the file-log plugin).
	Logging GatewayLoggingSpec `json:"logging,omitempty"`
	// TrustedIPs is the list of trusted client IP ranges Kong honours for
	// X-Forwarded-* headers (sets KONG_TRUSTED_IPS; see the Kong trusted_ips
	// reference). To trust all, use ["0.0.0.0/0", "::/0"].
	TrustedIPs []string `json:"trustedIps,omitempty"`
}

// AuthCreateSpec controls auth's user/role auto-provisioning. All
// values map to non-sensitive AUTH_* environment variables on the auth
// container (the .NET service); credentials are never configured here.
type AuthCreateSpec struct {
	// CreateUnknownUsers sets AUTH_CREATE_UNKNOWN_USERS (default false): create a
	// user for an authenticated principal not yet present in the database.
	// +kubebuilder:default=false
	CreateUnknownUsers bool `json:"createUnknownUsers,omitempty"`
	// CreateUnknownRoles sets AUTH_CREATE_UNKNOWN_ROLES (default false): create a
	// role for a token role not yet present in the database.
	// +kubebuilder:default=false
	CreateUnknownRoles bool `json:"createUnknownRoles,omitempty"`
}

// AuthSpec configures the auth component. The operator composes
// auth's .NET database connection string from the platform's database
// coordinates and DB-credentials Secret and stores it in an owner-referenced Secret
// the workload reads via secretKeyRef; no credentials are ever configured inline here.
type AuthSpec struct {
	// ComponentSpec is the shared per-component override surface (image, replicas,
	// resources, env, scheduling, serviceAccount, ...), inline so the CRD stays flat.
	// The DB connection string is composed by the operator and injected via secretKeyRef
	// from an owner-referenced Secret — it is never set through env here.
	ComponentSpec `json:",inline"`
	// Create controls user/role auto-provisioning behaviour.
	Create AuthCreateSpec `json:"create,omitempty"`
	// SyncPolicy sets SYNC_POLICY (default "create-only"): "create-only" creates
	// users/roles from the token without later syncing changes; "sync-data" keeps
	// the stored user's properties and roles in sync with the token.
	// +kubebuilder:validation:Enum=create-only;sync-data
	// +kubebuilder:default=create-only
	SyncPolicy string `json:"syncPolicy,omitempty"`
}

// FeAdministratorURLSpec holds the front-end administrator's runtime URLs, injected
// into the served config.js (window.__ENV__). These are non-sensitive routing paths.
type FeAdministratorURLSpec struct {
	// Base is the application's base path (informational; not emitted into config.js).
	Base string `json:"base,omitempty"`
	// API is the API base URL (config.js API_URL); defaults to "/api".
	// +kubebuilder:default=/api
	API string `json:"api,omitempty"`
	// Login is the login URL (config.js LOGIN_URL); defaults to "/login".
	// +kubebuilder:default=/login
	Login string `json:"login,omitempty"`
	// Logout is the logout URL (config.js LOGOUT_URL); defaults to "/logout".
	// +kubebuilder:default=/logout
	Logout string `json:"logout,omitempty"`
}

// FeAdministratorSpec configures the fe-administrator component: a static nginx
// front-end served with a generated config.js (window.__ENV__) carrying the API
// and login/logout URLs plus the proxy-enabled flag (from spec.common.proxy.enabled).
type FeAdministratorSpec struct {
	// ComponentSpec is the shared per-component override surface (image, replicas,
	// resources, env, scheduling, serviceAccount, ...), inline so the CRD stays flat.
	ComponentSpec `json:",inline"`
	// URL holds the runtime routing URLs injected into config.js.
	URL FeAdministratorURLSpec `json:"url,omitempty"`
}

// LetsEncryptSpec configures the ACME (Let's Encrypt) issuer used when
// edge.tls.source is "letsEncrypt": the email registered with the ACME account and the
// environment selecting the staging or production ACME directory. No secret material is
// held here — the account private key is created and stored by cert-manager.
type LetsEncryptSpec struct {
	// Email is the contact address registered with the ACME account (required for
	// Let's Encrypt).
	Email string `json:"email,omitempty"`
	// Environment selects the ACME directory: "staging" (acme-staging-v02) or
	// "production" (acme-v02).
	// +kubebuilder:validation:Enum=staging;production
	// +kubebuilder:default=production
	Environment string `json:"environment,omitempty"`
}

// EdgeTLSSpec configures how the edge obtains its serving certificate: "internal"
// provisions a self-signed CA and a cert-manager ingress-shim leaf certificate;
// "letsEncrypt" provisions an ACME issuer; "issuerRef" points the cert-manager shim at
// any existing Issuer/ClusterIssuer (Vault, corporate CA, Venafi, ...) the operator does
// not create; "secret" uses a caller-provided (bring-your-own) TLS Secret with no
// cert-manager objects. No certificate or key material is ever held here — TLS is
// always referenced by Secret name only.
//
// The XValidation rules enforce that the source-specific block is actually present:
// letsEncrypt needs an ACME contact email, issuerRef needs the issuer reference, and
// secret needs the bring-your-own TLS secretRef. Without them the edge would render
// an incomplete cert request and only surface a late EdgeReady=False.
// +kubebuilder:validation:XValidation:rule="self.source != 'letsEncrypt' || (has(self.letsEncrypt) && has(self.letsEncrypt.email) && self.letsEncrypt.email.size() > 0)",message="edge.tls.letsEncrypt.email is required when tls.source=letsEncrypt"
// +kubebuilder:validation:XValidation:rule="self.source != 'issuerRef' || has(self.issuerRef)",message="edge.tls.issuerRef is required when tls.source=issuerRef"
// +kubebuilder:validation:XValidation:rule="self.source != 'secret' || (has(self.secretRef) && self.secretRef.size() > 0)",message="edge.tls.secretRef is required when tls.source=secret"
type EdgeTLSSpec struct {
	// Source selects how the serving certificate is obtained: "internal" (self-signed
	// CA via cert-manager), "letsEncrypt" (ACME), "issuerRef" (any existing
	// cert-manager Issuer/ClusterIssuer signs the leaf via the shim), or "secret"
	// (bring-your-own TLS Secret, no cert-manager objects).
	// +kubebuilder:validation:Enum=internal;letsEncrypt;secret;issuerRef
	// +kubebuilder:default=internal
	Source string `json:"source,omitempty"`
	// SecretRef names the TLS Secret used as the Ingress tls secretName. For
	// source="secret" it is the caller-provided cert (required); for "internal",
	// "letsEncrypt", and "issuerRef" it overrides the default secret name
	// cert-manager populates (defaults to "ilm-ingress-tls").
	SecretRef *string `json:"secretRef,omitempty"`
	// LetsEncrypt configures the ACME issuer; required when source="letsEncrypt".
	LetsEncrypt *LetsEncryptSpec `json:"letsEncrypt,omitempty"`
	// IssuerRef references an existing cert-manager Issuer/ClusterIssuer that signs
	// the edge serving certificate. Required when source="issuerRef".
	IssuerRef *CertManagerIssuerRef `json:"issuerRef,omitempty"`
}

// RegisterAdminSpec configures the optional first-admin bootstrap. It is disabled
// by default. When enabled, it provisions the platform's first administrator via two
// INDEPENDENT, separately-togglable methods — a client CERTIFICATE admin (mTLS) and/or
// a PASSWORD admin (a Keycloak realm user) — sharing one identity (username/name/email).
//
//   - certificate (the default, on unless disabled): the operator wires Core's ADMIN_CERT
//     env from the admin client-certificate Secret and (for source=generated) provisions
//     that certificate declaratively via a cert-manager Certificate. See
//     AdminCertificateSpec.
//   - password (opt-in, requires keycloak.mode=managed): the operator creates a Keycloak
//     realm user (the superadmin attribute) whose password comes from a caller-provided
//     Secret. See AdminPasswordSpec.
//
// The operator NEVER generates credentials imperatively — no openssl, no kubectl, NO
// operator-minted password. The certificate keypair lives only in a cert-manager-managed
// Secret and the admin password lives only in a caller-provided Secret, both referenced by
// name; no certificate, key, or password material is ever inlined here.
//
// The XValidation rules close two silent-misconfiguration traps: an enabled password method
// must reference its password Secret, and an enabled bootstrap must enable at least one
// method (otherwise it is a no-op the user likely did not intend). The PlatformSpec-level
// rule additionally gates the password method on a managed Keycloak (the operator creates
// the realm user via the Keycloak admin API, which only exists for managed mode).
// +kubebuilder:validation:XValidation:rule="!has(self.password) || !has(self.password.enabled) || !self.password.enabled || (has(self.password.secretRef) && self.password.secretRef.size() > 0)",message="registerAdmin.password.secretRef is required when password.enabled is true"
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || !has(self.certificate) || !has(self.certificate.enabled) || self.certificate.enabled || (has(self.password) && has(self.password.enabled) && self.password.enabled)",message="registerAdmin requires at least one method: enable certificate or password"
type RegisterAdminSpec struct {
	// Enabled turns on the admin bootstrap. When false (the default) neither the
	// certificate nor the password method is reconciled (no ADMIN_CERT env, no admin
	// cert-manager objects, no Keycloak realm user).
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Username is the admin's login name — the certificate Subject CommonName and the
	// Keycloak realm user's username. Shared by both methods.
	Username string `json:"username,omitempty"`
	// Name is the admin's first/display name — registered in Core via the certificate method
	// and set as the Keycloak realm user's firstName. Shared by both methods.
	Name string `json:"name,omitempty"`
	// LastName is the admin's surname — set as the Keycloak realm user's lastName (password
	// method) and the cert admin's last name in Core. Optional, but recommended: without it
	// Keycloak prompts the admin to complete their profile (add a last name) on first login.
	// Shared by both methods.
	// +optional
	LastName string `json:"lastName,omitempty"`
	// Email is the admin's email — registered in Core via the certificate method and set
	// as the Keycloak realm user's email. Shared by both methods.
	Email string `json:"email,omitempty"`
	// Certificate configures the client-certificate (mTLS) admin method. When omitted it
	// defaults to ON (the certificate method is the historical single behaviour), so an
	// enabled registerAdmin with no certificate/password sub-block still bootstraps the
	// certificate admin. Set certificate.enabled=false to run password-only.
	// +optional
	Certificate *AdminCertificateSpec `json:"certificate,omitempty"`
	// Password configures the password (Keycloak realm user) admin method. It is OFF
	// unless present with enabled=true, and requires keycloak.mode=managed (enforced by a
	// PlatformSpec CEL rule — the operator creates the realm user via the Keycloak admin
	// API). The password is ALWAYS a referenced Secret, never operator-minted.
	// +optional
	Password *AdminPasswordSpec `json:"password,omitempty"`
}

// AdminCertificateSpec configures the client-certificate (mTLS) admin method of the
// first-admin bootstrap (spec.registerAdmin.certificate). When enabled, the operator wires
// Core's ADMIN_CERT env from the admin client-certificate Secret and (for source=generated)
// provisions that certificate declaratively via a cert-manager Certificate. No certificate
// or key material is ever inlined here: the keypair lives only in a (caller-provided or
// cert-manager-managed) Secret, referenced by name.
//
// When this method is enabled with source=provided, the caller-supplied SecretRef is
// required (the operator has no Secret to wire ADMIN_CERT from otherwise); the XValidation
// enforces it instead of letting it surface as a late failure.
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || self.source != 'provided' || (has(self.secretRef) && self.secretRef.size() > 0)",message="registerAdmin.certificate.secretRef is required when certificate.source=provided"
type AdminCertificateSpec struct {
	// Enabled turns on the certificate admin method. It DEFAULTS TO TRUE (opt-out): an
	// omitted block or an unset value is treated as enabled, so an enabled registerAdmin
	// keeps the historical single-method (certificate) behaviour out of the box. Set it to
	// false to run password-only. It is a pointer so an explicit false is distinguishable
	// from unset (a non-pointer bool with the true default would silently re-enable on the
	// wire when set to its zero value).
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Source selects where the admin client certificate comes from: "provided" (the
	// caller supplies the Secret named by SecretRef) or "generated" (cert-manager
	// issues it from IssuerRef, defaulting to the platform internal CA).
	// +kubebuilder:validation:Enum=provided;generated
	// +kubebuilder:default=provided
	Source string `json:"source,omitempty"`
	// SecretRef names the admin client-certificate Secret (kubernetes.io/tls, with
	// tls.crt/tls.key). It is required when source=provided; for source=generated it
	// is ignored (cert-manager populates a fixed admin certificate Secret).
	SecretRef *string `json:"secretRef,omitempty"`
	// CertKey is the key in the provided Secret holding the admin client certificate
	// (PEM). When empty it defaults to the operator's wiring-profile key ("tls.crt"). It
	// applies to source=provided only; for source=generated the cert-manager-populated
	// Secret uses the standard kubernetes.io/tls keys (tls.crt/tls.key).
	// +optional
	CertKey string `json:"certKey,omitempty"`
	// PrivateKeyKey is the key in the provided Secret holding the admin client private key
	// (PEM). When empty it defaults to the operator's wiring-profile key ("tls.key"). It
	// applies to source=provided only (see CertKey).
	// +optional
	PrivateKeyKey string `json:"privateKeyKey,omitempty"`
	// IssuerRef (source=generated) is the cert-manager Issuer/ClusterIssuer that signs
	// the admin certificate. When unset, the operator defaults to the platform internal
	// CA: it reuses the edge's internal CA issuer when the edge already provisions one
	// (edge.tls.source=internal), otherwise it provisions a dedicated self-signed admin
	// CA chain so generated works standalone. Reuses CertManagerIssuerRef.
	IssuerRef *CertManagerIssuerRef `json:"issuerRef,omitempty"`
}

// AdminPasswordSpec configures the password (Keycloak realm user) admin method of the
// first-admin bootstrap (spec.registerAdmin.password). When enabled, the operator creates a
// Keycloak realm user carrying the superadmin user attribute (groups: ["superadmin"], which
// the operator's default realm surfaces as the "roles" claim Core grants superadmin on),
// with a password sourced from the caller-provided Secret.
//
// SECURITY: the password is ALWAYS a referenced Secret, NEVER operator-minted and never
// inlined here. The operator reads the Secret read-only and sends the value to Keycloak's
// admin API once (to create the user); the value never reaches logs, status, conditions,
// events, or any rendered object. It requires keycloak.mode=managed (enforced by a
// PlatformSpec CEL rule), since the operator needs the Keycloak admin API to create the user.
type AdminPasswordSpec struct {
	// Enabled turns on the password admin method. It is OFF by default (the zero value):
	// the password admin is provisioned only when this is explicitly true.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// SecretRef names the Secret holding the admin password. It is REQUIRED when
	// password.enabled is true (the operator has no password to set on the realm user
	// otherwise; the XValidation enforces it). The value is consumed read-only and never
	// inlined or logged.
	SecretRef string `json:"secretRef,omitempty"`
	// PasswordKey is the key in SecretRef holding the password. When empty it defaults to
	// "password".
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// CertManagerIssuerRef references an existing cert-manager Issuer or ClusterIssuer
// the cert-manager ingress/gateway shim uses to mint the edge serving certificate.
// The operator creates no Issuer of its own for this source — it only stamps the
// shim annotations naming this issuer onto the Ingress/Gateway, and cert-manager
// provisions the leaf into the edge TLS Secret. No certificate or key material is
// ever held here.
type CertManagerIssuerRef struct {
	// Name of the Issuer/ClusterIssuer.
	Name string `json:"name"`
	// Kind is Issuer (namespaced) or ClusterIssuer.
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default=Issuer
	Kind string `json:"kind,omitempty"`
	// Group defaults to cert-manager.io.
	// +kubebuilder:default=cert-manager.io
	// +optional
	Group string `json:"group,omitempty"`
}

// EdgeSpec configures the platform's edge routing external traffic to the
// api-gateway (Kong) consumer port, paired with cert-manager TLS. The edge is
// either a Kubernetes Ingress (type="ingress", the default) or a Gateway API
// HTTPRoute/Gateway (type="gatewayAPI"). When nil or Enabled is false, the operator
// renders no edge objects.
//
// The XValidation rule closes a silent-misconfiguration trap: a gatewayAPI edge must say
// where to attach (own a Gateway via gatewayClassName, or attach to an existing one via
// parentRef) or no route is reachable — the omission would otherwise surface only as a late
// EdgeReady=False (or not at all).
//
// The "enabled edge needs a host" check lives on PlatformSpec, NOT here: the served hostname
// (and TLS host) may come from EITHER edge.host or the canonical spec.common.hostName, and only
// the platform root can see both sub-trees.
// +kubebuilder:validation:XValidation:rule="self.type != 'gatewayAPI' || (has(self.gatewayAPI) && (has(self.gatewayAPI.gatewayClassName) || has(self.gatewayAPI.parentRef)))",message="edge.type=gatewayAPI requires gatewayAPI.gatewayClassName or gatewayAPI.parentRef"
type EdgeSpec struct {
	// Enabled turns the edge on. When false (or the whole block is omitted) no
	// edge or cert-manager objects are rendered.
	Enabled bool `json:"enabled,omitempty"`
	// Type selects the edge implementation: "ingress" (a networking.k8s.io Ingress,
	// the default) or "gatewayAPI" (a gateway.networking.k8s.io HTTPRoute, optionally
	// with an operator-owned Gateway — the non-deprecated Ingress successor). "route"
	// (OpenShift) is reserved for a later milestone.
	// +kubebuilder:validation:Enum=ingress;gatewayAPI
	// +kubebuilder:default=ingress
	Type string `json:"type,omitempty"`
	// ClassName is the ingress class (e.g. "nginx"). The operator sets it as the Ingress's
	// spec.ingressClassName (the modern, non-deprecated selector) and references it from the
	// Let's Encrypt http01 solver. It applies to the Ingress edge only; the Gateway API edge
	// selects its class via gatewayAPI.gatewayClassName instead.
	ClassName *string `json:"className,omitempty"`
	// Host is the external hostname the edge serves and the TLS host. When empty it
	// falls back to the canonical spec.common.hostName. It is constrained to the hostname
	// charset (letters, digits, dot, underscore and hyphen, up to 253 characters), optionally
	// with a leading "*." wildcard label as Ingress rules and Gateway API listeners accept —
	// defence in depth for the browser-facing URLs the operator derives from it.
	// +kubebuilder:validation:Pattern=`^(\*\.)?[a-zA-Z0-9._-]{1,253}$`
	Host string `json:"host,omitempty"`
	// Annotations are extra annotations merged onto the Ingress (e.g. the nginx
	// auth-tls and backend-protocol settings). They apply to the Ingress edge only.
	Annotations map[string]string `json:"annotations,omitempty"`
	// TLS configures how the serving certificate is obtained. When nil the edge
	// renders no TLS block (Ingress) or no HTTPS listener cert wiring (Gateway API).
	TLS *EdgeTLSSpec `json:"tls,omitempty"`
	// GatewayAPI configures the Gateway API edge; used when type="gatewayAPI".
	GatewayAPI *GatewayAPISpec `json:"gatewayAPI,omitempty"`
}

// GatewayAPISpec configures the Gateway API edge (used when
// edge.type="gatewayAPI"). It selects between the operator owning its own
// Gateway (set GatewayClassName) or attaching its HTTPRoute to an existing,
// separately-managed Gateway (set ParentRef). The two are mutually exclusive in
// effect: ParentRef takes precedence when both are set, and the operator then
// renders only the HTTPRoute.
type GatewayAPISpec struct {
	// GatewayClassName, when set, makes the operator render its OWN Gateway of this
	// class (plus the HTTPRoute and cert-manager objects). Leave unset to attach to
	// an existing Gateway via ParentRef instead.
	GatewayClassName *string `json:"gatewayClassName,omitempty"`
	// ParentRef attaches the HTTPRoute to an EXISTING Gateway (the idiomatic
	// separation-of-concerns model). When set, the operator renders ONLY the
	// HTTPRoute and no Gateway/cert-manager objects (TLS is the Gateway's concern).
	ParentRef *GatewayParentRef `json:"parentRef,omitempty"`
}

// GatewayParentRef identifies an existing Gateway (and optionally a specific
// listener) the operator's HTTPRoute attaches to. It mirrors the Gateway API
// ParentReference fields the operator needs.
type GatewayParentRef struct {
	// Name is the name of the existing Gateway to attach the HTTPRoute to.
	Name string `json:"name"`
	// Namespace is the Gateway's namespace; defaults to the HTTPRoute's own
	// namespace when unset.
	// +optional
	Namespace *string `json:"namespace,omitempty"`
	// SectionName selects a specific listener on the Gateway by name; when unset
	// the route attaches to all compatible listeners.
	// +optional
	SectionName *string `json:"sectionName,omitempty"`
}

// PlatformPhase is a high-level summary of the platform lifecycle state.
type PlatformPhase string

// Platform lifecycle phase constants. The reconciler sets Progressing while a
// required Deployment (Core, and the auth provider) is not yet at desired replicas,
// Running once readiness is measured (not asserted) as met, or Degraded on a fatal
// error. The enum is kept honest to exactly the values the code emits, so
// callers/tooling never see a phase the operator can't set.
const (
	// PlatformPhaseProgressing means the platform's children are applied but at least
	// one required Deployment has not yet reached its desired ready replicas.
	PlatformPhaseProgressing PlatformPhase = "Progressing"
	PlatformPhaseRunning     PlatformPhase = "Running"
	PlatformPhaseDegraded    PlatformPhase = "Degraded"
)

// PlatformDeletionPolicy selects what happens to managed infrastructure when a
// Platform is deleted.
type PlatformDeletionPolicy string

// Platform deletion-policy constants.
const (
	// PlatformDeletionPolicyRetain leaves managed (upstream-operator) infrastructure
	// and its data intact on Platform deletion (the default and safe choice).
	PlatformDeletionPolicyRetain PlatformDeletionPolicy = "Retain"
	// PlatformDeletionPolicyDelete reclaims the managed (upstream-operator) infrastructure
	// — the CloudNativePG cluster, RabbitMQ broker, and Keycloak instance — and its data
	// on Platform deletion.
	PlatformDeletionPolicyDelete PlatformDeletionPolicy = "Delete"
)

// PlatformSpec is the desired state of an ILM platform instance.
//
// The cross-field XValidation rule gates the bundled provisioning service on a compatible
// broker: provisioning.mode=deploy renders the provisioning-rabbitmq service, which is
// RabbitMQ-specific, so it is only valid when messaging.brokerType=rabbitmq. The rule lives
// here (not on ProvisioningSpec) because it must see BOTH provisioning.mode and
// messaging.brokerType, which are in different sub-trees; messaging.brokerType defaults to
// rabbitmq, so an out-of-the-box deploy is accepted. (Until the validating webhook lands,
// this CEL rule is the create-time guard; the builder also no-ops the render for a
// non-rabbitmq broker, so a stale object can never render a broken service.)
// +kubebuilder:validation:XValidation:rule="!has(self.provisioning) || self.provisioning.mode != 'deploy' || self.messaging.brokerType == 'rabbitmq'",message="provisioning.mode=deploy requires messaging.brokerType=rabbitmq (the provisioning-rabbitmq service is RabbitMQ-specific)"
// A second cross-field rule: an enabled edge needs a public host, satisfied by EITHER
// edge.host (per-edge override) OR the canonical common.hostName. It lives here (not on
// EdgeSpec) because common.hostName is in a sibling sub-tree the EdgeSpec rule cannot see —
// so a hand-written CR that sets only common.hostName with an enabled edge is accepted.
// +kubebuilder:validation:XValidation:rule="!has(self.edge) || !has(self.edge.enabled) || !self.edge.enabled || (has(self.edge.host) && self.edge.host.size() > 0) || (has(self.common) && has(self.common.hostName) && self.common.hostName.size() > 0)",message="an enabled edge needs a public host: set edge.host or common.hostName"
//
// A third cross-field rule gates the password admin method on a MANAGED Keycloak: the
// operator creates the realm user via the Keycloak admin API, which only exists when the
// operator provisions Keycloak (keycloak.mode=managed). It lives here (not on
// RegisterAdminSpec) because keycloak.mode is in a sibling sub-tree the RegisterAdminSpec
// rule cannot see. The certificate method has no such requirement.
// +kubebuilder:validation:XValidation:rule="!has(self.registerAdmin) || !has(self.registerAdmin.password) || !has(self.registerAdmin.password.enabled) || !self.registerAdmin.password.enabled || (has(self.keycloak) && self.keycloak.mode == 'managed')",message="registerAdmin.password requires keycloak.mode=managed (the operator creates the realm user via the Keycloak admin API)"
//
// A fourth cross-field rule closes the HA hole in core.instanceId's own guards: the HA profile
// gives a component that sets NEITHER replicas NOR autoscaling a multi-replica default, which
// the CoreSpec rules cannot see (highAvailability is in a sibling sub-tree). With HA enabled,
// an explicit instanceId therefore requires an explicit single-replica core.
// +kubebuilder:validation:XValidation:rule="!has(self.core) || !has(self.core.instanceId) || !has(self.highAvailability) || !has(self.highAvailability.enabled) || !self.highAvailability.enabled || (has(self.core.replicas) && self.core.replicas <= 1)",message="core.instanceId with highAvailability.enabled requires an explicit single-replica core (set core.replicas: 1): the HA profile would otherwise give core a multi-replica default, and one explicit id shared by every replica emits identical certificate serial numbers"
//
// A fifth cross-field rule gates the time-quality-monitor sidecar's credentials on the broker
// mode: with a MANAGED broker the operator wires the topology-generated monitor-user Secret,
// but with an EXTERNAL broker it manages no users and has nothing to wire, so the CR must name
// a Secret. It lives here (not on TimeQualityMonitorSpec) because messaging.mode is in a
// sibling sub-tree that rule cannot see.
// +kubebuilder:validation:XValidation:rule="!has(self.core) || !has(self.core.timeQualityMonitor) || !has(self.core.timeQualityMonitor.enabled) || !self.core.timeQualityMonitor.enabled || self.messaging.mode == 'managed' || (has(self.core.timeQualityMonitor.credentials) && has(self.core.timeQualityMonitor.credentials.secretRef) && self.core.timeQualityMonitor.credentials.secretRef.size() > 0)",message="core.timeQualityMonitor.credentials.secretRef is required when messaging.mode=external (with an external broker the operator has no generated monitor-user Secret to wire)"
type PlatformSpec struct {
	// Version selects which platform version bundle the operator reconciles this
	// Platform against — the tested set of component images, env-var wiring, and managed-
	// RabbitMQ topology for that version. When empty, the operator uses its DEFAULT shipped
	// version (not necessarily the newest one it carries — a newer released version can
	// exist and is reachable by naming it explicitly), so an existing CR keeps today's
	// behaviour. One operator build carries a supported RANGE of versions, so you can run a
	// canary version in one namespace, or take an operator fix without moving the platform.
	//
	// This is intentionally NOT a CEL enum: the supported set grows as the operator ships
	// new versions, so the value is validated at RUNTIME (and, in a later milestone, by the
	// validating webhook) against the bundles this build carries. An unknown version does
	// not crash the operator — it surfaces as a Degraded condition listing the supported
	// versions. status.observedVersion reports the resolved version.
	// +optional
	Version string `json:"version,omitempty"`
	// Common holds configuration applied to EVERY platform component: shared image
	// defaults, outbound proxy, log level, the trusted-CA bundle, the platform's public
	// hostName, and the fleet-wide pod-template passthrough (init/sidecar containers,
	// volumes, ports, envFrom). Each component's inline ComponentSpec overrides/augments it.
	// +optional
	Common CommonSpec `json:"common,omitempty"`
	// Database is the required database connection (external or managed).
	// +kubebuilder:validation:Required
	Database DatabaseSpec `json:"database"`
	// Messaging is the required AMQP broker connection (external or managed).
	// +kubebuilder:validation:Required
	Messaging MessagingSpec `json:"messaging"`
	// Keycloak configures the platform's OIDC identity provider. When nil or mode=external,
	// the operator provisions no Keycloak (configure OIDC providers in the application
	// database); mode=managed provisions a Keycloak instance via the Keycloak Operator that
	// shares the platform database.
	// +optional
	Keycloak *KeycloakSpec `json:"keycloak,omitempty"`
	// HighAvailability is the platform-wide HA profile. When enabled the operator applies
	// HA defaults (multi-replica counts, a per-component PodDisruptionBudget, and pod
	// anti-affinity) to the stateless components, each overridable per component. Stateful
	// managed infrastructure HA is the upstream operators' concern (see HighAvailabilitySpec).
	// +optional
	HighAvailability *HighAvailabilitySpec `json:"highAvailability,omitempty"`
	// AdditionalEnv lists extra environment variables applied to every platform component
	// (e.g. OTEL_SDK_DISABLED). These are non-sensitive inline variables; secret env must
	// be supplied via the dedicated Secret references, never inline here. They are merged
	// BEFORE each component's own/derived env, so a component-specific variable of the same
	// name overrides a global one.
	// +optional
	AdditionalEnv []EnvVar `json:"additionalEnv,omitempty"`
	// NetworkPolicy configures the platform's default-deny network isolation. It is
	// ENABLED by default (opt-out): the operator renders networking.k8s.io/v1
	// NetworkPolicies that DENY cross-namespace/external ingress to the platform's pods
	// while leaving intra-platform traffic and the edge -> api-gateway path open
	// (egress stays permissive). Set networkPolicy.enabled=false to render none (e.g.
	// on a CNI without NetworkPolicy support). See NetworkPolicySpec.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
	// Core configures the ILM Core component (including the client-certificate
	// forwarding header).
	Core CoreSpec `json:"core,omitempty"`
	// Provisioning configures the remote-proxy provisioning Core is wired to. It is a
	// platform concern (not Core config): mode=external points Core at your own
	// provisioner, while mode=deploy renders the bundled provisioning-rabbitmq service as a
	// standalone operator-managed component and points Core at it. The operator derives
	// Core's PROVISIONING_API_URL/PROVISIONING_API_KEY from this block; the API key is
	// always a Secret reference, never inlined.
	// +optional
	Provisioning *ProvisioningSpec `json:"provisioning,omitempty"`
	// Auth configures the auth component (user/role provisioning,
	// sync policy). The DB connection string is composed by the operator, never set here.
	Auth AuthSpec `json:"auth,omitempty"`
	// Scheduler configures the scheduler component.
	Scheduler SchedulerSpec `json:"scheduler,omitempty"`
	// AuthOpaPolicies configures the auth-opa-policies component (the OPA bundle server).
	AuthOpaPolicies AuthOpaPoliciesSpec `json:"authOpaPolicies,omitempty"`
	// FeAdministrator configures the fe-administrator front-end (runtime config.js URLs).
	FeAdministrator FeAdministratorSpec `json:"feAdministrator,omitempty"`
	// Utils controls the optional utils component.
	Utils UtilsSpec `json:"utils,omitempty"`
	// Gateway configures the Kong API gateway and its declarative routing/plugins.
	Gateway GatewaySpec `json:"gateway,omitempty"`
	// Edge configures the platform's external edge (Ingress + cert-manager TLS).
	// When nil or disabled, no edge objects are rendered.
	Edge *EdgeSpec `json:"edge,omitempty"`
	// RegisterAdmin configures optional first-admin bootstrap. Disabled by default;
	// when enabled it gates Core's ADMIN_CERT env and (for source=generated) a
	// cert-manager-issued admin client certificate.
	RegisterAdmin *RegisterAdminSpec `json:"registerAdmin,omitempty"`
	// DeletionPolicy selects what happens to managed (upstream-operator) infrastructure
	// when this Platform is deleted. "Retain" (the default) leaves managed infrastructure
	// and its data intact; "Delete" reclaims the managed CloudNativePG cluster, RabbitMQ
	// broker, and Keycloak instance on Platform deletion. The operator's own namespaced
	// children (Deployments/Services/ServiceAccounts/ConfigMaps/Secrets) are reclaimed by
	// owner-reference garbage collection under BOTH policies; deletionPolicy governs only
	// the managed infrastructure (which carries no controller owner ref and is
	// prune-excluded).
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy PlatformDeletionPolicy `json:"deletionPolicy,omitempty"`
}

// NetworkPolicySpec controls the platform's default-deny network isolation
// (spec.networkPolicy). It is ENABLED BY DEFAULT (opt-out): when Enabled is unset or
// true the operator renders networking.k8s.io/v1 NetworkPolicies that isolate the
// platform's pods from cross-namespace/external reach while keeping every flow the
// platform needs open. The policies are intentionally a SAFE default-deny — they cannot
// break intra-platform traffic:
//
//   - INGRESS default-deny + intra-namespace allow: an ingress policy selects the
//     platform's pods (by app.kubernetes.io/part-of + instance) and allows ingress only
//     from pods in the SAME namespace (the platform's own components talk freely),
//     denying all other (cross-namespace / external) ingress by default. This is the
//     high-value, low-risk isolation: it blocks external reach without micromanaging the
//     intra-platform flows.
//   - EDGE allow: a second ingress policy allows ingress to the api-gateway on its
//     consumer port from the ingress-controller / Gateway namespace (default the
//     well-known ingress-nginx namespace, overridable via IngressNamespace) so the edge
//     entrypoint keeps working.
//   - EGRESS stays PERMISSIVE: an egress policy allows all egress (DNS + intra-namespace +
//     the DB/messaging/Keycloak endpoints). The primary security win is the ingress
//     default-deny; a too-strict egress is the easiest way to break managed-infra /
//     external connectivity, so tighter egress is left as a FUTURE hardening knob.
//
// SECURITY: the policies carry NO connection coordinates — only label selectors and the
// (non-secret) ingress-controller namespace name.
type NetworkPolicySpec struct {
	// Enabled turns the default-deny NetworkPolicies on. It DEFAULTS TO TRUE (opt-out):
	// a nil pointer or an unset value is treated as enabled, so an out-of-the-box platform
	// is network-isolated. Set it to false to render no NetworkPolicies (e.g. on a CNI
	// that does not enforce them, or when an external policy controller owns isolation).
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// IngressNamespace is the namespace the cluster's ingress controller / Gateway runs in;
	// the edge-allow policy permits ingress to the api-gateway from pods in this namespace
	// (matched by the kubernetes.io/metadata.name namespace label every namespace carries).
	// When empty it defaults to the well-known ingress-nginx namespace. Override it for a
	// different ingress controller (e.g. a Gateway API implementation's namespace). It is a
	// namespace NAME, never a credential or connection coordinate.
	// +optional
	IngressNamespace string `json:"ingressNamespace,omitempty"`
}

// CommonSpec holds configuration applied to EVERY platform component: shared image
// defaults, outbound proxy, log level, the trusted-CA bundle, the platform's public
// hostname, and the fleet-wide pod-template passthrough (init/sidecar containers,
// volumes, ports, envFrom). Each component's inline ComponentSpec overrides/augments
// what is set here. No secret VALUES live here — only references.
//
// It consolidates the shared image/proxy/logging/trustedCertificates settings plus a
// fleet-wide pod-template passthrough (initContainers / sidecarContainers /
// additionalVolumes / additionalVolumeMounts / additionalPorts / additionalEnv
// secrets+configMaps) that the per-component override surface (ComponentSpec) alone
// cannot express. The operator applies the passthrough fields to every component BEFORE
// that component's own ComponentSpec, so a component-specific override layers on top of
// the common one (the same global-then-component precedence as spec.additionalEnv).
//
// SCC SAFETY: InitContainers and Sidecars flow through the SAME fill-AND-force hardening
// (OpenShift restricted-v2) as every other container, so a common container cannot weaken
// pod security even if it sets Privileged:true / RunAsNonRoot:false / capabilities.Add.
//
// SECURITY: no secret VALUES are ever held here — AdditionalEnvFrom references whole
// Secrets/ConfigMaps by NAME only (projected via envFrom.secretRef / envFrom.configMapRef).
type CommonSpec struct {
	// Image is the shared image (registry/repository/pullPolicy/pullSecrets) for all
	// components; per-field defaults come from the selected version bundle when omitted,
	// and each component's image overrides per field.
	// +optional
	Image ImageSpec `json:"image,omitempty"`
	// HostName is the platform's canonical public FQDN. It is the single source of truth
	// for the external host: the edge uses it for its host/cert SAN when edge.host is unset,
	// and Keycloak (KC_HOSTNAME), the ilm OIDC client's redirect/web-origin/post-logout URIs,
	// and the in-pod OIDC registration script's browser-facing URLs all derive from it. The
	// gateway CORS origin also defaults to https://<hostName> when no explicit origins are
	// set. Set it even when you run your own ingress (edge.enabled=false) so the platform
	// still knows its public address. The resolver is PlatformHost: edge.host overrides this.
	//
	// NOTE: the fe-administrator runtime URLs are intentionally host-RELATIVE paths
	// (/api, /login, /logout) served from the same origin, so they do not embed hostName.
	//
	// It is constrained to the hostname charset (letters, digits, dot, underscore and hyphen,
	// up to 253 characters), optionally with a leading "*." wildcard label as Ingress rules and
	// Gateway API listeners accept — defence in depth for the browser-facing URLs the operator
	// derives from it, including the in-pod OIDC registration request.
	// +optional
	// +kubebuilder:validation:Pattern=`^(\*\.)?[a-zA-Z0-9._-]{1,253}$`
	HostName string `json:"hostName,omitempty"`
	// Proxy configures outbound HTTP(S) proxy support for all components.
	// +optional
	Proxy OutboundProxySpec `json:"proxy,omitempty"`
	// Logging configures the platform log level for all components.
	// +optional
	Logging LoggingSpec `json:"logging,omitempty"`
	// TrustedCertificates references the CA bundle (TRUSTED_CERTIFICATES) trusted by all
	// components. Reference only — the PEM is never inlined.
	// +optional
	TrustedCertificates TrustedCertificatesSpec `json:"trustedCertificates,omitempty"`

	// ── fleet-wide pod-template passthrough (unchanged from the former GlobalCustomization) ──

	// InitContainers are extra init containers appended to every component's pod, to run
	// before the component's main container (e.g. a custom-CA or Vault-init container).
	// They are SCC-hardened (forced restricted-v2 fields) like every other container. The
	// schema is preserved opaquely (x-kubernetes-preserve-unknown-fields) rather than
	// expanding the full corev1.Container OpenAPI inline, which would bloat the CRD past
	// the etcd object-size limit; the Go type stays []corev1.Container so it marshals
	// correctly and the kubelet validates it.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Sidecars are extra containers appended to every component's pod, to run alongside the
	// main container (e.g. a Vault Agent, an Istio proxy, or an OpenTelemetry collector). They
	// are SCC-hardened (forced restricted-v2 fields) like every other container. The schema is
	// preserved opaquely (x-kubernetes-preserve-unknown-fields) for the same CRD-size reason as
	// InitContainers; the Go type stays []corev1.Container.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Sidecars []corev1.Container `json:"sidecars,omitempty"`

	// Volumes are extra pod-level volumes added to every component, paired with VolumeMounts
	// for the main container. The schema is preserved opaquely (x-kubernetes-preserve-unknown-
	// fields) for the same CRD-size reason as InitContainers; the Go type stays []corev1.Volume.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts are extra mounts added to every component's main container (typically
	// pairing with Volumes above, or mounting a volume an injected sidecar shares).
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// AdditionalPorts are extra named container ports added to every component's main
	// container beyond its primary HTTP port.
	// +optional
	AdditionalPorts []corev1.ContainerPort `json:"additionalPorts,omitempty"`

	// AdditionalEnvFrom references whole Secrets/ConfigMaps (by NAME) whose every key is
	// projected as env on every component's main container via envFrom.secretRef /
	// envFrom.configMapRef. No secret values are inlined — references only.
	// +optional
	AdditionalEnvFrom *EnvFromSources `json:"additionalEnvFrom,omitempty"`

	// ── fleet-wide scheduling + pod metadata ──
	// Applied to EVERY (stateless) platform component as the base; each component's own
	// block overrides/augments these (see the precedence note on each field). Use them to
	// place or label the whole platform at once instead of repeating the block per component.
	// Managed infrastructure (CloudNativePG / RabbitMQ / Keycloak) is scheduled and annotated
	// via each managed block's own `overrides`, NOT these fields.

	// NodeSelector constrains every component's pods to nodes with matching labels. A
	// component's own nodeSelector is MERGED on top (the component's keys win on collision).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let every component's pods schedule onto tainted nodes. A component's own
	// tolerations are APPENDED — the union of common + component applies.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity sets pod affinity/anti-affinity + node affinity for every component. It is
	// REPLACE-not-merge: a component's own affinity wins wholesale, and an affinity set here
	// (or per component) overrides the high-availability profile's default anti-affinity. The
	// schema is preserved opaquely (x-kubernetes-preserve-unknown-fields) to keep the CRD
	// within the etcd object-size limit; the Go type stays corev1.Affinity so the kubelet
	// validates it.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// PodAnnotations are merged onto every component's pod template (e.g. fleet-wide Vault
	// Agent / Istio injection). A component's own podAnnotations win on key collision, and the
	// operator-managed annotations (e.g. the config checksum) always win.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are merged onto every component's pod template (e.g. fleet-wide cost/team
	// labels). A component's own podLabels win on key collision, and the operator-managed
	// selector labels always win (they are immutable selectors).
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`
}

// EnvFromSources lists whole Secrets and ConfigMaps to project as environment variables
// via envFrom (secretRef / configMapRef). It holds NAMES only — never secret values —
// and is used by the common customization passthrough (spec.common.additionalEnvFrom).
type EnvFromSources struct {
	// Secrets are Secret names projected via envFrom.secretRef (every key becomes an env var).
	// +optional
	Secrets []string `json:"secrets,omitempty"`

	// ConfigMaps are ConfigMap names projected via envFrom.configMapRef (every key becomes an
	// env var).
	// +optional
	ConfigMaps []string `json:"configMaps,omitempty"`
}

// PlatformStatus is the observed state.
type PlatformStatus struct {
	// Phase is a high-level summary of the platform state. The operator sets it to
	// "Progressing" (a required Deployment is not yet ready), "Running" (readiness
	// measured as met), or "Degraded" (a fatal error).
	// +kubebuilder:validation:Enum=Progressing;Running;Degraded
	// +optional
	Phase PlatformPhase `json:"phase,omitempty"`
	// ObservedGeneration is the most recent generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedVersion is the platform version bundle the operator resolved and
	// reconciled this Platform against — spec.version when set and known, otherwise the
	// operator's default version (not necessarily the newest one it carries). It lags
	// spec.version only while a reconcile is in flight; an unknown spec.version leaves it
	// at the last successfully-reconciled version and surfaces the error on the Degraded
	// condition.
	// +optional
	ObservedVersion string `json:"observedVersion,omitempty"`
	// Conditions represent the latest available observations of the platform's state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Upgrade is the in-flight messaging-migration state. It is present ONLY while a
	// migration is running — the engine sets it before the first side effect and clears
	// it once the migration completes — so its absence means no migration is in progress,
	// not merely that none has ever run.
	// +optional
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=plat
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.observedVersion`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Edge",type=string,JSONPath=`.status.conditions[?(@.type=="EdgeReady")].status`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Platform is the Schema for the platforms API.
//
// A Platform is a per-namespace SINGLETON: exactly one Platform is supported per
// namespace. The operator renders its children under clean, unscoped names (e.g.
// "core"), so a second Platform in the same namespace would collide. The controller
// enforces this with a runtime singleton guard — only the oldest Platform in a namespace
// reconciles; a second one is degraded immediately (AnotherPlatformExists). A validating
// admission webhook will replace that racy list-based guard with create-time enforcement
// in a later milestone. To run multiple platforms, use separate namespaces.
type Platform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PlatformSpec   `json:"spec,omitempty"`
	Status            PlatformStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformList contains a list of Platform.
type PlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Platform `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Platform{}, &PlatformList{})
}
