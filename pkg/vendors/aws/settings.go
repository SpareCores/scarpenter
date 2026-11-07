/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The built-in defaults (00a section 5.3, layer 1): a gp3 root volume of 64 GiB,
// encrypted with the account's default key, and IMDSv2 with a hop limit of 1,
// which keeps the metadata service out of reach of Pods without host networking
// (overview section 8.6).
const (
	defaultVolumeType = awsapi.VolumeTypeGP3
	defaultRootGiB    = 64
	defaultHopLimit   = 1
	// DefaultImageOwner is the official Flatcar account.
	DefaultImageOwner = "075585003325"

	giB = int64(1) << 30
)

const defaultSettings = `{"storage":{"root":{"volumeType":"gp3","volumeSize":"64Gi","encrypted":true}},` +
	`"metadataOptions":{"httpTokens":"required","httpPutResponseHopLimit":1}}`

// volume is a resolved EBS volume.
type volume struct {
	Type       awsapi.VolumeType
	GiB        int32
	IOPS       *int32
	Throughput *int32
	Encrypted  bool
	KMSKeyID   string
}

// gib converts a quantity to whole GiB, rounding up.
func gib(q *resource.Quantity) int32 {
	if q == nil || q.IsZero() {
		return 0
	}
	return int32((q.Value() + giB - 1) / giB)
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// decodeSettings decodes the merged vendor block (defaults, NodeClass block and
// the selected storage profile).
func decodeSettings(s vendors.VendorSettings) (awsapi.Settings, error) {
	var out awsapi.Settings
	if len(s.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(s.Raw, &out); err != nil {
		return out, fmt.Errorf("decoding the AWS settings: %w", err)
	}
	return out, nil
}

// rootVolume returns the resolved root volume of the settings.
func rootVolume(st awsapi.Settings) volume {
	v := volume{Type: defaultVolumeType, GiB: defaultRootGiB, Encrypted: true}
	r := st.Storage
	if r == nil || r.Root == nil {
		return v
	}
	if r.Root.VolumeType != "" {
		v.Type = r.Root.VolumeType
	}
	if g := gib(r.Root.VolumeSize); g > 0 {
		v.GiB = g
	}
	v.IOPS, v.Throughput = r.Root.IOPS, r.Root.Throughput
	v.Encrypted = boolOr(r.Root.Encrypted, true)
	v.KMSKeyID = r.Root.KMSKeyID
	return v
}

// dataVolume returns the resolved extra volume.
func dataVolume(d awsapi.DataVolume) volume {
	return volume{
		Type: d.VolumeType, GiB: gib(&d.VolumeSize), IOPS: d.IOPS, Throughput: d.Throughput,
		Encrypted: boolOr(d.Encrypted, true), KMSKeyID: d.KMSKeyID,
	}
}

// dataVolumes returns the extra volumes in the order of the profile.
func dataVolumes(st awsapi.Settings) []awsapi.DataVolume {
	if st.Storage == nil {
		return nil
	}
	return st.Storage.Data
}

// localStorage returns the local storage policy block, nil without one.
func localStorage(st awsapi.Settings) (policy, mountPath string, ok bool) {
	if st.Storage == nil || st.Storage.LocalStorage == nil {
		return "", "", false
	}
	ls := st.Storage.LocalStorage
	return string(ls.Policy), ls.MountPath, true
}

// metadata returns the resolved IMDS options.
func metadata(st awsapi.Settings) (tokens awsapi.HTTPTokens, hops int32) {
	tokens, hops = "required", defaultHopLimit
	if m := st.MetadataOptions; m != nil {
		if m.HTTPTokens != "" {
			tokens = m.HTTPTokens
		}
		if m.HTTPPutResponseHopLimit != nil {
			hops = *m.HTTPPutResponseHopLimit
		}
	}
	return tokens, hops
}

func decodePlacement(raw json.RawMessage) (awsapi.Placement, error) {
	var p awsapi.Placement
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

func decodeNetwork(raw json.RawMessage) (awsapi.Network, error) {
	var n awsapi.Network
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n); err != nil {
			return n, fmt.Errorf("decoding the network profile: %w", err)
		}
	}
	return n, nil
}

func decodeStorage(raw json.RawMessage) (awsapi.Storage, error) {
	var s awsapi.Storage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the size of the root volume,
// which the scheduler assumes as ephemeral storage.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := decodeSettings(s)
	if err != nil {
		return int64(defaultRootGiB) * giB
	}
	return int64(rootVolume(st).GiB) * giB
}
