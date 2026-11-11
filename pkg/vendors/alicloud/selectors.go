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
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the driver that the NodeClass can bring with `use`.
// KindImages is a kind that only accepts `use` (plan 14 A1): ResolveImage serves
// it, not Resolve.
const KindImages = selector.ImagesKind

// Request parameters of the Describe* calls that `filters` may set. Alibaba
// Cloud's List calls take typed top-level parameters, not a filter list: a
// different parameter is ANDed, a second value of one is a second call (ORed).
// `Filter.N` exists only for creation times and a few capability keys, which
// the driver uses for createdAfter and createdBefore and does not offer.
var (
	imageFilterNames = []string{
		"ImageFamily", "OSType", "Usage", "ActionType", "InstanceType", "SnapshotId", fieldResourceGroup, "IsPublic",
		"Usable", "IsSupportCloudinit", "IsSupportIoOptimized", "ShowExpired",
	}
	vswitchFilterNames       = []string{"IsDefault", "EnableIpv6", "RouteTableId", fieldResourceGroup}
	securityGroupFilterNames = []string{"NetworkType", "SecurityGroupType", "ServiceManaged", fieldResourceGroup}
	deploymentSetFilterNames = []string{"Strategy", "Type"}
)

func filterCaps(names []string) []vendors.FilterCap {
	out := make([]vendors.FilterCap, len(names))
	for i, n := range names {
		out[i] = vendors.FilterCap{Name: n, Side: vendors.SideServer}
	}
	return out
}

// Selectors implements vendors.ManagedResources: the `use` selector of the kinds
// a NodeClass can bring (resource-selectors.md, alicloud). ECS and VPC evaluate
// the fields they have a parameter for; the shared matcher re-applies every
// field to the answer, so a fuzzy server-side name is a superset the client
// narrows.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindSubnets: {
			Zonal: true, HasCreated: true, Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: "VSwitchName", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchZone:          {Field: fieldZoneID, Side: vendors.SideServer},
				vendors.MatchNetwork:       {Field: fieldVpcID, Side: vendors.SideServer},
				vendors.MatchCIDR:          {Field: "CidrBlock", Side: vendors.SideClient},
				vendors.MatchWithin:        {Field: "CidrBlock", Side: vendors.SideClient},
				vendors.MatchState:         {Field: fieldStatus, Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  {Field: fieldCreationTime, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: fieldCreationTime, Side: vendors.SideClient},
			},
			Filters:       filterCaps(vswitchFilterNames),
			DefaultSelect: common.SelectAll,
		},
		KindSecurityGroups: {
			HasCreated: true, Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: "SecurityGroupName", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchNetwork:       {Field: fieldVpcID, Side: vendors.SideServer},
				vendors.MatchDescription:   {Field: "Description", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:  {Field: fieldCreationTime, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: fieldCreationTime, Side: vendors.SideClient},
			},
			Filters:       filterCaps(securityGroupFilterNames),
			DefaultSelect: common.SelectAll,
		},
		KindDeploymentSets: {
			HasCreated: true,
			Name:       &vendors.FieldCap{Field: "DeploymentSetName", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchCreatedAfter:  {Field: fieldCreationTime, Side: vendors.SideClient},
				vendors.MatchCreatedBefore: {Field: fieldCreationTime, Side: vendors.SideClient},
			},
			Filters:       filterCaps(deploymentSetFilterNames),
			DefaultSelect: common.SelectExactlyOne,
		},
		KindImages: {
			HasCreated: true, Tags: vendors.TagsMap,
			Name: &vendors.FieldCap{Field: "ImageName", Side: vendors.SideServerClient},
			Match: map[string]vendors.FieldCap{
				vendors.MatchArchitecture:      {Field: "Architecture", Side: vendors.SideServer},
				vendors.MatchOwner:             {Field: "ImageOwnerAlias", Side: vendors.SideServer},
				vendors.MatchState:             {Field: fieldStatus, Side: vendors.SideServer},
				vendors.MatchIncludeDeprecated: {Field: fieldStatus, Side: vendors.SideServer},
				vendors.MatchDescription:       {Field: "Description", Side: vendors.SideClient},
				vendors.MatchCreatedAfter:      {Field: "CreationStartTime", Side: vendors.SideServerClient},
				vendors.MatchCreatedBefore:     {Field: "CreationEndTime", Side: vendors.SideServerClient},
			},
			Filters:       filterCaps(imageFilterNames),
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// VendorArch implements vendors.ArchNamer: the spelling of DescribeImages'
// Architecture, for {vendorArch}.
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case archAMD64:
		return "x86_64", true
	case archARM64:
		return "arm64", true
	}
	return "", false
}

