/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"golang.org/x/crypto/ssh"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Network is the network the VM attaches to, resolved by Prepare from the
// managed or brought resources.
type Network struct {
	// ResourceGroup holds the VM, its NIC and its public IP.
	ResourceGroup string
	// SubnetID and NSGID are ARM IDs; the NIC takes one NSG.
	SubnetID string
	NSGID    string
	// ASGIDs are the application security groups the NIC joins (isolation):
	// the one of a managed network security group.
	ASGIDs []string
	// PPGID is the proximity placement group of a placement profile.
	PPGID string
}

// NetworkOf reads the network of a launch from the resources Prepare resolved,
// by kind. The ID of a resource group is its name, the other kinds' IDs are ARM
// IDs.
func NetworkOf(p *vendors.Prepared) Network {
	var n Network
	if p == nil {
		return n
	}
	for _, r := range p.Resources {
		switch r.Kind {
		case KindResourceGroups:
			n.ResourceGroup = r.ID
		case KindSubnets:
			n.SubnetID = r.ID
		case KindSecurityGroups, KindRunSecurityGroups:
			n.NSGID = r.ID
		case KindAppSecGroups:
			n.ASGIDs = append(n.ASGIDs, r.ID)
		case KindPlacement:
			n.PPGID = r.ID
		}
	}
	return n
}

// ImagePlan is the purchase plan of a marketplace image, nil for an image
// without one.
type ImagePlan struct{ Name, Product, Publisher string }

// LaunchSpec is everything the VM, NIC and public IP bodies are built from.
type LaunchSpec struct {
	Cluster   string
	NodeClaim string
	Region    string
	Zone      string
	Size      string
	Spot      bool
	UserData  []byte
	Image     ImageRef
	Plan      *ImagePlan
	Settings  Settings
	// PublicIPv4 gives the node a Standard public IPv4 address.
	PublicIPv4 bool
	Network    Network
	// SKU is the size's table row; nil when the table has none (the
	// capability checks are then left to the API).
	SKU *SKU
	// SSHPublicKey is the authorized_keys line of the VM's one key.
	SSHPublicKey string
	// Tags are the Azure tags (see Tags).
	Tags map[string]string
}

