//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/static"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
)

// hosts maps the drivers of the conformance suite to their fake machines: the
// suite's hooks only get the driver.
var (
	hostsMu sync.Mutex
	hosts   = map[*Driver]*fakeHost{}
)

func registered(t *testing.T) *Driver {
	t.Helper()
	d, h := newTestDriver(t)
	hostsMu.Lock()
	hosts[d] = h
	hostsMu.Unlock()
	return d
}

func hostOf(d vendors.Driver) *fakeHost {
	hostsMu.Lock()
	defer hostsMu.Unlock()
	return hosts[d.(*Driver)]
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Fixture{
		New:    func(t *testing.T) vendors.Driver { return registered(t) },
		Region: Region, Zone: Zone, InstanceType: "small", MaxUserData: maxUserData,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			h := hostOf(d)
			isStart := func(name string) bool { return strings.HasPrefix(name, "qemu-system-") || name == "taskset" }
			failing := func(match func(name string, args []string) bool, err error) {
				h.fail = func(name string, args []string) error {
					if match(name, args) {
						return err
					}
					return nil
				}
			}
			start := func(name string, _ []string) bool { return isStart(name) }
			create := func(name string, args []string) bool { return name == "qemu-img" && args[0] == "create" }
			switch class {
			case vendors.InsufficientCapacity:
				failing(start, errors.New("qemu-system-x86_64: cannot allocate memory"))
			case vendors.Unsupported:
				failing(start, errors.New("Could not access KVM kernel module: Permission denied"))
			case vendors.QuotaExceeded:
				failing(create, errors.New("qemu-img: No space left on device"))
			case vendors.ZoneUnusable:
				h.hideBridge = DefaultBridge
			case vendors.Retryable:
				failing(start, errors.New("exit status 1"))
			case vendors.Fatal:
				failing(create, errors.New("qemu-img: invalid image"))
			default:
				t.Fatalf("no injection for %s", class)
			}
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","size":"1Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"pair":  json.RawMessage(`{"data":[{"name":"a","size":"1Gi","mountPath":"/a"},{"name":"b","size":"1Gi","mountPath":"/b"}]}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"raid0","mountPath":"/mnt/local"}}`),
			},
			vendors.ProfilePlacement: {"pinned": json.RawMessage(`{"pinCPUs":true}`)},
			vendors.ProfileNetwork:   {"e1000": json.RawMessage(`{"model":"e1000"}`)},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: Name, APIReference: "small", Family: "qemu", Arch: "amd64", VCPUs: 2, MemoryMiB: 2048},
			{Vendor: Name, APIReference: "local", Family: "qemu", Arch: "amd64", VCPUs: 2, MemoryMiB: 4096, LocalStorageGB: 8, LocalStorageCount: 2},
		},
		ResourceKind: KindNetwork,
		PreCreate: func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			h := hostOf(d)
			alias := make([]string, 0, len(tags))
			for k, v := range tags {
				alias = append(alias, k+"="+v)
			}
			h.mu.Lock()
			h.bridges["user-br"] = strings.Join(alias, " ")
			h.mu.Unlock()
			return vendors.ResourceRef{Kind: kind, Region: region, ID: "user-br", Name: "user-br"}
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			d2 := restartDriver(t, d.(*Driver))
			h := hostOf(d)
			hostsMu.Lock()
			hosts[d2] = h
			hostsMu.Unlock()
			return d2
		},
	})
}

func createRequest(name string) vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: Region, Zone: Zone, InstanceType: "small", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image:    vendors.Image{Ref: testImage},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), IdempotencyKey: "uid-" + name,
		Tags: map[vendors.TagKey]string{vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: name},
		Node: vendors.NodeInfo{Name: name, ProviderID: "scarpenter://qemu/local/" + name},
	}
}

