/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Error codes the driver itself raises (the vendor codes are ECS's).
const (
	codeTimeout          = "CallTimeout"
	codeCanceled         = "CallCanceled"
	codeTransport        = "TransportError"
	codeServerError      = "ServerError"
	codeThrottled        = "Throttling"
	codePlacementFull    = "PlacementGroupFull"
	codeNoVSwitch        = "NoVSwitchInZone"
	codeImageNotFound    = "ImageNotFound"
	codeImageAmbiguous   = "ImageAmbiguous"
	codeUserDataTooLarge = "UserDataTooLarge"
	codeInvalidSettings  = "InvalidSettings"
	codeGroupVPCMismatch = "SecurityGroupNotInVPC"
	codeInstanceVanished = "InstanceVanished"
	// CodeIdempotentMismatch is ECS's answer to a reused ClientToken with other
	// parameters. It is never classified: the create path resolves it.
	codeIdempotentMismatch = "IdempotentParameterMismatch"
)

// errorClasses maps ECS and VPC error codes to the driver's error classes
// (overview section 11.5, plan 21 section 6). It is the single table: classify
// reads it and a test covers every entry against a second copy. Each entry is
// marked in plan 21 as seen in the RunInstances error table (V) or as
// unverified (U); the U ones are marked here too.
//
//nolint:dupl // the independent copy in errors_test.go is the point
var errorClasses = map[string]vendors.Class{
	// Stock: the zone has no capacity for the type right now.
	"OperationDenied.NoStock":                    vendors.InsufficientCapacity,
	"Zone.NotOnSale":                             vendors.InsufficientCapacity,
	"InvalidOperation.PublicIpAddressNoStock":    vendors.InsufficientCapacity,
	"QuotaExceed.DeploymentSetInstanceQuotaFull": vendors.InsufficientCapacity, // as PlacementGroupFull
	"LackResource":                               vendors.InsufficientCapacity,
	// The type or the configuration does not exist where it was asked for.
	"InvalidInstanceType.ZoneNotSupported":       vendors.Unsupported,
	"InstanceType.Offline":                       vendors.Unsupported,
	"InvalidInstanceType.ValueNotSupported":      vendors.Unsupported,
	"ImageNotSupportInstanceType":                vendors.Unsupported,
	"InvalidDiskCategory.NotSupported":           vendors.Unsupported,
	"CategoryNotSupported":                       vendors.Unsupported,
	"InvalidInstanceType.NotSupportDiskCategory": vendors.Unsupported,
	"OperationDenied.CloudSSDNotSupported":       vendors.Unsupported,
	"OperationDenied.ZoneSystemCategoryNotMatch": vendors.Unsupported,
	"InvalidAccount.NotSupportSpot":              vendors.Unsupported,
	// Region-wide limits (none of the codes is known to be account-wide: the
	// vCPU quotas and the instance limits are per region).
	"QuotaExceed.ElasticQuota":               vendors.QuotaExceeded,
	"QuotaExceed.AfterpayInstance":           vendors.QuotaExceeded,
	"QuotaExceeded":                          vendors.QuotaExceeded, // instances in the VPC
	"QuotaExceed.PostPaidDisk":               vendors.QuotaExceeded,
	"QuotaExceeded.PostpaidDataDiskCapacity": vendors.QuotaExceeded,
	"QuotaExceeded.InternetBandwidth":        vendors.QuotaExceeded,
	"InstanceDiskNumLimitExceed":             vendors.QuotaExceeded, // U
	"InstanceDiskLimitExceeded":              vendors.QuotaExceeded,
	// The zone cannot host any instance.
	"QuotaExceeded.PrivateIpAddress":   vendors.ZoneUnusable, // the vSwitch is full
	"OperationDenied.ZoneNotAllowed":   vendors.ZoneUnusable,
	"Zone.NotOpen":                     vendors.ZoneUnusable,
	"SecurityGroupInstanceLimitExceed": vendors.ZoneUnusable, // the group is full
	// Transient.
	"Throttling.Resource":     vendors.Retryable,
	"Throttling":              vendors.Retryable, // U
	"Throttling.User":         vendors.Retryable, // U
	"Throttling.Api":          vendors.Retryable, // U
	"ServiceUnavailable":      vendors.Retryable,
	"InternalError":           vendors.Retryable,
	"IdempotentProcessing":    vendors.Retryable, // U
	"IncorrectInstanceStatus": vendors.Retryable, // U, on delete
	// Things that do not exist (get and delete).
	"InvalidInstanceId.NotFound": vendors.NotFound, // U
}

// Codes of the Fatal group the plan names, so that a test pins them: they are
// deliberately not in errorClasses (everything else is Fatal).
var fatalCodes = []string{
	"InvalidImageId.NotFound", "OperationDenied.ImageNotValid", "InvalidVSwitchId.NotFound",
	"InvalidSecurityGroup.VpcMismatch", "OperationDenied.InconsistentNetwork", "InvalidUserData.Forbidden",
	"Account.Arrearage", "InsufficientBalance.AgentCredit", "Forbidden.RAM",
	// More than 20 tags: a configuration error, not a limit that frees up. The
	// plan lists QuotaExceed.Tags as QuotaExceeded, which would stop every
	// offering of the region for a hand-made mistake (validation refuses it
	// before the call anyway).
	"QuotaExceed.Tags", "NumberExceed.Tags",
}

// regionDeniedCodes are the codes that say the credentials cannot use a region
// (classifyRegionRead turns them into RegionUnavailable).
var regionDeniedCodes = []string{
	"RegionUnauthorized",
	"InvalidRegion.NotSupport",    // U
	"RealNameAuthenticationError", // U
}

// dependencyPrefix starts the codes of a delete that fails because an
// instance still uses the resource (the codes are unverified, plan 21
// section 7): the dependency clears when the instances are gone.
const dependencyPrefix = "DependencyViolation"

// errContext is what the classification of one call knows about it.
type errContext struct {
	instanceType, zone string
	// regionRead marks a call that reads the facts of a region (DescribeZones):
	// the codes of regionDeniedCodes are then RegionUnavailable.
	regionRead bool
}

// sdkError returns the code and the HTTP status of an SDK error.
// The generated SDKs return *dara.SDKError; *tea.SDKError (the earlier
// generation, still returned by credentials and some utilities) is read too.
func sdkError(err error) (code string, status int, ok bool) {
	if e, found := errors.AsType[*dara.SDKError](err); found {
		return deref(e.Code), derefInt(e.StatusCode), true
	}
	if e, found := errors.AsType[*tea.SDKError](err); found {
		return deref(e.Code), derefInt(e.StatusCode), true
	}
	return "", 0, false
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func derefInt(p *int) int { return deref(p) }

// classify wraps an error of an ECS or VPC call into the driver's error
// classes. Errors that are already classified pass through unchanged.
// Everything not listed is Fatal. A throttling code that is not in the table
// (Throttling.*) and any HTTP 429 or 5xx is Retryable.
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
	if code, status, ok := sdkError(err); ok {
		if ec.regionRead && slices.Contains(regionDeniedCodes, code) {
			return vendors.NewError(vendors.RegionUnavailable, code, err)
		}
		if class, ok := errorClasses[code]; ok {
			return vendors.NewError(class, code, err)
		}
		switch {
		case strings.HasPrefix(code, "Throttling"), status == 429:
			return vendors.NewError(vendors.Retryable, firstNonEmpty(code, codeThrottled), err)
		case strings.HasPrefix(code, dependencyPrefix):
			return vendors.NewError(vendors.Retryable, code, err)
		case status >= 500:
			return vendors.NewError(vendors.Retryable, firstNonEmpty(code, codeServerError), err)
		}
		return vendors.NewError(vendors.Fatal, code, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return vendors.NewError(vendors.Retryable, codeTransport, err)
	}
	return vendors.NewError(vendors.Fatal, "", err)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiCode returns the ECS error code of err, "" when it has none.
func apiCode(err error) string {
	code, _, _ := sdkError(err)
	return code
}

// isCode reports whether err carries one of the ECS error codes (also when
// classified).
func isCode(err error, codes ...string) bool {
	c := apiCode(err)
	if c == "" {
		c = vendors.CodeOf(err)
	}
	return slices.Contains(codes, c)
}
