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
	FieldManager = "scarpenter-offered"
	// DefaultInterval is the shortest time between two evaluations of a class.
	DefaultInterval = 30 * time.Second
)

// Options configures the controller.
type Options struct {
	Client  client.Client
	Catalog *catalog.Catalog
	Clock   clock.Clock
	// Interval defaults to DefaultInterval.
	Interval time.Duration
}

// Controller publishes the offered types of the SCNodeClasses.
type Controller struct {
	opts Options

	mu sync.Mutex
	// last is when each class was last evaluated.
	last map[string]time.Time
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	return &Controller{opts: o, last: map[string]time.Time{}}
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
	// Before the driver facts are loaded every type looks unavailable, and
	// without a catalog for the class there is nothing to say.
	st := c.opts.Catalog.Status(nc.Name)
	if !c.opts.Catalog.FactsLoaded() || (!st.Ready && st.Reason != "Stale") {
		return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
	}
	report, err := c.opts.Catalog.Explain(ctx, catalog.Query{Class: nc.Name})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("explaining the offered types of %s: %w", nc.Name, err)
	}
	c.mu.Lock()
	c.last[nc.Name] = c.opts.Clock.Now()
	c.mu.Unlock()

	summaries := report.Summaries()
	for _, vendor := range nc.Spec.Vendors.Names() {
		if err := c.applyStatus(ctx, &nc, vendor, summaries[vendor]); err != nil {
			return reconcile.Result{}, err
		}
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

func (c *Controller) applyStatus(ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string,
	s catalog.Summary) error {
	want := statusOf(s)
	if cur := nc.Status.Vendors[vendor].Offered; cur != nil && sameContent(*cur, want) {
		return nil
	}
	want.ObservedAt = metav1.NewTime(c.opts.Clock.Now().Truncate(time.Minute))

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
		"types": int64(want.Types), "offerings": int64(want.Offerings), "availableOfferings": int64(want.AvailableOfferings),
		"observedAt": want.ObservedAt.UTC().Format(time.RFC3339),
	}
	if len(want.Dropped) > 0 {
		block["dropped"] = reasons(want.Dropped)
	}
	if len(want.Unavailable) > 0 {
		block["unavailable"] = reasons(want.Unavailable)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status":     map[string]any{"vendors": map[string]any{vendor: map[string]any{"offered": block}}},
	}}
	err := c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the offered types status of %s/%s: %w", nc.Name, vendor, err)
	}
	return nil
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
