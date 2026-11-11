/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// sgDescription is the description of the VPCs, vSwitches and groups SCarpenter
// creates.
const sgDescription = "Managed by SCarpenter"

// Kinds implements vendors.ManagedResources. The deletion order is deployment
// sets and run groups first (nothing depends on them but instances), then the
// NodeClass groups, then the network, which needs its groups gone. subnets is the
// VPC with its vSwitches: one managed resource, the VPC.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindDeploymentSets, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindRunSecurityGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 25,
			InNetwork: KindSubnets},
		{Kind: KindSecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30,
			InNetwork: KindSubnets},
		{Kind: KindSubnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	switch kind {
	case KindSubnets, KindSecurityGroups, KindRunSecurityGroups, KindDeploymentSets:
		return true
	}
	return false
}

// ---- tags and markers -------------------------------------------------------------------

// resourceTagMap are the tags of a managed resource: the NodeClass's own tags,
// then the ownership marker, which wins.
func resourceTagMap(spec vendors.CreateSpec) map[string]string {
	m := map[string]string{}
	maps.Copy(m, spec.ExtraTags)
	for k, v := range spec.Tags {
		m[TagKey(k)] = v
	}
	return m
}

func (d *Driver) vpcCreateTags(spec vendors.CreateSpec) []*vpc.CreateVpcRequestTag {
	m := resourceTagMap(spec)
	out := make([]*vpc.CreateVpcRequestTag, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, &vpc.CreateVpcRequestTag{Key: new(k), Value: new(m[k])})
	}
	return out
}

func (d *Driver) vswitchCreateTags(spec vendors.CreateSpec) []*vpc.CreateVSwitchRequestTag {
	m := resourceTagMap(spec)
	out := make([]*vpc.CreateVSwitchRequestTag, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, &vpc.CreateVSwitchRequestTag{Key: new(k), Value: new(m[k])})
	}
	return out
}

func (d *Driver) groupCreateTags(spec vendors.CreateSpec) []*ecs.CreateSecurityGroupRequestTag {
	m := resourceTagMap(spec)
	out := make([]*ecs.CreateSecurityGroupRequestTag, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, &ecs.CreateSecurityGroupRequestTag{Key: new(k), Value: new(m[k])})
	}
	return out
}

// findTags are the tags the driver looks a managed resource up by: the cluster,
// the marker and the kind, not the NodeClass (the name is per region and shared:
// a second NodeClass finds the resource of the first, as on AWS).
func findTags(tags map[vendors.TagKey]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range []vendors.TagKey{vendors.TagCluster, vendors.TagManagedBy, vendors.TagResource} {
		if v, ok := tags[k]; ok {
			out[k] = v
		}
	}
	return out
}

func (d *Driver) markerFilter(tags map[vendors.TagKey]string) map[string]string {
	out := map[string]string{}
	for k, v := range findTags(tags) {
		out[TagKey(k)] = v
	}
	return out
}

func vpcTagFilters(tags map[string]string) []*vpc.DescribeVpcsRequestTag {
	out := make([]*vpc.DescribeVpcsRequestTag, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, &vpc.DescribeVpcsRequestTag{Key: new(k), Value: new(tags[k])})
	}
	return out
}

func matchNetwork(vpcID string) common.ResourceMatch {
	return common.ResourceMatch{Network: []string{vpcID}}
}

// markerOf reads the marker tags (logical keys) out of a resource's tags.
func markerOf(byName map[string]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := byName[TagKey(k)]; ok {
			out[k] = v
		}
	}
	return out
}

func (d *Driver) markerOfVPC(v *vpc.DescribeVpcsResponseBodyVpcsVpc) map[vendors.TagKey]string {
	m := map[string]string{}
	if v.Tags != nil {
		for _, t := range v.Tags.Tag {
			m[deref(t.Key)] = deref(t.Value)
		}
	}
	return markerOf(m)
}

