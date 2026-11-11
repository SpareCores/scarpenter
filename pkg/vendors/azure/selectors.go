/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// KindImages is the boot image: a kind that only accepts `use` (plan 14 A1), so
// it is not one of Kinds and Resolve does not serve it; ResolveImage does.
const KindImages = selector.ImagesKind

// filterResourceGroup is the one native filter of the network kinds: the
// resource groups whose resources are candidates. ARM has no server-side filter
// on these lists, so it is evaluated here.
const filterResourceGroup = "resourceGroup"

// Selectors implements vendors.ManagedResources: the `use` selector of the kinds
// a NodeClass can bring (resource-selectors.md, azure). ARM lists the resources
// of the subscription and the shared matcher does the rest, so every field is
// evaluated on the client. The subnet is selected through its virtual network:
// tags are the VNet's, the name is <vnet>/<subnet>. Azure's Resource Graph
// expression is not supported in v1 (it is eventually consistent and needs
// another client).
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	rgFilter := []vendors.FilterCap{{Name: filterResourceGroup, Side: vendors.SideClient}}
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindResourceGroups: {
			Tags:          vendors.TagsMap,
			Name:          &vendors.FieldCap{Field: fieldName, Side: vendors.SideClient},
			DefaultSelect: common.SelectExactlyOne,
		},
		KindSubnets: {
			Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: "<virtual network>/<subnet>", Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork: {Field: "virtual network ID", Side: vendors.SideClient},
				vendors.MatchCIDR:    {Field: "addressPrefix", Side: vendors.SideClient},
				vendors.MatchWithin:  {Field: "addressPrefix", Side: vendors.SideClient},
				vendors.MatchState:   {Field: "provisioningState", Side: vendors.SideClient},
			},
			Filters:       rgFilter,
			DefaultSelect: common.SelectAll,
		},
		KindSecurityGroups: {
			Tags:          vendors.TagsMap,
			Name:          &vendors.FieldCap{Field: fieldName, Side: vendors.SideClient},
			Filters:       rgFilter,
			DefaultSelect: common.SelectExactlyOne,
		},
		KindPlacement: {
			Tags:          vendors.TagsMap,
			Name:          &vendors.FieldCap{Field: fieldName, Side: vendors.SideClient},
			Filters:       rgFilter,
			DefaultSelect: common.SelectExactlyOne,
		},
		KindImages: {
			Name: &vendors.FieldCap{Field: "publisher:offer:sku:version", Side: vendors.SideClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture: {Field: "architecture", Side: vendors.SideClient},
			},
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// idSpelling returns the spelling the selector used for an ID that equals id
// without regard to case (Azure IDs and names are case insensitive), so that
// the shared matcher, which compares exactly, finds it.
func idSpelling(ids []string, id string) string {
	for _, want := range ids {
		if strings.EqualFold(want, id) {
			return want
		}
	}
	return id
}

// groupNames turns the IDs of a resource group selector, names or ARM IDs, into
// names.
func groupNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, "/") {
			if r, err := arm.ParseResourceID(id); err == nil && r.ResourceGroupName != "" {
				id = r.ResourceGroupName
			}
		}
		out = append(out, id)
	}
	return out
}

// groupFilter reads the resourceGroup filter values of a selector.
func groupFilter(ref vendors.UseRef) []string {
	var out []string
	for _, f := range ref.Filters {
		if f.Name == filterResourceGroup {
			out = append(out, f.Values...)
		}
	}
	return out
}

func inGroup(id string, groups []string) bool {
	if len(groups) == 0 {
		return true
	}
	r, err := arm.ParseResourceID(id)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(groups, func(g string) bool { return strings.EqualFold(g, r.ResourceGroupName) })
}

func tagMap(tags map[string]*string) map[string]string {
	m := make(map[string]string, len(tags))
	for k, v := range tags {
		m[k] = deref(v)
	}
	return m
}

// groupItem, subnetItem, nsgItem and ppgItem are what the matcher works on.
type subnetItem struct {
	vnet armnetwork.VirtualNetwork
	sn   armnetwork.Subnet
}

func groupFields(ids []string) func(armresources.ResourceGroup) selector.Fields {
	return func(g armresources.ResourceGroup) selector.Fields {
		return selector.Fields{ID: idSpelling(ids, deref(g.Name)), Name: deref(g.Name), Tags: tagMap(g.Tags)}
	}
}

func subnetFields(ids []string) func(subnetItem) selector.Fields {
	return func(it subnetItem) selector.Fields {
		state := "available"
		if p := it.sn.Properties; p != nil {
			switch deref(p.ProvisioningState) {
			case armnetwork.ProvisioningStateUpdating:
				state = "pending"
			case armnetwork.ProvisioningStateFailed, armnetwork.ProvisioningStateDeleting:
				state = "failed"
			}
		}
		attrs := map[string][]string{vendors.MatchNetwork: {deref(it.vnet.ID)}, vendors.MatchState: {state}}
		if p := it.sn.Properties; p != nil {
			if pre := deref(p.AddressPrefix); pre != "" {
				attrs[vendors.MatchCIDR] = append(attrs[vendors.MatchCIDR], pre)
			}
			for _, pre := range p.AddressPrefixes {
				attrs[vendors.MatchCIDR] = append(attrs[vendors.MatchCIDR], deref(pre))
			}
		}
		return selector.Fields{ID: idSpelling(ids, deref(it.sn.ID)),
			Name: deref(it.vnet.Name) + "/" + deref(it.sn.Name), Tags: tagMap(it.vnet.Tags), Attrs: attrs}
	}
}

