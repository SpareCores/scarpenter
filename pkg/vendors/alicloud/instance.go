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
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// describeMax is the page size of DescribeInstances (its maximum).
const describeMax = 100

// idsFilter is the InstanceIds parameter: a JSON array of up to 100 IDs.
func idsFilter(ids ...string) *string {
	b, _ := json.Marshal(ids)
	return new(string(b))
}

// tagFilters are the Tag.N filters of DescribeInstances for logical tags.
func tagFilters(tags map[vendors.TagKey]string) []*ecs.DescribeInstancesRequestTag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, string(k))
	}
	slices.Sort(keys)
	out := make([]*ecs.DescribeInstancesRequestTag, 0, len(keys))
	for _, k := range keys {
		out = append(out, &ecs.DescribeInstancesRequestTag{
			Key: new(TagKey(vendors.TagKey(k))), Value: new(tags[vendors.TagKey(k)]),
		})
	}
	return out
}

// logicalTags reads the logical tags back from the instance's tags (the
// NodeClass's own tags are not part of the contract).
func logicalTags(in *ecs.DescribeInstancesResponseBodyInstancesInstance) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	if in.Tags == nil {
		return out
	}
	for _, t := range in.Tags.Tag {
		if k, ok := logicalKey(deref(t.TagKey)); ok {
			out[k] = deref(t.TagValue)
		}
	}
	return out
}

// addressesOf lists the instance's addresses: the primary private IPv4, the
// public IPv4 (or the EIP), and the ENI's addresses, without duplicates (the
// serving certificate approver checks them, overview section 8.5).
func addressesOf(in *ecs.DescribeInstancesResponseBodyInstancesInstance) []string {
	var out []string
	add := func(a string) {
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if v := in.VpcAttributes; v != nil && v.PrivateIpAddress != nil {
		for _, a := range v.PrivateIpAddress.IpAddress {
			add(deref(a))
		}
	}
	if in.PublicIpAddress != nil {
		for _, a := range in.PublicIpAddress.IpAddress {
			add(deref(a))
		}
	}
	if in.EipAddress != nil {
		add(deref(in.EipAddress.IpAddress))
	}
	if in.NetworkInterfaces != nil {
		for _, ni := range in.NetworkInterfaces.NetworkInterface {
			add(deref(ni.PrimaryIpAddress))
			if ni.Ipv6Sets != nil {
				for _, v6 := range ni.Ipv6Sets.Ipv6Set {
					add(deref(v6.Ipv6Address))
				}
			}
		}
	}
	return out
}

// toInstance converts an ECS instance.
func toInstance(region string, in *ecs.DescribeInstancesResponseBodyInstancesInstance) *vendors.Instance {
	capacity := vendors.CapacityOnDemand
	if s := deref(in.SpotStrategy); s != "" && s != "NoSpot" {
		capacity = vendors.CapacitySpot
	}
	return &vendors.Instance{
		Ref: vendors.NewInstanceRef(region, deref(in.InstanceId)), Region: region, Zone: deref(in.ZoneId),
		Type: deref(in.InstanceType), CapacityType: capacity, State: StateOf(deref(in.Status)),
		CreatedAt: parseTime(deref(in.CreationTime)), Tags: logicalTags(in), Addresses: addressesOf(in),
	}
}

func notFound(ref string) error {
	return vendors.NewError(vendors.NotFound, "InvalidInstanceId.NotFound", fmt.Errorf("instance %s not found", ref))
}

// describe reads a region's instances matching the request, all pages, and
// returns them as ECS reports them.
func (c *Cloud) describe(
	ctx context.Context, region string, in *ecs.DescribeInstancesRequest,
) ([]*ecs.DescribeInstancesResponseBodyInstancesInstance, error) {
	cl, err := c.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	in.RegionId = new(region)
	in.MaxResults = dara.Int32(describeMax)
	var out []*ecs.DescribeInstancesResponseBodyInstancesInstance
	for {
		page, err := call(ctx, c, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.DescribeInstancesResponseBody, error) {
				return cl.ECS.DescribeInstances(ctx, in)
			})
		if err != nil {
			return nil, err
		}
		if page.Instances != nil {
			out = append(out, page.Instances.Instance...)
		}
		if deref(page.NextToken) == "" {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}

// GetInstance reads an instance by its reference; NotFound when ECS does not
// know it (a released instance is gone) and for a reference it cannot parse.
func (c *Cloud) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if ref.Region() == "" || ref.ID() == "" {
		return nil, notFound(ref.String())
	}
	insts, err := c.describe(ctx, ref.Region(), &ecs.DescribeInstancesRequest{InstanceIds: idsFilter(ref.ID())})
	if err != nil {
		return nil, err
	}
	for _, in := range insts {
		if deref(in.InstanceId) == ref.ID() {
			return toInstance(ref.Region(), in), nil
		}
	}
	return nil, notFound(ref.String())
}

// matches reports whether the instance carries all the tags (the Tag.N filter
// has AND semantics according to the API, unverified, so the match is checked
// again client-side).
func matches(i *vendors.Instance, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		if i.Tags[k] != v {
			return false
		}
	}
	return true
}

