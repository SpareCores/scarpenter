/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vultr/govultr/v3"
)

var ctx = context.Background()

func status(err error) int {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Status
	}
	return -1
}

func newAccount() *Account {
	a := New()
	a.AddPlan("p1", 1, 1024, 25)
	a.AddRegion("ewr", "p1")
	return a
}

func TestPaginationCursor(t *testing.T) {
	a := newAccount()
	for i := range 25 {
		a.AddRegion(fmt.Sprintf("r%02d", i))
	}
	var seen []string
	opts := &govultr.ListOptions{PerPage: 10}
	pages := 0
	for {
		items, meta, err := a.ListRegions(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range items {
			seen = append(seen, r.ID)
		}
		if meta.Total != 26 {
			t.Fatalf("total %d", meta.Total)
		}
		if meta.Links.Next == "" {
			break
		}
		opts.Cursor = meta.Links.Next
	}
	if len(seen) != 26 || pages != 3 {
		t.Fatalf("%d items on %d pages", len(seen), pages)
	}
	if _, _, err := a.ListRegions(ctx, &govultr.ListOptions{Cursor: "junk"}); status(err) != 400 {
		t.Fatalf("junk cursor: %v", err)
	}
	if items, _, err := a.ListRegions(ctx, &govultr.ListOptions{Cursor: "99"}); err != nil || len(items) != 0 {
		t.Fatalf("past the end: %v %v", items, err)
	}
	// The page size is capped at 500.
	for i := range 600 {
		a.AddForeignInstance("ewr", "p1", fmt.Sprint(i))
	}
	items, meta, _ := a.ListInstances(ctx, &govultr.ListOptions{PerPage: 9999})
	if len(items) != 500 || meta.Links.Next != "500" {
		t.Fatalf("%d items, next %q", len(items), meta.Links.Next)
	}
}

func TestInstanceFiltersAndPasswordNeverListed(t *testing.T) {
	a := newAccount()
	a.AddRegion("fra", "p1")
	a.AddForeignInstance("ewr", "p1", "a=1", "b=2")
	a.AddForeignInstance("fra", "p1", "a=1")
	list := func(o govultr.ListOptions) int {
		items, _, err := a.ListInstances(ctx, &o)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range items {
			if in.DefaultPassword != "" {
				t.Error("the list returned a password")
			}
		}
		return len(items)
	}
	if n := list(govultr.ListOptions{}); n != 2 {
		t.Errorf("all: %d", n)
	}
	if n := list(govultr.ListOptions{Tag: "a=1"}); n != 2 {
		t.Errorf("tag a=1: %d", n)
	}
	if n := list(govultr.ListOptions{Tag: "b=2"}); n != 1 {
		t.Errorf("tag b=2: %d", n)
	}
	if n := list(govultr.ListOptions{Tag: "a"}); n != 0 {
		t.Errorf("a tag filter is a whole-string match: %d", n)
	}
	if n := list(govultr.ListOptions{Region: "fra", Tag: "a=1"}); n != 1 {
		t.Errorf("region and tag: %d", n)
	}
	if n := list(govultr.ListOptions{Label: "foreign"}); n != 2 {
		t.Errorf("label: %d", n)
	}
	if n := list(govultr.ListOptions{MainIP: "198.51.100.7"}); n != 2 {
		t.Errorf("main ip: %d", n)
	}
}

func TestServerDoesNotDeduplicateCreates(t *testing.T) {
	a := newAccount()
	req := &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", Tags: []string{"k=v"}}
	for range 2 {
		if _, err := a.CreateInstance(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.Instances()); n != 2 {
		t.Fatalf("%d instances", n)
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name   string
		req    govultr.InstanceCreateReq
		msg    string
		status int
	}{
		{"region", govultr.InstanceCreateReq{Region: "mars", Plan: "p1"}, MsgInvalidRegion, 400},
		{"plan", govultr.InstanceCreateReq{Region: "ewr", Plan: "nope"}, MsgInvalidPlan, 400},
		{"user data", govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", UserData: "not base64!"}, MsgInvalidUserData, 400},
		{"firewall", govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", FirewallGroupID: "x"}, MsgInvalidFirewall, 400},
		{"vpc", govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", AttachVPC: []string{"x"}}, MsgInvalidVPC, 400},
		{"snapshot", govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", SnapshotID: "x"}, MsgInvalidSnapshot, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAccount()
			_, err := a.CreateInstance(ctx, &c.req)
			var e *Error
			if !errors.As(err, &e) || e.Status != c.status || e.Message != c.msg {
				t.Fatalf("got %v", err)
			}
			if len(a.Instances()) != 0 {
				t.Fatal("an instance was created")
			}
		})
	}
	a := newAccount()
	a.SetAvailable("ewr")
	if _, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1"}); status(err) != 400 {
		t.Fatalf("out of stock: %v", err)
	}
	// A VPC of another region cannot be attached.
	a = newAccount()
	a.AddRegion("fra", "p1")
	v, _ := a.CreateVPC(ctx, &govultr.VPCReq{Region: "fra"})
	if _, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", AttachVPC: []string{v.ID}}); status(err) != 400 {
		t.Fatalf("cross-region VPC: %v", err)
	}
}

func TestPendingToActive(t *testing.T) {
	a := newAccount()
	in, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", EnableIPv6: new(true)})
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != "pending" || in.MainIP != "0.0.0.0" || in.ServerStatus != "none" || in.V6MainIP == "" {
		t.Fatalf("pending: %+v", in)
	}
	a.Advance()
	got, _ := a.GetInstance(ctx, in.ID)
	if got.Status != "active" || got.PowerStatus != "running" || got.ServerStatus != "ok" || got.MainIP == "0.0.0.0" {
		t.Fatalf("active: %+v", got)
	}
}

func TestFirewallGroupLifecycle(t *testing.T) {
	a := newAccount()
	g, err := a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "d"})
	if err != nil || g.MaxRuleCount != 50 {
		t.Fatalf("%+v %v", g, err)
	}
	in, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", FirewallGroupID: g.ID})
	if err != nil {
		t.Fatal(err)
	}
	err = a.DeleteFirewallGroup(ctx, g.ID)
	var e *Error
	if !errors.As(err, &e) || e.Message != MsgGroupInUse || e.Status != 400 {
		t.Fatalf("delete while in use: %v", err)
	}
	if got, _ := a.GetFirewallGroup(ctx, g.ID); got.InstanceCount != 1 {
		t.Fatalf("instance_count %d", got.InstanceCount)
	}
	if err := a.DeleteInstance(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteFirewallGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteFirewallGroup(ctx, g.ID); status(err) != 404 {
		t.Fatalf("second delete: %v", err)
	}
}

func TestFirewallRuleLimit(t *testing.T) {
	a := newAccount()
	g, _ := a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{})
	for i := range MaxRulesPerGroup {
		if _, err := a.CreateFirewallRule(ctx, g.ID, &govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "10.0.0.0", SubnetSize: 8, Port: fmt.Sprint(i + 1)}); err != nil {
			t.Fatalf("rule %d: %v", i, err)
		}
	}
	_, err := a.CreateFirewallRule(ctx, g.ID, &govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "10.0.0.0", SubnetSize: 8})
	var e *Error
	if !errors.As(err, &e) || e.Message != MsgRuleLimit {
		t.Fatalf("the 51st rule: %v", err)
	}
	rules, _, _ := a.ListFirewallRules(ctx, g.ID, &govultr.ListOptions{PerPage: 20})
	if len(rules) != 20 {
		t.Fatalf("%d rules on a page", len(rules))
	}
	if err := a.DeleteFirewallRule(ctx, g.ID, rules[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteFirewallRule(ctx, g.ID, rules[0].ID); status(err) != 404 {
		t.Fatalf("second rule delete: %v", err)
	}
	if got, _ := a.GetFirewallGroup(ctx, g.ID); got.RuleCount != MaxRulesPerGroup-1 {
		t.Fatalf("rule_count %d", got.RuleCount)
	}
	for _, bad := range []govultr.FirewallRuleReq{{IPType: "v5", Protocol: "tcp", Subnet: "x"}, {IPType: "v4", Protocol: "ipx", Subnet: "x"}, {IPType: "v4", Protocol: "tcp"}} {
		if _, err := a.CreateFirewallRule(ctx, g.ID, &bad); status(err) != 400 {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestVPCLifecycle(t *testing.T) {
	a := newAccount()
	v, err := a.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: "d"})
	if err != nil || v.V4Subnet == "" || v.V4SubnetMask == 0 {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := a.CreateVPC(ctx, &govultr.VPCReq{Region: "mars"}); status(err) != 400 {
		t.Fatalf("unknown region: %v", err)
	}
	in, _ := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1", AttachVPC: []string{v.ID}})
	if err := a.DeleteVPC(ctx, v.ID); status(err) != 400 {
		t.Fatalf("delete while attached: %v", err)
	}
	att, _, _ := a.ListVPCAttachments(ctx, v.ID, nil)
	if len(att) != 1 || att[0].LinkedSubscription.ID != in.ID {
		t.Fatalf("attachments %+v", att)
	}
	_ = a.DeleteInstance(ctx, in.ID)
	if err := a.DeleteVPC(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ListVPCAttachments(ctx, v.ID, nil); status(err) != 404 {
		t.Fatalf("attachments of a deleted VPC: %v", err)
	}
}

func TestErrorInjection(t *testing.T) {
	a := newAccount()
	a.FailNext("GetInstance", 2, &Error{Status: 429, Message: MsgRateLimited})
	for range 2 {
		if _, err := a.GetInstance(ctx, "x"); status(err) != 429 {
			t.Fatalf("injected: %v", err)
		}
	}
	if _, err := a.GetInstance(ctx, "x"); status(err) != 404 {
		t.Fatalf("after the injected ones: %v", err)
	}
	a.FailNext("GetInstance", -1, &Error{Message: "text only"})
	for range 3 {
		_, err := a.GetInstance(ctx, "x")
		var e *Error
		if !errors.As(err, &e) || e.HTTPStatus() != 0 || err.Error() != "text only" {
			t.Fatalf("sticky: %v", err)
		}
	}
	a.ClearErrors()
	if _, err := a.GetInstance(ctx, "x"); status(err) != 404 {
		t.Fatalf("after clear: %v", err)
	}
	if a.Calls("GetInstance") != 7 {
		t.Errorf("%d calls", a.Calls("GetInstance"))
	}
}

func TestSnapshotDescriptionFilter(t *testing.T) {
	a := New()
	a.AddSnapshot("flatcar-stable-1", "complete")
	a.AddSnapshot("flatcar-stable-10", "complete")
	count := func() int {
		items, _, _ := a.ListSnapshots(ctx, &govultr.ListOptions{Description: "flatcar-stable-1"})
		return len(items)
	}
	if count() != 1 {
		t.Errorf("exact: %d", count())
	}
	a.SubstringDescriptionFilter = true
	if count() != 2 {
		t.Errorf("substring: %d", count())
	}
	if _, err := a.GetSnapshot(ctx, "missing"); status(err) != 404 {
		t.Errorf("missing snapshot: %v", err)
	}
}

func TestInstanceLimitAndTimeoutAfterCommit(t *testing.T) {
	a := newAccount()
	a.InstanceLimit = 1
	a.TimeoutNextCreates(1)
	if _, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if len(a.Instances()) != 1 {
		t.Fatal("the create did not commit")
	}
	if _, err := a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "p1"}); status(err) != 400 {
		t.Fatalf("over the limit: %v", err)
	}
}
