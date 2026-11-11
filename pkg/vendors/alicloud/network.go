/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// managedVPCCIDR is the address range of a managed VPC; each zone gets a /20.
// No internet gateway or route table is needed: ECS gives a public IPv4 address
// through InternetMaxBandwidthOut, and the VPC has a default route table.
const managedVPCCIDR = "10.240.0.0/16"

// vswitchTTL is how long the driver trusts what it read about a vSwitch (overview
// section 11.2).
const vswitchTTL = 5 * time.Minute

// networkWaitMax is how long one Ensure waits for a new VPC or vSwitch to become
// Available (VPC resources are created asynchronously) before it gives up with a
// Retryable error, which the next call continues.
const networkWaitMax = 90 * time.Second

// networkPoll is how often the wait looks again.
const networkPoll = 2 * time.Second

// codeNetworkPending: a created VPC or vSwitch is not Available yet.
const codeNetworkPending = "NetworkPending"

// vswitchState is what a launch needs to know about a vSwitch.
type vswitchState struct {
	id, vpc, zone string
	free          int64
	ipv6          bool
}

type cachedVSwitch struct {
	info  vswitchState
	until time.Time
}

func stateOfVSwitch(s *vswitchInfo) vswitchState {
	return vswitchState{
		id: deref(s.VSwitchId), vpc: deref(s.VpcId), zone: deref(s.ZoneId), free: deref(s.AvailableIpAddressCount),
		ipv6: deref(s.Ipv6CidrBlock) != "" || deref(s.EnabledIpv6),
	}
}

func (d *Driver) rememberVSwitch(s *vswitchInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.vsw[deref(s.VSwitchId)] = cachedVSwitch{info: stateOfVSwitch(s), until: d.clock.Now().Add(vswitchTTL)}
}

// forgetVSwitch drops what is known about a vSwitch (it ran out of addresses).
func (d *Driver) forgetVSwitch(id string) {
	d.mu.Lock()
	delete(d.vsw, id)
	d.mu.Unlock()
}

// vswitchesByID returns the vSwitches, reading the ones not cached. An ID that
// does not exist is absent from the result.
func (d *Driver) vswitchesByID(ctx context.Context, region string, ids []string) (map[string]vswitchState, error) {
	out := map[string]vswitchState{}
	var missing []string
	d.mu.Lock()
	for _, id := range ids {
		if c, ok := d.vsw[id]; ok && d.clock.Now().Before(c.until) {
			out[id] = c.info
		} else {
			missing = append(missing, id)
		}
	}
	d.mu.Unlock()
	for _, id := range missing {
		got, err := d.pageVSwitches(ctx, region, &vpc.DescribeVSwitchesRequest{
			RegionId: new(region), VSwitchId: new(id), PageSize: dara.Int32(vswitchPage),
		})
		if err != nil {
			return nil, err
		}
		for _, s := range got {
			d.rememberVSwitch(s)
			out[deref(s.VSwitchId)] = stateOfVSwitch(s)
		}
	}
	return out, nil
}

// vpcVSwitches returns the vSwitches of a VPC in the zone ("" for every zone).
func (d *Driver) vpcVSwitches(ctx context.Context, region, vpcID, zone string) ([]vswitchState, error) {
	in := &vpc.DescribeVSwitchesRequest{RegionId: new(region), VpcId: new(vpcID), PageSize: dara.Int32(vswitchPage)}
	if zone != "" {
		in.ZoneId = new(zone)
	}
	got, err := d.pageVSwitches(ctx, region, in)
	if err != nil {
		return nil, err
	}
	out := make([]vswitchState, 0, len(got))
	for _, s := range got {
		d.rememberVSwitch(s)
		out = append(out, stateOfVSwitch(s))
	}
	return out, nil
}

// vswitchCandidates returns the vSwitches of the resolved references in the
// zone: a vSwitch reference is itself, a VPC reference (a managed network, or a
// brought VPC) stands for its vSwitches.
func (d *Driver) vswitchCandidates(
	ctx context.Context, region, zone string, refs []vendors.ResourceRef,
) ([]vswitchState, error) {
	var ids []string
	var cands []vswitchState
	for _, r := range refs {
		if r.Kind != KindSubnets {
			continue
		}
		if strings.HasPrefix(r.ID, "vpc-") {
			subs, err := d.vpcVSwitches(ctx, region, r.ID, zone)
			if err != nil {
				return nil, err
			}
			cands = append(cands, subs...)
			continue
		}
		ids = append(ids, r.ID)
	}
	if len(ids) > 0 {
		known, err := d.vswitchesByID(ctx, region, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if s, ok := known[id]; ok {
				cands = append(cands, s)
			}
		}
	}
	return slices.DeleteFunc(cands, func(s vswitchState) bool { return s.zone != zone }), nil
}

