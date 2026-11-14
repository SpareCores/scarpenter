/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// LaunchInstance implements oci.ComputeAPI. The retry policy of the request is
// not looked at: the fake never retries (the real adapter's tests cover that).
func (c *Cloud) LaunchInstance(ctx context.Context, regionName string, req core.LaunchInstanceRequest,
) (core.LaunchInstanceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launches = append(c.launches, req)
	if err := c.enter(ctx, "LaunchInstance"); err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	r, err := c.usable(regionName)
	if err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	d := req.LaunchInstanceDetails
	if deref(d.CompartmentId) != c.compartment {
		return core.LaunchInstanceResponse{}, notFound("compartment")
	}
	ad := deref(d.AvailabilityDomain)
	shapes, ok := r.shapes[ad]
	if !ok {
		return core.LaunchInstanceResponse{}, invalid("The availability domain %q does not exist in %s", ad, regionName)
	}
	shapeName := deref(d.Shape)
	shape, ok := shapes[shapeName]
	if !ok {
		return core.LaunchInstanceResponse{}, invalid(
			"Shape %s is not available in the availability domain %s", shapeName, ad)
	}
	src, _ := d.SourceDetails.(core.InstanceSourceViaImageDetails)
	if deref(src.ImageId) == "" {
		return core.LaunchInstanceResponse{}, invalid("The image is required")
	}
	if d.CreateVnicDetails == nil || deref(d.CreateVnicDetails.SubnetId) == "" {
		return core.LaunchInstanceResponse{}, Error(http.StatusBadRequest, "MissingParameter", "subnetId is required")
	}
	if err := c.checkVnic(regionName, ad, d.CreateVnicDetails); err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	ocpus, mem, err := sizeOf(shape, d.ShapeConfig)
	if err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	if total := metadataBytes(d.Metadata); total > 32000 {
		return core.LaunchInstanceResponse{}, invalid("The metadata is %d bytes; the limit is 32000", total)
	}

	// A replay of a launch that succeeded returns the original; the same token
	// with another request is a conflict. A launch that failed records nothing.
	fp := fingerprint(regionName, d)
	tkey := regionName + "|" + deref(req.OpcRetryToken)
	if tok, seen := c.tokens[tkey]; seen && deref(req.OpcRetryToken) != "" {
		if tok.fingerprint != fp {
			return core.LaunchInstanceResponse{}, Error(http.StatusConflict, "Conflict",
				"The retry token was already used with a different request")
		}
		return core.LaunchInstanceResponse{Instance: provisioning(*c.instances[tok.instanceID])}, nil
	}

	if c.stockouts[regionName+"|"+ad+"|"+shapeName] {
		return core.LaunchInstanceResponse{}, Error(http.StatusInternalServerError, "InternalError",
			"Out of host capacity.")
	}
	if err := c.checkLimits(regionName, ad, shape, ocpus, mem); err != nil {
		return core.LaunchInstanceResponse{}, err
	}

	id := c.nextID("instance", regionName)
	now := common.SDKTime{Time: c.now()}
	in := &core.Instance{
		AvailabilityDomain: &ad, CompartmentId: &c.compartment, Id: &id, LifecycleState: core.InstanceLifecycleStateRunning,
		Region: &regionName, Shape: &shapeName, TimeCreated: &now, DisplayName: d.DisplayName, FreeformTags: d.FreeformTags,
		Metadata: d.Metadata, ImageId: src.ImageId, SourceDetails: d.SourceDetails, LaunchOptions: d.LaunchOptions,
		InstanceOptions: d.InstanceOptions,
		ShapeConfig: &core.InstanceShapeConfig{Ocpus: new(float32(ocpus)),
			MemoryInGBs: new(float32(mem))},
	}
	if p := d.PreemptibleInstanceConfig; p != nil {
		in.PreemptibleInstanceConfig = &core.PreemptibleInstanceConfigDetails{PreemptionAction: p.PreemptionAction}
	}
	c.instances[id] = in
	c.boot[id] = bootVolume{ID: c.nextID("bootvolume", regionName), SizeGB: deref(src.BootVolumeSizeInGBs),
		VPUs: deref(src.BootVolumeVpusPerGB), KMSKey: deref(src.KmsKeyId)}
	c.attachVnic(regionName, in, d.CreateVnicDetails)
	if deref(req.OpcRetryToken) != "" {
		c.tokens[tkey] = tokenRec{fingerprint: fp, instanceID: id}
	}
	if c.lost > 0 {
		c.lost--
		return core.LaunchInstanceResponse{}, fmt.Errorf("reading the launch response: %w", context.DeadlineExceeded)
	}
	return core.LaunchInstanceResponse{Instance: provisioning(*in)}, nil
}

