/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package nodeclass holds what the SCNodeClass status needs. Plan 04 only has
// the per-vendor drift hash (overview section 6.3); the NodeClass controller of
// plan 05 grows around it.
package nodeclass

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// HashVersion is bumped when the rendering of a node changes in a
// node-affecting way, so that every node drifts once (overview section 6.3).
const HashVersion = "1"

// Hash is the drift hash of the NodeClass for one vendor: SHA-256 over the
// canonical JSON of the shared fields that change what a node is (flatcar,
// kubernetes, network, networking, kubelet, butane, sysexts, poweroffAfter,
// tags), that vendor's block without regions and resources, and HashVersion.
// Changing another vendor's block never changes it; neither do regions,
// resource references, metrics or selections.
//
// Not yet included, because plan 05 resolves them: the custom template and
// Butane content and the resourceVersion of the auth key Secret.
func Hash(nc *v1alpha1.SCNodeClass, vendor string) (string, error) {
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
		Block         map[string]any
	}{HashVersion, vendor, s.Flatcar, s.Kubernetes, s.Network, s.Networking, s.Kubelet, s.Butane, s.Sysexts,
		s.PoweroffAfter, s.Tags, generic})
	if err != nil {
		return "", fmt.Errorf("hashing the NodeClass: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
