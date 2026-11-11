/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory UpCloud, implementing api.API. It is faithful
// to the behaviours plan 19 relies on: servers are created in the maintenance
// state and settle to started; a server must be stopped before it is deleted,
// and a stop in progress is a maintenance state too; there is no idempotency
// token (a replayed create makes a second server); the list answers carry no
// labels (the details do); templates live in one zone and tier; account limits
// are account-wide and count other owners' servers; the per-server firewall is
// replaced as a whole; strict anti-affinity runs out of hosts. Anything the
// plan marks unverified is configurable here, so that the tests show which
// behaviour the driver depends on.
package fake

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// PlanSpec is a plan of the fake: UpCloud's plan record plus the tiers the plan
// accepts for its disks.
type PlanSpec struct {
	upcloud.Plan
	// AllowedTiers are the tiers a storage device of a server of this plan may
	// have (STORAGE_TIER_ILLEGAL otherwise); empty means every tier.
	AllowedTiers []string
}

// DefaultPlans returns a few plans in UpCloud's naming: general purpose with
// included MaxIOPS storage, a Developer plan with Standard storage, a plan family
// without included storage (Cloud Native) and a GPU plan. The names and sizes
// are examples for tests, not a copy of UpCloud's price list.
func DefaultPlans() []PlanSpec {
	p := func(name string, cores, memGB, storageGB int, tier string) PlanSpec {
		return PlanSpec{Plan: upcloud.Plan{
			Name: name, CoreNumber: cores, MemoryAmount: memGB * 1024, StorageSize: storageGB, StorageTier: tier,
		}}
	}
	gpu := p("GPU-8xCPU-64GB-1xL40S", 8, 64, 300, upcloud.StorageTierMaxIOPS)
	gpu.GPUAmount, gpu.GPUModel = 1, "NVIDIA L40S"
	return []PlanSpec{
		p("DEV-1xCPU-1GB-10GB", 1, 1, 10, upcloud.StorageTierStandard),
		p("1xCPU-1GB", 1, 1, 25, upcloud.StorageTierMaxIOPS),
		p("1xCPU-2GB", 1, 2, 50, upcloud.StorageTierMaxIOPS),
		p("2xCPU-4GB", 2, 4, 80, upcloud.StorageTierMaxIOPS),
		p("HICPU-8xCPU-16GB", 8, 16, 200, upcloud.StorageTierMaxIOPS),
		p("CLOUDNATIVE-2xCPU-4GB", 2, 4, 0, ""),
		gpu,
	}
}

