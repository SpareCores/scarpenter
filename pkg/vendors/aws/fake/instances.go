/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// maxUserData is EC2's limit of raw user-data.
const maxUserData = 16384

// spotCloseDelay is how long a one-time spot request stays active after its
// instance terminated (measured on 2026-11-05: two to three minutes).
const spotCloseDelay = 3 * time.Minute

// EC2 is the EC2 API of one region of the account.
type EC2 struct {
	a      *Account
	region string
}

// EC2 returns the EC2 client of the region.
func (a *Account) EC2(region string) *EC2 { return &EC2{a: a, region: region} }

// begin starts a call: counts it, applies failures and returns the region.
func (e *EC2) begin(ctx context.Context, op string, in any) (*region, error) {
	e.a.mu.Lock()
	if err := e.a.begin(ctx, op, in); err != nil {
		e.a.mu.Unlock()
		return nil, err
	}
	r, err := e.a.regionOf(e.region)
	if err != nil {
		e.a.mu.Unlock()
		return nil, err
	}
	e.a.closeSpotRequests(r)
	return r, nil
}

func (e *EC2) end() { e.a.mu.Unlock() }

// closeSpotRequests closes the spot requests whose instance ended long enough
// ago.
func (a *Account) closeSpotRequests(r *region) {
	for _, s := range r.spot {
		if s.state == "active" && !s.closeAt.IsZero() && !a.now().Before(s.closeAt) {
			s.state = "closed"
		}
	}
}

func (i *instance) toSDK() types.Instance {
	out := types.Instance{
		InstanceId: new(i.id), InstanceType: types.InstanceType(i.typ), ImageId: new(i.ami),
		State:      &types.InstanceState{Name: i.state, Code: new(stateCode(i.state))},
		LaunchTime: &i.launched, SubnetId: new(i.subnet), VpcId: new(i.vpc), ClientToken: new(i.token),
		Placement: &types.Placement{
			AvailabilityZone: new(i.az), GroupName: new(i.pg), Tenancy: types.Tenancy(i.tenancy),
		},
		Tags: tagList(i.tags), PrivateIpAddress: new(i.privateIP), RootDeviceName: new(i.root),
		MetadataOptions: i.metadata,
	}
	if i.publicIP != "" {
		out.PublicIpAddress = new(i.publicIP)
	}
	if i.lifecycle != "" {
		out.InstanceLifecycle = types.InstanceLifecycleType(i.lifecycle)
	}
	for _, g := range i.groups {
		out.SecurityGroups = append(out.SecurityGroups, types.GroupIdentifier{GroupId: new(g)})
	}
	ni := types.InstanceNetworkInterface{
		SubnetId: new(i.subnet), VpcId: new(i.vpc), PrivateIpAddress: new(i.privateIP),
		PrivateIpAddresses: []types.InstancePrivateIpAddress{{PrivateIpAddress: new(i.privateIP), Primary: new(true)}},
	}
	if i.publicIP != "" {
		ni.Association = &types.InstanceNetworkInterfaceAssociation{PublicIp: new(i.publicIP)}
	}
	out.NetworkInterfaces = []types.InstanceNetworkInterface{ni}
	return out
}

func stateCode(s types.InstanceStateName) int32 {
	switch s {
	case types.InstanceStateNamePending:
		return 0
	case types.InstanceStateNameRunning:
		return 16
	case types.InstanceStateNameShuttingDown:
		return 32
	case types.InstanceStateNameTerminated:
		return 48
	case types.InstanceStateNameStopping:
		return 64
	}
	return 80
}