func record1(t *testing.T, d *Driver, inst *vendors.Instance) *record {
	t.Helper()
	rec, err := d.readRecord(inst.Ref.ID())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCreateMakesTheFilesTheTapAndTheVMProcess(t *testing.T) {
	d, h := newTestDriver(t)
	req := createRequest("claim-1")
	req.Settings = vendors.VendorSettings{
		Raw: json.RawMessage(`{"accelerator":"kvm","storage":{"root":{"size":"30Gi"},
			"data":[{"name":"a","size":"1Gi","mountPath":"/a"},{"name":"b","size":"2Gi","mountPath":"/b","format":"raw"}]}}`),
		Network: json.RawMessage(`{"model":"e1000"}`),
	}
	inst, err := d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	rec := record1(t, d, inst)
	dir := d.instanceDir(rec.ID)
	if got, _ := os.ReadFile(filepath.Join(dir, fileIgnition)); string(got) != string(req.UserData) {
		t.Errorf("the Ignition file holds %q", got)
	}
	for _, f := range []string{"root.qcow2", "data-0.qcow2", "data-1.raw", fileEFIVars, filePID} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("the instance directory lacks %s: %v", f, err)
		}
	}
	if h.taps[rec.Tap] != DefaultBridge {
		t.Errorf("the tap %s is on %q, want %s", rec.Tap, h.taps[rec.Tap], DefaultBridge)
	}
	if len(h.started) != 1 {
		t.Fatalf("%d VMs were started", len(h.started))
	}
	cmd := strings.Join(h.started[0], " ")
	for _, want := range []string{
		"qemu-system-x86_64 -name " + rec.ID, "accel=kvm", "-smp 2 -m 2048", "tap,id=net0,ifname=" + rec.Tap,
		"e1000,netdev=net0,mac=" + rec.MAC, "serial=" + dataSerial("a"), "serial=" + dataSerial("b"), "file=" + filepath.Join(dir, "data-1.raw"),
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command line lacks %q:\n%s", want, cmd)
		}
	}
	if inst.State != vendors.StateRunning || inst.Region != Region || inst.Zone != Zone || inst.Tags[vendors.TagNodeClaim] != "claim-1" {
		t.Errorf("the instance is %+v", inst)
	}
}

func TestAddressesComeFromTheBridgesARPTable(t *testing.T) {
	d, h := newTestDriver(t)
	inst, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Addresses) != 0 {
		t.Fatalf("addresses %v before the VM has an ARP entry", inst.Addresses)
	}
	rec := record1(t, d, inst)
	h.arp = []neighbor{
		{IP: "192.168.1.9", MAC: rec.MAC, Dev: "other-bridge"}, {IP: "192.168.1.10", MAC: rec.MAC, Dev: DefaultBridge},
		{IP: "192.168.1.11", MAC: "52:54:00:00:00:00", Dev: DefaultBridge},
	}
	got, err := d.GetInstance(t.Context(), inst.Ref)
	if err != nil || !slices.Equal(got.Addresses, []string{"192.168.1.10"}) {
		t.Errorf("addresses %v, %v; want only the entry of this MAC on the VM's bridge", got.Addresses, err)
	}
}

func TestARestartedDriverFindsTheRunningVMAndDoesNotLaunchASecondOne(t *testing.T) {
	d, h := newTestDriver(t)
	first, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil {
		t.Fatal(err)
	}
	d2 := restartDriver(t, d)
	got, err := d2.GetInstance(t.Context(), first.Ref)
	if err != nil || got.State != vendors.StateRunning {
		t.Fatalf("after a restart: %+v, %v", got, err)
	}
	again, err := d2.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil || again.Ref != first.Ref {
		t.Fatalf("the replayed key returned %+v, %v; want %s", again, err, first.Ref)
	}
	if len(h.started) != 1 {
		t.Errorf("%d VMs were started, want one", len(h.started))
	}
	list, _ := d2.ListInstances(t.Context(), nil, map[vendors.TagKey]string{vendors.TagCluster: "my-cluster"})
	if len(list) != 1 {
		t.Errorf("the restarted driver lists %d instances", len(list))
	}
}

