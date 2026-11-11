/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// The resource kinds of the UpCloud driver (plan 19). The first three are the
// keys of spec.vendors.upcloud.resources that SCarpenter may create; images
// accepts `use` only (rung 3 of ADR-021).
const (
	// KindNetworks is an SDN private network of a zone: a /24 with DHCP, no router.
	KindNetworks vendors.ResourceKind = "networks"
	// KindFirewalls is an SDN firewall ruleset (stateful), attached to the private
	// interface of the nodes. The ruleset is account-wide, one managed ruleset per
	// zone by name.
	KindFirewalls vendors.ResourceKind = "firewalls"
	// KindServerGroups is an anti-affinity group of a run key and profile.
	KindServerGroups vendors.ResourceKind = "serverGroups"
	// KindImages is the boot template: a kind that only accepts `use` (plan 14 A1),
	// so it is not one of Kinds and Resolve does not serve it; ResolveImage does.
	KindImages = selector.ImagesKind
)

// Selectors implements vendors.ManagedResources: the `use` selector of the kinds
// a NodeClass can bring (resource-selectors.md, upcloud). UpCloud filters nothing
// by name, zone or range on the server and the lists carry no state worth a
// filter, so everything but the label filter of networks and server groups is
// evaluated on the client; the shared matcher applies every field to what the
// list returned.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindImages: {
			Zonal: true,
			// HasCreated: the storage API's created field is documented for backups;
			// whether a template carries it is unverified (S1). Without it orderBy
			// created finds nothing, and label:flatcar-version is the order to use.
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			Name:       &vendors.FieldCap{Field: "title", Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:  {Field: "label " + LabelFlatcarArch, Side: vendors.SideClient},
				vendors.MatchState:         {Field: "state", Side: vendors.SideClient},
				vendors.MatchZone:          {Field: "zone", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  {Field: "created", Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: "created", Side: vendors.SideClient},
			},
			Filters: []vendors.FilterCap{
				{Name: FilterTier, Side: vendors.SideClient},
				{Name: FilterTemplateType, Side: vendors.SideClient},
			},
			DefaultSelect: common.SelectExactlyOne,
		},
		KindNetworks: {
			Zonal: true,
			Tags:  vendors.TagsLabels,
			Name:  &vendors.FieldCap{Field: "name", Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchZone:   {Field: "zone", Side: vendors.SideClient},
				vendors.MatchCIDR:   {Field: "ip_networks.address", Side: vendors.SideClient},
				vendors.MatchWithin: {Field: "ip_networks.address", Side: vendors.SideClient},
			},
			Filters:       []vendors.FilterCap{{Name: FilterType, Side: vendors.SideClient}},
			DefaultSelect: common.SelectExactlyOne,
		},
		KindFirewalls: {
			Global:        true,
			Tags:          vendors.TagsLabels,
			Name:          &vendors.FieldCap{Field: "name", Side: vendors.SideClient},
			DefaultSelect: common.SelectExactlyOne,
		},
		KindServerGroups: {
			Global:        true,
			Tags:          vendors.TagsLabels,
			Name:          &vendors.FieldCap{Field: "title", Side: vendors.SideClient},
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

func networkFields(n upcloud.Network) selector.Fields {
	cidrs := make([]string, 0, len(n.IPNetworks))
	for _, in := range n.IPNetworks {
		cidrs = append(cidrs, in.Address)
	}
	attrs := map[string][]string{vendors.MatchZone: {n.Zone}}
	if len(cidrs) > 0 {
		attrs[vendors.MatchCIDR] = cidrs
	}
	return selector.Fields{ID: n.UUID, Name: n.Name, Tags: labelMap(n.Labels), Attrs: attrs}
}

func rulesetFields(r api.Ruleset) selector.Fields {
	return selector.Fields{ID: r.UUID, Name: r.Name, Tags: labelMap(r.Labels)}
}

func groupFields(g upcloud.ServerGroup) selector.Fields {
	return selector.Fields{ID: g.UUID, Name: g.Title, Tags: labelMap(g.Labels)}
}

// networkTypes are the types a `use` of networks lists: private by default (the
// list also holds the account's utility and public networks), or the values of
// the native filter `type`.
func networkTypes(fs []common.ResourceFilter) []string {
	for _, f := range fs {
		if f.Name == FilterType {
			return f.Values
		}
	}
	return []string{upcloud.NetworkTypePrivate}
}

// labelQueryFilters turns the exact tags of a selector into the label filters of
// the list calls: a pair as key and value, the value "*" as a key that exists.
// The matcher re-applies every tag to the answer, so a server that ignores a
// filter cannot return a wrong match.
func labelQueryFilters(tags map[string]string) []request.QueryFilter {
	out := make([]request.QueryFilter, 0, len(tags))
	for _, k := range sortedKeys(tags) {
		if tags[k] == "*" {
			out = append(out, request.FilterLabelKey{Key: k})
			continue
		}
		out = append(out, request.FilterLabel{Label: upcloud.Label{Key: k, Value: tags[k]}})
	}
	return out
}
