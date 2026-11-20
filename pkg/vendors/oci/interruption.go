/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultInterruptionPoll is the time between two polls of a region
// (--oci-interruption-poll): six list calls a minute whatever the number of nodes.
const DefaultInterruptionPoll = 10 * time.Second

// deletedTTL is how long the driver remembers an instance it terminated: far
// longer than OCI shows TERMINATING.
const deletedTTL = time.Hour

// NoticeWindow implements vendors.InterruptionWindow. OCI documents a notice of
// about two minutes before the termination begins, as an Events message
// (com.oraclecloud.computeapi.instancepreemptionaction), which the driver does not
// read. What the poll sees is the TERMINATING state, after the notice: the
// deadline is the next moments, so MinNotice is short.
func (d *Driver) NoticeWindow() vendors.NoticeWindow {
	return vendors.NoticeWindow{MinNotice: 5 * time.Second, MaxNotice: 2 * time.Minute}
}

// PollInterval implements vendors.InterruptionPoller.
func (d *Driver) PollInterval() time.Duration {
	if d.interruptionPoll > 0 {
		return d.interruptionPoll
	}
	return DefaultInterruptionPoll
}

// PollInterruptions implements vendors.InterruptionPoller (plan 26, section 4).
// The cheapest signal the Compute API gives is the lifecycle state in the list the
// driver already makes (one paged ListInstances of the compartment per region, no
// extra permission): a preemptible instance of this cluster that is STOPPING,
// STOPPED or TERMINATING, because nobody can stop a preemptible instance and the
// driver's own terminations are skipped. The instance has no field with the time of
// the preemption, no work request for it is documented and the notice's two
// minutes are only in the Events service, so the notice has no deadline: the
// controller assumes NoticeWindow().MinNotice from now. Faster than garbage
// collection, which waits for the Node to be NotReady.
func (d *Driver) PollInterruptions(ctx context.Context, region string) ([]vendors.Notice, error) {
	ok, err := d.subscribed(ctx, region)
	if err != nil || !ok {
		return nil, err
	}
	list, err := d.listRegion(ctx, region, "")
	if err != nil {
		return nil, err
	}
	var out []vendors.Notice
	for _, in := range list {
		inst := ToInstance(region, in)
		if inst.GoneReason == "" || inst.State != vendors.StateTerminating ||
			in.FreeformTags[TagKey(vendors.TagCluster)] != d.cfg.ClusterName || d.wasDeleted(inst.Ref) {
			continue
		}
		out = append(out, vendors.Notice{Ref: inst.Ref, Reason: v1alpha1.ReasonSpotInterrupted})
	}
	return out, nil
}

// noteDeleted remembers that the driver terminated the instance.
func (d *Driver) noteDeleted(ref vendors.InstanceRef) {
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for r, at := range d.deleted {
		if now.Sub(at) > deletedTTL {
			delete(d.deleted, r)
		}
	}
	d.deleted[ref] = now
}

func (d *Driver) wasDeleted(ref vendors.InstanceRef) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.deleted[ref]
	return ok
}
