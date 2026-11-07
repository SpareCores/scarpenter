//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/catalog/static"
	scopts "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The one region and zone of the driver, and the user-data limit it mirrors from
// AWS (the smallest of the vendors, overview section 8.2).
const (
	Region      = "local"
	Zone        = "local-a"
	maxUserData = 16 * 1024
)

//go:embed catalog.yaml
var catalogYAML []byte

func init() {
	vendors.Register(Name, func() vendors.Driver { return &Driver{} })
}

// Instance phases of a record: a record is written before the first side effect
// of a launch and moves to running when the VM process is started.
const (
	phaseCreating = "creating"
	phaseRunning  = "running"
)

// record is the state file of an instance (the source of truth; the VM process
// is checked against it).
type record struct {
	ID           string            `json:"id"`
	Key          string            `json:"key"`
	Phase        string            `json:"phase"`
	Region       string            `json:"region"`
	Zone         string            `json:"zone"`
	Type         string            `json:"type"`
	CapacityType string            `json:"capacityType"`
	Arch         string            `json:"arch"`
	CreatedAt    time.Time         `json:"createdAt"`
	Tags         map[string]string `json:"tags"`
	NodeName     string            `json:"nodeName,omitempty"`
	PID          int               `json:"pid,omitempty"`
	MAC          string            `json:"mac"`
	Tap          string            `json:"tap"`
	Bridge       string            `json:"bridge"`
	// Networks are the IDs of the managed networks the instance uses.
	Networks []string `json:"networks,omitempty"`
}

// Driver is the qemu driver.
type Driver struct {
	opts  Options
	cfg   vendors.Config
	host  host
	clock clock.Clock
	log   logr.Logger
	types map[string]static.Type
	imgs  *images

	// locks serialise the operations on one instance (by ID).
	locks sync.Map
	// netMu serialises the operations on managed networks.
	netMu sync.Mutex
}

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.ManagedResources   = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
	_ vendors.CatalogProvider    = (*Driver)(nil)
)

// New returns a driver with the given options and host (for tests; the
// registry's factory returns a driver that reads its options from the Init
// context and drives the real machine).
func New(opts Options) *Driver { return &Driver{opts: opts} }

// Vendor returns the vendor ID.
func (d *Driver) Vendor() string { return Name }

// CatalogYAML implements vendors.CatalogProvider.
func (d *Driver) CatalogYAML() []byte { return catalogYAML }

// TagKey implements vendors.Driver.
func (d *Driver) TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

// Init prepares the directories and reconciles the records with the running
// VM processes.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	if d.opts.StateDir == "" {
		d.opts = DefaultOptions()
		if o := FromContext(ctx); o != nil {
			d.opts = *o
		}
	}
	d.cfg = cfg
	d.log = cfg.Log
	if d.log.GetSink() == nil {
		d.log = logr.Discard()
	}
	if d.clock = cfg.Clock; d.clock == nil {
		d.clock = clock.RealClock{}
	}
	if d.host == nil {
		d.host = osHost{}
	}
	var file static.File
	if err := yaml.Unmarshal(catalogYAML, &file); err != nil || len(file.Vendors) != 1 {
		return fmt.Errorf("the embedded catalog is invalid: %v", err)
	}
	d.types = map[string]static.Type{}
	for _, t := range file.Vendors[0].Types {
		d.types[t.Name] = t
	}
	flatcarBase := ""
	if o := scopts.FromContext(ctx); o != nil {
		flatcarBase = o.FlatcarBaseURL
	}
	d.imgs = &images{
		cacheDir: d.opts.CacheDir, client: &http.Client{},
		resolver: artifacts.New(artifacts.Config{FlatcarBase: flatcarBase, Clock: d.clock}),
	}
	for _, dir := range []string{d.instancesDir(), d.networksDir(), d.opts.CacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the driver's own directories
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	d.reconcile(ctx)
	return nil
}

func (d *Driver) instancesDir() string { return filepath.Join(d.opts.StateDir, "instances") }
func (d *Driver) networksDir() string  { return filepath.Join(d.opts.StateDir, "networks") }

func (d *Driver) instanceDir(id string) string { return filepath.Join(d.instancesDir(), id) }

