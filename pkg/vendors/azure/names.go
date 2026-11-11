/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Vendor is the Keeper vendor ID.
const Vendor = "azure"

// Architectures and the boolean setting values, as the API spells them.
const (
	archAMD64   = "amd64"
	archARM64   = "arm64"
	settingTrue = "true"
)

// Publisher and offers of the official Flatcar marketplace images (plan 17
// section 1).
const (
	DefaultPublisher = "kinvolk"
	OfferAMD64       = "flatcar-container-linux-corevm-amd64"
	OfferARM64       = "flatcar-container-linux-corevm"
)

const (
	// maxVMName is the longest VM name (it is also the computer name).
	maxVMName = 64
	// ZoneNone is Keeper's name of the zone of a region without zones.
	ZoneNone = "0"
	// AdminUser is the VM's administrator account; access is Ignition's.
	AdminUser = "core"
)

// hash8 is the first 8 hex digits of the SHA-256 of s.
func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// VMName is the deterministic VM name <nodeclaim>-<hash8(cluster)>, at most 64
// characters: it takes the place of the idempotency label (plan 14, A3). A
// NodeClaim name too long for it is cut and given the hash of the full name,
// so that two long names stay distinct.
func VMName(cluster, nodeClaim string) string {
	suffix := "-" + hash8(cluster)
	if len(nodeClaim)+len(suffix) <= maxVMName {
		return nodeClaim + suffix
	}
	keep := maxVMName - len(suffix) - 9
	return nodeClaim[:keep] + "-" + hash8(nodeClaim) + suffix
}

// TagKey maps a logical tag key to an Azure tag name: scarpenter-<key> (the
// default key has slashes, which Azure forbids). The opt-in tag
// scarpenter-use is a different tag and never a logical key.
func TagKey(k vendors.TagKey) string { return "scarpenter-" + string(k) }

// TagNameName is the tag every instance carries besides the logical ones.
const TagNameName = "Name"

// Azure's tag limits.
const (
	maxTags       = 50
	maxTagValue   = 256
	maxTagNameLen = 512
)

var forbiddenTagChars = regexp.MustCompile(`[<>%&\\?/]`)

// ValidateTags checks the tags of a resource against Azure's rules: at most 50,
// names without < > % & \ ? / (at most 512 characters, case-insensitively
// unique), values at most 256 characters.
func ValidateTags(tags map[string]string) error {
	if len(tags) > maxTags {
		return invalidf("%d tags, Azure allows %d", len(tags), maxTags)
	}
	seen := make(map[string]string, len(tags))
	for k, v := range tags {
		switch {
		case k == "", len(k) > maxTagNameLen, forbiddenTagChars.MatchString(k):
			return invalidf("tag name %q is not valid for Azure", k)
		case len(v) > maxTagValue:
			return invalidf("value of tag %q exceeds %d characters", k, maxTagValue)
		}
		if other, dup := seen[strings.ToLower(k)]; dup {
			return vendors.NewError(vendors.Fatal, codeInvalidSettings,
				fmt.Errorf("tag names %q and %q are the same in Azure (names are case insensitive)", other, k))
		}
		seen[strings.ToLower(k)] = k
	}
	return nil
}

// Tags returns the Azure tags of a node: the logical tags through TagKey, the
// Name tag (the NodeClaim name) and the NodeClass's own tags, which cannot
// replace the logical ones.
func Tags(logical map[vendors.TagKey]string, extra map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(logical)+len(extra)+1)
	maps.Copy(out, extra)
	reserved := map[string]bool{strings.ToLower(TagNameName): true}
	for k := range logical {
		reserved[strings.ToLower(TagKey(k))] = true
	}
	for k := range extra {
		if reserved[strings.ToLower(k)] {
			return nil, invalidf("tag %q is reserved for SCarpenter", k)
		}
	}
	for k, v := range logical {
		out[TagKey(k)] = v
	}
	if nc := logical[vendors.TagNodeClaim]; nc != "" {
		out[TagNameName] = nc
	}
	if err := ValidateTags(out); err != nil {
		return nil, err
	}
	return out, nil
}

