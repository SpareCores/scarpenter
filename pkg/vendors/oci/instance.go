/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// launchResources are the resources of a launch, as Prepare resolved them.
type launchResources struct {
	subnet string
	nsgs   []string
}

// resourcesOf extracts the subnet and the network security groups (the
// NodeClass's and the run's) from the prepared resources.
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
		case KindNetworkSecurityGroups, KindRunNetworkSecurityGroup:
			if r.ID != "" && !slices.Contains(lr.nsgs, r.ID) {
				lr.nsgs = append(lr.nsgs, r.ID)
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

// bumpAttempt counts the attempt up after a definitive failure of the launch: the
// next try of the same offering is then a new request (see RetryToken).
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

// listRegion reads the compartment's instances of the region, every page, and
// drops the TERMINATED ones (OCI keeps them listed for about twelve hours). OCI
// has no tag filter: callers filter client-side; displayName narrows server-side.
func (d *Driver) listRegion(ctx context.Context, region, displayName string) ([]core.Instance, error) {
	req := core.ListInstancesRequest{CompartmentId: &d.compartment}
	if displayName != "" {
		req.DisplayName = &displayName
	}
	var out []core.Instance
	for {
		res, err := d.api.Compute.ListInstances(ctx, region, req)
		if err != nil {
			return nil, fmt.Errorf("listing the instances of %s: %w", region, classify(err, kindRead))
		}
		for _, in := range res.Items {
			if in.LifecycleState != core.InstanceLifecycleStateTerminated {
				out = append(out, in)
			}
		}
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			return out, nil
		}
		req.Page = res.OpcNextPage
	}
}

// hasTags reports whether the instance carries every logical tag.
func hasTags(in core.Instance, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		if got, ok := in.FreeformTags[TagKey(k)]; !ok || got != v {
			return false
		}
	}
	return true
}

// newest returns the most recently created instance of a non-empty list.
func newest(list []core.Instance) core.Instance {
	best := list[0]
	for _, in := range list[1:] {
		if in.TimeCreated != nil && (best.TimeCreated == nil || in.TimeCreated.After(best.TimeCreated.Time)) {
			best = in
		}
	}
	return best
}

// matching lists the instances of the region that carry all the tags.
func (d *Driver) matching(ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]core.Instance, error) {
	list, err := d.listRegion(ctx, region, tags[vendors.TagNodeClaim])
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(in core.Instance) bool { return !hasTags(in, tags) }), nil
}

// byKey finds the instance of an idempotency key in the region (one listing of
// the compartment, client-side filter), whatever availability domain, shape or
// capacity type it was launched with; nil when there is none. It is the lookup
// before every create: the opc-retry-token protects one request against a lost
// response but cannot give the contract (plan 25, section 5).
func (d *Driver) byKey(ctx context.Context, region, key string) (*vendors.Instance, error) {
	list, err := d.matching(ctx, region, map[vendors.TagKey]string{vendors.TagIdempotencyKey: key})
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return ToInstance(region, newest(list)), nil
}

// CreateInstance implements vendors.Driver (plan 25, section 5): the pre-launch
// checks against the tenancy's shape table (no call for a type that cannot
// launch), a lookup of the idempotency key in the region (a replay in another
// AD, shape or capacity type returns the original, and a failed create leaves
// the key unused), then one LaunchInstance without a retry policy. The call
// returns the PROVISIONING instance; the node that never registers is the core's
// 15 minute rule.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserData, fmt.Errorf(
			"the user-data is %d bytes; the driver accepts at most %d", len(req.UserData), MaxUserData))
	}
	if ok, err := d.subscribed(ctx, req.Region); err != nil {
		return nil, err
	} else if !ok {
		return nil, vendors.NewError(vendors.Unsupported, codeUnknownReg,
			fmt.Errorf("the region %q is not subscribed by the tenancy", req.Region))
	}
	if req.IdempotencyKey != "" {
		inst, err := d.byKey(ctx, req.Region, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if inst != nil {
			return inst, nil
		}
	}
	t, err := ParseType(req.InstanceType)
	if err != nil {
		return nil, err
	}
	ad, err := d.adOf(ctx, req.Region, req.Zone)
	if err != nil {
		return nil, err
	}
	shapes, err := d.shapesIn(ctx, req.Region, ad.Full)
	if err != nil {
		return nil, err
	}
	if err := t.Check(shapes, ad.Full); err != nil {
		return nil, err
	}
	if req.CapacityType == vendors.CapacitySpot && t.Sized && !IsFlexibleName(t.Shape) {
		return nil, vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
			fmt.Errorf("%s is not a .Flex shape; preemptible capacity is for flexible VM shapes", t.Shape))
	}
	key := attemptKey(req)
	spec, err := d.launchSpec(req, t, ad)
	if err != nil {
		return nil, err
	}
	spec.RetryToken = RetryToken(req.IdempotencyKey, req.Region, ad.Full, t, req.CapacityType, d.epochID, d.attempt(key))
	launch, err := BuildLaunch(spec)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	res, err := d.api.Compute.LaunchInstance(ctx, req.Region, launch)
	if err != nil {
		return d.launchFailed(ctx, req, key, err)
	}
	d.forgetAttempt(key)
	return ToInstance(req.Region, res.Instance), nil
}

