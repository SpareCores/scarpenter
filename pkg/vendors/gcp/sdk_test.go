/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// sdkServer serves canned JSON by path, records the requests, and gives the SDK
// adapter a client pointed at it. No credentials, no network beyond loopback.
func sdkServer(t *testing.T, handler http.HandlerFunc) (*SDK, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := NewSDK(t.Context(), "scarpenter/test", option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, &seen
}

func cannedReads(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/zones/z-a/instances/i1"):
			_, _ = fmt.Fprint(w, `{"name":"i1","status":"RUNNING","labels":{"scarpenter-cluster":"c1"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/zones/z-a/instances"):
			if r.URL.Query().Get("requestId") != "req-1" {
				t.Errorf("requestId = %q", r.URL.Query().Get("requestId"))
			}
			_, _ = fmt.Fprint(w, `{"name":"operation-1","status":"RUNNING","zone":"zones/z-a"}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/zones/z-a/instances/i1"):
			_, _ = fmt.Fprint(w, `{"name":"operation-2","status":"RUNNING"}`)
		case strings.HasSuffix(r.URL.Path, "/zones/z-a/operations/operation-1/wait"):
			_, _ = fmt.Fprint(w, `{"name":"operation-1","status":"DONE","error":{"errors":[{"code":"QUOTA_EXCEEDED","message":"Quota 'CPUS' exceeded.  Limit: 8.0 in region r."}]},"httpErrorStatusCode":403}`)
		case strings.HasSuffix(r.URL.Path, "/regions/r"):
			_, _ = fmt.Fprint(w, `{"name":"r","quotas":[{"metric":"CPUS","limit":24,"usage":3}]}`)
		case strings.HasSuffix(r.URL.Path, "/projects/p"):
			_, _ = fmt.Fprint(w, `{"name":"p","quotas":[{"metric":"CPUS_ALL_REGIONS","limit":32,"usage":1}]}`)
		case strings.HasSuffix(r.URL.Path, "/images/family/flatcar-stable"):
			_, _ = fmt.Fprint(w, `{"name":"flatcar-stable-4757-2-3","architecture":"X86_64","guestOsFeatures":[{"type":"GVNIC"}]}`)
		case strings.HasSuffix(r.URL.Path, "/global/images/missing"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"code":404,"message":"The resource 'projects/k/global/images/missing' was not found","errors":[{"reason":"notFound","message":"not found"}]}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func TestSDKReadsAndOperations(t *testing.T) {
	s, seen := sdkServer(t, cannedReads(t))
	ctx := t.Context()

	in, err := s.GetInstance(ctx, &computepb.GetInstanceRequest{Project: "p", Zone: "z-a", Instance: "i1"})
	if err != nil || in.GetStatus() != "RUNNING" || in.GetLabels()["scarpenter-cluster"] != "c1" {
		t.Fatalf("get = %v, %v", in, err)
	}
	op, err := s.InsertInstance(ctx, &computepb.InsertInstanceRequest{
		Project: "p", Zone: "z-a", RequestId: new("req-1"), InstanceResource: &computepb.Instance{Name: new("i1")},
	})
	if err != nil || op.GetName() != "operation-1" || op.GetStatus() != computepb.Operation_RUNNING {
		t.Fatalf("insert = %v, %v", op, err)
	}
	done, err := s.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{Project: "p", Zone: "z-a", Operation: "operation-1"})
	if err != nil || done.GetStatus() != computepb.Operation_DONE {
		t.Fatalf("wait = %v, %v", done, err)
	}
	// The finished operation's error classifies as an account-wide-free quota error.
	if err := classifyOperation(done, errContext{}); vendors.ClassOf(err) != vendors.QuotaExceeded {
		t.Fatalf("operation error = %v", err)
	}
	del, err := s.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: "p", Zone: "z-a", Instance: "i1"})
	if err != nil || del.GetName() != "operation-2" {
		t.Fatalf("delete = %v, %v", del, err)
	}

	// The quota reader works over the real client too.
	snap, err := (&QuotaReader{API: s, Project: "p"}).Quotas(ctx, "r")
	if err != nil || len(snap.Buckets) != 1 || snap.Buckets[0].Used != 3 {
		t.Fatalf("quotas = %v, %v", snap, err)
	}
	g, err := (&QuotaReader{API: s, Project: "p"}).GlobalQuotas(ctx)
	if err != nil || len(g.Buckets) != 1 || g.Buckets[0].ID != "CPUS_ALL_REGIONS" {
		t.Fatalf("global = %v, %v", g, err)
	}
	img, err := s.GetImageFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: "k", Family: "flatcar-stable"})
	if err != nil || img.GetGuestOsFeatures()[0].GetType() != "GVNIC" {
		t.Fatalf("image = %v, %v", img, err)
	}

	// A REST error reaches classify as a *googleapi.Error through the client's wrapping.
	_, err = s.GetImage(ctx, &computepb.GetImageRequest{Project: "k", Image: "missing"})
	if err == nil || vendors.ClassOf(classify(err, errContext{})) != vendors.NotFound || !IsNotFound(err) {
		t.Fatalf("missing image = %v", err)
	}
	if len(*seen) == 0 {
		t.Fatal("no request reached the server")
	}
}

