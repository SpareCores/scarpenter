/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// DefaultTerminatingGrace is how long an instance may be seen terminating before
// the core deletes it again (--terminating-grace). Instances a vendor is really
// deleting disappear well inside it; a stopped or powered-off instance, which
// every driver reports terminating, never does.
const DefaultTerminatingGrace = 10 * time.Minute

// TerminatingTracker remembers when each instance was first seen terminating, in
// memory only: a restart restarts the clock (like the idle time of a managed
// resource). It is safe for concurrent use.
type TerminatingTracker struct {
	clock clock.Clock
	grace time.Duration

	mu    sync.Mutex
	first map[string]time.Time
}

// NewTerminatingTracker returns a tracker; a nil clock is the real one and a
// grace of zero or less is DefaultTerminatingGrace.
func NewTerminatingTracker(c clock.Clock, grace time.Duration) *TerminatingTracker {
	if c == nil {
		c = clock.RealClock{}
	}
	if grace <= 0 {
		grace = DefaultTerminatingGrace
	}
	return &TerminatingTracker{clock: c, grace: grace, first: map[string]time.Time{}}
}

func trackerKey(vendor string, ref InstanceRef) string { return vendor + "|" + string(ref) }

// Expired records the sighting of a terminating instance and reports whether it
// has been terminating for longer than the grace period.
func (t *TerminatingTracker) Expired(vendor string, ref InstanceRef) bool {
	now := t.clock.Now()
	key := trackerKey(vendor, ref)
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.first[key]
	if !ok {
		t.first[key] = now
		return false
	}
	return now.Sub(at) >= t.grace
}

// Forget drops the instance (it is gone, or it is not terminating any more).
func (t *TerminatingTracker) Forget(vendor string, ref InstanceRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.first, trackerKey(vendor, ref))
}

// Retain drops every instance of the vendor and region that is not in keep: the
// instances a complete listing of the region no longer shows as terminating.
func (t *TerminatingTracker) Retain(vendor, region string, keep map[InstanceRef]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prefix := vendor + "|" + region + "/"
	for key := range t.first {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix && !keep[InstanceRef(key[len(vendor)+1:])] {
			delete(t.first, key)
		}
	}
}
