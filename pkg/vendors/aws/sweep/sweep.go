/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in an
// AWS account (docs/how-to-guides/testing.md#the-l5-lane): instances, placement
// groups, security groups, and the managed network (subnets, route tables,
// internet gateways, VPCs), selected by the cluster tag
// scarpenter.sparecores.com/cluster. It is the sweeper workflow's engine (every
// three hours, everything of a cluster named scarpenter-ci-* older than two
// hours) and the e2e workflow's last line of defence (one cluster, no age limit).
//
// It talks to EC2 through the driver's narrow API (pkg/vendors/aws.EC2API), so
// the in-memory fake of the driver tests runs it unchanged.
//
// Selection is conservative on purpose. A resource is touched only when its
// cluster tag matches (exactly, or by prefix) and, with an age limit, when it
// is old enough: instances by their launch time, everything else by the tag
// ci-created-at (Unix seconds), which the lane's NodeClasses and the workflow
// put on every resource through spec.tags. A resource without the tag has no
// known age and is left alone, and so are default VPCs, default security
// groups and main route tables, whatever their tags.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

const (
	// ClusterTag is the tag every SCarpenter resource carries with the cluster
	// name (vendors.DefaultTagKey(vendors.TagCluster)).
	ClusterTag = "scarpenter.sparecores.com/cluster"
	// CreatedAtTag holds the creation time of a resource without a creation time
	// of its own, in Unix seconds (RFC 3339 is accepted too).
	CreatedAtTag = "ci-created-at"
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"

	kindRegion = "region"
)

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
	// Regions limits the sweep to these regions; empty means every enabled region
	// of the account (see ParseRegions). Only in that all-regions mode is a region
	// whose endpoint cannot be reached skipped; a requested region that cannot be
	// reached is a failure.
	Regions []string
	// WaitTerminated bounds the wait, per region, for terminated instances to
	// disappear before the groups and the network they used are deleted.
	WaitTerminated time.Duration
	// PollInterval is the pause between those polls (default 10 s).
	PollInterval time.Duration
	// Now is the clock (default time.Now).
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

// Run sweeps the regions. It returns an error for a failure that makes the
// whole sweep meaningless (no clients, no region list); failures of single
// resources are items with the outcome Failed.
func Run(ctx context.Context, factory awsdriver.ClientFactory, home string, o Options) (*Report, error) {
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
		o.PollInterval = 10 * time.Second
	}
	s := &sweeper{o: o, rep: &Report{}}
	regions := o.Regions
	if len(regions) == 0 {
		s.allRegions = true
		cl, err := factory(ctx, home)
		if err != nil {
			return nil, fmt.Errorf("creating the clients of the home region %s: %w", home, err)
		}
		out, err := cl.EC2.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
		if err != nil {
			return nil, fmt.Errorf("listing the enabled regions: %w", err)
		}
		for _, r := range out.Regions {
			regions = append(regions, aws.ToString(r.RegionName))
		}
		slices.Sort(regions)
	}
	for _, region := range regions {
		cl, err := factory(ctx, region)
		if err != nil {
			s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Failed, Reason: err.Error()})
			continue
		}
		s.region(ctx, region, cl.EC2)
	}
	return s.rep, nil
}

type sweeper struct {
	o   Options
	rep *Report
	// allRegions: the regions come from the account, not from the caller.
	allRegions bool
}

// ParseRegions turns the value of a -regions flag into Options.Regions: a comma
// separated list (blanks and repeats dropped), or empty / "all" for every enabled
// region of the account (nil). "all" among other names is an error.
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

// unreachable reports an error that comes from the network layer: the endpoint of
// the region does not answer (dial or I/O timeout, name not resolved, connection
// refused), as opposed to an answer of the service. A cancelled sweep is not one.
func unreachable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if _, ok := errors.AsType[smithy.APIError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
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

// matches reports whether the cluster tag selects the resource.
func (s *sweeper) matches(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix) && len(cluster) > len(s.o.ClusterPrefix)
}

// clusterFilter is the server-side pre-selection; matches is the real check.
func (s *sweeper) clusterFilter() ec2types.Filter {
	v := s.o.Cluster
	if v == "" {
		v = s.o.ClusterPrefix + "*"
	}
	return ec2types.Filter{Name: aws.String("tag:" + ClusterTag), Values: []string{v}}
}

