//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package qemu is the development driver that boots real Flatcar VMs with QEMU
// on the machine SCarpenter runs on (overview section 13.3): the L4 test lane
// runs the controller out of cluster in a privileged dev container with it.
// Instances are qcow2 overlays of the official Flatcar QEMU image (downloaded,
// verified against the release's digests and cached), their user-data is passed
// through fw_cfg, and each VM is attached to a Linux bridge by a tap device.
// The VMs are daemonised processes, and every instance has a record in a state
// directory, so a restarted controller finds them again.
package qemu

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

// Name is the registration name of the driver and the vendor ID it serves.
const Name = "qemu"

// Defaults of the flags.
const (
	// DefaultBridge is the bridge of the default network.
	DefaultBridge = "scbr0"
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

// Options are the flags of the qemu driver, injected next to core's options.
type Options struct {
	// StateDir holds one directory per instance (record, disks, serial log) and
	// the records of managed networks; CacheDir the downloaded Flatcar images.
	StateDir string
	CacheDir string
	// LogDir, when set, receives the serial log of every instance that goes
	// away, so that a test lane can show how a VM booted after it was deleted.
	LogDir string
	// Bridge is the bridge of the default network (the network a NodeClass
	// without a resources.networks block uses, when it exists).
	Bridge string
	// LaunchDelay makes CreateInstance wait after the VM has started, before it
	// returns: for the L4 scenario that kills the controller in that window, so
	// that the replay of the launch is certain, not a matter of luck.
	LaunchDelay time.Duration
}

// DefaultOptions returns the options with every default.
func DefaultOptions() Options {
	return Options{StateDir: filepath.Join(defaultDir(), "state"), CacheDir: filepath.Join(defaultDir(), "cache"), Bridge: DefaultBridge}
}

func defaultDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "scarpenter-qemu")
}

// AddFlags registers the flags; the defaults come from SCARPENTER_QEMU_*.
func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	def := DefaultOptions()
	fs.StringVar(&o.StateDir, "qemu-state-dir", env.WithDefaultString("SCARPENTER_QEMU_STATE_DIR", def.StateDir), "Directory of the qemu driver's instance records and disks.")
	fs.StringVar(&o.CacheDir, "qemu-cache-dir", env.WithDefaultString("SCARPENTER_QEMU_CACHE_DIR", def.CacheDir), "Directory the qemu driver caches the downloaded Flatcar images in.")
	fs.StringVar(&o.LogDir, "qemu-log-dir", env.WithDefaultString("SCARPENTER_QEMU_LOG_DIR", ""), "If set, the serial logs of deleted qemu instances are moved here.")
	fs.DurationVar(&o.LaunchDelay, "qemu-launch-delay", env.WithDefaultDuration("SCARPENTER_QEMU_LAUNCH_DELAY", 0), "Test aid: how long CreateInstance waits after the VM has started before it returns.")
	fs.StringVar(&o.Bridge, "qemu-bridge", env.WithDefaultString("SCARPENTER_QEMU_BRIDGE", def.Bridge), "Bridge of the qemu driver's default network.")
}

// Parse parses the flags and validates the options.
func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags: %w", err)
	}
	if o.StateDir == "" || o.CacheDir == "" || o.Bridge == "" {
		return errors.New("--qemu-state-dir, --qemu-cache-dir and --qemu-bridge must not be empty")
	}
	if o.LaunchDelay < 0 {
		return errors.New("--qemu-launch-delay must not be negative")
	}
	if len(o.Bridge) > 15 {
		return fmt.Errorf("--qemu-bridge %q is longer than the 15 characters of a Linux interface name", o.Bridge)
	}
	return nil
}

// ToContext stores the options in the context.
func (o *Options) ToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, optionsKey{}, o)
}

// FromContext returns the options stored in the context, or nil.
func FromContext(ctx context.Context) *Options {
	v, _ := ctx.Value(optionsKey{}).(*Options)
	return v
}
