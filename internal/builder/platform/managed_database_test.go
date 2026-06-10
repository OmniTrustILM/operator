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
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managedDBPlatform returns a Platform with a managed database (instances/version/storage)
// and any extra mutation applied, in a namespace so the rendered objects carry it.
func managedDBPlatform(mutate func(*otilmv1alpha1.Platform)) *otilmv1alpha1.Platform {
	scClass := "fast-ssd"
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "managed",
				Managed: &otilmv1alpha1.ManagedDatabaseSpec{
					Instances: 3,
					Version:   "16",
					Storage:   otilmv1alpha1.StorageSpec{Size: "100Gi", StorageClass: &scClass},
				},
			},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// findManagedObj returns the first rendered object of the given kind, or nil (a
// nil-tolerant variant of edge_test.go's findUnstructured, for absence assertions).
func findManagedObj(objs []client.Object, kind string) *unstructured.Unstructured {
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if ok && u.GetKind() == kind {
			return u
		}
	}
	return nil
}

func TestResolveManagedDatabaseExternalRendersNothing(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "db-secret"},
			},
		},
	}
	assert.Nil(t, ResolveManagedDatabase(p), "external mode renders no CloudNativePG objects")
	assert.False(t, DatabaseManaged(p))
}

// TestResolveManagedDatabaseDefaultsToPooler proves the pooler is DEFAULT-ON: a managed database
// with no pgBouncer block renders the Cluster AND a CloudNativePG Pooler (chart parity — the
// fleet would otherwise exhaust Postgres's default max_connections).
func TestResolveManagedDatabaseDefaultsToPooler(t *testing.T) {
	objs := ResolveManagedDatabase(managedDBPlatform(nil))
	require.Len(t, objs, 2, "a managed database defaults to rendering the Cluster + the PgBouncer Pooler")
	require.NotNil(t, findManagedObj(objs, cnpgKindPooler), "the default-on CloudNativePG Pooler must be rendered")
}

// TestResolveManagedDatabasePoolerParameters locks the pgbouncer parameters a JDBC/.NET client
// fleet needs through TRANSACTION-mode pooling (chart parity): max_prepared_statements,
// ignore_startup_parameters, and — critically for the schema-multitenant database —
// server_reset_query=DISCARD ALL with server_reset_query_always=1 are set by default, and a caller
// override merges in + wins. Without server_reset_query_always the pooler skips DISCARD ALL in
// transaction mode, so a backend keeps the previous client's `SET search_path` and leaks it to the
// next service; Core's unqualified migration DDL then scatters across schemas and Flyway crash-loops.
func TestResolveManagedDatabasePoolerParameters(t *testing.T) {
	pooler := findManagedObj(ResolveManagedDatabase(managedDBPlatform(nil)), cnpgKindPooler)
	require.NotNil(t, pooler)
	mode, _, _ := unstructured.NestedString(pooler.Object, "spec", "pgbouncer", "poolMode")
	assert.Equal(t, "transaction", mode)
	params, found, err := unstructured.NestedStringMap(pooler.Object, "spec", "pgbouncer", "parameters")
	require.NoError(t, err)
	require.True(t, found, "the managed pooler must set default pgbouncer parameters")
	assert.Equal(t, "10", params["max_prepared_statements"])
	assert.Equal(t, "extra_float_digits", params["ignore_startup_parameters"])
	assert.Equal(t, "DISCARD ALL", params["server_reset_query"])
	assert.Equal(t, "1", params["server_reset_query_always"],
		"DISCARD ALL must run between transactions so search_path never leaks across schema-tenant services")
	assert.Equal(t, "100", params["default_pool_size"], "roomier server pool (chart parity)")
	assert.Equal(t, "1000", params["max_client_conn"], "high client-connection ceiling (chart parity)")

	// A caller override wins and merges with the defaults.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{
			Managed:    true,
			Parameters: map[string]string{"max_prepared_statements": "50", "max_client_conn": "2000"},
		}
	})
	pooler = findManagedObj(ResolveManagedDatabase(p), cnpgKindPooler)
	require.NotNil(t, pooler)
	params, _, _ = unstructured.NestedStringMap(pooler.Object, "spec", "pgbouncer", "parameters")
	assert.Equal(t, "50", params["max_prepared_statements"], "caller override wins")
	assert.Equal(t, "2000", params["max_client_conn"], "caller override wins over the default")
	assert.Equal(t, "extra_float_digits", params["ignore_startup_parameters"], testDefaultKeptMsg)
	assert.Equal(t, "1", params["server_reset_query_always"], testDefaultKeptMsg)
	assert.Equal(t, "100", params["default_pool_size"], testDefaultKeptMsg)
}

