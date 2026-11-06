/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package quotastatus makes a quota wait visible (overview section 5.9): where
// every offering a NodeClass allows in a region and capacity type is blocked by
// quota, it lists the scope in status.vendors.<v>.quota, sets the vendor
// condition QuotaAvailable=False (QuotaExhausted, or QuotaZero when the limit
// itself is 0 and a quota increase is needed) and emits a rate-limited Event.
// QuotaAvailable is informational: the placement itself is the catalog's.
//
// The status is written with server-side apply under the field manager
// scarpenter-quota and owns nothing but those two fields, so that the NodeClass
// controller can write the rest of the status.
package quotastatus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/quota"
)

// Names of the condition reasons and the field manager.
const (
	FieldManager    = "scarpenter-quota"
	ReasonAvailable = "QuotaAvailable"
	ReasonExhausted = "QuotaExhausted"
	ReasonZero      = "QuotaZero"

	// DefaultInterval is how often the status is recomputed.
	DefaultInterval = 15 * time.Second
	// MaxEntries caps status.vendors.<v>.quota.
	MaxEntries = 64
	// eventDedupe is the minimum time between equal Events.
	eventDedupe = 10 * time.Minute
	maxMessage  = 1000
)

// Options configures the controller.
type Options struct {
	Client  client.Client
	Catalog *catalog.Catalog
	Tracker *quota.Tracker
	// Recorder is optional.
	Recorder events.Recorder
	Clock    clock.Clock
	// Interval defaults to DefaultInterval.
	Interval time.Duration
}

// Controller computes the quota status of the SCNodeClasses.
type Controller struct {
	opts Options
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	return &Controller{opts: o}
}

// Register watches SCNodeClasses with the manager; every reconcile requeues
// itself after the interval.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("quota-status").For(&v1alpha1.SCNodeClass{}).Complete(c)
}

type scope struct{ vendor, region, capacityType string }

// Blocked is a scope where every allowed offering is blocked by quota.
type Blocked struct {
	Region       string
	CapacityType string
	Bucket       string
	Used, Limit  int64
	// Types is how many instance types the bucket blocks in the scope.
	Types int
}

// Reason is QuotaZero when the limit is 0, else QuotaExhausted.
func (b Blocked) Reason() string {
	if b.Limit == 0 {
		return ReasonZero
	}
	return ReasonExhausted
}

