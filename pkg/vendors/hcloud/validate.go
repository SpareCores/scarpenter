/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

var (
	// locationPattern is the shape of a location name (fsn1, nbg1, hel1, ash,
	// hil, sin); whether the project has it is the API's answer at launch.
	locationPattern = regexp.MustCompile(`^[a-z]{3}[0-9]?$`)
	// idPattern is a Hetzner object ID.
	idPattern = regexp.MustCompile(`^[0-9]{1,19}$`)
)

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// hcloud block and of the hcloud blocks of the profiles, and the rules that
// refuse silently degraded behaviour (ADR-017).
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.HCloud; b != nil {
		errs = append(errs, d.validateBlock(nc, b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.HCloud != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".hcloud", p.HCloud)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if p.HCloud != nil {
			errs = append(errs, validatePlacement("profiles.placement."+name+".hcloud", p.HCloud)...)
		}
	}
	if nc.Spec.Vendors.HCloud != nil {
		for _, k := range slices.Sorted(maps.Keys(nc.Spec.Tags)) {
			if strings.HasPrefix(k, "scarpenter.sparecores.com/") {
				errs = append(errs, fmt.Errorf("tags: the key %q is reserved for SCarpenter", k))
			}
		}
		if _, err := Labels(nil, nc.Spec.Tags); err != nil {
			errs = append(errs, fmt.Errorf("tags: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (d *Driver) validateBlock(nc *v1alpha1.SCNodeClass, b *hcapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !locationPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.hcloud.regions: %q is not a Hetzner location "+
				"(fsn1, nbg1, hel1, ash, hil, sin)", r))
		}
	}
	if k := b.SSHKey; k != "" && !validSSHKeyName(k) {
		errs = append(errs, fmt.Errorf("vendors.hcloud.sshKey: %q is not a key name or ID (no leading or trailing "+
			"whitespace, no control characters)", k))
	}
	errs = append(errs, validateStorage("vendors.hcloud.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.hcloud.networking.inbound", b.Networking.Inbound)...)
	}
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	if !boolOr(net.PublicIPv4, true) && !boolOr(net.PublicIPv6, false) {
		errs = append(errs, fmt.Errorf("networking: a server needs a public IPv4 or IPv6 address; private-only servers "+
			"are not supported in v1 (they need a NAT route in your network to reach the sysext hosts, and the "+
			"metadata service Ignition reads is not verified on them)"))
	}
	if !boolOr(net.PublicIPv4, true) && boolOr(net.PublicIPv6, false) && d.log.GetSink() != nil {
		d.log.Info("an IPv6-only NodeClass: github.com and its release hosts have no IPv6 address, so the default "+
			"sysext base does not work without a mirror", "nodeclass", nc.Name)
	}
	if r := b.Resources; r != nil {
		caps := (&Driver{}).Selectors()
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images)...)
		errs = append(errs, validateResource(KindNetworks, caps[KindNetworks], r.Networks)...)
		errs = append(errs, validateResource(KindFirewalls, caps[KindFirewalls], r.Firewalls)...)
		errs = append(errs, validateResource(KindPlacementGroups, caps[KindPlacementGroups], r.PlacementGroups)...)
		if n := r.Networks; n != nil {
			if n.Create != nil {
				errs = append(errs, errors.New("vendors.hcloud.resources.networks.create is not supported in v1: "+
					"bring the network with use (creating one needs an IP range that does not collide with yours)"))
			}
			// Hetzner firewalls filter only the public interface, so a shared private network
			// lets every attached node reach every other whatever the level: the NodeClass must
			// say it knows (open question 3 of plan 15). A network found by the opt-in label
			// is checked at the launch.
			if n.Use != nil && net.Isolation != common.IsolationNodeClass && net.Isolation != common.IsolationNone {
				errs = append(errs, isolationError(net.Isolation))
			}
		}
	}
	return errs
}

