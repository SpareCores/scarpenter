//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Files of an instance directory.
const (
	fileRecord    = "instance.json"
	fileEFIVars   = "efi-vars.qcow2"
	fileIgnition  = "ignition.json"
	fileSerialLog = "serial.log"
	filePID       = "qemu.pid"
)

// The accelerators the driver resolves to, and the arguments of ip(8) it uses
// more than once.
const (
	accelKVM = "kvm"
	accelTCG = "tcg"
	ipLink   = "link"
	ipAdd    = "add"
	ipSet    = "set"
)

// Defaults of the settings.
const (
	defaultRootSize = "20Gi"
	defaultFormat   = qemuapi.DiskFormat("qcow2")
	// localDiskBytes is the size of one local disk of a catalog type per GB of
	// the catalog's decimal gigabytes.
	gigabyte = int64(1000 * 1000 * 1000)
)

// defaultSettings are the driver's built-in settings (layer 1 of 00a section 3).
const defaultSettings = `{"accelerator":"auto","storage":{"root":{"size":"` + defaultRootSize + `","format":"qcow2"}}}`

// settings is the part of the merged vendor block the driver reads.
type settings struct {
	Accelerator qemuapi.Accelerator `json:"accelerator"`
	Storage     qemuapi.Storage     `json:"storage"`
}

func decodeSettings(s vendors.VendorSettings) (settings, error) {
	var out settings
	if len(s.Raw) > 0 {
		if err := json.Unmarshal(s.Raw, &out); err != nil {
			return out, fmt.Errorf("decoding the settings: %w", err)
		}
	}
	return out, nil
}

func (s settings) rootSize() int64 {
	if s.Storage.Root != nil && s.Storage.Root.Size != nil && !s.Storage.Root.Size.IsZero() {
		return s.Storage.Root.Size.Value()
	}
	return defaultRootBytes()
}

func defaultRootBytes() int64 {
	q := resource.MustParse(defaultRootSize)
	return q.Value()
}

// rootFile is the root disk image's name in the instance directory.
func rootFile(f qemuapi.DiskFormat) string { return "root." + string(f) }

func (s settings) rootFormat() qemuapi.DiskFormat {
	if s.Storage.Root != nil && s.Storage.Root.Format != "" {
		return s.Storage.Root.Format
	}
	return defaultFormat
}

// disk is an extra disk of a VM.
type disk struct {
	Kind string // data or local
	Name string
	// File is the image's name in the instance directory.
	File string
	// Serial is the virtio serial (at most 20 characters), which the guest sees
	// as /dev/disk/by-id/virtio-<serial> and as the SERIAL column of lsblk.
	Serial string
	Size   int64
	Format qemuapi.DiskFormat
}

// dataSerial is the serial of a data disk: derived from the volume's name.
func dataSerial(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "scv" + hex.EncodeToString(sum[:])[:12]
}

func localSerial(i int) string { return "scl" + strconv.Itoa(i) }

// diskPlan lists the extra disks of a VM: the data disks of the storage
// profile and, for a catalog type with local disks, the local disks. Prepare
// derives the device hints from the same plan, so that the guest finds what the
// driver attached.
func diskPlan(s settings, localCount int, localBytes int64) []disk {
	out := make([]disk, 0, len(s.Storage.Data)+localCount)
	for i, d := range s.Storage.Data {
		format := d.Format
		if format == "" {
			format = defaultFormat
		}
		out = append(out, disk{
			Kind: "data", Name: d.Name, File: fmt.Sprintf("data-%d.%s", i, format), Serial: dataSerial(d.Name),
			Size: roundUp(d.Size.Value()), Format: format,
		})
	}
	for i := range localCount {
		out = append(out, disk{
			Kind: "local", Name: strconv.Itoa(i), File: fmt.Sprintf("local-%d.qcow2", i), Serial: localSerial(i),
			Size: roundUp(localBytes), Format: defaultFormat,
		})
	}
	return out
}

