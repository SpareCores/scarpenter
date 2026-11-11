/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in an
// UpCloud account (docs/how-to-guides/testing.md#the-l5-lane): servers (stopped,
// then deleted with their storages), and then the server groups, SDN firewall
// rulesets, SDN networks and unattached disks, selected by the label
// scarpenter-cluster. It is the sweeper workflow's engine (every three hours,
// everything of a cluster named scarpenter-ci-* older than two hours) and the
// e2e workflow's last line of defence (one cluster, no age limit).
//
// UpCloud has no regions or projects: the account is the scope, so one list
// call with the label filter covers every zone. The engine talks to the API
// through the driver's narrow interface (pkg/vendors/upcloud/api) plus
// DeleteStorage, so the in-memory fake of the driver tests runs it unchanged.
//
// Selection is conservative on purpose. Only a resource whose cluster label
// matches (exactly, or by prefix) is touched; a resource labelled
// scarpenter-use (what an operator offers to SCarpenter, such as the Flatcar
// templates) never is, whatever else it carries. A server's age is its
// scarpenter-created label; a server without it has no known age and is left
// alone when an age limit is set. The other resources have no creation stamp the
// API reports reliably, so with an age limit they are swept only when no server
// of their cluster is alive and young or of unknown age (the cluster's run may
// still be using them), and a disk only when it is online and attached to
// nothing.
package sweep

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
const DefaultClusterPrefix = "scarpenter-ci-"

// ClusterLabel is the label every SCarpenter resource carries with the cluster
// name.
var ClusterLabel = upcloudTagKey(vendors.TagCluster)

func upcloudTagKey(k vendors.TagKey) string { return driver.TagKey(k) }

