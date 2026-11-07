/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Quota classes (overview section 11.8): EC2's vCPU quotas are per class, per
// region, separately for On-Demand and Spot. HPC and High Memory have no Spot
// quota: their types are On-Demand only.
const (
	ClassStandard = "Standard"
	ClassF        = "F"
	ClassG        = "G and VT"
	ClassInf      = "Inf"
	ClassP        = "P"
	ClassX        = "X"
	ClassTrn      = "Trn"
	ClassDL       = "DL"
	ClassHPC      = "HPC"
	ClassHighMem  = "High Memory"
)

// quotaCodes lists the 18 quota codes: class, On-Demand code, Spot code ("" for
// the classes without Spot).
var quotaCodes = []struct{ Class, OnDemand, Spot string }{
	{ClassStandard, "L-1216C47A", "L-34B43A08"},
	{ClassF, "L-74FC7D96", "L-88CF9481"},
	{ClassG, "L-DB2E81BA", "L-3819A6DF"},
	{ClassInf, "L-1945791B", "L-B5D1601B"},
	{ClassP, "L-417A185B", "L-7212CCBC"},
	{ClassX, "L-7295265B", "L-E3A00192"},
	{ClassTrn, "L-2C3B7624", "L-6B0D517C"},
	{ClassDL, "L-6E869C2A", "L-85EED4F7"},
	{ClassHPC, "L-F7808C92", ""},
	{ClassHighMem, "L-43DA4232", ""},
}

// ec2ServiceCode is the Service Quotas service code of EC2.
const ec2ServiceCode = "ec2"

// ClassOf maps an instance type to its quota class by the family rule of the
// overview (family = the name before the first dot): the prefixes mac, hpc, dl,
// trn, inf and vt first, in this order, then the first letter. ok is false for
// a family the rule does not know, which a test makes fail until it is
// classified, and for mac, which has no vCPU quota (Dedicated Hosts) and is not
// offered.
func ClassOf(instanceType string) (class string, ok bool) {
	family, _, _ := strings.Cut(instanceType, ".")
	switch {
	case family == "":
		return "", false
	case strings.HasPrefix(family, "mac"):
		return "", false
	case strings.HasPrefix(family, "hpc"):
		return ClassHPC, true
	case strings.HasPrefix(family, "dl"):
		return ClassDL, true
	case strings.HasPrefix(family, "trn"):
		return ClassTrn, true
	case strings.HasPrefix(family, "inf"):
		return ClassInf, true
	case strings.HasPrefix(family, "vt"):
		return ClassG, true
	}
	switch family[0] {
	case 'g':
		return ClassG, true
	case 'f':
		return ClassF, true
	case 'p':
		return ClassP, true
	case 'x':
		return ClassX, true
	case 'u':
		return ClassHighMem, true
	case 'a', 'c', 'd', 'h', 'i', 'm', 'r', 't', 'z':
		return ClassStandard, true
	}
	return "", false
}

// quotaCode returns the quota code of the class and capacity type; "" when the
// class has none (Spot of HPC and High Memory).
func quotaCode(class, capacityType string) string {
	for _, q := range quotaCodes {
		if q.Class == class {
			if capacityType == vendors.CapacitySpot {
				return q.Spot
			}
			return q.OnDemand
		}
	}
	return ""
}

// Consumption implements vendors.QuotaReporter: an instance consumes its
// DefaultVCpus of the bucket of its class and capacity type. A type the driver
// cannot classify or whose vCPUs it has not read yet is unknown (the reactive
// path of overview section 5.6 applies).
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	class, ok := ClassOf(instanceType)
	if !ok {
		return nil, false
	}
	code := quotaCode(class, capacityType)
	if code == "" {
		return nil, false
	}
	ti, ok := d.types.get(instanceType)
	if !ok || ti.VCPUs <= 0 {
		return nil, false
	}
	return []vendors.Consumption{{Bucket: code, Amount: ti.VCPUs}}, true
}

// limitsTTL is how long the driver reuses limits when the caller does not ask
// for fresh ones.
const limitsTTL = 15 * time.Minute

type regionQuota struct {
	limits   map[string]int64
	limitsAt time.Time
}

// GlobalQuotas implements vendors.QuotaReporter: EC2's vCPU quotas are all
// regional, so there are no account-wide buckets.
func (d *Driver) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// Quotas implements vendors.QuotaReporter: the limits of the 18 quota codes
// (GetServiceQuota, effective values) and the usage computed over every
// instance of the region (overview section 11.8).
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	limits, err := d.limits(ctx, cl, region)
	if err != nil {
		return nil, err
	}
	used, counted, err := d.usage(ctx, cl, region)
	if err != nil {
		return nil, err
	}
	snap := &vendors.QuotaSnapshot{Counted: counted}
	for _, q := range quotaCodes {
		for _, code := range []string{q.OnDemand, q.Spot} {
			if code == "" {
				continue
			}
			if limit, ok := limits[code]; ok {
				snap.Buckets = append(snap.Buckets, vendors.Bucket{
					ID: code, Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: limit, Used: used[code],
				})
			}
		}
	}
	return snap, nil
}

