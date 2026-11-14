/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The native filters of the network kinds: the parameters of their List calls.
const (
	filterVCN         = "vcn-id"
	filterLifecycle   = "lifecycle-state"
	filterDisplayName = "display-name"
)

func serverFilters(names ...string) []vendors.FilterCap {
	out := make([]vendors.FilterCap, len(names))
	for i, n := range names {
		out[i] = vendors.FilterCap{Name: n, Side: vendors.SideServer}
	}
	return out
}

// Selectors implements vendors.ManagedResources (plan 25, section 4): what each kind's `use`
// accepts. Tags are the freeform tags, matched on the client (OCI has no tag filter); the name
// is exact on the server and a glob on the client; nothing has an expression.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	name := &vendors.FieldCap{Field: "displayName", Side: vendors.SideServerClient}
	created := vendors.FieldCap{Field: "timeCreated", Side: vendors.SideClient}
	state := vendors.FieldCap{Field: "lifecycleState", Side: vendors.SideClient}
	network := vendors.FieldCap{Field: "vcnId", Side: vendors.SideServerClient}
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindSubnets: {
			Zonal: true, HasCreated: true, Tags: vendors.TagsMap, Name: name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork:       network,
				vendors.MatchCIDR:          {Field: "cidrBlock", Side: vendors.SideClient},
				vendors.MatchWithin:        {Field: "cidrBlock", Side: vendors.SideClient},
				vendors.MatchState:         state,
				vendors.MatchZone:          {Field: "availabilityDomain", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  created,
				vendors.MatchCreatedBefore: created,
			},
			Filters:       serverFilters(filterVCN, filterLifecycle, filterDisplayName),
			DefaultSelect: common.SelectExactlyOne,
		},
		KindNetworkSecurityGroups: {
			HasCreated: true, Tags: vendors.TagsMap, Name: name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork:       network,
				vendors.MatchState:         state,
				vendors.MatchCreatedAfter:  created,
				vendors.MatchCreatedBefore: created,
			},
			Filters:       serverFilters(filterVCN, filterLifecycle, filterDisplayName),
			DefaultSelect: common.SelectAll,
		},
		KindImages: {
			HasCreated: true, Tags: vendors.TagsMap, Name: name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:  {Field: "arch", Side: vendors.SideClient},
				vendors.MatchState:         {Field: "lifecycleState", Side: vendors.SideServerClient},
				vendors.MatchCreatedAfter:  created,
				vendors.MatchCreatedBefore: created,
			},
			Filters:       serverFilters(filterOS, filterOSVersion, filterShape, filterState),
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// networkState normalises the lifecycle states of the network objects (available, pending,
// failed).
func networkState(available bool, pending bool) string {
	switch {
	case available:
		return "available"
	case pending:
		return "pending"
	}
	return "failed"
}

func subnetState(s core.SubnetLifecycleStateEnum) string {
	return networkState(s == core.SubnetLifecycleStateAvailable, s == core.SubnetLifecycleStateProvisioning ||
		s == core.SubnetLifecycleStateUpdating)
}

func groupState(s core.NetworkSecurityGroupLifecycleStateEnum) string {
	return networkState(s == core.NetworkSecurityGroupLifecycleStateAvailable,
		s == core.NetworkSecurityGroupLifecycleStateProvisioning)
}

func subnetFields(s core.Subnet) selector.Fields {
	cidrs := []string{deref(s.CidrBlock)}
	if s.Ipv6CidrBlock != nil {
		cidrs = append(cidrs, *s.Ipv6CidrBlock)
	}
	cidrs = append(cidrs, s.Ipv6CidrBlocks...)
	attrs := map[string][]string{
		vendors.MatchNetwork: {deref(s.VcnId)}, vendors.MatchCIDR: cidrs, vendors.MatchState: {subnetState(s.LifecycleState)},
	}
	if s.AvailabilityDomain != nil {
		// A regional subnet has no zone: the attribute is absent, so a match.zone never excludes it.
		attrs[vendors.MatchZone] = []string{ADName(*s.AvailabilityDomain)}
	}
	f := selector.Fields{ID: deref(s.Id), Name: deref(s.DisplayName), Tags: s.FreeformTags, Attrs: attrs}
	if s.TimeCreated != nil {
		f.Created = s.TimeCreated.Time
	}
	return f
}

func groupFields(g core.NetworkSecurityGroup) selector.Fields {
	f := selector.Fields{ID: deref(g.Id), Name: deref(g.DisplayName), Tags: g.FreeformTags, Attrs: map[string][]string{
		vendors.MatchNetwork: {deref(g.VcnId)}, vendors.MatchState: {groupState(g.LifecycleState)}}}
	if g.TimeCreated != nil {
		f.Created = g.TimeCreated.Time
	}
	return f
}