// API is what the sweep calls: the driver's API plus the deletion of a disk.
type API interface {
	api.API
	DeleteStorage(ctx context.Context, r *request.DeleteStorageRequest) error
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

// Resource kinds of the report.
const (
	KindServer  = "server"
	KindGroup   = "serverGroup"
	KindRuleset = "firewallRuleset"
	KindNetwork = "network"
	KindStorage = "storage"
)

// Item is one resource the sweep looked at and acted on or skipped.
type Item struct {
	Kind    string
	ID      string
	Zone    string
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

type sweeper struct {
	a   API
	o   Options
	rep *Report
	// busy are the clusters with a server that is alive and young or of
	// unknown age: their other resources are not swept under an age limit.
	busy map[string]bool
}

// Run sweeps the account. It returns an error for a failure that makes the
// whole sweep meaningless (the server list cannot be read); failures of single
// resources are items with the outcome Failed.
func Run(ctx context.Context, a API, o Options) (*Report, error) {
	if o.Cluster == "" && o.ClusterPrefix == "" {
		o.ClusterPrefix = DefaultClusterPrefix
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	s := &sweeper{a: a, o: o, rep: &Report{}, busy: map[string]bool{}}
	if err := s.servers(ctx); err != nil {
		return nil, err
	}
	for _, step := range []func(context.Context) error{s.groups, s.rulesets, s.networks, s.storages} {
		if err := step(ctx); err != nil {
			s.o.Logf("sweep: %v", err)
			s.rep.Items = append(s.rep.Items, Item{Kind: "list", Outcome: Failed, Reason: err.Error()})
		}
	}
	slices.SortStableFunc(s.rep.Items, func(x, y Item) int {
		return strings.Compare(x.Kind+"/"+x.ID, y.Kind+"/"+y.ID)
	})
	return s.rep, nil
}

// selected says whether the cluster label value is one the sweep is about.
func (s *sweeper) selected(cluster string) bool {
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix)
}

func labelOf(ls []upcloud.Label, key string) (string, bool) {
	for _, l := range ls {
		if l.Key == key {
			return l.Value, true
		}
	}
	return "", false
}

// ours returns the cluster of a resource the sweep may touch: its cluster label
// is selected and it is not something an operator offers (scarpenter-use).
func (s *sweeper) ours(ls []upcloud.Label) (cluster string, ok bool) {
	if _, use := labelOf(ls, driver.LabelUse); use {
		return "", false
	}
	c, has := labelOf(ls, ClusterLabel)
	if !has || !s.selected(c) {
		return "", false
	}
	return c, true
}

func (s *sweeper) add(i Item) {
	s.rep.Items = append(s.rep.Items, i)
	s.o.Logf("sweep: %s %s %s (%s) %s", i.Outcome, i.Kind, i.ID, i.Cluster, i.Reason)
}

// act runs del unless the sweep is a dry run, and records the outcome.
func (s *sweeper) act(i Item, del func() error) {
	if s.o.DryRun {
		i.Outcome = WouldDelete
		s.add(i)
		return
	}
	if err := del(); err != nil && !vendors.IsClass(err, vendors.NotFound) {
		i.Outcome, i.Reason = Failed, err.Error()
	} else {
		i.Outcome = Deleted
	}
	s.add(i)
}

// swept says whether a resource of unknown age may be deleted: always without an
// age limit, else when its cluster has no server that may still be in use.
func (s *sweeper) swept(cluster string) (bool, string) {
	if s.o.MaxAge > 0 && s.busy[cluster] {
		return false, "the cluster has a server that is alive and not old enough or of unknown age"
	}
	return true, ""
}

func (s *sweeper) servers(ctx context.Context) error {
	lctx, cancel := driver.CallContext(ctx, driver.CallTimeout)
	list, err := s.a.GetServersWithFilters(lctx, &request.GetServersWithFiltersRequest{
		Filters: []request.QueryFilter{request.FilterLabelKey{Key: ClusterLabel}},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("listing the servers: %w", driver.Classify(err))
	}
	uuids := make([]string, 0, len(list.Servers))
	for _, sv := range list.Servers {
		uuids = append(uuids, sv.UUID)
	}
	slices.Sort(uuids)
	for _, id := range uuids {
		d, err := driver.GetServer(ctx, s.a, id)
		if err != nil {
			if !vendors.IsClass(err, vendors.NotFound) {
				s.add(Item{Kind: KindServer, ID: id, Outcome: Failed, Reason: err.Error()})
			}
			continue
		}
		cluster, ok := s.ours(d.Labels)
		if !ok {
			continue
		}
		it := Item{Kind: KindServer, ID: id, Zone: d.Zone, Cluster: cluster}
		if s.o.MaxAge > 0 {
			created, known := driver.Created(d.Labels)
			switch {
			case !known:
				s.busy[cluster] = true
				it.Outcome, it.Reason = Skipped, "no "+driver.LabelCreated+" label: the age is unknown"
				s.add(it)
				continue
			case s.o.Now().Sub(created) < s.o.MaxAge:
				s.busy[cluster] = true
				it.Outcome = Skipped
				it.Reason = fmt.Sprintf("created %s, younger than %s", created.Format(time.RFC3339), s.o.MaxAge)
				s.add(it)
				continue
			}
		}
		s.act(it, func() error { return driver.Terminate(ctx, s.a, id) })
	}
	return nil
}

func (s *sweeper) groups(ctx context.Context) error {
	cctx, cancel := driver.CallContext(ctx, driver.CallTimeout)
	defer cancel()
	list, err := s.a.GetServerGroups(cctx, &request.GetServerGroupsRequest{
		Filters: []request.QueryFilter{request.FilterLabelKey{Key: ClusterLabel}},
	})
	if err != nil {
		return fmt.Errorf("listing the server groups: %w", driver.Classify(err))
	}
	for _, g := range list {
		cluster, ok := s.ours(g.Labels)
		if !ok {
			continue
		}
		it := Item{Kind: KindGroup, ID: g.UUID, Cluster: cluster}
		if ok, why := s.swept(cluster); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.add(it)
			continue
		}
		s.act(it, func() error {
			return s.call(ctx, func(c context.Context) error {
				return s.a.DeleteServerGroup(c, &request.DeleteServerGroupRequest{UUID: g.UUID})
			})
		})
	}
	return nil
}

func (s *sweeper) rulesets(ctx context.Context) error {
	cctx, cancel := driver.CallContext(ctx, driver.CallTimeout)
	defer cancel()
	list, err := s.a.GetFirewallRulesets(cctx)
	if err != nil {
		return fmt.Errorf("listing the firewall rulesets: %w", driver.Classify(err))
	}
	for _, r := range list {
		labels := r.Labels
		if len(labels) == 0 {
			// The list answer may carry no labels (unverified, plan 19): the details do.
			d, err := s.a.GetFirewallRuleset(cctx, r.UUID)
			if err != nil {
				continue
			}
			labels = d.Labels
		}
		cluster, ok := s.ours(labels)
		if !ok {
			continue
		}
		it := Item{Kind: KindRuleset, ID: r.UUID, Cluster: cluster}
		if ok, why := s.swept(cluster); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.add(it)
			continue
		}
		s.act(it, func() error {
			return s.call(ctx, func(c context.Context) error {
				return s.a.DeleteFirewallRuleset(c, r.UUID)
			})
		})
	}
	return nil
}

func (s *sweeper) networks(ctx context.Context) error {
	cctx, cancel := driver.CallContext(ctx, driver.CallTimeout)
	defer cancel()
	list, err := s.a.GetNetworks(cctx, request.FilterLabelKey{Key: ClusterLabel})
	if err != nil {
		return fmt.Errorf("listing the networks: %w", driver.Classify(err))
	}
	for _, n := range list.Networks {
		if n.Type != upcloud.NetworkTypePrivate {
			continue
		}
		cluster, ok := s.ours(n.Labels)
		if !ok {
			continue
		}
		it := Item{Kind: KindNetwork, ID: n.UUID, Zone: n.Zone, Cluster: cluster}
		if ok, why := s.swept(cluster); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.add(it)
			continue
		}
		s.act(it, func() error {
			return s.call(ctx, func(c context.Context) error {
				return s.a.DeleteNetwork(c, &request.DeleteNetworkRequest{UUID: n.UUID})
			})
		})
	}
	return nil
}

