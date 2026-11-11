//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// maxUserData is the user-data size limit the fake enforces: AWS's 16 KiB, the
// smallest of the vendors (overview section 8.2).
const maxUserData = 16 * 1024

// rootDiskBytes is the fake's default root disk.
var rootDiskBytes = resource.MustParse("64Gi")

func init() {
	vendors.Register(Name, func() vendors.Driver { return &Driver{} })
}

// instance is the fake's record of an instance.
type instance struct {
	vendors.Instance
	claim    string
	nodeName string
	vcpus    int64
	key      string
	// userData is the size of the user-data the instance was launched with
	// (the fake boots nothing; the L3 lane checks the rendering by its size).
	userData int
	// settings, profiles, resources and devices are what the launch was given
	// (the resolved vendor settings, the profiles applied, the prepared
	// resources and device hints): the L3 scenarios assert on them.
	settings  string
	profiles  map[string]string
	resources []string
	devices   []string
}

// recordPrepared keeps what Prepare made ready: the resources the instance uses
// and the device hints it was given.
func (i *instance) recordPrepared(p *vendors.Prepared) {
	if p == nil {
		return
	}
	for _, r := range p.Resources {
		i.resources = append(i.resources, r.ID)
	}
	for _, dev := range p.Devices {
		i.devices = append(i.devices, dev.Name+"="+dev.ByID+" "+dev.Filesystem+" "+dev.MountPath)
	}
	if p.Local != nil {
		i.devices = append(i.devices, fmt.Sprintf("local=%s %s %s x%d", p.Local.DevicePattern, p.Local.Policy,
			p.Local.MountPath, p.Local.Count))
	}
}

// Driver is the fake driver.
type Driver struct {
	opts    Options
	optsSet bool
	cfg     vendors.Config
	clock   clock.Clock

	// cfgMu guards the cached steering ConfigMap; never held while taking mu
	// except by refreshConfig itself (order: cfgMu, then mu).
	cfgMu     sync.Mutex
	cfgAt     time.Time
	cfgLoaded bool
	conf      *config

	dataMu sync.Mutex
	data   *catalogData

	// resMu guards the fake account's resources (order: resMu, then mu).
	resMu     sync.Mutex
	resources map[string]*resourceRecord
	resLoaded bool
	// selMode is the SelectorMode.
	selMode atomic.Int32
	// unsupportedEpoch changes with the steering's unsupported and rootDisks rules.
	unsupportedEpoch atomic.Uint64

	mu        sync.Mutex
	seq       int
	instances map[string]*instance // by ID
	byKey     map[string]string    // idempotency key -> ID
	orphaned  map[string]bool
	// poweredOff are the NodeClaims of the powerOff key already handled.
	poweredOff map[string]bool
	ruleHits   map[int]int
	calls      []callRecord
	dirty      bool
	flushing   bool
	// revision counts the changes of the published state; published is the
	// revision the state ConfigMap holds; failures counts the failed writes in
	// a row (the retry backoff).
	revision  uint64
	published uint64
	failures  int
}

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.EpochReporter      = (*Driver)(nil)
	_ vendors.QuotaReporter      = (*Driver)(nil)
	_ vendors.OfferingLister     = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
)

// New returns a driver with the given options (for tests; the registry's
// factory returns a driver that reads its options from the Init context).
func New(opts Options) *Driver {
	d := &Driver{opts: opts, optsSet: true}
	return d
}

// Vendor returns the impersonated Keeper vendor.
func (d *Driver) Vendor() string {
	if d.opts.CatalogVendor == "" {
		return DefaultCatalogVendor
	}
	return d.opts.CatalogVendor
}

// Init prepares the driver. It reads no Keeper data: that happens on first use,
// with retries by the callers, since the manager may start before Keeper (the
// replay server of the L3 lane) is ready.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	if !d.optsSet {
		d.opts = DefaultOptions()
		if o := FromContext(ctx); o != nil {
			d.opts = *o
		}
	}
	d.opts.fill()
	d.cfg = cfg
	if cfg.Log.GetSink() == nil {
		d.cfg.Log = logr.Discard()
	}
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.instances, d.byKey = map[string]*instance{}, map[string]string{}
	d.resources = map[string]*resourceRecord{}
	// The revisions count up from the start time, so that a restarted
	// controller never publishes a lower revision than its predecessor did and
	// a waiter for the next revision cannot be released by the old state.
	d.revision = uint64(d.clock.Now().UnixMilli()) //nolint:gosec // a time, not a security value
	d.registerMetrics()
	d.orphaned, d.poweredOff, d.ruleHits = map[string]bool{}, map[string]bool{}, map[int]int{}
	return nil
}

