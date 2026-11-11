/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package garbagecollection deletes what SCarpenter launched or minted and no
// NodeClaim owns any more (overview section 10): instances tagged with this
// cluster whose NodeClaim does not exist, and bootstrap-token Secrets of
// NodeClaims that do not exist. Both must be older than a grace period, which
// covers the time between the vendor call and the NodeClaim status update.
// Every deletion is logged with its reason, published as an Event and counted.
//
// Managed vendor resources (plan 06) plug in through Options.Sweepers; a driver
// whose launch leaves resources of its own (vendors.LeftoverCollector) is asked
// to collect them per region after the instance sweep.
package garbagecollection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of the options.
const (
	DefaultInterval    = 2 * time.Minute
	DefaultGracePeriod = 5 * time.Minute
	// TokenNamespace and TokenType identify bootstrap-token Secrets.
	TokenNamespace = bootstrap.TokenNamespace
	TokenType      = bootstrap.TokenType
	// LabelNodeClaim is the label that ties a token Secret to its NodeClaim.
	LabelNodeClaim = bootstrap.LabelNodeClaim
)

// Kinds and results of the deletions counter.
const (
	KindInstance = "instance"
	KindToken    = "bootstrap-token"
	// KindLeftover counts what a vendors.LeftoverCollector deleted (the
	// resources a failed or interrupted launch left besides the instance).
	KindLeftover = "launch-leftover"

	ResultDeleted = "deleted"
	ResultDryRun  = "dry-run"
	ResultError   = "error"
)

// Reasons of a deletion; EventReason is the reason of the Events.
const (
	ReasonNoNodeClaim = "NodeClaimNotFound"
	ReasonUntagged    = "NoNodeClaimTag"
	// ReasonTerminating: the instance has been terminating for longer than the
	// terminating grace (a stopped or powered-off instance is reported
	// terminating by every driver and is never going to disappear by itself).
	ReasonTerminating = "Terminating"
	EventReason       = "GarbageCollected"
)

// Sweeper is an extension point run after the instance and token sweeps, for
// managed vendor resources (plan 06). nodeClaims is the set of existing
// NodeClaim names.
type Sweeper func(ctx context.Context, nodeClaims sets.Set[string]) error

// Options configures the collector.
type Options struct {
	Client client.Client
	// Reader lists the bootstrap token Secrets; nil uses Client. The manager's
	// APIReader keeps the controller from starting a cluster-wide informer on
	// Secrets (its RBAC for them is one namespace).
	Reader      client.Reader
	Registry    *vendors.Registry
	Clock       clock.Clock
	ClusterName string
	// Interval defaults to 2 minutes, GracePeriod to 5 minutes.
	Interval    time.Duration
	GracePeriod time.Duration
	// TerminatingGrace is how long an instance without a NodeClaim may be seen
	// terminating before it is deleted again; defaults to
	// vendors.DefaultTerminatingGrace.
	TerminatingGrace time.Duration
	// DryRun logs and counts what would be deleted without deleting it.
	DryRun   bool
	Recorder events.Recorder
	// Regions returns the regions to list for a vendor.
	Regions func(vendor string) []string
	// DeploymentRef names the object that receives the Event when the
	// instance's NodeClass is gone; nil or a nil result means log only.
	DeploymentRef func() *corev1.ObjectReference
	// OnDeleted is called after an instance was deleted (the quota tracker's
	// NotifyChanged).
	OnDeleted func(vendor, region string)
	Sweepers  []Sweeper
	// Registerer receives the metrics; nil registers none.
	Registerer prometheus.Registerer
}

// Result summarises one sweep.
type Result struct {
	Instances int // deleted (or would be, in dry-run)
	Tokens    int
}

// Collector is the garbage collector. It implements controller-runtime's
// Runnable and runs on the leader only.
type Collector struct {
	opts Options
	// terminating keeps the first sighting of each terminating instance.
	terminating *vendors.TerminatingTracker

	deletions *prometheus.CounterVec
	duration  prometheus.Histogram
	lastSweep prometheus.Gauge
}

