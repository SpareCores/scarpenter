/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"encoding/json"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

const gib = int64(1) << 30

// TestMinRootDiskBytes checks, per driver with a settable root disk, that the
// memory-derived minimum raises the size of the vendor block (in the driver's
// own unit, rounded up to GiB) and never lowers it, and that the drivers whose
// type fixes the root disk ignore it.
func TestMinRootDiskBytes(t *testing.T) {
	tests := []struct {
		name    string
		no      vendors.NodeOptions
		raw     string
		sizeGiB int64 // the size the vendor block sets
	}{
		{"aws", &aws.Driver{}, `{"storage":{"root":{"volumeType":"gp3","volumeSize":"128Gi"}}}`, 128},
		{"gcp", &gcp.Driver{}, `{"storage":{"root":{"sizeGb":128}}}`, 128},
		{"oci", &oci.Driver{}, `{"storage":{"root":{"sizeGb":128}}}`, 128},
		{"azure", &azure.Driver{}, `{"storage":{"root":{"diskSizeGB":128}}}`, 128},
		{"alicloud", &alicloud.Driver{}, `{"storage":{"root":{"size":"128Gi"}}}`, 128},
		{"upcloud", &upcloud.Driver{}, `{"storage":{"root":{"size":"128Gi"}}}`, 128},
		{"ovh", &ovh.Driver{}, `{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"128Gi"}}}}`, 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := func(minBytes int64) int64 {
				return tt.no.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(tt.raw), MinRootDiskBytes: minBytes})
			}
			if got := at(0); got != tt.sizeGiB*gib {
				t.Errorf("without a minimum = %d GiB, want %d", got/gib, tt.sizeGiB)
			}
			if got := at(64 * gib); got != tt.sizeGiB*gib {
				t.Errorf("a minimum below the size = %d GiB, want the size %d", got/gib, tt.sizeGiB)
			}
			if got := at(1024 * gib); got != 1024*gib {
				t.Errorf("a minimum above the size = %d GiB, want 1024", got/gib)
			}
			if got := at(1024*gib + 1); got != 1025*gib {
				t.Errorf("a minimum of 1 TiB and a byte = %d GiB, want 1025 (rounded up)", got/gib)
			}
		})
	}
}

// TestMinRootDiskBytesIgnoredWhereTheTypeFixesTheDisk covers Hetzner and Vultr.
func TestMinRootDiskBytesIgnoredWhereTheTypeFixesTheDisk(t *testing.T) {
	for name, no := range map[string]vendors.NodeOptions{"hcloud": &hcloud.Driver{}, "vultr": &vultr.Driver{}} {
		s := no.Defaults()
		s.MinRootDiskBytes = 1024 * gib
		if got := no.RootDiskBytes(s); got != 0 {
			t.Errorf("%s: root disk = %d, want 0 (the type's)", name, got)
		}
	}
}

// TestMinRootDiskBytesStopsAtTheVendorMaximum: a driver that knows its maximum
// stops there; the others take the full size.
func TestMinRootDiskBytesStopsAtTheVendorMaximum(t *testing.T) {
	tests := []struct {
		name    string
		no      vendors.NodeOptions
		raw     string
		wantGiB int64
	}{
		{"oci", &oci.Driver{}, `{"storage":{"root":{"sizeGb":128}}}`, oci.MaxRootGB},
		{"azure", &azure.Driver{}, `{"storage":{"root":{"diskSizeGB":128}}}`, 4095},
		{"alicloud", &alicloud.Driver{}, `{"storage":{"root":{"size":"128Gi"}}}`, 2048},
		{"aws", &aws.Driver{}, `{"storage":{"root":{"volumeType":"gp2","volumeSize":"128Gi"}}}`, 16384},
		{"gcp", &gcp.Driver{}, `{"storage":{"root":{"sizeGb":128}}}`, 65536},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := vendors.VendorSettings{Raw: json.RawMessage(tt.raw), MinRootDiskBytes: 1 << 20 * gib}
			if got := tt.no.RootDiskBytes(s); got != tt.wantGiB*gib {
				t.Errorf("root disk = %d GiB, want %d", got/gib, tt.wantGiB)
			}
		})
	}
}
