/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are ARM's).
const (
	codeTimeout          = "CallTimeout"
	codeCanceled         = "CallCanceled"
	codeTransport        = "TransportError"
	codeServerError      = "ServerError"
	codeImageNotFound    = "ImageNotFound"
	codeUserDataTooLarge = "UserDataTooLarge"
	codeInvalidSettings  = "InvalidSettings"
	codeNameConflict     = "NameConflict"
	codeNotOwned         = "NotOwned"
	// CodeMarketplaceTermsNotAccepted is the reason of ImagesReady=False when
	// the resolved image has terms the subscription has not accepted.
	CodeMarketplaceTermsNotAccepted = "MarketplaceTermsNotAccepted"
)

// Codes classified by more than the code (see classify).
const (
	codeSkuNotAvailable      = "SkuNotAvailable"
	codeOperationNotAllowed  = "OperationNotAllowed"
	codeInvalidParameter     = "InvalidParameter"
	codeAuthorizationFailed  = "AuthorizationFailed"
	codeResourceNotFound     = "ResourceNotFound"
	codePlatformImageMissing = "PlatformImageNotFound"
)

// errorClasses maps ARM error codes to the driver's error classes (plan 17
// section 5). It is the single table of the codes whose class depends on
// nothing else: classify reads it and a test covers every entry against an
// independent copy.
var errorClasses = map[string]vendors.Class{
	// Capacity: allocation failed inside the long-running operation.
	"AllocationFailed":                      vendors.InsufficientCapacity,
	"ZonalAllocationFailed":                 vendors.InsufficientCapacity,
	"OverconstrainedAllocationRequest":      vendors.InsufficientCapacity,
	"OverconstrainedZonalAllocationRequest": vendors.InsufficientCapacity,
	// The size is not offered, or cannot be spot.
	"AzureSpotIsNotSupportedForThisVMSize": vendors.Unsupported,
	// A zone the subscription cannot use for the size (the code is unverified:
	// the live verification list of plan 17); subnets are regional, so there is
	// no NoSubnetInZone.
	"ZoneNotAvailableForSubscription": vendors.ZoneUnusable,
	// Quotas.
	"QuotaExceeded":             vendors.QuotaExceeded,
	"PublicIPCountLimitReached": vendors.QuotaExceeded,
	// Transient.
	"TooManyRequests":                          vendors.Retryable,
	"InternalServerError":                      vendors.Retryable,
	"ServerBusy":                               vendors.Retryable,
	"RetryableError":                           vendors.Retryable,
	"ReferencedResourceNotProvisioned":         vendors.Retryable,
	"AnotherOperationInProgress":               vendors.Retryable,
	"OSProvisioningTimedOut":                   vendors.Retryable,
	"InUseSubnetCannotBeDeleted":               vendors.Retryable,
	"NetworkSecurityGroupInUse":                vendors.Retryable,
	"PublicIPAddressCannotBeDeleted":           vendors.Retryable,
	"InUseNetworkSecurityGroupCannotBeDeleted": vendors.Retryable,
	"ScopeLocked":                              vendors.Retryable,
	"ApplicationSecurityGroupInUse":            vendors.Retryable,
	"ProximityPlacementGroupInUse":             vendors.Retryable,
	// Things that do not exist.
	"ResourceNotFound":       vendors.NotFound,
	"ResourceGroupNotFound":  vendors.NotFound,
	"VMNotFound":             vendors.NotFound,
	codePlatformImageMissing: vendors.NotFound,
}

// regionUnavailableCodes are the codes of the calls that read a region's facts
// that say the subscription cannot use the region.
var regionUnavailableCodes = []string{
	"LocationNotAvailableForResourceType", "InvalidResourceLocation", "NotAvailableForSubscription",
}

// errContext is what the classification of one call knows about it.
type errContext struct {
	// size is the VM size of a launch, spot says its priority.
	size string
	spot bool
}

