/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// managed_database.go renders the operator-provisioned PostgreSQL infrastructure for a
// Platform whose database.mode=managed: a CloudNativePG Cluster (and, when
// pgBouncer.managed, a Pooler), emitted as preset-GVK *unstructured.Unstructured.
//
// WHY UNSTRUCTURED (preset GVK): exactly the rationale used for the edge's cert-manager
// / Gateway API objects (see edge.go ResolveEdge / newEdgeUnstructured). Adding a typed
// CloudNativePG dependency would pin an older k8s.io graph than this project; an
// unstructured object with its apiVersion/kind preset apply-loops cleanly through SSA
// because Scheme.ObjectKinds returns the preset GVK even for an unregistered type.
//
// This is the FIRST managed-infrastructure component and the pattern-setter the managed
// RabbitMQ and Keycloak components reuse: the reusable seams it factors out are
//   - managedLabels:        the recommended-labels helper for any managed-infra object;
//   - newManagedUnstructured: the preset-GVK unstructured constructor (CRD-neutral);
//   - applyManagedOverrides: the RFC 7396 JSON-merge-patch helper with a protected-path
//     guard, for the per-component `overrides` escape hatch.
//
// CNPG SCHEMA: the CloudNativePG apiVersion/Kind and the field paths below are validated
// end-to-end against the CloudNativePG operator (v1.29.1) by the managed-database e2e:
// CNPG accepts the rendered Cluster, round-trips its spec fields, reconciles it to
// "Cluster in healthy state", and generates the <cluster>-app Secret + <cluster>-rw Service
// the readback consumes. The Pooler shape is not exercised end-to-end (the e2e does not set
// pgBouncer.managed); its field paths are taken from the CNPG v1.29 Pooler API. No credential
// or connection coordinate is ever placed in these objects beyond what CNPG itself requires.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonCloudNativePGNotInstalled is the DatabaseReady=False reason when the database is
// managed but the cluster does not serve the CloudNativePG CRDs. It is exported so the
// controller and its tests share one definition (mirrors ReasonCertManagerNotInstalled).
const ReasonCloudNativePGNotInstalled = "CloudNativePGNotInstalled"

// DatabaseDependencies returns the upstream-operator / CRD-bundle prerequisites a managed
// database needs, or nil for an external database (which needs none — the platform
// connects directly to the caller's coordinates). For a managed database it requires the
// CloudNativePG Cluster CRD, and additionally the Pooler CRD when pgBouncer.managed. The
// reconciler probes each via the capability detector and gates the DatabaseReady
// condition on their presence; the result mirrors exactly what ResolveManagedDatabase
// renders so the two never drift. The actionable message names the install remedy and the
// external escape hatch.
func DatabaseDependencies(p *otilmv1alpha1.Platform) []CRDDependency {
	if !DatabaseManaged(p) {
		return nil
	}
	msg := "database.mode=managed requires the CloudNativePG operator (postgresql.cnpg.io); " +
		"install CloudNativePG or set spec.database.mode=external to bring your own PostgreSQL"
	deps := []CRDDependency{
		{
			GroupKind: schema.GroupKind{Group: cnpgGroup, Kind: cnpgKindCluster},
			Versions:  []string{"v1"},
			Reason:    ReasonCloudNativePGNotInstalled,
			Message:   msg,
		},
	}
	if poolerManaged(p) {
		deps = append(deps, CRDDependency{
			GroupKind: schema.GroupKind{Group: cnpgGroup, Kind: cnpgKindPooler},
			Versions:  []string{"v1"},
			Reason:    ReasonCloudNativePGNotInstalled,
			Message:   msg,
		})
	}
	return deps
}

