/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl // the fake repeats the SDK's long signatures and ARM texts
package fake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Resources is the resources API (the tag listing).
type Resources struct{ s *Subscription }

// Resources returns the resources API.
func (s *Subscription) Resources() *Resources { return &Resources{s} }

func hasTag(tags map[string]*string, name, value string) bool {
	for k, v := range tags {
		if strings.EqualFold(k, name) && val(v) == value {
			return true
		}
	}
	return false
}

// ListByTag lists the resources carrying the tag; like ARM it returns them
// without their tags, and it matches the tag name case-insensitively.
func (r *Resources) ListByTag(ctx context.Context, name, value string) ([]*armresources.GenericResourceExpanded, error) {
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "resources.list"); err != nil {
		return nil, err
	}
	var out []*armresources.GenericResourceExpanded
	for _, k := range sortedKeys(s.vms) {
		v := s.vms[k]
		if hasTag(v.vm.Tags, name, value) {
			out = append(out, &armresources.GenericResourceExpanded{ID: v.vm.ID, Name: v.vm.Name, Type: new("Microsoft.Compute/virtualMachines"), Location: v.vm.Location})
		}
	}
	for _, k := range sortedKeys(s.nics) {
		if n := s.nics[k]; hasTag(n.Tags, name, value) {
			out = append(out, &armresources.GenericResourceExpanded{ID: n.ID, Name: n.Name, Type: new("Microsoft.Network/networkInterfaces"), Location: n.Location})
		}
	}
	for _, k := range sortedKeys(s.pips) {
		if p := s.pips[k]; hasTag(p.Tags, name, value) {
			out = append(out, &armresources.GenericResourceExpanded{ID: p.ID, Name: p.Name, Type: new("Microsoft.Network/publicIPAddresses"), Location: p.Location})
		}
	}
	return out, nil
}

// SKUs is the compute SKU API.
type SKUs struct{ s *Subscription }

// SKUs returns the SKU API.
func (s *Subscription) SKUs() *SKUs { return &SKUs{s} }

// List returns the SKU table of a location; a location the subscription does
// not know is InvalidResourceLocation.
func (a *SKUs) List(ctx context.Context, loc string) ([]*armcompute.ResourceSKU, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "skus.list"); err != nil {
		return nil, err
	}
	l, ok := s.locations[strings.ToLower(loc)]
	if !ok {
		return nil, Error(http.StatusBadRequest, "InvalidResourceLocation", fmt.Sprintf("The location '%s' is not valid.", loc))
	}
	return slices.Clone(l.skus), nil
}

// Usages is the usage API (compute and network).
type Usages struct{ s *Subscription }

// Usages returns the usage API.
func (s *Subscription) Usages() *Usages { return &Usages{s} }

// ComputeUsages returns the compute usages of a location with the usage of the
// whole subscription: cores, lowPriorityCores, every family of the SKU table
// and a few counters the driver ignores.
func (a *Usages) ComputeUsages(ctx context.Context, loc string) ([]*armcompute.Usage, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "usages.compute"); err != nil {
		return nil, err
	}
	l, ok := s.locations[strings.ToLower(loc)]
	if !ok {
		return nil, Error(http.StatusBadRequest, "InvalidResourceLocation", fmt.Sprintf("The location '%s' is not valid.", loc))
	}
	names := []string{usageCores, usageSpot}
	for _, r := range l.skus {
		if f := val(r.Family); f != "" && !slices.Contains(names, f) {
			names = append(names, f)
		}
	}
	slices.Sort(names[2:])
	var out []*armcompute.Usage
	for _, n := range names {
		out = append(out, &armcompute.Usage{
			Name: &armcompute.UsageName{Value: new(n), LocalizedValue: new(n)}, Unit: new("Count"),
			CurrentValue: new(int32(s.usage(l, n))), Limit: new(limitOr(l, n)),
		})
	}
	out = append(out,
		&armcompute.Usage{Name: &armcompute.UsageName{Value: new("virtualMachines")}, Unit: new("Count"), CurrentValue: new(int32(len(s.vms))), Limit: new(int64(25000))},
		&armcompute.Usage{Name: &armcompute.UsageName{Value: new("availabilitySets")}, Unit: new("Count"), CurrentValue: new(int32(0)), Limit: new(int64(2500))})
	return out, nil
}

