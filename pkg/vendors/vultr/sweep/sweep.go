/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the L5 lane left in a
// Vultr account (docs/how-to-guides/testing.md#the-l5-lane): instances, firewall
// groups and VPCs. It is the sweeper workflow's engine (every three hours,
// everything of a cluster named scarpenter-ci-* older than two hours) and the
// e2e workflow's last line of defence (one cluster, no age limit).
//
// It talks to Vultr through the driver's narrow API (vultr.API), so the
// in-memory fake of the driver tests runs it unchanged.
//
// Selection is conservative on purpose:
//
//   - instances are touched only when they carry the cluster tag of a swept
//     cluster;
//   - firewall groups and VPCs have no tags, so only those whose description
//     carries the ownership marker of SCarpenter (vultr.ParseDescription) for a
//     swept cluster, or the lane's own brought objects, described "<cluster>-byo",
//     are touched; and only when no instance uses them;
//   - snapshots are never listed, let alone deleted: the Flatcar snapshot of the
//     lane is permanent, and Vultr cannot share it again.
//
// With an age limit an object must be old enough by its creation time; an
// unknown time is left alone unless the age limit is 0.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

const (
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"
	// ByoSuffix ends the description of the firewall group and the VPC the lane
	// brings itself (bring-your-own scenario): "<cluster>-byo".
	ByoSuffix = "-byo"

	pageSize = 500
	maxPages = 200
)

// Options select what is swept.
type Options struct {
	// Cluster selects one cluster by exact name. Without it ClusterPrefix selects
	// every cluster whose name starts with it (the default prefix when both are
	// empty).
	Cluster       string
	ClusterPrefix string
	// MaxAge is the age an object must have; 0 means any age, also unknown.
	MaxAge time.Duration
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool
	// Regions limits instances and VPCs to these regions; empty means every
	// region. Firewall groups are account-wide.
	Regions []string
	// WaitGone bounds the wait for deleted instances to disappear from the list
	// before the groups and VPCs they used are looked at.
	WaitGone time.Duration
	// PollInterval is the pause between those polls (default 5 s).
	PollInterval time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logf receives one line per decision (default: discard).
	Logf func(format string, args ...any)
}

// Outcome is what happened to an object.
type Outcome string

// The outcomes.
const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would-delete"
	Skipped     Outcome = "skipped"
	Failed      Outcome = "failed"
)

// Item is one object the sweep looked at and acted on or skipped.
type Item struct {
	Region  string
	Kind    string
	ID      string
	Cluster string
	Outcome Outcome
	// Reason says why the object was skipped or what failed.
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

type sweeper struct {
	api vultr.API
	o   Options
	rep *Report
}

// Run sweeps the account. It returns an error only for a failure that makes the
// whole sweep meaningless (a list that cannot be read); failures of single
// objects are items with the outcome Failed.
func Run(ctx context.Context, api vultr.API, o Options) (*Report, error) {
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
	s := &sweeper{api: api, o: o, rep: &Report{}}
	deleted, err := s.instances(ctx)
	if err != nil {
		return s.rep, fmt.Errorf("sweeping the instances: %w", err)
	}
	if len(deleted) > 0 && !o.DryRun {
		s.waitGone(ctx, deleted)
	}
	if err := s.groups(ctx); err != nil {
		return s.rep, fmt.Errorf("sweeping the firewall groups: %w", err)
	}
	if err := s.vpcs(ctx); err != nil {
		return s.rep, fmt.Errorf("sweeping the VPCs: %w", err)
	}
	return s.rep, nil
}

// matches says whether a cluster name is one the sweep is for.
func (s *sweeper) matches(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix)
}

func (s *sweeper) regionOK(region string) bool {
	return len(s.o.Regions) == 0 || slices.Contains(s.o.Regions, region)
}

// oldEnough applies the age limit to a creation stamp.
func (s *sweeper) oldEnough(created string) (bool, string) {
	if s.o.MaxAge == 0 {
		return true, ""
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false, "its creation time is unknown"
	}
	if age := s.o.Now().Sub(t); age < s.o.MaxAge {
		return false, fmt.Sprintf("it is %s old, the limit is %s", age.Round(time.Second), s.o.MaxAge)
	}
	return true, ""
}

func (s *sweeper) record(it Item) {
	s.rep.Items = append(s.rep.Items, it)
	s.o.Logf("%s %s %s (%s)%s", it.Outcome, it.Kind, it.ID, it.Cluster, suffix(it.Reason))
}

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// act deletes unless it is a dry run, and records the outcome.
func (s *sweeper) act(it Item, del func() error) bool {
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.record(it)
		return false
	}
	if err := del(); err != nil && !gone(err) {
		it.Outcome, it.Reason = Failed, err.Error()
		s.record(it)
		return false
	}
	it.Outcome = Deleted
	s.record(it)
	return true
}

