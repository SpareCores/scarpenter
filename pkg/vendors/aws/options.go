/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Device patterns the first-boot unit matches (00a section 11): an EBS volume's
// NVMe serial is "vol" plus the volume ID without the dash, which is known only
// after the launch, so the hint is the shell pattern vol* plus the size (the
// root volume has partitions and is skipped; volumes of equal size rely on the
// volume unit's lock). Instance store disks have the model name in their by-id
// path.
const (
	ebsSerialPattern    = "vol*"
	instanceStorePrefix = "nvme-Amazon_EC2_NVMe_Instance_Storage_*"
)

// gp3 includes 3000 IOPS and 125 MiB/s; only what is provisioned beyond costs.
const (
	gp3IncludedIOPS       = 3000
	gp3IncludedThroughput = 125
)

// Supported implements vendors.NodeOptions: the profiles of the NodeClass that
// the instance type can honour (ADR-017: nothing is degraded silently).
//   - storage: local storage (mount or raid0) needs instance store disks that
//     appear as NVMe devices; io2 above 64000 IOPS needs a Nitro instance;
//   - network: ENA Express needs a type that supports it;
//   - placement: a placement group of a strategy needs a type that supports the
//     strategy (burstable types have no cluster groups); a group by name
//     (use) is accepted, its strategy is not known here; dedicated tenancy is
//     accepted.
//
// The type facts come from DescribeInstanceTypes, read at runtime; until they
// are known a profile that depends on them is not offered, and the epoch of
// the node options changes when they arrive.
func (d *Driver) Supported(it vendors.CatalogType, _ string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	ti, known := d.types.get(it.APIReference)
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block, ti, known)
			case vendors.ProfileNetwork:
				ok = supportsNetwork(block, ti, known)
			case vendors.ProfilePlacement:
				ok = supportsPlacement(block, ti, known)
			}
			if ok {
				out[kind] = append(out[kind], name)
			}
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

func supportsStorage(block json.RawMessage, ti typeInfo, known bool) bool {
	st, err := decodeStorage(block)
	if err != nil {
		return false
	}
	if st.LocalStorage != nil && st.LocalStorage.Policy != common.LocalStorageIgnore && (!known || !ti.LocalNVMe) {
		return false
	}
	for _, v := range volumesOf(st) {
		if v.Type == awsapi.VolumeTypeIO2 && v.IOPS != nil && *v.IOPS > 64000 && (!known || !ti.Nitro) {
			return false
		}
	}
	return true
}

func volumesOf(st awsapi.Storage) []volume {
	var out []volume
	if st.Root != nil {
		out = append(out, rootVolume(awsapi.Settings{Storage: &st}))
	}
	for _, dv := range st.Data {
		out = append(out, dataVolume(dv))
	}
	return out
}

func supportsNetwork(block json.RawMessage, ti typeInfo, known bool) bool {
	n, err := decodeNetwork(block)
	if err != nil {
		return false
	}
	if n.ENAExpress != nil && n.ENAExpress.Enabled {
		return known && ti.ENAExpress
	}
	return true
}

func supportsPlacement(block json.RawMessage, ti typeInfo, known bool) bool {
	p, err := decodePlacement(block)
	if err != nil {
		return false
	}
	if pg := p.PlacementGroup; pg != nil && pg.Use == nil {
		return known && slices.Contains(ti.Strategies, string(pg.Strategy))
	}
	return true
}

// PriceDelta implements vendors.NodeOptions: the EBS volumes at Keeper's
// GB-month price per region and volume type divided by 730, plus the public
// IPv4 address where it is billed. The price is incomplete when a component is
// not priced: provisioned IOPS or throughput beyond what gp3 includes (and
// every io1 and io2 IOPS), dedicated tenancy, and any price the source lacks.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	vol := func(v volume) {
		price, ok := d.storagePrice(o.Region, string(v.Type))
		if !ok {
			complete = false
		} else {
			total += float64(v.GiB) * price / hoursPerMonth
		}
		switch v.Type {
		case awsapi.VolumeTypeGP3:
			if (v.IOPS != nil && *v.IOPS > gp3IncludedIOPS) || (v.Throughput != nil && *v.Throughput > gp3IncludedThroughput) {
				complete = false
			}
		case awsapi.VolumeTypeIO1, awsapi.VolumeTypeIO2:
			complete = false
		}
	}
	vol(rootVolume(st))
	for _, dv := range dataVolumes(st) {
		vol(dataVolume(dv))
	}
	if boolOr(s.Networking.PublicIPv4, true) {
		if p, ok := d.ipv4Price(o.Region); ok {
			total += p
		} else {
			complete = false
		}
	}
	if pl, err := decodePlacement(s.Placement); err == nil && pl.Tenancy == awsapi.Tenancy("dedicated") {
		complete = false
	}
	return total, complete
}

func (d *Driver) storagePrice(region, volumeType string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, volumeType)
}

