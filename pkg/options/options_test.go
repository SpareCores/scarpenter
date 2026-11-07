/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// driver is a NodeOptions whose defaults are a root disk and one data volume.
type driver struct{}

func (driver) Defaults() vendors.VendorSettings {
	return vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"volumeType":"gp3","volumeSize":"64Gi"}},"metadataOptions":{"httpTokens":"required","httpPutResponseHopLimit":1}}`)}
}

func (d driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	var v struct {
		Storage struct {
			Root struct {
				VolumeSize string `json:"volumeSize"`
			} `json:"root"`
		} `json:"storage"`
	}
	_ = json.Unmarshal(s.Raw, &v)
	q, _ := resource.ParseQuantity(v.Storage.Root.VolumeSize)
	return q.Value()
}

func (driver) Supported(vendors.CatalogType, string, vendors.Profiles) map[string][]string {
	return nil
}
func (driver) PriceDelta(vendors.VendorSettings, vendors.Offering) (float64, bool) { return 0, true }
func (driver) Prepare(context.Context, vendors.PrepareRequest) (*vendors.Prepared, error) {
	return &vendors.Prepared{}, nil
}

func gib(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func nodeClass() *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "bench"},
		Spec: v1alpha1.SCNodeClassSpec{
			Networking: common.Networking{Isolation: common.IsolationNodeClass},
			Vendors: v1alpha1.VendorBlocks{
				AWS: &aws.Settings{
					Regions: []string{"us-east-1"},
					Storage: &aws.Storage{Root: &aws.RootVolume{VolumeSize: gib("128Gi"), Encrypted: new(true)}},
					Networking: &common.NetworkingOverride{
						PublicIPv6: new(true),
					},
					MetadataOptions: &aws.MetadataOptions{HTTPTokens: "required"},
					Resources: &aws.Resources{
						Subnets: &common.Resource{Forbid: true},
					},
				},
				QEMU: &qemu.Settings{},
			},
			Profiles: v1alpha1.Profiles{
				Storage: map[string]v1alpha1.StorageProfile{
					"db": {
						AWS: &aws.Storage{
							Root: &aws.RootVolume{VolumeType: aws.VolumeTypeIO2, IOPS: new(int32(3000))},
							Data: []aws.DataVolume{{
								GuestVolume: common.GuestVolume{Name: "pgdata", MountPath: "/var/lib/pgdata", Filesystem: common.FilesystemXFS},
								VolumeType:  aws.VolumeTypeIO2, VolumeSize: *gib("500Gi"), IOPS: new(int32(32000)),
							}},
						},
					},
					"qemu-only": {QEMU: &qemu.Storage{Root: &qemu.RootDisk{Size: gib("10Gi")}}},
				},
				Placement: map[string]v1alpha1.PlacementProfile{
					"colocated": {AWS: &aws.Placement{PlacementGroup: &aws.PlacementGroup{Strategy: aws.PlacementGroupStrategy("cluster")}}},
				},
				Network: map[string]v1alpha1.NetworkProfile{
					"ena": {AWS: &aws.Network{ENAExpress: &aws.ENAExpress{Enabled: true}}},
				},
			},
		},
	}
}

func inputOf(t *testing.T, nc *v1alpha1.SCNodeClass, vendor string) options.Input {
	t.Helper()
	in, ok, err := options.InputFromNodeClass(nc, vendor)
	if err != nil || !ok {
		t.Fatalf("InputFromNodeClass(%s) = %v, %v", vendor, ok, err)
	}
	return in
}

func TestRegisteredKeysAreWellKnown(t *testing.T) {
	for _, s := range options.Specs() {
		if !options.Registered(s.Key) {
			t.Errorf("%s is not registered with Karpenter core", s.Key)
		}
		if !slices.Contains(v1alpha1.WellKnownLabels, s.Key) {
			t.Errorf("%s is not in the API package's well-known list", s.Key)
		}
		if !strings.HasPrefix(s.Key, v1alpha1.Group+"/") {
			t.Errorf("%s is outside the restricted domain", s.Key)
		}
		if got, ok := options.Lookup(s.Key); !ok || got.Kind != s.Kind {
			t.Errorf("Lookup(%s) = %+v, %v", s.Key, got, ok)
		}
	}
	kinds := map[string]options.Kind{}
	for _, s := range options.Specs() {
		kinds[s.Key] = s.Kind
	}
	for _, key := range []string{v1alpha1.LabelStorageProfile, v1alpha1.LabelNetworkProfile, v1alpha1.LabelPlacementProfile} {
		if kinds[key] != options.KindEnum {
			t.Errorf("%s must be an Enum", key)
		}
	}
	if kinds[v1alpha1.LabelPlacementGroup] != options.KindName {
		t.Error("the run key must be a free Name")
	}
	if !karpv1.RestrictedLabelDomains.Has(v1alpha1.Group) {
		t.Error("the domain is not restricted")
	}
}

func TestValidateValue(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{v1alpha1.LabelPlacementGroup, "pg-bench-1", true},
		{v1alpha1.LabelPlacementGroup, "Run_1.a", true},
		{v1alpha1.LabelPlacementGroup, "", false},
		{v1alpha1.LabelPlacementGroup, strings.Repeat("a", 64), false},
		{v1alpha1.LabelPlacementGroup, "has space", false},
		{v1alpha1.LabelStorageProfile, "db-io2", true},
		{"example.com/other", "x", false},
	} {
		if err := options.ValidateValue(tc.key, tc.value); (err == nil) != tc.ok {
			t.Errorf("ValidateValue(%s, %q) = %v, want ok=%v", tc.key, tc.value, err, tc.ok)
		}
	}
}

func TestInputStripsWhatTheDriverDoesNotDecideAndKeepsProfilesPerVendor(t *testing.T) {
	nc := nodeClass()
	in := inputOf(t, nc, "aws")
	var block map[string]any
	if err := json.Unmarshal(in.Block, &block); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"regions", "resources", "networking"} {
		if _, has := block[gone]; has {
			t.Errorf("the block still has %q: %s", gone, in.Block)
		}
	}
	if _, has := block["storage"]; !has {
		t.Errorf("the block lost its storage: %s", in.Block)
	}
	// The generic networking is the NodeClass's with the vendor's override.
	if !*in.Networking.PublicIPv4 || !*in.Networking.PublicIPv6 || in.Networking.Isolation != common.IsolationNodeClass {
		t.Errorf("networking = %+v", in.Networking)
	}

	names := in.ProfileNames()
	if !slices.Equal(names[labels.ProfileStorage], []string{"db"}) ||
		!slices.Equal(names[labels.ProfilePlacement], []string{"colocated"}) ||
		!slices.Equal(names[labels.ProfileNetwork], []string{"ena"}) {
		t.Errorf("aws profiles = %v: a profile without an aws block must not exist on aws", names)
	}
	q := inputOf(t, nc, "qemu")
	if got := q.ProfileNames(); !slices.Equal(got[labels.ProfileStorage], []string{"qemu-only"}) || len(got[labels.ProfilePlacement]) != 0 {
		t.Errorf("qemu profiles = %v", got)
	}
	if _, ok, _ := options.InputFromNodeClass(nc, "gcp"); ok {
		t.Error("a vendor without a block has no input")
	}
}

func TestResolveLayersDefaultsBlockAndProfiles(t *testing.T) {
	in := inputOf(t, nodeClass(), "aws")
	def, err := options.Resolve(driver{}, in, options.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(def.Raw, &got); err != nil {
		t.Fatal(err)
	}
	root := got["storage"].(map[string]any)["root"].(map[string]any)
	// The block overrides the default size, keeps the default type, adds encryption.
	if root["volumeSize"] != "128Gi" || root["volumeType"] != "gp3" || root["encrypted"] != true {
		t.Errorf("root disk = %v", root)
	}
	if got["metadataOptions"].(map[string]any)["httpPutResponseHopLimit"] != float64(1) {
		t.Errorf("a default the block does not mention was lost: %v", got["metadataOptions"])
	}
	if def.RootDiskBytes != 128<<30 {
		t.Errorf("RootDiskBytes = %d", def.RootDiskBytes)
	}
	for kind, name := range def.Profiles {
		if name != "default" {
			t.Errorf("profile %s = %s without a selection", kind, name)
		}
	}

	prof, err := options.Resolve(driver{}, in, options.Selection{Storage: "db", Network: "ena", Placement: "colocated"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(prof.Raw, &got); err != nil {
		t.Fatal(err)
	}
	st := got["storage"].(map[string]any)
	root = st["root"].(map[string]any)
	// The profile overrides what it sets and keeps the rest of the layer below.
	if root["volumeType"] != "io2" || root["iops"] != float64(3000) || root["volumeSize"] != "128Gi" || root["encrypted"] != true {
		t.Errorf("root disk with the profile = %v", root)
	}
	if data := st["data"].([]any); len(data) != 1 || data[0].(map[string]any)["name"] != "pgdata" {
		t.Errorf("data volumes = %v", st["data"])
	}
	if !strings.Contains(string(prof.Network), "enaExpress") || !strings.Contains(string(prof.Placement), "cluster") {
		t.Errorf("network %s, placement %s", prof.Network, prof.Placement)
	}
	if prof.Profiles["storage"] != "db" || prof.Profiles["network"] != "ena" || prof.Profiles["placement"] != "colocated" {
		t.Errorf("applied = %v", prof.Profiles)
	}
	// The profile is not applied to the default route.
	if strings.Contains(string(def.Raw), "pgdata") {
		t.Error("the profile leaked into the default settings")
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	in := inputOf(t, nodeClass(), "aws")
	sel := options.Selection{Storage: "db", Placement: "colocated"}
	first, err := options.Resolve(driver{}, in, sel)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, _ := options.Resolve(driver{}, in, sel)
		if string(again.Raw) != string(first.Raw) || string(again.Placement) != string(first.Placement) {
			t.Fatal("two resolutions of the same input differ")
		}
	}
}

func TestResolveRefusesAProfileWithoutABlock(t *testing.T) {
	in := inputOf(t, nodeClass(), "aws")
	_, err := options.Resolve(driver{}, in, options.Selection{Storage: "qemu-only"})
	if !errors.Is(err, options.ErrProfileNotDefined) {
		t.Errorf("a profile that has no block for the vendor: %v, want ErrProfileNotDefined (ADR-017)", err)
	}
	if err := options.Validate(driver{}, in); err != nil {
		t.Errorf("Validate of a good input: %v", err)
	}
	bad := in
	bad.Block = json.RawMessage(`[1,2]`)
	if err := options.Validate(driver{}, bad); err == nil {
		t.Error("Validate accepted a block that is not an object")
	}
}

func TestResolveNetworkingDefaults(t *testing.T) {
	got := options.ResolveNetworking(common.Networking{}, nil)
	if !*got.PublicIPv4 || *got.PublicIPv6 || got.Isolation != common.IsolationRun {
		t.Errorf("defaults = %+v", got)
	}
	got = options.ResolveNetworking(common.Networking{PublicIPv4: new(false), Isolation: common.IsolationNone},
		&common.NetworkingOverride{PublicIPv4: new(true), Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"10.0.0.0/8"}}}}})
	if !*got.PublicIPv4 || got.Isolation != common.IsolationNone || got.Inbound == nil || len(got.Inbound.Rules) != 1 {
		t.Errorf("override = %+v", got)
	}
}

func reqs(rs ...*scheduling.Requirement) scheduling.Requirements {
	return scheduling.NewRequirements(rs...)
}

func in(key string, values ...string) *scheduling.Requirement {
	return scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, values...)
}

func TestSelectPicksProfilesFromTheClaim(t *testing.T) {
	typ := reqs(
		in(v1alpha1.LabelStorageProfile, "default", "db", "fast"),
		in(v1alpha1.LabelNetworkProfile, "default"),
		in(v1alpha1.LabelPlacementProfile, "default", "colocated"))
	off := reqs()
	tests := []struct {
		name   string
		claim  scheduling.Requirements
		want   options.Selection
		wantOK bool
	}{
		{"no constraint is the default", reqs(), options.Selection{Storage: "default", Network: "default", Placement: "default"}, true},
		{"one profile", reqs(in(v1alpha1.LabelStorageProfile, "db")),
			options.Selection{Storage: "db", Network: "default", Placement: "default"}, true},
		{"several allowed: the default wins", reqs(in(v1alpha1.LabelStorageProfile, "db", "default")),
			options.Selection{Storage: "default", Network: "default", Placement: "default"}, true},
		{"several allowed without the default: first by name", reqs(in(v1alpha1.LabelStorageProfile, "fast", "db")),
			options.Selection{Storage: "db", Network: "default", Placement: "default"}, true},
		{"a profile the type cannot host", reqs(in(v1alpha1.LabelStorageProfile, "other")), options.Selection{}, false},
		{"a network profile the type lacks", reqs(in(v1alpha1.LabelNetworkProfile, "ena")), options.Selection{}, false},
		{"two kinds", reqs(in(v1alpha1.LabelStorageProfile, "fast"), in(v1alpha1.LabelPlacementProfile, "colocated")),
			options.Selection{Storage: "fast", Network: "default", Placement: "colocated"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := options.Select(tc.claim, typ, off)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("Select = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	// The offering narrows what the type allows (a zone that cannot host the profile).
	offNarrow := reqs(in(v1alpha1.LabelStorageProfile, "default"))
	if _, ok := options.Select(reqs(in(v1alpha1.LabelStorageProfile, "db")), typ, offNarrow); ok {
		t.Error("an offering that cannot host the profile was selected")
	}
}

func TestRunKey(t *testing.T) {
	if k, err := options.RunKey(reqs()); k != "" || err != nil {
		t.Errorf("no key: %q, %v", k, err)
	}
	if k, err := options.RunKey(reqs(in(v1alpha1.LabelPlacementGroup, "pg-1"))); k != "pg-1" || err != nil {
		t.Errorf("one key: %q, %v", k, err)
	}
	if _, err := options.RunKey(reqs(in(v1alpha1.LabelPlacementGroup, "a", "b"))); err == nil {
		t.Error("two keys are ambiguous")
	}
	if _, err := options.RunKey(reqs(scheduling.NewRequirement(v1alpha1.LabelPlacementGroup, corev1.NodeSelectorOpExists))); err == nil {
		t.Error("Exists names no key")
	}
}

func TestProfileHashesChangeOnlyWithTheirProfile(t *testing.T) {
	nc := nodeClass()
	in1 := inputOf(t, nc, "aws")
	sel := options.Selection{Storage: "db", Placement: "colocated"}
	h1 := in1.AppliedHashes(sel)
	if len(h1) != 2 || h1["storage/db"] == "" || h1["placement/colocated"] == "" {
		t.Fatalf("hashes = %v", h1)
	}
	if got := in1.AppliedHashes(options.Selection{}); len(got) != 0 {
		t.Errorf("the default route has no profile hashes: %v", got)
	}

	// Change the storage profile only.
	prof := nc.Spec.Profiles.Storage["db"]
	prof.AWS.Data[0].VolumeSize = *gib("600Gi")
	nc.Spec.Profiles.Storage["db"] = prof
	in2 := inputOf(t, nc, "aws")
	for key, old := range h1 {
		now, ok := in2.CurrentHash(key)
		if !ok {
			t.Fatalf("%s vanished", key)
		}
		if changed := now != old; changed != (key == "storage/db") {
			t.Errorf("%s changed=%v: only the edited profile may change", key, changed)
		}
	}
	// A removed profile is reported as absent.
	delete(nc.Spec.Profiles.Storage, "db")
	if _, ok := inputOf(t, nc, "aws").CurrentHash("storage/db"); ok {
		t.Error("a removed profile still has a hash")
	}
}

func TestResourceConfigReadsTheKinds(t *testing.T) {
	cfg, err := options.ResourceConfig(nodeClass(), "aws")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg["subnets"].Forbid || len(cfg) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg, err := options.ResourceConfig(nodeClass(), "gcp"); err != nil || cfg != nil {
		t.Errorf("a vendor without a block: %v, %v", cfg, err)
	}
}
