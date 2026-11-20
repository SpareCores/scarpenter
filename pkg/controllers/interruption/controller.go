/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package interruption is the interruption controller (ADR-027, plan 26): when a
// vendor announces that it will reclaim a spot or preemptible node, it makes
// Karpenter drain the node as early in the notice window as possible, records
// the reason, keeps the offering out of scheduling for a TTL and deletes the
// NodeClaim. Both notice paths (the node agent annotating its Node, the vendor
// poller) end in the Node annotations this controller reconciles.
package interruption

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultMargin is how long before the notice deadline the termination deadline
// is set: the time the instance needs after the kubelet stopped the containers.
const DefaultMargin = 5 * time.Second

// Event reasons, on the Node and the NodeClaim.
const (
	// EventSpotInterrupted: the vendor announced the reclaim of the node.
	EventSpotInterrupted = "SpotInterrupted"
	// EventIgnored: the notice was not acted on (the node is not spot, or the
	// reason is not known).
	EventIgnored = "InterruptionIgnored"
)

// ignoredDedupe is how long one InterruptionIgnored Event of a node is
// published once.
const ignoredDedupe = time.Hour

const nodeClassKind = "SCNodeClass"

// Options configures the Controller.
type Options struct {
	Client client.Client
	// Registry gives the notice window of each vendor's driver.
	Registry *vendors.Registry
	// Unavailable is SCarpenter's unavailable-offerings cache; nil leaves the
	// interrupted offering available.
	Unavailable *unavailable.Cache
	Clock       clock.Clock
	Recorder    events.Recorder
	// Margin is the time before the deadline at which Pods are force-deleted
	// (default DefaultMargin; a negative value is taken as zero).
	Margin     time.Duration
	Registerer prometheus.Registerer
}

// Controller reconciles Nodes that carry an interruption notice.
type Controller struct {
	opts    Options
	metrics *metrics

	// vanished are the on-demand NodeClaims already recorded by InstanceGone.
	vanishedMu sync.Mutex
	vanished   map[types.UID]time.Time
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Margin == 0 {
		o.Margin = DefaultMargin
	}
	o.Margin = max(o.Margin, 0)
	return &Controller{opts: o, metrics: newMetrics(o.Registerer), vanished: map[types.UID]time.Time{}}
}

// Register watches the Nodes that have the interruption annotation, and the
// NodeClaims that have it too (a failed delete is retried through them).
func (c *Controller) Register(mgr ctrl.Manager) error {
	annotated := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetAnnotations()[v1alpha1.AnnotationInterruption]
		return ok
	})
	err := ctrl.NewControllerManagedBy(mgr).Named("interruption").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true), MaxConcurrentReconciles: 4}).
		For(&corev1.Node{}, builder.WithPredicates(annotated)).
		Watches(&karpv1.NodeClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				if name := o.(*karpv1.NodeClaim).Status.NodeName; name != "" {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
				}
				return nil
			}), builder.WithPredicates(annotated)).
		Complete(c)
	if err != nil {
		return fmt.Errorf("registering the interruption controller: %w", err)
	}
	return nil
}

// notice is what the Node says about its interruption.
type notice struct {
	reason string
	source string
	// deadline is zero when the Node gave none or an unparsable one.
	deadline time.Time
}

func noticeOf(node *corev1.Node) notice {
	n := notice{
		reason: node.Annotations[v1alpha1.AnnotationInterruption],
		source: node.Annotations[v1alpha1.AnnotationInterruptionSource],
	}
	// The source is a metric label and a node can write anything: keep it to
	// the known ones.
	if n.source != v1alpha1.SourceVendorAPI {
		n.source = v1alpha1.SourceInstanceMetadata
	}
	if t, err := time.Parse(time.RFC3339, node.Annotations[v1alpha1.AnnotationInterruptionDeadline]); err == nil {
		n.deadline = t
	}
	return n
}

