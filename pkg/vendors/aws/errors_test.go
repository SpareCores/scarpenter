/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func apiErr(code, msg string) error {
	return &smithy.OperationError{ServiceID: "EC2", OperationName: "RunInstances",
		Err: &smithy.GenericAPIError{Code: code, Message: msg}}
}

// wantClasses is the table of overview section 11.5 and of the extra codes the
// driver knows, written out independently of the driver's table. Every code of
// the driver's table must be listed here, so a new code needs a decision in two
// places.
var wantClasses = map[string]vendors.Class{
	// 11.5: InsufficientCapacity
	"InsufficientInstanceCapacity":         vendors.InsufficientCapacity,
	"InsufficientHostCapacity":             vendors.InsufficientCapacity,
	"InsufficientReservedInstanceCapacity": vendors.InsufficientCapacity,
	"InsufficientCapacity":                 vendors.InsufficientCapacity,
	"SpotMaxPriceTooLow":                   vendors.InsufficientCapacity,
	// AWS provider's unfulfillable capacity codes that are stock, not quota
	"UnfulfillableCapacity":       vendors.InsufficientCapacity,
	"ReservationCapacityExceeded": vendors.InsufficientCapacity,
	"InsufficientAddressCapacity": vendors.InsufficientCapacity,
	"InsufficientVolumeCapacity":  vendors.InsufficientCapacity,
	// 11.5: Unsupported (InvalidParameterCombination is tested separately)
	"Unsupported": vendors.Unsupported,
	// 11.5: QuotaExceeded
	"VcpuLimitExceeded":            vendors.QuotaExceeded,
	"MaxSpotInstanceCountExceeded": vendors.QuotaExceeded,
	"InstanceLimitExceeded":        vendors.QuotaExceeded,
	"PendingVerification":          vendors.QuotaExceeded,
	// sc-inspector REGION_FATAL_ERROR_MARKERS and the limits of the other calls
	"VpcLimitExceeded":                   vendors.QuotaExceeded,
	"SubnetLimitExceeded":                vendors.QuotaExceeded,
	"InternetGatewayLimitExceeded":       vendors.QuotaExceeded,
	"RouteTableLimitExceeded":            vendors.QuotaExceeded,
	"SecurityGroupLimitExceeded":         vendors.QuotaExceeded,
	"RulesPerSecurityGroupLimitExceeded": vendors.QuotaExceeded,
	"PlacementGroupLimitExceeded":        vendors.QuotaExceeded,
	"AddressLimitExceeded":               vendors.QuotaExceeded,
	"VolumeLimitExceeded":                vendors.QuotaExceeded,
	"MaxIOPSLimitExceeded":               vendors.QuotaExceeded,
	// 11.5: ZoneUnusable
	"InsufficientFreeAddressesInSubnet": vendors.ZoneUnusable,
	// 11.5: Retryable (throttling and server errors), and dependencies
	"RequestLimitExceeded":        vendors.Retryable,
	"InternalError":               vendors.Retryable,
	"ServiceUnavailable":          vendors.Retryable,
	"Unavailable":                 vendors.Retryable,
	"Throttling":                  vendors.Retryable,
	"ThrottlingException":         vendors.Retryable,
	"ThrottledException":          vendors.Retryable,
	"EC2ThrottledException":       vendors.Retryable,
	"RequestThrottled":            vendors.Retryable,
	"RequestThrottledException":   vendors.Retryable,
	"TooManyRequestsException":    vendors.Retryable,
	"PriorRequestNotComplete":     vendors.Retryable,
	"InternalFailure":             vendors.Retryable,
	"RequestTimeout":              vendors.Retryable,
	"RequestExpired":              vendors.Retryable,
	"DependencyViolation":         vendors.Retryable,
	"InvalidPlacementGroup.InUse": vendors.Retryable,
	"InvalidGroup.InUse":          vendors.Retryable,
	"IncorrectState":              vendors.Retryable,
	"IncorrectInstanceState":      vendors.Retryable,
	// 11.5: NotFound
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

func TestEveryCodeIsClassified(t *testing.T) {
	for code, want := range wantClasses {
		t.Run(code, func(t *testing.T) {
			err := classify(apiErr(code, "message"), errContext{})
			if got := vendors.ClassOf(err); got != want {
				t.Errorf("%s: class %q, want %q", code, got, want)
			}
			if vendors.CodeOf(err) != code {
				t.Errorf("the code %q is lost: %q", code, vendors.CodeOf(err))
			}
		})
	}
	for code, class := range errorClasses {
		if want, ok := wantClasses[code]; !ok || want != class {
			t.Errorf("the driver classifies %s as %s, which the test table lacks or disagrees with (%v)", code, class, want)
		}
	}
}

func TestFatalCodes(t *testing.T) {
	// Everything else is Fatal, among them the codes of section 11.5's last row.
	for _, code := range []string{
		"UnauthorizedOperation", "InvalidAMIID.NotFound", "InvalidAMIID.Malformed", "InvalidParameterValue",
		"AuthFailure", "AuthFailure.ServiceLinkedRoleCreationNotPermitted", "OptInRequired", "InvalidUserData.Malformed",
		"InvalidKeyPair.NotFound", "SomethingNew",
	} {
		if got := vendors.ClassOf(classify(apiErr(code, "message"), errContext{})); got != vendors.Fatal {
			t.Errorf("%s: class %q, want Fatal", code, got)
		}
	}
}

func TestInvalidParameterCombination(t *testing.T) {
	ec := errContext{instanceType: "c7g.large", zone: "us-east-1e"}
	for name, tc := range map[string]struct {
		msg  string
		ctx  errContext
		want vendors.Class
	}{
		"names the type and the zone": {
			"Your requested instance type (c7g.large) is not supported in your requested Availability Zone (us-east-1e).", ec, vendors.Unsupported},
		"names the type only":     {"The instance type c7g.large cannot be launched", ec, vendors.Fatal},
		"names neither":           {"The architecture of the AMI does not match", ec, vendors.Fatal},
		"call without type, zone": {"c7g.large us-east-1e", errContext{}, vendors.Fatal},
	} {
		t.Run(name, func(t *testing.T) {
			err := classify(apiErr("InvalidParameterCombination", tc.msg), tc.ctx)
			if got := vendors.ClassOf(err); got != tc.want {
				t.Errorf("class %q, want %q", got, tc.want)
			}
		})
	}
}

type timeoutNet struct{}

func (timeoutNet) Error() string   { return "i/o timeout" }
func (timeoutNet) Timeout() bool   { return true }
func (timeoutNet) Temporary() bool { return true }

func TestTransportErrors(t *testing.T) {
	resp := func(code int) error {
		return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}}, Err: errors.New("boom")}}
	}
	for name, tc := range map[string]struct {
		err  error
		want vendors.Class
		code string
	}{
		"deadline":          {fmt.Errorf("calling: %w", context.DeadlineExceeded), vendors.Retryable, codeTimeout},
		"canceled":          {context.Canceled, vendors.Retryable, codeCanceled},
		"http 503":          {resp(503), vendors.Retryable, codeServerError},
		"http 400":          {resp(400), vendors.Fatal, ""},
		"network":           {&net.OpError{Op: "dial", Err: timeoutNet{}}, vendors.Retryable, codeTransport},
		"unknown":           {errors.New("failed to retrieve credentials"), vendors.Fatal, ""},
		"api error wrapped": {fmt.Errorf("x: %w", apiErr("VcpuLimitExceeded", "m")), vendors.QuotaExceeded, "VcpuLimitExceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			err := classify(tc.err, errContext{})
			if got := vendors.ClassOf(err); got != tc.want {
				t.Errorf("class %q, want %q (%v)", got, tc.want, err)
			}
			if tc.code != "" && vendors.CodeOf(err) != tc.code {
				t.Errorf("code %q, want %q", vendors.CodeOf(err), tc.code)
			}
		})
	}
}

