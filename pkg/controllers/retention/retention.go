/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package retention is the billing-period-aware node retention (ADR-024, plan
// 23). For a NodeClass that opts in (spec.billing.retainUntilPeriodEnd), a node
// of a vendor that bills by the started period (or by a minimum and then per
// second) is protected from Karpenter's voluntary disruption until a margin
// before the end of the period it has already paid for, so that the next job
// lands on it, and is then deleted by this controller before the next charge
// starts. Everything is a function of the node's billing start, the vendor's
// rule, the clock and the pods: the controller keeps no timers, a restart in
// the middle changes nothing. It shares the operator's cluster state with
// Karpenter core, so Karpenter's nomination, mark-for-deletion and
// disruption-budget accounting apply to its deletes.
package retention

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/karpenter/pkg/controllers/state"

	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Event reasons (on the Node).
const (
	EventRetainedUntil          = "RetainedUntil"
	EventReleasedAtPeriodEnd    = "ReleasedAtPeriodEnd"
	EventReleasedUnderPressure  = "ReleasedUnderPressure"
	EventReleaseBlockedByBudget = "ReleaseBlockedByBudget"
	EventRetentionSkipped       = "RetentionSkipped"
	EventBillingStartUnknown    = "BillingStartUnknown"
)

// The reasons a node is left alone (status condition, metric label and the
// RetentionSkipped Event).
const (
	SkipConsolidationNever = "ConsolidationNever"
	SkipUserDoNotDisrupt   = "UserDoNotDisrupt"
	SkipStaticPool         = "StaticPool"
	SkipSpot               = "Spot"
	SkipDrifted            = "Drifted"
	// The following leave a node alone without a message of their own: the
	// NodeClass status (or the NodeClaim) says why.
	skipInactive      = "Inactive"
	skipDeleting      = "Deleting"
	skipNoNodePool    = "NoNodePool"
	skipNoBillingTime = "NoBillingStart"
)

// The scopes of a release under pressure (the scope label of
// scarpenter_billing_released_under_pressure_total).
const (
	ScopeLimit       = "limit"
	ScopeQuota       = "quota"
	ScopeMaxRetained = "max-retained"
)

// Timing of the release sequence (plan 23 section 4).
const (
	// nominatedRetry is how long to wait before looking at a nominated node
	// again: Karpenter's nomination window is max(2*BatchMaxDuration, 10s).
	nominatedRetry = 20 * time.Second
	// drainWait is Karpenter's MinDrainTime: a kube-scheduler cycle that began
	// before the taint may still bind a pod.
	drainWait = 5 * time.Second
	// startRetry is how long to wait before asking again for a billing start
	// the driver did not give.
	startRetry = 5 * time.Minute
	// boundaryMissedAfter is how long an empty node may live past its boundary
	// before the margin is called too small.
	boundaryMissedAfter = 30 * time.Second
	// DefaultPressureInterval is how often the pressure signals are read.
	DefaultPressureInterval = 10 * time.Second
	// DefaultStatusInterval is how often the NodeClass status is recomputed.
	DefaultStatusInterval = 15 * time.Second
	// eventDedupe is the minimum time between equal Events.
	eventDedupe = 6 * time.Hour
	// requeueSlack is added to a requeue at a point in time, so that the clock
	// has passed it when the reconcile runs.
	requeueSlack = time.Second
	// refusalTTL is how long a quota refusal reported by the cloud provider
	// counts as pressure.
	refusalTTL = 3 * time.Minute
	// bootObserveWindow bounds how long after Initialized the boot time of a
	// node is observed, so that a restart does not count old nodes again.
	bootObserveWindow = 2 * time.Minute
)

// Defaults of the NodeClass settings, as the API defaults them.
const (
	DefaultMargin    = 3 * time.Minute
	DefaultMinPeriod = 10 * time.Minute
)

// Cluster is the part of Karpenter's cluster state the controller uses (the
// operator's *state.Cluster).
type Cluster interface {
	IsNodeNominated(providerID string) bool
	MarkForDeletion(providerIDs ...string)
	UnmarkForDeletion(providerIDs ...string)
	DeepCopyNodes() state.StateNodes
	NodePoolResourcesFor(nodePoolName string) corev1.ResourceList
	// PodNodeClaimMapping is the NodeClaim Karpenter decided to launch for a
	// pending pod ("" when none).
	PodNodeClaimMapping(podKey types.NamespacedName) string
}

// Rules resolves a vendor's billing rule for a region and family; keeperRow is
// false when Keeper has no row for the vendor. Empty region and family ask for
// the vendor-wide rule (the NodeClass status).
type Rules interface {
	Rule(vendor, region, family string) (rule billing.Rule, keeperRow bool)
}

// CatalogRules is Rules over the catalog's vendor table and the drivers'
// BillingDescriber capability.
type CatalogRules struct {
	Catalog  *catalog.Catalog
	Registry *vendors.Registry
}

// Rule implements Rules.
func (r CatalogRules) Rule(vendor, region, family string) (billing.Rule, bool) {
	facts, ok := r.Catalog.BillingFacts(vendor)
	var override *vendors.BillingRule
	if drv, has := r.Registry.Driver(vendor); has {
		if d, is := drv.(vendors.BillingDescriber); is {
			if rule, given := d.BillingRule(region, family); given {
				override = &rule
			}
		}
	}
	return billing.Resolve(facts, override), ok
}

// StartSource gives the billing start of an instance the NodeClaim has no
// annotation for (a node launched before the annotation existed): it returns
// the annotation value of the NodeClaim the cloud provider describes, empty
// when the driver has no stamp.
type StartSource interface {
	BillingStart(ctx context.Context, providerID string) (string, error)
}

// QuotaScope is a vendor, region and capacity type where launches are refused
// for quota.
type QuotaScope struct {
	Vendor, Region, CapacityType string
}

// QuotaSource reports the scopes where every offering of a NodeClass is
// blocked by the quota tracker.
type QuotaSource interface {
	BlockedScopes(ctx context.Context) ([]QuotaScope, error)
}