// launchFailed classifies the error of the launch call and keeps the attempt
// counter: a definitive failure counts the attempt up, so the next try of the
// same offering is a new request; a transient one (a lost response, a timeout)
// keeps the token, so that a replay of the same request is the same request. A
// token the service says another request used is resolved by the lookup of the
// key: the instance of this key is the answer, else the create is Fatal.
func (d *Driver) launchFailed(ctx context.Context, req vendors.CreateRequest, key string, err error,
) (*vendors.Instance, error) {
	if isRetryTokenConflict(err) {
		if req.IdempotencyKey != "" {
			if inst, lerr := d.byKey(ctx, req.Region, req.IdempotencyKey); lerr == nil && inst != nil {
				d.forgetAttempt(key)
				return inst, nil
			}
		}
		d.bumpAttempt(key)
		return nil, vendors.NewError(vendors.Fatal, codeTokenClash,
			fmt.Errorf("the retry token of the launch was used by another request: %w", err))
	}
	cerr := classify(err, kindLaunch)
	if !vendors.IsClass(cerr, vendors.Retryable) {
		d.bumpAttempt(key)
	}
	return nil, cerr
}

// launchSpec builds the resolved launch of a CreateRequest.
func (d *Driver) launchSpec(req vendors.CreateRequest, t InstanceType, ad availabilityDomain) (LaunchSpec, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	lr := resourcesOf(req.Prepared)
	if lr.subnet == "" {
		return LaunchSpec{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no subnet was prepared for the launch"))
	}
	tags, err := FreeformTags(req.Tags, req.ExtraTags)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	if want := st.Storage.Root.SizeGb * giB; req.Image.RootDiskBytes > want {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeSettings, fmt.Errorf(
			"the root volume of %d GiB is smaller than the image's root disk of %d GiB", st.Storage.Root.SizeGb,
			(req.Image.RootDiskBytes+giB-1)/giB))
	}
	name := req.Tags[vendors.TagNodeClaim]
	if name == "" {
		name = req.Node.Name
	}
	if name == "" {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeSettings, fmt.Errorf("the launch has no NodeClaim name"))
	}
	return LaunchSpec{
		CompartmentID: d.compartment, AvailabilityDomain: ad.Full, Type: t, DisplayName: name,
		Spot: req.CapacityType == vendors.CapacitySpot, Arm64: req.Arch == "arm64", ImageID: req.Image.Ref,
		Root: st.Storage.Root, SubnetID: lr.subnet, NSGIDs: lr.nsgs,
		PublicIPv4: boolOr(req.Settings.Networking.PublicIPv4, true),
		PublicIPv6: boolOr(req.Settings.Networking.PublicIPv6, false),
		Tags:       tags, UserData: req.UserData, ExtraMetadata: st.Metadata, IdempotencyKey: req.IdempotencyKey,
	}, nil
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// addresses reads the addresses of an instance: the private and public IPv4 and
// the IPv6 addresses of its attached VNICs, in a stable order (primary first).
// The serving certificate approver checks the node's IP SANs against them.
func (d *Driver) addresses(ctx context.Context, region string, in core.Instance) ([]string, error) {
	req := core.ListVnicAttachmentsRequest{CompartmentId: in.CompartmentId, InstanceId: in.Id}
	if req.CompartmentId == nil {
		req.CompartmentId = &d.compartment
	}
	var attachments []core.VnicAttachment
	for {
		res, err := d.api.Compute.ListVnicAttachments(ctx, region, req)
		if err != nil {
			return nil, classify(err, kindRead)
		}
		attachments = append(attachments, res.Items...)
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			break
		}
		req.Page = res.OpcNextPage
	}
	slices.SortStableFunc(attachments, func(a, b core.VnicAttachment) int { return deref(a.NicIndex) - deref(b.NicIndex) })
	var out []string
	add := func(a string) {
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	for _, a := range attachments {
		if a.LifecycleState != core.VnicAttachmentLifecycleStateAttached || a.VnicId == nil {
			continue
		}
		res, err := d.api.Network.GetVnic(ctx, region, core.GetVnicRequest{VnicId: a.VnicId})
		if isNotFound(err) {
			// Detached and deleted since the listing: the instance still exists.
			continue
		}
		if err != nil {
			return nil, classify(err, kindRead)
		}
		add(deref(res.PrivateIp))
		add(deref(res.PublicIp))
		for _, v6 := range res.Ipv6Addresses {
			add(v6)
		}
	}
	return out, nil
}

