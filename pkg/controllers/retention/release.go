/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// releasePeriodEnd is why a release starts at a deadline; the other reasons are
// the scopes of a release under pressure. The reason is the value of the
// releasing taint, so that the second half of the sequence knows which Event
// and metric to use after a restart.
const releasePeriodEnd = "period-end"

// What starting a release came to.
type releaseStep int

const (
	// stepStarted: marked for deletion and tainted; the sequence continues
	// after drainWait.
	stepStarted releaseStep = iota
	// stepNominated: Karpenter has a pod on its way to the node: busy.
	stepNominated
	// stepBlocked: a NodePool disruption budget forbids the deletion.
	stepBlocked
)

func hasReleasingTaint(node *corev1.Node) bool {
	return releasingTaint(node) != nil
}

func releasingTaint(node *corev1.Node) *corev1.Taint {
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key == v1alpha1.TaintReleasing {
			return &node.Spec.Taints[i]
		}
	}
	return nil
}

// reschedulable lists the pods that make a node busy: Karpenter's notion of
// reschedulable pods (no DaemonSet, static or finished pod, no terminating one).
func (c *Controller) reschedulable(ctx context.Context, r client.Reader, node string) ([]*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node}); err != nil {
		return nil, fmt.Errorf("listing the pods of node %s: %w", node, err)
	}
	var out []*corev1.Pod
	for i := range pods.Items {
		if podutils.IsReschedulable(&pods.Items[i]) {
			out = append(out, &pods.Items[i])
		}
	}
	return out, nil
}

// remaining is the number of deletions the NodePool's disruption budget for
// reason Empty still allows (negative when it is exceeded), computed from the
// same cluster-state snapshot Karpenter builds its budgets from, with
// Karpenter's own helpers. Nodes marked for deletion count as disrupting, and so
// do nodes Karpenter has already tainted or flagged as disrupted but not yet
// marked (markDisrupted runs before its MarkForDeletion).
func (c *Controller) remaining(ctx context.Context, pool *karpv1.NodePool) (clamped, raw int) {
	nodes := c.opts.Cluster.DeepCopyNodes()
	numNodes, disrupting := disruption.NodePoolStatsFromNodes(nodes, karpv1.DisruptionReasonEmpty)
	for _, n := range nodes {
		if !n.Managed() || !n.Initialized() || n.MarkedForDeletion() || n.Labels()[karpv1.NodePoolLabelKey] != pool.Name {
			continue
		}
		if disrupted(n) {
			disrupting[pool.Name]++
		}
	}
	m := disruption.NodePoolBudgetMap(ctx, c.opts.Clock, map[string]*karpv1.NodePool{pool.Name: pool},
		numNodes, disrupting, karpv1.DisruptionReasonEmpty)
	allowed := pool.MustGetAllowedDisruptions(c.opts.Clock, numNodes[pool.Name], karpv1.DisruptionReasonEmpty)
	return m[pool.Name], allowed - disrupting[pool.Name]
}

// disrupted reports a node Karpenter has begun to disrupt: its disrupted taint,
// or the NodeClaim's DisruptionReason condition.
func disrupted(n *state.StateNode) bool {
	if slices.ContainsFunc(n.Taints(), func(t corev1.Taint) bool { return t.Key == karpv1.DisruptedTaintKey }) {
		return true
	}
	return n.NodeClaim != nil && n.NodeClaim.StatusConditions().Get(karpv1.ConditionTypeDisruptionReason) != nil
}

