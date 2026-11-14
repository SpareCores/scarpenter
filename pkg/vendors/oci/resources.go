/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Kinds implements vendors.ManagedResources. The deletion order is the run groups first (nothing
// depends on them but instances), then the NodeClass's groups, then the network, which needs its
// groups gone. The images are not a managed kind (use only, ADR-021): ResolveImage serves them.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindRunNetworkSecurityGroup, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 25,
			InNetwork: KindSubnets},
		{Kind: KindNetworkSecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30,
			InNetwork: KindSubnets},
		{Kind: KindSubnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(k vendors.ResourceKind) bool {
	return k == KindSubnets || k == KindNetworkSecurityGroups || k == KindRunNetworkSecurityGroup
}

func isGroupKind(k vendors.ResourceKind) bool {
	return k == KindNetworkSecurityGroups || k == KindRunNetworkSecurityGroup
}

// pages collects every page of a list call.
func pages[T any](fetch func(page *string) (items []T, next *string, err error)) ([]T, error) {
	var out []T
	var page *string
	for {
		items, next, err := fetch(page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == nil || *next == "" {
			return out, nil
		}
		page = next
	}
}

// listFilters are the server-side parameters of a use block for the network kinds: the native
// filters, an exact name and a single network. The matcher re-applies everything.
type listFilters struct {
	vcn, name, state string
}

func filtersOf(ref vendors.UseRef) (listFilters, error) {
	var lf listFilters
	if ref.Name != "" && !selector.HasWildcard(ref.Name) {
		lf.name = ref.Name
	}
	if len(ref.Match.Network) == 1 {
		lf.vcn = ref.Match.Network[0]
	}
	for _, f := range ref.Filters {
		if len(f.Values) != 1 {
			return lf, vendors.NewError(vendors.Fatal, codeSettings, fmt.Errorf(
				"the filter %s takes one value, not %q", f.Name, f.Values))
		}
		switch v := f.Values[0]; f.Name {
		case filterVCN:
			lf.vcn = v
		case filterLifecycle:
			lf.state = strings.ToUpper(v)
		case filterDisplayName:
			lf.name = v
		}
	}
	return lf, nil
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// resourceTags returns the freeform tags of a managed resource: the NodeClass's own tags and the
// ownership marker, which wins.
func resourceTags(spec vendors.CreateSpec) (map[string]string, error) {
	return FreeformTags(spec.Tags, spec.ExtraTags)
}

// owned says whether the freeform tags carry the ownership marker of the cluster: cluster and
// managed-by (the NodeClass and kind tags are descriptive).
func (d *Driver) owned(free map[string]string) bool {
	return d.cfg.ClusterName != "" && free[TagKey(vendors.TagCluster)] == d.cfg.ClusterName &&
		free[TagKey(vendors.TagManagedBy)] == vendors.ManagedByValue
}

func tagsMatch(free map[string]string, want map[vendors.TagKey]string) bool {
	for k, v := range want {
		if got, ok := free[TagKey(k)]; !ok || got != v {
			return false
		}
	}
	return true
}

func sortRefs(out []vendors.ResourceRef) {
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
}

// Resolve implements vendors.ManagedResources: existing resources by IDs, name pattern or tags,
// never modified. An empty reference selects nothing (OCI has no default network).
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the OCI driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: OCI has no default network")
	}
	if ok, err := d.subscribed(ctx, region); err != nil {
		return nil, err
	} else if !ok {
		return nil, vendors.NewError(vendors.RegionUnavailable, codeUnknownReg,
			fmt.Errorf("the region %q is not subscribed by the tenancy", region))
	}
	if kind == KindSubnets {
		return d.resolveSubnets(ctx, region, ref)
	}
	return d.resolveGroups(ctx, kind, region, ref)
}

func (d *Driver) resolveSubnets(ctx context.Context, region string, ref vendors.UseRef) ([]vendors.ResourceRef, error) {
	var subs []core.Subnet
	lf, err := filtersOf(ref)
	if err != nil {
		return nil, err
	}
	if len(ref.IDs) > 0 {
		for _, id := range ref.IDs {
			res, err := d.api.Network.GetSubnet(ctx, region, core.GetSubnetRequest{SubnetId: &id})
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, classify(err, kindRead)
			}
			subs = append(subs, res.Subnet)
		}
	} else {
		subs, err = pages(func(page *string) ([]core.Subnet, *string, error) {
			req := core.ListSubnetsRequest{CompartmentId: &d.compartment, VcnId: ptr(lf.vcn), DisplayName: ptr(lf.name),
				LifecycleState: core.SubnetLifecycleStateEnum(lf.state), Page: page}
			res, err := d.api.Network.ListSubnets(ctx, region, req)
			return res.Items, res.OpcNextPage, classify(err, kindRead)
		})
		if err != nil {
			return nil, err
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[KindSubnets], subs, subnetFields)
	d.noteMissing(KindSubnets, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, s := range res.Items {
		d.rememberSubnet(region, s)
		out = append(out, d.subnetRef(region, s))
	}
	sortRefs(out)
	return out, nil
}

func (d *Driver) subnetRef(region string, s core.Subnet) vendors.ResourceRef {
	return vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: deref(s.Id), Name: deref(s.DisplayName),
		Network: deref(s.VcnId), Tags: TagsOf(s.FreeformTags), CreatedAt: timeOf(s.TimeCreated)}
}

func (d *Driver) resolveGroups(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	var gs []core.NetworkSecurityGroup
	lf, err := filtersOf(ref)
	if err != nil {
		return nil, err
	}
	if len(ref.IDs) > 0 {
		for _, id := range ref.IDs {
			res, err := d.api.Network.GetNetworkSecurityGroup(ctx, region, core.GetNetworkSecurityGroupRequest{
				NetworkSecurityGroupId: &id})
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, classify(err, kindRead)
			}
			gs = append(gs, res.NetworkSecurityGroup)
		}
	} else if gs, err = d.listGroups(ctx, region, lf); err != nil {
		return nil, err
	}
	res, err := selector.Apply(ref, d.Selectors()[KindNetworkSecurityGroups], gs, groupFields)
	d.noteMissing(kind, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, d.groupRef(kind, region, g))
	}
	sortRefs(out)
	return out, nil
}