func (d *Driver) markerOfVSwitch(s *vswitchInfo) map[vendors.TagKey]string {
	return markerOf(vswitchFields(s).Tags)
}

func (d *Driver) markerOfGroup(g *groupInfo) map[vendors.TagKey]string {
	return markerOf(groupFields(g).Tags)
}

// owned says whether the tags carry the ownership marker of the cluster:
// cluster and managed-by (the NodeClass and kind tags are descriptive).
func owned(tags map[vendors.TagKey]string, cluster string) bool {
	return tags[vendors.TagManagedBy] == vendors.ManagedByValue && tags[vendors.TagCluster] == cluster && cluster != ""
}

// markedFor says whether the tags carry the ownership marker of the cluster the
// reference belongs to.
func (d *Driver) markedFor(tags map[vendors.TagKey]string, ref vendors.ResourceRef) bool {
	cluster := ref.Tags[vendors.TagCluster]
	if d.cfg.ClusterName != "" && cluster != d.cfg.ClusterName {
		return false
	}
	return owned(tags, cluster)
}

// The marker of a deployment set, which takes no tags (CreateDeploymentSet has no
// Tag parameter and TagResources has no deployment set type): its description,
// key=value pairs of the logical tags.
const descriptionPrefix = "scarpenter.sparecores.com: "

func setDescription(spec vendors.CreateSpec) string {
	var parts []string
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := spec.Tags[k]; ok {
			parts = append(parts, string(k)+"="+v)
		}
	}
	return descriptionPrefix + strings.Join(parts, " ")
}

func markerOfSet(s *setInfo) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	rest, ok := strings.CutPrefix(deref(s.DeploymentSetDescription), descriptionPrefix)
	if !ok {
		return out
	}
	for kv := range strings.FieldsSeq(rest) {
		if k, v, ok := strings.Cut(kv, "="); ok && slices.Contains(vendors.ResourceTagKeys, vendors.TagKey(k)) {
			out[vendors.TagKey(k)] = v
		}
	}
	return out
}

// ---- Resolve, Default, Ensure ---------------------------------------------------------

