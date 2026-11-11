/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Codes the driver raises on its own.
const (
	codeUserDataTooLarge = "UserDataTooLarge"
	codeInvalidSettings  = "InvalidSettings"
	codeProjectMismatch  = "ProjectMismatch"
	codeNotOurs          = "NotOwned"
)

// launchResources are the resources of a launch, as Prepare resolved them.
type launchResources struct {
	subnet   string
	tags     []string
	policies []string
}

// resourcesOf extracts the subnet, the network tags and the resource policies
// from the prepared resources.
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
		case KindFirewalls, KindRunFirewalls:
			// A synthetic "no tag" firewall (an empty ID) adds no network tag.
			if r.ID != "" && !slices.Contains(lr.tags, r.ID) {
				lr.tags = append(lr.tags, r.ID)
			}
		case KindResourcePolicies:
			if !slices.Contains(lr.policies, r.ID) {
				lr.policies = append(lr.policies, r.ID)
			}
		}
	}
	return lr
}

// attemptKey identifies an offering of a launch for the attempt counter.
func attemptKey(req vendors.CreateRequest) string {
	return strings.Join([]string{req.IdempotencyKey, req.Zone, req.InstanceType, req.CapacityType}, "|")
}

func (d *Driver) attempt(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts[key]
}

// bumpAttempt counts the attempt up after a failure of the insert that
// is definitive: the next try of the same offering is then a new request (see
// RequestID).
func (d *Driver) bumpAttempt(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.attempts) >= maxAttempts {
		d.attempts = map[string]int{}
	}
	d.attempts[key]++
}

func (d *Driver) forgetAttempt(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.attempts, key)
}

// byKey finds the instance of an idempotency key anywhere in the project (one
// filtered aggregated list), whatever zone, type or capacity type it was launched
// with; nil when there is none.
func (d *Driver) byKey(ctx context.Context, api ComputeAPI, key string) (*vendors.Instance, error) {
	list, err := d.aggregated(ctx, api, LabelFilter(map[vendors.TagKey]string{vendors.TagIdempotencyKey: key}))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	in := newest(list)
	return ToInstance(RegionOfZone(zoneOf(in)), in), nil
}

func zoneOf(in *computepb.Instance) string { return lastSegment(in.GetZone()) }

// newest returns the most recently created instance.
func newest(list []*computepb.Instance) *computepb.Instance {
	best := list[0]
	for _, in := range list[1:] {
		if in.GetCreationTimestamp() > best.GetCreationTimestamp() {
			best = in
		}
	}
	return best
}

// aggregated reads every page of a project-wide instance list with the filter.
func (d *Driver) aggregated(ctx context.Context, api ComputeAPI, filter string) ([]*computepb.Instance, error) {
	req := &computepb.AggregatedListInstancesRequest{Project: d.project}
	if filter != "" {
		req.Filter = &filter
	}
	var out []*computepb.Instance
	for {
		res, err := api.AggregatedListInstances(ctx, req)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, k := range sortedScopes(res.GetItems()) {
			out = append(out, res.GetItems()[k].GetInstances()...)
		}
		if res.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = res.NextPageToken
	}
}

