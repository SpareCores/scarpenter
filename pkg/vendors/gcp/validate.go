/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

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
	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Syntax of what a NodeClass names.
var (
	regionPattern   = regexp.MustCompile(`^[a-z]+(-[a-z]+)*[0-9]+$`)
	projectPattern  = regexp.MustCompile(`^[a-z][-a-z0-9.:]{4,61}[a-z0-9]$`)
	serviceAcctRe   = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]@[a-z][-a-z0-9.]*\.(iam\.)?gserviceaccount\.com$`)
	resourceNameRe  = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	subnetIDPattern = regexp.MustCompile(
		`^(projects/[^/]+/regions/[a-z0-9-]+/subnetworks/)?[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	imageIDPattern  = regexp.MustCompile(`^(projects/[^/]+/global/images/)?[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	policyIDPattern = regexp.MustCompile(
		`^(projects/[^/]+/regions/[a-z0-9-]+/resourcePolicies/)?[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	tagIDPattern  = resourceNameRe
	kmsKeyPattern = regexp.MustCompile(
		`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+(/cryptoKeyVersions/[^/]+)?$`)
)

// maxUserTags leaves room for the labels SCarpenter adds (the six logical labels)
// within Compute Engine's 64 per resource.
const maxUserTags = 40

// diskLimits are the limits of the disk types: sizes in GiB, the largest
// provisioned IOPS and throughput (MiB/s), 0 where the type has none. The sizes
// of pd-balanced and hyperdisk-balanced are Keeper's storage table of 2026-11-10;
// the other rows are UNVERIFIED (Google's documentation, to be checked against
// Keeper's storage rows by the lane).
type diskLimits struct {
	minGiB, maxGiB int64
	maxIOPS        int64
	maxThroughput  int64
	bootable       bool
	// needsIOPS says that the type has no default and needs provisionedIops.
	needsIOPS bool
}

var diskTable = map[gcpapi.DiskType]diskLimits{
	gcpapi.DiskTypePDStandard:         {minGiB: 10, maxGiB: 65536, bootable: true},
	gcpapi.DiskTypePDBalanced:         {minGiB: 10, maxGiB: 65536, bootable: true},
	gcpapi.DiskTypePDSSD:              {minGiB: 10, maxGiB: 65536, bootable: true},
	gcpapi.DiskTypeHyperdiskBalanced:  {minGiB: 4, maxGiB: 65536, maxIOPS: 160000, maxThroughput: 2400, bootable: true},
	gcpapi.DiskTypeHyperdiskExtreme:   {minGiB: 64, maxGiB: 65536, maxIOPS: 350000, maxThroughput: 10000, needsIOPS: true},
	gcpapi.DiskTypeHyperdiskThroughpt: {minGiB: 2048, maxGiB: 32768, maxThroughput: 10000},
	gcpapi.DiskTypeHyperdiskML:        {minGiB: 4, maxGiB: 65536, maxThroughput: 1200000},
}

func validateDisk(path string, dk Disk, root bool) []error {
	lim, ok := diskTable[gcpapi.DiskType(dk.Type)]
	if !ok {
		return []error{fmt.Errorf("%s: unknown disk type %q", path, dk.Type)}
	}
	var errs []error
	if root && !lim.bootable {
		errs = append(errs, fmt.Errorf("%s: %s disks cannot be boot disks", path, dk.Type))
	}
	if dk.SizeGb < lim.minGiB || dk.SizeGb > lim.maxGiB {
		errs = append(errs, fmt.Errorf("%s: %s disks are %d to %d GiB, not %d", path, dk.Type, lim.minGiB, lim.maxGiB,
			dk.SizeGb))
	}
	if dk.ProvisionedIops > 0 {
		if lim.maxIOPS == 0 {
			errs = append(errs, fmt.Errorf("%s: provisionedIops is not valid for %s disks", path, dk.Type))
		} else if dk.ProvisionedIops > lim.maxIOPS {
			errs = append(errs, fmt.Errorf("%s: %s takes at most %d IOPS, not %d", path, dk.Type, lim.maxIOPS,
				dk.ProvisionedIops))
		}
	}
	if lim.needsIOPS && dk.ProvisionedIops == 0 {
		errs = append(errs, fmt.Errorf("%s: %s disks need provisionedIops", path, dk.Type))
	}
	if dk.ProvisionedThroughput > 0 {
		if lim.maxThroughput == 0 {
			errs = append(errs, fmt.Errorf("%s: provisionedThroughput is not valid for %s disks", path, dk.Type))
		} else if dk.ProvisionedThroughput > lim.maxThroughput {
			errs = append(errs, fmt.Errorf("%s: %s takes at most %d MiB/s, not %d", path, dk.Type, lim.maxThroughput,
				dk.ProvisionedThroughput))
		}
	}
	if dk.KMSKeyName != "" && !kmsKeyPattern.MatchString(dk.KMSKeyName) {
		errs = append(errs, fmt.Errorf("%s: kmsKeyName %q is not projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>",
			path, dk.KMSKeyName))
	}
	return errs
}

// validateStorage validates a storage block (the vendor block's or a profile's).
func validateStorage(path string, st *gcpapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if r := st.Root; r != nil {
		if r.DiskType == "" {
			// Without a pinned type the default of the machine type applies (a
			// boot disk of either kind is valid); only the size is checked.
			if r.SizeGb != nil && (*r.SizeGb < 10 || *r.SizeGb > 65536) {
				errs = append(errs, fmt.Errorf("%s.root: boot disks are 10 to 65536 GiB, not %d", path, *r.SizeGb))
			}
		} else {
			errs = append(errs, validateDisk(path+".root", rootDisk(gcpapi.Settings{Storage: st}, ""), true)...)
		}
	}
	names := map[string]bool{}
	for i, dd := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dd.Name] {
			errs = append(errs, fmt.Errorf("%s: the disk name %q is used twice", p, dd.Name))
		}
		names[dd.Name] = true
		errs = append(errs, validateDisk(p, dataDisk(dd), false)...)
	}
	if ls := st.LocalStorage; ls != nil && ls.Policy != common.LocalStorageIgnore && ls.MountPath == "" {
		errs = append(errs, fmt.Errorf("%s.localStorage: policy %s needs a mountPath", path, ls.Policy))
	}
	return errs
}

