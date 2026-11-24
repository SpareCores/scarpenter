/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package imagecopy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Constants of the garbage collection (plan 29, section 6): not flags. The idle
// TTL is --image-copy-idle-ttl (options.DefaultImageCopyIdleTTL).
const (
	// GCInterval is the period of the garbage collection loop.
	GCInterval = 10 * time.Minute
	// UnpinnedGrace is how long no SCNodeClass may pin a release, continuously,
	// before its copies go (rule c).
	UnpinnedGrace = 2 * time.Hour
	// PrimaryGrace is how long after the last launch that resolved a copy the
	// copy stays once a primary image exists (rule b): the launch cache's age.
	PrimaryGrace = 2 * time.Hour
)

// Reasons of a deletion (the metric's reason label and the Event).
const (
	ReasonIdle             = "Idle"
	ReasonPrimaryAvailable = "PrimaryAvailable"
	ReasonUnpinned         = "Unpinned"
	ReasonDuplicate        = "Duplicate"
	ReasonFailed           = "Failed"
	// EventDeleted is the Reason of the Event of a deletion.
	EventDeleted = "ImageCopyDeleted"
)

// Facts are what Decide knows about one copy.
type Facts struct {
	Copy vendors.ImageCopy
	Now  time.Time
	// IdleTTL is --image-copy-idle-ttl.
	IdleTTL time.Duration
	// LastResolve is the manager's in-memory time of the last launch that
	// resolved the copy; zero when none did in this process.
	LastResolve time.Time
	// AvailableSince is when the manager first saw the copy available (a copy
	// adopted after a restart counts from the adoption). A copy can be pending
	// for longer than the idle TTL, and its creation time is the start of the
	// copy, so the idle clock must not run from there. Zero: unknown, the
	// creation time stands.
	AvailableSince time.Time
	// PrimaryAvailable says the vendor's convention finds an image of the
	// release in the region without a copy.
	PrimaryAvailable bool
	// Pinned says a SCNodeClass of the cluster pins the release; otherwise
	// UnpinnedSince is when this process first saw it unpinned.
	Pinned        bool
	UnpinnedSince time.Time
	// Duplicate says another available copy of the key is older.
	Duplicate bool
}

// Decision is what to do with a copy: Delete with a Reason, or keep with Kept
// saying why.
type Decision struct {
	Delete bool
	Reason string
	Kept   string
}

// Decide applies the rules of plan 29, section 6. The sweep never
// deletes a copy an instance runs from, whatever Decide says. A copy is deleted
// when it failed or has been pending
// longer than CopyTimeout (e), is a duplicate (d), a primary image appeared and
// no launch resolved it for PrimaryGrace (b), its release has been unpinned
// for UnpinnedGrace (c), or it has been idle for IdleTTL (a). Idle time counts
// from the latest of the vendor's last-used time, the last resolve, the time
// the copy became available and the creation.
func Decide(f Facts) Decision {
	c := f.Copy
	switch {
	case c.State == vendors.CopyFailed,
		c.State == vendors.CopyPending && f.Now.Sub(c.Created) >= CopyTimeout:
		return Decision{Delete: true, Reason: ReasonFailed}
	case c.State == vendors.CopyPending:
		return Decision{Kept: "copying"}
	case f.Duplicate:
		return Decision{Delete: true, Reason: ReasonDuplicate}
	case f.PrimaryAvailable && (f.LastResolve.IsZero() || f.Now.Sub(f.LastResolve) >= PrimaryGrace):
		return Decision{Delete: true, Reason: ReasonPrimaryAvailable}
	case !f.Pinned && !f.UnpinnedSince.IsZero() && f.Now.Sub(f.UnpinnedSince) >= UnpinnedGrace:
		return Decision{Delete: true, Reason: ReasonUnpinned}
	}
	last := c.Created
	for _, t := range []time.Time{c.LastUsed, f.LastResolve, f.AvailableSince} {
		if t.After(last) {
			last = t
		}
	}
	if idle := f.Now.Sub(last); idle >= f.IdleTTL {
		return Decision{Delete: true, Reason: ReasonIdle}
	}
	return Decision{Kept: "used recently"}
}