// Reconcile recomputes and applies the quota status of one NodeClass.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var nc v1alpha1.SCNodeClass
	if err := c.opts.Client.Get(ctx, req.NamespacedName, &nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	blocked, err := c.Compute(ctx, nc.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, vendor := range nc.Spec.Vendors.Names() {
		if err := c.apply(ctx, &nc, vendor, blocked[vendor]); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
}

// Compute returns, per vendor, the scopes of the class where every offering is
// blocked by quota, at most MaxEntries per vendor, the buckets that block the
// most types first. Offerings with unknown quota are not blocked.
func (c *Controller) Compute(ctx context.Context, class string) (map[string][]Blocked, error) {
	types, err := c.opts.Catalog.InstanceTypes(ctx, catalog.Query{Class: class})
	if err != nil {
		return nil, fmt.Errorf("reading the instance types of %s: %w", class, err)
	}
	type tally struct {
		total   int
		blocked int
		// by counts the types per limiting bucket.
		by    map[string]int
		views map[string]quota.BucketView
	}
	tallies := map[scope]*tally{}
	for _, it := range types {
		ref, ok := c.opts.Catalog.Lookup(it.Name)
		if !ok {
			continue
		}
		for _, o := range it.Offerings {
			s := scope{ref.Vendor, o.Requirements.Get(corev1.LabelTopologyRegion).Any(), o.CapacityType()}
			t := tallies[s]
			if t == nil {
				t = &tally{by: map[string]int{}, views: map[string]quota.BucketView{}}
				tallies[s] = t
			}
			t.total++
			if blocked, b := c.opts.Tracker.Blocked(ref.Vendor, s.region, ref.APIReference, s.capacityType); blocked {
				t.blocked++
				t.by[b.ID]++
				t.views[b.ID] = b
			}
		}
	}
	out := map[string][]Blocked{}
	for s, t := range tallies {
		if t.total == 0 || t.blocked != t.total {
			continue
		}
		best := ""
		for id, n := range t.by {
			if best == "" || n > t.by[best] || (n == t.by[best] && id < best) {
				best = id
			}
		}
		v := t.views[best]
		out[s.vendor] = append(out[s.vendor], Blocked{
			Region: s.region, CapacityType: s.capacityType, Bucket: best, Used: v.Used, Limit: v.Limit, Types: t.by[best],
		})
	}
	for vendor, list := range out {
		slices.SortFunc(list, func(a, b Blocked) int {
			if a.Types != b.Types {
				return b.Types - a.Types
			}
			return strings.Compare(a.Region+"/"+a.CapacityType, b.Region+"/"+b.CapacityType)
		})
		if len(list) > MaxEntries {
			list = list[:MaxEntries]
		}
		// A stable order in the status, so that an unchanged state is an unchanged object.
		slices.SortFunc(list, func(a, b Blocked) int {
			return strings.Compare(a.Region+"/"+a.CapacityType, b.Region+"/"+b.CapacityType)
		})
		out[vendor] = list
	}
	return out, nil
}

// condition builds the condition of a vendor from its blocked scopes.
func condition(blocked []Blocked) (status metav1.ConditionStatus, reason, message string) {
	if len(blocked) == 0 {
		return metav1.ConditionTrue, ReasonAvailable, ""
	}
	reason = ReasonZero
	parts := make([]string, 0, len(blocked))
	for _, b := range blocked {
		if b.Limit != 0 {
			reason = ReasonExhausted
		}
		parts = append(parts, fmt.Sprintf("%s %s: bucket %s %d/%d", b.Region, b.CapacityType, b.Bucket, b.Used, b.Limit))
	}
	message = "every allowed offering is blocked by quota in: " + strings.Join(parts, "; ")
	if len(message) > maxMessage {
		message = message[:maxMessage-3] + "..."
	}
	return metav1.ConditionFalse, reason, message
}

func (c *Controller) apply(ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, blocked []Blocked) error {
	status, reason, message := condition(blocked)
	quotaStatus := make([]v1alpha1.QuotaStatus, len(blocked))
	for i, b := range blocked {
		quotaStatus[i] = v1alpha1.QuotaStatus{
			Region: b.Region, CapacityType: b.CapacityType, Bucket: b.Bucket, Used: b.Used, Limit: b.Limit, Reason: b.Reason(),
		}
	}

	now := metav1.NewTime(c.opts.Clock.Now())
	current := nc.Status.Vendors[vendor]
	changedCondition := true
	for _, cond := range current.Conditions {
		if cond.Type != v1alpha1.VendorConditionQuotaAvailable {
			continue
		}
		if string(cond.Status) == string(status) {
			now = cond.LastTransitionTime
		}
		changedCondition = string(cond.Status) != string(status) || cond.Reason != reason || cond.Message != message ||
			cond.ObservedGeneration != nc.Generation
	}
	if !changedCondition && slices.Equal(current.Quota, quotaStatus) {
		return nil
	}

	quotaList := make([]any, len(quotaStatus))
	for i, q := range quotaStatus {
		entry := map[string]any{
			"region": q.Region, "capacityType": q.CapacityType, "bucket": q.Bucket,
			"used": q.Used, "limit": q.Limit,
		}
		if q.Reason != "" {
			entry["reason"] = q.Reason
		}
		quotaList[i] = entry
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status": map[string]any{"vendors": map[string]any{vendor: map[string]any{
			"conditions": []any{map[string]any{
				"type":               v1alpha1.VendorConditionQuotaAvailable,
				"status":             string(status),
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": now.UTC().Format(time.RFC3339),
				"observedGeneration": nc.Generation,
			}},
			"quota": quotaList,
		}}},
	}}
	err := c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the quota status of %s/%s: %w", nc.Name, vendor, err)
	}
	c.emit(nc, vendor, blocked, current.Quota)
	return nil
}

// emit publishes an Event for every scope that became blocked or changed its
// reason (exhausted to zero).
func (c *Controller) emit(nc *v1alpha1.SCNodeClass, vendor string, blocked []Blocked, before []v1alpha1.QuotaStatus) {
	if c.opts.Recorder == nil {
		return
	}
	was := map[string]bool{}
	for _, q := range before {
		was[q.Region+"/"+q.CapacityType+"/"+q.Reason] = true
	}
	for _, b := range blocked {
		if was[b.Region+"/"+b.CapacityType+"/"+b.Reason()] {
			continue
		}
		msg := fmt.Sprintf("%s %s: every offering is blocked, bucket %s is at %d of %d",
			b.Region, b.CapacityType, b.Bucket, b.Used, b.Limit)
		if b.Limit == 0 {
			msg += "; the limit is 0, a quota increase is needed"
		}
		c.opts.Recorder.Publish(events.Event{
			InvolvedObject: nc,
			Type:           corev1.EventTypeWarning,
			Reason:         b.Reason(),
			Message:        msg,
			DedupeValues:   []string{nc.Name, vendor, b.Region, b.CapacityType, b.Reason()},
			DedupeTimeout:  eventDedupe,
		})
	}
}
