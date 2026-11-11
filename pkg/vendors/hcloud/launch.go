/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"fmt"
	"net"
	"strconv"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MaxUserData is the API's limit of the user-data field: 32 KiB. The bootstrap
// renderer's 16 KiB budget (overview section 8.2) is stricter and holds.
const MaxUserData = 32 * 1024

// MaxFirewallRules is the API's limit of rules per firewall.
const MaxFirewallRules = 500

// LaunchSpec is one POST /servers, resolved: everything is a name or an ID
// already (Prepare resolved the resources, the image lookup the snapshot).
// TODO(plan 14): the Driver builds it from vendors.CreateRequest and its
// Prepared resources.
type LaunchSpec struct {
	// Name is the NodeClaim name (also the node name).
	Name string
	// ServerType is the API reference (cpx22), Location the zone name.
	ServerType, Location string
	// ImageID is the resolved snapshot.
	ImageID  int64
	UserData []byte
	// Tags are the logical tags; ExtraTags the NodeClass's own.
	Tags      map[vendors.TagKey]string
	ExtraTags map[string]string
	// IdempotencyKey is the NodeClaim UID, written as the idempotency-key label.
	IdempotencyKey string
	// PublicIPv4 and PublicIPv6 map to public_net; at least one is required.
	PublicIPv4, PublicIPv6 bool
	// The IDs of the resolved or created resources; zero means none.
	NetworkID, FirewallID, PlacementGroupID int64
	// VolumeIDs are the pre-created volumes (attached at create, not mounted by
	// Hetzner: automount is false).
	VolumeIDs []int64
	// SSHKeyID is the resolved key, 0 for none.
	SSHKeyID int64
}

// BuildCreateOpts builds the create request (plan 15, "CreateInstance" table). It
// is pure: the same spec always gives the same request.
func BuildCreateOpts(s LaunchSpec) (hc.ServerCreateOpts, error) {
	if len(s.UserData) > MaxUserData {
		return hc.ServerCreateOpts{}, vendors.NewError(vendors.Fatal, CodeUserDataTooLarge, fmt.Errorf(
			"the user-data is %d bytes; Hetzner accepts at most %d", len(s.UserData), MaxUserData))
	}
	if !s.PublicIPv4 && !s.PublicIPv6 {
		return hc.ServerCreateOpts{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"a server needs a public IPv4 or IPv6 address: private-only servers are not supported"))
	}
	if s.Name == "" || s.ServerType == "" || s.Location == "" || s.ImageID == 0 {
		return hc.ServerCreateOpts{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"a launch needs a name, a server type, a location and an image"))
	}
	labels, err := Labels(vendors.IdempotencyTag(s.Tags, s.IdempotencyKey), s.ExtraTags)
	if err != nil {
		return hc.ServerCreateOpts{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	labels[LabelCapacityType] = vendors.CapacityOnDemand

	start, automount := true, false
	o := hc.ServerCreateOpts{
		Name:             ServerName(s.Name),
		ServerType:       &hc.ServerType{Name: s.ServerType},
		Image:            &hc.Image{ID: s.ImageID},
		Location:         &hc.Location{Name: s.Location},
		UserData:         string(s.UserData),
		StartAfterCreate: &start,
		Labels:           labels,
		PublicNet:        &hc.ServerCreatePublicNet{EnableIPv4: s.PublicIPv4, EnableIPv6: s.PublicIPv6},
	}
	if s.NetworkID != 0 {
		o.Networks = []*hc.Network{{ID: s.NetworkID}}
	}
	if s.FirewallID != 0 {
		o.Firewalls = []*hc.ServerCreateFirewall{{Firewall: hc.Firewall{ID: s.FirewallID}}}
	}
	if s.PlacementGroupID != 0 {
		o.PlacementGroup = &hc.PlacementGroup{ID: s.PlacementGroupID}
	}
	if len(s.VolumeIDs) > 0 {
		o.Automount = &automount
		for _, id := range s.VolumeIDs {
			o.Volumes = append(o.Volumes, &hc.Volume{ID: id})
		}
	}
	if s.SSHKeyID != 0 {
		o.SSHKeys = []*hc.SSHKey{{ID: s.SSHKeyID}}
	}
	return o, nil
}

// VolumeSpec is one storage.data entry as a volume create.
type VolumeSpec struct {
	// Name is the engine's deterministic name (unique per project).
	Name     string
	Location string
	// SizeGB is Hetzner's unit, 10 to 10240; SizeBytes converts Gi sizes.
	SizeGB    int
	Tags      map[vendors.TagKey]string
	ExtraTags map[string]string
}

// VolumeSizeGB converts a size in bytes (a Gi quantity of the NodeClass) to
// Hetzner's decimal GB, rounding up so the volume is never smaller than asked.
func VolumeSizeGB(bytes int64) int {
	const gb = 1_000_000_000
	return int((bytes + gb - 1) / gb)
}

// Volume size limits of the API, in GB.
const (
	MinVolumeGB = 10
	MaxVolumeGB = 10240
)

// BuildVolumeOpts builds a volume create: no format (the first-boot unit
// formats and labels it), no server (attached by the server create).
func BuildVolumeOpts(v VolumeSpec) (hc.VolumeCreateOpts, error) {
	if v.SizeGB < MinVolumeGB || v.SizeGB > MaxVolumeGB {
		return hc.VolumeCreateOpts{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"a volume is %d to %d GB, not %d", MinVolumeGB, MaxVolumeGB, v.SizeGB))
	}
	labels, err := Labels(v.Tags, v.ExtraTags)
	if err != nil {
		return hc.VolumeCreateOpts{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	automount := false
	return hc.VolumeCreateOpts{
		Name:      ResourceName(v.Name, MaxVolumeName),
		Size:      v.SizeGB,
		Location:  &hc.Location{Name: v.Location},
		Labels:    labels,
		Automount: &automount,
	}, nil
}

// FirewallRules turns the generic inbound rules into firewall rules: inbound
// only (outbound stays open), one rule per rule and protocol; the generic
// protocol "any" becomes one rule per protocol of tcp, udp and icmp. No rules
// give none: an attached firewall then denies all inbound traffic.
func FirewallRules(rules []common.InboundRule) ([]hc.FirewallRule, error) {
	var out []hc.FirewallRule
	for i, r := range rules {
		var sources []net.IPNet
		for _, c := range r.CIDRs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("inbound rule %d: %w", i, err)
			}
			sources = append(sources, *n)
		}
		protos := []hc.FirewallRuleProtocol{hc.FirewallRuleProtocol(r.Protocol)}
		if r.Protocol == common.Protocol("any") {
			protos = []hc.FirewallRuleProtocol{
				hc.FirewallRuleProtocolTCP, hc.FirewallRuleProtocolUDP, hc.FirewallRuleProtocolICMP,
			}
		}
		for _, p := range protos {
			fr := hc.FirewallRule{Direction: hc.FirewallRuleDirectionIn, SourceIPs: sources, Protocol: p}
			if r.FromPort != nil && (p == hc.FirewallRuleProtocolTCP || p == hc.FirewallRuleProtocolUDP) {
				port := strconv.Itoa(int(*r.FromPort))
				if r.ToPort != nil && *r.ToPort != *r.FromPort {
					port += "-" + strconv.Itoa(int(*r.ToPort))
				}
				fr.Port = &port
			}
			out = append(out, fr)
		}
	}
	if len(out) > MaxFirewallRules {
		return nil, fmt.Errorf("the inbound rules make %d firewall rules; Hetzner allows %d", len(out), MaxFirewallRules)
	}
	return out, nil
}