// NewLaunchSpec builds the spec of a launch from the contract's request, and
// checks what can be checked before the call: the user-data size, the tags and
// the settings against the size's capabilities (Unsupported where the size
// cannot have them).
func NewLaunchSpec(
	req vendors.CreateRequest, cluster string, img ImageRef, plan *ImagePlan, sku *SKU, sshKey string,
) (LaunchSpec, error) {
	st, err := DecodeSettings(req.Settings)
	if err != nil {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	tags, err := Tags(req.Tags, req.ExtraTags)
	if err != nil {
		return LaunchSpec{}, err
	}
	if len(req.UserData) > maxUserData {
		return LaunchSpec{}, vendors.NewError(vendors.Fatal, codeUserDataTooLarge,
			fmt.Errorf("user-data is %d bytes, the limit is %d", len(req.UserData), maxUserData))
	}
	key := st.AdminSSHPublicKey
	if key == "" {
		key = sshKey
	}
	l := LaunchSpec{
		Cluster: cluster, NodeClaim: req.Node.Name, Region: req.Region, Zone: req.Zone, Size: req.InstanceType,
		Spot: req.CapacityType == vendors.CapacitySpot, UserData: req.UserData, Image: img, Plan: plan,
		Settings: st, PublicIPv4: boolOr(req.Settings.Networking.PublicIPv4, true),
		Network: NetworkOf(req.Prepared), SKU: sku, SSHPublicKey: key, Tags: tags,
	}
	if l.NodeClaim == "" {
		l.NodeClaim = req.Tags[vendors.TagNodeClaim]
	}
	if l.NodeClaim == "" {
		return LaunchSpec{}, invalidf("the launch has no NodeClaim name")
	}
	if err := l.validate(); err != nil {
		return LaunchSpec{}, err
	}
	return l, nil
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// validate checks the settings against the size's capabilities.
func (l LaunchSpec) validate() error {
	unsupported := func(format string, a ...any) error {
		return vendors.NewError(vendors.Unsupported, codeInvalidSettings, fmt.Errorf(format, a...))
	}
	root := l.Settings.Root()
	if !validStorageType(root.StorageAccountType) {
		return invalidf("root storageAccountType %q is unknown", root.StorageAccountType)
	}
	if l.Network.ResourceGroup == "" || l.Network.SubnetID == "" {
		return invalidf("the launch has no resource group or subnet (Prepare did not resolve them)")
	}
	disks := l.Settings.DataDisks()
	for _, d := range disks {
		if !validStorageType(d.StorageAccountType) {
			return invalidf("data disk %q: storageAccountType %q is unknown", d.Name, d.StorageAccountType)
		}
		if d.Name == "" || d.DiskSizeGB <= 0 {
			return invalidf("data disk %q needs a name and a size", d.Name)
		}
	}
	if l.SKU == nil {
		return nil
	}
	if IsPremium(root.StorageAccountType) && !l.SKU.PremiumIO {
		return unsupported("%s does not support premium storage (root %s)", l.Size, root.StorageAccountType)
	}
	for _, d := range disks {
		if IsPremium(d.StorageAccountType) && !l.SKU.PremiumIO {
			return unsupported("%s does not support premium storage (disk %s)", l.Size, d.Name)
		}
	}
	if l.SKU.MaxDataDisks > 0 && len(disks) > l.SKU.MaxDataDisks {
		return unsupported("%s takes at most %d data disks, %d asked", l.Size, l.SKU.MaxDataDisks, len(disks))
	}
	if root.Ephemeral != nil && !l.SKU.EphemeralOSDisk {
		return unsupported("%s does not support ephemeral OS disks", l.Size)
	}
	if l.Spot && !l.SKU.LowPriorityCapable {
		return vendors.NewError(vendors.Unsupported, "AzureSpotIsNotSupportedForThisVMSize",
			fmt.Errorf("%s cannot be launched as Spot", l.Size))
	}
	if l.Settings.AcceleratedNetwork == settingTrue && !l.SKU.AcceleratedNetwork {
		return unsupported("%s does not support accelerated networking", l.Size)
	}
	return nil
}

// VMName is the VM's deterministic name.
func (l LaunchSpec) VMName() string { return VMName(l.Cluster, l.NodeClaim) }

// AcceleratedNetworking resolves the setting: auto is on where the size has the
// capability. The result is part of the NIC name, so a different value is a
// different NIC.
func (l LaunchSpec) AcceleratedNetworking() bool {
	switch l.Settings.AcceleratedNetwork {
	case settingTrue:
		return true
	case "false":
		return false
	}
	return l.SKU != nil && l.SKU.AcceleratedNetwork
}

// NICName carries the hash of the NIC's create parameters.
func (l LaunchSpec) NICName() string {
	parts := make([]string, 0, 4+len(l.Network.ASGIDs))
	parts = append(parts, l.Network.SubnetID, l.Network.NSGID,
		strconv.FormatBool(l.AcceleratedNetworking()), strconv.FormatBool(l.PublicIPv4))
	p := strings.Join(append(parts, l.Network.ASGIDs...), "|")
	return l.VMName() + "-nic-" + hash8(p)
}

// PIPName is the public IP's name.
func (l LaunchSpec) PIPName() string { return l.VMName() + "-pip" }

// PIP is the Standard static public IPv4 address, deleted with the VM. It has
// no zones field (zone-redundant in a region with zones, which a VM of any zone
// can use): its name does not carry the zone, and the zones of a public IP
// cannot change, so a zonal address would make the attempt in the next zone
// fail on the PUT of the address the previous zone left.
func (l LaunchSpec) PIP() armnetwork.PublicIPAddress {
	return armnetwork.PublicIPAddress{
		Location: new(l.Region),
		Tags:     tagPtrs(l.Tags),
		SKU:      &armnetwork.PublicIPAddressSKU{Name: new(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: new(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   new(armnetwork.IPVersionIPv4),
			DeleteOption:             new(armnetwork.DeleteOptionsDelete),
		},
	}
}

// NIC is the VM's network interface; pipID is "" without a public IP.
func (l LaunchSpec) NIC(pipID string) armnetwork.Interface {
	ipc := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Primary:                   new(true),
		PrivateIPAllocationMethod: new(armnetwork.IPAllocationMethodDynamic),
		Subnet:                    &armnetwork.Subnet{ID: new(l.Network.SubnetID)},
	}
	if pipID != "" {
		ipc.PublicIPAddress = &armnetwork.PublicIPAddress{ID: new(pipID)}
	}
	for _, id := range l.Network.ASGIDs {
		ipc.ApplicationSecurityGroups = append(ipc.ApplicationSecurityGroups,
			&armnetwork.ApplicationSecurityGroup{ID: new(id)})
	}
	props := &armnetwork.InterfacePropertiesFormat{
		EnableAcceleratedNetworking: new(l.AcceleratedNetworking()),
		IPConfigurations:            []*armnetwork.InterfaceIPConfiguration{{Name: new("ipconfig1"), Properties: ipc}},
	}
	if l.Network.NSGID != "" {
		props.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: new(l.Network.NSGID)}
	}
	return armnetwork.Interface{Location: new(l.Region), Tags: tagPtrs(l.Tags), Properties: props}
}

// DeviceHints returns the guest device path of every data disk: the LUN is the
// disk's index, so the user-data can be rendered before the VM exists
// (/dev/disk/azure/scsi1/lun<N>; the udev rules on Flatcar are a spike item).
func (l LaunchSpec) DeviceHints() []vendors.DeviceHint { return deviceHints(l.Settings) }

// deviceHints are the hints of the data disks of the settings.
func deviceHints(st Settings) []vendors.DeviceHint {
	disks := st.DataDisks()
	out := make([]vendors.DeviceHint, 0, len(disks))
	for i, d := range disks {
		fs := d.Filesystem
		if fs == "" {
			fs = "ext4"
		}
		out = append(out, vendors.DeviceHint{
			Name: d.Name, Filesystem: fs, MountPath: d.MountPath,
			ByID: fmt.Sprintf("/dev/disk/azure/scsi1/lun%d", i), SizeBytes: int64(d.DiskSizeGB) << 30,
		})
	}
	return out
}

// VM is the virtual machine body (plan 17 section 3, every field of the table).
func (l LaunchSpec) VM(nicID string) armcompute.VirtualMachine {
	name := l.VMName()
	root := l.Settings.Root()
	osDisk := &armcompute.OSDisk{
		Name:         new(name + "-os"),
		CreateOption: new(armcompute.DiskCreateOptionTypesFromImage),
		DiskSizeGB:   new(root.DiskSizeGB),
		Caching:      new(armcompute.CachingTypes(root.Caching)),
		DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
		ManagedDisk: &armcompute.ManagedDiskParameters{
			StorageAccountType: new(armcompute.StorageAccountTypes(root.StorageAccountType)),
		},
	}
	if root.DiskEncryptionSetID != "" {
		osDisk.ManagedDisk.DiskEncryptionSet = &armcompute.DiskEncryptionSetParameters{
			ID: new(root.DiskEncryptionSetID),
		}
	}
	if eph := root.Ephemeral; eph != nil {
		// An ephemeral OS disk has no storage account type and is read-only
		// cached.
		osDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{
			Option:    new(armcompute.DiffDiskOptionsLocal),
			Placement: new(armcompute.DiffDiskPlacement(eph.Placement)),
		}
		osDisk.Caching = new(armcompute.CachingTypesReadOnly)
		osDisk.ManagedDisk.StorageAccountType = nil
	}
	disks := l.Settings.DataDisks()
	data := make([]*armcompute.DataDisk, 0, len(disks))
	for i, d := range disks {
		data = append(data, &armcompute.DataDisk{
			Lun:          new(int32(i)),
			Name:         new(name + "-" + d.Name),
			CreateOption: new(armcompute.DiskCreateOptionTypesEmpty),
			DiskSizeGB:   new(d.DiskSizeGB),
			Caching:      new(armcompute.CachingTypes(d.Caching)),
			DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
			ManagedDisk: &armcompute.ManagedDiskParameters{
				StorageAccountType: new(armcompute.StorageAccountTypes(d.StorageAccountType)),
			},
		})
	}
	props := &armcompute.VirtualMachineProperties{
		HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes(l.Size))},
		OSProfile: &armcompute.OSProfile{
			ComputerName:  new(name),
			AdminUsername: new(AdminUser),
			CustomData:    new(base64.StdEncoding.EncodeToString(l.UserData)),
			LinuxConfiguration: &armcompute.LinuxConfiguration{
				DisablePasswordAuthentication: new(true),
				SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    new("/home/" + AdminUser + "/.ssh/authorized_keys"),
					KeyData: new(l.SSHPublicKey),
				}}},
			},
			AllowExtensionOperations: new(false),
		},
		StorageProfile: &armcompute.StorageProfile{
			ImageReference: &armcompute.ImageReference{
				Publisher: new(l.Image.Publisher), Offer: new(l.Image.Offer),
				SKU: new(l.Image.SKU), Version: new(l.Image.Version),
			},
			OSDisk:    osDisk,
			DataDisks: data,
		},
		NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
			ID: new(nicID),
			Properties: &armcompute.NetworkInterfaceReferenceProperties{
				Primary: new(true), DeleteOption: new(armcompute.DeleteOptionsDelete),
			},
		}}},
		DiagnosticsProfile: &armcompute.DiagnosticsProfile{
			BootDiagnostics: &armcompute.BootDiagnostics{Enabled: new(true)},
		},
	}
	if l.Settings.EncryptionAtHost != nil {
		props.SecurityProfile = &armcompute.SecurityProfile{EncryptionAtHost: l.Settings.EncryptionAtHost}
	}
	if l.Spot {
		props.Priority = new(armcompute.VirtualMachinePriorityTypesSpot)
		props.EvictionPolicy = new(armcompute.VirtualMachineEvictionPolicyTypesDelete)
		props.BillingProfile = &armcompute.BillingProfile{MaxPrice: new(float64(-1))}
	}
	if l.Network.PPGID != "" {
		props.ProximityPlacementGroup = &armcompute.SubResource{ID: new(l.Network.PPGID)}
	}
	vm := armcompute.VirtualMachine{
		Location:   new(l.Region),
		Zones:      zonesPtr(l.Zone),
		Tags:       tagPtrs(l.Tags),
		Properties: props,
	}
	if l.Plan != nil {
		vm.Plan = &armcompute.Plan{
			Name: new(l.Plan.Name), Product: new(l.Plan.Product), Publisher: new(l.Plan.Publisher),
		}
	}
	return vm
}

// zonesPtr is the zones field: the logical zone, omitted for zone 0.
func zonesPtr(zone string) []*string {
	if zone == "" || zone == ZoneNone {
		return nil
	}
	return []*string{new(zone)}
}

func tagPtrs(m map[string]string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = new(v)
	}
	return out
}

// GenerateDiscardedKey generates an ed25519 key and returns its public half as
// an authorized_keys line; the private half is dropped, so nobody can use the
// key (access is Ignition's business). It is the VM's key when the operator
// provides none, because the API wants a credential for a Linux VM.
func GenerateDiscardedKey() (string, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), nil
}