func (d *Driver) ipv4Price(region string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.IPv4Price(region)
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch
// ready through the resource provider (the engine decides whether a resource is
// brought, the account's default or managed): the subnet of the zone, the
// security groups, the placement group of the profile and, for the volumes and
// local disks, the device hints for the first-boot unit. An error of the
// provider is returned as it is: a ResourceMissingInZone must reach the attempt
// loop with its code, so that it skips only the zone.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}

	// The subnet of the zone: among the resolved subnets, or the zone's subnet
	// of the managed network. The request names no zone: a managed network is
	// one resource per region (its VPC) and must not get a name per zone; the
	// provider expands {zone} of a `use` name pattern with the zone of the
	// launch.
	subnetRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{Kind: KindSubnets})
	if err != nil {
		return nil, err
	}
	sn, err := d.chooseSubnet(ctx, req.Region, req.Zone, subnetRefs)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, vendors.ResourceRef{Kind: KindSubnets, Region: req.Region, ID: sn.id})

	groups, err := d.requireGroups(ctx, req, sn)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, groups...)

	if g := pl.PlacementGroup; g != nil {
		ref, err := d.requirePlacementGroup(ctx, req, g)
		if err != nil {
			return nil, err
		}
		if err := d.checkGroupSize(ctx, req.Region, req.Zone, ref.Name); err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}

	for _, dv := range dataVolumes(st) {
		fs := string(dv.Filesystem)
		if fs == "" {
			fs = string(common.FilesystemExt4)
		}
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: dv.Name, Filesystem: fs, MountPath: dv.MountPath,
			SerialPattern: ebsSerialPattern, SizeBytes: int64(gib(&dv.VolumeSize)) * giB,
		})
	}
	if policy, mount, ok := localStorage(st); ok && policy != "" && policy != string(common.LocalStorageIgnore) {
		count := req.Type.LocalStorageCount
		if ti, known := d.types.get(req.InstanceType); known && ti.LocalDisks > 0 {
			count = ti.LocalDisks
		}
		prep.Local = &vendors.LocalHint{
			Policy: policy, MountPath: mount, DevicePattern: instanceStorePrefix, Count: max(count, 1),
		}
	}
	return prep, nil
}

// requireGroups returns the security groups of the launch: those the NodeClass
// brings (use), or the group of the NodeClass in the VPC of the subnet, and, for
// isolation run with a run key, the group of the run. Brought groups must be in
// the subnet's VPC; where they are not the zone is skipped.
func (d *Driver) requireGroups(
	ctx context.Context, req vendors.PrepareRequest, sn subnetInfo) ([]vendors.ResourceRef, error,
) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params := SecurityGroupParams{
		VPC: sn.vpc,
		// Isolation run without a run key has no run to keep apart: the nodes of
		// the NodeClass are one implicit run and reach each other (a VPC-routed CNI
		// of the direct template needs it).
		SelfReference: net.Isolation == common.IsolationNodeClass ||
			(net.Isolation == common.IsolationRun && req.RunKey == ""),
	}
	if net.Inbound != nil {
		params.Inbound = net.Inbound.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSecurityGroups, Key: class + "/" + sn.vpc, Params: raw,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Managed {
			continue
		}
		vpc, err := d.groupVPC(ctx, req.Region, r.ID)
		if err != nil {
			return nil, err
		}
		if vpc != sn.vpc {
			return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone,
				fmt.Errorf("the security group %s is in %s, not in %s, the VPC of the subnet in %s", r.ID, vpc, sn.vpc, req.Zone))
		}
	}
	out := slices.Clone(refs)
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		raw, err := json.Marshal(SecurityGroupParams{VPC: sn.vpc, SelfReference: true})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunSecurityGroups, Key: class + "/" + sn.vpc + "/" + req.RunKey, Params: raw,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, run...)
	}
	return out, nil
}

// groupVPC returns the VPC of a security group (cached for the subnet TTL).
func (d *Driver) groupVPC(ctx context.Context, region, id string) (string, error) {
	key := region + "/" + id
	d.mu.Lock()
	if c, ok := d.sgCheck[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return c.vpc, nil
	}
	d.mu.Unlock()
	cl, err := d.clients(ctx, region)
	if err != nil {
		return "", err
	}
	gs, err := d.describeGroups(ctx, cl, []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{id}}})
	if err != nil {
		return "", err
	}
	if len(gs) == 0 {
		return "", vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("the security group %s does not exist in %s", id, region))
	}
	vpc := deref(gs[0].VpcId)
	d.mu.Lock()
	d.sgCheck[key] = cachedSG{vpc: vpc, until: d.clock.Now().Add(subnetTTL)}
	d.mu.Unlock()
	return vpc, nil
}

// requirePlacementGroup returns the placement group of the profile: a group by
// name (use) or one created per profile and run key (a profile that creates
// groups needs a run key).
func (d *Driver) requirePlacementGroup(ctx context.Context, req vendors.PrepareRequest, g *awsapi.PlacementGroup,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindPlacementGroups}
	if g.Use != nil {
		rr.Use = &vendors.UseRef{NamePattern: g.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates placement groups and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		params, err := json.Marshal(PlacementGroupParams{Strategy: string(g.Strategy), PartitionCount: g.PartitionCount})
		if err != nil {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		rr.Params = params
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	if len(refs) == 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no placement group for the profile %q", profile))
	}
	if len(refs) > 1 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the name of the placement group matches %d groups; it must match one", len(refs)))
	}
	return refs[0], nil
}
