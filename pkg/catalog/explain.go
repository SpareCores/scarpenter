/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Report says, for every server of a query's vendors, whether it is offered
// and why not (plan 28). It comes from the same evaluation as InstanceTypes.
type Report struct {
	// Types are sorted by vendor and instance type name.
	Types []TypeReport
}

// TypeReport is one server. Dropped is set when the type is not offered at
// all, and then Offerings is empty, except that NodePoolPreFilter and
// NodePoolRequirement drops are the NodePool's, not the catalog's.
type TypeReport struct {
	Vendor       string
	InstanceType string
	APIReference string
	Dropped      Reason
	Offerings    []OfferingReport
	// FitsRequests is false when the query has resource requests the type's
	// allocatable cannot hold (core then answers "had the required resources"
	// instead of "met all requirements").
	FitsRequests bool
}

// OfferingReport is one (region, zone, capacity type) of a type. Reason is
// empty for an available offering. Removed marks an offering the catalog does
// not hand to Karpenter at all (its zone, price or launchability), as opposed to
// one that is listed unavailable.
type OfferingReport struct {
	Region, Zone, CapacityType string
	Available                  bool
	Removed                    bool
	Reason                     Reason
}

// Explain returns the report of the query. A class the catalog does not know,
// or no snapshot, gives an empty report. With the NodePool's Requirements it also
// reports what core would remove from them: the types whose requirements do not
// intersect (NodePoolRequirement, with the first failing key) and, with Requests,
// the types that do not fit them. Vendors the NodePool excludes are not listed.
func (c *Catalog) Explain(_ context.Context, q Query) (Report, error) {
	c.mu.RLock()
	snap := c.snap
	class := c.classes[q.Class]
	c.mu.RUnlock()
	if snap == nil || class == nil {
		return Report{}, nil
	}
	return c.explain(snap, class, q), nil
}

func (c *Catalog) explain(snap *snapshot, class *Class, q Query) Report {
	f := q.Filter
	if q.Requirements != nil {
		f = NewFilter(q.Requirements)
	}
	return reportOf(c.evaluate(snap, class, q.Class, f, true), q)
}

func reportOf(e evaluation, q Query) Report {
	var r Report
	for _, d := range e.drops {
		r.Types = append(r.Types, TypeReport{
			Vendor: d.vendor, InstanceType: InstanceTypeName(d.vendor, d.apiReference), APIReference: d.apiReference,
			Dropped: d.reason, FitsRequests: true,
		})
	}
	for _, ev := range e.types {
		it := ev.bt.it
		tr := TypeReport{
			Vendor: ev.vendor, InstanceType: it.Name, APIReference: ev.bt.server.APIReference, FitsRequests: true,
		}
		if q.Requests != nil {
			tr.FitsRequests = resources.Fits(q.Requests, it.Allocatable())
		}
		if key := firstMismatch(it.Requirements, q.Requirements); key != "" {
			tr.Dropped = Reason{NodePoolRequirement, key}
			r.Types = append(r.Types, tr)
			continue
		}
		for j, i := range ev.keep {
			o := ev.bt.offerings[i]
			off := OfferingReport{
				Region: o.region, Zone: o.zone, CapacityType: o.capacityType, Available: ev.avail[j], Reason: ev.why[j],
			}
			tr.Offerings = append(tr.Offerings, off)
		}
		for _, d := range slices.Concat(ev.bt.removed, ev.filtered) {
			tr.Offerings = append(tr.Offerings, OfferingReport{
				Region: d.region, Zone: d.zone, CapacityType: d.capacityType, Removed: true, Reason: d.reason,
			})
		}
		r.Types = append(r.Types, tr)
	}
	slices.SortFunc(r.Types, func(a, b TypeReport) int {
		if d := strings.Compare(a.Vendor, b.Vendor); d != 0 {
			return d
		}
		return strings.Compare(a.InstanceType, b.InstanceType)
	})
	return r
}

// firstMismatch returns the first key, in sorted order, whose values the two
// requirement sets do not share, by the rules of core's Requirements.Intersects
// (which reports every such key but not as a value): "" when they intersect or
// there are no incoming requirements.
func firstMismatch(existing, incoming scheduling.Requirements) string {
	var keys []string
	for key := range incoming {
		if _, ok := existing[key]; ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		have, want := existing.Get(key), incoming.Get(key)
		if have.HasIntersection(want) {
			continue
		}
		if negative(have) && negative(want) {
			continue
		}
		return key
	}
	return ""
}

func negative(r *scheduling.Requirement) bool {
	return r.Operator() == corev1.NodeSelectorOpNotIn || r.Operator() == corev1.NodeSelectorOpDoesNotExist
}

// states flattens a report to the reasons that are in effect: one entry per
// dropped type and per offering that is removed or unavailable. A type or an
// offering that is fine has none.
func (r Report) states(only func(Code) bool) map[stateKey]Reason {
	out := map[stateKey]Reason{}
	keep := func(code Code) bool {
		if only != nil {
			return only(code)
		}
		return code != "" && code != NodePoolPreFilter
	}
	for _, t := range r.Types {
		if keep(t.Dropped.Code) {
			out[stateKey{vendor: t.Vendor, instanceType: t.InstanceType}] = t.Dropped
		}
		for _, o := range t.Offerings {
			if keep(o.Reason.Code) {
				out[stateKey{t.Vendor, t.InstanceType, o.Region, o.Zone, o.CapacityType}] = o.Reason
			}
		}
	}
	return out
}

type stateKey struct {
	vendor, instanceType, region, zone, capacityType string
}

