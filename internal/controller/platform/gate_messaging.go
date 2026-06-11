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
	"time"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/bom"
	platformbuilder "github.com/OmniTrustILM/operator/internal/builder/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Messaging gating condition type and reasons. MessagingReady is an ADJUNCT signal (like
// DatabaseReady / EdgeReady): it reports the managed broker's provisioning state without
// ever blocking the platform's Available condition — Core/scheduler wait on the generated
// per-user Secret via their secretKeyRef, exactly as they would wait on an external
// Secret. Reasons carry no identity material (no host/port/URI, no credentials).
const (
	// conditionMessagingReady reports whether the operator-provisioned (managed) broker is
	// provisioned and its Topology-generated Core-user credentials Secret is available. It
	// is set only for a managed broker; an external broker drops it (nothing to report).
	conditionMessagingReady = "MessagingReady"
	// reasonWaitingForMessaging: the rabbitmq.com CRDs are served and the RabbitmqCluster +
	// topology are applied, but the cluster is not yet Ready or the Core-user Secret is not
	// yet present. A transient, self-healing waiting state (requeue).
	reasonWaitingForMessaging = "WaitingForMessaging"
	// reasonMessagingReady: the managed cluster is Ready and the Core-user Secret is present.
	reasonMessagingReady = "Reconciled"
)

// messagingRequeueAfter is how long the reconciler waits before re-checking a managed
// broker that is applied but not yet Ready (the RabbitmqCluster is still provisioning, or
// the Topology Operator has not generated the Core-user Secret yet). The Secret Watch
// (SetupWithManager) re-enqueues on the generated Secret's creation, so this requeue is a
// backstop only — mirroring databaseRequeueAfter.
const messagingRequeueAfter = 15 * time.Second

// gateMessaging reconciles the managed broker (RabbitmqCluster + the messaging topology)
// and reflects its state on the MessagingReady condition. It is the messaging twin of
// gateDatabase, with the same extra state: a present CRD does not imply a Ready broker, so
// after applying the rabbitmq.com objects it probes the cluster's readiness and the
// generated Core-user Secret.
//
// Returns ready=true only when an external broker (nothing to provision — always "ready"
// from the platform's perspective) OR a managed broker whose cluster is Ready and whose
// Core-user Secret exists. requeue=true asks the caller to re-check soon. An error is
// returned only for an actual apply failure or a rejected override (a missing CRD or a
// not-yet-Ready cluster is non-fatal, like the database/edge).
//
// Behaviour (managed broker):
//   - render-time error (rejected override / malformed patch) → hard error (degrade).
//   - Cluster CRD absent → MessagingReady=False/RabbitMQNotInstalled, apply nothing, mark
//     the rabbitmq.com objects desired (prune-preservation across a flap), requeue.
//   - Topology CRD absent → MessagingReady=False/TopologyOperatorNotInstalled, ditto.
//   - both present → apply the RabbitmqCluster + topology; then probe readiness:
//     cluster not Ready or Core-user Secret missing → MessagingReady=False/WaitingForMessaging,
//     ready=false, requeue; both present → MessagingReady=True, ready=true.
//
// SECURITY: the condition message names only the spec field and the remedy — never a
// secret value or a connection coordinate.
func (r *Reconciler) gateMessaging(ctx context.Context, p *otilmv1alpha1.Platform, bundle bom.Bundle, desired desiredSet) (ready bool, requeue bool, err error) {
	gate := r.messagingGate(p)
	gate.versionGuard = messagingVersionGuard(p, bundle)
	return r.gateManagedInfra(ctx, p, desired, gate)
}

// messagingVersionGuard builds the major-version upgrade guard descriptor for the managed
// broker (nil when external). The running version is read from the live RabbitmqCluster's
// spec.image (the "-management" suffix stripped); the reference version is
// spec.messaging.managed.version, or the bundle's RabbitMQVersion when the CR pins none. A
// RabbitMQ major jump (3.x → 4.x) is the canonical wedge-the-broker case this guards.
func messagingVersionGuard(p *otilmv1alpha1.Platform, bundle bom.Bundle) *infraVersionGuard {
	if !platformbuilder.MessagingManaged(p) {
		return nil
	}
	return &infraVersionGuard{
		clusterGVK:       platformbuilder.ManagedMessagingClusterGVK(),
		clusterName:      platformbuilder.ManagedMessagingName(p),
		imageFieldPath:   platformbuilder.ManagedMessagingImageFieldPath(),
		versionFromImage: platformbuilder.ManagedMessagingVersionFromImage,
		desiredVersion:   platformbuilder.ManagedMessagingDesiredVersion(p),
		bundleVersion:    bundle.RabbitMQVersion,
		acknowledged:     p.Spec.Messaging.Managed.UpgradeAcknowledged,
		conditionPrefix:  "Messaging",
		upstreamLabel:    "RabbitMQ",
		ackFieldPath:     "spec.messaging.managed.upgradeAcknowledged",
	}
}