// Names of the fields the declarations repeat.
const (
	fieldStatus        = "Status"
	fieldCreationTime  = "CreationTime"
	fieldVpcID         = "VpcId"
	fieldZoneID        = "ZoneId"
	fieldResourceGroup = "ResourceGroupId"
)

// The portable architecture names and ECS's.
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// Normalised states of the shared matcher.
const (
	stateAvailable = "available"
	statePending   = "pending"
	stateFailed    = "failed"
)

// ---- the parameter cube -----------------------------------------------------------

// maxQueries bounds the calls one selector may make: IDs, owners, networks and
// filter values multiply, because the parameters take one value each.
const maxQueries = 64

// param is one request parameter with the values of the selector (ORed).
type param struct {
	name   string
	values []string
}

// cube expands parameters into one call each: the product of their values, in a
// fixed order. No parameter is one call without any.
func cube(params []param) ([]map[string]string, error) {
	out := []map[string]string{{}}
	for _, p := range params {
		if len(p.values) == 0 {
			continue
		}
		next := make([]map[string]string, 0, len(out)*len(p.values))
		for _, base := range out {
			for _, v := range p.values {
				m := maps.Clone(base)
				m[p.name] = v
				next = append(next, m)
			}
		}
		if len(next) > maxQueries {
			return nil, fmt.Errorf("the selector asks for more than %d calls (its ids, owners and filter values multiply: "+
				"ECS takes one value per parameter); narrow it", maxQueries)
		}
		out = next
	}
	return out, nil
}

// nativeParams turns `filters` into parameters, one per distinct name with the
// values of every filter of that name ORed together... A name given twice is
// ANDed by the API's one-value rule, so the values are intersected here (exact
// for values without wildcards, which validation requires).
func nativeParams(filters []common.ResourceFilter) (params []param, none bool) {
	byName := map[string][]string{}
	var order []string
	for _, f := range filters {
		if cur, ok := byName[f.Name]; ok {
			byName[f.Name] = slices.DeleteFunc(slices.Clone(cur), func(v string) bool { return !slices.Contains(f.Values, v) })
			if len(byName[f.Name]) == 0 {
				return nil, true
			}
			continue
		}
		byName[f.Name] = slices.Clone(f.Values)
		order = append(order, f.Name)
	}
	for _, n := range order {
		params = append(params, param{name: n, values: byName[n]})
	}
	return params, false
}

func boolParam(v string) *bool { return new(v == "true") }

// literalOf is the part of a name pattern the server can search for: the
// longest literal run of a glob (the whole name without wildcards).
func literalOf(pattern string) string {
	if !selector.HasWildcard(pattern) {
		return pattern
	}
	return selector.LongestLiteral(pattern)
}

func minuteTime(t time.Time) string { return t.UTC().Truncate(time.Minute).Format("2006-01-02T15:04Z") }

// ---- images ---------------------------------------------------------------------------

// imageStatuses maps normalised states to ECS's Status values.
func imageStatuses(states []string, deprecated bool) string {
	if len(states) == 0 {
		states = []string{stateAvailable}
	}
	var out []string
	for _, s := range states {
		switch s {
		case stateAvailable:
			out = append(out, "Available")
		case statePending:
			out = append(out, "Creating", "Waiting")
		case stateFailed:
			out = append(out, "UnAvailable", "CreateFailed")
		}
	}
	if deprecated {
		out = append(out, "Deprecated")
	}
	return strings.Join(out, ",")
}

// ownerParams maps an owner to ImageOwnerAlias and ImageOwnerId: self, system
// and marketplace are aliases, an account UID is "others" plus the UID.
func ownerQuery(owner string) (alias string, id *int64, err error) {
	switch owner {
	case "self", "system", "marketplace":
		return owner, nil, nil
	}
	n, perr := strconv.ParseInt(owner, 10, 64)
	if perr != nil {
		return "", nil, fmt.Errorf("the image owner %q is not self, system, marketplace or an account UID", owner)
	}
	return "others", &n, nil
}

