/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Class is what a caller does about an error.
type Class int

const (
	// Retryable: 429, 5xx and transport errors; try again later (RetryAfter, when set).
	Retryable Class = iota + 1
	// Fatal: the request or the credentials are wrong; retrying the same call is pointless.
	Fatal
	// NotFound: the object does not exist.
	NotFound
	// Ambiguous: a device query matched several devices; nothing is deleted.
	Ambiguous
)

func (c Class) String() string {
	switch c {
	case Retryable:
		return "Retryable"
	case Fatal:
		return "Fatal"
	case NotFound:
		return "NotFound"
	case Ambiguous:
		return "Ambiguous"
	}
	return "Unknown"
}

// Error is a classified API error. Its message holds the operation, the HTTP status and
// the API's own error text, never a request or response body, so it is safe to log and to
// put in a status condition.
type Error struct {
	Class      Class
	Op         string
	Status     int // HTTP status; 0 for transport errors and local failures
	Message    string
	RetryAfter time.Duration // set for 429 (and 503 with Retry-After)
	Err        error         // the cause of a transport error
}

func (e *Error) Error() string {
	s := e.Op + ": " + e.Class.String()
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the transport error, if any.
func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the class of err, or 0 when err is not an *Error.
func ClassOf(err error) Class {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class
	}
	return 0
}

// RetryAfterOf returns the wait an error asks for, or 0.
func RetryAfterOf(err error) time.Duration {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.RetryAfter
	}
	return 0
}

func classify(status int) Class {
	switch {
	case status == http.StatusNotFound:
		return NotFound
	case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout, status >= 500:
		return Retryable
	default:
		return Fatal
	}
}
