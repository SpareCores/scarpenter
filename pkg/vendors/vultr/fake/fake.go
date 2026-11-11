/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory Vultr API for the tests of the driver. It
// implements vultr.API (the package does not import vultr, so vultr's own tests
// can use it) with the behaviours plan 20 relies on: stock per region, snapshots
// that become complete, instances that become active with their IP, a list
// filter on one tag, cursor pagination, firewall groups and VPCs that cannot be
// deleted while used, no server-side idempotency, the account's instance limit,
// and error injection with a status and a message. The error texts are the
// plan's HYPOTHESES (spike S4 records the real ones); they are constants so that
// the classification tests and the fake agree on them.
package fake

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vultr/govultr/v3"
)

// Error is an API error with the HTTP status of its response; Status 0 is a
// lost status (govultr drops it): only the text is left.
type Error struct {
	Status  int
	Message string
}

// Error implements error.
func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// HTTPStatus returns the response status.
func (e *Error) HTTPStatus() int { return e.Status }

// The messages of the fake's errors.
const (
	MsgPlanUnavailable   = "Plan is currently unavailable in this location."
	MsgInvalidPlan       = "Invalid plan."
	MsgInvalidRegion     = "Invalid region."
	MsgInvalidSnapshot   = "Invalid snapshot ID."
	MsgSnapshotNotReady  = "Snapshot is not complete."
	MsgInvalidUserData   = "Invalid user_data: must be base64 encoded."
	MsgInvalidFirewall   = "Invalid firewall group ID."
	MsgInvalidVPC        = "Invalid VPC ID."
	MsgInstanceLimit     = "You have reached the account limit of instances."
	MsgNotFound          = "Not found."
	MsgGroupInUse        = "Unable to remove the firewall group: it is attached to instances."
	MsgVPCAttached       = "Unable to delete the VPC: it has attached resources."
	MsgInstanceInstalls  = "Unable to delete the instance: it is locked while installing."
	MsgRuleLimit         = "The firewall group has reached its maximum number of rules."
	MsgInvalidRule       = "Invalid firewall rule."
	MsgRateLimited       = "Rate limit exceeded."
	MaxRulesPerGroup     = 50
	defaultPerPage       = 100
	maxPerPage           = 500
	pendingIP            = "0.0.0.0"
	StatusPending        = "pending"
	StatusActive         = "active"
	PowerRunning         = "running"
	SnapshotComplete     = "complete"
	SnapshotPendingState = "pending"
)

// Account is the fake: one Vultr account.
type Account struct {
	mu  sync.Mutex
	seq int
	now func() time.Time

	// SubstringDescriptionFilter makes the snapshot description filter match a
	// substring instead of the whole text (which of the two Vultr does is
	// unverified, S1).
	SubstringDescriptionFilter bool
	// InstanceLimit is the account's maximum number of instances, 0 for none.
	InstanceLimit int
	// ListLag keeps new instances out of the instance list until Settle:
	// eventual consistency (unverified, S4).
	ListLag bool
	// RefuseDeleteWhilePending refuses to delete an instance that is still
	// installing (unverified, S4).
	RefuseDeleteWhilePending bool

	regions   []govultr.Region
	avail     map[string][]string
	plans     []govultr.Plan
	snapshots []*govultr.Snapshot
	instances []*instance
	groups    []*group
	vpcs      []*govultr.VPC

	// injected errors: failures[method] is consumed one per call; sticky stays.
	failures map[string][]error
	sticky   map[string]error
	// timeoutCreates make that many creates succeed server-side and fail for the
	// caller with a deadline error.
	timeoutCreates int

	calls  map[string]int
	inputs map[string][]any
}

type instance struct {
	govultr.Instance
	hidden   bool
	attached []string
}

type group struct {
	govultr.FirewallGroup
	rules  []govultr.FirewallRule
	nextID int
}

// New returns an empty account with a clock.
func New() *Account {
	return &Account{
		now: time.Now, avail: map[string][]string{}, failures: map[string][]error{}, sticky: map[string]error{},
		calls: map[string]int{}, inputs: map[string][]any{},
	}
}

// SetClock sets the time of date_created.
func (a *Account) SetClock(now func() time.Time) { a.now = now }

func (a *Account) id(prefix string) string {
	a.seq++
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, a.seq)
}

