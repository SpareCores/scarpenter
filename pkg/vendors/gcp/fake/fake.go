/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is the in-memory Compute Engine API behind gcp.ComputeAPI. It is
// faithful to the behaviours plan 16 relies on: unique instance names per zone
// (409), requestId replay, asynchronous operations that carry capacity, quota
// and organization-policy errors, states and transitions, a subset of the list
// filter language, paged lists, aggregated lists with an unreachable zone,
// networks, subnets, firewall rules and resource policies with the in-use
// refusal, a project without a default network, and per-call error injection.
//
// It does not import pkg/vendors/gcp (the interface is satisfied structurally;
// gcp's tests assert it), so the driver's tests can import it.
//
// Assumptions that the plan marks UNVERIFIED are the fake's choices and are
// marked here: which quota metrics an instance consumes (see metricsFor), and
// that a requestId replay returns the original operation even when it failed.
package fake

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

// Field names of the filter language the fake knows, and the Spot model.
const (
	fieldName   = "name"
	fieldStatus = "status"
	modelSpot   = "SPOT"
)

const apiBase = "https://www.googleapis.com/compute/v1/"

// Compute is the fake. The zero value is not usable: use New.
type Compute struct {
	mu      sync.Mutex
	project string
	now     func() time.Time
	seq     uint64

	regions   map[string]*computepb.Region
	zones     map[string]*computepb.Zone
	types     map[string]map[string]*computepb.MachineType // zone -> name
	images    map[string]*computepb.Image                  // project/name
	families  map[string]string                            // project/family -> name
	instances map[string]*computepb.Instance               // zone/name
	networks  map[string]*computepb.Network
	subnets   map[string]*computepb.Subnetwork // region/name
	firewalls map[string]*computepb.Firewall
	policies  map[string]*computepb.ResourcePolicy // region/name
	// limits are the project's quota limits: "<region>|<metric>", or "|<metric>"
	// for the project-wide ones.
	limits  map[string]float64
	foreign map[string]float64 // usage by other owners, same keys

	ops        map[string]*operation
	requestIDs map[string]string // method|scope|requestId -> operation name
	opPolls    int
	failOps    map[string][]opFailure
	injected   map[string][]error
	calls      map[string]int
	stockouts  map[string]bool
	orgPolicy  bool
	unreach    map[string]bool
	usedIPs    map[string]map[string]bool // subnet key -> addresses
}

type opFailure struct {
	code, message string
	status        int32
}

type operation struct {
	op     *computepb.Operation
	polls  int
	finish func()
	failed *opFailure
}

// New returns an empty fake for the project: no regions, no default network.
func New(project string) *Compute {
	return &Compute{
		project: project, now: time.Now,
		regions: map[string]*computepb.Region{}, zones: map[string]*computepb.Zone{},
		types: map[string]map[string]*computepb.MachineType{}, images: map[string]*computepb.Image{},
		families: map[string]string{}, instances: map[string]*computepb.Instance{},
		networks: map[string]*computepb.Network{}, subnets: map[string]*computepb.Subnetwork{},
		firewalls: map[string]*computepb.Firewall{}, policies: map[string]*computepb.ResourcePolicy{},
		limits: map[string]float64{}, foreign: map[string]float64{},
		ops: map[string]*operation{}, requestIDs: map[string]string{}, opPolls: 1,
		failOps: map[string][]opFailure{}, injected: map[string][]error{}, calls: map[string]int{},
		stockouts: map[string]bool{}, unreach: map[string]bool{}, usedIPs: map[string]map[string]bool{},
	}
}

// ----- errors -----

// Error builds the *googleapi.Error the REST client returns.
func Error(status int, reason, msg string) error {
	return &googleapi.Error{Code: status, Message: msg, Errors: []googleapi.ErrorItem{{Reason: reason, Message: msg}}}
}

func notFound(what string) error {
	return Error(http.StatusNotFound, "notFound", fmt.Sprintf("The resource '%s' was not found", what))
}

func invalid(format string, args ...any) error {
	return Error(http.StatusBadRequest, "invalid", fmt.Sprintf(format, args...))
}

func inUse(format string, args ...any) error {
	return Error(http.StatusBadRequest, "resourceInUseByAnotherResource", fmt.Sprintf(format, args...))
}

// ----- test setup -----

