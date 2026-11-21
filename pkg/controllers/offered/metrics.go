/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"maps"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

var (
	byStateDesc = prometheus.NewDesc("scarpenter_catalog_offerings_by_state",
		"Offerings of the offered types of a NodeClass and vendor, by state: available, or the reason code that "+
			"removed the offering or made it unavailable (at most nine series per NodeClass and vendor).",
		[]string{"nodeclass", "vendor", "state"}, nil)
	offeringDesc = prometheus.NewDesc("scarpenter_instance_type_offering_available",
		"1 when the offering (instance type, zone, capacity type) is available to Karpenter, 0 when listed unavailable. "+
			"Only with --metrics-offering-detail; one series per offering.",
		[]string{"nodeclass", "instance_type", "zone", "capacity_type"}, nil)
)

// stateAvailable is the state label of an available offering.
const stateAvailable = "available"

// metrics holds what the last evaluation of each class found, for the scrape.
type metrics struct {
	detail bool

	mu sync.Mutex
	// summaries are per class and vendor; offerings per class, only with detail.
	summaries map[string]map[string]catalog.Summary
	offerings map[string]map[offeringKey]bool
}

type offeringKey struct{ instanceType, zone, capacityType string }

func newMetrics(detail bool) *metrics {
	return &metrics{
		detail: detail, summaries: map[string]map[string]catalog.Summary{}, offerings: map[string]map[offeringKey]bool{},
	}
}

// record replaces what is known of a class.
func (m *metrics) record(class string, report catalog.Report) {
	var offerings map[offeringKey]bool
	if m.detail {
		offerings = map[offeringKey]bool{}
		for _, t := range report.Types {
			for _, o := range t.Offerings {
				if !o.Removed {
					offerings[offeringKey{t.InstanceType, o.Zone, o.CapacityType}] = o.Available
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaries[class] = report.Summaries()
	if m.detail {
		m.offerings[class] = offerings
	}
}

func (m *metrics) forget(class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.summaries, class)
	delete(m.offerings, class)
}

// Describe implements prometheus.Collector.
func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- byStateDesc
	if m.detail {
		ch <- offeringDesc
	}
}

// Collect implements prometheus.Collector.
func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, class := range slices.Sorted(maps.Keys(m.summaries)) {
		for vendor, s := range m.summaries[class] {
			ch <- prometheus.MustNewConstMetric(byStateDesc, prometheus.GaugeValue, float64(s.AvailableOfferings),
				class, vendor, stateAvailable)
			for _, rc := range s.Unavailable {
				ch <- prometheus.MustNewConstMetric(byStateDesc, prometheus.GaugeValue, float64(rc.Count),
					class, vendor, string(rc.Code))
			}
		}
		for k, available := range m.offerings[class] {
			v := 0.0
			if available {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(offeringDesc, prometheus.GaugeValue, v,
				class, k.instanceType, k.zone, k.capacityType)
		}
	}
}
