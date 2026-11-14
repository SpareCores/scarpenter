/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in the
// compartment of an OCI tenancy (docs/how-to-guides/testing.md#the-l5-lane-on-oci):
// instances, network security groups, subnets, route tables, internet gateways,
// security lists and VCNs, in that dependency order, and, when asked, unattached boot
// volumes. It is the sweeper workflow's engine (every three hours, everything of a
// cluster named scarpenter-ci-* older than two hours) and the e2e workflow's last line
// of defence (one cluster, no age limit).
//
// It talks to OCI through the driver's narrow API interfaces (oci.Clients), so the
// in-memory fake of the driver tests runs it unchanged; the command wraps the SDK
// adapter, which puts a deadline on every call. Selection is conservative on purpose
// and follows the two kinds of ownership of the lane (plan 25, sections 4 and 11):
//
//   - instances carry the freeform tag scarpenter-cluster=<cluster>;
//   - what SCarpenter manages (VCN, gateway, route table, security list, subnet,
//     network security group) carries scarpenter-cluster=<cluster> AND
//     scarpenter-managed-by=scarpenter, and a display name that starts with
//     scarpenter-<cluster>-: all three are required;
//   - what the lane's workflow brings itself (the byo scenario) is named
//     scarpenter-e2e-byo-* and tagged e2e-byo-cluster=<cluster>, a tag the controller's
//     garbage collector does not recognize, so that SCarpenter never touches it.
//
// A resource without one of these is never touched, an imported image included (images
// are not even listed). The compartment of the sweep is the controller's. With an age
// limit a resource is touched only when old enough, by its creation time. A VCN's own
// default route table and security list are untagged and go with the VCN. A boot volume
// goes with its instance (the terminate does not preserve it); one that stays is swept
// when asked (BootVolumes) by the same rule as the instances: the cluster tag, which it
// carries only when OCI copied the instance's tags to it at launch, and the age. An
// untagged boot volume is never touched: the teardown reports it.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

const (
	// ClusterTag is the freeform tag of every instance and managed resource with the
	// cluster name.
	ClusterTag = oci.TagPrefix + string(vendors.TagCluster)
	// ManagedByTag is the freeform tag that says SCarpenter owns a managed resource.
	ManagedByTag = oci.TagPrefix + string(vendors.TagManagedBy)
	// CreatedAtTag is the freeform tag the lane's NodeClasses put on every instance
	// (spec.tags, Unix seconds); the sweeper reads it only when an instance has no
	// creation time.
	CreatedAtTag = "ci-created-at"
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"
	// ByoNamePrefix is the name prefix of the resources the lane's workflow creates for
	// the bring-your-own scenario.
	ByoNamePrefix = "scarpenter-e2e-byo-"
	// LaneTag is the freeform tag of those resources: the cluster of the run.
	LaneTag = "e2e-byo-cluster"

	// The kinds of the report.
	kindRegion   = "region"
	kindInstance = "instance"
	kindGroup    = "network-security-group"
	kindSubnet   = "subnet"
	kindRoute    = "route-table"
	kindGateway  = "internet-gateway"
	kindList     = "security-list"
	kindVCN      = "vcn"
	kindVolume   = "boot-volume"
)

// ResourceNamePrefix is the display name prefix of the resources SCarpenter manages
// for a cluster.
func ResourceNamePrefix(cluster string) string { return oci.TagPrefix + cluster + "-" }

// VolumesAPI is the part of the Block Volume service the sweep needs. The driver has no
// block storage calls: the command implements it over the SDK.
type VolumesAPI interface {
	ListBootVolumes(ctx context.Context, region string, req core.ListBootVolumesRequest,
	) (core.ListBootVolumesResponse, error)
	DeleteBootVolume(ctx context.Context, region string, req core.DeleteBootVolumeRequest,
	) (core.DeleteBootVolumeResponse, error)
}

