/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the OVH L5 lane left in a
// Public Cloud project (docs/how-to-guides/testing.md#the-l5-lane): servers, then the
// security groups and server groups SCarpenter made for the cluster. It is the sweeper
// workflow's engine (every three hours, everything of a cluster named scarpenter-ci-*
// older than two hours) and the e2e workflow's last line of defence (one cluster, no age
// limit). It talks to OpenStack through the driver's narrow API (osapi), so the in-memory
// cloud of the driver tests runs it unchanged. It never touches images.
//
// Selection is conservative on purpose. A server is ours when its metadata names the
// cluster (scarpenter-cluster) and SCarpenter as its manager, and is swept by its own
// creation time. A security group is ours by the ownership marker in its description
// (cluster, managed-by, and the creation time the driver records), a server group, which
// has only a name, by the cluster's deterministic name prefix. A server group has no age:
// it is swept only when the cluster has no server left in the region and the cluster's
// oldest security group is old enough (a cluster without any group has nothing to age it
// by and is left alone unless the age limit is off). Neither the project's default security
// group nor anything without the marker is touched.
//
// Volumes are not swept: every volume the driver makes is part of a server create with
// delete_on_termination, carries no marker of its own, and goes with its server.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

const (
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"

	kindServer        = "server"
	kindSecurityGroup = "securityGroup"
	kindServerGroup   = "serverGroup"
)

// Options select what is swept.
type Options struct {
	// Cluster selects the resources of one cluster by exact name. Without it,
	// ClusterPrefix selects every cluster whose name starts with it (the default
	// prefix when both are empty).
	Cluster       string
	ClusterPrefix string
	// MaxAge is the age a resource must have; 0 means any age.
	MaxAge time.Duration
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool
	// Regions limits the sweep to these regions; empty means every compute region of the
	// project's catalog. A requested region that cannot be reached is a failure.
	Regions []string
	// WaitDeleted bounds the wait, per region, for deleted servers to disappear before
	// the groups they used are deleted.
	WaitDeleted time.Duration
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
type Report struct{ Items []Item }

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

// ParseRegions reads the -regions flag: a comma list, or empty or "all" for every
// region of the catalog.
func ParseRegions(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "all" {
		return nil, nil
	}
	var out []string
	for r := range strings.SplitSeq(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" || strings.ContainsAny(r, " /") {
			return nil, fmt.Errorf("%q is not a list of region names", s)
		}
		out = append(out, r)
	}
	return out, nil
}

type sweeper struct {
	o Options
	r *Report
}

// Run sweeps the regions. It returns an error for a failure that makes the whole sweep
// meaningless (no region list); failures of single resources are items with the outcome
// Failed.
func Run(ctx context.Context, p osapi.Provider, o Options) (*Report, error) {
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
	regions := o.Regions
	if len(regions) == 0 {
		var err error
		if regions, err = p.Regions(ctx); err != nil {
			return nil, fmt.Errorf("reading the catalog's regions: %w", err)
		}
	}
	s := &sweeper{o: o, r: &Report{}}
	for _, region := range regions {
		if err := ctx.Err(); err != nil {
			return s.r, err
		}
		s.region(ctx, p, region)
	}
	return s.r, nil
}

func (s *sweeper) add(region, kind, id, cluster string, out Outcome, reason string) {
	s.r.Items = append(s.r.Items, Item{Region: region, Kind: kind, ID: id, Cluster: cluster, Outcome: out, Reason: reason})
	s.o.Logf("%s %s %s %s (cluster %s) %s", region, kind, id, out, cluster, reason)
}

// ours says whether the cluster name is selected.
func (s *sweeper) ours(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix)
}

// oldEnough says whether a resource created at the time is past the age limit.
func (s *sweeper) oldEnough(created time.Time) bool {
	return s.o.MaxAge == 0 || (!created.IsZero() && s.o.Now().Sub(created) >= s.o.MaxAge)
}

// groupPrefix is what the name of every managed server group of the selected clusters
// starts with.
func (s *sweeper) groupPrefix() string {
	if s.o.Cluster != "" {
		return resources.NamePrefix(s.o.Cluster)
	}
	return "scarpenter-" + s.o.ClusterPrefix
}

func (s *sweeper) region(ctx context.Context, p osapi.Provider, region string) {
	cl, err := p.Clients(ctx, region)
	if err != nil {
		reason := "the region cannot be reached: " + err.Error()
		if len(s.o.Regions) == 0 && errors.Is(err, osapi.ErrNoEndpoint) {
			s.add(region, "region", region, "", Skipped, reason)
			return
		}
		s.add(region, "region", region, "", Failed, reason)
		return
	}
	left := s.servers(ctx, cl, region)
	oldest := s.securityGroups(ctx, cl, region, left)
	s.serverGroups(ctx, cl, region, left, oldest)
}

