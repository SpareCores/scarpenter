/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Results of a launch attempt and of a Create, as metric label values.
const labelResult = "result"

const (
	resultSuccess = "success"
	resultFailed  = "failed"
)

type metrics struct {
	launches *prometheus.CounterVec
	skipped  *prometheus.CounterVec
	attempts *prometheus.HistogramVec
	duration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		launches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_launches_total",
			Help: "Vendor launch calls by result: success or the driver error class.",
		}, []string{"vendor", "region", "capacity_type", "result"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_launch_skipped_total",
			Help: "Candidate offerings skipped without a vendor call, by reason (no quota headroom, unavailable, no image).",
		}, []string{"vendor", "region", "capacity_type", "reason"}),
		attempts: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scarpenter_launch_attempts",
			Help:    "Vendor calls one NodeClaim creation made.",
			Buckets: []float64{1, 2, 3, 4, 5, 8},
		}, []string{labelResult}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scarpenter_create_duration_seconds",
			Help:    "Duration of the CloudProvider Create call, by result.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2.5, 12),
		}, []string{labelResult}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.launches, m.skipped, m.attempts, m.duration} {
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
		panic(fmt.Sprintf("registering the cloudprovider metrics: %v", err))
	}
}
