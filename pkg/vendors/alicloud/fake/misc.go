/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl,gocyclo // the fake repeats the SDK shapes and the API's checks
package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

// ---- regions and zones --------------------------------------------------------

// DescribeRegions lists every region of the account (the call is global; the
// fake answers from any region's endpoint).
func (e *ECS) DescribeRegions(ctx context.Context, _ *ecs.DescribeRegionsRequest) (*ecs.DescribeRegionsResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DescribeCalls["DescribeRegions"]++
	if q := f.inject["DescribeRegions"]; len(q) > 0 {
		if q[0].times > 0 {
			q[0].times--
			if q[0].times == 0 {
				f.inject["DescribeRegions"] = q[1:]
			}
		}
		return nil, q[0].err
	}
	out := &ecs.DescribeRegionsResponseBody{RequestId: new("req"), Regions: &ecs.DescribeRegionsResponseBodyRegions{}}
	names := make([]string, 0, len(f.regions))
	for n := range f.regions {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		out.Regions.Region = append(out.Regions.Region, &ecs.DescribeRegionsResponseBodyRegionsRegion{
			RegionId: new(n), Status: new(f.regions[n].status),
			RegionEndpoint: new("ecs." + n + ".aliyuncs.com"),
		})
	}
	return out, nil
}

// DescribeZones lists the zones of the region; a denied region is
// RegionUnauthorized.
func (e *ECS) DescribeZones(ctx context.Context, _ *ecs.DescribeZonesRequest) (*ecs.DescribeZonesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeZones", e.region)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeZonesResponseBody{RequestId: new("req"), Zones: &ecs.DescribeZonesResponseBodyZones{}}
	for _, z := range r.zones {
		out.Zones.Zone = append(out.Zones.Zone, &ecs.DescribeZonesResponseBodyZonesZone{ZoneId: new(z), LocalName: new(z)})
	}
	return out, nil
}

// ---- images -------------------------------------------------------------------

// ImageSpec is an image of the account's catalog.
type ImageSpec struct {
	ID    string
	Name  string
	Owner string // self (default), system, others, marketplace
	Arch  string // x86_64 (default) or arm64
	// Public is the IsPublic flag (a community image).
	Public  bool
	Tags    map[string]string
	SizeGiB int32
	Created time.Time
	// Status defaults to Available.
	Status string
}

type image struct {
	ImageSpec
	status string
}

// AddImage adds an image to the region; an empty ID gets one.
func (f *Cloud) AddImage(regionName string, s ImageSpec) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	if s.ID == "" {
		s.ID = f.nextID("m")
	}
	if s.Owner == "" {
		s.Owner = "self"
	}
	if s.Arch == "" {
		s.Arch = "x86_64"
	}
	if s.SizeGiB == 0 {
		s.SizeGiB = 10
	}
	if s.Created.IsZero() {
		s.Created = f.clock.Now()
	}
	st := s.Status
	if st == "" {
		st = "Available"
	}
	r.images[s.ID] = &image{ImageSpec: s, status: st}
	return s.ID
}

