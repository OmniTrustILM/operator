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

// safe_error.go carries an error whose PUBLIC text is safe to put on a condition, an Event or
// a log while the original cause survives underneath for retry classification.
//
// WHY IT EXISTS. A wrapper that carefully omits an object name does not make the error
// leak-free: a Kubernetes StatusError's own text embeds the object it is about ("queues.
// rabbitmq.com \"ilm-ilm-2-19-0-core-events\" already exists"), and the operator's
// vhost-scoped topology names encode the virtual host they are scoped to — a broker
// coordinate. Wrapping such an error with %w therefore republishes the coordinate through
// degraded()/transientRequeue(), which render cause.Error() into the platform's status and
// Events. The safe wrapper is the fix: Error() returns ONLY the message the operator chose,
// and Unwrap() keeps the original reachable so errors.As/errors.Is — and therefore
// isTransient's Conflict / timeout / webhook-warming checks — still see the real API error.
//
// WHERE TO USE IT. Any error built from an apiserver failure about an object whose NAME is a
// coordinate (the managed messaging topology above all), on a path that can reach
// applyOrDegrade / degraded / a log line. Errors about the platform's own workloads
// (Deployments, Services, the Secrets the CR already names) do not need it: the CR and the
// status already carry those names.

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// safeError is an error with a curated public message and a hidden cause.
type safeError struct {
	// public is the whole of what Error() returns — chosen by the operator, never derived
	// from the cause's text.
	public string
	// cause is the original error, reachable only through Unwrap.
	cause error
}

// Error returns the public message alone. Nothing of the cause's own text is included.
func (e *safeError) Error() string { return e.public }

// Unwrap exposes the original cause to errors.Is / errors.As, which is what keeps the
// transience classification (and any other typed inspection) working through the wrapper.
func (e *safeError) Unwrap() error { return e.cause }

// safeErrorf wraps cause with a public message built from format/args. The FORMAT AND ARGS
// must be leak-free on their own: they are the entire published text, so a caller passes
// kinds, field paths and phase names — never an object name, a virtual host, a hostname or a
// credential.
func safeErrorf(cause error, format string, args ...any) error {
	return &safeError{public: fmt.Sprintf(format, args...), cause: cause}
}

// apiFailureReason names WHY the apiserver refused a call, in its own leak-free vocabulary
// ("Invalid", "AlreadyExists", "Forbidden", …). It is what makes a safe message actionable
// without quoting the API error's text: the reason is a fixed enum, the text is not.
func apiFailureReason(err error) string {
	if reason := apierrors.ReasonForError(err); reason != "" {
		return string(reason)
	}
	return "Unknown"
}
