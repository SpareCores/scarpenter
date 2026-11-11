/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func doneOp(code, msg string, status int32) *computepb.Operation {
	op := &computepb.Operation{Name: new("operation-1"), Status: computepb.Operation_DONE.Enum()}
	if code != "" {
		op.Error = &computepb.Error{Errors: []*computepb.Errors{{Code: new(code), Message: new(msg)}}}
		op.HttpErrorStatusCode = new(status)
	}
	return op
}

// TestErrorTable covers every entry of errorClasses as an operation error code.
func TestErrorTable(t *testing.T) {
	codes := make([]string, 0, len(errorClasses))
	for c := range errorClasses {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	for _, code := range codes {
		want := errorClasses[code]
		t.Run(code, func(t *testing.T) {
			err := classifyOperation(doneOp(code, "boom", 400), errContext{})
			if err == nil {
				t.Fatal("no error")
			}
			wantClass := want
			if code == CodeConditionNot {
				wantClass = vendors.Fatal
			}
			if got := vendors.ClassOf(err); got != wantClass {
				t.Fatalf("class = %s, want %s (%v)", got, wantClass, err)
			}
		})
	}
}

func TestClassifyOperation(t *testing.T) {
	ec := errContext{machineType: "n2-standard-2", zone: "europe-west3-a"}
	tests := []struct {
		name    string
		op      *computepb.Operation
		class   vendors.Class
		code    string
		account bool
	}{
		{"success", doneOp("", "", 0), "", "", false},
		{"capacity", doneOp("ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "The zone does not have enough resources", 503), vendors.InsufficientCapacity, "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", false},
		{"quota region", doneOp("QUOTA_EXCEEDED", "Quota 'CPUS' exceeded.  Limit: 24.0 in region europe-west3.", 403), vendors.QuotaExceeded, "QUOTA_EXCEEDED", false},
		{"quota family", doneOp("QUOTA_EXCEEDED", "Quota 'N2_CPUS' exceeded.  Limit: 8.0 in region europe-west3.", 403), vendors.QuotaExceeded, "QUOTA_EXCEEDED", false},
		{"quota all regions", doneOp("QUOTA_EXCEEDED", "Quota 'CPUS_ALL_REGIONS' exceeded.  Limit: 32.0.", 403), vendors.QuotaExceeded, "QUOTA_EXCEEDED", true},
		{"quota addresses", doneOp("QUOTA_EXCEEDED", "Quota 'IN_USE_ADDRESSES' exceeded.  Limit: 8.0 in region europe-west3.", 403), vendors.QuotaExceeded, "QUOTA_EXCEEDED", false},
		{"machine type missing", doneOp("NOT_FOUND", "The resource 'projects/p/zones/europe-west3-a/machineTypes/n2-standard-2' was not found", 404), vendors.Unsupported, "NOT_FOUND", false},
		{"other not found", doneOp("NOT_FOUND", "The resource 'projects/p/global/images/x' was not found", 404), vendors.NotFound, "NOT_FOUND", false},
		{"unsupported", doneOp("UNSUPPORTED_OPERATION", "Spot is not supported", 400), vendors.Unsupported, "UNSUPPORTED_OPERATION", false},
		{"subnet full", doneOp("IP_SPACE_EXHAUSTED", "Instance cannot be created: IP space exhausted", 400), vendors.ZoneUnusable, "IP_SPACE_EXHAUSTED", false},
		{"rate", doneOp("RESOURCE_OPERATION_RATE_EXCEEDED", "slow down", 429), vendors.Retryable, "RESOURCE_OPERATION_RATE_EXCEEDED", false},
		{"org policy", doneOp("CONDITION_NOT_MET", "Constraint constraints/compute.trustedImageProjects violated", 412), vendors.Fatal, CodeOrgPolicy, false},
		{"already exists", doneOp("ALREADY_EXISTS", "exists", 409), vendors.Fatal, "ALREADY_EXISTS", false},
		{"unknown code", doneOp("SOMETHING_NEW", "?", 400), vendors.Fatal, "SOMETHING_NEW", false},
		{"unknown 5xx", doneOp("SOMETHING_NEW", "?", 503), vendors.Retryable, codeServerError, false},
		{"status only", func() *computepb.Operation {
			op := doneOp("", "", 0)
			op.HttpErrorStatusCode, op.HttpErrorMessage = proto.Int32(503), new("unavailable")
			return op
		}(), vendors.Retryable, codeServerError, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyOperation(tc.op, ec)
			if tc.class == "" {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			if got := vendors.ClassOf(err); got != tc.class {
				t.Fatalf("class = %s, want %s (%v)", got, tc.class, err)
			}
			if got := vendors.CodeOf(err); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			if got := vendors.IsAccountWide(err); got != tc.account {
				t.Fatalf("account-wide = %v, want %v", got, tc.account)
			}
		})
	}
}

func TestClassifyOperationNotDone(t *testing.T) {
	op := &computepb.Operation{Name: new("op"), Status: computepb.Operation_RUNNING.Enum()}
	if got := vendors.ClassOf(classifyOperation(op, errContext{})); got != vendors.Retryable {
		t.Fatalf("running operation = %s", got)
	}
	if got := vendors.ClassOf(classifyOperation(nil, errContext{})); got != vendors.Fatal {
		t.Fatalf("nil operation = %s", got)
	}
}

func gerr(status int, msg string, reasons ...string) error {
	e := &googleapi.Error{Code: status, Message: msg}
	for _, r := range reasons {
		e.Errors = append(e.Errors, googleapi.ErrorItem{Reason: r, Message: msg})
	}
	return fmt.Errorf("compute: %w", e)
}

func TestClassifyHTTP(t *testing.T) {
	ec := errContext{machineType: "e2-small", zone: "us-central1-a"}
	tests := []struct {
		name  string
		err   error
		class vendors.Class
	}{
		{"429", gerr(429, "slow", "rateLimitExceeded"), vendors.Retryable},
		{"429 no reason", gerr(429, "slow"), vendors.Retryable},
		{"403 rate", gerr(403, "rate", "rateLimitExceeded"), vendors.Retryable},
		{"503", gerr(503, "down", "backendError"), vendors.Retryable},
		{"500 no reason", gerr(500, "down"), vendors.Retryable},
		{"404 instance", gerr(404, "The resource 'projects/p/zones/z/instances/i' was not found", "notFound"), vendors.NotFound},
		{"404 machine type", gerr(404, "The resource 'projects/p/zones/us-central1-a/machineTypes/e2-small' was not found", "notFound"), vendors.Unsupported},
		{"409", gerr(409, "already exists", "alreadyExists"), vendors.Fatal},
		{"400 in use", gerr(400, "in use", "resourceInUseByAnotherResource"), vendors.Retryable},
		{"403 quota", gerr(403, "Quota 'CPUS' exceeded. Limit: 8.0 in region r.", "quotaExceeded"), vendors.QuotaExceeded},
		{"403 permission", gerr(403, "Required 'compute.instances.create' permission", "forbidden"), vendors.Fatal},
		{"400 invalid", gerr(400, "Invalid value", "invalid"), vendors.Fatal},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), vendors.Retryable},
		{"canceled", context.Canceled, vendors.Retryable},
		{"net", &net.DNSError{Err: "no such host"}, vendors.Retryable},
		{"plain", errors.New("boom"), vendors.Fatal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vendors.ClassOf(classify(tc.err, ec)); got != tc.class {
				t.Fatalf("class = %s, want %s", got, tc.class)
			}
		})
	}
	if classify(nil, ec) != nil {
		t.Fatal("nil must stay nil")
	}
	pre := vendors.Errorf(vendors.NotFound, "x")
	if classify(pre, ec) != error(pre) {
		t.Fatal("a classified error must pass through")
	}
	if !vendors.IsAccountWide(classify(gerr(403, "Quota 'CPUS_ALL_REGIONS' exceeded. Limit: 8.0", "quotaExceeded"), ec)) {
		t.Fatal("CPUS_ALL_REGIONS must be account-wide")
	}
}

