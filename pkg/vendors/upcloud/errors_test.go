/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

// wantClasses is the table of plan 19 ("Error mapping"), written out
// independently of the driver's table. Every code of the driver's table must be
// listed here, so a new code needs a decision in two places.
var wantClasses = map[string]vendors.Class{
	"SERVER_RESOURCES_UNAVAILABLE":               vendors.InsufficientCapacity,
	"STORAGE_RESOURCES_UNAVAILABLE":              vendors.InsufficientCapacity,
	"SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA": vendors.InsufficientCapacity,
	"IP_ADDRESS_RESOURCES_UNAVAILABLE":           vendors.InsufficientCapacity,

	"CORE_MEMORY_UNSUPPORTED":    vendors.Unsupported,
	"PLAN_CORE_NUMBER_ILLEGAL":   vendors.Unsupported,
	"PLAN_MEMORY_AMOUNT_ILLEGAL": vendors.Unsupported,
	"STORAGE_TIER_ILLEGAL":       vendors.Unsupported,

	"SERVER_CORE_LIMIT_REACHED":          vendors.QuotaExceeded,
	"SERVER_MEMORY_LIMIT_REACHED":        vendors.QuotaExceeded,
	"MAXIOPS_STORAGE_LIMIT_REACHED":      vendors.QuotaExceeded,
	"SSD_STORAGE_LIMIT_REACHED":          vendors.QuotaExceeded,
	"HDD_STORAGE_LIMIT_REACHED":          vendors.QuotaExceeded,
	"SERVER_IP_LIMIT_REACHED":            vendors.QuotaExceeded,
	"NETWORK_LIMIT_REACHED":              vendors.QuotaExceeded,
	"DETACHED_FLOATING_IP_LIMIT_REACHED": vendors.QuotaExceeded,

	"ZONE_NOT_FOUND": vendors.ZoneUnusable,
	"ZONE_INVALID":   vendors.ZoneUnusable,

	"SERVER_CREATING_LIMIT_REACHED": vendors.Retryable,
	"SERVER_STATE_ILLEGAL":          vendors.Retryable,
	"STORAGE_STATE_ILLEGAL":         vendors.Retryable,
	"STORAGE_IN_USE":                vendors.Retryable,
	"NETWORK_IN_USE":                vendors.Retryable,
	"FIREWALL_RULESET_IN_USE":       vendors.Retryable,

	"SERVER_NOT_FOUND":           vendors.NotFound,
	"STORAGE_NOT_FOUND":          vendors.NotFound,
	"NETWORK_NOT_FOUND":          vendors.NotFound,
	"FIREWALL_RULESET_NOT_FOUND": vendors.NotFound,
}

// fatalCodes are classified by omission and pinned here: billing, our bugs and
// permission errors.
var fatalCodes = []string{
	"AUTHENTICATION_FAILED", "ACTION_FORBIDDEN", "SERVER_FORBIDDEN", "STORAGE_FORBIDDEN", "NETWORK_FORBIDDEN",
	"INSUFFICIENT_CREDITS", "USER_DATA_INVALID", "LABEL_LIMIT_REACHED", "ZONE_MISMATCH", "VALIDATION_ERROR",
	"STORAGE_DEVICE_LIMIT_REACHED", "SOMETHING_NEW",
}

func TestErrorTableMatchesPlan(t *testing.T) {
	for code, class := range errorClasses {
		want, ok := wantClasses[code]
		if !ok {
			t.Errorf("code %s is in the driver's table but not in the test's", code)
		} else if want != class {
			t.Errorf("code %s: driver says %s, plan says %s", code, class, want)
		}
	}
	for code := range wantClasses {
		if _, ok := errorClasses[code]; !ok {
			t.Errorf("code %s is in the plan's table but not in the driver's", code)
		}
	}
}