func (d *Driver) listGroups(ctx context.Context, region string, lf listFilters) ([]core.NetworkSecurityGroup, error) {
	return pages(func(page *string) ([]core.NetworkSecurityGroup, *string, error) {
		res, err := d.api.Network.ListNetworkSecurityGroups(ctx, region, core.ListNetworkSecurityGroupsRequest{
			CompartmentId: &d.compartment, VcnId: ptr(lf.vcn), DisplayName: ptr(lf.name),
			LifecycleState: core.NetworkSecurityGroupLifecycleStateEnum(lf.state), Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
}

func (d *Driver) groupRef(kind vendors.ResourceKind, region string, g core.NetworkSecurityGroup) vendors.ResourceRef {
	return vendors.ResourceRef{Kind: kind, Region: region, ID: deref(g.Id), Name: deref(g.DisplayName),
		Network: deref(g.VcnId), Tags: TagsOf(g.FreeformTags), CreatedAt: timeOf(g.TimeCreated)}
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker, else create with
// the marker in the creating call. created is false when an existing resource was adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the OCI driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	if ok, err := d.subscribed(ctx, region); err != nil {
		return vendors.ResourceRef{}, false, err
	} else if !ok {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.RegionUnavailable, codeUnknownReg,
			fmt.Errorf("the region %q is not subscribed by the tenancy", region))
	}
	if kind == KindSubnets {
		return d.ensureNetwork(ctx, region, spec)
	}
	return d.ensureGroup(ctx, kind, region, spec)
}

// List implements vendors.ManagedResources: the managed resources carrying all the tags,
// top-level resources only: a subnet stands for its VCN, gateway, route table and security list (a
// VCN with no subnet, left by an interrupted creation, stands for itself).
func (d *Driver) List(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	all, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, r := range all {
		if len(regions) > 0 && !slices.Contains(regions, r.Name) {
			continue
		}
		nets, err := d.listNetworks(ctx, r.Name, tags)
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", r.Name, err)
		}
		groups, err := d.listGroups(ctx, r.Name, listFilters{})
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", r.Name, err)
		}
		out = append(out, nets...)
		for _, g := range groups {
			if !d.owned(g.FreeformTags) || !tagsMatch(g.FreeformTags, tags) {
				continue
			}
			kind := vendors.ResourceKind(g.FreeformTags[TagKey(vendors.TagResource)])
			if !isGroupKind(kind) {
				kind = KindNetworkSecurityGroups
			}
			out = append(out, d.groupRef(kind, r.Name, g))
		}
	}
	for i := range out {
		out[i].Managed = true
	}
	sortRefs(out)
	return out, nil
}