func TestClassifyRegionRead(t *testing.T) {
	if got := vendors.ClassOf(classifyRegionRead(gerr(403, "denied", "forbidden"))); got != vendors.RegionUnavailable {
		t.Fatalf("403 = %s", got)
	}
	if got := vendors.ClassOf(classifyRegionRead(gerr(401, "no creds"))); got != vendors.RegionUnavailable {
		t.Fatalf("401 = %s", got)
	}
	if got := vendors.ClassOf(classifyRegionRead(gerr(403, "rate", "rateLimitExceeded"))); got != vendors.Retryable {
		t.Fatalf("403 rate = %s", got)
	}
	if got := vendors.ClassOf(classifyRegionRead(gerr(500, "x"))); got != vendors.Retryable {
		t.Fatalf("500 = %s", got)
	}
	if classifyRegionRead(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestIsAlreadyExistsAndNotFound(t *testing.T) {
	if !IsAlreadyExists(gerr(409, "x", "alreadyExists")) || !IsAlreadyExists(classify(gerr(409, "x", "alreadyExists"), errContext{})) {
		t.Fatal("409 must be already exists, raw and classified")
	}
	if IsAlreadyExists(gerr(404, "x")) || IsAlreadyExists(nil) {
		t.Fatal("404 is not already exists")
	}
	if !IsNotFound(gerr(404, "x")) || !IsNotFound(classify(gerr(404, "x", "notFound"), errContext{})) || IsNotFound(gerr(500, "x")) {
		t.Fatal("IsNotFound wrong")
	}
}

func TestQuotaMetricOf(t *testing.T) {
	for msg, want := range map[string]string{
		"Quota 'CPUS' exceeded.  Limit: 24.0 in region europe-west3.": "CPUS",
		"Quota 'PREEMPTIBLE_CPUS' exceeded. Limit: 0.0":               "PREEMPTIBLE_CPUS",
		"something else": "",
	} {
		if got := QuotaMetricOf(msg); got != want {
			t.Errorf("QuotaMetricOf(%q) = %q, want %q", msg, got, want)
		}
	}
}
