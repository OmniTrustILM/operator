/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
)

// TestManagedVersionFromImageRoundTrips proves the version parsers are the exact inverse of
// the *ImageForVersion composers used at render time — the contract the upgrade guard relies
// on when reading the running version off a live CR.
func TestManagedVersionFromImageRoundTrips(t *testing.T) {
	t.Run("database (CNPG bare major tag)", func(t *testing.T) {
		assert.Equal(t, "16", ManagedDatabaseVersionFromImage(cnpgImageForVersion("16")))
		assert.Equal(t, "17", ManagedDatabaseVersionFromImage(cnpgImageForVersion("17")))
		// An OS-qualified override-hatch tag is returned verbatim (the guard's majorOf parses it).
		assert.Equal(t, "16-standard-bookworm", ManagedDatabaseVersionFromImage("ghcr.io/cloudnative-pg/postgresql:16-standard-bookworm"))
	})
	t.Run("messaging (RabbitMQ -management suffix)", func(t *testing.T) {
		assert.Equal(t, "4.0", ManagedMessagingVersionFromImage(rabbitmqImageForVersion("4.0")))
		assert.Equal(t, "3.13.7", ManagedMessagingVersionFromImage(rabbitmqImageForVersion("3.13.7")))
		// A tag without the -management suffix (a user override image) is returned as-is.
		assert.Equal(t, "4.2.0", ManagedMessagingVersionFromImage("my-mirror/rabbitmq:4.2.0"))
	})
	t.Run("keycloak (plain tag)", func(t *testing.T) {
		assert.Equal(t, "26.4.0", ManagedKeycloakVersionFromImage(keycloakImageForVersion("26.4.0")))
		assert.Equal(t, "25.0.6", ManagedKeycloakVersionFromImage(keycloakImageForVersion("25.0.6")))
	})
}

// TestImageTagEdgeCases covers the registry-port and untagged-ref cases imageTag must handle.
func TestImageTagEdgeCases(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"repo:tag", "tag"},
		{"ghcr.io/cloudnative-pg/postgresql:16", "16"},
		{"registry:5000/cloudnative-pg/postgresql:16", "16"}, // host:port AND a tag
		{"registry:5000/repo", ""},                           // host:port, NO tag
		{"bare-repo", ""},                                    // no tag at all
		{"", ""},                                             // empty input
	}
	for _, c := range cases {
		t.Run(c.image, func(t *testing.T) {
			assert.Equal(t, c.want, imageTag(c.image))
		})
	}
}

// TestManagedImageFieldPaths pins each component's spec image-field path to where the builder
// actually writes it (a drift here would make the guard read/pin the wrong field).
func TestManagedImageFieldPaths(t *testing.T) {
	assert.Equal(t, []string{"spec", "imageName"}, ManagedDatabaseImageFieldPath(), "CNPG selects the engine via spec.imageName")
	assert.Equal(t, []string{"spec", "image"}, ManagedMessagingImageFieldPath(), "RabbitMQ selects the engine via spec.image")
	assert.Equal(t, []string{"spec", "image"}, ManagedKeycloakImageFieldPath(), "Keycloak selects the engine via spec.image")
}

// TestManagedDesiredVersion reads the CR's pinned version per component, and "" when external
// or unset (so the guard falls back to the bundle default).
func TestManagedDesiredVersion(t *testing.T) {
	t.Run("database managed pinned", func(t *testing.T) {
		p := managedDBPlatform(nil) // Version "16"
		assert.Equal(t, "16", ManagedDatabaseDesiredVersion(p))
	})
	t.Run("database external is empty", func(t *testing.T) {
		p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{
			Database: otilmv1alpha1.DatabaseSpec{Mode: "external", Host: "db", Port: 5432, Name: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"}},
		}}
		assert.Equal(t, "", ManagedDatabaseDesiredVersion(p))
	})
	t.Run("messaging managed pinned", func(t *testing.T) {
		p := managedMQPlatform(nil)
		assert.Equal(t, p.Spec.Messaging.Managed.Version, ManagedMessagingDesiredVersion(p))
	})
	t.Run("keycloak managed pinned", func(t *testing.T) {
		p := managedKCPlatform(func(p *otilmv1alpha1.Platform) { p.Spec.Keycloak.Managed.Version = "26.0" })
		assert.Equal(t, "26.0", ManagedKeycloakDesiredVersion(p))
	})
	t.Run("keycloak external is empty", func(t *testing.T) {
		p := &otilmv1alpha1.Platform{Spec: otilmv1alpha1.PlatformSpec{
			Keycloak: &otilmv1alpha1.KeycloakSpec{Mode: "external", Realm: "ilm"},
		}}
		assert.Equal(t, "", ManagedKeycloakDesiredVersion(p))
	})
}
