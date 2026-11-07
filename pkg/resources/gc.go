/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Reasons of a deletion, for logs and Events.
const (
	ReasonPerUse    = "PerUseUnused"
	ReasonIdleTTL   = "IdleTTLExpired"
	ReasonRetained  = "NodeClassDeleted"
	ReasonOrphan    = "NodeClassDeletedUnused"
	keptInUse       = "in use"
	keptPreCreated  = "kept by preCreate"
	keptRetain      = "retained"
	keptWithinGrace = "within the grace period"
)

// Lifecycle is the resolved lifecycle of a managed resource.
type Lifecycle struct {
	Policy common.LifecyclePolicy
	TTL    time.Duration
}

// LifecycleOf returns the lifecycle of a managed resource of the kind: the
// NodeClass's create.lifecycle where it sets one, else the kind's default
// policy with the default TTL.
func (e *Engine) LifecycleOf(spec vendors.KindSpec, cfg *common.Resource) Lifecycle {
	lc := Lifecycle{Policy: spec.DefaultPolicy, TTL: e.opts.DefaultTTL}
	if cfg != nil && cfg.Create != nil && cfg.Create.Lifecycle != nil {
		lc.Policy = cfg.Create.Lifecycle.Policy
		if cfg.Create.Lifecycle.TTL != nil {
			lc.TTL = cfg.Create.Lifecycle.TTL.Duration
		}
	}
	if lc.Policy == "" {
		lc.Policy = common.LifecyclePerUse
	}
	return lc
}

// Decision is what to do with a managed resource.
type Decision struct {
	Delete bool
	// Reason is the reason of a deletion; Kept says why a resource stays.
	Reason, Kept string
}

// Decide applies a lifecycle (00a section 9.3): perUse deletes after the grace
// once no instance uses the resource, idleTTL after the TTL, retain only when
// the NodeClass is gone (and then like perUse, once unused). A resource in use,
// or kept by preCreate while its region is listed, is never deleted. last is
// the time of the last use (or of the creation).
func Decide(lc Lifecycle, grace time.Duration, classExists, preCreated, inUse bool, last, now time.Time) Decision {
	switch {
	case inUse:
		return Decision{Kept: keptInUse}
	case preCreated && classExists:
		return Decision{Kept: keptPreCreated}
	case !classExists:
		if lc.Policy == common.LifecycleRetain {
			// Retained for the NodeClass: it is gone.
			if now.Sub(last) >= grace {
				return Decision{Delete: true, Reason: ReasonRetained}
			}
			return Decision{Kept: keptWithinGrace}
		}
	case lc.Policy == common.LifecycleRetain:
		return Decision{Kept: keptRetain}
	}
	idle := now.Sub(last)
	switch lc.Policy {
	case common.LifecycleIdleTTL:
		if idle >= lc.TTL {
			return Decision{Delete: true, Reason: ReasonIdleTTL}
		}
		return Decision{Kept: fmt.Sprintf("idle for %s of %s", idle.Round(time.Second), lc.TTL)}
	}
	if idle >= grace {
		if !classExists {
			return Decision{Delete: true, Reason: ReasonOrphan}
		}
		return Decision{Delete: true, Reason: ReasonPerUse}
	}
	return Decision{Kept: keptWithinGrace}
}

