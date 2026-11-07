//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// fakeHost is a machine without QEMU: it keeps the processes, bridges and taps
// the driver asks for, in memory, and creates the files the real tools would.
type fakeHost struct {
	mu      sync.Mutex
	nextPID int
	alive   map[int]string // pid -> instance ID
	bridges map[string]string
	taps    map[string]string // tap -> bridge
	arp     []neighbor
	kvm     bool
	cpus    int
	// started records the command lines of the VMs.
	started [][]string
	// fail, when set, is asked before every command and may fail it once.
	fail func(name string, args []string) error
	// hideBridge makes Bridges leave the bridge out once.
	hideBridge string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		nextPID: 4000, alive: map[int]string{}, bridges: map[string]string{DefaultBridge: ""}, taps: map[string]string{},
		kvm: true, cpus: 8,
	}
}

func (h *fakeHost) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail != nil {
		if err := h.fail(name, args); err != nil {
			h.fail = nil
			return nil, err
		}
	}
	switch {
	case name == "qemu-img":
		return nil, h.qemuImg(args)
	case name == "ip":
		return nil, h.ip(args)
	case strings.HasPrefix(name, "qemu-system-") || name == "taskset":
		return nil, h.start(append([]string{name}, args...))
	}
	return nil, fmt.Errorf("unexpected command %s", name)
}

func (h *fakeHost) qemuImg(args []string) error {
	switch args[0] {
	case "create":
		// qemu-img create [-q] -f fmt [-b base -F fmt] file size
		return os.WriteFile(args[len(args)-2], nil, 0o600)
	case "convert":
		return os.WriteFile(args[len(args)-1], nil, 0o600)
	case "resize":
		return nil
	}
	return fmt.Errorf("unexpected qemu-img %v", args)
}

func (h *fakeHost) ip(args []string) error {
	switch {
	case args[0] == "tuntap" && args[1] == "add":
		h.taps[args[3]] = ""
	case args[0] == "link" && args[1] == "set" && args[3] == "master":
		if _, ok := h.bridges[args[4]]; !ok {
			return fmt.Errorf("no bridge %s", args[4])
		}
		h.taps[args[2]] = args[4]
	case args[0] == "link" && args[1] == "set":
	case args[0] == "link" && args[1] == "add":
		h.bridges[args[2]] = ""
	case args[0] == "addr":
	case args[0] == "link" && args[1] == "delete":
		_, tap := h.taps[args[2]]
		_, br := h.bridges[args[2]]
		if !tap && !br {
			return fmt.Errorf("Cannot find device %q", args[2])
		}
		delete(h.taps, args[2])
		delete(h.bridges, args[2])
	default:
		return fmt.Errorf("unexpected ip %v", args)
	}
	return nil
}

func (h *fakeHost) start(cmd []string) error {
	arg := func(flag string) string {
		i := slices.Index(cmd, flag)
		if i < 0 {
			return ""
		}
		return cmd[i+1]
	}
	id, pidfile := arg("-name"), arg("-pidfile")
	if id == "" || pidfile == "" {
		return errors.New("a VM without a name or a pid file")
	}
	h.nextPID++
	h.alive[h.nextPID] = id
	h.started = append(h.started, cmd)
	return os.WriteFile(pidfile, fmt.Appendf(nil, "%d\n", h.nextPID), 0o600)
}

func (h *fakeHost) Alive(pid int, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive[pid] == id && id != ""
}

func (h *fakeHost) Terminate(_ context.Context, pid int, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.alive, pid)
	return nil
}

// exit makes the VM process of the instance end by itself.
func (h *fakeHost) exit(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for pid, i := range h.alive {
		if i == id {
			delete(h.alive, pid)
		}
	}
}

func (h *fakeHost) KVMUsable() bool { return h.kvm }
func (h *fakeHost) CPUs() int       { return h.cpus }

func (h *fakeHost) Bridges() ([]bridgeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []bridgeInfo
	for name, alias := range h.bridges {
		if name == h.hideBridge {
			continue
		}
		out = append(out, bridgeInfo{Name: name, Alias: alias})
	}
	h.hideBridge = ""
	return out, nil
}

func (h *fakeHost) Neighbors() ([]neighbor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.arp), nil
}

// testImage is the release reference of the cache fixture.
const testImage = "image"

// newTestDriver returns an initialised driver over a temporary state and cache
// directory and the fake host, with a complete release in the cache.
func newTestDriver(t *testing.T) (*Driver, *fakeHost) {
	t.Helper()
	root := t.TempDir()
	opts := DefaultOptions()
	opts.StateDir, opts.CacheDir = filepath.Join(root, "state"), filepath.Join(root, "cache")
	h := newFakeHost()
	d := New(opts)
	d.host = h
	seedRelease(t, filepath.Join(opts.CacheDir, testImage))
	if err := d.Init(t.Context(), vendors.Config{ClusterName: "my-cluster", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return d, h
}

func seedRelease(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"flatcar_production_qemu_uefi_efi_code.qcow2", "flatcar_production_qemu_uefi_efi_vars.qcow2", readyMarker} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The image is a qcow2 file of 1 KiB virtual size: a root disk must not be
	// smaller.
	if err := os.WriteFile(filepath.Join(dir, imageFile), qcow2Header(1024), 0o600); err != nil {
		t.Fatal(err)
	}
}

// restartDriver returns a second driver over the same state, cache and host: a
// restarted controller.
func restartDriver(t *testing.T, d *Driver) *Driver {
	t.Helper()
	d2 := New(d.opts)
	d2.host = d.host
	if err := d2.Init(t.Context(), vendors.Config{ClusterName: "my-cluster", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return d2
}
