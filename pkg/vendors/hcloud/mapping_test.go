/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"net"
	"slices"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestProviderID(t *testing.T) {
	id := ProviderID("fsn1", "nc-abc")
	if id != "scarpenter://hcloud/fsn1/nc-abc" {
		t.Fatal(id)
	}
	loc, nc, err := ParseProviderID(id)
	if err != nil || loc != "fsn1" || nc != "nc-abc" {
		t.Errorf("%q %q %v", loc, nc, err)
	}
	for _, bad := range []string{"", "aws://x/y/z", "scarpenter://aws/us-east-1/n", "scarpenter://hcloud/fsn1", "scarpenter://hcloud//n", "scarpenter://hcloud/a/b/c"} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestRef(t *testing.T) {
	ref := NewRef("nbg1", 4242)
	if ref != "nbg1/4242" {
		t.Fatal(ref)
	}
	loc, id, err := ParseRef(ref)
	if err != nil || loc != "nbg1" || id != 4242 {
		t.Errorf("%q %d %v", loc, id, err)
	}
	for _, bad := range []vendors.InstanceRef{"", "nbg1", "nbg1/", "/12", "nbg1/abc", "nbg1/0", "nbg1/-3"} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestCatalogNames(t *testing.T) {
	if CatalogName("cpx22") != "hcloud.cpx22" {
		t.Error("CatalogName")
	}
	if r, ok := APIReference("hcloud.cpx22"); !ok || r != "cpx22" {
		t.Error("APIReference")
	}
	for _, bad := range []string{"aws.m7i.large", "hcloud.", "cpx22"} {
		if _, ok := APIReference(bad); ok {
			t.Errorf("%q", bad)
		}
	}
	if Arch(hc.ArchitectureARM) != "arm64" || Arch(hc.ArchitectureX86) != "amd64" {
		t.Error("Arch")
	}
	if a, ok := ImageArch("arm64"); !ok || a != hc.ArchitectureARM {
		t.Error("ImageArch arm")
	}
	if _, ok := ImageArch("riscv"); ok {
		t.Error("ImageArch unknown")
	}
}

func TestState(t *testing.T) {
	for st, want := range map[hc.ServerStatus]string{
		hc.ServerStatusInitializing: vendors.StatePending,
		hc.ServerStatusStarting:     vendors.StatePending,
		hc.ServerStatusRunning:      vendors.StateRunning,
		hc.ServerStatusMigrating:    vendors.StateRunning,
		hc.ServerStatusRebuilding:   vendors.StateRunning,
		hc.ServerStatusUnknown:      vendors.StateRunning,
		hc.ServerStatusStopping:     vendors.StateTerminating,
		hc.ServerStatusOff:          vendors.StateTerminating,
		hc.ServerStatusDeleting:     vendors.StateTerminating,
	} {
		if got := State(st); got != want {
			t.Errorf("%s: %s, want %s", st, got, want)
		}
	}
	// A powered-off server is gone for Karpenter but is listed (plan 14 A4).
	if !ToInstance(&hc.Server{Status: hc.ServerStatusOff}, "fsn1").Gone() {
		t.Error("off must be Gone")
	}
}

func TestAddressesAndToInstance(t *testing.T) {
	_, v6net, _ := net.ParseCIDR("2a01:4f8:1:2::/64")
	s := &hc.Server{
		ID: 7, Status: hc.ServerStatusRunning, Created: time.Unix(100, 0),
		Location: &hc.Location{Name: "hel1"}, ServerType: &hc.ServerType{Name: "cx23"},
		Labels: map[string]string{"scarpenter.sparecores.com/cluster": "c"},
		PublicNet: hc.ServerPublicNet{
			IPv4: hc.ServerPublicNetIPv4{IP: net.ParseIP("203.0.113.9")},
			IPv6: hc.ServerPublicNetIPv6{IP: v6net.IP, Network: v6net},
		},
		PrivateNet: []hc.ServerPrivateNet{{IP: net.ParseIP("10.0.0.5")}},
	}
	got := ToInstance(s, "fsn1")
	if got.Ref != "hel1/7" || got.Region != "hel1" || got.Zone != "hel1" || got.Type != "cx23" ||
		got.CapacityType != vendors.CapacityOnDemand || got.State != vendors.StateRunning || !got.CreatedAt.Equal(time.Unix(100, 0)) {
		t.Errorf("%+v", got)
	}
	if want := []string{"203.0.113.9", "2a01:4f8:1:2::1", "10.0.0.5"}; !slices.Equal(got.Addresses, want) {
		t.Errorf("addresses %v, want %v", got.Addresses, want)
	}
	if got.Tags[vendors.TagCluster] != "c" {
		t.Errorf("tags %v", got.Tags)
	}
	// No public IPv4 (IPv6 only), no location: the fallback applies.
	s2 := &hc.Server{ID: 8, PublicNet: hc.ServerPublicNet{IPv6: hc.ServerPublicNetIPv6{Network: v6net, IP: v6net.IP}}}
	if g := ToInstance(s2, "ash"); g.Region != "ash" || !slices.Equal(g.Addresses, []string{"2a01:4f8:1:2::1"}) {
		t.Errorf("%+v", g)
	}
}

func TestLocationsAndZones(t *testing.T) {
	locs := []*hc.Location{
		{Name: "nbg1", NetworkZone: hc.NetworkZoneEUCentral},
		{Name: "ash", NetworkZone: hc.NetworkZoneUSEast},
		{Name: "fsn1", NetworkZone: hc.NetworkZoneEUCentral},
	}
	regs := RegionNames(locs)
	names := make([]string, 0, len(regs))
	for _, r := range regs {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"ash", "fsn1", "nbg1"}) {
		t.Errorf("regions %v", names)
	}
	if z, ok := LocationZones(locs, "ash"); !ok || len(z) != 1 || z[0] != (vendors.Zone{ID: "ash", Name: "ash"}) {
		t.Errorf("zones %v %v", z, ok)
	}
	if _, ok := LocationZones(locs, "nbg1-dc3"); ok {
		t.Error("an unknown location has no zone")
	}
	if NetworkZoneOf(locs, "ash") != hc.NetworkZoneUSEast || NetworkZoneOf(locs, "x") != "" {
		t.Error("NetworkZoneOf")
	}
	n := &hc.Network{Subnets: []hc.NetworkSubnet{
		{Type: hc.NetworkSubnetTypeCloud, NetworkZone: hc.NetworkZoneEUCentral},
		{Type: hc.NetworkSubnetTypeVSwitch, NetworkZone: hc.NetworkZoneUSEast},
	}}
	if !NetworkHasSubnetIn(n, hc.NetworkZoneEUCentral) || NetworkHasSubnetIn(n, hc.NetworkZoneUSEast) || NetworkHasSubnetIn(n, hc.NetworkZoneUSWest) {
		t.Error("NetworkHasSubnetIn")
	}
}

func TestLaunchable(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	fsn, nbg, ash := &hc.Location{Name: "fsn1"}, &hc.Location{Name: "nbg1"}, &hc.Location{Name: "ash"}
	typ := &hc.ServerType{Name: "cpx22", Locations: []hc.ServerTypeLocation{
		{Location: fsn, Available: true},
		{Location: nbg, Available: false},
		{Location: ash, Available: true, DeprecatableResource: hc.DeprecatableResource{Deprecation: &hc.DeprecationInfo{UnavailableAfter: now.Add(-time.Hour)}}},
	}}
	for loc, want := range map[string]bool{"fsn1": true, "nbg1": false, "ash": false, "hel1": false} {
		if got := Launchable(typ, loc, now); got != want {
			t.Errorf("%s: %v, want %v", loc, got, want)
		}
	}
	typ.Locations[2].Deprecation.UnavailableAfter = now.Add(time.Hour)
	if !Launchable(typ, "ash", now) {
		t.Error("announced but not yet past unavailable_after")
	}
	got := LaunchableTypes([]*hc.ServerType{typ, {Name: "cx23"}}, "fsn1", now)
	if len(got) != 1 || !slices.Equal(got["cpx22"], []string{"fsn1"}) {
		t.Errorf("%v", got)
	}
}

func TestVolumeByIDPath(t *testing.T) {
	if VolumeByIDPath(123) != "/dev/disk/by-id/scsi-0HC_Volume_123" {
		t.Error(VolumeByIDPath(123))
	}
}
