/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ConditionTypeBillingRetention is informational and not a dependent of Ready:
// whether billing retention is active for at least one vendor of the
// NodeClass, and how many nodes it leaves alone (ADR-024).
const ConditionTypeBillingRetention = "BillingRetention"

// Annotations and the taint of billing retention (ADR-024, plan 23).
const (
	// AnnotationBillingStart is the vendor's billing-start stamp of the
	// instance (RFC 3339), returned by Create on the NodeClaim and copied by
	// core to the Node at registration.
	AnnotationBillingStart = Group + "/billing-start"
	// AnnotationRetainUntil is set by the retention controller on a Node it
	// holds, with the deadline (RFC 3339). It is also the marker that the
	// controller put karpenter.sh/do-not-disrupt there: only then is that
	// annotation removed by it.
	AnnotationRetainUntil = Group + "/retain-until"
	// TaintReleasing is the NoSchedule taint of the release sequence: the node
	// is about to be deleted at the end of its paid period.
	TaintReleasing = Group + "/releasing"
)

// BillingSpec is the opt-in to keeping nodes until the end of the billing
// period they have already paid for (ADR-024). Each vendor's own billing rule
// decides whether the setting has any effect; the NodeClass status says so.
type BillingSpec struct {
	// retainUntilPeriodEnd keeps a node a vendor bills by the started period
	// (an hour on hcloud, UpCloud, Vultr), or by a minimum and then per second
	// (Alibaba), until its paid period is about to end, so that Karpenter
	// reuses it for the next job, and deletes it before the next charge starts.
	// Off by default.
	// +kubebuilder:default=false
	// +optional
	RetainUntilPeriodEnd bool `json:"retainUntilPeriodEnd,omitempty"`

	// margin is how long before the end of the period the node is released:
	// it has to cover the drain, the termination finalizer, the vendor's
	// delete and clock skew. At least 90 s; a vendor with a slow delete
	// widens it.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('90s')",message="margin must be at least 90s"
	// +kubebuilder:default="3m"
	// +optional
	Margin *metav1.Duration `json:"margin,omitempty"`

	// minPeriod is the shortest billing increment that is held to its end:
	// shorter increments are not worth a hold. It does not gate the hold of a
	// minimum (a minimum has its own 2 minute floor).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m')",message="minPeriod must be at least 1m"
	// +kubebuilder:default="10m"
	// +optional
	MinPeriod *metav1.Duration `json:"minPeriod,omitempty"`

	// maxRetainedNodes is an optional safety valve: it trims the held empty
	// nodes of the NodeClass down to this number, nearest deadline first.
	// Unset means no cap; a launch blocked by a NodePool limit or a vendor
	// quota releases held empty nodes first either way.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxRetainedNodes *int32 `json:"maxRetainedNodes,omitempty"`
}

// BillingStatus is what billing retention comes to for one vendor of the
// NodeClass.
type BillingStatus struct {
	// vendor is the vendor ID.
	// +required
	Vendor string `json:"vendor"`

	// active says that nodes of the vendor are held until their period ends.
	// +required
	Active bool `json:"active"`

	// reason is Active, or why not: Disabled, PerSecondBilling (no minimum
	// worth holding), MinimumTooShort, Unverified, UnsupportedShape,
	// NoKeeperData, NoBillingStart (the driver records no stamp), PeriodTooShort
	// or InvalidSettings.
	// +required
	Reason string `json:"reason"`

	// message explains the reason; with Active it says increments or minimum only.
	// +optional
	Message string `json:"message,omitempty"`

	// period is the billing period that is held to its end (the increment, or
	// the minimum of a minimum-only vendor); empty when none.
	// +optional
	Period string `json:"period,omitempty"`

	// source says where the rule comes from (Keeper's vendor table, the
	// driver).
	// +optional
	Source string `json:"source,omitempty"`
}
