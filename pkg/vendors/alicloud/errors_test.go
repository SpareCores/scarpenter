/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func sdkErr(code string, status int) error {
	return &dara.SDKError{Code: new(code), StatusCode: new(status), Message: new("message of " + code)}
}

// wantClasses is the table of plan 21 section 6, written out independently of
// the driver's table. Every code of the driver's table must be listed here, so
// a new code needs a decision in two places.
//
//nolint:dupl // an independent copy of the driver's table, on purpose
var wantClasses = map[string]vendors.Class{
	"OperationDenied.NoStock":                    vendors.InsufficientCapacity,
	"Zone.NotOnSale":                             vendors.InsufficientCapacity,
	"InvalidOperation.PublicIpAddressNoStock":    vendors.InsufficientCapacity,
	"QuotaExceed.DeploymentSetInstanceQuotaFull": vendors.InsufficientCapacity,
	"LackResource":                               vendors.InsufficientCapacity,

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

	"QuotaExceed.ElasticQuota":               vendors.QuotaExceeded,
	"QuotaExceed.AfterpayInstance":           vendors.QuotaExceeded,
	"QuotaExceeded":                          vendors.QuotaExceeded,
	"QuotaExceed.PostPaidDisk":               vendors.QuotaExceeded,
	"QuotaExceeded.PostpaidDataDiskCapacity": vendors.QuotaExceeded,
	"QuotaExceeded.InternetBandwidth":        vendors.QuotaExceeded,
	"InstanceDiskNumLimitExceed":             vendors.QuotaExceeded,
	"InstanceDiskLimitExceeded":              vendors.QuotaExceeded,
	"QuotaExceed.Tags":                       vendors.QuotaExceeded,

	"QuotaExceeded.PrivateIpAddress":   vendors.ZoneUnusable,
	"OperationDenied.ZoneNotAllowed":   vendors.ZoneUnusable,
	"Zone.NotOpen":                     vendors.ZoneUnusable,
	"SecurityGroupInstanceLimitExceed": vendors.ZoneUnusable,

	"Throttling.Resource":     vendors.Retryable,
	"Throttling":              vendors.Retryable,
	"Throttling.User":         vendors.Retryable,
	"Throttling.Api":          vendors.Retryable,
	"ServiceUnavailable":      vendors.Retryable,
	"InternalError":           vendors.Retryable,
	"IdempotentProcessing":    vendors.Retryable,
	"IncorrectInstanceStatus": vendors.Retryable,

	"InvalidInstanceId.NotFound": vendors.NotFound,
}

func TestErrorTableMatchesTheIndependentCopy(t *testing.T) {
	for code, class := range errorClasses {
		want, ok := wantClasses[code]
		if !ok {
			t.Errorf("%s is in the driver's table but not in the independent copy", code)
		} else if want != class {
			t.Errorf("%s: driver says %s, the copy says %s", code, class, want)
		}
	}
	for code := range wantClasses {
		if _, ok := errorClasses[code]; !ok {
			t.Errorf("%s is in the independent copy but not in the driver's table", code)
		}
	}
}

func TestClassifyEveryCode(t *testing.T) {
	for code, want := range wantClasses {
		t.Run(code, func(t *testing.T) {
			err := classify(sdkErr(code, 400), errContext{})
			if got := vendors.ClassOf(err); got != want {
				t.Fatalf("class = %s, want %s", got, want)
			}
			if got := vendors.CodeOf(err); got != code {
				t.Fatalf("code = %q, want %q", got, code)
			}
			if vendors.IsAccountWide(err) {
				t.Fatalf("no ECS code is known to be account-wide")
			}
		})
	}
}