// chooseVSwitch picks, among the resolved vSwitches (or the vSwitches of the
// VPCs), the one in the zone with the most free addresses. A zone without one is
// unusable (overview section 11.2).
func (d *Driver) chooseVSwitch(
	ctx context.Context, region, zone string, refs []vendors.ResourceRef,
) (vswitchState, error) {
	cands, err := d.vswitchCandidates(ctx, region, zone, refs)
	if err != nil {
		return vswitchState{}, err
	}
	if len(cands) == 0 {
		return vswitchState{}, vendors.NewError(vendors.ZoneUnusable, codeNoVSwitch,
			fmt.Errorf("no vSwitch in %s (%s)", zone, region))
	}
	slices.SortStableFunc(cands, func(a, b vswitchState) int {
		if a.free != b.free {
			if b.free > a.free {
				return 1
			}
			return -1
		}
		return strings.Compare(a.id, b.id)
	})
	return cands[0], nil
}

// launchVSwitch returns the vSwitch of a launch: the one Prepare chose, or, for a
// launch without prepared resources, the default VPC's default vSwitch of the
// zone.
func (d *Driver) launchVSwitch(ctx context.Context, region, zone, hint string) (vswitchState, error) {
	var refs []vendors.ResourceRef
	if hint != "" {
		refs = []vendors.ResourceRef{{Kind: KindSubnets, Region: region, ID: hint}}
	} else {
		var err error
		if refs, err = d.Default(ctx, KindSubnets, region); err != nil {
			return vswitchState{}, err
		}
	}
	return d.chooseVSwitch(ctx, region, zone, refs)
}

// launchResources are the resources of a launch: those Prepare resolved, and for
// a launch without them (the conformance suite, a caller that did not Prepare)
// the default VPC's default vSwitch of the zone and a security group of its VPC.
func (d *Driver) launchResources(ctx context.Context, req vendors.CreateRequest) (LaunchResources, error) {
	lr := resourcesOf(req.Prepared)
	sw, err := d.launchVSwitch(ctx, req.Region, req.Zone, lr.VSwitchID)
	if err != nil {
		return lr, err
	}
	lr.VSwitchID = sw.id
	if len(lr.SecurityGroupIDs) == 0 {
		id, err := d.fallbackGroup(ctx, req, sw.vpc)
		if err != nil {
			return lr, err
		}
		lr.SecurityGroupIDs = []string{id}
	}
	return lr, nil
}

// fallbackGroup is the group of a launch nobody prepared: a normal group of the
// VPC if it has one, else one the driver creates (ECS needs a group, and unlike
// other clouds it has no default one to fall back on), named by the cluster and
// the VPC, marked as ours and open to its own members.
func (d *Driver) fallbackGroup(ctx context.Context, req vendors.CreateRequest, vpcID string) (string, error) {
	groups, err := d.describeGroups(ctx, req.Region, vendors.UseRef{Match: matchNetwork(vpcID)})
	if err != nil {
		return "", err
	}
	slices.SortFunc(groups, func(a, b *groupInfo) int {
		return compare(deref(a.SecurityGroupId), deref(b.SecurityGroupId))
	})
	for _, g := range groups {
		if t := deref(g.SecurityGroupType); t == "" || t == "normal" {
			return deref(g.SecurityGroupId), nil
		}
	}
	params, err := json.Marshal(SecurityGroupParams{VPC: vpcID, SelfReference: true})
	if err != nil {
		return "", vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	ref, _, err := d.ensureSecurityGroup(ctx, KindSecurityGroups, req.Region, vendors.CreateSpec{
		Name: "scarpenter-" + d.cfg.ClusterName + "-nodes-" + vpcID,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: d.cfg.ClusterName, vendors.TagNodeClass: req.Tags[vendors.TagNodeClass],
			vendors.TagManagedBy: vendors.ManagedByValue, vendors.TagResource: string(KindSecurityGroups),
		},
		Params: params,
	})
	return ref.ID, err
}

// ---- the managed network ---------------------------------------------------------------