// CloudNativePG API coordinates and the operator-owned naming/contract constants. The
// objects are rendered as unstructured with this apiVersion preset (see the package
// doc); the names and the bootstrapped application database/owner are operator-owned so
// the readback (managedDBConnection) is a fixed contract independent of the CR.
const (
	// cnpgGroup / cnpgVersion / cnpgAPIVersion are the CloudNativePG API coordinates.
	// CNPG's GA API is postgresql.cnpg.io/v1 (Cluster and Pooler both live there).
	cnpgGroup      = "postgresql.cnpg.io"
	cnpgVersion    = "v1"
	cnpgAPIVersion = cnpgGroup + "/" + cnpgVersion
	// cnpgKindCluster / cnpgKindPooler are the CloudNativePG Kinds the operator renders.
	cnpgKindCluster = "Cluster"
	cnpgKindPooler  = "Pooler"

	// managedDBRole is the component label the managed database objects carry.
	managedDBRole = "database"
	// managedPoolerRole is the component label the managed pooler objects carry.
	managedPoolerRole = "database-pooler"

	// managedDBAppName is the bootstrapped application database name AND the owning role
	// CNPG creates in the managed cluster. The readback assumes both, so it is
	// operator-owned (a protected override path) — the platform's JDBC URL and the
	// auth connection string both target this database.
	managedDBAppName = "ilm"

	// cnpgRWServiceSuffix / cnpgAppSecretSuffix are CloudNativePG's generated-resource
	// naming conventions relative to the Cluster name <cluster>:
	//   <cluster>-rw   the read-write Service (primary) — the platform's DB host;
	//   <cluster>-app  the generated application Secret (basic-auth username/password).
	// CNPG creates both for the rendered Cluster (ServiceReadWriteSuffix="-rw",
	// ApplicationUserSecretSuffix="-app").
	cnpgRWServiceSuffix = "-rw"
	cnpgAppSecretSuffix = "-app"

	// managedDBPort is the PostgreSQL port the managed cluster (and its pooler) serve on.
	managedDBPort int32 = 5432

	// cnpgPoolerTypeRW routes the pooler at the cluster's read-write (primary) endpoint;
	// cnpgPoolerModeTransaction is the PgBouncer pool mode the platform uses (short,
	// stateless connections). These are the CNPG Pooler API field values (type rw|r|ro;
	// poolMode session|transaction).
	cnpgPoolerTypeRW          = "rw"
	cnpgPoolerModeTransaction = "transaction"

	// cnpgPoolerMaxPreparedStatements / cnpgPoolerIgnoreStartupParameters / cnpgPoolerServerResetQuery /
	// cnpgPoolerServerResetQueryAlways are the DEFAULT pgbouncer parameters a JDBC/.NET client fleet
	// needs to work through TRANSACTION-mode pooling — mirroring the Helm chart's pg-bouncer defaults.
	// Without max_prepared_statements the PostgreSQL JDBC driver's server-side prepared statements break
	// across pooled transactions (PgBouncer 1.21+ shares up to this many per server connection); without
	// ignore_startup_parameters the driver's extra_float_digits startup parameter is rejected by PgBouncer.
	//
	// server_reset_query_always=1 is REQUIRED for the platform's schema-multitenant database (core / auth /
	// keycloak / scheduler all share one database+user, isolated only by PostgreSQL SCHEMA, each selected
	// at runtime via a session-level `SET search_path`). PgBouncer's server_reset_query (DISCARD ALL) is by
	// default RUN ONLY IN SESSION MODE (server_reset_query_always=0); in transaction mode it is skipped, so a
	// backend keeps the search_path left by the previous client and LEAKS it to the next service that borrows
	// that backend. Core's unqualified migration DDL then resolves against another service's schema —
	// tables scatter across schemas and Flyway fails with "relation … does not exist / already exists",
	// crash-looping the platform on a managed deploy. Forcing DISCARD ALL after EVERY transaction
	// (server_reset_query_always=1) resets each backend to a clean state before reuse, so search_path never
	// leaks across services and every component's DDL lands in its own schema (validated end-to-end: a
	// managed everything-deploy migrates 134/134 first-try, 0 restarts, nothing in `public`). search_path
	// itself cannot be PRESERVED across pooled backends via track_extra_parameters on PostgreSQL ≤17 (it is
	// not a GUC_REPORT parameter, so PgBouncer never sees its value) — resetting between clients is the
	// portable fix.
	//
	// cnpgPoolerDefaultPoolSize / cnpgPoolerMaxClientConn mirror the Helm chart's pg-bouncer values
	// (100 / 1000) — a roomier server pool and a high client-connection ceiling so the full fleet
	// (core + auth + keycloak + scheduler + connectors) does not queue on the pooler. NOTE: a managed
	// CloudNativePG cluster defaults to max_connections=100, so default_pool_size=100 leaves no headroom
	// if the pool ever fills; raise spec.database.managed.overrides postgresql max_connections (or lower
	// default_pool_size via pgBouncer.parameters) for connection-heavy deployments. The caller can
	// override any of these via pgBouncer.parameters (caller wins).
	cnpgPoolerMaxPreparedStatements   = "10"
	cnpgPoolerIgnoreStartupParameters = "extra_float_digits"
	cnpgPoolerServerResetQuery        = "DISCARD ALL"
	cnpgPoolerServerResetQueryAlways  = "1"
	cnpgPoolerDefaultPoolSize         = "100"
	cnpgPoolerMaxClientConn           = "1000"
)