// DescribeImages filters by ImageId (a comma list), ImageName (fuzzy: a
// substring), ImageOwnerAlias, Architecture, IsPublic, Status (default
// Available) and Tag.N (AND), and pages by PageNumber and PageSize.
func (e *ECS) DescribeImages(ctx context.Context, in *ecs.DescribeImagesRequest) (*ecs.DescribeImagesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeImages", e.region)
	if err != nil {
		return nil, err
	}
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	status := "Available"
	if in.Status != nil {
		status = *in.Status
	}
	ids := parseIDs(in.ImageId)
	var all []*image
	for _, im := range r.images {
		switch {
		case !idFilter(ids, im.ID):
		case in.ImageName != nil && !strings.Contains(im.Name, *in.ImageName):
		case in.ImageOwnerAlias != nil && *in.ImageOwnerAlias != im.Owner:
		case in.Architecture != nil && *in.Architecture != im.Arch:
		case in.IsPublic != nil && *in.IsPublic != im.Public:
		case !tagSet(im.Tags).matches(filter):
		case !strings.EqualFold(status, im.status) && status != "":
		default:
			all = append(all, im)
		}
	}
	slices.SortFunc(all, func(a, b *image) int { return strings.Compare(a.ID, b.ID) })
	from, to := page(len(all), dara.Int32Value(in.PageNumber), dara.Int32Value(in.PageSize))
	out := &ecs.DescribeImagesResponseBody{
		RequestId: new("req"), RegionId: new(e.region), TotalCount: new(int32(len(all))),
		Images: &ecs.DescribeImagesResponseBodyImages{},
	}
	for _, im := range all[from:to] {
		si := &ecs.DescribeImagesResponseBodyImagesImage{
			ImageId: new(im.ID), ImageName: new(im.Name), ImageOwnerAlias: new(im.Owner),
			Architecture: new(im.Arch), IsPublic: new(im.Public), Size: new(im.SizeGiB),
			Status: new(im.status), CreationTime: new(im.Created.UTC().Format("2006-01-02T15:04:05Z")),
			Tags: &ecs.DescribeImagesResponseBodyImagesImageTags{},
		}
		for _, k := range sortedKeys(im.Tags) {
			si.Tags.Tag = append(si.Tags.Tag, &ecs.DescribeImagesResponseBodyImagesImageTagsTag{
				TagKey: new(k), TagValue: new(im.Tags[k]),
			})
		}
		out.Images.Image = append(out.Images.Image, si)
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ---- tags ---------------------------------------------------------------------

// tagsOf returns the tags of a taggable resource, nil when there is none.
func (r *regionState) tagsOf(typ, id string) tagSet {
	switch typ {
	case "instance":
		if i, ok := r.instances[id]; ok {
			return i.tags
		}
	case "securitygroup":
		if g, ok := r.groups[id]; ok {
			return g.tags
		}
	case "image":
		if im, ok := r.images[id]; ok {
			if im.Tags == nil {
				im.Tags = map[string]string{}
			}
			return im.Tags
		}
	}
	return nil
}

// TagResources adds tags to instances, security groups and images (deployment
// sets are not a taggable ECS resource type, and CreateDeploymentSet takes no
// tags: the ownership marker of a deployment set lives in its name).
func (e *ECS) TagResources(ctx context.Context, in *ecs.TagResourcesRequest) (*ecs.TagResourcesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "TagResources", e.region)
	if err != nil {
		return nil, err
	}
	typ := dara.StringValue(in.ResourceType)
	if !slices.Contains([]string{"instance", "securitygroup", "image"}, typ) {
		return nil, badRequest("InvalidResourceType.NotSupported", "the resource type %q is not taggable here", typ)
	}
	keys := make([]string, 0, len(in.Tag))
	for _, t := range in.Tag {
		keys = append(keys, dara.StringValue(t.Key))
	}
	if err := checkTagKeys(keys); err != nil {
		return nil, err
	}
	for _, id := range dara.StringSliceValue(in.ResourceId) {
		ts := r.tagsOf(typ, id)
		if ts == nil {
			return nil, badRequest("InvalidResourceId.NotFound", "the resource %q does not exist", id)
		}
		for _, t := range in.Tag {
			ts[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
		}
		if len(ts) > 20 {
			return nil, badRequest("QuotaExceed.Tags", "more than 20 tags on %s", id)
		}
	}
	return &ecs.TagResourcesResponseBody{RequestId: new("req")}, nil
}

// ListTagResources lists the tags of the resources of a type, filtered by ID
// and by Tag.N (AND).
func (e *ECS) ListTagResources(ctx context.Context, in *ecs.ListTagResourcesRequest) (*ecs.ListTagResourcesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "ListTagResources", e.region)
	if err != nil {
		return nil, err
	}
	typ := dara.StringValue(in.ResourceType)
	var ids []string
	switch typ {
	case "instance":
		for id := range r.instances {
			ids = append(ids, id)
		}
	case "securitygroup":
		for id := range r.groups {
			ids = append(ids, id)
		}
	case "image":
		for id := range r.images {
			ids = append(ids, id)
		}
	default:
		return nil, badRequest("InvalidResourceType.NotSupported", "the resource type %q is not taggable here", typ)
	}
	slices.Sort(ids)
	want := dara.StringSliceValue(in.ResourceId)
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	out := &ecs.ListTagResourcesResponseBody{RequestId: new("req"), TagResources: &ecs.ListTagResourcesResponseBodyTagResources{}}
	for _, id := range ids {
		ts := r.tagsOf(typ, id)
		if !idFilter(want, id) || !ts.matches(filter) {
			continue
		}
		for _, k := range sortedKeys(ts) {
			out.TagResources.TagResource = append(out.TagResources.TagResource, &ecs.ListTagResourcesResponseBodyTagResourcesTagResource{
				ResourceId: new(id), ResourceType: new(typ), TagKey: new(k), TagValue: new(ts[k]),
			})
		}
	}
	return out, nil
}

// ---- security groups ------------------------------------------------------------

type rule struct {
	id, direction, protocol, portRange, cidr, sourceGroup, policy string
}

type groupRec struct {
	id, name, vpc, kind string
	tags                tagSet
	rules               []rule
	created             time.Time
	// limit is how many instances the group takes (0 = unlimited).
	limit int
}

// SetGroupLimit limits how many instances the group takes, so that a launch
// into a full group fails with SecurityGroupInstanceLimitExceed.
func (f *Cloud) SetGroupLimit(regionName, id string, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).groups[id].limit = limit
}

