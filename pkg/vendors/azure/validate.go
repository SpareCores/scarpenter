/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	azapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/azure"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Syntax of what a NodeClass names.
var (
	regionPattern = regexp.MustCompile(`^[a-z][a-z0-9]{2,39}$`)
	groupPattern  = regexp.MustCompile(`^[-\w._()]{1,90}$`)
	armIDPattern  = regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/`)
	subnetPattern = regexp.MustCompile(
		`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/virtualNetworks/[^/]+/subnets/[^/]+$`)
	nsgPattern = regexp.MustCompile(
		`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/networkSecurityGroups/[^/]+$`)
	ppgPattern = regexp.MustCompile(
		`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Compute/proximityPlacementGroups/[^/]+$`)
	encryptionSetPattern = regexp.MustCompile(
		`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Compute/diskEncryptionSets/[^/]+$`)
)

// maxUserTags leaves room for the tags SCarpenter adds (the six logical tags
// and Name) within Azure's 50 per resource.
const maxUserTags = 40

// diskLimits are the sizes of managed disks in GiB (the documented limits: an
// OS disk up to 4095, a data disk up to 32767, at least 1).
const (
	maxRootGiB = 4095
	maxDataGiB = 32767
)

// NodeClassWarnings says what is valid in the NodeClass but has no effect: an
// explicit networking.isolation next to a network security group brought with
// use, which decides alone (a NIC takes one group; open question 11 of plan 17).
func (d *Driver) NodeClassWarnings(nc *v1alpha1.SCNodeClass) []string {
	b := nc.Spec.Vendors.Azure
	if b == nil || b.Resources == nil || b.Resources.SecurityGroups == nil || b.Resources.SecurityGroups.Use == nil {
		return nil
	}
	// Explicit: the default of the NodeClass is not a setting that was made.
	iso := nc.Spec.Networking.Isolation
	if b.Networking != nil && b.Networking.Isolation != "" {
		iso = b.Networking.Isolation
	}
	if iso == "" || iso == common.IsolationNone {
		return nil
	}
	return []string{fmt.Sprintf("networking.isolation %s has no effect on the brought network security group: it decides "+
		"alone, and a NIC takes one group", iso)}
}

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// azure block and of the Azure blocks of the profiles (regions, subscription,
// key, disk SKUs and sizes, resource references, tags) so that a mistake shows in
// the NodeClass's status and not in a failed launch. What needs the subscription
// (that a referenced subnet exists) is checked by the resource status, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.Azure; b != nil {
		errs = append(errs, d.validateBlock(b)...)
		errs = append(errs, validateEgress(nc, b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.Azure != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".azure", p.Azure)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Network {
		if p.Azure != nil && p.Azure.AcceleratedNetworking != nil {
			switch string(p.Azure.AcceleratedNetworking.Enabled) {
			case settingAuto, settingTrue, settingFalse:
			default:
				errs = append(errs, fmt.Errorf("profiles.network.%s.azure.acceleratedNetworking.enabled is auto, \"true\" or "+
					"\"false\", not %q", name, p.Azure.AcceleratedNetworking.Enabled))
			}
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if g := p.Azure; g != nil && g.ProximityPlacementGroup != nil {
			pg := g.ProximityPlacementGroup
			if (pg.Strategy == "") == (pg.Use == nil) {
				errs = append(errs, fmt.Errorf("profiles.placement.%s.azure.proximityPlacementGroup: set exactly one of "+
					"strategy and use", name))
			}
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (Azure allows 50 per resource and SCarpenter adds seven)",
			len(nc.Spec.Tags), maxUserTags))
	}
	for _, k := range slices.Sorted(maps.Keys(nc.Spec.Tags)) {
		if err := ValidateTags(map[string]string{k: nc.Spec.Tags[k]}); err != nil {
			errs = append(errs, fmt.Errorf("tags: %w", plain(err)))
		}
		if strings.HasPrefix(strings.ToLower(k), "scarpenter-") || strings.EqualFold(k, TagNameName) {
			errs = append(errs, fmt.Errorf("tags: the key %q is reserved for SCarpenter", k))
		}
	}
	return errors.Join(errs...)
}

func (d *Driver) validateBlock(b *azapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf(
				"vendors.azure.regions: %q is not an Azure location name (for example westeurope)", r))
		}
	}
	if b.SubscriptionID != "" && d.subscription != "" && !strings.EqualFold(b.SubscriptionID, d.subscription) {
		errs = append(errs, fmt.Errorf("vendors.azure.subscriptionID: this controller works in another subscription "+
			"than %s (the guard stops a NodeClass from targeting the wrong controller)", b.SubscriptionID))
	}
	if b.AdminSSHPublicKey != "" {
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(b.AdminSSHPublicKey)); err != nil {
			errs = append(errs, fmt.Errorf("vendors.azure.adminSSHPublicKey is not an authorized_keys line: %v", err))
		}
	}
	errs = append(errs, validateStorage("vendors.azure.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.azure.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := d.Selectors()
		errs = append(errs, validateResource(KindResourceGroups, caps[KindResourceGroups], r.ResourceGroups, validGroupID)...)
		errs = append(errs, validateResource(KindSubnets, caps[KindSubnets], r.Subnets, idLike(subnetPattern, "subnet"))...)
		errs = append(errs, validateResource(KindSecurityGroups, caps[KindSecurityGroups], r.SecurityGroups,
			idLike(nsgPattern, "network security group"))...)
		errs = append(errs, validateResource(KindPlacement, caps[KindPlacement], r.ProximityPlacementGroups,
			idLike(ppgPattern, "proximity placement group"))...)
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images, validImageID)...)
		if r.Images != nil && r.Images.Use != nil && r.Images.Use.Name != "" && !strings.Contains(r.Images.Use.Name, "{") {
			parts := strings.SplitN(r.Images.Use.Name, ":", 4)
			if len(parts) != 4 || slices.ContainsFunc(parts[:3], inexactPart) {
				errs = append(errs, fmt.Errorf("vendors.azure.resources.images.use.name: %q is not "+
					"publisher:offer:sku:<version glob> (the first three parts are exact)", r.Images.Use.Name))
			}
		}
	}
	return errs
}

func validateStorage(path string, st *azapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if r := st.Root; r != nil {
		if r.StorageAccountType != "" && !validStorageType(string(r.StorageAccountType)) {
			errs = append(errs, fmt.Errorf("%s.root.storageAccountType: %q is not one of %s", path, r.StorageAccountType,
				strings.Join(storageAccountTypes, ", ")))
		}
		if r.DiskSizeGB != 0 && (r.DiskSizeGB < 30 || r.DiskSizeGB > maxRootGiB) {
			errs = append(errs, fmt.Errorf("%s.root.diskSizeGB is 30 to %d, not %d", path, maxRootGiB, r.DiskSizeGB))
		}
		if r.DiskEncryptionSetID != "" && !encryptionSetPattern.MatchString(r.DiskEncryptionSetID) {
			errs = append(errs, fmt.Errorf("%s.root.diskEncryptionSetID is not the ARM ID of a disk encryption set", path))
		}
		if r.Ephemeral != nil && r.StorageAccountType != "" {
			errs = append(errs, fmt.Errorf("%s.root: an ephemeral OS disk has no storageAccountType", path))
		}
		if r.Ephemeral != nil && r.Caching != "" && r.Caching != "ReadOnly" {
			errs = append(errs, fmt.Errorf("%s.root: an ephemeral OS disk is read-only cached", path))
		}
	}
	names := map[string]bool{}
	for i, dd := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dd.Name] {
			errs = append(errs, fmt.Errorf("%s: the disk name %q is used twice", p, dd.Name))
		}
		names[dd.Name] = true
		if dd.StorageAccountType != "" && !validStorageType(string(dd.StorageAccountType)) {
			errs = append(errs, fmt.Errorf("%s.storageAccountType: %q is not one of %s", p, dd.StorageAccountType,
				strings.Join(storageAccountTypes, ", ")))
		}
		if dd.DiskSizeGB < 1 || dd.DiskSizeGB > maxDataGiB {
			errs = append(errs, fmt.Errorf("%s.diskSizeGB is 1 to %d, not %d", p, maxDataGiB, dd.DiskSizeGB))
		}
	}
	if ls := st.LocalStorage; ls != nil && ls.Policy != common.LocalStorageIgnore {
		errs = append(errs, fmt.Errorf("%s.localStorage: only the policy ignore is supported in v1 (mount and raid0 wait "+
			"for the device-path spike of plan 17)", path))
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
			p, err := netip.ParsePrefix(c)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is not a CIDR", path, i, c))
			case !p.Addr().Is4():
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is not IPv4: Azure nodes are IPv4 only in v1", path, i, c))
			}
		}
	}
	return errs
}

// validGroupID accepts the name of a resource group or its ARM ID.
func validGroupID(id string) error {
	if strings.HasPrefix(id, "/") {
		if !regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+$`).MatchString(id) {
			return errors.New("not the ARM ID of a resource group (/subscriptions/<id>/resourceGroups/<name>)")
		}
		return nil
	}
	if !groupPattern.MatchString(id) {
		return errors.New("not a resource group name")
	}
	return nil
}

