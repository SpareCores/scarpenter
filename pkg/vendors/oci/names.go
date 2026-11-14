/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Name is the registration name and the Keeper vendor ID of the driver.
const Name = "oci"

// Vendor is Name, for the catalog prefix (oci.VM.Standard.E5.Flex.2-16) and the
// vendor part of the provider ID.
const Vendor = Name

// TagPrefix starts every freeform tag key the driver writes: TagKey(k) is
// "scarpenter-<k>" (the restrictive mapping of the contract; plan 25, section 5).
// The prefix is reserved: a NodeClass's own tags may not use it.
const TagPrefix = "scarpenter-"

// Limits of OCI freeform tags.
const (
	maxTagKey   = 100
	maxTagValue = 256
	maxTags     = 64
)

// Resource kinds of the driver (plan 25, section 4). Step (c) implements
// ManagedResources over them; the launch reads the prepared ones.
const (
	KindSubnets                 vendors.ResourceKind = "subnets"
	KindNetworkSecurityGroups   vendors.ResourceKind = "networkSecurityGroups"
	KindRunNetworkSecurityGroup vendors.ResourceKind = "runNetworkSecurityGroups"
	KindImages                  vendors.ResourceKind = "images"
)

// TagKey maps a logical tag key to the freeform tag key.
func TagKey(k vendors.TagKey) string { return TagPrefix + string(k) }

// TagsOf reads the logical tags back from an instance's freeform tags: the
// driver's own keys only (foreign tags are not ours).
func TagsOf(freeform map[string]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range append(slices.Clone(vendors.TagKeys), vendors.TagResource) {
		if v, ok := freeform[TagKey(k)]; ok {
			out[k] = v
		}
	}
	return out
}

// FreeformTags builds the freeform tags of an instance: the logical tags and the
// NodeClass's own tags. A NodeClass tag in the reserved scarpenter- prefix or
// beyond OCI's limits is an error (ADR-017: refused, never changed).
func FreeformTags(tags map[vendors.TagKey]string, extra map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(tags)+len(extra))
	for k, v := range extra {
		if strings.HasPrefix(k, TagPrefix) {
			return nil, fmt.Errorf("the tag %q uses the reserved prefix %q", k, TagPrefix)
		}
		out[k] = v
	}
	for k, v := range tags {
		out[TagKey(k)] = v
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("%d tags; OCI allows %d freeform tags", len(out), maxTags)
	}
	for _, k := range slices.Sorted(maps.Keys(out)) {
		if k == "" || len(k) > maxTagKey {
			return nil, fmt.Errorf("the tag key %q is empty or longer than %d characters", k, maxTagKey)
		}
		if len(out[k]) > maxTagValue {
			return nil, fmt.Errorf("the value of the tag %q is longer than %d characters", k, maxTagValue)
		}
	}
	return out, nil
}

// ----- provider ID and instance reference -----

// ProviderID is scarpenter://oci/<region>/<nodeclaim>.
func ProviderID(region, nodeClaim string) string { return scope.ProviderID(Vendor, region, nodeClaim) }

// ParseProviderID splits an OCI provider ID; an ID of another vendor is an error.
func ParseProviderID(id string) (region, nodeClaim string, err error) {
	v, region, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if v != Vendor {
		return "", "", fmt.Errorf("provider ID %q is not a %s provider ID", id, Vendor)
	}
	return region, nodeClaim, nil
}

// ocidRe is the shape of an OCID: ocid1.<type>.<realm>.[<region>].<unique ID>,
// the type of an instance being "instance".
var ocidRe = regexp.MustCompile(`^ocid1\.[a-z]+\.oc[0-9]+\.[a-z0-9-]*\.[A-Za-z0-9._-]+$`)

// IsOCID reports whether s has the shape of an OCID of the given resource type
// ("instance", "compartment", "tenancy"); an empty type accepts any.
func IsOCID(s, resourceType string) bool {
	if !ocidRe.MatchString(s) {
		return false
	}
	return resourceType == "" || strings.HasPrefix(s, "ocid1."+resourceType+".")
}

// NewRef builds the InstanceRef <region>/<instance OCID>.
func NewRef(region, ocid string) vendors.InstanceRef { return vendors.NewInstanceRef(region, ocid) }

// ParseRef splits an InstanceRef. ok is false for a malformed reference (a
// region that is empty, an ID that is not an instance OCID), which the driver
// answers with NotFound, never Fatal.
func ParseRef(ref vendors.InstanceRef) (region, ocid string, ok bool) {
	region, ocid = ref.Region(), ref.ID()
	if region == "" || !IsOCID(ocid, "instance") {
		return "", "", false
	}
	return region, ocid, true
}

// APIReference turns a catalog instance type name (oci.VM.Standard.E5.Flex.2-16)
// into the type name the driver parses; a name without the prefix is returned
// as is.
func APIReference(catalogName string) string { return strings.TrimPrefix(catalogName, Vendor+".") }

// ----- availability domains -----

// ADName strips the tenancy prefix of an availability domain's name
// ("Xxxx:EU-FRANKFURT-1-AD-1" -> "EU-FRANKFURT-1-AD-1"): the prefix differs by
// tenancy and its colon is not a character of a label value, so the zone name
// (the value of topology.kubernetes.io/zone) never carries it.
func ADName(full string) string {
	if _, rest, ok := strings.Cut(full, ":"); ok {
		return rest
	}
	return full
}

var adNumberRe = regexp.MustCompile(`-AD-([0-9]+)$`)

// ADNumber is the number of the availability domain: the N of the name's suffix
// "-AD-<N>".
func ADNumber(name string) (int, bool) {
	m := adNumberRe.FindStringSubmatch(ADName(name))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// ZoneID is Keeper's zone ID of an availability domain of the region:
// <region>-ad-<N> (PHX-AD-1 of us-phoenix-1 -> us-phoenix-1-ad-1). The number
// is the tenancy's own numbering, not a portable identity (plan 25, section 6).
func ZoneID(region, adName string) (string, bool) {
	n, ok := ADNumber(adName)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s-ad-%d", region, n), true
}

// ----- the retry token -----

// RetryToken is the opc-retry-token of a launch: "scarpenter-" and 32 hex
// characters (43 in all, below OCI's limit) of the SHA-256 over the NodeClaim UID
// and the offering (region, availability domain, shape, size, capacity type), the
// per-process epoch and the attempt counter. Only a replay of the same offering
// shares a token, and the counter counts up after a definitive failure
// (plan 25, section 5).
func RetryToken(key, region, ad string, t InstanceType, capacityType, epoch string, attempt int) string {
	h := sha256.New()
	for _, f := range []string{key, region, ad, t.Shape, strconv.Itoa(t.OCPUs), strconv.Itoa(t.GB), capacityType, epoch,
		strconv.Itoa(attempt)} {
		// A length prefix keeps the fields from running into each other.
		_, _ = fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return TagPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}
