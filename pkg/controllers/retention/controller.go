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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/scope"
)

// Options configures the Controller.
type Options struct {
	Client client.Client
	// Reader reads straight from the API server: the release sequence re-reads
	// the pods with it, because the informer cache can lag a fresh binding.
	Reader client.Reader
	// Cluster is the operator's cluster state, shared with Karpenter core.
	Cluster Cluster
	Rules   Rules
	// Starts gives the billing start of a NodeClaim that has no annotation; nil
	// means such a node is left alone.
	Starts   StartSource
	Clock    clock.Clock
	Recorder events.Recorder
	Signals  *Signals
	// Quota reports the scopes blocked by quota; nil means only the cloud
	// provider's refusals (Signals) count.
	Quota QuotaSource
	// PressureInterval is how often the pressure signals are read (default
	// DefaultPressureInterval).
	PressureInterval time.Duration
	Registerer       prometheus.Registerer
}

// Controller is the retention controller: a reconciler of Nodes and a periodic
// read of the pressure signals (Start).
type Controller struct {
	opts    Options
	metrics *metrics

	// releaseMu serialises the budget check and the mark of a release, so that
	// two releases of one tick cannot both see the last free slot.
	releaseMu sync.Mutex

	// seen are the nodes observed empty under the hold, for the reuse metric
	// only; a restart loses at most one count.
	seenMu    sync.Mutex
	seenEmpty map[string]bool
	booted    map[string]bool
	blocked   map[string]bool
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Reader == nil {
		o.Reader = o.Client
	}
	if o.Signals == nil {
		o.Signals = NewSignals(o.Clock)
	}
	if o.PressureInterval <= 0 {
		o.PressureInterval = DefaultPressureInterval
	}
	return &Controller{
		opts: o, metrics: newMetrics(o.Registerer),
		seenEmpty: map[string]bool{}, booted: map[string]bool{}, blocked: map[string]bool{},
	}
}

// Register watches Nodes, and NodeClaims, Pods, NodeClasses and NodePools
// through their nodes, with the manager, and adds the pressure loop. Core
// indexes Pods by spec.nodeName; that index is what the pod lookups use.
func (c *Controller) Register(mgr ctrl.Manager) error {
	nodeOf := func(name string) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
	}
	err := ctrl.NewControllerManagedBy(mgr).Named("billing-retention").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true), MaxConcurrentReconciles: 1}).
		For(&corev1.Node{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				if name := o.(*corev1.Pod).Spec.NodeName; name != "" {
					return nodeOf(name)
				}
				return nil
			}), builder.WithPredicates()).
		Watches(&karpv1.NodeClaim{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				if name := o.(*karpv1.NodeClaim).Status.NodeName; name != "" {
					return nodeOf(name)
				}
				return nil
			})).
		Watches(&v1alpha1.SCNodeClass{}, handler.EnqueueRequestsFromMapFunc(c.nodesOfClass)).
		Watches(&karpv1.NodePool{}, handler.EnqueueRequestsFromMapFunc(c.nodesOfPool)).
		Complete(c)
	if err != nil {
		return fmt.Errorf("registering the billing retention controller: %w", err)
	}
	return mgr.Add(c)
}

func (c *Controller) nodesOfClass(ctx context.Context, o client.Object) []reconcile.Request {
	var claims karpv1.NodeClaimList
	if err := c.opts.Client.List(ctx, &claims); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range claims.Items {
		cl := &claims.Items[i]
		if cl.Spec.NodeClassRef != nil && cl.Spec.NodeClassRef.Name == o.GetName() && cl.Status.NodeName != "" {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: cl.Status.NodeName}})
		}
	}
	return out
}

func (c *Controller) nodesOfPool(ctx context.Context, o client.Object) []reconcile.Request {
	var nodes corev1.NodeList
	if err := c.opts.Client.List(ctx, &nodes, client.MatchingLabels{karpv1.NodePoolLabelKey: o.GetName()}); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(nodes.Items))
	for i := range nodes.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: nodes.Items[i].Name}})
	}
	return out
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *Controller) NeedLeaderElection() bool { return true }