// responseError returns the ARM error of err.
func responseError(err error) (*azcore.ResponseError, bool) {
	return errors.AsType[*azcore.ResponseError](err)
}

// classify wraps an error of an ARM call into the driver's error classes. The
// code decides, never the message text, with the exceptions the plan names:
// OperationNotAllowed (quota and "operation in progress" are only texts) and
// InvalidParameter naming an unavailable size. Errors that are already
// classified pass through. Everything not listed is Fatal.
func classify(err error, ec errContext) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return vendors.NewError(vendors.Retryable, codeTimeout, err)
	case errors.Is(err, context.Canceled):
		return vendors.NewError(vendors.Retryable, codeCanceled, err)
	}
	if re, ok := responseError(err); ok {
		code := re.ErrorCode
		msg := strings.ToLower(re.Error())
		if re.StatusCode == http.StatusTooManyRequests {
			// Throttling, whatever the code: the compute provider answers
			// 429 with OperationNotAllowed ("too many requests"), ARM with
			// SubscriptionRequestsThrottled. azcore has already retried.
			return vendors.NewError(vendors.Retryable, code, err)
		}
		switch code {
		case codeSkuNotAvailable:
			if ec.spot {
				return vendors.NewError(vendors.InsufficientCapacity, code, err)
			}
			return vendors.NewError(vendors.Unsupported, code, err)
		case codeOperationNotAllowed:
			switch {
			case strings.Contains(msg, "exceeding approved") && strings.Contains(msg, "quota"):
				return vendors.NewError(vendors.QuotaExceeded, code, err)
			case strings.Contains(msg, "another operation") && strings.Contains(msg, "progress"):
				return vendors.NewError(vendors.Retryable, code, err)
			}
			return vendors.NewError(vendors.Fatal, code, err)
		case codeInvalidParameter:
			if ec.size != "" && strings.Contains(msg, strings.ToLower(ec.size)) &&
				(strings.Contains(msg, "not available") || strings.Contains(msg, "not supported")) {
				return vendors.NewError(vendors.Unsupported, code, err)
			}
			return vendors.NewError(vendors.Fatal, code, err)
		}
		if class, ok := errorClasses[code]; ok {
			return vendors.NewError(class, code, err)
		}
		return vendors.NewError(classOfStatus(re.StatusCode), code, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, codeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

// classOfStatus classifies an error without a (known) code by its HTTP status:
// 404 is NotFound, 429 and 5xx are Retryable, the rest Fatal.
func classOfStatus(status int) vendors.Class {
	switch {
	case status == http.StatusNotFound:
		return vendors.NotFound
	case status == http.StatusTooManyRequests, status >= 500:
		return vendors.Retryable
	}
	return vendors.Fatal
}

// classifyRegionRead classifies the error of a call that reads the facts of a
// region (the SKU read, the locations): a region the subscription cannot use is
// RegionUnavailable, so that the facts skip it. AuthorizationFailed stays
// Fatal on launches but is a denied region on a read.
func classifyRegionRead(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*vendors.Error](err); !ok {
		if re, ok := responseError(err); ok &&
			(slices.Contains(regionUnavailableCodes, re.ErrorCode) || re.ErrorCode == codeAuthorizationFailed) {
			return vendors.NewError(vendors.RegionUnavailable, re.ErrorCode, err)
		}
	}
	return classify(err, errContext{})
}

// isNotFound reports whether err is an ARM 404 (or classified NotFound).
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if re, ok := responseError(err); ok {
		return re.StatusCode == http.StatusNotFound || classOfCode(re.ErrorCode) == vendors.NotFound
	}
	return vendors.IsClass(err, vendors.NotFound)
}

func classOfCode(code string) vendors.Class { return errorClasses[code] }

// invalidf is a Fatal error for settings or a request that cannot be used.
func invalidf(format string, args ...any) error {
	return vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(format, args...))
}
