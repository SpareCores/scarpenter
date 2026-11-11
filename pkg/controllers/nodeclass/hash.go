/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package nodeclass is the NodeClass controller (controller.go): it resolves
// the bootstrap artifacts, checks the NodeClass and each vendor's part of it,
// and publishes the conditions and the per-vendor drift hash (hash.go,
// overview sections 6.2 and 6.3) in the status.
package nodeclass

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// HashVersion is bumped when the rendering of a node changes in a
// node-affecting way, so that every node drifts once (overview section 6.3).
const HashVersion = "1"

// Inputs are the resolved things the drift hash covers beyond the spec.
type Inputs struct {
	// Resolved are the artifacts of status.resolved.
	Resolved *v1alpha1.ResolvedArtifacts
	// CustomTemplate and UserButane are the contents of the referenced
	// ConfigMaps (network.custom and butane).
	CustomTemplate string
	UserButane     string
	// AuthKeyVersion is the resourceVersion of the tailscale auth key Secret.
	AuthKeyVersion string
	// Hardening are the hardenings of spec.bootstrap.hardening that the vendor's
	// driver offers (vendors.OfferedHardenings): only those change its nodes'
	// user-data, so a hardening a vendor ignores does not drift its nodes.
	Hardening []string
}

// Hash is the drift hash of the NodeClass for one vendor: SHA-256 over the
// canonical JSON of the shared fields that change what a node is (flatcar,
// kubernetes, network, networking, kubelet, butane, sysexts, poweroffAfter,
// tags), the hardenings the vendor applies, that vendor's block without regions
// and resources, and HashVersion.
// Changing another vendor's block never changes it; neither do regions,
// resource references, metrics or selections. Besides the spec it covers what
// the spec points at (Inputs): the contents of the custom template and the
// Butane ConfigMaps, the resourceVersion of the auth key Secret (never the key)
// and the resolved image digests, so that a re-published release drifts nodes.
func Hash(nc *v1alpha1.SCNodeClass, vendor string, in Inputs) (string, error) {
	block, _ := nc.Spec.Vendors.Get(vendor)
	rawBlock, err := json.Marshal(block)
	if err != nil {
		return "", fmt.Errorf("hashing the %s block: %w", vendor, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(rawBlock, &generic); err != nil {
		return "", fmt.Errorf("hashing the %s block: %w", vendor, err)
	}
	delete(generic, "regions")
	delete(generic, "resources")

	s := &nc.Spec
	// The hardenings the vendor applies change the user-data, so they drift its
	// nodes; they are left out when none applies, so that a NodeClass without
	// them (or a vendor that offers none of them) keeps its hash. The list is a
	// set: its order does not matter.
	var hardening []string
	if len(in.Hardening) > 0 {
		hardening = slices.Sorted(slices.Values(in.Hardening))
	}
	raw, err := json.Marshal(struct {
		Version       string
		Vendor        string
		Flatcar       any
		Kubernetes    any
		Network       any
		Networking    any
		Kubelet       any
		Butane        any
		Sysexts       any
		PoweroffAfter any
		Tags          any
		Hardening     []string `json:",omitempty"`
		Block         map[string]any
		Inputs        hashInputs
	}{HashVersion, vendor, s.Flatcar, s.Kubernetes, s.Network, s.Networking, s.Kubelet, s.Butane, s.Sysexts,
		s.PoweroffAfter, s.Tags, hardening, generic, newHashInputs(in)})
	if err != nil {
		return "", fmt.Errorf("hashing the NodeClass: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// hashInputs is Inputs as the hash sees it: of the resolved artifacts only the
// digests, the identity of the content. Switching to a mirror of the same
// release (--sysext-base-url) changes URLs, not nodes.
type hashInputs struct {
	Digests        map[string]map[string]string
	CustomTemplate string
	UserButane     string
	AuthKeyVersion string
}

func newHashInputs(in Inputs) hashInputs {
	h := hashInputs{CustomTemplate: in.CustomTemplate, UserButane: in.UserButane, AuthKeyVersion: in.AuthKeyVersion}
	if r := in.Resolved; r != nil {
		h.Digests = map[string]map[string]string{"kubernetes": {}, "tailscale": {}}
		for arch, a := range r.Kubernetes {
			h.Digests["kubernetes"][arch] = a.SHA256
		}
		for arch, a := range r.Tailscale {
			h.Digests["tailscale"][arch] = a.SHA256
		}
	}
	return h
}
