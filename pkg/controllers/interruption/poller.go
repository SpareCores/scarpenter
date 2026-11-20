/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Poller defaults.
const (
	// DefaultSyncInterval is how often the poller looks for regions that gained
	// or lost their last spot NodeClaim.
	DefaultSyncInterval = 5 * time.Second
	// DefaultMaxBackoff caps the wait after failed polls.
	DefaultMaxBackoff = 5 * time.Minute
	// defaultPollInterval is used when a driver reports none.
	defaultPollInterval = 10 * time.Second
)

// PollerOptions tunes a Poller; zero values are the defaults.
type PollerOptions struct {
	SyncInterval time.Duration
	MaxBackoff   time.Duration
}

// Poller asks the drivers that can (vendors.InterruptionPoller) for interruption
// notices: one poll loop per (vendor, region) that has a spot NodeClaim, shared
// by all its instances, stopped when the last such NodeClaim is gone. A notice is
// written to the Node of its NodeClaim as the annotations the node agent writes
// (source vendor-api), so the Controller reacts to it the same way. One failed
// poll never stops anything: it is counted and the loop backs off.
type Poller struct {
	c    *Controller
	opts PollerOptions

	mu      sync.Mutex
	running map[pollKey]context.CancelFunc
	wg      sync.WaitGroup
}

type pollKey struct{ vendor, region string }

// Poller returns the poller that shares the controller's dependencies and
// metrics. Add it to the manager as a runnable.
func (c *Controller) Poller(o PollerOptions) *Poller {
	if o.SyncInterval <= 0 {
		o.SyncInterval = DefaultSyncInterval
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	return &Poller{c: c, opts: o, running: map[pollKey]context.CancelFunc{}}
}

// NeedLeaderElection makes the manager run the poller on the leader only.
func (p *Poller) NeedLeaderElection() bool { return true }

// Start syncs the poll loops with the NodeClaims until ctx ends, then waits for
// the loops to stop.
func (p *Poller) Start(ctx context.Context) error {
	defer p.wg.Wait()
	for {
		if err := p.Sync(ctx); err != nil {
			log.FromContext(ctx).Error(err, "syncing the interruption pollers")
		}
		timer := p.c.opts.Clock.NewTimer(p.opts.SyncInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.stopAll()
			return nil
		case <-timer.C():
		}
	}
}

// Sync starts the poll loop of every (vendor, region) that has a spot NodeClaim
// and whose driver can poll, and stops the loops of the others.
func (p *Poller) Sync(ctx context.Context) error {
	claims := &karpv1.NodeClaimList{}
	if err := p.c.opts.Client.List(ctx, claims); err != nil {
		return fmt.Errorf("listing the NodeClaims: %w", err)
	}
	want := map[pollKey]vendors.InterruptionPoller{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if !managed(claim) || claim.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeSpot {
			continue
		}
		vendor, region, _, err := scope.ParseProviderID(claim.Status.ProviderID)
		if err != nil || p.c.opts.Registry == nil {
			continue
		}
		drv, ok := p.c.opts.Registry.Driver(vendor)
		if !ok {
			continue
		}
		if poller, ok := drv.(vendors.InterruptionPoller); ok {
			want[pollKey{vendor, region}] = poller
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, stop := range p.running {
		if _, ok := want[key]; !ok {
			stop()
			delete(p.running, key)
		}
	}
	for key, poller := range want {
		if _, ok := p.running[key]; ok {
			continue
		}
		loopCtx, stop := context.WithCancel(ctx)
		p.running[key] = stop
		p.wg.Go(func() { p.loop(loopCtx, key, poller) })
	}
	return nil
}

// Running returns the (vendor, region) pairs that have a poll loop, as
// "vendor/region".
func (p *Poller) Running() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.running))
	for key := range p.running {
		out = append(out, key.vendor+"/"+key.region)
	}
	return out
}

func (p *Poller) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, stop := range p.running {
		stop()
		delete(p.running, key)
	}
}