// FindInstance looks an instance up by logical tags in the region; NotFound
// when there is none. The oldest match wins, so a duplicate never hides the
// original.
func (c *Cloud) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	insts, err := c.ListInstances(ctx, []string{region}, tags)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, notFound("with the tags")
	}
	slices.SortStableFunc(insts, func(a, b *vendors.Instance) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return insts[0], nil
}

// ListInstances returns the regions' instances that carry all the tags,
// terminated ones excluded (ECS does not return released instances at all) and
// terminating (stopped) ones included.
func (c *Cloud) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	var out []*vendors.Instance
	for _, region := range regions {
		insts, err := c.describe(ctx, region, &ecs.DescribeInstancesRequest{Tag: tagFilters(tags)})
		if err != nil {
			return nil, err
		}
		for _, in := range insts {
			i := toInstance(region, in)
			if i.State != vendors.StateTerminated && matches(i, tags) {
				out = append(out, i)
			}
		}
	}
	return out, nil
}

// CreateInstance launches one instance (plan 21 section 3, Idempotency). ECS
// has a ClientToken but no filter on it, so the idempotency is built on the
// nodeclaim tag: before each launch the instance of the NodeClaim in the region
// is looked up and returned if it exists (a replay with another offering
// returns the original); a launch is a RunInstances with the token of
// ClientToken(); an IdempotentParameterMismatch resolves through the same lookup,
// and when nothing is found the launch is repeated once with a salted token (a
// failed create leaves the key unused).
//
// TODO(plan 14): the Driver's CreateInstance wraps this with the image and
// resource resolution (Prepare) and the settings merge.
func (c *Cloud) CreateInstance(
	ctx context.Context, req vendors.CreateRequest, lr LaunchResources,
) (*vendors.Instance, error) {
	nodeClaim := req.Tags[vendors.TagNodeClaim]
	byClaim := map[vendors.TagKey]string{vendors.TagNodeClaim: nodeClaim}
	if nodeClaim != "" {
		if inst, err := c.FindInstance(ctx, req.Region, byClaim); err == nil {
			return inst, nil
		} else if !vendors.IsClass(err, vendors.NotFound) {
			return nil, err
		}
	}
	cl, err := c.Clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	ec := errContext{instanceType: req.InstanceType, zone: req.Zone}
	for attempt := 0; ; attempt++ {
		in, err := BuildRunInstances(req, lr, attempt)
		if err != nil {
			return nil, err
		}
		out, err := call(ctx, c, runTimeout, ec, func(ctx context.Context) (*ecs.RunInstancesResponseBody, error) {
			return cl.ECS.RunInstances(ctx, in)
		})
		if err == nil {
			return c.launched(ctx, req, out)
		}
		if !isCode(err, codeIdempotentMismatch) {
			return nil, err
		}
		// The token was used with other parameters: the instance it created is
		// the NodeClaim's; with none found the token is spent, use another.
		if inst, ferr := c.FindInstance(ctx, req.Region, byClaim); ferr == nil {
			return inst, nil
		} else if !vendors.IsClass(ferr, vendors.NotFound) {
			return nil, ferr
		}
		if attempt >= 1 {
			return nil, err
		}
	}
}