// ---- setup and inspection ----

// AddRegion adds a region with the plans in stock there.
func (a *Account) AddRegion(id string, availablePlans ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.regions = append(a.regions,
		govultr.Region{ID: id, City: id, Country: "XX", Options: []string{}, Connectivity: []string{}})
	a.avail[id] = slices.Clone(availablePlans)
}

// SetAvailable replaces the plans in stock in a region.
func (a *Account) SetAvailable(region string, plans ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.avail[region] = slices.Clone(plans)
}

// AddPlan adds a plan to the plan list; creates of other plans are invalid.
func (a *Account) AddPlan(id string, vcpus, ramMB, diskGB int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plans = append(a.plans, govultr.Plan{ID: id, VCPUCount: vcpus, RAM: ramMB, Disk: diskGB, Type: "vc2"})
}

// AddSnapshot adds a snapshot with a status (complete or pending).
func (a *Account) AddSnapshot(description, status string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &govultr.Snapshot{ID: a.id("5a000000"), Description: description, Status: status,
		DateCreated: a.now().UTC().Format(time.RFC3339), Size: 1 << 30, CompressedSize: 1 << 29}
	a.snapshots = append(a.snapshots, s)
	return s.ID
}

// Advance completes what is pending: snapshots become complete, instances become
// active and running with their IP and server_status ok.
func (a *Account) Advance() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.snapshots {
		if s.Status == SnapshotPendingState {
			s.Status = SnapshotComplete
		}
	}
	for _, in := range a.instances {
		if in.Status == StatusPending {
			in.Status, in.PowerStatus, in.ServerStatus = StatusActive, PowerRunning, "ok"
			in.MainIP = "203.0.113." + strconv.Itoa(a.seq%250+1)
			in.V6MainIP = ""
		}
	}
}

// Settle makes the lagging instances visible in the list.
func (a *Account) Settle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, in := range a.instances {
		in.hidden = false
	}
}

// SetInstanceStatus sets the status fields of an instance (a suspended or
// powered-off one, for the state map).
func (a *Account) SetInstanceStatus(id, status, power, server string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, in := range a.instances {
		if in.ID == id {
			in.Status, in.PowerStatus, in.ServerStatus = status, power, server
		}
	}
}

// AddForeignInstance adds an instance that belongs to someone else on the
// account: no SCarpenter tags. It is active.
func (a *Account) AddForeignInstance(region, plan string, tags ...string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	in := a.newInstance(&govultr.InstanceCreateReq{Region: region, Plan: plan, Label: "foreign", Tags: tags})
	in.Status, in.PowerStatus, in.ServerStatus, in.MainIP = StatusActive, PowerRunning, "ok", "198.51.100.7"
	return in.ID
}

// Instances returns copies of the instances, in creation order.
func (a *Account) Instances() []govultr.Instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]govultr.Instance, 0, len(a.instances))
	for _, in := range a.instances {
		out = append(out, in.Instance)
	}
	return out
}

// Groups returns copies of the firewall groups.
func (a *Account) Groups() []govultr.FirewallGroup {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]govultr.FirewallGroup, 0, len(a.groups))
	for _, g := range a.groups {
		out = append(out, g.FirewallGroup)
	}
	return out
}

// VPCs returns copies of the VPCs.
func (a *Account) VPCs() []govultr.VPC {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]govultr.VPC, len(a.vpcs))
	for i, v := range a.vpcs {
		out[i] = *v
	}
	return out
}

// Calls is the number of calls of a method (the method name of vultr.API).
func (a *Account) Calls(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[method]
}

// Inputs are the request bodies the method received, oldest first.
func (a *Account) Inputs(method string) []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.inputs[method])
}

// FailNext makes the next n calls of the method fail with err (n < 0: forever).
func (a *Account) FailNext(method string, n int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n < 0 {
		a.sticky[method] = err
		return
	}
	for range n {
		a.failures[method] = append(a.failures[method], err)
	}
}

// ClearErrors drops every injected error.
func (a *Account) ClearErrors() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures, a.sticky = map[string][]error{}, map[string]error{}
	a.timeoutCreates = 0
}

// TimeoutNextCreates makes the next n CreateInstance calls succeed on the
// server and fail for the caller with a deadline error (the response was lost).
func (a *Account) TimeoutNextCreates(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.timeoutCreates = n
}

