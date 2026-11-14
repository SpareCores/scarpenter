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
	"testing"

	"github.com/oracle/oci-go-sdk/v65/common"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

// TestClassify pins every row of the error table of plan 25, section 7, with the
// texts of the message-based rows. Every row is UNVERIFIED live (spike S5): this
// test is the table's documentation and the place the first live run corrects.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   callKind
		status int
		code   string
		msg    string
		class  vendors.Class
	}{
		// InsufficientCapacity: only an HTTP 500 InternalError with the text.
		{"capacity", kindLaunch, 500, "InternalError", "Out of host capacity.", vendors.InsufficientCapacity},
		{"capacity, other case", kindLaunch, 500, "InternalError", "OUT OF HOST CAPACITY for shape x", vendors.InsufficientCapacity},
		{"capacity for shape", kindLaunch, 500, "InternalError", "Out of capacity for shape VM.Standard.A1.Flex", vendors.InsufficientCapacity},
		{"capacity text on a 503 is transient", kindLaunch, 503, "InternalError", "Out of host capacity.", vendors.Retryable},
		{"capacity text on a 400 is fatal", kindLaunch, 400, "InvalidParameter", "Out of host capacity.", vendors.Fatal},
		{"internal error without the text", kindLaunch, 500, "InternalError", "An internal error occurred.", vendors.Retryable},
		// QuotaExceeded.
		{"limit", kindLaunch, 400, "LimitExceeded",
			"The following service limits were exceeded: standard-e5-core-count", vendors.QuotaExceeded},
		{"compartment quota", kindLaunch, 400, "QuotaExceeded", "A compartment quota was exceeded: standard-e5-core-count",
			vendors.QuotaExceeded},
		// Unsupported.
		{"incompatible shape", kindLaunch, 400, "InvalidParameter",
			"Shape VM.Standard.A2.Flex is not compatible with image ocid1.image.oc1..x", vendors.Unsupported},
		{"shape not available", kindLaunch, 400, "InvalidParameter", "Shape VM.Standard1.4 is not available", vendors.Unsupported},
		{"shape not supported in the AD", kindLaunch, 400, "InvalidParameter",
			"The shape is not supported in the availability domain Xxxx:EU-FRANKFURT-1-AD-1", vendors.Unsupported},
		{"extended memory", kindLaunch, 400, "InvalidParameter", "Extended memory is not enabled for the tenancy",
			vendors.Unsupported},
		{"other invalid parameter", kindLaunch, 400, "InvalidParameter", "metadata is too large", vendors.Fatal},
		// ZoneUnusable.
		{"subnet full", kindLaunch, 400, "InvalidParameter", "The subnet ocid1.subnet.oc1..x has no available IP addresses",
			vendors.ZoneUnusable},
		{"subnet full, conflict", kindLaunch, 409, "Conflict", "subnet ocid1.subnet.oc1..x: not enough free IP addresses",
			vendors.ZoneUnusable},
		// Retryable.
		{"too many requests", kindRead, 429, "TooManyRequests", "slow down", vendors.Retryable},
		{"500", kindRead, 500, "InternalError", "oops", vendors.Retryable},
		{"502", kindRead, 502, "", "bad gateway", vendors.Retryable},
		{"503", kindLaunch, 503, "ServiceUnavailable", "unavailable", vendors.Retryable},
		{"504", kindRead, 504, "", "timeout", vendors.Retryable},
		{"incorrect state", kindRead, 409, "IncorrectState", "the resource is in use", vendors.Retryable},
		{"lock conflict", kindRead, 409, "LockConflict", "locked", vendors.Retryable},
		{"501 is not transient", kindRead, 501, "MethodNotImplemented", "no", vendors.Fatal},
		// NotFound by the call.
		{"not found on a get", kindRead, 404, "NotAuthorizedOrNotFound", "gone", vendors.NotFound},
		{"404 without a code", kindRead, 404, "", "gone", vendors.NotFound},
		{"not found on the launch is fatal", kindLaunch, 404, "NotAuthorizedOrNotFound", "no subnet", vendors.Fatal},
		{"any 404 on the launch is fatal", kindLaunch, 404, "NotFound", "no image", vendors.Fatal},
		// RegionUnavailable from the calls that read a region's facts.
		{"region not found", kindRegion, 404, "NotAuthorizedOrNotFound", "region", vendors.RegionUnavailable},
		{"region not authenticated", kindRegion, 401, "NotAuthenticated", "who", vendors.RegionUnavailable},
		{"region forbidden", kindRegion, 403, "NotAuthorized", "no", vendors.RegionUnavailable},
		// Fatal.
		{"not authenticated", kindRead, 401, "NotAuthenticated", "who", vendors.Fatal},
		{"forbidden", kindLaunch, 403, "NotAuthorized", "no", vendors.Fatal},
		{"conflict", kindLaunch, 409, "Conflict", "retry token", vendors.Fatal},
		{"unknown", kindRead, 418, "Teapot", "short and stout", vendors.Fatal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(fake.Error(tc.status, tc.code, tc.msg), tc.kind)
			if got := vendors.ClassOf(err); got != tc.class {
				t.Fatalf("classified %q (%v), want %q", got, err, tc.class)
			}
			if vendors.IsAccountWide(err) {
				t.Error("the quota error is account-wide; OCI's Compute limits are per AD or region")
			}
			if code := vendors.CodeOf(err); tc.code != "" && code != tc.code && vendors.ClassOf(err) != vendors.Fatal {
				t.Errorf("code %q, want %q", code, tc.code)
			}
		})
	}
}

