/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"slices"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Quota metric names the driver acts on (regions.get and projects.get).
const (
	MetricCPUs           = "CPUS"
	MetricPreemptible    = "PREEMPTIBLE_CPUS"
	MetricCPUsAllRegions = "CPUS_ALL_REGIONS"
	MetricInstances      = "INSTANCES"
)

// QuotaReader reads the quota buckets of a project. GCP returns the usage with
// the limit (regions.get: quotas[] of metric, limit, usage), unlike AWS, so no
// usage is computed from instances.
//
// The Driver embeds it to implement vendors.QuotaReporter.
type QuotaReader struct {
	API     ComputeAPI
	Project string
	// Confirmed are the machine series whose quota consumption the spike or the
	// lane's quota scenario confirmed, with the metrics one vCPU of the series
	// consumes. A series not in it is unknown and takes the reactive path
	// (QuotaExceeded names the metric). Plan 16, section 5: which metrics one
	// machine type consumes is UNVERIFIED, so the zero value confirms nothing.
	Confirmed map[string][]string
}

// IsCPUMetric reports whether the metric is a vCPU bucket: CPUS, the series
// metrics (N2_CPUS), PREEMPTIBLE_CPUS and CPUS_ALL_REGIONS.
func IsCPUMetric(metric string) bool {
	return strings.HasSuffix(metric, "_CPUS") || metric == MetricCPUs || metric == MetricCPUsAllRegions ||
		strings.HasPrefix(metric, "CPUS_")
}

// unitOf is the unit of a bucket: vCPU for the CPU metrics, instances for the
// instance count, count for the rest.
func unitOf(metric string) string {
	switch {
	case IsCPUMetric(metric):
		return "vCPU"
	case metric == MetricInstances:
		return "instances"
	}
	return "count"
}

// buckets converts the quotas of a region or a project to buckets: the CPU
// buckets and the instance count; the other metrics (disks, addresses,
// firewalls) are not launch limits the tracker needs. Sorted by ID.
func buckets(qs []*computepb.Quota, scope vendors.QuotaScope) []vendors.Bucket {
	var out []vendors.Bucket
	for _, q := range qs {
		m := q.GetMetric()
		if !IsCPUMetric(m) && m != MetricInstances {
			continue
		}
		// CPUS_ALL_REGIONS is account-wide wherever it is reported.
		sc := scope
		if accountWideMetric(m) {
			sc = vendors.QuotaScopeGlobal
		}
		out = append(out, vendors.Bucket{
			ID: m, Scope: sc, Unit: unitOf(m), Limit: int64(q.GetLimit()), Used: int64(q.GetUsage()),
		})
	}
	slices.SortFunc(out, func(a, b vendors.Bucket) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Quotas returns the region's buckets from regions.get. A region the
// credentials cannot read is RegionUnavailable.
func (r *QuotaReader) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	reg, err := r.API.GetRegion(ctx, &computepb.GetRegionRequest{Project: r.Project, Region: region})
	if err != nil {
		return nil, classifyRegionRead(err)
	}
	var qs []*computepb.Quota
	for _, q := range reg.GetQuotas() {
		if !accountWideMetric(q.GetMetric()) {
			qs = append(qs, q)
		}
	}
	return &vendors.QuotaSnapshot{Buckets: buckets(qs, vendors.QuotaScopeRegion)}, nil
}

// GlobalQuotas returns the project-wide buckets from projects.get (CPUS_ALL_REGIONS
// when the project has it: only some do).
func (r *QuotaReader) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	p, err := r.API.GetProject(ctx, &computepb.GetProjectRequest{Project: r.Project})
	if err != nil {
		return nil, classify(err, errContext{})
	}
	var qs []*computepb.Quota
	for _, q := range p.GetQuotas() {
		if accountWideMetric(q.GetMetric()) {
			qs = append(qs, q)
		}
	}
	return &vendors.QuotaSnapshot{Buckets: buckets(qs, vendors.QuotaScopeGlobal)}, nil
}

// SeriesMetrics lists the metrics a series is documented to consume, as
// candidates for the spike to confirm: E2 and N1 share the CPUS pool, every
// other series has its own <SERIES>_CPUS. Newer series (c4, n4, ...) account
// per family through CPUS_PER_VM_FAMILY, whose dimension may exist only in the
// Cloud Quotas API: nil, unknown.
func SeriesMetrics(series string) []string {
	switch series {
	case "e2", "n1":
		return []string{MetricCPUs}
	case "c4", "c4a", "c4d", "n4", "n4a", "n4d", "a3", "a4", seriesA4X, "x4", seriesH4D, "z3", "m4", "g4":
		return nil
	case "":
		return nil
	}
	return []string{MetricCPUs, strings.ToUpper(series) + "_CPUS"}
}

// Consumption returns the buckets one instance of the machine type consumes:
// the vCPUs against each metric of the series, only for a series the reader's
// Confirmed table lists. Spot is unknown: Spot VMs consume standard quota until
// the project has preemptible quota in the region and only preemptible quota
// after, which depends on the region's PREEMPTIBLE_CPUS limit that the
// vendors.QuotaReporter signature (no region) cannot see.
func (r *QuotaReader) Consumption(machineType string, vcpus int64, capacityType string) ([]vendors.Consumption, bool) {
	if capacityType == vendors.CapacitySpot || vcpus <= 0 {
		return nil, false
	}
	metrics, ok := r.Confirmed[Family(machineType)]
	if !ok || len(metrics) == 0 {
		return nil, false
	}
	out := make([]vendors.Consumption, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, vendors.Consumption{Bucket: m, Amount: vcpus})
	}
	return out, true
}
