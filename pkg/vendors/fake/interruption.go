//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// noticeWindows are the lead times of the vendors with spot instances (plan 26
// section 6), so that the fake impersonates the one it was told to. A vendor
// not listed (no spot) gets the default window.
var noticeWindows = map[string]vendors.NoticeWindow{
	"aws":      {MinNotice: 2 * time.Minute, MaxNotice: 2 * time.Minute},
	"gcp":      {MinNotice: 30 * time.Second, MaxNotice: 2*time.Minute + 30*time.Second},
	"azure":    {MinNotice: 30 * time.Second, MaxNotice: 30 * time.Second},
	"oci":      {MinNotice: 2 * time.Minute, MaxNotice: 2 * time.Minute},
	"alicloud": {MinNotice: 5 * time.Minute, MaxNotice: 5 * time.Minute},
}

// NoticeWindow implements vendors.InterruptionWindow: the window of the
// impersonated vendor, unless --fake-min-notice or --fake-max-notice set one.
func (d *Driver) NoticeWindow() vendors.NoticeWindow {
	w, ok := noticeWindows[d.Vendor()]
	if !ok {
		w = vendors.DefaultNoticeWindow
	}
	if d.opts.MinNotice > 0 {
		w.MinNotice = d.opts.MinNotice
	}
	if d.opts.MaxNotice > 0 {
		w.MaxNotice = d.opts.MaxNotice
	}
	return w
}

var _ vendors.InterruptionWindow = (*Driver)(nil)

// PollInterval implements vendors.InterruptionPoller.
func (d *Driver) PollInterval() time.Duration {
	if d.opts.PollInterval > 0 {
		return d.opts.PollInterval
	}
	return defaultPollInterval
}

// PollInterruptions implements vendors.InterruptionPoller: the running
// instances of the region that Interrupt announced, as a vendor API that shows a
// preemption before it happens.
func (d *Driver) PollInterruptions(ctx context.Context, region string) ([]vendors.Notice, error) {
	d.refreshConfig(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []vendors.Notice
	for _, i := range d.instances {
		if i.Region == region && i.State == vendors.StateRunning && !i.noticeDeadline.IsZero() {
			out = append(out, vendors.Notice{
				Ref: i.Ref, Reason: v1alpha1.ReasonSpotInterrupted, Deadline: i.noticeDeadline,
			})
		}
	}
	slices.SortFunc(out, func(a, b vendors.Notice) int { return strings.Compare(string(a.Ref), string(b.Ref)) })
	return out, nil
}

// Interrupt announces the reclaim of a running instance at the deadline: from
// now on PollInterruptions lists it. The fake does not take the instance away
// at the deadline; whoever deletes it (the NodeClaim's deletion) ends it. It
// reports whether the instance was running.
func (d *Driver) Interrupt(ref vendors.InstanceRef, deadline time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	inst, ok := d.instances[ref.ID()]
	if !ok || inst.Region != ref.Region() || inst.State != vendors.StateRunning {
		return false
	}
	inst.noticeDeadline = deadline
	d.record("interrupt", inst.Region, inst.Zone, inst.Type, inst.CapacityType, inst.claim, "ok")
	return true
}

// Reclaim takes an instance away with no notice, the way a vendor reclaims a
// spot instance or a host dies: it is terminated at once and its Node is
// deleted (a kwok Node would stay Ready for ever). The driver gives no reason,
// like a vendor that cannot tell a reclaim from a crash. It reports whether the
// instance existed and was not terminated.
func (d *Driver) Reclaim(ref vendors.InstanceRef) bool { return d.ReclaimWithReason(ref, "") }

// ReclaimWithReason is Reclaim for a vendor that tells: reason is the
// Instance.GoneReason (v1alpha1.ReasonSpotInterrupted).
func (d *Driver) ReclaimWithReason(ref vendors.InstanceRef, reason string) bool {
	d.mu.Lock()
	inst, ok := d.instances[ref.ID()]
	if !ok || inst.Region != ref.Region() || inst.State == vendors.StateTerminated {
		d.mu.Unlock()
		return false
	}
	inst.State, inst.GoneReason = vendors.StateTerminated, reason
	node := inst.nodeName
	d.record("reclaim", inst.Region, inst.Zone, inst.Type, inst.CapacityType, inst.claim, "ok")
	d.mu.Unlock()
	if err := d.deleteNode(context.Background(), node); err != nil {
		d.cfg.Log.Error(err, "deleting the Node of a reclaimed instance", "node", node)
	}
	return true
}

func (d *Driver) initHandled() {
	if d.handled == nil {
		d.handled = map[string]bool{}
	}
}

// interruptClaims interrupts, once per NodeClaim of the interrupt key, its
// running instance with the deadline the given seconds from now.
func (d *Driver) interruptClaims(rules []interruptRule) {
	for _, r := range rules {
		d.mu.Lock()
		d.initHandled()
		done := d.handled["interrupt/"+r.NodeClaim]
		var ref vendors.InstanceRef
		for _, inst := range d.instances {
			if inst.claim == r.NodeClaim && inst.State == vendors.StateRunning {
				ref = inst.Ref
			}
		}
		if ref != "" {
			d.handled["interrupt/"+r.NodeClaim] = true
		}
		d.mu.Unlock()
		if !done && ref != "" {
			d.Interrupt(ref, d.clock.Now().Add(time.Duration(r.DeadlineSeconds)*time.Second))
		}
	}
}

// reclaimClaims reclaims, once per NodeClaim of the reclaim key, its running
// instance.
func (d *Driver) reclaimClaims(rules []reclaimRule) {
	for _, r := range rules {
		d.mu.Lock()
		d.initHandled()
		done := d.handled["reclaim/"+r.NodeClaim]
		var ref vendors.InstanceRef
		for _, inst := range d.instances {
			if inst.claim == r.NodeClaim && inst.State != vendors.StateTerminated {
				ref = inst.Ref
			}
		}
		if ref != "" {
			d.handled["reclaim/"+r.NodeClaim] = true
		}
		d.mu.Unlock()
		if !done && ref != "" {
			d.ReclaimWithReason(ref, r.Reason)
		}
	}
}

var _ vendors.InterruptionPoller = (*Driver)(nil)
