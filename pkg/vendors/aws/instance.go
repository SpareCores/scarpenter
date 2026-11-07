/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// defaultRootDevice is Flatcar's root device name, used when the image is not
// known to DescribeImages (a launch with it fails with InvalidAMIID anyway).
const defaultRootDevice = "/dev/xvda"

// launchStates are the states of instances that count as existing for Find and
// List; terminated and shutting-down ones are gone for Karpenter.
var launchStates = []string{
	string(ec2types.InstanceStateNamePending), string(ec2types.InstanceStateNameRunning),
	string(ec2types.InstanceStateNameStopping), string(ec2types.InstanceStateNameStopped),
}

// launchResources are the resources of a launch, as Prepare resolved them.
type launchResources struct {
	subnet         string
	groups         []string
	placementGroup string
}

// resourcesOf extracts the subnet, security groups and placement group from the
// prepared resources.
func resourcesOf(p *vendors.Prepared) launchResources {
	var lr launchResources
	if p == nil {
		return lr
	}
	for _, r := range p.Resources {
		switch r.Kind {
		case KindSubnets:
			if lr.subnet == "" {
				lr.subnet = r.ID
			}
		case KindSecurityGroups, KindRunSecurityGroups:
			if !slices.Contains(lr.groups, r.ID) {
				lr.groups = append(lr.groups, r.ID)
			}
		case KindPlacementGroups:
			lr.placementGroup = r.Name
		}
	}
	return lr
}

// CreateInstance implements vendors.Driver: one RunInstances call with the
// NodeClaim's UID as ClientToken. A replay of the token returns the original
// instance whatever else the request says: EC2 answers a replay with other
// parameters with IdempotentParameterMismatch, which resolves to the instance
// the token created.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooLarge, fmt.Errorf(
			"the user-data is %d bytes; EC2 accepts at most %d (a larger cluster CA or Ignition addition does not fit)",
			len(req.UserData), MaxUserData))
	}
	cl, err := d.clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	in, err := d.runInput(ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := cl.EC2.RunInstances(ctx, in)
	if err != nil {
		if isCode(err, "IdempotentParameterMismatch") && req.IdempotencyKey != "" {
			if inst, ferr := d.byToken(ctx, req.Region, req.IdempotencyKey); ferr == nil {
				return inst, nil
			}
		}
		if isCode(err, "InsufficientFreeAddressesInSubnet") && in.NetworkInterfaces != nil {
			d.forgetSubnet(deref(in.NetworkInterfaces[0].SubnetId))
		}
		return nil, classify(err, errContext{instanceType: req.InstanceType, zone: req.Zone})
	}
	if len(out.Instances) != 1 {
		return nil, vendors.Errorf(vendors.Fatal, "RunInstances returned %d instances for one", len(out.Instances))
	}
	return toInstance(req.Region, out.Instances[0]), nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// byToken finds the instance a ClientToken created, whatever its state.
func (d *Driver) byToken(ctx context.Context, region, token string) (*vendors.Instance, error) {
	insts, err := d.describeInstances(ctx, region, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: aws.String("client-token"), Values: []string{token}}},
	})
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance has the client token %s", token)
	}
	// A token names one instance.
	return toInstance(region, insts[0]), nil
}