// CreateSecurityGroup creates a normal group in a VPC, with its tags (the
// ownership marker is atomic with the creation). A ClientToken is not modelled.
func (e *ECS) CreateSecurityGroup(ctx context.Context, in *ecs.CreateSecurityGroupRequest) (*ecs.CreateSecurityGroupResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "CreateSecurityGroup", e.region)
	if err != nil {
		return nil, err
	}
	vpcID := dara.StringValue(in.VpcId)
	if _, ok := r.vpcs[vpcID]; !ok {
		return nil, badRequest("InvalidVpcId.NotFound", "the VPC %q does not exist", vpcID)
	}
	keys := make([]string, 0, len(in.Tag))
	tags := tagSet{}
	for _, t := range in.Tag {
		keys = append(keys, dara.StringValue(t.Key))
		tags[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	if err := checkTagKeys(keys); err != nil {
		return nil, err
	}
	kind := dara.StringValue(in.SecurityGroupType)
	if kind == "" {
		kind = "normal"
	}
	id := f.nextID("sg")
	r.groups[id] = &groupRec{id: id, name: dara.StringValue(in.SecurityGroupName), vpc: vpcID, kind: kind, tags: tags, created: f.clock.Now()}
	return &ecs.CreateSecurityGroupResponseBody{RequestId: new("req"), SecurityGroupId: new(id)}, nil
}

// DescribeSecurityGroups filters by id(s), name (exact, or a substring with
// FuzzyQuery), VPC, type and Tag.N (AND), and pages by NextToken and
// MaxResults. EcsCount is the number of instances in the group.
func (e *ECS) DescribeSecurityGroups(ctx context.Context, in *ecs.DescribeSecurityGroupsRequest) (*ecs.DescribeSecurityGroupsResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeSecurityGroups", e.region)
	if err != nil {
		return nil, err
	}
	ids := parseIDs(in.SecurityGroupIds)
	if in.SecurityGroupId != nil {
		ids = append(ids, *in.SecurityGroupId)
	}
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	var all []*groupRec
	for _, g := range r.groups {
		switch {
		case !idFilter(ids, g.id):
		case in.VpcId != nil && *in.VpcId != g.vpc:
		case in.SecurityGroupType != nil && *in.SecurityGroupType != g.kind:
		case in.SecurityGroupName != nil && dara.BoolValue(in.FuzzyQuery) && !strings.Contains(g.name, *in.SecurityGroupName):
		case in.SecurityGroupName != nil && !dara.BoolValue(in.FuzzyQuery) && *in.SecurityGroupName != g.name:
		case !g.tags.matches(filter):
		default:
			all = append(all, g)
		}
	}
	slices.SortFunc(all, func(a, b *groupRec) int { return strings.Compare(a.id, b.id) })
	from, to, next, err := tokenPage(len(all), in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeSecurityGroupsResponseBody{
		RequestId: new("req"), RegionId: new(e.region), TotalCount: new(int32(len(all))),
		SecurityGroups: &ecs.DescribeSecurityGroupsResponseBodySecurityGroups{},
	}
	if next != "" {
		out.NextToken = new(next)
	}
	for _, g := range all[from:to] {
		n := r.countIn(func(i *instance) bool { return slices.Contains(i.groups, g.id) })
		sg := &ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroup{
			SecurityGroupId: new(g.id), SecurityGroupName: new(g.name), VpcId: new(g.vpc),
			SecurityGroupType: new(g.kind), EcsCount: new(int32(n)), RuleCount: new(int32(len(g.rules))),
			CreationTime: new(g.created.UTC().Format("2006-01-02T15:04:05Z")),
			Tags:         &ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroupTags{},
		}
		for _, k := range sortedKeys(g.tags) {
			sg.Tags.Tag = append(sg.Tags.Tag, &ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroupTagsTag{
				TagKey: new(k), TagValue: new(g.tags[k]),
			})
		}
		out.SecurityGroups.SecurityGroup = append(out.SecurityGroups.SecurityGroup, sg)
	}
	return out, nil
}

// AuthorizeSecurityGroup adds an inbound rule (outbound is open by default; a
// duplicate rule is silently kept once: the code ECS answers is not known).
func (e *ECS) AuthorizeSecurityGroup(ctx context.Context, in *ecs.AuthorizeSecurityGroupRequest) (*ecs.AuthorizeSecurityGroupResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "AuthorizeSecurityGroup", e.region)
	if err != nil {
		return nil, err
	}
	g, ok := r.groups[dara.StringValue(in.SecurityGroupId)]
	if !ok {
		return nil, badRequest("InvalidSecurityGroupId.NotFound", "the security group does not exist")
	}
	nr := rule{
		direction: "ingress", protocol: dara.StringValue(in.IpProtocol), portRange: dara.StringValue(in.PortRange),
		cidr: dara.StringValue(in.SourceCidrIp), sourceGroup: dara.StringValue(in.SourceGroupId), policy: dara.StringValue(in.Policy),
	}
	if nr.protocol == "" {
		return nil, badRequest("MissingParameter.IpProtocol", "IpProtocol is required")
	}
	if nr.cidr == "" && nr.sourceGroup == "" {
		return nil, badRequest("MissingParameter.Source", "a source CIDR or group is required")
	}
	if nr.policy == "" {
		nr.policy = "accept"
	}
	for _, have := range g.rules {
		nr.id = have.id
		if have == nr {
			return &ecs.AuthorizeSecurityGroupResponseBody{RequestId: new("req")}, nil
		}
	}
	nr.id = f.nextID("sgr")
	g.rules = append(g.rules, nr)
	return &ecs.AuthorizeSecurityGroupResponseBody{RequestId: new("req")}, nil
}

// RevokeSecurityGroup removes the rules with the given IDs or the rule the
// fields describe; a rule that is not there is InvalidPermission.NotFound.
func (e *ECS) RevokeSecurityGroup(ctx context.Context, in *ecs.RevokeSecurityGroupRequest) (*ecs.RevokeSecurityGroupResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "RevokeSecurityGroup", e.region)
	if err != nil {
		return nil, err
	}
	g, ok := r.groups[dara.StringValue(in.SecurityGroupId)]
	if !ok {
		return nil, badRequest("InvalidSecurityGroupId.NotFound", "the security group does not exist")
	}
	ids := dara.StringSliceValue(in.SecurityGroupRuleId)
	before := len(g.rules)
	g.rules = slices.DeleteFunc(g.rules, func(x rule) bool {
		if len(ids) > 0 {
			return slices.Contains(ids, x.id)
		}
		return x.protocol == dara.StringValue(in.IpProtocol) && x.portRange == dara.StringValue(in.PortRange) &&
			x.cidr == dara.StringValue(in.SourceCidrIp) && x.sourceGroup == dara.StringValue(in.SourceGroupId)
	})
	if len(g.rules) == before {
		return nil, badRequest("InvalidPermission.NotFound", "no such rule")
	}
	return &ecs.RevokeSecurityGroupResponseBody{RequestId: new("req")}, nil
}

