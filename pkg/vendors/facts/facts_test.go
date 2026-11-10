/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package facts_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// driver is a vendor with NodeOptions: a root disk, a price per data volume
// GiB, and the profiles it can host (calls counts the price calls).
type driver struct {
	epoch  atomic.Uint64
	prices atomic.Int32
	// hosts says which profiles the types can host.
	hosts func(kind, name string, it vendors.CatalogType, zone string) bool
	// typeRoot is the root disk a type defines (RootDiskForType), by API reference.
	typeRoot map[string]int64
	// noRoot makes the settings say no root disk (RootDiskBytes 0).
	noRoot bool
}

func (d *driver) RootDiskForType(_ vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	b, ok := d.typeRoot[it.APIReference]
	return b, ok
}

func (d *driver) Vendor() string                             { return "aws" }
func (d *driver) Init(context.Context, vendors.Config) error { return nil }
func (d *driver) Regions(context.Context) ([]vendors.Region, error) {
	return []vendors.Region{{Name: "us-east-1"}}, nil
}
func (d *driver) Zones(context.Context, string) ([]vendors.Zone, error) {
	return []vendors.Zone{{ID: "use1-az1", Name: "us-east-1a"}}, nil
}
func (d *driver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return nil, nil
}
func (d *driver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }
func (d *driver) TagKey(k vendors.TagKey) string                            { return vendors.DefaultTagKey(k) }

func (d *driver) Defaults() vendors.VendorSettings {
	return vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"volumeSize":"64Gi"}}}`), RootDiskBytes: d.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"volumeSize":"64Gi"}}}`)})}
}

func (d *driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	if d.noRoot {
		return 0
	}
	var v struct {
		Storage struct {
			Root struct{ VolumeSize string }
		}
	}
	_ = json.Unmarshal(s.Raw, &v)
	q := resource.MustParse(v.Storage.Root.VolumeSize)
	return q.Value()
}

func (d *driver) Supported(it vendors.CatalogType, zone string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	for kind, byName := range p {
		for name := range byName {
			if d.hosts == nil || d.hosts(kind, name, it, zone) {
				out[kind] = append(out[kind], name)
			}
		}
	}
	// A name nobody defined: the facts must not pass it on.
	out[vendors.ProfileStorage] = append(out[vendors.ProfileStorage], "ghost")
	return out
}

func (d *driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	d.prices.Add(1)
	delta := float64(d.RootDiskBytes(s)-(64<<30)) / (1 << 30) * 0.01
	return delta, !strings.Contains(string(s.Raw), `"iops"`)
}

func (d *driver) Prepare(context.Context, vendors.PrepareRequest) (*vendors.Prepared, error) {
	return &vendors.Prepared{}, nil
}

func (d *driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

func nodeClass(root string) *v1alpha1.SCNodeClass {
	size := resource.MustParse(root)
	iops := new(int32)
	*iops = 4000
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: v1alpha1.SCNodeClassSpec{
			Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{Storage: &aws.Storage{Root: &aws.RootVolume{VolumeSize: &size}}}},
			Profiles: v1alpha1.Profiles{Storage: map[string]v1alpha1.StorageProfile{
				"plain":       {AWS: &aws.Storage{}},
				"provisioned": {AWS: &aws.Storage{Root: &aws.RootVolume{VolumeType: "gp3", IOPS: iops}}},
			}},
		},
	}
}

func setup(t *testing.T, d *driver) *facts.Facts {
	t.Helper()
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f
}

var srv = &model.Server{Vendor: "aws", APIReference: "m7i.large", Family: "m7i", Arch: "amd64", VCPUs: 2, MemoryMiB: 8192}

func TestSupportedPassesTheBlocksAndOnlyDefinedProfiles(t *testing.T) {
	d := &driver{hosts: func(kind, name string, it vendors.CatalogType, zone string) bool {
		return kind != vendors.ProfileStorage || name != "plain" || it.APIReference != "m7i.large"
	}}
	f := setup(t, d)
	class, err := catalog.ClassFromNodeClass(nodeClass("64Gi"))
	if err != nil {
		t.Fatal(err)
	}
	got := f.Supported("aws", &class, srv, "us-east-1a")
	if want := []string{"provisioned"}; !slices.Equal(got[labels.ProfileStorage], want) {
		t.Errorf("supported = %v, want %v: the driver's rule, the ghost name filtered out", got, want)
	}
	other := &model.Server{Vendor: "aws", APIReference: "c7i.large"}
	if got := f.Supported("aws", &class, other, "us-east-1a"); !slices.Equal(got[labels.ProfileStorage], []string{"plain", "provisioned"}) {
		t.Errorf("another type = %v", got)
	}
	if got := f.Supported("gcp", &class, srv, "z"); got != nil {
		t.Errorf("a vendor without a driver supports %v", got)
	}
}

