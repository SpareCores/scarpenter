/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in a
// Google Cloud project (docs/how-to-guides/testing.md#the-l5-lane): instances,
// resource policies, firewall rules, subnetworks and VPC networks. It is the
// sweeper workflow's engine (every three hours, everything of a cluster named
// scarpenter-ci-* older than two hours) and the e2e workflow's last line of
// defence (one cluster, no age limit).
//
// It talks to Compute Engine through the driver's narrow API (gcp.ComputeAPI), so
// the in-memory fake of the driver tests runs it unchanged. Every call should
// carry a deadline: the command wraps the API with gcp.WithTimeouts.
//
// Selection is conservative on purpose, and follows the two kinds of ownership
// the driver writes (plan 16, sections 3 and 9):
//
//   - instances carry the label scarpenter-cluster=<cluster>; their disks are
//     created with autoDelete and go with them (ComputeAPI has no disk calls: the
//     teardown of the lane checks for leftover disks with gcloud);
//   - firewall rules, subnetworks, networks and resource policies have no labels:
//     the name starts with scarpenter-<cluster>- AND the description is the marker
//     of gcp.MarkerDescription (gcp.ParseMarker), both required;
//   - what the lane's workflow brings itself (the byo scenario) is named
//     scarpenter-e2e-byo-* and described by LaneMarker, a marker the controller's
//     garbage collector does not recognize, so that SCarpenter never touches it.
//
// With an age limit a resource is touched only when old enough, by its
// creationTimestamp (every one of these resources has it). The default network
// is never deleted, and a network is deleted only when no subnet is left in it.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
)

const (
	// ClusterLabel is the label every instance of SCarpenter carries with the
	// cluster name (gcp.TagKey of the cluster tag).
	ClusterLabel = gcp.LabelPrefix + "cluster"
	// CreatedAtLabel is the label the lane's NodeClasses put on instances and
	// disks (spec.tags, Unix seconds); the sweeper reads it only when an
	// instance has no creation timestamp.
	CreatedAtLabel = "ci-created-at"
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"
	// ByoNamePrefix is the name prefix of the resources the lane's workflow
	// creates for the bring-your-own scenario.
	ByoNamePrefix = "scarpenter-e2e-byo-"
	// LaneMarkerPrefix starts the description of those resources.
	LaneMarkerPrefix = "scarpenter-e2e-byo: "

	// The kinds of the report.
	kindRegion   = "region"
	kindInstance = "instance"
	kindPolicy   = "resource-policy"
	kindFirewall = "firewall"
	kindSubnet   = "subnet"
	kindNetwork  = "network"

	scopeGlobal = "global"
	defaultNet  = "default"
)

// LaneMarker is the description of a resource the lane's workflow brings: the
// sweeper (and nothing else) recognizes it.
func LaneMarker(cluster string) string { return LaneMarkerPrefix + "cluster=" + cluster }

// ParseLaneMarker reads a description written by LaneMarker.
func ParseLaneMarker(desc string) (cluster string, ok bool) {
	rest, found := strings.CutPrefix(desc, LaneMarkerPrefix)
	if !found {
		return "", false
	}
	for f := range strings.FieldsSeq(rest) {
		if v, has := strings.CutPrefix(f, "cluster="); has && v != "" {
			return v, true
		}
	}
	return "", false
}

// Options select what is swept.
type Options struct {
	// Cluster selects the resources of one cluster by exact name. Without it,
	// ClusterPrefix selects every cluster whose name starts with it (the default
	// prefix when both are empty).
	Cluster       string
	ClusterPrefix string
	// MaxAge is the age a resource must have; 0 means any age (and then also
	// resources without a known age are swept).
	MaxAge time.Duration
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool
	// Regions limits the sweep to these regions (instances, resource policies,
	// subnetworks); empty means every region of the project (see ParseRegions).
	// Only in that all-regions mode is a region the credentials may not read
	// skipped; a requested region that cannot be read is a failure.
	Regions []string
	// WaitDeleted bounds the wait for one deletion to finish and for a resource
	// that is still in use by another one to be released (default 5 minutes).
	WaitDeleted time.Duration
	// PollInterval is the pause between retries of a deletion that found its
	// resource in use (default 5 s).
	PollInterval time.Duration
	// Now is the clock of the age limit (default time.Now); waits use the real clock.
	Now func() time.Time
	// Logf receives one line per decision (default: discard).
	Logf func(format string, args ...any)
}

// Outcome is what happened to a resource.
type Outcome string

// The outcomes.
const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would-delete"
	Skipped     Outcome = "skipped"
	Failed      Outcome = "failed"
)

