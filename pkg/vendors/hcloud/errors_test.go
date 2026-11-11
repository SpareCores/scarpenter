/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func apiErr(code hc.ErrorCode, fields ...string) error {
	e := hc.Error{Code: code, Message: "m"}
	if len(fields) > 0 {
		d := hc.ErrorDetailsInvalidInput{}
		for _, f := range fields {
			d.Fields = append(d.Fields, hc.ErrorDetailsInvalidInputField{Name: f})
		}
		e.Details = d
	}
	return fmt.Errorf("wrapped: %w", e)
}

// TestErrorTable pins the class of every code of the plan's table, and that the
// table in the code has no entry the test does not know.
func TestErrorTable(t *testing.T) {
	want := map[hc.ErrorCode]vendors.Class{
		hc.ErrorCodeResourceUnavailable:   vendors.InsufficientCapacity,
		hc.ErrorCodePlacementError:        vendors.InsufficientCapacity,
		hc.ErrorCodeNoSpaceLeftInLocation: vendors.InsufficientCapacity,
		hc.ErrorCodeInvalidServerType:     vendors.Unsupported,
		hc.ErrorCodeResourceLimitExceeded: vendors.QuotaExceeded,
		hc.ErrorCodeNoSubnetAvailable:     vendors.ZoneUnusable,
		hc.ErrorCodeIPNotAvailable:        vendors.ZoneUnusable,
		hc.ErrorCodeRateLimitExceeded:     vendors.Retryable,
		hc.ErrorCodeServerError:           vendors.Retryable,
		hc.ErrorCodeBadGateway:            vendors.Retryable,
		hc.ErrorCodeTimeout:               vendors.Retryable,
		"unavailable":                     vendors.Retryable,
		hc.ErrorCodeConflict:              vendors.Retryable,
		hc.ErrorCodeLocked:                vendors.Retryable,
		hc.ErrorCodeMaintenance:           vendors.Retryable,
		hc.ErrorCodeRobotUnavailable:      vendors.Retryable,
		hc.ErrorCodeServiceError:          vendors.Retryable,
		hc.ErrorCodeUniquenessError:       vendors.Retryable,
		hc.ErrorCodeResourceInUse:         vendors.Retryable,
		hc.ErrorCodeNotFound:              vendors.NotFound,
		hc.ErrorCodeUnauthorized:          vendors.Fatal,
		hc.ErrorCodeForbidden:             vendors.Fatal,
		hc.ErrorCodeJSONError:             vendors.Fatal,
		hc.ErrorCodeProtected:             vendors.Fatal,
		hc.ErrorDeprecatedAPIEndpoint:     vendors.Fatal,
		hc.ErrorUnsupportedError:          vendors.Fatal,
	}
	if len(want) != len(errorClasses) {
		t.Errorf("errorClasses has %d entries, the test pins %d", len(errorClasses), len(want))
	}
	for code, class := range want {
		got := classify(apiErr(code))
		if vendors.ClassOf(got) != class {
			t.Errorf("%s: class %s, want %s", code, vendors.ClassOf(got), class)
		}
		if vendors.CodeOf(got) != string(code) {
			t.Errorf("%s: code %q", code, vendors.CodeOf(got))
		}
		if wide := vendors.IsAccountWide(got); wide != (class == vendors.QuotaExceeded) {
			t.Errorf("%s: account-wide %v", code, wide)
		}
	}
}

func TestInvalidInputByField(t *testing.T) {
	for _, tc := range []struct {
		fields []string
		want   vendors.Class
	}{
		{[]string{"server_type"}, vendors.Unsupported},
		{[]string{"location"}, vendors.Unsupported},
		{[]string{"image"}, vendors.Unsupported},
		{[]string{"server_type", "image"}, vendors.Unsupported},
		{[]string{"server_type.name"}, vendors.Unsupported},
		{[]string{"user_data"}, vendors.Fatal},
		{[]string{"server_type", "name"}, vendors.Fatal},
		{[]string{"labels"}, vendors.Fatal},
		{nil, vendors.Fatal},
	} {
		if got := vendors.ClassOf(classify(apiErr(hc.ErrorCodeInvalidInput, tc.fields...))); got != tc.want {
			t.Errorf("invalid_input %v: %s, want %s", tc.fields, got, tc.want)
		}
	}
}

func TestUnknownCodeIsFatal(t *testing.T) {
	got := classify(apiErr("something_new"))
	if vendors.ClassOf(got) != vendors.Fatal || vendors.CodeOf(got) != "something_new" {
		t.Errorf("got %v", got)
	}
}

func TestActionErrorsUseTheTable(t *testing.T) {
	for code, want := range map[string]vendors.Class{
		"resource_unavailable": vendors.InsufficientCapacity,
		"placement_error":      vendors.InsufficientCapacity,
		"server_error":         vendors.Retryable,
		"weird":                vendors.Fatal,
	} {
		err := fmt.Errorf("w: %w", hc.ActionError{Code: code, Message: "x"})
		if got := vendors.ClassOf(classify(err)); got != want {
			t.Errorf("action %s: %s, want %s", code, got, want)
		}
	}
}

func TestTransportAndContext(t *testing.T) {
	if c := classify(context.DeadlineExceeded); vendors.ClassOf(c) != vendors.Retryable || vendors.CodeOf(c) != CodeTimeout {
		t.Errorf("deadline: %v", c)
	}
	if c := classify(fmt.Errorf("x: %w", context.Canceled)); vendors.CodeOf(c) != CodeCanceled {
		t.Errorf("canceled: %v", c)
	}
	var ne net.Error = &net.DNSError{IsTimeout: true}
	if c := classify(ne); vendors.ClassOf(c) != vendors.Retryable || vendors.CodeOf(c) != CodeTransport {
		t.Errorf("net: %v", c)
	}
	if c := classify(fmt.Errorf("hcloud: %w 502", hc.ErrStatusCode)); vendors.ClassOf(c) != vendors.Retryable || vendors.CodeOf(c) != CodeServerError {
		t.Errorf("502: %v", c)
	}
	if c := classify(fmt.Errorf("hcloud: %w 404", hc.ErrStatusCode)); vendors.ClassOf(c) != vendors.Fatal {
		t.Errorf("404 without body: %v", c)
	}
	if c := classify(errors.New("boom")); vendors.ClassOf(c) != vendors.Fatal {
		t.Errorf("plain: %v", c)
	}
	if classify(nil) != nil {
		t.Error("nil")
	}
}

func TestClassifiedPassThrough(t *testing.T) {
	in := vendors.NewError(vendors.NotFound, "x", errors.New("e"))
	if classify(in) != error(in) {
		t.Error("an already classified error is returned as is")
	}
}

func TestIsCode(t *testing.T) {
	if !isCode(apiErr(hc.ErrorCodeNotFound), hc.ErrorCodeNotFound) {
		t.Error("raw")
	}
	if !isCode(classify(apiErr(hc.ErrorCodeNotFound)), hc.ErrorCodeNotFound) {
		t.Error("classified")
	}
	if isCode(apiErr(hc.ErrorCodeConflict), hc.ErrorCodeNotFound) {
		t.Error("other")
	}
}
