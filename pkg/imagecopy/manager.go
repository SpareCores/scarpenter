/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package imagecopy copies the boot image of the pinned Flatcar release into
// regions that have none, when a launch wants the region, and deletes copies
// nothing uses any more (plan 29, ADR-032). The Manager is vendor-neutral: the
// vendor's API sits behind vendors.ImageCopier. The vendor's images are the
// only record; the queue, the failure times and the use times are in memory.
package imagecopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Constants of the behaviour (plan 29, section 3): not flags.
const (
	// MaxInFlight is the most copies in flight per vendor; the rest wait.
	MaxInFlight = 4
	// BackoffMin doubles to BackoffMax after every limit or throttling error
	// of a copy call.
	BackoffMin = time.Minute
	BackoffMax = 15 * time.Minute
	// CopyTimeout is how long a copy may stay pending before it fails.
	CopyTimeout = 2 * time.Hour
	// FailureTTL is how long a failed copy keeps its region reporting
	// ImageCopyFailed before the next demand retries (like hcloud's build retry,
	// but longer: a refusal rarely fixes itself).
	FailureTTL = 6 * time.Hour
	// SourceMissingRetry is how long "no source has the image" is remembered:
	// shorter than FailureTTL, since the source may only be missing for a release
	// that is about to be published.
	SourceMissingRetry = 15 * time.Minute
	// MissingGrace is how long a copy the vendor does not list may be waited for
	// after its start (a listing can lag a start); then it counts as failed, so
	// that a copy deleted by hand does not gate its region until CopyTimeout.
	MissingGrace = 5 * time.Minute
	// PollInterval is how often copies in flight are looked at.
	PollInterval = 30 * time.Second
	// TouchInterval is the most often a use is recorded at the vendor.
	TouchInterval = 24 * time.Hour
)

// Pin is a release the SCNodeClasses of a vendor pin and may copy (the
// architecture is not part of a pin). Regions are the regions of the classes
// that pin it; nil means any region.
type Pin struct {
	Channel, Version string
	Regions          []string
}

type key struct {
	region string
	rel    vendors.Release
}

func (k key) String() string {
	return fmt.Sprintf("%s %s %s in %s", k.rel.Channel, k.rel.Version, k.rel.Arch, k.region)
}

type phase int

const (
	phaseQueued phase = iota
	phaseCopying
	phaseAvailable
	phaseFailed
	phaseSourceMissing
)

// job is one image of one region: the state of a copy this process knows.
type job struct {
	key
	phase phase
	// seq is the demand order: a unit gets consecutive numbers.
	seq uint64
	// enqueued is when the job was queued: with the key it makes the token of
	// the copy, stable over the retries of a start and new for a new demand.
	enqueued time.Time
	ref      string
	// source is "<region>/<ref>" of the image being copied from.
	source string
	// startedAt is when the copy started (or was created, if adopted); until is
	// when a failure or a missing source is forgotten.
	startedAt, until time.Time
	// retryAt holds a queued job back (its sources could not be listed) while
	// the rest of the queue goes on.
	retryAt time.Time
	created time.Time
	// availableAt is when the manager first saw the copy available (adoption
	// time for a copy found available); the idle clock of the garbage collector
	// starts there, not at the copy's creation.
	availableAt   time.Time
	code, message string
	// lastResolve is the last time a launch resolved the copy; lastTouch the
	// last use recorded at the vendor.
	lastResolve, lastTouch time.Time
}

// Options configure a Manager.
type Options struct {
	Vendor  string
	Cluster string
	Driver  vendors.ImageCopier
	Clock   clock.Clock
	Log     logr.Logger
	// Regions lists every region of the vendor, for the garbage collector's
	// listing of the cluster's copies.
	Regions func(ctx context.Context) ([]string, error)
	// Invalidate is called with the region and ref of every image the manager
	// deleted, for the caches that hold the ID (the CloudProvider's).
	Invalidate func(region, ref string)
	// Metrics are shared by the managers; nil keeps private, unregistered ones.
	Metrics *Metrics
}

