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

// Package rabbitmq provides a read-mostly client for RabbitMQ's HTTP management API,
// used to observe a vhost's queue depths and client connections.
//
// SECURITY: the broker's management endpoint, vhost and administrator credentials are
// in-memory inputs only. No value returned by this package — least of all an error — may
// carry the host, the vhost, the username or the password, because callers place errors in
// status conditions, events and logs.
package rabbitmq

import (
	"context"
	"fmt"
)

// BrokerAdmin is the subset of RabbitMQ's HTTP management API the migration needs.
//
// Implementations must FAIL CLOSED: an error must never be reported as "drained". Anything
// the broker did not affirmatively report — an unreachable broker, a non-2xx status, a body
// that does not parse, a queue entry missing its depth fields — is an error, never a zero
// value. A caller polling for emptiness may treat only a successful, complete answer as
// evidence that a vhost is idle.
type BrokerAdmin interface {
	// Queues returns every queue on the vhost with its ready and unacknowledged depths
	// and its consumer count.
	Queues(ctx context.Context, vhost string) ([]QueueState, error)
	// BoundQueues returns the destination queue names bound from the given exchange on
	// the vhost — how dynamic per-proxy queues are discovered (never by name matching).
	BoundQueues(ctx context.Context, vhost, exchange string) ([]string, error)
	// Connections returns the number of client connections currently open on the vhost.
	Connections(ctx context.Context, vhost string) (int, error)
	// CloseConnections force-closes every client connection on the vhost. Used only on
	// the force path.
	CloseConnections(ctx context.Context, vhost string) error
}

// QueueState is one queue's drain-relevant state: how many messages the broker still holds
// for it, ready plus unacknowledged. A queue is drained only when both depths are zero.
//
// The broker also reports a CONSUMER count, which this type deliberately does not carry.
// Neither the drain nor the cleanup may gate on it: a healthy remote proxy is permanently
// attached to its own queue, so waiting for zero consumers would deadlock the migration
// against the very clients it exists to keep serving. What gates the cleanup is the count of
// open CONNECTIONS on the virtual host (BrokerAdmin.Connections), which is a different
// question with a different answer.
type QueueState struct {
	// Name is the queue name as reported by the broker.
	Name string
	// MessagesReady is the number of messages ready for delivery.
	MessagesReady int64
	// MessagesUnacked is the number of messages delivered but not yet acknowledged.
	MessagesUnacked int64
}

// Error is the only error type this package returns. It carries a generic, leak-free message
// plus at most the HTTP status code, and it tells the caller whether retrying is worthwhile.
//
// SECURITY: Message is a fixed phrase chosen by this package. It never embeds the broker
// host, the vhost, the exchange, the credentials, or any part of the broker's response body.
type Error struct {
	// StatusCode is the HTTP status the broker returned, or 0 when the call never completed.
	StatusCode int
	// Message is a generic description of the failed step.
	Message string
	// Retryable reports whether the failure may resolve on its own (transport hiccups,
	// 5xx responses, unparseable bodies) as opposed to a deterministic rejection.
	Retryable bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	suffix := ""
	if e.Retryable {
		suffix = " (retryable)"
	}
	if e.StatusCode == 0 {
		return fmt.Sprintf("broker error: %s%s", e.Message, suffix)
	}
	return fmt.Sprintf("broker error %d: %s%s", e.StatusCode, e.Message, suffix)
}