// imageRequests builds the DescribeImages calls of a selector: one per owner,
// image ID and value of a native filter, with everything else the call takes.
func imageRequests(region string, use vendors.UseRef, owners []string) ([]*ecs.DescribeImagesRequest, error) {
	native, none := nativeParams(use.Filters)
	if none {
		return nil, nil
	}
	params := slices.Concat([]param{{name: "ImageId", values: use.IDs}, {name: "owner", values: owners}}, native)
	combos, err := cube(params)
	if err != nil {
		return nil, err
	}
	var archs []string
	for _, a := range use.Match.Architecture {
		v, ok := (&Driver{}).VendorArch(a)
		if !ok {
			archs = nil // an unknown one is left to the matcher
			break
		}
		archs = append(archs, v)
	}
	// An architecture list is one more dimension: ECS takes one value.
	archCombos := []string{""}
	if len(archs) > 0 {
		archCombos = archs
	}
	tags := exactTags(use.Tags)
	imgTags := make([]*ecs.DescribeImagesRequestTag, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		imgTags = append(imgTags, &ecs.DescribeImagesRequestTag{Key: new(k), Value: new(tags[k])})
	}
	var filter []*ecs.DescribeImagesRequestFilter
	if t := use.Match.CreatedAfter; t != nil {
		filter = append(filter, &ecs.DescribeImagesRequestFilter{
			Key: new("CreationStartTime"), Value: new(minuteTime(t.Time)),
		})
	}
	if t := use.Match.CreatedBefore; t != nil {
		filter = append(filter, &ecs.DescribeImagesRequestFilter{Key: new("CreationEndTime"), Value: new(minuteTime(t.Time))})
	}
	deprecated := use.Match.IncludeDeprecated != nil && *use.Match.IncludeDeprecated
	var out []*ecs.DescribeImagesRequest
	for _, c := range combos {
		for _, arch := range archCombos {
			in := &ecs.DescribeImagesRequest{
				RegionId: new(region), Status: new(imageStatuses(use.Match.State, deprecated)), PageSize: dara.Int32(100),
				Tag: imgTags, Filter: filter,
			}
			if arch != "" {
				in.Architecture = new(arch)
			}
			if id := c["ImageId"]; id != "" {
				in.ImageId = new(id)
			}
			if o := c["owner"]; o != "" {
				alias, uid, err := ownerQuery(o)
				if err != nil {
					return nil, err
				}
				in.ImageOwnerAlias, in.ImageOwnerId = new(alias), uid
			}
			if use.Name != "" {
				if lit := literalOf(use.Name); lit != "" {
					in.ImageName = new(lit)
				}
			}
			if err := setImageParams(in, c); err != nil {
				return nil, err
			}
			out = append(out, in)
		}
	}
	return out, nil
}

// exactTags are the tags the server can filter on: a "*" (the key exists) is
// the client's, and ECS takes at most 20.
func exactTags(tags map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		if tags[k] != "*" && len(out) < maxTags {
			out[k] = tags[k]
		}
	}
	return out
}

func setImageParams(in *ecs.DescribeImagesRequest, c map[string]string) error {
	for name, v := range c {
		switch name {
		case "ImageId", "owner":
		case "ImageFamily":
			in.ImageFamily = new(v)
		case "OSType":
			in.OSType = new(v)
		case "Usage":
			in.Usage = new(v)
		case "ActionType":
			in.ActionType = new(v)
		case "InstanceType":
			in.InstanceType = new(v)
		case "SnapshotId":
			in.SnapshotId = new(v)
		case fieldResourceGroup:
			in.ResourceGroupId = new(v)
		case "IsPublic":
			in.IsPublic = boolParam(v)
		case "Usable":
			in.Usable = boolParam(v)
		case "IsSupportCloudinit":
			in.IsSupportCloudinit = boolParam(v)
		case "IsSupportIoOptimized":
			in.IsSupportIoOptimized = boolParam(v)
		case "ShowExpired":
			in.ShowExpired = boolParam(v)
		default:
			return fmt.Errorf("DescribeImages has no filter %q the driver sets (one of %s)", name,
				strings.Join(imageFilterNames, ", "))
		}
	}
	return nil
}

