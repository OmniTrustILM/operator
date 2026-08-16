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
	"context"
	"encoding/json"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// managedMQScheme is a scheme with the otilm types, core types, AND the RabbitMQ kinds
// (cluster + every topology kind) registered as unstructured so the fake client can
// store/GET the applied objects (the readiness probe GETs the cluster back).
func managedMQScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, otilmv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	clusterGVK := platformbuilder.ManagedMessagingClusterGVK()
	gv := clusterGVK.GroupVersion()
	for _, kind := range []string{"RabbitmqCluster", "Vhost", "User", "Permission", "Exchange", "Queue", "Binding"} {
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// managedMQPlatformCR is an in-namespace Platform with a managed broker.
func managedMQPlatformCR() *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{
			Messaging: otilmv1alpha1.MessagingSpec{
				Mode:       "managed",
				BrokerType: "rabbitmq",
				Managed: &otilmv1alpha1.ManagedMessagingSpec{
					Replicas: 1, Version: "4.0",
					Storage: otilmv1alpha1.StorageSpec{Size: "20Gi"},
				},
			},
		},
	}
}

// newManagedMQReconciler builds a Reconciler over a fake client seeded with the given
// objects and the given capability detector.
func newManagedMQReconciler(t *testing.T, det capabilityDetector, seed ...client.Object) *Reconciler {
	t.Helper()
	s := managedMQScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).Build()
	return &Reconciler{Client: c, Scheme: s, Capabilities: det}
}

func messagingReadyCondition(t *testing.T, p *otilmv1alpha1.Platform) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(p.Status.Conditions, "MessagingReady")
}

// readyClusterAndCoreSecret returns a Ready RabbitmqCluster and the Topology-generated
// Core-user Secret for the platform, for seeding the fake client.
func readyClusterAndCoreSecret(p *otilmv1alpha1.Platform) (*unstructured.Unstructured, *corev1.Secret) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	cluster.SetName(platformbuilder.ManagedMessagingName(p))
	cluster.SetNamespace(p.Namespace)
	_ = unstructured.SetNestedSlice(cluster.Object, []interface{}{
		map[string]interface{}{"type": "AllReplicasReady", "status": "True"},
	}, "status", "conditions")

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      platformbuilder.ResolveMessagingConnection(p).CredentialsSecretName,
		Namespace: p.Namespace,
	}}
	return cluster, secret
}

// readyVhost returns the Vhost CR with a Ready=True condition (the Messaging Topology
// Operator's signal that the vhost exists in the broker), for seeding the fake client so the
// two-phase apply proceeds past the vhost gate to declare the dependent topology.
func readyVhost(p *otilmv1alpha1.Platform) *unstructured.Unstructured {
	v := &unstructured.Unstructured{}
	v.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK())
	v.SetName(platformbuilder.ManagedMessagingVhostName(p))
	v.SetNamespace(p.Namespace)
	_ = unstructured.SetNestedSlice(v.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	return v
}

// listExchanges returns the Exchange topology CRs the gate has applied, for asserting whether
// the vhost-dependent topology was (or was deliberately not) declared.
func listExchanges(t *testing.T, r *Reconciler) []unstructured.Unstructured {
	t.Helper()
	var l unstructured.UnstructuredList
	l.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK().GroupVersion().WithKind("ExchangeList"))
	require.NoError(t, r.List(context.Background(), &l, client.InNamespace("ns")))
	return l.Items
}

func TestGateMessagingExternalIsReadyNoCondition(t *testing.T) {
	p := &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns"},
		Spec: otilmv1alpha1.PlatformSpec{Messaging: otilmv1alpha1.MessagingSpec{
			Mode: "external", Host: "mq", Port: 5672, VirtualHost: "ilm", Credentials: &otilmv1alpha1.CredentialsRef{SecretRef: "s"},
		}},
	}
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{}}, p)

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready, "an external broker is always ready from gateMessaging's view")
	assert.False(t, requeue)
	assert.Nil(t, messagingReadyCondition(t, p), "external mode sets no MessagingReady condition")
}

