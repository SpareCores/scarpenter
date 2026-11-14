/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
)

type snapServer struct {
	VendorID     string  `json:"vendor_id"`
	APIReference string  `json:"api_reference"`
	Status       string  `json:"status"`
	Arch         string  `json:"cpu_architecture"`
	Accelerators float64 `json:"accelerator_count"`
	VCPUs        int64   `json:"vcpus"`
	MemoryMiB    int64   `json:"memory_amount"`
	StorageSize  float64 `json:"storage_size"`
	Storages     []struct {
		Size float64 `json:"size"`
	} `json:"storages"`
}

func ovhServers(t testing.TB) []snapServer {
	out := vendortest.SnapshotRows[snapServer](t, "server", "ovh")
	if len(out) == 0 {
		t.Fatal("the snapshot has no OVH servers")
	}
	return out
}

// retiredAtKeeper are the classified families the snapshot has no type of,
// because Keeper keeps their types as inactive rows.
var retiredAtKeeper = slices.Concat(legacyFamilies, []string{"g1", "g2", "g3"})

// Every Keeper OVH family of the committed snapshot is launched or excluded, a
// launched one is an active x86_64 type without accelerators, and everything the
// snapshot marks inactive or accelerated is excluded (plan 18: a test asserts
// every family is launched, excluded or mapped).
func TestEveryFamilyIsClassified(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range ovhServers(t) {
		fam := Family(s.APIReference)
		seen[fam] = true
		ok, reason := Launchable(s.APIReference)
		if !ok && reason == "" {
			t.Errorf("%s: family %q is neither launched nor excluded", s.APIReference, fam)
		}
		if ok && (s.Arch != "x86_64" || s.Accelerators > 0) {
			t.Errorf("%s is launched but is %s with %v accelerators", s.APIReference, s.Arch, s.Accelerators)
		}
		if ok && s.Status != "active" {
			t.Errorf("%s is launched but Keeper marks it %s", s.APIReference, s.Status)
		}
		if s.Accelerators > 0 && reason != ReasonGPU {
			t.Errorf("%s has accelerators and must be excluded as gpu, got %q", s.APIReference, reason)
		}
		if s.Status == "inactive" && ok {
			t.Errorf("inactive %s is launched", s.APIReference)
		}
	}
	for _, f := range slices.Concat(LaunchedFamilies(), ExcludedFamilies()) {
		// Keeper's snapshot has the active types only: the families it retired
		// stay classified, so that a running node of one still resolves.
		if retired := slices.Contains(retiredAtKeeper, f); retired == seen[f] {
			t.Errorf("family %s is classified, in the snapshot: %v, listed as retired at Keeper: %v (stale or unlisted)",
				f, seen[f], retired)
		}
	}
	if !slices.IsSorted(ExcludedFamilies()) {
		t.Error("ExcludedFamilies is sorted")
	}
}

func TestFamilyAndFlex(t *testing.T) {
	for in, want := range map[string]string{"b3-8": "b3", "c3-16-flex": "c3", "t1-le-45": "t1", "d2-2": "d2", "vps": "vps"} {
		if got := Family(in); got != want {
			t.Errorf("Family(%s) = %s", in, got)
		}
	}
	// -flex variants are separate launched types.
	if ok, _ := Launchable("b3-8-flex"); !ok {
		t.Error("b3-8-flex is launched")
	}
	if ok, why := Launchable("bm-s1"); ok || why != ReasonMetal {
		t.Error("metal is excluded")
	}
	if ok, why := Launchable("zz9-1"); ok || why != "" {
		t.Error("an unknown family is neither")
	}
}

// The root disk is the type's storage_size, except i1, whose storage_size holds
// the local NVMe disks too.
func TestRootDiskFromKeeper(t *testing.T) {
	for _, s := range ovhServers(t) {
		if ok, _ := Launchable(s.APIReference); !ok {
			continue
		}
		var storages []float64
		for _, st := range s.Storages {
			storages = append(storages, st.Size)
		}
		got := RootDiskGB(s.APIReference, s.StorageSize, storages)
		want := s.StorageSize
		if strings.HasPrefix(s.APIReference, "i1-") {
			want = 50
		}
		if got != want {
			t.Errorf("%s: root disk %v GB, want %v", s.APIReference, got, want)
		}
		if got < 25 || got > 400 {
			t.Errorf("%s: root disk %v GB outside 25..400", s.APIReference, got)
		}
	}
	if RootDiskGB("i1-45", 1950, nil) != 1950 {
		t.Error("no storages list: storage_size")
	}
}