// runInput builds the RunInstances call (overview section 11.4).
func (d *Driver) runInput(ctx context.Context, req vendors.CreateRequest) (*ec2.RunInstancesInput, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	lr := resourcesOf(req.Prepared)
	subnetID, err := d.launchSubnet(ctx, req.Region, req.Zone, lr.subnet)
	if err != nil {
		return nil, err
	}
	root := d.rootDevice(ctx, req.Region, req.Image.Ref)
	rv := rootVolume(st)
	if want := int64(rv.GiB) * giB; req.Image.RootDiskBytes > want {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the root volume of %d GiB is smaller than the image's root disk of %d GiB", rv.GiB,
			(req.Image.RootDiskBytes+giB-1)/giB))
	}
	bdms := []ec2types.BlockDeviceMapping{{DeviceName: aws.String(root), Ebs: ebsDevice(rv)}}
	for i, dv := range dataVolumes(st) {
		bdms = append(bdms, ec2types.BlockDeviceMapping{
			DeviceName: aws.String(dataDeviceName(i)), Ebs: ebsDevice(dataVolume(dv)),
		})
	}
	nif := ec2types.InstanceNetworkInterfaceSpecification{
		DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnetID), Groups: lr.groups,
		AssociatePublicIpAddress: aws.Bool(boolOr(req.Settings.Networking.PublicIPv4, true)),
		DeleteOnTermination:      aws.Bool(true),
	}
	if boolOr(req.Settings.Networking.PublicIPv6, false) {
		nif.Ipv6AddressCount = aws.Int32(1)
	}
	net, err := decodeNetwork(req.Settings.Network)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	if x := net.ENAExpress; x != nil && x.Enabled {
		nif.EnaSrdSpecification = &ec2types.EnaSrdSpecificationRequest{
			EnaSrdEnabled:          aws.Bool(true),
			EnaSrdUdpSpecification: &ec2types.EnaSrdUdpSpecificationRequest{EnaSrdUdpEnabled: aws.Bool(boolOr(x.UDP, false))},
		}
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	placement := &ec2types.Placement{AvailabilityZone: aws.String(req.Zone)}
	if lr.placementGroup != "" {
		placement.GroupName = aws.String(lr.placementGroup)
	}
	if pl.Tenancy == awsapi.Tenancy("dedicated") {
		placement.Tenancy = ec2types.TenancyDedicated
	}
	tokens, hops := metadata(st)
	tags := d.instanceTags(req)
	in := &ec2.RunInstancesInput{
		ImageId: aws.String(req.Image.Ref), InstanceType: ec2types.InstanceType(req.InstanceType),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		UserData:            aws.String(base64.StdEncoding.EncodeToString(req.UserData)),
		BlockDeviceMappings: bdms, NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{nif},
		Placement: placement,
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{
			HttpTokens: ec2types.HttpTokensState(tokens), HttpPutResponseHopLimit: aws.Int32(hops),
			HttpEndpoint:         ec2types.InstanceMetadataEndpointStateEnabled,
			InstanceMetadataTags: ec2types.InstanceMetadataTagsStateDisabled,
		},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
			{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: tags},
		},
	}
	if req.IdempotencyKey != "" {
		in.ClientToken = aws.String(req.IdempotencyKey)
	}
	if req.CapacityType == vendors.CapacitySpot {
		// The IAM policy allows RunInstances on the spot request only with the
		// cluster tag, so the request is tagged too (EC2 refuses the tags of a
		// spot request on an on-demand launch).
		in.TagSpecifications = append(in.TagSpecifications, ec2types.TagSpecification{
			ResourceType: ec2types.ResourceTypeSpotInstancesRequest, Tags: tags,
		})
		in.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
			SpotOptions: &ec2types.SpotMarketOptions{
				SpotInstanceType:             ec2types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: ec2types.InstanceInterruptionBehaviorTerminate,
			},
		}
	}
	if st.InstanceProfile != "" {
		in.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Name: aws.String(st.InstanceProfile)}
	}
	return in, nil
}

// dataDeviceName names the device of the i-th extra volume: /dev/sdf, /dev/sdg,
// ... (the names EC2 recommends for data volumes; Nitro instances show them as
// NVMe devices, which the first-boot unit finds by serial).
func dataDeviceName(i int) string { return fmt.Sprintf("/dev/sd%c", 'f'+i) }

// ebsDevice is the EBS part of a block device mapping. Volumes are always
// deleted with the instance.
func ebsDevice(v volume) *ec2types.EbsBlockDevice {
	e := &ec2types.EbsBlockDevice{
		VolumeType: ec2types.VolumeType(v.Type), VolumeSize: aws.Int32(v.GiB), Encrypted: aws.Bool(v.Encrypted),
		DeleteOnTermination: aws.Bool(true),
	}
	if v.IOPS != nil && (v.Type == awsapi.VolumeTypeGP3 || v.Type == awsapi.VolumeTypeIO1 ||
		v.Type == awsapi.VolumeTypeIO2) {
		e.Iops = v.IOPS
	}
	if v.Throughput != nil && v.Type == awsapi.VolumeTypeGP3 {
		e.Throughput = v.Throughput
	}
	if v.Encrypted && v.KMSKeyID != "" {
		e.KmsKeyId = aws.String(v.KMSKeyID)
	}
	return e
}

