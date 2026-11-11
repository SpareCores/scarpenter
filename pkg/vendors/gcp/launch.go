/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Metadata keys the driver owns; the NodeClass's metadata may not set them.
const (
	MetaUserData        = "user-data"
	MetaSSHKeys         = "ssh-keys"
	MetaEnableOSLogin   = "enable-oslogin"
	MetaBlockProjectSSH = "block-project-ssh-keys"
)

// ReservedMetadataKeys are the metadata keys a NodeClass may not set.
var ReservedMetadataKeys = []string{MetaUserData, MetaSSHKeys, MetaEnableOSLogin, MetaBlockProjectSSH}

// Disk is one disk of a launch.
type Disk struct {
	// Name is the volume's name: the device name of a data volume, so that the
	// guest path /dev/disk/by-id/google-<name> is known before the call.
	Name string
	// Type is the disk type (pd-balanced, hyperdisk-balanced, ...), SizeGb the
	// size in GiB (GCP's "GB").
	Type   string
	SizeGb int64
	// ProvisionedIops and ProvisionedThroughput are for Hyperdisk, 0 unset.
	ProvisionedIops       int64
	ProvisionedThroughput int64
	// KMSKeyName is the customer-managed encryption key, "" for Google's.
	KMSKeyName string
	// Interface is NVME or SCSI for a data disk ("" leaves the API's choice).
	Interface string
}

// LaunchSpec is everything the launch request depends on, already resolved:
// the driver (once it exists) fills it from the CreateRequest, the settings and
// the prepared resources. BuildInsert is pure.
// TODO(plan 14): the driver fills this from vendors.CreateRequest and the
// resolved settings (unified `use` selector, `images` kind, RootDiskForType).
type LaunchSpec struct {
	Project string
	Zone    string
	// Name is the instance name (InstanceName of the NodeClaim).
	Name        string
	MachineType string
	// Spot selects the Spot provisioning model; false is on-demand.
	Spot bool
	// OnHostMaintenance is MIGRATE or TERMINATE for on-demand (by series); Spot
	// is always TERMINATE.
	OnHostMaintenance string
	// Image is the resolved image URL (projects/<p>/global/images/<n>).
	Image string
	// Labels are the labels of the instance and of its disks (Labels()).
	Labels map[string]string
	// UserData is the rendered Ignition.
	UserData []byte
	// ExtraMetadata is the block's metadata (never a reserved key).
	ExtraMetadata map[string]string
	Root          Disk
	Data          []Disk
	// Subnetwork is the subnet URL (SubnetworkURL).
	Subnetwork string
	// NetworkTags are the firewall's network tag and the run tag.
	NetworkTags []string
	PublicIPv4  bool
	PublicIPv6  bool
	// NicType is GVNIC or VIRTIO_NET, "" leaves the series' default; Tier1
	// selects the TIER_1 egress bandwidth.
	NicType string
	Tier1   bool
	// ServiceAccount is attached when set (needs iam.serviceAccounts.actAs).
	ServiceAccount string
	// ResourcePolicies are the placement policy URLs.
	ResourcePolicies []string
	// IdempotencyKey is the NodeClaim UID; the label scarpenter-idempotency-key
	// carries it and the request ID derives from it.
	IdempotencyKey string
	// CapacityType is the logical capacity type, for the request ID.
	CapacityType string
}