func (d *Driver) lock(id string) *sync.Mutex {
	m, _ := d.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Regions implements vendors.Driver.
func (d *Driver) Regions(context.Context) ([]vendors.Region, error) {
	return []vendors.Region{{Name: Region}}, nil
}

// Zones implements vendors.Driver.
func (d *Driver) Zones(_ context.Context, region string) ([]vendors.Zone, error) {
	if region != Region {
		return nil, vendors.Errorf(vendors.NotFound, "%q is not a region of the qemu driver (it has %q)", region, Region)
	}
	return []vendors.Zone{{ID: Zone, Name: Zone}}, nil
}

// ResolveImage implements vendors.Driver: it downloads the release's UEFI QEMU
// image and firmware, verifies them against the release's digest file and
// caches them. Image.Ref is the release's directory in the cache directory.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	rel, size, err := d.imgs.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	ref, err := filepath.Rel(d.opts.CacheDir, rel.dir)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	return &vendors.Image{Ref: ref, RootDiskBytes: size}, nil
}

// ValidateNodeClass implements vendors.NodeClassValidator: kvm needs /dev/kvm.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	if b := nc.Spec.Vendors.QEMU; b != nil && b.Accelerator == accelKVM && !d.host.KVMUsable() {
		return errors.New("spec.vendors.qemu.accelerator is kvm but /dev/kvm is not usable here")
	}
	return nil
}

// instanceID derives the instance's ID from the idempotency key, so that a
// replayed key finds the same instance (and a retry after a failed launch the
// same files).
func instanceID(key string) string {
	sum := sha256.Sum256([]byte("instance/" + key))
	return "i-" + hex.EncodeToString(sum[:])[:12]
}

func (d *Driver) readRecord(id string) (*record, error) {
	raw, err := os.ReadFile(filepath.Join(d.instanceDir(id), fileRecord)) //nolint:gosec // the driver's own state
	if err != nil {
		return nil, err
	}
	rec := &record{}
	if err := json.Unmarshal(raw, rec); err != nil {
		return nil, fmt.Errorf("decoding the record of %s: %w", id, err)
	}
	return rec, nil
}

