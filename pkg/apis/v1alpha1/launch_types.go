/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LaunchSpec is the launch retry policy of the NodeClass. Every field is
// optional; an unset one keeps the controller's flag. Changing it is not drift.
type LaunchSpec struct {
	// maxAttempts is how many vendor calls one NodeClaim creation may make
	// before it gives up (--max-launch-attempts for this class). Candidates
	// skipped for lack of quota headroom do not count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxAttempts *int32 `json:"maxAttempts,omitempty"`

	// maxDuration is the wall-clock budget of one NodeClaim creation: once it
	// is spent, no further candidate is tried and the launch fails with
	// InsufficientCapacity. It bounds when an attempt starts, not when it ends:
	// the worst case is maxDuration plus one vendor call (a GCP launch may wait
	// for the operation), and the first candidate is always tried. A Retryable
	// error is requeued by Karpenter and starts a fresh budget. At most 4m30s,
	// to stay under Karpenter's 5m launch timeout.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('4m30s')",message="maxDuration must be greater than 0 and at most 4m30s"
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`

	// insufficientCapacityTTL is how long this class keeps away from an
	// offering that had no capacity. It can only extend the controller's
	// --unavailable-insufficient-capacity-ttl for this class: every class
	// still shares the controller's mark. At most 24h. A change applies to the
	// marks made after it; the existing ones keep their expiry.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('24h')",message="insufficientCapacityTTL must be greater than 0 and at most 24h"
	// +optional
	InsufficientCapacityTTL *metav1.Duration `json:"insufficientCapacityTTL,omitempty"`

	// initializationTimeout is how long a node of this class may stay
	// registered without becoming Initialized (Ready, startup taints gone,
	// resources registered), counted from the creation of its Node. Once it is
	// over, SCarpenter deletes the NodeClaim and Karpenter terminates the
	// instance. Core has no deadline for this state. Default 1h; 0s disables
	// the check; at most 24h. A change applies to the nodes that are still
	// uninitialized within about 10 minutes.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('24h')",message="initializationTimeout must be at least 0 and at most 24h"
	// +optional
	InitializationTimeout *metav1.Duration `json:"initializationTimeout,omitempty"`
}
