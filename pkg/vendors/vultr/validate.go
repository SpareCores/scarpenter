/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

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
	vultrapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/vultr"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Syntax of what a NodeClass names.
var (
	// A Vultr region ID is a short lower-case code (ewr, fra, sao).
	regionPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)
	// Snapshots, firewall groups and VPCs are UUIDs.
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// maxUserTags bounds the NodeClass's own instance tags. Vultr documents no
// limit (spike S6 measures the count and the length), so this is our own
// ceiling, with room for the logical tags the driver adds.
const maxUserTags = 40

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// vultr block, what Vultr cannot do (a public IPv4 address is required, the
// firewall takes at most 50 rules, isolation against the VPC), the NodeClass's
// tags, and the API key. What needs the account (that a referenced snapshot
// exists) is the status of the images and resources, not this.
func (d *Driver) ValidateNodeClass(ctx context.Context, nc *v1alpha1.SCNodeClass) error {
	b := nc.Spec.Vendors.Vultr
	var errs []error
	if b != nil {
		errs = append(errs, d.validateBlock(nc, b)...)
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (Vultr documents no limit; SCarpenter adds six)",
			len(nc.Spec.Tags), maxUserTags))
	}
	for _, k := range slices.Sorted(maps.Keys(nc.Spec.Tags)) {
		switch {
		case k == "" || strings.Contains(k, "="):
			errs = append(errs, fmt.Errorf("tags: %q is not a valid key: Vultr tags are plain strings and "+
				"SCarpenter writes key=value", k))
		case strings.HasPrefix(k, vendors.DefaultTagKey("")):
			errs = append(errs, fmt.Errorf("tags: the key %q is reserved for SCarpenter", k))
		}
	}
	if len(errs) == 0 && d.ops != nil {
		if err := d.checkKey(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *Driver) validateBlock(nc *v1alpha1.SCNodeClass, b *vultrapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.vultr.regions: %q is not a Vultr region ID (ewr, fra, ...)", r))
		}
	}
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	errs = append(errs, validateNetworking(net, b.Resources)...)
	if r := b.Resources; r != nil {
		caps := (&Driver{}).Selectors()
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images)...)
		errs = append(errs, validateResource(KindFirewallGroups, caps[KindFirewallGroups], r.FirewallGroups)...)
		errs = append(errs, validateResource(KindNetworks, caps[KindNetworks], r.Networks)...)
		if r.Images != nil && r.Images.Use != nil && r.Images.Use.Match != nil {
			for _, a := range r.Images.Use.Match.Architecture {
				if a != "amd64" {
					errs = append(errs, fmt.Errorf("vendors.vultr.resources.images.use.match.architecture: %q: Vultr has "+
						"no Arm instances, only amd64", a))
				}
			}
		}
	}
	return errs
}

// validateNetworking refuses what Vultr cannot honour, never degrading it
// silently (ADR-017): no public IPv4 address, more than 50 firewall rules, and
// an isolation the configured VPC kind contradicts.
func validateNetworking(net common.Networking, res *vultrapi.Resources) []error {
	var errs []error
	if net.PublicIPv4 != nil && !*net.PublicIPv4 {
		errs = append(errs, fmt.Errorf("networking.publicIPv4: false is refused: %w", noPublicIPv4()))
	}
	if net.Inbound != nil {
		for i, r := range net.Inbound.Rules {
			for _, c := range r.CIDRs {
				if _, err := netip.ParsePrefix(c); err != nil {
					errs = append(errs, fmt.Errorf("networking.inbound.rules[%d]: %q is not a CIDR", i, c))
				}
			}
		}
		if _, err := FirewallRules(net.Inbound); err != nil {
			errs = append(errs, fmt.Errorf("networking.inbound: %w", err))
		}
	}
	var vpc *common.Resource
	if res != nil {
		vpc = res.Networks
	}
	switch iso := effectiveIsolation(net); {
	case iso == common.IsolationNone && vpc != nil && (vpc.Use != nil || vpc.Create != nil):
		errs = append(errs, errors.New("vendors.vultr.resources.networks: isolation none attaches no VPC, so a VPC "+
			"that is brought or created would be ignored: set networking.isolation to nodeclass, or drop networks"))
	case iso != common.IsolationNone && vpc != nil && vpc.Forbid:
		errs = append(errs, fmt.Errorf("vendors.vultr.resources.networks: forbid contradicts networking.isolation %s, "+
			"which puts the nodes in a VPC (on Vultr a run is the whole NodeClass): use isolation none", iso))
	}
	return errs
}

// validateResource checks a `use` block: the selector against the declaration of
// the kind, then what only Vultr knows: region keys and the shape of IDs.
func validateResource(kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.vultr.resources." + string(kind) + ".use"
	errs := selector.Validate(Vendor, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region (a Vultr region ID, or %q for every region)",
				path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if !strings.Contains(id, "{") && !uuidPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a valid ID (a UUID)", path, region, id))
			}
		}
	}
	return errs
}