// Manager is the copy manager of one vendor. It implements vendors.ImageCopies.
type Manager struct {
	opts Options
	clk  clock.Clock
	log  logr.Logger
	m    *Metrics

	signal chan struct{}
	kick   chan struct{}

	tickMu sync.Mutex
	mu     sync.Mutex
	jobs   map[key]*job
	seq    uint64
	loaded map[string]bool
	archs  map[string][]string
	// primaryFound are the keys the bump found a primary image for, so that a
	// signal does not ask the vendor again; Sync (every sweep) forgets them.
	primaryFound map[key]bool
	// sources caches CopySources per release for the reports (sourceMissing).
	sources      map[vendors.Release]cachedSources
	backoff      time.Duration
	backoffUntil time.Time
}

var _ vendors.ImageCopies = (*Manager)(nil)

// New returns a manager.
func New(opts Options) *Manager {
	m := &Manager{
		opts: opts, clk: opts.Clock, log: opts.Log, m: opts.Metrics,
		signal: make(chan struct{}, 1), kick: make(chan struct{}, 1),
		jobs: map[key]*job{}, loaded: map[string]bool{}, archs: map[string][]string{},
		primaryFound: map[key]bool{}, sources: map[vendors.Release]cachedSources{},
	}
	if m.clk == nil {
		m.clk = clock.RealClock{}
	}
	if m.log.GetSink() == nil {
		m.log = logr.Discard()
	}
	if m.m == nil {
		m.m = NewMetrics(nil)
	}
	m.log = m.log.WithName("imagecopy").WithValues("vendor", opts.Vendor)
	return m
}

// Vendor is the vendor the manager copies for.
func (m *Manager) Vendor() string { return m.opts.Vendor }

// Signal receives a value after every change a NodeClass status shows (a copy
// queued, available, failed or deleted); sends coalesce.
func (m *Manager) Signal() <-chan struct{} { return m.signal }

func (m *Manager) notify() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

// NeedLeaderElection says that the manager's loop runs on the leader only.
func (m *Manager) NeedLeaderElection() bool { return true }

// Start runs Tick every PollInterval, and at once after a demand, until the
// context ends.
func (m *Manager) Start(ctx context.Context) error {
	for {
		if err := m.Tick(ctx); err != nil {
			m.log.Error(err, "image copy tick had errors")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-m.kick:
		case <-m.clk.After(PollInterval):
		}
	}
}

