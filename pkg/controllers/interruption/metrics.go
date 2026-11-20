/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const labelVendor = "vendor"

type metrics struct {
	interruptions *prometheus.CounterVec
	lead          *prometheus.HistogramVec
	reaction      prometheus.Histogram
	pollErrors    *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		interruptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_interruptions_total",
			Help: "Interrupted spot and preemptible nodes, by instance type and zone. " +
				"The durable count: Events expire after an hour.",
		}, []string{labelVendor, "instance_type", "zone", "reason", "source"}),
		lead: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "scarpenter_interruption_lead_seconds",
			Help: "Seconds between the moment the controller saw an interruption notice and its deadline " +
				"(clamped to the vendor's notice window).",
			Buckets: []float64{0, 5, 10, 20, 30, 60, 90, 120, 180, 300, 600},
		}, []string{labelVendor, "source"}),
		reaction: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "scarpenter_interruption_reaction_seconds",
			Help:    "Seconds from the controller seeing an interruption notice to the NodeClaim delete.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		pollErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_interruption_poll_errors_total",
			Help: "Failed polls of a vendor's API for interruption notices, by vendor and region.",
		}, []string{labelVendor, "region"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.interruptions, m.lead, m.reaction, m.pollErrors} {
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
		panic(fmt.Sprintf("registering the interruption metrics: %v", err))
	}
}