// limits returns the effective limits per quota code, reading them again when
// the caller asks for fresh ones or the cache is old. A code Service Quotas does
// not know in the region has no bucket (its types are unknown quota).
func (d *Driver) limits(ctx context.Context, cl Clients, region string) (map[string]int64, error) {
	d.mu.Lock()
	rq := d.quotas[region]
	if rq != nil && !vendors.QuotaRefreshFrom(ctx).Limits && d.clock.Now().Sub(rq.limitsAt) < limitsTTL {
		out := cloneLimits(rq.limits)
		d.mu.Unlock()
		return out, nil
	}
	d.mu.Unlock()
	var codes []string
	for _, q := range quotaCodes {
		codes = append(codes, q.OnDemand)
		if q.Spot != "" {
			codes = append(codes, q.Spot)
		}
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		limits  = map[string]int64{}
		missing []string
		first   error
		sem     = make(chan struct{}, 6)
	)
	for _, code := range codes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out, err := cl.Quotas.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
				ServiceCode: aws.String(ec2ServiceCode), QuotaCode: aws.String(code),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && out.Quota != nil && out.Quota.Value != nil:
				limits[code] = int64(*out.Quota.Value)
			case err == nil || isCode(err, "NoSuchResourceException"):
				missing = append(missing, code)
			case first == nil:
				first = classify(err, errContext{})
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if len(missing) > 0 {
		// A quota that was never changed may only show in the listing.
		if err := d.listLimits(ctx, cl, missing, limits); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	d.quotas[region] = &regionQuota{limits: cloneLimits(limits), limitsAt: d.clock.Now()}
	d.mu.Unlock()
	return limits, nil
}

func cloneLimits(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	maps.Copy(out, m)
	return out
}

// listLimits fills the limits of the codes GetServiceQuota did not know from
// the (large) listing.
func (d *Driver) listLimits(ctx context.Context, cl Clients, codes []string, limits map[string]int64) error {
	in := &servicequotas.ListServiceQuotasInput{ServiceCode: aws.String(ec2ServiceCode), MaxResults: aws.Int32(100)}
	for {
		out, err := cl.Quotas.ListServiceQuotas(ctx, in)
		if err != nil {
			return classify(err, errContext{})
		}
		for _, q := range out.Quotas {
			if q.QuotaCode != nil && q.Value != nil && slices.Contains(codes, *q.QuotaCode) {
				limits[*q.QuotaCode] = int64(*q.Value)
			}
		}
		if out.NextToken == nil {
			return nil
		}
		in.NextToken = out.NextToken
	}
}

// countedStates are the instance states that count against the vCPU quota:
// pending and running (verified), plus stopping and shutting-down, which are too
// short to observe and count conservatively until they leave. stopped does not
// count (verified).
var countedStates = []string{
	string(ec2types.InstanceStateNamePending), string(ec2types.InstanceStateNameRunning),
	string(ec2types.InstanceStateNameStopping), string(ec2types.InstanceStateNameShuttingDown),
}

// usage computes the vCPUs in use per quota code over the whole account of the
// region, and the references of the instances counted:
//   - instances in a counted state: On-Demand when InstanceLifecycle is absent,
//     Spot when it is spot (scheduled and capacity-block instances are not
//     counted);
//   - spot requests in state open that have no instance yet: a fulfilled
//     request stays active for minutes after its instance ended, so requests
//     next to instances would count twice;
//   - the unused capacity of active On-Demand Capacity Reservations;
//   - vCPUs are the type's DefaultVCpus, never CpuOptions.
func (d *Driver) usage(
	ctx context.Context, cl Clients, region string) (map[string]int64, []vendors.InstanceRef, error,
) {
	insts, err := d.describeInstances(ctx, region, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("instance-state-name"), Values: countedStates},
	}})
	if err != nil {
		return nil, nil, err
	}
	spot, err := d.openSpotRequests(ctx, cl)
	if err != nil {
		return nil, nil, err
	}
	reservations, err := d.unusedReservations(ctx, cl)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, i := range insts {
		names = append(names, string(i.InstanceType))
	}
	for _, s := range spot {
		names = append(names, s.typ)
	}
	for _, r := range reservations {
		names = append(names, r.typ)
	}
	if err := d.ensureTypes(ctx, region, names); err != nil {
		return nil, nil, err
	}
	used := map[string]int64{}
	add := func(typ, capacityType string, n int64) {
		class, ok := ClassOf(typ)
		if !ok {
			return
		}
		ti, known := d.types.get(typ)
		if !known {
			d.warnOnce("unknown-vcpus/"+typ, "the vCPUs of an instance type are unknown; its usage is not counted", "type",
				typ)
			return
		}
		if code := quotaCode(class, capacityType); code != "" {
			used[code] += ti.VCPUs * n
		}
	}
	var counted []vendors.InstanceRef
	for _, i := range insts {
		capacityType := vendors.CapacityOnDemand
		switch i.InstanceLifecycle {
		case "":
		case ec2types.InstanceLifecycleTypeSpot:
			capacityType = vendors.CapacitySpot
		default:
			continue
		}
		add(string(i.InstanceType), capacityType, 1)
		counted = append(counted, vendors.NewInstanceRef(region, deref(i.InstanceId)))
	}
	for _, s := range spot {
		add(s.typ, vendors.CapacitySpot, 1)
	}
	for _, r := range reservations {
		add(r.typ, vendors.CapacityOnDemand, r.free)
	}
	return used, counted, nil
}

