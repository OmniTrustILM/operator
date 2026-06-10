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

// managed_upgrade.go exposes the per-component VERSION primitives the reconciler's
// major-version upgrade guard needs, keeping all image-ref construction AND parsing in the
// builder (the one place that knows each upstream operator's image-field path + image
// format). The guard itself (compare majors, set the condition, pin the running version
// back when blocked) lives in the controller; this file gives it leak-free, pure helpers:
//
//   - the spec image-field path on each upstream CR (where the operator writes the image,
//     and therefore where the running image is read back from the live CR);
//   - the parse from a running image ref back to its version string (the inverse of the
//     *ImageForVersion composers used at render time);
//   - the desired version for each component (spec.<infra>.managed.version, empty → "").
//
// The bundle default version (RabbitMQVersion/CNPGVersion/KeycloakVersion) is supplied by
// the reconciler, which already resolves the bundle once per reconcile — it is NOT read
// here, so this stays a pure, bundle-agnostic helper.

import (
	"strings"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// Image-field paths on each managed CR spec — the single source of where the operator
// writes the engine image, and therefore where the live (running) image is read back.
// CNPG selects the engine via spec.imageName; RabbitMQ and Keycloak via spec.image.
//
// VERIFIED at render time: cnpgImageForVersion writes spec.imageName (buildCNPGCluster),
// rabbitmqImageForVersion writes spec.image (buildRabbitmqCluster), keycloakImageForVersion
// writes spec.image (buildKeycloak). The running-version read uses these SAME paths.
//
//nolint:gochecknoglobals // immutable field-path constants expressed as slices
var (
	managedDatabaseImageFieldPath  = []string{"spec", "imageName"}
	managedMessagingImageFieldPath = []string{"spec", "image"}
	managedKeycloakImageFieldPath  = []string{"spec", "image"}
)

// ManagedDatabaseImageFieldPath returns the unstructured field path of the CloudNativePG
// Cluster's engine image (spec.imageName). The reconciler reads the running image from the
// live Cluster at this path and re-pins it there when blocking a major upgrade.
func ManagedDatabaseImageFieldPath() []string { return managedDatabaseImageFieldPath }

// ManagedMessagingImageFieldPath returns the unstructured field path of the RabbitmqCluster's
// engine image (spec.image).
func ManagedMessagingImageFieldPath() []string { return managedMessagingImageFieldPath }

// ManagedKeycloakImageFieldPath returns the unstructured field path of the Keycloak CR's
// engine image (spec.image).
func ManagedKeycloakImageFieldPath() []string { return managedKeycloakImageFieldPath }

// ManagedDatabaseVersionFromImage parses the major PostgreSQL version out of a CloudNativePG
// engine image ref. It is the inverse of cnpgImageForVersion ("ghcr.io/cloudnative-pg/
// postgresql:<major>"): it returns the tag (e.g. "16" from ".../postgresql:16"). An image
// with no tag, or one whose tag the operator did not compose, yields "" (treated by the
// guard as an UNKNOWN running version → not a detectable upgrade, applied freely). A user's
// override-hatch image (a private mirror / OS-qualified tag) also parses by tag, so a
// "16-standard-bookworm" tag yields "16-standard-bookworm" and majorOf still extracts 16.
func ManagedDatabaseVersionFromImage(image string) string { return imageTag(image) }

// ManagedMessagingVersionFromImage parses the RabbitMQ version out of a RabbitmqCluster engine
// image ref. It is the inverse of rabbitmqImageForVersion ("rabbitmq:<version>-management"):
// it returns the tag with the trailing "-management" stripped (e.g. "4.0" from
// "rabbitmq:4.0-management"). A tag without that suffix is returned verbatim so a user's
// override-hatch image still yields a comparable version.
func ManagedMessagingVersionFromImage(image string) string {
	return strings.TrimSuffix(imageTag(image), "-management")
}

// ManagedKeycloakVersionFromImage parses the Keycloak version out of a Keycloak CR engine
// image ref. It is the inverse of keycloakImageForVersion ("quay.io/keycloak/keycloak:
// <version>"): it returns the tag (e.g. "26.4.0" from ".../keycloak:26.4.0").
func ManagedKeycloakVersionFromImage(image string) string { return imageTag(image) }

// ManagedDatabaseDesiredVersion returns the version the CR requests for the managed database
// (spec.database.managed.version), or "" when managed mode is off or the version is unset
// (the caller falls back to the bundle default).
func ManagedDatabaseDesiredVersion(p *otilmv1alpha1.Platform) string {
	if !DatabaseManaged(p) {
		return ""
	}
	return p.Spec.Database.Managed.Version
}

// ManagedMessagingDesiredVersion returns the version the CR requests for the managed broker
// (spec.messaging.managed.version), or "" when managed mode is off or the version is unset.
func ManagedMessagingDesiredVersion(p *otilmv1alpha1.Platform) string {
	if !MessagingManaged(p) {
		return ""
	}
	return p.Spec.Messaging.Managed.Version
}

// ManagedKeycloakDesiredVersion returns the version the CR requests for the managed Keycloak
// (spec.keycloak.managed.version), or "" when managed mode is off or the version is unset.
func ManagedKeycloakDesiredVersion(p *otilmv1alpha1.Platform) string {
	if !KeycloakManaged(p) {
		return ""
	}
	return p.Spec.Keycloak.Managed.Version
}

// imageTag returns the tag portion of a "repo[:tag]" image ref (the substring after the LAST
// colon, so a registry "host:port/repo:tag" still yields "tag"). A ref with no tag, or one
// whose final segment after the last colon contains a path separator (i.e. the colon was a
// port, not a tag separator), yields "". The empty string is returned for an empty input.
func imageTag(image string) string {
	if image == "" {
		return ""
	}
	idx := strings.LastIndex(image, ":")
	if idx < 0 {
		return ""
	}
	tag := image[idx+1:]
	// A colon that is part of a "host:port" with no tag (e.g. "registry:5000/repo") leaves a
	// path separator in the candidate tag — that is not a real tag.
	if strings.ContainsAny(tag, "/") {
		return ""
	}
	return tag
}