// launched turns the answer of RunInstances into the instance: read back, or,
// when ECS does not show it yet, the pending instance the request describes.
func (c *Cloud) launched(
	ctx context.Context, req vendors.CreateRequest, out *ecs.RunInstancesResponseBody,
) (*vendors.Instance, error) {
	if out.InstanceIdSets == nil || len(out.InstanceIdSets.InstanceIdSet) == 0 {
		return nil, vendors.Errorf(vendors.Fatal, "RunInstances returned no instance")
	}
	ref := vendors.NewInstanceRef(req.Region, deref(out.InstanceIdSets.InstanceIdSet[0]))
	inst, err := c.GetInstance(ctx, ref)
	if err == nil {
		return inst, nil
	}
	if !vendors.IsClass(err, vendors.NotFound) {
		return nil, err
	}
	return &vendors.Instance{
		Ref: ref, Region: req.Region, Zone: req.Zone, Type: req.InstanceType, CapacityType: req.CapacityType,
		State: vendors.StatePending, CreatedAt: c.clock.Now(), Tags: req.Tags,
	}, nil
}

// CheckLaunched is the check for a stock shortage after acceptance (plan 21,
// Async): RunInstances accepts the call while the instance is Pending, and a
// shortage appears as a SystemFailure.Delete event after which the instance is
// released. An instance that is gone within launchTimeout of its creation
// is InsufficientCapacity (code InstanceVanished); one that is gone later is a
// plain NotFound. It returns nil when the instance exists.
//
// TODO(plan 14): the Driver's GetInstance calls this for instances younger than
// the launch timeout; how fast ECS releases the instance and whether
// DescribeInstances shows it first is unverified (step 0).
func (c *Cloud) CheckLaunched(
	ctx context.Context, ref vendors.InstanceRef, createdAt time.Time, launchTimeout time.Duration,
) error {
	_, err := c.GetInstance(ctx, ref)
	if err == nil || !vendors.IsClass(err, vendors.NotFound) {
		return err
	}
	if c.clock.Since(createdAt) <= launchTimeout {
		return vendors.NewError(vendors.InsufficientCapacity, codeInstanceVanished, fmt.Errorf(
			"instance %s was released right after it was created (SystemFailure.Delete)", ref))
	}
	return err
}

// DeleteInstance releases the instance: it reads it, refuses one that does not
// carry this cluster's tag (as AWS), and calls DeleteInstance with Force, which
// releases a running instance. An instance that is gone, or a reference that
// cannot be parsed, is success. Disks and the primary ENI go with the instance
// (DeleteWithInstance, DeleteOnRelease).
func (c *Cloud) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	inst, err := c.GetInstance(ctx, ref)
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			return nil
		}
		return err
	}
	if got := inst.Tags[vendors.TagCluster]; got != c.cluster {
		return vendors.NewError(vendors.Fatal, "NotOwned", fmt.Errorf(
			"instance %s is not tagged with cluster %q, refusing to delete it", ref, c.cluster))
	}
	cl, err := c.Clients(ctx, ref.Region())
	if err != nil {
		return err
	}
	in := &ecs.DeleteInstanceRequest{InstanceId: new(ref.ID()), Force: new(true)}
	_, err = call(ctx, c, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DeleteInstanceResponseBody, error) {
			return cl.ECS.DeleteInstance(ctx, in)
		})
	if err != nil && !isCode(err, "InvalidInstanceId.NotFound") {
		return err
	}
	return nil
}