// DescribeSecurityGroupAttribute returns the rules of a group.
func (e *ECS) DescribeSecurityGroupAttribute(ctx context.Context, in *ecs.DescribeSecurityGroupAttributeRequest) (*ecs.DescribeSecurityGroupAttributeResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeSecurityGroupAttribute", e.region)
	if err != nil {
		return nil, err
	}
	g, ok := r.groups[dara.StringValue(in.SecurityGroupId)]
	if !ok {
		return nil, badRequest("InvalidSecurityGroupId.NotFound", "the security group does not exist")
	}
	out := &ecs.DescribeSecurityGroupAttributeResponseBody{
		RequestId: new("req"), RegionId: new(e.region), SecurityGroupId: new(g.id),
		SecurityGroupName: new(g.name), VpcId: new(g.vpc),
		Permissions: &ecs.DescribeSecurityGroupAttributeResponseBodyPermissions{},
	}
	for _, x := range g.rules {
		p := &ecs.DescribeSecurityGroupAttributeResponseBodyPermissionsPermission{
			SecurityGroupRuleId: new(x.id), Direction: new(x.direction), IpProtocol: new(x.protocol),
			PortRange: new(x.portRange), SourceCidrIp: new(x.cidr), SourceGroupId: new(x.sourceGroup),
			Policy: new(x.policy),
		}
		out.Permissions.Permission = append(out.Permissions.Permission, p)
	}
	return out, nil
}