// InUse implements vendors.ManagedResources: whether a live instance uses the resource. When it is
// not in use the last use is unknown (OCI does not say), so the zero time is returned and the
// garbage collector counts the idle time from its first sighting of the resource.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if !isKind(ref.Kind) {
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the OCI driver has no resource kind %q", ref.Kind)
	}
	if isGroupKind(ref.Kind) {
		vnics, err := pages(func(page *string) ([]core.NetworkSecurityGroupVnic, *string, error) {
			res, err := d.api.Network.ListNetworkSecurityGroupVnics(ctx, ref.Region,
				core.ListNetworkSecurityGroupVnicsRequest{NetworkSecurityGroupId: &ref.ID, Page: page})
			return res.Items, res.OpcNextPage, err
		})
		if err != nil {
			return false, time.Time{}, d.gone(ref, err)
		}
		return len(vnics) > 0, d.inUseSince(len(vnics) > 0), nil
	}
	subnets, err := d.subnetIDs(ctx, ref)
	if err != nil {
		return false, time.Time{}, err
	}
	atts, err := pages(func(page *string) ([]core.VnicAttachment, *string, error) {
		res, err := d.api.Compute.ListVnicAttachments(ctx, ref.Region,
			core.ListVnicAttachmentsRequest{CompartmentId: &d.compartment, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return false, time.Time{}, err
	}
	for _, a := range atts {
		live := a.LifecycleState == core.VnicAttachmentLifecycleStateAttached ||
			a.LifecycleState == core.VnicAttachmentLifecycleStateAttaching
		if live && slices.Contains(subnets, deref(a.SubnetId)) {
			return true, d.clock.Now(), nil
		}
	}
	return false, time.Time{}, nil
}

func (d *Driver) inUseSince(inUse bool) time.Time {
	if inUse {
		return d.clock.Now()
	}
	return time.Time{}
}

// gone turns the 404 of a read of a managed resource into NotFound.
func (d *Driver) gone(ref vendors.ResourceRef, err error) error {
	if isNotFound(err) {
		return vendors.NewError(vendors.NotFound, vendors.CodeOf(classify(err, kindRead)),
			fmt.Errorf("the %s %s does not exist: %w", ref.Kind, ref.ID, err))
	}
	return classify(err, kindRead)
}

// Delete implements vendors.ManagedResources: only resources that carry the cluster's ownership
// marker are deleted (never one the user brought); a resource that is already gone is success; one
// that something still uses is Retryable (OCI's 409).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	switch {
	case ref.Kind == KindSubnets:
		return d.deleteNetwork(ctx, ref)
	case isGroupKind(ref.Kind):
		return d.deleteGroup(ctx, ref)
	}
	return vendors.Errorf(vendors.Fatal, "the OCI driver has no resource kind %q", ref.Kind)
}

var _ vendors.ManagedResources = (*Driver)(nil)

func timeOf(t *ocicommon.SDKTime) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}