// resolveRef parses a reference: a region
// that is not subscribed or an ID that is not an instance OCID is an instance
// that does not exist (ok false), never an error.
func (d *Driver) resolveRef(ctx context.Context, ref vendors.InstanceRef) (region, ocid string, ok bool, err error) {
	region, ocid, ok = ParseRef(ref)
	if !ok {
		return "", "", false, nil
	}
	sub, err := d.subscribed(ctx, region)
	if err != nil || !sub {
		return "", "", false, err
	}
	return region, ocid, true, nil
}

// GetInstance implements vendors.Driver: the instance, also when terminated, with
// its addresses.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	region, ocid, ok, err := d.resolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "the instance reference %q is not an instance of this driver", ref)
	}
	res, err := d.api.Compute.GetInstance(ctx, region, core.GetInstanceRequest{InstanceId: &ocid})
	if err != nil {
		cerr := classify(err, kindRead)
		if isNotFound(cerr) {
			return nil, vendors.NewError(vendors.NotFound, vendors.CodeOf(cerr),
				fmt.Errorf("instance %s does not exist: %w", ref, err))
		}
		return nil, cerr
	}
	inst := ToInstance(region, res.Instance)
	if inst.State != vendors.StateTerminated {
		if inst.Addresses, err = d.addresses(ctx, region, res.Instance); err != nil {
			return nil, err
		}
	}
	return inst, nil
}

// FindInstance implements vendors.Driver: the newest instance of the region with
// all the tags; NotFound when there is none, also for a region the tenancy has
// not subscribed.
func (d *Driver) FindInstance(ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	if ok, err := d.subscribed(ctx, region); err != nil {
		return nil, err
	} else if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "the region %q is not subscribed by the tenancy", region)
	}
	list, err := d.matching(ctx, region, tags)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance in %s has the tags %v", region, tags)
	}
	return ToInstance(region, newest(list)), nil
}

// ListInstances implements vendors.Driver: every instance of the regions with the
// tags except TERMINATED ones; a stopped one is terminating and stays listed, so
// that the garbage collector deletes it. A region the tenancy has not
// subscribed has none.
func (d *Driver) ListInstances(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	var out []*vendors.Instance
	for _, region := range regions {
		ok, err := d.subscribed(ctx, region)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		list, err := d.matching(ctx, region, tags)
		if err != nil {
			return nil, err
		}
		for _, in := range list {
			out = append(out, ToInstance(region, in))
		}
	}
	return out, nil
}

// DeleteInstance implements vendors.Driver: an instance that does not exist is
// success, and so is one that is terminating or terminated already. An instance
// that does not carry this cluster's tag is not ours (a wrong or tampered
// reference must not delete a foreign instance: OCI policy conditions cannot test
// freeform tags on every verb, so this check and the compartment are the walls)
// and is refused with a Fatal error. The boot volume goes with the instance.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	region, ocid, ok, err := d.resolveRef(ctx, ref)
	if err != nil || !ok {
		return err
	}
	res, err := d.api.Compute.GetInstance(ctx, region, core.GetInstanceRequest{InstanceId: &ocid})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return classify(err, kindRead)
	}
	in := res.Instance
	if d.cfg.ClusterName == "" || in.FreeformTags[TagKey(vendors.TagCluster)] != d.cfg.ClusterName {
		return vendors.NewError(vendors.Fatal, codeNotOurs, fmt.Errorf(
			"the instance %s does not carry the tag of cluster %q", ref, d.cfg.ClusterName))
	}
	if s := StateOf(in.LifecycleState); s == vendors.StateTerminated ||
		in.LifecycleState == core.InstanceLifecycleStateTerminating {
		return nil
	}
	_, err = d.api.Compute.TerminateInstance(ctx, region, core.TerminateInstanceRequest{
		InstanceId: &ocid, PreserveBootVolume: new(false), PreserveDataVolumesCreatedAtLaunch: new(false),
	})
	if err != nil && !isNotFound(err) {
		return classify(err, kindRead)
	}
	return nil
}