// Start runs the pressure loop until the context is cancelled.
func (c *Controller) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("billing-retention")
	t := time.NewTicker(c.opts.PressureInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.ReleasePressure(ctx); err != nil {
				logger.Error(err, "reading the pressure signals")
			}
		}
	}
}

// nodeInfo is what a reconcile knows about a Node that retention may concern.
type nodeInfo struct {
	node   *corev1.Node
	claim  *karpv1.NodeClaim
	pool   *karpv1.NodePool
	class  *v1alpha1.SCNodeClass
	vendor string
	region string
	// rule and status are the vendor's rule for the node's region and family
	// and what it comes to under the NodeClass's settings.
	rule     billing.Rule
	assess   billing.Status
	settings billing.Settings
	// margin is the NodeClass's margin; billing.Window widens it for the rule.
	start       time.Time
	initialized bool

	// skip is why the node is left alone ("" when it qualifies); message
	// is the Event's text for the reasons that have one.
	skip    string
	message string
}

func (in *nodeInfo) providerID() string { return in.node.Spec.ProviderID }

// qualifies reports whether the node is held and released by the controller.
func (in *nodeInfo) qualifies() bool { return in.skip == "" }

// held reports whether the controller holds the node: its marker is on it.
func (in *nodeInfo) held() bool { return in.node.Annotations[v1alpha1.AnnotationRetainUntil] != "" }

// settingsOf reads spec.billing with the API's defaults for what is absent.
func settingsOf(nc *v1alpha1.SCNodeClass) billing.Settings {
	s := billing.Settings{MinPeriod: DefaultMinPeriod, Margin: DefaultMargin}
	if b := nc.Spec.Billing; b != nil {
		s.Enabled = b.RetainUntilPeriodEnd
		if b.MinPeriod != nil {
			s.MinPeriod = b.MinPeriod.Duration
		}
		if b.Margin != nil {
			s.Margin = b.Margin.Duration
		}
	}
	return s
}

// inspect gathers what the controller needs about a node and decides whether
// it qualifies (plan 23 section 4). It returns nil for a node SCarpenter did not
// launch. An error is a failed read, to retry.
func (c *Controller) inspect(ctx context.Context, node *corev1.Node) (*nodeInfo, error) {
	vendor, region, claimName, err := scope.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, nil //nolint:nilerr // a node that is not SCarpenter's
	}
	claim := &karpv1.NodeClaim{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: claimName}, claim); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if ref := claim.Spec.NodeClassRef; ref == nil || ref.Group != v1alpha1.Group || ref.Kind != nodeClassKind {
		return nil, nil
	}
	in := &nodeInfo{node: node, claim: claim, vendor: vendor, region: region}
	in.class = &v1alpha1.SCNodeClass{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: claim.Spec.NodeClassRef.Name}, in.class); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		in.skip = skipInactive
		return in, nil
	}
	in.settings = settingsOf(in.class)
	in.rule, in.assess = c.assess(vendor, region, claim.Labels[v1alpha1.LabelFamily], in.settings)
	cond := func(typ string) bool { return claim.StatusConditions().Get(typ).IsTrue() }
	// The labels are what Karpenter's cluster state reads, the conditions what
	// the NodeClaim says; either is enough.
	in.initialized = node.Labels[karpv1.NodeInitializedLabelKey] == valueTrue || cond(karpv1.ConditionTypeInitialized)
	registered := node.Labels[karpv1.NodeRegisteredLabelKey] == valueTrue || cond(karpv1.ConditionTypeRegistered)

	pool := &karpv1.NodePool{}
	if name := claim.Labels[karpv1.NodePoolLabelKey]; name != "" {
		if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: name}, pool); err == nil {
			in.pool = pool
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}

	in.skip, in.message = skipReason(in, registered)
	if in.skip != "" {
		return in, nil
	}

	start, known, err := c.billingStart(ctx, in)
	if err != nil {
		return nil, err
	}
	if !known || start.After(c.opts.Clock.Now().Add(billing.SkewAllowance)) {
		in.skip = skipNoBillingTime
		if known {
			in.message = fmt.Sprintf("the billing start %s is in the future", start.Format(time.RFC3339))
		} else {
			in.message = "the driver recorded no billing start for the instance"
		}
		return in, nil
	}
	in.start = start
	return in, nil
}