func (i *instance) attrs(name string) ([]string, bool) {
	switch name {
	case "instance-state-name":
		return []string{string(i.state)}, true
	case "client-token":
		return []string{i.token}, true
	case "vpc-id":
		return []string{i.vpc}, true
	case "subnet-id":
		return []string{i.subnet}, true
	case "instance.group-id":
		return slices.Clone(i.groups), true
	case "placement-group-name":
		return []string{i.pg}, true
	case "availability-zone":
		return []string{i.az}, true
	case "instance-type":
		return []string{i.typ}, true
	case "instance-lifecycle":
		l := i.lifecycle
		if l == "" {
			l = "normal"
		}
		return []string{l}, true
	case "instance-id":
		return []string{i.id}, true
	}
	return nil, false
}

// DescribeInstances implements EC2API.
func (e *EC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options),
) (*ec2.DescribeInstancesOutput, error) {
	r, err := e.begin(ctx, "DescribeInstances", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.Instance
	for _, id := range in.InstanceIds {
		if !strings.HasPrefix(id, "i-") {
			return nil, apiError("InvalidInstanceID.Malformed", fmt.Sprintf("Invalid id: %q", id))
		}
		if !slices.ContainsFunc(r.insts, func(i *instance) bool { return i.id == id }) {
			return nil, apiError("InvalidInstanceID.NotFound", fmt.Sprintf("The instance ID '%s' does not exist", id))
		}
	}
	for _, i := range r.insts {
		if len(in.InstanceIds) > 0 && !slices.Contains(in.InstanceIds, i.id) {
			continue
		}
		ok, err := matches(in.Filters, i.tags, i.attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, i.toSDK())
		}
	}
	if e.a.shortPages && in.NextToken == nil && len(in.Filters) > 0 && len(out) > 0 {
		return &ec2.DescribeInstancesOutput{NextToken: new("0")}, nil
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	res := &ec2.DescribeInstancesOutput{NextToken: next}
	// One reservation per instance is enough for the driver; the real service
	// groups them.
	for _, i := range items {
		res.Reservations = append(res.Reservations, types.Reservation{Instances: []types.Instance{i}})
	}
	return res, nil
}

// TerminateInstances implements EC2API.
func (e *EC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options),
) (*ec2.TerminateInstancesOutput, error) {
	r, err := e.begin(ctx, "TerminateInstances", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	res := &ec2.TerminateInstancesOutput{}
	for _, id := range in.InstanceIds {
		if !strings.HasPrefix(id, "i-") {
			return nil, apiError("InvalidInstanceID.Malformed", fmt.Sprintf("Invalid id: %q", id))
		}
		idx := slices.IndexFunc(r.insts, func(i *instance) bool { return i.id == id })
		if idx < 0 {
			return nil, apiError("InvalidInstanceID.NotFound", fmt.Sprintf("The instance ID '%s' does not exist", id))
		}
		i := r.insts[idx]
		prev := i.state
		if i.state != types.InstanceStateNameTerminated {
			e.a.terminate(r, i)
		}
		res.TerminatingInstances = append(res.TerminatingInstances, types.InstanceStateChange{
			InstanceId: new(id), PreviousState: &types.InstanceState{Name: prev},
			CurrentState: &types.InstanceState{Name: i.state},
		})
	}
	return res, nil
}

// terminate ends an instance: its volumes go with it and a spot request closes
// after a delay.
func (a *Account) terminate(r *region, i *instance) {
	i.state = types.InstanceStateNameTerminated
	i.terminatedAt = a.now()
	for _, v := range i.volumes {
		delete(r.volumes, v)
	}
	for _, s := range r.spot {
		if s.instance == i.id {
			s.closeAt = a.now().Add(spotCloseDelay)
		}
	}
}

// RunInstances implements EC2API.
func (e *EC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options),
) (*ec2.RunInstancesOutput, error) {
	r, err := e.begin(ctx, "RunInstances", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	a := e.a
	hash := hashInput(in)
	if tok := deref(in.ClientToken); tok != "" {
		if prev, ok := r.tokens[tok]; ok {
			if prev.tokenHash != hash {
				return nil, apiError("IdempotentParameterMismatch",
					"Client token "+tok+" was used with different parameters")
			}
			return &ec2.RunInstancesOutput{Instances: []types.Instance{prev.toSDK()}}, nil
		}
	}
	inst, err := a.validateRun(r, in)
	if err != nil {
		return nil, err
	}
	inst.token, inst.tokenHash = deref(in.ClientToken), hash
	if inst.token != "" {
		r.tokens[inst.token] = inst
	}
	r.insts = append(r.insts, inst)
	return &ec2.RunInstancesOutput{Instances: []types.Instance{inst.toSDK()}}, nil
}

// validateRun applies the checks of the service in its order and builds the
// instance.
func (a *Account) validateRun(r *region, in *ec2.RunInstancesInput) (*instance, error) {
	if deref(in.MinCount) != 1 || deref(in.MaxCount) != 1 {
		return nil, apiError("InvalidParameterValue", "this driver launches one instance per call")
	}
	if deref(in.ImageId) == "" {
		return nil, apiError("MissingParameter", "The request must contain the parameter ImageId")
	}
	typ, ok := a.typeOf(string(in.InstanceType))
	if !ok {
		return nil, apiError("InvalidInstanceType", fmt.Sprintf("The instance type '%s' does not exist", in.InstanceType))
	}
	spotMarket := in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == types.ResourceTypeSpotInstancesRequest && !spotMarket {
			return nil, apiError("InvalidParameterValue", "You cannot tag a spot instances request on an On-Demand launch")
		}
	}
	var userData string
	if in.UserData != nil {
		raw, err := base64.StdEncoding.DecodeString(*in.UserData)
		if err != nil {
			return nil, apiError("InvalidUserData.Malformed", "Invalid BASE64 encoding of user data.")
		}
		if len(raw) > maxUserData {
			return nil, apiError("InvalidParameterValue", "User data is limited to 16384 bytes")
		}
		userData = *in.UserData
	}
	inst := &instance{
		id: a.id("i"), typ: typ.Name, ami: deref(in.ImageId), userData: userData, state: types.InstanceStateNameRunning,
		launched: a.now(), tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeInstance), tenancy: "default",
		metadata: metadataResponse(in.MetadataOptions),
	}
	if err := a.place(r, in, typ, inst); err != nil {
		return nil, err
	}
	if err := a.network(r, in, inst); err != nil {
		return nil, err
	}
	if err := a.quotaCheck(r, in, typ, inst); err != nil {
		return nil, err
	}
	a.attachVolumes(r, in, inst)
	if m := in.InstanceMarketOptions; m != nil && m.MarketType == types.MarketTypeSpot {
		inst.lifecycle = "spot"
		req := &spotRequest{id: a.id("sir"), instance: inst.id, typ: typ.Name, state: "active", created: a.now()}
		r.spot = append(r.spot, req)
		inst.spotRequest = req.id
	}
	return inst, nil
}