// SetClock sets the clock that stamps creationTimestamp.
func (c *Compute) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// AddRegion adds an UP region with quota limits by metric (zero usage; usage is
// computed). A region has no zones until AddZone.
func (c *Compute) AddRegion(name string, limits map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.regions[name] = &computepb.Region{
		Name: new(name), Id: new(c.seq), Status: new("UP"),
		SelfLink: new(c.url("regions/" + name)),
	}
	for m, l := range limits {
		c.limits[name+"|"+m] = l
	}
}

// AddZone adds an UP zone with its numeric ID (Keeper's zone_id).
func (c *Compute) AddZone(region, zone string, id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zones[zone] = &computepb.Zone{
		Name: new(zone), Id: new(id), Status: new("UP"),
		Region: new(c.url("regions/" + region)), SelfLink: new(c.url("zones/" + zone)),
	}
	c.types[zone] = map[string]*computepb.MachineType{}
}

// SetZoneStatus sets a zone's status (UP or DOWN).
func (c *Compute) SetZoneStatus(zone, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if z := c.zones[zone]; z != nil {
		z.Status = new(status)
	}
}

// SetZoneUnreachable makes aggregatedList skip the zone and report it in
// Unreachables (partial success).
func (c *Compute) SetZoneUnreachable(zone string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unreach[zone] = v
}

// AddMachineType adds a machine type to a zone; arch is X86_64 or ARM64.
func (c *Compute) AddMachineType(zone, name string, vcpus, memoryMB int32, arch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.types[zone][name] = &computepb.MachineType{
		Name: new(name), GuestCpus: new(vcpus), MemoryMb: new(memoryMB),
		Architecture: new(arch), Zone: new(zone),
		SelfLink: new(c.url("zones/" + zone + "/machineTypes/" + name)),
	}
}

// RemoveMachineType removes a machine type from a zone (a zone that does not
// offer it).
func (c *Compute) RemoveMachineType(zone, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.types[zone], name)
}

// AddImage adds an image of a project; family may be empty; arch is X86_64 or
// ARM64; features are guest OS feature types (GVNIC, UEFI_COMPATIBLE...).
// Images cannot be listed, only read by name or family, like kinvolk-public's.
func (c *Compute) AddImage(project, name, family, arch string, features ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img := &computepb.Image{
		Name: new(name), Family: new(family), Architecture: new(arch),
		DiskSizeGb: proto.Int64(12), Status: new("READY"),
		SelfLink: new(c.url("projects/" + project + "/global/images/" + name)),
	}
	for _, f := range features {
		img.GuestOsFeatures = append(img.GuestOsFeatures, &computepb.GuestOsFeature{Type: new(f)})
	}
	c.images[project+"/"+name] = img
	if family != "" {
		c.families[project+"/"+family] = name
	}
}

// SetLimit sets a quota limit; region "" is a project-wide metric.
func (c *Compute) SetLimit(region, metric string, limit float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limits[region+"|"+metric] = limit
}

// AddForeignUsage adds usage by other owners (other workloads of the project);
// region "" is project-wide.
func (c *Compute) AddForeignUsage(region, metric string, n float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.foreign[region+"|"+metric] += n
}

// Stockout makes launches of the machine type in the zone fail with
// ZONE_RESOURCE_POOL_EXHAUSTED; spot selects the capacity type.
func (c *Compute) Stockout(zone, machineType string, spot bool, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stockouts[stockKey(zone, machineType, spot)] = on
}

func stockKey(zone, mt string, spot bool) string { return fmt.Sprintf("%s|%s|%v", zone, mt, spot) }

// SetOrgPolicyViolation makes every launch fail with CONDITION_NOT_MET.
func (c *Compute) SetOrgPolicyViolation(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgPolicy = on
}

// SetOpPolls sets how many Get calls an operation needs before it reports DONE
// (Wait completes it at once); default 1.
func (c *Compute) SetOpPolls(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opPolls = n
}

// Inject makes the next times calls of the method (the ComputeAPI method name,
// "InsertInstance") return err before any effect.
func (c *Compute) Inject(method string, err error, times int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range times {
		c.injected[method] = append(c.injected[method], err)
	}
}

// InjectOpError makes the next mutating call of the method succeed in the
// request and fail in the finished operation with the code (no effect is
// applied).
func (c *Compute) InjectOpError(method, code, message string, status int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failOps[method] = append(c.failOps[method], opFailure{code, message, status})
}

// Calls returns how many times the method was called (including failed calls).
func (c *Compute) Calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

// begin counts the call and returns an injected error, if any.
func (c *Compute) begin(method string) error {
	c.calls[method]++
	if q := c.injected[method]; len(q) > 0 {
		c.injected[method] = q[1:]
		return q[0]
	}
	return nil
}

