/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

func api(service string, status int, msg string) error {
	return &osapi.APIError{Service: service, Status: status, Message: msg}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// TestClassify pins every text of textRules with its status, and the status
// rules; the texts are the ones Nova and Neutron use upstream and are to be
// confirmed on OVH by spike S7.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want vendors.Class
		code string
	}{
		// capacity
		{"no valid host", api("nova", 500, "No valid host was found. There are not enough hosts available."), vendors.InsufficientCapacity, CodeNoValidHost},
		{"retries exhausted", api("nova", 500, "Exceeded maximum number of retries. Exhausted all hosts available for retrying build failures for instance abc."), vendors.InsufficientCapacity, CodeMaxRetries},
		// quota
		{"nova instances", api("nova", 403, "Quota exceeded for instances: Requested 1, but already used 10 of 10 instances"), vendors.QuotaExceeded, CodeOverQuota},
		{"nova cores", api("nova", 403, "Quota exceeded for cores: Requested 4, but already used 8 of 8 cores"), vendors.QuotaExceeded, CodeOverQuota},
		{"nova ram", api("nova", 403, "Quota exceeded for ram: Requested 8192, but already used 0 of 4096 ram"), vendors.QuotaExceeded, CodeOverQuota},
		{"nova server_groups", api("nova", 403, "Quota exceeded for server_groups: Requested 1, but already used 10 of 10 server_groups"), vendors.QuotaExceeded, CodeOverQuota},
		{"nova server groups", api("nova", 403, "Quota exceeded, too many server groups."), vendors.QuotaExceeded, CodeOverQuota},
		{"nova port quota", api("nova", 403, "Maximum number of ports exceeded"), vendors.QuotaExceeded, CodeOverQuota},
		{"neutron ports", api("neutron", 409, "OverQuota: Quota exceeded for resources: ['port']."), vendors.QuotaExceeded, CodeOverQuota},
		{"neutron groups", api("neutron", 409, "OverQuota: Quota exceeded for resources: ['security_group']."), vendors.QuotaExceeded, CodeOverQuota},
		// unsupported
		{"flavor missing", api("nova", 400, "Invalid flavorRef provided."), vendors.Unsupported, CodeFlavorUnusable},
		{"flavor disk", api("nova", 400, "Flavor's disk is too small for requested image. Flavor disk is 1073741824 bytes."), vendors.Unsupported, CodeFlavorUnusable},
		{"flavor memory", api("nova", 400, "Flavor's memory is too small for requested image."), vendors.Unsupported, CodeFlavorUnusable},
		// zone
		{"ext-net full", api("neutron", 409, "IpAddressGenerationFailure: No more IP addresses available on network 1234."), vendors.ZoneUnusable, CodeIPExhausted},
		{"ext-net full in a fault", api("nova", 500, "Build of instance x aborted: No more IP addresses available on network 1234"), vendors.ZoneUnusable, CodeIPExhausted},
		// region
		{"no endpoint", fmt.Errorf("compute: %w", osapi.ErrNoEndpoint), vendors.RegionUnavailable, CodeNoEndpoint},
		{"keystone 401 is the credentials", api("keystone", 401, "The request you have made requires authentication."), vendors.Fatal, CodeUnauthorized},
		{"keystone 403", api("keystone", 403, "You are not authorized to perform the requested action."), vendors.RegionUnavailable, CodeRegionDenied},
		// not found
		{"server 404", api("nova", 404, "Instance x could not be found."), vendors.NotFound, CodeNotFound},
		{"group 404", api("neutron", 404, "SecurityGroupNotFound: Security group x does not exist"), vendors.NotFound, CodeNotFound},
		// retryable
		{"429", api("nova", 429, "Too Many Requests"), vendors.Retryable, CodeThrottled},
		{"500 unknown", api("nova", 500, "Internal Server Error"), vendors.Retryable, CodeServerError},
		{"502", api("neutron", 502, "Bad Gateway"), vendors.Retryable, CodeServerError},
		{"503", api("glance", 503, "Service Unavailable"), vendors.Retryable, CodeServerError},
		{"504", api("nova", 504, "Gateway Time-out"), vendors.Retryable, CodeServerError},
		{"conflict", api("nova", 409, "Cannot 'delete' instance x while it is in task_state deleting"), vendors.Retryable, CodeConflict},
		{"ambiguous group name is configuration", api("nova", 409, "Multiple security groups found matching 'nodes'. Use an ID to be more specific."), vendors.Fatal, CodeAmbiguousGroup},
		{"group in use", api("neutron", 409, "SecurityGroupInUse: Security Group x in use."), vendors.Retryable, CodeConflict},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), vendors.Retryable, CodeTimeout},
		{"canceled", context.Canceled, vendors.Retryable, CodeCanceled},
		{"transport", &net.OpError{Op: "dial", Err: timeoutErr{}}, vendors.Retryable, CodeTransport},
		// fatal
		{"image missing", api("nova", 400, "Invalid imageRef provided."), vendors.Fatal, CodeImageMissing},
		{"image unknown", api("nova", 400, "Can not find requested image"), vendors.Fatal, CodeImageMissing},
		{"image not active", api("nova", 400, "Image 1234 is not active."), vendors.Fatal, CodeImageNotActive},
		{"bad request", api("nova", 400, "Invalid input for field/attribute name."), vendors.Fatal, CodeBadRequest},
		{"401 with fresh credentials", api("nova", 401, "Authentication required"), vendors.Fatal, CodeUnauthorized},
		{"403 without a quota text", api("nova", 403, "Policy doesn't allow os_compute_api:servers:create to be performed."), vendors.Fatal, CodeForbidden},
		{"metadata items quota is ours to fix", api("nova", 403, "Quota exceeded for metadata items: Requested 130, but already used 0 of 128 metadata items"), vendors.Fatal, CodeForbidden},
		{"unknown error", errors.New("boom"), vendors.Fatal, ""},
		{"unknown status", api("nova", 418, "teapot"), vendors.Fatal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if vendors.ClassOf(got) != tc.want || vendors.CodeOf(got) != tc.code {
				t.Fatalf("got %s/%q, want %s/%q: %v", vendors.ClassOf(got), vendors.CodeOf(got), tc.want, tc.code, got)
			}
			if vendors.IsAccountWide(got) {
				t.Fatal("OVH limits are per region, never account-wide")
			}
		})
	}
	if Classify(nil) != nil {
		t.Fatal("nil stays nil")
	}
	pre := vendors.NewError(vendors.Unsupported, "x", errors.New("e"))
	if Classify(fmt.Errorf("wrapped: %w", pre)) == nil || vendors.ClassOf(Classify(fmt.Errorf("wrapped: %w", pre))) != vendors.Unsupported {
		t.Fatal("classified errors pass through")
	}
}

