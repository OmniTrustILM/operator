/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- pure detection helpers -------------------------------------------------

func TestIsMajorUpgrade(t *testing.T) {
	cases := []struct {
		name             string
		running, desired string
		want             bool
	}{
		{"first create (no running) is not an upgrade", "", "4.2.0", false},
		{"unknown desired is not an upgrade", "16", "", false},
		{"same major (patch/minor bump) is not an upgrade", "4.0", "4.2.0", false},
		{"same exact version is not an upgrade", "16", "16", false},
		{"downgrade is not gated here", keycloakVersion, "25.0.0", false},
		{"major increase IS an upgrade (rabbitmq 3->4)", rabbitVersion313, "4.2.0", true},
		{"major increase IS an upgrade (pg 16->17)", "16", "17", true},
		{"major increase IS an upgrade (keycloak 25->26)", "25.0.6", keycloakVersion, true},
		{"unparseable running is not a detectable upgrade", "latest", keycloakVersion, false},
		{"unparseable desired is not a detectable upgrade", "16", "latest", false},
		{"OS-qualified running tag still parses its major", "16-standard-bookworm", "17", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isMajorUpgrade(c.running, c.desired))
		})
	}
}

func TestMajorOf(t *testing.T) {
	cases := []struct {
		version string
		want    int
		ok      bool
	}{
		{"16", 16, true},
		{"4.0", 4, true},
		{keycloakVersion, 26, true},
		{"4.2.0-management", 4, true},
		{"16-standard-bookworm", 16, true},
		{"latest", 0, false},
		{"", 0, false},
		{"v16", 0, false}, // leading non-digit is unparseable (the operator composes bare numerics)
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			got, ok := majorOf(c.version)
			assert.Equal(t, c.ok, ok)
			if c.ok {
				assert.Equal(t, c.want, got)
			}
		})
	}
}

// --- integrated guard through gateDatabase (CNPG) ---------------------------

// runningCNPGCluster returns a CNPG Cluster whose spec.imageName pins PostgreSQL major 16
// (and a healthy phase so the readiness probe passes), plus the app Secret, for seeding the
// fake client — modelling an already-running managed database at the current major.
func runningCNPGCluster(p *otilmv1alpha1.Platform) (*unstructured.Unstructured, *corev1.Secret) {
	cluster, secret := readyClusterAndSecret(p)
	_ = unstructured.SetNestedField(cluster.Object, pgImage16, "spec", "imageName")
	return cluster, secret
}

func upgradeBlockedCond(t *testing.T, p *otilmv1alpha1.Platform, prefix string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, prefix+"UpgradeBlocked")
}

func TestGateDatabaseFirstCreateAppliesDesiredVersion(t *testing.T) {
	// No running Cluster yet (first creation): the desired major applies freely, no block.
	p := managedDBPlatformCR() // Version "16"
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p)

	_, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.Nil(t, upgradeBlockedCond(t, p, "Database"), "first creation is not an upgrade")

	// The applied Cluster carries the DESIRED image (postgresql:16), not pinned to anything.
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "imageName")
	assert.Equal(t, pgImage16, img)
}

func TestGateDatabaseMinorBumpApplies(t *testing.T) {
	// Running 16, desired 16 (same major): a patch/minor change applies freely, no block.
	p := managedDBPlatformCR() // Version "16"
	cluster, secret := runningCNPGCluster(p)
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)

	ready, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Nil(t, upgradeBlockedCond(t, p, "Database"), "a same-major change is not blocked")
}

