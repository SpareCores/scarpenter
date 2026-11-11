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
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Kinds implements vendors.ManagedResources. The deletion order is the firewall
// groups first, then the VPCs; the two do not depend on each other (an instance
// holds both), so the order only fixes the sequence of a region's sweep.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindFirewallGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
		{Kind: KindNetworks, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	return kind == KindFirewallGroups || kind == KindNetworks
}

// markerTags are the logical tags of a marker, as the engine reads them.
func markerTags(m Marker) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: m.Cluster, vendors.TagNodeClass: m.NodeClass, vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: m.ResourceKind,
	}
}

// markerFor builds the marker of a managed resource from the create spec and
// the region it is for.
func markerFor(kind vendors.ResourceKind, region string, spec vendors.CreateSpec) Marker {
	return Marker{
		Cluster: spec.Tags[vendors.TagCluster], NodeClass: spec.Tags[vendors.TagNodeClass],
		ResourceKind: string(kind), Region: region,
	}
}

// ownedBy says whether a description carries the ownership marker of the
// cluster and the kind: the driver touches nothing else.
func ownedBy(desc, cluster string, kind vendors.ResourceKind) bool {
	_, m, ok := ParseDescription(desc)
	return ok && cluster != "" && m.Cluster == cluster && m.ResourceKind == string(kind)
}

func (d *Driver) groupRef(g govultr.FirewallGroup) vendors.ResourceRef {
	ref := vendors.ResourceRef{
		Kind: KindFirewallGroups, ID: g.ID, Name: g.Description, CreatedAt: parseTime(g.DateCreated),
	}
	if name, m, ok := ParseDescription(g.Description); ok {
		ref.Name, ref.Region, ref.Tags = name, m.Region, markerTags(m)
		ref.Managed = m.Cluster == d.cluster && m.ResourceKind == string(KindFirewallGroups)
	}
	return ref
}

func (d *Driver) vpcRef(v govultr.VPC) vendors.ResourceRef {
	ref := vendors.ResourceRef{
		Kind: KindNetworks, Region: v.Region, ID: v.ID, Name: v.Description, Network: v.ID,
		CreatedAt: parseTime(v.DateCreated),
	}
	if name, m, ok := ParseDescription(v.Description); ok {
		ref.Name, ref.Tags = name, markerTags(m)
		ref.Managed = m.Cluster == d.cluster && m.ResourceKind == string(KindNetworks)
	}
	return ref
}

// Resolve implements vendors.ManagedResources: existing resources by IDs,
// description pattern or the portable attributes, never modified. Vultr lists
// take no filters, so the list is paged to the end and the shared matcher does
// the rest. Firewall groups are account-wide (the region is only the one the
// core asks in); VPCs are matched to the region.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the Vultr driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: Vultr has no default resource")
	}
	if kind == KindNetworks {
		vpcs, err := d.listVPCs(ctx, region)
		if err != nil {
			return nil, err
		}
		res, err := selector.Apply(ref, d.Selectors()[kind], vpcs, vpcFields)
		d.noteMissing(kind, res.Missing)
		if err != nil {
			return nil, err
		}
		return d.vpcRefs(res.Items), nil
	}
	groups, err := listAll(ctx, opOther, govultr.ListOptions{}, d.api.ListFirewallGroups)
	if err != nil {
		return nil, err
	}
	groups = byFilters(groups, ref.Filters, groupValue)
	res, err := selector.Apply(ref, d.Selectors()[kind], groups, groupFields)
	d.noteMissing(kind, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		r := d.groupRef(g)
		r.Region = region
		out = append(out, r)
	}
	return out, nil
}

// listVPCs lists the VPCs of one region ("" lists every region).
func (d *Driver) listVPCs(ctx context.Context, region string) ([]govultr.VPC, error) {
	all, err := listAll(ctx, opOther, govultr.ListOptions{}, d.api.ListVPCs)
	if err != nil {
		return nil, err
	}
	if region != "" {
		all = slices.DeleteFunc(all, func(v govultr.VPC) bool { return v.Region != region })
	}
	return all, nil
}

