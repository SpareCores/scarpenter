/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

func TestClassify(t *testing.T) {
	st := func(status int, msg string) error { return &APIError{Status: status, Message: msg} }
	cases := []struct {
		name        string
		err         error
		op          operation
		class       vendors.Class
		code        string
		accountWide bool
	}{
		{"nil", nil, opOther, "", "", false},
		{"429", st(429, "Rate limit exceeded."), opCreateInstance, vendors.Retryable, codeRateLimited, false},
		{"500", st(500, "oops"), opOther, vendors.Retryable, codeServerError, false},
		{"503 with a stock text is still retryable", st(503, "out of stock"), opCreateInstance, vendors.Retryable, codeServerError, false},
		{"canceled", fmt.Errorf("call: %w", context.Canceled), opOther, vendors.Retryable, codeCanceled, false},
		{"timeout", fmt.Errorf("call: %w", context.DeadlineExceeded), opCreateInstance, vendors.Retryable, codeTimeout, false},
		{"network", &net.OpError{Op: "dial", Err: errors.New("refused")}, opOther, vendors.Retryable, codeTransport, false},
		{"govultr gave up", errors.New("gave up after 4 attempts, last error: boom"), opOther, vendors.Retryable, codeRetriesExhaust, false},
		{"404 get", st(404, fake.MsgNotFound), opGet, vendors.NotFound, codeNotFound, false},
		{"404 delete", st(404, fake.MsgNotFound), opDelete, vendors.NotFound, codeNotFound, false},
		{"404 on create is a referenced resource, not the instance", st(404, "no such snapshot"), opCreateInstance, vendors.Fatal, codeRefNotFound, false},
		{"404 on a region", st(404, fake.MsgInvalidRegion), opRegion, vendors.RegionUnavailable, codeNotFound, false},
		{"400 on a region", st(400, "bad region"), opRegion, vendors.RegionUnavailable, codeInvalid, false},
		{"stock", st(400, fake.MsgPlanUnavailable), opCreateInstance, vendors.InsufficientCapacity, codeStock, false},
		{"out of stock", st(412, "The plan is out of stock."), opCreateInstance, vendors.InsufficientCapacity, codeStock, false},
		{"stock text on another operation is not capacity", st(400, fake.MsgPlanUnavailable), opDelete, vendors.Fatal, codeUnmatched, false},
		{"account limit", st(400, fake.MsgInstanceLimit), opCreateInstance, vendors.QuotaExceeded, codeAccountLimit, true},
		{"monthly cost limit", st(403, "Maximum instance cost reached."), opCreateInstance, vendors.QuotaExceeded, codeAccountLimit, true},
		{"firewall group limit", st(400, "Firewall group limit reached."), opOther, vendors.QuotaExceeded, codeResourceLimit, true},
		{"invalid plan", st(400, fake.MsgInvalidPlan), opCreateInstance, vendors.Unsupported, codeIncompatible, false},
		{"incompatible snapshot", st(400, "Snapshot is not compatible with this plan."), opCreateInstance, vendors.Unsupported, codeIncompatible, false},
		{"invalid region on a create gates the region", st(400, fake.MsgInvalidRegion), opCreateInstance, vendors.ZoneUnusable, codeRegionUnusable, false},
		{"bad user data", st(400, fake.MsgInvalidUserData), opCreateInstance, vendors.Fatal, codeUserData, false},
		{"delete of a used group", st(400, fake.MsgGroupInUse), opDelete, vendors.Retryable, codeDependency, false},
		{"delete of an attached VPC", st(400, fake.MsgVPCAttached), opDelete, vendors.Retryable, codeDependency, false},
		{"delete of an installing instance", st(400, fake.MsgInstanceInstalls), opDelete, vendors.Retryable, codeDependency, false},
		{"delete locked, any text", st(423, "?"), opDelete, vendors.Retryable, codeLocked, false},
		{"delete conflict, any text", st(409, "?"), opDelete, vendors.Retryable, codeLocked, false},
		{"401", st(401, "Unauthorized"), opOther, vendors.Fatal, codeUnauthorized, false},
		{"403", st(403, "Forbidden"), opOther, vendors.Fatal, codeForbidden, false},
		{"unmatched 400", st(400, "something new"), opCreateInstance, vendors.Fatal, codeUnmatched, false},
		{"a text alone never decides", &fake.Error{Status: 0, Message: fake.MsgPlanUnavailable}, opCreateInstance, vendors.Fatal, codeStatusUnknown, false},
		{"a lost status with a quota text is not quota", &fake.Error{Message: fake.MsgInstanceLimit}, opCreateInstance, vendors.Fatal, codeStatusUnknown, false},
		{"the fake's error carries the status", &fake.Error{Status: 404, Message: "x"}, opGet, vendors.NotFound, codeNotFound, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.err, c.op)
			if c.err == nil {
				if got != nil {
					t.Fatalf("classify(nil) = %v", got)
				}
				return
			}
			if vendors.ClassOf(got) != c.class || vendors.CodeOf(got) != c.code || vendors.IsAccountWide(got) != c.accountWide {
				t.Fatalf("classify = %v (class %s, code %q, account-wide %v), want %s %q %v",
					got, vendors.ClassOf(got), vendors.CodeOf(got), vendors.IsAccountWide(got), c.class, c.code, c.accountWide)
			}
			if !errors.Is(got, c.err) {
				t.Fatalf("the original error is not in the chain")
			}
		})
	}
}

func TestClassifyPassesDriverErrorsThrough(t *testing.T) {
	in := planNotAvailable("vc2-1c-1gb", "ewr")
	if got := classify(in, opCreateInstance); got != in {
		t.Fatalf("a driver error was rewrapped: %v", got)
	}
}

// Unmatched texts are Fatal and never capacity: capacity would make the core
// retry another offering forever.
func TestUnmatchedTextIsNeverCapacity(t *testing.T) {
	for _, msg := range []string{"", "bad request", "plan", "location", "stock"} {
		for _, op := range []operation{opOther, opCreateInstance, opGet, opDelete} {
			if c := vendors.ClassOf(classify(&APIError{Status: 400, Message: msg}, op)); c == vendors.InsufficientCapacity || c == vendors.QuotaExceeded {
				t.Errorf("%q on op %d classified %s", msg, op, c)
			}
		}
	}
}

func TestPlanNotAvailableIsCapacityNotQuota(t *testing.T) {
	err := planNotAvailable("vc2-1c-1gb", "ewr")
	if !vendors.IsClass(err, vendors.InsufficientCapacity) || vendors.IsAccountWide(err) {
		t.Fatalf("got %v", err)
	}
}