// assess resolves the rule of a node's region and family and what it comes to.
func (c *Controller) assess(vendor, region, family string, s billing.Settings) (billing.Rule, billing.Status) {
	rule, row := c.opts.Rules.Rule(vendor, region, family)
	return rule, rule.Assess(row, s)
}

// billingStart reads the billing start of the node: its annotation, else the
// NodeClaim's, else once from the cloud provider (a node launched before the
// annotation existed), which is then written to both objects.
func (c *Controller) billingStart(ctx context.Context, in *nodeInfo) (time.Time, bool, error) {
	raw := in.node.Annotations[v1alpha1.AnnotationBillingStart]
	if raw == "" {
		raw = in.claim.Annotations[v1alpha1.AnnotationBillingStart]
	}
	if raw == "" && c.opts.Starts != nil {
		got, err := c.opts.Starts.BillingStart(ctx, in.providerID())
		if err != nil {
			log.FromContext(ctx).V(1).Info("asking for the billing start failed", "node", in.node.Name, "error", err.Error())
			return time.Time{}, false, nil
		}
		if got == "" {
			return time.Time{}, false, nil
		}
		raw = got
		if err := c.annotate(ctx, in.claim, map[string]string{v1alpha1.AnnotationBillingStart: raw}); err != nil {
			return time.Time{}, false, err
		}
		if in.node.Annotations[v1alpha1.AnnotationBillingStart] == "" {
			if err := c.annotate(ctx, in.node, map[string]string{v1alpha1.AnnotationBillingStart: raw}); err != nil {
				return time.Time{}, false, err
			}
		}
	}
	if raw == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, nil //nolint:nilerr // an unreadable stamp is an unknown one
	}
	return t, true, nil
}

// annotate sets annotations on an object with a merge patch of just those keys.
func (c *Controller) annotate(ctx context.Context, obj client.Object, set map[string]string) error {
	orig := obj.DeepCopyObject().(client.Object)
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	maps.Copy(a, set)
	obj.SetAnnotations(a)
	if err := c.opts.Client.Patch(ctx, obj, client.MergeFrom(orig)); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// Reconcile decides what to do with one Node: hold it, release it, or leave it
// alone (plan 23 section 4).
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	node := &corev1.Node{}
	if err := c.opts.Client.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	in, err := c.inspect(ctx, node)
	if err != nil || in == nil {
		return reconcile.Result{}, err
	}
	if !in.qualifies() {
		return c.leaveAlone(ctx, in)
	}
	if hasReleasingTaint(node) {
		return c.continueRelease(ctx, in)
	}
	return c.hold(ctx, in)
}

// leaveAlone removes what the controller put on a node that does not qualify
// (the annotations it set, a release in flight) and says once why the node is
// left alone.
func (c *Controller) leaveAlone(ctx context.Context, in *nodeInfo) (reconcile.Result, error) {
	if hasReleasingTaint(in.node) {
		if err := c.undoRelease(ctx, in); err != nil {
			return reconcile.Result{}, err
		}
	}
	if err := c.dropHold(ctx, in.node); err != nil {
		return reconcile.Result{}, err
	}
	switch in.skip {
	case SkipSpot, SkipStaticPool, SkipConsolidationNever, SkipDrifted, SkipUserDoNotDisrupt:
		c.publish(in.node, corev1.EventTypeNormal, EventRetentionSkipped, in.skip+": "+in.message, in.skip)
	case skipNoBillingTime:
		c.publish(in.node, corev1.EventTypeWarning, EventBillingStartUnknown, in.message, "start")
		return reconcile.Result{RequeueAfter: startRetry}, nil
	}
	return reconcile.Result{}, nil
}

func (c *Controller) publish(node *corev1.Node, typ, reason, message string, dedupe ...string) {
	if c.opts.Recorder == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: node, Type: typ, Reason: reason, Message: message,
		DedupeValues: append([]string{node.Name}, dedupe...), DedupeTimeout: eventDedupe,
	})
}

