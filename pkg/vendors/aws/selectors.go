/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The EC2 filter names and normalised states the selector maps.
const (
	filterVPC         = "vpc-id"
	filterCIDR        = "cidr-block"
	filterTagKey      = "tag-key"
	filterTagPattern  = "tag:*"
	filterZoneID      = "availability-zone-id"
	filterState       = "state"
	filterDescription = "description"
	filterGroupName   = "group-name"
	stateAvailable    = "available"
	stateFailed       = "failed"
)

// Filter names of the EC2 Describe* calls, as the SDK documents them (a test
// checks each against the doc comment of the pinned SDK). A name ending in * is
// a pattern: tag:<key>.
var (
	subnetFilterNames = []string{
		"availability-zone", filterZoneID, "available-ip-address-count", filterCIDR,
		"customer-owned-ipv4-pool", "default-for-az", "enable-dns64", "enable-lni-at-device-index",
		"ipv6-cidr-block-association.ipv6-cidr-block", "ipv6-cidr-block-association.association-id",
		"ipv6-cidr-block-association.state", "ipv6-native", "map-customer-owned-ip-on-launch",
		"map-public-ip-on-launch", "outpost-arn", "owner-id",
		"private-dns-name-options-on-launch.hostname-type",
		"private-dns-name-options-on-launch.enable-resource-name-dns-a-record",
		"private-dns-name-options-on-launch.enable-resource-name-dns-aaaa-record",
		filterState, "subnet-arn", "subnet-id", filterTagPattern, filterTagKey, filterVPC,
	}
	securityGroupFilterNames = []string{
		filterDescription, "egress.ip-permission.cidr", "egress.ip-permission.from-port",
		"egress.ip-permission.group-id", "egress.ip-permission.group-name", "egress.ip-permission.ipv6-cidr",
		"egress.ip-permission.prefix-list-id", "egress.ip-permission.protocol", "egress.ip-permission.to-port",
		"egress.ip-permission.user-id", "group-id", filterGroupName, "ip-permission.cidr", "ip-permission.from-port",
		"ip-permission.group-id", "ip-permission.group-name", "ip-permission.ipv6-cidr",
		"ip-permission.prefix-list-id", "ip-permission.protocol", "ip-permission.to-port", "ip-permission.user-id",
		"owner-id", filterTagPattern, filterTagKey, filterVPC,
	}
	placementGroupFilterNames = []string{
		filterGroupName, "group-arn", "spread-level", filterState, "strategy", filterTagPattern, filterTagKey,
	}
)

func ec2Filters(names []string) []vendors.FilterCap {
	out := make([]vendors.FilterCap, len(names))
	for i, n := range names {
		out[i] = vendors.FilterCap{Name: n, Side: vendors.SideServer}
	}
	return out
}

// Selectors implements vendors.ManagedResources: the `use` selector of the
// kinds a NodeClass can bring (resource-selectors.md, aws). EC2 evaluates the
// fields it has a filter for; wildcards in names are EC2's `*` and `?` and the
// shared matcher re-applies them to the answer.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindSubnets: {
			Zonal: true,
			Tags:  vendors.TagsMap,
			Name:  &vendors.FieldCap{Field: "tag:Name", Side: vendors.SideServer},
			Match: map[string]vendors.FieldCap{
				vendors.MatchZone:    {Field: filterZoneID, Side: vendors.SideServer},
				vendors.MatchNetwork: {Field: filterVPC, Side: vendors.SideServer},
				vendors.MatchCIDR:    {Field: filterCIDR, Side: vendors.SideServer},
				vendors.MatchState:   {Field: filterState, Side: vendors.SideServer},
				vendors.MatchWithin:  {Field: "CidrBlock", Side: vendors.SideClient},
			},
			Filters:       ec2Filters(subnetFilterNames),
			DefaultSelect: common.SelectAll,
		},
		KindSecurityGroups: {
			Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: filterGroupName, Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork:     {Field: filterVPC, Side: vendors.SideServer},
				vendors.MatchDescription: {Field: filterDescription, Side: vendors.SideServerClient},
			},
			Filters:       ec2Filters(securityGroupFilterNames),
			DefaultSelect: common.SelectAll,
		},
		KindPlacementGroups: {
			Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: filterGroupName, Side: vendors.SideServer},
			Match: map[string]vendors.FieldCap{
				vendors.MatchState: {Field: filterState, Side: vendors.SideServer},
			},
			Filters:       ec2Filters(placementGroupFilterNames),
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// useFilter describes the filters of a kind that carry the fields of a UseRef.
type useFilter struct {
	// id and name are the filter names of the ID and the name; "" means the
	// kind has none (placement groups have no ID filter: the matcher checks
	// the IDs of the full list).
	id, name string
	// state maps a normalised state to the EC2 values.
	state func(string) []string
	// match are the filters of the match attributes: zone, network, cidr,
	// description (a kind without the attribute leaves it empty).
	zone, network, cidr, description string
}

