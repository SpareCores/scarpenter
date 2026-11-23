/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Event reasons of the CloudProvider, on the SCNodeClass.
const (
	// EventLaunchFailed: a vendor call failed with a Fatal error (or a call of the
	// launch's preparation timed out, and the region is tried again later); the
	// NodeClaim is retried until core's launch timeout.
	EventLaunchFailed = "LaunchFailed"
	// EventOfferingRejected: the vendor rejected the launch of an offering with
	// an error that is not known to be fatal for every type; the offering is
	// unavailable for an hour. The message is the vendor's.
	EventOfferingRejected = "OfferingRejected"
	// EventImageFailed: no candidate could be launched because its image could
	// not be resolved.
	EventImageFailed = "ImageResolutionFailed"
)

// eventDedupe is how long one event of a (class, vendor, code) is published
// once.
const eventDedupe = 5 * time.Minute

func (c *CloudProvider) publish(nc *v1alpha1.SCNodeClass, reason, message string, dedupe ...string) {
	if c.opts.Recorder == nil || nc == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: nc,
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        message,
		DedupeValues:   append([]string{nc.Name}, dedupe...),
		DedupeTimeout:  eventDedupe,
	})
}
