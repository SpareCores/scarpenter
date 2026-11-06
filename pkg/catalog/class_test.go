/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

func nodeClass(name string) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{
			AWS:  &aws.Settings{Regions: []string{"us-east-1", "eu-west-1"}},
			Fake: &fake.Settings{},
		}},
	}
}

func TestClassFromNodeClass(t *testing.T) {
	nc := nodeClass("default")
	maxPods, overhead := int32(58), 10.0
	nc.Spec.Kubelet = v1alpha1.KubeletSpec{
		MaxPods: &maxPods, VMMemoryOverheadPercent: &overhead,
		KubeReserved:   map[string]string{"cpu": "250m"},
		SystemReserved: map[string]string{"memory": "300Mi"},
		EvictionHard:   map[string]string{"memory.available": "5%", "nodefs.available": "10Gi"},
	}
	nc.Spec.Catalog = &v1alpha1.CatalogSpec{CPUFlagLabels: []string{"sve", "avx2", "avx2"}}
	cl, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	if cl.MaxPods != 58 || cl.VMMemoryOverheadPercent != 10 {
		t.Errorf("kubelet: %+v", cl)
	}
	if !slices.Equal(cl.CPUFlags, []string{"avx2", "sve"}) {
		t.Errorf("cpu flags = %v", cl.CPUFlags)
	}
	if got := cl.VendorNames(); !slices.Equal(got, []string{"aws", "fake"}) {
		t.Errorf("vendors = %v", got)
	}
	if got := cl.Vendors["aws"]; !slices.Equal(got, []string{"eu-west-1", "us-east-1"}) {
		t.Errorf("aws regions = %v", got)
	}
	if len(cl.Vendors["fake"]) != 0 {
		t.Errorf("fake regions = %v", cl.Vendors["fake"])
	}
	if cl.Hash == "" || cl.Name != "default" {
		t.Errorf("name/hash: %q %q", cl.Name, cl.Hash)
	}
	if !cl.AllowIncompletePrice {
		t.Error("incomplete prices are allowed by default")
	}
}

func TestClassDefaults(t *testing.T) {
	cl, err := catalog.ClassFromNodeClass(nodeClass("d"))
	if err != nil {
		t.Fatal(err)
	}
	if cl.MaxPods != 110 || cl.VMMemoryOverheadPercent != 7.5 {
		t.Errorf("defaults: %d pods, %v%% overhead", cl.MaxPods, cl.VMMemoryOverheadPercent)
	}
	if !slices.Equal(cl.CPUFlags, []string{"amx_tile", "avx2", "avx512f", "sha_ni"}) {
		t.Errorf("default flags = %v", cl.CPUFlags)
	}
}

func TestClassHashCoversWhatChangesTheTypes(t *testing.T) {
	base, _ := catalog.ClassFromNodeClass(nodeClass("a"))
	same, _ := catalog.ClassFromNodeClass(nodeClass("b"))
	if base.Hash != same.Hash {
		t.Error("the hash depends on the name: equal classes must share cache entries")
	}
	for name, mutate := range map[string]func(*v1alpha1.SCNodeClass){
		"max pods": func(nc *v1alpha1.SCNodeClass) { p := int32(30); nc.Spec.Kubelet.MaxPods = &p },
		"regions":  func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"us-east-1"} },
		"cpu flags": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Catalog = &v1alpha1.CatalogSpec{CPUFlagLabels: []string{"sve"}}
		},
		"reserved":   func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.SystemReserved = map[string]string{"cpu": "1"} },
		"vendors":    func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.Fake = nil },
		"incomplete": func(nc *v1alpha1.SCNodeClass) { f := false; nc.Spec.Pricing.AllowIncomplete = &f },
		"metrics": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Metrics = map[string]v1alpha1.Metric{"m": {Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)}}
		},
	} {
		nc := nodeClass("a")
		mutate(nc)
		cl, err := catalog.ClassFromNodeClass(nc)
		if err != nil {
			t.Fatal(err)
		}
		if cl.Hash == base.Hash {
			t.Errorf("changing %s does not change the hash", name)
		}
	}
}

func TestClassRejectsBadQuantities(t *testing.T) {
	nc := nodeClass("bad")
	nc.Spec.Kubelet.KubeReserved = map[string]string{"cpu": "lots"}
	nc.Spec.Kubelet.EvictionHard = map[string]string{"memory.available": "150%"}
	if _, err := catalog.ClassFromNodeClass(nc); err == nil {
		t.Fatal("bad quantities were accepted")
	}
}

func TestClassProfilesPerVendor(t *testing.T) {
	nc := nodeClass("p")
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{
		"db":   {AWS: &aws.Storage{}},
		"only": {Fake: &fake.Storage{}},
	}
	nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"colocated": {AWS: &aws.Placement{}}}
	cl, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	if got := cl.Profiles["aws"][labels.ProfileStorage]; !slices.Equal(got, []string{"db"}) {
		t.Errorf("aws storage profiles = %v", got)
	}
	if got := cl.Profiles["fake"][labels.ProfileStorage]; !slices.Equal(got, []string{"only"}) {
		t.Errorf("fake storage profiles = %v", got)
	}
	if got := cl.Profiles["fake"][labels.ProfilePlacement]; len(got) != 0 {
		t.Errorf("fake placement profiles = %v: a profile does not exist on a vendor without a block", got)
	}
}

// profileFacts supports every defined profile in one zone only.
type profileFacts struct{ catalog.StaticFacts }

func (profileFacts) Supported(_, _, zone string, defined labels.Profiles) map[labels.ProfileKind][]string {
	if zone != "zone-1" {
		return nil
	}
	return defined
}

func TestProfileLabelsFollowTheSupportedHook(t *testing.T) {
	src := &fakeSource{data: synthetic()}
	c := catalog.New(catalog.Options{Source: src, Facts: profileFacts{syntheticFacts()}})
	cl := catalog.NewClass("c", catalog.Scope{"v": nil})
	cl.Profiles = map[string]labels.Profiles{"v": {labels.ProfileStorage: {"db-io2"}}}
	cl.Finalize()
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	small := byName(all(t, c, catalog.Filter{}), "v.small")
	// The type allows the union over its zones.
	if r := small.Requirements.Get(v1alpha1.LabelStorageProfile); !r.Has("db-io2") || !r.Has("default") {
		t.Errorf("type storage profiles = %v", r.Values())
	}
	if r := small.Requirements.Get(v1alpha1.LabelNetworkProfile); r.Len() != 1 || !r.Has("default") {
		t.Errorf("type network profiles = %v", r.Values())
	}
	// Offerings of the zone that cannot host the profile say so.
	for _, o := range small.Offerings {
		r := o.Requirements.Get(v1alpha1.LabelStorageProfile)
		switch o.Zone() {
		case "zone-1":
			// Same as the union: nothing extra on the offering.
			if o.Requirements.Has(v1alpha1.LabelStorageProfile) && !r.Has("db-io2") {
				t.Errorf("zone-1 offering: %v", r.Values())
			}
		case "zone-2":
			if !r.Has("default") || r.Has("db-io2") {
				t.Errorf("zone-2 offering profiles = %v, want only default", r.Values())
			}
		}
	}
}
