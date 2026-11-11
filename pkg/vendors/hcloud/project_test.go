/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

type env struct {
	f   *fake.Fake
	p   *hcloud.Project
	clk *clocktesting.FakeClock
	x86 *hc.Image
	arm *hc.Image
	cl  string
}

func newEnv(t *testing.T, fo []fake.Option, po ...hcloud.ProjectOption) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	f := fake.New(append([]fake.Option{fake.WithClock(clk)}, fo...)...)
	x86, arm := f.AddFlatcarImages("stable", "4757.2.3")
	return &env{f: f, p: hcloud.NewProject(f, "c1", append([]hcloud.ProjectOption{hcloud.WithClock(clk)}, po...)...), clk: clk, x86: x86, arm: arm, cl: "c1"}
}

func (e *env) spec(name string) hcloud.LaunchSpec {
	return hcloud.LaunchSpec{
		Name: name, ServerType: "cx23", Location: "fsn1", ImageID: e.x86.ID, UserData: []byte("{}"), PublicIPv4: true,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: e.cl, vendors.TagNodeClaim: name, vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name,
	}
}

func class(err error) vendors.Class { return vendors.ClassOf(err) }

func TestRegionsZonesAndCaching(t *testing.T) {
	e := newEnv(t, nil)
	regs, err := e.p.Regions(ctx)
	if err != nil || len(regs) != 6 || regs[0].Name != "ash" {
		t.Fatalf("%v %v", regs, err)
	}
	z, err := e.p.Zones(ctx, "fsn1")
	if err != nil || len(z) != 1 || z[0].ID != "fsn1" || z[0].Name != "fsn1" {
		t.Errorf("%v %v", z, err)
	}
	if _, err := e.p.Zones(ctx, "nbg1-dc3"); class(err) != vendors.Unsupported {
		t.Errorf("%v", err)
	}
	if e.f.Calls("Locations") != 1 {
		t.Errorf("locations are cached: %d calls", e.f.Calls("Locations"))
	}
	e.clk.Step(61 * time.Minute)
	_, _ = e.p.Regions(ctx)
	if e.f.Calls("Locations") != 2 {
		t.Error("the cache lasts an hour")
	}
	if nz, _ := e.p.NetworkZone(ctx, "ash"); nz != hc.NetworkZoneUSEast {
		t.Error(nz)
	}
	e.f.Inject("Locations", fake.APIError(hc.ErrorCodeServerError, "x"), 1)
	e.clk.Step(2 * time.Hour)
	if _, err := e.p.Regions(ctx); class(err) != vendors.Retryable {
		t.Errorf("%v", err)
	}
}

func TestServerTypesLaunchableAndEpoch(t *testing.T) {
	e := newEnv(t, nil)
	got, err := e.p.LaunchableTypes(ctx, "ash")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cax11"]; ok || !slices.Equal(got["cx23"], []string{"ash"}) {
		t.Errorf("arm only in the EU: %v", got)
	}
	epoch := e.p.Epoch()
	if epoch == 0 {
		t.Error("the first load changes the epoch")
	}
	// Availability changes are not an epoch change; refresh happens after 3 minutes.
	e.f.SetAvailable("cx23", "fsn1", false)
	if g, _ := e.p.LaunchableTypes(ctx, "fsn1"); !slices.Contains(g["cx23"], "fsn1") {
		t.Error("cached for 3 minutes")
	}
	e.clk.Step(4 * time.Minute)
	if g, _ := e.p.LaunchableTypes(ctx, "fsn1"); g["cx23"] != nil {
		t.Errorf("out of stock after refresh: %v", g)
	}
	if e.p.Epoch() != epoch || e.p.NodeOptionsEpoch() != epoch {
		t.Error("availability does not change the type table epoch")
	}
	e.clk.Step(4 * time.Minute)
	e.f.SetDeprecation("cpx22", "fsn1", e.clk.Now(), e.clk.Now().Add(-time.Minute))
	if g, _ := e.p.LaunchableTypes(ctx, "fsn1"); g["cpx22"] != nil {
		t.Errorf("past unavailable_after: %v", g)
	}
	b, ok, err := e.p.RootDiskForType(ctx, "cpx22")
	if err != nil || !ok || b != 80_000_000_000 {
		t.Errorf("%d %v %v", b, ok, err)
	}
	if _, ok, _ := e.p.RootDiskForType(ctx, "nope"); ok {
		t.Error("unknown type")
	}
}