func idLike(p *regexp.Regexp, what string) func(string) error {
	return func(id string) error {
		if !p.MatchString(id) {
			return fmt.Errorf("not the ARM ID of a %s", what)
		}
		return nil
	}
}

// validImageID accepts a marketplace URN; a gallery image ID is not supported in
// v1.
func validImageID(id string) error {
	if armIDPattern.MatchString(id) {
		return errors.New("gallery images are not supported in v1: use a marketplace URN (publisher:offer:sku:version)")
	}
	if _, err := ParseURN(id); err != nil {
		return err
	}
	return nil
}

// validateResource checks a `use` block: the selector against the declaration of
// the kind (every field, match attribute and filter name), then what only Azure
// knows: region keys, the shape of IDs and the rules for tags.
func validateResource(
	kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource, validID func(string) error,
) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.azure.resources." + string(kind) + ".use"
	errs := selector.Validate(Vendor, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region (an Azure location name, or %q for every region)",
				path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if strings.Contains(id, "{") {
				continue
			}
			if err := validID(id); err != nil {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q: %w", path, region, id, err))
			}
		}
	}
	for k, v := range r.Use.Tags {
		if strings.Contains(k, "{") || strings.Contains(v, "{") {
			continue // checked after the placeholders are expanded
		}
		if v == "*" {
			v = ""
		}
		if err := ValidateTags(map[string]string{k: v}); err != nil {
			errs = append(errs, fmt.Errorf("%s.tags: %w", path, plain(err)))
		}
	}
	return errs
}

