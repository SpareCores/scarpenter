/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// HTTPStatus is implemented by the errors of API calls that know the HTTP
// status of the response: the real client's APIError and the fake's Error. A
// status of 0 means unknown (a transport error, or a status govultr dropped).
type HTTPStatus interface {
	error
	HTTPStatus() int
}

// APIError is what the real client wraps every failed call into. govultr turns
// an API error into a plain error and returns no *http.Response with it, so the
// status is captured by the client's transport and carried here.
type APIError struct {
	Status  int
	Message string
	Err     error
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// Unwrap returns the SDK's error.
func (e *APIError) Unwrap() error { return e.Err }

// HTTPStatus returns the response status, 0 when unknown.
func (e *APIError) HTTPStatus() int { return e.Status }

// Error codes the driver raises itself; the classification table below adds
// the codes of the text rules.
const (
	codeCanceled        = "CallCanceled"
	codeTimeout         = "CallTimeout"
	codeTransport       = "TransportError"
	codeRateLimited     = "RateLimited"
	codeServerError     = "ServerError"
	codeRetriesExhaust  = "RetriesExhausted"
	codeDependency      = "Dependency"
	codeLocked          = "Locked"
	codeNotFound        = "NotFound"
	codeRefNotFound     = "ReferencedResourceNotFound"
	codeUnauthorized    = "Unauthorized"
	codeForbidden       = "Forbidden"
	codeStatusUnknown   = "StatusUnknown"
	codeUnmatched       = "Unmatched"
	codeAccountLimit    = "AccountLimit"
	codeResourceLimit   = "ResourceLimit"
	codePlanUnavailable = "PlanUnavailable"
	codeStock           = "OutOfStock"
	codeInvalidPlan     = "InvalidPlan"
	codeIncompatible    = "Incompatible"
	codeRegionUnusable  = "RegionUnusable"
	codeImageNotFound   = "ImageNotFound"
	codeImageAmbiguous  = "ImageAmbiguous"
	codeUserData        = "InvalidUserData"
	codeUserDataTooBig  = "UserDataTooLarge"
	codeUnsupportedArch = "NoSuchInstances"
	codeNotOurs         = "NotOurInstance"
	codeNoPublicIPv4Off = "PublicIPv4Required"
	codeInvalid         = "InvalidRequest"
)

// operation is what a failed call was doing; some texts mean different things
// per operation.
type operation int

const (
	opOther operation = iota
	opCreateInstance
	opGet
	opDelete
	// opRegion is a call that reads a region's facts (the availability).
	opRegion
)

// textRule classifies a 4xx response by its message. This is the documented
// exception to "never by message text" (driver contract, error classes): the
// API has no error codes, the status alone cannot tell the cases apart, every
// text is pinned by errors_test.go, and the table is a HYPOTHESIS until spike
// S4 of plan 20 records the real messages. A rule never applies without a known
// status, and a text that matches nothing is Fatal.
type textRule struct {
	ops      []operation // empty: every operation
	statuses []int       // empty: any 4xx
	contains []string    // lower case; any one matches
	class    vendors.Class
	code     string
	// accountWide marks a QuotaExceeded of the account's limits.
	accountWide bool
}

// textRules are tried in order: the first match wins.
var textRules = []textRule{
	{
		ops:      []operation{opDelete},
		contains: []string{"attached", "in use", "still has", "instances using", "has instances", "locked", "not ready"},
		class:    vendors.Retryable, code: codeDependency,
	},
	{
		ops: []operation{opCreateInstance}, statuses: []int{400, 402, 403, 412, 422},
		contains: []string{
			"account limit", "instance limit", "limit of instances", "maximum number of instances",
			"maximum instance cost", "spending limit", "limit has been reached", "exceeded your", "cost limit",
		},
		class: vendors.QuotaExceeded, code: codeAccountLimit, accountWide: true,
	},
	{
		ops: []operation{opOther}, statuses: []int{400, 402, 403, 412, 422},
		contains: []string{
			"firewall group limit", "vpc limit", "maximum number of firewall groups", "maximum number of vpcs",
		},
		class: vendors.QuotaExceeded, code: codeResourceLimit, accountWide: true,
	},
	{
		ops: []operation{opCreateInstance}, statuses: []int{400, 412, 422},
		contains: []string{"user_data", "user data"},
		class:    vendors.Fatal, code: codeUserData,
	},
	{
		ops: []operation{opCreateInstance}, statuses: []int{400, 412, 422},
		contains: []string{
			"out of stock", "sold out", "no capacity", "insufficient capacity", "currently unavailable",
			"not currently available", "not available in this location", "not available in this region",
			"unavailable in this location",
		},
		class: vendors.InsufficientCapacity, code: codeStock,
	},
	{
		ops: []operation{opCreateInstance}, statuses: []int{400, 412, 422},
		contains: []string{
			"invalid plan", "plan not found", "unknown plan", "cannot be deployed", "incompatible", "not compatible",
			"does not support",
		},
		class: vendors.Unsupported, code: codeIncompatible,
	},
	{
		// Vultr has no zones, so a region the create refuses outright is the "zone
		// that cannot be used at all" of the contract: the region is gated for a
		// while instead of one (plan, region) pair at a time.
		ops: []operation{opCreateInstance}, statuses: []int{400, 412, 422},
		contains: []string{"invalid region", "unknown region", "region is disabled", "region is not enabled"},
		class:    vendors.ZoneUnusable, code: codeRegionUnusable,
	},
}

func (r textRule) applies(op operation, status int, msg string) bool {
	if len(r.ops) > 0 && !slices.Contains(r.ops, op) {
		return false
	}
	if len(r.statuses) > 0 && !slices.Contains(r.statuses, status) {
		return false
	}
	if len(r.statuses) == 0 && (status < 400 || status >= 500) {
		return false
	}
	for _, s := range r.contains {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// govultr's own text for a call whose retries ran out (it drops the response):
// not a Vultr message, so it may decide without a status. Only 429, 5xx and
// network errors are retried.
const retriesExhaustedText = "gave up after"

// classify wraps err into the contract's classes, by status first and then by
// message for the cases a status cannot tell apart. nil stays nil, a
// *vendors.Error passes through.
func classify(err error, op operation) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, codeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return vendors.NewError(vendors.Retryable, codeTimeout, err)
	}
	status := 0
	if se, ok := errors.AsType[HTTPStatus](err); ok {
		status = se.HTTPStatus()
	}
	msg := strings.ToLower(err.Error())
	switch {
	case status == http.StatusTooManyRequests:
		return vendors.NewError(vendors.Retryable, codeRateLimited, err)
	case status >= 500:
		return vendors.NewError(vendors.Retryable, codeServerError, err)
	case status == http.StatusNotFound:
		switch op {
		case opCreateInstance:
			// The call named a snapshot, group or VPC that does not exist: not the
			// instance, so not NotFound.
			return vendors.NewError(vendors.Fatal, codeRefNotFound, err)
		case opRegion:
			return vendors.NewError(vendors.RegionUnavailable, codeNotFound, err)
		}
		return vendors.NewError(vendors.NotFound, codeNotFound, err)
	case status == 0:
		if _, ok := errors.AsType[net.Error](err); ok {
			return vendors.NewError(vendors.Retryable, codeTransport, err)
		}
		if strings.Contains(msg, retriesExhaustedText) {
			return vendors.NewError(vendors.Retryable, codeRetriesExhaust, err)
		}
		// A text alone never decides.
		return vendors.NewError(vendors.Fatal, codeStatusUnknown, err)
	}
	for _, r := range textRules {
		if r.applies(op, status, msg) {
			e := vendors.NewError(r.class, r.code, err)
			if r.accountWide {
				e = e.AsAccountWide()
			}
			return e
		}
	}
	switch {
	case op == opDelete && (status == http.StatusConflict || status == http.StatusLocked):
		return vendors.NewError(vendors.Retryable, codeLocked, err)
	case op == opRegion && (status == http.StatusBadRequest || status == http.StatusForbidden):
		return vendors.NewError(vendors.RegionUnavailable, codeInvalid, err)
	case status == http.StatusUnauthorized:
		return vendors.NewError(vendors.Fatal, codeUnauthorized, err)
	case status == http.StatusForbidden:
		return vendors.NewError(vendors.Fatal, codeForbidden, err)
	}
	return vendors.NewError(vendors.Fatal, codeUnmatched, err)
}

// planNotAvailable is the error of the pre-call stock check: the plan is not in
// the region's available_plans. It is capacity, not quota (ADR-013).
func planNotAvailable(plan, region string) error {
	return vendors.NewError(vendors.InsufficientCapacity, codePlanUnavailable,
		fmt.Errorf("plan %s is not in the available plans of region %s", plan, region))
}
