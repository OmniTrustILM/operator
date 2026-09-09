/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// platformGR is a GroupResource for constructing apierrors in the transience tests.
var platformGR = schema.GroupResource{Group: otilmGroup, Resource: "platforms"}

// timeoutNetErr is a net.Error that reports Timeout()==true, modelling a transient transport
// error (a connection reset / temporary DNS / dial timeout) the REST client surfaces.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "dial tcp: i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

var _ net.Error = timeoutNetErr{}

func TestIsTransient(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"SSA conflict from a competing field manager", apierrors.NewConflict(platformGR, "ilm", errors.New("the object has been modified"))},
		{"server timeout", apierrors.NewServerTimeout(platformGR, "apply", 1)},
		{"too many requests (429)", apierrors.NewTooManyRequestsError("slow down")},
		{"service unavailable (503)", apierrors.NewServiceUnavailable("apiserver starting up")},
		{"timeout", apierrors.NewTimeoutError("request timed out", 1)},
		{"context deadline exceeded", context.DeadlineExceeded},
		{"context canceled", context.Canceled},
		{"wrapped conflict", fmt.Errorf("applying Deployment %q: %w", "core", apierrors.NewConflict(platformGR, "ilm", nil))},
		{"transient net timeout (REST transport)", timeoutNetErr{}},
		{"wrapped deadline", fmt.Errorf("reading status: %w", context.DeadlineExceeded)},
		// A just-installed upstream operator (its CRD served, so the capability probe passes)
		// whose admission webhook endpoint is not yet serving: the apply hits a failurePolicy=Fail
		// webhook and the apiserver returns a 500 naming the webhook call. Self-healing — must
		// requeue, not degrade a Platform whose dependencies are correctly installed.
		{"webhook registered but endpoint not serving yet (connection refused)",
			apierrors.NewInternalError(errors.New(`failed calling webhook "vhost.rabbitmq.com": failed to call webhook: Post "https://messaging-topology-operator-webhook-service.rabbitmq-system.svc:443/validate-rabbitmq-com-v1beta1-vhost": dial tcp 10.96.0.1:443: connect: connection refused`))},
		{"webhook service has no ready endpoints",
			apierrors.NewInternalError(errors.New(`failed calling webhook "user.rabbitmq.com": failed to call webhook: no endpoints available for service "messaging-topology-operator-webhook-service"`))},
		{"wrapped webhook-not-ready (the managed-infra apply wrap)",
			fmt.Errorf("applying managed broker %s %q: %w", "Vhost", "ilm",
				apierrors.NewInternalError(errors.New(`failed calling webhook "vhost.rabbitmq.com": connect: connection refused`)))},
	}
	for _, c := range transient {
		t.Run("transient/"+c.name, func(t *testing.T) {
			assert.True(t, isTransient(c.err), "must be classified transient")
		})
	}

	deterministic := []struct {
		name string
		err  error
	}{
		{"nil is not transient", nil},
		{"NotFound (a missing required Secret) is deterministic", apierrors.NewNotFound(platformGR, "missing-secret")},
		{"Invalid (a rejected override) is deterministic", apierrors.NewInvalid(schema.GroupKind{Group: otilmGroup, Kind: "Platform"}, "ilm", nil)},
		{"BadRequest is deterministic", apierrors.NewBadRequest("malformed")},
		{"Forbidden (an RBAC gap) is deterministic", apierrors.NewForbidden(platformGR, "ilm", errors.New("no perms"))},
		{"a plain render error is deterministic", errors.New("override rejects protected path spec.bootstrap.initdb.database")},
		// A generic apiserver 500 that is NOT a webhook-call failure stays deterministic: only
		// the specific webhook-not-ready vocabulary is folded into the self-healing path, so the
		// classifier is not over-broad.
		{"a generic apiserver InternalError (no webhook) is deterministic", apierrors.NewInternalError(errors.New("unexpected end of JSON input"))},
	}
	for _, c := range deterministic {
		t.Run("deterministic/"+c.name, func(t *testing.T) {
			assert.False(t, isTransient(c.err), "must NOT be classified transient")
		})
	}
}

// errPlatform is a minimal Platform with a known prior phase, for the routing tests.
func errPlatform(priorPhase otilmv1alpha1.PlatformPhase) *otilmv1alpha1.Platform {
	return &otilmv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "ilm", Namespace: "ns", Generation: 2},
		Status:     otilmv1alpha1.PlatformStatus{Phase: priorPhase},
	}
}