func TestClassifyFatalCodes(t *testing.T) {
	for _, code := range fatalCodes {
		if _, listed := errorClasses[code]; listed {
			t.Errorf("%s is in the class table but the plan says Fatal", code)
		}
		if got := vendors.ClassOf(classify(sdkErr(code, 400), errContext{})); got != vendors.Fatal {
			t.Errorf("%s: class = %s, want Fatal", code, got)
		}
	}
	// An unknown code and an unclassifiable error are Fatal too.
	for _, err := range []error{sdkErr("SomethingNew", 400), errors.New("boom"), sdkErr("", 0)} {
		if got := vendors.ClassOf(classify(err, errContext{})); got != vendors.Fatal {
			t.Errorf("%v: class = %s, want Fatal", err, got)
		}
	}
}

func TestIdempotentMismatchIsNeverClassified(t *testing.T) {
	if _, ok := errorClasses[codeIdempotentMismatch]; ok {
		t.Fatal("IdempotentParameterMismatch is resolved by the create path, never classified")
	}
	err := classify(sdkErr(codeIdempotentMismatch, 400), errContext{})
	if vendors.ClassOf(err) != vendors.Fatal || !isCode(err, codeIdempotentMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestClassifyTransientShapes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want vendors.Class
		code string
	}{
		"throttling variant":       {sdkErr("Throttling.Something", 400), vendors.Retryable, "Throttling.Something"},
		"429 without a known code": {sdkErr("SomethingElse", 429), vendors.Retryable, "SomethingElse"},
		"429 without a code":       {sdkErr("", 429), vendors.Retryable, codeThrottled},
		"500":                      {sdkErr("SystemBusy", 500), vendors.Retryable, "SystemBusy"},
		"502 without a code":       {sdkErr("", 502), vendors.Retryable, codeServerError},
		"dependency":               {sdkErr("DependencyViolation.Instance", 400), vendors.Retryable, "DependencyViolation.Instance"},
		"deadline":                 {fmt.Errorf("call: %w", context.DeadlineExceeded), vendors.Retryable, codeTimeout},
		"canceled":                 {context.Canceled, vendors.Retryable, codeCanceled},
		"net":                      {&net.DNSError{Err: "no such host", IsTemporary: true}, vendors.Retryable, codeTransport},
		"tea error":                {tea.NewSDKError(map[string]any{"code": "OperationDenied.NoStock", "message": "x"}), vendors.InsufficientCapacity, "OperationDenied.NoStock"},
		"wrapped sdk error":        {fmt.Errorf("RunInstances: %w", sdkErr("QuotaExceed.ElasticQuota", 403)), vendors.QuotaExceeded, "QuotaExceed.ElasticQuota"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := classify(c.err, errContext{})
			if vendors.ClassOf(err) != c.want || vendors.CodeOf(err) != c.code {
				t.Fatalf("got %s (%s), want %s (%s)", vendors.ClassOf(err), vendors.CodeOf(err), c.want, c.code)
			}
		})
	}
	if classify(nil, errContext{}) != nil {
		t.Fatal("nil must stay nil")
	}
	already := vendors.NewError(vendors.ZoneUnusable, "x", errors.New("y"))
	if classify(already, errContext{}) != error(already) {
		t.Fatal("a classified error must pass through unchanged")
	}
}

func TestClassifyRegionRead(t *testing.T) {
	for _, code := range regionDeniedCodes {
		err := classifyRegion(sdkErr(code, 403))
		if vendors.ClassOf(err) != vendors.RegionUnavailable || vendors.CodeOf(err) != code {
			t.Errorf("%s: got %s (%s)", code, vendors.ClassOf(err), vendors.CodeOf(err))
		}
	}
	// A real throttling is still Retryable, and nothing is lost for the rest.
	if got := vendors.ClassOf(classifyRegion(sdkErr("Throttling.User", 400))); got != vendors.Retryable {
		t.Errorf("throttling: %s", got)
	}
	if got := vendors.ClassOf(classifyRegion(sdkErr("Forbidden.RAM", 403))); got != vendors.Fatal {
		t.Errorf("Forbidden.RAM: %s", got)
	}
	if classifyRegion(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func classifyRegion(err error) error { return classify(err, errContext{regionRead: true}) }