// writeRecord replaces the record atomically.
func (d *Driver) writeRecord(rec *record) error {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(d.instanceDir(rec.ID), fileRecord)
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (d *Driver) records() []*record {
	entries, err := os.ReadDir(d.instancesDir())
	if err != nil {
		return nil
	}
	var out []*record
	for _, e := range entries {
		if rec, err := d.readRecord(e.Name()); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

// alive reports whether the record's VM process runs.
func (d *Driver) alive(rec *record) bool {
	return rec.PID > 0 && d.host.Alive(rec.PID, rec.ID)
}

// reconcile removes what a crash or a restart of the machine left behind:
// launches that never got a process and VMs whose process is gone.
func (d *Driver) reconcile(ctx context.Context) {
	for _, rec := range d.records() {
		if rec.Phase == phaseCreating || !d.alive(rec) {
			d.log.Info("removing the leftovers of an instance without a VM process", "instance", rec.ID, "phase", rec.Phase)
			d.removeInstance(ctx, rec)
		}
	}
}

// endProcess ends the VM process of a record that is being removed, if there is
// one. A launch that crashed (or failed to read the pid file) after qemu
// daemonised has a record without a pid: the pid file in the instance directory
// then names the process. Without this the VM would run on, with its tap and
// directory gone and nothing that ever ends it. Alive guards against a reused
// pid.
func (d *Driver) endProcess(ctx context.Context, rec *record) {
	pid := rec.PID
	if pid == 0 {
		raw, err := os.ReadFile(filepath.Join(d.instanceDir(rec.ID), filePID)) //nolint:gosec // the driver's own file
		if err != nil {
			return
		}
		if pid, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
			return
		}
	}
	if err := d.host.Terminate(context.WithoutCancel(ctx), pid, rec.ID); err != nil {
		d.log.Error(err, "ending the VM process of a removed instance", "instance", rec.ID, "pid", pid)
	}
}

// removeInstance removes the tap, moves the serial log to the log directory and
// removes the instance directory; failures are logged (the instance is gone
// either way for the caller).
func (d *Driver) removeInstance(ctx context.Context, rec *record) {
	d.endProcess(ctx, rec)
	if rec.Tap != "" {
		if _, err := d.host.Run(ctx, "ip", ipLink, "delete", rec.Tap); err != nil {
			d.log.V(1).Info("deleting the tap device", "tap", rec.Tap, "error", err.Error())
		}
	}
	dir := d.instanceDir(rec.ID)
	if d.opts.LogDir != "" {
		name := rec.NodeName
		if name == "" {
			name = rec.ID
		}
		if err := os.MkdirAll(d.opts.LogDir, 0o755); err == nil { //nolint:gosec // a log directory
			dst := filepath.Join(d.opts.LogDir, name+"-"+rec.ID+".serial.log")
			if err := copyFile(filepath.Join(dir, fileSerialLog), dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				d.log.Error(err, "keeping the serial log", "instance", rec.ID)
			}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		d.log.Error(err, "removing the instance directory", "instance", rec.ID)
	}
	d.touchNetworks(rec.Networks)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // the driver's own files
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // a log file
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// instance describes a record the way the contract wants it.
func (d *Driver) instance(rec *record) *vendors.Instance {
	state := vendors.StateRunning
	switch {
	case rec.Phase == phaseCreating:
		state = vendors.StatePending
	case !d.alive(rec):
		state = vendors.StateTerminated
	}
	inst := &vendors.Instance{
		Ref: vendors.NewInstanceRef(rec.Region, rec.ID), Region: rec.Region, Zone: rec.Zone, Type: rec.Type,
		CapacityType: rec.CapacityType, State: state, CreatedAt: rec.CreatedAt, Tags: map[vendors.TagKey]string{},
	}
	for k, v := range rec.Tags {
		inst.Tags[vendors.TagKey(k)] = v
	}
	if state == vendors.StateRunning {
		inst.Addresses = d.addresses(rec)
	}
	return inst
}

// addresses finds the VM's IP addresses in the ARP table of its bridge: the
// bridge's DHCP server is not ours to ask.
func (d *Driver) addresses(rec *record) []string {
	neigh, err := d.host.Neighbors()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range neigh {
		if n.MAC == rec.MAC && n.Dev == rec.Bridge {
			out = append(out, n.IP)
		}
	}
	return out
}

// CreateInstance implements vendors.Driver.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	key := req.IdempotencyKey
	if key == "" {
		key = req.Tags[vendors.TagNodeClaim]
	}
	if key == "" {
		return nil, vendors.Errorf(vendors.Fatal, "a launch needs an idempotency key")
	}
	id := instanceID(key)
	mu := d.lock(id)
	mu.Lock()
	defer mu.Unlock()
	if rec, err := d.readRecord(id); err == nil {
		if rec.Phase == phaseRunning && d.alive(rec) {
			return d.instance(rec), nil
		}
		// The first attempt did not get as far as a running VM, or the VM is
		// gone: start over, the key is unused.
		d.removeInstance(ctx, rec)
	}
	rec, err := d.launch(ctx, id, key, req)
	if err != nil {
		if rec != nil {
			d.removeInstance(context.WithoutCancel(ctx), rec)
		}
		return nil, err
	}
	return d.instance(rec), nil
}

func (d *Driver) validate(req vendors.CreateRequest) (static.Type, error) {
	typ, ok := d.types[req.InstanceType]
	switch {
	case req.Region != Region:
		return typ, vendors.Errorf(vendors.Fatal, "region %q is not a region of the qemu driver", req.Region)
	case req.Zone != Zone:
		return typ, vendors.Errorf(vendors.Fatal, "zone %q is not a zone of the qemu driver", req.Zone)
	case !ok:
		return typ, vendors.NewError(vendors.Fatal, "InvalidInstanceType",
			fmt.Errorf("unknown instance type %q", req.InstanceType))
	case req.CapacityType != vendors.CapacityOnDemand:
		return typ, vendors.NewError(vendors.Unsupported, "CapacityType",
			fmt.Errorf("the qemu driver has no %s capacity", req.CapacityType))
	case len(req.UserData) > maxUserData:
		return typ, vendors.NewError(vendors.Fatal, "UserDataTooLarge",
			fmt.Errorf("user-data is %d bytes, the limit is %d", len(req.UserData), maxUserData))
	case req.Arch != "" && req.Arch != typ.Arch:
		return typ, vendors.Errorf(vendors.Fatal, "%s is %s, not %s", req.InstanceType, typ.Arch, req.Arch)
	}
	return typ, nil
}

// bridgeOf returns the bridge the instance attaches to: the first network of the
// prepared resources, else the default bridge.
func (d *Driver) bridgeOf(req vendors.CreateRequest) (bridge string, networks []string) {
	if req.Prepared != nil {
		for _, r := range req.Prepared.Resources {
			if r.Kind != KindNetwork {
				continue
			}
			networks = append(networks, r.ID)
			if bridge == "" {
				bridge = r.ID
			}
		}
	}
	if bridge == "" {
		bridge = d.opts.Bridge
	}
	return bridge, networks
}

// launch makes the disks, the tap and the VM process. It returns the record also
// on failure, so that the caller can clean up what exists.
func (d *Driver) launch(ctx context.Context, id, key string, req vendors.CreateRequest) (*record, error) {
	typ, err := d.validate(req)
	if err != nil {
		return nil, err
	}
	set, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings", err)
	}
	accel, err := d.accelerator(set)
	if err != nil {
		return nil, err
	}
	bridge, networks := d.bridgeOf(req)
	bridges, err := d.host.Bridges()
	if err != nil {
		return nil, vendors.NewError(vendors.Retryable, "Bridges", err)
	}
	if !slices.ContainsFunc(bridges, func(b bridgeInfo) bool { return b.Name == bridge }) {
		return nil, vendors.NewError(vendors.ZoneUnusable, "NoBridge",
			fmt.Errorf("the bridge %q does not exist on this machine", bridge))
	}
	if !filepath.IsLocal(req.Image.Ref) {
		return nil, vendors.NewError(vendors.Fatal, "NoImage",
			fmt.Errorf("the image reference %q is not a cache path", req.Image.Ref))
	}
	rel := release{dir: filepath.Join(d.opts.CacheDir, req.Image.Ref)}
	if _, ok := d.imgs.cached(rel); !ok {
		return nil, vendors.NewError(vendors.Fatal, "NoImage", fmt.Errorf("the image %q is not in the cache", req.Image.Ref))
	}

	rec := &record{
		ID: id, Key: key, Phase: phaseCreating, Region: req.Region, Zone: req.Zone, Type: req.InstanceType,
		CapacityType: req.CapacityType, Arch: typ.Arch, CreatedAt: d.clock.Now().UTC(), Tags: map[string]string{},
		NodeName: req.Node.Name, MAC: macFor(id), Tap: tapFor(id), Bridge: bridge, Networks: networks,
	}
	for k, v := range req.Tags {
		rec.Tags[string(k)] = v
	}
	dir := d.instanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // an instance directory
		return nil, vendors.NewError(vendors.Fatal, "StateDir", err)
	}
	if err := d.writeRecord(rec); err != nil {
		return rec, vendors.NewError(vendors.Fatal, "StateDir", err)
	}

	spec := vmSpec{
		ID: id, Arch: typ.Arch, Accel: accel, VCPUs: typ.VCPUs, MemoryMiB: int(typ.MemoryMiB), Dir: dir,
		RootFile: rootFile(set.rootFormat()), RootFormat: set.rootFormat(), EFICode: rel.efiCode(),
		Tap: rec.Tap, MAC: rec.MAC, NICModel: nicModel(req.Settings.Network),
	}
	spec.Disks = diskPlan(set, typ.LocalStorageCount, localDiskBytes(typ))
	if pin, err := d.pinning(req.Settings.Placement, typ.VCPUs, id); err != nil {
		return rec, err
	} else {
		spec.Pin = pin
	}
	if err := d.prepareFiles(ctx, spec, rel, set, req.UserData); err != nil {
		return rec, err
	}
	if err := d.attachTap(ctx, rec); err != nil {
		return rec, err
	}
	bin, args, err := spec.command()
	if err != nil {
		return rec, vendors.NewError(vendors.Fatal, "VMCommand", err)
	}
	if _, err := d.host.Run(context.WithoutCancel(ctx), bin, args...); err != nil {
		return rec, classifyStart(err)
	}
	raw, err := os.ReadFile(spec.path(filePID)) //nolint:gosec // the driver's own file
	if err != nil {
		return rec, vendors.NewError(vendors.Retryable, "VMStart", fmt.Errorf("reading the pid file: %w", err))
	}
	if rec.PID, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
		return rec, vendors.NewError(vendors.Retryable, "VMStart", fmt.Errorf("the pid file holds %q", raw))
	}
	rec.Phase = phaseRunning
	if err := d.writeRecord(rec); err != nil {
		return rec, vendors.NewError(vendors.Fatal, "StateDir", err)
	}
	d.touchNetworks(rec.Networks)
	if d.opts.LaunchDelay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(d.opts.LaunchDelay):
		}
	}
	d.log.Info("started a VM", "instance", id, "type", req.InstanceType, "pid", rec.PID, "bridge", bridge,
		"accelerator", accel)
	return rec, nil
}