func metadataResponse(in *types.InstanceMetadataOptionsRequest) *types.InstanceMetadataOptionsResponse {
	if in == nil {
		return &types.InstanceMetadataOptionsResponse{
			HttpTokens: types.HttpTokensStateOptional, HttpPutResponseHopLimit: new(int32(1)),
		}
	}
	return &types.InstanceMetadataOptionsResponse{
		HttpTokens: in.HttpTokens, HttpPutResponseHopLimit: in.HttpPutResponseHopLimit,
		HttpEndpoint: in.HttpEndpoint,
	}
}

// place checks the zone, the placement group and the tenancy.
func (a *Account) place(r *region, in *ec2.RunInstancesInput, typ InstanceType, inst *instance) error {
	if p := in.Placement; p != nil {
		inst.az = deref(p.AvailabilityZone)
		if p.Tenancy != "" {
			inst.tenancy = string(p.Tenancy)
		}
		if g := deref(p.GroupName); g != "" {
			pg := r.pgByName(g)
			if pg == nil {
				return apiError("InvalidPlacementGroup.Unknown", fmt.Sprintf("The placement group '%s' is unknown.", g))
			}
			if pg.strategy == "cluster" && typ.Burstable {
				return apiError("InvalidParameterCombination",
					fmt.Sprintf("The instance type %s is not supported in a cluster placement group", typ.Name))
			}
			if pg.strategy == "spread" && r.groupSize(g, inst.az) >= 7 {
				return apiError("InsufficientInstanceCapacity", "You've reached the limit of instances in this spread "+
					"placement group. A spread placement group can have up to seven instances per Availability Zone.")
			}
			inst.pg = g
		}
	}
	return nil
}