// enter records a call and returns an injected error. Callers hold the lock.
func (a *Account) enter(method string, input any) error {
	a.calls[method]++
	if input != nil {
		a.inputs[method] = append(a.inputs[method], input)
	}
	if q := a.failures[method]; len(q) > 0 {
		a.failures[method] = q[1:]
		return q[0]
	}
	return a.sticky[method]
}

func fail(status int, msg string) error { return &Error{Status: status, Message: msg} }

// page cuts a list to the page the options ask for; the cursor is the index of
// the first item.
func page[T any](items []T, opts *govultr.ListOptions) ([]T, *govultr.Meta, error) {
	per, start := defaultPerPage, 0
	if opts != nil {
		if opts.PerPage > 0 {
			per = min(opts.PerPage, maxPerPage)
		}
		if opts.Cursor != "" {
			n, err := strconv.Atoi(opts.Cursor)
			if err != nil || n < 0 {
				return nil, nil, fail(http.StatusBadRequest, "Invalid cursor.")
			}
			start = n
		}
	}
	meta := &govultr.Meta{Total: len(items), Links: &govultr.Links{}}
	if start >= len(items) {
		return nil, meta, nil
	}
	end := min(start+per, len(items))
	if end < len(items) {
		meta.Links.Next = strconv.Itoa(end)
	}
	return slices.Clone(items[start:end]), meta, nil
}

// ---- regions and plans ----

// ListRegions implements vultr.API.
func (a *Account) ListRegions(_ context.Context, opts *govultr.ListOptions) ([]govultr.Region, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListRegions", nil); err != nil {
		return nil, nil, err
	}
	return page(a.regions, opts)
}

// RegionAvailability implements vultr.API.
func (a *Account) RegionAvailability(_ context.Context, regionID, _ string) (*govultr.PlanAvailability, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("RegionAvailability", regionID); err != nil {
		return nil, err
	}
	plans, ok := a.avail[regionID]
	if !ok {
		return nil, fail(http.StatusNotFound, MsgInvalidRegion)
	}
	return &govultr.PlanAvailability{AvailablePlans: slices.Clone(plans)}, nil
}

// ListPlans implements vultr.API.
func (a *Account) ListPlans(
	_ context.Context, _ string, opts *govultr.ListOptions,
) ([]govultr.Plan, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListPlans", nil); err != nil {
		return nil, nil, err
	}
	return page(a.plans, opts)
}

// ---- snapshots ----

// ListSnapshots implements vultr.API.
func (a *Account) ListSnapshots(
	_ context.Context, opts *govultr.ListOptions,
) ([]govultr.Snapshot, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListSnapshots", nil); err != nil {
		return nil, nil, err
	}
	var items []govultr.Snapshot
	for _, s := range a.snapshots {
		if opts != nil && opts.Description != "" {
			if a.SubstringDescriptionFilter && !strings.Contains(s.Description, opts.Description) {
				continue
			}
			if !a.SubstringDescriptionFilter && s.Description != opts.Description {
				continue
			}
		}
		items = append(items, *s)
	}
	return page(items, opts)
}

// GetSnapshot implements vultr.API.
func (a *Account) GetSnapshot(_ context.Context, id string) (*govultr.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("GetSnapshot", id); err != nil {
		return nil, err
	}
	for _, s := range a.snapshots {
		if s.ID == id {
			c := *s
			return &c, nil
		}
	}
	return nil, fail(http.StatusNotFound, MsgNotFound)
}

// ---- instances ----

func (a *Account) newInstance(req *govultr.InstanceCreateReq) *instance {
	in := &instance{Instance: govultr.Instance{
		ID: a.id("1a000000"), Region: req.Region, Plan: req.Plan, Label: req.Label, Hostname: req.Hostname,
		Tags: slices.Clone(req.Tags), SnapshotID: req.SnapshotID, FirewallGroupID: req.FirewallGroupID,
		Status: StatusPending, PowerStatus: PowerRunning, ServerStatus: "none", MainIP: pendingIP,
		V6MainIP: "", InternalIP: "", DefaultPassword: "fake-default-password",
		DateCreated: a.now().UTC().Format(time.RFC3339),
	}}
	if req.EnableIPv6 != nil && *req.EnableIPv6 {
		in.V6MainIP = "2001:db8::1"
		in.V6Network = "2001:db8::"
	}
	for _, p := range a.plans {
		if p.ID == req.Plan {
			in.Disk, in.RAM, in.VCPUCount = p.Disk, p.RAM, p.VCPUCount
		}
	}
	a.instances = append(a.instances, in)
	return in
}