// Item is one resource the sweep looked at and acted on or skipped.
type Item struct {
	// Region is the region of the resource, "global" for firewall rules and
	// networks.
	Region  string
	Kind    string
	ID      string
	Cluster string
	Outcome Outcome
	// Reason says why the resource was skipped or what failed.
	Reason string
}

// Report is the result of a sweep.
type Report struct {
	Items []Item
}

// Count returns how many items had the outcome.
func (r *Report) Count(o Outcome) int {
	n := 0
	for _, i := range r.Items {
		if i.Outcome == o {
			n++
		}
	}
	return n
}

// Run sweeps the project. It returns an error for a failure that makes the
// whole sweep meaningless (the region list); failures of single resources are
// items with the outcome Failed.
func Run(ctx context.Context, api gcp.ComputeAPI, project string, o Options) (*Report, error) {
	if o.Cluster == "" && o.ClusterPrefix == "" {
		o.ClusterPrefix = DefaultClusterPrefix
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.WaitDeleted <= 0 {
		o.WaitDeleted = 5 * time.Minute
	}
	s := &sweeper{o: o, api: api, project: project, rep: &Report{}, subnetsGone: map[string]bool{}}
	regions := o.Regions
	if len(regions) == 0 {
		s.allRegions = true
		list, err := pagedList(func(token *string) ([]*computepb.Region, *string, error) {
			out, err := api.ListRegions(ctx, &computepb.ListRegionsRequest{Project: project, PageToken: token})
			if err != nil {
				return nil, nil, err
			}
			return out.GetItems(), out.NextPageToken, nil
		})
		if err != nil {
			return nil, fmt.Errorf("listing the regions of project %s: %w", project, err)
		}
		for _, r := range list {
			regions = append(regions, r.GetName())
		}
		slices.Sort(regions)
	}
	s.regions = regions

	s.instances(ctx)
	for _, r := range regions {
		s.guard(r, kindPolicy, s.policies(ctx, r))
	}
	s.guard(scopeGlobal, kindFirewall, s.firewalls(ctx))
	for _, r := range regions {
		s.guard(r, kindSubnet, s.subnets(ctx, r))
	}
	s.guard(scopeGlobal, kindNetwork, s.networks(ctx))
	return s.rep, nil
}

type sweeper struct {
	o       Options
	api     gcp.ComputeAPI
	project string
	rep     *Report
	regions []string
	// allRegions: the regions come from the project, not from the caller.
	allRegions bool
	// subnetsGone are the subnets deleted (or that would be, in a dry run), by
	// region/name, so that a network can tell whether anything is left in it.
	subnetsGone map[string]bool
}

// ParseRegions turns the value of a -regions flag into Options.Regions: a comma
// separated list (blanks and repeats dropped), or empty / "all" for every region
// of the project (nil). "all" among other names is an error.
func ParseRegions(v string) ([]string, error) {
	var out []string
	all := false
	for r := range strings.SplitSeq(v, ",") {
		r = strings.TrimSpace(r)
		switch {
		case r == "":
		case strings.EqualFold(r, "all"):
			all = true
		case !slices.Contains(out, r):
			out = append(out, r)
		}
	}
	if all && len(out) > 0 {
		return nil, fmt.Errorf("regions: %q cannot be combined with region names", "all")
	}
	return out, nil
}

// guard records the failure of one stage: a region the credentials may not read
// is skipped when the regions were not asked for, anything else fails.
func (s *sweeper) guard(region, kind string, err error) {
	if err == nil {
		return
	}
	if s.allRegions && denied(err) {
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "no access: " + err.Error()})
		return
	}
	s.add(Item{Region: region, Kind: kind, ID: "*", Outcome: Failed, Reason: err.Error()})
}

func (s *sweeper) add(i Item) {
	s.rep.Items = append(s.rep.Items, i)
	line := fmt.Sprintf("%s %s %s %s", i.Region, i.Kind, i.ID, i.Outcome)
	if i.Cluster != "" {
		line += " cluster=" + i.Cluster
	}
	if i.Reason != "" {
		line += ": " + i.Reason
	}
	s.o.Logf("%s", line)
}

// ----- errors -----

func httpStatus(err error) int {
	if ge, ok := errors.AsType[*googleapi.Error](err); ok {
		return ge.Code
	}
	return 0
}

func hasReason(err error, reason string) bool {
	if ge, ok := errors.AsType[*googleapi.Error](err); ok {
		for _, e := range ge.Errors {
			if e.Reason == reason {
				return true
			}
		}
	}
	return false
}

func notFound(err error) bool { return httpStatus(err) == http.StatusNotFound }

