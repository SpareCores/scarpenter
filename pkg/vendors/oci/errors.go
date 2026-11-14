/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"

	"github.com/oracle/oci-go-sdk/v65/common"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are OCI's).
const (
	codeTimeout    = "CallTimeout"
	codeCanceled   = "CallCanceled"
	codeTransport  = "TransportError"
	codeHTTPPrefix = "HTTP"
	codeNotOurs    = "NotOwned"
	codeUserData   = "UserDataTooLarge"
	codeSettings   = "InvalidSettings"
	codeUnknownReg = "UnknownRegion"
	codeNoSuchZone = "UnknownZone"
	codeNotReady   = "NotImplemented"
	codeTokenClash = "RetryTokenConflict"
)

// OCI's error codes the driver acts on (the Oracle API errors page).
const (
	CodeInternalError        = "InternalError"
	CodeLimitExceeded        = "LimitExceeded"
	CodeQuotaExceeded        = "QuotaExceeded"
	CodeInvalidParameter     = "InvalidParameter"
	CodeTooManyRequests      = "TooManyRequests"
	CodeIncorrectState       = "IncorrectState"
	CodeLockConflict         = "LockConflict"
	CodeConflict             = "Conflict"
	CodeNotAuthorizedOrFound = "NotAuthorizedOrNotFound"
	CodeNotAuthenticated     = "NotAuthenticated"
)

// callKind says what a call is for, because the same status means different
// things: a 404 on a get is NotFound, on a launch it is a missing subnet or
// image or a missing permission (OCI does not say which).
type callKind int

const (
	// kindRead is a get or a list of one resource: 404 is NotFound.
	kindRead callKind = iota
	// kindLaunch is the launch call.
	kindLaunch
	// kindDelete is the delete of a resource: a 409 says that something still uses it,
	// which clears when the users are gone.
	kindDelete
	// kindRegion reads the facts of a region (subscriptions, availability
	// domains): a denied or unknown region is RegionUnavailable.
	kindRegion
)

// Message patterns of the classification table. Oracle's errors carry no
// machine-readable reason beyond the code, so a few rows are decided by the text
// (the contract's rule for message-based classification: each text is pinned
// by errors_test.go, the status is captured per request, an unmatched text is
// Fatal or Retryable, never InsufficientCapacity). All of them are UNVERIFIED
// live (plan 25, spike S5); the first live run corrects them.
var (
	capacityRe = regexp.MustCompile(`(?i)out of (host )?capacity`)
	// unsupportedRe: the shape cannot be used with the image or in the AD, or an
	// extended memory configuration the tenancy cannot take.
	unsupportedRe = regexp.MustCompile(`(?i)(shape .*(is )?not compatible with image|shape .*not available|` +
		`shape .*not supported in the availability domain|not supported in the availability domain|` +
		`extended memory|is not a valid shape)`)
	// subnetFullRe: the subnet has no free IP addresses.
	subnetFullRe = regexp.MustCompile(`(?i)subnet.*(no (more )?(free |available )?ip|not enough (free )?ip|` +
		`ip (addresses )?(are )?exhausted|out of ip)`)
	limitNameRe = regexp.MustCompile(`(?i)(?:limits? exceeded:|limits were exceeded:)\s*([a-z0-9-]+)`)
)