// ManagedDatabaseName returns the stable name of the CloudNativePG Cluster the operator
// renders for a Platform: "<platform>-db". It is operator-owned (a protected override
// path) so the generated <cluster>-rw Service and <cluster>-app Secret names — which the
// readback resolves — are a fixed function of the Platform name.
func ManagedDatabaseName(p *otilmv1alpha1.Platform) string {
	return p.Name + "-db"
}

// ManagedPoolerName returns the stable name of the CloudNativePG Pooler the operator
// renders when pgBouncer.managed is set: "<platform>-db-pooler".
func ManagedPoolerName(p *otilmv1alpha1.Platform) string {
	return ManagedDatabaseName(p) + "-pooler"
}

// ManagedDatabaseClusterGVK returns the preset GroupVersionKind of the CloudNativePG
// Cluster the operator renders. The reconciler uses it to GET the Cluster (unstructured)
// when probing readiness and to set up the Watch.
func ManagedDatabaseClusterGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgKindCluster}
}

// DatabaseManaged reports whether the Platform's database is operator-provisioned
// (mode=managed with the managed block present). It is the single predicate the builder,
// the readback, the gating, and the deletion path share so they never drift.
func DatabaseManaged(p *otilmv1alpha1.Platform) bool {
	return p.Spec.Database.Mode == databaseModeManaged && p.Spec.Database.Managed != nil
}

// poolerManaged reports whether the operator provisions a PgBouncer pooler in front of a
// managed database. It is DEFAULT-ON for a managed database: the ILM fleet (core + auth
// + keycloak + scheduler) opens enough direct connection pools to exhaust CloudNativePG's
// default max_connections=100 — auth then 500s and Core crash-loops on its boot-time
// resource sync — so the operator fronts the database with a CloudNativePG Pooler (PgBouncer,
// transaction mode) UNLESS the caller explicitly opts out with pgBouncer.managed=false. A nil
// pgBouncer block keeps the pooler on. Mirrors the Helm chart, which ships PgBouncer for the
// same reason. Only meaningful for a managed database (an external database brings its own
// pooling); when customizing the pgBouncer block, keep managed=true to retain the pooler.
func poolerManaged(p *otilmv1alpha1.Platform) bool {
	if !DatabaseManaged(p) {
		return false
	}
	pb := p.Spec.Database.PgBouncer
	return pb == nil || pb.Managed
}

// databaseModeManaged / databaseModeExternal are the spec.database.mode literals.
const (
	databaseModeManaged  = "managed"
	databaseModeExternal = "external"
)

// ResolveManagedDatabase returns the CloudNativePG objects the operator provisions for a
// managed database, or nil when the database is external (or managed but mis-specified).
// It renders a Cluster and, when pgBouncer.managed, a Pooler — both as preset-GVK
// unstructured objects with NO owner reference (the reconciler decides ownership per the
// deletion-safety contract: managed CRs carry no controller ownerRef and are
// prune-excluded, so a transient de-render never deletes the database).
//
// SECURITY: no credential or connection coordinate is placed in these objects beyond
// what CNPG requires to provision the cluster (instances/version/storage/resources and
// the bootstrapped app database/owner name). The credentials are CNPG-generated and read
// back by reference (never inlined).
func ResolveManagedDatabase(p *otilmv1alpha1.Platform) []client.Object {
	if !DatabaseManaged(p) {
		return nil
	}
	objs := []client.Object{buildCNPGCluster(p)}
	if poolerManaged(p) {
		objs = append(objs, buildCNPGPooler(p))
	}
	return objs
}