// hold runs the deadline sequence for a qualifying node that is not being
// released: put the hold on before the deadline, and at the deadline release
// an empty node or let a busy one go for the rest of the period.
func (c *Controller) hold(ctx context.Context, in *nodeInfo) (reconcile.Result, error) {
	now := c.opts.Clock.Now()
	logger := log.FromContext(ctx).WithValues("node", in.node.Name, "vendor", in.vendor)
	boundary, deadline, inMargin, last := billing.Window(in.start, in.rule, in.settings.MinPeriod, in.settings.Margin, now)
	if boundary.IsZero() {
		return reconcile.Result{}, c.dropHold(ctx, in.node)
	}
	untilBoundary := boundary.Sub(now) + requeueSlack
	if last && !now.Before(boundary) {
		// The last boundary passed: there is no next period, every second
		// costs, and Karpenter's own consolidation takes the node over.
		return reconcile.Result{}, c.dropHold(ctx, in.node)
	}

	pods, err := c.reschedulable(ctx, c.opts.Client, in.node.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	empty := len(pods) == 0 && in.initialized
	c.observe(in, empty, now)

	if !inMargin {
		c.missedBoundary(in, empty, now)
		if err := c.ensureHold(ctx, in, deadline); err != nil {
			return reconcile.Result{}, err
		}
		logger.V(1).Info("held", "billingStart", in.start, "boundary", boundary, "deadline", deadline)
		return reconcile.Result{RequeueAfter: deadline.Sub(now) + requeueSlack}, nil
	}
	if !empty {
		// Busy (or not Initialized, which is never deleted by SCarpenter): let
		// Karpenter replace it at a moment that costs nothing, hold again after
		// the boundary. A pod event that empties it takes the other branch.
		logger.Info("busy at the deadline, hold released for the window", "deadline", deadline, "boundary", boundary,
			"initialized", in.initialized, "pods", len(pods))
		if err := c.dropHold(ctx, in.node); err != nil {
			return reconcile.Result{}, err
		}
		if last {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{RequeueAfter: untilBoundary}, nil
	}

	step, err := c.startRelease(ctx, in, releasePeriodEnd)
	if err != nil {
		return reconcile.Result{}, err
	}
	switch step {
	case stepNominated:
		// Karpenter is about to bind a pod here: busy. Keep the hold and look
		// again when the nomination has lapsed.
		if err := c.ensureHold(ctx, in, deadline); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: nominatedRetry}, nil
	case stepBlocked:
		c.blockedByBudget(in, boundary)
		if last {
			return reconcile.Result{}, c.dropHold(ctx, in.node)
		}
		// Held into the next period: one more is paid, the next deadline is
		// tried then.
		if err := c.ensureHold(ctx, in, deadline); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: untilBoundary}, nil
	}
	logger.Info("releasing at the period end", "deadline", deadline, "boundary", boundary)
	return reconcile.Result{RequeueAfter: drainWait}, nil
}

// ensureHold puts the hold on the node: Karpenter's do-not-disrupt, and the
// marker that says SCarpenter set it, with the deadline.
func (c *Controller) ensureHold(ctx context.Context, in *nodeInfo, deadline time.Time) error {
	want := deadline.UTC().Format(time.RFC3339)
	a := in.node.Annotations
	if a[karpv1.DoNotDisruptAnnotationKey] == valueTrue && a[v1alpha1.AnnotationRetainUntil] == want {
		return nil
	}
	if err := c.annotate(ctx, in.node, map[string]string{
		karpv1.DoNotDisruptAnnotationKey: valueTrue, v1alpha1.AnnotationRetainUntil: want,
	}); err != nil {
		return err
	}
	c.publish(in.node, corev1.EventTypeNormal, EventRetainedUntil,
		fmt.Sprintf("held until %s, the end of the billing period less the margin", want), want)
	return nil
}

