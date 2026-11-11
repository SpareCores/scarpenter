/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

// knownPlans are plan names in UpCloud's naming and the family each belongs
// to: the test of "a new plan family must be classified". Examples, not a copy
// of the price list. The same check runs over the UpCloud plans of the committed
// Keeper snapshot (driver_test.go).
var knownPlans = map[string]Family{
	"1xCPU-1GB": FamilyGeneral, "1xCPU-2GB": FamilyGeneral, "2xCPU-4GB": FamilyGeneral, "20xCPU-128GB": FamilyGeneral,
	"DEV-1xCPU-1GB-10GB": FamilyDeveloper, "DEV-2xCPU-4GB-20GB": FamilyDeveloper,
	"STARTER-1xCPU-1GB": FamilyStarter, "PREMIUM-2xCPU-4GB": FamilyPremium,
	"HICPU-8xCPU-16GB": FamilyHiCPU, "HICPU-64xCPU-128GB": FamilyHiCPU,
	"HIMEM-2xCPU-16GB": FamilyHiMem, "HIMEM-32xCPU-256GB": FamilyHiMem,
	"CLOUDNATIVE-2xCPU-4GB": FamilyCloudNative,
	"GPU-8xCPU-64GB-1xL40S": FamilyGPU, "GPU-SPOT-8xCPU-64GB-1xL40S": FamilyGPU,
	"MYSTERY-4xCPU-8GB": FamilyUnknown, "": FamilyUnknown, "1xCPU": FamilyUnknown,
}

func TestFamilyOf(t *testing.T) {
	for name, want := range knownPlans {
		if got := FamilyOf(name); got != want {
			t.Errorf("%q: family %q, want %q", name, got, want)
		}
	}
}

func TestFamilySupported(t *testing.T) {
	for f, want := range map[Family]bool{
		FamilyGeneral: true, FamilyStarter: true, FamilyPremium: true, FamilyDeveloper: true, FamilyHiCPU: true, FamilyHiMem: true, FamilyCloudNative: true,
		FamilyGPU: false, FamilyUnknown: false,
	} {
		if f.Supported() != want {
			t.Errorf("%q supported = %v", f, f.Supported())
		}
	}
}

func TestFakeDefaultPlansAreClassified(t *testing.T) {
	ps, err := fake.New(fake.Options{}).GetPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range PlansOf(ps) {
		if p.Family == FamilyUnknown {
			t.Errorf("plan %s has no family", p.Name)
		}
	}
}

func TestPlanDefaults(t *testing.T) {
	plans := PlansOf(&upcloud.Plans{Plans: []upcloud.Plan{
		{Name: "DEV-1xCPU-1GB-10GB", CoreNumber: 1, MemoryAmount: 1024, StorageSize: 10, StorageTier: "standard"},
		{Name: "1xCPU-2GB", CoreNumber: 1, MemoryAmount: 2048, StorageSize: 50, StorageTier: "maxiops"},
		{Name: "CLOUDNATIVE-2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096},
		{Name: "GPU-8xCPU-64GB-1xL40S", CoreNumber: 8, MemoryAmount: 65536, StorageSize: 300, StorageTier: "maxiops", GPUAmount: 1},
	}})
	for name, tc := range map[string]struct {
		tier       string
		gb         int
		ipv4       bool
		supported  bool
		diskGiB    int64
		rootTierOK bool
	}{
		"DEV-1xCPU-1GB-10GB":    {"standard", 25, true, true, 25, true},
		"1xCPU-2GB":             {"maxiops", 50, true, true, 50, true},
		"CLOUDNATIVE-2xCPU-4GB": {"maxiops", 25, false, true, 25, true},
		"GPU-8xCPU-64GB-1xL40S": {"maxiops", 300, true, false, 300, true},
	} {
		p := plans[name]
		if p.DefaultRootTier() != tc.tier || p.DefaultRootGB() != tc.gb || p.HasIPv4() != tc.ipv4 ||
			p.Supported() != tc.supported || p.RootDiskBytes() != tc.diskGiB<<30 {
			t.Errorf("%s: tier %s gb %d ipv4 %v supported %v bytes %d", name, p.DefaultRootTier(), p.DefaultRootGB(),
				p.HasIPv4(), p.Supported(), p.RootDiskBytes())
		}
	}
	if p, err := plans.ValidatePlan("1xCPU-2GB"); err != nil || p.Cores != 1 || p.MemoryMB != 2048 {
		t.Errorf("ValidatePlan = %+v %v", p, err)
	}
	for _, bad := range []string{"GPU-8xCPU-64GB-1xL40S", "nope"} {
		if _, err := plans.ValidatePlan(bad); err == nil {
			t.Errorf("%s validated", bad)
		}
	}
}