func TestFindImage(t *testing.T) {
	e := newEnv(t, nil)
	im, err := e.p.FindImage(ctx, "amd64", "stable", "4757.2.3")
	if err != nil || im.ID != e.x86.ID {
		t.Fatalf("%v %v", im, err)
	}
	im, err = e.p.FindImage(ctx, "arm64", "stable", "4757.2.3")
	if err != nil || im.ID != e.arm.ID {
		t.Fatalf("%v %v", im, err)
	}
	if _, err := e.p.FindImage(ctx, "amd64", "stable", "1.0.0"); class(err) != vendors.NotFound || vendors.CodeOf(err) != hcloud.CodeImageNotFound {
		t.Errorf("%v", err)
	}
	if _, err := e.p.FindImage(ctx, "amd64", "beta", "4757.2.3"); class(err) != vendors.NotFound {
		t.Errorf("channel: %v", err)
	}
	if _, err := e.p.FindImage(ctx, "riscv", "stable", "4757.2.3"); class(err) != vendors.Fatal {
		t.Errorf("%v", err)
	}
	e.f.AddImage(hc.ArchitectureX86, map[string]string{"os": "flatcar", "flatcar-channel": "stable", "flatcar-version": "4757.2.3"})
	if _, err := e.p.FindImage(ctx, "amd64", "stable", "4757.2.3"); class(err) != vendors.Fatal || vendors.CodeOf(err) != hcloud.CodeImageAmbiguous {
		t.Errorf("two matches: %v", err)
	}
	if _, err := e.p.FindImage(ctx, "arm64", "stable", "4757.2.3"); err != nil {
		t.Error("the other architecture is unaffected")
	}
}

func TestResolveSSHKey(t *testing.T) {
	e := newEnv(t, nil)
	k := e.f.AddSSHKey("ops")
	if id, err := e.p.ResolveSSHKey(ctx, "ops"); err != nil || id != k.ID {
		t.Errorf("%d %v", id, err)
	}
	if _, err := e.p.ResolveSSHKey(ctx, "nope"); class(err) != vendors.Unsupported {
		t.Errorf("%v", err)
	}
}

func TestCreate(t *testing.T) {
	e := newEnv(t, nil)
	inst, err := e.p.Create(ctx, e.spec("nc-1"))
	if err != nil {
		t.Fatal(err)
	}
	// The create response is read before the start action: pending.
	if inst.State != vendors.StatePending || inst.Region != "fsn1" || inst.Zone != "fsn1" || inst.Type != "cx23" || inst.CapacityType != "on-demand" {
		t.Errorf("%+v", inst)
	}
	if inst.Tags[vendors.TagIdempotencyKey] != "uid-nc-1" || inst.Tags[vendors.TagNodeClaim] != "nc-1" || len(inst.Addresses) != 1 {
		t.Errorf("%+v", inst)
	}
	got, err := e.p.Get(ctx, inst.Ref)
	if err != nil || got.Ref != inst.Ref || got.State != vendors.StateRunning {
		t.Errorf("%v %v", got, err)
	}
}

func TestCreateReplayReturnsOriginal(t *testing.T) {
	e := newEnv(t, nil)
	first, err := e.p.Create(ctx, e.spec("nc-1"))
	if err != nil {
		t.Fatal(err)
	}
	// The replay asks for another zone and type; the contract wants the original.
	replay := e.spec("nc-1")
	replay.ServerType, replay.Location = "cpx22", "nbg1"
	again, err := e.p.Create(ctx, replay)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != first.Ref || again.Type != "cx23" || again.Zone != "fsn1" {
		t.Errorf("replay returned %+v, want the original %+v", again, first)
	}
	if e.f.ServerCount() != 1 {
		t.Errorf("%d servers", e.f.ServerCount())
	}
}

