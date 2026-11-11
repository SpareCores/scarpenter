/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults implements vendors.NodeOptions (00a section 5.3, layer 1). The vendor
// block has nothing the driver defaults: the root disk is the plan's (not
// configurable: the size is a property of the plan, and a bigger sibling plan is
// forbidden by ADR-017), the public IPv4 address is included in the plan and the
// networking defaults are the generic ones.
func (d *Driver) Defaults() vendors.VendorSettings {
	return vendors.VendorSettings{Raw: json.RawMessage(`{}`)}
}

// RootDiskBytes implements vendors.NodeOptions: unknown per NodeClass, the plan
// decides (RootDiskForType).
func (d *Driver) RootDiskBytes(vendors.VendorSettings) int64 { return 0 }

// RootDiskForType implements vendors.NodeOptions: the plan's disk, read from
// Keeper's storage_size (plan 14 A2).
func (d *Driver) RootDiskForType(_ vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	b := RootDiskForType(it)
	return b, b > 0
}

// Supported implements vendors.NodeOptions: nothing. Vultr has no storage
// features in v1 (block storage volumes are deferred), no NIC features and no
// placement or tenancy choice, so a profile with a `vultr` block makes the
// offerings that select it unavailable with the reason (ADR-017), never a
// silently different node. "default" is always valid and is the core's.
func (d *Driver) Supported(vendors.CatalogType, string, vendors.Profiles) map[string][]string {
	return map[string][]string{}
}

// PriceDelta implements vendors.NodeOptions: zero and complete. The public IPv4
// address is included in the plan price and nothing else is added in v1 (reserved
// IPs and block storage are not used; IPv6 is free on Vultr: unverified, spike
// S7).
func (d *Driver) PriceDelta(vendors.VendorSettings, vendors.Offering) (float64, bool) { return 0, true }

// FirewallGroupParams are the create parameters of a managed firewall group: the
// inbound rules of the settings. They are part of the group's name (the engine
// hashes the parameters), so a changed ruleset never adopts the group of the old
// one.
type FirewallGroupParams struct {
	Inbound *common.Inbound `json:"inbound,omitempty"`
}

// effectiveIsolation is the isolation of the settings: the CRD default is run.
func effectiveIsolation(n common.Networking) common.Isolation {
	if n.Isolation == "" {
		return common.IsolationRun
	}
	return n.Isolation
}

// needsVPC says whether the isolation puts the nodes in a VPC. Vultr has no run
// key (no placement profiles), so a run is the whole NodeClass: run and nodeclass
// both mean the nodes of the NodeClass reach each other privately, which only a
// VPC does. none attaches none.
func needsVPC(n common.Networking) bool {
	return effectiveIsolation(n) != common.IsolationNone
}

// Prepare implements vendors.NodeOptions: the firewall group and, with an
// isolation that needs one, the VPC of the region, through the resource provider
// (the engine decides whether they are brought, or managed). An error of the
// provider is returned as it is.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params, err := json.Marshal(FirewallGroupParams{Inbound: net.Inbound})
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalid, err)
	}
	prep := &vendors.Prepared{}
	groups, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindFirewallGroups, Key: class, Params: params, Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	if len(groups) != 1 {
		return nil, vendors.NewError(vendors.Fatal, codeInvalid, fmt.Errorf(
			"the firewall groups resolved to %d groups; a Vultr instance takes exactly one (select exactlyOne)", len(groups)))
	}
	prep.Resources = append(prep.Resources, groups...)
	if needsVPC(net) {
		vpcs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindNetworks, Key: class, Settings: &req.Settings,
		})
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, vpcs...)
	}
	return prep, nil
}

// launchOptions builds the launch from the request: the snapshot, the prepared
// firewall group and VPCs, and the networking. A request without a prepared
// firewall group (nobody ran Prepare: the tests) launches without one; the core
// always prepares.
func launchOptions(req vendors.CreateRequest) (LaunchOptions, error) {
	net := req.Settings.Networking
	if net.PublicIPv4 != nil && !*net.PublicIPv4 {
		return LaunchOptions{}, noPublicIPv4()
	}
	lo := LaunchOptions{SnapshotID: req.Image.Ref, IPv6: net.PublicIPv6 != nil && *net.PublicIPv6}
	if req.Prepared == nil {
		return lo, nil
	}
	for _, r := range req.Prepared.Resources {
		switch r.Kind {
		case KindFirewallGroups:
			if lo.FirewallGroupID != "" && lo.FirewallGroupID != r.ID {
				return LaunchOptions{}, vendors.NewError(vendors.Fatal, codeInvalid,
					fmt.Errorf("two firewall groups were prepared (%s, %s); an instance takes one", lo.FirewallGroupID, r.ID))
			}
			lo.FirewallGroupID = r.ID
		case KindNetworks:
			if !slices.Contains(lo.VPCIDs, r.ID) {
				lo.VPCIDs = append(lo.VPCIDs, r.ID)
			}
		}
	}
	return lo, nil
}

// noPublicIPv4 is the Unsupported error of publicIPv4: false: a node without a
// public address needs vpc_only and a NAT gateway, which v1 does not manage.
func noPublicIPv4() error {
	return vendors.NewError(vendors.Unsupported, codeNoPublicIPv4Off, fmt.Errorf(
		"publicIPv4: false is not supported on %s in v1: it needs a VPC-only instance behind a NAT gateway", Vendor))
}

// BillingRule implements vendors.BillingDescriber. Vultr bills a started hour
// (the minimum billing unit is one hour; charges begin at deploy and a powered-off
// server is still charged until it is destroyed), the same as Keeper's vendor row
// (https://docs.vultr.com/products/compute/cloud-compute/billing). Whether the
// started hour is billed in full on destroy, whether the period counts from the
// start rather than from the clock hour, and how far date_created lies from the
// billing start (a later stamp is the unsafe side) are unverified until spike S7
// measures them, so the rule is reported unverified and billing retention stays
// off for Vultr (ADR-024, plan 23).
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	hour, charged := time.Hour, true
	return vendors.BillingRule{
		Increment: &hour, Minimum: &hour, StoppedCharged: &charged, Verified: false,
		Source: "Vultr billing documentation: hourly, one hour minimum; not verified live (plan 20, spike S7)",
	}, true
}
