/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"slices"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Regions returns the regions the credentials can use: DescribeRegions read
// from the home region, intersected with the regions Keeper has as active when
// active is not nil (Keeper's catalog has regions an account never sees, and
// the account sees regions the catalog does not have). The result is sorted.
func (c *Cloud) Regions(ctx context.Context, home string, active []string) ([]vendors.Region, error) {
	cl, err := c.Clients(ctx, home)
	if err != nil {
		return nil, err
	}
	out, err := call(ctx, c, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DescribeRegionsResponseBody, error) {
			return cl.ECS.DescribeRegions(ctx, &ecs.DescribeRegionsRequest{})
		})
	if err != nil {
		return nil, err
	}
	var regions []vendors.Region
	if out.Regions != nil {
		for _, r := range out.Regions.Region {
			id := deref(r.RegionId)
			if id == "" || (active != nil && !slices.Contains(active, id)) {
				continue
			}
			regions = append(regions, vendors.Region{Name: id})
		}
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return compareStr(a.Name, b.Name) })
	return regions, nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Zones returns the zones of a region. The zone ID is the zone name
// (cn-hangzhou-h, ap-southeast-1a): ECS has no per-account zone shuffling, so
// ID and Name are the same. A region the credentials cannot use is
// RegionUnavailable (the facts skip it with one warning). A zone without a
// vSwitch is unusable for launch, not for listing.
func (c *Cloud) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	cl, err := c.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	out, err := call(ctx, c, callTimeout, errContext{regionRead: true},
		func(ctx context.Context) (*ecs.DescribeZonesResponseBody, error) {
			return cl.ECS.DescribeZones(ctx, &ecs.DescribeZonesRequest{RegionId: new(region)})
		})
	if err != nil {
		return nil, err
	}
	var zones []vendors.Zone
	if out.Zones != nil {
		for _, z := range out.Zones.Zone {
			if id := deref(z.ZoneId); id != "" {
				zones = append(zones, vendors.Zone{ID: id, Name: id})
			}
		}
	}
	slices.SortFunc(zones, func(a, b vendors.Zone) int { return compareStr(a.ID, b.ID) })
	return zones, nil
}

// StatusAvailable is the Status DescribeAvailableResource gives a type that can
// be bought (the other is SoldOut).
const StatusAvailable = "Available"

// LaunchableTypes maps the API reference of each launchable type to the zone
// IDs where it can be launched with the capacity type: DescribeAvailableResource
// per region (DestinationResource=InstanceType, ResourceType=instance,
// InstanceChargeType=PostPaid, and the spot strategy for spot) reports per zone
// each type's Status; the Available ones are returned, the types the driver
// never launches (Launchable) are dropped. The driver refreshes it every 5
// minutes.
func (c *Cloud) LaunchableTypes(ctx context.Context, region, capacityType string) (map[string][]string, error) {
	cl, err := c.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	in := &ecs.DescribeAvailableResourceRequest{
		RegionId: new(region), DestinationResource: new("InstanceType"),
		ResourceType: new("instance"), InstanceChargeType: new("PostPaid"),
	}
	if capacityType == vendors.CapacitySpot {
		in.SpotStrategy = new("SpotAsPriceGo")
	}
	out, err := call(ctx, c, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DescribeAvailableResourceResponseBody, error) {
			return cl.ECS.DescribeAvailableResource(ctx, in)
		})
	if err != nil {
		return nil, err
	}
	types := map[string][]string{}
	if out.AvailableZones == nil {
		return types, nil
	}
	for _, z := range out.AvailableZones.AvailableZone {
		zone := deref(z.ZoneId)
		if zone == "" || z.AvailableResources == nil {
			continue
		}
		for _, ar := range z.AvailableResources.AvailableResource {
			if ar.SupportedResources == nil {
				continue
			}
			for _, sr := range ar.SupportedResources.SupportedResource {
				name := deref(sr.Value)
				if deref(sr.Status) != StatusAvailable || !Launchable(name) || slices.Contains(types[name], zone) {
					continue
				}
				types[name] = append(types[name], zone)
			}
		}
	}
	for _, zones := range types {
		slices.Sort(zones)
	}
	return types, nil
}
