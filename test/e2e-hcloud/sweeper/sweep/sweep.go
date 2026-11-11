/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the Hetzner Cloud L5 lane left in a
// project: servers, volumes, placement groups, firewalls and unassigned Primary IPs, selected by
// the label scarpenter.sparecores.com/cluster. It is the sweeper
// workflow's engine (every three hours, everything of a cluster named scarpenter-ci-* older
// than two hours) and the lane's last line of defence (one cluster, no age limit).
//
// It talks to the project through the driver's narrow API (pkg/vendors/hcloud.API), so the
// in-memory fake of the driver tests runs it unchanged; the one call that API lacks, the list of
// Primary IPs, is the IPLister.
//
// Selection is conservative on purpose. The API cannot match a label value by prefix, so each
// kind is listed by the presence of the cluster label (one call per kind) and filtered here:
// a resource is touched only when its cluster label matches (exactly, or by prefix) and, with an
// age limit, when it is old enough (every kind has a creation time). A resource without the
// label is never listed. Images (the Flatcar snapshots, which every run needs) are never
// listed at all, so they cannot be deleted. Deletion order: servers (waited for), volumes,
// placement groups, firewalls, Primary IPs. The network of the bring-your-own scenario is not here:
// the API interface of the driver cannot delete one, so the lane's teardown deletes it with the CLI.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

const (
	// ClusterLabel is the label every SCarpenter resource carries with the cluster name.
	ClusterLabel = "scarpenter.sparecores.com/cluster"
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"
	// CreatedAtLabel is the label the lane's NodeClasses and the workflow put on everything
	// they make (Unix seconds); the sweep decides by the API's own creation time, not by it.
	CreatedAtLabel = "ci-created-at"
)

// IPLister lists the project's Primary IPs (hcloud.API has no list call).
type IPLister interface {
	PrimaryIPs(ctx context.Context) ([]*hc.PrimaryIP, error)
}

// Options select what is swept.
type Options struct {
	// Cluster selects the resources of one cluster by exact name; without it ClusterPrefix
	// (DefaultClusterPrefix when empty) selects every cluster whose name starts with it.
	Cluster       string
	ClusterPrefix string
	// MaxAge is the age a resource must have; 0 means any age.
	MaxAge time.Duration
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool
	// WaitDeleted bounds the wait for the deleted servers to be gone before the resources they
	// used are deleted.
	WaitDeleted time.Duration
	// PollInterval is the pause between the polls of that wait (default 5 s).
	PollInterval time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logf receives one line per decision.
	Logf func(format string, args ...any)
}

// Outcome is what happened to one resource.
type Outcome string

// The outcomes.
const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would delete"
	Skipped     Outcome = "skipped"
	Failed      Outcome = "failed"
)

// Item is one resource the sweep looked at and acted on.
type Item struct {
	Kind    string
	ID      int64
	Name    string
	Cluster string
	Outcome Outcome
	Reason  string
}

// Report is what a sweep did.
type Report struct{ Items []Item }

// Count is the number of items with the outcome.
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
	api hcloud.API
	ips IPLister
	o   Options
	rep *Report
}

