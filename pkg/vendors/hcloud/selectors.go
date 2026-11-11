/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the driver (plan 15, "Resource kinds").
const (
	// KindImages are the Flatcar snapshots (not managed: use only).
	KindImages = selector.ImagesKind
	// KindNetworks are private networks (not managed in v1: use only).
	KindNetworks vendors.ResourceKind = "networks"
	// KindFirewalls are firewalls: brought, or one managed deny-inbound firewall
	// per location.
	KindFirewalls vendors.ResourceKind = "firewalls"
	// KindPlacementGroups are spread placement groups, one per run key.
	KindPlacementGroups vendors.ResourceKind = "placementGroups"
	// KindVolumes are the data volumes, one per node and storage.data entry
	// (internal: no block key, no use).
	KindVolumes vendors.ResourceKind = "volumes"
)

// The image filters of GET /images the use block may name.
const (
	filterType   = "type"
	filterStatus = "status"
	filterBound  = "bound_to"
)

// Selectors implements vendors.ManagedResources (ADR-023): what each kind
// supports of the `use` selector and who evaluates it. Every kind is
// project-wide and listed with one call; `name` is exact on the server (a glob is
// finished client-side) and `tags` and `expression` are the one label selector.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindImages: {
			Global:     true,
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			// Snapshots have no name: it matches system images only.
			Name: &vendors.FieldCap{Field: "name", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:      {Field: "architecture", Side: vendors.SideServer},
				vendors.MatchState:             {Field: "status", Side: vendors.SideServer},
				vendors.MatchIncludeDeprecated: {Field: "include_deprecated", Side: vendors.SideServer},
				vendors.MatchDescription:       {Field: "description", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:      {Field: "created", Side: vendors.SideClient},
				vendors.MatchCreatedBefore:     {Field: "created", Side: vendors.SideClient},
			},
			Filters: []vendors.FilterCap{
				{Name: filterType, Side: vendors.SideServer},
				{Name: filterStatus, Side: vendors.SideServer},
				{Name: filterBound, Side: vendors.SideServer},
			},
			Expression:    labelSelectorLanguage,
			DefaultSelect: common.SelectExactlyOne,
		},
		KindNetworks: {
			// Zonal: {zone} is the location, match.zone the network zone of a subnet.
			Zonal:      true,
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			Name:       &vendors.FieldCap{Field: "name", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchZone:          {Field: "subnets.network_zone", Side: vendors.SideClient},
				vendors.MatchCIDR:          {Field: "ip_range", Side: vendors.SideClient},
				vendors.MatchWithin:        {Field: "ip_range", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  {Field: "created", Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: "created", Side: vendors.SideClient},
			},
			Expression: labelSelectorLanguage,
			// A server joins one network in v1.
			DefaultSelect: common.SelectExactlyOne,
		},
		KindFirewalls: {
			// A firewall is project-wide: one listing serves every location.
			Global:     true,
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			Name:       &vendors.FieldCap{Field: "name", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchCreatedAfter:  {Field: "created", Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: "created", Side: vendors.SideClient},
			},
			Expression: labelSelectorLanguage,
			// A server can carry several firewalls.
			DefaultSelect: common.SelectAll,
		},
		KindPlacementGroups: {
			Global:     true,
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			Name:       &vendors.FieldCap{Field: "name", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchCreatedAfter:  {Field: "created", Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: "created", Side: vendors.SideClient},
			},
			Expression:    labelSelectorLanguage,
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// labelSelectorLanguage names the language of `expression`: the Hetzner label
// selector (comma is AND; k=v, k!=v, k, !k, k in (a,b), k notin (a,b)).
const labelSelectorLanguage = "label-selector"

// VendorArch implements vendors.ArchNamer: Hetzner's own spelling of the
// architecture, for filters and expressions ({vendorArch}).
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case ArchAMD64:
		return "x86", true
	case ArchARM64:
		return "arm", true
	}
	return "", false
}