// LimitNameOf extracts the limit name from the message of a LimitExceeded error
// ("The following service limits were exceeded: standard-e5-core-count"); "" when
// the message has another form.
func LimitNameOf(msg string) string {
	if m := limitNameRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// serviceError reads the SDK's service error out of an error chain.
func serviceError(err error) (common.ServiceError, bool) {
	var se common.ServiceError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// classify wraps the error of an API call into the driver's classes (plan 25,
// section 7). An error that is already classified passes through. Everything not
// listed is Fatal.
func classify(err error, kind callKind) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, common.DeadlineExceededByBackoff):
		return vendors.NewError(vendors.Retryable, codeTimeout, err)
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, codeCanceled, err)
	}
	if se, ok := serviceError(err); ok {
		return classifyService(se.GetHTTPStatusCode(), se.GetCode(), se.GetMessage(), kind, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, codeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

// classifyService is the table. status is the HTTP status, code OCI's error code.
func classifyService(status int, code, msg string, kind callKind, cause error) error {
	switch {
	// Capacity first: it is a 500, which every other 5xx row would call
	// transient.
	case status == http.StatusInternalServerError && code == CodeInternalError && capacityRe.MatchString(msg):
		return vendors.NewError(vendors.InsufficientCapacity, code, cause)
	case code == CodeLimitExceeded || code == CodeQuotaExceeded:
		// The limits of the Compute service are per availability domain or region,
		// not account-wide.
		return vendors.NewError(vendors.QuotaExceeded, code, cause)
	case status == http.StatusBadRequest && code == CodeInvalidParameter:
		switch {
		case unsupportedRe.MatchString(msg):
			return vendors.NewError(vendors.Unsupported, code, cause)
		case subnetFullRe.MatchString(msg):
			return vendors.NewError(vendors.ZoneUnusable, code, cause)
		}
		return vendors.NewError(vendors.Fatal, code, cause)
	case status == http.StatusConflict && code == CodeConflict && subnetFullRe.MatchString(msg):
		return vendors.NewError(vendors.ZoneUnusable, code, cause)
	case status == http.StatusTooManyRequests || code == CodeTooManyRequests:
		return vendors.NewError(vendors.Retryable, codeOf(code, status), cause)
	case code == CodeIncorrectState || code == CodeLockConflict || (kind == kindDelete && status == http.StatusConflict):
		// A resource still in use or locked: clears when the users are gone.
		return vendors.NewError(vendors.Retryable, code, cause)
	}
	switch {
	case kind == kindRegion && (status == http.StatusNotFound || code == CodeNotAuthenticated ||
		status == http.StatusUnauthorized || status == http.StatusForbidden):
		return vendors.NewError(vendors.RegionUnavailable, codeOf(code, status), cause)
	case status == http.StatusNotFound && kind == kindLaunch:
		// A missing subnet, image or compartment, or a missing permission: OCI
		// does not say which, and the launch cannot be retried into success. The
		// launch has no instance yet, so it is never NotFound.
		return vendors.NewError(vendors.Fatal, code, fmt.Errorf(
			"%w (a subnet, image or compartment of the launch is missing, or the policy does not allow it)", cause))
	case status == http.StatusNotFound:
		return vendors.NewError(vendors.NotFound, codeOf(code, status), cause)
	case status >= http.StatusInternalServerError && status != http.StatusNotImplemented:
		return vendors.NewError(vendors.Retryable, codeOf(code, status), cause)
	}
	return vendors.NewError(vendors.Fatal, codeOf(code, status), cause)
}

func codeOf(code string, status int) string {
	if code != "" {
		return code
	}
	return fmt.Sprintf("%s%d", codeHTTPPrefix, status)
}

// isRetryTokenConflict reports whether err is the answer to an opc-retry-token
// that was used by another request (HTTP 409; the code is UNVERIFIED, S4/S5): the
// driver resolves it by looking the instance up by its idempotency key. The
// conflicts that mean "still in use" (IncorrectState, LockConflict) and a full
// subnet (ZoneUnusable in the table) are not it.
func isRetryTokenConflict(err error) bool {
	se, ok := serviceError(err)
	if !ok || se.GetHTTPStatusCode() != http.StatusConflict || subnetFullRe.MatchString(se.GetMessage()) {
		return false
	}
	switch se.GetCode() {
	case CodeIncorrectState, CodeLockConflict:
		return false
	}
	return true
}

// isNotFound reports whether err says the resource does not exist, before or
// after classification.
func isNotFound(err error) bool {
	if vendors.IsClass(err, vendors.NotFound) {
		return true
	}
	se, ok := serviceError(err)
	return ok && se.GetHTTPStatusCode() == http.StatusNotFound
}
