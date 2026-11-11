/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

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
	ucapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/upcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// zonePattern is the shape of an UpCloud zone ID: fi-hel1, de-fra1, us-nyc1.
var zonePattern = regexp.MustCompile(`^[a-z]{2}-[a-z]{3}[0-9]$`)

func validateTier(path, tier string, hdd bool) []error {
	switch tier {
	case "", string(ucapi.TierMaxIOPS), string(ucapi.TierStandard):
		return nil
	case string(ucapi.TierHDD):
		if hdd {
			return nil
		}
	}
	return []error{fmt.Errorf("%s: %q is not a tier a root disk can have (maxiops or standard)", path, tier)}
}

// validateStorage validates a storage block (the vendor block's or a profile's)
// against the limits of UpCloud's storage: 10 to 1024 GB, the root and the data
// disks within the number of devices a server takes.
func validateStorage(path string, st *ucapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	sized := func(p string, gb int) {
		if gb < MinStorageGB || gb > MaxStorageGB {
			errs = append(errs, fmt.Errorf("%s: a disk is %d to %d GB, not %d", p, MinStorageGB, MaxStorageGB, gb))
		}
	}
	if r := st.Root; r != nil {
		errs = append(errs, validateTier(path+".root.tier", string(r.Tier), false)...)
		if gb := gbOf(r.Size); r.Size != nil {
			sized(path+".root.size", gb)
		}
	}
	if 1+len(st.Data) > MaxStorageDevices {
		errs = append(errs, fmt.Errorf("%s.data: %d disks and the root exceed the %d storage devices of a server",
			path, len(st.Data), MaxStorageDevices))
	}
	names := map[string]bool{}
	for i, v := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[v.Name] {
			errs = append(errs, fmt.Errorf("%s: the disk name %q is used twice", p, v.Name))
		}
		names[v.Name] = true
		sized(p+".size", gbOf(&v.Size))
	}
	return errs
}

func validatePlacement(path string, p *ucapi.Placement) []error {
	if p == nil || p.ServerGroup == nil {
		return []error{fmt.Errorf("%s.serverGroup is required", path)}
	}
	return nil
}

// validateResource checks a `use` block: the selector against the declaration of
// the kind (every field, match attribute and filter name), then what only UpCloud
// knows: zone keys, the shape of IDs and the label rules.
func validateResource(kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.upcloud.resources." + string(kind) + ".use"
	errs := selector.Validate(Vendor, kind, path, r.Use, caps)
	for zone, ids := range r.Use.IDs {
		if zone != selector.RegionAll && !zonePattern.MatchString(zone) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a zone (an UpCloud zone ID such as fi-hel1, or %q for every zone)",
				path, zone, selector.RegionAll))
		}
		for _, id := range ids {
			if !strings.Contains(id, "{") && !IsUUID(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not an UpCloud UUID", path, zone, id))
			}
		}
	}
	for k, v := range r.Use.Tags {
		if strings.Contains(k, "{") || strings.Contains(v, "{") {
			continue // checked after the placeholders are expanded
		}
		if err := ValidateLabelKey(k); err != nil {
			errs = append(errs, fmt.Errorf("%s.tags: %w", path, err))
		}
		if len(v) > MaxLabelValue {
			errs = append(errs, fmt.Errorf("%s.tags: the value of %q is longer than %d characters", path, k, MaxLabelValue))
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
	}
	if _, err := PublicFirewallRules(FirewallOptions{Inbound: inboundOf(in), Tailscale: true}); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return errs
}

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// upcloud block and of the UpCloud blocks of the profiles (zones, disk tiers,
// sizes, inbound rules, resource references, tags and labels, the Flatcar channel)
// so that a mistake shows in the NodeClass's status and not in a failed launch.
// What needs the account (that a referenced network exists) is checked by the
// resource status, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if nc.Spec.Vendors.UpCloud != nil && nc.Spec.Flatcar.Channel == "lts" {
		errs = append(errs, fmt.Errorf("flatcar.channel: lts is not supported on UpCloud: its Ignition predates "+
			"the upcloud platform (needs 2.23 or later); use stable, beta or alpha"))
	}
	if b := nc.Spec.Vendors.UpCloud; b != nil {
		errs = append(errs, validateBlock(b)...)
		errs = append(errs, validateEgress(nc, b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.UpCloud != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".upcloud", p.UpCloud)...)
		}
	}
	var override *common.NetworkingOverride
	if b := nc.Spec.Vendors.UpCloud; b != nil {
		override = b.Networking
	}
	run := isolationRun(options.ResolveNetworking(nc.Spec.Networking, override).Isolation)
	for _, name := range slices.Sorted(maps.Keys(nc.Spec.Profiles.Placement)) {
		p := nc.Spec.Profiles.Placement[name]
		if p.UpCloud == nil {
			continue
		}
		path := "profiles.placement." + name + ".upcloud"
		perr := validatePlacement(path, p.UpCloud)
		errs = append(errs, perr...)
		if run && len(perr) == 0 && needsRunKey(*p.UpCloud) {
			errs = append(errs, fmt.Errorf("%s: the server group is created per run key, and isolation run (the default) "+
				"needs one SDN network per run key, which UpCloud nodes do not get in this version, so the profile is not "+
				"offered; set spec.networking.isolation to nodeclass or none", path))
		}
	}
	if len(nc.Spec.Tags) > 0 {
		if err := ValidateExtraTags(nc.Spec.Tags); err != nil {
			errs = append(errs, fmt.Errorf("tags: %w", err))
		}
	}
	return errors.Join(errs...)
}

func validateBlock(b *ucapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !zonePattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.upcloud.regions: %q is not an UpCloud zone (for example fi-hel1)", r))
		}
	}
	errs = append(errs, validateStorage("vendors.upcloud.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.upcloud.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := (&Driver{}).Selectors()
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images)...)
		errs = append(errs, validateResource(KindNetworks, caps[KindNetworks], r.Networks)...)
		errs = append(errs, validateResource(KindFirewalls, caps[KindFirewalls], r.Firewalls)...)
		errs = append(errs, validateResource(KindServerGroups, caps[KindServerGroups], r.ServerGroups)...)
	}
	return errs
}

// validateEgress refuses nodes without a public IPv4 address where nothing gives
// them egress: the utility network has no internet access (unverified), so a node
// needs a private network whose gateway the user provides (a NAT gateway or a
// router) to fetch its sysexts and container images.
func validateEgress(nc *v1alpha1.SCNodeClass, b *ucapi.Settings) []error {
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	if boolOr(net.PublicIPv4, true) {
		return nil
	}
	if r := b.Resources; r != nil && r.Networks != nil && r.Networks.Use != nil {
		return nil
	}
	return []error{fmt.Errorf("networking.publicIPv4: false is refused: nodes without a public IPv4 address would have " +
		"no egress for sysexts and container images, because the utility network has no internet access. " +
		"Bring a private network with a NAT gateway or a router (vendors.upcloud.resources.networks.use), " +
		"or keep publicIPv4 on")}
}