func denied(err error) bool {
	c := httpStatus(err)
	return c == http.StatusForbidden || c == http.StatusUnauthorized
}

func inUse(err error) bool { return hasReason(err, "resourceInUseByAnotherResource") }

// ----- selection -----

// matches reports whether the cluster name selects the resource.
func (s *sweeper) matches(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix) && len(cluster) > len(s.o.ClusterPrefix)
}

func (s *sweeper) inRegions(region string) bool {
	return s.allRegions || slices.Contains(s.regions, region)
}

// created parses a creationTimestamp (RFC 3339, with an offset).
func created(ts string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, ts)
	return t, err == nil
}

// createdAtLabel parses the ci-created-at label (Unix seconds).
func createdAtLabel(labels map[string]string) (time.Time, bool) {
	var sec int64
	if v, ok := labels[CreatedAtLabel]; ok {
		if _, err := fmt.Sscanf(v, "%d", &sec); err == nil && sec > 0 {
			return time.Unix(sec, 0), true
		}
	}
	return time.Time{}, false
}

// old decides the age limit: ok is false with the reason when the resource is too
// young or its age is unknown.
func (s *sweeper) old(t time.Time, known bool) (ok bool, reason string) {
	if s.o.MaxAge <= 0 {
		return true, ""
	}
	if !known {
		return false, "no creation time"
	}
	if age := s.o.Now().Sub(t); age < s.o.MaxAge {
		return false, fmt.Sprintf("only %s old (limit %s)", age.Round(time.Second), s.o.MaxAge)
	}
	return true, ""
}

var validRunes = regexp.MustCompile(`^[a-z0-9-]+$`)

// owned decides whether a label-less resource belongs to a selected cluster: the
// name prefix and the description marker must agree. cluster is "" when the
// resource is none of the sweep's business (another cluster, no marker, not
// ours), reason explains a resource of a selected cluster that is not swept.
func (s *sweeper) owned(name, desc string) (cluster, reason string, ok bool) {
	if strings.HasPrefix(name, ByoNamePrefix) {
		c, found := ParseLaneMarker(desc)
		if !found || !s.matches(c) {
			return "", "", false
		}
		return c, "", true
	}
	c, _, _, found := gcp.ParseMarker(desc)
	if !found || !s.matches(c) || !validRunes.MatchString(c) {
		return "", "", false
	}
	if !strings.HasPrefix(name, gcp.ResourceNamePrefix(c)) {
		return c, "the name does not start with " + gcp.ResourceNamePrefix(c), false
	}
	return c, "", true
}

// ----- operations -----

// del runs one deletion, waits for its operation and records the outcome. It
// returns true when the resource is gone (or would be). A resource that is still
// in use by another one is retried until WaitDeleted passes.
func (s *sweeper) del(
	ctx context.Context, region, kind, id, cluster string, fn func() (*computepb.Operation, error),
) bool {
	it := Item{Region: region, Kind: kind, ID: id, Cluster: cluster}
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.add(it)
		return true
	}
	deadline := time.Now().Add(s.o.WaitDeleted)
	for {
		op, err := fn()
		if err == nil {
			err = s.wait(ctx, op, deadline)
		}
		switch {
		case err == nil || notFound(err):
			it.Outcome = Deleted
			s.add(it)
			return true
		case inUse(err) && time.Now().Before(deadline) && ctx.Err() == nil:
			select {
			case <-ctx.Done():
			case <-time.After(s.o.PollInterval):
			}
		default:
			it.Outcome, it.Reason = Failed, err.Error()
			s.add(it)
			return false
		}
	}
}

// wait waits for an operation to be DONE (the server long-polls and may return it
// still RUNNING), up to the deadline, and turns an operation error into an error.
func (s *sweeper) wait(ctx context.Context, op *computepb.Operation, deadline time.Time) error {
	for op != nil && op.GetStatus() != computepb.Operation_DONE {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("operation %s still %s after %s", op.GetName(), op.GetStatus(), s.o.WaitDeleted)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		switch {
		case op.GetZone() != "":
			op, err = s.api.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{
				Project: s.project, Zone: last(op.GetZone()), Operation: op.GetName()})
		case op.GetRegion() != "":
			op, err = s.api.WaitRegionOperation(ctx, &computepb.WaitRegionOperationRequest{
				Project: s.project, Region: last(op.GetRegion()), Operation: op.GetName()})
		default:
			op, err = s.api.WaitGlobalOperation(ctx, &computepb.WaitGlobalOperationRequest{
				Project: s.project, Operation: op.GetName()})
		}
		if err != nil {
			return err
		}
	}
	if op == nil {
		return nil
	}
	if errs := op.GetError().GetErrors(); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.GetCode()+": "+e.GetMessage())
		}
		return fmt.Errorf("operation %s failed: %s", op.GetName(), strings.Join(msgs, "; "))
	}
	return nil
}

