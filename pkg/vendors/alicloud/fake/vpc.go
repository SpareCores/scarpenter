/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the fake repeats the SDK's long method signatures and type names
package fake

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
)

// VPC is the VPC endpoint of one region.
type VPC struct {
	f      *Cloud
	region string
}

// VPC returns the client of the region.
func (f *Cloud) VPC(region string) *VPC { return &VPC{f: f, region: region} }

type vpcRec struct {
	id, name, cidr string
	isDefault      bool
	tags           tagSet
	created        time.Time
	tokens         map[string]string
}

// status is Pending for NetworkPendingFor after the creation.
func (x *vpcRec) status(now time.Time, pendingFor time.Duration) string {
	if now.Sub(x.created) < pendingFor {
		return "Pending"
	}
	return statusAvailable
}

func (v *vswitchRec) status(now time.Time, pendingFor time.Duration) string {
	if now.Sub(v.created) < pendingFor {
		return "Pending"
	}
	return statusAvailable
}

type vswitchRec struct {
	id, name, vpc, zone string
	cidr                netip.Prefix
	isDefault           bool
	tags                tagSet
	created             time.Time
	// used are the addresses given to instances, on top of the reserved ones.
	used []netip.Addr
	// extra reduces the free addresses (other users of the vSwitch).
	extra int
}

func (v *vswitchRec) total() int { return 1 << (32 - v.cidr.Bits()) }

func (v *vswitchRec) free() int { return v.total() - reservedAddresses - len(v.used) - v.extra }

// allocate gives the next address of the vSwitch (after the reserved ones).
func (v *vswitchRec) allocate() string {
	a := v.cidr.Addr()
	for i := 0; i < reservedAddresses+len(v.used)+v.extra; i++ {
		a = a.Next()
	}
	for slices.Contains(v.used, a) {
		a = a.Next()
	}
	v.used = append(v.used, a)
	return a.String()
}

// Fill takes n addresses of the vSwitch away, so that a launch finds it full
// (QuotaExceeded.PrivateIpAddress when none is left).
func (f *Cloud) Fill(regionName, vswitchID string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).vswitches[vswitchID].extra += n
}

// AddDefaultNetwork gives the region a default VPC (172.16.0.0/12) and a
// default vSwitch (a /20) in every zone, as new accounts have, and returns the
// VPC ID and the vSwitch IDs by zone.
func (f *Cloud) AddDefaultNetwork(regionName string) (vpcID string, vswitches map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	vpcID = f.nextID("vpc")
	old := f.clock.Now().Add(-24 * time.Hour) // a default network is not new
	r.vpcs[vpcID] = &vpcRec{id: vpcID, name: "default", cidr: "172.16.0.0/12", isDefault: true, tags: tagSet{}, created: old}
	vswitches = map[string]string{}
	for i, z := range r.zones {
		id := f.nextID("vsw")
		r.vswitches[id] = &vswitchRec{
			id: id, name: "default", vpc: vpcID, zone: z, isDefault: true, tags: tagSet{}, created: old,
			cidr: netip.MustParsePrefix(fmt.Sprintf("172.16.%d.0/20", i*16)),
		}
		vswitches[z] = id
	}
	return vpcID, vswitches
}

func tagsFromCreate(keys []string, vals []string) (tagSet, error) {
	if err := checkTagKeys(keys); err != nil {
		return nil, err
	}
	t := tagSet{}
	for i, k := range keys {
		t[k] = vals[i]
	}
	return t, nil
}