func TestClassifyTransport(t *testing.T) {
	svc := fake.Error(http.StatusNotFound, "NotAuthorizedOrNotFound", "x")
	already := vendors.NewError(vendors.QuotaExceeded, "X", errors.New("already"))
	for _, tc := range []struct {
		name  string
		err   error
		class vendors.Class
		code  string
	}{
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), vendors.Retryable, codeTimeout},
		{"backoff past the deadline", common.DeadlineExceededByBackoff, vendors.Retryable, codeTimeout},
		{"canceled", context.Canceled, vendors.Retryable, codeCanceled},
		{"network", &net.OpError{Op: "dial", Err: errors.New("refused")}, vendors.Retryable, codeTransport},
		{"wrapped service error", fmt.Errorf("listing: %w", svc), vendors.NotFound, "NotAuthorizedOrNotFound"},
		{"already classified", fmt.Errorf("wrapped: %w", already), vendors.QuotaExceeded, "X"},
		{"plain", errors.New("boom"), vendors.Fatal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(tc.err, kindRead)
			if vendors.ClassOf(err) != tc.class || vendors.CodeOf(err) != tc.code {
				t.Errorf("classified %q %q (%v), want %q %q", vendors.ClassOf(err), vendors.CodeOf(err), err, tc.class, tc.code)
			}
		})
	}
	if classify(nil, kindRead) != nil {
		t.Error("nil is classified")
	}
}

func TestRetryTokenConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"conflict", fake.Error(409, "Conflict", "retry token"), true},
		{"conflict, another code", fake.Error(409, "RetryTokenInUse", "x"), true},
		{"incorrect state", fake.Error(409, "IncorrectState", "in use"), false},
		{"lock conflict", fake.Error(409, "LockConflict", "locked"), false},
		{"subnet full", fake.Error(409, "Conflict", "subnet ocid1.subnet.oc1..x: not enough free IP addresses"), false},
		{"a 400", fake.Error(400, "Conflict", "x"), false},
		{"not a service error", errors.New("409"), false},
		{"wrapped", fmt.Errorf("launch: %w", fake.Error(409, "Conflict", "x")), true},
	} {
		if got := isRetryTokenConflict(tc.err); got != tc.want {
			t.Errorf("%s: isRetryTokenConflict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLimitNameOf(t *testing.T) {
	for msg, want := range map[string]string{
		"The following service limits were exceeded: standard-e5-core-count":                                                     "standard-e5-core-count",
		"The following service limits were exceeded: standard-e5-core-count. Request a service limit increase from the console.": "standard-e5-core-count",
		"something else": "",
	} {
		if got := LimitNameOf(msg); got != want {
			t.Errorf("LimitNameOf(%q) = %q, want %q", msg, got, want)
		}
	}
}