func last(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }

// pagedList collects the items of a paginated call.
func pagedList[T any](next func(token *string) ([]T, *string, error)) ([]T, error) {
	var out []T
	var token *string
	for {
		items, nt, err := next(token)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if nt == nil || *nt == "" {
			return out, nil
		}
		token = nt
	}
}

// ----- instances -----

// instances deletes the selected instances (all states: a stopped one still
// costs its disks) and waits for the deletions, so that the policies, subnets
// and networks they used can go next. All deletions are issued first, the
// operations run server side concurrently.
func (s *sweeper) instances(ctx context.Context) {
	all, err := pagedList(func(token *string) ([]*computepb.Instance, *string, error) {
		// No server-side label filter: the project of the lane is dedicated, the
		// real check is client-side, and a filter dialect that silently matched
		// nothing would make the sweeper a no-op.
		out, err := s.api.AggregatedListInstances(ctx, &computepb.AggregatedListInstancesRequest{
			Project: s.project, PageToken: token})
		if err != nil {
			return nil, nil, err
		}
		var items []*computepb.Instance
		for _, scoped := range out.GetItems() {
			items = append(items, scoped.GetInstances()...)
		}
		return items, out.NextPageToken, nil
	})
	if err != nil {
		s.guard(scopeGlobal, kindInstance, fmt.Errorf("listing instances: %w", err))
		return
	}
	type pending struct {
		it   Item
		op   *computepb.Operation
		zone string
	}
	var todo []pending
	for _, in := range all {
		zone, name := last(in.GetZone()), in.GetName()
		region := gcp.RegionOfZone(zone)
		cluster := in.GetLabels()[ClusterLabel]
		if !s.matches(cluster) || !s.inRegions(region) {
			continue
		}
		id := zone + "/" + name
		t, known := created(in.GetCreationTimestamp())
		if !known {
			t, known = createdAtLabel(in.GetLabels())
		}
		if ok, reason := s.old(t, known); !ok {
			s.add(Item{Region: region, Kind: kindInstance, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
			continue
		}
		it := Item{Region: region, Kind: kindInstance, ID: id, Cluster: cluster}
		if s.o.DryRun {
			it.Outcome = WouldDelete
			s.add(it)
			continue
		}
		op, err := s.api.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: s.project, Zone: zone, Instance: name})
		switch {
		case notFound(err):
			it.Outcome = Deleted
			s.add(it)
		case err != nil:
			it.Outcome, it.Reason = Failed, err.Error()
			s.add(it)
		default:
			todo = append(todo, pending{it: it, op: op, zone: zone})
		}
	}
	deadline := time.Now().Add(s.o.WaitDeleted)
	for _, p := range todo {
		if err := s.wait(ctx, p.op, deadline); err != nil && !notFound(err) {
			p.it.Outcome, p.it.Reason = Failed, err.Error()
		} else {
			p.it.Outcome = Deleted
		}
		s.add(p.it)
	}
}

// ----- resource policies, firewalls, subnetworks, networks -----