func (d *Driver) vpcRefs(vpcs []govultr.VPC) []vendors.ResourceRef {
	out := make([]vendors.ResourceRef, 0, len(vpcs))
	for _, v := range vpcs {
		out = append(out, d.vpcRef(v))
	}
	return out
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the description of the creating call. created is
// false when an existing resource was adopted. A resource of that name without
// the marker of the cluster is never adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if err := d.ready(); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the Vultr driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	desc, err := FormatDescription(spec.Name, markerFor(kind, region, spec))
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalid, err)
	}
	if kind == KindNetworks {
		return d.ensureVPC(ctx, region, spec, desc)
	}
	return d.ensureGroup(ctx, region, spec, desc)
}

func (d *Driver) ensureGroup(ctx context.Context, region string, spec vendors.CreateSpec, desc string,
) (vendors.ResourceRef, bool, error) {
	var p FirewallGroupParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalid,
				fmt.Errorf("the firewall group parameters: %w", err))
		}
	}
	want, err := FirewallRules(p.Inbound)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalid, err)
	}
	groups, err := listAll(ctx, opOther, govultr.ListOptions{}, d.api.ListFirewallGroups)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	var g *govultr.FirewallGroup
	for i := range groups {
		name, _, parsed := ParseDescription(groups[i].Description)
		if parsed && name == spec.Name {
			g = &groups[i]
			break
		}
	}
	created := false
	if g == nil {
		out, err := d.api.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: desc})
		if err != nil {
			return vendors.ResourceRef{}, false, classify(err, opOther)
		}
		g, created = out, true
	} else if !ownedBy(g.Description, d.cluster, KindFirewallGroups) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a firewall group named %s exists without the ownership marker of cluster %s: not adopted", spec.Name, d.cluster)
	}
	if err := d.syncRules(ctx, g.ID, want); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	ref := d.groupRef(*g)
	ref.Region = region
	return ref, created, nil
}

// ruleKey identifies a firewall rule by what it allows.
type ruleKey struct {
	ipType, protocol, port, subnet string
	size                           int
}

func keyOfReq(r govultr.FirewallRuleReq) ruleKey {
	return ruleKey{r.IPType, r.Protocol, r.Port, r.Subnet, r.SubnetSize}
}

func keyOfRule(r govultr.FirewallRule) ruleKey {
	return ruleKey{r.IPType, r.Protocol, r.Port, r.Subnet, r.SubnetSize}
}

// syncRules makes the group's rules exactly the wanted ones: the rules it lacks
// are created, the others deleted. The name of a managed group hashes its
// parameters, so a group of a changed ruleset is another group; the sync repairs
// a creation that stopped halfway.
func (d *Driver) syncRules(ctx context.Context, groupID string, want []govultr.FirewallRuleReq) error {
	have, err := listAll(ctx, opOther, govultr.ListOptions{},
		func(ctx context.Context, o *govultr.ListOptions) ([]govultr.FirewallRule, *govultr.Meta, error) {
			return d.api.ListFirewallRules(ctx, groupID, o)
		})
	if err != nil {
		return err
	}
	wanted := map[ruleKey]bool{}
	for _, w := range want {
		wanted[keyOfReq(w)] = true
	}
	present := map[ruleKey]bool{}
	for _, h := range have {
		k := keyOfRule(h)
		if !wanted[k] || present[k] {
			if err := d.api.DeleteFirewallRule(ctx, groupID, h.ID); err != nil {
				if e := classify(err, opDelete); !vendors.IsClass(e, vendors.NotFound) {
					return e
				}
			}
			continue
		}
		present[k] = true
	}
	for i := range want {
		if present[keyOfReq(want[i])] {
			continue
		}
		if _, err := d.api.CreateFirewallRule(ctx, groupID, &want[i]); err != nil {
			return classify(err, opOther)
		}
	}
	return nil
}

func (d *Driver) ensureVPC(ctx context.Context, region string, spec vendors.CreateSpec, desc string,
) (vendors.ResourceRef, bool, error) {
	vpcs, err := d.listVPCs(ctx, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	for _, v := range vpcs {
		name, _, parsed := ParseDescription(v.Description)
		if !parsed || name != spec.Name {
			continue
		}
		if !ownedBy(v.Description, d.cluster, KindNetworks) {
			return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
				"a VPC named %s exists in %s without the ownership marker of cluster %s: not adopted", spec.Name, region, d.cluster)
		}
		return d.vpcRef(v), false, nil
	}
	// Vultr picks a free range when none is given (unverified, spike S6).
	v, err := d.api.CreateVPC(ctx, &govultr.VPCReq{Region: region, Description: desc})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, opOther)
	}
	return d.vpcRef(*v), true, nil
}