func tagMap(tags []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// createdAt parses the ci-created-at tag.
func createdAt(tags map[string]string) (time.Time, bool) {
	v, ok := tags[CreatedAtTag]
	if !ok {
		return time.Time{}, false
	}
	if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(sec, 0), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// verdict decides whether a resource is to be swept: its cluster tag must
// match, and with an age limit it must be old enough (created is its creation
// time when the API has one, else the tag decides).
func (s *sweeper) verdict(tags map[string]string, created *time.Time) (cluster string, ok bool, reason string) {
	cluster = tags[ClusterTag]
	if !s.matches(cluster) {
		return cluster, false, "the cluster tag does not match"
	}
	if s.o.MaxAge <= 0 {
		return cluster, true, ""
	}
	var t time.Time
	switch {
	case created != nil && !created.IsZero():
		t = *created
	default:
		var known bool
		if t, known = createdAt(tags); !known {
			return cluster, false, "no creation time (tag " + CreatedAtTag + " missing)"
		}
	}
	if age := s.o.Now().Sub(t); age < s.o.MaxAge {
		return cluster, false, fmt.Sprintf("only %s old (limit %s)", age.Round(time.Second), s.o.MaxAge)
	}
	return cluster, true, ""
}

// notTerminated are the states of an instance that still exists, running or on its way
// out (the last one is the way out).
var notTerminated = []string{"pending", "running", "stopping", "stopped", "shutting-down"}

func isCode(err error, codes ...string) bool {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return slices.Contains(codes, apiErr.ErrorCode())
	}
	return false
}

// notFound reports a deletion that found nothing left to delete.
func notFound(err error) bool {
	return isCode(err, "InvalidInstanceID.NotFound", "InvalidGroup.NotFound", "InvalidPlacementGroup.Unknown",
		"InvalidSubnetID.NotFound", "InvalidRouteTableID.NotFound", "InvalidInternetGatewayID.NotFound",
		"InvalidVpcID.NotFound", "InvalidAssociationID.NotFound", "Gateway.NotAttached")
}

// delete runs one deletion and records it. It returns false when the resource is
// still there, so that the caller can try again after its dependencies went.
func (s *sweeper) del(region, kind, id, cluster string, fn func() error) bool {
	it := Item{Region: region, Kind: kind, ID: id, Cluster: cluster}
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.add(it)
		return true
	}
	if err := fn(); err != nil && !notFound(err) {
		it.Outcome, it.Reason = Failed, err.Error()
		s.add(it)
		return false
	}
	it.Outcome = Deleted
	s.add(it)
	return true
}

// paged collects the items of a paginated call.
func paged[T any](next func(token *string) ([]T, *string, error)) ([]T, error) {
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

func (s *sweeper) region(ctx context.Context, region string, c awsdriver.EC2API) {
	fail := func(kind string, err error) {
		s.add(Item{Region: region, Kind: kind, ID: "*", Outcome: Failed, Reason: err.Error()})
	}
	terminated, err := s.instances(ctx, region, c)
	if isCode(err, "UnauthorizedOperation", "AccessDenied", "AuthFailure", "OptInRequired") {
		// The role has no access to the region (a region limit of the policy, a region
		// the account has not opted into): nothing of the lane can exist there either.
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "no access: " + err.Error()})
		return
	}
	if s.allRegions && unreachable(ctx, err) {
		// Only when the regions were not asked for: an opt-in region of the account may
		// be down or blocked, and nothing of the lane can be swept there anyway.
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "unreachable: " + err.Error()})
		return
	}
	if err != nil {
		fail("instance", err)
	}
	if len(terminated) > 0 && !s.o.DryRun {
		s.waitTerminated(ctx, region, c, terminated)
	}
	if err := s.placementGroups(ctx, region, c); err != nil {
		fail("placement-group", err)
	}
	if err := s.securityGroups(ctx, region, c); err != nil {
		fail("security-group", err)
	}
	if err := s.network(ctx, region, c); err != nil {
		fail("network", err)
	}
	if err := s.spotRequests(ctx, region, c); err != nil {
		fail("spot-request", err)
	}
}

