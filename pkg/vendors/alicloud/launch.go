/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strconv"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Resource kinds of the driver (plan 21 section 2).
const (
	KindSubnets           vendors.ResourceKind = "subnets" // vSwitches and their VPC
	KindSecurityGroups    vendors.ResourceKind = "securityGroups"
	KindRunSecurityGroups vendors.ResourceKind = "runSecurityGroups"
	KindDeploymentSets    vendors.ResourceKind = "deploymentSets"
)

// MaxUserData is SCarpenter's user-data budget (overview section 8.2); ECS
// takes 32 KB before the Base64 encoding, so the budget always fits.
const MaxUserData = 16 * 1024

// LaunchResources are the resources of a launch, as Prepare resolved them.
type LaunchResources struct {
	VSwitchID        string
	SecurityGroupIDs []string
	DeploymentSetID  string
}

// resourcesOf extracts the vSwitch, the security groups and the deployment set
// from the prepared resources (Prepare resolved them and checked the zone of the
// vSwitch).
func resourcesOf(p *vendors.Prepared) LaunchResources {
	var lr LaunchResources
	if p == nil {
		return lr
	}
	for _, r := range p.Resources {
		switch r.Kind {
		case KindSubnets:
			if lr.VSwitchID == "" {
				lr.VSwitchID = r.ID
			}
		case KindSecurityGroups, KindRunSecurityGroups:
			if !slices.Contains(lr.SecurityGroupIDs, r.ID) {
				lr.SecurityGroupIDs = append(lr.SecurityGroupIDs, r.ID)
			}
		case KindDeploymentSets:
			lr.DeploymentSetID = r.ID
		}
	}
	return lr
}

func invalid(format string, args ...any) error {
	return vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(format, args...))
}

// instanceTags are the tags of the instance (RunInstances' Tag.N tags the
// instance, its disks and its primary ENI in one parameter): the NodeClass's
// own tags and the logical tags, which win, as key to value in a fixed order.
// The Name tag AWS writes is not needed: InstanceName is the NodeClaim.
func instanceTags(req vendors.CreateRequest) []*ecs.RunInstancesRequestTag {
	m := map[string]string{}
	maps.Copy(m, req.ExtraTags)
	for k, v := range req.Tags {
		m[TagKey(k)] = v
	}
	keys := slices.Sorted(maps.Keys(m))
	out := make([]*ecs.RunInstancesRequestTag, 0, len(keys))
	for _, k := range keys {
		out = append(out, &ecs.RunInstancesRequestTag{Key: new(k), Value: new(m[k])})
	}
	return out
}

func tagValue(b bool) *string {
	if b {
		return new("true")
	}
	return nil
}

