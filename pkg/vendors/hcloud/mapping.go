/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Name is the Keeper vendor ID, and the registration name of the driver.
const Name = "hcloud"

// Flatcar architectures (the catalog's) and Hetzner's.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// ProviderID is scarpenter://hcloud/<location>/<nodeclaim> (ADR-007).
func ProviderID(location, nodeClaim string) string {
	return scope.ProviderID(Name, location, nodeClaim)
}

// ParseProviderID splits a provider ID of this vendor into location and
// NodeClaim name; the ID of another vendor is an error.
func ParseProviderID(id string) (location, nodeClaim string, err error) {
	vendor, location, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if vendor != Name {
		return "", "", fmt.Errorf("provider ID %q is not an %s one", id, Name)
	}
	return location, nodeClaim, nil
}

// NewRef builds the instance reference <location>/<server ID>.
func NewRef(location string, serverID int64) vendors.InstanceRef {
	return vendors.NewInstanceRef(location, strconv.FormatInt(serverID, 10))
}

// ParseRef reads a reference back; a reference that is not <location>/<number>
// is an error (the contract treats it as an instance that does not exist).
func ParseRef(ref vendors.InstanceRef) (location string, serverID int64, err error) {
	location, id := ref.Region(), ref.ID()
	if location == "" || id == "" {
		return "", 0, fmt.Errorf("instance reference %q is not <location>/<server ID>", ref)
	}
	serverID, err = strconv.ParseInt(id, 10, 64)
	if err != nil || serverID <= 0 {
		return "", 0, fmt.Errorf("instance reference %q has no numeric server ID", ref)
	}
	return location, serverID, nil
}

// CatalogName is the catalog name of an API server type: hcloud.cpx22.
func CatalogName(apiReference string) string { return Name + "." + apiReference }

// APIReference is the inverse of CatalogName; ok is false for another vendor's name.
func APIReference(catalogName string) (string, bool) {
	ref, ok := strings.CutPrefix(catalogName, Name+".")
	return ref, ok && ref != ""
}

// Arch maps Hetzner's architecture to the catalog's (amd64, arm64).
func Arch(a hc.Architecture) string {
	if a == hc.ArchitectureARM {
		return ArchARM64
	}
	return ArchAMD64
}

// ImageArch maps the catalog's architecture to the API's image and server type
// architecture; ok is false for an unknown one.
func ImageArch(arch string) (hc.Architecture, bool) {
	switch arch {
	case ArchAMD64:
		return hc.ArchitectureX86, true
	case ArchARM64:
		return hc.ArchitectureARM, true
	}
	return "", false
}

