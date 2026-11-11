/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl,gocyclo // the fake repeats the SDK shapes and the API's checks
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

// ECS is the ECS endpoint of one region.
type ECS struct {
	f      *Cloud
	region string
}

// ECS returns the client of the region.
func (f *Cloud) ECS(region string) *ECS { return &ECS{f: f, region: region} }

// Statuses of an instance.
const (
	StatusPending = "Pending"
	StatusRunning = "Running"
	StatusStopped = "Stopped"
)

// userDataMax is the size of the user data before its Base64 encoding.
const userDataMax = 32 * 1024

// reservedAddresses is how many addresses of every vSwitch ECS reserves (plan
// 21 section 2 says 4; unverified).
const reservedAddresses = 4

type instance struct {
	id, name, host, typ, zone, image, vswitch, vpc, deploymentSet string
	spot                                                          bool
	groups                                                        []string
	tags                                                          tagSet
	created                                                       time.Time
	stopped                                                       bool
	vanishAt                                                      time.Time
	bandwidth                                                     int32
	privateIP, publicIP                                           string
	userData                                                      string
	vcpus                                                         int64
	tokenKey                                                      string
}

func (i *instance) status(now time.Time, pendingFor time.Duration) string {
	switch {
	case i.stopped:
		return StatusStopped
	case now.Sub(i.created) < pendingFor:
		return StatusPending
	default:
		return StatusRunning
	}
}

// reap releases the instances that ECS released for lack of stock.
func (f *Cloud) reap(r *regionState) {
	now := f.clock.Now()
	for id, i := range r.instances {
		if !i.vanishAt.IsZero() && !now.Before(i.vanishAt) {
			f.release(r, id)
		}
	}
}

// release removes an instance and gives its address back.
func (f *Cloud) release(r *regionState, id string) {
	if i, ok := r.instances[id]; ok {
		if vs, ok := r.vswitches[i.vswitch]; ok {
			vs.used = slices.DeleteFunc(vs.used, func(a netip.Addr) bool { return a.String() == i.privateIP })
		}
	}
	delete(r.instances, id)
}

// Stop stops an instance (the user, the OS or ECS did): it exists and its disks
// cost, but it is not running.
func (f *Cloud) Stop(regionName, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.mustRegion(regionName).instances[id]; ok {
		i.stopped = true
	}
}

// Release removes an instance as if ECS had released it.
func (f *Cloud) Release(regionName, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release(f.mustRegion(regionName), id)
}