// duplicates returns the refs of the available copies that have an older
// available copy of the same key (the oldest, then the lowest ref, stays).
func duplicates(copies []vendors.ImageCopy) map[string]bool {
	sorted := slices.Clone(copies)
	slices.SortFunc(sorted, func(a, b vendors.ImageCopy) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	seen := map[key]bool{}
	out := map[string]bool{}
	for _, c := range sorted {
		if c.State != vendors.CopyAvailable {
			continue
		}
		k := key{c.Region, c.Release}
		if seen[k] {
			out[c.Region+"/"+c.Ref] = true
		}
		seen[k] = true
	}
	return out
}

// PinnedReleases lists the releases the SCNodeClasses with a block for the
// vendor pin, with the regions of the classes that pin each (nil when one of them
// has no region list). A class that names its images or an owner is left out:
// it never uses a copy.
func PinnedReleases(ctx context.Context, r client.Reader, vendor string) ([]Pin, error) {
	var list v1alpha1.SCNodeClassList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing SCNodeClasses: %w", err)
	}
	var pins []Pin
	for i := range list.Items {
		nc := &list.Items[i]
		regions, ok := nc.Spec.Vendors.Regions(vendor)
		if !ok || nc.Spec.Vendors.ImageOwner(vendor) != "" {
			continue
		}
		if cfgs, err := options.ResourceConfig(nc, vendor); err != nil || cfgs[string(selector.ImagesKind)].Use != nil {
			continue
		}
		p := Pin{Channel: string(nc.Spec.Flatcar.Channel), Version: nc.Spec.Flatcar.Version}
		if p.Channel == "" {
			p.Channel = selector.DefaultChannel
		}
		j := slices.IndexFunc(pins, func(q Pin) bool { return q.Channel == p.Channel && q.Version == p.Version })
		switch {
		case j < 0:
			if len(regions) > 0 {
				p.Regions = slices.Clone(regions)
			}
			pins = append(pins, p)
		case len(regions) == 0:
			pins[j].Regions = nil
		case pins[j].Regions != nil:
			for _, region := range regions {
				if !slices.Contains(pins[j].Regions, region) {
					pins[j].Regions = append(pins[j].Regions, region)
				}
			}
		}
	}
	return pins, nil
}

// CollectorOptions configure the Collector.
type CollectorOptions struct {
	// Managers are the managers of the vendors that have copies on.
	Managers []*Manager
	// Pinned lists the releases the SCNodeClasses pin for a vendor
	// (PinnedReleases); a sweep that cannot read them deletes nothing.
	Pinned  func(ctx context.Context, vendor string) ([]Pin, error)
	IdleTTL time.Duration
	// Interval defaults to GCInterval.
	Interval time.Duration
	Clock    clock.Clock
	// Recorder and DeploymentRef publish the Event of a deletion on the
	// controller's Deployment: a copy belongs to no class.
	Recorder      events.Recorder
	DeploymentRef func() *corev1.ObjectReference
}

// Collector is the garbage collection loop of the copies. It runs on the
// leader only.
type Collector struct {
	opts CollectorOptions
	// unpinned is when this process first saw a release without a pin, per
	// vendor; it restarts with the process.
	unpinned map[string]time.Time
}

// NewCollector returns a collector.
func NewCollector(opts CollectorOptions) *Collector {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Interval <= 0 {
		opts.Interval = GCInterval
	}
	return &Collector{opts: opts, unpinned: map[string]time.Time{}}
}