func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Lookup implements vendors.ImageCopies (plan 29, section 4, steps 4 and 5).
func (m *Manager) Lookup(ctx context.Context, req vendors.ImageRequest, demand bool) (*vendors.Image, error) {
	// TODO(plan 29, step 5): on Alibaba the owner "self" is the default, not an
	// owner; it must not opt a request out of copies.
	if !req.Use.IsDefault() || req.Owner != "" {
		return nil, nil
	}
	if err := m.ensure(ctx, req.Region); err != nil {
		return nil, err
	}
	rel := vendors.Release{Channel: req.Channel, Version: req.Version, Arch: req.Arch}
	k := key{req.Region, rel}
	if !demand {
		// A report of a key with no state says whether a copy could be made at
		// all, so that an unpublished release is not "copied on demand".
		m.mu.Lock()
		known := m.jobs[k] != nil
		m.mu.Unlock()
		if !known {
			missing, err := m.sourceMissing(ctx, rel, req.Region)
			if err != nil {
				return nil, err
			}
			if missing {
				return nil, notFound(vendors.CodeImageCopySourceMissing,
					"no source region has the %s image of Flatcar %s %s for the copy into %s",
					rel.Arch, rel.Channel, rel.Version, req.Region)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clk.Now()
	j := m.jobs[k]
	if j != nil && (j.phase == phaseFailed || j.phase == phaseSourceMissing) && !now.Before(j.until) {
		// The failure is forgotten: the key is on demand again.
		delete(m.jobs, k)
		m.gauges()
		j = nil
	}
	if j == nil {
		if !demand {
			return nil, notFound(vendors.CodeImageCopyOnDemand,
				"no %s image of Flatcar %s %s in %s; a copy is made when a launch wants the region",
				req.Arch, req.Channel, req.Version, req.Region)
		}
		m.enqueueUnit(req.Region, req.Channel, req.Version, m.archs[req.Region])
		m.notify()
		m.wake()
		j = m.jobs[k]
		if j == nil {
			// The driver does not serve the architecture in the region.
			return nil, nil
		}
	}
	switch j.phase {
	case phaseAvailable:
		if demand {
			j.lastResolve = now
		}
		return &vendors.Image{Ref: j.ref, Channel: rel.Channel, Version: rel.Version}, nil
	case phaseFailed:
		return nil, notFound(vendors.CodeImageCopyFailed, "the copy of %s failed (%s: %s), retried after %s",
			k, j.code, j.message, j.until.UTC().Format(time.RFC3339))
	case phaseSourceMissing:
		return nil, notFound(vendors.CodeImageCopySourceMissing,
			"no source region has the %s image of Flatcar %s %s for the copy into %s",
			rel.Arch, rel.Channel, rel.Version, req.Region)
	case phaseCopying:
		return nil, notFound(vendors.CodeImageCopying, "copying %s from %s, started %s",
			k, j.source, j.startedAt.UTC().Format(time.RFC3339))
	}
	return nil, notFound(vendors.CodeImageCopying, "the copy of %s is queued", k)
}

type cachedSources struct {
	srcs  []vendors.ImageSource
	until time.Time
}

// sourceMissing says whether no region but the destination has the release's
// image to copy from. The vendor's answer is kept for SourceMissingRetry, so a
// report costs a vendor call once per release in that time.
func (m *Manager) sourceMissing(ctx context.Context, rel vendors.Release, region string) (bool, error) {
	m.mu.Lock()
	cs, ok := m.sources[rel]
	m.mu.Unlock()
	if !ok || !m.clk.Now().Before(cs.until) {
		srcs, err := m.opts.Driver.CopySources(ctx, rel)
		if err != nil {
			return false, fmt.Errorf("listing the sources of %s %s %s: %w", rel.Channel, rel.Version, rel.Arch, err)
		}
		cs = cachedSources{srcs: srcs, until: m.clk.Now().Add(SourceMissingRetry)}
		m.mu.Lock()
		m.sources[rel] = cs
		m.mu.Unlock()
	}
	return !slices.ContainsFunc(cs.srcs, func(s vendors.ImageSource) bool { return s.Region != region }), nil
}

func notFound(code, format string, args ...any) error {
	return vendors.NewError(vendors.NotFound, code, fmt.Errorf(format, args...))
}

// ensure reads the region's copies and architectures the first time the
// region is asked about (a restarted or new leader adopts what is there).
func (m *Manager) ensure(ctx context.Context, region string) error {
	m.mu.Lock()
	loaded, archs := m.loaded[region], m.archs[region]
	m.mu.Unlock()
	if loaded && archs != nil {
		return nil
	}
	var copies []vendors.ImageCopy
	if !loaded {
		var err error
		if copies, err = m.opts.Driver.ListCopies(ctx, []string{region}); err != nil {
			return fmt.Errorf("listing the image copies of %s: %w", region, err)
		}
	}
	if archs == nil {
		var err error
		if archs, err = m.opts.Driver.Architectures(ctx, region); err != nil {
			return fmt.Errorf("listing the architectures of %s: %w", region, err)
		}
		if archs == nil {
			archs = []string{}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded[region] {
		m.adopt(copies)
		m.loaded[region] = true
	}
	m.archs[region] = archs
	return nil
}

// adopt takes the vendor's images as the state of the keys that have none: a
// pending image is a copy in flight, an available one is served. Of several
// available copies of a key (a race) the oldest wins.
func (m *Manager) adopt(copies []vendors.ImageCopy) {
	sorted := slices.Clone(copies)
	slices.SortFunc(sorted, func(a, b vendors.ImageCopy) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	for _, c := range sorted {
		if c.Release.Version == "" || c.Release.Arch == "" || c.State == vendors.CopyFailed {
			continue // the garbage collector deletes failed images
		}
		k := key{c.Region, c.Release}
		if m.jobs[k] != nil {
			continue
		}
		j := &job{key: k, ref: c.Ref, source: c.Source, startedAt: c.Created, created: c.Created}
		j.phase = phaseCopying
		if c.State == vendors.CopyAvailable {
			j.phase, j.availableAt = phaseAvailable, m.clk.Now()
		}
		m.jobs[k] = j
	}
	m.gauges()
}

// enqueueUnit queues the release in the region for every architecture that has
// no state yet: both architectures are copied together, since the image gate is
// per region. The caller holds the lock.
func (m *Manager) enqueueUnit(region, channel, version string, archs []string) {
	for _, arch := range archs {
		k := key{region, vendors.Release{Channel: channel, Version: version, Arch: arch}}
		if m.jobs[k] != nil {
			continue
		}
		m.seq++
		m.jobs[k] = &job{key: k, phase: phaseQueued, seq: m.seq, enqueued: m.clk.Now()}
	}
	m.gauges()
}

// gauges publishes the counts of the states. The caller holds the lock.
func (m *Manager) gauges() {
	n := map[string]float64{}
	for _, j := range m.jobs {
		switch j.phase {
		case phaseQueued:
			n["queued"]++
		case phaseCopying:
			n["pending"]++
		case phaseAvailable:
			n["available"]++
		case phaseFailed, phaseSourceMissing:
			n["failed"]++
		}
	}
	for _, s := range copyStates {
		m.m.copies.WithLabelValues(m.opts.Vendor, s).Set(n[s])
	}
}

// Tick does the periodic work: it looks at the copies in flight, starts queued
// copies while fewer than MaxInFlight run, and records uses at the vendor.
func (m *Manager) Tick(ctx context.Context) error {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	err := m.poll(ctx)
	m.start(ctx)
	return errors.Join(err, m.touch(ctx))
}

type failure struct {
	j    *job
	copy *vendors.ImageCopy
	code string
	msg  string
}

// poll reads the regions with copies in flight: an available copy is served,
// a failed or timed out one is deleted and remembered as failed.
func (m *Manager) poll(ctx context.Context) error {
	m.mu.Lock()
	var regions []string
	for _, j := range m.jobs {
		if j.phase == phaseCopying && !slices.Contains(regions, j.region) {
			regions = append(regions, j.region)
		}
	}
	m.mu.Unlock()
	if len(regions) == 0 {
		return nil
	}
	slices.Sort(regions)
	listed, err := m.opts.Driver.ListCopies(ctx, regions)
	if err != nil {
		return fmt.Errorf("polling the image copies: %w", err)
	}
	byRef := map[string]*vendors.ImageCopy{}
	for i := range listed {
		byRef[listed[i].Region+"/"+listed[i].Ref] = &listed[i]
	}

	now := m.clk.Now()
	var failures []failure
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.phase != phaseCopying || !slices.Contains(regions, j.region) {
			continue
		}
		c := byRef[j.region+"/"+j.ref]
		switch {
		case c != nil && c.State == vendors.CopyAvailable:
			j.phase, j.created, j.availableAt = phaseAvailable, c.Created, now
			m.m.completed.WithLabelValues(m.opts.Vendor, j.region).Inc()
			m.m.duration.WithLabelValues(m.opts.Vendor).Observe(now.Sub(j.startedAt).Seconds())
			m.notify()
		case c != nil && c.State == vendors.CopyFailed:
			failures = append(failures, failure{j: j, copy: c, code: "CopyFailed", msg: c.Message})
		case c == nil && now.Sub(j.startedAt) >= MissingGrace:
			failures = append(failures, failure{j: j, code: "CopyMissing",
				msg: fmt.Sprintf("the vendor does not list the copy %s", j.ref)})
		case now.Sub(j.startedAt) >= CopyTimeout:
			failures = append(failures, failure{j: j, copy: c, code: "CopyTimeout",
				msg: fmt.Sprintf("still pending after %s", CopyTimeout)})
		}
	}
	m.gauges()
	m.mu.Unlock()

	var errs []error
	for _, f := range failures {
		if f.copy != nil {
			if err := m.deleteImage(ctx, *f.copy, ReasonFailed); err != nil {
				// The garbage collector retries the deletion (rule e).
				errs = append(errs, err)
			}
		}
		m.fail(f.j, f.code, f.msg)
	}
	return errors.Join(errs...)
}

// fail marks the copy failed for FailureTTL.
func (m *Manager) fail(j *job, code, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j.phase, j.code, j.message, j.ref = phaseFailed, code, msg, ""
	j.until = m.clk.Now().Add(FailureTTL)
	m.m.failed.WithLabelValues(m.opts.Vendor, j.region, code).Inc()
	m.log.Info("image copy failed", "region", j.region, "channel", j.rel.Channel, "version", j.rel.Version,
		"arch", j.rel.Arch, "code", code, "message", msg)
	m.gauges()
	m.notify()
}

// start starts the queued copies in demand order while a slot is free, one
// source choice per release. A limit or throttling error keeps the copy queued
// and pauses all starts of the vendor with a doubling backoff.
func (m *Manager) start(ctx context.Context) {
	sources := map[vendors.Release][]vendors.ImageSource{}
	for {
		m.mu.Lock()
		now := m.clk.Now()
		if now.Before(m.backoffUntil) {
			m.mu.Unlock()
			return
		}
		inflight := 0
		var next *job
		for _, j := range m.jobs {
			switch {
			case j.phase == phaseCopying:
				inflight++
			case j.phase == phaseQueued && !now.Before(j.retryAt) && (next == nil || j.seq < next.seq):
				next = j
			}
		}
		if next == nil || inflight >= MaxInFlight {
			m.mu.Unlock()
			return
		}
		k, enqueued := next.key, next.enqueued
		m.mu.Unlock()

		srcs, ok := sources[k.rel]
		if !ok {
			var err error
			if srcs, err = m.opts.Driver.CopySources(ctx, k.rel); err != nil {
				// One release's failing sources must not stall the queue: hold this
				// job back and go on with the others.
				m.mu.Lock()
				next.retryAt = m.clk.Now().Add(BackoffMin)
				m.mu.Unlock()
				m.log.Info("the sources of an image copy cannot be listed, trying again later",
					"region", k.region, "version", k.rel.Version, "arch", k.rel.Arch, "error", err.Error())
				continue
			}
			sources[k.rel] = srcs
		}
		i := slices.IndexFunc(srcs, func(s vendors.ImageSource) bool { return s.Region != k.region })
		if i < 0 {
			m.mu.Lock()
			next.phase, next.until = phaseSourceMissing, m.clk.Now().Add(SourceMissingRetry)
			m.log.Info("no source for an image copy", "region", k.region, "version", k.rel.Version, "arch", k.rel.Arch)
			m.gauges()
			m.notify()
			m.mu.Unlock()
			continue
		}
		src := srcs[i]
		ref, err := m.opts.Driver.StartCopy(ctx, src, k.region, k.rel, m.tags(k.rel, src), m.token(k, enqueued))
		if err != nil {
			if vendors.IsClass(err, vendors.QuotaExceeded) || vendors.IsClass(err, vendors.Retryable) {
				m.mu.Lock()
				m.backoff = min(max(2*m.backoff, BackoffMin), BackoffMax)
				m.backoffUntil = m.clk.Now().Add(m.backoff)
				m.mu.Unlock()
				m.log.Info("image copy limited, waiting", "region", k.region, "backoff", m.backoff, "error", err.Error())
				return
			}
			m.fail(next, codeOf(err), err.Error())
			continue
		}
		m.mu.Lock()
		m.backoff = 0
		next.phase, next.ref, next.source = phaseCopying, ref, src.Region+"/"+src.Ref
		next.startedAt = m.clk.Now()
		m.m.started.WithLabelValues(m.opts.Vendor, k.region).Inc()
		m.gauges()
		m.notify()
		m.mu.Unlock()
	}
}

func codeOf(err error) string {
	if c := vendors.CodeOf(err); c != "" {
		return c
	}
	return vendors.CodeUnknown
}

func (m *Manager) tags(rel vendors.Release, src vendors.ImageSource) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster:     m.opts.Cluster,
		vendors.TagManagedBy:   vendors.ManagedByValue,
		vendors.TagResource:    vendors.ResourceKindImageCopy,
		vendors.TagImageArch:   rel.Arch,
		vendors.TagImageSource: src.Region + "/" + src.Ref,
	}
}

// token derives the idempotency token of a copy from the cluster and the key,
// and the time the job was queued: the retries of one start repeat the token
// (a start whose answer was lost does not copy twice), a new demand after a
// failure, also after a restart, never repeats a failed copy's. A restarted
// leader adopts the copy in flight by listing, before it queues anything.
func (m *Manager) token(k key, enqueued time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		m.opts.Cluster, k.region, k.rel.Channel, k.rel.Version, k.rel.Arch, fmt.Sprint(enqueued.UnixNano())}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// touch records at the vendor the launches of copies it does not track itself
// (a no-op call for AWS), at most once per TouchInterval and copy.
func (m *Manager) touch(ctx context.Context) error {
	type due struct {
		j  *job
		c  vendors.ImageCopy
		at time.Time
	}
	var todo []due
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.phase == phaseAvailable && j.lastResolve.After(j.lastTouch) &&
			(j.lastTouch.IsZero() || j.lastResolve.Sub(j.lastTouch) >= TouchInterval) {
			todo = append(todo, due{j, j.asCopy(), j.lastResolve})
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, d := range todo {
		if err := m.opts.Driver.TouchCopy(ctx, d.c, d.at); err != nil {
			errs = append(errs, fmt.Errorf("recording the use of %s: %w", d.c.Ref, err))
			continue
		}
		m.mu.Lock()
		d.j.lastTouch = d.at
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (j *job) asCopy() vendors.ImageCopy {
	return vendors.ImageCopy{Region: j.region, Ref: j.ref, Release: j.rel, Source: j.source,
		State: vendors.CopyAvailable, Created: j.created}
}

// Sync reads every region's copies of the cluster, makes the state match them
// (a copy deleted by hand is forgotten, one found is adopted) and returns them.
func (m *Manager) Sync(ctx context.Context) ([]vendors.ImageCopy, error) {
	regions, err := m.opts.Regions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the regions: %w", err)
	}
	copies, err := m.opts.Driver.ListCopies(ctx, regions)
	if err != nil {
		return nil, fmt.Errorf("listing the image copies: %w", err)
	}
	listed := map[string]bool{}
	for _, c := range copies {
		listed[c.Region+"/"+c.Ref] = true
	}
	m.mu.Lock()
	changed := false
	type image struct{ region, ref string }
	var dropped []image
	for k, j := range m.jobs {
		if j.phase == phaseAvailable && slices.Contains(regions, k.region) && !listed[k.region+"/"+j.ref] {
			delete(m.jobs, k)
			dropped = append(dropped, image{k.region, j.ref})
			changed = true
		}
	}
	m.primaryFound = map[key]bool{}
	for _, r := range regions {
		m.loaded[r] = true
	}
	n := len(m.jobs)
	m.adopt(copies)
	if changed || len(m.jobs) != n {
		m.notify()
	}
	m.gauges()
	m.mu.Unlock()
	// A copy deleted behind the manager's back must leave the caches that hold
	// its ID too, or a launch is served a deleted image.
	if m.opts.Invalidate != nil {
		for _, d := range dropped {
			m.opts.Invalidate(d.region, d.ref)
		}
	}
	return copies, nil
}

// lastResolve is the last time a launch resolved the copy, zero if none did in
// this process.
func (m *Manager) lastResolve(region, ref string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, j := range m.jobs {
		if k.region == region && j.ref == ref {
			return j.lastResolve
		}
	}
	return time.Time{}
}

// availableSince is when the copy became available as the manager saw it. A
// copy whose job still polls (the listing already shows it available) counts as
// available now: the poll settles it within a poll interval, and until then it
// must not look idle since its creation.
func (m *Manager) availableSince(region, ref string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, j := range m.jobs {
		if k.region == region && j.ref == ref {
			if j.phase == phaseCopying {
				return m.clk.Now()
			}
			return j.availableAt
		}
	}
	return time.Time{}
}

// primaryAvailable says whether the vendor's convention finds an image of the
// release in the region without a copy (garbage collection rule b).
func (m *Manager) primaryAvailable(ctx context.Context, region string, rel vendors.Release) (bool, error) {
	_, found, err := m.opts.Driver.Primary(ctx, region, rel)
	return found, err
}

// inUse says whether an instance of the account runs from the copy.
func (m *Manager) inUse(ctx context.Context, c vendors.ImageCopy) (bool, error) {
	return m.opts.Driver.CopyInUse(ctx, c)
}

// Delete deletes a copy and forgets it: the vendor's image and snapshots, the
// manager's state, and through Options.Invalidate every cache that holds the ID.
func (m *Manager) Delete(ctx context.Context, c vendors.ImageCopy, reason string) error {
	if err := m.deleteImage(ctx, c, reason); err != nil {
		return err
	}
	m.mu.Lock()
	k := key{c.Region, c.Release}
	forgot := false
	if j := m.jobs[k]; j != nil && j.ref == c.Ref {
		delete(m.jobs, k)
		forgot = true
	}
	m.gauges()
	m.mu.Unlock()
	if forgot {
		// Another copy of the key (a duplicate) may be left: serve it.
		if left, err := m.opts.Driver.ListCopies(ctx, []string{c.Region}); err == nil {
			m.mu.Lock()
			m.adopt(left)
			m.mu.Unlock()
		}
	}
	m.notify()
	return nil
}

// deleteImage deletes the vendor's image, counts it and invalidates the caches.
func (m *Manager) deleteImage(ctx context.Context, c vendors.ImageCopy, reason string) error {
	if err := m.opts.Driver.DeleteCopy(ctx, c); err != nil {
		return fmt.Errorf("deleting the image copy %s in %s: %w", c.Ref, c.Region, err)
	}
	m.m.deleted.WithLabelValues(m.opts.Vendor, reason).Inc()
	if m.opts.Invalidate != nil {
		m.opts.Invalidate(c.Region, c.Ref)
	}
	m.log.Info("image copy deleted", "region", c.Region, "ref", c.Ref, "channel", c.Release.Channel,
		"version", c.Release.Version, "arch", c.Release.Arch, "reason", reason)
	return nil
}

// Bump enqueues the pinned releases into every region where the cluster has a
// copy of another release that an instance runs from, so that drift replaces
// the node in the same region after a short copy instead of leaving it
// (plan 29, section 6). A release the vendor already has an image of there, or
// that has a state, is left alone.
func (m *Manager) Bump(ctx context.Context, pins []Pin) error {
	m.mu.Lock()
	byRegion := map[string][]vendors.ImageCopy{}
	for _, j := range m.jobs {
		if j.phase == phaseAvailable || j.phase == phaseCopying {
			c := j.asCopy()
			if j.phase == phaseCopying {
				c.State = vendors.CopyPending
			}
			byRegion[j.region] = append(byRegion[j.region], c)
		}
	}
	m.mu.Unlock()

	var errs []error
	for region, copies := range byRegion {
		for _, p := range pins {
			if err := m.bumpRegion(ctx, region, copies, p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) bumpRegion(ctx context.Context, region string, copies []vendors.ImageCopy, p Pin) error {
	if p.Regions != nil && !slices.Contains(p.Regions, region) {
		return nil
	}
	if err := m.ensure(ctx, region); err != nil {
		return err
	}
	// The architectures of the release that have no state yet: the cheap check
	// first, so that a region that was bumped before costs no vendor call.
	var need []string
	m.mu.Lock()
	for _, arch := range m.archs[region] {
		k := key{region, vendors.Release{Channel: p.Channel, Version: p.Version, Arch: arch}}
		if m.jobs[k] == nil && !m.primaryFound[k] {
			need = append(need, arch)
		}
	}
	m.mu.Unlock()
	if len(need) == 0 {
		return nil
	}
	running := false
	for _, c := range copies {
		if c.Release.Channel == p.Channel && c.Release.Version == p.Version || c.State != vendors.CopyAvailable {
			continue
		}
		inUse, err := m.opts.Driver.CopyInUse(ctx, c)
		if err != nil {
			return fmt.Errorf("checking whether %s is in use: %w", c.Ref, err)
		}
		if inUse {
			running = true
			break
		}
	}
	if !running {
		return nil
	}
	var enqueue []string
	for _, arch := range need {
		rel := vendors.Release{Channel: p.Channel, Version: p.Version, Arch: arch}
		_, found, err := m.opts.Driver.Primary(ctx, region, rel)
		if err != nil {
			return fmt.Errorf("looking for the primary image in %s: %w", region, err)
		}
		if found {
			m.mu.Lock()
			m.primaryFound[key{region, rel}] = true
			m.mu.Unlock()
		} else {
			enqueue = append(enqueue, arch)
		}
	}
	if len(enqueue) == 0 {
		return nil
	}
	m.mu.Lock()
	m.enqueueUnit(region, p.Channel, p.Version, enqueue)
	m.mu.Unlock()
	m.notify()
	m.wake()
	return nil
}
