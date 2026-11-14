/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// serviceCompute is the Limits service's name of Compute.
const serviceCompute = "compute"

// The forms of a limit name and the units of their buckets (plan 25, section 7).
const (
	coreSuffix   = "-core-count"
	memorySuffix = "-memory-count"
	countSuffix  = "-count"

	UnitOCPU      = "OCPU"
	UnitGB        = "GB"
	UnitInstances = "instances"
)

// limitDef is what the driver keeps of a limit definition: whether it is read per
// availability domain or per region, and whether its availability can be read.
type limitDef struct {
	scope     limits.LimitDefinitionSummaryScopeTypeEnum
	supported bool
}

// limitForm classifies a limit name by its form: the unit of its bucket and
// whether it counts OCPUs, GB or instances. ok is false for a name of another
// form, which makes every type of the shape that lists it unknown (reactive).
func limitForm(name string) (unit string, ok bool) {
	switch {
	case strings.HasSuffix(name, coreSuffix):
		return UnitOCPU, true
	case strings.HasSuffix(name, memorySuffix):
		return UnitGB, true
	case strings.HasSuffix(name, countSuffix):
		return UnitInstances, true
	}
	return "", false
}

// limitDefs returns the definitions of the Compute limits of the region, cached for
// an hour.
func (d *Driver) limitDefs(ctx context.Context, region string) (map[string]limitDef, error) {
	d.mu.Lock()
	if v, ok := d.limits[region].get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	req := limits.ListLimitDefinitionsRequest{CompartmentId: &d.tenancy, ServiceName: new(serviceCompute)}
	out := map[string]limitDef{}
	for {
		res, err := d.api.Limits.ListLimitDefinitions(ctx, region, req)
		if err != nil {
			return nil, fmt.Errorf("listing the Compute limit definitions of %s: %w", region, classify(err, kindRead))
		}
		for _, def := range res.Items {
			out[deref(def.Name)] = limitDef{scope: def.ScopeType, supported: deref(def.IsResourceAvailabilitySupported)}
		}
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			break
		}
		req.Page = res.OpcNextPage
	}
	d.mu.Lock()
	d.limits[region] = set(out, d.clock.Now().Add(factsTTL))
	d.mu.Unlock()
	return out, nil
}

// availability is one reading of a limit: the usage and what is left.
type availability struct{ used, available int64 }

// readLimit reads one limit of the region: per availability domain for an AD-scoped
// definition, once for the others. The usage is read on the tenancy (the root
// compartment), because the contract wants the whole account's usage; that the
// root's figure includes the sub-compartments' is UNVERIFIED (spike S3).
// skip is true for a limit whose availability the service does not answer for:
// the shapes that consume it are then unknown.
func (d *Driver) readLimit(ctx context.Context, region, name string, def limitDef, ads []availabilityDomain,
) (reads []availability, skip bool, err error) {
	scopes := []string{""}
	if def.scope == limits.LimitDefinitionSummaryScopeTypeAd {
		scopes = scopes[:0]
		for _, a := range ads {
			scopes = append(scopes, a.Full)
		}
	}
	for _, ad := range scopes {
		req := limits.GetResourceAvailabilityRequest{
			ServiceName: new(serviceCompute), LimitName: &name, CompartmentId: &d.tenancy,
		}
		if ad != "" {
			req.AvailabilityDomain = &ad
		}
		res, err := d.api.Limits.GetResourceAvailability(ctx, region, req)
		if err != nil {
			if se, ok := serviceError(err); ok && (se.GetHTTPStatusCode() == http.StatusBadRequest ||
				se.GetHTTPStatusCode() == http.StatusNotFound) {
				return nil, true, nil
			}
			return nil, false, fmt.Errorf("reading the limit %s of %s: %w", name, region, classify(err, kindRead))
		}
		reads = append(reads, availability{used: deref(res.Used), available: deref(res.Available)})
	}
	return reads, false, nil
}

