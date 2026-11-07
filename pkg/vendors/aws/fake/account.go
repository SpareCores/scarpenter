/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory EC2 and Service Quotas for the tests of the AWS
// driver: one Account with regions, availability zones (with IDs), default
// VPCs, images, instances with state transitions, ClientToken idempotency,
// security groups, placement groups, VPCs, subnets, internet gateways and route
// tables (with deletion dependencies), instances and spot requests of other
// owners, capacity reservations, instance type offerings per zone ID and
// per-call error injection. Fidelity gaps, so that tests do not rely on them:
// instances start running (never pending); filtered pages are never short unless
// WithShortFilteredPages is set; ClientTokens never expire (the real service
// forgets them after a while) and only RunInstances and CreateRouteTable model
// them; a reservation holds one instance; there is no IAM; a spot request closes
// three minutes after its instance ended (measured). It implements the driver's narrow API interfaces
// and no SDK transport: errors are smithy API errors with the codes the real
// service returns, so the driver's classification runs on them unchanged.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// Option configures an Account.
type Option func(*Account)

// WithClock sets the clock of the account (launch times, the delayed closing of
// spot requests).
func WithClock(c clock.Clock) Option { return func(a *Account) { a.clock = c } }

// WithPageSize sets how many items the Describe calls return per page; a small
// value makes the driver's pagination run.
func WithPageSize(n int) Option { return func(a *Account) { a.pageSize = n } }

// WithBigPageSize sets the page size of the calls that list many items (instance
// types, offerings, spot requests, capacity reservations); the real service
// pages instance types by 100.
func WithBigPageSize(n int) Option { return func(a *Account) { a.bigPage = n } }

// WithShortFilteredPages makes a filtered DescribeInstances answer its first page
// empty with a NextToken, as EC2 may when filters drop what the page held.
func WithShortFilteredPages() Option { return func(a *Account) { a.shortPages = true } }

// WithSlowIPv6 makes the association of an Amazon-provided IPv6 block take n
// DescribeVpcs calls (it shows as associating until then), as EC2 does.
func WithSlowIPv6(n int) Option { return func(a *Account) { a.ipv6Delay = n } }

// WithRegions replaces the default regions.
func WithRegions(r ...RegionSpec) Option { return func(a *Account) { a.regionSpecs = r } }

// AZ is an availability zone of a region.
type AZ struct {
	// Name is the account's zone name (us-east-1a), ID the portable zone ID.
	Name, ID string
	// Type is availability-zone (default) or local-zone.
	Type string
	// OptIn is opt-in-not-required (default), opted-in or not-opted-in.
	OptIn string
}

// RegionSpec describes a region of the account.
type RegionSpec struct {
	Name string
	// Disabled makes the region an opt-in region the account has not enabled.
	Disabled bool
	AZs      []AZ
	// NoDefaultVPC leaves the region without a default VPC.
	NoDefaultVPC bool
	// DefaultVPCIPv6 gives the default VPC and its subnets IPv6 (a default VPC has
	// none until someone associates it).
	DefaultVPCIPv6 bool
}

// Account is an AWS account: every region and everything in it.
type Account struct {
	mu          sync.Mutex
	clock       clock.Clock
	pageSize    int
	bigPage     int
	shortPages  bool
	ipv6Delay   int
	regionSpecs []RegionSpec
	regions     map[string]*region
	order       []string
	nextID      int
	types       []InstanceType
	failures    []*Failure
	calls       map[string]int
	inputs      map[string][]any
}

// New returns an account with us-east-1 and eu-central-1 (three zones each, a
// default VPC and default subnets), the 2026 vCPU quota defaults and no images,
// instance types or instances.
func New(opts ...Option) *Account {
	a := &Account{
		clock: clocktesting.NewFakeClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)), pageSize: 3, bigPage: 100,
		regions: map[string]*region{}, calls: map[string]int{}, inputs: map[string][]any{},
	}
	for _, o := range opts {
		o(a)
	}
	if a.regionSpecs == nil {
		a.regionSpecs = []RegionSpec{DefaultRegion("us-east-1", "use1"), DefaultRegion("eu-central-1", "euc1")}
	}
	for _, s := range a.regionSpecs {
		a.addRegionLocked(s)
	}
	return a
}

// DefaultRegion returns a region with three zones named <region>a to <region>c
// and the IDs <prefix>-az1 to <prefix>-az3.
func DefaultRegion(name, prefix string) RegionSpec {
	azs := make([]AZ, 0, 3)
	for i, l := range []string{"a", "b", "c"} {
		azs = append(azs, AZ{Name: name + l, ID: fmt.Sprintf("%s-az%d", prefix, i+1)})
	}
	return RegionSpec{Name: name, AZs: azs}
}

// Clock returns the account's clock.
func (a *Account) Clock() clock.Clock { return a.clock }

