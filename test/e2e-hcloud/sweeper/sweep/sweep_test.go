/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"slices"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
	"github.com/SpareCores/scarpenter/test/e2e-hcloud/sweeper/sweep"
)

type env struct {
	t   *testing.T
	f   *fake.Fake
	clk *clocktesting.FakeClock
	ips []*hc.PrimaryIP
}

func (e *env) PrimaryIPs(context.Context) ([]*hc.PrimaryIP, error) { return e.ips, nil }

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	return &env{t: t, f: fake.New(fake.WithClock(clk)), clk: clk}
}

func cluster(name string) map[string]string { return map[string]string{sweep.ClusterLabel: name} }

// everything makes one resource of each kind for the cluster and returns the server's ID.
func (e *env) everything(name string) int64 {
	e.t.Helper()
	ctx := context.Background()
	x86, _ := e.f.AddFlatcarImages("stable", "4757.2.3")
	res, err := e.f.CreateServer(ctx, hc.ServerCreateOpts{
		Name: "srv-" + name, ServerType: &hc.ServerType{Name: "cx23"}, Location: &hc.Location{Name: "fsn1"},
		Image: &hc.Image{ID: x86.ID}, Labels: cluster(name), PublicNet: &hc.ServerCreatePublicNet{EnableIPv4: true},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "vol-" + name, Size: 10, Location: &hc.Location{Name: "fsn1"}, Labels: cluster(name)}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg-" + name, Type: hc.PlacementGroupTypeSpread, Labels: cluster(name)}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "fw-" + name, Labels: cluster(name)}); err != nil {
		e.t.Fatal(err)
	}
	return res.Server.ID
}

func (e *env) run(o sweep.Options) *sweep.Report {
	e.t.Helper()
	o.Now = e.clk.Now
	o.PollInterval = time.Millisecond
	rep, err := sweep.Run(context.Background(), e.f, e, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func (e *env) counts() (servers, volumes, groups, firewalls int) {
	ctx := context.Background()
	s, _ := e.f.Servers(ctx, hc.ServerListOpts{})
	v, _ := e.f.Volumes(ctx, hc.VolumeListOpts{})
	g, _ := e.f.PlacementGroups(ctx, hc.PlacementGroupListOpts{})
	f, _ := e.f.Firewalls(ctx, hc.FirewallListOpts{})
	return len(s), len(v), len(g), len(f)
}

func TestOldClustersAreSweptYoungOnesAreNot(t *testing.T) {
	e := newEnv(t)
	e.everything("scarpenter-ci-1")
	e.clk.Step(3 * time.Hour)
	e.everything("scarpenter-ci-2") // young
	e.f.Tick()
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Deleted) != 4 || rep.Count(sweep.Failed) != 0 {
		t.Fatalf("%+v", rep.Items)
	}
	if s, v, g, f := e.counts(); s != 1 || v != 1 || g != 1 || f != 1 {
		t.Errorf("the young cluster keeps its resources: %d %d %d %d", s, v, g, f)
	}
	if rep.Count(sweep.Skipped) != 4 {
		t.Errorf("the young resources are reported skipped: %+v", rep.Items)
	}
}

func TestServersGoBeforeWhatTheyUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// One server attached to a volume, a group and a firewall: they can only be deleted once it is gone.
	x86, _ := e.f.AddFlatcarImages("stable", "4757.2.3")
	vol, _ := e.f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "v", Size: 10, Location: &hc.Location{Name: "fsn1"}, Labels: cluster("scarpenter-ci-9")})
	pg, _ := e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "p", Type: hc.PlacementGroupTypeSpread, Labels: cluster("scarpenter-ci-9")})
	fw, _ := e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "f", Labels: cluster("scarpenter-ci-9")})
	if _, err := e.f.CreateServer(ctx, hc.ServerCreateOpts{
		Name: "s", ServerType: &hc.ServerType{Name: "cx23"}, Location: &hc.Location{Name: "fsn1"}, Image: &hc.Image{ID: x86.ID},
		Labels: cluster("scarpenter-ci-9"), Volumes: []*hc.Volume{vol}, PlacementGroup: pg,
		Firewalls: []*hc.ServerCreateFirewall{{Firewall: *fw}}, PublicNet: &hc.ServerCreatePublicNet{EnableIPv4: true},
	}); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(3 * time.Hour)
	// A deleting server is removed by the next Tick; the sweep waits for that.
	go func() {
		for range 200 {
			time.Sleep(2 * time.Millisecond)
			e.f.Tick()
		}
	}()
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour, WaitDeleted: time.Second})
	if rep.Count(sweep.Failed) != 0 || rep.Count(sweep.Deleted) != 4 {
		t.Fatalf("%+v", rep.Items)
	}
	if s, v, g, f := e.counts(); s+v+g+f != 0 {
		t.Errorf("left: %d %d %d %d", s, v, g, f)
	}
	// The order of the report is the order of the deletions.
	kinds := make([]string, 0, len(rep.Items))
	for _, it := range rep.Items {
		kinds = append(kinds, it.Kind)
	}
	if !slices.Equal(kinds, []string{"server", "volume", "placement-group", "firewall"}) {
		t.Errorf("order %v", kinds)
	}
}

