/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// ExpressionLanguage is the language of `expression`: the filter string of every
// Compute list call (AIP-160, or RE2 with eq and ne; the two cannot be mixed in
// one request).
const ExpressionLanguage = "gcp-filter"

func serverFilters(names ...string) []vendors.FilterCap {
	out := make([]vendors.FilterCap, len(names))
	for i, n := range names {
		out[i] = vendors.FilterCap{Name: n, Side: vendors.SideServer}
	}
	return out
}

// Selectors implements vendors.ManagedResources: the `use` selector of the kinds a
// NodeClass can bring (resource-selectors.md, gcp). The Compute list calls take
// one `filter` string; the driver sends what it can express in it for sure (an
// exact name, the labels of `tags`, the native `filters` as equalities, an
// `expression` as written) and the shared matcher re-applies every field the
// answer carries data for. Networks, subnetworks, firewall rules and resource
// policies have no labels, so their `tags` are rejected.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	name := &vendors.FieldCap{Field: "name", Side: vendors.SideServerClient}
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindSubnets: {
			HasCreated: true,
			Name:       name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork:     {Field: "network", Side: vendors.SideClient},
				vendors.MatchCIDR:        {Field: "ipCidrRange", Side: vendors.SideClient},
				vendors.MatchWithin:      {Field: "ipCidrRange", Side: vendors.SideClient},
				vendors.MatchState:       {Field: "state", Side: vendors.SideClient},
				vendors.MatchDescription: {Field: "description", Side: vendors.SideClient},
				vendors.MatchCreatedAfter: {
					Field: "creationTimestamp", Side: vendors.SideClient,
				},
				vendors.MatchCreatedBefore: {Field: "creationTimestamp", Side: vendors.SideClient},
			},
			Filters: serverFilters("purpose", "role", "stackType", "ipv6AccessType", "ipv6CidrRange",
				"privateIpGoogleAccess", "enableFlowLogs"),
			Expression:    ExpressionLanguage,
			DefaultSelect: common.SelectAll,
		},
		KindFirewalls: {
			Name: &vendors.FieldCap{Field: "targetTags", Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork: {Field: "network", Side: vendors.SideClient},
				vendors.MatchState:   {Field: "disabled", Side: vendors.SideClient},
			},
			Filters:       serverFilters("direction", "priority", "disabled", "logConfig.enable"),
			Expression:    ExpressionLanguage,
			DefaultSelect: common.SelectAll,
		},
		KindResourcePolicies: {
			HasCreated: true,
			Name:       name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchState:       {Field: "status", Side: vendors.SideClient},
				vendors.MatchDescription: {Field: "description", Side: vendors.SideClient},
			},
			Filters: serverFilters("groupPlacementPolicy.collocation", "groupPlacementPolicy.vmCount",
				"groupPlacementPolicy.availabilityDomainCount", "groupPlacementPolicy.gpuTopology"),
			Expression:    ExpressionLanguage,
			DefaultSelect: common.SelectExactlyOne,
		},
		KindImages: {
			Global:     true,
			HasCreated: true,
			Tags:       vendors.TagsLabels,
			Name:       name,
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:      {Field: "architecture", Side: vendors.SideClient},
				vendors.MatchOwner:             {Field: "project", Side: vendors.SideServer},
				vendors.MatchState:             {Field: "status", Side: vendors.SideClient},
				vendors.MatchIncludeDeprecated: {Field: "deprecated.state", Side: vendors.SideClient},
				vendors.MatchDescription:       {Field: "description", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:      {Field: "creationTimestamp", Side: vendors.SideClient},
				vendors.MatchCreatedBefore:     {Field: "creationTimestamp", Side: vendors.SideClient},
			},
			Filters: append(serverFilters("family", "deprecated.state", "sourceType", "diskSizeGb", "id", "licenses",
				"storageLocations", "guestOsFeatures"), vendors.FilterCap{Name: "labels.*", Side: vendors.SideServer}),
			Expression:    ExpressionLanguage,
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// listFilter builds the filter string of a list call for a selector: the
// expression as written, else the exact name and the native filters as AIP-160
// equalities (several values ORed).
func listFilter(use vendors.UseRef) string {
	if use.Expression != "" {
		return use.Expression
	}
	var parts []string
	if use.Name != "" && !selector.HasWildcard(use.Name) {
		parts = append(parts, fmt.Sprintf("(name = %q)", use.Name))
	}
	for _, f := range use.Filters {
		switch len(f.Values) {
		case 0:
		case 1:
			parts = append(parts, fmt.Sprintf("(%s = %q)", f.Name, f.Values[0]))
		default:
			alts := make([]string, len(f.Values))
			for i, v := range f.Values {
				alts[i] = fmt.Sprintf("(%s = %q)", f.Name, v)
			}
			parts = append(parts, "("+strings.Join(alts, " OR ")+")")
		}
	}
	return strings.Join(parts, " ")
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// network attribute values of a resource: the network's name and its URL, so that
// a match.network of either spelling matches.
func networkAttrs(url string) []string {
	if url == "" {
		return nil
	}
	out := []string{lastSegment(url)}
	if u, ok := fromProjects(url); ok {
		out = append(out, u)
	}
	return out
}

func subnetFields(s *computepb.Subnetwork, project, region string) selector.Fields {
	state := "available"
	switch s.GetState() {
	case "READY", "":
	default:
		state = "failed"
	}
	cidrs := []string{s.GetIpCidrRange()}
	if s.GetIpv6CidrRange() != "" {
		cidrs = append(cidrs, s.GetIpv6CidrRange())
	}
	if s.GetExternalIpv6Prefix() != "" {
		cidrs = append(cidrs, s.GetExternalIpv6Prefix())
	}
	return selector.Fields{
		ID: subnetID(s, project, region), Name: s.GetName(), Created: parseTime(s.GetCreationTimestamp()),
		Attrs: map[string][]string{
			vendors.MatchNetwork: networkAttrs(s.GetNetwork()), vendors.MatchCIDR: cidrs,
			vendors.MatchState: {state}, vendors.MatchDescription: {s.GetDescription()},
		},
	}
}

// subnetID is the canonical reference of a subnetwork.
func subnetID(s *computepb.Subnetwork, project, region string) string {
	if u, ok := fromProjects(s.GetSelfLink()); ok && strings.Contains(u, "/subnetworks/") {
		return u
	}
	return SubnetworkURL(project, region, s.GetName())
}

func policyID(p *computepb.ResourcePolicy, project, region string) string {
	if u, ok := fromProjects(p.GetSelfLink()); ok && strings.Contains(u, "/resourcePolicies/") {
		return u
	}
	return ProjectPrefix(project) + "regions/" + region + "/resourcePolicies/" + p.GetName()
}

func policyFields(p *computepb.ResourcePolicy, project, region string) selector.Fields {
	state := "available"
	switch p.GetStatus() {
	case "READY", "":
	case "CREATING":
		state = "pending"
	default:
		state = "failed"
	}
	return selector.Fields{
		ID: policyID(p, project, region), Name: p.GetName(), Created: parseTime(p.GetCreationTimestamp()),
		Attrs: map[string][]string{vendors.MatchState: {state}, vendors.MatchDescription: {p.GetDescription()}},
	}
}

// fwTag is a network tag as the firewall kind sees it: the rules that target it.
type fwTag struct {
	tag   string
	rules []*computepb.Firewall
}

func (t fwTag) enabled() bool {
	for _, r := range t.rules {
		if !r.GetDisabled() {
			return true
		}
	}
	return false
}

func firewallFields(t fwTag) selector.Fields {
	state := "available"
	if !t.enabled() {
		state = "failed"
	}
	var nets []string
	for _, r := range t.rules {
		for _, n := range networkAttrs(r.GetNetwork()) {
			nets = append(nets, n)
		}
	}
	return selector.Fields{ID: t.tag, Name: t.tag,
		Attrs: map[string][]string{vendors.MatchNetwork: nets, vendors.MatchState: {state}}}
}
