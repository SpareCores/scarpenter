/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	retainedNodes   *prometheus.GaugeVec
	retainedSeconds *prometheus.CounterVec
	reuse           *prometheus.CounterVec
	periodsSaved    *prometheus.CounterVec
	deletesDeadline *prometheus.CounterVec
	blockedBudget   *prometheus.CounterVec
	releasedPress   *prometheus.CounterVec
	skippedNodes    *prometheus.GaugeVec
	boundaryMissed  *prometheus.CounterVec
	overshoot       *prometheus.CounterVec
	bootSeconds     *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	vendor := []string{"vendor"}
	m := &metrics{
		retainedNodes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "scarpenter_billing_retained_nodes",
			Help: "Nodes held until the end of their billing period.",
		}, []string{"vendor", "nodeclass"}),
		retainedSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_retained_seconds_total",
			Help: "Node seconds spent held until the end of the billing period.",
		}, vendor),
		reuse: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_reuse_total",
			Help: "Nodes that were empty under the hold and received a pod.",
		}, vendor),
		periodsSaved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_periods_saved_estimate_total",
			Help: "Estimated billing periods saved: one per reuse of a held empty node (an estimate, not an invoice).",
		}, vendor),
		deletesDeadline: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_deletes_at_deadline_total",
			Help: "Empty nodes deleted before the end of their billing period.",
		}, vendor),
		blockedBudget: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_blocked_by_budget_total",
			Help: "Releases that a NodePool disruption budget blocked.",
		}, vendor),
		releasedPress: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_released_under_pressure_total",
			Help: "Held empty nodes released early for a launch blocked by a NodePool limit or a vendor quota, or by maxRetainedNodes.",
		}, []string{"vendor", "scope"}),
		skippedNodes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "scarpenter_billing_skipped_nodes",
			Help: "Nodes billing retention leaves alone, by reason.",
		}, []string{"vendor", "nodeclass", "reason"}),
		boundaryMissed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_boundary_missed_total",
			Help: "Empty nodes still alive 30 s past their period boundary: the margin is too small.",
		}, vendor),
		overshoot: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_billing_budget_overshoot_total",
			Help: "Releases that left a NodePool budget exceeded by one node (the race with Karpenter's own disruption).",
		}, vendor),
		bootSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scarpenter_billing_boot_seconds",
			Help:    "Seconds from the billing start of a node to its Initialized condition.",
			Buckets: []float64{15, 30, 60, 90, 120, 180, 240, 300, 420, 600},
		}, vendor),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{
			m.retainedNodes, m.retainedSeconds, m.reuse, m.periodsSaved, m.deletesDeadline, m.blockedBudget,
			m.releasedPress, m.skippedNodes, m.boundaryMissed, m.overshoot, m.bootSeconds,
		} {
			register(reg, c)
		}
	}
	return m
}

// register adds the collector; one of an earlier instance (a test that builds
// several) is replaced.
func register(reg prometheus.Registerer, col prometheus.Collector) {
	err := reg.Register(col)
	if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
		reg.Unregister(already.ExistingCollector)
		err = reg.Register(col)
	}
	if err != nil {
		panic(fmt.Sprintf("registering the billing retention metrics: %v", err))
	}
}