// LogicalTags reads the logical tags back from an Azure tag map; names match
// case-insensitively, as Azure's do.
func LogicalTags(tags map[string]*string) map[vendors.TagKey]string {
	byLower := make(map[string]string, len(tags))
	for k, v := range tags {
		if v != nil {
			byLower[strings.ToLower(k)] = *v
		}
	}
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.TagKeys {
		if v, ok := byLower[strings.ToLower(TagKey(k))]; ok {
			out[k] = v
		}
	}
	return out
}

// ProviderID is scarpenter://azure/<region>/<nodeclaim>.
func ProviderID(region, nodeClaim string) string { return scope.ProviderID(Vendor, region, nodeClaim) }

// ParseProviderID splits an Azure provider ID into region and NodeClaim name.
func ParseProviderID(id string) (region, nodeClaim string, err error) {
	v, region, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if v != Vendor {
		return "", "", fmt.Errorf("provider ID %q is not an Azure one", id)
	}
	return region, nodeClaim, nil
}

// InstanceRefOf is <region>/<resourceGroup>/<vmName>: the ID part holds the
// resource group, which Azure needs for every call.
func InstanceRefOf(region, resourceGroup, vmName string) vendors.InstanceRef {
	return vendors.NewInstanceRef(region, resourceGroup+"/"+vmName)
}

// ParseInstanceRef splits a reference. ok is false for anything that is not
// <region>/<group>/<vm>; the driver treats such a reference as an instance
// that does not exist.
func ParseInstanceRef(ref vendors.InstanceRef) (region, resourceGroup, vmName string, ok bool) {
	parts := strings.Split(string(ref), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// ImageRef is a marketplace image URN.
type ImageRef struct {
	Publisher, Offer, SKU, Version string
}

// URN is publisher:offer:sku:version.
func (i ImageRef) URN() string {
	return strings.Join([]string{i.Publisher, i.Offer, i.SKU, i.Version}, ":")
}

// ParseURN parses publisher:offer:sku:version.
func ParseURN(urn string) (ImageRef, error) {
	p := strings.Split(urn, ":")
	if len(p) != 4 || slicesContainsEmpty(p) {
		return ImageRef{}, fmt.Errorf("image reference %q is not publisher:offer:sku:version", urn)
	}
	return ImageRef{Publisher: p[0], Offer: p[1], SKU: p[2], Version: p[3]}, nil
}

func slicesContainsEmpty(s []string) bool { return slices.Contains(s, "") }

// zoneName returns the zone name of a VM: its single logical zone, or "0".
func zoneName(zones []*string) string {
	if len(zones) == 0 || zones[0] == nil || *zones[0] == "" {
		return ZoneNone
	}
	return *zones[0]
}

// Resource kinds of the managed-resources machinery (plan 17 section 2). The
// application security groups of the isolation rules are not kinds of their own:
// each belongs to a network security group of the driver (its name plus the
// suffix asgSuffix), is created and deleted with it, and never listed.
const (
	KindResourceGroups = vendors.ResourceKind("resourceGroups")
	KindSubnets        = vendors.ResourceKind("subnets")
	KindSecurityGroups = vendors.ResourceKind("securityGroups")
	KindPlacement      = vendors.ResourceKind("proximityPlacementGroups")
	// KindAppSecGroups only names the application security group a managed
	// network security group brings, in Prepared (the NIC joins it); it is not a
	// kind of Kinds.
	KindAppSecGroups = vendors.ResourceKind("applicationSecurityGroups")
	// KindRunSecurityGroups is the network security group of a run key (isolation
	// run): a NIC takes one group, so the run has its own, with its own
	// application security group for the nodes of the run.
	KindRunSecurityGroups = vendors.ResourceKind("runSecurityGroups")
)
