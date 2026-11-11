/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/billing"
)

// FieldManager is the field manager of the status this controller writes
// (status.billing and the BillingRetention condition).
const FieldManager = "scarpenter-billing"

// StatusController computes status.billing and the BillingRetention condition of
// the SCNodeClasses, counts the nodes retention leaves alone and keeps the gauges
// of the retained and skipped nodes. The status is written with server-side apply
// under its own field manager and owns nothing else.
type StatusController struct {
	c        *Controller
	interval time.Duration

	mu      sync.Mutex
	logged  map[string]bool
	lastRun map[string]time.Time // per NodeClass, for the retained-seconds counter
}

// NewStatusController returns the status controller of c.
func NewStatusController(c *Controller, interval time.Duration) *StatusController {
	if interval <= 0 {
		interval = DefaultStatusInterval
	}
	return &StatusController{c: c, interval: interval, logged: map[string]bool{}, lastRun: map[string]time.Time{}}
}

// Register watches SCNodeClasses with the manager; every reconcile requeues itself
// after the interval.
func (s *StatusController) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("billing-status").
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&v1alpha1.SCNodeClass{}).Complete(s)
}

// nodeTally is what the nodes of one vendor of a NodeClass come to.
type nodeTally struct {
	nodes, held, noStart int
	skipped              map[string]int
}

// Reconcile recomputes and applies the billing status of one NodeClass.
func (s *StatusController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var nc v1alpha1.SCNodeClass
	if err := s.c.opts.Client.Get(ctx, req.NamespacedName, &nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	tallies, err := s.tally(ctx, &nc)
	if err != nil {
		return reconcile.Result{}, err
	}
	statuses := s.statuses(ctx, &nc, tallies)
	s.metrics(&nc, tallies)
	if err := s.apply(ctx, &nc, statuses, tallies); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: s.interval}, nil
}