// instanceTags are the tags of the instance, its volumes and its network
// interface: the NodeClass's own tags, the logical tags and Name=<nodeclaim>.
// The logical tags win over the NodeClass's.
func (d *Driver) instanceTags(req vendors.CreateRequest) []ec2types.Tag {
	m := map[string]string{}
	maps.Copy(m, req.ExtraTags)
	if name := req.Tags[vendors.TagNodeClaim]; name != "" {
		m["Name"] = name
	}
	for k, v := range req.Tags {
		m[d.TagKey(k)] = v
	}
	return tagList(m)
}

// tagList converts a map to SDK tags in a fixed order.
func tagList(m map[string]string) []ec2types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// rootDevice returns the root device name of the image (cached); a missing or
// unreadable image falls back to Flatcar's /dev/xvda.
func (d *Driver) rootDevice(ctx context.Context, region, ami string) string {
	key := region + "/" + ami
	d.mu.Lock()
	if r, ok := d.roots[key]; ok {
		d.mu.Unlock()
		return r
	}
	d.mu.Unlock()
	cl, err := d.clients(ctx, region)
	if err != nil {
		return defaultRootDevice
	}
	out, err := cl.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
	if err != nil || len(out.Images) != 1 || deref(out.Images[0].RootDeviceName) == "" {
		return defaultRootDevice
	}
	name := *out.Images[0].RootDeviceName
	d.mu.Lock()
	d.roots[key] = name
	d.mu.Unlock()
	return name
}

// toInstance converts an EC2 instance.
func toInstance(region string, in ec2types.Instance) *vendors.Instance {
	inst := &vendors.Instance{
		Ref: vendors.NewInstanceRef(region, deref(in.InstanceId)), Region: region, Type: string(in.InstanceType),
		CapacityType: vendors.CapacityOnDemand, State: stateOf(in.State), Tags: map[vendors.TagKey]string{},
	}
	if in.Placement != nil {
		inst.Zone = deref(in.Placement.AvailabilityZone)
	}
	if in.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
		inst.CapacityType = vendors.CapacitySpot
	}
	if in.LaunchTime != nil {
		inst.CreatedAt = *in.LaunchTime
	}
	byName := map[string]string{}
	for _, t := range in.Tags {
		byName[deref(t.Key)] = deref(t.Value)
	}
	for _, k := range vendors.TagKeys {
		if v, ok := byName[vendors.DefaultTagKey(k)]; ok {
			inst.Tags[k] = v
		}
	}
	inst.Addresses = addressesOf(in)
	return inst
}

// stateOf maps an EC2 state: stopped and stopping instances still exist (and
// still cost for their volumes), so they are not gone; shutting-down is
// terminating.
func stateOf(s *ec2types.InstanceState) string {
	if s == nil {
		return vendors.StatePending
	}
	switch s.Name {
	case ec2types.InstanceStateNamePending:
		return vendors.StatePending
	case ec2types.InstanceStateNameShuttingDown:
		return vendors.StateTerminating
	case ec2types.InstanceStateNameTerminated:
		return vendors.StateTerminated
	}
	return vendors.StateRunning
}

// addressesOf lists the instance's addresses: the private and public IPv4
// addresses and the IPv6 addresses of every network interface. The serving
// certificate approver checks the node's IP SANs against them.
func addressesOf(in ec2types.Instance) []string {
	var out []string
	add := func(a *string) {
		if a != nil && *a != "" && !slices.Contains(out, *a) {
			out = append(out, *a)
		}
	}
	add(in.PrivateIpAddress)
	add(in.PublicIpAddress)
	for _, ni := range in.NetworkInterfaces {
		add(ni.PrivateIpAddress)
		for _, p := range ni.PrivateIpAddresses {
			add(p.PrivateIpAddress)
			if p.Association != nil {
				add(p.Association.PublicIp)
			}
		}
		if ni.Association != nil {
			add(ni.Association.PublicIp)
		}
		for _, v6 := range ni.Ipv6Addresses {
			add(v6.Ipv6Address)
		}
	}
	return out
}