// DeleteSecurityGroup deletes a group; one that instances use is a
// DependencyViolation.Instance (the exact code is unverified, plan 21 section 7).
func (e *ECS) DeleteSecurityGroup(ctx context.Context, in *ecs.DeleteSecurityGroupRequest) (*ecs.DeleteSecurityGroupResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DeleteSecurityGroup", e.region)
	if err != nil {
		return nil, err
	}
	id := dara.StringValue(in.SecurityGroupId)
	if _, ok := r.groups[id]; !ok {
		return nil, badRequest("InvalidSecurityGroupId.NotFound", "the security group does not exist")
	}
	if r.inUse(func(i *instance) bool { return slices.Contains(i.groups, id) }) {
		return nil, badRequest("DependencyViolation.Instance", "the security group is used by instances")
	}
	delete(r.groups, id)
	return &ecs.DeleteSecurityGroupResponseBody{RequestId: new("req")}, nil
}

// ---- deployment sets --------------------------------------------------------------

type setRec struct {
	id, name, strategy string
	created            time.Time
	limit              int
}

// DefaultDeploymentSetLimit is how many instances a deployment set takes in
// the fake (the 20-instance limits are from vendor-capabilities.md, unverified).
const DefaultDeploymentSetLimit = 20

// SetDeploymentSetLimit changes how many instances a deployment set takes.
func (f *Cloud) SetDeploymentSetLimit(regionName, id string, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).sets[id].limit = limit
}