// bucketOf merges the readings of a limit into one region bucket. The limit of
// an availability-domain-scoped definition is per AD, but Consumption has no zone
// argument (plan 25, open question 6): the bucket is the region's usage (the sum
// of the ADs') and the headroom of the AD with the most room, so that the tracker
// blocks a type only when no AD has room for it and never wrongly. Limit is
// derived (used plus that headroom); the per-AD numbers are not reported.
func bucketOf(name string, reads []availability) vendors.Bucket {
	var used, best int64
	for _, r := range reads {
		used += r.used
		best = max(best, r.available)
	}
	unit, _ := limitForm(name)
	return vendors.Bucket{ID: name, Scope: vendors.QuotaScopeRegion, Unit: unit, Limit: used + best, Used: used}
}

// Quotas implements vendors.QuotaReporter: one bucket per Compute limit name that
// the region's shapes consume, from the Limits service (GetResourceAvailability),
// and the cluster's instances that hold quota. A limit the service cannot answer
// for is left out and its shapes are unknown. The calls run one after the other:
// a region has a few dozen names in about three ADs, read at the tracker's pace.
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	ads, err := d.domains(ctx, region)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, a := range ads {
		shapes, err := d.shapesIn(ctx, region, a.Full)
		if err != nil {
			return nil, err
		}
		for _, si := range shapes {
			for _, n := range si.QuotaNames {
				if _, ok := limitForm(n); ok {
					names[n] = true
				}
			}
		}
	}
	defs, err := d.limitDefs(ctx, region)
	if err != nil {
		return nil, err
	}
	snap := &vendors.QuotaSnapshot{}
	unsupported := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		def, ok := defs[name]
		if !ok || !def.supported {
			unsupported[name] = true
			continue
		}
		reads, skip, err := d.readLimit(ctx, region, name, def, ads)
		if err != nil {
			return nil, err
		}
		if skip || len(reads) == 0 {
			unsupported[name] = true
			continue
		}
		snap.Buckets = append(snap.Buckets, bucketOf(name, reads))
	}
	d.mu.Lock()
	for n := range unsupported {
		d.noQuota[n] = true
	}
	d.mu.Unlock()
	list, err := d.listRegion(ctx, region, "")
	if err != nil {
		return nil, err
	}
	for _, in := range list {
		if d.cfg.ClusterName == "" || in.FreeformTags[TagKey(vendors.TagCluster)] == d.cfg.ClusterName {
			snap.Counted = append(snap.Counted, NewRef(region, deref(in.Id)))
		}
	}
	slices.Sort(snap.Counted)
	return snap, nil
}

// GlobalQuotas implements vendors.QuotaReporter: none (the Compute limits are per
// availability domain or region).
func (d *Driver) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// Consumption implements vendors.QuotaReporter: one entry per limit name of the
// type's shape, by the name's form: *-core-count the OCPUs (a sized type: from
// the name; a fixed shape: the shape's own), *-memory-count the GB, any other
// *-count one instance. A type is unknown (reactive) when its shape is not in a
// shape table the driver has read, lists no limit or a name of another form, a
// limit whose availability cannot be read, and for every spot type: whether
// preemptible capacity has a limit of its own is UNVERIFIED (spike S9).
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	if capacityType == vendors.CapacitySpot {
		return nil, false
	}
	t, err := ParseType(instanceType)
	if err != nil {
		return nil, false
	}
	si, ok := d.shapeByName(t.Shape)
	if !ok || len(si.QuotaNames) == 0 || t.Sized != si.Flexible {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []vendors.Consumption
	for _, n := range si.QuotaNames {
		unit, ok := limitForm(n)
		if !ok || d.noQuota[n] {
			return nil, false
		}
		amount := int64(1)
		switch unit {
		case UnitOCPU:
			amount = t.amount(si.OCPUs, int64(t.OCPUs))
		case UnitGB:
			amount = t.amount(si.MemoryGB, int64(t.GB))
		}
		if amount <= 0 {
			return nil, false
		}
		if !slices.ContainsFunc(out, func(c vendors.Consumption) bool { return c.Bucket == n }) {
			out = append(out, vendors.Consumption{Bucket: n, Amount: amount})
		}
	}
	return out, len(out) > 0
}

// amount is what the type consumes: the size of a sized type, the shape's own size
// (rounded up) for a fixed one.
func (t InstanceType) amount(fixed float64, sized int64) int64 {
	if t.Sized {
		return sized
	}
	return int64(math.Ceil(fixed))
}
