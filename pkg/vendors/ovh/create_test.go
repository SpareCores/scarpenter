/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

var fastWait = WaitOptions{Timeout: time.Second, Interval: time.Millisecond}

func createCloud(t *testing.T) (*fake.Cloud, osapi.Clients, osapi.CreateServerRequest) {
	t.Helper()
	ctx := context.Background()
	c := fake.New()
	c.AddRegion("GRA11", fake.RegionConfig{Flavors: []osapi.Flavor{{Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50}}})
	cl, err := c.Clients(ctx, "GRA11")
	if err != nil {
		t.Fatal(err)
	}
	img := c.AddImage("GRA11", osapi.Image{Name: "flatcar"})
	fl, _ := cl.Compute.ListFlavors(ctx)
	nets, _ := cl.Network.ListNetworks(ctx)
	r := baseRequest()
	r.Image.Ref = img
	settings, _ := ParseSettings(nil)
	req, err := BuildCreateRequest(LaunchPlan{Request: r, Settings: settings, FlavorID: fl[0].ID, ExternalNetworkID: nets[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	return c, cl, req
}

func keyOf(req osapi.CreateServerRequest) string {
	return req.Metadata[TagKey(vendors.TagIdempotencyKey)]
}

// A server that the scheduler fails goes to ERROR: CreateAndWait deletes it
// and returns the fault's class, and a lookup by the key finds nothing live.
func TestCreateAndWaitDeletesErrorServer(t *testing.T) {
	ctx := context.Background()
	c, cl, req := createCloud(t)
	c.SetNoValidHost("GRA11", "b3-8", true)
	s, err := CreateAndWait(ctx, cl.Compute, req, fastWait)
	if s != nil || !vendors.IsClass(err, vendors.InsufficientCapacity) || vendors.CodeOf(err) != CodeNoValidHost {
		t.Fatalf("got %v, %v", s, err)
	}
	all, _ := cl.Compute.ListServers(ctx, osapi.ServerFilter{Name: "^" + req.Name + "$"})
	if len(all) != 1 || all[0].TaskState != osapi.TaskDeleting || ServerState(all[0]) != vendors.StateTerminating {
		t.Fatalf("the ERROR server is being deleted: %+v", all)
	}
	if m, stale := FindByKey(all, keyOf(req)); m != nil || len(stale) != 0 {
		t.Fatalf("a server being deleted is not a match: %v %v", m, stale)
	}
	if l, _ := cl.Compute.ListServers(ctx, osapi.ServerFilter{}); len(l) != 0 {
		t.Fatalf("gone after the deleting call: %v", l)
	}
	if ids := c.ServerIDs("GRA11"); len(ids) != 0 {
		t.Fatalf("gone after the deleting call: %v", ids)
	}
	// The next attempt, with stock back, creates a fresh server.
	c.SetNoValidHost("GRA11", "b3-8", false)
	if s, err = CreateAndWait(ctx, cl.Compute, req, fastWait); err != nil || s.Status == osapi.StatusError {
		t.Fatalf("retry: %v %v", s, err)
	}
}

// When the delete of the ERROR server fails the error is Retryable (not the
// fault's class), and the server is left for FindByKey's stale list.
func TestCreateAndWaitDeleteFails(t *testing.T) {
	ctx := context.Background()
	c, cl, req := createCloud(t)
	c.SetNoValidHost("GRA11", "b3-8", true)
	c.InjectError("GRA11", "DeleteServer", &osapi.APIError{Service: "nova", Status: 500, Message: "boom"}, 1)
	_, err := CreateAndWait(ctx, cl.Compute, req, fastWait)
	if !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != CodeErrorServerLeft {
		t.Fatalf("got %v", err)
	}
	all, _ := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
	m, stale := FindByKey(all, keyOf(req))
	if m != nil || len(stale) != 1 || stale[0].Status != osapi.StatusError {
		t.Fatalf("stale ERROR server: %v %v", m, stale)
	}
}

func TestCreateAndWaitOutcomes(t *testing.T) {
	ctx := context.Background()
	c, cl, req := createCloud(t)
	// Scheduled: returned once the task state leaves scheduling.
	s, err := CreateAndWait(ctx, cl.Compute, req, fastWait)
	if err != nil || s.TaskState == osapi.TaskScheduling || s.ID == "" {
		t.Fatalf("scheduled: %+v %v", s, err)
	}
	if m, _ := FindByKey([]osapi.Server{*s}, keyOf(req)); m == nil || m.ID != s.ID {
		t.Fatal("the live server is the match")
	}
	// Still scheduling when a poll fails: returned as last seen, ID kept.
	c.InjectError("GRA11", "GetServer", &osapi.APIError{Service: "nova", Status: 503, Message: "down"}, 1)
	req.Name = "nc-2"
	if s, err = CreateAndWait(ctx, cl.Compute, req, fastWait); err != nil || s.ID == "" || s.TaskState != osapi.TaskScheduling {
		t.Fatalf("poll failure: %+v %v", s, err)
	}
	// Timeout 0 is the default, a canceled context returns the server as is.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	req.Name = "nc-3"
	if s, err = CreateAndWait(cctx, cl.Compute, req, WaitOptions{Interval: time.Hour}); err != nil || s.ID == "" {
		t.Fatalf("canceled: %+v %v", s, err)
	}
	// A create error is classified.
	c.SetLimits("GRA11", osapi.Limits{MaxInstances: 1})
	req.Name = "nc-4"
	if _, err = CreateAndWait(ctx, cl.Compute, req, fastWait); !vendors.IsClass(err, vendors.QuotaExceeded) {
		t.Fatalf("quota: %v", err)
	}
	if m, _ := FindByKey(nil, ""); m != nil {
		t.Fatal("no key, no match")
	}
}

func TestServerState(t *testing.T) {
	for _, tc := range []struct {
		status, task, want string
	}{
		{"ACTIVE", "", vendors.StateRunning},
		{"ACTIVE", "deleting", vendors.StateTerminating},
		{"ERROR", "deleting", vendors.StateTerminating},
		{"BUILD", "scheduling", vendors.StatePending},
		{"DELETED", "deleting", vendors.StateTerminated},
	} {
		if got := ServerState(osapi.Server{Status: tc.status, TaskState: tc.task}); got != tc.want {
			t.Errorf("%s/%s: %s, want %s", tc.status, tc.task, got, tc.want)
		}
	}
}
