/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package vultr is the lower layers of the Vultr driver (plan 20): the SDK
// client behind the API interface, an in-memory fake of it (package fake), the
// error classification, and the pure mapping code (provider IDs, tags, plan
// families, the launch request, the instance state, firewall rules, resource
// descriptions). The vendors.Driver itself, its registration and the NodeClass
// API plug in on top of Ops once plan 14 has landed (see the TODO(plan 14)
// seams).
package vultr

import (
	"context"

	"github.com/vultr/govultr/v3"
)

// Vendor is the Keeper vendor ID.
const Vendor = "vultr"

// API is the part of the Vultr API the driver calls, in the shape of govultr's
// methods without the *http.Response: the HTTP status of a failed call travels
// in the error (see HTTPStatus). It is narrow on purpose: the in-memory fake
// (pkg/vendors/vultr/fake) implements exactly this, and so does the real client
// (NewClient). Vultr has one global endpoint, so there is one client and no
// per-region clients.
type API interface {
	ListRegions(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Region, *govultr.Meta, error)
	// RegionAvailability lists the plans in stock in a region.
	RegionAvailability(ctx context.Context, regionID, planType string) (*govultr.PlanAvailability, error)
	ListPlans(ctx context.Context, planType string, opts *govultr.ListOptions) ([]govultr.Plan, *govultr.Meta, error)

	ListSnapshots(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Snapshot, *govultr.Meta, error)
	GetSnapshot(ctx context.Context, id string) (*govultr.Snapshot, error)

	CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error)
	GetInstance(ctx context.Context, id string) (*govultr.Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	ListInstances(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Instance, *govultr.Meta, error)

	CreateFirewallGroup(ctx context.Context, req *govultr.FirewallGroupReq) (*govultr.FirewallGroup, error)
	GetFirewallGroup(ctx context.Context, id string) (*govultr.FirewallGroup, error)
	DeleteFirewallGroup(ctx context.Context, id string) error
	ListFirewallGroups(ctx context.Context, opts *govultr.ListOptions) ([]govultr.FirewallGroup, *govultr.Meta, error)
	CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (*govultr.FirewallRule, error)
	ListFirewallRules(
		ctx context.Context, groupID string, opts *govultr.ListOptions,
	) ([]govultr.FirewallRule, *govultr.Meta, error)
	DeleteFirewallRule(ctx context.Context, groupID string, ruleID int) error

	CreateVPC(ctx context.Context, req *govultr.VPCReq) (*govultr.VPC, error)
	GetVPC(ctx context.Context, id string) (*govultr.VPC, error)
	DeleteVPC(ctx context.Context, id string) error
	ListVPCs(ctx context.Context, opts *govultr.ListOptions) ([]govultr.VPC, *govultr.Meta, error)
	ListVPCAttachments(
		ctx context.Context, vpcID string, opts *govultr.ListOptions,
	) ([]govultr.VPCAttachment, *govultr.Meta, error)
}

// Plan types of the list calls.
const planTypeAll = "all"