func TestExactClusterIgnoresAgeAndSiblings(t *testing.T) {
	e := newEnv(t)
	e.everything("scarpenter-ci-1")
	e.everything("scarpenter-ci-12") // a sibling the bare prefix of -cluster must not match
	e.f.Tick()
	rep := e.run(sweep.Options{Cluster: "scarpenter-ci-1"})
	if rep.Count(sweep.Deleted) != 4 {
		t.Fatalf("%+v", rep.Items)
	}
	if s, _, _, _ := e.counts(); s != 1 {
		t.Errorf("the sibling's server is kept: %d", s)
	}
}

func TestOtherPrefixesAndUnlabelledObjectsAreNeverTouched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.everything("production-cluster")
	_, _ = e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "unlabelled"})
	_, _ = e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "other-label", Labels: map[string]string{"team": "x"}})
	e.clk.Step(48 * time.Hour)
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour})
	if len(rep.Items) != 0 {
		t.Errorf("%+v", rep.Items)
	}
	if s, v, g, f := e.counts(); s != 1 || v != 1 || g != 1 || f != 3 {
		t.Errorf("%d %d %d %d", s, v, g, f)
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	e := newEnv(t)
	e.everything("scarpenter-ci-1")
	e.clk.Step(3 * time.Hour)
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour, DryRun: true})
	if rep.Count(sweep.WouldDelete) != 4 || rep.Count(sweep.Deleted) != 0 {
		t.Fatalf("%+v", rep.Items)
	}
	if s, v, g, f := e.counts(); s != 1 || v != 1 || g != 1 || f != 1 {
		t.Errorf("%d %d %d %d", s, v, g, f)
	}
}

func TestSnapshotsAreNeverListed(t *testing.T) {
	e := newEnv(t)
	e.f.AddImage(hc.ArchitectureX86, map[string]string{sweep.ClusterLabel: "scarpenter-ci-1", "os": "flatcar"})
	e.clk.Step(48 * time.Hour)
	e.run(sweep.Options{MaxAge: 0, Cluster: "scarpenter-ci-1"})
	if n := e.f.Calls("Images"); n != 0 {
		t.Errorf("the sweep listed images %d times", n)
	}
	if ims, _ := e.f.Images(context.Background(), hc.ImageListOpts{}); len(ims) != 1 {
		t.Error("a snapshot is gone")
	}
}

func TestOnlyUnassignedPrimaryIPsOfTheClusterAreDeleted(t *testing.T) {
	e := newEnv(t)
	old := e.clk.Now().Add(-5 * time.Hour)
	e.ips = []*hc.PrimaryIP{
		{ID: 1, Name: "free", Labels: cluster("scarpenter-ci-1"), Created: old},
		{ID: 2, Name: "assigned", Labels: cluster("scarpenter-ci-1"), Created: old, AssigneeID: 77},
		{ID: 3, Name: "foreign", Labels: cluster("keep-me"), Created: old},
		{ID: 4, Name: "young", Labels: cluster("scarpenter-ci-1"), Created: e.clk.Now()},
	}
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Deleted) != 1 || e.f.Calls("DeletePrimaryIP") != 1 {
		t.Errorf("%+v, %d calls", rep.Items, e.f.Calls("DeletePrimaryIP"))
	}
}

func TestAFailedDeletionIsReportedAndTheRestGoOn(t *testing.T) {
	e := newEnv(t)
	e.everything("scarpenter-ci-1")
	e.clk.Step(3 * time.Hour)
	e.f.Inject("DeleteVolume", fake.APIError(hc.ErrorCodeLocked, "locked"), -1)
	e.f.Tick()
	rep := e.run(sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Failed) != 1 || rep.Count(sweep.Deleted) != 3 {
		t.Errorf("%+v", rep.Items)
	}
}