// CreateInstance implements vultr.API. The server does not deduplicate: two
// creates make two instances.
func (a *Account) CreateInstance(_ context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("CreateInstance", *req); err != nil {
		return nil, err
	}
	if a.InstanceLimit > 0 && len(a.instances) >= a.InstanceLimit {
		return nil, fail(http.StatusBadRequest, MsgInstanceLimit)
	}
	if _, ok := a.avail[req.Region]; !ok {
		return nil, fail(http.StatusBadRequest, MsgInvalidRegion)
	}
	known := len(a.plans) == 0
	for _, p := range a.plans {
		known = known || p.ID == req.Plan
	}
	if !known {
		return nil, fail(http.StatusBadRequest, MsgInvalidPlan)
	}
	if !slices.Contains(a.avail[req.Region], req.Plan) {
		return nil, fail(http.StatusBadRequest, MsgPlanUnavailable)
	}
	if req.SnapshotID != "" {
		var snap *govultr.Snapshot
		for _, s := range a.snapshots {
			if s.ID == req.SnapshotID {
				snap = s
			}
		}
		switch {
		case snap == nil:
			return nil, fail(http.StatusBadRequest, MsgInvalidSnapshot)
		case snap.Status != SnapshotComplete:
			return nil, fail(http.StatusBadRequest, MsgSnapshotNotReady)
		}
	}
	if req.UserData != "" {
		if _, err := base64.StdEncoding.DecodeString(req.UserData); err != nil {
			return nil, fail(http.StatusBadRequest, MsgInvalidUserData)
		}
	}
	var grp *group
	if req.FirewallGroupID != "" {
		if grp = a.group(req.FirewallGroupID); grp == nil {
			return nil, fail(http.StatusBadRequest, MsgInvalidFirewall)
		}
	}
	for _, v := range req.AttachVPC {
		if vpc := a.vpc(v); vpc == nil || vpc.Region != req.Region {
			return nil, fail(http.StatusBadRequest, MsgInvalidVPC)
		}
	}
	in := a.newInstance(req)
	in.hidden = a.ListLag
	in.attached = slices.Clone(req.AttachVPC)
	if grp != nil {
		grp.InstanceCount++
	}
	out := in.Instance
	if a.timeoutCreates > 0 {
		a.timeoutCreates--
		return nil, context.DeadlineExceeded
	}
	return &out, nil
}

func (a *Account) find(id string) (int, *instance) {
	for i, in := range a.instances {
		if in.ID == id {
			return i, in
		}
	}
	return -1, nil
}

// GetInstance implements vultr.API.
func (a *Account) GetInstance(_ context.Context, id string) (*govultr.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("GetInstance", id); err != nil {
		return nil, err
	}
	_, in := a.find(id)
	if in == nil {
		return nil, fail(http.StatusNotFound, MsgNotFound)
	}
	out := in.Instance
	return &out, nil
}

// DeleteInstance implements vultr.API.
func (a *Account) DeleteInstance(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("DeleteInstance", id); err != nil {
		return err
	}
	i, in := a.find(id)
	if in == nil {
		return fail(http.StatusNotFound, MsgNotFound)
	}
	if a.RefuseDeleteWhilePending && in.Status == StatusPending {
		return fail(http.StatusBadRequest, MsgInstanceInstalls)
	}
	if g := a.group(in.FirewallGroupID); g != nil {
		g.InstanceCount--
	}
	a.instances = slices.Delete(a.instances, i, i+1)
	return nil
}