// CreateVpc creates a VPC with its tags (CreateVpc takes Tag.N: the marker is
// atomic, which the plan listed as unverified); the ClientToken makes a replay
// return the same VPC.
func (v *VPC) CreateVpc(ctx context.Context, in *vpc.CreateVpcRequest) (*vpc.CreateVpcResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "CreateVpc", v.region)
	if err != nil {
		return nil, err
	}
	cidr := dara.StringValue(in.CidrBlock)
	if _, err := netip.ParsePrefix(cidr); err != nil {
		return nil, badRequest("InvalidCidrBlock.Malformed", "the CIDR block %q is not valid", cidr)
	}
	var keys, vals []string
	for _, t := range in.Tag {
		keys, vals = append(keys, dara.StringValue(t.Key)), append(vals, dara.StringValue(t.Value))
	}
	tags, err := tagsFromCreate(keys, vals)
	if err != nil {
		return nil, err
	}
	if tok := dara.StringValue(in.ClientToken); tok != "" {
		for _, x := range r.vpcs {
			if x.tokens[tok] == cidr {
				return &vpc.CreateVpcResponseBody{RequestId: new("req"), VpcId: new(x.id)}, nil
			}
		}
	}
	id := f.nextID("vpc")
	rec := &vpcRec{id: id, name: dara.StringValue(in.VpcName), cidr: cidr, tags: tags, created: f.clock.Now(), tokens: map[string]string{}}
	if tok := dara.StringValue(in.ClientToken); tok != "" {
		rec.tokens[tok] = cidr
	}
	r.vpcs[id] = rec
	return &vpc.CreateVpcResponseBody{RequestId: new("req"), VpcId: new(id), VRouterId: new("vrt-" + id)}, nil
}

// DescribeVpcs filters by VpcId, VpcName, IsDefault and Tag.N (AND), pages by
// PageNumber and PageSize.
func (v *VPC) DescribeVpcs(ctx context.Context, in *vpc.DescribeVpcsRequest) (*vpc.DescribeVpcsResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeVpcs", v.region)
	if err != nil {
		return nil, err
	}
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	var all []*vpcRec
	for _, x := range r.vpcs {
		switch {
		case in.VpcId != nil && *in.VpcId != x.id:
		case in.VpcName != nil && *in.VpcName != x.name:
		case in.IsDefault != nil && *in.IsDefault != x.isDefault:
		case !x.tags.matches(filter):
		default:
			all = append(all, x)
		}
	}
	slices.SortFunc(all, func(a, b *vpcRec) int { return strings.Compare(a.id, b.id) })
	from, to := page(len(all), dara.Int32Value(in.PageNumber), dara.Int32Value(in.PageSize))
	out := &vpc.DescribeVpcsResponseBody{RequestId: new("req"), TotalCount: new(int32(len(all))), Vpcs: &vpc.DescribeVpcsResponseBodyVpcs{}}
	for _, x := range all[from:to] {
		sdk := &vpc.DescribeVpcsResponseBodyVpcsVpc{
			VpcId: new(x.id), VpcName: new(x.name), CidrBlock: new(x.cidr), IsDefault: new(x.isDefault),
			RegionId: new(v.region), Status: new(x.status(f.clock.Now(), f.NetworkPendingFor)),
			CreationTime: new(x.created.UTC().Format("2006-01-02T15:04:05Z")),
			Tags:         &vpc.DescribeVpcsResponseBodyVpcsVpcTags{},
			VSwitchIds:   &vpc.DescribeVpcsResponseBodyVpcsVpcVSwitchIds{},
		}
		for _, k := range sortedKeys(x.tags) {
			sdk.Tags.Tag = append(sdk.Tags.Tag, &vpc.DescribeVpcsResponseBodyVpcsVpcTagsTag{Key: new(k), Value: new(x.tags[k])})
		}
		for _, s := range r.vswitches {
			if s.vpc == x.id {
				sdk.VSwitchIds.VSwitchId = append(sdk.VSwitchIds.VSwitchId, new(s.id))
			}
		}
		slices.SortFunc(sdk.VSwitchIds.VSwitchId, func(a, b *string) int { return strings.Compare(*a, *b) })
		out.Vpcs.Vpc = append(out.Vpcs.Vpc, sdk)
	}
	return out, nil
}