// tally counts the nodes of the NodeClass per vendor: how many are held, how
// many have no billing start, and how many are left alone for which reason.
func (s *StatusController) tally(ctx context.Context, nc *v1alpha1.SCNodeClass) (map[string]*nodeTally, error) {
	out := map[string]*nodeTally{}
	var claims karpv1.NodeClaimList
	if err := s.c.opts.Client.List(ctx, &claims); err != nil {
		return nil, fmt.Errorf("listing the NodeClaims: %w", err)
	}
	for i := range claims.Items {
		cl := &claims.Items[i]
		if ref := cl.Spec.NodeClassRef; ref == nil || ref.Group != v1alpha1.Group || ref.Kind != "SCNodeClass" ||
			ref.Name != nc.Name || cl.Status.NodeName == "" {
			continue
		}
		var node corev1.Node
		if err := s.c.opts.Client.Get(ctx, types.NamespacedName{Name: cl.Status.NodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		in, err := s.c.inspect(ctx, &node)
		if err != nil {
			return nil, err
		}
		if in == nil {
			continue
		}
		t := out[in.vendor]
		if t == nil {
			t = &nodeTally{skipped: map[string]int{}}
			out[in.vendor] = t
		}
		t.nodes++
		if in.held() {
			t.held++
		}
		switch in.skip {
		case SkipConsolidationNever, SkipUserDoNotDisrupt, SkipStaticPool, SkipSpot, SkipDrifted:
			t.skipped[in.skip]++
		case skipNoBillingTime:
			t.noStart++
		}
	}
	return out, nil
}

// statuses is the per-vendor status of the NodeClass.
func (s *StatusController) statuses(ctx context.Context, nc *v1alpha1.SCNodeClass, tallies map[string]*nodeTally) []v1alpha1.BillingStatus {
	settings := settingsOf(nc)
	var out []v1alpha1.BillingStatus
	for _, vendor := range nc.Spec.Vendors.Names() {
		// The vendor-wide rule (empty region and family); the rule of a node is
		// the one of its own region and family.
		rule, row := s.c.opts.Rules.Rule(vendor, "", "")
		st := rule.Assess(row, settings)
		s.logRule(ctx, vendor, rule, st)
		if t := tallies[vendor]; st.Active && t != nil && t.nodes > 0 && t.noStart == t.nodes {
			st = billing.Status{Reason: billing.ReasonNoBillingStart,
				Message: "the driver recorded no billing start for the nodes of the vendor", Margin: st.Margin}
		}
		bs := v1alpha1.BillingStatus{
			Vendor: vendor, Active: st.Active, Reason: st.Reason, Message: st.Message, Source: rule.Source,
		}
		if st.Active {
			bs.Period = st.Period.String()
		}
		out = append(out, bs)
	}
	return out
}

// logRule logs the rule a vendor resolved to, once per vendor and outcome, so
// that an operator can see it.
func (s *StatusController) logRule(ctx context.Context, vendor string, rule billing.Rule, st billing.Status) {
	key := fmt.Sprintf("%s/%s/%s/%v", vendor, rule.Describe(), st.Reason, rule.Verified)
	s.mu.Lock()
	seen := s.logged[key]
	s.logged[key] = true
	s.mu.Unlock()
	if seen {
		return
	}
	log.FromContext(ctx).Info("billing rule resolved", "vendor", vendor, "increment", rule.Increment.String(),
		"minimum", rule.Minimum.String(), "period", st.Period.String(), "source", rule.Source, "verified", rule.Verified,
		"reason", st.Reason, "deleteLatency", rule.DeleteLatency.String())
}

// metrics sets the gauges of the retained and the skipped nodes, and counts the
// seconds the held nodes spent held since the last run.
func (s *StatusController) metrics(nc *v1alpha1.SCNodeClass, tallies map[string]*nodeTally) {
	m := s.c.metrics
	now := s.c.opts.Clock.Now()
	s.mu.Lock()
	last := s.lastRun[nc.Name]
	elapsed := now.Sub(last)
	if last.IsZero() || elapsed > 5*s.interval {
		elapsed = 0
	}
	s.lastRun[nc.Name] = now
	s.mu.Unlock()
	for vendor, t := range tallies {
		m.retainedNodes.WithLabelValues(vendor, nc.Name).Set(float64(t.held))
		if t.held > 0 && elapsed > 0 {
			m.retainedSeconds.WithLabelValues(vendor).Add(float64(t.held) * elapsed.Seconds())
		}
		for _, reason := range []string{SkipConsolidationNever, SkipUserDoNotDisrupt, SkipStaticPool, SkipSpot, SkipDrifted} {
			m.skippedNodes.WithLabelValues(vendor, nc.Name, reason).Set(float64(t.skipped[reason]))
		}
	}
}

// condition builds the BillingRetention condition from the vendor statuses and
// the nodes left alone.
func condition(statuses []v1alpha1.BillingStatus, tallies map[string]*nodeTally) (metav1.ConditionStatus, string, string) {
	active := false
	var parts []string
	for _, st := range statuses {
		active = active || st.Active
		parts = append(parts, st.Vendor+": "+st.Reason)
	}
	left := map[string]int{}
	for _, t := range tallies {
		for reason, n := range t.skipped {
			left[reason] += n
		}
	}
	msg := strings.Join(parts, "; ")
	if len(left) > 0 {
		var counts []string
		for _, reason := range slices.Sorted(maps.Keys(left)) {
			counts = append(counts, fmt.Sprintf("%s=%d", reason, left[reason]))
		}
		msg += "; nodes left alone: " + strings.Join(counts, ", ")
	}
	reason := billing.ReasonActive
	status := metav1.ConditionTrue
	if !active {
		status = metav1.ConditionFalse
		reason = "NotActive"
		if len(statuses) > 0 && statuses[0].Reason == billing.ReasonDisabled {
			reason = billing.ReasonDisabled
		}
	}
	if len(msg) > 1000 {
		msg = msg[:997] + "..."
	}
	return status, reason, msg
}

func (s *StatusController) apply(ctx context.Context, nc *v1alpha1.SCNodeClass, statuses []v1alpha1.BillingStatus,
	tallies map[string]*nodeTally) error {
	status, reason, message := condition(statuses, tallies)
	now := metav1.NewTime(s.c.opts.Clock.Now())
	changed := !slices.Equal(nc.Status.Billing, statuses)
	found := false
	for _, cond := range nc.Status.Conditions {
		if cond.Type != v1alpha1.ConditionTypeBillingRetention {
			continue
		}
		found = true
		if string(cond.Status) == string(status) {
			now = cond.LastTransitionTime
		}
		changed = changed || string(cond.Status) != string(status) || cond.Reason != reason || cond.Message != message ||
			cond.ObservedGeneration != nc.Generation
	}
	if !changed && found {
		return nil
	}
	list := make([]any, len(statuses))
	for i, st := range statuses {
		entry := map[string]any{"vendor": st.Vendor, "active": st.Active, "reason": st.Reason}
		for k, v := range map[string]string{"message": st.Message, "period": st.Period, "source": st.Source} {
			if v != "" {
				entry[k] = v
			}
		}
		list[i] = entry
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status": map[string]any{
			"billing": list,
			"conditions": []any{map[string]any{
				"type":               v1alpha1.ConditionTypeBillingRetention,
				"status":             string(status),
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": now.UTC().Format(time.RFC3339),
				"observedGeneration": nc.Generation,
			}},
		},
	}}
	err := s.c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("applying the billing status of %s: %w", nc.Name, err)
	}
	return nil
}