func TestSupportedDropsIncompletePricesWhenTheClassDoesNotAllowThem(t *testing.T) {
	f := setup(t, &driver{})
	nc := nodeClass("64Gi")
	nc.Spec.Pricing.AllowIncomplete = new(false)
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	got := f.Supported("aws", &class, srv, "us-east-1a")
	if !slices.Equal(got[labels.ProfileStorage], []string{"plain"}) {
		t.Errorf("supported = %v: the profile with provisioned IOPS has an incomplete price and must not exist", got)
	}
	allow, _ := catalog.ClassFromNodeClass(nodeClass("64Gi"))
	if got := f.Supported("aws", &allow, srv, "us-east-1a"); !slices.Equal(got[labels.ProfileStorage], []string{"plain", "provisioned"}) {
		t.Errorf("with incomplete prices allowed: %v", got)
	}
}

func TestPriceDeltaAndRootDiskFollowTheClassDefaults(t *testing.T) {
	d := &driver{}
	f := setup(t, d)
	small, _ := catalog.ClassFromNodeClass(nodeClass("64Gi"))
	big, _ := catalog.ClassFromNodeClass(nodeClass("164Gi"))
	if delta, ok := f.PriceDelta("aws", &small, srv, "us-east-1a", "on-demand"); delta != 0 || !ok {
		t.Errorf("small = %v, %v", delta, ok)
	}
	if delta, ok := f.PriceDelta("aws", &big, srv, "us-east-1a", "on-demand"); delta < 0.99 || delta > 1.01 || !ok {
		t.Errorf("a 100 GiB larger root disk costs %v, want 1.00", delta)
	}
	if b, ok := f.RootDiskBytes("aws", &big, srv); !ok || b != 164<<30 {
		t.Errorf("root disk of the class = %d, %v; the scheduler must assume the class's size", b, ok)
	}
	if b, _ := f.RootDiskBytes("aws", &small, srv); b != 64<<30 {
		t.Errorf("small root disk = %d", b)
	}
}

func TestRootDiskIsPerType(t *testing.T) {
	class, _ := catalog.ClassFromNodeClass(nodeClass("64Gi"))
	withStorage := &model.Server{Vendor: "aws", APIReference: "cx22", Family: "cx", VCPUs: 2, LocalStorageGB: 40}
	other := &model.Server{Vendor: "aws", APIReference: "cx32", Family: "cx", VCPUs: 4}

	// The type defines its root disk: the driver's answer wins over the
	// settings, per type.
	f := setup(t, &driver{typeRoot: map[string]int64{"cx22": 40 << 30, "cx32": 80 << 30}})
	for srv, want := range map[*model.Server]int64{withStorage: 40 << 30, other: 80 << 30} {
		if b, ok := f.RootDiskBytes("aws", &class, srv); !ok || b != want {
			t.Errorf("%s: root disk %d, %v; want %d", srv.APIReference, b, ok, want)
		}
	}
	// A type the driver does not define uses the settings.
	f = setup(t, &driver{typeRoot: map[string]int64{"cx22": 40 << 30}})
	if b, ok := f.RootDiskBytes("aws", &class, other); !ok || b != 64<<30 {
		t.Errorf("a type without its own root disk: %d, %v; want the settings' 64 GiB", b, ok)
	}
	// A driver that answers 0 and settings that say nothing: the catalog's
	// storage size of the type, and unknown without one.
	f = setup(t, &driver{typeRoot: map[string]int64{"cx22": 0}, noRoot: true})
	if b, ok := f.RootDiskBytes("aws", &class, withStorage); !ok || b != 40_000_000_000 {
		t.Errorf("fallback to the catalog: %d, %v; want 40 GB", b, ok)
	}
	if b, ok := f.RootDiskBytes("aws", &class, other); ok || b != 0 {
		t.Errorf("nothing says: %d, %v; want unknown", b, ok)
	}
	// A vendor without NodeOptions falls back to the catalog too.
	if b, ok := f.RootDiskBytes("nodriver", &class, withStorage); !ok || b != 40_000_000_000 {
		t.Errorf("a vendor without NodeOptions: %d, %v", b, ok)
	}
}

func TestEpochFollowsTheDriver(t *testing.T) {
	d := &driver{}
	f := setup(t, d)
	before := f.Epoch()
	d.epoch.Add(1)
	if f.Epoch() == before {
		t.Error("a driver whose answers changed did not change the epoch: the catalog would serve stale profile labels")
	}
}