func (s *sweeper) policies(ctx context.Context, region string) error {
	list, err := pagedList(func(token *string) ([]*computepb.ResourcePolicy, *string, error) {
		out, err := s.api.ListResourcePolicies(ctx, &computepb.ListResourcePoliciesRequest{
			Project: s.project, Region: region, PageToken: token})
		if err != nil {
			return nil, nil, err
		}
		return out.GetItems(), out.NextPageToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing resource policies: %w", err)
	}
	for _, p := range list {
		name := p.GetName()
		cluster, reason, ok := s.owned(name, p.GetDescription())
		if !ok {
			s.skipOwned(region, kindPolicy, name, cluster, reason)
			continue
		}
		t, known := created(p.GetCreationTimestamp())
		if young, why := s.old(t, known); !young {
			s.add(Item{Region: region, Kind: kindPolicy, ID: name, Cluster: cluster, Outcome: Skipped, Reason: why})
			continue
		}
		s.del(ctx, region, kindPolicy, name, cluster, func() (*computepb.Operation, error) {
			return s.api.DeleteResourcePolicy(ctx, &computepb.DeleteResourcePolicyRequest{
				Project: s.project, Region: region, ResourcePolicy: name})
		})
	}
	return nil
}

// skipOwned records a resource of a selected cluster that is not swept for a reason
// (cluster is "" for resources that are none of the sweep's business).
func (s *sweeper) skipOwned(region, kind, id, cluster, reason string) {
	if cluster != "" && reason != "" {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
	}
}

func (s *sweeper) firewalls(ctx context.Context) error {
	list, err := pagedList(func(token *string) ([]*computepb.Firewall, *string, error) {
		out, err := s.api.ListFirewalls(ctx, &computepb.ListFirewallsRequest{Project: s.project, PageToken: token})
		if err != nil {
			return nil, nil, err
		}
		return out.GetItems(), out.NextPageToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing firewall rules: %w", err)
	}
	for _, f := range list {
		name := f.GetName()
		cluster, reason, ok := s.owned(name, f.GetDescription())
		if !ok {
			s.skipOwned(scopeGlobal, kindFirewall, name, cluster, reason)
			continue
		}
		t, known := created(f.GetCreationTimestamp())
		if young, why := s.old(t, known); !young {
			s.add(Item{Region: scopeGlobal, Kind: kindFirewall, ID: name, Cluster: cluster, Outcome: Skipped, Reason: why})
			continue
		}
		s.del(ctx, scopeGlobal, kindFirewall, name, cluster, func() (*computepb.Operation, error) {
			return s.api.DeleteFirewall(ctx, &computepb.DeleteFirewallRequest{Project: s.project, Firewall: name})
		})
	}
	return nil
}

func (s *sweeper) subnets(ctx context.Context, region string) error {
	list, err := pagedList(func(token *string) ([]*computepb.Subnetwork, *string, error) {
		out, err := s.api.ListSubnetworks(ctx, &computepb.ListSubnetworksRequest{
			Project: s.project, Region: region, PageToken: token})
		if err != nil {
			return nil, nil, err
		}
		return out.GetItems(), out.NextPageToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing subnetworks: %w", err)
	}
	for _, sn := range list {
		name := sn.GetName()
		cluster, reason, ok := s.owned(name, sn.GetDescription())
		if !ok {
			s.skipOwned(region, kindSubnet, name, cluster, reason)
			continue
		}
		t, known := created(sn.GetCreationTimestamp())
		if young, why := s.old(t, known); !young {
			s.add(Item{Region: region, Kind: kindSubnet, ID: name, Cluster: cluster, Outcome: Skipped, Reason: why})
			continue
		}
		if s.del(ctx, region, kindSubnet, name, cluster, func() (*computepb.Operation, error) {
			return s.api.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{
				Project: s.project, Region: region, Subnetwork: name})
		}) {
			s.subnetsGone[region+"/"+name] = true
		}
	}
	return nil
}

// subnetKey turns a subnetwork URL into region/name.
func subnetKey(url string) string {
	name := last(url)
	rest := strings.TrimSuffix(url, "/subnetworks/"+name)
	return last(rest) + "/" + name
}

func (s *sweeper) networks(ctx context.Context) error {
	list, err := pagedList(func(token *string) ([]*computepb.Network, *string, error) {
		out, err := s.api.ListNetworks(ctx, &computepb.ListNetworksRequest{Project: s.project, PageToken: token})
		if err != nil {
			return nil, nil, err
		}
		return out.GetItems(), out.NextPageToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing networks: %w", err)
	}
	for _, n := range list {
		name := n.GetName()
		cluster, reason, ok := s.owned(name, n.GetDescription())
		if !ok {
			s.skipOwned(scopeGlobal, kindNetwork, name, cluster, reason)
			continue
		}
		if name == defaultNet {
			s.add(Item{Region: scopeGlobal, Kind: kindNetwork, ID: name, Cluster: cluster, Outcome: Skipped,
				Reason: "the default network is never deleted"})
			continue
		}
		t, known := created(n.GetCreationTimestamp())
		if young, why := s.old(t, known); !young {
			s.add(Item{Region: scopeGlobal, Kind: kindNetwork, ID: name, Cluster: cluster, Outcome: Skipped, Reason: why})
			continue
		}
		// A network is deleted when nothing is left in it. Subnets of another region
		// than the swept ones (or of no sweep's business) keep it alive.
		var left []string
		for _, u := range n.GetSubnetworks() {
			if !s.subnetsGone[subnetKey(u)] {
				left = append(left, subnetKey(u))
			}
		}
		if len(left) > 0 {
			s.add(Item{Region: scopeGlobal, Kind: kindNetwork, ID: name, Cluster: cluster, Outcome: Skipped,
				Reason: "subnets are left in it: " + strings.Join(left, ", ")})
			continue
		}
		s.del(ctx, scopeGlobal, kindNetwork, name, cluster, func() (*computepb.Operation, error) {
			return s.api.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: s.project, Network: name})
		})
	}
	return nil
}
