/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the Vultr driver (plan 20, section 3.2). The keys of
// spec.vendors.vultr.resources.
const (
	// KindFirewallGroups is the firewall group of a NodeClass: account-wide at
	// Vultr, one managed group per NodeClass and region by name.
	KindFirewallGroups vendors.ResourceKind = "firewallGroups"
	// KindNetworks is the VPC of a region.
	KindNetworks vendors.ResourceKind = "networks"
	// KindImages is the boot image, a kind that only accepts `use` (plan 14 A1):
	// the snapshot, global in the account. It is not one of Kinds and Resolve
	// does not serve it; ResolveImage does.
	KindImages = selector.ImagesKind
)

// The names of the response fields the selectors map to.
const (
	fieldDescription = "description"
	fieldDateCreated = "date_created"
)

// Normalised states of a snapshot (the matcher's vocabulary).
const (
	stateAvailable = "available"
	statePending   = "pending"
	stateFailed    = "failed"
)

// The native filters of the kinds: response fields, all matched by the client
// (Vultr has no filter language; plan 20, resource-selectors.md).
var (
	snapshotFilters = []string{"os_id", "app_id", "size", "compressed_size"}
	groupFilters    = []string{"instance_count", "rule_count"}
)

func clientFilters(names []string) []vendors.FilterCap {
	out := make([]vendors.FilterCap, len(names))
	for i, n := range names {
		out[i] = vendors.FilterCap{Name: n, Side: vendors.SideClient}
	}
	return out
}

// Selectors implements vendors.ManagedResources. Vultr lists take no filter
// language: a hint such as the snapshot's description parameter is re-checked, and
// everything else is the client's. Snapshots, firewall groups and VPCs carry a
// description and no tags, so `tags` is rejected on every kind.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindImages: {
			Global:     true,
			HasCreated: true,
			Name:       &vendors.FieldCap{Field: fieldDescription, Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture: {Field: "", Side: vendors.SideClient},
				vendors.MatchState:        {Field: "status", Side: vendors.SideClient},
				vendors.MatchCreatedAfter: {Field: fieldDateCreated, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {
					Field: fieldDateCreated, Side: vendors.SideClient,
				},
			},
			Filters:       clientFilters(snapshotFilters),
			DefaultSelect: common.SelectExactlyOne,
		},
		KindFirewallGroups: {
			Global:     true,
			HasCreated: true,
			Name:       &vendors.FieldCap{Field: fieldDescription, Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchCreatedAfter:  {Field: fieldDateCreated, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: fieldDateCreated, Side: vendors.SideClient},
			},
			Filters:       clientFilters(groupFilters),
			DefaultSelect: common.SelectExactlyOne,
		},
		KindNetworks: {
			HasCreated: true,
			Name:       &vendors.FieldCap{Field: fieldDescription, Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchCIDR:          {Field: "v4_subnet", Side: vendors.SideClient},
				vendors.MatchWithin:        {Field: "v4_subnet", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  {Field: fieldDateCreated, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: fieldDateCreated, Side: vendors.SideClient},
			},
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// snapshotState normalises a snapshot's status: complete is available, pending
// is pending, anything else (failed, deleted) is never available.
func snapshotState(status string) string {
	switch status {
	case "complete":
		return stateAvailable
	case "pending":
		return statePending
	}
	return stateFailed
}

func snapshotFields(s govultr.Snapshot) selector.Fields {
	return selector.Fields{
		ID: s.ID, Name: s.Description, Created: parseTime(s.DateCreated),
		Attrs: map[string][]string{
			vendors.MatchState:        {snapshotState(s.Status)},
			vendors.MatchArchitecture: {archAMD64}, // Vultr's cloud plans are x86_64 only
		},
	}
}

func groupFields(g govultr.FirewallGroup) selector.Fields {
	return selector.Fields{ID: g.ID, Name: g.Description, Created: parseTime(g.DateCreated)}
}

func vpcFields(v govultr.VPC) selector.Fields {
	f := selector.Fields{ID: v.ID, Name: v.Description, Created: parseTime(v.DateCreated), Attrs: map[string][]string{}}
	if v.V4Subnet != "" {
		if a, err := netip.ParseAddr(v.V4Subnet); err == nil {
			f.Attrs[vendors.MatchCIDR] = []string{netip.PrefixFrom(a, v.V4SubnetMask).String()}
		}
	}
	return f
}

// snapshotValue reads a native filter name off a snapshot.
func snapshotValue(s govultr.Snapshot, name string) (string, bool) {
	switch name {
	case "os_id":
		return strconv.Itoa(s.OsID), true
	case "app_id":
		return strconv.Itoa(s.AppID), true
	case "size":
		return strconv.FormatInt(s.Size, 10), true
	case "compressed_size":
		return strconv.FormatInt(s.CompressedSize, 10), true
	}
	return "", false
}

func groupValue(g govultr.FirewallGroup, name string) (string, bool) {
	switch name {
	case "instance_count":
		return strconv.Itoa(g.InstanceCount), true
	case "rule_count":
		return strconv.Itoa(g.RuleCount), true
	}
	return "", false
}

// byFilters keeps the items that pass every native filter: a filter's values are
// ORed, filters of different names are ANDed, filters of one name are ANDed too
// (two entries must both hold). Values are exact strings.
func byFilters[T any](items []T, fs []common.ResourceFilter, value func(T, string) (string, bool)) []T {
	if len(fs) == 0 {
		return items
	}
	return slices.DeleteFunc(slices.Clone(items), func(it T) bool {
		for _, f := range fs {
			v, ok := value(it, f.Name)
			if !ok || !slices.Contains(f.Values, v) {
				return true
			}
		}
		return false
	})
}
