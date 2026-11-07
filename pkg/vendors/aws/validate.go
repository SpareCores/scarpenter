/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

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
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Syntax of what a NodeClass names.
var (
	regionPattern          = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	instanceProfilePattern = regexp.MustCompile(`^[\w+=,.@-]{1,128}$`)
	subnetIDPattern        = regexp.MustCompile(`^subnet-[0-9a-f]{8,17}$`)
	groupIDPattern         = regexp.MustCompile(`^sg-[0-9a-f]{8,17}$`)
	placementIDPattern     = regexp.MustCompile(`^pg-[0-9a-f]{8,17}$`)
	tagKeyPattern          = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]{1,128}$`)
	tagValuePattern        = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]{0,256}$`)
)

// maxUserTags leaves room for the tags SCarpenter adds (the five logical tags
// and Name) within EC2's 50 per resource.
const maxUserTags = 40

// volumeLimits are the limits of the EBS volume types as the AWS documentation
// states them (volume sizes in GiB, IOPS, throughput in MiB/s). iopsPerGiB is
// the largest ratio of provisioned IOPS to size, 0 where there is none.
type volumeLimits struct {
	minGiB, maxGiB   int32
	iops             bool
	minIOPS, maxIOPS int32
	iopsPerGiB       int32
	throughput       bool
	minThroughput    int32
	maxThroughput    int32
	bootable         bool
}

var ebsLimits = map[awsapi.VolumeType]volumeLimits{
	awsapi.VolumeTypeGP2: {minGiB: 1, maxGiB: 16384, bootable: true},
	awsapi.VolumeTypeGP3: {minGiB: 1, maxGiB: 65536, iops: true, minIOPS: 3000, maxIOPS: 80000, iopsPerGiB: 500,
		throughput: true, minThroughput: 125, maxThroughput: 2000, bootable: true},
	awsapi.VolumeTypeIO1: {minGiB: 4, maxGiB: 16384, iops: true, minIOPS: 100, maxIOPS: 64000, iopsPerGiB: 50,
		bootable: true},
	awsapi.VolumeTypeIO2: {minGiB: 4, maxGiB: 65536, iops: true, minIOPS: 100, maxIOPS: 256000, iopsPerGiB: 1000,
		bootable: true},
	awsapi.VolumeTypeST1:      {minGiB: 125, maxGiB: 16384},
	awsapi.VolumeTypeSC1:      {minGiB: 125, maxGiB: 16384},
	awsapi.VolumeTypeStandard: {minGiB: 1, maxGiB: 1024, bootable: true},
}

// validateVolume checks a resolved volume against the limits of its type.
func validateVolume(path string, v volume, root bool) []error {
	lim, ok := ebsLimits[v.Type]
	if !ok {
		return []error{fmt.Errorf("%s: unknown volume type %q", path, v.Type)}
	}
	var errs []error
	if root && !lim.bootable {
		errs = append(errs, fmt.Errorf("%s: %s volumes cannot be boot volumes", path, v.Type))
	}
	if v.GiB < lim.minGiB || v.GiB > lim.maxGiB {
		errs = append(errs, fmt.Errorf("%s: %s volumes are %d to %d GiB, not %d", path, v.Type, lim.minGiB, lim.maxGiB,
			v.GiB))
	}
	if v.IOPS != nil {
		switch {
		case !lim.iops:
			errs = append(errs, fmt.Errorf("%s: iops is not valid for %s volumes", path, v.Type))
		case *v.IOPS < lim.minIOPS || *v.IOPS > lim.maxIOPS:
			errs = append(errs, fmt.Errorf("%s: %s iops are %d to %d, not %d", path, v.Type, lim.minIOPS, lim.maxIOPS,
				*v.IOPS))
		case int64(*v.IOPS) > int64(lim.iopsPerGiB)*int64(v.GiB):
			errs = append(errs, fmt.Errorf("%s: %s volumes take at most %d IOPS per GiB, %d IOPS need %d GiB",
				path, v.Type, lim.iopsPerGiB, *v.IOPS, (int64(*v.IOPS)+int64(lim.iopsPerGiB)-1)/int64(lim.iopsPerGiB)))
		}
	}
	if v.Type == awsapi.VolumeTypeIO1 || v.Type == awsapi.VolumeTypeIO2 {
		if v.IOPS == nil {
			errs = append(errs, fmt.Errorf("%s: %s volumes need iops", path, v.Type))
		}
	}
	if v.Throughput != nil {
		switch {
		case !lim.throughput:
			errs = append(errs, fmt.Errorf("%s: throughput is only valid for gp3 volumes", path))
		case *v.Throughput < lim.minThroughput || *v.Throughput > lim.maxThroughput:
			errs = append(errs, fmt.Errorf("%s: gp3 throughput is %d to %d MiB/s, not %d", path, lim.minThroughput,
				lim.maxThroughput, *v.Throughput))
		default:
			// gp3 takes at most a quarter MiB/s per provisioned IOPS; 3000 IOPS
			// are included when none are set.
			iops := int64(gp3IncludedIOPS)
			if v.IOPS != nil {
				iops = int64(*v.IOPS)
			}
			if int64(*v.Throughput)*4 > iops {
				errs = append(errs, fmt.Errorf("%s: gp3 throughput of %d MiB/s needs at least %d IOPS (a quarter MiB/s per IOPS)",
					path, *v.Throughput, int64(*v.Throughput)*4))
			}
		}
	}
	if v.KMSKeyID != "" && !v.Encrypted {
		errs = append(errs, fmt.Errorf("%s: kmsKeyID needs encrypted", path))
	}
	return errs
}