// loop polls the region until ctx ends. The wait doubles from the poll interval
// on each failed poll, up to the maximum, and resets on a success. A notice that
// cannot be written yet (its NodeClaim has no Node) is kept and written once the
// Node exists, even if the vendor no longer shows it; it is dropped when its
// NodeClaim is gone.
func (p *Poller) loop(ctx context.Context, key pollKey, poller vendors.InterruptionPoller) {
	logger := log.FromContext(ctx).WithValues("vendor", key.vendor, "region", key.region)
	pending := map[vendors.InstanceRef]vendors.Notice{}
	var backoff time.Duration
	for {
		interval := poller.PollInterval()
		if interval <= 0 {
			interval = defaultPollInterval
		}
		notices, err := poller.PollInterruptions(ctx, key.region)
		wait := interval
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			p.c.metrics.pollErrors.WithLabelValues(key.vendor, key.region).Inc()
			backoff = min(max(2*backoff, 2*interval), p.opts.MaxBackoff)
			wait = backoff
			logger.Error(err, "polling for interruption notices failed", "retry-in", wait.String())
		default:
			backoff = 0
			for _, n := range notices {
				pending[n.Ref] = n
			}
		}
		if err := p.report(ctx, pending); err != nil {
			logger.Error(err, "reporting interruption notices")
		}
		timer := p.c.opts.Clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

// managed reports whether the NodeClaim is SCarpenter's.
func managed(claim *karpv1.NodeClaim) bool {
	ref := claim.Spec.NodeClassRef
	return ref != nil && ref.Group == v1alpha1.Group && ref.Kind == nodeClassKind
}

// report writes each pending notice to the Node of its NodeClaim and drops the
// notices that are done: written, the Node has a notice already, the
// NodeClaim's instance was deleted already (InstanceTerminating), or the
// NodeClaim is not SCarpenter's (gone, or another cluster's instance).
func (p *Poller) report(ctx context.Context, pending map[vendors.InstanceRef]vendors.Notice) error {
	if len(pending) == 0 {
		return nil
	}
	claims := &karpv1.NodeClaimList{}
	if err := p.c.opts.Client.List(ctx, claims); err != nil {
		return fmt.Errorf("listing the NodeClaims: %w", err)
	}
	byRef := map[string]*karpv1.NodeClaim{}
	for i := range claims.Items {
		if ref := claims.Items[i].Annotations[v1alpha1.AnnotationInstanceRef]; ref != "" && managed(&claims.Items[i]) {
			byRef[ref] = &claims.Items[i]
		}
	}
	var firstErr error
	for ref, n := range pending {
		claim, ok := byRef[ref.String()]
		// Core sets InstanceTerminating once a Delete of the instance went
		// through: the drain is over, and a terminating instance is most likely
		// SCarpenter's own delete. The condition is stored on the NodeClaim, so
		// this holds after a restart, when a driver no longer remembers what it
		// deleted.
		if !ok || claim.StatusConditions().Get(karpv1.ConditionTypeInstanceTerminating).IsTrue() {
			delete(pending, ref)
			continue
		}
		done, err := p.annotate(ctx, claim, n)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if done {
			delete(pending, ref)
		}
	}
	return firstErr
}

// annotate writes the notice on the Node and reports whether the notice is
// done. A NodeClaim without a Node (not registered yet) is not done. A Node
// that has a SpotInterrupted notice already (the node agent's) keeps it; any
// other value is the node's own, which the controller ignores, and is
// overwritten so that it cannot hide the vendor's notice.
func (p *Poller) annotate(ctx context.Context, claim *karpv1.NodeClaim, n vendors.Notice) (bool, error) {
	if claim.Status.NodeName == "" {
		return false, nil
	}
	node := &corev1.Node{}
	if err := p.c.opts.Client.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, node); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if node.Annotations[v1alpha1.AnnotationInterruption] == v1alpha1.ReasonSpotInterrupted {
		return true, nil
	}
	reason := n.Reason
	if reason == "" {
		reason = v1alpha1.ReasonSpotInterrupted
	}
	want := map[string]string{
		v1alpha1.AnnotationInterruption:       reason,
		v1alpha1.AnnotationInterruptionSource: v1alpha1.SourceVendorAPI,
	}
	stored := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	// A deadline the node wrote must not stand in for the vendor's.
	delete(node.Annotations, v1alpha1.AnnotationInterruptionDeadline)
	if !n.Deadline.IsZero() {
		want[v1alpha1.AnnotationInterruptionDeadline] = n.Deadline.UTC().Format(time.RFC3339)
	}
	maps.Copy(node.Annotations, want)
	if err := p.c.opts.Client.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
		return false, fmt.Errorf("annotating Node %s with the interruption notice: %w", node.Name, err)
	}
	log.FromContext(ctx).WithValues("node", node.Name, "nodeclaim", claim.Name, "reason", reason).
		Info("found an interruption notice through the vendor API")
	return true, nil
}