// dropHold removes the hold, but only when the marker says SCarpenter set it.
func (c *Controller) dropHold(ctx context.Context, node *corev1.Node) error {
	if node.Annotations[v1alpha1.AnnotationRetainUntil] == "" {
		return nil
	}
	orig := node.DeepCopy()
	delete(node.Annotations, v1alpha1.AnnotationRetainUntil)
	delete(node.Annotations, karpv1.DoNotDisruptAnnotationKey)
	if err := c.opts.Client.Patch(ctx, node, client.MergeFrom(orig)); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// blockedByBudget reports a release that a disruption budget forbade.
func (c *Controller) blockedByBudget(in *nodeInfo, boundary time.Time) {
	c.metrics.blockedBudget.WithLabelValues(in.vendor).Inc()
	c.seenMu.Lock()
	c.blocked[in.node.Name] = true
	c.seenMu.Unlock()
	c.publish(in.node, corev1.EventTypeWarning, EventReleaseBlockedByBudget,
		fmt.Sprintf("the disruption budget of NodePool %s allows no deletion of an empty node; "+
			"the node stays until the next deadline after %s", in.pool.Name, boundary.UTC().Format(time.RFC3339)),
		boundary.Format(time.RFC3339))
}

// observe keeps the reuse metric: a held node that was seen empty and now has a
// pod was reused. The state is for the metric only.
func (c *Controller) observe(in *nodeInfo, empty bool, now time.Time) {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	name := in.node.Name
	switch {
	case empty && in.held():
		c.seenEmpty[name] = true
	case !empty && c.seenEmpty[name]:
		if in.initialized {
			c.metrics.reuse.WithLabelValues(in.vendor).Inc()
			c.metrics.periodsSaved.WithLabelValues(in.vendor).Inc()
		}
		delete(c.seenEmpty, name)
	}
	if cond := in.claim.StatusConditions().Get(karpv1.ConditionTypeInitialized); cond.IsTrue() && !c.booted[name] &&
		now.Sub(cond.LastTransitionTime.Time) < bootObserveWindow {
		c.booted[name] = true
		c.metrics.bootSeconds.WithLabelValues(in.vendor).Observe(cond.LastTransitionTime.Sub(in.start).Seconds())
	}
}

// missedBoundary counts an empty node that is held again after a period that
// ended 30 s or more ago: the previous deadline's release did not delete it and
// no budget was the reason, so the margin was too small. The new deadline
// written by ensureHold stops it from being counted twice.
func (c *Controller) missedBoundary(in *nodeInfo, empty bool, now time.Time) {
	prev, err := time.Parse(time.RFC3339, in.node.Annotations[v1alpha1.AnnotationRetainUntil])
	if err != nil || !empty {
		return
	}
	c.seenMu.Lock()
	blocked := c.blocked[in.node.Name]
	delete(c.blocked, in.node.Name)
	c.seenMu.Unlock()
	if blocked {
		return
	}
	boundary := prev.Add(billing.EffectiveMargin(in.settings.Margin, in.rule))
	if now.Sub(boundary) >= boundaryMissedAfter {
		c.metrics.boundaryMissed.WithLabelValues(in.vendor).Inc()
	}
}

// skipReason says why a node is left alone, "" when it qualifies, with the text
// of the Event for the reasons that have one.
func skipReason(in *nodeInfo, registered bool) (reason, message string) {
	node, claim := in.node, in.claim
	cond := func(typ string) bool { return claim.StatusConditions().Get(typ).IsTrue() }
	switch {
	case !in.settings.Enabled || !in.assess.Active:
		return skipInactive, ""
	case !claim.DeletionTimestamp.IsZero() || !node.DeletionTimestamp.IsZero():
		return skipDeleting, ""
	case !registered:
		return skipInactive, ""
	case in.pool == nil:
		return skipNoNodePool, ""
	case node.Labels[karpv1.CapacityTypeLabelKey] == karpv1.CapacityTypeSpot ||
		claim.Labels[karpv1.CapacityTypeLabelKey] == karpv1.CapacityTypeSpot:
		return SkipSpot, "spot nodes are never held"
	case in.pool.Spec.Replicas != nil:
		return SkipStaticPool, "the NodePool is static"
	case in.pool.Spec.Disruption.ConsolidateAfter.Duration == nil:
		return SkipConsolidationNever, "the NodePool has consolidateAfter: Never"
	case cond(karpv1.ConditionTypeDrifted):
		return SkipDrifted, "the NodeClaim is drifted and will be replaced"
	case claim.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" ||
		(node.Annotations[karpv1.DoNotDisruptAnnotationKey] != "" && !in.held()):
		return SkipUserDoNotDisrupt,
			"the node has a karpenter.sh/do-not-disrupt annotation that SCarpenter did not set"
	}
	return "", ""
}
