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
	"net/netip"
	"slices"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// Instance statuses the fake sets.
const (
	statusProvisioning = "PROVISIONING"
	statusRunning      = "RUNNING"
	statusStopping     = "STOPPING"
)

// ----- instances -----

// metricsFor lists the quota metrics an instance consumes. UNVERIFIED (the plan's
// spike decides): the fake assumes an instance consumes CPUS plus its series'
// <SERIES>_CPUS when the region has that limit (e2 and n1 only CPUS), plus
// CPUS_ALL_REGIONS when the project has it; a Spot VM consumes only
// PREEMPTIBLE_CPUS when the region's limit for it is above 0, else the
// on-demand metrics.
func (c *Compute) metricsFor(region string, in *computepb.Instance) []string {
	spot := in.GetScheduling().GetProvisioningModel() == modelSpot
	if spot && c.limits[region+"|PREEMPTIBLE_CPUS"] > 0 {
		return []string{"PREEMPTIBLE_CPUS"}
	}
	out := []string{"CPUS"}
	series, _, _ := strings.Cut(last(in.GetMachineType()), "-")
	if series != "e2" && series != "n1" {
		if _, ok := c.limits[region+"|"+strings.ToUpper(series)+"_CPUS"]; ok {
			out = append(out, strings.ToUpper(series)+"_CPUS")
		}
	}
	if _, ok := c.limits["|CPUS_ALL_REGIONS"]; ok {
		out = append(out, "CPUS_ALL_REGIONS")
	}
	return out
}

// counts says whether the instance's status consumes CPU quota (stopped
// instances do not).
func counts(in *computepb.Instance) bool {
	switch in.GetStatus() {
	case "STOPPED", "TERMINATED", "SUSPENDED":
		return false
	}
	return true
}

func (c *Compute) vcpus(in *computepb.Instance) float64 {
	zone := last(in.GetZone())
	if mt := c.types[zone][last(in.GetMachineType())]; mt != nil {
		return float64(mt.GetGuestCpus())
	}
	return 0
}

// usage is the usage of a metric in the region (region "" for the project).
func (c *Compute) usage(region, metric string) float64 {
	u := c.foreign[region+"|"+metric]
	for _, in := range c.instances {
		r := regionOf(last(in.GetZone()))
		if !counts(in) || (region != "" && r != region) {
			continue
		}
		for _, m := range c.metricsFor(r, in) {
			if m == metric {
				u += c.vcpus(in)
			}
		}
	}
	return u
}

func (c *Compute) quotas(region string) []*computepb.Quota {
	var out []*computepb.Quota
	for _, k := range sortedKeys(c.limits) {
		r, m, _ := strings.Cut(k, "|")
		if r != region {
			continue
		}
		out = append(out, &computepb.Quota{Metric: new(m), Limit: new(c.limits[k]), Usage: new(c.usage(region, m))})
	}
	return out
}

// quotaFailure returns the QUOTA_EXCEEDED failure the new instance would cause.
func (c *Compute) quotaFailure(region string, in *computepb.Instance) *opFailure {
	n := c.vcpus(in)
	for _, m := range c.metricsFor(region, in) {
		scope := region
		if m == "CPUS_ALL_REGIONS" {
			scope = ""
		}
		limit := c.limits[scope+"|"+m]
		if c.usage(scope, m)+n > limit {
			msg := fmt.Sprintf("Quota '%s' exceeded.  Limit: %.1f in region %s.", m, limit, region)
			if scope == "" {
				msg = fmt.Sprintf("Quota '%s' exceeded.  Limit: %.1f.", m, limit)
			}
			return &opFailure{"QUOTA_EXCEEDED", msg, http.StatusForbidden}
		}
	}
	return nil
}