// buildCNPGCluster renders the CloudNativePG Cluster for the managed database. The spec
// is built from the typed surface (instances/version/storage/resources + the bootstrap
// app database/owner) and then the caller's Overrides JSON-merge patch is applied onto
// it, with the operator-owned paths protected. A protected-path or malformed override
// surfaces as an error embedded in the returned object's annotation so the reconciler
// can degrade with a clear message (ResolveManagedDatabase callers route through the
// reconciler's apply, which reports the error — see managedDatabaseError).
func buildCNPGCluster(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	m := p.Spec.Database.Managed

	storage := map[string]interface{}{
		"size": m.Storage.Size, // spec.storage.size (a Quantity string)
	}
	if m.Storage.StorageClass != nil && *m.Storage.StorageClass != "" {
		storage["storageClass"] = *m.Storage.StorageClass // spec.storage.storageClass
	}

	spec := map[string]interface{}{
		"instances": int64(managedInstances(m)),
		"storage":   storage,
		// bootstrap.initdb seeds the application database and its owning role. These two
		// values are the readback contract (managedDBConnection.Name == managedDBAppName,
		// and the generated <cluster>-app Secret authenticates this owner) and are
		// therefore operator-owned + protected from Overrides. CNPG provisions the "ilm"
		// database/owner the readback consumes.
		"bootstrap": map[string]interface{}{
			"initdb": cnpgInitDB(p),
		},
	}

	// PostgreSQL version → CNPG image. CNPG selects the engine version via spec.imageName
	// (a fully-qualified image ref). The operator composes the community image
	// "ghcr.io/cloudnative-pg/postgresql:<major>" from the requested major version; when
	// the version is empty CNPG applies its own default image (imageName omitted).
	// NOTE(cnpg): the BARE major tag (":16") is a rolling tag CNPG's postgres-containers
	// publishes but documents as DEPRECATED (legacy Debian bullseye "system" images, to be
	// removed at bullseye EOL). It works against current registries; for long-term pinning
	// the override escape hatch (or a future minor/OS-qualified tag, e.g.
	// "16-standard-bookworm") is preferred. CNPG also supports an imageCatalog reference.
	if m.Version != "" {
		spec["imageName"] = cnpgImageForVersion(m.Version)
	}

	if m.Resources != nil {
		// CNPG passes spec.resources straight to the instance pods' container resources
		// (a core/v1 ResourceRequirements).
		if res, err := toUnstructured(m.Resources); err == nil {
			spec["resources"] = res
		}
	}

	// DEFERRED: m.Backup (schedule/retention) is accepted on the CR but NOT yet rendered
	// onto the Cluster spec. CNPG backups need object-store coordinates (referenced by
	// Secret, never inlined) and a ScheduledBackup object; that deep wiring + restore is a
	// later milestone. The field is shipped now so the CR surface is forward-compatible.
	// When implemented, render spec.backup.{barmanObjectStore,retentionPolicy} here.

	u := newManagedUnstructured(p, cnpgAPIVersion, cnpgKindCluster, ManagedDatabaseName(p), managedDBRole, spec)

	// Apply the caller's Overrides JSON-merge patch (RFC 7396) onto the rendered spec,
	// rejecting the operator-owned protected paths. On error, stamp the object so the
	// reconciler degrades with an actionable, leak-free message rather than applying a
	// half-merged spec.
	if err := applyManagedOverrides(u, m.Overrides); err != nil {
		setManagedDatabaseError(u, err)
	}
	return u
}

