/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the AWS driver (00a section 9). The first three are the
// keys of spec.vendors.aws.resources. runSecurityGroups is not configurable: it
// is the extra group of a run key (isolation: run), which lives as long as an
// instance of the run uses it.
const (
	// KindSubnets is the regional network: the account's default VPC and its
	// default subnets as found, or, where the region has no default VPC or the
	// NodeClass says create, a managed VPC with one public subnet per zone, an
	// internet gateway and a route table. A managed network is one resource, the
	// VPC; its parts live and die with it.
	KindSubnets vendors.ResourceKind = "subnets"
	// KindSecurityGroups is the group of a NodeClass in a VPC.
	KindSecurityGroups vendors.ResourceKind = "securityGroups"
	// KindRunSecurityGroups is the group of a run key in a VPC.
	KindRunSecurityGroups vendors.ResourceKind = "runSecurityGroups"
	// KindPlacementGroups is a placement group of a run key and profile.
	KindPlacementGroups vendors.ResourceKind = "placementGroups"
)

// Kinds implements vendors.ManagedResources. The deletion order is placement
// groups and run groups first (nothing depends on them but instances), then the
// NodeClass groups, then the network, which needs its groups gone.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindPlacementGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindRunSecurityGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 25},
		{Kind: KindSecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
		{Kind: KindSubnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	switch kind {
	case KindSubnets, KindSecurityGroups, KindRunSecurityGroups, KindPlacementGroups:
		return true
	}
	return false
}

// resourceTags returns the tags of a managed resource: the NodeClass's own tags,
// then the ownership marker and the name, which win.
func (d *Driver) resourceTags(spec vendors.CreateSpec) []ec2types.Tag {
	m := map[string]string{}
	maps.Copy(m, spec.ExtraTags)
	m["Name"] = spec.Name
	for k, v := range spec.Tags {
		m[d.TagKey(k)] = v
	}
	return tagList(m)
}

// markerTags are the logical tags a managed resource carries, read from EC2 tags.
func (d *Driver) markerTags(tags []ec2types.Tag) map[vendors.TagKey]string {
	byName := map[string]string{}
	for _, t := range tags {
		byName[deref(t.Key)] = deref(t.Value)
	}
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := byName[d.TagKey(k)]; ok {
			out[k] = v
		}
	}
	return out
}

// owned says whether the tags carry the ownership marker of the cluster:
// cluster and managed-by (the NodeClass and kind tags are descriptive).
func owned(tags map[vendors.TagKey]string, cluster string) bool {
	return tags[vendors.TagManagedBy] == vendors.ManagedByValue && tags[vendors.TagCluster] == cluster && cluster != ""
}

func nameOf(tags []ec2types.Tag) string {
	for _, t := range tags {
		if deref(t.Key) == "Name" {
			return deref(t.Value)
		}
	}
	return ""
}

// Resolve implements vendors.ManagedResources: existing resources by IDs, name
// pattern or tags, never modified. With an empty reference the default VPC's
// default subnets; security groups and placement groups have no default
// (SCarpenter creates them). Filters are used instead of ID lists so that an ID
// that does not exist is an empty result, not an error.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the AWS driver has no resource kind %q", kind)
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	switch kind {
	case KindSubnets:
		return d.resolveSubnets(ctx, cl, region, ref)
	case KindPlacementGroups:
		return d.resolvePlacementGroups(ctx, cl, region, ref)
	}
	if ref.IsDefault() {
		return nil, nil
	}
	return d.resolveSecurityGroups(ctx, cl, kind, region, ref)
}