func (o *Options) fill() {
	def := DefaultOptions()
	if o.CatalogVendor == "" {
		o.CatalogVendor = def.CatalogVendor
	}
	if o.ConfigNamespace == "" {
		o.ConfigNamespace = def.ConfigNamespace
	}
	if o.ConfigName == "" {
		o.ConfigName = def.ConfigName
	}
	if o.StateName == "" {
		o.StateName = def.StateName
	}
	if o.ResourcesName == "" {
		o.ResourcesName = def.ResourcesName
	}
}

// catalog returns the impersonated vendor's tables, reading them on first use.
func (d *Driver) catalog(ctx context.Context) (*catalogData, error) {
	d.dataMu.Lock()
	defer d.dataMu.Unlock()
	if d.data != nil {
		return d.data, nil
	}
	data, err := loadCatalogData(ctx, d.cfg.KeeperURL, d.Vendor())
	if err != nil {
		return nil, vendors.NewError(vendors.Retryable, "CatalogUnavailable", err)
	}
	d.data = data
	return data, nil
}

// loadedCatalog is catalog for the methods without an error return: it returns
// nil when the tables cannot be read yet.
func (d *Driver) loadedCatalog() *catalogData {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := d.catalog(ctx)
	if err != nil {
		return nil
	}
	return data
}

// TagKey implements vendors.Driver.
func (d *Driver) TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

// Regions implements vendors.Driver.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	data, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.Region, 0, len(data.regions))
	for _, r := range data.regions {
		out = append(out, vendors.Region{Name: r})
	}
	slices.SortFunc(out, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Zones implements vendors.Driver.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	data, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(data.regions, region) {
		return nil, vendors.Errorf(vendors.NotFound, "region %q is not a region of %s", region, d.Vendor())
	}
	zones := slices.Clone(data.zones[region])
	slices.SortFunc(zones, func(a, b vendors.Zone) int { return strings.Compare(a.Name, b.Name) })
	return zones, nil
}

// ResolveImage implements vendors.Driver. Without an `images` selector every
// release has a synthesised image. With one, the image is selected from the
// `images` list of the steering ConfigMap by the shared matcher, narrowed to the
// request's architecture, and must say it is the pinned release (ADR-017): an
// image that says another one is ImageVersionMismatch, one that says none
// ImageVersionUnknown, and neither is returned.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if req.Arch != "amd64" && req.Arch != "arm64" {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q", req.Arch)
	}
	if req.Use.IsDefault() {
		return &vendors.Image{
			Ref:           fmt.Sprintf("fake-flatcar-%s-%s-%s", req.Channel, req.Version, req.Arch),
			RootDiskBytes: rootDiskBytes.Value(),
			Channel:       req.Channel, Version: req.Version,
		}, nil
	}
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "no image for %s in %s: match.architecture %v excludes it",
			req.Arch, req.Region, req.Use.Match.Architecture)
	}
	refs, err := d.Resolve(ctx, KindImages, req.Region, use)
	if err != nil {
		return nil, selector.ImageError(err, req.Region, req.Arch)
	}
	if len(refs) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no image for %s in %s", req.Arch, req.Region)
	}
	var rec existingResource
	for _, e := range d.current().images {
		if e.ID == refs[0].ID && (e.Region == req.Region || e.Region == selector.RegionAll) {
			rec = e
		}
	}
	channel, version := rec.Channel, rec.Version
	if channel == "" {
		channel = rec.Tags["flatcar-channel"]
	}
	if version == "" {
		version = rec.Tags["flatcar-version"]
	}
	if err := vendors.CheckImageVersion(rec.ID, channel, version, req); err != nil {
		return nil, err
	}
	return &vendors.Image{Ref: rec.ID, RootDiskBytes: rootDiskBytes.Value(), Channel: channel, Version: version}, nil
}

