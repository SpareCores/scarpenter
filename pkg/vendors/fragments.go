/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"

// VendorBootFragments is the optional capability of a driver whose vendor's
// image or network needs configuration on every node (a DNS setting, a udev
// rule). The core merges the fragments into the node's user-data whatever the
// NodeClass says; the fragments of the driver come before the metadata
// restriction.
type VendorBootFragments interface {
	// BootFragments returns the fragments; tailnet is set when the node's
	// network template is tailscale.
	BootFragments(tailnet bool) []BootFragment
}

// BootFragmentsOf returns the fragments the core builds for a node of the
// driver: the driver's own (tailnet is set when the network template is
// tailscale), then the metadata restriction (Restricted or HostOnly, not Open)
// when the driver declares metadata addresses, and the interruption agent
// when spot is set (a spot launch) and the driver has a notice check. The agent
// goes into the restriction's fragment when there is one: one part of the
// user-data saves a gzip header and a base64 wrapper.
func BootFragmentsOf(d Driver, access v1alpha1.InstanceMetadataAccess, spot, tailnet bool) ([]BootFragment, error) {
	var out []BootFragment
	if vf, ok := d.(VendorBootFragments); ok {
		out = append(out, vf.BootFragments(tailnet)...)
	}
	restricted := false
	hostOnly := access == v1alpha1.InstanceMetadataHostOnly
	if access != v1alpha1.InstanceMetadataOpen {
		_, ok, err := restrictWith(d, agentParts{}, hostOnly)
		if err != nil {
			return nil, err
		}
		restricted = ok
	}
	var agent agentParts
	haveAgent := false
	if spot {
		var err error
		if agent, haveAgent, err = agentPartsOf(d, restricted); err != nil {
			return nil, err
		}
	}
	if restricted {
		f, _, err := restrictWith(d, agent, hostOnly)
		if err != nil {
			return nil, err
		}
		return append(out, f), nil
	}
	if haveAgent {
		f, _, err := InterruptionAgent(d, false)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