// Run sweeps the project. A failed deletion is in the report (and does not stop the others);
// the error is only for what stops the sweep: a list that failed.
func Run(ctx context.Context, api hcloud.API, ips IPLister, o Options) (*Report, error) {
	if o.Cluster == "" && o.ClusterPrefix == "" {
		o.ClusterPrefix = DefaultClusterPrefix
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.PollInterval == 0 {
		o.PollInterval = 5 * time.Second
	}
	s := &sweeper{api: api, ips: ips, o: o, rep: &Report{}}
	var errs []error
	for _, step := range []func(context.Context) error{
		s.servers, s.volumes, s.placementGroups, s.firewalls, s.primaryIPs,
	} {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return s.rep, errors.Join(errs...)
}

// matches says whether a resource with these labels and creation time is the sweep's, and why not.
func (s *sweeper) matches(labels map[string]string, created time.Time) (cluster string, ok bool, why string) {
	cluster, has := labels[ClusterLabel]
	switch {
	case !has:
		return "", false, "no cluster label"
	case s.o.Cluster != "" && cluster != hcloud.LabelValue(s.o.Cluster):
		return cluster, false, "another cluster"
	case s.o.Cluster == "" && !strings.HasPrefix(cluster, s.o.ClusterPrefix):
		return cluster, false, "not a cluster of the prefix " + s.o.ClusterPrefix
	}
	if s.o.MaxAge > 0 {
		if created.IsZero() {
			return cluster, false, "no creation time"
		}
		if age := s.o.Now().Sub(created); age < s.o.MaxAge {
			return cluster, false, fmt.Sprintf("only %s old", age.Round(time.Second))
		}
	}
	return cluster, true, ""
}

// handle records and performs one deletion.
func (s *sweeper) handle(
	kind string, id int64, name string, labels map[string]string, created time.Time, del func() error,
) {
	cluster, ok, why := s.matches(labels, created)
	if !ok {
		if _, has := labels[ClusterLabel]; has && why != "another cluster" && !strings.HasPrefix(why, "not a cluster") {
			s.add(Item{Kind: kind, ID: id, Name: name, Cluster: cluster, Outcome: Skipped, Reason: why})
		}
		return
	}
	it := Item{Kind: kind, ID: id, Name: name, Cluster: cluster}
	switch {
	case s.o.DryRun:
		it.Outcome = WouldDelete
	default:
		if err := del(); err != nil && !isNotFound(err) {
			it.Outcome, it.Reason = Failed, err.Error()
		} else {
			it.Outcome = Deleted
		}
	}
	s.add(it)
}

func (s *sweeper) add(it Item) {
	s.rep.Items = append(s.rep.Items, it)
	extra := ""
	if it.Reason != "" {
		extra = ": " + it.Reason
	}
	s.o.Logf("%s %s %d (%s) of %s%s", it.Outcome, it.Kind, it.ID, it.Name, it.Cluster, extra)
}

func isNotFound(err error) bool { return hc.IsError(err, hc.ErrorCodeNotFound) }

// present is the label selector that lists everything that has a cluster label.
const present = ClusterLabel

func (s *sweeper) servers(ctx context.Context) error {
	list, err := s.api.Servers(ctx, hc.ServerListOpts{ListOpts: hc.ListOpts{LabelSelector: present}})
	if err != nil {
		return fmt.Errorf("listing servers: %w", err)
	}
	var deleted []int64
	for _, sv := range list {
		before := len(s.rep.Items)
		s.handle("server", sv.ID, sv.Name, sv.Labels, sv.Created, func() error {
			act, err := s.api.DeleteServer(ctx, sv)
			if err != nil {
				return err
			}
			if act != nil {
				return s.api.WaitForAction(ctx, act)
			}
			return nil
		})
		if len(s.rep.Items) > before && s.rep.Items[len(s.rep.Items)-1].Outcome == Deleted {
			deleted = append(deleted, sv.ID)
		}
	}
	if len(deleted) == 0 || s.o.DryRun {
		return nil
	}
	return s.waitGone(ctx, deleted)
}

// waitGone waits for the servers to disappear, so that volumes, groups and firewalls are free.
func (s *sweeper) waitGone(ctx context.Context, ids []int64) error {
	deadline := s.o.Now().Add(s.o.WaitDeleted)
	for {
		left := 0
		for _, id := range ids {
			sv, err := s.api.GetServer(ctx, id)
			if err != nil {
				return fmt.Errorf("waiting for server %d: %w", id, err)
			}
			if sv != nil {
				left++
			}
		}
		if left == 0 {
			return nil
		}
		if !s.o.Now().Before(deadline) {
			s.o.Logf("%d deleted server(s) are still there; what depends on them may stay until the next sweep", left)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.o.PollInterval):
		}
	}
}

func (s *sweeper) volumes(ctx context.Context) error {
	list, err := s.api.Volumes(ctx, hc.VolumeListOpts{ListOpts: hc.ListOpts{LabelSelector: present}})
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}
	for _, v := range list {
		s.handle("volume", v.ID, v.Name, v.Labels, v.Created, func() error { return s.api.DeleteVolume(ctx, v) })
	}
	return nil
}

func (s *sweeper) placementGroups(ctx context.Context) error {
	list, err := s.api.PlacementGroups(ctx, hc.PlacementGroupListOpts{ListOpts: hc.ListOpts{LabelSelector: present}})
	if err != nil {
		return fmt.Errorf("listing placement groups: %w", err)
	}
	for _, g := range list {
		s.handle("placement-group", g.ID, g.Name, g.Labels, g.Created,
			func() error { return s.api.DeletePlacementGroup(ctx, g) })
	}
	return nil
}

func (s *sweeper) firewalls(ctx context.Context) error {
	list, err := s.api.Firewalls(ctx, hc.FirewallListOpts{ListOpts: hc.ListOpts{LabelSelector: present}})
	if err != nil {
		return fmt.Errorf("listing firewalls: %w", err)
	}
	for _, f := range list {
		s.handle("firewall", f.ID, f.Name, f.Labels, f.Created, func() error { return s.api.DeleteFirewall(ctx, f) })
	}
	return nil
}

func (s *sweeper) primaryIPs(ctx context.Context) error {
	if s.ips == nil {
		return nil
	}
	list, err := s.ips.PrimaryIPs(ctx)
	if err != nil {
		return fmt.Errorf("listing Primary IPs: %w", err)
	}
	for _, ip := range list {
		if ip.AssigneeID != 0 {
			continue // an assigned one goes with its server or is not ours to judge
		}
		s.handle("primary-ip", ip.ID, ip.Name, ip.Labels, ip.Created, func() error { return s.api.DeletePrimaryIP(ctx, ip) })
	}
	return nil
}
