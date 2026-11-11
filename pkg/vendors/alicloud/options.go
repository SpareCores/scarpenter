/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// hoursPerMonth converts a GB-month price to an hourly one (00a section 10).
const hoursPerMonth = 730

// diskSerialPattern is the device hint of a data disk: ECS disks exist at launch
// and their serial is the disk ID, unknown before the launch, so the first-boot
// unit matches by size among the whole disks that are not mounted and have no
// partitions (the system disk has both), and its lock separates equal sizes. The
// serial of a virtio disk is the disk ID without "d-" (/dev/disk/by-id/virtio-<id>)
// and the NVMe pattern is unverified (plan 21 step 0), so no narrower pattern is
// claimed.
const diskSerialPattern = "*"

// defaultSettings are the built-in defaults (layer 1, plan 21 section 4): a
// cloud_essd PL0 system disk of 64 GiB, not encrypted (an encrypted system disk is
// refused in some zones), a public IPv4 of up to 100 Mbit/s pay by traffic, and
// metadata access without a token, which Ignition's aliyun provider needs.
const defaultSettings = `{"storage":{"root":{"category":"cloud_essd","performanceLevel":"PL0","size":"64Gi",` +
	`"encrypted":false}},` +
	`"internetMaxBandwidthOut":100,"internetChargeType":"PayByTraffic","metadataOptions":{"httpTokens":"optional"}}`

// encryptedRootDenied are the zones that refuse an encrypted system disk (the
// RunInstances page: Zone D of Hong Kong and Zone A of Singapore). With
// encrypted: true the offerings of these zones are unavailable, explicitly
// (ADR-017).
var encryptedRootDenied = map[string]bool{"ap-southeast-1a": true, "cn-hongkong-d": true}

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskForType implements vendors.NodeOptions: the system disk is a disk of
// the settings, not of the type.
func (d *Driver) RootDiskForType(vendors.VendorSettings, vendors.CatalogType) (int64, bool) {
	return 0, false
}

// RootDiskBytes implements vendors.NodeOptions: the size of the system disk, which
// the scheduler assumes as ephemeral storage.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 { return RootDiskBytes(s) }

func decodeStorage(raw json.RawMessage) (api.Storage, error) {
	var s api.Storage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

func decodeNetwork(raw json.RawMessage) error {
	var n api.Network
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("decoding the network profile: %w", err)
		}
	}
	return nil
}

func decodePlacement(raw json.RawMessage) (api.Placement, error) {
	var p api.Placement
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

// Supported implements vendors.NodeOptions: the profiles of the NodeClass that the
// instance type can honour in the zone (ADR-017: nothing is degraded silently).
//   - storage: the disk categories the family sells, the number of disks the type
//     takes, an encrypted system disk only where ECS allows one; local-storage
//     policies are supported by no type in v1 (deferred);
//   - network: any;
//   - placement: a deployment set by strategy or by name is accepted for every
//     type (which families each strategy takes is unverified: the launch error
//     settles it).
//
// The type facts come from DescribeInstanceTypes, read at runtime; until they are
// known a profile that depends on them is not offered, and the epoch of the node
// options changes when they arrive.
func (d *Driver) Supported(it vendors.CatalogType, zone string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	ti, known := d.cloud.Types.Get(it.APIReference)
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block, it.APIReference, zone, ti, known)
			case vendors.ProfileNetwork:
				ok = decodeNetwork(block) == nil
			case vendors.ProfilePlacement:
				_, err := decodePlacement(block)
				ok = err == nil
			}
			if ok {
				out[kind] = append(out[kind], name)
			}
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

// entryFamily says whether the type belongs to the families that sell
// cloud_essd_entry (ecs.u1 and ecs.e: plan 21 section 4).
func entryFamily(instanceType string) bool {
	fam, ok := FamilyOf(instanceType)
	if !ok {
		return false
	}
	name := strings.TrimPrefix(fam, "ecs.")
	return name == "u1" || strings.HasPrefix(name, "e-") || name == "e"
}

func supportsStorage(block json.RawMessage, instanceType, zone string, ti TypeInfo, known bool) bool {
	st, err := decodeStorage(block)
	if err != nil {
		return false
	}
	if ls := st.LocalStorage; ls != nil && ls.Policy != common.LocalStorageIgnore {
		return false
	}
	disks := make([]resolvedDisk, 0, 1+len(st.Data))
	if st.Root != nil {
		root := rootDisk(Settings{Storage: &st})
		if root.Encrypted && encryptedRootDenied[zone] {
			return false
		}
		disks = append(disks, root)
	}
	disks = append(disks, dataDisks(Settings{Storage: &st})...)
	for _, dk := range disks {
		if dk.Category == CategoryESSDEntry && !entryFamily(instanceType) {
			return false
		}
	}
	// The system disk is one of the disks the type takes (DiskQuantity; whether it
	// counts the system disk is unverified, so the check is the conservative one).
	if n := len(st.Data); n > 0 && (!known || (ti.DiskQuantity > 0 && int64(n)+1 > ti.DiskQuantity)) {
		return false
	}
	return true
}