func TestCreateNameInUseByAnotherClaim(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.p.Create(ctx, e.spec("nc-1")); err != nil {
		t.Fatal(err)
	}
	other := e.spec("nc-1")
	other.IdempotencyKey = "uid-other"
	_, err := e.p.Create(ctx, other)
	if class(err) != vendors.Retryable || vendors.CodeOf(err) != hcloud.CodeNameInUse {
		t.Errorf("%v", err)
	}
	noKey := e.spec("nc-1")
	noKey.IdempotencyKey = ""
	if _, err := e.p.Create(ctx, noKey); vendors.CodeOf(err) != hcloud.CodeNameInUse {
		t.Errorf("without a key a same-name server is never adopted: %v", err)
	}
}

func TestCreateLongNameKeyAndLabels(t *testing.T) {
	e := newEnv(t, nil)
	long := "a-very-long-nodeclaim-name-that-exceeds-the-sixty-three-characters-of-a-label-1"
	spec := e.spec(long)
	first, err := e.p.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.p.Create(ctx, spec)
	if err != nil || again.Ref != first.Ref {
		t.Fatalf("replay of a shortened name: %v %v", again, err)
	}
	found, err := e.p.Find(ctx, "fsn1", map[vendors.TagKey]string{vendors.TagNodeClaim: long})
	if err != nil || found.Ref != first.Ref {
		t.Errorf("the lookup by the long name maps the same way: %v %v", found, err)
	}
	other := e.spec(long[:len(long)-1] + "2")
	if o, err := e.p.Create(ctx, other); err != nil || o.Ref == first.Ref {
		t.Errorf("two long names with the same prefix are two servers: %v %v", o, err)
	}
}

func TestCreateErrorClasses(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*env, *hcloud.LaunchSpec)
		class vendors.Class
		code  string
		wide  bool
	}{
		"out of stock":  {func(e *env, _ *hcloud.LaunchSpec) { e.f.SetAvailable("cx23", "fsn1", false) }, vendors.InsufficientCapacity, "resource_unavailable", false},
		"arm in ash":    {func(e *env, s *hcloud.LaunchSpec) { s.ServerType, s.Location, s.ImageID = "cax11", "ash", e.arm.ID }, vendors.Unsupported, "invalid_input", false},
		"wrong arch":    {func(e *env, s *hcloud.LaunchSpec) { s.ServerType = "cax11" }, vendors.Unsupported, "invalid_input", false},
		"unknown image": {func(_ *env, s *hcloud.LaunchSpec) { s.ImageID = 1 }, vendors.Unsupported, "invalid_input", false},
		"project limit": {func(e *env, _ *hcloud.LaunchSpec) { e.f.AddForeignServer("fsn1", "cx23", nil) }, vendors.QuotaExceeded, "resource_limit_exceeded", true},
		"rate limited":  {func(e *env, _ *hcloud.LaunchSpec) { e.f.SetRateLimited(true) }, vendors.Retryable, "rate_limit_exceeded", false},
		"server error": {func(e *env, _ *hcloud.LaunchSpec) {
			e.f.Inject("CreateServer", fake.APIError(hc.ErrorCodeServerError, "x"), 1)
		}, vendors.Retryable, "server_error", false},
		"unauthorized": {func(e *env, _ *hcloud.LaunchSpec) {
			e.f.Inject("CreateServer", fake.APIError(hc.ErrorCodeUnauthorized, "x"), 1)
		}, vendors.Fatal, "unauthorized", false},
		"user data large": {func(_ *env, s *hcloud.LaunchSpec) { s.UserData = make([]byte, hcloud.MaxUserData+1) }, vendors.Fatal, hcloud.CodeUserDataTooLarge, false},
	} {
		t.Run(name, func(t *testing.T) {
			var lim []fake.Option
			if name == "project limit" {
				lim = []fake.Option{fake.WithLimits(fake.Limits{Servers: 1})}
			}
			e := newEnv(t, lim)
			s := e.spec("nc-1")
			tc.setup(e, &s)
			_, err := e.p.Create(ctx, s)
			if class(err) != tc.class || vendors.CodeOf(err) != tc.code || vendors.IsAccountWide(err) != tc.wide {
				t.Errorf("class %s code %q wide %v: %v", class(err), vendors.CodeOf(err), vendors.IsAccountWide(err), err)
			}
			if tc.class != vendors.Retryable && e.f.ServerCount() > 1 {
				t.Error("a failed create leaves nothing")
			}
			// A failed create leaves the key unused: the next attempt launches.
			e.f.ClearInjected()
			e.f.SetRateLimited(false)
			if name == "out of stock" || name == "rate limited" || name == "server error" {
				e.f.SetAvailable("cx23", "fsn1", true)
				if _, err := e.p.Create(ctx, e.spec("nc-1")); err != nil {
					t.Errorf("retry: %v", err)
				}
			}
		})
	}
}

