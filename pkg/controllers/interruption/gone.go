/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// vanishedDedupe is how long an on-demand instance found gone is remembered, so
// that the several Delete calls core makes for one NodeClaim record it once.
const vanishedDedupe = time.Hour

// InstanceGone records an instance that was found gone without SCarpenter having
// deleted it (plan 26 section 5): the CloudProvider calls it from Delete. Core's
// garbage collection has already deleted (or is about to delete) the NodeClaim,
// so nothing is deleted here; this only gives the loss its reason.
//
// A spot NodeClaim gets the Interrupted condition, the offering is marked
// unavailable and the interruption counted; the reason is SpotInterrupted
// (source vendor-api) when the driver says the vendor reclaimed the instance
// (goneReason), else ReclaimedWithoutNotice (source gc). An on-demand NodeClaim
// only gets an InstanceVanished Event and the count. A NodeClaim that already
// carries a notice was handled by Reconcile and is left alone.
func (c *Controller) InstanceGone(ctx context.Context, claim *karpv1.NodeClaim, vendor, goneReason string) {
	if _, noticed := claim.Annotations[v1alpha1.AnnotationInterruption]; noticed {
		return
	}
	spot := claim.Labels[karpv1.CapacityTypeLabelKey] == karpv1.CapacityTypeSpot
	reason, source := v1alpha1.ReasonInstanceVanished, v1alpha1.SourceGC
	if spot {
		reason = v1alpha1.ReasonReclaimedWithoutNotice
		if goneReason == v1alpha1.ReasonSpotInterrupted {
			reason, source = v1alpha1.ReasonSpotInterrupted, v1alpha1.SourceVendorAPI
		}
	}
	if spot {
		first, err := c.markGone(ctx, claim, reason)
		if err != nil {
			log.FromContext(ctx).Error(err, "recording the loss of the spot instance", "nodeclaim", claim.Name)
			return
		}
		if !first {
			return
		}
	} else if !c.firstVanished(claim) {
		return
	}

	instanceType, zone := claim.Labels[corev1.LabelInstanceTypeStable], claim.Labels[corev1.LabelTopologyZone]
	if spot && c.opts.Unavailable != nil && instanceType != "" && zone != "" {
		c.opts.Unavailable.MarkInterrupted(vendor, instanceType, zone, karpv1.CapacityTypeSpot)
	}
	c.metrics.interruptions.WithLabelValues(vendor, instanceType, zone, reason, source).Inc()
	msg := fmt.Sprintf("%s instance %s in %s is gone and SCarpenter did not delete it (%s)",
		vendor, instanceType, zone, reason)
	if reason == v1alpha1.ReasonSpotInterrupted {
		msg = fmt.Sprintf("%s spot instance %s in %s was reclaimed by the vendor, no notice was seen",
			vendor, instanceType, zone)
	}
	c.publish(0, corev1.EventTypeWarning, reason, msg, claim)
}

// markGone sets the Interrupted condition of a spot NodeClaim and reports
// whether this call set it (false: it was set already, or the claim is gone).
// DisruptionReason is not set: the node is already drained when this runs.
func (c *Controller) markGone(ctx context.Context, claim *karpv1.NodeClaim, reason string) (first bool, err error) {
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &karpv1.NodeClaim{}
		if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: claim.Name}, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		_, noticed := fresh.Annotations[v1alpha1.AnnotationInterruption]
		if noticed || fresh.StatusConditions().Get(v1alpha1.ConditionInterrupted) != nil {
			return nil
		}
		stored := fresh.DeepCopy()
		fresh.StatusConditions().SetTrueWithReason(v1alpha1.ConditionInterrupted, reason, reason)
		patch := client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})
		if err := c.opts.Client.Status().Patch(ctx, fresh, patch); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err //nolint:wrapcheck // RetryOnConflict needs the conflict itself
		}
		first = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("setting the Interrupted condition of NodeClaim %s: %w", claim.Name, err)
	}
	return first, nil
}

// firstVanished remembers the NodeClaim, in memory, and reports whether it was
// not remembered already.
func (c *Controller) firstVanished(claim *karpv1.NodeClaim) bool {
	now := c.opts.Clock.Now()
	c.vanishedMu.Lock()
	defer c.vanishedMu.Unlock()
	for uid, at := range c.vanished {
		if now.Sub(at) > vanishedDedupe {
			delete(c.vanished, uid)
		}
	}
	if _, ok := c.vanished[claim.UID]; ok {
		return false
	}
	c.vanished[claim.UID] = now
	return true
}
