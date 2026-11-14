/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The built-in defaults of the root volume (plan 25, section 8): OCI's minimum is
// 50 GB and the Flatcar image is about 46.6 GiB.
const (
	DefaultRootGB          = 64
	DefaultRootPerformance = PerformanceBalanced
	MinRootGB              = 50
	MaxRootGB              = 32768
	giB                    = int64(1) << 30
)

// The performance levels of the boot volume, Keeper's storage IDs; the level
// sets the volume performance units per GB. Keeper's fourth level, lower-cost (0
// VPUs), is for block volumes: a boot volume takes 10 VPUs per GB at least
// (bootVolumeVpusPerGB of the launch).
const (
	PerformanceBalanced        = "balanced"
	PerformanceHigher          = "higher-performance"
	PerformanceUltraHigh       = "ultra-high-performance"
	defaultUltraVPUs     int64 = 30
)

// Metadata keys the driver owns; the NodeClass's metadata may not set them.
const (
	MetaUserData      = "user_data"
	MetaSSHAuthorized = "ssh_authorized_keys"
)

// ReservedMetadataKeys are the metadata keys a NodeClass may not set: user_data
// is the Ignition, and the nodes carry no SSH keys (Ignition installs keys).
var ReservedMetadataKeys = []string{MetaUserData, MetaSSHAuthorized}

// MaxMetadataBytes is OCI's limit for the metadata and extended metadata of an
// instance together; the launch checks the total of every value, base64
// user_data included, before the call.
const MaxMetadataBytes = 32000

// MaxUserData is the largest user-data the driver accepts: SCarpenter's 16 KiB
// budget for the rendered Ignition. Its base64 (about 21.8 KB) and the block's
// metadata must still fit MaxMetadataBytes, which the launch checks.
const MaxUserData = 16 * 1024

// maxNSGs is the number of network security groups OCI allows on one VNIC.
const maxNSGs = 5

// settings is the part of the resolved vendor block the launch reads (the
// storage and metadata of pkg/apis/v1alpha1/oci, with the driver's defaults).
type settings struct {
	Storage struct {
		Root RootVolume `json:"root"`
	} `json:"storage"`
	Metadata map[string]string `json:"metadata"`
}

// RootVolume is storage.root of the vendor block.
type RootVolume struct {
	SizeGb      int64  `json:"sizeGb"`
	Performance string `json:"performance"`
	VpusPerGb   int64  `json:"vpusPerGb"`
	KmsKeyID    string `json:"kmsKeyId"`
}

// decodeSettings reads the merged vendor block of a launch; fields it does not
// know are an error (ADR-017), and what it leaves out gets the defaults.
func decodeSettings(s vendors.VendorSettings) (settings, error) {
	var st settings
	if len(bytes.TrimSpace(s.Raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(s.Raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&st); err != nil {
			return settings{}, fmt.Errorf("decoding the vendor settings: %w", err)
		}
	}
	r := &st.Storage.Root
	if r.SizeGb == 0 {
		r.SizeGb = DefaultRootGB
	}
	if r.Performance == "" {
		r.Performance = DefaultRootPerformance
	}
	if r.SizeGb < MinRootGB || r.SizeGb > MaxRootGB {
		return settings{}, fmt.Errorf("storage.root.sizeGb is %d; OCI boot volumes are %d to %d GB",
			r.SizeGb, MinRootGB, MaxRootGB)
	}
	if _, err := r.vpus(); err != nil {
		return settings{}, err
	}
	for k := range st.Metadata {
		if slices.Contains(ReservedMetadataKeys, k) {
			return settings{}, fmt.Errorf("metadata key %q is reserved", k)
		}
	}
	return st, nil
}

// vpus is the volume performance units per GB of the root volume: 10 and 20 for
// balanced and higher-performance, vpusPerGb (30 to 120 in steps of 10, default
// 30) for ultra-high-performance.
func (r RootVolume) vpus() (int64, error) {
	switch r.Performance {
	case PerformanceBalanced:
		return 10, r.noVPUs()
	case PerformanceHigher:
		return 20, r.noVPUs()
	case PerformanceUltraHigh:
		v := r.VpusPerGb
		if v == 0 {
			v = defaultUltraVPUs
		}
		if v < 30 || v > 120 || v%10 != 0 {
			return 0, fmt.Errorf("storage.root.vpusPerGb is %d; ultra-high-performance takes 30 to 120 in steps of 10", v)
		}
		return v, nil
	}
	return 0, fmt.Errorf("storage.root.performance %q is not one of %s, %s, %s (a boot volume has no lower-cost level)",
		r.Performance, PerformanceBalanced, PerformanceHigher, PerformanceUltraHigh)
}

func (r RootVolume) noVPUs() error {
	if r.VpusPerGb != 0 {
		return fmt.Errorf("storage.root.vpusPerGb is for %s only", PerformanceUltraHigh)
	}
	return nil
}