// gone says whether an error is Vultr's 404: the object is already deleted.
func gone(err error) bool {
	if se, ok := errors.AsType[vultr.HTTPStatus](err); ok {
		return se.HTTPStatus() == http.StatusNotFound
	}
	return false
}

func page[T any](ctx context.Context,
	fetch func(context.Context, *govultr.ListOptions) ([]T, *govultr.Meta, error)) ([]T, error) {
	var all []T
	opts := govultr.ListOptions{PerPage: pageSize}
	for range maxPages {
		items, meta, err := fetch(ctx, &opts)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if meta == nil || meta.Links == nil || meta.Links.Next == "" || meta.Links.Next == opts.Cursor {
			return all, nil
		}
		opts.Cursor = meta.Links.Next
	}
	return nil, fmt.Errorf("a list does not end after %d pages", maxPages)
}

// instances deletes the instances of the swept clusters and returns their IDs.
func (s *sweeper) instances(ctx context.Context) ([]string, error) {
	all, err := page(ctx, s.api.ListInstances)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, in := range all {
		cluster := vultr.TagStyleDomain.ParseTags(in.Tags)[vendors.TagCluster]
		if !s.matches(cluster) || !s.regionOK(in.Region) {
			continue
		}
		it := Item{Region: in.Region, Kind: "instance", ID: in.ID, Cluster: cluster}
		if ok, why := s.oldEnough(in.DateCreated); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.record(it)
			continue
		}
		if s.act(it, func() error { return s.api.DeleteInstance(ctx, in.ID) }) {
			deleted = append(deleted, in.ID)
		}
	}
	return deleted, nil
}

// waitGone polls until the deleted instances are out of the list, or the wait
// is over (the groups and VPCs are then skipped as still used).
func (s *sweeper) waitGone(ctx context.Context, ids []string) {
	deadline := s.o.Now().Add(s.o.WaitGone)
	for {
		all, err := page(ctx, s.api.ListInstances)
		if err == nil {
			left := 0
			for _, in := range all {
				if slices.Contains(ids, in.ID) {
					left++
				}
			}
			if left == 0 {
				return
			}
		}
		if !s.o.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.o.PollInterval):
		}
	}
}

// owner reads the cluster a firewall group or VPC belongs to from its
// description: SCarpenter's marker, or the lane's "<cluster>-byo".
func owner(desc string, kind vendors.ResourceKind) (cluster string, ok bool) {
	if _, m, parsed := vultr.ParseDescription(desc); parsed && m.ResourceKind == string(kind) {
		return m.Cluster, true
	}
	if c, found := strings.CutSuffix(desc, ByoSuffix); found && c != "" {
		return c, true
	}
	return "", false
}

func (s *sweeper) groups(ctx context.Context) error {
	all, err := page(ctx, s.api.ListFirewallGroups)
	if err != nil {
		return err
	}
	for _, g := range all {
		cluster, ok := owner(g.Description, vultr.KindFirewallGroups)
		if !ok || !s.matches(cluster) {
			continue
		}
		it := Item{Kind: "firewall-group", ID: g.ID, Cluster: cluster}
		if ok, why := s.oldEnough(g.DateCreated); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.record(it)
			continue
		}
		// The list's instance_count may lag the deletion; the group is read again.
		cur, err := s.api.GetFirewallGroup(ctx, g.ID)
		if err != nil {
			it.Outcome, it.Reason = Failed, err.Error()
			s.record(it)
			continue
		}
		if cur.InstanceCount > 0 {
			it.Outcome, it.Reason = Skipped, fmt.Sprintf("%d instance(s) still use it", cur.InstanceCount)
			s.record(it)
			continue
		}
		s.act(it, func() error { return s.api.DeleteFirewallGroup(ctx, g.ID) })
	}
	return nil
}

func (s *sweeper) vpcs(ctx context.Context) error {
	all, err := page(ctx, s.api.ListVPCs)
	if err != nil {
		return err
	}
	for _, v := range all {
		cluster, ok := owner(v.Description, vultr.KindNetworks)
		if !ok || !s.matches(cluster) || !s.regionOK(v.Region) {
			continue
		}
		it := Item{Region: v.Region, Kind: "vpc", ID: v.ID, Cluster: cluster}
		if ok, why := s.oldEnough(v.DateCreated); !ok {
			it.Outcome, it.Reason = Skipped, why
			s.record(it)
			continue
		}
		att, _, err := s.api.ListVPCAttachments(ctx, v.ID, &govultr.ListOptions{PerPage: 1})
		if err != nil {
			it.Outcome, it.Reason = Failed, err.Error()
			s.record(it)
			continue
		}
		if len(att) > 0 {
			it.Outcome, it.Reason = Skipped, "something is still attached to it"
			s.record(it)
			continue
		}
		s.act(it, func() error { return s.api.DeleteVPC(ctx, v.ID) })
	}
	return nil
}
