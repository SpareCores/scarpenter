/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"slices"
	"strings"
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
)

func TestVendorNames(t *testing.T) {
	if got := (v1alpha1.VendorBlocks{}).Names(); len(got) != 0 {
		t.Errorf("empty blocks: got %v", got)
	}
	all := v1alpha1.VendorBlocks{QEMU: &qemu.Settings{}, AWS: &aws.Settings{}, Fake: &fake.Settings{}}
	if got, want := all.Names(), []string{"aws", "fake", "qemu"}; !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestLabelsAreRegisteredWithCore(t *testing.T) {
	for _, l := range v1alpha1.WellKnownLabels {
		if !strings.HasPrefix(l, v1alpha1.Group+"/") {
			t.Errorf("%s is not in the %s domain", l, v1alpha1.Group)
		}
		if !karpv1.WellKnownLabels.Has(l) {
			t.Errorf("%s is not registered as a well-known label", l)
		}
		if err := karpv1.IsRestrictedLabel(l); err != nil {
			t.Errorf("%s is rejected by core: %v", l, err)
		}
	}
	if !karpv1.RestrictedLabelDomains.Has(v1alpha1.Group) {
		t.Errorf("%s is not a restricted label domain", v1alpha1.Group)
	}
	// Typos in the domain are rejected (ADR-015).
	if err := karpv1.IsRestrictedLabel(v1alpha1.Group + "/vendr"); err == nil {
		t.Error("an unknown key in the SCarpenter domain was accepted")
	}
}

func TestDynamicLabelNames(t *testing.T) {
	p := v1alpha1.Group + "/"
	for got, want := range map[string]string{
		v1alpha1.MetricLabel("single-core"):           p + "metric-single-core",
		v1alpha1.MetricPercentileLabel("single-core"): p + "metric-single-core-pct",
		v1alpha1.MetricPerUSDLabel("single-core"):     p + "metric-single-core-per-usd",
		v1alpha1.CPUFlagLabel("avx2"):                 p + "cpu-flag-avx2",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestVendorAccessors(t *testing.T) {
	blocks := v1alpha1.VendorBlocks{AWS: &aws.Settings{}}
	if b, ok := blocks.Get("aws"); !ok || b.(*aws.Settings) != blocks.AWS {
		t.Errorf("Get(aws) = %v, %v", b, ok)
	}
	for _, v := range []string{"fake", "qemu", "hcloud"} {
		if _, ok := blocks.Get(v); ok {
			t.Errorf("Get(%s) reported a block", v)
		}
	}
	sp := v1alpha1.StorageProfile{QEMU: &qemu.Storage{}}
	if b, ok := sp.For("qemu"); !ok || b.(*qemu.Storage) != sp.QEMU {
		t.Errorf("StorageProfile.For(qemu) = %v, %v", b, ok)
	}
	if _, ok := sp.For("aws"); ok {
		t.Error("StorageProfile.For(aws) reported a block")
	}
	np := v1alpha1.NetworkProfile{AWS: &aws.Network{}}
	if _, ok := np.For("aws"); !ok {
		t.Error("NetworkProfile.For(aws) missing")
	}
	if _, ok := np.For("fake"); ok {
		t.Error("NetworkProfile.For(fake) reported a block")
	}
	pp := v1alpha1.PlacementProfile{Fake: &fake.Placement{}}
	if _, ok := pp.For("fake"); !ok {
		t.Error("PlacementProfile.For(fake) missing")
	}
	if _, ok := pp.For("qemu"); ok {
		t.Error("PlacementProfile.For(qemu) reported a block")
	}
}

func TestAccessorsReturnPlainNilForAbsentBlocks(t *testing.T) {
	if b, ok := (v1alpha1.VendorBlocks{}).Get("aws"); ok || b != nil {
		t.Errorf("Get on an absent block = %#v, %v; want nil, false", b, ok)
	}
	if b, ok := (v1alpha1.StorageProfile{}).For("qemu"); ok || b != nil {
		t.Errorf("StorageProfile.For = %#v, %v; want nil, false", b, ok)
	}
	if b, ok := (v1alpha1.NetworkProfile{}).For("fake"); ok || b != nil {
		t.Errorf("NetworkProfile.For = %#v, %v; want nil, false", b, ok)
	}
	if b, ok := (v1alpha1.PlacementProfile{}).For("aws"); ok || b != nil {
		t.Errorf("PlacementProfile.For = %#v, %v; want nil, false", b, ok)
	}
}