func validatePlacement(path string, p *gcpapi.Placement) []error {
	if p == nil || p.ResourcePolicy == nil {
		return nil
	}
	rp := p.ResourcePolicy
	var errs []error
	n := 0
	if rp.GroupPlacement != nil {
		n++
	}
	if rp.Spread != nil {
		n++
		if rp.Spread.AvailabilityDomainCount < 1 || rp.Spread.AvailabilityDomainCount > 8 {
			errs = append(errs, fmt.Errorf("%s.resourcePolicy.spread: availabilityDomainCount is 1 to 8", path))
		}
	}
	if rp.Use != nil {
		n++
		if !resourceNameRe.MatchString(rp.Use.Name) {
			errs = append(errs, fmt.Errorf("%s.resourcePolicy.use: %q is not a resource policy name", path, rp.Use.Name))
		}
	}
	if n != 1 {
		errs = append(errs, fmt.Errorf("%s.resourcePolicy: set exactly one of groupPlacement, spread and use", path))
	}
	return errs
}

// validateResource checks a `use` block: the selector against the declaration of
// the kind, then what only GCP knows: region keys, the shape of IDs.
func validateResource(
	kind vendors.ResourceKind, caps vendors.SelectorCaps, r *common.Resource, idPattern *regexp.Regexp,
) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	path := "vendors.gcp.resources." + string(kind) + ".use"
	errs := selector.Validate(Name, kind, path, r.Use, caps)
	for region, ids := range r.Use.IDs {
		if region != selector.RegionAll && !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.ids: %q is not a region (a GCP region name, or %q for every region)",
				path, region, selector.RegionAll))
		}
		for _, id := range ids {
			if idPattern != nil && !strings.Contains(id, "{") && !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.ids[%s]: %q is not a valid ID", path, region, id))
			}
		}
	}
	for k := range r.Use.Tags {
		if kind != KindImages {
			continue // rejected by the declaration: no labels
		}
		if !strings.Contains(k, "{") && !labelKeyRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("%s.tags: %q is not a valid label key", path, k))
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

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the gcp
// block and of the GCP blocks of the profiles (regions, project, service account,
// metadata keys, disk types, sizes, IOPS and throughput against the disk limits,
// tags, resource references) so that a mistake shows in the NodeClass's status
// and not in a failed launch. What needs the project (that a referenced subnetwork
// exists) is checked by the resource status, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.GCP; b != nil {
		errs = append(errs, d.validateBlock(b)...)
		errs = append(errs, validateEgress(nc, b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.GCP != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".gcp", p.GCP)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if p.GCP != nil {
			errs = append(errs, validatePlacement("profiles.placement."+name+".gcp", p.GCP)...)
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (Compute Engine allows 64 labels per resource and "+
			"SCarpenter adds six)", len(nc.Spec.Tags), maxUserTags))
	}
	if err := ValidateUserTags(nc.Spec.Tags); err != nil {
		errs = append(errs, fmt.Errorf("tags: %w", err))
	}
	return errors.Join(errs...)
}