// validateInsert applies the synchronous validation of instances.insert: what
// the API refuses in the HTTP response, before any operation exists.
func (c *Compute) validateInsert(r *computepb.InsertInstanceRequest) (
	in *computepb.Instance, machine *computepb.MachineType, sub *computepb.Subnetwork, err error,
) {
	zone := r.GetZone()
	region := regionOf(zone)
	if c.zones[zone] == nil {
		return nil, nil, nil, notFound("projects/" + r.GetProject() + "/zones/" + zone)
	}
	in = proto.Clone(r.GetInstanceResource()).(*computepb.Instance)
	name := in.GetName()
	if name == "" {
		return nil, nil, nil,
			invalid("Invalid value for field 'resource.name': ''. Must be a match of regex '[a-z]([-a-z0-9]{0,61}[a-z0-9])?'")
	}
	mt := last(in.GetMachineType())
	machine = c.types[zone][mt]
	if machine == nil {
		return nil, nil, nil, notFound("projects/" + r.GetProject() + "/zones/" + zone + "/machineTypes/" + mt)
	}
	if _, exists := c.instances[zone+"/"+name]; exists {
		return nil, nil, nil, Error(http.StatusConflict, "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/zones/%s/instances/%s' already exists", r.GetProject(), zone, name))
	}
	if len(in.GetDisks()) == 0 || in.GetDisks()[0].GetInitializeParams().GetSourceImage() == "" {
		return nil, nil, nil,
			invalid("Invalid value for field 'resource.disks[0].initializeParams.sourceImage': ''. Boot disk image is required.")
	}
	img, ferr := c.findImage(in.GetDisks()[0].GetInitializeParams().GetSourceImage())
	if ferr != nil {
		return nil, nil, nil, ferr
	}
	if img.GetArchitecture() != "" && img.GetArchitecture() != machine.GetArchitecture() {
		return nil, nil, nil, invalid("The image architecture %s does not match the machine type %s architecture %s",
			img.GetArchitecture(), mt, machine.GetArchitecture())
	}
	if in.GetScheduling().GetProvisioningModel() == modelSpot && in.GetScheduling().GetOnHostMaintenance() == "MIGRATE" {
		return nil, nil, nil, invalid("Spot VMs cannot be live migrated: onHostMaintenance must be TERMINATE")
	}
	if len(in.GetNetworkInterfaces()) == 0 {
		return nil, nil, nil, invalid("At least one network interface is required")
	}
	nic := in.GetNetworkInterfaces()[0]
	subRegion := region
	if i := strings.Index(nic.GetSubnetwork(), "regions/"); i >= 0 {
		subRegion, _, _ = strings.Cut(nic.GetSubnetwork()[i+len("regions/"):], "/")
	}
	sub = c.subnets[subRegion+"/"+last(nic.GetSubnetwork())]
	if sub == nil {
		return nil, nil, nil, notFound(nic.GetSubnetwork())
	}
	if subRegion != region {
		return nil, nil, nil, invalid("Subnetwork %s is not in region %s", nic.GetSubnetwork(), region)
	}
	if len(nic.GetIpv6AccessConfigs()) > 0 && sub.GetStackType() != "IPV4_IPV6" {
		return nil, nil, nil, invalid("Subnetwork %s has no IPv6 range", sub.GetName())
	}
	for _, p := range in.GetResourcePolicies() {
		if c.policies[region+"/"+last(p)] == nil {
			return nil, nil, nil, notFound(p)
		}
	}
	return in, machine, sub, nil
}