// BuildInsert builds the instances.insert request: every field of the plan's
// launch table. It does not call the API.
func BuildInsert(s LaunchSpec) (*computepb.InsertInstanceRequest, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	labels := maps.Clone(s.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if s.IdempotencyKey != "" {
		labels[TagKey(vendors.TagIdempotencyKey)] = LabelValue(s.IdempotencyKey)
	}
	disks, err := s.disks(labels)
	if err != nil {
		return nil, err
	}
	inst := &computepb.Instance{
		Name:              new(s.Name),
		MachineType:       new(MachineTypeURL(s.Zone, s.MachineType)),
		Disks:             disks,
		NetworkInterfaces: []*computepb.NetworkInterface{s.networkInterface()},
		Labels:            labels,
		Scheduling:        s.scheduling(),
		Metadata:          &computepb.Metadata{Items: s.metadata()},
		ResourcePolicies:  slices.Clone(s.ResourcePolicies),
	}
	if len(s.NetworkTags) > 0 {
		inst.Tags = &computepb.Tags{Items: slices.Clone(s.NetworkTags)}
	}
	if s.Tier1 {
		inst.NetworkPerformanceConfig = &computepb.NetworkPerformanceConfig{TotalEgressBandwidthTier: new("TIER_1")}
	}
	if s.ServiceAccount != "" {
		inst.ServiceAccounts = []*computepb.ServiceAccount{{
			Email: new(s.ServiceAccount), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		}}
	}
	req := &computepb.InsertInstanceRequest{Project: s.Project, Zone: s.Zone, InstanceResource: inst}
	// Without a key there is no request ID: one derived from the offering alone
	// would be shared by every keyless launch of the same offering, and the API
	// would answer the second with the first one's operation.
	if s.IdempotencyKey != "" {
		req.RequestId = new(RequestID(s.IdempotencyKey, s.Zone, s.MachineType, s.CapacityType))
	}
	return req, nil
}

func (s LaunchSpec) validate() error {
	if s.Project == "" || s.Zone == "" || s.Name == "" || s.MachineType == "" || s.Image == "" || s.Subnetwork == "" {
		return errors.New("launch: project, zone, name, machine type, image and subnetwork are required")
	}
	if s.Root.Type == "" || s.Root.SizeGb <= 0 {
		return errors.New("launch: the root disk needs a type and a size")
	}
	for k := range s.ExtraMetadata {
		if slices.Contains(ReservedMetadataKeys, k) {
			return fmt.Errorf("launch: metadata key %q is reserved", k)
		}
	}
	if s.NicType != "" && s.NicType != "GVNIC" && s.NicType != "VIRTIO_NET" {
		return fmt.Errorf("launch: unknown NIC type %q", s.NicType)
	}
	return nil
}

// disks builds the boot disk and the data volumes.
func (s LaunchSpec) disks(labels map[string]string) ([]*computepb.AttachedDisk, error) {
	disks := []*computepb.AttachedDisk{{
		Boot: new(true), AutoDelete: new(true), Type: new("PERSISTENT"),
		DeviceName:        new(s.Name),
		InitializeParams:  initParams(s.Root, s.Zone, s.Name, labels, &s.Image),
		DiskEncryptionKey: kms(s.Root),
	}}
	for _, d := range s.Data {
		if d.Name == "" || d.Type == "" || d.SizeGb <= 0 {
			return nil, fmt.Errorf("launch: data volume %q needs a name, a type and a size", d.Name)
		}
		ad := &computepb.AttachedDisk{
			// autoDelete is not the default for an attached disk: without it
			// the data volumes outlive the instance.
			AutoDelete: new(true), Type: new("PERSISTENT"),
			DeviceName:        new(d.Name),
			InitializeParams:  initParams(d, s.Zone, s.Name+"-"+d.Name, labels, nil),
			DiskEncryptionKey: kms(d),
		}
		if d.Interface != "" {
			ad.Interface = new(d.Interface)
		}
		disks = append(disks, ad)
	}
	return disks, nil
}

func (s LaunchSpec) networkInterface() *computepb.NetworkInterface {
	nic := &computepb.NetworkInterface{Subnetwork: new(s.Subnetwork)}
	if s.NicType != "" {
		nic.NicType = new(s.NicType)
	}
	if s.PublicIPv4 {
		nic.AccessConfigs = []*computepb.AccessConfig{{Name: new("External NAT"), Type: new("ONE_TO_ONE_NAT")}}
	}
	if s.PublicIPv6 {
		nic.StackType = new("IPV4_IPV6")
		nic.Ipv6AccessConfigs = []*computepb.AccessConfig{{Name: new("External IPv6"), Type: new("DIRECT_IPV6")}}
	} else {
		nic.StackType = new("IPV4_ONLY")
	}
	return nic
}

func (s LaunchSpec) scheduling() *computepb.Scheduling {
	sched := &computepb.Scheduling{AutomaticRestart: new(false)}
	if s.Spot {
		sched.ProvisioningModel = new("SPOT")
		// The default action is STOP, which leaves a stopped instance and its
		// disks behind.
		sched.InstanceTerminationAction = new("DELETE")
		sched.OnHostMaintenance = new("TERMINATE")
		return sched
	}
	sched.ProvisioningModel = new("STANDARD")
	sched.OnHostMaintenance = new(cmpDefault(s.OnHostMaintenance, "MIGRATE"))
	return sched
}

// metadata builds the metadata items, sorted by key.
func (s LaunchSpec) metadata() []*computepb.Items {
	meta := map[string]string{
		MetaUserData:        string(s.UserData),
		MetaBlockProjectSSH: "TRUE",
		MetaEnableOSLogin:   "FALSE",
	}
	maps.Copy(meta, s.ExtraMetadata)
	keys := slices.Sorted(maps.Keys(meta))
	items := make([]*computepb.Items, 0, len(keys))
	for _, k := range keys {
		items = append(items, &computepb.Items{Key: new(k), Value: new(meta[k])})
	}
	return items
}

func cmpDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func kms(d Disk) *computepb.CustomerEncryptionKey {
	if d.KMSKeyName == "" {
		return nil
	}
	return &computepb.CustomerEncryptionKey{KmsKeyName: new(d.KMSKeyName)}
}

func initParams(
	d Disk, zone, diskName string, labels map[string]string, image *string,
) *computepb.AttachedDiskInitializeParams {
	p := &computepb.AttachedDiskInitializeParams{
		DiskName: new(diskName), DiskType: new(DiskTypeURL(zone, d.Type)),
		DiskSizeGb: new(d.SizeGb), Labels: maps.Clone(labels),
	}
	if image != nil {
		p.SourceImage = image
	}
	if d.ProvisionedIops > 0 {
		p.ProvisionedIops = new(d.ProvisionedIops)
	}
	if d.ProvisionedThroughput > 0 {
		p.ProvisionedThroughput = new(d.ProvisionedThroughput)
	}
	return p
}

// hyperdiskOnlySeries boot only from Hyperdisk. UNVERIFIED: the list comes from
// the plan (vendor-capabilities); the spike and a test against Keeper's types
// keep it honest.
var hyperdiskOnlySeries = []string{"c4", "n4", "c4a", "n4a", "a3", "a4", "a4x"}

// DefaultRootDiskType is the one type-dependent default of the root disk
// (plan 16, section 6): hyperdisk-balanced for the series that boot only from
// Hyperdisk, pd-balanced for the rest.
func DefaultRootDiskType(machineType string) string {
	if slices.Contains(hyperdiskOnlySeries, Family(machineType)) {
		return "hyperdisk-balanced"
	}
	return "pd-balanced"
}

// DataDevicePath is the guest path of a data volume (the device name is chosen
// at launch): the udev rules of google-guest-configs create it. NVMe naming is
// UNVERIFIED (spike).
func DataDevicePath(name string) string { return "/dev/disk/by-id/google-" + name }