// describeInstances reads every page of a DescribeInstances call.
func (d *Driver) describeInstances(ctx context.Context, region string, in *ec2.DescribeInstancesInput,
) ([]ec2types.Instance, error) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []ec2types.Instance
	for {
		res, err := cl.EC2.DescribeInstances(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, r := range res.Reservations {
			out = append(out, r.Instances...)
		}
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// GetInstance implements vendors.Driver; terminated instances are returned
// (EC2 keeps them visible for about an hour).
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	id := ref.ID()
	if id == "" {
		return nil, vendors.Errorf(vendors.NotFound, "the instance reference %q has no instance ID", ref)
	}
	insts, err := d.describeInstances(ctx, ref.Region(), &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "instance %s does not exist", ref)
	}
	return toInstance(ref.Region(), insts[0]), nil
}

// tagFilters are the EC2 filters for logical tags.
func (d *Driver) tagFilters(tags map[vendors.TagKey]string) []ec2types.Filter {
	var out []ec2types.Filter
	for _, k := range vendors.TagKeys {
		if v, ok := tags[k]; ok {
			out = append(out, ec2types.Filter{Name: aws.String("tag:" + d.TagKey(k)), Values: []string{v}})
		}
	}
	return out
}

func (d *Driver) instanceFilters(tags map[vendors.TagKey]string) []ec2types.Filter {
	return append(d.tagFilters(tags), ec2types.Filter{Name: aws.String("instance-state-name"), Values: launchStates})
}

// FindInstance implements vendors.Driver: the newest instance with all the
// tags that is not gone.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string) (*vendors.Instance, error,
) {
	if !d.knownRegion(ctx, region) {
		return nil, vendors.Errorf(vendors.NotFound, "the account cannot use the region %q", region)
	}
	insts, err := d.describeInstances(ctx, region, &ec2.DescribeInstancesInput{Filters: d.instanceFilters(tags)})
	if err != nil {
		return nil, err
	}
	var best *ec2types.Instance
	for i := range insts {
		if best == nil || deref(insts[i].LaunchTime).After(deref(best.LaunchTime)) {
			best = &insts[i]
		}
	}
	if best == nil {
		return nil, vendors.Errorf(vendors.NotFound, "no instance in %s has the tags %v", region, tags)
	}
	return toInstance(region, *best), nil
}

// ListInstances implements vendors.Driver. Regions the account cannot use have
// no instances.
func (d *Driver) ListInstances(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	var out []*vendors.Instance
	for _, region := range regions {
		if !d.knownRegion(ctx, region) {
			continue
		}
		insts, err := d.describeInstances(ctx, region, &ec2.DescribeInstancesInput{Filters: d.instanceFilters(tags)})
		if err != nil {
			return nil, fmt.Errorf("listing the instances of %s: %w", region, err)
		}
		for _, i := range insts {
			if inst := toInstance(region, i); !inst.Gone() {
				out = append(out, inst)
			}
		}
	}
	return out, nil
}

// DeleteInstance implements vendors.Driver: an instance that does not exist or is
// already terminated is success. An instance that does not carry this cluster's
// tag is not ours (a wrong or tampered instance reference must not terminate a
// foreign instance when the credentials are broader than the documented policy)
// and is refused with a Fatal error.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	id := ref.ID()
	if id == "" {
		return nil
	}
	cl, err := d.clients(ctx, ref.Region())
	if err != nil {
		return err
	}
	out, err := cl.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		if isCode(err, "InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed") {
			return nil
		}
		return classify(err, errContext{})
	}
	seen := false
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			seen = true
			inst := toInstance(ref.Region(), i)
			if inst.State == vendors.StateTerminated {
				return nil
			}
			if d.cfg.ClusterName == "" || inst.Tags[vendors.TagCluster] != d.cfg.ClusterName {
				return vendors.Errorf(vendors.Fatal, "the instance %s does not carry the tag of cluster %q and is not terminated",
					ref, d.cfg.ClusterName)
			}
		}
	}
	if !seen {
		// Nothing came back to check the ownership of: there is nothing to terminate.
		return nil
	}
	if _, err := cl.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		if isCode(err, "InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed") {
			return nil
		}
		return classify(err, errContext{})
	}
	return nil
}

// knownRegion says whether the account can use the region (it is listed by
// DescribeRegions); a failure to list is taken as known, so that a transient
// error surfaces from the call itself.
func (d *Driver) knownRegion(ctx context.Context, region string) bool {
	regions, err := d.Regions(ctx)
	if err != nil {
		return strings.TrimSpace(region) != ""
	}
	return slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region })
}
