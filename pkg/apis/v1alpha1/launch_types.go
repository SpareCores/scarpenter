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
	// InsufficientCapacity. At most 4m30s, so that it ends before Karpenter's
	// 5m launch timeout; a vendor call that has started is not interrupted.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('4m30s')",message="maxDuration must be greater than 0 and at most 4m30s"
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`

	// insufficientCapacityTTL is how long this class keeps away from an
	// offering that had no capacity. It can only extend the controller's
	// --unavailable-insufficient-capacity-ttl for this class: every class
	// still shares the controller's mark.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="insufficientCapacityTTL must be greater than 0"
	// +optional
	InsufficientCapacityTTL *metav1.Duration `json:"insufficientCapacityTTL,omitempty"`
}