// servers sweeps the servers and returns, per selected cluster, how many of its servers
// are still there afterwards (younger than the limit, being deleted, or failed).
func (s *sweeper) servers(ctx context.Context, cl osapi.Clients, region string) map[string]int {
	left := map[string]int{}
	servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
	if err != nil {
		s.add(region, kindServer, "*", "", Failed, "listing the servers: "+err.Error())
		return left
	}
	var deleted []string
	for _, srv := range servers {
		cluster := srv.Metadata[ovh.TagKey(vendors.TagCluster)]
		if !s.ours(cluster) || srv.Metadata[ovh.TagKey(vendors.TagManagedBy)] != vendors.ManagedByValue ||
			ovh.ServerState(srv) == vendors.StateTerminated {
			continue
		}
		if !s.oldEnough(srv.Created) {
			left[cluster]++
			s.add(region, kindServer, srv.ID, cluster, Skipped, "younger than the age limit")
			continue
		}
		if s.o.DryRun {
			left[cluster]++
			s.add(region, kindServer, srv.ID, cluster, WouldDelete, "")
			continue
		}
		if err := cl.Compute.DeleteServer(ctx, srv.ID); err != nil && !ovh.IsNotFound(err) {
			left[cluster]++
			s.add(region, kindServer, srv.ID, cluster, Failed, err.Error())
			continue
		}
		deleted = append(deleted, srv.ID)
		s.add(region, kindServer, srv.ID, cluster, Deleted, "")
	}
	if len(deleted) > 0 {
		s.waitGone(ctx, cl, deleted)
	}
	return left
}

// waitGone waits until the deleted servers are no longer listed, at most WaitDeleted.
func (s *sweeper) waitGone(ctx context.Context, cl osapi.Clients, ids []string) {
	deadline := time.Now().Add(s.o.WaitDeleted)
	for {
		servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
		if err == nil && !slices.ContainsFunc(servers, func(x osapi.Server) bool { return slices.Contains(ids, x.ID) }) {
			return
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.o.PollInterval):
		}
	}
}

// securityGroups sweeps the managed security groups of the selected clusters and returns
// the oldest creation time of a group per cluster (before the sweep): what serverGroups
// ages by.
func (s *sweeper) securityGroups(
	ctx context.Context, cl osapi.Clients, region string, left map[string]int,
) map[string]time.Time {
	oldest := map[string]time.Time{}
	groups, err := cl.Network.ListSecurityGroups(ctx)
	if err != nil {
		s.add(region, kindSecurityGroup, "*", "", Failed, "listing the security groups: "+err.Error())
		return oldest
	}
	for _, g := range groups {
		tags := ovh.MarkerTags(g.Description)
		cluster := tags[vendors.TagCluster]
		if tags[vendors.TagManagedBy] != vendors.ManagedByValue || !s.ours(cluster) {
			continue
		}
		created, _ := ovh.MarkerCreated(g.Description)
		if cur, seen := oldest[cluster]; !created.IsZero() && (!seen || created.Before(cur)) {
			oldest[cluster] = created
		}
		switch {
		case left[cluster] > 0:
			s.add(region, kindSecurityGroup, g.ID, cluster, Skipped, "a server of the cluster is left")
		case !s.oldEnough(created):
			s.add(region, kindSecurityGroup, g.ID, cluster, Skipped, "younger than the age limit, or no creation time")
		case s.o.DryRun:
			s.add(region, kindSecurityGroup, g.ID, cluster, WouldDelete, "")
		default:
			if err := cl.Network.DeleteSecurityGroup(ctx, g.ID); err != nil && !ovh.IsNotFound(err) {
				s.add(region, kindSecurityGroup, g.ID, cluster, Failed, err.Error())
				continue
			}
			s.add(region, kindSecurityGroup, g.ID, cluster, Deleted, "")
		}
	}
	return oldest
}

// serverGroups sweeps the managed server groups: by name prefix, and only for a cluster
// with no server left whose oldest security group is past the age limit (a server group
// has no creation time).
func (s *sweeper) serverGroups(
	ctx context.Context, cl osapi.Clients, region string, left map[string]int, oldest map[string]time.Time,
) {
	groups, err := cl.Compute.ListServerGroups(ctx)
	if err != nil {
		s.add(region, kindServerGroup, "*", "", Failed, "listing the server groups: "+err.Error())
		return
	}
	for _, g := range groups {
		if !strings.HasPrefix(g.Name, s.groupPrefix()) {
			continue
		}
		cluster := s.clusterOfGroup(g.Name, oldest)
		switch {
		case len(g.Members) > 0:
			s.add(region, kindServerGroup, g.ID, cluster, Skipped, "it has members")
		case s.o.MaxAge > 0 && !s.groupOldEnough(cluster, oldest, left):
			s.add(region, kindServerGroup, g.ID, cluster, Skipped, "no security group of the cluster shows it is old enough")
		case s.o.DryRun:
			s.add(region, kindServerGroup, g.ID, cluster, WouldDelete, "")
		default:
			if err := cl.Compute.DeleteServerGroup(ctx, g.ID); err != nil && !ovh.IsNotFound(err) {
				s.add(region, kindServerGroup, g.ID, cluster, Failed, err.Error())
				continue
			}
			s.add(region, kindServerGroup, g.ID, cluster, Deleted, "")
		}
	}
}

// clusterOfGroup is the selected cluster whose name prefix the group's name carries ("" when
// the clusters of the sweep cannot be told apart by name: the prefix mode with groups only).
func (s *sweeper) clusterOfGroup(name string, known map[string]time.Time) string {
	if s.o.Cluster != "" {
		return s.o.Cluster
	}
	for c := range known {
		if strings.HasPrefix(name, resources.NamePrefix(c)) {
			return c
		}
	}
	return ""
}

func (s *sweeper) groupOldEnough(cluster string, oldest map[string]time.Time, left map[string]int) bool {
	if cluster == "" || left[cluster] > 0 {
		return false
	}
	at, ok := oldest[cluster]
	return ok && s.oldEnough(at)
}