func (c *Compute) checkProject(p string) error {
	if p != c.project {
		return Error(http.StatusForbidden, "forbidden", fmt.Sprintf("Required 'compute.projects.get' permission for 'projects/%s'", p))
	}
	return nil
}

// ----- naming helpers -----

func (c *Compute) url(path string) string { return apiBase + "projects/" + c.project + "/" + path }

func last(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }

// regionOf returns the region of a zone name.
func regionOf(zone string) string {
	if i := strings.LastIndexByte(zone, '-'); i >= 0 {
		return zone[:i]
	}
	return zone
}

func (c *Compute) timestamp() string { return c.now().UTC().Format(time.RFC3339) }

// ----- operations -----

// newOp registers an operation. scope is "zones/<z>", "regions/<r>" or "global".
// finish runs when the operation completes (the effect on the resources); failed
// non-nil makes the operation end in error with no effect.
func (c *Compute) newOp(kind, scope, target string, failed *opFailure, finish func()) *computepb.Operation {
	c.seq++
	name := "operation-" + strconv.FormatUint(c.seq, 10)
	op := &computepb.Operation{
		Name: new(name), Id: new(c.seq), OperationType: new(kind),
		Status: computepb.Operation_RUNNING.Enum(), InsertTime: new(c.timestamp()),
		TargetLink: new(c.url(target)), SelfLink: new(c.url(scopePath(scope) + "operations/" + name)),
	}
	switch {
	case strings.HasPrefix(scope, "zones/"):
		op.Zone = new(c.url(scope))
	case strings.HasPrefix(scope, "regions/"):
		op.Region = new(c.url(scope))
	}
	c.ops[name] = &operation{op: op, finish: finish, failed: failed}
	return op
}

func scopePath(scope string) string {
	if scope == "global" {
		return "global/"
	}
	return scope + "/"
}

// complete finishes the operation: applies its effect or records its error.
func (c *Compute) complete(o *operation) {
	if o.op.GetStatus() == computepb.Operation_DONE {
		return
	}
	if o.failed != nil {
		o.op.Error = &computepb.Error{Errors: []*computepb.Errors{{Code: new(o.failed.code), Message: new(o.failed.message)}}}
		o.op.HttpErrorStatusCode = new(o.failed.status)
		o.op.HttpErrorMessage = new(http.StatusText(int(o.failed.status)))
	} else if o.finish != nil {
		o.finish()
	}
	o.op.Status = computepb.Operation_DONE.Enum()
	o.op.Progress = proto.Int32(100)
	o.op.EndTime = new(c.timestamp())
}

func (c *Compute) wait(project, name string) (*computepb.Operation, error) {
	if err := c.checkProject(project); err != nil {
		return nil, err
	}
	o, ok := c.ops[name]
	if !ok {
		return nil, notFound("projects/" + project + "/operations/" + name)
	}
	c.complete(o)
	return proto.Clone(o.op).(*computepb.Operation), nil
}

func (c *Compute) get(project, name string) (*computepb.Operation, error) {
	if err := c.checkProject(project); err != nil {
		return nil, err
	}
	o, ok := c.ops[name]
	if !ok {
		return nil, notFound("projects/" + project + "/operations/" + name)
	}
	o.polls++
	if o.polls >= c.opPolls {
		c.complete(o)
	}
	return proto.Clone(o.op).(*computepb.Operation), nil
}

func (c *Compute) WaitZoneOperation(_ context.Context, r *computepb.WaitZoneOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("WaitZoneOperation"); err != nil {
		return nil, err
	}
	return c.wait(r.GetProject(), r.GetOperation())
}

func (c *Compute) GetZoneOperation(_ context.Context, r *computepb.GetZoneOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetZoneOperation"); err != nil {
		return nil, err
	}
	return c.get(r.GetProject(), r.GetOperation())
}

func (c *Compute) WaitRegionOperation(_ context.Context, r *computepb.WaitRegionOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("WaitRegionOperation"); err != nil {
		return nil, err
	}
	return c.wait(r.GetProject(), r.GetOperation())
}

func (c *Compute) GetRegionOperation(_ context.Context, r *computepb.GetRegionOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetRegionOperation"); err != nil {
		return nil, err
	}
	return c.get(r.GetProject(), r.GetOperation())
}

func (c *Compute) WaitGlobalOperation(_ context.Context, r *computepb.WaitGlobalOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("WaitGlobalOperation"); err != nil {
		return nil, err
	}
	return c.wait(r.GetProject(), r.GetOperation())
}

