//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// bridgeInfo is a bridge of the machine; Alias is its interface alias, where a
// user can put "key=value" tags that a `use` selector matches.
type bridgeInfo struct {
	Name  string
	Alias string
}

// neighbor is an entry of the machine's ARP table.
type neighbor struct {
	IP  string
	MAC string
	Dev string
}

// host is everything the driver does to the machine outside its own files:
// commands, processes, the network. Tests replace it.
type host interface {
	// Run runs a command to completion and returns its combined output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Alive reports whether the process is still the VM with the instance ID
	// (a reused PID is not).
	Alive(pid int, id string) bool
	// Terminate ends the VM process: SIGTERM, then SIGKILL after a grace
	// period. A process that is gone is not an error.
	Terminate(ctx context.Context, pid int, id string) error
	// KVMUsable reports whether /dev/kvm can be opened.
	KVMUsable() bool
	// CPUs is the number of CPUs the VMs can be pinned to.
	CPUs() int
	// Bridges lists the machine's bridges.
	Bridges() ([]bridgeInfo, error)
	// Neighbors returns the ARP table.
	Neighbors() ([]neighbor, error)
}

// osHost is the real machine.
type osHost struct{}

func (osHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (osHost) Alive(pid int, id string) bool {
	cmdline, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
	if err != nil {
		return false
	}
	// A zombie has an empty command line; the VM's own is "... -name <id> ...".
	return bytes.Contains(cmdline, []byte("\x00-name\x00"+id+"\x00"))
}

const (
	termGrace = 15 * time.Second
	killGrace = 5 * time.Second
)

func (h osHost) Terminate(ctx context.Context, pid int, id string) error {
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{{syscall.SIGTERM, termGrace}, {syscall.SIGKILL, killGrace}} {
		if !h.Alive(pid, id) {
			return nil
		}
		if err := syscall.Kill(pid, step.sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signalling the VM process %d: %w", pid, err)
		}
		deadline := time.NewTimer(step.grace)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		defer deadline.Stop()
	wait:
		for h.Alive(pid, id) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("waiting for the VM process %d: %w", pid, ctx.Err())
			case <-deadline.C:
				break wait
			case <-tick.C:
			}
		}
	}
	if h.Alive(pid, id) {
		return fmt.Errorf("the VM process %d did not exit", pid)
	}
	return nil
}

func (osHost) KVMUsable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func (osHost) CPUs() int { return runtime.NumCPU() }

func (osHost) Bridges() ([]bridgeInfo, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, fmt.Errorf("listing the network interfaces: %w", err)
	}
	var out []bridgeInfo
	for _, e := range entries {
		dir := filepath.Join("/sys/class/net", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "bridge")); err != nil {
			continue
		}
		alias, _ := os.ReadFile(filepath.Join(dir, "ifalias"))
		out = append(out, bridgeInfo{Name: e.Name(), Alias: strings.TrimSpace(string(alias))})
	}
	return out, nil
}

func (osHost) Neighbors() ([]neighbor, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, fmt.Errorf("reading the ARP table: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []neighbor
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		// IP address, HW type, Flags, HW address, Mask, Device.
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[2] == "0x0" {
			continue
		}
		out = append(out, neighbor{IP: fields[0], MAC: strings.ToLower(fields[3]), Dev: fields[5]})
	}
	return out, sc.Err()
}