// Resolve implements vendors.ManagedResources: existing resources by IDs, name
// pattern or tags, never modified. An empty reference selects nothing: the
// default VPC's default vSwitches are Default's.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the alicloud driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the vendor's default is Default's")
	}
	switch kind {
	case KindSubnets:
		subs, err := d.describeVSwitches(ctx, region, ref)
		if err != nil {
			return nil, err
		}
		res, err := selector.Apply(ref, d.Selectors()[KindSubnets], subs, vswitchFields)
		d.noteMissing(KindSubnets, region, res.Missing)
		if err != nil {
			return nil, err
		}
		return d.vswitchRefs(region, res.Items), nil
	case KindDeploymentSets:
		sets, err := d.describeSets(ctx, region, ref)
		if err != nil {
			return nil, err
		}
		res, err := selector.Apply(ref, d.Selectors()[KindDeploymentSets], sets, setFields)
		d.noteMissing(KindDeploymentSets, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, s := range res.Items {
			out = append(out, vendors.ResourceRef{
				Kind: KindDeploymentSets, Region: region, ID: deref(s.DeploymentSetId), Name: deref(s.DeploymentSetName),
			})
		}
		slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
		return out, nil
	}
	groups, err := d.describeGroups(ctx, region, ref)
	if err != nil {
		return nil, err
	}
	res, err := selector.Apply(ref, d.Selectors()[kind], groups, groupFields)
	d.noteMissing(kind, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{
			Kind: kind, Region: region, ID: deref(g.SecurityGroupId), Name: deref(g.SecurityGroupName), Network: deref(g.VpcId),
		})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// vswitchRefs are the references of vSwitches, sorted by ID, with the VPC they
// belong to.
func (d *Driver) vswitchRefs(region string, subs []*vswitchInfo) []vendors.ResourceRef {
	out := make([]vendors.ResourceRef, 0, len(subs))
	for _, s := range subs {
		d.rememberVSwitch(s)
		out = append(out, vendors.ResourceRef{
			Kind: KindSubnets, Region: region, ID: deref(s.VSwitchId), Name: deref(s.VSwitchName), Network: deref(s.VpcId),
			CreatedAt: parseTime(deref(s.CreationTime)),
		})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Default implements vendors.ResourceDefaults: the default VPC's default vSwitches
// of the region, used as found and never modified. Security groups and deployment
// sets have no default: the account's default group is shared with everything
// else in the VPC and cannot isolate runs, so SCarpenter creates its own.
func (d *Driver) Default(ctx context.Context, kind vendors.ResourceKind, region string) ([]vendors.ResourceRef, error) {
	if kind != KindSubnets {
		return nil, nil
	}
	vpcs, err := d.findVPCs(ctx, region, &vpc.DescribeVpcsRequest{RegionId: new(region), IsDefault: new(true)})
	if err != nil {
		return nil, err
	}
	if len(vpcs) == 0 {
		return nil, nil
	}
	subs, err := d.pageVSwitches(ctx, region, &vpc.DescribeVSwitchesRequest{
		RegionId: new(region), VpcId: vpcs[0].VpcId, IsDefault: new(true), PageSize: dara.Int32(vswitchPage),
	})
	if err != nil {
		return nil, err
	}
	return d.vswitchRefs(region, subs), nil
}

// DefaultSuits implements vendors.DefaultChecker: publicIPv6 is not supported on
// ECS (it needs an IPv6 gateway the driver does not manage), so no default
// vSwitch suits it.
func (d *Driver) DefaultSuits(
	_ context.Context, kind vendors.ResourceKind, _ string, _ []vendors.ResourceRef, s vendors.VendorSettings,
) (bool, string, error) {
	if kind == KindSubnets && boolOr(s.Networking.PublicIPv6, false) {
		return false, errIPv6Unsupported, nil
	}
	return true, "", nil
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the creating call (a deployment set takes none:
// its marker is its description). created is false when an existing resource was
// adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"the alicloud driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	switch kind {
	case KindSubnets:
		return d.ensureNetwork(ctx, region, spec)
	case KindDeploymentSets:
		return d.ensureDeploymentSet(ctx, region, spec)
	}
	return d.ensureSecurityGroup(ctx, kind, region, spec)
}

// regionsOf returns the regions to list: the given ones the account can use, or
// every region of the account.
func (d *Driver) regionsOf(ctx context.Context, regions []string) ([]string, error) {
	all, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var usable []string
	for _, r := range all {
		if len(regions) == 0 || slices.Contains(regions, r.Name) {
			usable = append(usable, r.Name)
		}
	}
	return usable, nil
}

// List implements vendors.ManagedResources: the managed resources carrying all the
// tags, top-level resources only (the VPC of a managed network stands for its
// vSwitches). A region the account cannot use is skipped.
func (d *Driver) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	usable, err := d.regionsOf(ctx, regions)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, region := range usable {
		for _, list := range []func(context.Context, string, map[vendors.TagKey]string) ([]vendors.ResourceRef, error){
			d.listNetworks, d.listSecurityGroups, d.listDeploymentSets,
		} {
			refs, err := list(ctx, region, tags)
			if err != nil {
				if vendors.IsClass(err, vendors.RegionUnavailable) || isCode(err, regionDeniedCodes...) {
					break
				}
				return nil, fmt.Errorf("listing the managed resources of %s: %w", region, err)
			}
			out = append(out, refs...)
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
	return out, nil
}

// tagFilter turns logical tags into the Tag.N filters (at most 20).
func (d *Driver) tagFilter(tags map[vendors.TagKey]string) map[string]string {
	out := map[string]string{}
	for k, v := range tags {
		out[TagKey(k)] = v
	}
	return out
}

func (d *Driver) listNetworks(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	vpcs, err := d.findVPCs(ctx, region, &vpc.DescribeVpcsRequest{
		RegionId: new(region), Tag: vpcTagFilters(d.tagFilter(tags)),
	})
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, v := range vpcs {
		marker := d.markerOfVPC(v)
		if !matchesTags(marker, tags) || deref(v.IsDefault) {
			continue
		}
		out = append(out, vendors.ResourceRef{
			Kind: KindSubnets, Region: region, ID: deref(v.VpcId), Name: deref(v.VpcName),
			Managed: marker[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: marker, Network: deref(v.VpcId),
			CreatedAt: parseTime(deref(v.CreationTime)),
		})
	}
	return out, nil
}

// matchesTags says whether the marker carries every wanted tag.
func matchesTags(marker, want map[vendors.TagKey]string) bool {
	for k, v := range want {
		if marker[k] != v {
			return false
		}
	}
	return true
}

// InUse implements vendors.ManagedResources: whether a live instance uses the
// resource. When it is not in use the last use is unknown (ECS does not say), so
// the zero time is returned and the garbage collector counts the idle time from
// its first sighting of the resource.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if !isKind(ref.Kind) {
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the alicloud driver has no resource kind %q", ref.Kind)
	}
	exists, err := d.resourceExists(ctx, ref)
	if err != nil {
		return false, time.Time{}, err
	}
	if !exists {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the %s %s does not exist", ref.Kind, ref.ID)
	}
	if ref.Kind == KindDeploymentSets {
		sets, err := d.describeSets(ctx, ref.Region, vendors.UseRef{IDs: []string{ref.ID}})
		if err != nil {
			return false, time.Time{}, err
		}
		for _, s := range sets {
			if deref(s.InstanceAmount) > 0 {
				return true, d.clock.Now(), nil
			}
		}
		return false, time.Time{}, nil
	}
	in := &ecs.DescribeInstancesRequest{}
	switch ref.Kind {
	case KindSubnets:
		in.VpcId = new(ref.ID)
	default:
		in.SecurityGroupId = new(ref.ID)
	}
	insts, err := d.cloud.describeFirst(ctx, ref.Region, in)
	if err != nil {
		return false, time.Time{}, err
	}
	if insts {
		return true, d.clock.Now(), nil
	}
	return false, time.Time{}, nil
}

// resourceExists says whether the resource still exists.
func (d *Driver) resourceExists(ctx context.Context, ref vendors.ResourceRef) (bool, error) {
	switch ref.Kind {
	case KindSubnets:
		vpcs, err := d.findVPCs(ctx, ref.Region, &vpc.DescribeVpcsRequest{RegionId: new(ref.Region), VpcId: new(ref.ID)})
		return len(vpcs) > 0, err
	case KindDeploymentSets:
		sets, err := d.describeSets(ctx, ref.Region, vendors.UseRef{IDs: []string{ref.ID}})
		return len(sets) > 0, err
	}
	groups, err := d.describeGroups(ctx, ref.Region, vendors.UseRef{IDs: []string{ref.ID}})
	return len(groups) > 0, err
}

// Delete implements vendors.ManagedResources: only resources that carry the
// cluster's ownership marker are deleted (never one the user brought); a resource
// that is already gone is success; one that something still uses is Retryable
// (ECS's dependency errors).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	switch ref.Kind {
	case KindSubnets:
		if !strings.HasPrefix(ref.ID, "vpc-") {
			// A vSwitch is never a managed resource by itself: the managed resource
			// is the VPC that holds the vSwitches.
			return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.ID)
		}
		return d.deleteNetwork(ctx, ref)
	case KindSecurityGroups, KindRunSecurityGroups:
		return d.deleteSecurityGroup(ctx, ref)
	case KindDeploymentSets:
		return d.deleteDeploymentSet(ctx, ref)
	}
	return vendors.Errorf(vendors.Fatal, "the alicloud driver has no resource kind %q", ref.Kind)
}