// New returns a collector.
func New(opts Options) *Collector {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.GracePeriod <= 0 {
		opts.GracePeriod = DefaultGracePeriod
	}
	c := &Collector{
		opts:        opts,
		terminating: vendors.NewTerminatingTracker(opts.Clock, opts.TerminatingGrace),
		deletions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_gc_deletions_total",
			Help: "Orphans the garbage collector deleted (or would have, in dry-run), by vendor, kind, reason and result.",
		}, []string{"vendor", "kind", "reason", "result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "scarpenter_gc_sweep_duration_seconds",
			Help:    "Duration of a garbage collection sweep.",
			Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
		}),
		lastSweep: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "scarpenter_gc_last_sweep_timestamp_seconds",
			Help: "Unix time the last garbage collection sweep finished.",
		}),
	}
	if opts.Registerer != nil {
		for _, col := range []prometheus.Collector{c.deletions, c.duration, c.lastSweep} {
			register(opts.Registerer, col)
		}
	}
	return c
}

// register replaces a collector of an earlier instance (tests, restarts).
func register(reg prometheus.Registerer, col prometheus.Collector) {
	err := reg.Register(col)
	if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
		reg.Unregister(already.ExistingCollector)
		err = reg.Register(col)
	}
	if err != nil {
		panic(fmt.Sprintf("registering the garbage collection metrics: %v", err))
	}
}

// NeedLeaderElection says that the collector runs on the leader only.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start sweeps every interval until the context ends.
func (c *Collector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("garbagecollection")
	for {
		if res, err := c.Sweep(ctx); err != nil {
			logger.Error(err, "garbage collection sweep had errors")
		} else if res.Instances+res.Tokens > 0 {
			logger.Info("garbage collection sweep finished", "instances", res.Instances, "tokens", res.Tokens)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.opts.Clock.After(c.opts.Interval):
		}
	}
}