// Sweep deletes the managed resources whose lifecycle says so, in the
// driver's dependency order per region (overview section 10). It deletes only
// resources that carry both the deterministic name and the ownership marker,
// and never one of use. Errors of single vendors, regions or resources are
// collected and do not stop the rest. It is the garbage collector's Sweeper.
func (e *Engine) Sweep(ctx context.Context, _ sets.Set[string]) error {
	classes := map[string]*v1alpha1.SCNodeClass{}
	if e.opts.Reader != nil {
		var list v1alpha1.SCNodeClassList
		if err := e.opts.Reader.List(ctx, &list); err != nil {
			return fmt.Errorf("listing SCNodeClasses: %w", err)
		}
		for i := range list.Items {
			classes[list.Items[i].Name] = &list.Items[i]
		}
	}
	full := e.fullSweep(classes)
	var errs []error
	for _, vendor := range e.opts.Registry.Vendors() {
		if err := e.sweepVendor(ctx, vendor, classes, full); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fullSweep says whether this sweep lists every region of the drivers: every
// FullSweepEvery-th sweep, and right after a NodeClass disappeared. The regions
// in use are the regions of the NodeClasses and NodeClaims that exist; resources
// of a region that left that set (the last NodeClass deleted, a region removed
// from one, a vendor block removed) are found by their ownership marker here.
func (e *Engine) fullSweep(classes map[string]*v1alpha1.SCNodeClass) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweeps++
	full := e.sweeps%e.opts.FullSweepEvery == 1 || e.opts.FullSweepEvery == 1
	for name := range e.knownClasses {
		if classes[name] == nil {
			full = true
		}
	}
	e.knownClasses = sets.New[string]()
	for name := range classes {
		e.knownClasses.Insert(name)
	}
	return full
}

type candidate struct {
	ref     vendors.ResourceRef
	spec    vendors.KindSpec
	why     string
	backoff bool
}

func (e *Engine) sweepVendor(
	ctx context.Context, vendor string, classes map[string]*v1alpha1.SCNodeClass, full bool,
) error {
	mr, kinds, ok := e.managed(vendor)
	if !ok || e.opts.Regions == nil {
		return nil
	}
	regions := e.opts.Regions(vendor)
	inUse := slices.Clone(regions)
	e.mu.Lock()
	regions = append(slices.Clone(regions), sets.List(e.watch[vendor])...)
	e.mu.Unlock()
	if full && e.opts.AllRegions != nil {
		regions = append(regions, e.opts.AllRegions(vendor)...)
	}
	slices.Sort(regions)
	regions = slices.Compact(regions)
	if len(regions) == 0 {
		return nil
	}
	refs, err := mr.List(ctx, regions, map[vendors.TagKey]string{
		vendors.TagCluster: e.opts.ClusterName, vendors.TagManagedBy: vendors.ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("listing the managed %s resources: %w", vendor, err)
	}
	e.watchRegions(vendor, inUse, refs)
	planned := e.opts.Clock.Now()
	doomed, errs := e.plan(ctx, mr, vendor, kinds, refs, classes)
	return errors.Join(append(errs, e.deleteOrdered(ctx, mr, vendor, doomed, planned)...)...)
}

// lifecycleRank orders policies by how long they keep a resource.
func lifecycleRank(l Lifecycle) (int, time.Duration) {
	switch l.Policy {
	case common.LifecycleRetain:
		return 3, 0
	case common.LifecycleIdleTTL:
		return 2, l.TTL
	}
	return 1, 0
}

// governing returns the lifecycle that governs a managed resource, whether its
// region is protected by preCreate, and whether a live NodeClass governs it. A
// shared (regional) resource is governed by every live NodeClass that maps to
// it (the region in scope, the kind neither used nor forbidden): the most
// conservative lifecycle wins and any of them may protect it with preCreate. A
// resource no live NodeClass maps to falls back to the NodeClass that created
// it, and without one to the kind's default.
func (e *Engine) governing(
	vendor string, spec vendors.KindSpec, ref vendors.ResourceRef, classes map[string]*v1alpha1.SCNodeClass,
) (lc Lifecycle, preCreated, live bool) {
	if spec.Scope == vendors.ScopeRegion {
		found := false
		for _, nc := range classes {
			regions, ok := nc.Spec.Vendors.Regions(vendor)
			if !ok || (len(regions) > 0 && !slices.Contains(regions, ref.Region)) {
				continue
			}
			cfgs, err := options.ResourceConfig(nc, vendor)
			if err != nil {
				continue
			}
			cfg, has := cfgs[string(ref.Kind)]
			if has && (cfg.Use != nil || cfg.Forbid) {
				continue
			}
			var p *common.Resource
			if has {
				p = &cfg
				preCreated = preCreated || preCreateListed(&cfg, ref.Region)
			}
			l := e.LifecycleOf(spec, p)
			r, ttl := lifecycleRank(l)
			cr, cttl := lifecycleRank(lc)
			if !found || r > cr || (r == cr && ttl > cttl) {
				lc = l
			}
			found = true
		}
		if found {
			return lc, preCreated, true
		}
	}
	nc := classes[ref.Tags[vendors.TagNodeClass]]
	cfg, pre := e.configOf(nc, vendor, ref)
	return e.LifecycleOf(spec, cfg), pre, nc != nil
}

// plan applies the lifecycles to the listed resources and returns the ones to
// delete, with the errors of the checks that could not be made.
func (e *Engine) plan(
	ctx context.Context, mr vendors.ManagedResources, vendor string, kinds map[vendors.ResourceKind]vendors.KindSpec,
	refs []vendors.ResourceRef, classes map[string]*v1alpha1.SCNodeClass,
) (doomed []candidate, errs []error) {
	logger := log.FromContext(ctx).WithName("resources").WithValues("vendor", vendor)
	now := e.opts.Clock.Now()
	prefix := NamePrefix(e.opts.ClusterName)
	counts := map[string]int{}
	for _, ref := range refs {
		// Both the deterministic name and the ownership marker, or it is not ours.
		if !ref.Managed || !strings.HasPrefix(ref.Name, prefix) ||
			ref.Tags[vendors.TagCluster] != e.opts.ClusterName || ref.Tags[vendors.TagManagedBy] != vendors.ManagedByValue {
			continue
		}
		spec, known := kinds[ref.Kind]
		if !known {
			continue
		}
		counts[string(ref.Kind)+"/"+ref.Region]++
		lc, preCreated, live := e.governing(vendor, spec, ref, classes)
		inUse, last, err := mr.InUse(ctx, ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("asking whether %s %s is in use: %w", ref.Kind, ref.ID, err))
			continue
		}
		last, fl := e.observe(vendor, ref, inUse, last, now)

		d := Decide(lc, e.opts.PerUseGrace, live, preCreated, inUse, last, now)
		if !d.Delete {
			logger.V(1).Info("keeping a managed resource", "kind", ref.Kind, "id", ref.ID, "reason", d.Kept)
			continue
		}
		// A resource backing off after a failed deletion is not retried yet, but
		// what depends on it still waits for it.
		doomed = append(doomed, candidate{ref: ref, spec: spec, why: d.Reason, backoff: fl != nil && now.Before(fl.next)})
	}
	e.setManaged(vendor, counts)
	return doomed, errs
}

// configOf returns the NodeClass's configuration of the resource's kind and
// whether its region is protected by preCreate; nil without the NodeClass.
func (e *Engine) configOf(nc *v1alpha1.SCNodeClass, vendor string, ref vendors.ResourceRef) (*common.Resource, bool) {
	if nc == nil {
		return nil, false
	}
	cfgs, err := options.ResourceConfig(nc, vendor)
	if err != nil {
		return nil, false
	}
	c, ok := cfgs[string(ref.Kind)]
	if !ok {
		return nil, false
	}
	return &c, preCreateListed(&c, ref.Region)
}

// observe records what the sweep learned about a resource and returns the time
// of its last use: the latest of the driver's answer, the last launch that
// required it, the first time this process saw it (a restart restarts the idle
// clock, whatever a driver that cannot say when a resource was last used
// reports) and now when it is in use. It also returns the failed deletion.
func (e *Engine) observe(
	vendor string, ref vendors.ResourceRef, inUse bool, last, now time.Time,
) (time.Time, *failure) {
	id := vendor + "/" + ref.ID
	e.mu.Lock()
	defer e.mu.Unlock()
	seen, ok := e.firstSeen[id]
	if !ok {
		seen = now
		e.firstSeen[id] = now
	}
	last = latest(last, e.touched[id], seen)
	if inUse {
		last = now
	}
	e.observed[id] = observation{inUse: inUse, last: last}
	return last, e.failures[id]
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// deleteOrdered deletes in the driver's dependency order within a region: lower
// orders first; when a deletion fails, the kinds that depend on it in the same
// region wait for the next sweep.
func (e *Engine) deleteOrdered(
	ctx context.Context, mr vendors.ManagedResources, vendor string, doomed []candidate, planned time.Time,
) []error {
	logger := log.FromContext(ctx).WithName("resources").WithValues("vendor", vendor)
	slices.SortFunc(doomed, func(a, b candidate) int {
		if c := strings.Compare(a.ref.Region, b.ref.Region); c != 0 {
			return c
		}
		if a.spec.DeleteOrder != b.spec.DeleteOrder {
			return a.spec.DeleteOrder - b.spec.DeleteOrder
		}
		return strings.Compare(a.ref.ID, b.ref.ID)
	})
	var errs []error
	blocked := map[string]int{}
	block := func(c candidate) {
		if _, ok := blocked[c.ref.Region]; !ok {
			blocked[c.ref.Region] = c.spec.DeleteOrder
		}
	}
	for _, c := range doomed {
		if limit, ok := blocked[c.ref.Region]; ok && c.spec.DeleteOrder > limit {
			logger.V(1).Info("waiting for a dependency before deleting", "kind", c.ref.Kind, "id", c.ref.ID)
			continue
		}
		if c.backoff {
			block(c)
			continue
		}
		if err := e.deleteOne(ctx, mr, vendor, c, planned); err != nil {
			errs = append(errs, err)
			block(c)
		}
	}
	return errs
}

func preCreateListed(cfg *common.Resource, region string) bool {
	if cfg.Create == nil || cfg.Create.Timing == nil || cfg.Create.Timing.Mode != common.TimingPreCreate {
		return false
	}
	return slices.Contains(cfg.Create.Timing.Regions, region)
}

func (e *Engine) deleteOne(
	ctx context.Context, mr vendors.ManagedResources, vendor string, c candidate, planned time.Time,
) error {
	ref := c.ref
	class := ref.Tags[vendors.TagNodeClass]
	// Not while a launch ensures the resource: the lock serialises with it, and a
	// launch since the plan was made keeps the resource.
	mu := e.lock(vendor, ref.Kind, ref.Region)
	mu.Lock()
	defer mu.Unlock()
	e.mu.Lock()
	touched := e.touched[vendor+"/"+ref.ID]
	e.mu.Unlock()
	if touched.After(planned) {
		return nil
	}
	logger := log.FromContext(ctx).WithName("resources").WithValues(
		"vendor", vendor, "kind", ref.Kind, "region", ref.Region, "name", ref.Name, "id", ref.ID, "reason", c.why)
	if e.opts.DryRun {
		logger.Info("dry-run: would delete the managed resource")
		e.metrics.operation(vendor, ref.Kind, ref.Region, opDelete, resultDryRun)
		e.event(ctx, class, "Normal", EventDeleted,
			fmt.Sprintf("would delete the %s %s (%s) in %s: %s", ref.Kind, ref.Name, ref.ID, ref.Region, c.why), ref.ID)
		return nil
	}
	id := vendor + "/" + ref.ID
	if err := mr.Delete(ctx, ref); err != nil && !vendors.IsClass(err, vendors.NotFound) {
		e.mu.Lock()
		fl := e.failures[id]
		if fl == nil {
			fl = &failure{}
			e.failures[id] = fl
		}
		fl.attempts++
		fl.next = e.opts.Clock.Now().Add(min(maxBackoff, 30*time.Second<<min(fl.attempts-1, 5)))
		fl.message = err.Error()
		first := fl.attempts == 1
		e.mu.Unlock()
		e.metrics.operation(vendor, ref.Kind, ref.Region, opDelete, resultError)
		logger.Error(err, "deleting the managed resource failed, will retry", "attempts", fl.attempts)
		if first {
			e.event(ctx, class, "Warning", EventDeleteFailed,
				fmt.Sprintf("deleting the %s %s (%s) in %s failed, it is left in place and retried: %v",
					ref.Kind, ref.Name, ref.ID, ref.Region, err), ref.ID)
		}
		return fmt.Errorf("deleting %s %s: %w", ref.Kind, ref.ID, err)
	}
	e.mu.Lock()
	delete(e.failures, id)
	delete(e.observed, id)
	delete(e.touched, id)
	delete(e.seen, id)
	delete(e.firstSeen, id)
	maps.DeleteFunc(e.ensured, func(_ string, v cachedRefs) bool {
		return len(v.refs) == 1 && v.refs[0].ID == ref.ID
	})
	e.mu.Unlock()
	e.metrics.operation(vendor, ref.Kind, ref.Region, opDelete, resultOK)
	logger.Info("deleted the managed resource")
	e.event(ctx, class, "Normal", EventDeleted,
		fmt.Sprintf("deleted the %s %s (%s) in %s: %s", ref.Kind, ref.Name, ref.ID, ref.Region, c.why), ref.ID)
	return nil
}

// setManaged publishes the counts and zeroes the series of kinds and regions
// that have no resource any more.
func (e *Engine) setManaged(vendor string, counts map[string]int) {
	e.mu.Lock()
	old := e.managedKeys[vendor]
	cur := sets.New[string]()
	for k := range counts {
		cur.Insert(k)
	}
	e.managedKeys[vendor] = cur
	e.mu.Unlock()
	for k := range old {
		if !cur.Has(k) {
			kind, region, _ := strings.Cut(k, "/")
			e.metrics.managed.WithLabelValues(vendor, kind, region).Set(0)
		}
	}
	for k, n := range counts {
		kind, region, _ := strings.Cut(k, "/")
		e.metrics.managed.WithLabelValues(vendor, kind, region).Set(float64(n))
	}
}

// watchRegions remembers the regions outside the regions in use where managed
// resources were found, so that the sweeps between two full ones keep looking
// at them until they are empty.
func (e *Engine) watchRegions(vendor string, inUse []string, refs []vendors.ResourceRef) {
	w := sets.New[string]()
	for _, r := range refs {
		if r.Managed && !slices.Contains(inUse, r.Region) {
			w.Insert(r.Region)
		}
	}
	e.mu.Lock()
	e.watch[vendor] = w
	e.mu.Unlock()
}
