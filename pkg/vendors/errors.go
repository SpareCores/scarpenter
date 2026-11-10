/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"errors"
	"fmt"
	"slices"
)

// Class is the class of a driver error (overview section 7.2). Drivers wrap
// every vendor error into one of them by the vendor's error codes, never by
// message text (unless the vendor only provides text).
type Class string

// The error classes.
const (
	// InsufficientCapacity: the vendor has no stock for the type in the zone
	// with the capacity type right now.
	InsufficientCapacity Class = "InsufficientCapacity"
	// Unsupported: the type is not offered in the zone.
	Unsupported Class = "Unsupported"
	// QuotaExceeded: a region-wide limit is reached.
	QuotaExceeded Class = "QuotaExceeded"
	// ZoneUnusable: the zone cannot be used at all (no subnet, no free
	// addresses).
	ZoneUnusable Class = "ZoneUnusable"
	// RegionUnavailable: the credentials cannot read or use the region (denied
	// by policy, not opted in, disabled). Returned by the calls that list a
	// region's facts, so that one region does not block the others.
	RegionUnavailable Class = "RegionUnavailable"
	// Retryable: a transient error (throttling, internal error).
	Retryable Class = "Retryable"
	// NotFound: the instance or resource does not exist.
	NotFound Class = "NotFound"
	// Fatal: everything else.
	Fatal Class = "Fatal"
)

// Classes lists every class.
var Classes = []Class{
	InsufficientCapacity, Unsupported, QuotaExceeded, ZoneUnusable, RegionUnavailable, Retryable, NotFound, Fatal,
}

// Valid reports whether c is one of the classes.
func (c Class) Valid() bool {
	return slices.Contains(Classes, c)
}

// Error is a driver error with its class. Code is the vendor's error code, for
// logs and metrics.
type Error struct {
	Class Class
	Code  string
	Err   error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s): %v", e.Class, e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

// Unwrap returns the vendor's error.
func (e *Error) Unwrap() error { return e.Err }

// NewError wraps err into the class; code is the vendor's error code (may be
// empty).
func NewError(class Class, code string, err error) *Error {
	if err == nil {
		err = errors.New(string(class))
	}
	return &Error{Class: class, Code: code, Err: err}
}

// Errorf is NewError with a formatted message and no code.
func Errorf(class Class, format string, args ...any) *Error {
	return &Error{Class: class, Err: fmt.Errorf(format, args...)}
}

// ClassOf returns the class of the first *Error in the chain; an error without
// one is Fatal, nil has no class ("").
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class
	}
	return Fatal
}

// IsClass reports whether err carries the class.
func IsClass(err error, class Class) bool {
	return err != nil && ClassOf(err) == class
}

// CodeOf returns the vendor error code of the first *Error in the chain.
func CodeOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