type openRequest struct{ typ string }

// openSpotRequests returns the spot requests still waiting for capacity.
func (d *Driver) openSpotRequests(ctx context.Context, cl Clients) ([]openRequest, error) {
	var out []openRequest
	in := &ec2.DescribeSpotInstanceRequestsInput{Filters: []ec2types.Filter{{Name: aws.String("state"),
		Values: []string{"open"}}}}
	for {
		res, err := cl.EC2.DescribeSpotInstanceRequests(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, r := range res.SpotInstanceRequests {
			if r.InstanceId != nil && *r.InstanceId != "" {
				continue
			}
			if r.LaunchSpecification != nil && r.LaunchSpecification.InstanceType != "" {
				out = append(out, openRequest{typ: string(r.LaunchSpecification.InstanceType)})
			}
		}
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

type reservation struct {
	typ  string
	free int64
}

// unusedReservations returns the unused capacity of the active On-Demand
// Capacity Reservations (from the documentation, medium-high confidence: it
// counts against the quota like a running instance).
func (d *Driver) unusedReservations(ctx context.Context, cl Clients) ([]reservation, error) {
	var out []reservation
	in := &ec2.DescribeCapacityReservationsInput{Filters: []ec2types.Filter{{Name: aws.String("state"),
		Values: []string{"active"}}}}
	for {
		res, err := cl.EC2.DescribeCapacityReservations(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, r := range res.CapacityReservations {
			if r.ReservationType == ec2types.CapacityReservationTypeCapacityBlock || r.AvailableInstanceCount == nil ||
				*r.AvailableInstanceCount <= 0 || r.InstanceType == nil {
				continue
			}
			out = append(out, reservation{typ: *r.InstanceType, free: int64(*r.AvailableInstanceCount)})
		}
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// warnOnce logs a message once per key.
func (d *Driver) warnOnce(key, msg string, kv ...any) {
	d.mu.Lock()
	seen := d.userSeen[key]
	d.userSeen[key] = true
	d.mu.Unlock()
	if !seen {
		d.log.Info(msg, kv...)
	}
}

// LaunchableTypes implements vendors.OfferingLister: DescribeInstanceTypeOfferings
// located by zone ID, which matches Keeper's zone IDs directly. mac types are
// left out: they need Dedicated Hosts and run macOS, not Flatcar, so the
// catalog drops them where the lister is known. Reading the offerings also reads
// the vCPUs of the offered types, which the quota consumption needs.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	in := &ec2.DescribeInstanceTypeOfferingsInput{LocationType: ec2types.LocationTypeAvailabilityZoneId,
		MaxResults: aws.Int32(1000)}
	for {
		res, err := cl.EC2.DescribeInstanceTypeOfferings(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, o := range res.InstanceTypeOfferings {
			typ := string(o.InstanceType)
			if o.Location == nil || strings.HasPrefix(typ, "mac") {
				continue
			}
			if !slices.Contains(out[typ], *o.Location) {
				out[typ] = append(out[typ], *o.Location)
			}
		}
		if res.NextToken == nil {
			break
		}
		in.NextToken = res.NextToken
	}
	names := make([]string, 0, len(out))
	for typ, zones := range out {
		slices.Sort(zones)
		names = append(names, typ)
	}
	if err := d.ensureTypes(ctx, region, names); err != nil {
		return nil, fmt.Errorf("reading the instance types of %s: %w", region, err)
	}
	return out, nil
}