func TestClassifyEveryCode(t *testing.T) {
	forms := map[string]func(string, int) *upcloud.Problem{"problem+json": fake.Problem, "legacy": fake.LegacyProblem}
	for form, mk := range forms {
		for code, class := range wantClasses {
			t.Run(form+"/"+code, func(t *testing.T) {
				err := Classify(fmt.Errorf("call: %w", mk(code, http.StatusBadRequest)))
				if got := vendors.ClassOf(err); got != class {
					t.Fatalf("class = %s, want %s", got, class)
				}
				if vendors.CodeOf(err) != code {
					t.Errorf("code = %q, want %q", vendors.CodeOf(err), code)
				}
				if wide := vendors.IsAccountWide(err); wide != (class == vendors.QuotaExceeded) {
					t.Errorf("account-wide = %v for class %s", wide, class)
				}
			})
		}
	}
}

func TestClassifyFatalByOmission(t *testing.T) {
	for _, code := range fatalCodes {
		// Even with a 5xx status: a listed-as-fatal code is the API's answer.
		err := Classify(fake.Problem(code, http.StatusBadRequest))
		if vendors.ClassOf(err) != vendors.Fatal || vendors.CodeOf(err) != code {
			t.Errorf("%s: class %s code %q, want Fatal", code, vendors.ClassOf(err), vendors.CodeOf(err))
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyTransport(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		class vendors.Class
		code  string
	}{
		"nil":               {nil, "", ""},
		"deadline":          {context.DeadlineExceeded, vendors.Retryable, CodeTimeout},
		"wrapped deadline":  {fmt.Errorf("x: %w", context.DeadlineExceeded), vendors.Retryable, CodeTimeout},
		"canceled":          {context.Canceled, vendors.Retryable, CodeCanceled},
		"5xx unlisted code": {fake.Problem("SERVICE_ERROR", 500), vendors.Retryable, CodeHTTP5xx},
		"502 gateway":       {fake.LegacyProblem("BAD_GATEWAY", 502), vendors.Retryable, CodeHTTP5xx},
		"429 throttled":     {fake.Problem("TOO_MANY_REQUESTS", 429), vendors.Retryable, CodeThrottled},
		"net error":         {net.Error(timeoutErr{}), vendors.Retryable, CodeTransport},
		"plain":             {errors.New("boom"), vendors.Fatal, ""},
		"404 unlisted":      {fake.Problem("SOMETHING_NOT_FOUND", 404), vendors.Fatal, "SOMETHING_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Classify(tc.err)
			if tc.err == nil {
				if err != nil {
					t.Fatalf("nil became %v", err)
				}
				return
			}
			if got := vendors.ClassOf(err); got != tc.class {
				t.Fatalf("class = %s, want %s", got, tc.class)
			}
			if got := vendors.CodeOf(err); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestClassifyKeepsClassifiedErrors(t *testing.T) {
	in := vendors.NewError(vendors.ZoneUnusable, "X", errors.New("x"))
	if out := Classify(fmt.Errorf("wrapped: %w", in)); vendors.ClassOf(out) != vendors.ZoneUnusable {
		t.Fatalf("class changed to %s", vendors.ClassOf(out))
	}
}

func TestIsGone(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		gone bool
	}{
		"listed not found":  {Classify(fake.Problem("SERVER_NOT_FOUND", 404)), true},
		"unlisted 404":      {fake.Problem("WHATEVER", 404), true},
		"conflict":          {Classify(fake.Problem("SERVER_STATE_ILLEGAL", 409)), false},
		"fatal":             {Classify(fake.Problem("ACTION_FORBIDDEN", 403)), false},
		"transport":         {errors.New("x"), false},
		"nil":               {nil, false},
		"classified 404 NF": {vendors.NewError(vendors.NotFound, "", errors.New("x")), true},
	} {
		if got := isGone(tc.err); got != tc.gone {
			t.Errorf("%s: isGone = %v, want %v", name, got, tc.gone)
		}
	}
}