// NeedLeaderElection says that the collector runs on the leader only.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start sweeps every interval until the context ends.
func (c *Collector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("imagecopy-gc")
	for {
		if err := c.Sweep(ctx); err != nil {
			logger.Error(err, "image copy garbage collection had errors")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.opts.Clock.After(c.opts.Interval):
		}
	}
}

// Sweep runs one pass for every manager: it syncs the manager with the
// vendor's copies, bumps the pinned releases into the regions that need them,
// and deletes what Decide says.
func (c *Collector) Sweep(ctx context.Context) error {
	var errs []error
	for _, m := range c.opts.Managers {
		pins, err := c.opts.Pinned(ctx, m.Vendor())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: reading the pinned releases: %w", m.Vendor(), err))
			continue
		}
		if err := c.sweep(ctx, m, pins); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Vendor(), err))
		}
	}
	return errors.Join(errs...)
}

func (c *Collector) sweep(ctx context.Context, m *Manager, pins []Pin) error {
	now := c.opts.Clock.Now()
	var errs []error
	// Sync returns the copies of the regions that answered with the error of
	// those that did not: only what was listed is decided on.
	copies, err := m.Sync(ctx)
	if err != nil {
		if vendors.FailedRegions(err) == nil {
			return err
		}
		errs = append(errs, err)
	}
	// A pinned release has no unpinned time, whatever copies exist now: a
	// release that is pinned again starts its grace afresh.
	for _, p := range pins {
		delete(c.unpinned, m.Vendor()+"/"+p.Channel+"/"+p.Version)
	}
	if err := m.Bump(ctx, pins); err != nil {
		errs = append(errs, err)
	}
	dup := duplicates(copies)
	primary := map[key]bool{}
	for _, cp := range copies {
		pinned := slices.ContainsFunc(pins, func(p Pin) bool {
			return p.Channel == cp.Release.Channel && p.Version == cp.Release.Version
		})
		uk := m.Vendor() + "/" + cp.Release.Channel + "/" + cp.Release.Version
		if _, ok := c.unpinned[uk]; !pinned && !ok {
			c.unpinned[uk] = now
		}
		f := Facts{
			Copy: cp, Now: now, IdleTTL: c.opts.IdleTTL, LastResolve: m.lastResolve(cp.Region, cp.Ref),
			AvailableSince: m.availableSince(cp.Region, cp.Ref),
			Pinned:         pinned, UnpinnedSince: c.unpinned[uk], Duplicate: dup[cp.Region+"/"+cp.Ref],
		}
		if cp.State == vendors.CopyAvailable && (f.LastResolve.IsZero() || now.Sub(f.LastResolve) >= PrimaryGrace) {
			k := key{cp.Region, cp.Release}
			found, ok := primary[k]
			if !ok {
				if found, err = m.primaryAvailable(ctx, cp.Region, cp.Release); err != nil {
					errs = append(errs, err)
					continue
				}
				primary[k] = found
			}
			f.PrimaryAvailable = found
		}
		d := Decide(f)
		if !d.Delete {
			continue
		}
		inUse, err := m.inUse(ctx, cp)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if inUse {
			continue
		}
		if err := m.Delete(ctx, cp, d.Reason); err != nil {
			errs = append(errs, err)
			continue
		}
		c.event(cp, d.Reason)
	}
	return errors.Join(errs...)
}

func (c *Collector) event(cp vendors.ImageCopy, reason string) {
	if c.opts.Recorder == nil || c.opts.DeploymentRef == nil {
		return
	}
	ref := c.opts.DeploymentRef()
	if ref == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: ref, Type: corev1.EventTypeNormal, Reason: EventDeleted,
		Message: fmt.Sprintf("deleted the %s image copy %s of Flatcar %s %s in %s: %s",
			cp.Release.Arch, cp.Ref, cp.Release.Channel, cp.Release.Version, cp.Region, reason),
		DedupeValues: []string{EventDeleted, cp.Region, cp.Ref}, DedupeTimeout: time.Hour,
	})
}
