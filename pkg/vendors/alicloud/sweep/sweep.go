/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in an
// Alibaba Cloud account (docs/how-to-guides/testing.md#the-l5-lane): instances,
// deployment sets, security groups, vSwitches and VPCs, selected by the cluster tag
// scarpenter.sparecores.com/cluster. It is the sweeper workflow's engine (every
// three hours, everything of a cluster named scarpenter-ci-* older than two
// hours) and the e2e workflow's last line of defence (one cluster, no age limit).
//
// It talks to ECS and VPC through the driver's narrow API (alicloud.ECSAPI and
// alicloud.VPCAPI), so the in-memory fake of the driver tests runs it unchanged.
//
// Selection is conservative on purpose. A resource is touched only when its
// cluster marker matches (exactly, or by prefix) and, with an age limit, when it
// is old enough: every kind has a creation time in its API, which decides, and the
// tag ci-created-at (Unix seconds) stands in where the API gives none. A resource
// without the marker is never touched, a deployment set carries the marker in its
// description (it takes no tags), and default VPCs and default vSwitches are
// left alone, whatever their tags (a marked group in a default VPC is the
// driver's own and is deleted).
package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
)

const (
	// ClusterTag is the tag every SCarpenter resource carries with the cluster name
	// (alicloud.TagKey(vendors.TagCluster)).
	ClusterTag = "scarpenter.sparecores.com/cluster"
	// CreatedAtTag holds the creation time of a resource in Unix seconds (RFC 3339
	// is accepted too), where the API has none.
	CreatedAtTag = "ci-created-at"
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"

	kindRegion = "region"
	pageSize   = 50
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
	// Regions limits the sweep to these regions; empty means every region of the
	// account (see ParseRegions). Only in that all-regions mode is a region whose
	// endpoint cannot be reached skipped; a requested region that cannot be reached
	// is a failure.
	Regions []string
	// WaitReleased bounds the wait, per region, for deleted instances to be gone
	// before the groups and the network they used are deleted.
	WaitReleased time.Duration
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

// Run sweeps the regions. It returns an error for a failure that makes the whole
// sweep meaningless (no clients, no region list); failures of single resources are
// items with the outcome Failed.
func Run(ctx context.Context, factory alicloud.ClientFactory, home string, o Options) (*Report, error) {
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
		out, err := cl.ECS.DescribeRegions(ctx, &ecs.DescribeRegionsRequest{})
		if err != nil {
			return nil, fmt.Errorf("listing the regions: %w", err)
		}
		if out.Regions != nil {
			for _, r := range out.Regions.Region {
				regions = append(regions, deref(r.RegionId))
			}
		}
		slices.Sort(regions)
	}
	for _, region := range regions {
		cl, err := factory(ctx, region)
		if err != nil {
			s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Failed, Reason: err.Error()})
			continue
		}
		s.region(ctx, region, cl)
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
// separated list (blanks and repeats dropped), or empty / "all" for every region of
// the account (nil). "all" among other names is an error.
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

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// unreachable reports an error that comes from the network layer: the endpoint of
// the region does not answer, as opposed to an answer of the service. A cancelled
// sweep is not one.
func unreachable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if _, ok := errors.AsType[*dara.SDKError](err); ok {
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

func code(err error) string {
	if e, ok := errors.AsType[*dara.SDKError](err); ok {
		return deref(e.Code)
	}
	return ""
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

// matches reports whether the cluster marker selects the resource.
func (s *sweeper) matches(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix) && len(cluster) > len(s.o.ClusterPrefix)
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

// parseTime reads the time formats of ECS and VPC: RFC 3339 and the minute form of
// DescribeInstances.
func parseTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// verdict decides whether a resource is to be swept: its cluster marker must match,
// and with an age limit it must be old enough (created is its creation time when
// the API has one, else the tag decides).
func (s *sweeper) verdict(cluster string, tags map[string]string, created time.Time) (ok bool, reason string) {
	if !s.matches(cluster) {
		return false, "the cluster tag does not match"
	}
	if s.o.MaxAge <= 0 {
		return true, ""
	}
	t := created
	if t.IsZero() {
		var known bool
		if t, known = createdAt(tags); !known {
			return false, "no creation time (tag " + CreatedAtTag + " missing)"
		}
	}
	if age := s.o.Now().Sub(t); age < s.o.MaxAge {
		return false, fmt.Sprintf("only %s old (limit %s)", age.Round(time.Second), s.o.MaxAge)
	}
	return true, ""
}

// skip records a skipped resource of a selected cluster (resources of other
// clusters are not reported: they are none of the sweep's business).
func (s *sweeper) skip(region, kind, id, cluster, reason string) {
	if s.matches(cluster) {
		s.add(Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: Skipped, Reason: reason})
	}
}

// notFound reports a deletion that found nothing left to delete.
func notFound(err error) bool {
	switch code(err) {
	case "InvalidInstanceId.NotFound", "InvalidSecurityGroupId.NotFound", "InvalidDeploymentSetId.NotFound",
		"InvalidVSwitchId.NotFound", "InvalidVpcID.NotFound", "InvalidVpcId.NotFound":
		return true
	}
	return false
}

// del runs one deletion and records it. It returns false when the resource is still
// there, so that the caller can try again after its dependencies went.
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

// delQuiet is del that records only success: a failure may be resolved by a later
// pass, and the caller reports what is left.
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

func (s *sweeper) region(ctx context.Context, region string, cl alicloud.Clients) {
	fail := func(kind string, err error) {
		s.add(Item{Region: region, Kind: kind, ID: "*", Outcome: Failed, Reason: err.Error()})
	}
	deleted, err := s.instances(ctx, region, cl.ECS)
	switch c := code(err); {
	case c == "RegionUnauthorized" || c == "Forbidden.RAM" || strings.HasPrefix(c, "Forbidden"):
		// The role has no access to the region (a region limit of the policy, a region the
		// account cannot use): nothing of the lane can exist there either.
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "no access: " + err.Error()})
		return
	case s.allRegions && unreachable(ctx, err):
		s.add(Item{Region: region, Kind: kindRegion, ID: region, Outcome: Skipped, Reason: "unreachable: " + err.Error()})
		return
	case err != nil:
		fail("instance", err)
	}
	if len(deleted) > 0 && !s.o.DryRun {
		s.waitReleased(ctx, region, cl.ECS, deleted)
	}
	if err := s.deploymentSets(ctx, region, cl.ECS); err != nil {
		fail("deployment-set", err)
	}
	if err := s.securityGroups(ctx, region, cl.ECS); err != nil {
		fail("security-group", err)
	}
	if err := s.network(ctx, region, cl.VPC); err != nil {
		fail("network", err)
	}
}