func (r *region) pgByName(name string) *placementGroup {
	for _, g := range r.pgs {
		if g.name == name {
			return g
		}
	}
	return nil
}

func (r *region) groupSize(name, az string) int {
	n := 0
	for _, i := range r.insts {
		if i.pg == name && i.az == az && i.live() {
			n++
		}
	}
	return n
}

// live says whether the instance still counts as running somewhere.
func (i *instance) live() bool {
	return i.state != types.InstanceStateNameTerminated
}

// network resolves the subnet, the zone and the security groups.
func (a *Account) network(r *region, in *ec2.RunInstancesInput, inst *instance) error {
	subnetID := deref(in.SubnetId)
	groups := in.SecurityGroupIds
	public := false
	setPublic := false
	if len(in.NetworkInterfaces) > 0 {
		ni := in.NetworkInterfaces[0]
		subnetID, groups = deref(ni.SubnetId), ni.Groups
		if ni.AssociatePublicIpAddress != nil {
			public, setPublic = *ni.AssociatePublicIpAddress, true
		}
	}
	if inst.az != "" {
		if z, ok := r.zoneByName(inst.az); !ok || z.Type != "availability-zone" {
			return apiError("InvalidParameterValue", "Invalid availability zone: ["+inst.az+"]")
		}
	}
	var sn *subnet
	if subnetID != "" {
		sn = r.subnets[subnetID]
		if sn == nil {
			return apiError("InvalidSubnetID.NotFound", fmt.Sprintf("The subnet ID '%s' does not exist", subnetID))
		}
	} else {
		// Without a subnet the service uses the default VPC's default subnet of the zone.
		for _, s := range r.subnets {
			if s.defaultForAZ && (inst.az == "" || s.az == inst.az) {
				sn = s
			}
		}
		if sn == nil {
			return apiError("MissingInput", "No subnet: the zone has no default subnet")
		}
	}
	if inst.az != "" && inst.az != sn.az {
		return apiError("InvalidParameterValue", fmt.Sprintf("Value (%s) for parameter availabilityZone is invalid. "+
			"Subnet '%s' is in the availability zone %s", inst.az, sn.id, sn.az))
	}
	if _, ok := r.zoneByName(sn.az); !ok {
		return apiError("InvalidParameterValue", "unknown zone "+sn.az)
	}
	inst.az, inst.subnet, inst.vpc = sn.az, sn.id, sn.vpc
	for _, g := range groups {
		s := r.sgs[g]
		if s == nil {
			return apiError("InvalidGroup.NotFound", fmt.Sprintf("The security group '%s' does not exist", g))
		}
		if s.vpc != sn.vpc {
			return apiError("InvalidParameter", fmt.Sprintf("Security group %s and subnet %s belong to different networks.", g, sn.id))
		}
	}
	if len(groups) == 0 {
		for _, s := range r.sgs {
			if s.vpc == sn.vpc && s.name == "default" {
				groups = []string{s.id}
			}
		}
	}
	inst.groups = slices.Clone(groups)
	if sn.free <= 0 {
		return apiError("InsufficientFreeAddressesInSubnet", "There are not enough free addresses in subnet "+sn.id)
	}
	sn.free--
	r.cidrNext++
	inst.privateIP = fmt.Sprintf("10.%d.%d.%d", (r.cidrNext>>16)&255, (r.cidrNext>>8)&255, r.cidrNext&255)
	if (setPublic && public) || (!setPublic && sn.mapPublic) {
		inst.publicIP = fmt.Sprintf("203.0.%d.%d", (r.cidrNext>>8)&255, r.cidrNext&255)
	}
	return nil
}

