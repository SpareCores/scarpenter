/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Report says, for every server of a query's vendors, whether it is offered
// and why not (plan 28). It comes from the same evaluation as InstanceTypes.
type Report struct {
	// Types are sorted by vendor and instance type name.
	Types []TypeReport
}

// TypeReport is one server. Dropped is set when the type is not offered at
// all, and then Offerings is empty.
type TypeReport struct {
	Vendor       string
	InstanceType string
	APIReference string
	Dropped      Reason
	Offerings    []OfferingReport
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
// or no snapshot, gives an empty report.
func (c *Catalog) Explain(_ context.Context, q Query) (Report, error) {
	c.mu.RLock()
	snap := c.snap
	class := c.classes[q.Class]
	c.mu.RUnlock()
	if snap == nil || class == nil {
		return Report{}, nil
	}
	return reportOf(c.evaluate(snap, class, q.Class, q.Filter)), nil
}

func reportOf(e evaluation) Report {
	var r Report
	for _, d := range e.drops {
		r.Types = append(r.Types, TypeReport{
			Vendor: d.vendor, InstanceType: InstanceTypeName(d.vendor, d.apiReference), APIReference: d.apiReference,
			Dropped: d.reason,
		})
	}
	for _, ev := range e.types {
		tr := TypeReport{Vendor: ev.vendor, InstanceType: ev.bt.it.Name, APIReference: ev.bt.server.APIReference}
		for j, i := range ev.keep {
			o := ev.bt.offerings[i]
			tr.Offerings = append(tr.Offerings, OfferingReport{
				Region: o.region, Zone: o.zone, CapacityType: o.capacityType, Available: ev.avail[j], Reason: ev.why[j],
			})
		}
		for _, d := range ev.bt.removed {
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

// states flattens a report to the reasons that are in effect: one entry per
// dropped type and per offering that is removed or unavailable. A type or an
// offering that is fine has none.
func (r Report) states() map[stateKey]Reason {
	out := map[stateKey]Reason{}
	for _, t := range r.Types {
		if t.Dropped.Code != "" {
			out[stateKey{vendor: t.Vendor, instanceType: t.InstanceType}] = t.Dropped
		}
		for _, o := range t.Offerings {
			if o.Reason.Code != "" {
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

// offeredLog remembers, per class, the last logged generation and its states.
type offeredLog struct {
	mu     sync.Mutex
	gen    map[string]string
	states map[string]map[stateKey]Reason
}

// logOffered logs, at V(1), the class's reasons that changed since the report of
// the previous generation (class level: no NodePool filter, no pin). The first
// report of a class, and a burst of changes, are logged one line per reason with
// the type names: the log level is raised by a restart, after which every state
// is a change, and a type that never changes again would never be named.
func (c *Catalog) logOffered(ctx context.Context, snap *snapshot, class *Class, gen string) {
	logger := log.FromContext(ctx)
	if !logger.V(1).Enabled() {
		return
	}
	l := &c.offered
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.gen[class.Name]; ok && (last == gen || genOlder(gen, last)) {
		return
	}
	cur := reportOf(c.evaluate(snap, class, class.Name, Filter{})).states()
	prev, seen := l.states[class.Name]
	if l.gen == nil {
		l.gen, l.states = map[string]string{}, map[string]map[stateKey]Reason{}
	}
	l.gen[class.Name], l.states[class.Name] = gen, cur

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