// describeImages lists the images of a selector (all pages of every call),
// without duplicates, in the order of their IDs.
func (d *Driver) describeImages(
	ctx context.Context, region string, use vendors.UseRef, owners []string,
) ([]*ecs.DescribeImagesResponseBodyImagesImage, error) {
	reqs, err := imageRequests(region, use, owners)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*ecs.DescribeImagesResponseBodyImagesImage
	for _, in := range reqs {
		for page := int32(1); ; page++ {
			in.PageNumber = new(page)
			res, err := call(ctx, d.cloud, callTimeout, errContext{},
				func(ctx context.Context) (*ecs.DescribeImagesResponseBody, error) {
					return cl.ECS.DescribeImages(ctx, in)
				})
			if err != nil {
				return nil, err
			}
			var n int
			if res.Images != nil {
				for _, im := range res.Images.Image {
					n++
					if id := deref(im.ImageId); !seen[id] {
						seen[id] = true
						out = append(out, im)
					}
				}
			}
			if n == 0 || int(page)*100 >= int(deref(res.TotalCount)) {
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b *ecs.DescribeImagesResponseBodyImagesImage) int {
		return compare(deref(a.ImageId), deref(b.ImageId))
	})
	return out, nil
}

// imageFields reads what the matcher re-applies of an image. The owner is left
// out: it is an alias or an account UID, and only ECS can say whether it
// matches.
func imageFields(im *ecs.DescribeImagesResponseBodyImagesImage) selector.Fields {
	arch := deref(im.Architecture)
	switch arch {
	case "x86_64":
		arch = archAMD64
	}
	// A status this code does not know is kept as ECS spells it: never available.
	state, deprecated := deref(im.Status), false
	switch state {
	case statusAvailable:
		state = stateAvailable
	case "Deprecated":
		state, deprecated = stateAvailable, true
	case "Creating", "Waiting":
		state = statePending
	case "UnAvailable", "CreateFailed":
		state = stateFailed
	}
	attrs := map[string][]string{
		vendors.MatchState: {state}, vendors.MatchDescription: {deref(im.Description)},
	}
	if arch != "" {
		attrs[vendors.MatchArchitecture] = []string{arch}
	}
	if deprecated {
		attrs[vendors.MatchIncludeDeprecated] = []string{"true"}
	}
	tags := map[string]string{}
	if im.Tags != nil {
		for _, t := range im.Tags.Tag {
			tags[deref(t.TagKey)] = deref(t.TagValue)
		}
	}
	return selector.Fields{
		ID: deref(im.ImageId), Name: deref(im.ImageName), Tags: tags, Attrs: attrs,
		Created: parseTime(deref(im.CreationTime)),
	}
}

// ---- vSwitches --------------------------------------------------------------------------

const (
	vswitchPage = 50 // DescribeVSwitches takes at most 50 per page
	groupPage   = 100
	setPage     = 100
)

type vswitchInfo = vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitch

func vpcTags(tags map[string]string) []*vpc.DescribeVSwitchesRequestTag {
	ex := exactTags(tags)
	out := make([]*vpc.DescribeVSwitchesRequestTag, 0, len(ex))
	for _, k := range slices.Sorted(maps.Keys(ex)) {
		out = append(out, &vpc.DescribeVSwitchesRequestTag{Key: new(k), Value: new(ex[k])})
	}
	return out
}

// vswitchRequests builds the DescribeVSwitches calls of a selector.
func vswitchRequests(region string, ref vendors.UseRef) ([]*vpc.DescribeVSwitchesRequest, error) {
	native, none := nativeParams(ref.Filters)
	if none {
		return nil, nil
	}
	params := slices.Concat([]param{
		{name: "VSwitchId", values: ref.IDs}, {name: fieldVpcID, values: ref.Match.Network},
		{name: "ZoneId", values: ref.Match.Zone},
	}, native)
	combos, err := cube(params)
	if err != nil {
		return nil, err
	}
	var out []*vpc.DescribeVSwitchesRequest
	for _, c := range combos {
		in := &vpc.DescribeVSwitchesRequest{
			RegionId: new(region), PageSize: dara.Int32(vswitchPage), Tag: vpcTags(ref.Tags),
		}
		if ref.Name != "" && !selector.HasWildcard(ref.Name) {
			in.VSwitchName = new(ref.Name)
		}
		for name, v := range c {
			switch name {
			case "VSwitchId":
				in.VSwitchId = new(v)
			case fieldVpcID:
				in.VpcId = new(v)
			case "ZoneId":
				in.ZoneId = new(v)
			case "IsDefault":
				in.IsDefault = boolParam(v)
			case "EnableIpv6":
				in.EnableIpv6 = boolParam(v)
			case "RouteTableId":
				in.RouteTableId = new(v)
			case fieldResourceGroup:
				in.ResourceGroupId = new(v)
			default:
				return nil, fmt.Errorf("DescribeVSwitches has no filter %q the driver sets (one of %s)", name,
					strings.Join(vswitchFilterNames, ", "))
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// describeVSwitches lists the vSwitches of a selector, all pages, without
// duplicates.
func (d *Driver) describeVSwitches(ctx context.Context, region string, ref vendors.UseRef) ([]*vswitchInfo, error) {
	reqs, err := vswitchRequests(region, ref)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	var out []*vswitchInfo
	seen := map[string]bool{}
	for _, in := range reqs {
		got, err := d.pageVSwitches(ctx, region, in)
		if err != nil {
			return nil, err
		}
		for _, s := range got {
			if id := deref(s.VSwitchId); !seen[id] {
				seen[id] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// pageVSwitches reads every page of one DescribeVSwitches call.
func (d *Driver) pageVSwitches(
	ctx context.Context, region string, in *vpc.DescribeVSwitchesRequest,
) ([]*vswitchInfo, error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []*vswitchInfo
	for page := int32(1); ; page++ {
		in.PageNumber = new(page)
		res, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*vpc.DescribeVSwitchesResponseBody, error) {
				return cl.VPC.DescribeVSwitches(ctx, in)
			})
		if err != nil {
			return nil, err
		}
		n := 0
		if res.VSwitches != nil {
			n = len(res.VSwitches.VSwitch)
			out = append(out, res.VSwitches.VSwitch...)
		}
		if n == 0 || int(page)*vswitchPage >= int(deref(res.TotalCount)) {
			return out, nil
		}
	}
}

func vswitchFields(s *vswitchInfo) selector.Fields {
	state := deref(s.Status)
	switch state {
	case statusAvailable:
		state = stateAvailable
	case "Pending":
		state = statePending
	}
	attrs := map[string][]string{
		vendors.MatchZone: {deref(s.ZoneId)}, vendors.MatchNetwork: {deref(s.VpcId)},
		vendors.MatchCIDR: {deref(s.CidrBlock)}, vendors.MatchState: {state},
	}
	if v6 := deref(s.Ipv6CidrBlock); v6 != "" {
		attrs[vendors.MatchCIDR] = append(attrs[vendors.MatchCIDR], v6)
	}
	tags := map[string]string{}
	if s.Tags != nil {
		for _, t := range s.Tags.Tag {
			tags[deref(t.Key)] = deref(t.Value)
		}
	}
	return selector.Fields{
		ID: deref(s.VSwitchId), Name: deref(s.VSwitchName), Tags: tags, Attrs: attrs,
		Created: parseTime(deref(s.CreationTime)),
	}
}

// ---- security groups ---------------------------------------------------------------------

type groupInfo = ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroup

func groupRequests(region string, ref vendors.UseRef) ([]*ecs.DescribeSecurityGroupsRequest, error) {
	native, none := nativeParams(ref.Filters)
	if none {
		return nil, nil
	}
	params := slices.Concat([]param{{name: fieldVpcID, values: ref.Match.Network}}, native)
	combos, err := cube(params)
	if err != nil {
		return nil, err
	}
	ex := exactTags(ref.Tags)
	tags := make([]*ecs.DescribeSecurityGroupsRequestTag, 0, len(ex))
	for _, k := range slices.Sorted(maps.Keys(ex)) {
		tags = append(tags, &ecs.DescribeSecurityGroupsRequestTag{Key: new(k), Value: new(ex[k])})
	}
	var out []*ecs.DescribeSecurityGroupsRequest
	for _, c := range combos {
		in := &ecs.DescribeSecurityGroupsRequest{RegionId: new(region), MaxResults: dara.Int32(groupPage), Tag: tags}
		if len(ref.IDs) > 0 {
			b, _ := json.Marshal(ref.IDs)
			in.SecurityGroupIds = new(string(b))
		}
		if lit := literalOf(ref.Name); ref.Name != "" && lit != "" {
			// The exactness of the name parameter is unverified: a fuzzy query is a
			// superset the matcher narrows.
			in.SecurityGroupName, in.FuzzyQuery = new(lit), new(true)
		}
		for name, v := range c {
			switch name {
			case fieldVpcID:
				in.VpcId = new(v)
			case "NetworkType":
				in.NetworkType = new(v)
			case "SecurityGroupType":
				in.SecurityGroupType = new(v)
			case "ServiceManaged":
				in.ServiceManaged = boolParam(v)
			case fieldResourceGroup:
				in.ResourceGroupId = new(v)
			default:
				return nil, fmt.Errorf("DescribeSecurityGroups has no filter %q the driver sets (one of %s)", name,
					strings.Join(securityGroupFilterNames, ", "))
			}
		}
		out = append(out, in)
	}
	return out, nil
}

func (d *Driver) describeGroups(ctx context.Context, region string, ref vendors.UseRef) ([]*groupInfo, error) {
	reqs, err := groupRequests(region, ref)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	var out []*groupInfo
	seen := map[string]bool{}
	for _, in := range reqs {
		got, err := d.pageGroups(ctx, region, in)
		if err != nil {
			return nil, err
		}
		for _, g := range got {
			if id := deref(g.SecurityGroupId); !seen[id] {
				seen[id] = true
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// pageGroups follows NextToken through one DescribeSecurityGroups call.
func (d *Driver) pageGroups(
	ctx context.Context, region string, in *ecs.DescribeSecurityGroupsRequest,
) ([]*groupInfo, error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []*groupInfo
	for {
		res, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.DescribeSecurityGroupsResponseBody, error) {
				return cl.ECS.DescribeSecurityGroups(ctx, in)
			})
		if err != nil {
			return nil, err
		}
		if res.SecurityGroups != nil {
			out = append(out, res.SecurityGroups.SecurityGroup...)
		}
		if deref(res.NextToken) == "" {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

func groupFields(g *groupInfo) selector.Fields {
	tags := map[string]string{}
	if g.Tags != nil {
		for _, t := range g.Tags.Tag {
			tags[deref(t.TagKey)] = deref(t.TagValue)
		}
	}
	return selector.Fields{
		ID: deref(g.SecurityGroupId), Name: deref(g.SecurityGroupName), Tags: tags, Created: parseTime(deref(g.CreationTime)),
		Attrs: map[string][]string{
			vendors.MatchNetwork: {deref(g.VpcId)}, vendors.MatchDescription: {deref(g.Description)},
		},
	}
}

// ---- deployment sets ---------------------------------------------------------------------

type setInfo = ecs.DescribeDeploymentSetsResponseBodyDeploymentSetsDeploymentSet

func setRequests(region string, ref vendors.UseRef) ([]*ecs.DescribeDeploymentSetsRequest, error) {
	native, none := nativeParams(ref.Filters)
	if none {
		return nil, nil
	}
	combos, err := cube(native)
	if err != nil {
		return nil, err
	}
	var out []*ecs.DescribeDeploymentSetsRequest
	for _, c := range combos {
		in := &ecs.DescribeDeploymentSetsRequest{RegionId: new(region), PageSize: dara.Int32(setPage)}
		if len(ref.IDs) > 0 {
			b, _ := json.Marshal(ref.IDs)
			in.DeploymentSetIds = new(string(b))
		}
		if ref.Name != "" && !selector.HasWildcard(ref.Name) {
			in.DeploymentSetName = new(ref.Name)
		}
		for name, v := range c {
			switch name {
			case "Strategy":
				in.Strategy = new(v)
			case "Type":
				in.Type = new(v)
			default:
				return nil, fmt.Errorf("DescribeDeploymentSets has no filter %q the driver sets (one of %s)", name,
					strings.Join(deploymentSetFilterNames, ", "))
			}
		}
		out = append(out, in)
	}
	return out, nil
}

func (d *Driver) describeSets(ctx context.Context, region string, ref vendors.UseRef) ([]*setInfo, error) {
	reqs, err := setRequests(region, ref)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []*setInfo
	seen := map[string]bool{}
	for _, in := range reqs {
		for page := int32(1); ; page++ {
			in.PageNumber = new(page)
			res, err := call(ctx, d.cloud, callTimeout, errContext{},
				func(ctx context.Context) (*ecs.DescribeDeploymentSetsResponseBody, error) {
					return cl.ECS.DescribeDeploymentSets(ctx, in)
				})
			if err != nil {
				return nil, err
			}
			n := 0
			if res.DeploymentSets != nil {
				for _, s := range res.DeploymentSets.DeploymentSet {
					n++
					if id := deref(s.DeploymentSetId); !seen[id] {
						seen[id] = true
						out = append(out, s)
					}
				}
			}
			if n == 0 || int(page)*setPage >= int(deref(res.TotalCount)) {
				break
			}
		}
	}
	return out, nil
}

func setFields(s *setInfo) selector.Fields {
	return selector.Fields{
		ID: deref(s.DeploymentSetId), Name: deref(s.DeploymentSetName), Created: parseTime(deref(s.CreationTime)),
	}
}
