/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package imagecopy

import (
	"errors"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the counters, the histogram and the gauge of the image copies,
// shared by the managers of all vendors (the vendor is a label).
type Metrics struct {
	started   *prometheus.CounterVec
	completed *prometheus.CounterVec
	failed    *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	deleted   *prometheus.CounterVec
	copies    *prometheus.GaugeVec
}

const labelVendor = "vendor"

var regionLabels = []string{labelVendor, "region"}

// The states of the scarpenter_image_copies gauge.
var copyStates = []string{"queued", "pending", "available", "failed"}

// NewMetrics creates the metrics and registers them with reg (nil registers
// none). A collector of an earlier instance (tests, restarts) is replaced.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		started: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_image_copies_started_total",
			Help: "Image copies started, by vendor and destination region.",
		}, regionLabels),
		completed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_image_copies_completed_total",
			Help: "Image copies that became available, by vendor and destination region.",
		}, regionLabels),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_image_copies_failed_total",
			Help: "Image copies that failed (refused, failed or timed out), by vendor, destination region and error code.",
		}, append(slices.Clone(regionLabels), "code")),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "scarpenter_image_copy_duration_seconds",
			Help:    "Time from the start of an image copy to its availability, by vendor.",
			Buckets: prometheus.ExponentialBuckets(30, 2, 9),
		}, []string{labelVendor}),
		deleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_image_copies_deleted_total",
			Help: "Image copies deleted, by vendor and reason (Idle, PrimaryAvailable, Unpinned, Duplicate, Failed).",
		}, []string{labelVendor, "reason"}),
		copies: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "scarpenter_image_copies",
			Help: "Image copies the manager knows, by vendor and state (queued, pending, available, failed).",
		}, []string{labelVendor, "state"}),
	}
	if reg == nil {
		return m
	}
	for _, c := range []prometheus.Collector{m.started, m.completed, m.failed, m.duration, m.deleted, m.copies} {
		err := reg.Register(c)
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			reg.Unregister(already.ExistingCollector)
			err = reg.Register(c)
		}
		if err != nil {
			panic(fmt.Sprintf("registering the image copy metrics: %v", err))
		}
	}
	return m
}