func nsgFields(ids []string) func(armnetwork.SecurityGroup) selector.Fields {
	return func(g armnetwork.SecurityGroup) selector.Fields {
		return selector.Fields{ID: idSpelling(ids, deref(g.ID)), Name: deref(g.Name), Tags: tagMap(g.Tags)}
	}
}

func ppgFields(ids []string) func(armcompute.ProximityPlacementGroup) selector.Fields {
	return func(g armcompute.ProximityPlacementGroup) selector.Fields {
		return selector.Fields{ID: idSpelling(ids, deref(g.ID)), Name: deref(g.Name), Tags: tagMap(g.Tags)}
	}
}

func sameLocation(a, b string) bool { return strings.EqualFold(strings.ReplaceAll(a, " ", ""), b) }

// noteMissing logs the IDs a `use` named that Azure did not return: a missing ID
// does not fail the others, so it would otherwise go unnoticed.
func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

// Resolve implements vendors.ManagedResources: existing resources by IDs, name
// pattern or tags, never modified. A resource group is found by name (the ID is
// the name; an ARM ID is accepted), the others by ARM ID. Virtual networks,
// network security groups and proximity placement groups must be in the region:
// a NIC, a VM and the group it joins share the region.
func (d *Driver) Resolve(
	ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the Azure driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: Azure has no default resource")
	}
	caps := d.Selectors()[kind]
	switch kind {
	case KindResourceGroups:
		return d.resolveGroups(ctx, caps, region, ref)
	case KindSubnets:
		return d.resolveSubnets(ctx, caps, region, ref)
	case KindSecurityGroups, KindRunSecurityGroups:
		return d.resolveNSGs(ctx, kind, caps, region, ref)
	}
	return d.resolvePPGs(ctx, caps, region, ref)
}

func (d *Driver) resolveGroups(
	ctx context.Context, caps vendors.SelectorCaps, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	groups, err := d.clients.Groups.List(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	ref.IDs = groupNames(ref.IDs)
	res, err := selectorApply(ref, caps, groups, groupFields(ref.IDs))
	d.noteMissing(KindResourceGroups, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: KindResourceGroups, Region: region,
			ID: idSpelling(ref.IDs, deref(g.Name)), Name: deref(g.Name)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *Driver) resolveSubnets(
	ctx context.Context, caps vendors.SelectorCaps, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	vnets, err := d.clients.VNets.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	groups := groupFilter(ref)
	var items []subnetItem
	for _, v := range vnets {
		if !sameLocation(deref(v.Location), region) || !inGroup(deref(v.ID), groups) || v.Properties == nil {
			continue
		}
		for _, sn := range v.Properties.Subnets {
			if sn != nil {
				items = append(items, subnetItem{vnet: v, sn: *sn})
			}
		}
	}
	res, err := selectorApply(ref, caps, items, subnetFields(ref.IDs))
	d.noteMissing(KindSubnets, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: idSpelling(ref.IDs, deref(it.sn.ID)),
			Name: deref(it.vnet.Name) + "/" + deref(it.sn.Name), Network: deref(it.vnet.ID)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (d *Driver) resolveNSGs(
	ctx context.Context, kind vendors.ResourceKind, caps vendors.SelectorCaps, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	all, err := d.clients.NSGs.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	groups := groupFilter(ref)
	var items []armnetwork.SecurityGroup
	for _, g := range all {
		if sameLocation(deref(g.Location), region) && inGroup(deref(g.ID), groups) {
			items = append(items, g)
		}
	}
	res, err := selectorApply(ref, caps, items, nsgFields(ref.IDs))
	d.noteMissing(kind, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: idSpelling(ref.IDs, deref(g.ID)),
			Name: deref(g.Name)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (d *Driver) resolvePPGs(
	ctx context.Context, caps vendors.SelectorCaps, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	all, err := d.clients.PPGs.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	groups := groupFilter(ref)
	var items []armcompute.ProximityPlacementGroup
	for _, g := range all {
		if sameLocation(deref(g.Location), region) && inGroup(deref(g.ID), groups) {
			items = append(items, g)
		}
	}
	res, err := selectorApply(ref, caps, items, ppgFields(ref.IDs))
	d.noteMissing(KindPlacement, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: KindPlacement, Region: region, ID: idSpelling(ref.IDs, deref(g.ID)),
			Name: deref(g.Name)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// selectorApply is selector.Apply with the filters of the kind taken out first
// (they are evaluated while listing) and a clear error for a field the kind
// cannot evaluate.
func selectorApply[T any](
	ref vendors.UseRef, caps vendors.SelectorCaps, listed []T, fields func(T) selector.Fields,
) (selector.Result[T], error) {
	if ref.Expression != "" {
		return selector.Result[T]{}, fmt.Errorf("expression is not supported by the Azure driver")
	}
	return selector.Apply(ref, caps, listed, fields)
}