func TestAVMThatExitedIsReportedOnceThenForgotten(t *testing.T) {
	d, h := newTestDriver(t)
	inst, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil {
		t.Fatal(err)
	}
	rec := record1(t, d, inst)
	h.exit(rec.ID)
	got, err := d.GetInstance(t.Context(), inst.Ref)
	if err != nil || !got.Gone() {
		t.Fatalf("a VM that powered off: %+v, %v; want a terminated instance", got, err)
	}
	if _, err := d.GetInstance(t.Context(), inst.Ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("the second Get: %v, want NotFound", err)
	}
	if _, err := os.Stat(d.instanceDir(rec.ID)); err == nil {
		t.Error("the directory of the exited VM was kept")
	}
	if _, ok := h.taps[rec.Tap]; ok {
		t.Error("the tap of the exited VM was kept")
	}
	// A VM that exits is not listed either, and its key can launch again.
	second, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil || second.State != vendors.StateRunning {
		t.Fatalf("a new launch with the key: %+v, %v", second, err)
	}
}

func TestInitRemovesTheLeftoversOfACrashAndOfDeadVMs(t *testing.T) {
	d, h := newTestDriver(t)
	running, err := d.CreateInstance(t.Context(), createRequest("running"))
	if err != nil {
		t.Fatal(err)
	}
	dead, err := d.CreateInstance(t.Context(), createRequest("dead"))
	if err != nil {
		t.Fatal(err)
	}
	h.exit(record1(t, d, dead).ID)
	// A launch that crashed before it had a process.
	crashed := &record{ID: "i-crashed", Phase: phaseCreating, Region: Region, Tap: "sctcrashed"}
	if err := os.MkdirAll(d.instanceDir(crashed.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.writeRecord(crashed); err != nil {
		t.Fatal(err)
	}
	h.taps[crashed.Tap] = DefaultBridge

	restartDriver(t, d)
	for _, id := range []string{record1(t, d, running).ID} {
		if _, err := os.Stat(d.instanceDir(id)); err != nil {
			t.Errorf("the running instance was removed: %v", err)
		}
	}
	for _, id := range []string{"i-crashed", instanceID("uid-dead")} {
		if _, err := os.Stat(d.instanceDir(id)); err == nil {
			t.Errorf("the leftovers of %s survived the restart", id)
		}
	}
	if _, ok := h.taps[crashed.Tap]; ok {
		t.Error("the tap of the crashed launch survived")
	}
}

func TestAFailedLaunchLeavesNothingAndTheKeyUnused(t *testing.T) {
	d, h := newTestDriver(t)
	h.fail = func(name string, _ []string) error {
		if strings.HasPrefix(name, "qemu-system-") {
			return errors.New("exit status 1")
		}
		return nil
	}
	if _, err := d.CreateInstance(t.Context(), createRequest("claim-1")); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a VM that does not start: %v, want Retryable", err)
	}
	entries, _ := os.ReadDir(d.instancesDir())
	if len(entries) != 0 || len(h.taps) != 0 {
		t.Errorf("a failed launch left %d directories and %d taps", len(entries), len(h.taps))
	}
	if _, err := d.CreateInstance(t.Context(), createRequest("claim-1")); err != nil {
		t.Fatalf("the retry with the same key: %v", err)
	}
}

func TestDeleteEndsTheProcessRemovesTheFilesAndKeepsTheSerialLog(t *testing.T) {
	d, h := newTestDriver(t)
	logs := filepath.Join(t.TempDir(), "logs")
	d.opts.LogDir = logs
	inst, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
	if err != nil {
		t.Fatal(err)
	}
	rec := record1(t, d, inst)
	if err := os.WriteFile(filepath.Join(d.instanceDir(rec.ID), fileSerialLog), []byte("Flatcar boot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if h.Alive(rec.PID, rec.ID) {
		t.Error("the VM process is still running")
	}
	if _, err := os.Stat(d.instanceDir(rec.ID)); err == nil {
		t.Error("the instance directory was kept")
	}
	if _, ok := h.taps[rec.Tap]; ok {
		t.Error("the tap was kept")
	}
	got, err := os.ReadFile(filepath.Join(logs, "claim-1-"+rec.ID+".serial.log"))
	if err != nil || string(got) != "Flatcar boot" {
		t.Errorf("the serial log in the log directory: %q, %v", got, err)
	}
}

func TestCreateRefusesWhatTheDriverCannotLaunch(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*vendors.CreateRequest)
		hide   bool
		class  vendors.Class
	}{
		"spot":         {func(r *vendors.CreateRequest) { r.CapacityType = vendors.CapacitySpot }, false, vendors.Unsupported},
		"unknown type": {func(r *vendors.CreateRequest) { r.InstanceType = "huge" }, false, vendors.Fatal},
		"other region": {func(r *vendors.CreateRequest) { r.Region = "elsewhere" }, false, vendors.Fatal},
		"other zone":   {func(r *vendors.CreateRequest) { r.Zone = "local-b" }, false, vendors.Fatal},
		"arm64":        {func(r *vendors.CreateRequest) { r.Arch = "arm64" }, false, vendors.Fatal},
		"no image":     {func(r *vendors.CreateRequest) { r.Image.Ref = "not-downloaded" }, false, vendors.Fatal},
		"escaping ref": {func(r *vendors.CreateRequest) { r.Image.Ref = "../../etc" }, false, vendors.Fatal},
		"no bridge":    {func(*vendors.CreateRequest) {}, true, vendors.ZoneUnusable},
		"kvm missing": {func(r *vendors.CreateRequest) {
			r.Settings.Raw = json.RawMessage(`{"accelerator":"kvm"}`)
		}, false, vendors.Unsupported},
		"root disk smaller than the image": {func(r *vendors.CreateRequest) {
			r.Settings.Raw = json.RawMessage(`{"storage":{"root":{"size":"512"}}}`)
		}, false, vendors.Fatal},
	} {
		t.Run(name, func(t *testing.T) {
			d, h := newTestDriver(t)
			h.kvm = name != "kvm missing"
			if tc.hide {
				delete(h.bridges, DefaultBridge)
			}
			req := createRequest("claim-1")
			tc.mutate(&req)
			_, err := d.CreateInstance(t.Context(), req)
			if !vendors.IsClass(err, tc.class) {
				t.Errorf("got %v, want class %s", err, tc.class)
			}
			if entries, _ := os.ReadDir(d.instancesDir()); len(entries) != 0 {
				t.Errorf("a refused launch left %d instance directories", len(entries))
			}
		})
	}
}