var (
	subnetFilters = useFilter{id: "subnet-id", name: "tag:Name", zone: filterZoneID, network: filterVPC,
		cidr: filterCIDR, state: func(s string) []string { return []string{s} }}
	groupFilters = useFilter{id: "group-id", name: filterGroupName, network: filterVPC, description: filterDescription}
	pgFilters    = useFilter{name: filterGroupName, state: func(s string) []string {
		if s == stateFailed {
			return []string{string(ec2types.PlacementGroupStateDeleting), string(ec2types.PlacementGroupStateDeleted)}
		}
		return []string{s}
	}}
)

// useFilters builds the EC2 filter list of a selector: the whole selector the
// vendor can evaluate, tags as tag:<k> or tag-key, the native filters passed
// through. A field EC2 cannot filter on is left to the shared matcher.
func useFilters(ref vendors.UseRef, f useFilter) []ec2types.Filter {
	var out []ec2types.Filter
	add := func(name string, values ...string) {
		if name != "" && len(values) > 0 {
			out = append(out, ec2types.Filter{Name: aws.String(name), Values: values})
		}
	}
	if f.id != "" {
		add(f.id, ref.IDs...)
	}
	if ref.Name != "" {
		add(f.name, ref.Name)
	}
	for _, k := range slices.Sorted(maps.Keys(ref.Tags)) {
		if ref.Tags[k] == "*" {
			add(filterTagKey, k)
		} else {
			add("tag:"+k, ref.Tags[k])
		}
	}
	m := ref.Match
	add(f.zone, m.Zone...)
	add(f.network, m.Network...)
	if m.CIDR != "" {
		add(f.cidr, m.CIDR)
	}
	if m.Description != "" {
		add(f.description, m.Description)
	}
	if f.state != nil {
		states := m.State
		if len(states) == 0 {
			states = []string{stateAvailable}
		}
		values := make([]string, 0, len(states))
		for _, s := range states {
			values = append(values, f.state(s)...)
		}
		add(filterState, values...)
	}
	for _, fl := range ref.Filters {
		add(fl.Name, fl.Values...)
	}
	return out
}

func tagMap(tags []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[deref(t.Key)] = deref(t.Value)
	}
	return m
}

func subnetFields(s ec2types.Subnet) selector.Fields {
	attrs := map[string][]string{
		vendors.MatchZone:    {deref(s.AvailabilityZoneId)},
		vendors.MatchNetwork: {deref(s.VpcId)},
		vendors.MatchCIDR:    {deref(s.CidrBlock)},
		vendors.MatchState:   {string(s.State)},
	}
	if s.State == ec2types.SubnetStateUnavailable || s.State == ec2types.SubnetStateFailed {
		attrs[vendors.MatchState] = []string{stateFailed}
	}
	for _, a := range s.Ipv6CidrBlockAssociationSet {
		attrs[vendors.MatchCIDR] = append(attrs[vendors.MatchCIDR], deref(a.Ipv6CidrBlock))
	}
	return selector.Fields{ID: deref(s.SubnetId), Name: nameOf(s.Tags), Tags: tagMap(s.Tags), Attrs: attrs}
}

func groupFields(g ec2types.SecurityGroup) selector.Fields {
	attrs := map[string][]string{
		vendors.MatchNetwork:     {deref(g.VpcId)},
		vendors.MatchDescription: {deref(g.Description)},
	}
	return selector.Fields{ID: deref(g.GroupId), Name: deref(g.GroupName), Tags: tagMap(g.Tags), Attrs: attrs}
}

func placementGroupFields(g ec2types.PlacementGroup) selector.Fields {
	state := stateAvailable
	switch g.State {
	case ec2types.PlacementGroupStatePending:
		state = "pending"
	case ec2types.PlacementGroupStateDeleting, ec2types.PlacementGroupStateDeleted:
		state = stateFailed
	}
	return selector.Fields{ID: deref(g.GroupId), Name: deref(g.GroupName), Tags: tagMap(g.Tags),
		Attrs: map[string][]string{vendors.MatchState: {state}}}
}

// VendorArch implements vendors.ArchNamer: the spelling of EC2's architecture
// filter, for {vendorArch}.
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case archAMD64:
		return "x86_64", true
	case archARM64:
		return archARM64, true
	}
	return "", false
}
