/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

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
	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Syntax of what a NodeClass names.
var (
	regionPattern     = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+$`)
	vswitchIDPattern  = regexp.MustCompile(`^vsw-[0-9a-z]{6,40}$`)
	groupIDPattern    = regexp.MustCompile(`^sg-[0-9a-z]{6,40}$`)
	setIDPattern      = regexp.MustCompile(`^ds-[0-9a-z]{6,40}$`)
	imageIDPattern    = regexp.MustCompile(`^m-[0-9a-z]{6,40}$`)
	imageOwnerPattern = regexp.MustCompile(`^([0-9]{1,20}|self|system|marketplace)$`)
	tagKeyPattern     = regexp.MustCompile(`^[^\s].{0,127}$`)
)

// maxUserTags leaves room for the tags SCarpenter adds (the five logical tags)
// within the 20 that ECS takes on an instance (QuotaExceed.Tags and
// NumberExceed.Tags are configuration errors, not quota).
const maxUserTags = maxTags - 5

// diskLimits are the size limits of a disk category as the ECS documentation states
// them (GiB); levels maps an ESSD's performance level to its minimum size.
type diskLimits struct {
	minGiB, maxGiB int64
	levels         map[string]int64
}

var categoryLimits = map[string]diskLimits{
	CategoryESSD: {minGiB: 1, maxGiB: 32768, levels: map[string]int64{"PL0": 1, "PL1": 20, "PL2": 461, "PL3": 1261}},
	CategoryAuto: {minGiB: 1, maxGiB: 65536}, CategoryESSDEntry: {minGiB: 10, maxGiB: 32768},
	CategoryEfficiency: {minGiB: 20, maxGiB: 32768}, CategorySSD: {minGiB: 20, maxGiB: 32768},
}

// validateDisk checks a resolved disk against the limits of its category. A
// system disk is 20 to 2048 GiB (a Linux image) and at least as large as the
// category allows.
func validateDisk(path string, dk resolvedDisk, system bool) []error {
	lim, ok := categoryLimits[dk.Category]
	if !ok {
		return []error{fmt.Errorf("%s: unknown disk category %q", path, dk.Category)}
	}
	var errs []error
	minGiB, maxGiB := lim.minGiB, lim.maxGiB
	if system {
		minGiB, maxGiB = max(minGiB, minSystemDiskGiB), min(maxGiB, maxSystemDiskGiB)
	}
	if dk.Category == CategoryESSD && dk.Level != "" {
		if m, ok := lim.levels[dk.Level]; ok {
			minGiB = max(minGiB, m)
		}
	}
	if dk.GiB < minGiB || dk.GiB > maxGiB {
		errs = append(errs, fmt.Errorf("%s: %s disks of level %s are %d to %d GiB, not %d", path, dk.Category,
			dk.Level, minGiB, maxGiB, dk.GiB))
	}
	if dk.Iops != nil || dk.Bursting != nil {
		if dk.Category != CategoryAuto {
			errs = append(errs, fmt.Errorf("%s: provisionedIops and burstingEnabled are only valid for cloud_auto", path))
		} else if dk.Iops != nil {
			base := min(1800+50*dk.GiB, 50000)
			limit := min(50000, 1000*dk.GiB-base)
			if *dk.Iops < 0 || *dk.Iops > limit {
				errs = append(errs, fmt.Errorf("%s: provisionedIops of a %d GiB cloud_auto disk are 0 to %d, not %d",
					path, dk.GiB, limit, *dk.Iops))
			}
		}
	}
	if dk.KMSKeyID != "" && !dk.Encrypted {
		errs = append(errs, fmt.Errorf("%s: kmsKeyID needs encrypted", path))
	}
	if dk.Level != "" && dk.Category != CategoryESSD {
		errs = append(errs, fmt.Errorf("%s: performanceLevel is only valid for cloud_essd", path))
	}
	return errs
}

// validateStorage validates a storage block (the vendor block's or a profile's).
func validateStorage(path string, st *api.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if st.Root != nil {
		errs = append(errs, validateDisk(path+".root", rootDisk(Settings{Storage: st}), true)...)
	}
	names := map[string]bool{}
	for i, dd := range dataDisks(Settings{Storage: st}) {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dd.Name] {
			errs = append(errs, fmt.Errorf("%s: the disk name %q is used twice", p, dd.Name))
		}
		names[dd.Name] = true
		errs = append(errs, validateDisk(p, dd, false)...)
	}
	return errs
}

func validatePlacement(path string, p *api.Placement) []error {
	if p == nil || p.DeploymentSet == nil {
		return nil
	}
	if (p.DeploymentSet.Strategy == "") == (p.DeploymentSet.Use == nil) {
		return []error{fmt.Errorf("%s.deploymentSet: set exactly one of strategy and use", path)}
	}
	return nil
}

func validateNetworkProfile(path string, n *api.Network) []error {
	if n == nil {
		return nil
	}
	if v := n.InternetMaxBandwidthOut; v != nil && (*v < 0 || *v > maxInternetBandwidth) {
		return []error{fmt.Errorf("%s.internetMaxBandwidthOut is 0 to %d Mbit/s, not %d", path, maxInternetBandwidth, *v)}
	}
	return nil
}

// validateResource checks a `use` block: the selector against the declaration of
// the kind (every field, match attribute and filter name), then what only Alibaba
// Cloud knows: region keys, the shape of IDs and the rules for tags.
func validateResource(
	kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource, idPattern *regexp.Regexp,
) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.alicloud.resources." + string(kind) + ".use"
	errs := selector.Validate(Vendor, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region (an Alibaba Cloud region ID, or %q for every region)",
				path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if idPattern != nil && !strings.Contains(id, "{") && !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a valid ID", path, region, id))
			}
		}
	}
	// A request parameter takes one value: filters under one name are ANDed by
	// intersecting their values, which needs values without wildcards.
	for i, f := range r.Use.Filters {
		for _, g := range r.Use.Filters[:i] {
			if g.Name == f.Name && (slices.ContainsFunc(f.Values, selector.HasWildcard) ||
				slices.ContainsFunc(g.Values, selector.HasWildcard)) {
				errs = append(errs, fmt.Errorf("%s.filters[%d]: %q is given twice with a wildcard value; ECS takes one "+
					"value per parameter, so give it once", path, i, f.Name))
				break
			}
		}
		if slices.ContainsFunc(f.Values, selector.HasWildcard) {
			errs = append(errs, fmt.Errorf("%s.filters[%d]: the values of %q are passed to ECS as they are, "+
				"without wildcards", path, i, f.Name))
		}
	}
	for k, v := range r.Use.Tags {
		if strings.Contains(k, "{") || strings.Contains(v, "{") {
			continue // checked after the placeholders are expanded
		}
		if v == "*" {
			v = ""
		}
		if err := validateTag(k, v); err != nil {
			errs = append(errs, fmt.Errorf("%s.tags: %w", path, err))
		}
	}
	return errs
}

// validateTag checks a tag against ECS's rules: a key of 1 to 128 characters that
// does not start with aliyun or acs: and does not contain http:// or https://, a
// value of at most 128 characters.
func validateTag(k, v string) error {
	lk := strings.ToLower(k)
	switch {
	case strings.HasPrefix(lk, "aliyun") || strings.HasPrefix(lk, "acs:"):
		return fmt.Errorf("the tag key %q has a reserved prefix (aliyun or acs:) which ECS does not allow", k)
	case strings.Contains(lk, "http://") || strings.Contains(lk, "https://"):
		return fmt.Errorf("the tag key %q must not contain http:// or https://", k)
	case !tagKeyPattern.MatchString(k):
		return fmt.Errorf("%q is not a valid tag key (1 to 128 characters, not starting with a space)", k)
	case len(v) > 128:
		return fmt.Errorf("the value of the tag %q is longer than 128 characters", k)
	}
	return nil
}

func validateInbound(path string, in *common.Inbound) []error {
	if in == nil {
		return nil
	}
	var errs []error
	for i, r := range in.Rules {
		for _, c := range r.CIDRs {
			p, err := netip.ParsePrefix(c)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is not a CIDR", path, i, c))
			case !p.Addr().Is4():
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is an IPv6 range; IPv6 is not supported on Alibaba Cloud yet",
					path, i, c))
			}
		}
	}
	return errs
}

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// alicloud block and of the alicloud blocks of the profiles (regions, metadata
// options, disk categories, sizes, performance levels and IOPS against the ECS
// limits, tags, resource references) so that a mistake shows in the NodeClass's
// status and not in a failed launch. What needs the account (that a referenced
// vSwitch exists) is checked by the resource status, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.Alicloud; b != nil {
		errs = append(errs, validateBlock(b)...)
		errs = append(errs, validateNetworking(nc, b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.Alicloud != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".alicloud", p.Alicloud)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Network {
		if p.Alicloud != nil {
			errs = append(errs, validateNetworkProfile("profiles.network."+name+".alicloud", p.Alicloud)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if p.Alicloud != nil {
			errs = append(errs, validatePlacement("profiles.placement."+name+".alicloud", p.Alicloud)...)
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (ECS allows %d per instance and SCarpenter adds five)",
			len(nc.Spec.Tags), maxUserTags, maxTags))
	}
	for _, k := range slices.Sorted(maps.Keys(nc.Spec.Tags)) {
		if err := validateTag(k, nc.Spec.Tags[k]); err != nil {
			errs = append(errs, fmt.Errorf("tags: %w", err))
		}
		if strings.HasPrefix(k, "scarpenter.sparecores.com/") {
			errs = append(errs, fmt.Errorf("tags: the key %q is reserved for SCarpenter", k))
		}
	}
	return errors.Join(errs...)
}

func validateBlock(b *api.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.alicloud.regions: %q is not an Alibaba Cloud region ID", r))
		}
	}
	if v := b.InternetMaxBandwidthOut; v != nil && (*v < 0 || *v > maxInternetBandwidth) {
		errs = append(errs, fmt.Errorf("vendors.alicloud.internetMaxBandwidthOut is 0 to %d Mbit/s, not %d",
			maxInternetBandwidth, *v))
	}
	errs = append(errs, validateStorage("vendors.alicloud.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.alicloud.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := (&Driver{}).Selectors()
		errs = append(errs, validateResource(KindSubnets, caps[KindSubnets], r.Subnets, vswitchIDPattern)...)
		errs = append(errs, validateResource(KindSecurityGroups, caps[KindSecurityGroups], r.SecurityGroups,
			groupIDPattern)...)
		errs = append(errs, validateResource(KindDeploymentSets, caps[KindDeploymentSets], r.DeploymentSets, setIDPattern)...)
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images, imageIDPattern)...)
		if r.Images != nil && r.Images.Use != nil && r.Images.Use.Match != nil {
			for _, o := range r.Images.Use.Match.Owner {
				if !strings.Contains(o, "{") && !imageOwnerPattern.MatchString(o) {
					errs = append(errs, fmt.Errorf("vendors.alicloud.resources.images.use.match.owner: %q is not an account "+
						"UID, self, system or marketplace", o))
				}
			}
		}
	}
	return errs
}

// validateNetworking refuses what ECS cannot do here: publicIPv6 (it needs an
// IPv6 gateway the driver does not manage), and nodes without a public IPv4
// address where nothing gives them egress: the account's default VPC and the
// managed network have no NAT gateway, so a node without a public address could
// not fetch its sysexts or its container images. With vSwitches the NodeClass
// brings (resources.subnets.use) the driver cannot see a NAT gateway or a proxy
// reliably, so it allows it: egress is the user's job there.
func validateNetworking(nc *v1alpha1.SCNodeClass, b *api.Settings) []error {
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	var errs []error
	if boolOr(net.PublicIPv6, false) {
		errs = append(errs, errors.New("networking.publicIPv6: true is refused: IPv6 on ECS needs an IPv6 gateway the "+
			"driver does not manage (deferred)"))
	}
	public := boolOr(net.PublicIPv4, true) && (b.InternetMaxBandwidthOut == nil || *b.InternetMaxBandwidthOut > 0)
	if public {
		return errs
	}
	if r := b.Resources; r != nil && r.Subnets != nil && r.Subnets.Use != nil {
		return errs
	}
	return append(errs, errors.New("networking.publicIPv4: false (or internetMaxBandwidthOut: 0) is refused: "+
		"nodes without a "+
		"public IPv4 address would have no egress for sysexts and container images, because the account's default VPC "+
		"and the managed network have no NAT gateway. Use your own private vSwitches with a NAT gateway or a proxy "+
		"(vendors.alicloud.resources.subnets.use), or keep the public address"))
}