func TestCreateFailedActionDeletesServer(t *testing.T) {
	e := newEnv(t, nil)
	e.f.FailCreateAction("resource_unavailable", "no capacity")
	_, err := e.p.Create(ctx, e.spec("nc-1"))
	if class(err) != vendors.InsufficientCapacity || vendors.CodeOf(err) != "resource_unavailable" {
		t.Errorf("%v", err)
	}
	if e.f.ServerCount() != 0 {
		t.Error("the half-created server is deleted before the create returns")
	}
	e.f.FailCreateAction("", "")
	if _, err := e.p.Create(ctx, e.spec("nc-1")); err != nil {
		t.Errorf("the name and the key are free again: %v", err)
	}
}

func TestCreateActionTimeoutDeletesServer(t *testing.T) {
	e := newEnv(t, nil)
	e.f.Inject("WaitForAction", context.DeadlineExceeded, 1)
	_, err := e.p.Create(ctx, e.spec("nc-1"))
	if class(err) != vendors.Retryable || vendors.CodeOf(err) != hcloud.CodeTimeout {
		t.Errorf("%v", err)
	}
	e.f.Tick()
	if e.f.ServerCount() != 0 {
		t.Error("deleted")
	}
}

func TestCreateWithResources(t *testing.T) {
	e := newEnv(t, nil)
	nw := e.f.AddNetwork("net", "10.0.0.0/16", nil, hc.NetworkZoneEUCentral)
	fw, _ := e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "fw"})
	pg, _ := e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg", Type: hc.PlacementGroupTypeSpread})
	vo, _ := hcloud.BuildVolumeOpts(hcloud.VolumeSpec{Name: "vol-1", Location: "fsn1", SizeGB: 10})
	vol, err := e.f.CreateVolume(ctx, vo)
	if err != nil {
		t.Fatal(err)
	}
	k := e.f.AddSSHKey("ops")
	s := e.spec("nc-1")
	s.NetworkID, s.FirewallIDs, s.PlacementGroupID, s.VolumeIDs, s.SSHKeyID, s.PublicIPv6 = nw.ID, []int64{fw.ID}, pg.ID, []int64{vol.ID}, k.ID, true
	inst, err := e.p.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Addresses) != 3 {
		t.Errorf("v4, v6 and private addresses: %v", inst.Addresses)
	}
}

func TestPlacementGroupFull(t *testing.T) {
	e := newEnv(t, nil)
	pg, _ := e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg", Type: hc.PlacementGroupTypeSpread})
	for i := range hcloud.MaxSpreadMembers {
		s := e.spec(fmt.Sprintf("nc-%d", i))
		s.PlacementGroupID = pg.ID
		if _, err := e.p.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		e.clk.Step(6 * time.Second)
	}
	s := e.spec("nc-x")
	s.PlacementGroupID = pg.ID
	_, err := e.p.Create(ctx, s)
	if class(err) != vendors.InsufficientCapacity || vendors.CodeOf(err) != hcloud.CodePlacementFull {
		t.Errorf("%v", err)
	}
	if e.f.Calls("CreateServer") != hcloud.MaxSpreadMembers {
		t.Error("a full group is refused before the call")
	}
}