// Options select what is swept.
type Options struct {
	// Compartment is the compartment of the lane; Tenancy the tenancy (the region
	// subscriptions and the availability domains of the boot volume sweep). Both are
	// required.
	Compartment string
	Tenancy     string
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
	// Regions limits the sweep to these regions; empty means every subscribed region
	// (see ParseRegions). Only in that all-regions mode is a region the credentials may
	// not read skipped; a requested region that cannot be read is a failure.
	Regions []string
	// BootVolumes also sweeps the available boot volumes whose freeform tag
	// scarpenter-cluster selects them and that are old enough, as the instances (never an
	// untagged one). It needs the volumes client.
	BootVolumes bool
	// WaitDeleted bounds the wait for an instance to be terminated and for a resource
	// that is still in use by another one to be released (default 5 minutes).
	WaitDeleted time.Duration
	// PollInterval is the pause between retries (default 5 s).
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

// Run sweeps the compartment. It returns an error for a failure that makes the whole
// sweep meaningless (the region list, a missing option); failures of single resources
// are items with the outcome Failed. volumes may be nil unless Options.BootVolumes.
func Run(ctx context.Context, api oci.Clients, volumes VolumesAPI, o Options) (*Report, error) {
	if o.Compartment == "" || o.Tenancy == "" {
		return nil, errors.New("the compartment and the tenancy are required")
	}
	if o.BootVolumes && volumes == nil {
		return nil, errors.New("sweeping boot volumes needs the block volume client")
	}
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
	s := &sweeper{o: o, api: api, vol: volumes, rep: &Report{}}
	regions := o.Regions
	if len(regions) == 0 {
		s.allRegions = true
		res, err := api.Identity.ListRegionSubscriptions(ctx, identity.ListRegionSubscriptionsRequest{TenancyId: &o.Tenancy})
		if err != nil {
			return nil, fmt.Errorf("listing the region subscriptions: %w", err)
		}
		for _, r := range res.Items {
			if r.Status == identity.RegionSubscriptionStatusReady && r.RegionName != nil {
				regions = append(regions, *r.RegionName)
			}
		}
		slices.Sort(regions)
	}
	for _, r := range regions {
		s.guard(r, s.region(ctx, r))
	}
	return s.rep, nil
}

type sweeper struct {
	o   Options
	api oci.Clients
	vol VolumesAPI
	rep *Report
	// allRegions: the regions come from the subscriptions, not from the caller.
	allRegions bool
}

// ParseRegions turns the value of a -regions flag into Options.Regions: a comma
// separated list (blanks and repeats dropped), or empty / "all" for every subscribed
// region (nil). "all" among other names is an error.
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

// guard records the failure of a region: one the credentials may not read is skipped
// when the regions were not asked for, anything else fails.
func (s *sweeper) guard(region string, err error) {
	if err == nil {
		return
	}
	if s.allRegions && denied(err) {
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "no access: " + err.Error()})
		return
	}
	s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Failed, Reason: err.Error()})
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
	var se common.ServiceError
	if errors.As(err, &se) {
		return se.GetHTTPStatusCode()
	}
	return 0
}

func notFound(err error) bool { return httpStatus(err) == http.StatusNotFound }

func denied(err error) bool {
	c := httpStatus(err)
	return c == http.StatusForbidden || c == http.StatusUnauthorized
}

// inUse: a 409 is OCI's answer to a delete of a resource another one still uses.
func inUse(err error) bool { return httpStatus(err) == http.StatusConflict }

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

// old decides the age limit: ok is false with the reason when the resource is too young
// or its age is unknown.
func (s *sweeper) old(t *common.SDKTime) (ok bool, reason string) {
	if s.o.MaxAge <= 0 {
		return true, ""
	}
	if t == nil || t.IsZero() {
		return false, "no creation time"
	}
	if age := s.o.Now().Sub(t.Time); age < s.o.MaxAge {
		return false, fmt.Sprintf("only %s old (limit %s)", age.Round(time.Second), s.o.MaxAge)
	}
	return true, ""
}

