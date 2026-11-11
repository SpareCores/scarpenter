/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	ucapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The built-in defaults (00a section 5.3, layer 1): an encrypted root disk. The
// tier and the size of the root disk are not in the defaults: they depend on the
// plan (its included storage and tier) and on the template, like GCP's Hyperdisk
// series. This is the one type-dependent default of the driver, documented.
const defaultSettings = `{"storage":{"root":{"encrypted":true}}}`

const giB = int64(1) << 30

// gbOf converts a quantity to whole GB, rounding up. UpCloud's GB are read as
// GiB (the storage unit of the API; unverified, spike S4).
func gbOf(q *resource.Quantity) int {
	if q == nil || q.IsZero() {
		return 0
	}
	return int((q.Value() + giB - 1) / giB)
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// decodeSettings decodes the merged vendor block (defaults, NodeClass block and
// the selected storage profile).
func decodeSettings(s vendors.VendorSettings) (ucapi.Settings, error) {
	var out ucapi.Settings
	if len(s.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(s.Raw, &out); err != nil {
		return out, fmt.Errorf("decoding the UpCloud settings: %w", err)
	}
	return out, nil
}

func decodePlacement(raw json.RawMessage) (ucapi.Placement, error) {
	var p ucapi.Placement
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

func decodeStorage(raw json.RawMessage) (ucapi.Storage, error) {
	var s ucapi.Storage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

// rootSetting is what the settings say about the root disk: a tier and a size of
// 0 or "" mean "the default for the plan".
type rootSetting struct {
	Tier      string
	SizeGB    int
	Encrypted bool
}

func rootOf(st ucapi.Settings) rootSetting {
	r := rootSetting{Encrypted: true}
	if st.Storage == nil || st.Storage.Root == nil {
		return r
	}
	root := st.Storage.Root
	r.Tier = string(root.Tier)
	r.SizeGB = gbOf(root.Size)
	r.Encrypted = boolOr(root.Encrypted, true)
	return r
}

// dataOf returns the resolved extra volumes in the order of the profile.
func dataOf(st ucapi.Settings) []DataVolume {
	if st.Storage == nil {
		return nil
	}
	out := make([]DataVolume, 0, len(st.Storage.Data))
	for _, d := range st.Storage.Data {
		out = append(out, DataVolume{
			Name: d.Name, Tier: string(d.Tier), SizeGB: gbOf(&d.Size), Encrypted: boolOr(d.Encrypted, true),
		})
	}
	return out
}

// filesystemOf returns the filesystem of a volume, ext4 by default.
func filesystemOf(st ucapi.Settings, name string) string {
	if st.Storage != nil {
		for _, d := range st.Storage.Data {
			if d.Name == name && d.Filesystem != "" {
				return string(d.Filesystem)
			}
		}
	}
	return "ext4"
}