func TestResolveManagedDatabaseRendersCluster(t *testing.T) {
	// Opt OUT of the default-on pooler so this renders exactly the Cluster.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	})
	objs := ResolveManagedDatabase(p)
	require.Len(t, objs, 1, "a managed database with the pooler opted out renders exactly the Cluster")

	cluster := findManagedObj(objs, cnpgKindCluster)
	require.NotNil(t, cluster, "the CloudNativePG Cluster must be rendered")

	// GVK + identity.
	assert.Equal(t, cnpgAPIVersion, cluster.GetAPIVersion())
	assert.Equal(t, cnpgKindCluster, cluster.GetKind())
	assert.Equal(t, testILMDB, cluster.GetName(), "Cluster name is <platform>-db")
	assert.Equal(t, "ns", cluster.GetNamespace())

	// Recommended labels (managed-by/instance drive the watch + prune-exclusion).
	labels := cluster.GetLabels()
	assert.Equal(t, common.ManagedByValue, labels[common.ManagedByLabel])
	assert.Equal(t, "ilm", labels[common.InstanceLabel])
	assert.Equal(t, managedDBRole, labels[common.ComponentLabel])

	// instances.
	instances, found, err := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(3), instances)

	// storage.{size,storageClass}.
	size, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "size")
	assert.Equal(t, "100Gi", size)
	sc, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	assert.Equal(t, "fast-ssd", sc)

	// version → imageName.
	image, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	assert.Contains(t, image, ":16", "the major version selects the CNPG image tag")

	// bootstrap.initdb.{database,owner} == the app DB the readback assumes.
	db, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "database")
	owner, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "owner")
	assert.Equal(t, "ilm", db)
	assert.Equal(t, "ilm", owner)

	// With no MANAGED Keycloak sharing this DB, there is no schema to pre-create: the
	// postInitApplicationSQL hook is absent (a plain app-DB bootstrap, behaviour unchanged).
	_, hasSQL, _ := unstructured.NestedSlice(cluster.Object, "spec", "bootstrap", "initdb", "postInitApplicationSQL")
	assert.False(t, hasSQL, "without a managed Keycloak there should be no postInitApplicationSQL")
}

// TestResolveManagedDatabasePreCreatesKeycloakSchema asserts the managed-DB + managed-Keycloak
// combination renders a postInitApplicationSQL that pre-creates Keycloak's dedicated schema in
// the shared database. Keycloak/Liquibase does NOT create the schema and crash-loops on a
// missing one (observed with Keycloak 26.4.0), so CNPG must create it at bootstrap.
func TestResolveManagedDatabasePreCreatesKeycloakSchema(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		// Add a managed Keycloak sharing this managed database.
		p.Spec.Keycloak = &otilmv1alpha1.KeycloakSpec{
			Mode:    "managed",
			Managed: &otilmv1alpha1.ManagedKeycloakSpec{Instances: 1, Version: "26.4.0"},
		}
	})
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)

	sql, found, err := unstructured.NestedStringSlice(cluster.Object, "spec", "bootstrap", "initdb", "postInitApplicationSQL")
	require.NoError(t, err)
	require.True(t, found, "a managed Keycloak sharing the managed DB must add postInitApplicationSQL")
	require.Len(t, sql, 1)
	// The SQL pre-creates Keycloak's schema (idempotent) owned by the app owner Keycloak
	// authenticates as, so Keycloak's migration into spec.db.schema=keycloak succeeds.
	assert.Equal(t, "CREATE SCHEMA IF NOT EXISTS keycloak AUTHORIZATION ilm", sql[0])
}

