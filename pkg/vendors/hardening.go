/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// BootFragment is a Butane config (variant flatcar, version 1.1.0) a driver
// contributes to the node's user-data. The core renders it like the user's own
// Butane (the template variables of the node bootstrap reference are available)
// and merges it after the base config and before the network template and the
// user's Butane, so those can still override it.
type BootFragment struct {
	// Name identifies the fragment in errors and in the user-data's parts.
	Name   string
	Butane string
}

// BootFragmentRequest describes the node a driver contributes fragments to.
type BootFragmentRequest struct {
	Region       string
	Zone         string
	InstanceType string
	Arch         string
	// Hardening are the hardenings the NodeClass enabled
	// (spec.bootstrap.hardening) and the driver offers: the core passes only the
	// names Hardenings lists.
	Hardening []string
}

// NodeHardener is the optional capability of a driver that contributes Butane
// fragments to the node's configuration (plan 14 A7). The core must not branch on
// a vendor, so what a vendor needs in the base config goes through here:
//
//   - the hardenings of spec.bootstrap.hardening (off by default, they change what
//     Pods on the node can reach), such as blocking Pod access to the vendor's
//     metadata address;
//   - fragments that are always on, because the vendor's image needs them (the
//     GCP arm64 image lacks the udev rules of /dev/disk/by-id/google-<name>).
type NodeHardener interface {
	// Hardenings lists the names of the opt-in hardenings the driver offers.
	Hardenings() []string
	// BootFragments returns the always-on fragments and those of the requested
	// hardenings, in a fixed order. The result must depend only on the request.
	BootFragments(req BootFragmentRequest) ([]BootFragment, error)
}

// HardeningMetadataFromPods is the name of the hardening that drops Pod traffic
// to the vendor's metadata address.
const HardeningMetadataFromPods = "metadata-from-pods"

// BlockMetadataFromPods is the fragment of the hardening metadata-from-pods for a
// vendor whose metadata address (169.254.169.254 on GCP, 100.100.100.200 on
// Alibaba) is reachable from Pods because it has nothing like AWS's hop limit.
// It drops the traffic that is forwarded to the address: the node's own units
// (Ignition, the kubelet, time sync) use the output path and are unaffected, and
// so are Pods in the host network, which are not forwarded. A CNI that bypasses
// netfilter for Pod traffic is not covered.
func BlockMetadataFromPods(address string) (BootFragment, error) {
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is4() {
		return BootFragment{}, fmt.Errorf("the metadata address %q is not an IPv4 address", address)
	}
	unit := strings.NewReplacer("ADDRESS", addr.String()).Replace(`variant: flatcar
version: 1.1.0
systemd:
  units:
    - name: scarpenter-block-metadata.service
      enabled: true
      contents: |
        [Unit]
        Description=Drop Pod traffic forwarded to the metadata address ADDRESS
        DefaultDependencies=no
        After=systemd-sysctl.service
        Before=network-pre.target kubelet.service shutdown.target
        Wants=network-pre.target
        Conflicts=shutdown.target
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        # Position 1: nothing a CNI or kube-proxy appends later can accept it first.
        ExecStart=/usr/sbin/iptables -w -I FORWARD 1 -d ADDRESS -j DROP
        ExecStop=/usr/sbin/iptables -w -D FORWARD -d ADDRESS -j DROP
        [Install]
        WantedBy=multi-user.target
`)
	return BootFragment{Name: "block-metadata-from-pods", Butane: unit}, nil
}

// OfferedHardenings returns, in the order of enabled, the hardenings the driver
// offers, and those it does not (a driver that is no NodeHardener offers none).
func OfferedHardenings(d Driver, enabled []string) (offered, unoffered []string) {
	h, ok := d.(NodeHardener)
	var have []string
	if ok {
		have = h.Hardenings()
	}
	for _, name := range enabled {
		if slices.Contains(have, name) {
			offered = append(offered, name)
		} else {
			unoffered = append(unoffered, name)
		}
	}
	return offered, unoffered
}

// BootFragmentsFor returns the boot fragments the driver contributes to a node:
// its always-on fragments and those of the hardenings in enabled that it offers
// (req.Hardening is set here). A driver that is no NodeHardener contributes none.
func BootFragmentsFor(d Driver, enabled []string, req BootFragmentRequest) ([]BootFragment, error) {
	h, ok := d.(NodeHardener)
	if !ok {
		return nil, nil
	}
	req.Hardening, _ = OfferedHardenings(d, enabled)
	out, err := h.BootFragments(req)
	if err != nil {
		return nil, fmt.Errorf("the boot fragments of %s: %w", d.Vendor(), err)
	}
	return out, nil
}
