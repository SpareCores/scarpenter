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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
	karpresources "sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/scope"
)

// candidate is a held empty node that can be released early.
type candidate struct {
	in       *nodeInfo
	deadline time.Time
	capacity corev1.ResourceList
}

// ReleasePressure is one reading of the pressure signals (plan 23 section 4,
// step 4): where a launch is blocked by a NodePool limit or a vendor quota, or a
// NodeClass holds more empty nodes than maxRetainedNodes, held empty nodes are
// released early, the nearest deadline first (ties by oldest start), only as many
// as cover the request, with the budget check and release sequence of a deadline
// release. A candidate that a budget blocks loses its hold and the next one is
// tried. Nothing here has state of its own: it runs again on every tick while the
// pressure lasts.
func (c *Controller) ReleasePressure(ctx context.Context) error {
	cands, err := c.candidates(ctx)
	if err != nil || len(cands) == 0 {
		return err
	}
	done := map[string]bool{}
	c.trimMaxRetained(ctx, cands, done)

	pending, err := c.blockedPods(ctx)
	if err != nil {
		return err
	}
	refusals := c.opts.Signals.quotaRefusals()
	if len(pending) == 0 {
		return nil
	}
	if err := c.releaseForLimits(ctx, cands, pending, done); err != nil {
		return err
	}
	return c.releaseForQuota(ctx, cands, pending, refusals, done)
}

// candidates lists the held empty nodes, in release order: nearest deadline
// first, ties by oldest billing start.
func (c *Controller) candidates(ctx context.Context) ([]*candidate, error) {
	var nodes corev1.NodeList
	if err := c.opts.Client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing the nodes: %w", err)
	}
	var out []*candidate
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Annotations[v1alpha1.AnnotationRetainUntil] == "" || hasReleasingTaint(node) {
			continue
		}
		in, err := c.inspect(ctx, node)
		if err != nil {
			return nil, err
		}
		if in == nil || !in.qualifies() || !in.initialized {
			continue
		}
		pods, err := c.reschedulable(ctx, c.opts.Client, node.Name)
		if err != nil {
			return nil, err
		}
		if len(pods) > 0 {
			continue
		}
		deadline, err := time.Parse(time.RFC3339, node.Annotations[v1alpha1.AnnotationRetainUntil])
		if err != nil {
			deadline = in.start
		}
		out = append(out, &candidate{in: in, deadline: deadline, capacity: node.Status.Capacity})
	}
	slices.SortFunc(out, func(a, b *candidate) int {
		if d := a.deadline.Compare(b.deadline); d != 0 {
			return d
		}
		if d := a.in.start.Compare(b.in.start); d != 0 {
			return d
		}
		return strings.Compare(a.in.node.Name, b.in.node.Name)
	})
	return out, nil
}

// release starts the release of a candidate under pressure and says whether the
// node is on its way out. A node that a budget blocks loses its hold and is an
// ordinary empty node that Karpenter's emptiness deletes when the budget opens; a
// nominated one is busy and is left.
func (c *Controller) release(ctx context.Context, cand *candidate, why string, done map[string]bool) bool {
	in := cand.in
	step, err := c.startRelease(ctx, in, why)
	if err != nil {
		log.FromContext(ctx).Error(err, "starting the release of a held node", "node", in.node.Name)
		return false
	}
	switch step {
	case stepStarted:
		done[in.node.Name] = true
		return true
	case stepBlocked:
		done[in.node.Name] = true
		c.metrics.blockedBudget.WithLabelValues(in.vendor).Inc()
		c.publish(in.node, corev1.EventTypeWarning, EventReleaseBlockedByBudget,
			fmt.Sprintf("the node would be released (%s) but the disruption budget of NodePool %s allows no deletion; "+
				"it loses its hold and Karpenter deletes it when the budget allows", scopeText(why), in.pool.Name),
			"pressure", why)
		if err := c.dropHold(ctx, in.node); err != nil {
			log.FromContext(ctx).Error(err, "dropping the hold of a node blocked by a budget", "node", in.node.Name)
		}
	}
	return false
}

