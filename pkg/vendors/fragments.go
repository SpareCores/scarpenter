/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

// VendorBootFragments is the optional capability of a driver whose vendor's
// image or network needs configuration on every node (a DNS setting, a udev
// rule). The core merges the fragments into the node's user-data whatever the
// NodeClass says; the fragments of the driver come before the metadata
// restriction.
type VendorBootFragments interface {
	BootFragments() []BootFragment
}

// BootFragmentsOf returns the fragments the core builds for a node of the
// driver: the driver's own, then the metadata restriction when restrictMetadata
// is set and the driver declares metadata addresses, and the interruption agent
// when spot is set (a spot launch) and the driver has a notice check. The agent
// goes into the restriction's fragment when there is one: one part of the
// user-data saves a gzip header and a base64 wrapper.
func BootFragmentsOf(d Driver, restrictMetadata, spot bool) ([]BootFragment, error) {
	var out []BootFragment
	if vf, ok := d.(VendorBootFragments); ok {
		out = append(out, vf.BootFragments()...)
	}
	restricted := false
	if restrictMetadata {
		_, ok, err := RestrictMetadata(d)
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
		f, _, err := restrictWith(d, agent)
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