// owned decides whether a resource belongs to a selected cluster: the tags and the name
// must agree. cluster is "" when the resource is none of the sweep's business (another
// cluster, no marker, not ours); reason explains a resource of a selected cluster that
// is not swept.
func (s *sweeper) owned(name string, tags map[string]string) (cluster, reason string, ok bool) {
	if strings.HasPrefix(name, ByoNamePrefix) {
		c := tags[LaneTag]
		if !s.matches(c) {
			return "", "", false
		}
		return c, "", true
	}
	c := tags[ClusterTag]
	if !s.matches(c) {
		return "", "", false
	}
	switch {
	case tags[ManagedByTag] != vendors.ManagedByValue:
		return c, "no " + ManagedByTag + "=" + vendors.ManagedByValue + " tag", false
	case !strings.HasPrefix(name, ResourceNamePrefix(c)):
		return c, "the name does not start with " + ResourceNamePrefix(c), false
	}
	return c, "", true
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// pagedList collects the items of a paginated call.
func pagedList[T any](next func(page *string) ([]T, *string, error)) ([]T, error) {
	var out []T
	var page *string
	for {
		items, np, err := next(page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if np == nil || *np == "" {
			return out, nil
		}
		page = np
	}
}

// del runs one deletion and records the outcome. A resource that is still in use by another
// one is retried until WaitDeleted passes.
func (s *sweeper) del(ctx context.Context, region, kind, id, cluster string, fn func() error) {
	it := Item{Region: region, Kind: kind, ID: id, Cluster: cluster}
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.add(it)
		return
	}
	deadline := time.Now().Add(s.o.WaitDeleted)
	for {
		err := fn()
		switch {
		case err == nil || notFound(err):
			it.Outcome = Deleted
			s.add(it)
			return
		case inUse(err) && time.Now().Before(deadline) && ctx.Err() == nil:
			select {
			case <-ctx.Done():
			case <-time.After(s.o.PollInterval):
			}
		default:
			it.Outcome, it.Reason = Failed, err.Error()
			s.add(it)
			return
		}
	}
}

// skip records a resource of a selected cluster that is not swept for a reason (cluster
// is "" for resources that are none of the sweep's business).
func (s *sweeper) skip(region, kind, id, cluster, reason string) {
	if cluster != "" && reason != "" {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
	}
}

// pick runs the common part of the network kinds: owned, old enough; it returns whether
// the resource is to be deleted, and records the skips.
func (s *sweeper) pick(region, kind, id, name string, tags map[string]string, created *common.SDKTime,
) (cluster string, ok bool) {
	cluster, reason, ok := s.owned(name, tags)
	if !ok {
		s.skip(region, kind, id, cluster, reason)
		return cluster, false
	}
	if young, why := s.old(created); !young {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Skipped, Reason: why})
		return cluster, false
	}
	return cluster, true
}

// ----- one region -----

// region sweeps one region in the dependency order: the instances (terminated, so that
// their VNICs are gone), the groups, the subnets, the route tables, the gateways, the
// security lists, the VCNs, and the boot volumes. A failure of one stage is returned
// after the later ones ran (they delete what they can).
func (s *sweeper) region(ctx context.Context, region string) error {
	stages := []func(context.Context, string) error{
		s.instances, s.groups, s.subnets, s.routeTables, s.gateways, s.securityLists, s.vcns,
	}
	if s.o.BootVolumes {
		stages = append(stages, s.bootVolumes)
	}
	var errs []error
	for _, st := range stages {
		if err := st(ctx, region); err != nil {
			errs = append(errs, err)
			if denied(err) {
				break // the next stages would be refused too
			}
		}
	}
	return errors.Join(errs...)
}

// ----- instances -----