func localDiskBytes(t static.Type) int64 {
	if t.LocalStorageCount == 0 {
		return 0
	}
	return int64(t.LocalStorageGB * float64(gigabyte) / float64(t.LocalStorageCount))
}

// accelerator resolves auto to kvm when /dev/kvm is usable and to tcg otherwise.
func (d *Driver) accelerator(s settings) (string, error) {
	switch s.Accelerator {
	case "", qemuapi.Accelerator("auto"):
		if d.host.KVMUsable() {
			return accelKVM, nil
		}
		return accelTCG, nil
	case accelKVM:
		if !d.host.KVMUsable() {
			return "", vendors.NewError(vendors.Unsupported, "NoKVM",
				errors.New("the accelerator is kvm but /dev/kvm is not usable"))
		}
		return accelKVM, nil
	case accelTCG:
		return accelTCG, nil
	}
	return "", vendors.Errorf(vendors.Fatal, "unknown accelerator %q", s.Accelerator)
}

func nicModel(network json.RawMessage) string {
	var n qemuapi.Network
	if len(network) > 0 {
		_ = json.Unmarshal(network, &n)
	}
	return string(n.Model)
}

// pinning chooses the host CPUs of a VM with a placement profile that pins:
// consecutive CPUs, starting at a position derived from the instance ID, so that
// VMs spread over the host.
func (d *Driver) pinning(placement json.RawMessage, vcpus int, id string) ([]int, error) {
	if len(placement) == 0 {
		return nil, nil
	}
	var p qemuapi.Placement
	if err := json.Unmarshal(placement, &p); err != nil {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings", fmt.Errorf("decoding the placement profile: %w", err))
	}
	if p.PinCPUs == nil || !*p.PinCPUs {
		return nil, nil
	}
	cpus := d.host.CPUs()
	if vcpus > cpus {
		return nil, vendors.NewError(vendors.Unsupported, "Pinning",
			fmt.Errorf("%d vCPUs cannot be pinned on %d host CPUs", vcpus, cpus))
	}
	sum := sha256.Sum256([]byte(id))
	start := int(sum[0]) % cpus
	out := make([]int, vcpus)
	for i := range out {
		out[i] = (start + i) % cpus
	}
	slices.Sort(out)
	return out, nil
}

