/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Operations and results of the operations counter.
const (
	opCreate = "create"
	opAdopt  = "adopt"
	opDelete = "delete"

	resultOK     = "ok"
	resultError  = "error"
	resultDryRun = "dry-run"
)

type metrics struct {
	operations *prometheus.CounterVec
	managed    *prometheus.GaugeVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_resource_operations_total",
			Help: "Creations, adoptions and deletions of managed vendor resources by vendor, kind, region, operation, result.",
		}, []string{"vendor", "kind", "region", "operation", "result"}),
		managed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "scarpenter_resources_managed",
			Help: "Managed vendor resources found by the last garbage collection sweep, by vendor, kind and region.",
		}, []string{"vendor", "kind", "region"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.operations, m.managed} {
			err := reg.Register(c)
			if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
				reg.Unregister(already.ExistingCollector)
				err = reg.Register(c)
			}
			if err != nil {
				panic(fmt.Sprintf("registering the resource metrics: %v", err))
			}
		}
	}
	return m
}

func (m *metrics) operation(vendor string, kind vendors.ResourceKind, region, op, result string) {
	m.operations.WithLabelValues(vendor, string(kind), region, op, result).Inc()
}