// ensureNetwork finds or creates the managed VPC of the name with its vSwitches
// (one per zone). Every part carries the ownership marker from the call that
// creates it, and the whole is completed on every call, so a creation interrupted
// half way is finished by the next one.
func (d *Driver) ensureNetwork(
	ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	vpcs, err := d.findVPCs(ctx, region, &vpc.DescribeVpcsRequest{
		RegionId: new(region), VpcName: new(spec.Name), Tag: vpcTagFilters(d.markerFilter(spec.Tags)),
	})
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	var vpcID string
	var tags map[vendors.TagKey]string
	created := false
	if len(vpcs) > 0 {
		vpcID, tags = deref(vpcs[0].VpcId), d.markerOfVPC(vpcs[0])
	} else {
		in := &vpc.CreateVpcRequest{
			RegionId: new(region), CidrBlock: new(managedVPCCIDR), VpcName: new(spec.Name),
			Description: new(sgDescription), ClientToken: new(clientToken("vpc", spec.Name)),
			Tag: d.vpcCreateTags(spec),
		}
		out, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*vpc.CreateVpcResponseBody, error) { return cl.VPC.CreateVpc(ctx, in) })
		if err != nil {
			return vendors.ResourceRef{}, false, err
		}
		vpcID, created, tags = deref(out.VpcId), true, spec.Tags
	}
	if err := d.waitNetwork(ctx, region, vpcID, "vpc"); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if err := d.completeNetwork(ctx, region, vpcID, spec); err != nil {
		return vendors.ResourceRef{}, false, fmt.Errorf("completing the network %s: %w", spec.Name, err)
	}
	return vendors.ResourceRef{
		Kind: KindSubnets, Region: region, ID: vpcID, Name: spec.Name, Managed: true, Tags: tags, Network: vpcID,
	}, created, nil
}

func clientToken(parts ...string) string {
	return ClientToken(strings.Join(parts, "/"), "", "", "", "", "", "", 0)
}

// completeNetwork creates the vSwitches the managed VPC lacks: one per zone, the
// first free /20 of the VPC's range (a zone that appears between others must not
// collide with a vSwitch that exists).
func (d *Driver) completeNetwork(ctx context.Context, region, vpcID string, spec vendors.CreateSpec) error {
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return err
	}
	have, err := d.pageVSwitches(ctx, region, &vpc.DescribeVSwitchesRequest{
		RegionId: new(region), VpcId: new(vpcID), PageSize: dara.Int32(vswitchPage),
	})
	if err != nil {
		return err
	}
	used := map[string]bool{}
	byZone := map[string]string{}
	for _, s := range have {
		used[deref(s.CidrBlock)] = true
		byZone[deref(s.ZoneId)] = deref(s.VSwitchId)
	}
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return err
	}
	var ids []string
	for _, z := range zones {
		if id, ok := byZone[z.Name]; ok {
			ids = append(ids, id)
			continue
		}
		cidr := ""
		for n := range 16 {
			if c := fmt.Sprintf("10.240.%d.0/20", n*16); !used[c] {
				cidr = c
				break
			}
		}
		if cidr == "" {
			return vendors.Errorf(vendors.Fatal, "the managed network %s has no free /20 left for %s", vpcID, z.Name)
		}
		used[cidr] = true
		s := spec
		s.Name = spec.Name + "-" + z.Name
		in := &vpc.CreateVSwitchRequest{
			RegionId: new(region), VpcId: new(vpcID), ZoneId: new(z.Name), CidrBlock: new(cidr), VSwitchName: new(s.Name),
			Description: new(sgDescription), ClientToken: new(clientToken("vsw", vpcID, z.Name, cidr)),
			Tag: d.vswitchCreateTags(s),
		}
		out, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*vpc.CreateVSwitchResponseBody, error) {
				return cl.VPC.CreateVSwitch(ctx, in)
			})
		if err != nil {
			return err
		}
		ids = append(ids, deref(out.VSwitchId))
	}
	for _, id := range ids {
		if err := d.waitNetwork(ctx, region, id, "vswitch"); err != nil {
			return err
		}
	}
	return nil
}

// waitNetwork waits for a VPC or a vSwitch to be Available. Creation is
// asynchronous: a launch into a pending vSwitch fails. After the wait it gives up
// with a Retryable error and the next Ensure continues.
func (d *Driver) waitNetwork(ctx context.Context, region, id, what string) error {
	looks := max(1, int(networkWaitMax/networkPoll))
	for i := range looks {
		ok, err := d.networkAvailable(ctx, region, id, what)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if i == looks-1 {
			break
		}
		if err := d.cloud.sleep(ctx, networkPoll); err != nil {
			return classify(err, errContext{})
		}
	}
	return vendors.NewError(vendors.Retryable, codeNetworkPending, fmt.Errorf("the %s %s is not available yet", what, id))
}