// BuildRunInstances builds the one RunInstances call of a launch (plan 21
// section 3, NodeClaim to instance): no launch templates, one instance, pay as
// you go, user data Base64, disks that go with the instance, the logical tags
// on everything, the public IPv4 as a bandwidth, and for spot the market price
// without a protection period. attempt salts the ClientToken (0 first).
//
// Differences from the plan, found on the SDK: the metadata options are
// request fields (HttpTokens, HttpEndpoint, HttpPutResponseHopLimit), not
// SecurityOptions, and SystemDisk.Size is a string while DataDisk.Size is a
// number.
func BuildRunInstances(req vendors.CreateRequest, lr LaunchResources, attempt int) (*ecs.RunInstancesRequest, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooLarge, fmt.Errorf(
			"the user data is %d bytes, the budget is %d", len(req.UserData), MaxUserData))
	}
	name, err := InstanceName(req.Tags[vendors.TagNodeClaim])
	if err != nil {
		return nil, invalid("%v", err)
	}
	if req.Image.Ref == "" {
		return nil, vendors.NewError(vendors.Fatal, codeImageNotFound, fmt.Errorf("no image for the launch"))
	}
	if lr.VSwitchID == "" {
		return nil, vendors.NewError(vendors.ZoneUnusable, codeNoVSwitch, fmt.Errorf(
			"no vSwitch for zone %s", req.Zone))
	}
	if len(lr.SecurityGroupIDs) == 0 {
		return nil, invalid("no security group for the launch")
	}
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	if v := req.Settings.Networking.PublicIPv6; v != nil && *v {
		return nil, invalid("publicIPv6 is not supported on ECS yet (it needs an IPv6 gateway the driver does not manage)")
	}
	root := rootDisk(st)
	if root.GiB < minSystemDiskGiB || root.GiB > maxSystemDiskGiB {
		return nil, invalid("the root disk of %d GiB is outside %d to %d GiB", root.GiB, minSystemDiskGiB, maxSystemDiskGiB)
	}
	if want := root.GiB * giB; req.Image.RootDiskBytes > want {
		return nil, invalid("the root disk of %d GiB is smaller than the image's disk of %d GiB",
			root.GiB, (req.Image.RootDiskBytes+giB-1)/giB)
	}
	tags := instanceTags(req)
	if len(tags) > maxTags {
		return nil, invalid("%d tags, ECS takes at most %d", len(tags), maxTags)
	}
	mbit, charge := bandwidth(st, req.Settings.Networking.PublicIPv4)
	if mbit < 0 || mbit > maxInternetBandwidth {
		return nil, invalid("internetMaxBandwidthOut %d is outside 0 to %d Mbit/s", mbit, maxInternetBandwidth)
	}
	sys := systemDisk(root)
	data := dataDiskRequests(st)
	in := &ecs.RunInstancesRequest{
		RegionId: new(req.Region), ZoneId: new(req.Zone),
		ImageId: new(req.Image.Ref), InstanceType: new(req.InstanceType),
		VSwitchId: new(lr.VSwitchID), SecurityGroupIds: dara.StringSlice(lr.SecurityGroupIDs),
		Amount: dara.Int32(1), MinAmount: dara.Int32(1), InstanceChargeType: new("PostPaid"),
		InstanceName: new(name), HostName: new(HostName(req.Tags[vendors.TagNodeClaim])),
		UserData:   new(base64.StdEncoding.EncodeToString(req.UserData)),
		SystemDisk: sys, DataDisk: data,
		InternetMaxBandwidthOut: new(mbit), InternetChargeType: new(charge),
		HttpTokens: new(httpTokens(st)), HttpEndpoint: new("enabled"),
		Tag: tags,
	}
	if req.IdempotencyKey != "" {
		in.ClientToken = new(ClientToken(req.IdempotencyKey, req.Region, req.Zone, req.InstanceType,
			req.CapacityType, req.Image.Ref, lr.VSwitchID, attempt))
	}
	if st.ResourceGroupID != "" {
		in.ResourceGroupId = new(st.ResourceGroupID)
	}
	if lr.DeploymentSetID != "" {
		in.DeploymentSetId = new(lr.DeploymentSetID)
	}
	if req.CapacityType == vendors.CapacitySpot {
		in.SpotStrategy = new("SpotAsPriceGo")
		in.SpotDuration = dara.Int32(0)
		in.SpotInterruptionBehavior = new("Terminate")
	}
	return in, nil
}

// systemDisk is the SystemDisk of the launch: the performance level only for
// ESSD, the KMS key only when encrypted, provisioned IOPS and bursting only for
// cloud_auto.
func systemDisk(root resolvedDisk) *ecs.RunInstancesRequestSystemDisk {
	sys := &ecs.RunInstancesRequestSystemDisk{
		Category: new(root.Category), Size: new(strconv.FormatInt(root.GiB, 10)),
		Encrypted: tagValue(root.Encrypted),
	}
	if root.Level != "" && root.Category == CategoryESSD {
		sys.PerformanceLevel = new(root.Level)
	}
	if root.Encrypted && root.KMSKeyID != "" {
		sys.KMSKeyId = new(root.KMSKeyID)
	}
	if root.Category == CategoryAuto {
		sys.ProvisionedIops, sys.BurstingEnabled = root.Iops, root.Bursting
	}
	return sys
}

// dataDiskRequests are the DataDisk.N of the launch, all deleted with the
// instance.
func dataDiskRequests(st Settings) []*ecs.RunInstancesRequestDataDisk {
	disks := dataDisks(st)
	data := make([]*ecs.RunInstancesRequestDataDisk, 0, len(disks))
	for _, d := range disks {
		dd := &ecs.RunInstancesRequestDataDisk{
			Category: new(d.Category), Size: new(int32(d.GiB)), DeleteWithInstance: new(true),
			Encrypted: tagValue(d.Encrypted),
		}
		if d.Name != "" {
			dd.DiskName = new(d.Name)
		}
		if d.Level != "" && d.Category == CategoryESSD {
			dd.PerformanceLevel = new(d.Level)
		}
		if d.Encrypted && d.KMSKeyID != "" {
			dd.KMSKeyId = new(d.KMSKeyID)
		}
		if d.Category == CategoryAuto {
			dd.ProvisionedIops, dd.BurstingEnabled = d.Iops, d.Bursting
		}
		data = append(data, dd)
	}
	return data
}