// validateEgress refuses what the managed network cannot give: Azure nodes are
// IPv4 only in v1, and without a public IPv4 address nothing gives them egress
// (the managed subnet has no NAT gateway, and a brought subnet's outbound access
// cannot be seen), unless the NodeClass brings its own subnets, where egress is
// the operator's job.
func validateEgress(nc *v1alpha1.SCNodeClass, b *azapi.Settings) []error {
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	var errs []error
	if boolOr(net.PublicIPv6, false) {
		errs = append(errs, errors.New("networking.publicIPv6: Azure nodes are IPv4 only in v1"))
	}
	if boolOr(net.PublicIPv4, true) {
		return errs
	}
	if r := b.Resources; r != nil && r.Subnets != nil && r.Subnets.Use != nil {
		return errs
	}
	return append(errs, errors.New("networking.publicIPv4: false is refused: nodes without a public IPv4 address would "+
		"have no egress for sysexts and container images, because the managed subnet has no NAT gateway. "+
		"Use your own subnets with a NAT gateway (vendors.azure.resources.subnets.use), or keep publicIPv4 on"))
}

// plain is the message of a driver error without its class: what a NodeClass
// author reads in the status.
func plain(err error) error {
	if ve, ok := errors.AsType[*vendors.Error](err); ok && ve.Err != nil {
		return ve.Err
	}
	return err
}