func (d *Driver) validateBlock(b *gcpapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.gcp.regions: %q is not a GCP region name", r))
		}
	}
	if b.Project != "" {
		switch {
		case !projectPattern.MatchString(b.Project):
			errs = append(errs, fmt.Errorf("vendors.gcp.project: %q is not a project ID", b.Project))
		case d.project != "" && b.Project != d.project:
			errs = append(errs, fmt.Errorf("vendors.gcp.project: %q is not the project of the controller (%q): "+
				"the controller targets one project (--gcp-project)", b.Project, d.project))
		}
	}
	if b.ImageProject != "" && !projectPattern.MatchString(b.ImageProject) {
		errs = append(errs, fmt.Errorf("vendors.gcp.imageProject: %q is not a project ID", b.ImageProject))
	}
	if b.ServiceAccount != "" && !serviceAcctRe.MatchString(b.ServiceAccount) {
		errs = append(errs, fmt.Errorf("vendors.gcp.serviceAccount: %q is not a service account e-mail", b.ServiceAccount))
	}
	for _, k := range slices.Sorted(maps.Keys(b.Metadata)) {
		if slices.Contains(ReservedMetadataKeys, k) {
			errs = append(errs, fmt.Errorf("vendors.gcp.metadata: the key %q is reserved for SCarpenter", k))
		}
	}
	errs = append(errs, validateStorage("vendors.gcp.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.gcp.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		caps := d.Selectors()
		errs = append(errs, validateResource(KindSubnets, caps[KindSubnets], r.Subnets.Common(), subnetIDPattern)...)
		errs = append(errs, validatePool(r.Subnets)...)
		errs = append(errs, validateResource(KindFirewalls, caps[KindFirewalls], r.Firewalls, tagIDPattern)...)
		errs = append(errs, validateResource(KindResourcePolicies, caps[KindResourcePolicies], r.ResourcePolicies,
			policyIDPattern)...)
		errs = append(errs, validateResource(KindImages, caps[KindImages], r.Images, imageIDPattern)...)
	}
	return errs
}

// validateEgress refuses nodes without a public IPv4 address where nothing gives
// them egress: the default network and the managed network have no Cloud NAT, so
// a node without an external address could not fetch its sysexts or its container
// images. With subnets the NodeClass brings the driver cannot see a NAT or a
// proxy reliably, so it allows it: egress is the user's job there.
func validateEgress(nc *v1alpha1.SCNodeClass, b *gcpapi.Settings) []error {
	net := options.ResolveNetworking(nc.Spec.Networking, b.Networking)
	if boolOr(net.PublicIPv4, true) {
		return nil
	}
	if r := b.Resources; r != nil && r.Subnets != nil && r.Subnets.Use != nil {
		return nil
	}
	return []error{fmt.Errorf("networking.publicIPv4: false is refused: nodes without an external IPv4 address would " +
		"have no egress for sysexts and container images, because the default network and the managed network have no " +
		"Cloud NAT. Use your own subnetworks with a Cloud NAT or a proxy (vendors.gcp.resources.subnets.use), or keep " +
		"publicIPv4 on")}
}
