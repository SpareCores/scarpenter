/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// DescribeRegions implements EC2API: only the enabled regions unless AllRegions
// is set.
func (e *EC2) DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeRegionsOutput, error) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if err := e.a.begin(ctx, "DescribeRegions", in); err != nil {
		return nil, err
	}
	out := &ec2.DescribeRegionsOutput{}
	for _, name := range e.a.order {
		r := e.a.regions[name]
		if r.disabled && !deref(in.AllRegions) {
			continue
		}
		status := "opt-in-not-required"
		if r.disabled {
			status = "not-opted-in"
		}
		out.Regions = append(out.Regions, types.Region{RegionName: new(name), OptInStatus: new(status),
			Endpoint: new("ec2." + name + ".amazonaws.com")})
	}
	return out, nil
}

func (z AZ) attrs(name string) ([]string, bool) {
	switch name {
	case "zone-type":
		return []string{z.Type}, true
	case "opt-in-status":
		return []string{z.OptIn}, true
	case "state":
		return []string{"available"}, true
	case "zone-name":
		return []string{z.Name}, true
	case "zone-id":
		return []string{z.ID}, true
	}
	return nil, false
}

// DescribeAvailabilityZones implements EC2API.
func (e *EC2) DescribeAvailabilityZones(ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options),
) (*ec2.DescribeAvailabilityZonesOutput, error) {
	r, err := e.begin(ctx, "DescribeAvailabilityZones", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	out := &ec2.DescribeAvailabilityZonesOutput{}
	for _, z := range r.azs {
		ok, err := matches(in.Filters, nil, z.attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out.AvailabilityZones = append(out.AvailabilityZones, types.AvailabilityZone{
			ZoneName: new(z.Name), ZoneId: new(z.ID), RegionName: new(r.name), State: types.AvailabilityZoneStateAvailable,
			ZoneType: new(z.Type), OptInStatus: types.AvailabilityZoneOptInStatus(z.OptIn),
		})
	}
	return out, nil
}

func (i *Image) attrs(name string) ([]string, bool) {
	switch name {
	case "name":
		return []string{i.Name}, true
	case "state":
		return []string{i.State}, true
	case "architecture":
		return []string{i.Arch}, true
	case "image-id":
		return []string{i.ID}, true
	case "owner-id":
		return []string{i.Owner}, true
	case "description":
		return []string{i.Description}, true
	case "creation-date":
		return []string{i.created().Format("2006-01-02T15:04:05.000Z")}, true
	case "root-device-type":
		return []string{"ebs"}, true
	case "virtualization-type":
		return []string{"hvm"}, true
	}
	return nil, false
}

func (i *Image) created() time.Time {
	if i.Created.IsZero() {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return i.Created.UTC()
}

// DescribeImages implements EC2API.
func (e *EC2) DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options),
) (*ec2.DescribeImagesOutput, error) {
	r, err := e.begin(ctx, "DescribeImages", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	for _, id := range in.ImageIds {
		if _, ok := r.images[id]; !ok {
			return nil, apiError("InvalidAMIID.NotFound", fmt.Sprintf("The image id '[%s]' does not exist", id))
		}
	}
	out := &ec2.DescribeImagesOutput{}
	for _, id := range sortedKeys(r.images) {
		img := r.images[id]
		if len(in.ImageIds) > 0 && !slices.Contains(in.ImageIds, id) {
			continue
		}
		if len(in.Owners) > 0 && !slices.Contains(in.Owners, img.Owner) {
			continue
		}
		ok, err := matches(in.Filters, img.Tags, img.attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if img.Deprecated && !deref(in.IncludeDeprecated) {
			continue
		}
		var tags []types.Tag
		for _, k := range sortedKeys(img.Tags) {
			tags = append(tags, types.Tag{Key: new(k), Value: new(img.Tags[k])})
		}
		out.Images = append(out.Images, types.Image{
			Tags: tags, Description: new(img.Description), CreationDate: new(img.created().Format("2006-01-02T15:04:05.000Z")),
			ImageId: new(img.ID), Name: new(img.Name), OwnerId: new(img.Owner), Architecture: types.ArchitectureValues(img.Arch),
			State: types.ImageState(img.State), RootDeviceName: new(img.RootDevice), RootDeviceType: types.DeviceTypeEbs,
			BlockDeviceMappings: []types.BlockDeviceMapping{{
				DeviceName: new(img.RootDevice), Ebs: &types.EbsBlockDevice{VolumeSize: new(img.RootGiB)}}},
		})
	}
	return out, nil
}

func (t InstanceType) info() types.InstanceTypeInfo {
	arch := types.ArchitectureTypeX8664
	if t.Arch == "arm64" {
		arch = types.ArchitectureTypeArm64
	}
	strategies := []types.PlacementGroupStrategy{types.PlacementGroupStrategySpread, types.PlacementGroupStrategyPartition}
	if !t.Burstable {
		strategies = append(strategies, types.PlacementGroupStrategyCluster)
	}
	hv := types.InstanceTypeHypervisorNitro
	if t.NonNitro {
		hv = types.InstanceTypeHypervisorXen
	}
	info := types.InstanceTypeInfo{
		InstanceType: types.InstanceType(t.Name), Hypervisor: hv, CurrentGeneration: new(true),
		BurstablePerformanceSupported: new(t.Burstable),
		VCpuInfo:                      &types.VCpuInfo{DefaultVCpus: new(t.VCPUs), DefaultCores: new(max(t.VCPUs/2, 1)), DefaultThreadsPerCore: new(min(t.VCPUs, 2))},
		ProcessorInfo:                 &types.ProcessorInfo{SupportedArchitectures: []types.ArchitectureType{arch}},
		NetworkInfo:                   &types.NetworkInfo{EnaSrdSupported: new(t.ENAExpress)},
		PlacementGroupInfo:            &types.PlacementGroupInfo{SupportedStrategies: strategies},
		SupportedInRegion:             new(true),
		InstanceStorageSupported:      new(t.LocalDisks > 0),
	}
	if t.LocalDisks > 0 {
		info.InstanceStorageInfo = &types.InstanceStorageInfo{
			NvmeSupport: types.EphemeralNvmeSupportRequired, TotalSizeInGB: &t.DiskGB,
			Disks: []types.DiskInfo{{Count: new(t.LocalDisks), SizeInGB: new(t.DiskGB / int64(t.LocalDisks)), Type: types.DiskTypeSsd}},
		}
	}
	return info
}

// DescribeInstanceTypes implements EC2API.
func (e *EC2) DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options),
) (*ec2.DescribeInstanceTypesOutput, error) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	if err := e.a.begin(ctx, "DescribeInstanceTypes", in); err != nil {
		return nil, err
	}
	var out []types.InstanceTypeInfo
	for _, want := range in.InstanceTypes {
		if _, ok := e.a.typeOf(string(want)); !ok {
			return nil, apiError("InvalidInstanceType", fmt.Sprintf("The following supplied instance types do not exist: [%s]", want))
		}
	}
	for _, t := range e.a.types {
		if len(in.InstanceTypes) > 0 && !slices.Contains(in.InstanceTypes, types.InstanceType(t.Name)) {
			continue
		}
		out = append(out, t.info())
	}
	// The real call pages 100 types at a time; MaxResults is honoured up to the
	// account's page size so that pagination is exercised.
	items, next := page(out, in.NextToken, in.MaxResults, e.a.bigPage)
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: items, NextToken: next}, nil
}