func TestGateDatabaseMajorBumpWithoutAckIsBlocked(t *testing.T) {
	// Running 16, desired 17 (MAJOR) without ack: blocked. The Cluster keeps imageName :16,
	// a UpgradeBlocked condition + Warning are set, requeue is requested, NOT Degraded.
	p := managedDBPlatformCR()
	p.Spec.Database.Managed.Version = "17"
	cluster, secret := runningCNPGCluster(p)
	rec := record.NewFakeRecorder(8)
	s := managedDBScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, cluster, secret).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec, Capabilities: programmableDetector{available: map[string]bool{cnpgGroup: true}}}

	ready, requeue, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err, "a blocked upgrade is NOT an error (adjunct, never degrades)")
	assert.True(t, ready, "the running 16 cluster stays Ready; only the bump is suppressed")
	assert.True(t, requeue, "a blocked upgrade requests a requeue to re-check after acknowledgement")

	cond := upgradeBlockedCond(t, p, "Database")
	require.NotNil(t, cond, "a major bump without ack sets DatabaseUpgradeBlocked")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonMajorUpgradeNeedsAck, cond.Reason)
	assert.Contains(t, cond.Message, "16→17")
	assert.Contains(t, cond.Message, "spec.database.managed.upgradeAcknowledged=true")
	assert.Contains(t, cond.Message, "CloudNativePG")

	// CRITICAL: the upstream Cluster keeps the RUNNING image (:16) — the major bump is suppressed.
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "imageName")
	assert.Equal(t, pgImage16, img, "the running version must be re-pinned, NOT bumped to 17")

	// A Warning Event was recorded; the platform is NOT Degraded (no phase set by the gate).
	e := drainEvent(rec)
	assert.Contains(t, e, "Warning")
	assert.Contains(t, e, reasonMajorUpgradeNeedsAck)
	assert.NotEqual(t, otilmv1alpha1.PlatformPhaseDegraded, p.Status.Phase, "a blocked upgrade must NOT degrade the platform")
}

func TestGateDatabaseMajorBumpWithAckApplies(t *testing.T) {
	// Running 16, desired 17 (MAJOR) WITH ack: applies the new version, no block.
	p := managedDBPlatformCR()
	p.Spec.Database.Managed.Version = "17"
	p.Spec.Database.Managed.UpgradeAcknowledged = true
	cluster, secret := runningCNPGCluster(p)
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)

	ready, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Nil(t, upgradeBlockedCond(t, p, "Database"), "an acknowledged major upgrade is not blocked")

	// The Cluster now carries the DESIRED image (:17).
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedDatabaseClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: dbSecretRef}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "imageName")
	assert.Equal(t, "ghcr.io/cloudnative-pg/postgresql:17", img, "an acknowledged upgrade applies the new version")
}

func TestGateDatabaseBlockedThenAcknowledgedClearsCondition(t *testing.T) {
	// A blocked upgrade sets the condition; acknowledging it on the next reconcile clears it.
	p := managedDBPlatformCR()
	p.Spec.Database.Managed.Version = "17"
	cluster, secret := runningCNPGCluster(p)
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)

	_, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	require.NotNil(t, upgradeBlockedCond(t, p, "Database"))

	// User acknowledges; the next gate pass clears the UpgradeBlocked condition.
	p.Spec.Database.Managed.UpgradeAcknowledged = true
	_, _, err = r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.Nil(t, upgradeBlockedCond(t, p, "Database"), "acknowledging clears the blocked condition")
}

// --- integrated guard through gateMessaging (RabbitMQ 3->4, the canonical case) ----

// runningRabbitCluster returns a Ready RabbitmqCluster whose spec.image pins the given running
// version (rabbitmq:<v>-management) plus the Core-user Secret, for seeding the fake client.
func runningRabbitCluster(p *otilmv1alpha1.Platform, runningVersion string) (*unstructured.Unstructured, *corev1.Secret) {
	cluster, secret := readyClusterAndCoreSecret(p)
	_ = unstructured.SetNestedField(cluster.Object, "rabbitmq:"+runningVersion+"-management", "spec", "image")
	return cluster, secret
}

