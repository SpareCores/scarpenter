/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"fmt"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Template is the resolved boot image of a zone: a private custom template made
// by the operator (rung 3 of ADR-021), found by labels or ID.
// TODO(plan 14): filled from the images kind (ImageRequest.Use) and
// ReportImages; the driver's own resolution lists the zone's private templates by
// the scarpenter-image=flatcar label convention and requires exactly one match
// that is online.
type Template struct {
	UUID   string
	Tier   string
	SizeGB int
}

// Root is the root disk of a node.
type Root struct {
	Tier      string
	SizeGB    int
	Encrypted bool
}

// DataVolume is a data volume, created in the server call and deleted with it.
type DataVolume struct {
	Name      string
	Tier      string
	SizeGB    int
	Encrypted bool
}

// Networking is what the node's interfaces are.
type Networking struct {
	PublicIPv4 bool
	PublicIPv6 bool
	// PrivateNetwork is the SDN network UUID of the optional private interface;
	// empty without one. The utility interface is always there.
	PrivateNetwork string
}

// LaunchSpec is everything the create request needs, already resolved: the
// part of the driver that turns a vendors.CreateRequest and the NodeClass's
// settings into it is TODO(plan 14) (the unified use selector, RootDiskForType,
// the images kind); this layer is pure.
type LaunchSpec struct {
	Zone string
	Plan Plan
	// Name is the NodeClaim name: hostname and title of the server.
	Name       string
	Template   Template
	Root       Root
	Data       []DataVolume
	Networking Networking
	// ServerGroup is the group UUID when the placement profile asks.
	ServerGroup string
	// UserData is the Ignition config, sent as the raw JSON body (not a URL).
	UserData []byte
	// Labels come from ServerLabels.
	Labels []upcloud.Label
}

// Validate checks the spec against the API's rules and the driver's, with the
// class a failed check has: Unsupported where the combination cannot be
// launched, Fatal for our own mistakes.
func (s LaunchSpec) Validate() error {
	if s.Zone == "" {
		return vendors.Errorf(vendors.Fatal, "no zone")
	}
	if err := ValidateName(s.Name); err != nil {
		return vendors.NewError(vendors.Fatal, "InvalidName", err)
	}
	if !s.Plan.Supported() {
		return vendors.Errorf(vendors.Unsupported, "plan %q (%s family) is not supported", s.Plan.Name, s.Plan.Family)
	}
	if s.Template.UUID == "" {
		return vendors.Errorf(vendors.Fatal, "no template resolved for zone %s", s.Zone)
	}
	if len(s.UserData) == 0 {
		return vendors.Errorf(vendors.Fatal, "no user-data: the node would boot unconfigured")
	}
	if err := s.validateDisks(); err != nil {
		return err
	}
	return s.validateNetworking()
}

func (s LaunchSpec) validateDisks() error {
	// Deployments from a custom template keep the template's tier.
	if s.Root.Tier != s.Template.Tier {
		return vendors.NewError(vendors.Unsupported, "TierMismatch", fmt.Errorf(
			"root tier %q differs from the tier %q of template %s", s.Root.Tier, s.Template.Tier, s.Template.UUID))
	}
	if s.Root.SizeGB < s.Template.SizeGB || s.Root.SizeGB > MaxStorageGB {
		return vendors.NewError(vendors.Unsupported, "RootSize", fmt.Errorf(
			"root size %d GB must be %d to %d GB", s.Root.SizeGB, s.Template.SizeGB, MaxStorageGB))
	}
	if 1+len(s.Data) > MaxStorageDevices {
		return vendors.NewError(vendors.Unsupported, "TooManyDevices", fmt.Errorf(
			"%d storage devices exceed the driver's limit of %d", 1+len(s.Data), MaxStorageDevices))
	}
	for _, d := range s.Data {
		if d.Name == "" || d.SizeGB < MinStorageGB || d.SizeGB > MaxStorageGB {
			return vendors.NewError(vendors.Unsupported, "DataVolume", fmt.Errorf(
				"data volume %q: size %d GB must be %d to %d GB", d.Name, d.SizeGB, MinStorageGB, MaxStorageGB))
		}
		switch d.Tier {
		case upcloud.StorageTierMaxIOPS, upcloud.StorageTierStandard, upcloud.StorageTierHDD:
		default:
			return vendors.NewError(vendors.Unsupported, "DataVolume", fmt.Errorf(
				"data volume %q: unknown tier %q", d.Name, d.Tier))
		}
	}
	return nil
}