// TestApplyOrDegradeTransientDoesNotDegrade: a transient (Conflict) apply error requeues
// (returns the error) WITHOUT setting Phase=Degraded, writing a Degraded condition, or
// recording a Warning Event — the platform keeps its prior phase.
func TestApplyOrDegradeTransientDoesNotDegrade(t *testing.T) {
	s := reconcileScheme(t)
	p := errPlatform(otilmv1alpha1.PlatformPhaseRunning)
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	cause := apierrors.NewConflict(platformGR, "ilm", errors.New("modified"))
	res, err := r.applyOrDegrade(context.Background(), p, "ApplyError", cause)

	require.Error(t, err, "a transient error is returned so controller-runtime backs off")
	assert.Equal(t, cause, err)
	assert.Zero(t, res.RequeueAfter, "backoff is controller-runtime's exponential retry, not a fixed RequeueAfter")
	assert.NotEqual(t, otilmv1alpha1.PlatformPhaseDegraded, p.Status.Phase,
		"a transient apply error must NOT flip the platform to Degraded")
	assert.Equal(t, otilmv1alpha1.PlatformPhaseRunning, p.Status.Phase, "the prior phase is preserved")
	assert.Nil(t, meta.FindStatusCondition(p.Status.Conditions, "Degraded"), "no Degraded condition is written")
	assert.Empty(t, drainEvent(rec), "no Warning Event is recorded for a transient error")
}

// TestApplyOrDegradeDeterministicDegrades: a deterministic (Invalid) apply error degrades —
// Phase=Degraded, a Degraded condition, a Warning Event, and the error is returned.
func TestApplyOrDegradeDeterministicDegrades(t *testing.T) {
	s := reconcileScheme(t)
	p := errPlatform(otilmv1alpha1.PlatformPhaseProgressing)
	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s, Recorder: rec}

	cause := apierrors.NewInvalid(schema.GroupKind{Group: otilmGroup, Kind: "Platform"}, "ilm", nil)
	_, err := r.applyOrDegrade(context.Background(), p, "ApplyError", cause)

	require.Error(t, err)
	assert.Equal(t, cause, err)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseDegraded, p.Status.Phase, "a deterministic error degrades")
	cond := meta.FindStatusCondition(p.Status.Conditions, "Degraded")
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "ApplyError", cond.Reason)
	assert.Equal(t, p.Generation, cond.ObservedGeneration)
	// The Degraded MESSAGE carries the actual cause (the API error), not just the reason code,
	// so the condition is actionable — a bare "Message: ApplyError" tells the operator nothing.
	assert.Equal(t, cause.Error(), cond.Message,
		"the Degraded message must surface the cause, not repeat the reason")
	assert.Contains(t, cond.Message, "is invalid", "the underlying API error is exposed")
	e := drainEvent(rec)
	assert.Contains(t, e, "Warning")
	assert.Contains(t, e, "ApplyError")
	assert.Contains(t, e, "is invalid", "the Warning event also carries the cause")
}

// TestTransientRequeuePreservesConditions: transientRequeue does not clear conditions the
// gates set earlier in the reconcile (it simply does not persist) — the prior phase + any
// adjunct condition remain untouched in memory.
func TestTransientRequeuePreservesConditions(t *testing.T) {
	s := reconcileScheme(t)
	p := errPlatform(otilmv1alpha1.PlatformPhaseRunning)
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: "Available", Status: metav1.ConditionTrue, Reason: "AllComponentsReady", ObservedGeneration: p.Generation,
	})
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).WithObjects(p).Build()
	r := &Reconciler{Client: c, Scheme: s}

	_, err := r.transientRequeue(context.Background(), p, "ApplyError", errors.New("conflict"))
	require.Error(t, err)
	assert.Equal(t, otilmv1alpha1.PlatformPhaseRunning, p.Status.Phase)
	avail := meta.FindStatusCondition(p.Status.Conditions, "Available")
	require.NotNil(t, avail)
	assert.Equal(t, metav1.ConditionTrue, avail.Status, "transientRequeue leaves the Available condition intact")
	assert.Nil(t, meta.FindStatusCondition(p.Status.Conditions, "Degraded"))
}