// instances terminates the selected instances that are not gone yet and returns
// their IDs.
func (s *sweeper) instances(ctx context.Context, region string, c awsdriver.EC2API) ([]string, error) {
	live := notTerminated[:4]
	all, err := paged(func(token *string) ([]ec2types.Instance, *string, error) {
		out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{NextToken: token, Filters: []ec2types.Filter{
			s.clusterFilter(), {Name: aws.String("instance-state-name"), Values: live}}})
		if err != nil {
			return nil, nil, err
		}
		var items []ec2types.Instance
		for _, r := range out.Reservations {
			items = append(items, r.Instances...)
		}
		return items, out.NextToken, nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	var ids []string
	for _, i := range all {
		id := aws.ToString(i.InstanceId)
		cluster, ok, reason := s.verdict(tagMap(i.Tags), i.LaunchTime)
		if !ok {
			if cluster != "" && s.matches(cluster) {
				s.add(Item{Region: region, Kind: "instance", ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
			}
			continue
		}
		if s.del(region, "instance", id, cluster, func() error {
			_, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
			return err
		}) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// waitTerminated waits until the instances are terminated (or gone), up to
// WaitTerminated: groups and subnets cannot be deleted while an instance that
// used them is still shutting down. (The in-memory fake terminates at once, so the tests
// cannot show the wait; the real service takes up to a minute or two.)
func (s *sweeper) waitTerminated(ctx context.Context, region string, c awsdriver.EC2API, ids []string) {
	deadline := s.o.Now().Add(s.o.WaitTerminated)
	for {
		out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids, Filters: []ec2types.Filter{
			{Name: aws.String("instance-state-name"), Values: notTerminated}}})
		if err != nil && !isCode(err, "InvalidInstanceID.NotFound") {
			s.o.Logf("%s waiting for the instances to terminate: %v", region, err)
			return
		}
		n := 0
		if out != nil {
			for _, r := range out.Reservations {
				n += len(r.Instances)
			}
		}
		if n == 0 || !s.o.Now().Before(deadline) {
			if n > 0 {
				s.o.Logf("%s %d instances are still shutting down; groups they use may stay for the next sweep", region, n)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.o.PollInterval):
		}
	}
}

func (s *sweeper) placementGroups(ctx context.Context, region string, c awsdriver.EC2API) error {
	out, err := c.DescribePlacementGroups(ctx,
		&ec2.DescribePlacementGroupsInput{Filters: []ec2types.Filter{s.clusterFilter()}})
	if err != nil {
		return fmt.Errorf("listing placement groups: %w", err)
	}
	for _, g := range out.PlacementGroups {
		name := aws.ToString(g.GroupName)
		cluster, ok, reason := s.verdict(tagMap(g.Tags), nil)
		if !ok {
			s.skip(region, "placement-group", name, cluster, reason)
			continue
		}
		s.del(region, "placement-group", name, cluster, func() error {
			_, err := c.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: aws.String(name)})
			return err
		})
	}
	return nil
}

// skip records a skipped resource of a selected cluster (resources of other
// clusters are not reported: they are none of the sweep's business).
func (s *sweeper) skip(region, kind, id, cluster, reason string) {
	if s.matches(cluster) {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
	}
}

func (s *sweeper) securityGroups(ctx context.Context, region string, c awsdriver.EC2API) error {
	groups, err := paged(func(token *string) ([]ec2types.SecurityGroup, *string, error) {
		out, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{NextToken: token,
			Filters: []ec2types.Filter{s.clusterFilter()}})
		if err != nil {
			return nil, nil, err
		}
		return out.SecurityGroups, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing security groups: %w", err)
	}
	var todo []ec2types.SecurityGroup
	for _, g := range groups {
		id := aws.ToString(g.GroupId)
		cluster, ok, reason := s.verdict(tagMap(g.Tags), nil)
		switch {
		case aws.ToString(g.GroupName) == "default":
			s.skip(region, "security-group", id, cluster, "a default group is never deleted")
		case !ok:
			s.skip(region, "security-group", id, cluster, reason)
		default:
			todo = append(todo, g)
		}
	}
	// Groups that refer to each other block each other's deletion: retry while a
	// pass makes progress.
	for len(todo) > 0 {
		var left []ec2types.SecurityGroup
		for _, g := range todo {
			id := aws.ToString(g.GroupId)
			if !s.delQuiet(region, "security-group", id, tagMap(g.Tags)[ClusterTag], func() error {
				_, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
				return err
			}) {
				left = append(left, g)
			}
		}
		if len(left) == len(todo) {
			for _, g := range left {
				s.add(Item{Region: region, Kind: "security-group", ID: aws.ToString(g.GroupId),
					Cluster: tagMap(g.Tags)[ClusterTag], Outcome: Failed, Reason: "still in use"})
			}
			return nil
		}
		todo = left
	}
	return nil
}

// delQuiet is del that records only success: a failure may be resolved by a
// later pass, and the caller reports what is left.
func (s *sweeper) delQuiet(region, kind, id, cluster string, fn func() error) bool {
	if s.o.DryRun {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: WouldDelete})
		return true
	}
	if err := fn(); err != nil && !notFound(err) {
		return false
	}
	s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Deleted})
	return true
}

