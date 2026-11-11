/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/sweep"
)

var now = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

type account struct {
	t *testing.T
	f *fake.Fake
}

func newAccount(t *testing.T) *account {
	return &account{t: t, f: fake.New(fake.Options{AutoSettle: true})}
}

func labels(cluster string, age time.Duration, extra ...string) map[string]string {
	m := map[string]string{}
	if cluster != "" {
		m["scarpenter-cluster"] = cluster
	}
	if age >= 0 {
		m["scarpenter-created"] = now.Add(-age).Format(time.RFC3339)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func (a *account) server(title string, l map[string]string) string {
	return a.f.AddServer(fake.ServerSpec{Zone: "fi-hel1", Plan: "1xCPU-1GB", Title: title, Labels: l})
}

func (a *account) network(name string, l map[string]string) string {
	ls := make([]upcloud.Label, 0, len(l))
	for k, v := range l {
		ls = append(ls, upcloud.Label{Key: k, Value: v})
	}
	n, err := a.f.CreateNetwork(context.Background(), &request.CreateNetworkRequest{
		Name: name, Zone: "fi-hel1", Labels: ls,
		IPNetworks: upcloud.IPNetworkSlice{{Address: "10.9." + string(rune('0'+len(name)%9)) + ".0/24", DHCP: upcloud.True, Family: "IPv4"}},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return n.UUID
}

func (a *account) ruleset(name string, l map[string]string) string {
	ls := make([]upcloud.Label, 0, len(l))
	for k, v := range l {
		ls = append(ls, upcloud.Label{Key: k, Value: v})
	}
	r, err := a.f.CreateFirewallRuleset(context.Background(), api.CreateRulesetRequest{Name: name, Labels: ls})
	if err != nil {
		a.t.Fatal(err)
	}
	return r.UUID
}

func (a *account) group(title string, l map[string]string) string {
	ls := make(upcloud.LabelSlice, 0, len(l))
	for k, v := range l {
		ls = append(ls, upcloud.Label{Key: k, Value: v})
	}
	g, err := a.f.CreateServerGroup(context.Background(), &request.CreateServerGroupRequest{Title: title, Labels: &ls})
	if err != nil {
		a.t.Fatal(err)
	}
	return g.UUID
}

func (a *account) run(o sweep.Options) *sweep.Report {
	a.t.Helper()
	o.Now = func() time.Time { return now }
	rep, err := sweep.Run(context.Background(), a.f, o)
	if err != nil {
		a.t.Fatal(err)
	}
	return rep
}

func outcome(rep *sweep.Report, id string) sweep.Outcome {
	for _, i := range rep.Items {
		if i.ID == id {
			return i.Outcome
		}
	}
	return ""
}

func TestSweepsOldClusterResourcesOnly(t *testing.T) {
	a := newAccount(t)
	old := labels("scarpenter-ci-1", 5*time.Hour)
	oldSrv := a.server("old", old)
	foreign := a.server("foreign", labels("production", 5*time.Hour))
	unlabelled := a.server("plain", labels("", -1))
	netID, rsID := a.network("n", old), a.ruleset("r", old)
	grpID := a.group("g", old)
	orphan := a.f.AddStorage("fi-hel1", "orphan", "maxiops", 25, old)
	tmpl := a.f.AddTemplate("fi-hel1", "flatcar", "maxiops", 10, labels("scarpenter-ci-1", 9*time.Hour, "scarpenter-use", "true"))
	useNet := a.network("use", labels("scarpenter-ci-1", 9*time.Hour, "scarpenter-use", "true"))

	rep := a.run(sweep.Options{MaxAge: 2 * time.Hour})
	for _, id := range []string{oldSrv, netID, rsID, grpID, orphan} {
		if outcome(rep, id) != sweep.Deleted {
			t.Errorf("%s: %q, want deleted", id, outcome(rep, id))
		}
	}
	for _, id := range []string{foreign, unlabelled, tmpl, useNet} {
		if outcome(rep, id) != "" {
			t.Errorf("%s was looked at: %q", id, outcome(rep, id))
		}
	}
	for _, id := range []string{foreign, unlabelled} {
		if _, ok := a.f.Server(id); !ok {
			t.Errorf("server %s was deleted", id)
		}
	}
	if _, ok := a.f.Server(oldSrv); ok {
		t.Error("the old server is still there")
	}
	if rep.Count(sweep.Failed) != 0 || rep.Count(sweep.Deleted) != 5 {
		t.Errorf("report: %+v", rep.Items)
	}
}

func TestYoungAndUnknownAgeServersProtectTheirCluster(t *testing.T) {
	a := newAccount(t)
	young := a.server("young", labels("scarpenter-ci-2", 10*time.Minute))
	noAge := a.server("noage", labels("scarpenter-ci-3", -1))
	n2 := a.network("n2", labels("scarpenter-ci-2", 5*time.Hour))
	n3 := a.network("n3x", labels("scarpenter-ci-3", 5*time.Hour))
	st := a.f.AddStorage("fi-hel1", "young-disk", "maxiops", 25, labels("scarpenter-ci-2", 10*time.Minute))
	rep := a.run(sweep.Options{MaxAge: 2 * time.Hour})
	for _, id := range []string{young, noAge, n2, n3, st} {
		if outcome(rep, id) != sweep.Skipped {
			t.Errorf("%s: %q, want skipped", id, outcome(rep, id))
		}
	}
	// Without an age limit everything of the cluster goes, the unknown age too.
	rep = a.run(sweep.Options{Cluster: "scarpenter-ci-3"})
	if outcome(rep, noAge) != sweep.Deleted || outcome(rep, n3) != sweep.Deleted {
		t.Errorf("exact cluster, no age limit: %+v", rep.Items)
	}
	if outcome(rep, young) != "" || outcome(rep, n2) != "" {
		t.Errorf("another cluster was touched: %+v", rep.Items)
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	a := newAccount(t)
	l := labels("scarpenter-ci-4", 5*time.Hour)
	s := a.server("s", l)
	n := a.network("n4", l)
	rep := a.run(sweep.Options{MaxAge: 2 * time.Hour, DryRun: true})
	if rep.Count(sweep.WouldDelete) != 2 || rep.Count(sweep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep.Items)
	}
	if _, ok := a.f.Server(s); !ok {
		t.Error("a dry run deleted a server")
	}
	if outcome(rep, n) != sweep.WouldDelete {
		t.Errorf("network: %q", outcome(rep, n))
	}
}

func TestAttachedDiskAndFailuresAreReported(t *testing.T) {
	a := newAccount(t)
	l := labels("scarpenter-ci-5", 5*time.Hour)
	s := a.server("s", l)
	// A server whose delete fails is Failed and does not stop the sweep.
	a.f.InjectCode("DeleteServerAndStorages", "INTERNAL_ERROR", 500, 1)
	orphan := a.f.AddStorage("fi-hel1", "o", "maxiops", 25, l)
	rep := a.run(sweep.Options{MaxAge: 2 * time.Hour})
	if outcome(rep, s) != sweep.Failed {
		t.Errorf("server: %q, want failed: %+v", outcome(rep, s), rep.Items)
	}
	if outcome(rep, orphan) != sweep.Deleted {
		t.Errorf("orphan disk: %q", outcome(rep, orphan))
	}
	// The next sweep finishes the job.
	rep = a.run(sweep.Options{MaxAge: 2 * time.Hour})
	if outcome(rep, s) != sweep.Deleted {
		t.Errorf("second sweep: %q", outcome(rep, s))
	}
}

func TestListFailureIsAnError(t *testing.T) {
	a := newAccount(t)
	a.f.Inject("GetServersWithFilters", errors.New("boom"), 0)
	if _, err := sweep.Run(context.Background(), a.f, sweep.Options{}); err == nil {
		t.Fatal("no error")
	}
}

func TestOtherListFailuresAreItems(t *testing.T) {
	a := newAccount(t)
	a.f.Inject("GetNetworks", errors.New("boom"), 0)
	rep := a.run(sweep.Options{MaxAge: time.Hour})
	if rep.Count(sweep.Failed) != 1 {
		t.Errorf("report: %+v", rep.Items)
	}
}

func TestAttachedStorageIsSkipped(t *testing.T) {
	a := newAccount(t)
	// A node disk is labelled in the create call and belongs to its server.
	a.f.AddServer(fake.ServerSpec{Zone: "fi-hel1", Plan: "1xCPU-1GB", Title: "keep",
		Labels: labels("scarpenter-ci-6", 10*time.Minute)})
	rep := a.run(sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Deleted) != 0 {
		t.Errorf("report: %+v", rep.Items)
	}
}
