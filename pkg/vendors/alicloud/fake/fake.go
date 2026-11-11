/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory Alibaba Cloud ECS and VPC (plan 21 section 7):
// regions and zones, instance types and their per-zone stock, images of several
// owners, instances that go from Pending to Running and vanish when released,
// ClientToken semantics (including IdempotentParameterMismatch), tags with AND
// filters, paging, VPCs (default or none), vSwitches with free-address counts,
// security groups, deployment sets, account attributes, and error injection per
// call. It implements the methods of ECSAPI and VPCAPI of the parent package
// (the parent's tests check that at compile time; this package does not import
// it).
//
// Faithful where the plan relies on it: the codes it returns are the codes of
// plan 21 section 6; behaviours the plan marks unverified are options or
// comments here, so a spike result changes the fake in one place.
package fake

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibabacloud-go/tea/dara"
	"k8s.io/utils/clock"
)

// Stock states of DescribeAvailableResource.
const (
	StatusAvailable = "Available"
	StatusSoldOut   = "SoldOut"
)

// statusAvailable is the Status of an image, a VPC and a vSwitch that can be
// used (not the stock status, which happens to read the same).
const statusAvailable = "Available"

// Resource types of TagResources, UntagResources and ListTagResources.
const (
	resInstance      = "instance"
	resSecurityGroup = "securitygroup"
	resImage         = "image"
)

// Capacity types of the offerings.
const (
	OnDemand = "on-demand"
	Spot     = "spot"
)

// Default page sizes of the list calls (the APIs' defaults, which small test
// fleets exceed on purpose so that paging is exercised).
const (
	DefaultPageSize = 10
	MaxPageSize     = 100
)

// TypeSpec is an instance type.
type TypeSpec struct {
	Name      string
	VCPUs     int32
	MemoryGiB float32
	Arch      string // amd64 (default) or arm64
	NVMe      bool
	Disks     int32 // DiskQuantity
	LocalDisk int32
}

type offerKey struct{ zone, typ, capacity string }

type token struct {
	params string
	ids    []string
}

type injection struct {
	err   error
	times int // 0 = forever
}

// Cloud is the fake account: every region of it.
type Cloud struct {
	mu    sync.Mutex
	clock clock.Clock
	seq   int

	// PendingFor is how long an instance is Pending before it is Running.
	PendingFor time.Duration
	// ConsumeTokenOnFailure makes a RunInstances call that failed consume its
	// ClientToken (whether ECS does is unverified, plan 21 step 0); the default
	// is a failed call leaves the token unused.
	ConsumeTokenOnFailure bool
	// NetworkPendingFor is how long a new VPC or vSwitch is Pending before it is
	// Available (VPC resources are created asynchronously).
	NetworkPendingFor time.Duration
	// DescribeCalls counts the calls per method, for tests that bound them.
	DescribeCalls map[string]int

	regions map[string]*regionState
	types   map[string]TypeSpec
	inject  map[string][]*injection
}

type regionState struct {
	name        string
	status      string
	denied      bool
	noSpot      bool
	zones       []string
	offers      map[offerKey]string
	postpaid    int64
	spot        int64
	vanishAfter map[offerKey]time.Duration

	instances map[string]*instance
	images    map[string]*image
	vpcs      map[string]*vpcRec
	vswitches map[string]*vswitchRec
	groups    map[string]*groupRec
	sets      map[string]*setRec
	tokens    map[string]*token
}

// New returns an empty account. A nil clock is the real one; tests pass a fake
// clock to move instances from Pending to Running and to age resources.
func New(c clock.Clock) *Cloud {
	if c == nil {
		c = clock.RealClock{}
	}
	return &Cloud{
		clock: c, regions: map[string]*regionState{}, types: map[string]TypeSpec{},
		inject: map[string][]*injection{}, DescribeCalls: map[string]int{},
	}
}

// AddRegion adds a region with its zones (zone IDs are the zone names).
func (f *Cloud) AddRegion(name string, zones ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regions[name] = &regionState{
		name: name, status: "available", zones: zones, offers: map[offerKey]string{},
		postpaid: 10000, spot: 10000, vanishAfter: map[offerKey]time.Duration{},
		instances: map[string]*instance{}, images: map[string]*image{}, vpcs: map[string]*vpcRec{},
		vswitches: map[string]*vswitchRec{}, groups: map[string]*groupRec{}, sets: map[string]*setRec{},
		tokens: map[string]*token{},
	}
}

// AddType adds an instance type to the account's catalog.
func (f *Cloud) AddType(t TypeSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Arch == "" {
		t.Arch = "amd64"
	}
	f.types[t.Name] = t
}

// Deny makes every call of the region fail with RegionUnauthorized (an account
// that cannot use the region).
func (f *Cloud) Deny(regionName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).denied = true
}

// DisableSpot makes spot launches in the region fail with
// InvalidAccount.NotSupportSpot.
func (f *Cloud) DisableSpot(regionName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).noSpot = true
}

// SetQuota sets the region's vCPU limits for pay-as-you-go and spot instances.
func (f *Cloud) SetQuota(regionName string, postpaidVCPUs, spotVCPUs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	r.postpaid, r.spot = postpaidVCPUs, spotVCPUs
}