func TestPinnedPlacementRunsTheVMUnderTasksetOnItsOwnCPUs(t *testing.T) {
	d, h := newTestDriver(t)
	req := createRequest("claim-1")
	req.Settings.Placement = json.RawMessage(`{"pinCPUs":true}`)
	if _, err := d.CreateInstance(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	cmd := h.started[0]
	if cmd[0] != "taskset" || cmd[1] != "-c" || cmd[3] != "qemu-system-x86_64" || len(strings.Split(cmd[2], ",")) != 2 {
		t.Errorf("a pinned VM starts with %v", cmd[:4])
	}
	h.cpus = 1
	req = createRequest("claim-2")
	req.Settings.Placement = json.RawMessage(`{"pinCPUs":true}`)
	if _, err := d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("pinning 2 vCPUs on 1 CPU: %v, want Unsupported", err)
	}
}

func TestCommandLines(t *testing.T) {
	base := vmSpec{
		ID: "i-abc", Arch: "amd64", Accel: "kvm", VCPUs: 2, MemoryMiB: 4096, Dir: "/state/instances/i-abc",
		RootFile: "root.qcow2", RootFormat: "qcow2", EFICode: "/cache/code.qcow2", Tap: "sctdeadbeef", MAC: "52:54:00:01:02:03",
		Disks: []disk{
			{Kind: "data", File: "data-0.qcow2", Serial: "scvfeedface0123", Format: "qcow2"},
			{Kind: "local", File: "local-0.qcow2", Serial: "scl0", Format: "qcow2"},
		},
	}
	tcg := base
	tcg.Accel = "tcg"
	arm := base
	arm.Arch = "arm64"
	pinned := base
	pinned.Pin = []int{2, 3}
	e1000 := base
	e1000.NICModel = "e1000"
	var out strings.Builder
	for _, tc := range []struct {
		name string
		spec vmSpec
	}{{"amd64-kvm", base}, {"amd64-tcg", tcg}, {"arm64-kvm", arm}, {"pinned", pinned}, {"e1000", e1000}} {
		bin, args, err := tc.spec.command()
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("# " + tc.name + "\n" + bin + "\n")
		for _, a := range args {
			out.WriteString("  " + a + "\n")
		}
	}
	testutil.Golden(t, "commands.golden", []byte(out.String()))
	if _, _, err := (vmSpec{Arch: "riscv"}).command(); err == nil {
		t.Error("an unsupported architecture produced a command line")
	}
}

