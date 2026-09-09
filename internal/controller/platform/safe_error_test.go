/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package platform

// These specs cover the wrapper that keeps a Kubernetes error's own text out of the platform's
// status, its Events and its logs — while keeping the error itself reachable for the transience
// classification every apply path makes.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// topologyStatusError is the shape of the problem: a StatusError about a vhost-scoped topology
// object. Its OWN message names the object, whose name encodes the virtual host it is scoped to
// — so wrapping it with %w republishes a broker coordinate however carefully the wrapper is
// worded.
func topologyStatusError() error {
	return apierrors.NewAlreadyExists(
		schema.GroupResource{Group: "rabbitmq.com", Resource: "queues"},
		"ilm-"+sourceVirtualHost+"-core-events")
}

// TestSafeErrorPublishesOnlyItsOwnMessage: Error() is the operator's chosen text and nothing
// else — the cause contributes not one character, because that text is what degraded() writes
// verbatim into the Degraded condition and the Warning Event.
func TestSafeErrorPublishesOnlyItsOwnMessage(t *testing.T) {
	cause := topologyStatusError()
	require.Contains(t, cause.Error(), sourceVirtualHost, "the case is only interesting if the cause leaks")

	wrapped := safeErrorf(cause, "applying a managed messaging %s failed (%s)", "Queue", apiFailureReason(cause))

	assert.Equal(t, "applying a managed messaging Queue failed (AlreadyExists)", wrapped.Error())
	assert.NotContains(t, wrapped.Error(), sourceVirtualHost)
	assertNoBrokerCoordinates(t, wrapped.Error())
}

// TestSafeErrorKeepsTheCauseReachable: the point of hiding the text is that nothing else about
// the error is hidden. isTransient must still tell a retryable conflict from a deterministic
// rejection through the wrapper, or every wrapped conflict would flip the platform to Degraded.
func TestSafeErrorKeepsTheCauseReachable(t *testing.T) {
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "rabbitmq.com", Resource: "queues"}, "ilm-"+sourceVirtualHost+"-core", errors.New("conflict"))
	wrapped := safeErrorf(conflict, "applying a managed messaging Queue failed (%s)", apiFailureReason(conflict))

	assert.True(t, isTransient(wrapped), "a conflict stays retryable through the wrapper")
	assert.True(t, apierrors.IsConflict(errors.Unwrap(wrapped)))
	assert.NotContains(t, wrapped.Error(), sourceVirtualHost)

	rejected := safeErrorf(apierrors.NewBadRequest("nope"), "applying a managed messaging Queue failed (%s)", "BadRequest")
	assert.False(t, isTransient(rejected), "a deterministic rejection still degrades")
}

// TestApiFailureReasonIsLeakFree: the reason is the apiserver's own fixed vocabulary, which is
// what makes a safe message actionable without quoting the error text that carries the name.
func TestApiFailureReasonIsLeakFree(t *testing.T) {
	assert.Equal(t, string(metav1.StatusReasonAlreadyExists), apiFailureReason(topologyStatusError()))
	assert.Equal(t, "Unknown", apiFailureReason(errors.New("something the apiserver did not classify")))
}