// Reconcile handles the notice of one Node (steps of plan 26 section 3). Every
// step is idempotent: a second reconcile changes nothing, and what must happen
// once (the cache entry, the metrics, the Events) happens when the NodeClaim has
// no Interrupted condition yet.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	seen := c.opts.Clock.Now()
	node := &corev1.Node{}
	if err := c.opts.Client.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if _, ok := node.Annotations[v1alpha1.AnnotationInterruption]; !ok {
		return reconcile.Result{}, nil
	}
	vendor, _, claimName, err := scope.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil //nolint:nilerr // a node that is not SCarpenter's
	}
	claim := &karpv1.NodeClaim{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: claimName}, claim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// The node says which claim it is; the claim must agree, so that a node
	// cannot make SCarpenter delete another node's claim.
	if ref := claim.Spec.NodeClassRef; ref == nil || ref.Group != v1alpha1.Group || ref.Kind != nodeClassKind ||
		claim.Status.ProviderID != node.Spec.ProviderID || claim.Status.NodeName != node.Name {
		return reconcile.Result{}, nil
	}
	n := noticeOf(node)
	if msg := c.ignoreReason(claim, n); msg != "" {
		c.publish(ignoredDedupe, corev1.EventTypeWarning, EventIgnored, msg, node, claim)
		return reconcile.Result{}, nil
	}

	first := claim.StatusConditions().Get(v1alpha1.ConditionInterrupted) == nil
	deadline, assumed := c.deadline(claim, vendor, n, seen)
	if err := c.tighten(ctx, claim, node, n, deadline); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.markInterrupted(ctx, claim, n.reason); err != nil {
		return reconcile.Result{}, err
	}
	if first {
		c.recordOnce(vendor, node, claim, n, deadline, assumed, seen)
	}
	if claim.DeletionTimestamp == nil {
		if err := c.opts.Client.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("deleting NodeClaim %s: %w", claim.Name, err)
		}
	}
	if first {
		c.metrics.reaction.Observe(c.opts.Clock.Since(seen).Seconds())
		log.FromContext(ctx).WithValues("nodeclaim", claim.Name, "reason", n.reason, "source", n.source,
			"deadline", deadline.Format(time.RFC3339)).Info("interrupted node, deleted its NodeClaim")
	}
	return reconcile.Result{}, nil
}

// ignoreReason says why a notice is not acted on ("" when it is).
func (c *Controller) ignoreReason(claim *karpv1.NodeClaim, n notice) string {
	switch {
	case claim.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeSpot:
		return fmt.Sprintf("the node is not a spot node (capacity type %q): an interruption notice is only acted on for spot",
			claim.Labels[karpv1.CapacityTypeLabelKey])
	case n.reason != v1alpha1.ReasonSpotInterrupted:
		// The node writes the reason: keep the Event message short.
		return fmt.Sprintf("the interruption reason %q is not known (known: %s)",
			n.reason[:min(len(n.reason), 64)], v1alpha1.ReasonSpotInterrupted)
	}
	return ""
}

// deadline is the effective deadline: the one already on the NodeClaim, else the
// Node's clamped into the vendor's notice window, else (none given) the
// window's minimum from now. assumed is true in the last case.
func (c *Controller) deadline(
	claim *karpv1.NodeClaim, vendor string, n notice, now time.Time,
) (t time.Time, assumed bool) {
	// The NodeClaim's deadline is only trusted once this controller marked the
	// claim: before that it may come from a NodePool template.
	if claim.StatusConditions().Get(v1alpha1.ConditionInterrupted).IsTrue() {
		if prev, err := time.Parse(time.RFC3339, claim.Annotations[v1alpha1.AnnotationInterruptionDeadline]); err == nil {
			return prev, false
		}
	}
	window := vendors.DefaultNoticeWindow
	if c.opts.Registry != nil {
		if d, ok := c.opts.Registry.Driver(vendor); ok {
			window = vendors.NoticeWindowOf(d)
		}
	}
	t = n.deadline
	if t.IsZero() {
		t, assumed = now.Add(window.MinNotice), true
	}
	return minTime(maxTime(t, now), now.Add(window.MaxNotice)).UTC().Truncate(time.Second), assumed
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// tighten sets the termination deadline of the NodeClaim (core force-deletes
// the Pods whose grace would outlast it, also on a claim that is already being
// deleted for another reason) and copies the notice to the NodeClaim.
func (c *Controller) tighten(
	ctx context.Context, claim *karpv1.NodeClaim, node *corev1.Node, n notice, deadline time.Time,
) error {
	termination := deadline.Add(-c.opts.Margin)
	// Core derives the timestamp from the NodeClaim's terminationGracePeriod
	// only when the claim has none, so setting ours first must keep that bound
	// (the claim is deleted now, or already was).
	if tgp := claim.Spec.TerminationGracePeriod; tgp != nil {
		deleted := c.opts.Clock.Now()
		if claim.DeletionTimestamp != nil {
			deleted = claim.DeletionTimestamp.Time
		}
		termination = minTime(termination, deleted.Add(tgp.Duration))
	}
	if err := nodeclaimutils.TightenTerminationTimestampAnnotation(ctx, c.opts.Client, claim, termination); err != nil {
		return fmt.Errorf("tightening the termination deadline of NodeClaim %s: %w", claim.Name, err)
	}
	want := map[string]string{
		v1alpha1.AnnotationInterruption:         n.reason,
		v1alpha1.AnnotationInterruptionDeadline: deadline.Format(time.RFC3339),
		v1alpha1.AnnotationInterruptionSource:   n.source,
	}
	changed := false
	for k, v := range want {
		changed = changed || claim.Annotations[k] != v
	}
	if changed {
		stored := claim.DeepCopy()
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		maps.Copy(claim.Annotations, want)
		if err := c.opts.Client.Patch(ctx, claim, client.MergeFrom(stored)); err != nil {
			return fmt.Errorf("annotating NodeClaim %s: %w", claim.Name, err)
		}
	}
	// The poller path writes the same record on the Node, so that it looks the
	// same whichever path found the notice (the agent's Node already has it).
	if node.Annotations[v1alpha1.AnnotationInterruptionDeadline] != want[v1alpha1.AnnotationInterruptionDeadline] ||
		node.Annotations[v1alpha1.AnnotationInterruptionSource] != n.source {
		stored := node.DeepCopy()
		maps.Copy(node.Annotations, want)
		if err := c.opts.Client.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return fmt.Errorf("annotating Node %s: %w", node.Name, err)
		}
	}
	return nil
}