// InstanceIDs lists the instance IDs of the region, sorted.
func (f *Cloud) InstanceIDs(regionName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	f.reap(r)
	out := make([]string, 0, len(r.instances))
	for id := range r.instances {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// AddForeignInstance adds a running instance that is not SCarpenter's (no tags)
// of the given type, for the account-wide usage of the quota; it returns the ID.
func (f *Cloud) AddForeignInstance(regionName, zone, typ string, spot bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	id := f.nextID("i")
	r.instances[id] = &instance{
		id: id, name: "foreign", typ: typ, zone: zone, spot: spot, created: f.clock.Now().Add(-time.Hour),
		tags: tagSet{}, vcpus: int64(f.types[typ].VCPUs),
	}
	return id
}

// usedVCPUs sums the vCPUs of the region's instances of the capacity type;
// stopped instances count (whether they do is unverified, plan 21 section 6).
func (r *regionState) usedVCPUs(spot bool) int64 {
	var n int64
	for _, i := range r.instances {
		if i.spot == spot {
			n += i.vcpus
		}
	}
	return n
}

func (r *regionState) inUse(pred func(*instance) bool) bool {
	for _, i := range r.instances {
		if pred(i) {
			return true
		}
	}
	return false
}

// runParams is what a replay of a ClientToken must repeat: every request field
// except the token itself (ECS answers other parameters with
// IdempotentParameterMismatch).
func runParams(in *ecs.RunInstancesRequest) string {
	c := *in
	c.ClientToken = nil
	return fmt.Sprintf("%x", sha256.Sum256([]byte(c.String())))
}

// RunInstances implements the launch of plan 21 section 3: validation, the
// offering and its stock, the vCPU quota, the vSwitch's free addresses, the
// group's size, the deployment set's size, ClientToken semantics. It creates
// the instance Pending.
func (e *ECS) RunInstances(ctx context.Context, in *ecs.RunInstancesRequest) (*ecs.RunInstancesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "RunInstances", e.region)
	if err != nil {
		return nil, err
	}
	tokKey := ""
	if tok := dara.StringValue(in.ClientToken); tok != "" {
		if len(tok) > 64 {
			return nil, badRequest("InvalidClientToken.ValueNotSupported", "the ClientToken exceeds 64 characters")
		}
		tokKey = tok
		params := runParams(in)
		if rec, ok := r.tokens[tok]; ok {
			if rec.params != params {
				return nil, badRequest("IdempotentParameterMismatch", "the ClientToken was used with other parameters")
			}
			return runResponse(rec.ids), nil
		}
	}
	inst, err := f.launch(r, in)
	if err != nil {
		if f.ConsumeTokenOnFailure && tokKey != "" {
			r.tokens[tokKey] = &token{params: runParams(in)}
		}
		return nil, err
	}
	if tokKey != "" {
		r.tokens[tokKey] = &token{params: runParams(in), ids: []string{inst.id}}
		inst.tokenKey = tokKey
	}
	return runResponse([]string{inst.id}), nil
}

func runResponse(ids []string) *ecs.RunInstancesResponseBody {
	out := &ecs.RunInstancesResponseBody{RequestId: new("req"), InstanceIdSets: &ecs.RunInstancesResponseBodyInstanceIdSets{}}
	for _, id := range ids {
		out.InstanceIdSets.InstanceIdSet = append(out.InstanceIdSets.InstanceIdSet, new(id))
	}
	return out
}

func (f *Cloud) launch(r *regionState, in *ecs.RunInstancesRequest) (*instance, error) {
	if dara.Int32Value(in.Amount) != 1 || dara.Int32Value(in.MinAmount) != 1 {
		return nil, badRequest("InvalidParameter", "the fake takes Amount=1 and MinAmount=1")
	}
	if dara.StringValue(in.InstanceChargeType) != "PostPaid" {
		return nil, badRequest("InvalidChargeType.ValueNotSupported", "only PostPaid is supported")
	}
	img, ok := r.images[dara.StringValue(in.ImageId)]
	if !ok || img.status != "Available" {
		return nil, badRequest("InvalidImageId.NotFound", "the image %q does not exist", dara.StringValue(in.ImageId))
	}
	vs, ok := r.vswitches[dara.StringValue(in.VSwitchId)]
	if !ok {
		return nil, badRequest("InvalidVSwitchId.NotFound", "the vSwitch %q does not exist", dara.StringValue(in.VSwitchId))
	}
	zone := dara.StringValue(in.ZoneId)
	if zone != "" && zone != vs.zone {
		return nil, badRequest("InvalidParameter.Mismatch", "the vSwitch is in zone %s, not %s", vs.zone, zone)
	}
	zone = vs.zone
	groups := dara.StringSliceValue(in.SecurityGroupIds)
	if len(groups) == 0 {
		return nil, badRequest("MissingParameter.SecurityGroupId", "a security group is required")
	}
	for _, g := range groups {
		sg, ok := r.groups[g]
		if !ok {
			return nil, badRequest("InvalidSecurityGroupId.NotFound", "the security group %q does not exist", g)
		}
		if sg.vpc != vs.vpc {
			return nil, badRequest("InvalidSecurityGroup.VpcMismatch", "the security group is in another VPC than the vSwitch")
		}
	}
	if ud := dara.StringValue(in.UserData); ud != "" {
		raw, err := base64.StdEncoding.DecodeString(ud)
		if err != nil {
			return nil, badRequest("InvalidUserData.NotBase64", "the user data is not Base64")
		}
		if len(raw) > userDataMax {
			return nil, badRequest("InvalidUserData.SizeExceeded", "the user data exceeds %d bytes", userDataMax)
		}
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
	typ := dara.StringValue(in.InstanceType)
	spec, ok := f.types[typ]
	if !ok {
		return nil, badRequest("InvalidInstanceType.ValueNotSupported", "the instance type %q does not exist", typ)
	}
	spot := dara.StringValue(in.SpotStrategy) != "" && dara.StringValue(in.SpotStrategy) != "NoSpot"
	capacity := OnDemand
	if spot {
		capacity = Spot
		if r.noSpot {
			return nil, badRequest("InvalidAccount.NotSupportSpot", "the account cannot buy spot instances")
		}
	}
	if err := checkDisks(in); err != nil {
		return nil, err
	}
	status, offered := r.offers[offerKey{zone, typ, capacity}]
	if !offered {
		return nil, badRequest("InvalidInstanceType.ZoneNotSupported", "%s is not sold in %s", typ, zone)
	}
	if status != StatusAvailable {
		return nil, badRequest("OperationDenied.NoStock", "%s is sold out in %s", typ, zone)
	}
	limit := r.postpaid
	if spot {
		limit = r.spot
	}
	if r.usedVCPUs(spot)+int64(spec.VCPUs) > limit {
		return nil, badRequest("QuotaExceed.ElasticQuota", "the vCPU quota of the region is exhausted")
	}
	if vs.free() <= 0 {
		return nil, badRequest("QuotaExceeded.PrivateIpAddress", "the vSwitch has no free address")
	}
	for _, g := range groups {
		sg := r.groups[g]
		if sg.limit > 0 && r.countIn(func(i *instance) bool { return slices.Contains(i.groups, g) }) >= sg.limit {
			return nil, badRequest("SecurityGroupInstanceLimitExceed", "the security group is full")
		}
	}
	ds := dara.StringValue(in.DeploymentSetId)
	if ds != "" {
		set, ok := r.sets[ds]
		if !ok {
			return nil, badRequest("InvalidDeploymentSetId.NotFound", "the deployment set %q does not exist", ds)
		}
		if set.limit > 0 && r.countIn(func(i *instance) bool { return i.deploymentSet == ds }) >= set.limit {
			return nil, badRequest("QuotaExceed.DeploymentSetInstanceQuotaFull", "the deployment set is full")
		}
	}
	now := f.clock.Now()
	id := f.nextID("i")
	i := &instance{
		id: id, name: dara.StringValue(in.InstanceName), host: dara.StringValue(in.HostName), typ: typ, zone: zone,
		image: img.ID, vswitch: vs.id, vpc: vs.vpc, deploymentSet: ds, spot: spot, groups: groups, tags: tags,
		created: now, bandwidth: dara.Int32Value(in.InternetMaxBandwidthOut), vcpus: int64(spec.VCPUs),
		userData: dara.StringValue(in.UserData),
	}
	i.privateIP = vs.allocate()
	if i.bandwidth > 0 {
		i.publicIP = fmt.Sprintf("203.0.113.%d", f.seq%250+1)
	}
	if d, ok := r.vanishAfter[offerKey{zone, typ, capacity}]; ok {
		i.vanishAt = now.Add(d)
	}
	r.instances[id] = i
	return i, nil
}

// checkDisks validates the disk parameters the driver sets.
func checkDisks(in *ecs.RunInstancesRequest) error {
	if sd := in.SystemDisk; sd != nil {
		size, err := strconv.Atoi(dara.StringValue(sd.Size))
		if dara.StringValue(sd.Size) != "" && (err != nil || size < 20 || size > 2048) {
			return badRequest("InvalidSystemDiskSize.ValueNotSupported", "the system disk size %q is outside 20 to 2048 GiB",
				dara.StringValue(sd.Size))
		}
		switch c := dara.StringValue(sd.Category); c {
		case "", "cloud_essd", "cloud_auto", "cloud_essd_entry", "cloud_efficiency", "cloud_ssd":
		default:
			return badRequest("InvalidDiskCategory.NotSupported", "the disk category %q is not supported", c)
		}
	}
	for _, d := range in.DataDisk {
		switch c := dara.StringValue(d.Category); c {
		case "", "cloud_essd", "cloud_auto", "cloud_essd_entry", "cloud_efficiency", "cloud_ssd":
		default:
			return badRequest("InvalidDiskCategory.NotSupported", "the disk category %q is not supported", c)
		}
	}
	return nil
}

func (r *regionState) countIn(pred func(*instance) bool) int {
	n := 0
	for _, i := range r.instances {
		if pred(i) {
			n++
		}
	}
	return n
}

// DescribeInstances implements the list: InstanceIds (a JSON array), Tag.N with
// AND semantics (unverified in the plan; AND here), Status, VpcId, VSwitchId,
// SecurityGroupId, ZoneId, InstanceName, DeploymentSetId by the Filter list is
// not modelled; paging by NextToken and MaxResults (default 10, maximum 100).
// Released instances are not returned.
func (e *ECS) DescribeInstances(ctx context.Context, in *ecs.DescribeInstancesRequest) (*ecs.DescribeInstancesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeInstances", e.region)
	if err != nil {
		return nil, err
	}
	ids := parseIDs(in.InstanceIds)
	filter := map[string]string{}
	for _, t := range in.Tag {
		filter[dara.StringValue(t.Key)] = dara.StringValue(t.Value)
	}
	now := f.clock.Now()
	var all []*instance
	for _, i := range r.instances {
		switch {
		case !idFilter(ids, i.id):
		case !i.tags.matches(filter):
		case in.Status != nil && *in.Status != i.status(now, f.PendingFor):
		case in.VpcId != nil && *in.VpcId != i.vpc:
		case in.VSwitchId != nil && *in.VSwitchId != i.vswitch:
		case in.ZoneId != nil && *in.ZoneId != i.zone:
		case in.SecurityGroupId != nil && !slices.Contains(i.groups, *in.SecurityGroupId):
		case in.InstanceName != nil && *in.InstanceName != i.name:
		default:
			all = append(all, i)
		}
	}
	slices.SortFunc(all, func(a, b *instance) int { return strings.Compare(a.id, b.id) })
	from, to, next, err := tokenPage(len(all), in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeInstancesResponseBody{
		RequestId: new("req"), TotalCount: new(int32(len(all))),
		Instances: &ecs.DescribeInstancesResponseBodyInstances{},
	}
	if next != "" {
		out.NextToken = new(next)
	}
	for _, i := range all[from:to] {
		out.Instances.Instance = append(out.Instances.Instance, i.toSDK(e.region, now, f.PendingFor))
	}
	return out, nil
}

func (i *instance) toSDK(region string, now time.Time, pendingFor time.Duration) *ecs.DescribeInstancesResponseBodyInstancesInstance {
	out := &ecs.DescribeInstancesResponseBodyInstancesInstance{
		InstanceId: new(i.id), InstanceName: new(i.name), HostName: new(i.host),
		InstanceType: new(i.typ), ZoneId: new(i.zone), RegionId: new(region),
		ImageId: new(i.image), Status: new(i.status(now, pendingFor)),
		CreationTime: new(i.created.UTC().Format("2006-01-02T15:04Z")), DeploymentSetId: new(i.deploymentSet),
		InstanceChargeType: new("PostPaid"), SpotStrategy: new("NoSpot"),
		InternetMaxBandwidthOut: new(i.bandwidth),
		VpcAttributes: &ecs.DescribeInstancesResponseBodyInstancesInstanceVpcAttributes{
			VSwitchId: new(i.vswitch), VpcId: new(i.vpc),
			PrivateIpAddress: &ecs.DescribeInstancesResponseBodyInstancesInstanceVpcAttributesPrivateIpAddress{
				IpAddress: dara.StringSlice([]string{i.privateIP}),
			},
		},
		SecurityGroupIds: &ecs.DescribeInstancesResponseBodyInstancesInstanceSecurityGroupIds{SecurityGroupId: dara.StringSlice(i.groups)},
		PublicIpAddress:  &ecs.DescribeInstancesResponseBodyInstancesInstancePublicIpAddress{},
		Tags:             &ecs.DescribeInstancesResponseBodyInstancesInstanceTags{},
		NetworkInterfaces: &ecs.DescribeInstancesResponseBodyInstancesInstanceNetworkInterfaces{
			NetworkInterface: []*ecs.DescribeInstancesResponseBodyInstancesInstanceNetworkInterfacesNetworkInterface{
				{NetworkInterfaceId: new("eni-" + i.id), PrimaryIpAddress: new(i.privateIP), Type: new("Primary")},
			},
		},
	}
	if i.spot {
		out.SpotStrategy = new("SpotAsPriceGo")
	}
	if i.publicIP != "" {
		out.PublicIpAddress.IpAddress = dara.StringSlice([]string{i.publicIP})
	}
	keys := make([]string, 0, len(i.tags))
	for k := range i.tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out.Tags.Tag = append(out.Tags.Tag, &ecs.DescribeInstancesResponseBodyInstancesInstanceTagsTag{
			TagKey: new(k), TagValue: new(i.tags[k]),
		})
	}
	return out
}

// DeleteInstance releases an instance. Without Force only a Stopped one can be
// released; with Force a Running one too; a Pending (or Starting) one is
// IncorrectInstanceStatus either way (unverified, plan 21 section 6), which the
// driver retries.
func (e *ECS) DeleteInstance(ctx context.Context, in *ecs.DeleteInstanceRequest) (*ecs.DeleteInstanceResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DeleteInstance", e.region)
	if err != nil {
		return nil, err
	}
	id := dara.StringValue(in.InstanceId)
	i, ok := r.instances[id]
	if !ok {
		return nil, badRequest("InvalidInstanceId.NotFound", "the instance %q does not exist", id)
	}
	switch st := i.status(f.clock.Now(), f.PendingFor); {
	case st == StatusStopped:
	case st == StatusRunning && dara.BoolValue(in.Force):
	default:
		return nil, badRequest("IncorrectInstanceStatus", "the instance is %s", st)
	}
	f.release(r, id)
	return &ecs.DeleteInstanceResponseBody{RequestId: new("req")}, nil
}

// DescribeInstanceTypes lists the catalog: InstanceTypes (1 to 10 names, an
// unknown name is just not returned), NextToken and MaxResults paging.
func (e *ECS) DescribeInstanceTypes(ctx context.Context, in *ecs.DescribeInstanceTypesRequest) (*ecs.DescribeInstanceTypesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.enter(ctx, "DescribeInstanceTypes", e.region); err != nil {
		return nil, err
	}
	names := dara.StringSliceValue(in.InstanceTypes)
	if len(names) > 10 {
		return nil, badRequest("InvalidParameter.InstanceTypes", "at most 10 instance types, got %d", len(names))
	}
	var all []TypeSpec
	for _, t := range f.types {
		if len(names) == 0 || slices.Contains(names, t.Name) {
			all = append(all, t)
		}
	}
	slices.SortFunc(all, func(a, b TypeSpec) int { return strings.Compare(a.Name, b.Name) })
	size := int32(DefaultPageSize)
	if in.MaxResults != nil && *in.MaxResults > 0 {
		size = int32(*in.MaxResults)
	}
	from, to, next, err := tokenPage(len(all), in.NextToken, &size)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeInstanceTypesResponseBody{
		RequestId: new("req"), InstanceTypes: &ecs.DescribeInstanceTypesResponseBodyInstanceTypes{},
	}
	if next != "" {
		out.NextToken = new(next)
	}
	for _, t := range all[from:to] {
		arch, nvme := "X86", "unsupported"
		if t.Arch == "arm64" {
			arch = "ARM"
		}
		if t.NVMe {
			nvme = "required"
		}
		out.InstanceTypes.InstanceType = append(out.InstanceTypes.InstanceType,
			&ecs.DescribeInstanceTypesResponseBodyInstanceTypesInstanceType{
				InstanceTypeId: new(t.Name), CpuCoreCount: new(t.VCPUs), MemorySize: new(t.MemoryGiB),
				CpuArchitecture: new(arch), NvmeSupport: new(nvme), DiskQuantity: new(t.Disks),
				LocalStorageAmount: new(t.LocalDisk),
			})
	}
	return out, nil
}

// DescribeAvailableResource reports, per zone, the types of the offerings with
// their stock Status (Available or SoldOut) and StatusCategory (WithStock,
// WithoutStock); the spot strategy selects the spot offerings. Only
// DestinationResource=InstanceType is modelled.
func (e *ECS) DescribeAvailableResource(ctx context.Context, in *ecs.DescribeAvailableResourceRequest) (*ecs.DescribeAvailableResourceResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeAvailableResource", e.region)
	if err != nil {
		return nil, err
	}
	if dest := dara.StringValue(in.DestinationResource); dest != "InstanceType" {
		return nil, badRequest("InvalidParameter.DestinationResource", "the fake models DestinationResource=InstanceType only, got %q", dest)
	}
	capacity := OnDemand
	if s := dara.StringValue(in.SpotStrategy); s != "" && s != "NoSpot" {
		capacity = Spot
	}
	out := &ecs.DescribeAvailableResourceResponseBody{
		RequestId: new("req"), AvailableZones: &ecs.DescribeAvailableResourceResponseBodyAvailableZones{},
	}
	for _, z := range r.zones {
		if zid := dara.StringValue(in.ZoneId); zid != "" && zid != z {
			continue
		}
		var res []*ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResourceSupportedResourcesSupportedResource
		names := make([]string, 0)
		for k := range r.offers {
			if k.zone == z && k.capacity == capacity {
				names = append(names, k.typ)
			}
		}
		slices.Sort(names)
		for _, n := range names {
			st := r.offers[offerKey{z, n, capacity}]
			cat := "WithStock"
			if st != StatusAvailable {
				cat = "WithoutStock"
			}
			res = append(res, &ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResourceSupportedResourcesSupportedResource{
				Value: new(n), Status: new(st), StatusCategory: new(cat),
			})
		}
		out.AvailableZones.AvailableZone = append(out.AvailableZones.AvailableZone,
			&ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZone{
				RegionId: new(e.region), ZoneId: new(z), Status: new(StatusAvailable),
				StatusCategory: new("WithStock"),
				AvailableResources: &ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResources{
					AvailableResource: []*ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResource{{
						Type: new("InstanceType"),
						SupportedResources: &ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResourceSupportedResources{
							SupportedResource: res,
						},
					}},
				},
			})
	}
	return out, nil
}

// DescribeAccountAttributes returns the vCPU limits and usage of the region
// (max-/used-postpaid-instance-vcpu-count, max-/used-spot-instance-vcpu-count);
// the usage covers every instance of the account in the region.
func (e *ECS) DescribeAccountAttributes(ctx context.Context, in *ecs.DescribeAccountAttributesRequest) (*ecs.DescribeAccountAttributesResponseBody, error) {
	f := e.f
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.enter(ctx, "DescribeAccountAttributes", e.region)
	if err != nil {
		return nil, err
	}
	values := map[string]int64{
		"max-postpaid-instance-vcpu-count":  r.postpaid,
		"used-postpaid-instance-vcpu-count": r.usedVCPUs(false),
		"max-spot-instance-vcpu-count":      r.spot,
		"used-spot-instance-vcpu-count":     r.usedVCPUs(true),
	}
	out := &ecs.DescribeAccountAttributesResponseBody{
		RequestId: new("req"), AccountAttributeItems: &ecs.DescribeAccountAttributesResponseBodyAccountAttributeItems{},
	}
	for _, name := range dara.StringSliceValue(in.AttributeName) {
		v, ok := values[name]
		if !ok {
			continue
		}
		out.AccountAttributeItems.AccountAttributeItem = append(out.AccountAttributeItems.AccountAttributeItem,
			&ecs.DescribeAccountAttributesResponseBodyAccountAttributeItemsAccountAttributeItem{
				AttributeName: new(name),
				AttributeValues: &ecs.DescribeAccountAttributesResponseBodyAccountAttributeItemsAccountAttributeItemAttributeValues{
					ValueItem: []*ecs.DescribeAccountAttributesResponseBodyAccountAttributeItemsAccountAttributeItemAttributeValuesValueItem{
						{Value: new(strconv.FormatInt(v, 10))},
					},
				},
			})
	}
	return out, nil
}