func TestGetFindList(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.p.Create(ctx, e.spec("nc-a"))
	b := e.spec("nc-b")
	b.Location = "nbg1"
	bi, _ := e.p.Create(ctx, b)
	e.f.AddForeignServer("fsn1", "cx23", map[string]string{"owner": "someone"})

	if _, err := e.p.Get(ctx, "garbage"); class(err) != vendors.NotFound {
		t.Errorf("unparseable ref: %v", err)
	}
	if _, err := e.p.Get(ctx, "fsn1/999999"); class(err) != vendors.NotFound {
		t.Errorf("unknown server: %v", err)
	}
	all, err := e.p.List(ctx, nil, map[vendors.TagKey]string{vendors.TagCluster: "c1"})
	if err != nil || len(all) != 2 {
		t.Fatalf("%d %v", len(all), err)
	}
	if fsn, _ := e.p.List(ctx, []string{"fsn1"}, map[vendors.TagKey]string{vendors.TagCluster: "c1"}); len(fsn) != 1 || fsn[0].Ref != a.Ref {
		t.Errorf("location filter: %v", fsn)
	}
	if other, _ := e.p.List(ctx, nil, map[vendors.TagKey]string{vendors.TagCluster: "c2"}); len(other) != 0 {
		t.Error("tag filter")
	}
	f, err := e.p.Find(ctx, "nbg1", map[vendors.TagKey]string{vendors.TagIdempotencyKey: "uid-nc-b"})
	if err != nil || f.Ref != bi.Ref {
		t.Errorf("%v %v", f, err)
	}
	if _, err := e.p.Find(ctx, "fsn1", map[vendors.TagKey]string{vendors.TagIdempotencyKey: "uid-nc-b"}); class(err) != vendors.NotFound {
		t.Errorf("a key is found in its own location only: %v", err)
	}
}

func TestSharedListIsCached(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.p.Create(ctx, e.spec("nc-a"))
	before := e.f.Calls("Servers")
	for range 50 {
		_, _ = e.p.List(ctx, nil, nil)
		_, _ = e.p.Find(ctx, "fsn1", map[vendors.TagKey]string{vendors.TagNodeClaim: "nc-a"})
	}
	if got := e.f.Calls("Servers") - before; got != 1 {
		t.Errorf("100 callers made %d list calls, want 1", got)
	}
	e.clk.Step(6 * time.Second)
	_, _ = e.p.List(ctx, nil, nil)
	if got := e.f.Calls("Servers") - before; got != 2 {
		t.Errorf("after 5 seconds the list is read again: %d", got)
	}
	// A create or a delete drops the shared list.
	_, _ = e.p.Create(ctx, e.spec("nc-b"))
	all, _ := e.p.List(ctx, nil, nil)
	if len(all) != 2 {
		t.Errorf("the new server is visible at once: %d", len(all))
	}
}

func TestStoppedServerIsListedTerminatingAndDeleted(t *testing.T) {
	e := newEnv(t, nil)
	inst, _ := e.p.Create(ctx, e.spec("nc-1"))
	_, id, _ := hcloud.ParseRef(inst.Ref)
	e.f.PowerOff(id)
	e.clk.Step(10 * time.Second)
	got, err := e.p.Get(ctx, inst.Ref)
	if err != nil || got.State != vendors.StateTerminating || !got.Gone() {
		t.Fatalf("%+v %v", got, err)
	}
	list, _ := e.p.List(ctx, nil, nil)
	if len(list) != 1 || list[0].State != vendors.StateTerminating {
		t.Errorf("a stopped server stays listed for the garbage collector: %v", list)
	}
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if e.f.ServerCount() != 0 {
		t.Error("deleted")
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t, nil)
	inst, _ := e.p.Create(ctx, e.spec("nc-1"))
	if err := e.p.Delete(ctx, "garbage"); err != nil {
		t.Errorf("an unparseable reference is a server that is gone: %v", err)
	}
	if err := e.p.Delete(ctx, "fsn1/424242"); err != nil {
		t.Errorf("unknown: %v", err)
	}
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Errorf("idempotent: %v", err)
	}
	if e.f.ServerCount() != 0 {
		t.Error("gone")
	}
	if _, err := e.p.Get(ctx, inst.Ref); class(err) != vendors.NotFound {
		t.Errorf("a deleted server is gone, not terminated: %v", err)
	}
}

