/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
)

func TestResolveDatabaseConnectionExternalUnchanged(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{
				Mode: "external", Host: "postgres.example.com", Port: 5433, Name: "ilmdb", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "ilm-db"},
			},
		},
	}
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, "postgres.example.com", conn.Host, "external host comes straight from the spec")
	assert.Equal(t, int32(5433), conn.Port)
	assert.Equal(t, "ilmdb", conn.Name)
	assert.Equal(t, "ilm-db", conn.CredentialsSecretName)
}

func TestResolveDatabaseConnectionManagedResolvesToCNPG(t *testing.T) {
	// Opt OUT of the default-on pooler so this exercises the direct <cluster>-rw path.
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	})
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, "ilm-db-rw", conn.Host, "without a managed pooler the managed host is the CNPG read-write Service <cluster>-rw")
	assert.Equal(t, int32(5432), conn.Port)
	assert.Equal(t, "ilm", conn.Name, "managed name is the bootstrapped application database")
	assert.Equal(t, "ilm-db-app", conn.CredentialsSecretName, "managed creds come from the CNPG-generated <cluster>-app Secret")
}

func TestResolveDatabaseConnectionManagedWithPoolerRoutesThroughPooler(t *testing.T) {
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: true}
	})
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, "ilm-db-pooler", conn.Host, "with a managed pooler the platform connects through the Pooler Service")
	// The app Secret is still the cluster's generated Secret (the pooler authenticates the
	// same role); only the host changes.
	assert.Equal(t, "ilm-db-app", conn.CredentialsSecretName)
}

// TestResolveDatabaseConnectionManagedDefaultsToPooler proves the pooler is DEFAULT-ON for a
// managed database: with no pgBouncer block the platform connects through the Pooler Service
// (not <cluster>-rw), so the ILM fleet does not exhaust Postgres's connection slots.
func TestResolveDatabaseConnectionManagedDefaultsToPooler(t *testing.T) {
	p := managedDBPlatform(nil) // managed DB, no pgBouncer block → pooler on by default
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, "ilm-db-pooler", conn.Host, "a managed database defaults to connecting through the PgBouncer Pooler")
}

// TestResolveDatabaseConnectionWiresThroughDatabaseURL proves the mode-agnostic readback
// drives the SAME wiring profile JDBC-URL render for both modes: the builders consume the
// resolved facts identically, so a managed dependent is wired exactly like an external one.
func TestResolveDatabaseConnectionWiresThroughDatabaseURL(t *testing.T) {
	w := bom.Wiring()

	// Opt out of the default pooler so the managed host is the <cluster>-rw Service.
	managed := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		p.Spec.Database.PgBouncer = &otilmv1alpha1.PgBouncerSpec{Managed: false}
	})
	mc := ResolveDatabaseConnection(managed)
	managedURL := w.DatabaseURL(mc.Host, mc.Port, mc.Name)
	assert.Equal(t, "jdbc:postgresql://ilm-db-rw:5432/ilm?characterEncoding=UTF-8", managedURL)

	external := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{
		Mode: "external", Host: "db", Port: 5432, Name: "ilmdb", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"},
	}}}
	ec := ResolveDatabaseConnection(external)
	externalURL := w.DatabaseURL(ec.Host, ec.Port, ec.Name)
	assert.Equal(t, "jdbc:postgresql://db:5432/ilmdb?characterEncoding=UTF-8", externalURL)
}

// TestResolveDatabaseConnectionDefaultKeysWhenUnmapped proves an external Secret without
// key overrides resolves to the BOM default in-Secret keys (username/password).
func TestResolveDatabaseConnectionDefaultKeysWhenUnmapped(t *testing.T) {
	w := bom.Wiring()
	p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{
		Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"},
	}}}
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, w.DatabaseCred.UsernameKey, conn.UsernameKey, "unmapped username key defaults to the BOM key")
	assert.Equal(t, w.DatabaseCred.PasswordKey, conn.PasswordKey, "unmapped password key defaults to the BOM key")
	assert.Equal(t, "username", conn.UsernameKey)
	assert.Equal(t, "password", conn.PasswordKey)
}

// TestResolveDatabaseConnectionMappedKeysExternal proves a user's in-Secret key overrides
// (the External-Secrets/Vault/CNPG-shaped Secret case: POSTGRES_USER/POSTGRES_PASSWORD)
// flow onto the resolved connection, which feeds BOTH the secretKeyRef wiring AND the
// composed-connection-string read.
func TestResolveDatabaseConnectionMappedKeysExternal(t *testing.T) {
	p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{Database: otilmv1alpha1.DatabaseSpec{
		Mode: "external", Host: "db", Port: 5432, Name: "ilm",
		Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "pg", UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD"},
	}}}
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, "pg", conn.CredentialsSecretName)
	assert.Equal(t, "POSTGRES_USER", conn.UsernameKey, "mapped username key wins over the BOM default")
	assert.Equal(t, "POSTGRES_PASSWORD", conn.PasswordKey, "mapped password key wins over the BOM default")
}

// TestResolveDatabaseConnectionManagedIgnoresUserKeys proves a managed database ALWAYS uses
// the upstream CNPG operator's generated-Secret keys (the BOM defaults), even if the user
// set key overrides on the spec — the mapping is an external-mode-only contract.
func TestResolveDatabaseConnectionManagedIgnoresUserKeys(t *testing.T) {
	w := bom.Wiring()
	p := managedDBPlatform(func(p *otilmv1alpha1.Platform) {
		// A stray user mapping on a managed spec must not leak into the upstream readback.
		p.Spec.Database.Credentials = &otilmv1alpha1.CredentialsRef{UsernameKey: "POSTGRES_USER", PasswordKey: "POSTGRES_PASSWORD"}
	})
	conn := ResolveDatabaseConnection(p)
	assert.Equal(t, w.DatabaseCred.UsernameKey, conn.UsernameKey, "managed keeps the CNPG-generated username key")
	assert.Equal(t, w.DatabaseCred.PasswordKey, conn.PasswordKey, "managed keeps the CNPG-generated password key")
}
