/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MaxStatusEntries caps status.vendors.<v>.resources.
const MaxStatusEntries = 128

// Report is the state of a vendor's resources for a NodeClass, per region.
type Report struct {
	// Resources are the referenced and managed resources, sorted.
	Resources []v1alpha1.ResourceStatus
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
	c := &checker{e: e, nc: nc, vendor: vendor, mr: mr, kinds: kinds, cfgs: cfgs, rep: &rep, seen: map[string]bool{}}
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

	managed, err := mr.List(ctx, regions, map[vendors.TagKey]string{
		vendors.TagCluster: e.opts.ClusterName, vendors.TagManagedBy: vendors.ManagedByValue,
	})
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("listing the managed %s resources: %w", vendor, err))
	}
	for _, r := range managed {
		if r.Managed && strings.HasPrefix(r.Name, NamePrefix(e.opts.ClusterName)) && c.uses(r) {
			c.add(r.Kind, r.Region, r)
		}
	}

	slices.SortFunc(rep.Resources, func(a, b v1alpha1.ResourceStatus) int {
		return strings.Compare(a.Kind+"/"+a.Region+"/"+a.ID, b.Kind+"/"+b.Region+"/"+b.ID)
	})
	if len(rep.Resources) > MaxStatusEntries {
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
	mr     vendors.ManagedResources
	kinds  map[vendors.ResourceKind]vendors.KindSpec
	cfgs   map[string]common.Resource
	rep    *Report
	seen   map[string]bool
	// zones are the zones of the region being checked that lack a resource.
	zones map[string]string
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

// add records a resource of the status once.
func (c *checker) add(kind vendors.ResourceKind, region string, ref vendors.ResourceRef) {
	k := string(kind) + "/" + region + "/" + ref.ID
	if c.seen[k] {
		return
	}
	c.seen[k] = true
	rs := v1alpha1.ResourceStatus{Kind: string(kind), Region: region, ID: ref.ID, Managed: ref.Managed}
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
	for _, ks := range c.mr.Kinds() {
		problem, transient := c.kind(ctx, ks, c.cfgs[string(ks.Kind)], region)
		if problem != "" {
			problems = append(problems, problem)
		}
		failed = failed || transient
	}
	return problems, failed
}

// kind checks one kind in one region.
func (c *checker) kind(
	ctx context.Context, ks vendors.KindSpec, cfg common.Resource, region string,
) (problem string, transient bool) {
	e := c.e
	found := func(refs []vendors.ResourceRef) {
		for _, r := range refs {
			c.add(ks.Kind, region, r)
		}
	}
	resolve := func(ref vendors.UseRef) ([]vendors.ResourceRef, bool) {
		refs, err := e.resolve(ctx, c.mr, c.vendor, ks.Kind, region, ref)
		if err != nil {
			c.rep.Errors = append(c.rep.Errors,
				fmt.Errorf("resolving the %s of %s in %s: %w", ks.Kind, c.vendor, region, err))
			return nil, true
		}
		return refs, false
	}
	switch {
	case cfg.Use != nil && strings.Contains(cfg.Use.Name, "{zone}"):
		return c.perZone(ks, cfg, region, resolve, found)
	case cfg.Use != nil:
		ref, err := useRefFor(cfg.Use, region, "")
		if err != nil {
			return fmt.Sprintf("%s: %v", ks.Kind, err), false
		}
		refs, failed := resolve(ref)
		if failed {
			return "", true
		}
		if len(refs) == 0 {
			return fmt.Sprintf("%s: nothing matches %s", ks.Kind, describe(ref)), false
		}
		found(refs)
	case cfg.Forbid:
		refs, failed := resolve(vendors.UseRef{})
		if failed {
			return "", true
		}
		if len(refs) == 0 {
			return fmt.Sprintf("%s: creating it is forbidden and the vendor has no default", ks.Kind), false
		}
		found(refs)
	case cfg.Create == nil && ks.Scope == vendors.ScopeRegion:
		refs, failed := resolve(vendors.UseRef{})
		if failed {
			return "", true
		}
		found(refs)
	case ks.Scope == vendors.ScopeRegion && preCreateListed(&cfg, region):
		ref, err := e.ensure(ctx, c.nc, c.mr, c.vendor, region, c.kinds[ks.Kind], vendors.ResourceRequest{Kind: ks.Kind})
		if err != nil {
			return fmt.Sprintf("%s: creating it failed: %v", ks.Kind, err), false
		}
		c.add(ks.Kind, region, ref)
	}
	return "", false
}

// perZone checks a name pattern with {zone}: it is expanded for every zone of
// the region. The region is unavailable when no zone has a match; a zone
// without one fails only the launches into it (they are rejected with
// ResourceMissing, the rest of the region is fine).
func (c *checker) perZone(
	ks vendors.KindSpec, cfg common.Resource, region string,
	resolve func(vendors.UseRef) ([]vendors.ResourceRef, bool), found func([]vendors.ResourceRef),
) (problem string, transient bool) {
	var zones []string
	if c.e.opts.Zones != nil {
		zones = c.e.opts.Zones(c.vendor, region)
	}
	if len(zones) == 0 {
		return fmt.Sprintf("%s: the pattern %q needs the zones of the region, which are not known",
			ks.Kind, cfg.Use.Name), false
	}
	matched := 0
	var empty []string
	for _, zone := range zones {
		ref, err := useRefFor(cfg.Use, region, zone)
		if err != nil {
			return fmt.Sprintf("%s: %v", ks.Kind, err), false
		}
		refs, failed := resolve(ref)
		if failed {
			return "", true
		}
		if len(refs) == 0 {
			empty = append(empty, zone)
			continue
		}
		matched++
		found(refs)
	}
	for _, z := range empty {
		if matched > 0 {
			if c.zones == nil {
				c.zones = map[string]string{}
			}
			c.zones[z] = fmt.Sprintf("%s: nothing matches the pattern %q in %s", ks.Kind, cfg.Use.Name, z)
		}
	}
	if matched == 0 {
		return fmt.Sprintf("%s: nothing matches the pattern %q in any zone (%s)", ks.Kind, cfg.Use.Name,
			strings.Join(empty, ", ")), false
	}
	return "", false
}