// checkVnic checks what the launch names: the subnet exists in the region and serves the
// availability domain, allows the public address asked for, and the groups are in its VCN.
func (c *Cloud) checkVnic(regionName, ad string, v *core.CreateVnicDetails) error {
	id := deref(v.SubnetId)
	sub, ok := c.net.subnets[id]
	if !ok || c.net.objRegion[id] != regionName {
		return notFound("subnet " + id)
	}
	if sub.AvailabilityDomain != nil && *sub.AvailabilityDomain != ad {
		return invalid("The subnet %s is in the availability domain %s, not in %s", id, *sub.AvailabilityDomain, ad)
	}
	if deref(sub.ProhibitPublicIpOnVnic) && deref(v.AssignPublicIp) {
		return invalid("The subnet %s prohibits public IP addresses on VNICs", id)
	}
	if deref(v.AssignIpv6Ip) && sub.Ipv6CidrBlock == nil {
		return invalid("The subnet %s is not enabled for IPv6", id)
	}
	for _, g := range v.NsgIds {
		nsg, ok := c.net.nsgs[g]
		if !ok || c.net.objRegion[g] != regionName {
			return notFound("network security group " + g)
		}
		if deref(nsg.VcnId) != deref(sub.VcnId) {
			return invalid("The network security group %s is not in the VCN of the subnet", g)
		}
	}
	return nil
}

func provisioning(in core.Instance) core.Instance {
	in.LifecycleState = core.InstanceLifecycleStateProvisioning
	return in
}

// sizeOf validates the shape configuration of a launch against the shape and
// returns the OCPUs and memory the instance gets.
func sizeOf(shape core.Shape, cfg *core.LaunchInstanceShapeConfigDetails) (ocpus, mem float64, err error) {
	if !deref(shape.IsFlexible) {
		if cfg != nil && (cfg.Ocpus != nil || cfg.MemoryInGBs != nil) {
			return 0, 0, invalid("shapeConfig is not accepted for the fixed shape %s", deref(shape.Shape))
		}
		return float64(deref(shape.Ocpus)), float64(deref(shape.MemoryInGBs)), nil
	}
	if cfg == nil || cfg.Ocpus == nil {
		return 0, 0, invalid("shapeConfig with ocpus is required for the flexible shape %s", deref(shape.Shape))
	}
	ocpus = float64(*cfg.Ocpus)
	if cfg.MemoryInGBs != nil {
		mem = float64(*cfg.MemoryInGBs)
	} else {
		mem = ocpus * float64(deref(shape.MemoryOptions.DefaultPerOcpuInGBs))
	}
	o, m := shape.OcpuOptions, shape.MemoryOptions
	switch {
	case ocpus < float64(deref(o.Min)) || ocpus > float64(deref(o.Max)):
		return 0, 0, invalid("shapeConfig.ocpus %g is outside %g to %g", ocpus, deref(o.Min), deref(o.Max))
	case mem < float64(deref(m.MinInGBs)) || mem > float64(deref(m.MaxInGBs)):
		return 0, 0, invalid("shapeConfig.memoryInGBs %g is outside %g to %g", mem, deref(m.MinInGBs), deref(m.MaxInGBs))
	case mem/ocpus < float64(deref(m.MinPerOcpuInGBs))-1e-9 || mem/ocpus > float64(deref(m.MaxPerOcpuInGBs))+1e-9:
		return 0, 0, invalid("shapeConfig: %g GB per OCPU is outside %g to %g", mem/ocpus, deref(m.MinPerOcpuInGBs),
			deref(m.MaxPerOcpuInGBs))
	}
	return ocpus, mem, nil
}

func metadataBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(v)
	}
	return n
}

// fingerprint is what a retry token binds a request to.
func fingerprint(regionName string, d core.LaunchInstanceDetails) string {
	var ocpus, mem float32
	if d.ShapeConfig != nil {
		ocpus, mem = deref(d.ShapeConfig.Ocpus), deref(d.ShapeConfig.MemoryInGBs)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%g|%g|%t|%s", regionName, deref(d.CompartmentId), deref(d.AvailabilityDomain),
		deref(d.Shape), ocpus, mem, d.PreemptibleInstanceConfig != nil, deref(d.DisplayName))
}

// attachVnic creates the VNIC and its attachment of a new instance.
func (c *Cloud) attachVnic(regionName string, in *core.Instance, v *core.CreateVnicDetails) {
	n := c.seq
	vnicID := c.nextID("vnic", regionName)
	vnic := core.Vnic{
		Id: &vnicID, AvailabilityDomain: in.AvailabilityDomain, CompartmentId: &c.compartment,
		LifecycleState: core.VnicLifecycleStateAvailable, IsPrimary: new(true), SubnetId: v.SubnetId,
		NsgIds: slices.Clone(v.NsgIds), FreeformTags: v.FreeformTags, DisplayName: v.DisplayName,
		PrivateIp: new(fmt.Sprintf("10.240.%d.%d", n/250, n%250+2)),
	}
	if deref(v.AssignPublicIp) {
		vnic.PublicIp = new(fmt.Sprintf("203.0.113.%d", n%250+1))
	}
	if deref(v.AssignIpv6Ip) {
		vnic.Ipv6Addresses = []string{fmt.Sprintf("2001:db8::%x", n)}
	}
	c.vnics[vnicID] = vnic
	attID := c.nextID("vnicattachment", regionName)
	c.attach[*in.Id] = []core.VnicAttachment{{
		Id: &attID, AvailabilityDomain: in.AvailabilityDomain, CompartmentId: &c.compartment, InstanceId: in.Id,
		LifecycleState: core.VnicAttachmentLifecycleStateAttached, TimeCreated: in.TimeCreated, VnicId: &vnicID,
		SubnetId: v.SubnetId, NicIndex: new(0),
	}}
}

// GetInstance implements oci.ComputeAPI. A TERMINATED instance is returned.
func (c *Cloud) GetInstance(ctx context.Context, regionName string, req core.GetInstanceRequest,
) (core.GetInstanceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "GetInstance"); err != nil {
		return core.GetInstanceResponse{}, err
	}
	in, err := c.find(regionName, deref(req.InstanceId))
	if err != nil {
		return core.GetInstanceResponse{}, err
	}
	return core.GetInstanceResponse{Instance: *in}, nil
}

func (c *Cloud) find(regionName, id string) (*core.Instance, error) {
	if _, err := c.usable(regionName); err != nil {
		return nil, err
	}
	in, ok := c.instances[id]
	if !ok || deref(in.Region) != regionName {
		return nil, notFound("instance " + id)
	}
	return in, nil
}

// ListInstances implements oci.ComputeAPI: the instances of the compartment, every
// state (a TERMINATED one stays listed), by creation time.
func (c *Cloud) ListInstances(ctx context.Context, regionName string, req core.ListInstancesRequest,
) (core.ListInstancesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListInstances"); err != nil {
		return core.ListInstancesResponse{}, err
	}
	if _, err := c.usable(regionName); err != nil {
		return core.ListInstancesResponse{}, err
	}
	if deref(req.CompartmentId) != c.compartment {
		return core.ListInstancesResponse{}, notFound("compartment")
	}
	var all []core.Instance
	for _, in := range c.instances {
		switch {
		case deref(in.Region) != regionName:
		case req.DisplayName != nil && deref(in.DisplayName) != *req.DisplayName:
		case req.AvailabilityDomain != nil && deref(in.AvailabilityDomain) != *req.AvailabilityDomain:
		case req.LifecycleState != "" && in.LifecycleState != req.LifecycleState:
		default:
			all = append(all, *in)
		}
	}
	slices.SortFunc(all, func(a, b core.Instance) int {
		if t := a.TimeCreated.Compare(b.TimeCreated.Time); t != 0 {
			return t
		}
		return strings.Compare(deref(a.Id), deref(b.Id))
	})
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListInstancesResponse{Items: page, OpcNextPage: next}, nil
}