// markInterrupted sets the Interrupted condition (SCarpenter's own) and
// DisruptionReason (so that core's eviction Events and metrics carry the reason)
// on the NodeClaim.
func (c *Controller) markInterrupted(ctx context.Context, claim *karpv1.NodeClaim, reason string) error {
	conds := claim.StatusConditions()
	if conds.Get(v1alpha1.ConditionInterrupted).IsTrue() && conds.Get(karpv1.ConditionTypeDisruptionReason).IsTrue() {
		return nil
	}
	stored := claim.DeepCopy()
	conds.SetTrueWithReason(v1alpha1.ConditionInterrupted, reason, reason)
	conds.SetTrueWithReason(karpv1.ConditionTypeDisruptionReason, reason, reason)
	patch := client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})
	if err := c.opts.Client.Status().Patch(ctx, claim, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("setting the Interrupted condition of NodeClaim %s: %w", claim.Name, err)
	}
	return nil
}

// recordOnce keeps the interrupted offering out of scheduling, counts the
// interruption and publishes the Events.
func (c *Controller) recordOnce(
	vendor string, node *corev1.Node, claim *karpv1.NodeClaim, n notice, deadline time.Time, assumed bool, seen time.Time,
) {
	instanceType, zone := claim.Labels[corev1.LabelInstanceTypeStable], claim.Labels[corev1.LabelTopologyZone]
	if c.opts.Unavailable != nil && instanceType != "" && zone != "" {
		c.opts.Unavailable.MarkInterrupted(vendor, instanceType, zone, karpv1.CapacityTypeSpot)
	}
	lead := max(deadline.Sub(seen), 0)
	c.metrics.interruptions.WithLabelValues(vendor, instanceType, zone, n.reason, n.source).Inc()
	c.metrics.lead.WithLabelValues(vendor, n.source).Observe(lead.Seconds())

	msg := fmt.Sprintf("%s spot instance %s in %s is being reclaimed: deadline %s, %s from now (source %s)",
		vendor, instanceType, zone, deadline.Format(time.RFC3339), lead.Round(time.Second), n.source)
	if assumed {
		msg += "; the notice gave no usable deadline, the vendor's shortest notice is assumed"
	}
	c.publish(0, corev1.EventTypeWarning, EventSpotInterrupted, msg, node, claim)
}

// publish sends the Event on each object; dedupe is how long a repeat is
// suppressed (0: not at all, Events are only published once per interruption).
func (c *Controller) publish(dedupe time.Duration, typ, reason, message string, objs ...client.Object) {
	if c.opts.Recorder == nil {
		return
	}
	for _, o := range objs {
		ev := events.Event{InvolvedObject: o, Type: typ, Reason: reason, Message: message}
		if dedupe > 0 {
			ev.DedupeValues, ev.DedupeTimeout = []string{o.GetName(), reason}, dedupe
		}
		c.opts.Recorder.Publish(ev)
	}
}