func TestGateMessagingClusterOperatorAbsentNotReady(t *testing.T) {
	p := managedMQPlatformCR()
	// Cluster Operator absent (rabbitmq.com group not served at all).
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: false}}, p)

	desired := newDesiredSet()
	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), desired)
	require.NoError(t, err, "a missing RabbitMQ CRD is non-fatal")
	assert.False(t, ready, "without the RabbitMQ Cluster Operator the managed broker is not ready")
	assert.True(t, requeue, "absent CRD requests a requeue to self-heal")
	assert.NotEmpty(t, desired, "an active managed-messaging gate keeps its objects desired across a CRD flap")

	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, platformbuilder.ReasonRabbitMQNotInstalled, cond.Reason)
	assert.Contains(t, cond.Message, "spec.messaging.mode=external", "message must be actionable")
}

// TestGateMessagingTopologyOperatorAbsentNotReady models the Cluster Operator present but
// the Messaging Topology Operator absent. A per-kind detector distinguishes the two.
func TestGateMessagingTopologyOperatorAbsentNotReady(t *testing.T) {
	p := managedMQPlatformCR()
	r := newManagedMQReconciler(t, kindDetector{available: map[string]bool{
		"RabbitmqCluster": true,  // Cluster Operator present
		"Vhost":           false, // Topology Operator absent
	}}, p)

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready)
	assert.True(t, requeue)

	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, platformbuilder.ReasonTopologyOperatorNotInstalled, cond.Reason,
		"the Topology-Operator-absent case has its own distinct reason")
}

func TestGateMessagingPresentClusterNotReadyWaits(t *testing.T) {
	p := managedMQPlatformCR()
	// Both operators present, but the cluster has not been created yet (no cluster, no Secret).
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p)

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready, "the cluster is not yet Ready / its Core-user Secret is absent")
	assert.True(t, requeue)

	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForMessaging", cond.Reason)

	// The cluster + topology must have been APPLIED. Spot-check the RabbitmqCluster.
	var applied unstructured.Unstructured
	applied.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: messagingSecretRef}, &applied))
	assert.Equal(t, common.ManagedByValue, applied.GetLabels()[common.ManagedByLabel])
	// Deletion safety: the applied cluster carries NO controller owner reference.
	assert.Empty(t, applied.GetOwnerReferences(), "the managed cluster must carry no owner reference")

	// A topology CR (the Vhost) must also have been applied. The fixture pins no spec.version,
	// so it runs the DEFAULT bundle — 2.19.0, whose "/" virtual host scopes vhost-bound names
	// with "-default". Kept a literal (not ManagedMessagingVhostName) so a naming regression is
	// a diff rather than a tautology; the unscoped legacy names stay frozen by the builder's
	// own TestLegacyTopologyNamesAreFrozen, which is pinned to 2.18.0.
	var vhost unstructured.Unstructured
	vhost.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK().GroupVersion().WithKind("Vhost"))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "ilm-messaging-default-vhost"}, &vhost))
	assert.Empty(t, vhost.GetOwnerReferences(), "topology CRs carry no owner reference either")
}

func TestGateMessagingReadyWhenClusterReadyAndSecretPresent(t *testing.T) {
	p := managedMQPlatformCR()
	cluster, secret := readyClusterAndCoreSecret(p)
	// The Vhost CR is Ready, so the two-phase apply proceeds past the vhost gate to declare the
	// dependent topology, and the gate reports the broker fully Ready.
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p, cluster, secret, readyVhost(p))

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.True(t, ready, "a Ready cluster + Ready vhost with its Core-user Secret present is ready")
	assert.False(t, requeue)

	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Reconciled", cond.Reason)

	// The vhost is Ready, so phase 2 ran: the dependent exchanges were declared.
	assert.NotEmpty(t, listExchanges(t, r), "with the vhost Ready, phase 2 declares the dependent topology")
}

func TestGateMessagingClusterReadyButSecretMissingWaits(t *testing.T) {
	p := managedMQPlatformCR()
	cluster, _ := readyClusterAndCoreSecret(p) // seed the Ready cluster but NOT the Core-user Secret
	// Seed a Ready vhost too, so the gate passes the vhost phase and waits specifically on the
	// missing Core-user Secret (the case this test exercises).
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p, cluster, readyVhost(p))

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready, "a Ready cluster without its generated Core-user Secret is still waiting")
	assert.True(t, requeue)
	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, "WaitingForMessaging", cond.Reason)
}