// TerminateInstance implements oci.ComputeAPI. A terminated instance stays
// listed; its boot volume is deleted unless preserveBootVolume says otherwise.
func (c *Cloud) TerminateInstance(ctx context.Context, regionName string, req core.TerminateInstanceRequest,
) (core.TerminateInstanceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "TerminateInstance"); err != nil {
		return core.TerminateInstanceResponse{}, err
	}
	in, err := c.find(regionName, deref(req.InstanceId))
	if err != nil {
		return core.TerminateInstanceResponse{}, err
	}
	in.LifecycleState = core.InstanceLifecycleStateTerminated
	if !deref(req.PreserveBootVolume) {
		delete(c.boot, *in.Id)
	}
	for i := range c.attach[*in.Id] {
		c.attach[*in.Id][i].LifecycleState = core.VnicAttachmentLifecycleStateDetached
	}
	return core.TerminateInstanceResponse{}, nil
}

// ListVnicAttachments implements oci.ComputeAPI.
func (c *Cloud) ListVnicAttachments(ctx context.Context, regionName string, req core.ListVnicAttachmentsRequest,
) (core.ListVnicAttachmentsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListVnicAttachments"); err != nil {
		return core.ListVnicAttachmentsResponse{}, err
	}
	if _, err := c.usable(regionName); err != nil {
		return core.ListVnicAttachmentsResponse{}, err
	}
	if deref(req.CompartmentId) != c.compartment {
		return core.ListVnicAttachmentsResponse{}, notFound("compartment")
	}
	var all []core.VnicAttachment
	for id, atts := range c.attach {
		if req.InstanceId == nil || *req.InstanceId == id {
			all = append(all, atts...)
		}
	}
	slices.SortFunc(all, func(a, b core.VnicAttachment) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListVnicAttachmentsResponse{Items: page, OpcNextPage: next}, nil
}

// GetVnic implements oci.NetworkAPI.
func (c *Cloud) GetVnic(ctx context.Context, regionName string, req core.GetVnicRequest) (core.GetVnicResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "GetVnic"); err != nil {
		return core.GetVnicResponse{}, err
	}
	if _, err := c.usable(regionName); err != nil {
		return core.GetVnicResponse{}, err
	}
	v, ok := c.vnics[deref(req.VnicId)]
	if !ok {
		return core.GetVnicResponse{}, notFound("vnic")
	}
	return core.GetVnicResponse{Vnic: v}, nil
}

// ListShapes implements oci.ComputeAPI: the shapes of an availability domain.
func (c *Cloud) ListShapes(ctx context.Context, regionName string, req core.ListShapesRequest,
) (core.ListShapesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListShapes"); err != nil {
		return core.ListShapesResponse{}, err
	}
	r, err := c.usable(regionName)
	if err != nil {
		return core.ListShapesResponse{}, err
	}
	if deref(req.CompartmentId) != c.compartment {
		return core.ListShapesResponse{}, notFound("compartment")
	}
	shapes, ok := r.shapes[deref(req.AvailabilityDomain)]
	if !ok {
		return core.ListShapesResponse{}, invalid("The availability domain %q does not exist in %s",
			deref(req.AvailabilityDomain), regionName)
	}
	all := make([]core.Shape, 0, len(shapes))
	for _, name := range sortedKeys(shapes) {
		all = append(all, shapes[name])
	}
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListShapesResponse{Items: page, OpcNextPage: next}, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