// validateStorage validates a storage block (the vendor block's or a profile's).
func validateStorage(path string, st *awsapi.Storage) []error {
	if st == nil {
		return nil
	}
	var errs []error
	if st.Root != nil {
		errs = append(errs, validateVolume(path+".root", rootVolume(awsapi.Settings{Storage: st}), true)...)
	}
	names := map[string]bool{}
	for i, dv := range st.Data {
		p := fmt.Sprintf("%s.data[%d]", path, i)
		if names[dv.Name] {
			errs = append(errs, fmt.Errorf("%s: the volume name %q is used twice", p, dv.Name))
		}
		names[dv.Name] = true
		errs = append(errs, validateVolume(p, dataVolume(dv), false)...)
	}
	if ls := st.LocalStorage; ls != nil && ls.Policy != common.LocalStorageIgnore && ls.MountPath == "" {
		errs = append(errs, fmt.Errorf("%s.localStorage: policy %s needs a mountPath", path, ls.Policy))
	}
	return errs
}

func validatePlacement(path string, p *awsapi.Placement) []error {
	if p == nil || p.PlacementGroup == nil {
		return nil
	}
	g := p.PlacementGroup
	var errs []error
	if (g.Strategy == "") == (g.Use == nil) {
		errs = append(errs, fmt.Errorf("%s.placementGroup: set exactly one of strategy and use", path))
	}
	if g.PartitionCount != nil && g.Strategy != awsapi.PlacementGroupStrategy("partition") {
		errs = append(errs, fmt.Errorf("%s.placementGroup: partitionCount is only valid with the partition strategy", path))
	}
	if g.PartitionCount != nil && (*g.PartitionCount < 1 || *g.PartitionCount > 7) {
		errs = append(errs, fmt.Errorf("%s.placementGroup: partitionCount is 1 to 7", path))
	}
	return errs
}

func validateResource(path string, r *common.Resource, idPattern *regexp.Regexp) []error {
	if r == nil || r.Use == nil {
		return nil
	}
	var errs []error
	for region, ids := range r.Use.IDs {
		if !regionPattern.MatchString(region) {
			errs = append(errs, fmt.Errorf("%s.use.ids: %q is not a region", path, region))
		}
		for _, id := range ids {
			if idPattern != nil && !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.use.ids[%s]: %q is not a valid ID", path, region, id))
			}
		}
	}
	if r.Use.Selector != nil {
		for k, v := range r.Use.Selector.Tags {
			if err := validateTag(k, v); err != nil {
				errs = append(errs, fmt.Errorf("%s.use.selector.tags: %w", path, err))
			}
		}
	}
	return errs
}