// ListInstances implements vultr.API: tag takes one value and matches a whole tag
// string; region, label and main_ip filter too.
func (a *Account) ListInstances(
	_ context.Context, opts *govultr.ListOptions,
) ([]govultr.Instance, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListInstances", nil); err != nil {
		return nil, nil, err
	}
	var items []govultr.Instance
	for _, in := range a.instances {
		switch {
		case in.hidden:
			continue
		case opts != nil && opts.Tag != "" && !slices.Contains(in.Tags, opts.Tag):
			continue
		case opts != nil && opts.Region != "" && in.Region != opts.Region:
			continue
		case opts != nil && opts.Label != "" && in.Label != opts.Label:
			continue
		case opts != nil && opts.MainIP != "" && in.MainIP != opts.MainIP:
			continue
		}
		c := in.Instance
		c.DefaultPassword = "" // the list never returns it
		items = append(items, c)
	}
	return page(items, opts)
}

// ---- firewall ----

func (a *Account) group(id string) *group {
	for _, g := range a.groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// CreateFirewallGroup implements vultr.API.
func (a *Account) CreateFirewallGroup(
	_ context.Context, req *govultr.FirewallGroupReq,
) (*govultr.FirewallGroup, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("CreateFirewallGroup", *req); err != nil {
		return nil, err
	}
	now := a.now().UTC().Format(time.RFC3339)
	g := &group{FirewallGroup: govultr.FirewallGroup{
		ID: a.id("fa000000"), Description: req.Description, DateCreated: now, DateModified: now,
		MaxRuleCount: MaxRulesPerGroup,
	}, nextID: 1}
	a.groups = append(a.groups, g)
	out := g.FirewallGroup
	return &out, nil
}

// GetFirewallGroup implements vultr.API.
func (a *Account) GetFirewallGroup(_ context.Context, id string) (*govultr.FirewallGroup, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("GetFirewallGroup", id); err != nil {
		return nil, err
	}
	g := a.group(id)
	if g == nil {
		return nil, fail(http.StatusNotFound, MsgNotFound)
	}
	out := g.FirewallGroup
	return &out, nil
}

// DeleteFirewallGroup implements vultr.API: refused while instances use it.
func (a *Account) DeleteFirewallGroup(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("DeleteFirewallGroup", id); err != nil {
		return err
	}
	g := a.group(id)
	if g == nil {
		return fail(http.StatusNotFound, MsgNotFound)
	}
	if g.InstanceCount > 0 {
		return fail(http.StatusBadRequest, MsgGroupInUse)
	}
	a.groups = slices.DeleteFunc(a.groups, func(x *group) bool { return x.ID == id })
	return nil
}

// ListFirewallGroups implements vultr.API.
func (a *Account) ListFirewallGroups(
	_ context.Context, opts *govultr.ListOptions,
) ([]govultr.FirewallGroup, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListFirewallGroups", nil); err != nil {
		return nil, nil, err
	}
	items := make([]govultr.FirewallGroup, len(a.groups))
	for i, g := range a.groups {
		items[i] = g.FirewallGroup
	}
	return page(items, opts)
}

var ruleProtocols = []string{"tcp", "udp", "icmp", "gre", "esp", "ah"}

// CreateFirewallRule implements vultr.API: at most 50 rules per group.
func (a *Account) CreateFirewallRule(
	_ context.Context, groupID string, req *govultr.FirewallRuleReq,
) (*govultr.FirewallRule, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("CreateFirewallRule", *req); err != nil {
		return nil, err
	}
	g := a.group(groupID)
	if g == nil {
		return nil, fail(http.StatusNotFound, MsgNotFound)
	}
	if (req.IPType != "v4" && req.IPType != "v6") || !slices.Contains(ruleProtocols, req.Protocol) || req.Subnet == "" {
		return nil, fail(http.StatusBadRequest, MsgInvalidRule)
	}
	if len(g.rules) >= MaxRulesPerGroup {
		return nil, fail(http.StatusBadRequest, MsgRuleLimit)
	}
	r := govultr.FirewallRule{ID: g.nextID, Action: "accept", IPType: req.IPType, Protocol: req.Protocol, Port: req.Port,
		Subnet: req.Subnet, SubnetSize: req.SubnetSize, Source: req.Source, Notes: req.Notes}
	g.nextID++
	g.rules = append(g.rules, r)
	g.RuleCount = len(g.rules)
	return &r, nil
}

// ListFirewallRules implements vultr.API.
func (a *Account) ListFirewallRules(
	_ context.Context, groupID string, opts *govultr.ListOptions,
) ([]govultr.FirewallRule, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListFirewallRules", nil); err != nil {
		return nil, nil, err
	}
	g := a.group(groupID)
	if g == nil {
		return nil, nil, fail(http.StatusNotFound, MsgNotFound)
	}
	return page(g.rules, opts)
}