// PriceDelta implements vendors.NodeOptions: the disks at Keeper's GB-month price
// per region and category (and level for ESSD) divided by 730. The public IPv4
// address is free and traffic is not priced (for any vendor). The price is
// incomplete when a component is not priced: a disk the source lacks a price for
// (cloud_auto, a level it has no row for), provisioned IOPS or bursting, and pay
// by bandwidth.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	disk := func(dk resolvedDisk) {
		price, ok := d.diskPrice(o.Region, dk.Category, dk.Level)
		if !ok {
			complete = false
		} else {
			total += float64(dk.GiB) * price / hoursPerMonth
		}
		if dk.Iops != nil || (dk.Bursting != nil && *dk.Bursting) {
			complete = false
		}
	}
	disk(rootDisk(st))
	for _, dk := range dataDisks(st) {
		disk(dk)
	}
	if _, charge := bandwidth(st, s.Networking.PublicIPv4); charge == string(api.PayByBandwidth) {
		complete = false
	}
	return total, complete
}

func (d *Driver) diskPrice(region, category, level string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, category, level)
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch
// ready through the resource provider (the engine decides whether a resource is
// brought, the account's default or managed): the vSwitch of the zone, the
// security groups, the deployment set of the profile and the device hints of the
// data disks for the first-boot unit. An error of the provider is returned as it
// is: a ResourceMissingInZone must reach the attempt loop with its code, so that
// it skips only the zone.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}

	// The vSwitch of the zone: among the resolved vSwitches, or the zone's vSwitch
	// of the managed network. The request names no zone: a managed network is one
	// resource per region (its VPC) and must not get a name per zone; the provider
	// expands {zone} of a `use` name pattern with the zone of the launch.
	subnetRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{Kind: KindSubnets, Settings: &req.Settings})
	if err != nil {
		return nil, err
	}
	if boolOr(req.Settings.Networking.PublicIPv6, false) {
		return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, fmt.Errorf(
			"publicIPv6 is not supported on Alibaba Cloud yet: it needs an IPv6 gateway the driver does not manage"))
	}
	sw, err := d.chooseVSwitch(ctx, req.Region, req.Zone, subnetRefs)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, vendors.ResourceRef{Kind: KindSubnets, Region: req.Region, ID: sw.id})

	groups, err := d.requireGroups(ctx, req, sw)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, groups...)

	if ds := pl.DeploymentSet; ds != nil {
		ref, err := d.requireDeploymentSet(ctx, req, ds)
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}

	if st.Storage != nil {
		for i, dk := range dataDisks(st) {
			raw := st.Storage.Data[i]
			prep.Devices = append(prep.Devices, vendors.DeviceHint{
				Name: raw.Name, Filesystem: orDefault(string(raw.Filesystem), string(common.FilesystemExt4)),
				MountPath: raw.MountPath, SerialPattern: diskSerialPattern, SizeBytes: dk.GiB * giB,
			})
		}
	}
	return prep, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// requireGroups returns the security groups of the launch: those the NodeClass
// brings (use), or the group of the NodeClass in the VPC of the vSwitch, and, for
// isolation run with a run key, the group of the run. Brought groups must be in
// the vSwitch's VPC; where they are not the zone is skipped.
func (d *Driver) requireGroups(
	ctx context.Context, req vendors.PrepareRequest, sw vswitchState,
) ([]vendors.ResourceRef, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params := SecurityGroupParams{
		VPC: sw.vpc,
		// Isolation run without a run key has no run to keep apart: the nodes of
		// the NodeClass are one implicit run and reach each other (a VPC-routed CNI
		// of the direct template needs it).
		SelfReference: net.Isolation == common.IsolationNodeClass ||
			(net.Isolation == common.IsolationRun && req.RunKey == ""),
	}
	if net.Inbound != nil {
		params.Inbound = net.Inbound.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSecurityGroups, Key: class + "/" + sw.vpc, Params: raw, Network: sw.vpc, Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Managed {
			continue
		}
		vpcID, err := d.groupVPC(ctx, req.Region, r.ID)
		if err != nil {
			return nil, err
		}
		if vpcID != sw.vpc {
			return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, fmt.Errorf(
				"the security group %s is in %s, not in %s, the VPC of the vSwitch in %s", r.ID, vpcID, sw.vpc, req.Zone))
		}
	}
	out := slices.Clone(refs)
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		raw, err := json.Marshal(SecurityGroupParams{VPC: sw.vpc, SelfReference: true})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunSecurityGroups, Key: class + "/" + sw.vpc + "/" + req.RunKey, Params: raw,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, run...)
	}
	return out, nil
}

