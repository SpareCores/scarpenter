/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

func TestParseType(t *testing.T) {
	for _, tc := range []struct {
		name string
		want driver.InstanceType
	}{
		{"VM.Standard.E5.Flex.2-16", driver.InstanceType{Shape: "VM.Standard.E5.Flex", Sized: true, OCPUs: 2, GB: 16}},
		{"VM.Standard.A1.Flex.80-320", driver.InstanceType{Shape: "VM.Standard.A1.Flex", Sized: true, OCPUs: 80, GB: 320}},
		{"VM.Standard.E5.Flex.126-1008", driver.InstanceType{Shape: "VM.Standard.E5.Flex", Sized: true, OCPUs: 126, GB: 1008}},
		{"BM.Standard3.64", driver.InstanceType{Shape: "BM.Standard3.64"}},
		{"VM.Standard2.1", driver.InstanceType{Shape: "VM.Standard2.1"}},
		{"VM.Standard.E2.1.Micro", driver.InstanceType{Shape: "VM.Standard.E2.1.Micro"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := driver.ParseType(tc.name)
			if err != nil || got != tc.want {
				t.Fatalf("ParseType = %+v, %v; want %+v", got, err, tc.want)
			}
			if got.String() != tc.name {
				t.Errorf("String = %q, want %q", got.String(), tc.name)
			}
		})
	}
}

// The names that do not parse are rejected, never guessed (plan 25, section 2).
func TestParseTypeRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class vendors.Class
	}{
		{"VM.Standard.E5.Flex", vendors.Unsupported},
		{"VM.Standard.E5.Flex.2", vendors.Unsupported},
		{"VM.Standard.E5.Flex.2-16-3", vendors.Unsupported},
		{"VM.Standard.E5.Flex.0-4", vendors.Unsupported},
		{"VM.Standard.E5.Flex.02-16", vendors.Unsupported},
		{"VM.Standard.E5.Flex.2-0", vendors.Unsupported},
		{"VM.Standard.E5.Flex.1.5-8", vendors.Unsupported},
		{"VM.Standard.E5.Flex.2.0-16", vendors.Unsupported},
		{"VM.Standard.E5.Flex.2-16.5", vendors.Unsupported},
		{"e5.2-16", vendors.Fatal},
		{"oci.VM.Standard.E5.Flex.2-16", vendors.Fatal},
		{"", vendors.Fatal},
		{"VM.", vendors.Fatal},
		{"vm.Standard.E5.Flex.2-16", vendors.Fatal},
		{"VM.Standard.E5.Flex.2-16 ", vendors.Fatal},
		{"VM.Standard.E5.Flex.99999999999999999999-16", vendors.Unsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := driver.ParseType(tc.name)
			if !vendors.IsClass(err, tc.class) {
				t.Fatalf("ParseType(%q) = %+v, %v; want class %s", tc.name, got, err, tc.class)
			}
			if code := vendors.CodeOf(err); code != driver.CodeInvalidInstanceType {
				t.Errorf("code %q, want %q", code, driver.CodeInvalidInstanceType)
			}
		})
	}
}

func TestFormatType(t *testing.T) {
	for _, tc := range []struct {
		shape      string
		ocpus, gb  float64
		want       string
		roundTrips bool
	}{
		{"VM.Standard.E5.Flex", 2, 16, "VM.Standard.E5.Flex.2-16", true},
		{"VM.Standard.A1.Flex", 80, 320, "VM.Standard.A1.Flex.80-320", true},
		{"VM.Standard.E5.Flex", 1.5, 8, "VM.Standard.E5.Flex.1.5-8", false}, // foreign: %g, never ours
	} {
		got := driver.FormatType(tc.shape, tc.ocpus, tc.gb)
		if got != tc.want {
			t.Errorf("FormatType(%q, %g, %g) = %q, want %q", tc.shape, tc.ocpus, tc.gb, got, tc.want)
		}
		if _, err := driver.ParseType(got); (err == nil) != tc.roundTrips {
			t.Errorf("ParseType(%q) error = %v, round trip expected: %v", got, err, tc.roundTrips)
		}
	}
}