// ReportImages implements vendors.ImageReporter: the image of the NodeClass's
// release per region of its vendor block (every region of the catalog without a
// list) and architecture, resolved like a launch does. A region without an image,
// or with one that is not the pinned release, is reported not ready with the
// reason.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	regions, _ := nc.Spec.Vendors.Regions(d.Vendor())
	if len(regions) == 0 {
		all, err := d.Regions(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			regions = append(regions, r.Name)
		}
	}
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = selector.DefaultChannel
	}
	var out []v1alpha1.ImageStatus
	for _, region := range regions {
		for _, arch := range []string{archAMD64, archARM64} {
			st := v1alpha1.ImageStatus{Region: region, Arch: arch}
			use, _, err := selector.ImageUse(nc, d, region, arch)
			var img *vendors.Image
			if err == nil {
				img, err = d.ResolveImage(ctx, vendors.ImageRequest{
					Region: region, Arch: arch, Channel: channel, Version: nc.Spec.Flatcar.Version, Use: use,
				})
			}
			switch {
			case err == nil:
				st.Ref, st.Ready = img.Ref, true
			case vendors.IsClass(err, vendors.Retryable):
				return nil, fmt.Errorf("%s/%s: %w", region, arch, err)
			default:
				st.Message = err.Error()
				if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) {
					st.Reason = code
				}
			}
			out = append(out, st)
		}
	}
	return out, nil
}

// Hardenings implements vendors.NodeHardener: the fake offers the metadata
// hardening so that the hook is exercised without a cloud (its address is the
// one of AWS, GCP and Azure).
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	if slices.Contains(req.Hardening, vendors.HardeningMetadataFromPods) {
		f, err := vendors.BlockMetadataFromPods("169.254.169.254")
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// ValidateNodeClass accepts every NodeClass.
func (d *Driver) ValidateNodeClass(context.Context, *v1alpha1.SCNodeClass) error { return nil }

func (d *Driver) record(op, region, zone, typ, capacityType, claim, result string) {
	d.calls = append(d.calls, callRecord{
		Time: d.clock.Now().UTC(), Op: op, Region: region, Zone: zone, InstanceType: typ,
		CapacityType: capacityType, NodeClaim: claim, Result: result,
	})
	if len(d.calls) > maxCalls {
		d.calls = slices.Clone(d.calls[len(d.calls)-maxCalls:])
	}
	d.markDirtyLocked()
}

func copyInstance(i *instance) *vendors.Instance {
	out := i.Instance
	out.Tags = maps.Clone(i.Tags)
	out.Addresses = slices.Clone(i.Addresses)
	return &out
}

// CreateInstance implements vendors.Driver.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	d.reloadConfig(ctx, true)
	conf := d.current()
	claim := req.Node.Name
	if claim == "" {
		claim = req.Tags[vendors.TagNodeClaim]
	}

	d.mu.Lock()
	if id, ok := d.byKey[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		if inst := d.instances[id]; inst != nil && inst.State != vendors.StateTerminated {
			out := copyInstance(inst)
			d.mu.Unlock()
			return out, nil
		}
	}
	d.mu.Unlock()

	fail := func(class vendors.Class, code string, err error) (*vendors.Instance, error) {
		d.mu.Lock()
		d.record("create", req.Region, req.Zone, req.InstanceType, req.CapacityType, claim, string(class))
		d.mu.Unlock()
		return nil, vendors.NewError(class, code, err)
	}

	if len(req.UserData) > maxUserData {
		return fail(vendors.Fatal, "UserDataTooLarge",
			fmt.Errorf("user-data is %d bytes, the limit is %d", len(req.UserData), maxUserData))
	}
	data, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	srv, ok := data.servers[req.InstanceType]
	if !ok {
		return fail(vendors.Fatal, "InvalidInstanceType", fmt.Errorf("unknown instance type %q", req.InstanceType))
	}
	if !slices.ContainsFunc(data.zones[req.Region], func(z vendors.Zone) bool { return z.Name == req.Zone }) &&
		len(data.zones[req.Region]) > 0 {
		return fail(vendors.Fatal, "InvalidZone", fmt.Errorf("zone %q is not a zone of region %q", req.Zone, req.Region))
	}
	if req.CapacityType != vendors.CapacityOnDemand && req.CapacityType != vendors.CapacitySpot {
		return fail(vendors.Fatal, "InvalidCapacityType", fmt.Errorf("unknown capacity type %q", req.CapacityType))
	}

	// Injected errors.
	d.mu.Lock()
	if i, hit := conf.matchRule(d.Vendor()+".", req); hit {
		rule := conf.errors[i]
		if rule.Count == 0 || d.ruleHits[i] < rule.Count {
			d.ruleHits[i]++
			d.record("create", req.Region, req.Zone, req.InstanceType, req.CapacityType, claim, string(rule.Error))
			d.mu.Unlock()
			err := vendors.NewError(rule.Error, "Injected"+string(rule.Error),
				errors.New("injected by the fake driver's ConfigMap"))
			err.AccountWide = rule.AccountWide
			return nil, err
		}
	}
	d.mu.Unlock()

	delay := d.opts.LaunchDelay
	if conf.launchDelay != nil {
		delay = *conf.launchDelay
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, vendors.NewError(vendors.Retryable, "Cancelled", ctx.Err())
		case <-time.After(delay):
		}
	}

	d.mu.Lock()
	// Re-check idempotency: a concurrent call with the same key may have won.
	if id, ok := d.byKey[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		if inst := d.instances[id]; inst != nil && inst.State != vendors.StateTerminated {
			out := copyInstance(inst)
			d.mu.Unlock()
			return out, nil
		}
	}
	if bucket, scope, exceeded := d.exceedsQuotaLocked(conf, req, srv); exceeded {
		d.record("create", req.Region, req.Zone, req.InstanceType, req.CapacityType, claim, string(vendors.QuotaExceeded))
		d.mu.Unlock()
		err := vendors.NewError(vendors.QuotaExceeded, "VcpuLimitExceeded",
			fmt.Errorf("the quota bucket %s of %s has no room for %d more", bucket, req.Region, srv.vcpus))
		// An account-wide bucket is the whole account's limit.
		err.AccountWide = scope == vendors.QuotaScopeGlobal
		return nil, err
	}
	d.seq++
	id := fmt.Sprintf("i-fake-%06d", d.seq)
	inst := &instance{
		Instance: vendors.Instance{
			Ref: vendors.NewInstanceRef(req.Region, id), Region: req.Region, Zone: req.Zone, Type: req.InstanceType,
			CapacityType: req.CapacityType, State: vendors.StateRunning, CreatedAt: d.clock.Now(),
			Tags:      vendors.IdempotencyTag(req.Tags, req.IdempotencyKey),
			Addresses: []string{fmt.Sprintf("10.%d.%d.%d", d.seq/65536%256, d.seq/256%256, d.seq%256)},
		},
		claim: claim, nodeName: req.Node.Name, vcpus: srv.vcpus, key: req.IdempotencyKey,
		userData: len(req.UserData), settings: string(req.Settings.Raw), profiles: maps.Clone(req.Settings.Profiles),
	}
	inst.recordPrepared(req.Prepared)
	d.instances[id] = inst
	if req.IdempotencyKey != "" {
		d.byKey[req.IdempotencyKey] = id
	}
	d.mu.Unlock()

	if err := d.createNode(ctx, req); err != nil {
		d.mu.Lock()
		delete(d.instances, id)
		delete(d.byKey, req.IdempotencyKey)
		d.record("create", req.Region, req.Zone, req.InstanceType, req.CapacityType, claim, string(vendors.Retryable))
		d.mu.Unlock()
		return nil, vendors.NewError(vendors.Retryable, "NodeCreateFailed", err)
	}

	d.touchResources(inst.resources)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record("create", req.Region, req.Zone, req.InstanceType, req.CapacityType, claim, "ok")
	return copyInstance(inst), nil
}