func (s *sweeper) instances(ctx context.Context, region string) error {
	list, err := pagedList(func(page *string) ([]core.Instance, *string, error) {
		// No server-side tag filter exists: the compartment of the lane is dedicated and
		// the real check is client-side.
		res, err := s.api.Compute.ListInstances(ctx, region, core.ListInstancesRequest{
			CompartmentId: &s.o.Compartment, Page: page})
		return res.Items, res.OpcNextPage, err
	})
	if err != nil {
		return fmt.Errorf("listing instances: %w", err)
	}
	var todo []Item
	for _, in := range list {
		id := deref(in.Id)
		cluster := in.FreeformTags[ClusterTag]
		if !s.matches(cluster) || in.LifecycleState == core.InstanceLifecycleStateTerminated {
			continue
		}
		created := in.TimeCreated
		if created == nil {
			created = createdAtTag(in.FreeformTags)
		}
		if ok, reason := s.old(created); !ok {
			s.add(Item{Region: region, Kind: kindInstance, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
			continue
		}
		it := Item{Region: region, Kind: kindInstance, ID: id, Cluster: cluster}
		if s.o.DryRun {
			it.Outcome = WouldDelete
			s.add(it)
			continue
		}
		if in.LifecycleState != core.InstanceLifecycleStateTerminating {
			_, err := s.api.Compute.TerminateInstance(ctx, region, core.TerminateInstanceRequest{
				InstanceId: in.Id, PreserveBootVolume: new(false), PreserveDataVolumesCreatedAtLaunch: new(false)})
			if err != nil && !notFound(err) {
				it.Outcome, it.Reason = Failed, err.Error()
				s.add(it)
				continue
			}
		}
		todo = append(todo, it)
	}
	// All terminations are issued first: they run server side concurrently. The groups,
	// subnets and VCNs of the next stages need the VNICs gone, so wait for TERMINATED.
	deadline := time.Now().Add(s.o.WaitDeleted)
	for _, it := range todo {
		if err := s.terminated(ctx, region, it.ID, deadline); err != nil {
			it.Outcome, it.Reason = Failed, err.Error()
		} else {
			it.Outcome = Deleted
		}
		s.add(it)
	}
	return nil
}

// terminated waits for the instance to reach TERMINATED (or to be gone).
func (s *sweeper) terminated(ctx context.Context, region, id string, deadline time.Time) error {
	for {
		res, err := s.api.Compute.GetInstance(ctx, region, core.GetInstanceRequest{InstanceId: &id})
		switch {
		case notFound(err), err == nil && res.LifecycleState == core.InstanceLifecycleStateTerminated:
			return nil
		case err != nil:
			return err
		case !time.Now().Before(deadline):
			return fmt.Errorf("still %s after %s", res.LifecycleState, s.o.WaitDeleted)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.o.PollInterval):
		}
	}
}

// createdAtTag reads the ci-created-at tag (Unix seconds).
func createdAtTag(tags map[string]string) *common.SDKTime {
	var sec int64
	if v, ok := tags[CreatedAtTag]; ok {
		if _, err := fmt.Sscanf(v, "%d", &sec); err == nil && sec > 0 {
			return &common.SDKTime{Time: time.Unix(sec, 0)}
		}
	}
	return nil
}

// ----- the network -----

// resource is what the network kinds have in common for the sweep.
type resource struct {
	id, name   string
	tags       map[string]string
	created    *common.SDKTime
	terminated bool
}

// sweepKind lists the resources of one kind and deletes the ones that are owned and old enough.
func sweepKind[T any](ctx context.Context, s *sweeper, region, kind string,
	list func(page *string) ([]T, *string, error), view func(T) resource, del func(id string) error,
) error {
	items, err := pagedList(list)
	if err != nil {
		return fmt.Errorf("listing %ss: %w", kind, err)
	}
	for _, it := range items {
		r := view(it)
		if r.terminated {
			continue
		}
		cluster, ok := s.pick(region, kind, r.id, r.name, r.tags, r.created)
		if !ok {
			continue
		}
		s.del(ctx, region, kind, r.id, cluster, func() error { return del(r.id) })
	}
	return nil
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (s *sweeper) groups(ctx context.Context, region string) error {
	return sweepKind(ctx, s, region, kindGroup,
		func(page *string) ([]core.NetworkSecurityGroup, *string, error) {
			res, err := s.api.Network.ListNetworkSecurityGroups(ctx, region, core.ListNetworkSecurityGroupsRequest{
				CompartmentId: &s.o.Compartment, Page: page})
			return res.Items, res.OpcNextPage, err
		},
		func(g core.NetworkSecurityGroup) resource {
			return resource{deref(g.Id), deref(g.DisplayName), g.FreeformTags, g.TimeCreated,
				g.LifecycleState == core.NetworkSecurityGroupLifecycleStateTerminated}
		},
		func(id string) error {
			_, err := s.api.Network.DeleteNetworkSecurityGroup(ctx, region, core.DeleteNetworkSecurityGroupRequest{
				NetworkSecurityGroupId: &id})
			return err
		})
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (s *sweeper) subnets(ctx context.Context, region string) error {
	return sweepKind(ctx, s, region, kindSubnet,
		func(page *string) ([]core.Subnet, *string, error) {
			res, err := s.api.Network.ListSubnets(ctx, region, core.ListSubnetsRequest{
				CompartmentId: &s.o.Compartment, Page: page})
			return res.Items, res.OpcNextPage, err
		},
		func(sn core.Subnet) resource {
			return resource{deref(sn.Id), deref(sn.DisplayName), sn.FreeformTags, sn.TimeCreated,
				sn.LifecycleState == core.SubnetLifecycleStateTerminated}
		},
		func(id string) error {
			_, err := s.api.Network.DeleteSubnet(ctx, region, core.DeleteSubnetRequest{SubnetId: &id})
			return err
		})
}

func (s *sweeper) allSubnets(ctx context.Context, region string) ([]core.Subnet, error) {
	list, err := pagedList(func(page *string) ([]core.Subnet, *string, error) {
		res, err := s.api.Network.ListSubnets(ctx, region, core.ListSubnetsRequest{
			CompartmentId: &s.o.Compartment, Page: page})
		return res.Items, res.OpcNextPage, err
	})
	if err != nil {
		return nil, fmt.Errorf("listing subnets: %w", err)
	}
	return list, nil
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (s *sweeper) routeTables(ctx context.Context, region string) error {
	return sweepKind(ctx, s, region, kindRoute,
		func(page *string) ([]core.RouteTable, *string, error) {
			res, err := s.api.Network.ListRouteTables(ctx, region, core.ListRouteTablesRequest{
				CompartmentId: &s.o.Compartment, Page: page})
			return res.Items, res.OpcNextPage, err
		},
		func(r core.RouteTable) resource {
			return resource{deref(r.Id), deref(r.DisplayName), r.FreeformTags, r.TimeCreated,
				r.LifecycleState == core.RouteTableLifecycleStateTerminated}
		},
		func(id string) error {
			_, err := s.api.Network.DeleteRouteTable(ctx, region, core.DeleteRouteTableRequest{RtId: &id})
			return err
		})
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (s *sweeper) gateways(ctx context.Context, region string) error {
	return sweepKind(ctx, s, region, kindGateway,
		func(page *string) ([]core.InternetGateway, *string, error) {
			res, err := s.api.Network.ListInternetGateways(ctx, region, core.ListInternetGatewaysRequest{
				CompartmentId: &s.o.Compartment, Page: page})
			return res.Items, res.OpcNextPage, err
		},
		func(g core.InternetGateway) resource {
			return resource{deref(g.Id), deref(g.DisplayName), g.FreeformTags, g.TimeCreated,
				g.LifecycleState == core.InternetGatewayLifecycleStateTerminated}
		},
		func(id string) error {
			_, err := s.api.Network.DeleteInternetGateway(ctx, region, core.DeleteInternetGatewayRequest{IgId: &id})
			return err
		})
}

//nolint:dupl // the same listing of a regional kind, over another SDK type
func (s *sweeper) securityLists(ctx context.Context, region string) error {
	return sweepKind(ctx, s, region, kindList,
		func(page *string) ([]core.SecurityList, *string, error) {
			res, err := s.api.Network.ListSecurityLists(ctx, region, core.ListSecurityListsRequest{
				CompartmentId: &s.o.Compartment, Page: page})
			return res.Items, res.OpcNextPage, err
		},
		func(l core.SecurityList) resource {
			return resource{deref(l.Id), deref(l.DisplayName), l.FreeformTags, l.TimeCreated,
				l.LifecycleState == core.SecurityListLifecycleStateTerminated}
		},
		func(id string) error {
			_, err := s.api.Network.DeleteSecurityList(ctx, region, core.DeleteSecurityListRequest{SecurityListId: &id})
			return err
		})
}

// vcns deletes the selected VCNs that have no subnet left. A subnet the sweep does not
// select (one of a user, or one that is too young) keeps the VCN alive; the selected
// ones are deleted by the stage before, and OCI's asynchronous termination of them is
// waited for by the retry of the VCN's own deletion (a 409 until they are gone).
func (s *sweeper) vcns(ctx context.Context, region string) error {
	list, err := pagedList(func(page *string) ([]core.Vcn, *string, error) {
		res, err := s.api.Network.ListVcns(ctx, region, core.ListVcnsRequest{CompartmentId: &s.o.Compartment, Page: page})
		return res.Items, res.OpcNextPage, err
	})
	if err != nil {
		return fmt.Errorf("listing VCNs: %w", err)
	}
	var subs []core.Subnet
	subsRead := false
	for _, v := range list {
		if v.LifecycleState == core.VcnLifecycleStateTerminated {
			continue
		}
		id := deref(v.Id)
		cluster, ok := s.pick(region, kindVCN, id, deref(v.DisplayName), v.FreeformTags, v.TimeCreated)
		if !ok {
			continue
		}
		if !subsRead {
			if subs, err = s.allSubnets(ctx, region); err != nil {
				return err
			}
			subsRead = true
		}
		var left []string
		for _, sn := range subs {
			if deref(sn.VcnId) == id && sn.LifecycleState != core.SubnetLifecycleStateTerminated && !s.selected(sn) {
				left = append(left, deref(sn.Id))
			}
		}
		if len(left) > 0 {
			s.add(Item{Region: region, Kind: kindVCN, ID: id, Cluster: cluster, Outcome: Skipped,
				Reason: "subnets are left in it: " + strings.Join(left, " ")})
			continue
		}
		s.del(ctx, region, kindVCN, id, cluster, func() error {
			_, err := s.api.Network.DeleteVcn(ctx, region, core.DeleteVcnRequest{VcnId: &id})
			return err
		})
	}
	return nil
}

// selected says whether the subnet stage deletes the subnet.
func (s *sweeper) selected(sn core.Subnet) bool {
	if _, _, ok := s.owned(deref(sn.DisplayName), sn.FreeformTags); !ok {
		return false
	}
	old, _ := s.old(sn.TimeCreated)
	return old
}

// ----- boot volumes -----

func (s *sweeper) bootVolumes(ctx context.Context, region string) error {
	ads, err := s.api.Identity.ListAvailabilityDomains(ctx, region, identity.ListAvailabilityDomainsRequest{
		CompartmentId: &s.o.Tenancy})
	if err != nil {
		return fmt.Errorf("listing the availability domains: %w", err)
	}
	for _, ad := range ads.Items {
		list, err := pagedList(func(page *string) ([]core.BootVolume, *string, error) {
			res, err := s.vol.ListBootVolumes(ctx, region, core.ListBootVolumesRequest{
				CompartmentId: &s.o.Compartment, AvailabilityDomain: ad.Name, Page: page})
			return res.Items, res.OpcNextPage, err
		})
		if err != nil {
			return fmt.Errorf("listing boot volumes: %w", err)
		}
		for _, v := range list {
			if v.LifecycleState != core.BootVolumeLifecycleStateAvailable {
				continue
			}
			id := deref(v.Id)
			cluster := v.FreeformTags[ClusterTag]
			if !s.matches(cluster) {
				continue // untagged, or another cluster's
			}
			if young, why := s.old(v.TimeCreated); !young {
				s.add(Item{Region: region, Kind: kindVolume, ID: id, Cluster: cluster, Outcome: Skipped, Reason: why})
				continue
			}
			s.delVolume(ctx, region, id, cluster)
		}
	}
	return nil
}

// delVolume deletes one boot volume; the 409 of a volume that is still attached is a
// skip, not a failure (the volume of a running instance is not the sweep's).
func (s *sweeper) delVolume(ctx context.Context, region, id, cluster string) {
	it := Item{Region: region, Kind: kindVolume, ID: id, Cluster: cluster}
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.add(it)
		return
	}
	_, err := s.vol.DeleteBootVolume(ctx, region, core.DeleteBootVolumeRequest{BootVolumeId: &id})
	switch {
	case err == nil || notFound(err):
		it.Outcome = Deleted
	case inUse(err):
		it.Outcome, it.Reason = Skipped, "attached or in use: "+err.Error()
	default:
		it.Outcome, it.Reason = Failed, err.Error()
	}
	s.add(it)
}