// instances deletes the selected instances and returns the IDs it asked ECS to
// release.
func (s *sweeper) instances(ctx context.Context, region string, c alicloud.ECSAPI) ([]string, error) {
	in := &ecs.DescribeInstancesRequest{RegionId: new(region), MaxResults: dara.Int32(100)}
	if s.o.Cluster != "" {
		in.Tag = []*ecs.DescribeInstancesRequestTag{{Key: new(ClusterTag), Value: new(s.o.Cluster)}}
	}
	var ids []string
	for {
		out, err := c.DescribeInstances(ctx, in)
		if err != nil {
			return ids, fmt.Errorf("listing instances: %w", err)
		}
		if out.Instances != nil {
			for _, i := range out.Instances.Instance {
				id := deref(i.InstanceId)
				tags := map[string]string{}
				if i.Tags != nil {
					for _, t := range i.Tags.Tag {
						tags[deref(t.TagKey)] = deref(t.TagValue)
					}
				}
				cluster := tags[ClusterTag]
				if ok, reason := s.verdict(cluster, tags, parseTime(deref(i.CreationTime))); !ok {
					s.skip(region, "instance", id, cluster, reason)
					continue
				}
				if s.del(region, "instance", id, cluster, func() error {
					_, err := c.DeleteInstance(ctx, &ecs.DeleteInstanceRequest{InstanceId: new(id), Force: new(true)})
					return err
				}) {
					ids = append(ids, id)
				}
			}
		}
		if deref(out.NextToken) == "" {
			return ids, nil
		}
		in.NextToken = out.NextToken
	}
}