func limitOr(l *location, name string) int64 {
	if v, ok := l.limits[name]; ok {
		return v
	}
	return 0
}

// NetworkUsages returns the network usages of a location.
func (a *Usages) NetworkUsages(ctx context.Context, loc string) ([]*armnetwork.Usage, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "usages.network"); err != nil {
		return nil, err
	}
	l, ok := s.locations[strings.ToLower(loc)]
	if !ok {
		return nil, Error(http.StatusBadRequest, "InvalidResourceLocation", fmt.Sprintf("The location '%s' is not valid.", loc))
	}
	n := s.pipCount(l.name) + l.foreign["StandardPublicIPAddresses"]
	unit := armnetwork.UsageUnitCount
	return []*armnetwork.Usage{
		{Name: &armnetwork.UsageName{Value: new("StandardPublicIPAddresses")}, Unit: &unit, CurrentValue: new(n), Limit: new(limitOr(l, "StandardPublicIPAddresses"))},
		{Name: &armnetwork.UsageName{Value: new("VirtualNetworks")}, Unit: &unit, CurrentValue: new(int64(0)), Limit: new(int64(1000))},
	}, nil
}

// Images is the marketplace image API.
type Images struct{ s *Subscription }

// Images returns the image API.
func (s *Subscription) Images() *Images { return &Images{s} }

// Get returns an image version; PlatformImageNotFound (404) when the version
// does not exist in the location.
func (a *Images) Get(ctx context.Context, loc, publisher, offer, sku, version string) (armcompute.VirtualMachineImage, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "image.get"); err != nil {
		return armcompute.VirtualMachineImage{}, err
	}
	k := strings.ToLower(strings.Join([]string{publisher, offer, sku, version}, ":"))
	im, ok := s.images[k]
	if !ok || im.missingIn[strings.ToLower(loc)] {
		return armcompute.VirtualMachineImage{}, Error(http.StatusNotFound, "PlatformImageNotFound",
			fmt.Sprintf("The platform image '%s' is not available. Verify that all fields in the storage profile are correct.", k))
	}
	out := armcompute.VirtualMachineImage{
		Name: new(version), Location: new(loc),
		ID: new(fmt.Sprintf("/Subscriptions/%s/Providers/Microsoft.Compute/Locations/%s/Publishers/%s/ArtifactTypes/VMImage/Offers/%s/Skus/%s/Versions/%s", s.ID, loc, publisher, offer, sku, version)),
		Properties: &armcompute.VirtualMachineImageProperties{
			OSDiskImage: &armcompute.OSDiskImage{OperatingSystem: new(armcompute.OperatingSystemTypesLinux)},
			Plan:        im.plan,
		},
	}
	return out, nil
}

// ListSKUs lists the SKU names of an offer; an offer without images is
// NotFound.
func (a *Images) ListSKUs(ctx context.Context, loc, publisher, offer string) ([]string, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "image.skus"); err != nil {
		return nil, err
	}
	prefix := strings.ToLower(publisher + ":" + offer + ":")
	set := map[string]bool{}
	for k := range s.images {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			sku, _, _ := strings.Cut(rest, ":")
			set[sku] = true
		}
	}
	if len(set) == 0 {
		return nil, notFound("NotFound", "offer "+offer)
	}
	return sortedKeys(set), nil
}

// Agreements is the marketplace ordering API.
type Agreements struct{ s *Subscription }

// Agreements returns the marketplace ordering API.
func (s *Subscription) Agreements() *Agreements { return &Agreements{s} }

// Accepted reports whether the terms of the plan are accepted.
func (a *Agreements) Accepted(ctx context.Context, publisher, offer, plan string) (bool, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "agreement.get"); err != nil {
		return false, err
	}
	return s.agreement[strings.ToLower(publisher+"/"+offer+"/"+plan)], nil
}