func (c *Compute) InsertInstance(_ context.Context, r *computepb.InsertInstanceRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("InsertInstance"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	zone := r.GetZone()
	if op := c.replay("InsertInstance", "zones/"+zone, r.RequestId); op != nil {
		return op, nil
	}
	region := regionOf(zone)
	in, _, sub, err := c.validateInsert(r)
	if err != nil {
		return nil, err
	}
	name, mt := in.GetName(), last(in.GetMachineType())
	nic := in.GetNetworkInterfaces()[0]
	scope := "zones/" + zone
	target := scope + "/instances/" + name
	in.Zone = new(c.url(scope))
	var failed *opFailure
	switch {
	case c.peekOpFailure("InsertInstance"):
		failed = c.takeOpFailure("InsertInstance")
	case c.orgPolicy:
		failed = &opFailure{"CONDITION_NOT_MET",
			"Constraint constraints/compute.trustedImageProjects violates the organization policy.",
			http.StatusPreconditionFailed}
	case c.stockouts[stockKey(zone, mt, in.GetScheduling().GetProvisioningModel() == modelSpot)]:
		failed = &opFailure{"ZONE_RESOURCE_POOL_EXHAUSTED",
			fmt.Sprintf("The zone 'projects/%s/zones/%s' does not have enough resources available to fulfill the request.",
				r.GetProject(), zone), http.StatusServiceUnavailable}
	default:
		failed = c.quotaFailure(region, in)
	}
	if failed != nil {
		op := c.newOp("insert", scope, target, failed, nil)
		c.remember("InsertInstance", scope, r.RequestId, op)
		return proto.Clone(op).(*computepb.Operation), nil
	}
	ip, err := c.allocateIP(region, sub)
	if err != nil {
		failed = &opFailure{"IP_SPACE_EXHAUSTED",
			"Instance cannot be created: IP space exhausted in subnetwork " + sub.GetName(), http.StatusBadRequest}
		op := c.newOp("insert", scope, target, failed, nil)
		c.remember("InsertInstance", scope, r.RequestId, op)
		return proto.Clone(op).(*computepb.Operation), nil
	}

	c.seq++
	in.Id = new(c.seq)
	in.MachineType = new(c.url(scope + "/machineTypes/" + mt))
	in.SelfLink = new(c.url(target))
	in.Status = new(statusProvisioning)
	in.CreationTimestamp = new(c.timestamp())
	in.Fingerprint = new(fmt.Sprintf("fp-%d", c.seq))
	nic.NetworkIP = new(ip)
	nic.Network = sub.Network
	for i, ac := range nic.GetAccessConfigs() {
		ac.NatIP = new(fmt.Sprintf("34.%d.%d.%d", c.seq%200, i, c.seq%250+1))
	}
	for _, ac := range nic.GetIpv6AccessConfigs() {
		ac.ExternalIpv6 = new(fmt.Sprintf("2600:1900:4000:%x::", c.seq))
		nic.Ipv6Address = new(fmt.Sprintf("2600:1900:4000:%x::", c.seq))
	}
	// Disks get their device-visible fields; autoDelete is kept as sent.
	for i, d := range in.GetDisks() {
		d.Index = new(int32(i))
		if d.GetDeviceName() == "" {
			d.DeviceName = new(fmt.Sprintf("persistent-disk-%d", i))
		}
	}
	key := zone + "/" + name
	c.instances[key] = in
	op := c.newOp("insert", scope, target, nil, func() {
		if cur := c.instances[key]; cur != nil && cur.GetStatus() == statusProvisioning {
			cur.Status = new(statusRunning)
		}
	})
	c.remember("InsertInstance", scope, r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

// allocateIP returns the lowest free address of the subnet, skipping the first
// two (network, gateway) and the last two (broadcast, reserved).
func (c *Compute) allocateIP(region string, sub *computepb.Subnetwork) (string, error) {
	key := region + "/" + sub.GetName()
	p, err := netip.ParsePrefix(sub.GetIpCidrRange())
	if err != nil {
		return "", err
	}
	used := c.usedIPs[key]
	if used == nil {
		used = map[string]bool{}
		c.usedIPs[key] = used
	}
	a := p.Masked().Addr().Next().Next()
	for ; p.Contains(a); a = a.Next() {
		if !p.Contains(a.Next()) || !p.Contains(a.Next().Next()) {
			break
		}
		if !used[a.String()] {
			used[a.String()] = true
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("no free address in %s", p)
}

func (c *Compute) DeleteInstance(_ context.Context, r *computepb.DeleteInstanceRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteInstance"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	scope := "zones/" + r.GetZone()
	if op := c.replay("DeleteInstance", scope, r.RequestId); op != nil {
		return op, nil
	}
	key := r.GetZone() + "/" + r.GetInstance()
	in := c.instances[key]
	if in == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/zones/%s/instances/%s", r.GetProject(), r.GetZone(), r.GetInstance()))
	}
	var failed *opFailure
	if c.takeOpFailure("DeleteInstance") != nil {
		failed = &opFailure{"INTERNAL_ERROR", "injected failure", http.StatusInternalServerError}
	}
	if failed == nil {
		in.Status = new(statusStopping)
	}
	op := c.newOp("delete", scope, scope+"/instances/"+r.GetInstance(), failed, func() {
		if cur := c.instances[key]; cur != nil {
			c.releaseIPs(cur)
			delete(c.instances, key)
		}
	})
	c.remember("DeleteInstance", scope, r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

func (c *Compute) releaseIPs(in *computepb.Instance) {
	region := regionOf(last(in.GetZone()))
	for _, nic := range in.GetNetworkInterfaces() {
		delete(c.usedIPs[region+"/"+last(nic.GetSubnetwork())], nic.GetNetworkIP())
	}
}

func (c *Compute) GetInstance(_ context.Context, r *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetInstance"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	in := c.instances[r.GetZone()+"/"+r.GetInstance()]
	if in == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/zones/%s/instances/%s", r.GetProject(), r.GetZone(), r.GetInstance()))
	}
	return proto.Clone(in).(*computepb.Instance), nil
}

func instanceValues(in *computepb.Instance) func(string) ([]string, bool) {
	return func(f string) ([]string, bool) {
		switch {
		case f == fieldName:
			return []string{in.GetName()}, true
		case f == fieldStatus:
			return []string{in.GetStatus()}, true
		case f == "tags.items":
			return in.GetTags().GetItems(), true
		case strings.HasPrefix(f, "labels."):
			if v, ok := in.GetLabels()[strings.TrimPrefix(f, "labels.")]; ok {
				return []string{v}, true
			}
			return nil, true
		}
		return nil, false
	}
}

// filtered returns the instances (sorted by zone and name) of the zones that
// pass the filter.
func (c *Compute) filtered(inZone func(zone string) bool, filter *string) ([]*computepb.Instance, error) {
	var out []*computepb.Instance
	for _, k := range sortedKeys(c.instances) {
		in := c.instances[k]
		if !inZone(last(in.GetZone())) {
			continue
		}
		ok, err := matchFilter(filter, instanceValues(in))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, in)
		}
	}
	return out, nil
}

func (c *Compute) ListInstances(_ context.Context, r *computepb.ListInstancesRequest) (*computepb.InstanceList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListInstances"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if c.zones[r.GetZone()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/zones/" + r.GetZone())
	}
	all, err := c.filtered(func(z string) bool { return z == r.GetZone() }, r.Filter)
	if err != nil {
		return nil, err
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.InstanceList{NextPageToken: next}
	for _, in := range all[lo:hi] {
		out.Items = append(out.Items, proto.Clone(in).(*computepb.Instance))
	}
	return out, nil
}

func (c *Compute) AggregatedListInstances(
	_ context.Context, r *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("AggregatedListInstances"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	all, err := c.filtered(func(z string) bool { return !c.unreach[z] }, r.Filter)
	if err != nil {
		return nil, err
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.InstanceAggregatedList{Items: map[string]*computepb.InstancesScopedList{}, NextPageToken: next}
	for _, in := range all[lo:hi] {
		k := "zones/" + last(in.GetZone())
		if out.Items[k] == nil {
			out.Items[k] = &computepb.InstancesScopedList{}
		}
		out.Items[k].Instances = append(out.Items[k].Instances, proto.Clone(in).(*computepb.Instance))
	}
	for _, z := range sortedKeys(c.unreach) {
		if c.unreach[z] {
			out.Unreachables = append(out.Unreachables, "zones/"+z)
		}
	}
	return out, nil
}

// SetStatus sets the status of an instance (STOPPED, TERMINATED, SUSPENDED,
// REPAIRING, STAGING...): a state change made outside SCarpenter.
func (c *Compute) SetStatus(zone, name, status string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := c.instances[zone+"/"+name]
	if in == nil {
		return false
	}
	in.Status = new(status)
	return true
}

// Instances returns a snapshot of every instance, sorted by zone and name.
func (c *Compute) Instances() []*computepb.Instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*computepb.Instance, 0, len(c.instances))
	for _, k := range sortedKeys(c.instances) {
		out = append(out, proto.Clone(c.instances[k]).(*computepb.Instance))
	}
	return out
}

// Usage returns the computed usage of a quota metric in the region ("" for a
// project-wide metric): the foreign usage plus the instances.
func (c *Compute) Usage(region, metric string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage(region, metric)
}

// ----- facts -----

func (c *Compute) ListZones(_ context.Context, r *computepb.ListZonesRequest) (*computepb.ZoneList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListZones"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	var all []*computepb.Zone
	for _, k := range sortedKeys(c.zones) {
		z := c.zones[k]
		ok, err := matchFilter(r.Filter, func(f string) ([]string, bool) {
			switch f {
			case fieldName:
				return []string{z.GetName()}, true
			case fieldStatus:
				return []string{z.GetStatus()}, true
			case "region":
				return []string{z.GetRegion()}, true
			}
			return nil, false
		})
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, z)
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.ZoneList{NextPageToken: next}
	for _, z := range all[lo:hi] {
		out.Items = append(out.Items, proto.Clone(z).(*computepb.Zone))
	}
	return out, nil
}

func (c *Compute) regionWithQuotas(name string) *computepb.Region {
	r := proto.Clone(c.regions[name]).(*computepb.Region)
	r.Quotas = c.quotas(name)
	for _, k := range sortedKeys(c.zones) {
		if regionOf(k) == name {
			r.Zones = append(r.Zones, c.url("zones/"+k))
		}
	}
	return r
}

func (c *Compute) GetRegion(_ context.Context, r *computepb.GetRegionRequest) (*computepb.Region, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetRegion"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if c.regions[r.GetRegion()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/regions/" + r.GetRegion())
	}
	return c.regionWithQuotas(r.GetRegion()), nil
}

