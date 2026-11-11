/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// MaxStatusEntries caps status.vendors.<v>.resources.
const MaxStatusEntries = 128

// Report is the state of a vendor's resources for a NodeClass, per region.
type Report struct {
	// Resources are the referenced and managed resources, sorted.
	Resources []v1alpha1.ResourceStatus
	// Counts are the resources per region and kind, set only when there are
	// more than MaxStatusEntries (Resources is then cut).
	Counts []v1alpha1.ResourceCount
	// Regions says per checked region whether the resources the NodeClass
	// needs are ready, with the reason when not. A region whose check failed
	// for a transient reason is absent: its state stays as it was.
	Regions map[string]RegionState
	// Errors are the transient failures.
	Errors []error
}

// NotReady returns the regions that are not ready, sorted, with their reasons.
func (r Report) NotReady() (regions []string, reasons map[string]string) {
	reasons = map[string]string{}
	for region, st := range r.Regions {
		if !st.Ready {
			regions = append(regions, region)
			reasons[region] = st.Reason
		}
	}
	slices.Sort(regions)
	return regions, reasons
}

// Check evaluates the NodeClass's resource configuration for the vendor in the
// regions (00a section 9): `use` references are resolved, `forbid` kinds need
// the vendor's default, preCreate resources are created (or adopted), and the
// managed resources of the NodeClass are listed with their last use. It
// records the verdict of each region for the catalog.
func (e *Engine) Check(ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, regions []string) Report {
	rep := Report{Regions: map[string]RegionState{}}
	mr, kinds, ok := e.managed(vendor)
	if !ok {
		return rep
	}
	cfgs, err := options.ResourceConfig(nc, vendor)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return rep
	}
	drv, _ := e.opts.Registry.Driver(vendor)
	c := &checker{e: e, nc: nc, vendor: vendor, drv: drv, mr: mr, kinds: kinds, cfgs: cfgs, rep: &rep,
		seen: map[string]bool{},
		refs: map[vendors.ResourceKind][]vendors.ResourceRef{}}
	// The managed resources are listed first: a kind that lives in a network is
	// constrained to the managed network that already exists, as at a launch.
	managed, err := mr.List(ctx, regions, map[vendors.TagKey]string{
		vendors.TagCluster: e.opts.ClusterName, vendors.TagManagedBy: vendors.ManagedByValue,
	})
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("listing the managed %s resources: %w", vendor, err))
	}
	c.managed = managed
	for _, region := range regions {
		c.zones = nil
		problems, failed := c.region(ctx, region)
		switch {
		case len(problems) > 0:
			rep.Regions[region] = RegionState{Reason: strings.Join(problems, "; ")}
		case !failed: // a transient failure keeps the last verdict
			rep.Regions[region] = RegionState{Ready: true, Zones: c.zones}
		}
	}

	for _, r := range managed {
		if r.Managed && strings.HasPrefix(r.Name, NamePrefix(e.opts.ClusterName)) && c.uses(r) {
			c.add(r.Kind, r.Region, r, v1alpha1.ResourceSourceCreated)
		}
	}

	slices.SortFunc(rep.Resources, func(a, b v1alpha1.ResourceStatus) int {
		return strings.Compare(a.Kind+"/"+a.Region+"/"+a.ID, b.Kind+"/"+b.Region+"/"+b.ID)
	})
	if len(rep.Resources) > MaxStatusEntries {
		counts := map[[2]string]int32{}
		for _, r := range rep.Resources {
			counts[[2]string{r.Region, r.Kind}]++
		}
		for k, n := range counts {
			rep.Counts = append(rep.Counts, v1alpha1.ResourceCount{Region: k[0], Kind: k[1], Count: n})
		}
		slices.SortFunc(rep.Counts, func(a, b v1alpha1.ResourceCount) int {
			return strings.Compare(a.Region+"/"+a.Kind, b.Region+"/"+b.Kind)
		})
		rep.Resources = rep.Resources[:MaxStatusEntries]
	}
	for region, st := range rep.Regions {
		e.setRegion(nc.Name, vendor, region, st)
	}
	return rep
}

