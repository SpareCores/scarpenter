/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/sweep"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

const (
	ci      = "scarpenter-ci-100"
	otherCI = "scarpenter-ci-200"
)

func tags(cluster string) []string {
	return []string{"scarpenter.sparecores.com/cluster=" + cluster, "scarpenter.sparecores.com/managed-by=scarpenter"}
}

func marker(kind, cluster string) string {
	return "scarpenter-" + cluster + "-" + kind + "-ewr | cluster=" + cluster +
		" nodeclass=default managed-by=scarpenter resource-kind=" + kind + " region=ewr"
}

type world struct {
	a *fake.Account
	// the clock moves between creations
	now time.Time
}

func newWorld() *world {
	a := fake.New()
	a.AddRegion("ewr", "vc2-1c-1gb")
	a.AddRegion("fra", "vc2-1c-1gb")
	w := &world{a: a, now: t0}
	a.SetClock(func() time.Time { return w.now })
	return w
}

func (w *world) at(t time.Time) { w.now = t }

func run(t *testing.T, w *world, o sweep.Options) *sweep.Report {
	t.Helper()
	o.Now = func() time.Time { return t0.Add(3 * time.Hour) }
	o.PollInterval = time.Millisecond
	rep, err := sweep.Run(context.Background(), w.a, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestSweepsOldClusterObjectsAndNothingElse(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	// Old, of a lane cluster.
	old, _ := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(ci)})
	g, _ := w.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: marker("firewallGroups", ci)})
	byo, _ := w.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: ci + "-byo"})
	_, _ = w.a.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: marker("networks", ci)})
	// Not ours: another prefix, a user's own objects, a snapshot.
	prod, _ := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags("prod")})
	foreign := w.a.AddForeignInstance("ewr", "vc2-1c-1gb", "team=infra")
	userG, _ := w.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "the operator's group"})
	prodG, _ := w.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: marker("firewallGroups", "prod")})
	snap := w.a.AddSnapshotAs("", "flatcar-stable-4757.2.3-amd64", fake.SnapshotComplete, time.Time{})
	// Too young.
	w.at(t0.Add(2*time.Hour + 30*time.Minute))
	young, _ := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(otherCI)})

	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Failed) != 0 || rep.Count(sweep.Deleted) != 4 || rep.Count(sweep.Skipped) != 1 {
		t.Fatalf("report: %+v", rep.Items)
	}
	ids := map[string]bool{}
	for _, in := range w.a.Instances() {
		ids[in.ID] = true
	}
	if ids[old.ID] || !ids[prod.ID] || !ids[foreign] || !ids[young.ID] {
		t.Errorf("instances left: %v", ids)
	}
	left := map[string]bool{}
	for _, x := range w.a.Groups() {
		left[x.ID] = true
	}
	if left[g.ID] || left[byo.ID] || !left[userG.ID] || !left[prodG.ID] {
		t.Errorf("groups left: %v", left)
	}
	if len(w.a.VPCs()) != 0 {
		t.Errorf("the VPC is still there")
	}
	if snap == "" {
		t.Error("a snapshot was touched")
	}
	if w.a.Calls("ListSnapshots") != 0 {
		t.Error("snapshots were even listed")
	}
}

func TestOneClusterNoAgeLimitAndDryRun(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	w.a.AddForeignInstance("ewr", "vc2-1c-1gb")
	mine, _ := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(ci)})
	_, _ = w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(otherCI)})

	dry := run(t, w, sweep.Options{Cluster: ci, DryRun: true})
	if dry.Count(sweep.WouldDelete) != 1 || len(w.a.Instances()) != 3 {
		t.Fatalf("dry run: %+v", dry.Items)
	}
	rep := run(t, w, sweep.Options{Cluster: ci})
	if rep.Count(sweep.Deleted) != 1 {
		t.Fatalf("report: %+v", rep.Items)
	}
	for _, in := range w.a.Instances() {
		if in.ID == mine.ID {
			t.Error("the cluster's instance is still there")
		}
	}
	if len(w.a.Instances()) != 2 {
		t.Errorf("%d instances left, want the foreign one and the other cluster's", len(w.a.Instances()))
	}
}

func TestUsedObjectsAreSkippedUntilTheInstanceIsGone(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	g, _ := w.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: marker("firewallGroups", ci)})
	v, _ := w.a.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: marker("networks", ci)})
	// An instance of ANOTHER cluster holds them: it is not swept, so they stay.
	if _, err := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{
		Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags("prod"), FirewallGroupID: g.ID, AttachVPC: []string{v.ID},
	}); err != nil {
		t.Fatal(err)
	}
	rep := run(t, w, sweep.Options{Cluster: ci})
	if rep.Count(sweep.Skipped) != 2 || rep.Count(sweep.Deleted) != 0 || len(w.a.Groups()) != 1 || len(w.a.VPCs()) != 1 {
		t.Fatalf("report: %+v", rep.Items)
	}
	// An instance of the swept cluster that uses them goes first, then they do.
	w2 := newWorld()
	g2, _ := w2.a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: marker("firewallGroups", ci)})
	v2, _ := w2.a.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: marker("networks", ci)})
	if _, err := w2.a.CreateInstance(ctx, &govultr.InstanceCreateReq{
		Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(ci), FirewallGroupID: g2.ID, AttachVPC: []string{v2.ID},
	}); err != nil {
		t.Fatal(err)
	}
	rep = run(t, w2, sweep.Options{Cluster: ci})
	if rep.Count(sweep.Deleted) != 3 || len(w2.a.Groups()) != 0 || len(w2.a.VPCs()) != 0 || len(w2.a.Instances()) != 0 {
		t.Fatalf("report: %+v", rep.Items)
	}
}

func TestRegionsLimitInstancesAndVPCs(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	_, _ = w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(ci)})
	_, _ = w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "fra", Plan: "vc2-1c-1gb", Tags: tags(ci)})
	_, _ = w.a.CreateVPC(ctx, &govultr.VPCReq{Region: "fra", Description: marker("networks", ci)})
	rep := run(t, w, sweep.Options{Cluster: ci, Regions: []string{"ewr"}})
	if rep.Count(sweep.Deleted) != 1 || len(w.a.Instances()) != 1 || len(w.a.VPCs()) != 1 {
		t.Fatalf("report: %+v", rep.Items)
	}
}

func TestFailuresAreItemsAndListErrorsAreErrors(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	in, _ := w.a.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", Tags: tags(ci)})
	w.a.FailNext("DeleteInstance", 1, &fake.Error{Status: 503, Message: "down"})
	rep := run(t, w, sweep.Options{Cluster: ci})
	if rep.Count(sweep.Failed) != 1 {
		t.Fatalf("report: %+v", rep.Items)
	}
	// A 404 on delete is a success: it is gone.
	w.a.FailNext("DeleteInstance", 1, &fake.Error{Status: 404, Message: fake.MsgNotFound})
	if rep = run(t, w, sweep.Options{Cluster: ci}); rep.Count(sweep.Deleted) != 1 {
		t.Fatalf("report: %+v (%s)", rep.Items, in.ID)
	}
	for _, m := range []string{"ListInstances", "ListFirewallGroups", "ListVPCs"} {
		w.a.FailNext(m, 1, &fake.Error{Status: 503, Message: "down"})
		if _, err := sweep.Run(ctx, w.a, sweep.Options{Cluster: ci}); err == nil {
			t.Errorf("a failing %s did not fail the sweep", m)
		}
	}
}