func TestGateMessagingRabbitMajorBumpWithoutAckIsBlocked(t *testing.T) {
	// The headline case: running RabbitMQ 3.13.7, desired 4.x (the bundle default 4.2.0) without
	// ack → blocked; the broker keeps its running 3.x image, the platform is NOT degraded.
	p := managedMQPlatformCR()
	p.Spec.Messaging.Managed.Version = "" // fall back to the bundle default (4.2.0)
	cluster, secret := runningRabbitCluster(p, rabbitVersion313)
	rec := record.NewFakeRecorder(8)
	s := managedMQScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, cluster, secret, readyVhost(p)).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec, Capabilities: programmableDetector{available: map[string]bool{rabbitmqGroup: true}}}

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready, "the running 3.x broker stays Ready; only the bump is suppressed")
	assert.True(t, requeue)

	cond := upgradeBlockedCond(t, p, "Messaging")
	require.NotNil(t, cond)
	assert.Equal(t, reasonMajorUpgradeNeedsAck, cond.Reason)
	assert.Contains(t, cond.Message, "3.13.7→"+guardTestBundle().RabbitMQVersion)
	assert.Contains(t, cond.Message, "spec.messaging.managed.upgradeAcknowledged=true")
	assert.Contains(t, cond.Message, "RabbitMQ")

	// The RabbitmqCluster keeps its running 3.x image — NOT bumped to 4.x (which would need
	// feature flags + all-quorum-queues first and could wedge the broker).
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: messagingSecretRef}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "image")
	assert.Equal(t, "rabbitmq:3.13.7-management", img, "the running RabbitMQ version must be re-pinned, NOT bumped to 4.x")
	assert.NotEqual(t, otilmv1alpha1.PlatformPhaseDegraded, p.Status.Phase)
}

func TestGateMessagingRabbitMajorBumpWithAckApplies(t *testing.T) {
	p := managedMQPlatformCR()
	p.Spec.Messaging.Managed.Version = "4.2.0"
	p.Spec.Messaging.Managed.UpgradeAcknowledged = true
	cluster, secret := runningRabbitCluster(p, rabbitVersion313)
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p, cluster, secret, readyVhost(p))

	ready, _, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Nil(t, upgradeBlockedCond(t, p, "Messaging"))

	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: messagingSecretRef}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "image")
	assert.Equal(t, "rabbitmq:4.2.0-management", img, "an acknowledged RabbitMQ major upgrade applies")
}

// --- integrated guard through gateKeycloak ----------------------------------

func TestGateKeycloakMajorBumpWithoutAckIsBlocked(t *testing.T) {
	p := managedKCPlatformCR()
	p.Spec.Keycloak.Managed.Version = keycloakVersion
	// Seed a Ready Keycloak CR running 25.x.
	kc := readyKeycloakCR(p)
	_ = unstructured.SetNestedField(kc.Object, "quay.io/keycloak/keycloak:25.0.6", "spec", "image")
	rec := record.NewFakeRecorder(8)
	s := managedKCScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p, kc).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec, Capabilities: programmableDetector{available: map[string]bool{keycloakGroup: true}}}

	ready, requeue, err := r.gateKeycloak(context.Background(), p, guardTestBundle(), newDesiredSet(), true)
	require.NoError(t, err)
	assert.True(t, ready)
	assert.True(t, requeue)

	cond := upgradeBlockedCond(t, p, "Keycloak")
	require.NotNil(t, cond)
	assert.Equal(t, reasonMajorUpgradeNeedsAck, cond.Reason)
	assert.Contains(t, cond.Message, "25.0.6→26.4.0")
	assert.Contains(t, cond.Message, "spec.keycloak.managed.upgradeAcknowledged=true")

	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedKeycloakGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: platformbuilder.ManagedKeycloakName(p)}, &applied))
	img, _, _ := unstructured.NestedString(applied.Object, "spec", "image")
	assert.Equal(t, "quay.io/keycloak/keycloak:25.0.6", img, "the running Keycloak version must be re-pinned")
	assert.NotEqual(t, otilmv1alpha1.PlatformPhaseDegraded, p.Status.Phase)
}

// --- no-leak guard ----------------------------------------------------------

// TestUpgradeGuardNoSecretLeak asserts the blocked condition/message carries only version
// strings + the remedy field — never a credential or connection coordinate.
func TestUpgradeGuardNoSecretLeak(t *testing.T) {
	p := managedDBPlatformCR()
	p.Spec.Database.Managed.Version = "17"
	cluster, secret := runningCNPGCluster(p)
	r := newManagedDBReconciler(t, programmableDetector{available: map[string]bool{cnpgGroup: true}}, p, cluster, secret)

	_, _, err := r.gateDatabase(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)

	got := conditionsString(t, p)
	assert.NotContains(t, got, "ilm-db-rw", "no connection coordinate in the condition")
	assert.NotContains(t, got, "Password", "no credential in the condition")
	// Sanity: the actionable parts ARE present.
	assert.Contains(t, got, "16→17")
}