// checker holds what the checks of one NodeClass and vendor share.
type checker struct {
	e      *Engine
	nc     *v1alpha1.SCNodeClass
	vendor string
	drv    vendors.Driver
	mr     vendors.ManagedResources
	kinds  map[vendors.ResourceKind]vendors.KindSpec
	cfgs   map[string]common.Resource
	rep    *Report
	seen   map[string]bool
	// zones are the zones of the region being checked that lack a resource.
	zones map[string]string
	// refs are the resources found in the region being checked, by kind.
	refs map[vendors.ResourceKind][]vendors.ResourceRef
	// managed are the managed resources of the cluster in the regions checked.
	managed []vendors.ResourceRef
}

// uses says whether the NodeClass has anything to do with a managed resource:
// it created it, or the resource is shared infrastructure the NodeClass maps to
// (another NodeClass may have created it).
func (c *checker) uses(r vendors.ResourceRef) bool {
	if r.Tags[vendors.TagNodeClass] == c.nc.Name {
		return true
	}
	if c.kinds[r.Kind].Scope != vendors.ScopeRegion {
		return false
	}
	regions, _ := c.nc.Spec.Vendors.Regions(c.vendor)
	if len(regions) > 0 && !slices.Contains(regions, r.Region) {
		return false
	}
	cfg := c.cfgs[string(r.Kind)]
	return cfg.Use == nil && !cfg.Forbid
}

// add records a resource of the status once, with the step that produced it.
func (c *checker) add(kind vendors.ResourceKind, region string, ref vendors.ResourceRef, src v1alpha1.ResourceSource) {
	k := string(kind) + "/" + region + "/" + ref.ID
	if c.seen[k] {
		return
	}
	c.seen[k] = true
	c.refs[kind] = append(c.refs[kind], ref)
	rs := v1alpha1.ResourceStatus{Kind: string(kind), Region: region, ID: ref.ID, Name: ref.Name, Managed: ref.Managed,
		Source: src}
	if ref.Managed {
		id := c.vendor + "/" + ref.ID
		c.e.mu.Lock()
		if o, ok := c.e.observed[id]; ok && !o.last.IsZero() {
			rs.LastUsed = &metav1.Time{Time: o.last}
		}
		if f := c.e.failures[id]; f != nil {
			rs.Message = "deletion failed, retrying: " + f.message
		}
		c.e.mu.Unlock()
	}
	c.rep.Resources = append(c.rep.Resources, rs)
}

// region checks every kind in one region: the problems that make its offerings
// unavailable, and whether a transient failure left a kind unchecked.
func (c *checker) region(ctx context.Context, region string) (problems []string, failed bool) {
	c.refs = map[vendors.ResourceKind][]vendors.ResourceRef{}
	// A kind that lives in a network is checked after the kind that provides it.
	for _, ks := range inNetworkOrder(c.mr.Kinds()) {
		problem, transient := c.kind(ctx, ks, c.cfgs[string(ks.Kind)], region)
		if problem != "" {
			problems = append(problems, problem)
		}
		failed = failed || transient
	}
	if len(problems) == 0 && !failed {
		problems = append(problems, c.zoneGaps(ctx, region)...)
	}
	return problems, failed
}