func (s *sweeper) storages(ctx context.Context) error {
	cctx, cancel := driver.CallContext(ctx, driver.CallTimeout)
	defer cancel()
	list, err := s.a.GetStorages(cctx, &request.GetStoragesRequest{
		Access:  upcloud.StorageAccessPrivate,
		Filters: []request.QueryFilter{request.FilterLabelKey{Key: ClusterLabel}},
	})
	if err != nil {
		return fmt.Errorf("listing the storages: %w", driver.Classify(err))
	}
	for _, st := range list.Storages {
		if st.Type == upcloud.StorageTypeTemplate {
			continue // a template is never a node's disk
		}
		cluster, ok := s.ours(st.Labels)
		if !ok {
			continue
		}
		it := Item{Kind: KindStorage, ID: st.UUID, Zone: st.Zone, Cluster: cluster}
		d, err := s.a.GetStorageDetails(cctx, &request.GetStorageDetailsRequest{UUID: st.UUID})
		if err != nil {
			continue // gone meanwhile, or not readable: not ours to guess
		}
		switch {
		case len(d.ServerUUIDs) > 0:
			it.Outcome, it.Reason = Skipped, "attached to a server"
			s.add(it)
			continue
		case d.State != upcloud.StorageStateOnline:
			it.Outcome, it.Reason = Skipped, "state "+d.State+": a create may be in progress"
			s.add(it)
			continue
		}
		if s.o.MaxAge > 0 {
			if created, known := driver.Created(d.Labels); known && s.o.Now().Sub(created) < s.o.MaxAge {
				it.Outcome, it.Reason = Skipped, "younger than "+s.o.MaxAge.String()
				s.add(it)
				continue
			}
			if ok, why := s.swept(cluster); !ok {
				it.Outcome, it.Reason = Skipped, why
				s.add(it)
				continue
			}
		}
		s.act(it, func() error {
			return s.call(ctx, func(c context.Context) error {
				return s.a.DeleteStorage(c, &request.DeleteStorageRequest{
					UUID: st.UUID, Backups: request.DeleteStorageBackupsModeDelete})
			})
		})
	}
	return nil
}

// call runs one API call with the driver's deadline and classifies its error.
func (s *sweeper) call(ctx context.Context, f func(context.Context) error) error {
	c, cancel := driver.CallContext(ctx, driver.CallTimeout)
	defer cancel()
	return driver.Classify(f(c))
}