// Failure makes calls of an operation fail.
type Failure struct {
	// Op is the API operation (RunInstances, DescribeInstances, ...).
	Op string
	// Code and Message are the smithy API error; Block instead blocks the call
	// until its context ends (a hung connection).
	Code, Message string
	Block         bool
	// Match, if set, restricts the failure to calls whose input it accepts.
	Match func(input any) bool
	// Count is how many calls fail; 0 means one, a negative count is endless.
	Count int
}

// Fail registers a failure.
func (a *Account) Fail(f Failure) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f.Count == 0 {
		f.Count = 1
	}
	a.failures = append(a.failures, &f)
}

// Calls returns how many calls of the operation were made.
func (a *Account) Calls(op string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[op]
}

// Inputs returns the inputs of the calls of the operation, oldest first.
func (a *Account) Inputs(op string) []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.inputs[op])
}

// begin counts a call and applies a matching failure; it must be called with
// the account locked and returns with it locked.
func (a *Account) begin(ctx context.Context, op string, in any) error {
	a.calls[op]++
	a.inputs[op] = append(a.inputs[op], in)
	for i, f := range a.failures {
		if f.Op != op || (f.Match != nil && !f.Match(in)) {
			continue
		}
		if f.Count > 0 {
			f.Count--
			if f.Count == 0 {
				a.failures = slices.Delete(a.failures, i, i+1)
			}
		}
		if f.Block {
			a.mu.Unlock()
			<-ctx.Done()
			a.mu.Lock()
			return ctx.Err()
		}
		return apiError(f.Code, f.Message)
	}
	return ctx.Err()
}

func apiError(code, msg string) error {
	if msg == "" {
		msg = code
	}
	return &smithy.GenericAPIError{Code: code, Message: msg, Fault: smithy.FaultClient}
}

func (a *Account) id(prefix string) string {
	a.nextID++
	return fmt.Sprintf("%s-%017x", prefix, a.nextID)
}

func (a *Account) now() time.Time { return a.clock.Now() }

// regionOf returns the region or the error the service gives for an unknown one.
func (a *Account) regionOf(name string) (*region, error) {
	r, ok := a.regions[name]
	if !ok {
		return nil, apiError("InvalidRegion", fmt.Sprintf("unknown region %s", name))
	}
	if r.disabled {
		return nil, apiError("AuthFailure", "the region "+name+" is not enabled for the account")
	}
	return r, nil
}

// tagsOf converts SDK tags.
func tagsOf(in []types.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range in {
		if t.Key != nil {
			out[*t.Key] = deref(t.Value)
		}
	}
	return out
}

func tagList(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.Tag{Key: new(k), Value: new(m[k])})
	}
	return out
}

// tagsFromSpecs returns the tags the specifications give to the resource type.
func tagsFromSpecs(specs []types.TagSpecification, rt types.ResourceType) map[string]string {
	out := map[string]string{}
	for _, s := range specs {
		if s.ResourceType == rt {
			maps.Copy(out, tagsOf(s.Tags))
		}
	}
	return out
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// glob matches an EC2 filter value: * and ? are wildcards.
func glob(pattern, s string) bool {
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// matches evaluates EC2 filters against an object's attributes: every filter
// must match (values of one filter are alternatives). attr returns the values of
// a filter name; tag:<key> filters are answered from tags.
func matches(filters []types.Filter, tags map[string]string, attr func(name string) ([]string, bool)) (bool, error) {
	for _, f := range filters {
		name := deref(f.Name)
		var have []string
		if key, ok := strings.CutPrefix(name, "tag:"); ok {
			if v, present := tags[key]; present {
				have = []string{v}
			}
		} else if name == "tag-key" {
			for k := range tags {
				have = append(have, k)
			}
		} else {
			v, known := attr(name)
			if !known {
				return false, apiError("InvalidParameterValue", "the filter "+name+" is not valid")
			}
			have = v
		}
		ok := false
		for _, want := range f.Values {
			if slices.ContainsFunc(have, func(h string) bool { return glob(want, h) }) {
				ok = true
			}
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// page returns the page of items that starts at the token (an index) and the
// next token.
func page[T any](items []T, token *string, max *int32, def int) ([]T, *string) {
	start := 0
	if token != nil {
		fmt.Sscanf(*token, "%d", &start) //nolint:errcheck // a bad token starts at 0
	}
	size := def
	if max != nil && int(*max) > 0 {
		size = min(int(*max), def)
	}
	if start > len(items) {
		start = len(items)
	}
	end := min(start+size, len(items))
	var next *string
	if end < len(items) {
		next = new(fmt.Sprint(end))
	}
	return items[start:end], next
}

// hashInput is the idempotency fingerprint of a request.
func hashInput(in any) string {
	raw, _ := json.Marshal(in)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