// labelSelector builds the label selector of a use block: the `tags` as
// equality terms (key presence for "*"), ANDed with the `expression`. A pair
// that is no valid label (a value with a space) is left to the client-side
// matcher, which re-applies every field anyway.
func labelSelector(ref vendors.UseRef) string {
	var terms []string
	for _, k := range slices.Sorted(maps.Keys(ref.Tags)) {
		v := ref.Tags[k]
		switch {
		case !labelNameKey(k):
		case v == "*":
			terms = append(terms, k)
		case labelValueRE.MatchString(v) && len(v) <= maxLabelValue:
			terms = append(terms, k+"="+v)
		}
	}
	if e := strings.TrimSpace(ref.Expression); e != "" {
		terms = append(terms, e)
	}
	return strings.Join(terms, ",")
}

// labelNameKey reports whether k can stand in a label selector.
func labelNameKey(k string) bool { return ValidLabelKey(k) }

// exactName is the name the API can filter on: only a name without wildcards;
// a glob is matched client-side.
func exactName(name string) string {
	if name == "" || selector.HasWildcard(name) || strings.Contains(name, `\`) {
		return ""
	}
	return name
}

// labelsAsTags are the labels of an object as the matcher's tags.
func labelsAsTags(l map[string]string) map[string]string { return l }

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func imageFields(im *hc.Image) selector.Fields {
	attrs := map[string][]string{
		vendors.MatchState:       {imageState(im.Status)},
		vendors.MatchDescription: {im.Description},
	}
	if a := Arch(im.Architecture); a != "" {
		attrs[vendors.MatchArchitecture] = []string{a}
	}
	if im.Deprecation != nil {
		attrs[vendors.MatchIncludeDeprecated] = []string{"true"}
	}
	return selector.Fields{ID: itoa(im.ID), Name: im.Name, Tags: labelsAsTags(im.Labels), Created: im.Created, Attrs: attrs}
}

// imageStatusUnavailable is the API's status for an image that cannot be used
// (the SDK has no constant for it).
const imageStatusUnavailable hc.ImageStatus = "unavailable"

// The portable states of an image (available, pending, failed).
func imageState(s hc.ImageStatus) string {
	switch s {
	case hc.ImageStatusAvailable:
		return "available"
	case hc.ImageStatusCreating:
		return "pending"
	}
	return "failed"
}

// imageStatuses are the API statuses for the portable states (default available).
func imageStatuses(states []string) []hc.ImageStatus {
	if len(states) == 0 {
		return []hc.ImageStatus{hc.ImageStatusAvailable}
	}
	var out []hc.ImageStatus
	for _, s := range states {
		switch s {
		case "available":
			out = append(out, hc.ImageStatusAvailable)
		case "pending":
			out = append(out, hc.ImageStatusCreating)
		case "failed":
			out = append(out, imageStatusUnavailable)
		}
	}
	return out
}

func networkFields(n *hc.Network) selector.Fields {
	attrs := map[string][]string{}
	for _, sn := range n.Subnets {
		attrs[vendors.MatchZone] = append(attrs[vendors.MatchZone], string(sn.NetworkZone))
	}
	if n.IPRange != nil {
		attrs[vendors.MatchCIDR] = []string{n.IPRange.String()}
	}
	return selector.Fields{ID: itoa(n.ID), Name: n.Name, Tags: labelsAsTags(n.Labels), Created: n.Created, Attrs: attrs}
}

func firewallFields(f *hc.Firewall) selector.Fields {
	return selector.Fields{ID: itoa(f.ID), Name: f.Name, Tags: labelsAsTags(f.Labels), Created: f.Created}
}

func placementGroupFields(g *hc.PlacementGroup) selector.Fields {
	return selector.Fields{ID: itoa(g.ID), Name: g.Name, Tags: labelsAsTags(g.Labels), Created: g.Created}
}