func (c *Compute) ListRegions(_ context.Context, r *computepb.ListRegionsRequest) (*computepb.RegionList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListRegions"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	var all []string
	for _, k := range sortedKeys(c.regions) {
		ok, err := matchFilter(r.Filter, func(f string) ([]string, bool) {
			switch f {
			case fieldName:
				return []string{k}, true
			case fieldStatus:
				return []string{c.regions[k].GetStatus()}, true
			}
			return nil, false
		})
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, k)
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.RegionList{NextPageToken: next}
	for _, k := range all[lo:hi] {
		out.Items = append(out.Items, c.regionWithQuotas(k))
	}
	return out, nil
}

func (c *Compute) ListMachineTypes(
	_ context.Context, r *computepb.ListMachineTypesRequest,
) (*computepb.MachineTypeList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListMachineTypes"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	types, ok := c.types[r.GetZone()]
	if !ok {
		return nil, notFound("projects/" + r.GetProject() + "/zones/" + r.GetZone())
	}
	var all []*computepb.MachineType
	for _, k := range sortedKeys(types) {
		if ok, err := matchFilter(r.Filter, nameValues(k)); err != nil {
			return nil, err
		} else if ok {
			all = append(all, types[k])
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.MachineTypeList{NextPageToken: next}
	for _, mt := range all[lo:hi] {
		out.Items = append(out.Items, proto.Clone(mt).(*computepb.MachineType))
	}
	return out, nil
}

func (c *Compute) GetProject(_ context.Context, r *computepb.GetProjectRequest) (*computepb.Project, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetProject"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	return &computepb.Project{Name: new(c.project), Quotas: c.quotas("")}, nil
}

// ----- images -----

func (c *Compute) findImage(ref string) (*computepb.Image, error) {
	i := strings.Index(ref, "projects/")
	if i < 0 {
		return nil, invalid("Invalid value for field 'sourceImage': '%s'", ref)
	}
	parts := strings.Split(ref[i:], "/") // projects/<p>/global/images/<n> or .../images/family/<f>
	if len(parts) < 5 || parts[2] != "global" || parts[3] != "images" {
		return nil, invalid("Invalid value for field 'sourceImage': '%s'", ref)
	}
	project := parts[1]
	if len(parts) == 6 && parts[4] == "family" {
		name, ok := c.families[project+"/"+parts[5]]
		if !ok {
			return nil, notFound(ref)
		}
		return c.images[project+"/"+name], nil
	}
	img := c.images[project+"/"+parts[4]]
	if img == nil {
		return nil, notFound(ref)
	}
	return img, nil
}

func (c *Compute) GetImage(_ context.Context, r *computepb.GetImageRequest) (*computepb.Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetImage"); err != nil {
		return nil, err
	}
	img := c.images[r.GetProject()+"/"+r.GetImage()]
	if img == nil {
		return nil, notFound("projects/" + r.GetProject() + "/global/images/" + r.GetImage())
	}
	return proto.Clone(img).(*computepb.Image), nil
}

func (c *Compute) GetImageFromFamily(
	_ context.Context, r *computepb.GetFromFamilyImageRequest,
) (*computepb.Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetImageFromFamily"); err != nil {
		return nil, err
	}
	name, ok := c.families[r.GetProject()+"/"+r.GetFamily()]
	if !ok {
		return nil, notFound("projects/" + r.GetProject() + "/global/images/family/" + r.GetFamily())
	}
	return proto.Clone(c.images[r.GetProject()+"/"+name]).(*computepb.Image), nil
}