// waitReleased waits until the instances are gone, up to WaitReleased: groups and
// vSwitches cannot be deleted while an instance that used them is still being
// released.
func (s *sweeper) waitReleased(ctx context.Context, region string, c alicloud.ECSAPI, ids []string) {
	deadline := s.o.Now().Add(s.o.WaitReleased)
	for {
		b, _ := jsonList(ids)
		out, err := c.DescribeInstances(ctx, &ecs.DescribeInstancesRequest{RegionId: new(region), InstanceIds: new(b)})
		if err != nil {
			s.o.Logf("%s waiting for the instances to be released: %v", region, err)
			return
		}
		n := 0
		if out.Instances != nil {
			n = len(out.Instances.Instance)
		}
		if n == 0 || !s.o.Now().Before(deadline) {
			if n > 0 {
				s.o.Logf("%s %d instances are still being released; groups they use may stay for the next sweep", region, n)
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

// deploymentSets deletes the sets whose description carries a selected cluster: a
// deployment set takes no tags, the driver writes its marker into the description.
func (s *sweeper) deploymentSets(ctx context.Context, region string, c alicloud.ECSAPI) error {
	in := &ecs.DescribeDeploymentSetsRequest{RegionId: new(region), PageSize: dara.Int32(100)}
	for page := int32(1); ; page++ {
		in.PageNumber = new(page)
		out, err := c.DescribeDeploymentSets(ctx, in)
		if err != nil {
			return fmt.Errorf("listing deployment sets: %w", err)
		}
		n := 0
		if out.DeploymentSets != nil {
			for _, set := range out.DeploymentSets.DeploymentSet {
				n++
				id := deref(set.DeploymentSetId)
				cluster := markerCluster(deref(set.DeploymentSetDescription))
				if ok, reason := s.verdict(cluster, nil, parseTime(deref(set.CreationTime))); !ok {
					s.skip(region, "deployment-set", id, cluster, reason)
					continue
				}
				s.del(region, "deployment-set", id, cluster, func() error {
					_, err := c.DeleteDeploymentSet(ctx, &ecs.DeleteDeploymentSetRequest{
						RegionId: new(region), DeploymentSetId: new(id),
					})
					return err
				})
			}
		}
		if n == 0 || int(page)*100 >= int(deref(out.TotalCount)) {
			return nil
		}
	}
}

// markerCluster reads the cluster out of the description of a deployment set the
// driver made ("scarpenter.sparecores.com: cluster=<name> nodeclass=...").
func markerCluster(desc string) string {
	rest, ok := strings.CutPrefix(desc, "scarpenter.sparecores.com: ")
	if !ok {
		return ""
	}
	for kv := range strings.FieldsSeq(rest) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == string(vendors.TagCluster) {
			return v
		}
	}
	return ""
}

// securityGroups deletes the selected groups; groups that refer to each other
// block each other's deletion, so it retries while a pass makes progress.
func (s *sweeper) securityGroups(ctx context.Context, region string, c alicloud.ECSAPI) error {
	in := &ecs.DescribeSecurityGroupsRequest{RegionId: new(region), MaxResults: dara.Int32(100)}
	var todo []*ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroup
	for {
		out, err := c.DescribeSecurityGroups(ctx, in)
		if err != nil {
			return fmt.Errorf("listing security groups: %w", err)
		}
		if out.SecurityGroups != nil {
			for _, g := range out.SecurityGroups.SecurityGroup {
				tags := map[string]string{}
				if g.Tags != nil {
					for _, t := range g.Tags.Tag {
						tags[deref(t.TagKey)] = deref(t.TagValue)
					}
				}
				cluster := tags[ClusterTag]
				if ok, reason := s.verdict(cluster, tags, parseTime(deref(g.CreationTime))); !ok {
					s.skip(region, "security-group", deref(g.SecurityGroupId), cluster, reason)
					continue
				}
				todo = append(todo, g)
			}
		}
		if deref(out.NextToken) == "" {
			break
		}
		in.NextToken = out.NextToken
	}
	for len(todo) > 0 {
		var left []*ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroup
		for _, g := range todo {
			id := deref(g.SecurityGroupId)
			if !s.delQuiet(region, "security-group", id, clusterOf(g.Tags), func() error {
				_, err := c.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{
					RegionId: new(region), SecurityGroupId: new(id),
				})
				return err
			}) {
				left = append(left, g)
			}
		}
		if len(left) == len(todo) {
			for _, g := range left {
				s.add(Item{Region: region, Kind: "security-group", ID: deref(g.SecurityGroupId), Cluster: clusterOf(g.Tags),
					Outcome: Failed, Reason: "still in use"})
			}
			return nil
		}
		todo = left
	}
	return nil
}

func clusterOf(t *ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroupTags) string {
	if t == nil {
		return ""
	}
	for _, tag := range t.Tag {
		if deref(tag.TagKey) == ClusterTag {
			return deref(tag.TagValue)
		}
	}
	return ""
}

// network deletes the vSwitches and the VPCs of the selected clusters, in
// dependency order: the managed network of SCarpenter and the vSwitches a test
// brings itself. A default VPC and a default vSwitch are never deleted.
func (s *sweeper) network(ctx context.Context, region string, c alicloud.VPCAPI) error {
	subs, err := s.vswitches(ctx, region, c)
	if err != nil {
		return err
	}
	for _, sw := range subs {
		id := deref(sw.VSwitchId)
		tags := map[string]string{}
		if sw.Tags != nil {
			for _, t := range sw.Tags.Tag {
				tags[deref(t.Key)] = deref(t.Value)
			}
		}
		cluster := tags[ClusterTag]
		ok, reason := s.verdict(cluster, tags, parseTime(deref(sw.CreationTime)))
		switch {
		case deref(sw.IsDefault):
			s.skip(region, "vswitch", id, cluster, "a default vSwitch is never deleted")
		case !ok:
			s.skip(region, "vswitch", id, cluster, reason)
		default:
			s.del(region, "vswitch", id, cluster, func() error {
				_, err := c.DeleteVSwitch(ctx, &vpc.DeleteVSwitchRequest{RegionId: new(region), VSwitchId: new(id)})
				return err
			})
		}
	}
	in := &vpc.DescribeVpcsRequest{RegionId: new(region), PageSize: dara.Int32(pageSize)}
	for page := int32(1); ; page++ {
		in.PageNumber = new(page)
		out, err := c.DescribeVpcs(ctx, in)
		if err != nil {
			return fmt.Errorf("listing VPCs: %w", err)
		}
		n := 0
		if out.Vpcs != nil {
			for _, v := range out.Vpcs.Vpc {
				n++
				id := deref(v.VpcId)
				tags := map[string]string{}
				if v.Tags != nil {
					for _, t := range v.Tags.Tag {
						tags[deref(t.Key)] = deref(t.Value)
					}
				}
				cluster := tags[ClusterTag]
				ok, reason := s.verdict(cluster, tags, parseTime(deref(v.CreationTime)))
				switch {
				case deref(v.IsDefault):
					s.skip(region, "vpc", id, cluster, "a default VPC is never deleted")
				case !ok:
					s.skip(region, "vpc", id, cluster, reason)
				default:
					s.del(region, "vpc", id, cluster, func() error {
						_, err := c.DeleteVpc(ctx, &vpc.DeleteVpcRequest{RegionId: new(region), VpcId: new(id)})
						return err
					})
				}
			}
		}
		if n == 0 || int(page)*pageSize >= int(deref(out.TotalCount)) {
			return nil
		}
	}
}

func (s *sweeper) vswitches(
	ctx context.Context, region string, c alicloud.VPCAPI,
) ([]*vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitch, error) {
	in := &vpc.DescribeVSwitchesRequest{RegionId: new(region), PageSize: dara.Int32(pageSize)}
	var out []*vpc.DescribeVSwitchesResponseBodyVSwitchesVSwitch
	for page := int32(1); ; page++ {
		in.PageNumber = new(page)
		res, err := c.DescribeVSwitches(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("listing vSwitches: %w", err)
		}
		n := 0
		if res.VSwitches != nil {
			n = len(res.VSwitches.VSwitch)
			out = append(out, res.VSwitches.VSwitch...)
		}
		if n == 0 || int(page)*pageSize >= int(deref(res.TotalCount)) {
			return out, nil
		}
	}
}

// jsonList is the JSON array of IDs the InstanceIds parameter takes.
func jsonList(ids []string) (string, error) {
	b, err := json.Marshal(ids)
	return string(b), err
}