// startRelease runs the first half of the release sequence of an empty node:
// the nomination check, the budget, the mark in cluster state (so the scheduler
// stops using the node) and the NoSchedule taint. The second half
// (continueRelease) runs after drainWait.
func (c *Controller) startRelease(ctx context.Context, in *nodeInfo, why string) (releaseStep, error) {
	if c.opts.Cluster.IsNodeNominated(in.providerID()) {
		return stepNominated, nil
	}
	c.releaseMu.Lock()
	defer c.releaseMu.Unlock()
	// Another path (a deadline, the pressure loop) may have started the release
	// of this node since it was read.
	fresh := &corev1.Node{}
	if err := c.opts.Reader.Get(ctx, client.ObjectKeyFromObject(in.node), fresh); err != nil {
		return stepBlocked, fmt.Errorf("reading node %s: %w", in.node.Name, err)
	}
	if hasReleasingTaint(fresh) {
		return stepStarted, nil
	}
	in.node = fresh
	if free, _ := c.remaining(ctx, in.pool); free < 1 {
		return stepBlocked, nil
	}
	// The mark comes before the delete, so that Karpenter's next budget build
	// sees the slot taken under the cluster mutex.
	c.opts.Cluster.MarkForDeletion(in.providerID())
	if _, raw := c.remaining(ctx, in.pool); raw < 0 {
		// Karpenter marked a node of the pool between our check and our mark.
		c.metrics.overshoot.WithLabelValues(in.vendor).Inc()
	}
	orig := in.node.DeepCopy()
	in.node.Spec.Taints = append(in.node.Spec.Taints, corev1.Taint{
		Key: v1alpha1.TaintReleasing, Value: why, Effect: corev1.TaintEffectNoSchedule,
		TimeAdded: &metav1.Time{Time: c.opts.Clock.Now()},
	})
	// The taints are an atomic list: a patch from a stale copy would drop a
	// taint added since (Karpenter's own, the node lifecycle's), so it is
	// guarded by the resource version, as Karpenter's taint patches are.
	locked := client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})
	if err := c.opts.Client.Patch(ctx, in.node, locked); err != nil {
		c.opts.Cluster.UnmarkForDeletion(in.providerID())
		return stepBlocked, fmt.Errorf("tainting node %s: %w", in.node.Name, err)
	}
	return stepStarted, nil
}

// continueRelease is the second half of the release sequence, after drainWait:
// the pods are read again from the API server (the informer cache can lag a
// fresh binding), and a pod, or a nomination, means the node is busy after all:
// the mark and the taint are undone. Otherwise the NodeClaim is deleted and
// Karpenter drains and terminates the node.
func (c *Controller) continueRelease(ctx context.Context, in *nodeInfo) (reconcile.Result, error) {
	taint := releasingTaint(in.node)
	// A restart loses the mark, not the taint.
	c.opts.Cluster.MarkForDeletion(in.providerID())
	if taint.TimeAdded != nil {
		if wait := drainWait - c.opts.Clock.Since(taint.TimeAdded.Time); wait > 0 {
			return reconcile.Result{RequeueAfter: wait + requeueSlack}, nil
		}
	}
	pods, err := c.reschedulable(ctx, c.opts.Reader, in.node.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(pods) > 0 || c.opts.Cluster.IsNodeNominated(in.providerID()) {
		log.FromContext(ctx).Info("a pod arrived during the release, keeping the node",
			"node", in.node.Name, "pods", len(pods))
		if err := c.undoRelease(ctx, in); err != nil {
			return reconcile.Result{}, err
		}
		return c.hold(ctx, in)
	}
	// The cache can lag our own delete of an earlier reconcile: read the claim
	// from the API server so that one release is deleted and counted once.
	claim := &karpv1.NodeClaim{}
	if err := c.opts.Reader.Get(ctx, client.ObjectKeyFromObject(in.claim), claim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !claim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if err := c.opts.Client.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("deleting NodeClaim %s: %w", in.claim.Name, err)
	}
	c.released(in, taint.Value)
	return reconcile.Result{}, nil
}

// released reports a NodeClaim deleted by the controller.
func (c *Controller) released(in *nodeInfo, why string) {
	switch why {
	case releasePeriodEnd:
		c.metrics.deletesDeadline.WithLabelValues(in.vendor).Inc()
		c.publish(in.node, corev1.EventTypeNormal, EventReleasedAtPeriodEnd,
			"the node was empty at the end of its paid period less the margin and is deleted before the next period starts",
			"released")
	default:
		c.metrics.releasedPress.WithLabelValues(in.vendor, why).Inc()
		c.publish(in.node, corev1.EventTypeNormal, EventReleasedUnderPressure,
			fmt.Sprintf("the empty node is released before the end of its paid period (%s)", scopeText(why)), "released")
	}
}

func scopeText(scope string) string {
	switch scope {
	case ScopeLimit:
		return "a launch is blocked by a NodePool limit"
	case ScopeMaxRetained:
		return "more nodes are held than spec.billing.maxRetainedNodes"
	}
	return "a launch is blocked by a vendor quota"
}

// undoRelease takes back the mark and the taint of a release that did not
// complete.
func (c *Controller) undoRelease(ctx context.Context, in *nodeInfo) error {
	c.opts.Cluster.UnmarkForDeletion(in.providerID())
	node := in.node
	orig := node.DeepCopy()
	node.Spec.Taints = slices.DeleteFunc(slices.Clone(node.Spec.Taints), func(t corev1.Taint) bool {
		return t.Key == v1alpha1.TaintReleasing
	})
	locked := client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})
	if err := c.opts.Client.Patch(ctx, node, locked); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}
