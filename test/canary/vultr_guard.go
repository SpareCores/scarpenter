/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"strings"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

// The read-only guard of the Vultr canary. Vultr's ACLs are coarse and a key cannot be
// limited to reads of firewall groups, VPCs and snapshots (plan 20, spike S5), so the
// guard is the wall that matters: the driver is given a client that refuses every call
// that is not a Get or a List (and the region's availability, which is a read) before it
// reaches the network. The wrapper implements vultr.API method by method (a method
// added to the interface breaks the build here), and vultr_guard_test.go checks by
// reflection that every method of the interface is either passed through as a read or
// refused. audit and errReadOnly are the AWS guard's.

// isReadVultr reports whether a method of vultr.API is a read.
func isReadVultr(op string) bool {
	return strings.HasPrefix(op, "Get") || strings.HasPrefix(op, "List") || op == "RegionAvailability"
}

// roVultr is the guarded client.
type roVultr struct {
	inner vultr.API
	audit *audit
}

var _ vultr.API = (*roVultr)(nil)

func readOnlyVultr(inner vultr.API, a *audit) vultr.API { return &roVultr{inner: inner, audit: a} }

func (g *roVultr) ListRegions(ctx context.Context, o *govultr.ListOptions) ([]govultr.Region, *govultr.Meta, error) {
	g.audit.read("vultr:ListRegions")
	return g.inner.ListRegions(ctx, o)
}

func (g *roVultr) RegionAvailability(ctx context.Context, region, planType string) (*govultr.PlanAvailability, error) {
	g.audit.read("vultr:RegionAvailability")
	return g.inner.RegionAvailability(ctx, region, planType)
}

func (g *roVultr) ListPlans(
	ctx context.Context, planType string, o *govultr.ListOptions,
) ([]govultr.Plan, *govultr.Meta, error) {
	g.audit.read("vultr:ListPlans")
	return g.inner.ListPlans(ctx, planType, o)
}

func (g *roVultr) ListSnapshots(
	ctx context.Context, o *govultr.ListOptions,
) ([]govultr.Snapshot, *govultr.Meta, error) {
	g.audit.read("vultr:ListSnapshots")
	return g.inner.ListSnapshots(ctx, o)
}

func (g *roVultr) GetSnapshot(ctx context.Context, id string) (*govultr.Snapshot, error) {
	g.audit.read("vultr:GetSnapshot")
	return g.inner.GetSnapshot(ctx, id)
}

func (g *roVultr) CreateInstance(context.Context, *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	return nil, g.audit.deny("vultr:CreateInstance")
}

func (g *roVultr) GetInstance(ctx context.Context, id string) (*govultr.Instance, error) {
	g.audit.read("vultr:GetInstance")
	return g.inner.GetInstance(ctx, id)
}

func (g *roVultr) DeleteInstance(context.Context, string) error {
	return g.audit.deny("vultr:DeleteInstance")
}

func (g *roVultr) ListInstances(
	ctx context.Context, o *govultr.ListOptions,
) ([]govultr.Instance, *govultr.Meta, error) {
	g.audit.read("vultr:ListInstances")
	return g.inner.ListInstances(ctx, o)
}

func (g *roVultr) CreateFirewallGroup(context.Context, *govultr.FirewallGroupReq) (*govultr.FirewallGroup, error) {
	return nil, g.audit.deny("vultr:CreateFirewallGroup")
}

func (g *roVultr) GetFirewallGroup(ctx context.Context, id string) (*govultr.FirewallGroup, error) {
	g.audit.read("vultr:GetFirewallGroup")
	return g.inner.GetFirewallGroup(ctx, id)
}

func (g *roVultr) DeleteFirewallGroup(context.Context, string) error {
	return g.audit.deny("vultr:DeleteFirewallGroup")
}

func (g *roVultr) ListFirewallGroups(
	ctx context.Context, o *govultr.ListOptions,
) ([]govultr.FirewallGroup, *govultr.Meta, error) {
	g.audit.read("vultr:ListFirewallGroups")
	return g.inner.ListFirewallGroups(ctx, o)
}

func (g *roVultr) CreateFirewallRule(
	context.Context, string, *govultr.FirewallRuleReq,
) (*govultr.FirewallRule, error) {
	return nil, g.audit.deny("vultr:CreateFirewallRule")
}

func (g *roVultr) ListFirewallRules(
	ctx context.Context, groupID string, o *govultr.ListOptions,
) ([]govultr.FirewallRule, *govultr.Meta, error) {
	g.audit.read("vultr:ListFirewallRules")
	return g.inner.ListFirewallRules(ctx, groupID, o)
}

func (g *roVultr) DeleteFirewallRule(context.Context, string, int) error {
	return g.audit.deny("vultr:DeleteFirewallRule")
}

func (g *roVultr) CreateVPC(context.Context, *govultr.VPCReq) (*govultr.VPC, error) {
	return nil, g.audit.deny("vultr:CreateVPC")
}

func (g *roVultr) GetVPC(ctx context.Context, id string) (*govultr.VPC, error) {
	g.audit.read("vultr:GetVPC")
	return g.inner.GetVPC(ctx, id)
}

func (g *roVultr) DeleteVPC(context.Context, string) error { return g.audit.deny("vultr:DeleteVPC") }

func (g *roVultr) ListVPCs(ctx context.Context, o *govultr.ListOptions) ([]govultr.VPC, *govultr.Meta, error) {
	g.audit.read("vultr:ListVPCs")
	return g.inner.ListVPCs(ctx, o)
}

func (g *roVultr) ListVPCAttachments(
	ctx context.Context, vpcID string, o *govultr.ListOptions,
) ([]govultr.VPCAttachment, *govultr.Meta, error) {
	g.audit.read("vultr:ListVPCAttachments")
	return g.inner.ListVPCAttachments(ctx, vpcID, o)
}
