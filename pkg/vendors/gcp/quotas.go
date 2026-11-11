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

// ConfirmedSeries are the machine series whose quota consumption the spike or the
// lane's quota scenario confirmed, with the metrics one vCPU of the series
// consumes (QuotaReader.Confirmed). It is empty: which metrics one machine type
// consumes is UNVERIFIED (plan 16, section 5), so every type is unknown quota and
// takes the reactive path (QuotaExceeded names the metric) until a live run adds
// a series here with the lane's `quota` scenario as its proof.
var ConfirmedSeries = map[string][]string{}

// WithConfirmedSeries replaces the confirmed series table, for the tests.
func WithConfirmedSeries(m map[string][]string) Option {
	return func(d *Driver) { d.confirmed = m }
}

func (d *Driver) reader(api ComputeAPI) *QuotaReader {
	c := d.confirmed
	if c == nil {
		c = ConfirmedSeries
	}
	return &QuotaReader{API: api, Project: d.project, Confirmed: c}
}

// Consumption implements vendors.QuotaReporter: the vCPUs of the type against the
// metrics of its series, for the confirmed series only. A type whose vCPUs the
// driver has not read yet (the offerings read them) is unknown.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	d.mu.Lock()
	m, ok := d.machines[instanceType]
	d.mu.Unlock()
	if !ok || m.VCPUs <= 0 {
		return nil, false
	}
	return d.reader(nil).Consumption(instanceType, m.VCPUs, capacityType)
}

// Quotas implements vendors.QuotaReporter: regions.get gives limit and usage of
// the CPU and instance buckets, so no usage is computed from instances; Counted
// are the cluster's instances in the region that hold quota, so that the
// tracker releases their in-flight reservations.
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := d.reader(api).Quotas(ctx, region)
	if err != nil {
		return nil, err
	}
	var filter string
	if d.cfg.ClusterName != "" {
		filter = LabelFilter(map[vendors.TagKey]string{vendors.TagCluster: d.cfg.ClusterName})
	}
	list, err := d.aggregated(ctx, api, filter)
	if err != nil {
		return nil, err
	}
	for _, in := range list {
		if RegionOfZone(zoneOf(in)) == region && holdsQuota(in.GetStatus()) {
			snap.Counted = append(snap.Counted, NewRef(region, zoneOf(in), in.GetName()))
		}
	}
	slices.Sort(snap.Counted)
	return snap, nil
}

// holdsQuota says whether an instance in the status counts against the CPU quota:
// a stopped or terminated one does not.
func holdsQuota(status string) bool {
	switch status {
	case StatusStopped, StatusTerminated, StatusSuspended:
		return false
	}
	return true
}

// GlobalQuotas implements vendors.QuotaReporter: CPUS_ALL_REGIONS of projects.get,
// when the project has it.
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	return d.reader(api).GlobalQuotas(ctx)
}

// droppedSeries are the series the driver removes from the catalog: GPU machine
// types launch like any type but have no driver (NVIDIA) support in v1 (like AWS's
// mac types). UNVERIFIED list.
var droppedSeries = []string{"a2", "a3", "a4", "a4x", "g2", "g4", "ct3", "ct3p", "ct5l", "ct5lp", "ct5p", "ct6e", "tpu7",
	"tpu7x"}

// launchable says whether the machine type is offered by the driver: no custom
// machine types, no bare metal, no GPU series.
func launchable(name string) bool {
	return !strings.Contains(name, "-custom-") && !strings.HasPrefix(name, "custom-") &&
		!strings.HasSuffix(name, "-metal") && !strings.Contains(name, "-metal-") && !slices.Contains(droppedSeries, Family(name))
}

// LaunchableTypes implements vendors.OfferingLister: machineTypes.list per zone of
// the region, keyed by Keeper's numeric zone ID. A machine type a zone does not
// list is not offered there. The stock is not available through an API: capacity
// stays reactive. Reading the offerings also reads the vCPUs and architecture of
// the types, which the quota consumption needs.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	machines := map[string]machineInfo{}
	for _, z := range zones {
		req := &computepb.ListMachineTypesRequest{Project: d.project, Zone: z.Name}
		for {
			res, err := api.ListMachineTypes(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("listing the machine types of %s: %w", z.Name, classify(err, errContext{}))
			}
			for _, mt := range res.GetItems() {
				name := mt.GetName()
				if !launchable(name) {
					continue
				}
				if !slices.Contains(out[name], z.ID) {
					out[name] = append(out[name], z.ID)
				}
				machines[name] = machineInfo{VCPUs: int64(mt.GetGuestCpus()), Arch: mt.GetArchitecture()}
			}
			if res.GetNextPageToken() == "" {
				break
			}
			req.PageToken = res.NextPageToken
		}
	}
	for _, ids := range out {
		slices.Sort(ids)
	}
	changed := false
	d.mu.Lock()
	for n, m := range machines {
		if d.machines[n] != m {
			d.machines[n], changed = m, true
		}
	}
	d.mu.Unlock()
	if changed {
		d.epoch.Add(1)
	}
	return out, nil
}