func TestClassifiedErrorsPassThrough(t *testing.T) {
	orig := vendors.NewError(vendors.ZoneUnusable, codeNoSubnet, errors.New("no subnet"))
	if got := classify(fmt.Errorf("wrapped: %w", orig), errContext{}); vendors.ClassOf(got) != vendors.ZoneUnusable || vendors.CodeOf(got) != codeNoSubnet {
		t.Errorf("a classified error changed: %v", got)
	}
	if classify(nil, errContext{}) != nil {
		t.Error("nil is not nil")
	}
}

func TestResourceLimitCodesMatchSCInspectorMarkers(t *testing.T) {
	// sc-inspector's REGION_FATAL_ERROR_MARKERS (inspector/lib.py at the commit
	// the plan pins): errors that block every instance type of the region.
	for _, code := range []string{
		"VpcLimitExceeded", "SubnetLimitExceeded", "InternetGatewayLimitExceeded", "RouteTableLimitExceeded",
		"SecurityGroupLimitExceeded", "AddressLimitExceeded",
	} {
		if errorClasses[code] != vendors.QuotaExceeded {
			t.Errorf("%s is a region-wide limit sc-inspector gives up the region for; the driver says %q", code, errorClasses[code])
		}
	}
	// The AWS provider's unfulfillable capacity codes, split by what they mean
	// here: stock codes are InsufficientCapacity, limits are QuotaExceeded.
	for code, want := range map[string]vendors.Class{
		"InsufficientInstanceCapacity": vendors.InsufficientCapacity, "MaxSpotInstanceCountExceeded": vendors.QuotaExceeded,
		"VcpuLimitExceeded": vendors.QuotaExceeded, "UnfulfillableCapacity": vendors.InsufficientCapacity,
		"Unsupported": vendors.Unsupported, "InsufficientFreeAddressesInSubnet": vendors.ZoneUnusable,
		"SpotMaxPriceTooLow": vendors.InsufficientCapacity, "ReservationCapacityExceeded": vendors.InsufficientCapacity,
	} {
		if errorClasses[code] != want {
			t.Errorf("%s: %q, want %q", code, errorClasses[code], want)
		}
	}
}
