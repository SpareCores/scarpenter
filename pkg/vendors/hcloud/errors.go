/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are the API's
// error.code values).
const (
	CodeTimeout          = "CallTimeout"
	CodeCanceled         = "CallCanceled"
	CodeTransport        = "TransportError"
	CodeServerError      = "ServerError"
	CodePlacementFull    = "PlacementFull"
	CodeNameInUse        = "NameInUse"
	CodeUserDataTooLarge = "UserDataTooLarge"
	CodeInvalidSettings  = "InvalidSettings"
	CodeImageNotFound    = "ImageNotFound"
	CodeImageAmbiguous   = "ImageAmbiguous"
	CodeUnsupportedLoc   = "UnsupportedLocation"
)

// API error codes the SDK has no constant for.
const (
	apiCodeUnavailable hc.ErrorCode = "unavailable"
)

// errorClasses maps the API's error.code to the driver's error class (plan 15,
// "Error mapping"). It is the single table: classify reads it and a test covers
// every entry. invalid_input is not in it (it depends on the field, see
// classify); neither is anything unlisted, which is Fatal.
//
// The mapping of resource_unavailable and placement_error and the field names
// of invalid_input are from the API documentation and from how the Cluster API
// provider reads them; the exact responses are unverified until the L5 lane.
var errorClasses = map[hc.ErrorCode]vendors.Class{
	// Stock: the location has no capacity for the type, or the placement failed.
	hc.ErrorCodeResourceUnavailable:   vendors.InsufficientCapacity,
	hc.ErrorCodePlacementError:        vendors.InsufficientCapacity,
	hc.ErrorCodeNoSpaceLeftInLocation: vendors.InsufficientCapacity,
	// The type does not exist where it was asked for.
	hc.ErrorCodeInvalidServerType: vendors.Unsupported,
	// Project limits (account-wide, see classify).
	hc.ErrorCodeResourceLimitExceeded: vendors.QuotaExceeded,
	// The network of the location is full.
	hc.ErrorCodeNoSubnetAvailable: vendors.ZoneUnusable,
	hc.ErrorCodeIPNotAvailable:    vendors.ZoneUnusable,
	// Transient errors, and dependencies that clear when the servers using a
	// resource are gone.
	hc.ErrorCodeRateLimitExceeded: vendors.Retryable,
	hc.ErrorCodeServerError:       vendors.Retryable,
	hc.ErrorCodeBadGateway:        vendors.Retryable,
	hc.ErrorCodeTimeout:           vendors.Retryable,
	apiCodeUnavailable:            vendors.Retryable,
	hc.ErrorCodeConflict:          vendors.Retryable,
	hc.ErrorCodeLocked:            vendors.Retryable,
	hc.ErrorCodeMaintenance:       vendors.Retryable,
	hc.ErrorCodeRobotUnavailable:  vendors.Retryable,
	hc.ErrorCodeServiceError:      vendors.Retryable,
	hc.ErrorCodeUniquenessError:   vendors.Retryable,
	hc.ErrorCodeResourceInUse:     vendors.Retryable,
	// Things that do not exist.
	hc.ErrorCodeNotFound: vendors.NotFound,
	// Fatal, listed so that every code the plan names is pinned.
	hc.ErrorCodeUnauthorized:      vendors.Fatal,
	hc.ErrorCodeForbidden:         vendors.Fatal,
	hc.ErrorCodeJSONError:         vendors.Fatal,
	hc.ErrorCodeProtected:         vendors.Fatal,
	hc.ErrorDeprecatedAPIEndpoint: vendors.Fatal,
	hc.ErrorUnsupportedError:      vendors.Fatal,
}

// unsupportedFields are the invalid_input fields that mean the type, location
// or image does not fit the request (type not offered in the location,
// architecture mismatch with the snapshot).
var unsupportedFields = []string{"server_type", "location", "image"}

// classify wraps an error of an API call into the driver's error classes.
// Errors that are already classified pass through unchanged. Classification is
// by the API's error code, never by message (the one exception is a bare HTTP
// 5xx without a JSON body, which the SDK reports only as a wrapped status).
func classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return vendors.NewError(vendors.Retryable, CodeTimeout, err)
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, CodeCanceled, err)
	}
	if apiErr, ok := errors.AsType[hc.Error](err); ok {
		return fromCode(apiErr.Code, invalidFields(apiErr.Details), err)
	}
	if actErr, ok := errors.AsType[hc.ActionError](err); ok {
		return fromCode(hc.ErrorCode(actErr.Code), nil, err)
	}
	if errors.Is(err, hc.ErrStatusCode) {
		if st, perr := strconv.Atoi(err.Error()[max(0, len(err.Error())-3):]); perr == nil && st >= 500 {
			return vendors.NewError(vendors.Retryable, CodeServerError, err)
		}
		return vendors.NewError(vendors.Fatal, "", err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, CodeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

func invalidFields(details any) []string {
	d, ok := details.(hc.ErrorDetailsInvalidInput)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(d.Fields))
	for _, f := range d.Fields {
		names = append(names, f.Name)
	}
	return names
}

func fromCode(code hc.ErrorCode, fields []string, err error) error {
	if code == hc.ErrorCodeInvalidInput {
		if len(fields) > 0 && allUnsupportedFields(fields) {
			return vendors.NewError(vendors.Unsupported, string(code), err)
		}
		return vendors.NewError(vendors.Fatal, string(code), err)
	}
	class, ok := errorClasses[code]
	if !ok {
		return vendors.NewError(vendors.Fatal, string(code), err)
	}
	e := vendors.NewError(class, string(code), err)
	if class == vendors.QuotaExceeded {
		// Hetzner's limits are per project, not per location.
		return e.AsAccountWide()
	}
	return e
}

func allUnsupportedFields(fields []string) bool {
	for _, f := range fields {
		ok := false
		for _, u := range unsupportedFields {
			if f == u || strings.HasPrefix(f, u+".") || strings.HasPrefix(f, u+"[") {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// isCode reports whether err carries the API error code (also when classified).
func isCode(err error, codes ...hc.ErrorCode) bool {
	if hc.IsError(err, codes...) {
		return true
	}
	c := vendors.CodeOf(err)
	for _, want := range codes {
		if c == string(want) {
			return true
		}
	}
	return false
}