// Options configure a Fake.
type Options struct {
	// Zones are the zone IDs; default fi-hel1, de-fra1, nl-ams1.
	Zones []string
	// Plans default to DefaultPlans.
	Plans []PlanSpec
	// Limits are the account's resource limits; a zero field means no limit.
	Limits upcloud.ResourceLimits
	// MaxUserData is the largest user_data accepted (bytes); default 16384.
	MaxUserData int
	// MaxStorageDevices per server; default 16 (the number of the API table; the
	// driver caps lower until spike S3 says otherwise).
	MaxStorageDevices int
	// MaxNetworks is the account's network limit; default 10.
	MaxNetworks int
	// CreatingLimit bounds servers being created at once; default 5.
	CreatingLimit int
	// HostsPerZone is how many hosts a strict anti-affinity group can spread
	// over; default 3.
	HostsPerZone int
	// LooseTemplateTier lets a clone have another tier than its template; by
	// default it is refused (the plan marks this unverified, S1).
	LooseTemplateTier bool
	// AutoSettle completes every transition at once, so that no server is ever
	// seen in maintenance.
	AutoSettle bool
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Fake is the in-memory UpCloud. Methods are safe for concurrent use.
type Fake struct {
	mu   sync.Mutex
	opts Options
	now  func() time.Time
	seq  int

	zones    []upcloud.Zone
	plans    map[string]PlanSpec
	planList []string
	limits   upcloud.ResourceLimits

	servers  map[string]*server
	storages map[string]*storage
	networks map[string]*network
	groups   map[string]*group
	rulesets map[string]*ruleset

	outOfStock map[string]bool // zone/plan
	noIPs      map[string]bool // zone
	calls      map[string]int
	injected   map[string][]injected
}

// New builds a fake.
func New(o Options) *Fake {
	if len(o.Zones) == 0 {
		o.Zones = []string{"fi-hel1", "de-fra1", "nl-ams1"}
	}
	if o.Plans == nil {
		o.Plans = DefaultPlans()
	}
	if o.MaxUserData == 0 {
		o.MaxUserData = 16384
	}
	if o.MaxStorageDevices == 0 {
		o.MaxStorageDevices = 16
	}
	if o.MaxNetworks == 0 {
		o.MaxNetworks = 10
	}
	if o.CreatingLimit == 0 {
		o.CreatingLimit = 5
	}
	if o.HostsPerZone == 0 {
		o.HostsPerZone = 3
	}
	f := &Fake{
		opts: o, now: o.Now, limits: o.Limits,
		plans:   map[string]PlanSpec{},
		servers: map[string]*server{}, storages: map[string]*storage{}, networks: map[string]*network{},
		groups: map[string]*group{}, rulesets: map[string]*ruleset{},
		outOfStock: map[string]bool{}, noIPs: map[string]bool{},
		calls: map[string]int{}, injected: map[string][]injected{},
	}
	if f.now == nil {
		f.now = time.Now
	}
	for _, z := range o.Zones {
		f.zones = append(f.zones, upcloud.Zone{ID: z, Description: z, Public: upcloud.True})
	}
	for _, p := range o.Plans {
		f.plans[p.Name] = p
		f.planList = append(f.planList, p.Name)
	}
	return f
}

// uuid makes a deterministic UUID-shaped ID.
func (f *Fake) uuid(kind byte) string {
	f.seq++
	return fmt.Sprintf("%02x%06x-0000-4000-8000-%012x", kind, f.seq, f.seq)
}

// Problem is the error the real API returns for a code, in the problem+json
// form (type is a URL whose fragment holds ERROR_<code>).
func Problem(code string, status int) *upcloud.Problem {
	return &upcloud.Problem{
		Type:   "https://developers.upcloud.com/1.3/errors#ERROR_" + code,
		Title:  code,
		Status: status,
	}
}

// LegacyProblem is the same error in the legacy form, where the type holds the
// bare code (the API still answers some calls that way).
func LegacyProblem(code string, status int) *upcloud.Problem {
	return &upcloud.Problem{Type: code, Title: code, Status: status}
}

func notFound(code string) error { return Problem(code, http.StatusNotFound) }

func invalid(code string) error { return Problem(code, http.StatusBadRequest) }

func conflict(code string) error { return Problem(code, http.StatusConflict) }

type injected struct {
	err   error
	times int // 0: forever
}

// Inject makes the next times calls of method (the API method name, for
// example CreateServer) fail with err; times 0 fails every call until Clear.
func (f *Fake) Inject(method string, err error, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injected[method] = append(f.injected[method], injected{err: err, times: times})
}

// InjectCode injects a problem with the code and status.
func (f *Fake) InjectCode(method, code string, status, times int) {
	f.Inject(method, Problem(code, status), times)
}

// Clear drops the injected errors of method ("" for all).
func (f *Fake) Clear(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "" {
		f.injected = map[string][]injected{}
		return
	}
	delete(f.injected, method)
}

// Calls returns how many times method was called.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// begin counts the call, takes the lock and returns the function that releases
// it; it returns the context's error or an injected one instead (the lock is
// then already released).
func (f *Fake) begin(ctx context.Context, method string) (func(), error) {
	f.mu.Lock()
	f.calls[method]++
	if err := ctx.Err(); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	if q := f.injected[method]; len(q) > 0 {
		in := q[0]
		if in.times == 1 {
			f.injected[method] = q[1:]
		} else if in.times > 1 {
			q[0].times--
		}
		f.mu.Unlock()
		return nil, in.err
	}
	return f.mu.Unlock, nil
}

// SetOutOfStock makes creates of the plan in the zone fail with
// SERVER_RESOURCES_UNAVAILABLE (UpCloud has no stock API: capacity is reactive).
func (f *Fake) SetOutOfStock(zone, plan string, out bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outOfStock[zone+"/"+plan] = out
}

// SetNoAddresses makes the zone run out of IP addresses.
func (f *Fake) SetNoAddresses(zone string, none bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noIPs[zone] = none
}

// SetLimits replaces the account's resource limits.
func (f *Fake) SetLimits(l upcloud.ResourceLimits) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = l
}

// GetZones lists the zones.
func (f *Fake) GetZones(ctx context.Context) (*upcloud.Zones, error) {
	done, err := f.begin(ctx, "GetZones")
	if err != nil {
		return nil, err
	}
	defer done()
	return &upcloud.Zones{Zones: append([]upcloud.Zone(nil), f.zones...)}, nil
}

// GetPlans lists the plans.
func (f *Fake) GetPlans(ctx context.Context) (*upcloud.Plans, error) {
	done, err := f.begin(ctx, "GetPlans")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &upcloud.Plans{}
	for _, n := range f.planList {
		out.Plans = append(out.Plans, f.plans[n].Plan)
	}
	return out, nil
}

// GetAccount returns the account with its limits (no usage: the driver
// computes it from the lists, as the real API has none either, S7).
func (f *Fake) GetAccount(ctx context.Context) (*upcloud.Account, error) {
	done, err := f.begin(ctx, "GetAccount")
	if err != nil {
		return nil, err
	}
	defer done()
	return &upcloud.Account{UserName: "fake", Credits: 100, ResourceLimits: f.limits}, nil
}

var _ api.API = (*Fake)(nil)