const (
	// maxLoggedChanges is the number of changes of one report above which they
	// are logged per reason instead of per type and offering.
	maxLoggedChanges = 50
	// maxLoggedTypes is the number of types one per-reason line names.
	maxLoggedTypes = 50
	// available is the "reason" of something that has none, in the log.
	available = "Available"
)

// offeredLog remembers, per class and NodePool, the last logged generation, the
// class and NodePool it was computed for, and its states.
type offeredLog struct {
	mu     sync.Mutex
	gen    map[string]string
	inputs map[string]string
	states map[string]map[stateKey]Reason
}

// logOffered logs, at V(1), the class's reasons that changed since the report of
// the previous generation (class level: no NodePool filter, no pin). The first
// report of a class, and a burst of changes, are logged one line per reason with
// the type names: the log level is raised by a restart, after which every state
// is a change, and a type that never changes again would never be named.
func (c *Catalog) logOffered(ctx context.Context, snap *snapshot, class *Class, gen string) {
	c.logChanges(ctx, snap, class, gen, "", Query{Class: class.Name}, nil)
}

// LogNodePool is logOffered for what depends on a NodePool: the types whose
// requirements do not meet the NodePool's and the types a pinned selection
// profile cannot rank. The class-level reasons are not repeated, and nor are
// the pre-filter's, which core would apply too.
func (c *Catalog) LogNodePool(ctx context.Context, class, nodePool string, reqs scheduling.Requirements) {
	c.mu.RLock()
	snap, cl := c.snap, c.classes[class]
	c.mu.RUnlock()
	if snap == nil || cl == nil {
		return
	}
	c.logChanges(ctx, snap, cl, c.generation(snap), nodePool, Query{Class: class, Requirements: reqs},
		func(code Code) bool { return code == Unrankable || code == NodePoolRequirement })
}

// logChanges logs the changes of the report of the query since the one of the
// previous generation, kept per class and NodePool ("" for the class level);
// only keeps the codes it accepts, nil accepts the class-level ones.
func (c *Catalog) logChanges(ctx context.Context, snap *snapshot, class *Class, gen, nodePool string, q Query,
	only func(Code) bool) {
	logger := log.FromContext(ctx)
	if !logger.V(1).Enabled() {
		return
	}
	if nodePool != "" {
		logger = logger.WithValues("nodepool", nodePool)
	}
	scope := class.Name + "/" + nodePool
	l := &c.offered
	l.mu.Lock()
	defer l.mu.Unlock()
	// An edit of the class or of the NodePool's requirements changes the report
	// without a new generation.
	input := class.Hash + "|" + requirementsKey(q.Requirements)
	if last, ok := l.gen[scope]; ok && l.inputs[scope] == input && (last == gen || genOlder(gen, last)) {
		return
	}
	cur := c.explain(snap, class, q).states(only)
	prev, seen := l.states[scope]
	if l.gen == nil {
		l.gen, l.inputs, l.states = map[string]string{}, map[string]string{}, map[string]map[stateKey]Reason{}
	}
	l.gen[scope], l.inputs[scope], l.states[scope] = gen, input, cur

	type change struct {
		key      stateKey
		was, now Reason
	}
	var changes []change
	for k, now := range cur {
		if was := prev[k]; was.Code != now.Code {
			changes = append(changes, change{k, was, now})
		}
	}
	for k, was := range prev {
		if _, still := cur[k]; !still {
			changes = append(changes, change{k, was, Reason{}})
		}
	}
	slices.SortFunc(changes, func(a, b change) int {
		return strings.Compare(a.key.String(), b.key.String())
	})
	logger = logger.V(1)
	if seen && len(changes) <= maxLoggedChanges {
		for _, ch := range changes {
			code := string(ch.now.Code)
			if code == "" {
				code = available
			}
			logger.Info("offered types changed", "nodeclass", class.Name, "vendor", ch.key.vendor,
				"type", ch.key.instanceType, "zone", ch.key.zone, "capacityType", ch.key.capacityType,
				"reason", code, "detail", ch.now.Detail, "was", string(ch.was.Code))
		}
		return
	}
	// One line per vendor and reason, naming up to maxLoggedTypes types a line.
	type group struct{ vendor, reason string }
	type typeKey struct {
		group
		instanceType string
	}
	listed := map[typeKey]bool{}
	names := map[group][]string{}
	counts := map[group]int{}
	var order []group
	for _, ch := range changes {
		g := group{ch.key.vendor, string(ch.now.Code)}
		if g.reason == "" {
			g.reason = available
		}
		if _, ok := counts[g]; !ok {
			order = append(order, g)
		}
		counts[g]++
		name := ch.key.instanceType
		if ch.now.Detail != "" {
			name += " (" + ch.now.Detail + ")"
		}
		if tk := (typeKey{g, ch.key.instanceType}); !listed[tk] {
			listed[tk] = true
			names[g] = append(names[g], name)
		}
	}
	for _, g := range order {
		for part, n := range slices.Collect(slices.Chunk(names[g], maxLoggedTypes)) {
			logger.Info("offered types changed", "nodeclass", class.Name, "vendor", g.vendor, "reason", g.reason,
				"count", counts[g], "part", strconv.Itoa(part+1), "types", n)
		}
	}
}

func (k stateKey) String() string {
	return strings.Join([]string{k.vendor, k.instanceType, k.region, k.zone, k.capacityType}, "/")
}

// requirementsKey is a canonical string of requirements with every value
// (Requirements.String abbreviates long value lists).
func requirementsKey(reqs scheduling.Requirements) string {
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(reqs)) {
		r := reqs[key]
		values := r.Values()
		slices.Sort(values)
		b.WriteString(r.String() + " " + strings.Join(values, ",") + ";")
	}
	return b.String()
}