// RegionNames lists the locations as the driver's regions, in name order.
func RegionNames(locations []*hc.Location) []vendors.Region {
	out := make([]vendors.Region, 0, len(locations))
	for _, l := range locations {
		out = append(out, vendors.Region{Name: l.Name})
	}
	slices.SortFunc(out, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// LocationZone is the zone of a location: Hetzner has none, so the location is
// its own (dummy) zone, and the Keeper zone ID equals the region.
func LocationZone(location string) vendors.Zone {
	return vendors.Zone{ID: location, Name: location}
}

// LocationZones finds the location and returns its single zone; ok is false for
// a location the project does not list.
func LocationZones(locations []*hc.Location, location string) ([]vendors.Zone, bool) {
	for _, l := range locations {
		if l.Name == location {
			return []vendors.Zone{LocationZone(location)}, true
		}
	}
	return nil, false
}

// NetworkZoneOf is the network zone (eu-central, us-east, us-west,
// ap-southeast) of a location, "" when unknown.
func NetworkZoneOf(locations []*hc.Location, location string) hc.NetworkZone {
	for _, l := range locations {
		if l.Name == location {
			return l.NetworkZone
		}
	}
	return ""
}

// NetworkHasSubnetIn reports whether a network has a cloud subnet in the
// network zone (the check of Resolve for the networks kind, plan 15).
func NetworkHasSubnetIn(n *hc.Network, zone hc.NetworkZone) bool {
	return slices.ContainsFunc(n.Subnets, func(s hc.NetworkSubnet) bool {
		return s.Type == hc.NetworkSubnetTypeCloud && s.NetworkZone == zone
	})
}

// Launchable reports whether the type's entry for the location says it can be
// launched now: available, and not past the end of its deprecation.
func Launchable(t *hc.ServerType, location string, now time.Time) bool {
	for _, l := range t.Locations {
		if l.Location == nil || l.Location.Name != location {
			continue
		}
		if !l.Available {
			return false
		}
		if l.Deprecation != nil && !l.Deprecation.UnavailableAfter.IsZero() && !now.Before(l.Deprecation.UnavailableAfter) {
			return false
		}
		return true
	}
	return false
}

// LaunchableTypes maps the API reference of each type launchable in the
// location to the zone IDs (the location itself): the answer of an
// OfferingLister, from one GET /server_types.
func LaunchableTypes(types []*hc.ServerType, location string, now time.Time) map[string][]string {
	out := map[string][]string{}
	for _, t := range types {
		if Launchable(t, location, now) {
			out[t.Name] = []string{location}
		}
	}
	return out
}

// State maps a server status to an instance state (plan 15, "State mapping"):
// initializing and starting are pending; running, migrating, rebuilding and
// unknown are running; stopping, off and deleting are terminating. Off is
// terminating on purpose (the stopped-node rule, plan 14 A4): a powered-off
// server still bills, is no node, and is collected.
func State(s hc.ServerStatus) string {
	switch s {
	case hc.ServerStatusInitializing, hc.ServerStatusStarting:
		return vendors.StatePending
	case hc.ServerStatusStopping, hc.ServerStatusOff, hc.ServerStatusDeleting:
		return vendors.StateTerminating
	default:
		return vendors.StateRunning
	}
}

// Addresses lists the server's addresses: the public IPv4, the IPv6 address
// <prefix>::1 of its /64 (unverified that this is the address the guest uses),
// and the private network IPs.
func Addresses(s *hc.Server) []string {
	var out []string
	if !s.PublicNet.IPv4.IsUnspecified() {
		out = append(out, s.PublicNet.IPv4.IP.String())
	}
	if ip := ipv6Guest(s.PublicNet.IPv6); ip != nil {
		out = append(out, ip.String())
	}
	for _, pn := range s.PrivateNet {
		if pn.IP != nil {
			out = append(out, pn.IP.String())
		}
	}
	return out
}

func ipv6Guest(v6 hc.ServerPublicNetIPv6) net.IP {
	if v6.IsUnspecified() {
		return nil
	}
	base := v6.IP
	if v6.Network != nil {
		base = v6.Network.IP
	}
	ip := slices.Clone(base.To16())
	if ip == nil {
		return nil
	}
	ip[15] = 1
	return ip
}

// ToInstance converts a server. The location is the server's own; a server with
// no location (the API always sends one) falls back to the argument.
func ToInstance(s *hc.Server, fallbackLocation string) *vendors.Instance {
	loc := fallbackLocation
	if s.Location != nil {
		loc = s.Location.Name
	}
	typ := ""
	if s.ServerType != nil {
		typ = s.ServerType.Name
	}
	return &vendors.Instance{
		Ref:          NewRef(loc, s.ID),
		Region:       loc,
		Zone:         loc,
		Type:         typ,
		CapacityType: vendors.CapacityOnDemand,
		State:        State(s.Status),
		CreatedAt:    s.Created,
		Tags:         TagsOf(s.Labels),
		Addresses:    Addresses(s),
	}
}

// VolumeByIDPath is the stable device path of a volume, known before the
// server exists (00a section 11).
func VolumeByIDPath(volumeID int64) string {
	return "/dev/disk/by-id/scsi-0HC_Volume_" + strconv.FormatInt(volumeID, 10)
}