// TestGateMessagingWaitsForVhostBeforeTopology proves the two-phase apply that fixes the
// vhost_not_found race: with the broker cluster Ready and the Core-user Secret present but the
// Vhost CR NOT yet Ready, the gate applies the cluster + vhost (phase 1) but DEFERS the
// vhost-scoped topology (no Exchange is declared) and waits — so the Messaging Topology
// Operator never declares an exchange against a not-yet-created vhost (the failure that wedged
// Core with "no exchange 'czertainly' in vhost 'czertainly'").
func TestGateMessagingWaitsForVhostBeforeTopology(t *testing.T) {
	p := managedMQPlatformCR()
	cluster, secret := readyClusterAndCoreSecret(p) // cluster Ready + Secret present, but NO Ready vhost
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p, cluster, secret)

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)
	assert.False(t, ready, "the Vhost CR is not yet Ready, so the broker is not ready")
	assert.True(t, requeue, "waiting on the vhost requests a requeue")

	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "WaitingForMessaging", cond.Reason)

	// Phase 1 ran: the vhost (a prerequisite) was applied.
	var vhost unstructured.Unstructured
	vhost.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK())
	require.NoError(t, r.Get(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: platformbuilder.ManagedMessagingVhostName(p)}, &vhost),
		"phase 1 applies the vhost prerequisite")

	// Phase 2 was DEFERRED: no exchange may be declared until the vhost reports Ready.
	assert.Empty(t, listExchanges(t, r),
		"no exchange may be declared before the vhost is Ready — this is the vhost_not_found race fix")
}

func TestGateMessagingRejectedOverrideIsHardError(t *testing.T) {
	p := managedMQPlatformCR()
	bad, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"rabbitmq": map[string]interface{}{
			"additionalConfig": "default_user = evil"}},
	})
	p.Spec.Messaging.Managed.Overrides = &runtime.RawExtension{Raw: bad}
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p)

	_, _, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.Error(t, err, "a protected-field override is a deterministic user error → hard error (degrade)")
	assert.Contains(t, err.Error(), "spec.rabbitmq.additionalConfig")
}

func TestGateMessagingTransientDetectionErrorRequeues(t *testing.T) {
	p := managedMQPlatformCR()
	r := newManagedMQReconciler(t, programmableDetector{err: assertAnError()}, p)

	ready, requeue, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err, "a transient detector error must not become a hard reconcile error")
	assert.False(t, ready)
	assert.True(t, requeue)
	cond := messagingReadyCondition(t, p)
	require.NotNil(t, cond)
	assert.Equal(t, platformbuilder.ReasonRabbitMQNotInstalled, cond.Reason)
}

// TestGateMessagingNoLeak asserts the MessagingReady condition carries no connection
// coordinate or credential — only the generic field/remedy text.
func TestGateMessagingNoLeak(t *testing.T) {
	p := managedMQPlatformCR()
	r := newManagedMQReconciler(t, programmableDetector{available: map[string]bool{rabbitmqGroup: true}}, p)
	_, _, err := r.gateMessaging(context.Background(), p, guardTestBundle(), newDesiredSet())
	require.NoError(t, err)

	b, err := json.Marshal(p.Status.Conditions)
	require.NoError(t, err)
	condStr := string(b)
	conn := platformbuilder.ResolveMessagingConnection(p)
	assert.NotContains(t, condStr, conn.Host, "the resolved broker host must not appear in conditions")
	assert.NotContains(t, condStr, conn.CredentialsSecretName, "the credentials Secret name must not appear in conditions")
}

// kindDetector reports availability keyed by KIND (not group), so a test can model the
// Cluster Operator present while the Messaging Topology Operator is absent (both share the
// rabbitmq.com group, so a group-keyed detector cannot distinguish them).
type kindDetector struct {
	available map[string]bool
}

func (d kindDetector) Available(gk schema.GroupKind, _ ...string) (bool, error) {
	return d.available[gk.Kind], nil
}