// vmSpec is everything the QEMU command line depends on.
type vmSpec struct {
	ID        string
	Arch      string
	Accel     string // kvm or tcg
	VCPUs     int
	MemoryMiB int
	Dir       string
	// RootFile and RootFormat describe the root disk image in Dir.
	RootFile   string
	RootFormat qemuapi.DiskFormat
	Disks      []disk
	EFICode    string
	Tap        string
	MAC        string
	NICModel   string
	// Pin lists the host CPUs the VM is pinned to (taskset); empty: not pinned.
	Pin []int
}

func (s vmSpec) path(name string) string { return filepath.Join(s.Dir, name) }

// command returns the program and arguments that start the VM as a daemon. The
// layout follows flatcar_production_qemu_uefi.sh of the release (UEFI firmware
// in pflash, the image as a virtio-blk disk with boot index 1, virtio network
// and rng, Ignition through fw_cfg), without a console: the serial port goes to
// a file.
func (s vmSpec) command() (string, []string, error) {
	var bin string
	var machine, cpu []string
	switch s.Arch {
	case "amd64":
		bin = "qemu-system-x86_64"
		machine = []string{
			"-global", "ICH9-LPC.disable_s3=1", "-global", "driver=cfi.pflash01,property=secure,value=on",
			"-machine", "q35,accel=" + s.Accel + ",smm=on",
		}
		cpu = []string{"-cpu", map[bool]string{true: "host", false: "max"}[s.Accel == accelKVM]}
	case "arm64":
		bin = "qemu-system-aarch64"
		machine = []string{"-machine", "virt,accel=" + s.Accel + ",gic-version=3"}
		cpu = []string{"-cpu", map[bool]string{true: "host", false: "max"}[s.Accel == accelKVM]}
	default:
		return "", nil, fmt.Errorf("unsupported architecture %q", s.Arch)
	}
	nic := s.NICModel
	if nic == "" {
		nic = "virtio-net-pci"
	}
	args := []string{"-name", s.ID, "-daemonize", "-pidfile", s.path(filePID), "-display", "none", "-monitor", "none"}
	args = append(args, machine...)
	args = append(args, cpu...)
	args = append(args,
		"-smp", strconv.Itoa(s.VCPUs), "-m", strconv.Itoa(s.MemoryMiB),
		"-drive", "if=pflash,unit=0,file="+s.EFICode+",format=qcow2,readonly=on",
		"-drive", "if=pflash,unit=1,file="+s.path(fileEFIVars)+",format=qcow2",
		"-drive", "if=none,id=root,format="+string(s.RootFormat)+",file="+s.path(s.RootFile),
		"-device", "virtio-blk-pci,drive=root,bootindex=1",
	)
	for i, d := range s.Disks {
		id := "disk" + strconv.Itoa(i)
		args = append(args,
			"-drive", "if=none,id="+id+",format="+string(d.Format)+",file="+s.path(d.File),
			"-device", "virtio-blk-pci,drive="+id+",serial="+d.Serial)
	}
	args = append(args,
		"-netdev", "tap,id=net0,ifname="+s.Tap+",script=no,downscript=no",
		"-device", nic+",netdev=net0,mac="+s.MAC,
		"-object", "rng-random,filename=/dev/urandom,id=rng0", "-device", "virtio-rng-pci,rng=rng0",
		"-fw_cfg", "name=opt/org.flatcar-linux/config,file="+s.path(fileIgnition),
		"-serial", "file:"+s.path(fileSerialLog),
	)
	if len(s.Pin) > 0 {
		cpus := make([]string, len(s.Pin))
		for i, c := range s.Pin {
			cpus[i] = strconv.Itoa(c)
		}
		return "taskset", append([]string{"-c", strings.Join(cpus, ","), bin}, args...), nil
	}
	return bin, args, nil
}

// macFor derives a locally administered MAC from the instance ID.
func macFor(id string) string {
	sum := sha256.Sum256([]byte("mac/" + id))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// tapFor derives the tap device's name (at most 15 characters).
func tapFor(id string) string {
	sum := sha256.Sum256([]byte("tap/" + id))
	return "sct" + hex.EncodeToString(sum[:])[:8]
}