func TestDeleteRefusesForeignServers(t *testing.T) {
	e := newEnv(t, nil)
	foreign := e.f.AddForeignServer("fsn1", "cx23", map[string]string{"owner": "x"})
	err := e.p.Delete(ctx, hcloud.NewRef("fsn1", foreign.ID))
	if class(err) != vendors.Fatal || vendors.CodeOf(err) != "NotOwned" {
		t.Errorf("no marker: %v", err)
	}
	e2 := newEnv(t, nil)
	e2.cl = "other"
	inst, _ := e2.p.Create(ctx, e2.spec("nc-1"))
	mine := hcloud.NewProject(e2.f, "c1")
	if err := mine.Delete(ctx, inst.Ref); vendors.CodeOf(err) != "NotOwned" {
		t.Errorf("another cluster's server: %v", err)
	}
	if e2.f.ServerCount() != 1 {
		t.Error("untouched")
	}
	if err := hcloud.NewProject(e2.f, "other").Delete(ctx, inst.Ref); err != nil {
		t.Errorf("its own cluster: %v", err)
	}
}

func TestDeleteErrors(t *testing.T) {
	e := newEnv(t, nil)
	inst, _ := e.p.Create(ctx, e.spec("nc-1"))
	_, id, _ := hcloud.ParseRef(inst.Ref)
	e.f.ProtectServer(id, true)
	if err := e.p.Delete(ctx, inst.Ref); class(err) != vendors.Fatal || vendors.CodeOf(err) != "protected" {
		t.Errorf("protected: %v", err)
	}
	e.f.ProtectServer(id, false)
	e.f.Inject("DeleteServer", fake.APIError(hc.ErrorCodeLocked, "locked"), 1)
	if err := e.p.Delete(ctx, inst.Ref); class(err) != vendors.Retryable {
		t.Errorf("locked: %v", err)
	}
	e.f.Inject("DeleteServer", fake.APIError(hc.ErrorCodeNotFound, "gone"), 1)
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Errorf("not_found on the delete is success: %v", err)
	}
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Errorf("%v", err)
	}
	e.f.Inject("GetServer", fake.APIError(hc.ErrorCodeNotFound, "gone"), 1)
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Errorf("not_found on the get: %v", err)
	}
}

func TestDeleteCleansUnassignedPrimaryIPs(t *testing.T) {
	e := newEnv(t, []fake.Option{fake.WithPrimaryIPAutoDelete(false)})
	s := e.spec("nc-1")
	s.PublicIPv6 = true
	inst, _ := e.p.Create(ctx, s)
	if len(e.f.PrimaryIPs()) != 2 {
		t.Fatal("two Primary IPs")
	}
	if err := e.p.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.f.PrimaryIPs()); n != 0 {
		t.Errorf("%d Primary IPs left, they bill monthly", n)
	}
}

func TestPrimaryIPCleanupLeavesAssignedAndAutoDelete(t *testing.T) {
	e := newEnv(t, nil)
	inst, _ := e.p.Create(ctx, e.spec("nc-1"))
	ips := e.f.PrimaryIPs()
	if err := e.p.CleanupPrimaryIPs(ctx, []int64{ips[0].ID, 424242}); err != nil {
		t.Errorf("assigned and unknown IPs are left alone: %v", err)
	}
	if len(e.f.PrimaryIPs()) != 1 {
		t.Error("untouched")
	}
	e.f.Inject("GetPrimaryIP", fake.APIError(hc.ErrorCodeServerError, "x"), 1)
	if err := e.p.CleanupPrimaryIPs(ctx, []int64{ips[0].ID}); class(err) != vendors.Retryable {
		t.Errorf("%v", err)
	}
	_ = inst
}