func sortedScopes(m map[string]*computepb.InstancesScopedList) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// CreateInstance implements vendors.Driver (plan 16, section 3): one
// instances.insert in the zone, after a project-wide lookup of the idempotency
// label (a replay in another zone, type or capacity type returns the original,
// and a failed create leaves the key unused). The insert carries a request ID
// that is the same for a retry of the same offering after a lost response and
// new after a definitive failure (RequestID). The insert's operation is waited
// for and its error classified; the instance is then read for its addresses.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooLarge, fmt.Errorf(
			"the user-data is %d bytes; Compute Engine accepts a metadata value of at most %d", len(req.UserData),
			MaxUserData))
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	if req.IdempotencyKey != "" {
		inst, err := d.byKey(ctx, api, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if inst != nil {
			return inst, nil
		}
	}
	key := attemptKey(req)
	spec, err := d.launchSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	spec.Epoch, spec.Attempt = d.epochID, d.attempt(key)
	insert, err := BuildInsert(spec)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	ec := errContext{machineType: req.InstanceType, zone: req.Zone}
	op, err := api.InsertInstance(ctx, insert)
	if err != nil {
		if IsAlreadyExists(err) {
			return d.existing(ctx, api, req, spec.Name)
		}
		cerr := classify(err, ec)
		if !vendors.IsClass(cerr, vendors.Retryable) {
			d.bumpAttempt(key)
		}
		return nil, cerr
	}
	if err := d.finish(ctx, api, op, ec); err != nil {
		if vendors.CodeOf(err) == CodeAlreadyExists {
			return d.existing(ctx, api, req, spec.Name)
		}
		// An operation still running at the limit, or a transport failure while
		// waiting, is unknown and keeps the request ID; a finished failure does not.
		if !(vendors.IsClass(err, vendors.Retryable) && vendors.CodeOf(err) == codeTimeout) {
			d.bumpAttempt(key)
		}
		return nil, err
	}
	in, err := api.GetInstance(ctx, &computepb.GetInstanceRequest{
		Project: d.project, Zone: req.Zone, Instance: spec.Name})
	if err != nil {
		return nil, classify(err, ec)
	}
	d.forgetAttempt(key)
	return ToInstance(req.Region, in), nil
}

// existing answers an insert that found its name taken (409): the instance is the
// one of this key when its idempotency label says so, else it is somebody else's
// and the create is Fatal.
func (d *Driver) existing(ctx context.Context, api ComputeAPI, req vendors.CreateRequest, name string,
) (*vendors.Instance, error) {
	in, err := api.GetInstance(ctx, &computepb.GetInstanceRequest{Project: d.project, Zone: req.Zone, Instance: name})
	if err != nil {
		return nil, classify(err, errContext{})
	}
	if req.IdempotencyKey != "" &&
		in.GetLabels()[TagKey(vendors.TagIdempotencyKey)] == LabelValue(req.IdempotencyKey) {
		return ToInstance(req.Region, in), nil
	}
	return nil, vendors.NewError(vendors.Fatal, CodeAlreadyExists, fmt.Errorf(
		"an instance named %s already exists in %s and is not the one of this NodeClaim", name, req.Zone))
}

// launchSpec builds the resolved launch of a CreateRequest.
func (d *Driver) launchSpec(ctx context.Context, req vendors.CreateRequest) (LaunchSpec, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	if st.Project != "" && st.Project != d.project {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeProjectMismatch, fmt.Errorf(
			"the NodeClass names the project %q, the controller targets %q", st.Project, d.project))
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	net, err := decodeNetwork(req.Settings.Network)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	lr := resourcesOf(req.Prepared)
	if lr.subnet == "" {
		// Prepare always names the subnet; a launch without one (a caller that
		// prepared only part of the resources) falls back to the project's default.
		defs, err := d.Default(ctx, KindSubnets, req.Region)
		if err != nil {
			return LaunchSpec{}, err
		}
		if len(defs) == 0 {
			return LaunchSpec{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
				fmt.Errorf("no subnet was prepared for the launch and the project has no default network"))
		}
		lr.subnet = defs[0].ID
	}
	labels, err := Labels(req.Tags, req.ExtraTags)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	root := rootDisk(st, req.InstanceType)
	if want := root.SizeGb * giB; req.Image.RootDiskBytes > want {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the root disk of %d GiB is smaller than the image's root disk of %d GiB", root.SizeGb,
			(req.Image.RootDiskBytes+giB-1)/giB))
	}
	iface := DiskInterface(req.InstanceType)
	var data []Disk
	for _, dd := range dataDisks(st) {
		disk := dataDisk(dd)
		disk.Interface = iface
		data = append(data, disk)
	}
	compact := pl.ResourcePolicy != nil && pl.ResourcePolicy.GroupPlacement != nil
	spot := req.CapacityType == vendors.CapacitySpot
	spec := LaunchSpec{
		Project: d.project, Zone: req.Zone, Name: InstanceName(req.Node.Name), MachineType: req.InstanceType,
		Spot: spot, OnHostMaintenance: MaintenanceFor(req.InstanceType, compact),
		Image: ImageURL(d.project, req.Image.Ref), Labels: labels, UserData: req.UserData,
		ExtraMetadata: st.Metadata, Root: root, Data: data, Subnetwork: lr.subnet, NetworkTags: lr.tags,
		PublicIPv4: boolOr(req.Settings.Networking.PublicIPv4, true),
		PublicIPv6: boolOr(req.Settings.Networking.PublicIPv6, false),
		NicType:    string(net.NicType), Tier1: boolOr(net.Tier1, false),
		ServiceAccount: st.ServiceAccount, ResourcePolicies: lr.policies,
		IdempotencyKey: req.IdempotencyKey, CapacityType: req.CapacityType,
	}
	if spec.Name == "" || (req.Node.Name == "" && req.Tags[vendors.TagNodeClaim] != "") {
		spec.Name = InstanceName(req.Tags[vendors.TagNodeClaim])
	}
	return spec, nil
}