// List implements vendors.ManagedResources: the managed firewall groups and VPCs
// of the regions (every region without a list) that carry all the tags of the
// ownership marker (cluster, nodeclass, managed-by, resource-kind). Both lists
// are account-wide calls; a group's region is in its marker.
func (d *Driver) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	match := func(ref vendors.ResourceRef) bool {
		if !ref.Managed || (len(regions) > 0 && !slices.Contains(regions, ref.Region)) {
			return false
		}
		for k, v := range tags {
			if ref.Tags[k] != v {
				return false
			}
		}
		return true
	}
	var out []vendors.ResourceRef
	groups, err := listAll(ctx, opOther, govultr.ListOptions{}, d.api.ListFirewallGroups)
	if err != nil {
		return nil, fmt.Errorf("listing the managed firewall groups: %w", err)
	}
	for _, g := range groups {
		if ref := d.groupRef(g); match(ref) {
			out = append(out, ref)
		}
	}
	vpcs, err := d.listVPCs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing the managed VPCs: %w", err)
	}
	for _, v := range vpcs {
		if ref := d.vpcRef(v); match(ref) {
			out = append(out, ref)
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
	return out, nil
}

// InUse implements vendors.ManagedResources: a firewall group is in use while an
// instance carries it (instance_count), a VPC while something is attached. The
// last use is not known when idle (Vultr does not say), so the zero time is
// returned and the garbage collector counts the idle time from its first
// sighting.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if err := d.ready(); err != nil {
		return false, time.Time{}, err
	}
	switch ref.Kind {
	case KindFirewallGroups:
		g, err := d.api.GetFirewallGroup(ctx, ref.ID)
		if err != nil {
			return false, time.Time{}, classify(err, opGet)
		}
		if FirewallGroupInUse(*g) {
			return true, d.clock.Now(), nil
		}
		return false, time.Time{}, nil
	case KindNetworks:
		if _, err := d.api.GetVPC(ctx, ref.ID); err != nil {
			return false, time.Time{}, classify(err, opGet)
		}
		att, _, err := d.api.ListVPCAttachments(ctx, ref.ID, listOne())
		if err != nil {
			return false, time.Time{}, classify(err, opOther)
		}
		if len(att) > 0 {
			return true, d.clock.Now(), nil
		}
		return false, time.Time{}, nil
	}
	return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the Vultr driver has no resource kind %q", ref.Kind)
}

// Delete implements vendors.ManagedResources: only a resource whose description
// carries the cluster's ownership marker is deleted (never one the user brought);
// one that is already gone is success; one that an instance still holds is
// Retryable (the dependency texts).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	if err := d.ready(); err != nil {
		return err
	}
	var desc string
	switch ref.Kind {
	case KindFirewallGroups:
		g, err := d.api.GetFirewallGroup(ctx, ref.ID)
		if err != nil {
			return goneIsFine(classify(err, opGet))
		}
		desc = g.Description
	case KindNetworks:
		v, err := d.api.GetVPC(ctx, ref.ID)
		if err != nil {
			return goneIsFine(classify(err, opGet))
		}
		desc = v.Description
	default:
		return vendors.Errorf(vendors.Fatal, "the Vultr driver has no resource kind %q", ref.Kind)
	}
	if !ownedBy(desc, d.cluster, ref.Kind) {
		return vendors.Errorf(vendors.Fatal, "the %s %s does not carry the ownership marker of cluster %s: "+
			"it was not created by SCarpenter and is never deleted", ref.Kind, ref.ID, d.cluster)
	}
	var err error
	if ref.Kind == KindFirewallGroups {
		err = d.api.DeleteFirewallGroup(ctx, ref.ID)
	} else {
		err = d.api.DeleteVPC(ctx, ref.ID)
	}
	if err != nil {
		return goneIsFine(classify(err, opDelete))
	}
	return nil
}

func goneIsFine(err error) error {
	if vendors.IsClass(err, vendors.NotFound) {
		return nil
	}
	return err
}
