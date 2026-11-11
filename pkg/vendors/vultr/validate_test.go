/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	vultrapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

const uuid1 = "5a000000-0000-4000-8000-000000000001"

func vultrClass(mut func(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings)) *v1alpha1.SCNodeClass {
	nc := nodeClass(v1alpha1.SCNodeClassSpec{})
	b := &vultrapi.Settings{}
	nc.Spec.Vendors.Vultr = b
	if mut != nil {
		mut(nc, b)
	}
	return nc
}

func withResources(r vultrapi.Resources) func(*v1alpha1.SCNodeClass, *vultrapi.Settings) {
	return func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) { b.Resources = &r }
}

func TestValidateNodeClass(t *testing.T) {
	no, yes := false, true
	isolation := func(i common.Isolation) func(*v1alpha1.SCNodeClass, *vultrapi.Settings) {
		return func(nc *v1alpha1.SCNodeClass, _ *vultrapi.Settings) { nc.Spec.Networking.Isolation = i }
	}
	ids := func(k string, id ...string) *common.Resource {
		return &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{k: id}}}
	}
	many := make([]common.InboundRule, 0, 40)
	for i := range 20 {
		many = append(many, common.InboundRule{Protocol: "any", CIDRs: []string{fmt.Sprintf("10.%d.0.0/16", i), fmt.Sprintf("10.%d.0.0/17", i)}})
	}
	cases := []struct {
		name string
		mut  func(*v1alpha1.SCNodeClass, *vultrapi.Settings)
		want string // empty: valid
	}{
		{"defaults", nil, ""},
		{"regions", func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) { b.Regions = []string{"ewr", "fra"} }, ""},
		{"a bad region", func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) { b.Regions = []string{"us-east-1"} }, "not a Vultr region ID"},
		{"no public IPv4", func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			b.Networking = &common.NetworkingOverride{PublicIPv4: &no}
		}, "publicIPv4: false"},
		{"public IPv4 on", func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			b.Networking = &common.NetworkingOverride{PublicIPv4: &yes}
		}, ""},
		{"too many firewall rules", func(_ *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			b.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: many}}
		}, "firewall rules"},
		{"a bad CIDR", func(nc *v1alpha1.SCNodeClass, _ *vultrapi.Settings) {
			nc.Spec.Networking.Inbound = &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"nope"}}}}
		}, "not a CIDR"},
		{"isolation none with a VPC", func(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			isolation(common.IsolationNone)(nc, b)
			withResources(vultrapi.Resources{Networks: ids("ewr", uuid1)})(nc, b)
		}, "isolation none attaches no VPC"},
		{"isolation none, a created VPC", func(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			isolation(common.IsolationNone)(nc, b)
			withResources(vultrapi.Resources{Networks: &common.Resource{Create: &common.ResourceCreate{}}})(nc, b)
		}, "isolation none attaches no VPC"},
		{"isolation nodeclass with a VPC", func(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			isolation(common.IsolationNodeClass)(nc, b)
			withResources(vultrapi.Resources{Networks: ids("ewr", uuid1)})(nc, b)
		}, ""},
		{"the default isolation forbids the VPC", withResources(vultrapi.Resources{Networks: &common.Resource{Forbid: true}}), "forbid contradicts"},
		{"isolation none forbids the VPC", func(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings) {
			isolation(common.IsolationNone)(nc, b)
			withResources(vultrapi.Resources{Networks: &common.Resource{Forbid: true}})(nc, b)
		}, ""},
		{"snapshot and group IDs", withResources(vultrapi.Resources{Images: ids("*", uuid1), FirewallGroups: ids("*", uuid1)}), ""},
		{"a bad snapshot ID", withResources(vultrapi.Resources{Images: ids("*", "snap-1")}), "not a valid ID"},
		{"a bad ID key", withResources(vultrapi.Resources{Images: ids("us-east-1", uuid1)}), "not a region"},
		{"a placeholder ID", withResources(vultrapi.Resources{Images: ids("*", "{channel}")}), ""},
		{"tags are rejected", withResources(vultrapi.Resources{FirewallGroups: &common.Resource{Use: &common.ResourceUse{
			Tags: map[string]string{"a": "b"}}}}), "tags are not supported"},
		{"an unknown filter", withResources(vultrapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
			Filters: []common.ResourceFilter{{Name: "tag", Values: []string{"x"}}}}}}), "not a images filter"},
		{"arm64 images", withResources(vultrapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{Name: "x",
			Match: &common.ResourceMatch{Architecture: []string{"arm64"}}}}}), "no Arm instances"},
		{"the owner is not a field", withResources(vultrapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
			Match: &common.ResourceMatch{Owner: []string{"self"}}}}}), "match.owner is not supported"},
		{"a name for the VPC", withResources(vultrapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{Name: "private-*"}}}), ""},
		{"tags with an equals sign", func(nc *v1alpha1.SCNodeClass, _ *vultrapi.Settings) {
			nc.Spec.Tags = map[string]string{"a=b": "c"}
		}, "not a valid key"},
		{"a reserved tag", func(nc *v1alpha1.SCNodeClass, _ *vultrapi.Settings) {
			nc.Spec.Tags = map[string]string{"scarpenter.sparecores.com/cluster": "x"}
		}, "reserved"},
		{"tags", func(nc *v1alpha1.SCNodeClass, _ *vultrapi.Settings) {
			nc.Spec.Tags = map[string]string{"team": "infra"}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDrvEnv(t)
			err := e.d.ValidateNodeClass(context.Background(), vultrClass(c.mut))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestValidateNodeClassTooManyTags(t *testing.T) {
	e := newDrvEnv(t)
	nc := vultrClass(nil)
	nc.Spec.Tags = map[string]string{}
	for i := range 41 {
		nc.Spec.Tags[fmt.Sprintf("k%d", i)] = "v"
	}
	if err := e.d.ValidateNodeClass(context.Background(), nc); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateNodeClassChecksTheKey(t *testing.T) {
	ctx := context.Background()
	e := newDrvEnv(t)
	nc := vultrClass(nil)

	// A refused key (an allowlist that lacks the controller's address) is a
	// verdict, cached for a minute and then looked at again.
	e.acct.FailNext("ListInstances", 1, &fake.Error{Status: 401, Message: "Unauthorized IP address."})
	err := e.d.ValidateNodeClass(ctx, nc)
	if err == nil || !strings.Contains(err.Error(), "VULTR_API_KEY") || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("a refused key: %v", err)
	}
	calls := e.acct.Calls("ListInstances")
	if err := e.d.ValidateNodeClass(ctx, nc); err == nil || e.acct.Calls("ListInstances") != calls {
		t.Fatalf("the verdict is not cached: %v", err)
	}
	e.clk.Step(2 * time.Minute)
	if err := e.d.ValidateNodeClass(ctx, nc); err != nil {
		t.Fatalf("the key works again: %v", err)
	}
	if err := e.d.ValidateNodeClass(ctx, nc); err != nil || e.acct.Calls("ListInstances") != calls+1 {
		t.Fatalf("the success is not cached: %v", err)
	}

	// Throttling or an outage says nothing about the key: no verdict, no gate.
	e.clk.Step(10 * time.Minute)
	e.acct.FailNext("ListInstances", 1, &fake.Error{Status: 503, Message: "down"})
	if err := e.d.ValidateNodeClass(ctx, nc); err != nil {
		t.Fatalf("an outage gated the vendor: %v", err)
	}
}

func TestValidateNodeClassWithoutABlockOnlyChecksTheTags(t *testing.T) {
	e := newDrvEnv(t)
	nc := nodeClass(v1alpha1.SCNodeClassSpec{})
	if err := e.d.ValidateNodeClass(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
}