// DeleteFirewallRule implements vultr.API.
func (a *Account) DeleteFirewallRule(_ context.Context, groupID string, ruleID int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("DeleteFirewallRule", ruleID); err != nil {
		return err
	}
	g := a.group(groupID)
	if g == nil {
		return fail(http.StatusNotFound, MsgNotFound)
	}
	n := len(g.rules)
	g.rules = slices.DeleteFunc(g.rules, func(r govultr.FirewallRule) bool { return r.ID == ruleID })
	if len(g.rules) == n {
		return fail(http.StatusNotFound, MsgNotFound)
	}
	g.RuleCount = len(g.rules)
	return nil
}

// ---- VPCs ----

func (a *Account) vpc(id string) *govultr.VPC {
	for _, v := range a.vpcs {
		if v.ID == id {
			return v
		}
	}
	return nil
}

// CreateVPC implements vultr.API: without a subnet the fake picks a free /24 of
// 10.x.0.0 (what Vultr does is unverified).
func (a *Account) CreateVPC(_ context.Context, req *govultr.VPCReq) (*govultr.VPC, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("CreateVPC", *req); err != nil {
		return nil, err
	}
	if _, ok := a.avail[req.Region]; !ok {
		return nil, fail(http.StatusBadRequest, MsgInvalidRegion)
	}
	v := &govultr.VPC{ID: a.id("0c000000"), Region: req.Region, Description: req.Description,
		V4Subnet: req.V4Subnet, V4SubnetMask: req.V4SubnetMask, DateCreated: a.now().UTC().Format(time.RFC3339)}
	if v.V4Subnet == "" {
		v.V4Subnet, v.V4SubnetMask = fmt.Sprintf("10.%d.0.0", len(a.vpcs)+1), 24
	}
	a.vpcs = append(a.vpcs, v)
	out := *v
	return &out, nil
}

// GetVPC implements vultr.API.
func (a *Account) GetVPC(_ context.Context, id string) (*govultr.VPC, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("GetVPC", id); err != nil {
		return nil, err
	}
	v := a.vpc(id)
	if v == nil {
		return nil, fail(http.StatusNotFound, MsgNotFound)
	}
	out := *v
	return &out, nil
}

func (a *Account) attachments(vpcID string) []govultr.VPCAttachment {
	var out []govultr.VPCAttachment
	for _, in := range a.instances {
		if slices.Contains(in.attached, vpcID) {
			out = append(out, govultr.VPCAttachment{ID: in.ID, Type: "instance",
				LinkedSubscription: govultr.VPCAttachmentLinkedSubscription{Type: "instance", ID: in.ID}})
		}
	}
	return out
}

// DeleteVPC implements vultr.API: refused while instances are attached.
func (a *Account) DeleteVPC(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("DeleteVPC", id); err != nil {
		return err
	}
	if a.vpc(id) == nil {
		return fail(http.StatusNotFound, MsgNotFound)
	}
	if len(a.attachments(id)) > 0 {
		return fail(http.StatusBadRequest, MsgVPCAttached)
	}
	a.vpcs = slices.DeleteFunc(a.vpcs, func(v *govultr.VPC) bool { return v.ID == id })
	return nil
}

// ListVPCs implements vultr.API.
func (a *Account) ListVPCs(_ context.Context, opts *govultr.ListOptions) ([]govultr.VPC, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListVPCs", nil); err != nil {
		return nil, nil, err
	}
	items := make([]govultr.VPC, len(a.vpcs))
	for i, v := range a.vpcs {
		items[i] = *v
	}
	return page(items, opts)
}

// ListVPCAttachments implements vultr.API.
func (a *Account) ListVPCAttachments(
	_ context.Context, vpcID string, opts *govultr.ListOptions,
) ([]govultr.VPCAttachment, *govultr.Meta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enter("ListVPCAttachments", nil); err != nil {
		return nil, nil, err
	}
	if a.vpc(vpcID) == nil {
		return nil, nil, fail(http.StatusNotFound, MsgNotFound)
	}
	return page(a.attachments(vpcID), opts)
}