// DeleteVpc deletes a VPC; one with vSwitches or security groups is a
// DependencyViolation.* (the codes are unverified).
func (v *VPC) DeleteVpc(ctx context.Context, in *vpc.DeleteVpcRequest) (*vpc.DeleteVpcResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DeleteVpc", v.region)
	if err != nil {
		return nil, err
	}
	id := dara.StringValue(in.VpcId)
	if _, ok := r.vpcs[id]; !ok {
		return nil, badRequest("InvalidVpcID.NotFound", "the VPC does not exist")
	}
	for _, s := range r.vswitches {
		if s.vpc == id {
			return nil, badRequest("DependencyViolation.VSwitch", "the VPC has vSwitches")
		}
	}
	for _, g := range r.groups {
		if g.vpc == id {
			return nil, badRequest("DependencyViolation.SecurityGroup", "the VPC has security groups")
		}
	}
	delete(r.vpcs, id)
	return &vpc.DeleteVpcResponseBody{RequestId: new("req")}, nil
}

// CreateVSwitch creates a vSwitch in a zone of the region, inside the VPC's
// CIDR block, with its tags.
func (v *VPC) CreateVSwitch(ctx context.Context, in *vpc.CreateVSwitchRequest) (*vpc.CreateVSwitchResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "CreateVSwitch", v.region)
	if err != nil {
		return nil, err
	}
	pv, ok := r.vpcs[dara.StringValue(in.VpcId)]
	if !ok {
		return nil, badRequest("InvalidVpcId.NotFound", "the VPC does not exist")
	}
	zone := dara.StringValue(in.ZoneId)
	if !slices.Contains(r.zones, zone) {
		return nil, badRequest("InvalidZoneId.NotFound", "the zone %q does not exist", zone)
	}
	cidr, err := netip.ParsePrefix(dara.StringValue(in.CidrBlock))
	if err != nil {
		return nil, badRequest("InvalidCidrBlock.Malformed", "the CIDR block is not valid")
	}
	parent := netip.MustParsePrefix(pv.cidr)
	if !parent.Contains(cidr.Addr()) || cidr.Bits() < parent.Bits() || cidr.Bits() > 29 {
		return nil, badRequest("InvalidCidrBlock.NotWithinVpc", "the CIDR block is outside the VPC or too small")
	}
	for _, s := range r.vswitches {
		if s.vpc == pv.id && s.cidr.Overlaps(cidr) {
			return nil, badRequest("InvalidCidrBlock.Overlapped", "the CIDR block overlaps another vSwitch")
		}
	}
	var keys, vals []string
	for _, t := range in.Tag {
		keys, vals = append(keys, dara.StringValue(t.Key)), append(vals, dara.StringValue(t.Value))
	}
	tags, err := tagsFromCreate(keys, vals)
	if err != nil {
		return nil, err
	}
	id := f.nextID("vsw")
	r.vswitches[id] = &vswitchRec{
		id: id, name: dara.StringValue(in.VSwitchName), vpc: pv.id, zone: zone, cidr: cidr.Masked(), tags: tags, created: f.clock.Now(),
	}
	return &vpc.CreateVSwitchResponseBody{RequestId: new("req"), VSwitchId: new(id)}, nil
}