// cnpgInitDB renders the CNPG spec.bootstrap.initdb block: the operator-owned application
// database + owner (the readback contract), plus — when a MANAGED Keycloak shares this managed
// database — a postInitApplicationSQL statement that pre-creates Keycloak's dedicated schema.
//
// WHY the schema must be pre-created: a managed Keycloak shares this PostgreSQL under a
// dedicated schema (spec.db.schema=keycloak → KC_DB_SCHEMA), but Keycloak/Liquibase does NOT
// create the schema — it expects it to pre-exist and otherwise crash-loops with
// 'schema "keycloak" does not exist'. CNPG runs postInitApplicationSQL as a superuser in the
// application database right after bootstrap, so "CREATE SCHEMA IF NOT EXISTS keycloak
// AUTHORIZATION <owner>" creates the schema owned by the app owner Keycloak authenticates as.
// IF NOT EXISTS keeps it idempotent/safe. This only applies to the MANAGED-DB + MANAGED-Keycloak
// combination (an external DB is the operator's responsibility to prepare; an external Keycloak
// uses its own DB). Without the pre-created schema the managed Keycloak crash-loops on the
// missing schema during its Liquibase migration.
func cnpgInitDB(p *otilmv1alpha1.Platform) map[string]interface{} {
	initdb := map[string]interface{}{
		"database": managedDBAppName,
		"owner":    managedDBAppName,
	}
	if KeycloakManaged(p) {
		initdb["postInitApplicationSQL"] = []interface{}{
			"CREATE SCHEMA IF NOT EXISTS " + keycloakDBSchema + " AUTHORIZATION " + managedDBAppName,
		}
	}
	return initdb
}

// managedInstances returns the configured instance count, defaulting to 1 (the CRD also
// defaults this, but the builder is a pure function callable without apiserver defaulting
// in unit tests).
func managedInstances(m *otilmv1alpha1.ManagedDatabaseSpec) int32 {
	if m.Instances < 1 {
		return 1
	}
	return m.Instances
}

// cnpgImageForVersion composes the CloudNativePG community PostgreSQL image for a major
// version: ghcr.io/cloudnative-pg/postgresql:<major>. See the bare-major-tag deprecation
// NOTE(cnpg) at the imageName call site; deployments that pin a minor/OS-qualified tag or
// a private mirror use the override escape hatch.
func cnpgImageForVersion(version string) string {
	return "ghcr.io/cloudnative-pg/postgresql:" + version
}

// buildCNPGPooler renders the CloudNativePG Pooler that fronts the managed cluster with
// PgBouncer whenever poolerManaged (default-on for a managed database). It targets the
// cluster's read-write endpoint in transaction pool mode; extra PgBouncer parameters from the
// CR are passed through (sorted for deterministic output). A nil pgBouncer block is the
// default-on case and renders with defaults (1 instance, no extra parameters). No credentials
// are placed here.
//
// The Pooler field paths below (spec.cluster.name, spec.instances, spec.type,
// spec.pgbouncer.poolMode, spec.pgbouncer.parameters) are the CNPG v1.29 Pooler API.
func buildCNPGPooler(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	pb := p.Spec.Database.PgBouncer // may be nil (default-on) — all reads below are nil-guarded

	// pgbouncer.parameters: start from the DEFAULTS a JDBC/.NET client fleet needs to work
	// through transaction-mode pooling (max_prepared_statements, ignore_startup_parameters, and —
	// critically for the schema-multitenant database — server_reset_query + server_reset_query_always
	// so DISCARD ALL actually runs between transactions and search_path never leaks across services;
	// see the consts), then overlay the caller's pgBouncer.parameters (caller wins). CNPG passes the
	// merged map straight to the generated pgbouncer.ini. Without these defaults the platform's
	// JDBC/Npgsql clients hit prepared-statement / startup-parameter errors AND cross-service
	// search_path leakage that scatters Core's migration DDL across schemas (crash-looping the deploy).
	params := map[string]interface{}{
		"max_prepared_statements":   cnpgPoolerMaxPreparedStatements,
		"ignore_startup_parameters": cnpgPoolerIgnoreStartupParameters,
		"server_reset_query":        cnpgPoolerServerResetQuery,
		"server_reset_query_always": cnpgPoolerServerResetQueryAlways,
		"default_pool_size":         cnpgPoolerDefaultPoolSize,
		"max_client_conn":           cnpgPoolerMaxClientConn,
	}
	if pb != nil {
		for _, k := range sortedKeys(pb.Parameters) {
			params[k] = pb.Parameters[k]
		}
	}
	pgbouncer := map[string]interface{}{
		"poolMode":   cnpgPoolerModeTransaction,
		"parameters": params,
	}

	spec := map[string]interface{}{
		// spec.cluster.name references the managed Cluster by name (same namespace).
		"cluster":   map[string]interface{}{"name": ManagedDatabaseName(p)},
		"instances": int64(poolerInstances(pb)),
		"type":      cnpgPoolerTypeRW,
		"pgbouncer": pgbouncer,
	}
	return newManagedUnstructured(p, cnpgAPIVersion, cnpgKindPooler, ManagedPoolerName(p), managedPoolerRole, spec)
}