func imageValues(img *computepb.Image) func(string) ([]string, bool) {
	return func(f string) ([]string, bool) {
		switch {
		case f == fieldName:
			return []string{img.GetName()}, true
		case f == fieldStatus:
			return []string{img.GetStatus()}, true
		case f == "family":
			return []string{img.GetFamily()}, true
		case f == "architecture":
			return []string{img.GetArchitecture()}, true
		case f == "deprecated.state":
			return []string{img.GetDeprecated().GetState()}, true
		case strings.HasPrefix(f, "labels."):
			if v, ok := img.GetLabels()[strings.TrimPrefix(f, "labels.")]; ok {
				return []string{v}, true
			}
			return nil, true
		}
		return nil, false
	}
}

// ListImages lists the images of the project that are not publicly shared (a
// publicly shared image never appears in a list), newest first with
// orderBy=creationTimestamp desc and by name otherwise.
func (c *Compute) ListImages(_ context.Context, r *computepb.ListImagesRequest) (*computepb.ImageList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListImages"); err != nil {
		return nil, err
	}
	var all []*computepb.Image
	for _, k := range sortedKeys(c.images) {
		if !strings.HasPrefix(k, r.GetProject()+"/") || c.public[k] {
			continue
		}
		ok, err := matchFilter(r.Filter, imageValues(c.images[k]))
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, c.images[k])
		}
	}
	if r.GetOrderBy() == "creationTimestamp desc" {
		slices.SortStableFunc(all, func(a, b *computepb.Image) int {
			return strings.Compare(b.GetCreationTimestamp(), a.GetCreationTimestamp())
		})
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.ImageList{NextPageToken: next}
	for _, img := range all[lo:hi] {
		out.Items = append(out.Items, proto.Clone(img).(*computepb.Image))
	}
	return out, nil
}