// prepareFiles creates the images, the firmware variables and the Ignition file.
func (d *Driver) prepareFiles(ctx context.Context, spec vmSpec, rel release, set settings, userData []byte) error {
	ctx = context.WithoutCancel(ctx)
	rootSize := set.rootSize()
	if virtual, err := qcow2Size(rel.image()); err != nil {
		return vendors.NewError(vendors.Fatal, "ImageFormat", err)
	} else if rootSize < virtual {
		return vendors.NewError(vendors.Fatal, "RootDiskTooSmall",
			fmt.Errorf("the root disk of %d bytes is smaller than the image (%d bytes)", rootSize, virtual))
	}
	size := strconv.FormatInt(rootSize, 10)
	var err error
	if spec.RootFormat == "raw" {
		// A raw image cannot have a backing file: it is a full copy.
		_, err = d.host.Run(ctx, "qemu-img", "convert", "-f", "qcow2", "-O", "raw", rel.image(), spec.path(spec.RootFile))
		if err == nil {
			_, err = d.host.Run(ctx, "qemu-img", "resize", "-f", "raw", spec.path(spec.RootFile), size)
		}
	} else {
		_, err = d.host.Run(ctx, "qemu-img", "create", "-q", "-f", "qcow2", "-b", rel.image(), "-F", "qcow2",
			spec.path(spec.RootFile), size)
	}
	if err != nil {
		return classifyDisk("RootDisk", err)
	}
	for _, dk := range spec.Disks {
		if _, err := d.host.Run(ctx, "qemu-img", "create", "-q", "-f", string(dk.Format), spec.path(dk.File),
			strconv.FormatInt(dk.Size, 10)); err != nil {
			return classifyDisk("DataDisk", err)
		}
	}
	if err := copyFile(rel.efiVars(), spec.path(fileEFIVars)); err != nil {
		return vendors.NewError(vendors.Fatal, "EFIVars", err)
	}
	if err := os.WriteFile(spec.path(fileIgnition), userData, 0o600); err != nil {
		return vendors.NewError(vendors.Fatal, "UserData", err)
	}
	return nil
}

