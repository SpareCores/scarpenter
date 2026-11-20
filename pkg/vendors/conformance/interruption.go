/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// interruptionPoller checks the optional vendors.InterruptionPoller: an
// instance nobody announced has no notice, and, with the fixture's Interrupt
// hook, an announced one is listed with the reason and its deadline, an
// unannounced neighbour is not, and a deleted one is not listed any more.
func interruptionPoller(t *testing.T, e *env) {
	p, ok := e.d.(vendors.InterruptionPoller)
	if !ok {
		t.Skip("the driver has no InterruptionPoller")
	}
	if p.PollInterval() <= 0 {
		t.Errorf("PollInterval = %s, want a positive interval", p.PollInterval())
	}
	inst := e.create(t, e.request(t))
	other := e.create(t, e.request(t))
	notices := func() map[vendors.InstanceRef]vendors.Notice {
		t.Helper()
		list, err := p.PollInterruptions(t.Context(), e.region)
		if err != nil {
			t.Fatalf("PollInterruptions(%s): %v", e.region, err)
		}
		out := map[vendors.InstanceRef]vendors.Notice{}
		for _, n := range list {
			out[n.Ref] = n
		}
		return out
	}
	if got := notices(); len(got) != 0 {
		t.Fatalf("PollInterruptions of instances nobody announced returned %v", got)
	}
	if e.fx.Interrupt == nil {
		t.Skip("the fixture has no Interrupt hook")
	}
	deadline := time.Now().Add(time.Minute).Truncate(time.Second)
	e.fx.Interrupt(t, e.d, inst.Ref, deadline)
	got := notices()
	n, ok := got[inst.Ref]
	if !ok || len(got) != 1 {
		t.Fatalf("PollInterruptions after the notice of %s returned %v, want only it", inst.Ref, got)
	}
	if n.Reason != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("the notice reason = %q, want %q", n.Reason, v1alpha1.ReasonSpotInterrupted)
	}
	if !n.Deadline.IsZero() && !n.Deadline.Equal(deadline) {
		t.Errorf("the notice deadline = %s, want %s (or none)", n.Deadline, deadline)
	}
	if _, ok := got[other.Ref]; ok {
		t.Errorf("the instance %s that was not announced has a notice", other.Ref)
	}
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got := notices(); len(got) != 0 {
		t.Errorf("PollInterruptions after the delete returned %v, want none", got)
	}
}
