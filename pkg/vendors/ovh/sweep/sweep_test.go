/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

var ctx = context.Background()

const (
	region  = "GRA11"
	cluster = "scarpenter-ci-1234"
)

type world struct {
	cloud *fake.Cloud
	now   time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	c := fake.New()
	c.DeleteReads = 1
	c.Now = func() time.Time { return w.now }
	c.AddRegion(region, fake.RegionConfig{Flavors: []osapi.Flavor{{Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50}}})
	c.AddRegion("SBG5", fake.RegionConfig{NoEndpoint: true})
	c.AddImage(region, osapi.Image{ID: "image", Name: "flatcar"})
	w.cloud = c
	return w
}

func (w *world) driver(t *testing.T, cl string) *ovh.Driver {
	t.Helper()
	d := ovh.New(ovh.WithProvider(w.cloud), ovh.WithScheduleWait(ovh.WaitOptions{Timeout: time.Second, Interval: time.Millisecond}))
	if err := d.Init(ctx, vendors.Config{ClusterName: cl, Clock: clocktesting.NewFakeClock(w.now), Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return d
}

// server launches a node of the cluster through the driver, aged by the fake's clock.
func (w *world) server(t *testing.T, cl, name string, age time.Duration) string {
	t.Helper()
	w.now = w.now.Add(-age)
	defer func() { w.now = w.now.Add(age) }()
	inst, err := w.driver(t, cl).CreateInstance(ctx, vendors.CreateRequest{
		Region: region, Zone: "nova", InstanceType: "b3-8", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image: vendors.Image{Ref: "image"}, UserData: []byte("{}"),
		Settings: vendors.VendorSettings{Raw: json.RawMessage(`{}`)},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: cl, vendors.TagNodeClaim: name, vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name, Node: vendors.NodeInfo{Name: name},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inst.Ref.ID()
}

func (w *world) group(t *testing.T, cl, name string, age time.Duration) osapi.SecurityGroup {
	t.Helper()
	w.now = w.now.Add(-age)
	defer func() { w.now = w.now.Add(age) }()
	d := w.driver(t, cl)
	// The driver stamps the marker with its own clock: make it the fake's.
	ref, _, err := d.Ensure(ctx, ovh.KindSecurityGroups, region, vendors.CreateSpec{Name: name, Tags: map[vendors.TagKey]string{
		vendors.TagCluster: cl, vendors.TagNodeClass: "c", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: string(ovh.KindSecurityGroups),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return osapi.SecurityGroup{ID: ref.ID, Name: name}
}

func (w *world) sweep(t *testing.T, o Options) *Report {
	t.Helper()
	o.Now = func() time.Time { return w.now }
	o.WaitDeleted, o.PollInterval = time.Second, time.Millisecond
	rep, err := Run(ctx, w.cloud, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestParseRegions(t *testing.T) {
	for in, want := range map[string]int{"": 0, "all": 0, "GRA11": 1, "GRA11, SBG5": 2} {
		got, err := ParseRegions(in)
		if err != nil || len(got) != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseRegions("GRA 11"); err == nil {
		t.Error("a name with a space")
	}
	if _, err := ParseRegions("GRA11,,SBG5"); err == nil {
		t.Error("an empty name")
	}
}

func TestSweepsOldClustersOnly(t *testing.T) {
	w := newWorld(t)
	oldSrv := w.server(t, cluster, "old", 3*time.Hour)
	newSrv := w.server(t, cluster, "new", 10*time.Minute)
	otherSrv := w.server(t, "production", "prod", 5*time.Hour) // another cluster
	foreign := w.cloud.AddForeignServer(region, "somebody-else", "b3-8")
	rep := w.sweep(t, Options{MaxAge: 2 * time.Hour})
	if rep.Count(Deleted) != 1 || rep.Count(Failed) != 0 {
		t.Fatalf("%+v", rep.Items)
	}
	// What is left is listed through the fake's reads, which also finish the delete.
	cl, _ := w.cloud.Clients(ctx, region)
	var ids []string
	for range 3 {
		ids = ids[:0]
		servers, _ := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
		for _, s := range servers {
			ids = append(ids, s.ID)
		}
	}
	for _, want := range []string{newSrv, otherSrv, foreign} {
		if !contains(ids, want) {
			t.Errorf("%s was swept: %v", want, ids)
		}
	}
	if contains(ids, oldSrv) {
		t.Errorf("the old server is still there: %v", ids)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestDryRunAndOneCluster(t *testing.T) {
	w := newWorld(t)
	a := w.server(t, cluster, "a", 3*time.Hour)
	w.server(t, "scarpenter-ci-9999", "b", 3*time.Hour)
	rep := w.sweep(t, Options{MaxAge: 2 * time.Hour, DryRun: true})
	if rep.Count(WouldDelete) != 2 || rep.Count(Deleted) != 0 {
		t.Fatalf("%+v", rep.Items)
	}
	// One cluster, any age: the workflow's own last line of defence.
	fresh := w.server(t, cluster, "fresh", 0)
	rep = w.sweep(t, Options{Cluster: cluster})
	var got []string
	for _, i := range rep.Items {
		if i.Outcome == Deleted {
			got = append(got, i.ID)
		}
	}
	if len(got) != 2 || !contains(got, a) || !contains(got, fresh) {
		t.Errorf("deleted %v", got)
	}
}

func TestGroupsAreSweptAfterTheirServers(t *testing.T) {
	w := newWorld(t)
	w.server(t, cluster, "old", 3*time.Hour)
	sg := w.group(t, cluster, "scarpenter-scarpenter-ci-1-abc-securitygroups-gra11", 3*time.Hour)
	young := w.group(t, "scarpenter-ci-7777", "scarpenter-scarpenter-ci-7-abc-securitygroups-gra11", time.Hour)
	cl, _ := w.cloud.Clients(ctx, region)
	user, _ := cl.Network.CreateSecurityGroup(ctx, "scarpenter-scarpenter-ci-user", "a user group")
	grp, _ := cl.Compute.CreateServerGroup(ctx, resources.NamePrefix(cluster)+"servergroups-gra11-abc", "affinity")
	used, _ := cl.Compute.CreateServerGroup(ctx, resources.NamePrefix(cluster)+"servergroups-gra11-def", "affinity")
	_ = used
	rep := w.sweep(t, Options{MaxAge: 2 * time.Hour})
	by := map[string]Item{}
	for _, i := range rep.Items {
		by[i.ID] = i
	}
	if by[sg.ID].Outcome != Deleted {
		t.Errorf("the old cluster's group: %+v", by[sg.ID])
	}
	if by[young.ID].Outcome != Skipped {
		t.Errorf("a young cluster's group: %+v", by[young.ID])
	}
	if _, touched := by[user.ID]; touched {
		t.Errorf("a group without the marker is not ours: %+v", by[user.ID])
	}
	if by[grp.ID].Outcome != Deleted {
		t.Errorf("the old cluster's server group: %+v", by[grp.ID])
	}
	// The project's default group stays whatever it is called.
	groups, _ := cl.Network.ListSecurityGroups(ctx)
	if !hasName(groups, "default") || !hasName(groups, user.Name) {
		t.Errorf("groups %+v", groups)
	}
}

func hasName(gs []osapi.SecurityGroup, n string) bool {
	for _, g := range gs {
		if g.Name == n {
			return true
		}
	}
	return false
}

func TestAServerThatStaysKeepsItsGroups(t *testing.T) {
	w := newWorld(t)
	w.group(t, cluster, "scarpenter-scarpenter-ci-1-abc-securitygroups-gra11", 3*time.Hour)
	w.server(t, cluster, "young", 5*time.Minute)
	cl, _ := w.cloud.Clients(ctx, region)
	_, _ = cl.Compute.CreateServerGroup(ctx, resources.NamePrefix(cluster)+"servergroups-gra11-abc", "affinity")
	rep := w.sweep(t, Options{MaxAge: 2 * time.Hour})
	if rep.Count(Deleted) != 0 || rep.Count(Skipped) < 2 {
		t.Errorf("%+v", rep.Items)
	}
}

func TestRegionsAndFailures(t *testing.T) {
	w := newWorld(t)
	// Every region of the catalog: SBG5 has no endpoint and is skipped, not a failure.
	rep := w.sweep(t, Options{})
	if rep.Count(Failed) != 0 {
		t.Errorf("%+v", rep.Items)
	}
	// A requested region that cannot be reached is a failure.
	rep = w.sweep(t, Options{Regions: []string{"SBG5"}})
	if rep.Count(Failed) != 1 {
		t.Errorf("%+v", rep.Items)
	}
	w.cloud.InjectError(region, "ListServers", errors.New("boom"), 0)
	rep = w.sweep(t, Options{Regions: []string{region}})
	if rep.Count(Failed) < 1 {
		t.Errorf("a failing list: %+v", rep.Items)
	}
	w.cloud.ClearErrors()
	w.cloud.InjectError("", "Regions", errors.New("no catalog"), 0)
	if _, err := Run(ctx, w.cloud, Options{}); err == nil {
		t.Error("no region list is an error")
	}
}

func TestADeleteThatFails(t *testing.T) {
	w := newWorld(t)
	w.server(t, cluster, "old", 3*time.Hour)
	w.cloud.InjectError(region, "DeleteServer", &osapi.APIError{Service: "nova", Status: 503}, 0)
	rep := w.sweep(t, Options{MaxAge: 2 * time.Hour})
	if rep.Count(Failed) != 1 || rep.Count(Deleted) != 0 {
		t.Errorf("%+v", rep.Items)
	}
}