func (s LaunchSpec) validateNetworking() error {
	n := s.Networking
	if n.PublicIPv6 && !n.PublicIPv4 && n.PrivateNetwork == "" {
		return nil
	}
	if !n.PublicIPv4 && !n.PublicIPv6 && n.PrivateNetwork == "" {
		return vendors.NewError(vendors.Unsupported, "NoRoute", fmt.Errorf(
			"publicIPv4: false needs a private network with a gateway: the utility network has no internet access"))
	}
	return nil
}

// BuildCreateServer builds the create call (plan 19, "CreateInstance"). It is
// pure: tests compare its JSON to golden files.
func BuildCreateServer(s LaunchSpec) (*request.CreateServerRequest, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	labels := upcloud.LabelSlice(s.Labels)
	r := &request.CreateServerRequest{
		Zone:     s.Zone,
		Plan:     s.Plan.Name,
		Hostname: s.Name,
		Title:    s.Name,
		// metadata is what Ignition reads the user-data from, and is off by default.
		Metadata: upcloud.True,
		Firewall: "on",
		UserData: string(s.UserData),
		// Ignition installs the keys: no one-time password is created or mailed.
		LoginUser:           &request.LoginUser{CreatePassword: "no"},
		PasswordDelivery:    request.PasswordDeliveryNone,
		TimeZone:            "UTC",
		SimpleBackup:        "no",
		RemoteAccessEnabled: upcloud.False,
		Labels:              &labels,
		ServerGroup:         s.ServerGroup,
		Networking:          &request.CreateServerNetworking{Interfaces: s.interfaces()},
		StorageDevices:      s.devices(),
	}
	return r, nil
}

func (s LaunchSpec) devices() request.CreateServerStorageDeviceSlice {
	// The root is a clone of the template, in the template's tier, at the
	// resolved size; the data volumes are created in the same call. The create
	// device has no labels field in the SDK: LabelStorages sets them after.
	devs := make(request.CreateServerStorageDeviceSlice, 0, 1+len(s.Data))
	devs = append(devs, request.CreateServerStorageDevice{
		Action:    request.CreateServerStorageDeviceActionClone,
		Storage:   s.Template.UUID,
		Title:     s.Name + "-root",
		Size:      s.Root.SizeGB,
		Tier:      s.Root.Tier,
		Encrypted: upcloud.FromBool(s.Root.Encrypted),
		Type:      upcloud.StorageTypeDisk,
	})
	for _, d := range s.Data {
		devs = append(devs, request.CreateServerStorageDevice{
			Action:    request.CreateServerStorageDeviceActionCreate,
			Title:     s.Name + "-" + d.Name,
			Size:      d.SizeGB,
			Tier:      d.Tier,
			Encrypted: upcloud.FromBool(d.Encrypted),
			Type:      upcloud.StorageTypeDisk,
		})
	}
	return devs
}

func (s LaunchSpec) interfaces() request.CreateServerInterfaceSlice {
	n := s.Networking
	var out request.CreateServerInterfaceSlice
	if n.PublicIPv4 || n.PublicIPv6 {
		var ips request.CreateServerIPAddressSlice
		if n.PublicIPv4 {
			ips = append(ips, request.CreateServerIPAddress{Family: upcloud.IPAddressFamilyIPv4})
		}
		if n.PublicIPv6 {
			ips = append(ips, request.CreateServerIPAddress{Family: upcloud.IPAddressFamilyIPv6})
		}
		out = append(out, request.CreateServerInterface{Type: upcloud.NetworkTypePublic, IPAddresses: ips})
	}
	out = append(out, request.CreateServerInterface{
		Type:        upcloud.NetworkTypeUtility,
		IPAddresses: request.CreateServerIPAddressSlice{{Family: upcloud.IPAddressFamilyIPv4}},
	})
	if n.PrivateNetwork != "" {
		out = append(out, request.CreateServerInterface{
			Type:        upcloud.NetworkTypePrivate,
			Network:     n.PrivateNetwork,
			IPAddresses: request.CreateServerIPAddressSlice{{Family: upcloud.IPAddressFamilyIPv4}},
		})
	}
	return out
}
