/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are EC2's).
const (
	codeTimeout          = "CallTimeout"
	codeCanceled         = "CallCanceled"
	codeTransport        = "TransportError"
	codeServerError      = "ServerError"
	codePlacementFull    = "PlacementGroupFull"
	codeNoSubnet         = "NoSubnetInZone"
	codeImageNotFound    = "ImageNotFound"
	codeImageAmbiguous   = "ImageAmbiguous"
	codeUserDataTooLarge = "UserDataTooLarge"
	codeInvalidSettings  = "InvalidSettings"
	codeSecurityGroupVPC = "SecurityGroupNotInVPC"
)

// errorClasses maps the EC2 and Service Quotas error codes to the driver's
// error classes (overview section 11.5). It is the single table: classify reads
// it and a test covers every entry. The classes of the first five groups were
// cross-checked against the AWS Karpenter provider's pkg/errors/errors.go and
// sc-inspector's markers (inspector/lib.py: RETRYABLE_START_ERROR_MARKERS and
// REGION_FATAL_ERROR_MARKERS) at the commits the plan pins; the differences are
// listed in docs/reference/aws-driver.md.
var errorClasses = map[string]vendors.Class{
	// Stock: the zone has no capacity for the type right now.
	"InsufficientInstanceCapacity":         vendors.InsufficientCapacity,
	"InsufficientHostCapacity":             vendors.InsufficientCapacity,
	"InsufficientReservedInstanceCapacity": vendors.InsufficientCapacity,
	"InsufficientCapacity":                 vendors.InsufficientCapacity,
	"SpotMaxPriceTooLow":                   vendors.InsufficientCapacity,
	"UnfulfillableCapacity":                vendors.InsufficientCapacity,
	"ReservationCapacityExceeded":          vendors.InsufficientCapacity,
	"InsufficientAddressCapacity":          vendors.InsufficientCapacity,
	"InsufficientVolumeCapacity":           vendors.InsufficientCapacity,
	// The type or the configuration does not exist where it was asked for.
	"Unsupported": vendors.Unsupported,
	// Region-wide limits. The vCPU and spot limits are the quota the tracker
	// reads; the resource limits are the ones sc-inspector treats as blocking
	// every instance type of the region.
	"VcpuLimitExceeded":                  vendors.QuotaExceeded,
	"MaxSpotInstanceCountExceeded":       vendors.QuotaExceeded,
	"InstanceLimitExceeded":              vendors.QuotaExceeded,
	"PendingVerification":                vendors.QuotaExceeded,
	"VolumeLimitExceeded":                vendors.QuotaExceeded,
	"MaxIOPSLimitExceeded":               vendors.QuotaExceeded,
	"VpcLimitExceeded":                   vendors.QuotaExceeded,
	"SubnetLimitExceeded":                vendors.QuotaExceeded,
	"InternetGatewayLimitExceeded":       vendors.QuotaExceeded,
	"RouteTableLimitExceeded":            vendors.QuotaExceeded,
	"SecurityGroupLimitExceeded":         vendors.QuotaExceeded,
	"RulesPerSecurityGroupLimitExceeded": vendors.QuotaExceeded,
	"PlacementGroupLimitExceeded":        vendors.QuotaExceeded,
	"AddressLimitExceeded":               vendors.QuotaExceeded,
	// The zone cannot host any instance.
	"InsufficientFreeAddressesInSubnet": vendors.ZoneUnusable,
	// Transient: throttling, server errors, and dependencies that clear when the
	// instances using a resource are gone.
	"RequestLimitExceeded":        vendors.Retryable,
	"Throttling":                  vendors.Retryable,
	"ThrottlingException":         vendors.Retryable,
	"ThrottledException":          vendors.Retryable,
	"EC2ThrottledException":       vendors.Retryable,
	"RequestThrottled":            vendors.Retryable,
	"RequestThrottledException":   vendors.Retryable,
	"TooManyRequestsException":    vendors.Retryable,
	"PriorRequestNotComplete":     vendors.Retryable,
	"InternalError":               vendors.Retryable,
	"InternalFailure":             vendors.Retryable,
	"ServiceUnavailable":          vendors.Retryable,
	"Unavailable":                 vendors.Retryable,
	"RequestTimeout":              vendors.Retryable,
	"RequestExpired":              vendors.Retryable,
	"DependencyViolation":         vendors.Retryable,
	"InvalidPlacementGroup.InUse": vendors.Retryable,
	"InvalidGroup.InUse":          vendors.Retryable,
	"IncorrectState":              vendors.Retryable,
	"IncorrectInstanceState":      vendors.Retryable,
	// Things that do not exist.
	"InvalidInstanceID.NotFound":        vendors.NotFound,
	"InvalidInstanceID.Malformed":       vendors.NotFound,
	"InvalidGroup.NotFound":             vendors.NotFound,
	"InvalidGroupId.Malformed":          vendors.NotFound,
	"InvalidVpcID.NotFound":             vendors.NotFound,
	"InvalidSubnetID.NotFound":          vendors.NotFound,
	"InvalidInternetGatewayID.NotFound": vendors.NotFound,
	"InvalidRouteTableID.NotFound":      vendors.NotFound,
	"InvalidAssociationID.NotFound":     vendors.NotFound,
	"InvalidPlacementGroup.Unknown":     vendors.NotFound,
	"Gateway.NotAttached":               vendors.NotFound,
	"NoSuchResourceException":           vendors.NotFound,
}

// Codes classified by what the message says, which EC2 documents only as text:
// InvalidParameterCombination is Unsupported only when it names the instance
// type and the zone of the call (the type does not exist there).
const codeInvalidParameterCombination = "InvalidParameterCombination"

// errContext is what the classification of one call knows about it.
type errContext struct {
	instanceType, zone string
}

// classify wraps an error of an EC2 or Service Quotas call into the driver's
// error classes. Errors that are already classified pass through unchanged.
// Everything not listed is Fatal (UnauthorizedOperation, InvalidAMIID.*,
// InvalidParameterValue, AuthFailure.ServiceLinkedRoleCreationNotPermitted ...).
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
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		code := apiErr.ErrorCode()
		if code == codeInvalidParameterCombination {
			if ec.instanceType != "" && ec.zone != "" &&
				strings.Contains(apiErr.ErrorMessage(), ec.instanceType) && strings.Contains(apiErr.ErrorMessage(), ec.zone) {
				return vendors.NewError(vendors.Unsupported, code, err)
			}
			return vendors.NewError(vendors.Fatal, code, err)
		}
		if class, ok := errorClasses[code]; ok {
			return vendors.NewError(class, code, err)
		}
		if re, ok := errors.AsType[*awshttp.ResponseError](err); ok && re.HTTPStatusCode() >= 500 {
			return vendors.NewError(vendors.Retryable, codeServerError, err)
		}
		return vendors.NewError(vendors.Fatal, code, err)
	}
	if re, ok := errors.AsType[*awshttp.ResponseError](err); ok && re.HTTPStatusCode() >= 500 {
		return vendors.NewError(vendors.Retryable, codeServerError, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, codeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

// apiCode returns the EC2 error code of err, "" when it has none.
func apiCode(err error) string {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return apiErr.ErrorCode()
	}
	return ""
}

// isCode reports whether err carries the EC2 error code (also when classified).
func isCode(err error, codes ...string) bool {
	c := apiCode(err)
	if c == "" {
		c = vendors.CodeOf(err)
	}
	return slices.Contains(codes, c)
}
