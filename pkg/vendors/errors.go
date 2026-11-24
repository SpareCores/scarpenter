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
	"time"
)

// CodeUnknown is the code of an error whose vendor gave none, so that the text
// of every error is "<Class> (<Code>): <message>".
const CodeUnknown = "Unknown"

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
	// DailyLimit: the vendor refuses to create more resources in the region until
	// a daily limit resets (OCI's daily resource creation limit). Deleting nodes
	// does not help. The region is unavailable until Error.ResetAt, or the next
	// 00:00 UTC when the driver does not set it.
	DailyLimit Class = "DailyLimit"
	// InvalidOffering: the vendor rejected the launch parameters of this offering
	// (type, zone, capacity type) for a reason that is not known to be of the
	// offering: an unrecognised 400, or a configuration the operator can fix
	// (an image that does not list the shape). The offering is marked
	// unavailable for an hour and the next candidate is tried.
	InvalidOffering Class = "InvalidOffering"
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
	InsufficientCapacity, Unsupported, QuotaExceeded, DailyLimit, InvalidOffering, ZoneUnusable, RegionUnavailable,
	Retryable, NotFound, Fatal,
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
	// RegionWide is set by a driver on an InsufficientCapacity error that the
	// vendor documents as being of the whole region, not of the zone the call
	// was made in: the unavailable-offerings cache then marks the type in every
	// zone of the region.
	RegionWide bool
	// PreLaunch is set by a driver on an Unsupported error found by its own
	// check, before any vendor call, as opposed to the vendor's "not offered"
	// answer. Such a check is deterministic from tables the driver read, so the
	// unavailable-offerings cache keeps its mark for minutes, not a day.
	PreLaunch bool
	// ClassScoped is set by a driver on an InvalidOffering error that comes from
	// the NodeClass's configuration: the offering is then unavailable for that
	// NodeClass only.
	ClassScoped bool
	// ResetAt is when the limit of a DailyLimit error resets, when the vendor
	// documents it; zero means the next 00:00 UTC.
	ResetAt time.Time
}

// Error implements error.
func (e *Error) Error() string {
	code := e.Code
	if code == "" {
		code = CodeUnknown
	}
	return fmt.Sprintf("%s (%s): %v", e.Class, code, e.Err)
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

// AsClassScoped returns a copy of the error marked as caused by the NodeClass's
// configuration (see Error.ClassScoped).
func (e *Error) AsClassScoped() *Error {
	c := *e
	c.ClassScoped = true
	return &c
}

// IsClassScoped reports whether the first *Error in the chain is an
// InvalidOffering of the NodeClass's configuration.
func IsClassScoped(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class == InvalidOffering && e.ClassScoped
	}
	return false
}

// AsRegionWide returns a copy of the error marked as a region-wide capacity
// error (see Error.RegionWide).
func (e *Error) AsRegionWide() *Error {
	c := *e
	c.RegionWide = true
	return &c
}

// IsRegionWide reports whether the first *Error in the chain is an
// InsufficientCapacity that applies to the whole region.
func IsRegionWide(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class == InsufficientCapacity && e.RegionWide
	}
	return false
}

// AsPreLaunch returns err marked as found by the driver's own check before the
// vendor call (see Error.PreLaunch); an error that is not an *Error is returned
// unchanged.
func AsPreLaunch(err error) error {
	if e, ok := errors.AsType[*Error](err); ok {
		c := *e
		c.PreLaunch = true
		return &c
	}
	return err
}

// CodeNoSpot is the code of the Unsupported error of a spot request to a vendor
// that has no spot capacity.
const CodeNoSpot = "NoSpot"

// NoSpot is the error of a driver whose vendor has no spot capacity, for a
// request with CapacityType spot. It is Unsupported (ADR-017: the node is never
// launched on-demand under a spot NodeClaim) and PreLaunch (no vendor call was
// made). Keeper has no spot prices for such a vendor, so the catalog does not
// offer spot there and this is a backstop.
func NoSpot(vendor string) error {
	return AsPreLaunch(NewError(Unsupported, CodeNoSpot, fmt.Errorf("%s has no spot instances", vendor)))
}

// IsPreLaunch reports whether the first *Error in the chain is marked PreLaunch.
func IsPreLaunch(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.PreLaunch
	}
	return false
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