func TestVCPUsPerOCPU(t *testing.T) {
	for _, tc := range []struct {
		shape string
		n     int
		ok    bool
	}{
		{"VM.Standard.E5.Flex", 2, true},
		{"VM.Standard3.Flex", 2, true},
		{"VM.Optimized3.Flex", 2, true},
		{"VM.Standard2.4", 2, true},
		{"BM.Standard3.64", 2, true},
		{"VM.Standard.A1.Flex", 1, true},
		{"BM.Standard.A1.160", 1, true},
		{"VM.Standard.A2.Flex", 2, true},
		{"VM.Standard.A4.Flex", 2, true},
		{"BM.Standard.A4.Ax.48", 2, true},
		{"VM.Standard.A5.Flex", 0, false}, // an unknown Arm generation is never guessed
	} {
		n, ok := driver.VCPUsPerOCPU(tc.shape)
		if n != tc.n || ok != tc.ok {
			t.Errorf("VCPUsPerOCPU(%q) = %d, %v; want %d, %v", tc.shape, n, ok, tc.n, tc.ok)
		}
		if got := driver.Arm(tc.shape); got != strings.Contains(tc.shape, ".A") {
			t.Errorf("Arm(%q) = %v", tc.shape, got)
		}
	}
}

// Every row of the embedded catalog parses, and for sized rows the vCPUs are the
// OCPUs times the shape's k and the memory the GB times 1024 MiB (plan 25, section
// 2): a row that fails is a data error the test names.
func TestCatalogRowsParse(t *testing.T) {
	var sized, fixed int
	for _, r := range catalogRows(t) {
		typ, err := driver.ParseType(r.ID)
		if err != nil {
			t.Errorf("%s does not parse: %v", r.ID, err)
			continue
		}
		if typ.String() != r.ID {
			t.Errorf("%s: String = %q", r.ID, typ.String())
		}
		if r.Arm != driver.Arm(typ.Shape) {
			t.Errorf("%s: the row's architecture and the shape name disagree", r.ID)
		}
		if !typ.Sized {
			fixed++
			continue
		}
		sized++
		k, ok := driver.VCPUsPerOCPU(typ.Shape)
		if !ok {
			t.Errorf("%s: the vCPU table does not know the shape", r.ID)
			continue
		}
		if r.VCPUs != typ.OCPUs*k {
			t.Errorf("%s: %d vCPUs, want %d OCPUs x %d", r.ID, r.VCPUs, typ.OCPUs, k)
		}
		if r.MemoryMiB != float64(typ.GB*1024) {
			t.Errorf("%s: %v MiB, want %d GB", r.ID, r.MemoryMiB, typ.GB)
		}
	}
	// Plan 25, section 2: 287 active rows, 256 sized of 12 shapes, 31 fixed.
	if sized != 256 || fixed != 31 {
		t.Errorf("%d sized and %d fixed active rows; want 256 and 31", sized, fixed)
	}
}

// Every catalog type launches in the fake tenancy, whose shape tables the fake derives
// from the rows, except the extended-memory sizes of E5 (beyond 94 OCPUs), which v1
// does not launch: those fail before any launch call.
func TestCatalogTypesLaunch(t *testing.T) {
	e := newEnv(t)
	for i, r := range catalogRows(t) {
		req := request(r.ID)
		req.IdempotencyKey = fmt.Sprintf("uid-%d", i)
		req.Tags[vendors.TagNodeClaim] = fmt.Sprintf("claim-%d", i)
		req.Node.Name = req.Tags[vendors.TagNodeClaim]
		if r.Arm {
			req.Arch = "arm64"
		}
		before := e.c.Calls("LaunchInstance")
		inst, err := e.d.CreateInstance(t.Context(), req)
		typ, _ := driver.ParseType(r.ID)
		if typ.Shape == "VM.Standard.E5.Flex" && typ.OCPUs > 94 {
			if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeExtendedMemory {
				t.Errorf("%s: %v, want Unsupported %s", r.ID, err, driver.CodeExtendedMemory)
			}
			if e.c.Calls("LaunchInstance") != before {
				t.Errorf("%s: a launch call was made for an extended memory size", r.ID)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", r.ID, err)
			continue
		}
		if inst.Type != r.ID {
			t.Errorf("%s: the instance's type is %q", r.ID, inst.Type)
		}
	}
}