func (d *Driver) resolveSubnets(
	ctx context.Context, cl Clients, region string, ref vendors.UseRef) ([]vendors.ResourceRef, error,
) {
	var filters []ec2types.Filter
	if ref.IsDefault() {
		vpcs, err := cl.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			Filters: []ec2types.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}},
		})
		if err != nil {
			return nil, classify(err, errContext{})
		}
		if len(vpcs.Vpcs) == 0 {
			return nil, nil
		}
		filters = []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{deref(vpcs.Vpcs[0].VpcId)}},
			{Name: aws.String("default-for-az"), Values: []string{"true"}},
		}
	} else {
		filters = useFilters(ref, subnetFilters)
	}
	subs, err := d.describeSubnetPages(ctx, cl, &ec2.DescribeSubnetsInput{Filters: filters})
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		d.rememberSubnet(s)
	}
	if !ref.IsDefault() {
		res, err := selector.Apply(ref, d.Selectors()[KindSubnets], subs, subnetFields)
		d.noteMissing(KindSubnets, region, res.Missing)
		if err != nil {
			return nil, err
		}
		subs = res.Items
	}
	out := make([]vendors.ResourceRef, 0, len(subs))
	for _, s := range subs {
		out = append(out, vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: deref(s.SubnetId),
			Name: nameOf(s.Tags)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// noteMissing logs the IDs a `use` named that the vendor did not return: a
// missing ID does not fail the others, so it would otherwise go unnoticed.
func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

func (d *Driver) describeSubnetPages(
	ctx context.Context, cl Clients, in *ec2.DescribeSubnetsInput) ([]ec2types.Subnet, error,
) {
	var out []ec2types.Subnet
	for {
		res, err := cl.EC2.DescribeSubnets(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.Subnets...)
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the creating call. created is false when an
// existing resource was adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the AWS driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	switch kind {
	case KindSubnets:
		return d.ensureNetwork(ctx, cl, region, spec)
	case KindPlacementGroups:
		return d.ensurePlacementGroup(ctx, cl, region, spec)
	}
	return d.ensureSecurityGroup(ctx, cl, kind, region, spec)
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

// List implements vendors.ManagedResources: the managed resources carrying all
// the tags, top-level resources only (the VPC of a managed network stands for
// its subnets, gateway and route table).
func (d *Driver) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string) ([]vendors.ResourceRef, error,
) {
	usable, err := d.regionsOf(ctx, regions)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, region := range usable {
		cl, err := d.clients(ctx, region)
		if err != nil {
			return nil, err
		}
		filters := d.tagFilters(tags)
		for _, list := range []func(context.Context, Clients, string, []ec2types.Filter) ([]vendors.ResourceRef, error){
			d.listNetworks, d.listSecurityGroups, d.listPlacementGroups,
		} {
			refs, err := list(ctx, cl, region, filters)
			if err != nil {
				return nil, fmt.Errorf("listing the managed resources of %s: %w", region, err)
			}
			out = append(out, refs...)
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID,
			string(b.Kind)+b.ID)
	})
	return out, nil
}

// kindOfTags reads the kind of a managed resource from its marker, falling back
// to the kind of the API it was listed from.
func (d *Driver) kindOfTags(tags map[vendors.TagKey]string, fallback vendors.ResourceKind) vendors.ResourceKind {
	if k := vendors.ResourceKind(tags[vendors.TagResource]); isKind(k) {
		return k
	}
	return fallback
}

// InUse implements vendors.ManagedResources: whether a live instance uses the
// resource. When it is not in use the last use is unknown (EC2 does not say), so
// the zero time is returned and the garbage collector counts the idle time from
// its first sighting of the resource.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	var filter string
	switch ref.Kind {
	case KindSubnets:
		filter = "vpc-id"
	case KindSecurityGroups, KindRunSecurityGroups:
		filter = "instance.group-id"
	case KindPlacementGroups:
		filter = "placement-group-name"
	default:
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the AWS driver has no resource kind %q", ref.Kind)
	}
	value := ref.ID
	if ref.Kind == KindPlacementGroups {
		value = ref.Name
	}
	exists, err := d.resourceExists(ctx, ref)
	if err != nil {
		return false, time.Time{}, err
	}
	if !exists {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the %s %s does not exist", ref.Kind, ref.ID)
	}
	cl, err := d.clients(ctx, ref.Region)
	if err != nil {
		return false, time.Time{}, err
	}
	// A filtered DescribeInstances can return a short or empty page with a
	// NextToken: follow it until an instance is found or the token ends.
	in := &ec2.DescribeInstancesInput{
		MaxResults: aws.Int32(5),
		Filters: []ec2types.Filter{
			{Name: aws.String(filter), Values: []string{value}},
			{Name: aws.String("instance-state-name"), Values: listStates},
		},
	}
	for {
		res, err := cl.EC2.DescribeInstances(ctx, in)
		if err != nil {
			return false, time.Time{}, classify(err, errContext{})
		}
		for _, r := range res.Reservations {
			if len(r.Instances) > 0 {
				return true, d.clock.Now(), nil
			}
		}
		if res.NextToken == nil {
			return false, time.Time{}, nil
		}
		in.NextToken = res.NextToken
	}
}

// Delete implements vendors.ManagedResources: only resources that carry the
// cluster's ownership marker are deleted (never one the user brought); a
// resource that is already gone is success; one that something still uses is
// Retryable (EC2's dependency errors).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	cl, err := d.clients(ctx, ref.Region)
	if err != nil {
		return err
	}
	switch ref.Kind {
	case KindSubnets:
		if !strings.HasPrefix(ref.ID, "vpc-") {
			// A subnet is never a managed resource by itself: the managed
			// resource is the VPC that holds the subnets.
			return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.ID)
		}
		return d.deleteNetwork(ctx, cl, ref)
	case KindSecurityGroups, KindRunSecurityGroups:
		return d.deleteSecurityGroup(ctx, cl, ref)
	case KindPlacementGroups:
		return d.deletePlacementGroup(ctx, cl, ref)
	}
	return vendors.Errorf(vendors.Fatal, "the AWS driver has no resource kind %q", ref.Kind)
}

// resourceExists says whether the resource still exists.
func (d *Driver) resourceExists(ctx context.Context, ref vendors.ResourceRef) (bool, error) {
	cl, err := d.clients(ctx, ref.Region)
	if err != nil {
		return false, err
	}
	switch ref.Kind {
	case KindSubnets:
		out, err := cl.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{ref.ID}}},
		})
		if err != nil {
			return false, classify(err, errContext{})
		}
		return len(out.Vpcs) > 0, nil
	case KindSecurityGroups, KindRunSecurityGroups:
		out, err := cl.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{ref.ID}}},
		})
		if err != nil {
			return false, classify(err, errContext{})
		}
		return len(out.SecurityGroups) > 0, nil
	}
	out, err := cl.EC2.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-name"), Values: []string{ref.Name}}},
	})
	if err != nil {
		return false, classify(err, errContext{})
	}
	return len(out.PlacementGroups) > 0, nil
}
