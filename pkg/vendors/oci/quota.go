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
	"time"

	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// serviceCompute is the Limits service's name of Compute.
const serviceCompute = "compute"

// limitValuesTTL is how long the limit values are reused when the caller does not
// ask for fresh limits: they change rarely.
const limitValuesTTL = 30 * time.Minute

// The forms of a limit name and the units of their buckets (plan 25, section 7).
const (
	coreForm    = "-core"
	memoryForm  = "-memory"
	countSuffix = "-count"

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

// limitParts splits a limit name into the unit of its bucket (OCPUs, GB or
// instances) and its family: the name without "-count", the scope infix and the
// form. ok is false for a name of another form, which makes every type of the
// shape that lists it unknown (reactive).
func limitParts(name string) (unit, family string, ok bool) {
	base, found := strings.CutSuffix(name, countSuffix)
	if !found {
		return "", "", false
	}
	// OCI names a scoped limit with an infix between the form and "-count":
	// standard-e3-core-ad-count, standard-a1-core-regional-count.
	for _, scope := range []string{"-ad", "-regional", "-global"} {
		if b, ok := strings.CutSuffix(base, scope); ok {
			base = b
			break
		}
	}
	if b, ok := strings.CutSuffix(base, coreForm); ok {
		return UnitOCPU, b, true
	}
	if b, ok := strings.CutSuffix(base, memoryForm); ok {
		return UnitGB, b, true
	}
	return UnitInstances, base, true
}

// limitForm classifies a limit name by the unit of its bucket.
func limitForm(name string) (unit string, ok bool) {
	unit, _, ok = limitParts(name)
	return unit, ok
}

// IsCoreLimit says whether the limit name counts OCPUs, whatever its scope infix.
func IsCoreLimit(name string) bool {
	unit, ok := limitForm(name)
	return ok && unit == UnitOCPU
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

// limitKey identifies a limit value: the limit name and the full name of its
// availability domain ("" for a region-scoped limit).
type limitKey struct{ name, ad string }

// limitValues returns the limit values of the Compute limits of the region (one
// ListLimitValues call, paginated), reading them again when the caller asks for
// fresh limits or the cache is older than limitValuesTTL.
func (d *Driver) limitValues(ctx context.Context, region string) (map[limitKey]int64, error) {
	d.mu.Lock()
	if v, ok := d.values[region].get(d.clock.Now()); ok && !vendors.QuotaRefreshFrom(ctx).Limits {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	req := limits.ListLimitValuesRequest{CompartmentId: &d.tenancy, ServiceName: new(serviceCompute)}
	out := map[limitKey]int64{}
	for {
		res, err := d.api.Limits.ListLimitValues(ctx, region, req)
		if err != nil {
			return nil, fmt.Errorf("listing the Compute limit values of %s: %w", region, classify(err, kindRead))
		}
		for _, v := range res.Items {
			if v.Value != nil {
				out[limitKey{deref(v.Name), deref(v.AvailabilityDomain)}] = *v.Value
			}
		}
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			break
		}
		req.Page = res.OpcNextPage
	}
	d.mu.Lock()
	d.values[region] = set(out, d.clock.Now().Add(limitValuesTTL))
	d.mu.Unlock()
	return out, nil
}

// zeroEverywhere says whether the limit is listed as 0 in every AD (or in the
// region): nothing can launch against it, so its usage need not be read. A limit
// value that is not listed does not count as 0.
func zeroEverywhere(name string, def limitDef, ads []availabilityDomain, values map[limitKey]int64) bool {
	if def.scope != limits.LimitDefinitionSummaryScopeTypeAd {
		v, ok := values[limitKey{name, ""}]
		return ok && v == 0
	}
	for _, a := range ads {
		if v, ok := values[limitKey{name, a.Full}]; !ok || v != 0 {
			return false
		}
	}
	return len(ads) > 0
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
// derived (used plus that headroom); the per-AD numbers go to the zones.
func bucketOf(name string, reads []availability) vendors.Bucket {
	var used, best int64
	for _, r := range reads {
		used += r.used
		best = max(best, r.available)
	}
	unit, family, _ := limitParts(name)
	return vendors.Bucket{
		ID: name, Scope: vendors.QuotaScopeRegion, Unit: unit, Family: family, Limit: used + best, Used: used,
	}
}

// zonesOf is the true limit and usage of an AD-scoped limit in each AD, in the
// order of ads and reads. The limit is the listed value, else the usage plus
// what is available.
func zonesOf(name string, ads []availabilityDomain, reads []availability, values map[limitKey]int64,
) []vendors.ZoneBucket {
	out := make([]vendors.ZoneBucket, 0, len(ads))
	for i, a := range ads {
		var r availability
		if i < len(reads) {
			r = reads[i]
		}
		limit, ok := values[limitKey{name, a.Full}]
		if !ok {
			limit = r.used + r.available
		}
		out = append(out, vendors.ZoneBucket{Zone: a.Name, ID: name, Limit: limit, Used: r.used})
	}
	return out
}

// Quotas implements vendors.QuotaReporter: one bucket per Compute limit name that
// the region's shapes consume, and the cluster's instances that hold quota. The
// limit values come from one cached ListLimitValues; a name whose limit is 0
// everywhere is a 0/0 bucket without further calls, and the others are read with
// GetResourceAvailability (per AD for AD-scoped names), which also gives the
// per-AD zones. A limit the service cannot answer for is left out and its shapes
// are unknown. The calls run one after the other, at the tracker's pace.
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
	values, err := d.limitValues(ctx, region)
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
		if zeroEverywhere(name, def, ads, values) {
			b := bucketOf(name, nil)
			snap.Buckets = append(snap.Buckets, b)
			if def.scope == limits.LimitDefinitionSummaryScopeTypeAd {
				snap.Zones = append(snap.Zones, zonesOf(name, ads, nil, values)...)
			}
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
		if def.scope == limits.LimitDefinitionSummaryScopeTypeAd {
			snap.Zones = append(snap.Zones, zonesOf(name, ads, reads, values)...)
		}
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