// Offer sets the stock status of types in a zone for a capacity type; a type
// that is not offered in a zone cannot be launched there
// (InvalidInstanceType.ZoneNotSupported).
func (f *Cloud) Offer(regionName, zone, capacity, status string, types ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	for _, t := range types {
		r.offers[offerKey{zone, t, capacity}] = status
	}
}

// OfferEverywhere offers every type of the catalog in every zone of the
// region, for both capacity types, Available.
func (f *Cloud) OfferEverywhere(regionName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	for _, z := range r.zones {
		for t := range f.types {
			r.offers[offerKey{z, t, OnDemand}] = StatusAvailable
			r.offers[offerKey{z, t, Spot}] = StatusAvailable
		}
	}
}

// VanishAfter makes instances launched for the type in the zone and capacity
// type disappear d after their creation: ECS accepted the call, then released
// the instance for lack of stock (SystemFailure.Delete).
func (f *Cloud) VanishAfter(regionName, zone, capacity, typ string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mustRegion(regionName).vanishAfter[offerKey{zone, typ, capacity}] = d
}

// Inject makes the next times calls of the method fail with err; times 0 is for
// every call until Clear. The method is the API name (RunInstances).
func (f *Cloud) Inject(method string, err error, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inject[method] = append(f.inject[method], &injection{err: err, times: times})
}

// Clear removes the injected errors of the method; "" removes all.
func (f *Cloud) Clear(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "" {
		f.inject = map[string][]*injection{}
		return
	}
	delete(f.inject, method)
}

// APIError is an error as the SDK returns it: *dara.SDKError with the ECS code
// and the HTTP status.
func APIError(code string, status int, msg string) error {
	return &dara.SDKError{Code: new(code), StatusCode: new(status), Message: new(msg)}
}

func badRequest(code, format string, args ...any) error {
	return APIError(code, 400, fmt.Sprintf(format, args...))
}

func (f *Cloud) mustRegion(name string) *regionState {
	r, ok := f.regions[name]
	if !ok {
		panic("fake: unknown region " + name)
	}
	return r
}

// enter is the start of every API call: it counts the call, applies the
// injected errors and the denial of the region, and returns the region.
func (f *Cloud) enter(ctx context.Context, method, regionName string) (*regionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.DescribeCalls[method]++
	if q := f.inject[method]; len(q) > 0 {
		in := q[0]
		if in.times > 0 {
			in.times--
			if in.times == 0 {
				f.inject[method] = q[1:]
			}
		}
		return nil, in.err
	}
	r, ok := f.regions[regionName]
	if !ok {
		return nil, badRequest("InvalidRegionId.NotFound", "the region %q does not exist", regionName)
	}
	if r.denied {
		return nil, APIError("RegionUnauthorized", 403, "the account cannot use region "+regionName)
	}
	f.reap(r)
	return r, nil
}

// Calls returns how many times the method was called.
func (f *Cloud) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.DescribeCalls[method]
}

func (f *Cloud) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-fake%06d", prefix, f.seq)
}

// ---- helpers shared by the services -----------------------------------------

// tagSet is a resource's tags.
type tagSet map[string]string

func (t tagSet) matches(filter map[string]string) bool {
	for k, v := range filter {
		if got, ok := t[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func checkTagKeys(keys []string) error {
	if len(keys) > 20 {
		return badRequest("QuotaExceed.Tags", "at most 20 tags, got %d", len(keys))
	}
	for _, k := range keys {
		if k == "" || strings.HasPrefix(k, "aliyun") || strings.HasPrefix(k, "acs:") || strings.Contains(k, "http://") ||
			strings.Contains(k, "https://") || len(k) > 128 {
			return badRequest("InvalidTagKey.Malformed", "invalid tag key %q", k)
		}
	}
	return nil
}

// page returns the PageNumber/PageSize window of n items (size 0 is the
// default, above the maximum is the maximum).
func page(n int, number, size int32) (from, to int) {
	s := int(size)
	if s <= 0 {
		s = DefaultPageSize
	}
	s = min(s, MaxPageSize)
	p := max(int(number), 1)
	from = min((p-1)*s, n)
	return from, min(from+s, n)
}

// tokenPage is the NextToken/MaxResults window: the token is the index of the
// next item.
func tokenPage(n int, next *string, maxResults *int32) (from, to int, nextToken string, err error) {
	if next != nil && *next != "" {
		from, err = strconv.Atoi(*next)
		if err != nil || from < 0 || from > n {
			return 0, 0, "", badRequest("InvalidNextToken.Malformed", "the NextToken is not valid")
		}
	}
	size := DefaultPageSize
	if maxResults != nil && *maxResults > 0 {
		size = min(int(*maxResults), MaxPageSize)
	}
	to = min(from+size, n)
	if to < n {
		nextToken = strconv.Itoa(to)
	}
	return from, to, nextToken, nil
}

// parseIDs reads a JSON array parameter (["i-1","i-2"]) or a comma list.
func parseIDs(s *string) []string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := strings.TrimSpace(*s)
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func idFilter(ids []string, id string) bool { return len(ids) == 0 || slices.Contains(ids, id) }
