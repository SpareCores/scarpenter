/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"context"
	"time"
)

// NoticeWindow is how far ahead a vendor announces the loss of a spot or
// preemptible instance. The interruption controller clamps a node's claimed
// deadline into it: a notice is never believed to give more than MaxNotice, and
// a notice without a usable deadline gives MinNotice.
type NoticeWindow struct {
	// MinNotice is the shortest lead time the vendor documents.
	MinNotice time.Duration
	// MaxNotice is the longest.
	MaxNotice time.Duration
}

// DefaultNoticeWindow is used for a driver that declares none: the shortest documented
// lead (Azure's 30 s) and a few minutes above the longest (Alibaba's 5 min).
var DefaultNoticeWindow = NoticeWindow{MinNotice: 30 * time.Second, MaxNotice: 5 * time.Minute}

// InterruptionWindow is the optional capability of a driver whose vendor has
// spot or preemptible instances (plan 26). A driver without it gets
// DefaultNoticeWindow.
type InterruptionWindow interface {
	NoticeWindow() NoticeWindow
}

// NoticeWindowOf returns the driver's notice window, or the default.
func NoticeWindowOf(d Driver) NoticeWindow {
	if w, ok := d.(InterruptionWindow); ok {
		nw := w.NoticeWindow()
		if nw.MinNotice > 0 && nw.MaxNotice >= nw.MinNotice {
			return nw
		}
	}
	return DefaultNoticeWindow
}

// Notice is the vendor's announcement that it will take an instance away.
type Notice struct {
	Ref InstanceRef
	// Reason is v1alpha1.ReasonSpotInterrupted; another value is ignored by
	// the interruption controller, with an Event.
	Reason string
	// Deadline is when the vendor takes the instance. Zero when the API only
	// shows that the instance is going (a terminating state): the controller
	// then assumes the vendor's shortest notice from now.
	Deadline time.Time
}

// InterruptionPoller is the optional capability of a driver whose vendor shows
// an interruption in its API instead of (or besides) the instance metadata
// (plan 26 section 4). The core runs one poller per (vendor, region) that has a
// spot NodeClaim, however many instances there are, so PollInterruptions makes a
// bounded number of calls per region: one paged list, not one call per
// instance.
type InterruptionPoller interface {
	// PollInterruptions returns the notices of the cluster's instances in the
	// region that are announced or under way; an instance that is already
	// terminated is not one. It may return the same notice on every poll while
	// the instance lives. Errors carry their class (ClassOf): Retryable ones
	// (throttling included) back the poller off.
	PollInterruptions(ctx context.Context, region string) ([]Notice, error)
	// PollInterval is the time between two polls of a region.
	PollInterval() time.Duration
}