func validateTag(k, v string) error {
	switch {
	case strings.HasPrefix(strings.ToLower(k), "aws:"):
		return fmt.Errorf("the tag key %q has the reserved prefix aws: which EC2 does not allow", k)
	case !tagKeyPattern.MatchString(k):
		return fmt.Errorf("%q is not a valid tag key (1 to 128 letters, digits, spaces and _ . : / = + - @)", k)
	case !tagValuePattern.MatchString(v):
		return fmt.Errorf("the value of the tag %q is not valid (up to 256 letters, digits, spaces and _ . : / = + - @)", k)
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
			if _, err := netip.ParsePrefix(c); err != nil {
				errs = append(errs, fmt.Errorf("%s.rules[%d]: %q is not a CIDR", path, i, c))
			}
		}
	}
	return errs
}

// ValidateNodeClass implements vendors.NodeClassValidator: the syntax of the
// aws block and of the AWS blocks of the profiles (regions, instance profile
// name, metadata options, EBS types, sizes, IOPS and throughput against the EBS
// limits, tags, resource references) so that a mistake shows in the NodeClass's
// status and not in a failed launch. What needs the account (that a referenced
// subnet exists) is checked by the resource status, not here.
func (d *Driver) ValidateNodeClass(_ context.Context, nc *v1alpha1.SCNodeClass) error {
	var errs []error
	if b := nc.Spec.Vendors.AWS; b != nil {
		errs = append(errs, validateBlock(b)...)
	}
	for name, p := range nc.Spec.Profiles.Storage {
		if p.AWS != nil {
			errs = append(errs, validateStorage("profiles.storage."+name+".aws", p.AWS)...)
		}
	}
	for name, p := range nc.Spec.Profiles.Placement {
		if p.AWS != nil {
			errs = append(errs, validatePlacement("profiles.placement."+name+".aws", p.AWS)...)
		}
	}
	if len(nc.Spec.Tags) > maxUserTags {
		errs = append(errs, fmt.Errorf("tags: %d tags, at most %d (EC2 allows 50 per resource and SCarpenter adds six)",
			len(nc.Spec.Tags), maxUserTags))
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

func validateBlock(b *awsapi.Settings) []error {
	var errs []error
	for _, r := range b.Regions {
		if !regionPattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("vendors.aws.regions: %q is not an AWS region name", r))
		}
	}
	if b.InstanceProfile != "" && !instanceProfilePattern.MatchString(b.InstanceProfile) {
		errs = append(errs, fmt.Errorf("vendors.aws.instanceProfile: %q is not a valid instance profile name "+
			"(1 to 128 letters, digits and + = , . @ _ -)", b.InstanceProfile))
	}
	if m := b.MetadataOptions; m != nil {
		if m.HTTPPutResponseHopLimit != nil && (*m.HTTPPutResponseHopLimit < 1 || *m.HTTPPutResponseHopLimit > 64) {
			errs = append(errs, fmt.Errorf("vendors.aws.metadataOptions.httpPutResponseHopLimit is 1 to 64, not %d",
				*m.HTTPPutResponseHopLimit))
		}
		if m.HTTPTokens != "" && m.HTTPTokens != "required" && m.HTTPTokens != "optional" {
			errs = append(errs, fmt.Errorf("vendors.aws.metadataOptions.httpTokens is required or optional, not %q",
				m.HTTPTokens))
		}
	}
	errs = append(errs, validateStorage("vendors.aws.storage", b.Storage)...)
	if b.Networking != nil {
		errs = append(errs, validateInbound("vendors.aws.networking.inbound", b.Networking.Inbound)...)
	}
	if r := b.Resources; r != nil {
		errs = append(errs, validateResource("vendors.aws.resources.subnets", r.Subnets, subnetIDPattern)...)
		errs = append(errs, validateResource("vendors.aws.resources.securityGroups", r.SecurityGroups, groupIDPattern)...)
		errs = append(errs, validateResource("vendors.aws.resources.placementGroups", r.PlacementGroups,
			placementIDPattern)...)
	}
	return errs
}
