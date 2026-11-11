/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

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
	filterImageID     = "image-id"
	filterOwnerID     = "owner-id"
	filterName        = "name"
	filterArch        = "architecture"
	// The request parameters of DescribeImages that `filters` may set.
	paramExecutableUser  = "executable-user"
	paramIncludeDisabled = "include-disabled"
	stateAvailable       = "available"
	stateFailed          = "failed"
	statePending         = "pending"
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
		"map-public-ip-on-launch", "outpost-arn", filterOwnerID,
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
		filterOwnerID, filterTagPattern, filterTagKey, filterVPC,
	}
	imageFilterNames = []string{
		"architecture", "block-device-mapping.delete-on-termination", "block-device-mapping.device-name",
		"block-device-mapping.snapshot-id", "block-device-mapping.volume-size", "block-device-mapping.volume-type",
		"block-device-mapping.encrypted", "boot-mode", "creation-date", filterDescription, "ena-support",
		"free-tier-eligible", "hypervisor", "image-allowed", filterImageID, "image-type",
		"image-watermark.source-image-creation-time", "image-watermark.source-image-id",
		"image-watermark.source-image-region", "image-watermark.watermark-creation-time", "image-watermark.watermark-key",
		"instance-type-specification.supported-instance-type", "instance-type-specification.unsupported-instance-type",
		"is-public", "kernel-id", "manifest-location", filterName, "owner-alias", filterOwnerID, "platform", "product-code",
		"product-code.type", "public-ssm-parameter-name", "ramdisk-id", "root-device-name", "root-device-type",
		"scan-result.critical-fix-available-since", "scan-result.finding-scan-date", "scan-result.high-fix-available-since",
		"scan-result.highest-finding-severity-label", "scan-result.informational-fix-available-since",
		"scan-result.low-fix-available-since", "scan-result.medium-fix-available-since", "source-image-id",
		"source-image-region", "source-instance-id", filterState, "state-reason-code", "state-reason-message",
		"sriov-net-support", filterTagPattern, filterTagKey, "virtualization-type",
		// Request parameters of the call, offered as filters (ExecutableUsers, IncludeDisabled).
		paramExecutableUser, paramIncludeDisabled,
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
		KindImages: {
			HasCreated: true,
			Tags:       vendors.TagsMap,
			Name:       &vendors.FieldCap{Field: filterName, Side: vendors.SideServer},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:      {Field: filterArch, Side: vendors.SideServer},
				vendors.MatchOwner:             {Field: "Owners", Side: vendors.SideServer},
				vendors.MatchState:             {Field: filterState, Side: vendors.SideServer},
				vendors.MatchIncludeDeprecated: {Field: "IncludeDeprecated", Side: vendors.SideServer},
				vendors.MatchDescription:       {Field: filterDescription, Side: vendors.SideServerClient},
				vendors.MatchCreatedAfter:      {Field: "CreationDate", Side: vendors.SideClient},
				vendors.MatchCreatedBefore:     {Field: "CreationDate", Side: vendors.SideClient},
			},
			Filters:       ec2Filters(imageFilterNames),
			DefaultSelect: common.SelectExactlyOne,
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
	// arch is the filter of match.architecture, with the EC2 spelling of the
	// portable names (images).
	arch string
}

var (
	subnetFilters = useFilter{id: "subnet-id", name: "tag:Name", zone: filterZoneID, network: filterVPC,
		cidr: filterCIDR, state: func(s string) []string {
			if s == stateFailed {
				return []string{string(ec2types.SubnetStateUnavailable), string(ec2types.SubnetStateFailed),
					string(ec2types.SubnetStateFailedInsufficientCapacity)}
			}
			return []string{s}
		}}
	groupFilters = useFilter{id: "group-id", name: filterGroupName, network: filterVPC, description: filterDescription}
	imageFilters = useFilter{id: filterImageID, name: filterName, description: filterDescription, arch: filterArch,
		state: func(s string) []string {
			switch s {
			case stateFailed:
				return []string{
					string(ec2types.ImageStateFailed), string(ec2types.ImageStateError), string(ec2types.ImageStateInvalid),
				}
			case statePending:
				return []string{string(ec2types.ImageStatePending), string(ec2types.ImageStateTransient)}
			}
			return []string{s}
		}}
	pgFilters = useFilter{name: filterGroupName, state: func(s string) []string {
		if s == stateFailed {
			return []string{string(ec2types.PlacementGroupStateDeleting), string(ec2types.PlacementGroupStateDeleted)}
		}
		return []string{s}
	}}
)

