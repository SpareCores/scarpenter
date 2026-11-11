/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The decimal gigabyte Hetzner counts volumes and disks in.
const gb = int64(1_000_000_000)

// defaultSettings are the driver's built-in settings (layer 1 of 00a section
// 3): nothing to set. The root disk is the server type's, public IPv4 is on and
// IPv6 off (the generic networking defaults), there is no network, a managed
// deny-inbound firewall is created and there is no placement group.
const defaultSettings = `{}`

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// sizeGB is the size of a quantity in whole GB, rounded up.
func sizeGB(q resource.Quantity) int {
	if q.IsZero() {
		return 0
	}
	return int((q.Value() + gb - 1) / gb)
}

func decodeSettings(s vendors.VendorSettings) (hcapi.Settings, error) {
	var out hcapi.Settings
	if len(s.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(s.Raw, &out); err != nil {
		return out, fmt.Errorf("decoding the Hetzner settings: %w", err)
	}
	return out, nil
}

func decodePlacement(raw json.RawMessage) (hcapi.Placement, error) {
	var p hcapi.Placement
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

func decodeStorage(raw json.RawMessage) (hcapi.Storage, error) {
	var s hcapi.Storage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

func dataVolumes(st hcapi.Settings) []hcapi.DataVolume {
	if st.Storage == nil {
		return nil
	}
	return st.Storage.Data
}