// trimMaxRetained releases the held empty nodes of each NodeClass beyond its
// maxRetainedNodes.
func (c *Controller) trimMaxRetained(ctx context.Context, cands []*candidate, done map[string]bool) {
	byClass := map[string][]*candidate{}
	for _, cand := range cands {
		byClass[cand.in.class.Name] = append(byClass[cand.in.class.Name], cand)
	}
	for _, list := range byClass {
		b := list[0].in.class.Spec.Billing
		if b == nil || b.MaxRetainedNodes == nil {
			continue
		}
		excess := len(list) - int(*b.MaxRetainedNodes)
		for _, cand := range list {
			if excess <= 0 {
				break
			}
			// A node whose hold a budget took is no longer a held node either.
			if c.release(ctx, cand, ScopeMaxRetained, done) || done[cand.in.node.Name] {
				excess--
			}
		}
	}
}

// blockedPods are the pods that wait for capacity Karpenter has not yet decided
// to launch: provisionable, and with no NodeClaim mapped to them.
func (c *Controller) blockedPods(ctx context.Context) ([]*corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.opts.Client.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("listing the pods: %w", err)
	}
	var out []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if podutils.IsProvisionable(p) && !podutils.IsTerminating(p) &&
			c.opts.Cluster.PodNodeClaimMapping(types.NamespacedName{Namespace: p.Namespace, Name: p.Name}) == "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// mayUsePool is a rough test of whether a pending pod could run on a node of the
// pool: the nodepool selector and the pool's taints. The scheduler decides for
// real; a pod wrongly attributed only releases a node of a pool that is at its
// limit anyway.
func mayUsePool(pod *corev1.Pod, pool *karpv1.NodePool) bool {
	if want, ok := pod.Spec.NodeSelector[karpv1.NodePoolLabelKey]; ok && want != pool.Name {
		return false
	}
	return scheduling.Taints(pool.Spec.Template.Spec.Taints).ToleratesPod(pod) == nil
}

// releaseForLimits releases held empty nodes of every NodePool whose limits
// leave no room for the pending pods.
func (c *Controller) releaseForLimits(
	ctx context.Context, cands []*candidate, pending []*corev1.Pod, done map[string]bool,
) error {
	byPool := map[string][]*candidate{}
	for _, cand := range cands {
		if cand.in.pool != nil && len(cand.in.pool.Spec.Limits) > 0 {
			byPool[cand.in.pool.Name] = append(byPool[cand.in.pool.Name], cand)
		}
	}
	for _, list := range byPool {
		pool := list[0].in.pool
		used := c.opts.Cluster.NodePoolResourcesFor(pool.Name)
		remaining := corev1.ResourceList{}
		for r, limit := range pool.Spec.Limits {
			left := limit.DeepCopy()
			left.Sub(used[r])
			remaining[r] = left
		}
		// A new node as small as the smallest held one must fit as well.
		smallest := corev1.ResourceList{}
		for _, cand := range list {
			for r := range remaining {
				q := capacityOf(cand, r)
				if cur, ok := smallest[r]; !ok || q.Cmp(cur) < 0 {
					smallest[r] = q
				}
			}
		}
		need := corev1.ResourceList{}
		blocked := 0
		for _, pod := range pending {
			if !mayUsePool(pod, pool) {
				continue
			}
			req := karpresources.Ceiling(pod).Requests
			if blockedBy(req, smallest, remaining) {
				blocked++
				for r := range remaining {
					q := need[r]
					q.Add(requestOf(req, r))
					need[r] = q
				}
			}
		}
		if blocked == 0 {
			continue
		}
		shortfall := corev1.ResourceList{}
		for r, left := range remaining {
			short := need[r]
			short.Sub(left)
			if short.Sign() > 0 {
				shortfall[r] = short
			}
		}
		c.releaseUntilCovered(ctx, list, ScopeLimit, shortfall, done)
	}
	return nil
}