// exceedsQuotaLocked reports a consumed bucket that cannot take the instance.
func (d *Driver) exceedsQuotaLocked(
	conf *config, req vendors.CreateRequest, srv serverInfo,
) (bucket string, scope vendors.QuotaScope, exceeded bool) {
	ids, known := conf.consumes(srv.family, req.CapacityType)
	if !known {
		return "", "", false
	}
	for _, id := range ids {
		b, ok := conf.bucket(id, req.Region)
		if !ok {
			continue
		}
		used := b.Used + d.ownUsageLocked(conf, id, req.Region, b.Scope)
		if b.Limit < used+srv.vcpus {
			return id, b.Scope, true
		}
	}
	return "", "", false
}

// ownUsageLocked sums the vCPUs of the fake's own live instances that consume
// the bucket (in the region for a regional bucket).
func (d *Driver) ownUsageLocked(conf *config, bucket, region string, scope vendors.QuotaScope) int64 {
	var sum int64
	for _, i := range d.instances {
		if i.Gone() || (scope == vendors.QuotaScopeRegion && i.Region != region) {
			continue
		}
		if slices.Contains(d.bucketsOf(conf, i), bucket) {
			sum += i.vcpus
		}
	}
	return sum
}

func (d *Driver) bucketsOf(conf *config, i *instance) []string {
	family := ""
	if data := d.cachedData(); data != nil {
		family = data.servers[i.Type].family
	}
	ids, _ := conf.consumes(family, i.CapacityType)
	return ids
}

