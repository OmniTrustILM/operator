/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// db_connection.go is the mode-agnostic readback seam: it resolves the database
// connection facts (host/port/name + the credentials Secret name) the platform's
// builders and the controller's composed-Secret reconciler consume, REGARDLESS of
// whether the database is external (coordinates from the spec) or managed (coordinates
// from the CloudNativePG-generated Service + app Secret).
//
// This is the crux of the managed-database work: by funnelling both modes through one
// DatabaseConnection, every downstream consumer (DatabaseURL wiring in resolve.go,
// sharedCredSecretEnv, the controller's reconcileAuthDBSecret) stays mode-agnostic — a
// managed dependent is started and wired EXACTLY like an external one (a host/port + a
// secretKeyRef into a credentials Secret), with only the source of those facts differing.
// It is also the seam the managed RabbitMQ/Keycloak readbacks reuse.

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
)

// DatabaseConnection is the resolved, mode-agnostic database connection the platform's
// components are wired to. For external mode it is the CR coordinates verbatim; for
// managed mode it is the CloudNativePG-generated read-write Service (or the Pooler
// Service when pgBouncer.managed), the fixed app database name, and the CNPG-generated
// app Secret name.
//
// SECURITY: this struct holds only NON-secret coordinates plus the NAME of a credentials
// Secret — never a secret value. It must never be placed (in whole or in part) into
// status, conditions, events, or logs; the credentials are consumed by reference
// (secretKeyRef) only.
type DatabaseConnection struct {
	// Host is the database hostname the platform connects to (an in-cluster Service name
	// for managed mode, the caller's host for external mode).
	Host string
	// Port is the database port.
	Port int32
	// Name is the application database name.
	Name string
	// CredentialsSecretName names the Secret holding the basic-auth username/password the
	// platform authenticates with (the caller's Secret for external mode, the
	// CNPG-generated <cluster>-app Secret for managed mode). The <cluster>-app Secret carries
	// both the username and password keys the wiring profile reads.
	CredentialsSecretName string
	// UsernameKey / PasswordKey are the EFFECTIVE in-Secret keys the username/password live
	// under. For external mode they are the user's spec.database.credentials key overrides
	// when set, else the wiring-profile defaults (username/password). For managed mode they
	// are ALWAYS the wiring-profile defaults — the keys are the upstream CNPG operator's
	// generated-Secret convention (which matches username/password), NOT user-mappable. The
	// secretKeyRef wiring (sharedCredSecretEnv), the operator-side read for the composed
	// auth connection string (reconcileAuthDBSecret), AND the managed Keycloak CR's
	// spec.db.{usernameSecret,passwordSecret} (keycloakDBBlock) all source these keys, so a
	// single mapping feeds every path.
	UsernameKey string
	PasswordKey string
}

// ResolveDatabaseConnection resolves the mode-agnostic DatabaseConnection for a Platform:
//
//   - external → the spec coordinates (host/port/name) and credentials.secretRef verbatim,
//     with the in-Secret keys resolved as pick(spec override, bom default).
//   - managed  → Host is the CNPG read-write Service "<cluster>-rw" (or the Pooler
//     Service "<pooler>" when pgBouncer.managed), Port is 5432, Name is the bootstrapped
//     application database ("ilm"), and CredentialsSecretName is the CNPG-generated
//     "<cluster>-app" Secret.
//
// The CNPG generated-resource names are a fixed function of the operator-owned Cluster
// name (ManagedDatabaseName), which is why those override paths are protected — so this
// readback can never be invalidated by a caller's Overrides.
func ResolveDatabaseConnection(p *otilmv1alpha1.Platform) DatabaseConnection {
	w := wiringFor(p)
	if DatabaseManaged(p) {
		cluster := ManagedDatabaseName(p)
		host := cluster + cnpgRWServiceSuffix
		if poolerManaged(p) {
			// CNPG creates a Service named after the Pooler (<pooler-name>); the platform
			// connects to it instead of the cluster's read-write Service. Default-on for a
			// managed database (poolerManaged), so this is the normal managed-DB wiring.
			host = ManagedPoolerName(p)
		}
		return DatabaseConnection{
			Host:                  host,
			Port:                  managedDBPort,
			Name:                  managedDBAppName,
			CredentialsSecretName: cluster + cnpgAppSecretSuffix,
			// Managed: the keys are the CNPG operator's generated-Secret convention (which
			// matches the wiring defaults), NOT user-mappable.
			UsernameKey: w.DatabaseCred.UsernameKey,
			PasswordKey: w.DatabaseCred.PasswordKey,
		}
	}
	creds := p.Spec.Database.Credentials
	return DatabaseConnection{
		Host: p.Spec.Database.Host,
		Port: p.Spec.Database.Port,
		Name: p.Spec.Database.Name,
		// External: the user's mapping wins; the wiring profile supplies the default key.
		CredentialsSecretName: credentialsSecretRef(creds),
		UsernameKey:           credUsernameKey(creds, w),
		PasswordKey:           credPasswordKey(creds, w),
	}
}

// credentialsSecretRef returns the referenced Secret name from a CredentialsRef, or ""
// when the ref is nil/unset (external mode without credentials — admission rejects it).
func credentialsSecretRef(c *otilmv1alpha1.CredentialsRef) string {
	if c == nil {
		return ""
	}
	return c.SecretRef
}

// credUsernameKey resolves the effective username key as pick(spec override, bom default),
// keeping the wiring profile as the single source of the default key.
func credUsernameKey(c *otilmv1alpha1.CredentialsRef, w bom.WiringProfile) string {
	if c != nil && c.UsernameKey != "" {
		return c.UsernameKey
	}
	return w.DatabaseCred.UsernameKey
}

// credPasswordKey resolves the effective password key as pick(spec override, bom default).
func credPasswordKey(c *otilmv1alpha1.CredentialsRef, w bom.WiringProfile) string {
	if c != nil && c.PasswordKey != "" {
		return c.PasswordKey
	}
	return w.DatabaseCred.PasswordKey
}
