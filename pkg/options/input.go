/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Input is what an SCNodeClass says about the nodes of one vendor (layers 2
// and 3 of 00a section 3), as plain data: the catalog and the CloudProvider
// resolve settings from it without a Kubernetes client.
type Input struct {
	// Block is the vendor block without regions, resources and networking
	// (JSON of the vendor package's Settings).
	Block json.RawMessage
	// Resources is the vendor block's resources member, as written; it is not
	// part of Block (it is not node configuration: it does not drift nodes).
	Resources json.RawMessage
	// Networking is spec.networking with the vendor block's override applied.
	Networking common.Networking
	// Profiles are the vendor's blocks of the profiles by kind and name; a
	// profile without a block for the vendor is absent (ADR-017).
	Profiles map[string]map[string]json.RawMessage
}

// ProfileNames returns the profile names by kind in the shape the catalog
// uses, sorted.
func (in Input) ProfileNames() labels.Profiles {
	out := labels.Profiles{}
	for kind, byName := range in.Profiles {
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		slices.Sort(names)
		if len(names) > 0 {
			out[labels.ProfileKind(kind)] = names
		}
	}
	return out
}

// InputFromNodeClass extracts the vendor's Input; ok is false when the
// NodeClass has no block for the vendor.
func InputFromNodeClass(nc *v1alpha1.SCNodeClass, vendor string) (in Input, ok bool, err error) {
	block, ok := nc.Spec.Vendors.Get(vendor)
	if !ok {
		return Input{}, false, nil
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return Input{}, true, fmt.Errorf("encoding the %s block: %w", vendor, err)
	}
	m, err := decode(raw)
	if err != nil {
		return Input{}, true, err
	}
	var override *common.NetworkingOverride
	if o, has := m["networking"]; has {
		ob, err := json.Marshal(o)
		if err != nil {
			return Input{}, true, fmt.Errorf("encoding the %s networking override: %w", vendor, err)
		}
		override = &common.NetworkingOverride{}
		if err := json.Unmarshal(ob, override); err != nil {
			return Input{}, true, fmt.Errorf("decoding the %s networking override: %w", vendor, err)
		}
	}
	if r, has := m["resources"]; has {
		if in.Resources, err = json.Marshal(r); err != nil {
			return Input{}, true, fmt.Errorf("encoding the %s resources: %w", vendor, err)
		}
	}
	delete(m, "regions")
	delete(m, "resources")
	delete(m, "networking")
	if in.Block, err = json.Marshal(m); err != nil {
		return Input{}, true, fmt.Errorf("encoding the %s block: %w", vendor, err)
	}
	in.Networking = ResolveNetworking(nc.Spec.Networking, override)

	in.Profiles = map[string]map[string]json.RawMessage{}
	add := func(kind, name string, p interface{ For(string) (any, bool) }) error {
		b, has := p.For(vendor)
		if !has {
			return nil
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("encoding the %s profile %s: %w", kind, name, err)
		}
		if in.Profiles[kind] == nil {
			in.Profiles[kind] = map[string]json.RawMessage{}
		}
		in.Profiles[kind][name] = raw
		return nil
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if err := add(vendors.ProfileStorage, name, p); err != nil {
			return Input{}, true, err
		}
	}
	for name, p := range nc.Spec.Profiles.Network {
		if err := add(vendors.ProfileNetwork, name, p); err != nil {
			return Input{}, true, err
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if err := add(vendors.ProfilePlacement, name, p); err != nil {
			return Input{}, true, err
		}
	}
	return in, true, nil
}

// ResolveNetworking applies the vendor's override to the generic networking
// and the API defaults (public IPv4 on, IPv6 off, isolation run) to what is
// unset.
func ResolveNetworking(base common.Networking, override *common.NetworkingOverride) common.Networking {
	out := base
	if out.PublicIPv4 == nil {
		out.PublicIPv4 = new(true)
	}
	if out.PublicIPv6 == nil {
		out.PublicIPv6 = new(false)
	}
	if out.Isolation == "" {
		out.Isolation = common.IsolationRun
	}
	if override == nil {
		return out
	}
	if override.PublicIPv4 != nil {
		out.PublicIPv4 = override.PublicIPv4
	}
	if override.PublicIPv6 != nil {
		out.PublicIPv6 = override.PublicIPv6
	}
	if override.Inbound != nil {
		out.Inbound = override.Inbound
	}
	if override.Isolation != "" {
		out.Isolation = override.Isolation
	}
	return out
}

// ResourceConfig returns how the vendor's resource kinds are provided
// (spec.vendors.<vendor>.resources), by kind; kinds the NodeClass says nothing
// about are absent.
func ResourceConfig(nc *v1alpha1.SCNodeClass, vendor string) (map[string]common.Resource, error) {
	block, ok := nc.Spec.Vendors.Get(vendor)
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return nil, fmt.Errorf("encoding the %s block: %w", vendor, err)
	}
	var v struct {
		Resources map[string]common.Resource `json:"resources"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decoding the %s resources: %w", vendor, err)
	}
	return v.Resources, nil
}

// ProfileHash is the drift hash of one profile's block for a vendor: changing
// the profile drifts only the nodes that use it (overview section 6.3).
func ProfileHash(block json.RawMessage) string {
	m, err := decode(block)
	if err != nil {
		m = map[string]any{"invalid": string(block)}
	}
	canonical, _ := json.Marshal(m) // map keys are sorted
	sum := sha256.Sum256(append([]byte("profile-v1:"), canonical...))
	return hex.EncodeToString(sum[:])[:16]
}

// ProfileKey is the key of a profile in the hashes recorded on a NodeClaim.
func ProfileKey(kind, name string) string { return kind + "/" + name }

// AppliedHashes returns the hashes of the non-default profiles of the
// selection, by ProfileKey: what a NodeClaim records at launch.
func (in Input) AppliedHashes(sel Selection) map[string]string {
	out := map[string]string{}
	for _, kind := range kinds {
		name := sel.Name(kind)
		if name == vendors.DefaultProfile {
			continue
		}
		if b, ok := in.Profiles[kind][name]; ok {
			out[ProfileKey(kind, name)] = ProfileHash(b)
		}
	}
	return out
}

// CurrentHash returns the current hash of the profile named by a ProfileKey;
// ok is false when the profile does not exist (any more) for the vendor.
func (in Input) CurrentHash(key string) (hash string, ok bool) {
	for _, kind := range kinds {
		for name, b := range in.Profiles[kind] {
			if ProfileKey(kind, name) == key {
				return ProfileHash(b), true
			}
		}
	}
	return "", false
}