// Sweep runs one collection. Errors of single drivers, regions or objects are
// collected and do not stop the rest.
func (c *Collector) Sweep(ctx context.Context) (Result, error) {
	start := c.opts.Clock.Now()
	logger := log.FromContext(ctx).WithName("garbagecollection")
	var res Result
	var errs []error

	var claims karpv1.NodeClaimList
	if err := c.opts.Client.List(ctx, &claims); err != nil {
		return res, fmt.Errorf("listing NodeClaims: %w", err)
	}
	names := sets.New[string]()
	for i := range claims.Items {
		names.Insert(claims.Items[i].Name)
	}

	for _, vendor := range c.opts.Registry.Vendors() {
		n, err := c.sweepVendor(ctx, logger, vendor, names)
		res.Instances += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	n, err := c.sweepTokens(ctx, logger, names)
	res.Tokens = n
	if err != nil {
		errs = append(errs, err)
	}
	for _, s := range c.opts.Sweepers {
		if err := s(ctx, names); err != nil {
			errs = append(errs, err)
		}
	}
	c.duration.Observe(c.opts.Clock.Since(start).Seconds())
	c.lastSweep.Set(float64(c.opts.Clock.Now().Unix()))
	return res, errors.Join(errs...)
}

func (c *Collector) sweepVendor(ctx context.Context, logger logr.Logger, vendor string,
	claims sets.Set[string]) (int, error) {
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok || c.opts.Regions == nil {
		return 0, nil
	}
	tags := map[vendors.TagKey]string{vendors.TagCluster: c.opts.ClusterName}
	var errs []error
	deleted := 0
	// One region at a time, so that a failing region does not hide the others.
	for _, region := range c.opts.Regions(vendor) {
		insts, err := drv.ListInstances(ctx, []string{region}, tags)
		if err != nil {
			errs = append(errs, fmt.Errorf("listing %s instances in %s: %w", vendor, region, err))
			continue
		}
		stillTerminating := map[vendors.InstanceRef]bool{}
		for _, inst := range insts {
			if inst.State == vendors.StateTerminated {
				continue
			}
			// The clock of a terminating instance runs from its first sighting,
			// whether or not its NodeClaim exists yet.
			terminatingTooLong := false
			if inst.State == vendors.StateTerminating {
				stillTerminating[inst.Ref] = true
				terminatingTooLong = c.terminating.Expired(vendor, inst.Ref)
			}
			claim := inst.Tags[vendors.TagNodeClaim]
			if claim != "" && claims.Has(claim) {
				continue
			}
			if c.opts.Clock.Since(inst.CreatedAt) < c.opts.GracePeriod {
				continue
			}
			reason := ReasonNoNodeClaim
			switch {
			case inst.State == vendors.StateTerminating && !terminatingTooLong:
				// The vendor may well be deleting it.
				continue
			case inst.State == vendors.StateTerminating:
				reason = ReasonTerminating
			case claim == "":
				reason = ReasonUntagged
			}
			handled, err := c.deleteInstance(ctx, logger, drv, inst, reason)
			if err != nil {
				errs = append(errs, err)
			}
			if handled {
				deleted++
				if reason == ReasonTerminating {
					// The next delete of it comes after another grace.
					c.terminating.Forget(vendor, inst.Ref)
				}
			}
		}
		c.terminating.Retain(vendor, region, stillTerminating)
		if lc, ok := drv.(vendors.LeftoverCollector); ok && !c.opts.DryRun {
			if err := c.collectLeftovers(ctx, logger, lc, vendor, region, claims); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return deleted, errors.Join(errs...)
}

// collectLeftovers asks a LeftoverCollector driver for one region. The NodeClaim
// check is live, not the sweep's snapshot: a NodeClaim created after the
// snapshot may already have a launch in flight whose resources the driver lists,
// and its launch starts only once this manager's cache holds the NodeClaim, so a
// cache read made after the driver's listing sees it. A read that fails keeps
// the resource.
func (c *Collector) collectLeftovers(ctx context.Context, logger logr.Logger, lc vendors.LeftoverCollector,
	vendor, region string, claims sets.Set[string]) error {
	exists := func(name string) bool {
		if claims.Has(name) {
			return true
		}
		err := c.opts.Client.Get(ctx, types.NamespacedName{Name: name}, &karpv1.NodeClaim{})
		return !apierrors.IsNotFound(err)
	}
	n, err := lc.CollectLeftovers(ctx, region, exists)
	if n > 0 {
		logger.Info("deleted launch leftovers", "vendor", vendor, "region", region, "count", n)
		c.deletions.WithLabelValues(vendor, KindLeftover, ReasonNoNodeClaim, ResultDeleted).Add(float64(n))
		if c.opts.OnDeleted != nil {
			c.opts.OnDeleted(vendor, region)
		}
	}
	if err != nil {
		c.deletions.WithLabelValues(vendor, KindLeftover, ReasonNoNodeClaim, ResultError).Inc()
		return fmt.Errorf("collecting %s launch leftovers in %s: %w", vendor, region, err)
	}
	return nil
}

// deleteInstance deletes (or, in dry-run, only reports) one orphan; it returns
// whether the orphan counts as handled.
func (c *Collector) deleteInstance(ctx context.Context, logger logr.Logger, drv vendors.Driver,
	inst *vendors.Instance, reason string) (bool, error) {
	vendor := drv.Vendor()
	l := logger.WithValues("vendor", vendor, "instance", inst.Ref.String(), "nodeclaim", inst.Tags[vendors.TagNodeClaim],
		"reason", reason, "age", c.opts.Clock.Since(inst.CreatedAt).Round(time.Second).String())
	if c.opts.DryRun {
		l.Info("dry-run: would delete the orphan instance")
		c.deletions.WithLabelValues(vendor, KindInstance, reason, ResultDryRun).Inc()
		c.event(ctx, inst.Tags[vendors.TagNodeClass], "would delete orphan instance %s (%s)", inst.Ref, reason)
		return true, nil
	}
	if err := drv.DeleteInstance(ctx, inst.Ref); err != nil && !vendors.IsClass(err, vendors.NotFound) {
		c.deletions.WithLabelValues(vendor, KindInstance, reason, ResultError).Inc()
		l.Error(err, "deleting the orphan instance failed")
		return false, fmt.Errorf("deleting %s instance %s: %w", vendor, inst.Ref, err)
	}
	l.Info("deleted the orphan instance")
	c.deletions.WithLabelValues(vendor, KindInstance, reason, ResultDeleted).Inc()
	c.event(ctx, inst.Tags[vendors.TagNodeClass], "deleted orphan instance %s (%s)", inst.Ref, reason)
	if c.opts.OnDeleted != nil {
		c.opts.OnDeleted(vendor, inst.Ref.Region())
	}
	return true, nil
}

func (c *Collector) sweepTokens(ctx context.Context, logger logr.Logger, claims sets.Set[string]) (int, error) {
	var secrets corev1.SecretList
	reader := c.opts.Reader
	if reader == nil {
		reader = c.opts.Client
	}
	if err := reader.List(ctx, &secrets, client.InNamespace(TokenNamespace),
		client.HasLabels{LabelNodeClaim}); err != nil {
		return 0, fmt.Errorf("listing bootstrap tokens: %w", err)
	}
	const reason = ReasonNoNodeClaim
	var errs []error
	n := 0
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Type != TokenType || claims.Has(s.Labels[LabelNodeClaim]) ||
			c.opts.Clock.Since(s.CreationTimestamp.Time) < c.opts.GracePeriod {
			continue
		}
		l := logger.WithValues("secret", s.Name, "nodeclaim", s.Labels[LabelNodeClaim], "reason", reason)
		if c.opts.DryRun {
			l.Info("dry-run: would delete the orphan bootstrap token")
			c.event(ctx, s.Labels[bootstrap.LabelNodeClass],
				"would delete the bootstrap token %s of the missing NodeClaim %s (%s)", s.Name, s.Labels[LabelNodeClaim], reason)
			c.deletions.WithLabelValues("", KindToken, reason, ResultDryRun).Inc()
			n++
			continue
		}
		// The UID precondition keeps a re-created Secret of the same name safe.
		err := c.opts.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID})
		if err != nil && !apierrors.IsNotFound(err) {
			c.deletions.WithLabelValues("", KindToken, reason, ResultError).Inc()
			errs = append(errs, fmt.Errorf("deleting bootstrap token %s: %w", s.Name, err))
			continue
		}
		l.Info("deleted the orphan bootstrap token")
		c.deletions.WithLabelValues("", KindToken, reason, ResultDeleted).Inc()
		c.event(ctx, s.Labels[bootstrap.LabelNodeClass], "deleted the bootstrap token %s of the missing NodeClaim %s (%s)",
			s.Name, s.Labels[LabelNodeClaim], reason)
		n++
	}
	return n, errors.Join(errs...)
}

// event publishes on the NodeClass when it still exists, else on the
// Deployment reference; without either nothing is published (the deletion is
// logged anyway).
func (c *Collector) event(ctx context.Context, nodeClass, format string, args ...any) {
	if c.opts.Recorder == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	evt := events.Event{
		Type: corev1.EventTypeNormal, Reason: EventReason, Message: msg,
		DedupeValues: []string{msg}, DedupeTimeout: time.Minute,
	}
	if nodeClass != "" {
		nc := &v1alpha1.SCNodeClass{}
		if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: nodeClass}, nc); err == nil {
			evt.InvolvedObject = nc
			c.opts.Recorder.Publish(evt)
			return
		}
	}
	if c.opts.DeploymentRef != nil {
		if ref := c.opts.DeploymentRef(); ref != nil {
			evt.InvolvedObject = ref
			c.opts.Recorder.Publish(evt)
		}
	}
}