// zoneGaps asks a driver with the ZoneChecker capability which zones cannot host a
// node with the NodeClass's default settings, given the resources found. The zones
// are gated individually; a region where every zone is a gap is a problem of the
// region. An error is not a verdict: the zones stay as they were.
func (c *checker) zoneGaps(ctx context.Context, region string) []string {
	drv, ok := c.e.opts.Registry.Driver(c.vendor)
	if !ok {
		return nil
	}
	zc, isZC := drv.(vendors.ZoneChecker)
	no, isNO := drv.(vendors.NodeOptions)
	if !isZC || !isNO {
		return nil
	}
	in, has, err := options.InputFromNodeClass(c.nc, c.vendor)
	if err != nil || !has {
		return nil
	}
	settings, err := options.Resolve(no, in, options.Selection{})
	if err != nil {
		return nil
	}
	gaps, err := zc.ZoneGaps(ctx, region, settings, c.refs)
	if err != nil || len(gaps) == 0 {
		return nil
	}
	var zones []string
	if c.e.opts.Zones != nil {
		zones = c.e.opts.Zones(c.vendor, region)
	}
	if len(zones) > 0 && len(gaps) >= len(zones) {
		var reasons []string
		for _, z := range slices.Sorted(maps.Keys(gaps)) {
			reasons = append(reasons, z+": "+gaps[z])
		}
		return []string{"no zone can host the nodes: " + strings.Join(reasons, "; ")}
	}
	if c.zones == nil {
		c.zones = map[string]string{}
	}
	maps.Copy(c.zones, gaps)
	return nil
}

// kind checks one kind in one region, following the resolution order of ADR-022
// (order.go): an explicit use, else the opt-in tag, the vendor's default, and
// creation, which the check does only for a preCreate (a launch creates the
// rest on demand).
func (c *checker) kind(
	ctx context.Context, ks vendors.KindSpec, cfg common.Resource, region string,
) (problem string, transient bool) {
	e := c.e
	networks := c.networks(ks, region)
	found := func(src v1alpha1.ResourceSource, refs []vendors.ResourceRef) {
		for _, r := range refs {
			c.add(ks.Kind, region, r, src)
		}
	}
	in := orderIn{nc: c.nc, drv: c.drv, mr: c.mr, vendor: c.vendor, spec: ks, region: region, networks: networks}
	// resolve returns the resources, or none: why the selector selected nothing
	// (or too much), with the counts after each stage.
	resolve := func(ref vendors.UseRef) (refs []vendors.ResourceRef, none string, failed bool) {
		refs, err := e.resolve(ctx, c.mr, c.vendor, ks.Kind, region, ref)
		switch {
		case isSelection(err):
			return nil, err.Error(), false
		case err != nil:
			c.rep.Errors = append(c.rep.Errors,
				fmt.Errorf("resolving the %s of %s in %s: %w", ks.Kind, c.vendor, region, err))
			return nil, "", true
		}
		if refs = in.inNetworks(refs); len(refs) == 0 {
			return nil, "nothing matches " + describe(ref) + networkNote(networks), false
		}
		return refs, "", false
	}
	switch {
	case cfg.Use != nil && selector.UsesZone(cfg.Use):
		return c.perZone(ks, cfg, region, in, resolve, found)
	case cfg.Use != nil:
		ref, err := e.useRefFor(c.nc, c.mr, c.vendor, ks.Kind, cfg.Use, region, "")
		if err != nil {
			return fmt.Sprintf("%s: %v", ks.Kind, err), false
		}
		in.constrain(&ref, c.mr.Selectors()[ks.Kind])
		refs, none, failed := resolve(ref)
		if failed {
			return "", true
		}
		if none != "" {
			return fmt.Sprintf("%s: %s", ks.Kind, none), false
		}
		found(v1alpha1.ResourceSourceUse, refs)
	case cfg.Create == nil:
		// Neither use nor create (or forbid): discovery, then the vendor's
		// default. Nothing found is fine unless creation is forbidden: the
		// resource is created on demand.
		res, err := e.discoverAndDefault(ctx, in, cfg.Forbid)
		switch {
		case isSelection(err):
			return fmt.Sprintf("%s: %v", ks.Kind, err), false
		case err != nil:
			c.rep.Errors = append(c.rep.Errors,
				fmt.Errorf("resolving the %s of %s in %s: %w", ks.Kind, c.vendor, region, err))
			return "", true
		case len(res.refs) > 0:
			found(res.source, res.refs)
		case ks.Optional:
			// Nothing provides an optional kind: nodes go without it.
		case cfg.Forbid:
			return failureMessage(ks.Kind, "", res.tried, "creating it is forbidden"), false
		}
	case ks.Scope == vendors.ScopeRegion && preCreateListed(&cfg, region):
		ref, err := e.ensure(ctx, c.nc, c.mr, c.vendor, region, c.kinds[ks.Kind], vendors.ResourceRequest{Kind: ks.Kind})
		if err != nil {
			return fmt.Sprintf("%s: creating it failed: %v", ks.Kind, err), false
		}
		c.add(ks.Kind, region, ref, v1alpha1.ResourceSourceCreated)
	}
	return "", false
}

