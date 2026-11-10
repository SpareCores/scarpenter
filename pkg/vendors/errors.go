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
// message text, with one exception: a vendor whose API has no error codes (the
// qemu driver, Vultr, OVH's OpenStack faults) may be classified by the HTTP status
// plus the message text, under the conditions in docs/reference/driver-contract.md
// (each text documented and pinned by a table-driven test, the status captured
// per request, an unmatched text Fatal and never InsufficientCapacity).
type Class string

// The error classes.
const (
	// InsufficientCapacity: the vendor has no stock for the type in the zone
	// with the capacity type right now.
	InsufficientCapacity Class = "InsufficientCapacity"
	// Unsupported: the type is not offered in the zone.
	Unsupported Class = "Unsupported"
	// QuotaExceeded: a limit is reached: the region's, or with Error.AccountWide
	// the account's.
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
	// AccountWide is set by a driver on a QuotaExceeded of a vendor whose limits
	// are account-wide (project or account limits, not per region): the
	// unavailable-offerings cache then marks the capacity type unavailable in
	// every region of the vendor, not in the one the call was made in.
	AccountWide bool
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

// AsAccountWide returns a copy of the error marked as an account-wide quota
// error (see Error.AccountWide).
func (e *Error) AsAccountWide() *Error {
	c := *e
	c.AccountWide = true
	return &c
}

// IsAccountWide reports whether the first *Error in the chain is a QuotaExceeded
// that applies to the whole account.
func IsAccountWide(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class == QuotaExceeded && e.AccountWide
	}
	return false
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