// attachTap creates the VM's tap device and puts it on the bridge. QEMU opens it
// with script=no: the driver owns its lifetime.
func (d *Driver) attachTap(ctx context.Context, rec *record) error {
	ctx = context.WithoutCancel(ctx)
	// A tap of a launch that crashed.
	_, _ = d.host.Run(ctx, "ip", ipLink, "delete", rec.Tap)
	steps := [][]string{
		{"tuntap", ipAdd, "dev", rec.Tap, "mode", "tap", "user", strconv.Itoa(os.Geteuid())},
		{ipLink, ipSet, rec.Tap, "master", rec.Bridge},
		{ipLink, ipSet, rec.Tap, "up"},
	}
	for _, s := range steps {
		if _, err := d.host.Run(ctx, "ip", s...); err != nil {
			return vendors.NewError(vendors.Retryable, "Tap", err)
		}
	}
	return nil
}

// GetInstance implements vendors.Driver.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	rec, ok := d.lookup(ref)
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "instance %s does not exist", ref)
	}
	inst := d.instance(rec)
	if inst.Gone() {
		// The VM exited by itself (poweroff): report it once, then forget it.
		d.reap(ctx, rec)
	}
	return inst, nil
}

func (d *Driver) lookup(ref vendors.InstanceRef) (*record, bool) {
	rec, err := d.readRecord(ref.ID())
	if err != nil || rec.Region != ref.Region() {
		return nil, false
	}
	return rec, true
}

// reap removes the leftovers of a VM whose process is gone, unless a launch of
// the instance is in progress.
func (d *Driver) reap(ctx context.Context, rec *record) {
	mu := d.lock(rec.ID)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()
	if cur, err := d.readRecord(rec.ID); err == nil && cur.Phase == phaseRunning && !d.alive(cur) {
		d.removeInstance(ctx, cur)
	}
}

// FindInstance implements vendors.Driver: the oldest live instance with the
// tags.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	list, err := d.ListInstances(ctx, []string{region}, tags)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance in %s with the tags", region)
	}
	return list[0], nil
}

// ListInstances implements vendors.Driver; no regions means every region. A VM
// whose process is gone is not listed (and its leftovers are removed).
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	var out []*vendors.Instance
	for _, rec := range d.records() {
		if len(regions) > 0 && !slices.Contains(regions, rec.Region) {
			continue
		}
		inst := d.instance(rec)
		if inst.Gone() {
			d.reap(ctx, rec)
			continue
		}
		match := true
		for k, v := range tags {
			if inst.Tags[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, inst)
		}
	}
	slices.SortFunc(out, func(a, b *vendors.Instance) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Ref), string(b.Ref))
	})
	return out, nil
}

// DeleteInstance implements vendors.Driver: the VM process is ended, the tap and
// the instance directory removed; an unknown instance is not an error.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	mu := d.lock(ref.ID())
	mu.Lock()
	defer mu.Unlock()
	rec, ok := d.lookup(ref)
	if !ok {
		return nil
	}
	if rec.PID > 0 {
		if err := d.host.Terminate(ctx, rec.PID, rec.ID); err != nil {
			return vendors.NewError(vendors.Retryable, "VMStop", err)
		}
	}
	d.removeInstance(ctx, rec)
	return nil
}

// classifyStart classifies the failure of the QEMU process to start. QEMU has no
// error codes, only messages (the exception of the rule that drivers classify by
// code, overview section 7.2): a host that cannot allocate the VM's memory is out
// of capacity, one that cannot use KVM cannot offer this accelerator, anything
// else is transient.
func classifyStart(err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "cannot allocate memory"):
		return vendors.NewError(vendors.InsufficientCapacity, "NoMemory", err)
	case strings.Contains(msg, "kvm") && (strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "no such file") || strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "could not access")):
		return vendors.NewError(vendors.Unsupported, "NoKVM", err)
	}
	return vendors.NewError(vendors.Retryable, "VMStart", err)
}

// classifyDisk classifies the failure to create a disk image: a full disk is the
// machine's quota, the rest is fatal.
func classifyDisk(code string, err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "no space left on device") {
		return vendors.NewError(vendors.QuotaExceeded, "NoSpace", err)
	}
	return vendors.NewError(vendors.Fatal, code, err)
}