// attachVolumes creates the volumes of the block device mappings and records the
// root device name.
func (a *Account) attachVolumes(r *region, in *ec2.RunInstancesInput, inst *instance) {
	inst.root = "/dev/xvda"
	if img, ok := r.images[inst.ami]; ok {
		inst.root = img.RootDevice
	}
	tags := tagsFromSpecs(in.TagSpecifications, types.ResourceTypeVolume)
	for _, bd := range in.BlockDeviceMappings {
		if bd.Ebs == nil {
			continue
		}
		v := &volume{id: a.id("vol"), tags: tags}
		r.volumes[v.id] = v
		inst.volumes = append(inst.volumes, v.id)
	}
	if len(in.BlockDeviceMappings) == 0 {
		v := &volume{id: a.id("vol"), tags: tags}
		r.volumes[v.id] = v
		inst.volumes = append(inst.volumes, v.id)
	}
}

// SetInstanceState moves an instance of the region to a state (stopping,
// shutting-down, stopped, ...) the way the service would over time.
func (a *Account) SetInstanceState(regionName, id string, s types.InstanceStateName) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	for _, i := range r.insts {
		if i.id == id {
			i.state = s
			if s == types.InstanceStateNameTerminated {
				a.terminate(r, i)
			}
		}
	}
}

// AddForeignInstance adds an instance of another workload of the account (no
// SCarpenter tags): what the account-wide quota usage must count.
func (a *Account) AddForeignInstance(regionName, typ string, spot bool, state types.InstanceStateName) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	i := &instance{
		id: a.id("i"), typ: typ, state: state, launched: a.now(), tags: map[string]string{"Name": "other"},
		owner: "other", ami: "ami-other", tenancy: "default",
	}
	if len(r.azs) > 0 {
		i.az = r.azs[0].Name
	}
	if spot {
		i.lifecycle = "spot"
		r.spot = append(r.spot, &spotRequest{id: a.id("sir"), instance: i.id, typ: typ, state: "active", created: a.now()})
	}
	r.insts = append(r.insts, i)
	return i.id
}

// AddOpenSpotRequest adds a spot request still waiting for capacity (it has no
// instance yet).
func (a *Account) AddOpenSpotRequest(regionName, typ string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	s := &spotRequest{id: a.id("sir"), typ: typ, state: "open", created: a.now()}
	r.spot = append(r.spot, s)
	return s.id
}

// AssignSpotRequest gives an open spot request its instance without closing the
// request's state change yet: the moment in between, in which the request is
// still open but already has an instance, must not be counted twice.
func (a *Account) AssignSpotRequest(regionName, requestID, instanceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.regions[regionName].spot {
		if s.id == requestID {
			s.instance = instanceID
		}
	}
}

// SpotRequests returns the ID and state of the region's spot requests.
func (a *Account) SpotRequests(regionName string) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	a.closeSpotRequests(r)
	out := map[string]string{}
	for _, s := range r.spot {
		out[s.id] = s.state
	}
	return out
}

// InstanceStates returns the state of every instance of the region by ID.
func (a *Account) InstanceStates(regionName string) map[string]types.InstanceStateName {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]types.InstanceStateName{}
	for _, i := range a.regions[regionName].insts {
		out[i.id] = i.state
	}
	return out
}

// UserData returns the base64 user-data an instance was launched with.
func (a *Account) UserData(regionName, id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, i := range a.regions[regionName].insts {
		if i.id == id {
			return i.userData
		}
	}
	return ""
}

// Volumes returns the IDs of the volumes of the region.
func (a *Account) Volumes(regionName string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.regions[regionName].volumes))
	for id := range a.regions[regionName].volumes {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