func TestTheEmbeddedCatalogIsAStaticCatalogWithLocalDisksOnOneType(t *testing.T) {
	src, err := static.Parse(New(Options{}).CatalogYAML())
	if err != nil {
		t.Fatal(err)
	}
	data, err := src.Load(t.Context(), catalog.Request{Scope: catalog.Scope{Name: nil}})
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Regions) != 1 || data.Regions[0].Name != Region || len(data.Zones) != 1 || data.Zones[0].Name != Zone {
		t.Errorf("regions %+v zones %+v", data.Regions, data.Zones)
	}
	got := map[string][3]float64{}
	for _, s := range data.Servers {
		got[s.APIReference] = [3]float64{float64(s.VCPUs), s.MemoryMiB, s.LocalStorageGB}
		if s.Arch != "amd64" {
			t.Errorf("%s is %s", s.APIReference, s.Arch)
		}
	}
	want := map[string][3]float64{"small": {2, 2048, 0}, "medium": {2, 4096, 0}, "local": {2, 4096, 8}}
	if len(got) != 3 || got["small"] != want["small"] || got["medium"] != want["medium"] || got["local"] != want["local"] {
		t.Errorf("types %v, want %v", got, want)
	}
	d, _ := newTestDriver(t)
	for name := range want {
		if _, ok := d.types[name]; !ok {
			t.Errorf("the driver does not know the catalog type %s", name)
		}
	}
	if d.types["local"].LocalStorageCount != 2 {
		t.Errorf("local disks: %d", d.types["local"].LocalStorageCount)
	}
}

// A launch that crashed after qemu daemonised has a record without a pid. The VM
// must not run on unseen once the record is cleaned up: not by a restarted
// controller, and not by the replay of the launch.
func TestACrashedLaunchDoesNotLeaveItsVMRunning(t *testing.T) {
	crash := func(t *testing.T) (*Driver, *fakeHost, string) {
		t.Helper()
		d, h := newTestDriver(t)
		inst, err := d.CreateInstance(t.Context(), createRequest("claim-1"))
		if err != nil {
			t.Fatal(err)
		}
		rec := record1(t, d, inst)
		// Back to what the record looked like before it was rewritten with the pid.
		rec.Phase, rec.PID = phaseCreating, 0
		if err := d.writeRecord(rec); err != nil {
			t.Fatal(err)
		}
		if len(h.alive) != 1 {
			t.Fatalf("%d VM processes before the crash", len(h.alive))
		}
		return d, h, rec.ID
	}
	t.Run("restart", func(t *testing.T) {
		d, h, _ := crash(t)
		restartDriver(t, d)
		if len(h.alive) != 0 {
			t.Errorf("%d VM processes run on after the restart removed their record", len(h.alive))
		}
	})
	t.Run("replay", func(t *testing.T) {
		d, h, _ := crash(t)
		if _, err := d.CreateInstance(t.Context(), createRequest("claim-1")); err != nil {
			t.Fatal(err)
		}
		if len(h.started) != 2 || len(h.alive) != 1 {
			t.Errorf("%d VMs started, %d running: the replay must replace the first VM, not add one", len(h.started), len(h.alive))
		}
	})
}

// The launch delay holds CreateInstance after the VM runs, and a cancelled
// context ends the wait: the instance is then there.
func TestLaunchDelayWaitsAfterTheVMStartedUntilTheContextEnds(t *testing.T) {
	d, h := newTestDriver(t)
	d.opts.LaunchDelay = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := d.CreateInstance(ctx, createRequest("claim-1"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("CreateInstance returned during the delay: %v", err)
	case <-func() chan struct{} {
		started := make(chan struct{})
		go func() {
			for {
				h.mu.Lock()
				n := len(h.started)
				h.mu.Unlock()
				if n > 0 {
					close(started)
					return
				}
				runtime.Gosched()
			}
		}()
		return started
	}():
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(h.alive) != 1 {
		t.Errorf("%d VMs run", len(h.alive))
	}
}