// CreateDeploymentSet creates a set (Availability or LowLatency). There is no
// Tag parameter: the marker of a managed set is its name.
func (e *ECS) CreateDeploymentSet(ctx context.Context, in *ecs.CreateDeploymentSetRequest) (*ecs.CreateDeploymentSetResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "CreateDeploymentSet", e.region)
	if err != nil {
		return nil, err
	}
	strategy := dara.StringValue(in.Strategy)
	if strategy == "" {
		strategy = "Availability"
	}
	if !slices.Contains([]string{"Availability", "AvailabilityGroup", "LowLatency"}, strategy) {
		return nil, badRequest("InvalidParameter.Strategy", "unknown strategy %q", strategy)
	}
	name := dara.StringValue(in.DeploymentSetName)
	for _, s := range r.sets {
		if s.name == name && name != "" {
			return nil, badRequest("InvalidDeploymentSetName.Duplicated", "a deployment set named %q exists", name)
		}
	}
	id := f.nextID("ds")
	r.sets[id] = &setRec{id: id, name: name, strategy: strategy, created: f.clock.Now(), limit: DefaultDeploymentSetLimit}
	return &ecs.CreateDeploymentSetResponseBody{RequestId: new("req"), DeploymentSetId: new(id)}, nil
}

// DescribeDeploymentSets filters by IDs (a JSON array) and name (exact), pages
// by PageNumber and PageSize.
func (e *ECS) DescribeDeploymentSets(ctx context.Context, in *ecs.DescribeDeploymentSetsRequest) (*ecs.DescribeDeploymentSetsResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeDeploymentSets", e.region)
	if err != nil {
		return nil, err
	}
	ids := parseIDs(in.DeploymentSetIds)
	var all []*setRec
	for _, s := range r.sets {
		switch {
		case !idFilter(ids, s.id):
		case in.DeploymentSetName != nil && *in.DeploymentSetName != s.name:
		default:
			all = append(all, s)
		}
	}
	slices.SortFunc(all, func(a, b *setRec) int { return strings.Compare(a.id, b.id) })
	from, to := page(len(all), dara.Int32Value(in.PageNumber), dara.Int32Value(in.PageSize))
	out := &ecs.DescribeDeploymentSetsResponseBody{
		RequestId: new("req"), RegionId: new(e.region), TotalCount: new(int32(len(all))),
		DeploymentSets: &ecs.DescribeDeploymentSetsResponseBodyDeploymentSets{},
	}
	for _, s := range all[from:to] {
		n := r.countIn(func(i *instance) bool { return i.deploymentSet == s.id })
		out.DeploymentSets.DeploymentSet = append(out.DeploymentSets.DeploymentSet, &ecs.DescribeDeploymentSetsResponseBodyDeploymentSetsDeploymentSet{
			DeploymentSetId: new(s.id), DeploymentSetName: new(s.name), Strategy: new(s.strategy),
			InstanceAmount: new(int32(n)), CreationTime: new(s.created.UTC().Format("2006-01-02T15:04:05Z")),
		})
	}
	return out, nil
}

// DeleteDeploymentSet deletes a set; one with instances is a
// DependencyViolation.Instance (the code is unverified).
func (e *ECS) DeleteDeploymentSet(ctx context.Context, in *ecs.DeleteDeploymentSetRequest) (*ecs.DeleteDeploymentSetResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DeleteDeploymentSet", e.region)
	if err != nil {
		return nil, err
	}
	id := dara.StringValue(in.DeploymentSetId)
	if _, ok := r.sets[id]; !ok {
		return nil, badRequest("InvalidDeploymentSetId.NotFound", "the deployment set does not exist")
	}
	if r.inUse(func(i *instance) bool { return i.deploymentSet == id }) {
		return nil, badRequest("DependencyViolation.Instance", "the deployment set has instances")
	}
	delete(r.sets, id)
	return &ecs.DeleteDeploymentSetResponseBody{RequestId: new("req")}, nil
}