func TestResolveManagedDatabaseResources(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.Managed.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
		}
	})
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)

	reqCPU, _, _ := unstructured.NestedString(cluster.Object, "spec", "resources", "requests", "cpu")
	assert.Equal(t, "2", reqCPU)
	limMem, _, _ := unstructured.NestedString(cluster.Object, "spec", "resources", "limits", "memory")
	assert.Equal(t, "8Gi", limMem)
}

func TestResolveManagedDatabaseNoVersionOmitsImage(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Database.Managed.Version = "" })
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	_, found, _ := unstructured.NestedString(cluster.Object, "spec", "imageName")
	assert.False(t, found, "with no version the CNPG default image is used (imageName omitted)")
}

func TestResolveManagedDatabaseWithManagedPooler(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{
			Managed: true, Instances: 2,
			Parameters: map[string]string{"max_client_conn": "200"},
		}
	})
	objs := ResolveManagedDatabase(p)
	require.Len(t, objs, 2, "a managed database with a managed pooler renders the Cluster + Pooler")

	pooler := findManagedObj(objs, cnpgKindPooler)
	require.NotNil(t, pooler, "the CloudNativePG Pooler must be rendered")
	assert.Equal(t, "ilm-db-pooler", pooler.GetName())

	clusterName, _, _ := unstructured.NestedString(pooler.Object, "spec", "cluster", "name")
	assert.Equal(t, testILMDB, clusterName, "the Pooler references the managed Cluster by name")

	instances, _, _ := unstructured.NestedInt64(pooler.Object, "spec", "instances")
	assert.Equal(t, int64(2), instances)

	typ, _, _ := unstructured.NestedString(pooler.Object, "spec", "type")
	assert.Equal(t, "rw", typ)
	mode, _, _ := unstructured.NestedString(pooler.Object, "spec", "pgbouncer", "poolMode")
	assert.Equal(t, "transaction", mode)
	param, _, _ := unstructured.NestedString(pooler.Object, "spec", "pgbouncer", "parameters", "max_client_conn")
	assert.Equal(t, "200", param)
}

func TestResolveManagedDatabasePgBouncerNotManagedNoPooler(t *testing.T) {
	// pgBouncer present but Managed=false → no Pooler object (explicit opt-out).
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	})
	objs := ResolveManagedDatabase(p)
	assert.Len(t, objs, 1, "pgBouncer.managed=false renders no Pooler")
	assert.Nil(t, findManagedObj(objs, cnpgKindPooler))
}

func TestResolveManagedDatabaseInstancesDefault(t *testing.T) {
	// Instances unset (0 in a raw struct) defaults to 1 in the pure builder.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Database.Managed.Instances = 0 })
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	assert.Equal(t, int64(1), instances)
}

// --- Overrides (RFC 7396 JSON-merge patch) -----------------------------------

func rawExt(t *testing.T, v map[string]interface{}) *runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return &runtime.RawExtension{Raw: b}
}

