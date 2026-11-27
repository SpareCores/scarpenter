/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package offered publishes what the catalog offers Karpenter for each
// SCNodeClass (plan 28): a bounded summary in status.vendors.<v>.offered and,
// for the classes that ask for it, the full per-type listing as a ConfigMap.
//
// Both come from one catalog report of the class (Catalog.Explain with the zero
// filter and no NodePool), so they cannot disagree with what Karpenter is given
// or with each other. The status is written with server-side apply under the
// field manager scarpenter-offered and owns nothing but that field; nothing is
// written while the content is unchanged, and at most every Interval.
package offered

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

const (
	// FieldManager owns status.vendors.<v>.offered.
	FieldManager    = "scarpenter-offered"
	kindSCNodeClass = "SCNodeClass"
	// DefaultInterval is the shortest time between two evaluations of a class.
	DefaultInterval = 30 * time.Second
)

// Options configures the controller.
type Options struct {
	Client client.Client
	// Reader reads without the cache: the listing ConfigMaps found at start.
	Reader  client.Reader
	Catalog *catalog.Catalog
	Clock   clock.Clock
	// Quotas adds the quota facts to the listing; nil leaves them out.
	Quotas Quotas
	// Namespace is where the listing ConfigMaps go (the controller's own); empty
	// turns the listing off.
	Namespace string
	// OfferingDetail adds one gauge per offering (--metrics-offering-detail).
	OfferingDetail bool
	// Registerer receives the metrics; nil registers none.
	Registerer prometheus.Registerer
	// Interval defaults to DefaultInterval.
	Interval time.Duration
	// ListingLimit is the cap of a compressed listing; it defaults to
	// MaxListingBytes.
	ListingLimit int
}

// Controller publishes the offered types of the SCNodeClasses.
type Controller struct {
	opts Options

	mu sync.Mutex
	// last is when each class was last evaluated.
	last map[string]time.Time
	// listings is what this controller knows about the ConfigMaps it wrote: the
	// class each belongs to and the digest of its last content, with the
	// owner's UID. found says that the ConfigMaps of an earlier run were read.
	listings map[string]listing
	found    bool

	metrics *metrics
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.ListingLimit <= 0 {
		o.ListingLimit = MaxListingBytes
	}
	c := &Controller{
		opts: o, last: map[string]time.Time{}, listings: map[string]listing{}, metrics: newMetrics(o.OfferingDetail),
	}
	if o.Registerer != nil {
		o.Registerer.MustRegister(c.metrics)
	}
	return c
}

// Register watches SCNodeClasses with the manager; every reconcile requeues
// itself after the interval.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("offered-types").For(&v1alpha1.SCNodeClass{}).Complete(c)
}

// Reconcile evaluates one class and publishes the result.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var nc v1alpha1.SCNodeClass
	if err := c.opts.Client.Get(ctx, req.NamespacedName, &nc); err != nil {
		if apierrors.IsNotFound(err) {
			c.forget(req.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		c.forget(nc.Name)
		return reconcile.Result{}, nil
	}
	if wait := c.wait(nc.Name); wait > 0 {
		return reconcile.Result{RequeueAfter: wait}, nil
	}
	// Without a catalog for the class there is nothing to say.
	st := c.opts.Catalog.Status(nc.Name)
	if !st.Ready && st.Reason != "Stale" {
		return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
	}
	report, err := c.opts.Catalog.Explain(ctx, catalog.Query{Class: nc.Name})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("explaining the offered types of %s: %w", nc.Name, err)
	}
	// Readiness is read once: it changes with time, and the status and the
	// listing of one reconcile must agree on it.
	ready := map[string]bool{}
	for _, vendor := range nc.Spec.Vendors.Names() {
		ready[vendor] = c.opts.Catalog.OfferedReady(vendor)
	}
	// Before a vendor's driver facts are loaded every type of it looks
	// unavailable, and before its launchable types are reported types it cannot
	// launch still look offered: such a vendor is left as it is, the others are
	// reported.
	report.Types = slices.DeleteFunc(report.Types, func(t catalog.TypeReport) bool {
		return !ready[t.Vendor]
	})
	c.mu.Lock()
	c.last[nc.Name] = c.opts.Clock.Now()
	c.mu.Unlock()

	c.metrics.record(nc.Name, report)
	if err := c.applyStatus(ctx, &nc, report.Summaries(), ready); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.publish(ctx, &nc, report, ready); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
}

