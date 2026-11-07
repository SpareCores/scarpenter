//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// networkOnly is a ResourceProvider that hands out the default bridge.
type networkOnly struct{}

func (networkOnly) Require(_ context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	return []vendors.ResourceRef{{Kind: req.Kind, Region: Region, ID: DefaultBridge}}, nil
}

func prepare(t *testing.T, d *Driver, typ, storage string) *vendors.Prepared {
	t.Helper()
	in := options.Input{Block: json.RawMessage(`{}`), Profiles: vendors.Profiles{vendors.ProfileStorage: {"p": json.RawMessage(storage)}}}
	settings, err := options.Resolve(d, in, options.Selection{Storage: "p"})
	if err != nil {
		t.Fatal(err)
	}
	prep, err := d.Prepare(t.Context(), vendors.PrepareRequest{
		Region: Region, Zone: Zone, InstanceType: typ, Settings: settings, Resources: networkOnly{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return prep
}

func TestPrepareFindsDisksOfOneSizeByPatternAndSizeAndUniqueOnesByID(t *testing.T) {
	d, _ := newTestDriver(t)
	prep := prepare(t, d, "small", `{"data":[
		{"name":"a","size":"1Gi","mountPath":"/a"},
		{"name":"b","size":"1Gi","mountPath":"/b","filesystem":"xfs"},
		{"name":"c","size":"2Gi","mountPath":"/c"}]}`)
	if len(prep.Resources) != 1 || prep.Resources[0].ID != DefaultBridge {
		t.Errorf("resources %+v", prep.Resources)
	}
	want := []vendors.DeviceHint{
		{Name: "a", Filesystem: "ext4", MountPath: "/a", SerialPattern: "scv*", SizeBytes: 1 << 30},
		{Name: "b", Filesystem: "xfs", MountPath: "/b", SerialPattern: "scv*", SizeBytes: 1 << 30},
		{Name: "c", Filesystem: "ext4", MountPath: "/c", ByID: "/dev/disk/by-id/virtio-" + dataSerial("c")},
	}
	if len(prep.Devices) != len(want) {
		t.Fatalf("devices %+v", prep.Devices)
	}
	for i := range want {
		if prep.Devices[i] != want[i] {
			t.Errorf("device %d: %+v, want %+v", i, prep.Devices[i], want[i])
		}
	}
	if prep.Local != nil {
		t.Errorf("local hint %+v without local storage", prep.Local)
	}
}

func TestPrepareHintsAndTheAttachedSerialsAgree(t *testing.T) {
	d, _ := newTestDriver(t)
	prep := prepare(t, d, "local", `{"data":[{"name":"db","size":"1Gi","mountPath":"/db"}],"localStorage":{"policy":"raid0","mountPath":"/scratch"}}`)
	if prep.Local == nil || prep.Local.Count != 2 || prep.Local.Policy != "raid0" || prep.Local.MountPath != "/scratch" ||
		prep.Local.DevicePattern != "virtio-scl*" {
		t.Fatalf("local hint %+v", prep.Local)
	}
	// What the guest is told to look for is what the VM gets.
	set, _ := decodeSettings(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"data":[{"name":"db","size":"1Gi","mountPath":"/db"}]}}`)})
	plan := diskPlan(set, 2, 4_000_000_000)
	if len(plan) != 3 || !strings.HasSuffix(prep.Devices[0].ByID, plan[0].Serial) || plan[1].Serial != "scl0" || plan[2].Serial != "scl1" {
		t.Errorf("plan %+v does not match the hints %+v", plan, prep.Devices)
	}
}

func TestPrepareRejectsLocalStorageWithoutAMountPath(t *testing.T) {
	d, _ := newTestDriver(t)
	settings := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"localStorage":{"policy":"mount"}}}`)}
	_, err := d.Prepare(t.Context(), vendors.PrepareRequest{InstanceType: "local", Settings: settings, Resources: networkOnly{}})
	if !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("%v, want Fatal", err)
	}
}

func TestSupportedLeavesOutWhatTheTypeOrHostCannotDo(t *testing.T) {
	d, h := newTestDriver(t)
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {
			"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/l"}}`),
			"skip":  json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
			"data":  json.RawMessage(`{"data":[{"name":"a","size":"1Gi","mountPath":"/a"}]}`),
			"bad":   json.RawMessage(`{"data":"nope"}`),
		},
		vendors.ProfilePlacement: {"pin": json.RawMessage(`{"pinCPUs":true}`), "loose": json.RawMessage(`{"pinCPUs":false}`)},
		vendors.ProfileNetwork:   {"nic": json.RawMessage(`{"model":"e1000"}`)},
	}
	small := vendors.CatalogType{VCPUs: 2}
	got := d.Supported(small, Zone, profiles)
	if strings.Join(got[vendors.ProfileStorage], ",") != "data,skip" || strings.Join(got[vendors.ProfilePlacement], ",") != "loose,pin" ||
		strings.Join(got[vendors.ProfileNetwork], ",") != "nic" {
		t.Errorf("small: %v", got)
	}
	withLocal := vendors.CatalogType{VCPUs: 2, LocalStorageGB: 8, LocalStorageCount: 2}
	if got := d.Supported(withLocal, Zone, profiles); strings.Join(got[vendors.ProfileStorage], ",") != "data,local,skip" {
		t.Errorf("local type: %v", got)
	}
	h.cpus = 1
	if got := d.Supported(small, Zone, profiles); strings.Join(got[vendors.ProfilePlacement], ",") != "loose" {
		t.Errorf("one host CPU: %v", got)
	}
}

func TestDefaultsAndRootDisk(t *testing.T) {
	d, _ := newTestDriver(t)
	def := d.Defaults()
	if def.RootDiskBytes != 20<<30 {
		t.Errorf("the default root disk is %d bytes", def.RootDiskBytes)
	}
	block := json.RawMessage(`{"storage":{"root":{"size":"50Gi"}}}`)
	s, err := options.Resolve(d, options.Input{Block: block}, options.Selection{})
	if err != nil || s.RootDiskBytes != 50<<30 {
		t.Errorf("a vendor block with a root size: %d, %v", s.RootDiskBytes, err)
	}
	if delta, complete := d.PriceDelta(s, vendors.Offering{}); delta != 0 || !complete {
		t.Errorf("price delta %v, %v", delta, complete)
	}
}