// DescribeVSwitches filters by VSwitchId, VpcId, ZoneId, name, IsDefault and
// Tag.N (AND), pages by PageNumber and PageSize, and reports the free
// addresses.
func (v *VPC) DescribeVSwitches(ctx context.Context, in *vpc.DescribeVSwitchesRequest) (*vpc.DescribeVSwitchesResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeVSwitches", v.region)
	if err != nil {
		return nil, err
	}
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	var all []*vswitchRec
	for _, s := range r.vswitches {
		switch {
		case in.VSwitchId != nil && *in.VSwitchId != s.id:
		case in.VpcId != nil && *in.VpcId != s.vpc:
		case in.ZoneId != nil && *in.ZoneId != s.zone:
		case in.VSwitchName != nil && *in.VSwitchName != s.name:
		case in.IsDefault != nil && *in.IsDefault != s.isDefault:
		case !s.tags.matches(filter):
		default:
			all = append(all, s)
		}
	}
	slices.SortFunc(all, func(a, b *vswitchRec) int { return strings.Compare(a.id, b.id) })
	from, to := page(len(all), dara.Int32Value(in.PageNumber), dara.Int32Value(in.PageSize))
	out := &vpc.DescribeVSwitchesResponseBody{
		RequestId: new("req"), TotalCount: new(int32(len(all))), VSwitches: &vpc.DescribeVSwitchesResponseBodyVSwitches{},
	}
	for _, s := range all[from:to] {
		sdk := &vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitch{
			VSwitchId: new(s.id), VSwitchName: new(s.name), VpcId: new(s.vpc), ZoneId: new(s.zone),
			CidrBlock: new(s.cidr.String()), IsDefault: new(s.isDefault), Status: new(s.status(f.clock.Now(), f.NetworkPendingFor)),
			AvailableIpAddressCount: new(int64(max(s.free(), 0))),
			CreationTime:            new(s.created.UTC().Format("2006-01-02T15:04:05Z")),
			Tags:                    &vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitchTags{},
		}
		for _, k := range sortedKeys(s.tags) {
			sdk.Tags.Tag = append(sdk.Tags.Tag, &vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitchTagsTag{Key: new(k), Value: new(s.tags[k])})
		}
		out.VSwitches.VSwitch = append(out.VSwitches.VSwitch, sdk)
	}
	return out, nil
}

// DeleteVSwitch deletes a vSwitch; one with instances is a
// DependencyViolation.Instance (the code is unverified).
func (v *VPC) DeleteVSwitch(ctx context.Context, in *vpc.DeleteVSwitchRequest) (*vpc.DeleteVSwitchResponseBody, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DeleteVSwitch", v.region)
	if err != nil {
		return nil, err
	}
	id := dara.StringValue(in.VSwitchId)
	if _, ok := r.vswitches[id]; !ok {
		return nil, badRequest("InvalidVSwitchId.NotFound", "the vSwitch does not exist")
	}
	if r.inUse(func(i *instance) bool { return i.vswitch == id }) {
		return nil, badRequest("DependencyViolation.Instance", "the vSwitch has instances")
	}
	delete(r.vswitches, id)
	return &vpc.DeleteVSwitchResponseBody{RequestId: new("req")}, nil
}

// RemoveDefaultNetwork removes the region's default VPC, its vSwitches and the
// groups in it (an account without a default VPC, as a deleted one is).
func (f *Cloud) RemoveDefaultNetwork(regionName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	for id, v := range r.vpcs {
		if !v.isDefault {
			continue
		}
		for sid, s := range r.vswitches {
			if s.vpc == id {
				delete(r.vswitches, sid)
			}
		}
		for gid, g := range r.groups {
			if g.vpc == id {
				delete(r.groups, gid)
			}
		}
		delete(r.vpcs, id)
	}
}

// SetCreated sets the creation time of a VPC, vSwitch, security group,
// deployment set or image, for the tests of the creation-time selectors.
func (f *Cloud) SetCreated(regionName, id string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	switch {
	case r.vpcs[id] != nil:
		r.vpcs[id].created = at
	case r.vswitches[id] != nil:
		r.vswitches[id].created = at
	case r.groups[id] != nil:
		r.groups[id].created = at
	case r.sets[id] != nil:
		r.sets[id].created = at
	case r.images[id] != nil:
		r.images[id].Created = at
	}
}

// DefaultVPC returns the ID of the region's default VPC, "" without one.
func (f *Cloud) DefaultVPC(regionName string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, v := range f.mustRegion(regionName).vpcs {
		if v.isDefault {
			return id
		}
	}
	return ""
}