// poolerInstances returns the configured pooler instance count, defaulting to 1.
func poolerInstances(pb *otilmv1alpha1.PgBouncerSpec) int32 {
	if pb == nil || pb.Instances < 1 {
		return 1
	}
	return pb.Instances
}

// sortedKeys returns the keys of a string map in sorted order, for deterministic
// rendering of map-valued fields (so the rendered object is stable across reconciles).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// managedLabels returns the standard recommended labels for a managed-infrastructure
// object with the given component role. It mirrors edgeLabels (same five recommended
// keys) and is the shared helper the managed RabbitMQ/Keycloak components reuse.
func managedLabels(p *otilmv1alpha1.Platform, role string) map[string]string {
	return map[string]string{
		common.NameLabel:      role,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: role,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
}

// newManagedUnstructured returns a preset-GVK unstructured managed-infra object with
// apiVersion/kind/name/namespace/labels and the given spec populated. It is the
// CRD-neutral constructor behind the CNPG objects (and the managed RabbitMQ/Keycloak
// objects later): the controller's SSA apply loop resolves the preset GVK via
// Scheme.ObjectKinds even though the type is not registered in the scheme. It mirrors the
// edge's newEdgeUnstructured exactly, generalized to any managed-infra apiVersion.
func newManagedUnstructured(p *otilmv1alpha1.Platform, apiVersion, kind, name, role string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	u.SetNamespace(p.Namespace)
	u.SetLabels(managedLabels(p, role))
	_ = unstructured.SetNestedMap(u.Object, spec, "spec")
	return u
}

// managedOverrideProtectedPaths are the operator-owned paths a caller's Overrides
// JSON-merge patch may NOT touch. metadata.name/namespace/ownerReferences keep the
// object identity + ownership the operator controls; bootstrap.initdb.database/owner keep
// the readback contract (the app database/owner the platform connects to). A patch
// touching any of these is rejected with a clear error.
var managedOverrideProtectedPaths = [][]string{
	{"metadata", "name"},
	{"metadata", "namespace"},
	{"metadata", "ownerReferences"},
	{"spec", "bootstrap", "initdb", "database"},
	{"spec", "bootstrap", "initdb", "owner"},
}

// applyManagedOverrides applies a caller's RFC 7396 JSON-merge patch onto the rendered
// CloudNativePG object, rejecting any patch that addresses a protected (operator-owned)
// path. It delegates to the mode-neutral applyOverridesWithProtectedPaths (shared with the
// managed-messaging builder); only the protected-path set and the field-name prefix in the
// error message differ.
func applyManagedOverrides(u *unstructured.Unstructured, overrides *runtime.RawExtension) error {
	return applyOverridesWithProtectedPaths(u, overrides, managedOverrideProtectedPaths, "database.managed.overrides")
}

// applyOverridesWithProtectedPaths applies a caller's RFC 7396 JSON-merge patch onto the
// rendered object, rejecting any patch that addresses one of the given protected (operator-
// owned) paths. A nil/empty patch is a no-op. The merge is performed on the WHOLE object
// (so a caller can patch spec.* freely) while the protected-path guard runs first against
// the patch's own shape, so an attempt to override a protected field fails fast with a
// message naming the path — the readback/topology contract cannot be silently broken.
// fieldName is the CR field path quoted in error messages (e.g. "database.managed.overrides").
//
// It is the mode-neutral core both managed-infra builders (database, messaging) reuse for
// their `overrides` escape hatch — exactly the same way they share newManagedUnstructured.
func applyOverridesWithProtectedPaths(u *unstructured.Unstructured, overrides *runtime.RawExtension, protectedPaths [][]string, fieldName string) error {
	if overrides == nil || len(overrides.Raw) == 0 {
		return nil
	}
	var patch map[string]interface{}
	if err := json.Unmarshal(overrides.Raw, &patch); err != nil {
		return fmt.Errorf("%s is not a valid JSON object: %w", fieldName, err)
	}
	// Reject protected paths present in the patch BEFORE merging, so the rendered object
	// never reflects a forbidden change.
	for _, path := range protectedPaths {
		if patchHasPath(patch, path) {
			return fmt.Errorf("%s may not override the operator-owned field %q", fieldName, joinPath(path))
		}
	}
	u.Object = mergePatchMap(u.Object, patch)
	return nil
}

// errFromString returns an error carrying exactly the given message (no wrapping), used to
// re-materialize a render-time error stashed in an object annotation. It avoids fmt's
// %-verb interpretation of the stored message.
func errFromString(msg string) error { return errors.New(msg) }

// patchHasPath reports whether the JSON-merge patch addresses the given object path
// (i.e. the path is present as nested keys in the patch, regardless of value — even a
// null, which in RFC 7396 means "delete"). It only descends through map nodes; a
// non-map at an intermediate path means the patch does not address the deeper key.
func patchHasPath(patch map[string]interface{}, path []string) bool {
	cur := patch
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// mergePatchMap applies an RFC 7396 JSON-merge patch (patch) onto a target map and
// returns the result. Per RFC 7396: a null value in the patch deletes the target key; a
// map value recurses (merging into the target's map, or replacing a non-map); any other
// value replaces the target value. The target is not mutated (a shallow-then-deep copy is
// produced) so the helper is safe on the rendered object's map.
func mergePatchMap(target, patch map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(target))
	for k, v := range target {
		out[k] = v
	}
	for k, pv := range patch {
		if pv == nil {
			delete(out, k)
			continue
		}
		pm, pIsMap := pv.(map[string]interface{})
		if !pIsMap {
			out[k] = pv
			continue
		}
		if tm, tIsMap := out[k].(map[string]interface{}); tIsMap {
			out[k] = mergePatchMap(tm, pm)
		} else {
			out[k] = mergePatchMap(map[string]interface{}{}, pm)
		}
	}
	return out
}

// joinPath renders a dotted path for an error message (e.g. "spec.bootstrap.initdb.owner").
func joinPath(path []string) string {
	s := ""
	for i, p := range path {
		if i > 0 {
			s += "."
		}
		s += p
	}
	return s
}

// managedDatabaseErrorAnnotation is the annotation key the builder uses to carry a
// render-time error (e.g. a rejected override) out of the pure builder to the reconciler,
// which surfaces it as a Degraded condition. It is operator-internal and stripped before
// apply (the reconciler reads it, then deletes it). SECURITY: the error text names only a
// field path / parse failure — never a secret or coordinate.
const managedDatabaseErrorAnnotation = "otilm.com/managed-database-error"

// setManagedDatabaseError stamps a render-time error onto the object via an annotation so
// the reconciler can detect-and-degrade rather than applying an invalid object.
func setManagedDatabaseError(u *unstructured.Unstructured, err error) {
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[managedDatabaseErrorAnnotation] = err.Error()
	u.SetAnnotations(ann)
}

// ManagedDatabaseRenderError returns the render-time error carried by a managed-database
// object (a rejected override or malformed patch), or nil when the object rendered
// cleanly. The reconciler calls this on each ResolveManagedDatabase object before apply.
func ManagedDatabaseRenderError(obj client.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	if msg := u.GetAnnotations()[managedDatabaseErrorAnnotation]; msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// toUnstructured marshals a typed value (e.g. corev1.ResourceRequirements) to its
// generic map representation for embedding in an unstructured spec.
func toUnstructured(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