func isolationError(have common.Isolation) error {
	if have == "" {
		have = common.IsolationRun
	}
	return fmt.Errorf("networking.isolation is %s: Hetzner firewalls filter only the public interface, so nodes "+
		"attached to a private network (vendors.hcloud.resources.networks.use, or a network with the opt-in label) "+
		"can reach each other at every level. Set networking.isolation (or vendors.hcloud.networking.isolation) to "+
		"nodeclass (and bring a network dedicated to the NodeClass) or none; run is not possible until SCarpenter "+
		"creates per-run networks", have)
}

func validateResource(kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.hcloud.resources." + string(kind) + ".use"
	errs := selector.Validate(Name, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !locationPattern.MatchString(region) && !strings.Contains(region, "{") {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a location (fsn1, nbg1, ..., or %q for every location)",
				path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if !strings.Contains(id, "{") && !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a Hetzner ID (a number)", path, region, id))
			}
		}
	}
	for k, v := range r.Use.Tags {
		if strings.Contains(k, "{") || strings.Contains(v, "{") {
			continue // checked after the placeholders are expanded
		}
		if !ValidLabelKey(k) {
			errs = append(errs, fmt.Errorf("%s.tags: %q is not a valid label key", path, k))
		}
		if v != "*" && (len(v) > maxLabelValue || !labelValueRE.MatchString(v)) {
			errs = append(errs, fmt.Errorf("%s.tags: the value %q of %q is not a valid label value (up to 63 "+
				"letters, digits, . _ -)", path, v, k))
		}
	}
	return errs
}

func validateInbound(path string, in *common.Inbound) []error {
	if in == nil {
		return nil
	}
	var errs []error
	for i, r := range in.Rules {
		for _, c := range r.CIDRs {
			if _, err := netip.ParsePrefix(c); err != nil {
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is not a CIDR", path, i, c))
			}
		}
		switch r.Protocol {
		case "tcp", "udp", "icmp", "any":
		default:
			errs = append(errs, fmt.Errorf("%s.rules[%d]: the protocol %q is not tcp, udp, icmp or any", path, i, r.Protocol))
		}
	}
	if _, err := FirewallRules(in.Rules); err != nil && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return errs
}

func validateStorage(path string, st *hcapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if len(st.Data) > MaxVolumes {
		errs = append(errs, fmt.Errorf("%s.data: %d volumes, a server takes at most %d", path, len(st.Data), MaxVolumes))
	}
	names, mounts := map[string]bool{}, map[string]bool{}
	for i, dv := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dv.Name] {
			errs = append(errs, fmt.Errorf("%s: the volume name %q is used twice", p, dv.Name))
		}
		names[dv.Name] = true
		if mounts[dv.MountPath] {
			errs = append(errs, fmt.Errorf("%s: the mount path %q is used twice", p, dv.MountPath))
		}
		mounts[dv.MountPath] = true
		if !strings.HasPrefix(dv.MountPath, "/") {
			errs = append(errs, fmt.Errorf("%s: the mount path %q is not absolute", p, dv.MountPath))
		}
		if n := sizeGB(dv.Size); n < MinVolumeGB || n > MaxVolumeGB {
			errs = append(errs, fmt.Errorf("%s: a Hetzner volume is %d to %d GB, not %d", p, MinVolumeGB, MaxVolumeGB, n))
		}
	}
	return errs
}

func validatePlacement(path string, p *hcapi.Placement) []error {
	if p == nil || p.PlacementGroup == nil {
		return nil
	}
	g := p.PlacementGroup
	if (g.Type == "") == (g.Use == nil) {
		return []error{fmt.Errorf("%s.placementGroup: set exactly one of type and use", path)}
	}
	return nil
}

// validSSHKeyName: a name or ID without leading or trailing whitespace and
// without control characters.
func validSSHKeyName(k string) bool {
	return strings.TrimSpace(k) == k && !strings.ContainsFunc(k, func(r rune) bool { return r < 0x20 })
}