// networks are the networks the resources of the provider kind were resolved to
// in the region, for a kind that lives in a network. Where the provider kind
// resolved to nothing a launch creates (or adopts) the managed network of the
// region, so the dependent kind is constrained to it once it exists, as at a
// launch; none while it does not exist (it is created on demand).
func (c *checker) networks(ks vendors.KindSpec, region string) []string {
	if ks.InNetwork == "" {
		return nil
	}
	if found := networksOf(c.refs[ks.InNetwork]); len(found) > 0 {
		return found
	}
	cfg := c.cfgs[string(ks.InNetwork)]
	if cfg.Use != nil || cfg.Forbid {
		return nil
	}
	// The name a launch's Ensure of the provider kind gives it (no zone, no key).
	name := ResourceName(c.e.opts.ClusterName, ks.InNetwork, region, "", "")
	for _, r := range c.managed {
		if r.Kind == ks.InNetwork && r.Region == region && r.Managed && r.Name == name {
			if r.Network != "" {
				return []string{r.Network}
			}
			return []string{r.ID}
		}
	}
	return nil
}

// perZone checks a use block with {zone}: it is expanded for every zone of the
// region. The region is unavailable when no zone has a match; a zone without one
// fails only the launches into it (they are rejected with ResourceMissing, the
// rest of the region is fine).
func (c *checker) perZone(
	ks vendors.KindSpec, cfg common.Resource, region string, in orderIn,
	resolve func(vendors.UseRef) ([]vendors.ResourceRef, string, bool),
	found func(v1alpha1.ResourceSource, []vendors.ResourceRef),
) (problem string, transient bool) {
	var zones []string
	if c.e.opts.Zones != nil {
		zones = c.e.opts.Zones(c.vendor, region)
	}
	if len(zones) == 0 {
		return fmt.Sprintf("%s: {zone} needs the zones of the region, which are not known", ks.Kind), false
	}
	matched := 0
	reasons := map[string]string{}
	var empty []string
	for _, zone := range zones {
		ref, err := c.e.useRefFor(c.nc, c.mr, c.vendor, ks.Kind, cfg.Use, region, zone)
		if err != nil {
			return fmt.Sprintf("%s: %v", ks.Kind, err), false
		}
		in.constrain(&ref, c.mr.Selectors()[ks.Kind])
		refs, none, failed := resolve(ref)
		if failed {
			return "", true
		}
		if none != "" {
			empty = append(empty, zone)
			reasons[zone] = none
			continue
		}
		matched++
		found(v1alpha1.ResourceSourceUse, refs)
	}
	for _, z := range empty {
		if matched > 0 {
			if c.zones == nil {
				c.zones = map[string]string{}
			}
			c.zones[z] = fmt.Sprintf("%s in %s: %s", ks.Kind, z, reasons[z])
		}
	}
	if matched == 0 {
		var parts []string
		for _, z := range empty {
			parts = append(parts, z+": "+reasons[z])
		}
		return fmt.Sprintf("%s: nothing matches in any zone (%s)", ks.Kind, strings.Join(parts, "; ")), false
	}
	return "", false
}