func TestClassifyHints(t *testing.T) {
	err := Classify(api("nova", 409, "Multiple security groups found matching 'nodes'. Use an ID to be more specific."))
	if !strings.Contains(err.Error(), "unique") || !strings.Contains(err.Error(), "Multiple security groups") {
		t.Errorf("ambiguous group message: %v", err)
	}
	var ae *osapi.APIError
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Error("the hint keeps the API error in the chain")
	}
}

// Reading the catalog is not about one region: a Keystone 401 or 403 there
// is Fatal, never RegionUnavailable (the contract allows that only from Zones).
func TestCatalogRegions(t *testing.T) {
	ctx := context.Background()
	m, err := NewRegionMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := fake.New()
	c.AddRegion("GRA11", fake.RegionConfig{})
	c.AddRegion("MARS1", fake.RegionConfig{})
	joined, unknown, err := CatalogRegions(ctx, c, m)
	if err != nil || len(joined) != 1 || joined[0].Name != "GRA11" || len(unknown) != 1 || unknown[0] != "MARS1" {
		t.Fatalf("joined %v unknown %v err %v", joined, unknown, err)
	}
	for _, st := range []int{401, 403} {
		c.InjectError("", "Regions", api("keystone", st, "denied"), 1)
		_, _, err = CatalogRegions(ctx, c, m)
		if !vendors.IsClass(err, vendors.Fatal) {
			t.Errorf("keystone %d on the catalog: %v", st, err)
		}
	}
	c.InjectError("", "Regions", api("keystone", 503, "down"), 1)
	if _, _, err = CatalogRegions(ctx, c, m); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("keystone 503 on the catalog: %v", err)
	}
}

// A text alone never decides: the same words with another status do not match.
func TestTextNeedsItsStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		not  vendors.Class
		note string
	}{
		{api("nova", 400, "No valid host was found"), vendors.InsufficientCapacity, "capacity text on a 400"},
		{api("nova", 200, "No valid host was found"), vendors.InsufficientCapacity, "on a 200"},
		{api("neutron", 500, "Quota exceeded for cores"), vendors.QuotaExceeded, "quota text on a 500 of neutron"},
		{api("glance", 403, "Quota exceeded for instances"), vendors.QuotaExceeded, "quota text from another service"},
		{api("nova", 404, "Invalid flavorRef provided"), vendors.Unsupported, "flavor text on a 404"},
	} {
		if got := Classify(tc.err); vendors.ClassOf(got) == tc.not {
			t.Errorf("%s classified as %s", tc.note, tc.not)
		}
	}
}

func TestClassifyFault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault *osapi.Fault
		want  vendors.Class
		code  string
	}{
		{"no valid host", &osapi.Fault{Code: 500, Message: "No valid host was found. There are not enough hosts available."}, vendors.InsufficientCapacity, CodeNoValidHost},
		{"retries", &osapi.Fault{Code: 500, Message: "Exceeded maximum number of retries."}, vendors.InsufficientCapacity, CodeMaxRetries},
		{"ip", &osapi.Fault{Code: 500, Message: "No more IP addresses available on network x"}, vendors.ZoneUnusable, CodeIPExhausted},
		{"unknown stays fatal", &osapi.Fault{Code: 500, Message: "Build of instance x aborted: Failed to allocate the network(s)"}, vendors.Fatal, CodeServerFault},
		{"no fault", nil, vendors.Fatal, CodeServerFault},
		{"text with a wrong code is not capacity", &osapi.Fault{Code: 400, Message: "No valid host was found"}, vendors.Fatal, CodeBadRequest},
	} {
		got := ClassifyFault(tc.fault)
		if vendors.ClassOf(got) != tc.want || vendors.CodeOf(got) != tc.code {
			t.Errorf("%s: got %s/%q: %v", tc.name, vendors.ClassOf(got), vendors.CodeOf(got), got)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(api("nova", 404, "x")) || IsNotFound(api("nova", 500, "x")) || IsNotFound(nil) {
		t.Fatal("IsNotFound")
	}
}