// ref parses an instance reference; ok is false for a malformed one.
func (d *Driver) ref(ref vendors.InstanceRef) (region, zone, name string, ok bool) {
	return ParseRef(ref)
}

// GetInstance implements vendors.Driver.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	region, zone, name, ok := d.ref(ref)
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "the instance reference %q is not an instance of this driver", ref)
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	in, err := api.GetInstance(ctx, &computepb.GetInstanceRequest{Project: d.project, Zone: zone, Instance: name})
	if err != nil {
		cerr := classify(err, errContext{})
		if IsNotFound(cerr) || IsNotFound(err) {
			return nil, vendors.NewError(vendors.NotFound, vendors.CodeOf(cerr), fmt.Errorf("instance %s does not exist: %w", ref, err))
		}
		return nil, cerr
	}
	return ToInstance(region, in), nil
}

// FindInstance implements vendors.Driver: the newest instance of the region with
// all the tags.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string) (*vendors.Instance, error,
) {
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	list, err := d.aggregated(ctx, api, LabelFilter(tags))
	if err != nil {
		return nil, err
	}
	list = slices.DeleteFunc(list, func(in *computepb.Instance) bool { return RegionOfZone(zoneOf(in)) != region })
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance in %s has the tags %v", region, tags)
	}
	return ToInstance(region, newest(list)), nil
}

// ListInstances implements vendors.Driver: every instance of the regions with
// the tags, in every status (a stopped instance is terminating and stays listed,
// so that the garbage collector deletes it).
func (d *Driver) ListInstances(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	list, err := d.aggregated(ctx, api, LabelFilter(tags))
	if err != nil {
		return nil, fmt.Errorf("listing the instances: %w", err)
	}
	var out []*vendors.Instance
	for _, in := range list {
		region := RegionOfZone(zoneOf(in))
		if slices.Contains(regions, region) {
			out = append(out, ToInstance(region, in))
		}
	}
	return out, nil
}

// DeleteInstance implements vendors.Driver: an instance that does not exist is
// success. An instance that does not carry this cluster's label is not ours (a
// wrong or tampered reference must not delete a foreign instance: GCP IAM
// conditions cannot test labels, so this check is the only wall) and is refused
// with a Fatal error. The disks go with the instance (autoDelete).
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	_, zone, name, ok := d.ref(ref)
	if !ok {
		return nil
	}
	api, err := d.compute(ctx)
	if err != nil {
		return err
	}
	in, err := api.GetInstance(ctx, &computepb.GetInstanceRequest{Project: d.project, Zone: zone, Instance: name})
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return classify(err, errContext{})
	}
	if d.cfg.ClusterName == "" || in.GetLabels()[TagKey(vendors.TagCluster)] != LabelValue(d.cfg.ClusterName) {
		return vendors.NewError(vendors.Fatal, codeNotOurs, fmt.Errorf(
			"the instance %s does not carry the label of cluster %q", ref, d.cfg.ClusterName))
	}
	op, err := api.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: d.project, Zone: zone, Instance: name})
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return classify(err, errContext{})
	}
	// The delete is accepted: wait once for the operation so that its error shows,
	// but a slow one is not a failure of the call.
	done, err := d.waitOp(ctx, api, op, d.waitTimeout)
	if err != nil {
		return err
	}
	if done.GetStatus() != computepb.Operation_DONE {
		return nil
	}
	if err := classifyOperation(done, errContext{}); err != nil && !vendors.IsClass(err, vendors.NotFound) {
		return err
	}
	return nil
}
