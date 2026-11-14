/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ociapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/oci"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// regionPattern is the shape of an OCI region name (eu-frankfurt-1).
var regionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]+$`)

// maxUserTags leaves room for the seven tags SCarpenter adds to the 64 OCI allows.
const maxUserTags = maxTags - 7

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the oci block (regions,
// the boot volume, metadata keys, tags, resource references, and the networking options a managed
// network cannot honour) so that a mistake shows in the NodeClass's status and not in a failed
// launch. What needs the tenancy (that a referenced subnet exists, that a region is subscribed) is
// checked by the resource status and the launch, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	b := nc.Spec.Vendors.OCI
	if b == nil {
		return nil
	}
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.oci.regions: %q is not an OCI region name", r))
		}
	}
	// The part of the block the launch decodes (storage and metadata).
	raw, err := json.Marshal(struct {
		Storage  *ociapi.Storage   `json:"storage,omitempty"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}{b.Storage, b.Metadata})
	if err == nil {
		_, err = decodeSettings(vendors.VendorSettings{Raw: raw})
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("vendors.oci: %w", err))
	}
	if b.Storage != nil && b.Storage.Root != nil {
		if k := b.Storage.Root.KmsKeyID; k != "" && !IsOCID(k, "key") {
			errs = append(errs, fmt.Errorf("vendors.oci.storage.root.kmsKeyId: %q is not a key OCID", k))
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (OCI allows %d freeform tags and SCarpenter adds seven)",
			len(nc.Spec.Tags), maxUserTags, maxTags))
	}
	if _, err := FreeformTags(nil, nc.Spec.Tags); err != nil {
		errs = append(errs, fmt.Errorf("tags: %w", err))
	}
	if b.Networking != nil && b.Networking.Inbound != nil {
		errs = append(errs, validateInbound("vendors.oci.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := d.Selectors()
		errs = append(errs, validateResource(KindSubnets, caps[KindSubnets], r.Subnets.Common(), "subnet")...)
		errs = append(errs, validateResource(KindNetworkSecurityGroups, caps[KindNetworkSecurityGroups],
			r.NetworkSecurityGroups, "networksecuritygroup")...)
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images, "image")...)
		if c := r.Subnets; c != nil && c.Create != nil && c.Create.CIDR != "" {
			if _, err := subnetCIDR(c.Create.CIDR); err != nil {
				errs = append(errs, fmt.Errorf("vendors.oci.resources.subnets.create.cidr: %w", err))
			} else if !privateRange(c.Create.CIDR) {
				errs = append(errs, fmt.Errorf("vendors.oci.resources.subnets.create.cidr: %q is not inside 10.0.0.0/8, "+
					"172.16.0.0/12 or 192.168.0.0/16 (private ranges)", c.Create.CIDR))
			}
		}
	}
	errs = append(errs, validateManagedNetwork(nc, b)...)
	return errors.Join(errs...)
}

// validateManagedNetwork refuses what a managed network cannot honour: it is one regional public
// subnet without a NAT gateway or an IPv6 prefix, so a node there needs a public IPv4 address and
// has no IPv6. With subnets the NodeClass brings, the driver cannot see a NAT or a prefix reliably
// here: the resource check reports the zones (ZoneGaps).
func validateManagedNetwork(nc *v1alpha1.SCNodeClass, b *ociapi.Settings) []error {
	if r := b.Resources; r != nil && r.Subnets != nil && r.Subnets.Use != nil {
		return nil
	}
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	var errs []error
	if !boolOr(net.PublicIPv4, true) {
		errs = append(errs, errors.New("networking.publicIPv4 is false, but the managed network has no NAT "+
			"gateway, so a node could not fetch its sysexts or container images: bring a subnet with a NAT gateway "+
			"(resources.subnets.use) or keep publicIPv4 true"))
	}
	if boolOr(net.PublicIPv6, false) {
		errs = append(errs, errors.New("networking.publicIPv6 is true, but the managed network has no IPv6 prefix in v1: "+
			"bring a subnet that has one (resources.subnets.use)"))
	}
	return errs
}

// privateRange reports whether the range is inside one of the private IPv4 ranges.
func privateRange(s string) bool {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return false
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if rp := netip.MustParsePrefix(r); rp.Contains(p.Addr()) && rp.Bits() <= p.Bits() {
			return true
		}
	}
	return false
}

func validateInbound(path string, in *common.Inbound) []error {
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

// validateResource checks a resource block: the selector against the kind's declaration, the region
// keys of ids (OCIDs are regional: a region name, never *) and the shape of the OCIDs of the type.
func validateResource(kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource, ocidType string,
) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.oci.resources." + string(kind) + ".use"
	errs := selector.Validate(Name, kind, path, r.Use, caps)
	for _, region := range slices.Sorted(maps.Keys(r.Use.IDs)) {
		if !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region name (OCIDs are regional: no %q)", path, region,
				selector.RegionAll))
		}
		for _, id := range r.Use.IDs[region] {
			if !strings.Contains(id, "{") && !IsOCID(id, ocidType) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a %s OCID", path, region, id, ocidType))
			}
		}
	}
	for k := range r.Use.Tags {
		if !strings.Contains(k, "{") && (k == "" || len(k) > maxTagKey) {
			errs = append(errs, fmt.Errorf("%s.tags: %q is not a valid freeform tag key", path, k))
		}
	}
	return errs
}

var _ vendors.NodeClassValidator = (*Driver)(nil)