func TestSDKListPaging(t *testing.T) {
	s, seen := sdkServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/zones/z-a/instances"):
			if q.Get("pageToken") == "" {
				_, _ = fmt.Fprint(w, `{"items":[{"name":"a"},{"name":"b"}],"nextPageToken":"t2"}`)
			} else {
				_, _ = fmt.Fprint(w, `{"items":[{"name":"c"}]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/aggregated/instances"):
			if q.Get("filter") != `labels.x = "y"` {
				t.Errorf("filter = %q", q.Get("filter"))
			}
			_, _ = fmt.Fprint(w, `{"items":{"zones/z-a":{"instances":[{"name":"a"}]},"zones/z-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/zones"):
			_, _ = fmt.Fprint(w, `{"items":[{"name":"z-a","id":"2000","status":"UP"}]}`)
		case strings.HasSuffix(r.URL.Path, "/z-a/machineTypes"):
			_, _ = fmt.Fprint(w, `{"items":[{"name":"e2-small","guestCpus":2}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ctx := t.Context()
	p1, err := s.ListInstances(ctx, &computepb.ListInstancesRequest{Project: "p", Zone: "z-a", MaxResults: proto.Uint32(2)})
	if err != nil || len(p1.Items) != 2 || p1.GetNextPageToken() != "t2" {
		t.Fatalf("page 1 = %v, %v", p1, err)
	}
	p2, err := s.ListInstances(ctx, &computepb.ListInstancesRequest{Project: "p", Zone: "z-a", MaxResults: proto.Uint32(2), PageToken: p1.NextPageToken})
	if err != nil || len(p2.Items) != 1 || p2.NextPageToken != nil {
		t.Fatalf("page 2 = %v, %v", p2, err)
	}
	agg, err := s.AggregatedListInstances(ctx, &computepb.AggregatedListInstancesRequest{Project: "p", Filter: new(`labels.x = "y"`)})
	if err != nil || len(agg.Items["zones/z-a"].GetInstances()) != 1 || agg.Items["zones/z-b"] == nil {
		t.Fatalf("aggregated = %v, %v", agg, err)
	}
	zs, err := s.ListZones(ctx, &computepb.ListZonesRequest{Project: "p"})
	if err != nil || len(zs.Items) != 1 || zs.Items[0].GetId() != 2000 {
		t.Fatalf("zones = %v, %v", zs, err)
	}
	mts, err := s.ListMachineTypes(ctx, &computepb.ListMachineTypesRequest{Project: "p", Zone: "z-a"})
	if err != nil || len(mts.Items) != 1 || mts.Items[0].GetGuestCpus() != 2 {
		t.Fatalf("machine types = %v, %v", mts, err)
	}
	if len(*seen) != 5 {
		t.Fatalf("requests = %v", *seen)
	}
}

// TestSDKErrorsClassify checks that the errors the generated REST client returns
// classify as the table says, from the HTTP bodies Google sends.
func TestSDKErrorsClassify(t *testing.T) {
	respond := func(status int, body string) *SDK {
		s, _ := sdkServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, body)
		})
		return s
	}
	get := func(s *SDK, region bool) error {
		if region {
			_, err := s.GetRegion(t.Context(), &computepb.GetRegionRequest{Project: "p", Region: "r"})
			return classifyRegionRead(err)
		}
		_, err := s.GetInstance(t.Context(), &computepb.GetInstanceRequest{Project: "p", Zone: "z-a", Instance: "i"})
		return classify(err, errContext{})
	}
	tests := []struct {
		name   string
		status int
		body   string
		region bool
		class  vendors.Class
	}{
		{"not found", 404, `{"error":{"code":404,"message":"gone","errors":[{"reason":"notFound"}]}}`, false, vendors.NotFound},
		{"permission denied on a region", 403, `{"error":{"code":403,"message":"denied","errors":[{"reason":"forbidden"}]}}`, true, vendors.RegionUnavailable},
		{"permission denied elsewhere", 403, `{"error":{"code":403,"message":"denied","errors":[{"reason":"forbidden"}]}}`, false, vendors.Fatal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vendors.ClassOf(get(respond(tc.status, tc.body), tc.region)); got != tc.class {
				t.Fatalf("class = %s, want %s", got, tc.class)
			}
		})
	}
}

func TestCloserOf(t *testing.T) {
	var nilClient *SDK
	if closerOf(nilClient) != nil {
		t.Fatal("a nil client has no closer")
	}
	// Close of a partly built SDK must not panic.
	if err := (&SDK{}).Close(); err != nil {
		t.Fatal(err)
	}
}
