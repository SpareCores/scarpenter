/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// CreateInstance implements vendors.Driver: one launch attempt. The VM's
// deterministic name takes the place of the idempotency key (plan 14, A3): a
// replay returns the VM of the name whatever else the request says, and a failed
// launch leaves nothing behind that would keep the next attempt from launching
// (Compute.Launch). The image is the URN ResolveImage returned; its purchase plan
// and the terms are read again here (cached for an hour).
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooLarge, fmt.Errorf(
			"the user-data is %d bytes; Azure accepts at most %d (a larger cluster CA or Ignition addition does not fit)",
			len(req.UserData), MaxUserData))
	}
	if err := d.checkZone(ctx, req.Region, req.Zone); err != nil {
		return nil, err
	}
	img, err := ParseURN(req.Image.Ref)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	res, _, err := d.images.Lookup(ctx, req.Region, img)
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			// The image the status reported is gone: nothing a next zone fixes.
			return nil, vendors.NewError(vendors.Fatal, codeImageNotFound, err)
		}
		return nil, err
	}
	if err := d.images.CheckTerms(ctx, res); err != nil {
		return nil, err
	}
	skus, err := d.skuTable(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	var sku *SKU
	if s, ok := skus[req.InstanceType]; ok {
		sku = &s
	}
	spec, err := NewLaunchSpec(req, d.cfg.ClusterName, res.Image, res.Plan, sku, d.sshKey)
	if err != nil {
		return nil, err
	}
	return d.compute.Launch(ctx, spec)
}

// GetInstance implements vendors.Driver.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	return d.compute.Get(ctx, ref)
}

// FindInstance implements vendors.Driver.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	return d.compute.Find(ctx, region, tags)
}

// ListInstances implements vendors.Driver: the VMs of the cluster in the
// regions, the terminating ones (stopped, deallocated, being deleted) included.
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	return d.compute.List(ctx, regions, tags)
}

// DeleteInstance implements vendors.Driver: a VM that is gone, or a reference
// that cannot be parsed, is success; a VM without this cluster's tag is refused.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	return d.compute.Delete(ctx, ref)
}

// Quotas implements vendors.QuotaReporter: the regional vCPU buckets (total,
// family, Spot) and the public IP addresses, as the Usage APIs report them (the
// usage of the whole subscription).
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	// Consumption maps a size by the SKU table of the regions read so far.
	_, _ = d.skuTable(ctx, region)
	snap, err := d.quota.Quotas(ctx, region)
	if err != nil {
		return nil, err
	}
	// The usage is the subscription's, so the reservations of the in-flight
	// ledger are released by the VMs of this cluster the usage already contains.
	// Best effort: without the list the reservations expire on their own.
	if refs, err := d.compute.Refs(ctx, region); err == nil {
		snap.Counted = refs
	}
	return snap, nil
}

// GlobalQuotas implements vendors.QuotaReporter: every Azure limit is per region.
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	return d.quota.GlobalQuotas(ctx)
}

// Consumption implements vendors.QuotaReporter.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	return d.quota.Consumption(instanceType, capacityType)
}

// checkZone refuses a zone the region does not have: ZoneUnusable, which the
// attempt loop answers with the next zone.
func (d *Driver) checkZone(ctx context.Context, region, zone string) error {
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(zones, func(z vendors.Zone) bool { return z.Name == zone }) {
		return vendors.NewError(vendors.ZoneUnusable, "ZoneNotInRegion",
			fmt.Errorf("the region %s has no zone %q", region, zone))
	}
	return nil
}