func (d *Driver) cachedData() *catalogData {
	d.dataMu.Lock()
	defer d.dataMu.Unlock()
	return d.data
}

// GetInstance implements vendors.Driver.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	d.refreshConfig(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	inst, ok := d.instances[ref.ID()]
	if !ok || inst.Region != ref.Region() {
		return nil, vendors.Errorf(vendors.NotFound, "instance %s does not exist", ref)
	}
	return copyInstance(inst), nil
}

func hasTags(have, want map[vendors.TagKey]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// FindInstance implements vendors.Driver: the oldest live instance with the
// tags, so a retry finds the first launch.
func (d *Driver) FindInstance(ctx context.Context, region string,
	tags map[vendors.TagKey]string) (*vendors.Instance, error) {
	list, err := d.ListInstances(ctx, []string{region}, tags)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance in %s with the tags", region)
	}
	return list[0], nil
}

// ListInstances implements vendors.Driver; no regions means every region.
// Powered-off (terminating) instances are listed, terminated ones are not.
func (d *Driver) ListInstances(ctx context.Context, regions []string,
	tags map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	d.refreshConfig(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*vendors.Instance
	for _, i := range d.instances {
		if i.State == vendors.StateTerminated || (len(regions) > 0 && !slices.Contains(regions, i.Region)) ||
			!hasTags(i.Tags, tags) {
			continue
		}
		out = append(out, copyInstance(i))
	}
	slices.SortFunc(out, func(a, b *vendors.Instance) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Ref), string(b.Ref))
	})
	return out, nil
}

// PowerOff stops an instance the way a guest poweroff or a stop call outside
// SCarpenter would: it stays, reported terminating (its disks keep billing on a
// real vendor), until it is deleted. It reports whether the instance existed.
func (d *Driver) PowerOff(ref vendors.InstanceRef) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	inst, ok := d.instances[ref.ID()]
	if !ok || inst.Region != ref.Region() || inst.State == vendors.StateTerminated {
		return false
	}
	inst.State = vendors.StateTerminating
	d.record("poweroff", inst.Region, inst.Zone, inst.Type, inst.CapacityType, inst.claim, "ok")
	return true
}

// DeleteInstance implements vendors.Driver: terminated at once, idempotent; a
// powered-off instance is deleted like any other.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	d.mu.Lock()
	inst, ok := d.instances[ref.ID()]
	if !ok || inst.Region != ref.Region() || inst.State == vendors.StateTerminated {
		d.mu.Unlock()
		return nil
	}
	inst.State = vendors.StateTerminated
	node := inst.nodeName
	resIDs := slices.Clone(inst.resources)
	d.record("delete", inst.Region, inst.Zone, inst.Type, inst.CapacityType, inst.claim, "ok")
	d.mu.Unlock()
	d.touchResources(resIDs)
	if err := d.deleteNode(ctx, node); err != nil {
		return vendors.NewError(vendors.Retryable, "NodeDeleteFailed", errors.New(err.Error()))
	}
	return nil
}

// powerOffClaims powers off, once per NodeClaim of the powerOff key, the running
// instance of the NodeClaim and deletes its Node: a stopped node stops reporting,
// and the kwok Node would stay Ready for ever.
func (d *Driver) powerOffClaims(ctx context.Context, claims []string) {
	var nodes []string
	d.mu.Lock()
	for _, claim := range claims {
		if d.poweredOff[claim] {
			continue
		}
		for _, inst := range d.instances {
			if inst.claim != claim || inst.State != vendors.StateRunning {
				continue
			}
			d.poweredOff[claim] = true
			inst.State = vendors.StateTerminating
			d.record("poweroff", inst.Region, inst.Zone, inst.Type, inst.CapacityType, inst.claim, "ok")
			nodes = append(nodes, inst.nodeName)
		}
	}
	d.mu.Unlock()
	for _, node := range nodes {
		if err := d.deleteNode(ctx, node); err != nil {
			d.cfg.Log.Error(err, "deleting the Node of a powered-off instance", "node", node)
		}
	}
}