// wait is how long until the class may be evaluated again.
func (c *Controller) wait(class string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.last[class]; ok {
		if d := c.opts.Interval - c.opts.Clock.Since(last); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Controller) forget(class string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.last, class)
	c.metrics.forget(class)
	// The class owns its ConfigMaps, so they go with it; a class of the same name
	// that comes back gets them written again.
	for name, l := range c.listings {
		if l.class == class {
			delete(c.listings, name)
		}
	}
}

// statusOf converts a summary to the status block, without the time.
func statusOf(s catalog.Summary) v1alpha1.OfferedStatus {
	return v1alpha1.OfferedStatus{
		Types: count(s.Types), Offerings: count(s.Offerings), AvailableOfferings: count(s.AvailableOfferings),
		Dropped: reasonsOf(s.Dropped), Unavailable: reasonsOf(s.Unavailable),
	}
}

func reasonsOf(in []catalog.ReasonCount) []v1alpha1.OfferedReason {
	out := make([]v1alpha1.OfferedReason, 0, len(in))
	for _, rc := range in {
		out = append(out, v1alpha1.OfferedReason{Reason: string(rc.Code), Count: count(rc.Count), Types: rc.Types})
	}
	return out
}

// count converts a count of the catalog, which is far below the limit.
func count(n int) int32 { return int32(min(n, math.MaxInt32)) } //nolint:gosec // clamped

// sameContent compares two blocks apart from the time.
func sameContent(a, b v1alpha1.OfferedStatus) bool {
	a.ObservedAt, b.ObservedAt = metav1.Time{}, metav1.Time{}
	return a.Types == b.Types && a.Offerings == b.Offerings && a.AvailableOfferings == b.AvailableOfferings &&
		slices.EqualFunc(a.Dropped, b.Dropped, sameReason) && slices.EqualFunc(a.Unavailable, b.Unavailable, sameReason)
}

func sameReason(a, b v1alpha1.OfferedReason) bool {
	return a.Reason == b.Reason && a.Count == b.Count && slices.Equal(a.Types, b.Types)
}

// applyStatus writes the blocks of every vendor in one apply, when any of them
// changed: an apply drops the fields this manager owned and left out, so one
// apply per vendor would remove the other vendors' blocks.
func (c *Controller) applyStatus(ctx context.Context, nc *v1alpha1.SCNodeClass,
	summaries map[string]catalog.Summary, ready map[string]bool) error {
	now := metav1.NewTime(c.opts.Clock.Now().Truncate(time.Minute))
	vendors := map[string]any{}
	changed := false
	for _, vendor := range nc.Spec.Vendors.Names() {
		if !ready[vendor] {
			// Kept as it is: left out of the apply, it would be removed.
			if cur := nc.Status.Vendors[vendor].Offered; cur != nil {
				vendors[vendor] = map[string]any{"offered": offeredBlock(*cur)}
			}
			continue
		}
		want := statusOf(summaries[vendor])
		if cur := nc.Status.Vendors[vendor].Offered; cur != nil && sameContent(*cur, want) {
			want.ObservedAt = cur.ObservedAt
		} else {
			want.ObservedAt, changed = now, true
		}
		vendors[vendor] = map[string]any{"offered": offeredBlock(want)}
	}
	if !changed {
		return nil
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       kindSCNodeClass,
		"metadata":   map[string]any{"name": nc.Name},
		"status":     map[string]any{"vendors": vendors},
	}}
	err := c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the offered types status of %s: %w", nc.Name, err)
	}
	return nil
}

// offeredBlock is the apply body of one vendor's block.
func offeredBlock(o v1alpha1.OfferedStatus) map[string]any {
	reasons := func(in []v1alpha1.OfferedReason) []any {
		out := make([]any, len(in))
		for i, r := range in {
			e := map[string]any{"reason": r.Reason, "count": int64(r.Count)}
			if len(r.Types) > 0 {
				e["types"] = toAny(r.Types)
			}
			out[i] = e
		}
		return out
	}
	block := map[string]any{
		"types": int64(o.Types), "offerings": int64(o.Offerings), "availableOfferings": int64(o.AvailableOfferings),
		"observedAt": o.ObservedAt.UTC().Format(time.RFC3339),
	}
	if len(o.Dropped) > 0 {
		block["dropped"] = reasons(o.Dropped)
	}
	if len(o.Unavailable) > 0 {
		block["unavailable"] = reasons(o.Unavailable)
	}
	return block
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