// useFilters builds the EC2 filter list of a selector: the whole selector the
// vendor can evaluate, tags as tag:<k> or tag-key, the native filters passed
// through. A field EC2 cannot filter on is left to the shared matcher.
//
// Every filter name is sent once: what EC2 does with a name given twice is not
// documented. A name the selector's own fields produce twice (two tags with
// "*", a name and a Name tag) is sent once and the matcher, which re-applies
// every field, enforces the rest; a native filter that names one of them
// replaces it on the wire for the same reason. Native filters given twice
// under one name are ANDed by intersecting their values, which is exact for
// values without wildcards (validation rejects the others). none is true when
// the intersection is empty: nothing can match and EC2 is not asked.
func useFilters(ref vendors.UseRef, f useFilter) (out []ec2types.Filter, none bool, err error) {
	add := func(name string, values ...string) {
		if name == "" || len(values) == 0 || slices.ContainsFunc(out, func(x ec2types.Filter) bool {
			return deref(x.Name) == name
		}) {
			return // the matcher enforces this one
		}
		out = append(out, ec2types.Filter{Name: aws.String(name), Values: values})
	}
	if f.id != "" {
		add(f.id, ref.IDs...)
	}
	if ref.Name != "" {
		add(f.name, ref.Name)
	}
	for _, k := range slices.Sorted(maps.Keys(ref.Tags)) {
		// Tag keys and values are exact; EC2 reads * ? and \ in a filter value
		// as wildcards and an escape, so they are escaped.
		if ref.Tags[k] == "*" {
			add(filterTagKey, ec2Literal(k))
		} else {
			add("tag:"+k, ec2Literal(ref.Tags[k]))
		}
	}
	m := ref.Match
	if f.arch != "" {
		var arches []string
		for _, a := range m.Architecture {
			if v, ok := imageArches[a]; ok {
				arches = append(arches, string(v))
			}
		}
		if len(arches) == len(m.Architecture) { // an unknown one is left to the matcher
			add(f.arch, arches...)
		}
	}
	add(f.zone, m.Zone...)
	add(f.network, m.Network...)
	if v, ok := ec2CIDR(m.CIDR); ok {
		add(f.cidr, v)
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
	native, err := mergeNative(ref.Filters)
	if err != nil {
		return nil, false, err
	}
	for _, fl := range native {
		if len(fl.Values) == 0 {
			return nil, true, nil
		}
		out = slices.DeleteFunc(out, func(x ec2types.Filter) bool { return deref(x.Name) == fl.Name })
		out = append(out, ec2types.Filter{Name: aws.String(fl.Name), Values: fl.Values})
	}
	return out, false, nil
}

// mergeNative ANDs native filters that share a name into one by intersecting
// their values (an empty intersection matches nothing). That is exact only for
// values without wildcards: a repeated name with a wildcard value is an error.
func mergeNative(fs []common.ResourceFilter) ([]common.ResourceFilter, error) {
	var out []common.ResourceFilter
	for _, fl := range fs {
		i := slices.IndexFunc(out, func(x common.ResourceFilter) bool { return x.Name == fl.Name })
		if i < 0 {
			out = append(out, common.ResourceFilter{Name: fl.Name, Values: slices.Clone(fl.Values)})
			continue
		}
		if slices.ContainsFunc(out[i].Values, selector.HasWildcard) || slices.ContainsFunc(fl.Values, selector.HasWildcard) {
			return nil, fmt.Errorf("the filter %s is given twice with a wildcard value: EC2 takes each filter name "+
				"once, so give it once", fl.Name)
		}
		out[i].Values = slices.DeleteFunc(out[i].Values, func(v string) bool { return !slices.Contains(fl.Values, v) })
	}
	return out, nil
}

// ec2Literal escapes the wildcards of an EC2 filter value, so that EC2 compares
// it exactly, as the shared matcher does.
func ec2Literal(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`).Replace(s)
}

// ec2CIDR is the value of the cidr-block filter for match.cidr: the IPv4 range
// in EC2's canonical spelling (10.0.0.1/16 is the range 10.0.0.0/16, which the
// matcher compares on parsed prefixes). An IPv6 range is not sent (cidr-block is
// the IPv4 block): the matcher compares it with the subnet's IPv6 blocks.
func ec2CIDR(text string) (string, bool) {
	p, err := netip.ParsePrefix(text)
	if err != nil || !p.Addr().Is4() {
		return "", false
	}
	return p.Masked().String(), true
}

// imageFields reads what the matcher re-applies of an AMI. The owner is left
// out: it is an account ID, an alias such as self or amazon, or the owner of a
// shared image, so only EC2 can say whether it matches.
func imageFields(im ec2types.Image) selector.Fields {
	arch := ""
	for k, v := range imageArches {
		if v == im.Architecture {
			arch = k
		}
	}
	state := stateAvailable
	switch im.State {
	case ec2types.ImageStatePending, ec2types.ImageStateTransient:
		state = statePending
	case ec2types.ImageStateFailed, ec2types.ImageStateError, ec2types.ImageStateInvalid, ec2types.ImageStateDeregistered:
		state = stateFailed
	}
	attrs := map[string][]string{vendors.MatchState: {state}, vendors.MatchDescription: {deref(im.Description)}}
	if arch != "" {
		attrs[vendors.MatchArchitecture] = []string{arch}
	}
	f := selector.Fields{ID: deref(im.ImageId), Name: deref(im.Name), Tags: tagMap(im.Tags), Attrs: attrs}
	if t, err := time.Parse(time.RFC3339, deref(im.CreationDate)); err == nil {
		f.Created = t
	}
	return f
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
	switch s.State {
	case ec2types.SubnetStateUnavailable, ec2types.SubnetStateFailed, ec2types.SubnetStateFailedInsufficientCapacity:
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