// createOrphans creates the instances of the ConfigMap's orphans list that do
// not exist yet: tagged with the cluster, without a Node.
func (d *Driver) createOrphans(ctx context.Context, orphans []orphanConfig) {
	if len(orphans) == 0 {
		return
	}
	data, err := d.catalog(ctx)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, o := range orphans {
		if d.orphaned[o.Name] {
			continue
		}
		d.orphaned[o.Name] = true
		d.seq++
		id := fmt.Sprintf("i-fake-%06d", d.seq)
		state := vendors.StateRunning
		if o.PoweredOff {
			state = vendors.StateTerminating
		}
		d.instances[id] = &instance{
			Instance: vendors.Instance{
				Ref: vendors.NewInstanceRef(o.Region, id), Region: o.Region, Zone: o.Zone, Type: o.InstanceType,
				CapacityType: o.CapacityType, State: state,
				CreatedAt: d.clock.Now().Add(-time.Duration(o.AgeSeconds) * time.Second),
				Tags: map[vendors.TagKey]string{
					vendors.TagCluster: d.cfg.ClusterName, vendors.TagNodeClaim: o.Name, vendors.TagManagedBy: vendors.ManagedByValue,
				},
			},
			claim: o.Name, vcpus: data.servers[o.InstanceType].vcpus,
		}
		d.record("create", o.Region, o.Zone, o.InstanceType, o.CapacityType, o.Name, "ok")
	}
}

// Quotas implements vendors.QuotaReporter.
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	return d.snapshot(ctx, region, vendors.QuotaScopeRegion)
}

// GlobalQuotas implements vendors.QuotaReporter.
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	return d.snapshot(ctx, "", vendors.QuotaScopeGlobal)
}

func (d *Driver) snapshot(ctx context.Context, region string,
	scope vendors.QuotaScope) (*vendors.QuotaSnapshot, error) {
	d.refreshConfig(ctx)
	if _, err := d.catalog(ctx); err != nil {
		return nil, err
	}
	conf := d.current()
	snap := &vendors.QuotaSnapshot{}
	if conf.quota == nil {
		return snap, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	counted := map[vendors.InstanceRef]bool{}
	for _, bc := range conf.quota.Buckets {
		b, _ := conf.bucket(bc.ID, region)
		if b.Scope != scope {
			continue
		}
		b.Used += d.ownUsageLocked(conf, b.ID, region, b.Scope)
		for _, i := range d.instances {
			if i.Gone() || (scope == vendors.QuotaScopeRegion && i.Region != region) {
				continue
			}
			if slices.Contains(d.bucketsOf(conf, i), b.ID) {
				counted[i.Ref] = true
			}
		}
		snap.Buckets = append(snap.Buckets, b)
	}
	snap.Counted = slices.Sorted(maps.Keys(counted))
	return snap, nil
}

// Consumption implements vendors.QuotaReporter: the buckets of the first
// matching rule with the type's vCPUs; unknown without a quota configuration.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	d.refreshConfig(context.Background())
	conf := d.current()
	data := d.loadedCatalog()
	if data == nil {
		return nil, false
	}
	srv, ok := data.servers[instanceType]
	if !ok {
		return nil, false
	}
	ids, known := conf.consumes(srv.family, capacityType)
	if !known {
		return nil, false
	}
	out := make([]vendors.Consumption, 0, len(ids))
	for _, id := range ids {
		out = append(out, vendors.Consumption{Bucket: id, Amount: srv.vcpus})
	}
	return out, true
}

// LaunchableTypes implements vendors.OfferingLister: every type in every zone
// except what the ConfigMap's notLaunchable lists.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	d.refreshConfig(ctx)
	data, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	zones := data.zones[region]
	conf := d.current()
	out := make(map[string][]string, len(data.servers))
	for typ := range data.servers {
		var ids []string
		for _, z := range zones {
			if !conf.blocked(region, z, typ) {
				ids = append(ids, z.ID)
			}
		}
		if len(ids) > 0 {
			slices.Sort(ids)
			out[typ] = ids
		}
	}
	return out, nil
}

func (c *config) blocked(region string, z vendors.Zone, typ string) bool {
	if c.quota == nil {
		return false
	}
	for _, n := range c.quota.NotLaunchable {
		if n.Region == region && (n.Zone == "" || n.Zone == z.Name || n.Zone == z.ID) && slices.Contains(n.Types, typ) {
			return true
		}
	}
	return false
}