func TestQuotaWithoutLimit(t *testing.T) {
	e := newEnv(t, nil)
	if _, ok := e.p.Consumption("cx23", "on-demand"); ok {
		t.Error("without a limit the quota is unknown (reactive)")
	}
	g, err := e.p.GlobalQuotas(ctx)
	if err != nil || len(g.Buckets) != 0 {
		t.Errorf("%+v %v", g, err)
	}
	if r, err := e.p.Quotas(ctx, "fsn1"); err != nil || len(r.Buckets) != 0 {
		t.Errorf("%+v %v", r, err)
	}
}

func TestQuotaWithLimit(t *testing.T) {
	e := newEnv(t, nil, hcloud.WithServerLimit(6))
	a, _ := e.p.Create(ctx, e.spec("nc-a"))
	f := e.f.AddForeignServer("nbg1", "cx23", nil)
	e.clk.Step(10 * time.Second)
	g, err := e.p.GlobalQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Buckets) != 1 {
		t.Fatalf("%+v", g)
	}
	b := g.Buckets[0]
	if b.ID != "servers" || b.Scope != vendors.QuotaScopeGlobal || b.Unit != "instances" || b.Limit != 6 || b.Used != 2 {
		t.Errorf("%+v", b)
	}
	if !slices.Contains(g.Counted, a.Ref) || !slices.Contains(g.Counted, hcloud.NewRef("nbg1", f.ID)) {
		t.Errorf("counted %v", g.Counted)
	}
	c, ok := e.p.Consumption("cx23", "on-demand")
	if !ok || len(c) != 1 || c[0] != (vendors.Consumption{Bucket: "servers", Amount: 1}) {
		t.Errorf("%v %v", c, ok)
	}
	if r, _ := e.p.Quotas(ctx, "fsn1"); len(r.Buckets) != 0 {
		t.Error("no regional buckets")
	}
}

func TestRateLimitPassThrough(t *testing.T) {
	e := newEnv(t, nil)
	_, _ = e.p.Regions(ctx)
	if rl := e.p.RateLimit(); rl.Limit != 3600 || !rl.Known() {
		t.Errorf("%+v", rl)
	}
	if e.p.API() != hcloud.API(e.f) {
		t.Error("API")
	}
}

// A server of the name that is being deleted (a discarded failed create whose
// delete did not complete) is no replay of the key: the key stays unused.
func TestCreateReplayOfDeletingServerIsNameInUse(t *testing.T) {
	e := newEnv(t, nil)
	inst, err := e.p.Create(ctx, e.spec("nc-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, id, _ := hcloud.ParseRef(inst.Ref)
	e.f.SetStatus(id, hc.ServerStatusDeleting)
	_, err = e.p.Create(ctx, e.spec("nc-1"))
	if class(err) != vendors.Retryable || vendors.CodeOf(err) != hcloud.CodeNameInUse {
		t.Errorf("replay of a deleting server: %v", err)
	}
}

// A rate limit answered without a JSON body is Retryable, not Fatal: the
// launch backs off instead of failing.
func TestBareRateLimitIsRetryable(t *testing.T) {
	for _, bare := range []bool{false, true} {
		e := newEnv(t, nil)
		e.f.SetBareRateLimit(bare)
		e.f.SetRateLimited(true)
		_, err := e.p.Create(ctx, e.spec("nc-1"))
		if class(err) != vendors.Retryable {
			t.Errorf("bare=%v create: %v", bare, err)
		}
		if _, err := e.p.List(ctx, nil, nil); class(err) != vendors.Retryable {
			t.Errorf("bare=%v list: %v", bare, err)
		}
		if bare && vendors.CodeOf(err) != hcloud.CodeRateLimited {
			t.Errorf("code %q", vendors.CodeOf(err))
		}
	}
}
