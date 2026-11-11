/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func baseRequest() vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: "ewr", InstanceType: "vc2-2c-4gb", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: "claim-1", vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "default", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		ExtraTags:      map[string]string{"team": "data"},
		IdempotencyKey: "11111111-2222-3333-4444-555555555555",
		Node:           vendors.NodeInfo{Name: "claim-1"},
	}
}

func baseLaunch() LaunchOptions {
	return LaunchOptions{SnapshotID: "5a000000-0000-4000-8000-000000000001", FirewallGroupID: "fa000000-0000-4000-8000-000000000002"}
}

func golden(t *testing.T, name string, r *govultr.InstanceCreateReq) {
	t.Helper()
	raw, err := MarshalCreateRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, name, raw)
}

func TestCreateRequestGoldenDefaults(t *testing.T) {
	r, err := BuildCreateRequest(baseRequest(), baseLaunch())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_request_defaults.golden.json", r)
}

func TestCreateRequestGoldenEverything(t *testing.T) {
	lo := baseLaunch()
	lo.VPCIDs = []string{"0c000000-0000-4000-8000-000000000003"}
	lo.IPv6 = true
	r, err := BuildCreateRequest(baseRequest(), lo)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_request_everything.golden.json", r)
}

func TestCreateRequestFields(t *testing.T) {
	req := baseRequest()
	r, err := BuildCreateRequest(req, baseLaunch())
	if err != nil {
		t.Fatal(err)
	}
	if r.Label != "claim-1" || r.Hostname != "claim-1" || r.Region != "ewr" || r.Plan != "vc2-2c-4gb" {
		t.Errorf("name fields: %+v", r)
	}
	if raw, err := base64.StdEncoding.DecodeString(r.UserData); err != nil || string(raw) != string(req.UserData) {
		t.Errorf("user data is not the base64 of the Ignition: %q %v", r.UserData, err)
	}
	if r.EnableIPv6 == nil || *r.EnableIPv6 || r.Backups != "disabled" || r.ActivationEmail == nil || *r.ActivationEmail ||
		r.DDOSProtection == nil || *r.DDOSProtection {
		t.Errorf("flags: %+v", r)
	}
	if r.OsID != 0 || r.ScriptID != "" || len(r.SSHKeys) != 0 || r.DisablePublicIPv4 != nil || r.VPCOnly != nil {
		t.Errorf("fields the plan leaves unset: %+v", r)
	}
	var found bool
	for _, tag := range r.Tags {
		found = found || tag == "scarpenter.sparecores.com/idempotency-key=11111111-2222-3333-4444-555555555555"
	}
	if !found {
		t.Errorf("no idempotency tag in %v", r.Tags)
	}
	// The input is not modified by the idempotency tag.
	if _, ok := req.Tags[vendors.TagIdempotencyKey]; ok {
		t.Error("the request's tags were modified")
	}
	// A claim without a key writes no idempotency tag.
	req.IdempotencyKey = ""
	r, _ = BuildCreateRequest(req, baseLaunch())
	for _, tag := range r.Tags {
		if strings.Contains(tag, "idempotency") {
			t.Errorf("tag %q without a key", tag)
		}
	}
}