// DescribeInstanceTypeOfferings implements EC2API: the offerings of the zones of
// the region, located by zone ID, zone name or region.
func (e *EC2) DescribeInstanceTypeOfferings(ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	r, err := e.begin(ctx, "DescribeInstanceTypeOfferings", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.InstanceTypeOffering
	for _, t := range e.a.types {
		offered := r.offered[t.Name]
		if offered == nil {
			offered = map[string]bool{}
			for _, z := range r.azs {
				offered[z.ID] = true
			}
		}
		if in.LocationType == types.LocationTypeRegion {
			out = append(out, types.InstanceTypeOffering{InstanceType: types.InstanceType(t.Name),
				Location: new(r.name), LocationType: in.LocationType})
			continue
		}
		for _, z := range r.azs {
			if !offered[z.ID] || z.Type != "availability-zone" {
				continue
			}
			loc := z.ID
			if in.LocationType == types.LocationTypeAvailabilityZone {
				loc = z.Name
			}
			out = append(out, types.InstanceTypeOffering{InstanceType: types.InstanceType(t.Name),
				Location: new(loc), LocationType: in.LocationType})
		}
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.bigPage*10)
	return &ec2.DescribeInstanceTypeOfferingsOutput{InstanceTypeOfferings: items, NextToken: next}, nil
}

// DescribeSpotInstanceRequests implements EC2API.
func (e *EC2) DescribeSpotInstanceRequests(ctx context.Context, in *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	r, err := e.begin(ctx, "DescribeSpotInstanceRequests", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.SpotInstanceRequest
	for _, s := range r.spot {
		attrs := func(name string) ([]string, bool) {
			switch name {
			case "state":
				return []string{s.state}, true
			case "instance-id":
				return []string{s.instance}, true
			}
			return nil, false
		}
		ok, err := matches(in.Filters, nil, attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		req := types.SpotInstanceRequest{
			SpotInstanceRequestId: new(s.id), State: types.SpotInstanceState(s.state), CreateTime: &s.created,
			LaunchSpecification: &types.LaunchSpecification{InstanceType: types.InstanceType(s.typ)},
		}
		if s.instance != "" {
			req.InstanceId = new(s.instance)
		}
		out = append(out, req)
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.bigPage)
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: items, NextToken: next}, nil
}

// DescribeCapacityReservations implements EC2API.
func (e *EC2) DescribeCapacityReservations(ctx context.Context, in *ec2.DescribeCapacityReservationsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeCapacityReservationsOutput, error) {
	r, err := e.begin(ctx, "DescribeCapacityReservations", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.CapacityReservation
	for _, c := range r.crs {
		attrs := func(name string) ([]string, bool) {
			if name == "state" {
				return []string{c.State}, true
			}
			return nil, false
		}
		ok, err := matches(in.Filters, nil, attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, types.CapacityReservation{
			CapacityReservationId: new(c.ID), InstanceType: new(c.Type), AvailabilityZone: new(c.AZ),
			TotalInstanceCount: new(c.Total), AvailableInstanceCount: new(c.Free), State: types.CapacityReservationState(c.State),
			ReservationType: types.CapacityReservationType(c.Kind),
		})
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.bigPage)
	return &ec2.DescribeCapacityReservationsOutput{CapacityReservations: items, NextToken: next}, nil
}