// releaseForQuota releases held empty nodes of every vendor and region where a
// launch is refused for quota: what the cloud provider reported, and what the
// quota tracker says.
func (c *Controller) releaseForQuota(ctx context.Context, cands []*candidate, pending []*corev1.Pod,
	refusals map[QuotaScope]int64, done map[string]bool) error {
	want := map[QuotaScope]int64{}
	maps.Copy(want, refusals)
	if c.opts.Quota != nil {
		scopes, err := c.opts.Quota.BlockedScopes(ctx)
		if err != nil {
			return fmt.Errorf("reading the quota scopes: %w", err)
		}
		var cpu int64
		for _, p := range pending {
			q := karpresources.Ceiling(p).Requests[corev1.ResourceCPU]
			cpu += q.Value()
		}
		for _, s := range scopes {
			want[s] = max(want[s], cpu)
		}
	}
	for sc, cpu := range want {
		if sc.CapacityType != karpv1.CapacityTypeOnDemand {
			continue
		}
		var list []*candidate
		for _, cand := range cands {
			if cand.in.vendor == sc.Vendor && cand.in.region == sc.Region {
				list = append(list, cand)
			}
		}
		if len(list) == 0 {
			continue
		}
		// Releases of an earlier tick that are still on their way out already
		// cover part of the request.
		covered := c.releasing(ctx, sc)
		short := resource.NewQuantity(max(cpu, 1)-covered, resource.DecimalSI)
		if short.Sign() <= 0 {
			continue
		}
		if c.releaseUntilCovered(ctx, list, ScopeQuota, corev1.ResourceList{corev1.ResourceCPU: *short}, done) {
			c.opts.Signals.clear(sc)
		}
	}
	return nil
}

// releasing is the vCPUs of the nodes of the scope that a release under quota
// pressure is already removing.
func (c *Controller) releasing(ctx context.Context, s QuotaScope) int64 {
	var nodes corev1.NodeList
	if err := c.opts.Client.List(ctx, &nodes); err != nil {
		return 0
	}
	var cpu int64
	for i := range nodes.Items {
		n := &nodes.Items[i]
		t := releasingTaint(n)
		if t == nil || t.Value != ScopeQuota {
			continue
		}
		vendor, region, _, err := scope.ParseProviderID(n.Spec.ProviderID)
		if err == nil && vendor == s.Vendor && region == s.Region {
			q := n.Status.Capacity[corev1.ResourceCPU]
			cpu += q.Value()
		}
	}
	return cpu
}

// releaseUntilCovered releases candidates in order until the capacity they free
// covers the shortfall (at least one node), and says whether any was released.
func (c *Controller) releaseUntilCovered(ctx context.Context, list []*candidate, why string,
	shortfall corev1.ResourceList, done map[string]bool) bool {
	freed := corev1.ResourceList{}
	released := 0
	for _, cand := range list {
		if done[cand.in.node.Name] {
			continue
		}
		if released > 0 && covers(freed, shortfall) {
			break
		}
		if !c.release(ctx, cand, why, done) {
			continue
		}
		released++
		for r := range shortfall {
			q := freed[r]
			q.Add(capacityOf(cand, r))
			freed[r] = q
		}
	}
	return released > 0
}

func covers(freed, shortfall corev1.ResourceList) bool {
	for r, want := range shortfall {
		got := freed[r]
		if got.Cmp(want) < 0 {
			return false
		}
	}
	return true
}

// capacityOf is what a node frees of a resource; a node frees itself for the
// "nodes" limit.
func capacityOf(cand *candidate, r corev1.ResourceName) resource.Quantity {
	if r == karpresources.Node {
		return resource.MustParse("1")
	}
	return cand.capacity[r]
}

func requestOf(req corev1.ResourceList, r corev1.ResourceName) resource.Quantity {
	if r == karpresources.Node {
		return resource.MustParse("1")
	}
	return req[r]
}

// blockedBy reports whether a pod's request, or a node as small as the smallest
// held one, no longer fits the room a NodePool's limits leave.
func blockedBy(req, smallest, remaining corev1.ResourceList) bool {
	for r, left := range remaining {
		if left.Sign() <= 0 {
			return true
		}
		need := requestOf(req, r)
		if s := smallest[r]; s.Cmp(need) > 0 {
			need = s
		}
		if need.Cmp(left) > 0 {
			return true
		}
	}
	return false
}
