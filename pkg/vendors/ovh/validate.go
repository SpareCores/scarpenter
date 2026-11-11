/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

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
	ovhapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/ovh"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

var (
	// API region names are the Keystone catalog's: upper case, digits and dashes.
	regionPattern = regexp.MustCompile(`^[A-Z0-9]+(-[A-Z0-9]+)*$`)
	// Glance, Neutron and Nova identify resources by UUID.
	idPattern = uuidRe
)

// maxUserTags leaves room in Nova's 128 metadata items for the logical tags of
// SCarpenter (six) and some margin.
const maxUserTags = 100

// validateTag checks a NodeClass tag as Nova metadata: the key (with the prefix
// scarpenter-tag-) and the value are at most 255 characters.
func validateTag(k, v string) error {
	switch {
	case k == "":
		return errors.New("a tag key is empty")
	case len(ExtraTagPrefix)+len(k) > MaxMetadataLen:
		return fmt.Errorf("the tag key %q is longer than %d characters with the prefix %s",
			k, MaxMetadataLen-len(ExtraTagPrefix), ExtraTagPrefix)
	case len(v) > MaxMetadataLen:
		return fmt.Errorf("the value of the tag %q is longer than %d characters", k, MaxMetadataLen)
	}
	return nil
}

func validateVolume(path, volumeType string, sizeGiB int64) []error {
	var errs []error
	if !slices.Contains(KnownVolumeTypes, volumeType) {
		errs = append(errs, fmt.Errorf("%s: the volume type %q is not one of %v", path, volumeType, KnownVolumeTypes))
	}
	if sizeGiB < MinVolumeGiB || sizeGiB > MaxVolumeGiB {
		errs = append(errs, fmt.Errorf("%s: volumes are %d to %d GiB, not %d", path, MinVolumeGiB, MaxVolumeGiB, sizeGiB))
	}
	return errs
}

func validateStorage(path string, st *ovhapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if st.LocalStorage != nil {
		errs = append(errs, fmt.Errorf("%s.localStorage: not supported on OVH: "+
			"the local disks of OVH flavors are the root disk", path))
	}
	if st.Root != nil && st.Root.BootFromVolume != nil {
		b := st.Root.BootFromVolume
		errs = append(errs, validateVolume(path+".root.bootFromVolume", string(b.VolumeType), gib(b.Size))...)
	}
	if len(st.Data) > MaxDataVolume {
		errs = append(errs, fmt.Errorf("%s.data: %d volumes, at most %d", path, len(st.Data), MaxDataVolume))
	}
	names := map[string]bool{}
	for i, dv := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dv.Name] {
			errs = append(errs, fmt.Errorf("%s: the volume name %q is used twice", p, dv.Name))
		}
		names[dv.Name] = true
		errs = append(errs, validateVolume(p, string(dv.VolumeType), gib(dv.Size))...)
	}
	return errs
}

func validatePlacement(path string, p *ovhapi.Placement) []error {
	if p == nil || p.ServerGroup == nil {
		return nil
	}
	g := p.ServerGroup
	var errs []error
	if (g.Policy == "") == (g.Use == nil) {
		errs = append(errs, fmt.Errorf("%s.serverGroup: set exactly one of policy and use", path))
	}
	if g.Policy != "" && g.Policy != ovhapi.PolicyAffinity && g.Policy != ovhapi.PolicyAntiAffinity {
		errs = append(errs, fmt.Errorf("%s.serverGroup.policy: %q is not affinity or anti-affinity", path, g.Policy))
	}
	return errs
}

func validateResource(kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.ovh.resources." + string(kind) + ".use"
	errs := selector.Validate(Name, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region (an OVH region name of the Keystone "+
				"catalog such as GRA11, or %q for every region)", path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if !strings.Contains(id, "{") && !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a valid ID (a UUID)", path, region, id))
			}
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
	return errs
}

func validateBlock(b *ovhapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.ovh.regions: %q is not an OVH region name of the Keystone catalog "+
				"(upper case, such as GRA11 or EU-WEST-PAR)", r))
		}
	}
	errs = append(errs, validateStorage("vendors.ovh.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.ovh.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := (&Driver{}).Selectors()
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images)...)
		errs = append(errs, validateResource(KindSecurityGroups, caps[KindSecurityGroups], r.SecurityGroups)...)
		errs = append(errs, validateResource(KindServerGroups, caps[KindServerGroups], r.ServerGroups)...)
	}
	return errs
}

// ValidateNodeClass implements vendors.NodeClassValidator: what no region can
// fix (ADR-017: nothing is degraded silently).
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.OVH; b != nil {
		errs = append(errs, validateBlock(b)...)
		net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
		if net.PublicIPv4 != nil && !*net.PublicIPv4 {
			errs = append(errs, errors.New("networking.publicIPv4: false is refused: a node without a public IPv4 address "+
				"needs a billed gateway for its egress, which is not supported"))
		}
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.OVH != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".ovh", p.OVH)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if p.OVH != nil {
			errs = append(errs, validatePlacement("profiles.placement."+name+".ovh", p.OVH)...)
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (Nova allows %d metadata items and SCarpenter adds some)",
			len(nc.Spec.Tags), maxUserTags, MaxMetadataItems))
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