// network deletes the subnets, route tables, internet gateways and VPCs of the
// selected clusters, in dependency order: the managed network of SCarpenter
// (docs/reference/aws-driver.md#resources-and-network) and the subnets a test
// brings itself.
func (s *sweeper) network(ctx context.Context, region string, c awsdriver.EC2API) error {
	f := []ec2types.Filter{s.clusterFilter()}
	rts, err := paged(func(token *string) ([]ec2types.RouteTable, *string, error) {
		out, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{NextToken: token, Filters: f})
		if err != nil {
			return nil, nil, err
		}
		return out.RouteTables, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing route tables: %w", err)
	}
	for _, rt := range rts {
		id := aws.ToString(rt.RouteTableId)
		cluster, ok, reason := s.verdict(tagMap(rt.Tags), nil)
		main := slices.ContainsFunc(rt.Associations,
			func(a ec2types.RouteTableAssociation) bool { return aws.ToBool(a.Main) })
		switch {
		case main:
			s.skip(region, "route-table", id, cluster, "a main route table is never deleted")
		case !ok:
			s.skip(region, "route-table", id, cluster, reason)
		default:
			for _, a := range rt.Associations {
				s.disassociate(ctx, region, c, aws.ToString(a.RouteTableAssociationId), cluster)
			}
			s.del(region, "route-table", id, cluster, func() error {
				_, err := c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(id)})
				return err
			})
		}
	}

	subnets, err := paged(func(token *string) ([]ec2types.Subnet, *string, error) {
		out, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{NextToken: token, Filters: f})
		if err != nil {
			return nil, nil, err
		}
		return out.Subnets, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing subnets: %w", err)
	}
	for _, sn := range subnets {
		id := aws.ToString(sn.SubnetId)
		cluster, ok, reason := s.verdict(tagMap(sn.Tags), nil)
		if !ok {
			s.skip(region, "subnet", id, cluster, reason)
			continue
		}
		s.del(region, "subnet", id, cluster, func() error {
			_, err := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(id)})
			return err
		})
	}

	gws, err := paged(func(token *string) ([]ec2types.InternetGateway, *string, error) {
		out, err := c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{NextToken: token, Filters: f})
		if err != nil {
			return nil, nil, err
		}
		return out.InternetGateways, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing internet gateways: %w", err)
	}
	for _, g := range gws {
		id := aws.ToString(g.InternetGatewayId)
		cluster, ok, reason := s.verdict(tagMap(g.Tags), nil)
		if !ok {
			s.skip(region, "internet-gateway", id, cluster, reason)
			continue
		}
		s.del(region, "internet-gateway", id, cluster, func() error {
			for _, a := range g.Attachments {
				if _, err := c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
					InternetGatewayId: aws.String(id), VpcId: a.VpcId}); err != nil && !notFound(err) {
					return fmt.Errorf("detaching from %s: %w", aws.ToString(a.VpcId), err)
				}
			}
			_, err := c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(id)})
			return err
		})
	}

	vpcs, err := paged(func(token *string) ([]ec2types.Vpc, *string, error) {
		out, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{NextToken: token, Filters: f})
		if err != nil {
			return nil, nil, err
		}
		return out.Vpcs, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing VPCs: %w", err)
	}
	for _, v := range vpcs {
		id := aws.ToString(v.VpcId)
		cluster, ok, reason := s.verdict(tagMap(v.Tags), nil)
		switch {
		case aws.ToBool(v.IsDefault):
			s.skip(region, "vpc", id, cluster, "a default VPC is never deleted")
		case !ok:
			s.skip(region, "vpc", id, cluster, reason)
		default:
			s.del(region, "vpc", id, cluster, func() error {
				_, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(id)})
				return err
			})
		}
	}
	return nil
}

// disassociate removes a route table association (the subnet's, before either is deleted).
func (s *sweeper) disassociate(ctx context.Context, region string, c awsdriver.EC2API, id, cluster string) {
	if s.o.DryRun {
		return
	}
	_, err := c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: aws.String(id)})
	if err != nil && !notFound(err) {
		s.add(Item{Region: region, Kind: "route-table-association", ID: id, Cluster: cluster,
			Outcome: Failed, Reason: err.Error()})
	}
}

// spotRequests only reports the tagged spot requests that are still open or
// active: a one-time request closes with its instance, a few minutes after it
// ended, and cancelling it needs a permission the policy does not grant.
func (s *sweeper) spotRequests(ctx context.Context, region string, c awsdriver.EC2API) error {
	reqs, err := paged(func(token *string) ([]ec2types.SpotInstanceRequest, *string, error) {
		out, err := c.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{NextToken: token,
			Filters: []ec2types.Filter{s.clusterFilter(), {Name: aws.String("state"), Values: []string{"open", "active"}}}})
		if err != nil {
			return nil, nil, err
		}
		return out.SpotInstanceRequests, out.NextToken, nil
	})
	if err != nil {
		return fmt.Errorf("listing spot requests: %w", err)
	}
	for _, r := range reqs {
		cluster := tagMap(r.Tags)[ClusterTag]
		s.skip(region, "spot-request", aws.ToString(r.SpotInstanceRequestId), cluster,
			"reported only, a one-time request closes with its instance (state "+string(r.State)+")")
	}
	return nil
}