func (c *Compute) GetGlobalOperation(_ context.Context, r *computepb.GetGlobalOperationRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetGlobalOperation"); err != nil {
		return nil, err
	}
	return c.get(r.GetProject(), r.GetOperation())
}

// replay returns the operation of an earlier request with the same ID (the
// documented requestId rule; that a failed operation is replayed as failed is
// UNVERIFIED).
func (c *Compute) replay(method, scope string, id *string) *computepb.Operation {
	if id == nil || *id == "" {
		return nil
	}
	if name, ok := c.requestIDs[method+"|"+scope+"|"+*id]; ok {
		return proto.Clone(c.ops[name].op).(*computepb.Operation)
	}
	return nil
}

func (c *Compute) remember(method, scope string, id *string, op *computepb.Operation) {
	if id != nil && *id != "" {
		c.requestIDs[method+"|"+scope+"|"+*id] = op.GetName()
	}
}

// takeOpFailure returns an injected operation failure for the method.
func (c *Compute) takeOpFailure(method string) *opFailure {
	q := c.failOps[method]
	if len(q) == 0 {
		return nil
	}
	f := q[0]
	c.failOps[method] = q[1:]
	return &f
}

// ----- paging and filters -----

const defaultPageSize = 500

// pageBounds returns the slice bounds of the page and the next token.
func pageBounds(n int, max *uint32, token *string) (lo, hi int, next *string, err error) {
	size := defaultPageSize
	if max != nil && *max > 0 {
		size = int(*max)
	}
	if token != nil && *token != "" {
		lo, err = strconv.Atoi(*token)
		if err != nil || lo < 0 || lo > n {
			return 0, 0, nil, invalid("Invalid value for field 'pageToken': '%s'", *token)
		}
	}
	hi = min(lo+size, n)
	if hi < n {
		t := strconv.Itoa(hi)
		next = &t
	}
	return lo, hi, next, nil
}

var (
	clauseRe = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\s*(=|!=|eq|ne)\s*(.+)$`)
	andRe    = regexp.MustCompile(`(?i)\s+AND\s+`)
	orRe     = regexp.MustCompile(`(?i)\s+OR\s+`)
	quotedRe = regexp.MustCompile(`"[^"]*"|'[^']*'`)
)

// matchFilter evaluates a subset of the list filter language: clauses
// "<field> <op> <value>" joined by AND, op one of = != eq ne, the value a
// quoted or bare regular expression that must match a value of the field
// completely. OR and other forms are rejected, as an invalid filter. values
// returns the values of a field (labels.<k>, name, tags.items, status).
func matchFilter(filter *string, values func(field string) ([]string, bool)) (bool, error) {
	if filter == nil || strings.TrimSpace(*filter) == "" {
		return true, nil
	}
	if orRe.MatchString(quotedRe.ReplaceAllString(*filter, `""`)) {
		return false, invalid("Invalid value for field 'filter': '%s'. OR is not supported by the fake.", *filter)
	}
	for _, clause := range andRe.Split(strings.TrimSpace(*filter), -1) {
		clause = strings.TrimSpace(clause)
		for strings.HasPrefix(clause, "(") && strings.HasSuffix(clause, ")") {
			clause = strings.TrimSpace(clause[1 : len(clause)-1])
		}
		m := clauseRe.FindStringSubmatch(clause)
		if m == nil {
			return false, invalid("Invalid value for field 'filter': '%s'. Unsupported filter clause.", clause)
		}
		field, op, rhs := m[1], m[2], strings.TrimSpace(m[3])
		if len(rhs) >= 2 && (rhs[0] == '"' || rhs[0] == '\'') && rhs[len(rhs)-1] == rhs[0] {
			rhs = rhs[1 : len(rhs)-1]
		}
		re, err := regexp.Compile("^(?:" + rhs + ")$")
		if err != nil {
			return false, invalid("Invalid value for field 'filter': '%s'. Bad regular expression.", rhs)
		}
		vals, known := values(field)
		if !known {
			return false, invalid("Invalid value for field 'filter': '%s'. Unknown field %q.", clause, field)
		}
		hit := false
		for _, v := range vals {
			if re.MatchString(v) {
				hit = true
			}
		}
		if (op == "=" || op == "eq") != hit {
			return false, nil
		}
	}
	return true, nil
}

func nameValues(name string) func(string) ([]string, bool) {
	return func(f string) ([]string, bool) {
		if f == fieldName {
			return []string{name}, true
		}
		return nil, false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