func TestCreateRequestNameFallsBackToTheNodeClaimTag(t *testing.T) {
	req := baseRequest()
	req.Node.Name = ""
	r, err := BuildCreateRequest(req, baseLaunch())
	if err != nil || r.Label != "claim-1" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCreateRequestRejects(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*vendors.CreateRequest, *LaunchOptions)
		class vendors.Class
		code  string
	}{
		{"no region", func(r *vendors.CreateRequest, _ *LaunchOptions) { r.Region = "" }, vendors.Fatal, codeInvalid},
		{"no plan", func(r *vendors.CreateRequest, _ *LaunchOptions) { r.InstanceType = "" }, vendors.Fatal, codeInvalid},
		{"no snapshot", func(_ *vendors.CreateRequest, o *LaunchOptions) { o.SnapshotID = "" }, vendors.Fatal, codeInvalid},
		{"no name", func(r *vendors.CreateRequest, _ *LaunchOptions) {
			r.Node.Name = ""
			delete(r.Tags, vendors.TagNodeClaim)
		}, vendors.Fatal, codeInvalid},
		{"no user data", func(r *vendors.CreateRequest, _ *LaunchOptions) { r.UserData = nil }, vendors.Fatal, codeInvalid},
		{"spot", func(r *vendors.CreateRequest, _ *LaunchOptions) { r.CapacityType = vendors.CapacitySpot }, vendors.Unsupported, codeUnsupportedArch},
		{"user data over the budget", func(r *vendors.CreateRequest, _ *LaunchOptions) { r.UserData = make([]byte, MaxUserDataBytes+1) }, vendors.Fatal, codeUserDataTooBig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, lo := baseRequest(), baseLaunch()
			c.edit(&req, &lo)
			_, err := BuildCreateRequest(req, lo)
			if vendors.ClassOf(err) != c.class || (err != nil && vendors.CodeOf(err) != c.code) {
				t.Fatalf("got %v", err)
			}
		})
	}
	req := baseRequest()
	req.UserData = make([]byte, MaxUserDataBytes)
	if _, err := BuildCreateRequest(req, baseLaunch()); err != nil {
		t.Fatalf("user data at the budget: %v", err)
	}
}

func TestStateOf(t *testing.T) {
	cases := []struct {
		status, power, server, want string
	}{
		{"pending", "running", "none", vendors.StatePending},
		{"active", "running", "ok", vendors.StateRunning},
		{"active", "running", "installingbooting", vendors.StatePending},
		{"active", "running", "none", vendors.StatePending},
		{"active", "stopped", "ok", vendors.StateTerminating},
		{"suspended", "running", "ok", vendors.StateTerminating},
		{"suspended", "stopped", "none", vendors.StateTerminating},
		{"resizing", "running", "ok", vendors.StatePending},
		{"", "", "", vendors.StatePending},
	}
	for _, c := range cases {
		got := StateOf(govultr.Instance{Status: c.status, PowerStatus: c.power, ServerStatus: c.server})
		if got != c.want {
			t.Errorf("%s/%s/%s: %s, want %s", c.status, c.power, c.server, got, c.want)
		}
	}
}

func TestToInstance(t *testing.T) {
	in := govultr.Instance{
		ID: "abc", Region: "ewr", Plan: "vc2-1c-1gb", Status: "active", PowerStatus: "running", ServerStatus: "ok",
		MainIP: "203.0.113.5", V6MainIP: "2001:db8::1", InternalIP: "10.1.0.4", DateCreated: "2026-11-10T12:00:00+00:00",
		Tags:            []string{"scarpenter.sparecores.com/cluster=c", "team=data"},
		DefaultPassword: "secret-password",
	}
	got := ToInstance(in, TagStyleDomain)
	if got.Ref != "ewr/abc" || got.Zone != "ewr" || got.Type != "vc2-1c-1gb" || got.State != vendors.StateRunning ||
		got.CapacityType != vendors.CapacityOnDemand || got.Tags[vendors.TagCluster] != "c" || len(got.Tags) != 1 {
		t.Errorf("got %+v", got)
	}
	if want := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC); !got.CreatedAt.Equal(want) {
		t.Errorf("created %v", got.CreatedAt)
	}
	if len(got.Addresses) != 3 || got.Addresses[0] != "203.0.113.5" {
		t.Errorf("addresses %v", got.Addresses)
	}
	// The default password is never carried, in any field.
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "secret-password") {
		t.Errorf("the password leaked: %s", raw)
	}
	// A pending instance has no address yet.
	in.Status, in.MainIP, in.V6MainIP, in.InternalIP = "pending", "0.0.0.0", "", ""
	if got := ToInstance(in, TagStyleDomain); len(got.Addresses) != 0 || got.State != vendors.StatePending {
		t.Errorf("pending: %+v", got)
	}
}