func (d *Driver) networkAvailable(ctx context.Context, region, id, what string) (bool, error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return false, err
	}
	if what == "vswitch" {
		got, err := d.pageVSwitches(ctx, region, &vpc.DescribeVSwitchesRequest{
			RegionId: new(region), VSwitchId: new(id), PageSize: dara.Int32(vswitchPage),
		})
		if err != nil {
			return false, err
		}
		return len(got) == 1 && deref(got[0].Status) == statusAvailable, nil
	}
	in := &vpc.DescribeVpcsRequest{RegionId: new(region), VpcId: new(id)}
	out, err := call(ctx, d.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*vpc.DescribeVpcsResponseBody, error) { return cl.VPC.DescribeVpcs(ctx, in) })
	if err != nil {
		return false, err
	}
	return out.Vpcs != nil && len(out.Vpcs.Vpc) == 1 && deref(out.Vpcs.Vpc[0].Status) == "Available", nil
}

// findVPCs reads every page of a DescribeVpcs call.
func (d *Driver) findVPCs(
	ctx context.Context, region string, in *vpc.DescribeVpcsRequest,
) ([]*vpc.DescribeVpcsResponseBodyVpcsVpc, error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	in.PageSize = dara.Int32(50)
	var out []*vpc.DescribeVpcsResponseBodyVpcsVpc
	for page := int32(1); ; page++ {
		in.PageNumber = new(page)
		res, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*vpc.DescribeVpcsResponseBody, error) { return cl.VPC.DescribeVpcs(ctx, in) })
		if err != nil {
			return nil, err
		}
		n := 0
		if res.Vpcs != nil {
			n = len(res.Vpcs.Vpc)
			out = append(out, res.Vpcs.Vpc...)
		}
		if n == 0 || int(page)*50 >= int(deref(res.TotalCount)) {
			return out, nil
		}
	}
}

// deleteNetwork deletes a managed VPC: its marked vSwitches first, then the VPC.
// A vSwitch or a VPC something still uses is Retryable (the dependency errors).
func (d *Driver) deleteNetwork(ctx context.Context, ref vendors.ResourceRef) error {
	cl, err := d.cloud.Clients(ctx, ref.Region)
	if err != nil {
		return err
	}
	vpcs, err := d.findVPCs(ctx, ref.Region, &vpc.DescribeVpcsRequest{RegionId: new(ref.Region), VpcId: new(ref.ID)})
	if err != nil {
		return err
	}
	if len(vpcs) == 0 {
		return nil
	}
	if !d.markedFor(d.markerOfVPC(vpcs[0]), ref) || deref(vpcs[0].IsDefault) {
		return vendors.Errorf(vendors.Fatal, "the VPC %s was not created by SCarpenter and is never deleted", ref.ID)
	}
	subs, err := d.pageVSwitches(ctx, ref.Region, &vpc.DescribeVSwitchesRequest{
		RegionId: new(ref.Region), VpcId: new(ref.ID), PageSize: dara.Int32(vswitchPage),
	})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if !d.markedFor(d.markerOfVSwitch(s), ref) {
			continue // not ours: the VPC delete below says it is still used
		}
		in := &vpc.DeleteVSwitchRequest{RegionId: new(ref.Region), VSwitchId: s.VSwitchId}
		_, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*vpc.DeleteVSwitchResponseBody, error) {
				return cl.VPC.DeleteVSwitch(ctx, in)
			})
		if err != nil && !isCode(err, "InvalidVSwitchId.NotFound") {
			return err
		}
		d.forgetVSwitch(deref(s.VSwitchId))
	}
	// The groups of the VPC that carry our marker go with it (the engine deletes the
	// groups it manages first; this takes the ones the driver made for a launch
	// nobody prepared). A group an instance still uses fails the delete: Retryable.
	groups, err := d.describeGroups(ctx, ref.Region, vendors.UseRef{Match: matchNetwork(ref.ID)})
	if err != nil {
		return err
	}
	for _, g := range groups {
		if d.markedFor(d.markerOfGroup(g), ref) {
			if err := d.deleteSecurityGroup(ctx, vendors.ResourceRef{
				Kind: KindSecurityGroups, Region: ref.Region, ID: deref(g.SecurityGroupId), Tags: ref.Tags,
			}); err != nil {
				return err
			}
		}
	}
	in := &vpc.DeleteVpcRequest{RegionId: new(ref.Region), VpcId: new(ref.ID)}
	_, err = call(ctx, d.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*vpc.DeleteVpcResponseBody, error) { return cl.VPC.DeleteVpc(ctx, in) })
	if err != nil && !isCode(err, "InvalidVpcID.NotFound", "InvalidVpcId.NotFound") {
		return err
	}
	return nil
}