// messagingGate describes the managed broker (RabbitMQ Cluster + Messaging Topology
// operators) for the shared gateManagedInfra / handleManagedInfraDeletion paths: its
// predicate, rendered CRs (cluster + the full topology), render-error accessor, CRD
// dependencies (Cluster Operator + Topology Operator), the cluster+Core-user-Secret
// readiness probe, and the MessagingReady condition vocabulary. databaseGate is its
// deliberate sibling: both populate the same managedInfraGate shape with their distinct
// values (the shared shape is the point), so the structural similarity dupl flags is
// intentional.
//
//nolint:dupl // deliberate per-component descriptor sibling of databaseGate; see doc above
func (r *Reconciler) messagingGate(p *otilmv1alpha1.Platform) managedInfraGate {
	return managedInfraGate{
		managed:      platformbuilder.MessagingManaged(p),
		kind:         "broker",
		name:         platformbuilder.ManagedMessagingName(p),
		objects:      platformbuilder.ResolveManagedMessaging(p),
		renderError:  platformbuilder.ManagedMessagingRenderError,
		dependencies: platformbuilder.MessagingDependencies(p),
		// Ready when the RabbitmqCluster reports Ready AND the Topology-generated Core-user
		// Secret exists.
		ready: func(ctx context.Context) bool {
			return managedReadyProbe(ctx,
				func(ctx context.Context) (bool, error) { return r.managedBrokerReady(ctx, p) },
				func(ctx context.Context) bool { return r.managedCoreUserSecretPresent(ctx, p) })
		},
		conditionType:  conditionMessagingReady,
		waitingReason:  reasonWaitingForMessaging,
		waitingMessage: "waiting for the managed broker to become ready",
		readyReason:    reasonMessagingReady,
		readyMessage:   "managed messaging reconciled",
		detectionLabel: "messaging",
		// Two-phase apply: declare the broker + its Vhost first, and only declare the
		// vhost-scoped topology (exchanges/queues/bindings/permissions) once the Vhost CR
		// reports Ready. Otherwise the Messaging Topology Operator races — it declares an
		// exchange against the not-yet-created vhost, fails it (vhost_not_found), and converges
		// only after a slow per-object backoff, wedging Core (which publishes to that exchange).
		isPrereq:             isMessagingPrereqObject,
		prereqReady:          func(ctx context.Context) bool { return r.managedVhostReady(ctx, p) },
		prereqWaitingMessage: "waiting for the managed broker virtual host before declaring its topology",
	}
}

// managedBrokerReady reports whether the managed RabbitmqCluster is Ready. It GETs the
// cluster as unstructured (its GVK is preset; the type is not registered in the scheme)
// and checks the RabbitMQ Cluster Operator's documented readiness signal:
//   - status.conditions[type=AllReplicasReady].status == "True", and/or
//   - status.conditions[type=ClusterAvailable].status == "True".
//
// A NotFound (the cluster was just applied and has no status yet, or detection is
// momentarily behind) is "not ready". The managed-messaging e2e
// (test/e2e/platform_test.go) drove a real RabbitmqCluster to Ready and the operator's
// MessagingReady condition converged to True via exactly these two condition types — both
// AllReplicasReady and ClusterAvailable are RabbitmqClusterConditionType constants the
// Cluster Operator sets (internal/status/status.go), and the e2e observed the cluster reach
// them. Re-verify when bumping the pinned Cluster Operator.
func (r *Reconciler) managedBrokerReady(ctx context.Context, p *otilmv1alpha1.Platform) (bool, error) {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(platformbuilder.ManagedMessagingClusterGVK())
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ManagedMessagingName(p)}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return false, nil
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		if (t == "AllReplicasReady" || t == "ClusterAvailable") && cm["status"] == string(metav1.ConditionTrue) {
			return true, nil
		}
	}
	return false, nil
}

// managedCoreUserSecretPresent reports whether the Messaging Topology Operator-generated
// Core-user credentials Secret exists yet. The Topology Operator creates it once the User
// CR reconciles; until then a dependent's secretKeyRef would not resolve. A read error
// other than NotFound is treated as "not present" (the requeue retries) so a transient API
// blip never degrades the platform. SECURITY: only the Secret's existence is checked — its
// content is never read.
func (r *Reconciler) managedCoreUserSecretPresent(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	name := platformbuilder.ResolveMessagingConnection(p).CredentialsSecretName
	if name == "" {
		return false
	}
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, &s); err != nil {
		return false
	}
	return true
}

// managedVhostReady reports whether the managed broker's Vhost CR reports Ready. The Messaging
// Topology Operator sets a status condition of type "Ready" (string status "True") on each
// topology object once it has declared it in the broker. gateManagedInfra gates the
// vhost-scoped topology (exchanges/queues/bindings/permissions) on this so they are never
// declared against a not-yet-created vhost — which the Topology Operator would fail as
// vhost_not_found and retry only on a slow backoff, wedging Core (it publishes to the
// czertainly exchange and crash-loops with "no exchange ... in vhost" until the topology
// converges). A NotFound (the Vhost CR was just applied and has no status yet) or any read
// error is "not ready" so the requeue retries.
func (r *Reconciler) managedVhostReady(ctx context.Context, p *otilmv1alpha1.Platform) bool {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(platformbuilder.ManagedMessagingVhostGVK())
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: platformbuilder.ManagedMessagingVhostName(p)}, &u); err != nil {
		return false
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == conditionTypeReady && cm["status"] == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

// isMessagingPrereqObject reports whether a rendered managed-messaging object is a topology
// prerequisite — the RabbitmqCluster (the broker) or the Vhost — that must exist before the
// vhost-scoped topology (exchanges/queues/bindings/permissions) can be declared. It keys off
// the rendered object's Kind via the builder's preset GVKs (no new string literals).
func isMessagingPrereqObject(obj client.Object) bool {
	k := obj.GetObjectKind().GroupVersionKind().Kind
	return k == platformbuilder.ManagedMessagingClusterGVK().Kind || k == platformbuilder.ManagedMessagingVhostGVK().Kind
}