type cachedSG struct {
	vpc   string
	until time.Time
}

// groupVPC returns the VPC of a security group (cached for the vSwitch TTL).
func (d *Driver) groupVPC(ctx context.Context, region, id string) (string, error) {
	key := region + "/" + id
	d.mu.Lock()
	if c, ok := d.sgVPC[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return c.vpc, nil
	}
	d.mu.Unlock()
	gs, err := d.describeGroups(ctx, region, vendors.UseRef{IDs: []string{id}})
	if err != nil {
		return "", err
	}
	if len(gs) == 0 {
		return "", vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("the security group %s does not exist in %s", id, region))
	}
	vpcID := deref(gs[0].VpcId)
	d.mu.Lock()
	d.sgVPC[key] = cachedSG{vpc: vpcID, until: d.clock.Now().Add(vswitchTTL)}
	d.mu.Unlock()
	return vpcID, nil
}

// requireDeploymentSet returns the deployment set of the profile: a set by name
// (use) or one created per profile and run key (a profile that creates sets needs
// a run key).
func (d *Driver) requireDeploymentSet(
	ctx context.Context, req vendors.PrepareRequest, ds *api.DeploymentSet,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindDeploymentSets}
	if ds.Use != nil {
		rr.Use = &vendors.UseRef{Name: ds.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates deployment sets and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		params, err := json.Marshal(DeploymentSetParams{Strategy: string(ds.Strategy)})
		if err != nil {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		rr.Params = params
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	if len(refs) == 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no deployment set for the profile %q", profile))
	}
	if len(refs) > 1 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the name of the deployment set matches %d sets; it must match one", len(refs)))
	}
	return refs[0], nil
}

// ZoneGaps implements vendors.ZoneChecker: the zones that cannot host a node with
// the resolved default settings. An encrypted system disk is refused in Zone D of
// Hong Kong and Zone A of Singapore, so with encrypted: true those zones are gaps
// (explicit, ADR-017: not a silent downgrade); publicIPv6 is supported nowhere.
// found holds the resources the NodeClass's configuration resolved; a zone
// without any vSwitch is not a gap here (NoVSwitchInZone handles it).
func (d *Driver) ZoneGaps(
	ctx context.Context, region string, s vendors.VendorSettings, _ map[vendors.ResourceKind][]vendors.ResourceRef,
) (map[string]string, error) {
	st, err := decodeSettings(s)
	if err != nil {
		return nil, nil // the NodeClass validation reports a settings error
	}
	wantIPv6 := boolOr(s.Networking.PublicIPv6, false)
	encrypted := rootDisk(st).Encrypted
	if !wantIPv6 && !encrypted {
		return nil, nil
	}
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return nil, err
	}
	gaps := map[string]string{}
	for _, z := range zones {
		switch {
		case wantIPv6:
			gaps[z.Name] = errIPv6Unsupported
		case encryptedRootDenied[z.Name]:
			gaps[z.Name] = fmt.Sprintf("ECS refuses an encrypted system disk in %s (storage.root.encrypted: true)", z.Name)
		}
	}
	if len(gaps) == 0 {
		return nil, nil
	}
	return gaps, nil
}

// ---- hardening ----------------------------------------------------------------------

// metadataAddress is the ECS instance metadata endpoint.
// errIPv6Unsupported is the reason of the zones publicIPv6 makes unusable.
const errIPv6Unsupported = "publicIPv6 is not supported on Alibaba Cloud yet: " +
	"it needs an IPv6 gateway the driver does not manage"

const metadataAddress = "100.100.100.200"

// Hardenings implements vendors.NodeHardener: metadata-from-pods drops Pod
// traffic forwarded to the metadata address. ECS's hardened mode (HttpTokens
// required) would also keep Pods away, but it stops Ignition's aliyun provider
// from reading the user data, so the driver keeps it optional and offers this
// instead (plan 21 section 5; off by default, enabled per NodeClass).
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	if slices.Contains(req.Hardening, vendors.HardeningMetadataFromPods) {
		f, err := vendors.BlockMetadataFromPods(metadataAddress)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