func TestManagedDatabaseOverridesMergeApplies(t *testing.T) {
	// An override that adds an unprotected field and tweaks a nested one merges in.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"primaryUpdateStrategy": "unsupervised",
				"postgresql": map[string]interface{}{
					"parameters": map[string]interface{}{"max_connections": "200"},
				},
			},
		})
	})
	objs := ResolveManagedDatabase(p)
	cluster := findManagedObj(objs, cnpgKindCluster)
	require.NotNil(t, cluster)
	require.NoError(t, ManagedDatabaseRenderError(cluster), "a non-protected override must apply cleanly")

	strategy, _, _ := unstructured.NestedString(cluster.Object, "spec", "primaryUpdateStrategy")
	assert.Equal(t, "unsupervised", strategy)
	maxConn, _, _ := unstructured.NestedString(cluster.Object, "spec", "postgresql", "parameters", "max_connections")
	assert.Equal(t, "200", maxConn)

	// The operator-rendered fields survive the merge (instances untouched).
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	assert.Equal(t, int64(3), instances)
}

func TestManagedDatabaseOverridesRejectProtectedInitdb(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.Managed.Overrides = rawExt(t, map[string]interface{}{
			"spec": map[string]interface{}{
				"bootstrap": map[string]interface{}{
					"initdb": map[string]interface{}{"database": "evil"},
				},
			},
		})
	})
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	err := ManagedDatabaseRenderError(cluster)
	require.Error(t, err, "overriding the bootstrapped database (a readback-contract field) must be rejected")
	assert.Contains(t, err.Error(), "spec.bootstrap.initdb.database")

	// The protected value must be UNCHANGED despite the rejected patch.
	db, _, _ := unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "database")
	assert.Equal(t, "ilm", db, "the rejected override must not mutate the protected field")
}

func TestManagedDatabaseOverridesRejectProtectedMetadataName(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.Managed.Overrides = rawExt(t, map[string]interface{}{
			"metadata": map[string]interface{}{"name": "hijack"},
		})
	})
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	err := ManagedDatabaseRenderError(cluster)
	require.Error(t, err, "overriding metadata.name must be rejected")
	assert.Contains(t, err.Error(), "metadata.name")
	assert.Equal(t, testILMDB, cluster.GetName(), "the rejected override must not rename the Cluster")
}

func TestManagedDatabaseOverridesMalformedRejected(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.Managed.Overrides = &runtime.RawExtension{Raw: []byte(`"not an object"`)}
	})
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	require.Error(t, ManagedDatabaseRenderError(cluster), "a non-object override must be rejected")
}

func TestManagedDatabaseDependencies(t *testing.T) {
	// External: no deps.
	ext := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{Mode: "external"}}}
	assert.Nil(t, DatabaseDependencies(ext))

	// Managed with the pooler opted out: the Cluster CRD only.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	})
	deps := DatabaseDependencies(p)
	require.Len(t, deps, 1)
	assert.Equal(t, cnpgGroup, deps[0].GroupKind.Group)
	assert.Equal(t, cnpgKindCluster, deps[0].GroupKind.Kind)
	assert.Equal(t, ReasonCloudNativePGNotInstalled, deps[0].Reason)
	assert.Contains(t, deps[0].Message, "spec.database.mode=external", "the message names the escape hatch")

	// Managed + managed pooler: Cluster AND Pooler CRDs.
	pp := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: true}
	})
	depsP := DatabaseDependencies(pp)
	require.Len(t, depsP, 2)
	assert.Equal(t, cnpgKindPooler, depsP[1].GroupKind.Kind)
}

// TestManagedDatabaseNoLeak asserts the rendered Cluster carries no credential material —
// only the non-secret sizing/identity CNPG needs. (CNPG generates the app Secret; the
// operator never inlines credentials.)
func TestManagedDatabaseNoLeak(t *testing.T) {
	p := managedDBPlatform(nil)
	cluster := findManagedObj(ResolveManagedDatabase(p), cnpgKindCluster)
	require.NotNil(t, cluster)
	b, err := json.Marshal(cluster.Object)
	require.NoError(t, err)
	body := string(b)
	for _, forbidden := range []string{"password", "Password", "secretKey", "stringData"} {
		assert.NotContains(t, body, forbidden, "the Cluster object must carry no credential material")
	}
}
