/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined with SHA-1, not a security use.
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

// Vendor is the vendor name of the driver: the catalog prefix (gcp.e2-small)
// and the vendor part of the provider ID.
const Vendor = "gcp"

// LabelPrefix starts every label key the driver writes: TagKey(k) is
// "scarpenter-<k>" (the contract's restrictive-charset mapping).
const LabelPrefix = "scarpenter-"

// TagKey maps a logical tag key to the label key (plan 16, section 3).
func TagKey(k vendors.TagKey) string { return LabelPrefix + string(k) }

// ----- provider ID and instance reference -----

// ProviderID is scarpenter://gcp/<region>/<nodeclaim>.
func ProviderID(region, nodeClaim string) string {
	return scope.ProviderID(Vendor, region, nodeClaim)
}

// ParseProviderID splits a GCP provider ID; an ID of another vendor is an error.
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

// NewRef builds the InstanceRef <region>/<zone>/<instance name>: the ID part
// holds the zone because every instance call needs it (InstanceRef.ID() cuts
// at the first slash, so an ID with a slash is fine).
func NewRef(region, zone, name string) vendors.InstanceRef {
	return vendors.NewInstanceRef(region, zone+"/"+name)
}

var (
	zoneRe = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*[0-9]+-[a-z]$`)
	// instanceNameRe is RFC 1035 as GCE applies it to resource names.
	instanceNameRe = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
)

// ParseRef splits an InstanceRef. ok is false for a malformed reference (the
// conformance suite's "<region>/i-never-existed"), which the driver answers
// with NotFound, never Fatal.
func ParseRef(ref vendors.InstanceRef) (region, zone, name string, ok bool) {
	region = ref.Region()
	zone, name, found := strings.Cut(ref.ID(), "/")
	if !found || !zoneRe.MatchString(zone) || !instanceNameRe.MatchString(name) || RegionOfZone(zone) != region {
		return "", "", "", false
	}
	return region, zone, name, true
}

// RegionOfZone returns the region of a zone name (europe-west3-a ->
// europe-west3).
func RegionOfZone(zone string) string {
	i := strings.LastIndexByte(zone, '-')
	if i < 0 {
		return zone
	}
	return zone[:i]
}

// InstanceName is the GCE instance name of a NodeClaim: the NodeClaim name when
// it is a valid instance name (RFC 1035: lower case, digits and dashes, starts
// with a letter, at most 63 characters), else scarpenter-<hash>.
func InstanceName(nodeClaim string) string {
	if instanceNameRe.MatchString(nodeClaim) {
		return nodeClaim
	}
	sum := sha256.Sum256([]byte(nodeClaim))
	return "scarpenter-" + hex.EncodeToString(sum[:])[:16]
}

// ----- catalog names to vendor names -----

// APIReference turns a catalog instance type name (gcp.e2-small) into the
// machine type name (e2-small); a name without the prefix is returned as is.
func APIReference(catalogName string) string {
	return strings.TrimPrefix(catalogName, Vendor+".")
}

// Family is the machine series of a machine type (n2-standard-8 -> n2).
func Family(machineType string) string {
	f, _, _ := strings.Cut(machineType, "-")
	return f
}

// MachineTypeURL is the partial URL of a machine type in a zone.
func MachineTypeURL(zone, machineType string) string {
	return "zones/" + zone + "/machineTypes/" + machineType
}

// DiskTypeURL is the partial URL of a disk type in a zone.
func DiskTypeURL(zone, diskType string) string {
	return "zones/" + zone + "/diskTypes/" + diskType
}

// ProjectPrefix returns the "projects/<p>/" part for a project.
func ProjectPrefix(project string) string { return "projects/" + project + "/" }

// fromProjects cuts a URL down to its projects/<p>/... part; ok is false when
// the reference has none.
func fromProjects(ref string) (string, bool) {
	if strings.HasPrefix(ref, "projects/") {
		return ref, true
	}
	if i := strings.Index(ref, "/projects/"); i >= 0 {
		return ref[i+1:], true
	}
	return "", false
}

// SubnetworkURL normalises a subnet reference to
// projects/<p>/regions/<r>/subnetworks/<n>: a full form is kept (a Shared VPC
// host project's subnet), a short name gets the controller's project and the
// region.
func SubnetworkURL(project, region, ref string) string {
	if u, ok := fromProjects(ref); ok && strings.Contains(u, "/subnetworks/") {
		return u
	}
	return ProjectPrefix(project) + "regions/" + region + "/subnetworks/" + ref[strings.LastIndex(ref, "/")+1:]
}

// ImageURL normalises an image reference to projects/<p>/global/images/<n>; a
// short name gets the project.
func ImageURL(project, ref string) string {
	if u, ok := fromProjects(ref); ok && strings.Contains(u, "/images/") {
		return u
	}
	return ProjectPrefix(project) + "global/images/" + ref[strings.LastIndex(ref, "/")+1:]
}

// ----- labels -----

var (
	labelValueRe = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
	labelKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
)

// maxLabels is GCE's limit of labels per resource.
const maxLabels = 64

// LabelValue maps any string to a valid label value, deterministically: valid
// values (lower-case ASCII letters, digits, "_" and "-", up to 63 characters) are
// unchanged; everything else is lower-cased with other characters replaced by
// "-", shortened to 54 characters, and given a hash of the original, so that
// two different inputs do not collide. It is the one function used for writing
// labels and for filtering, so that FindInstance and ListInstances match what
// CreateInstance wrote.
func LabelValue(v string) string {
	if labelValueRe.MatchString(v) {
		return v
	}
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if len(s) > 54 {
		s = s[:54]
	}
	sum := sha256.Sum256([]byte(v))
	return s + "-" + hex.EncodeToString(sum[:4])
}

// Labels builds the labels of an instance or disk: the logical tags under their
// TagKey names and the NodeClass's own tags (validated by ValidateUserTags).
func Labels(tags map[vendors.TagKey]string, extra map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(tags)+len(extra))
	for k, v := range tags {
		out[TagKey(k)] = LabelValue(v)
	}
	if err := ValidateUserTags(extra); err != nil {
		return nil, err
	}
	maps.Copy(out, extra)
	if len(out) > maxLabels {
		return nil, fmt.Errorf("%d labels exceed GCE's limit of %d", len(out), maxLabels)
	}
	return out, nil
}

// ValidateUserTags checks the NodeClass tags against the label charset: keys
// start with a letter and hold lower-case letters, digits, "_" and "-"; keys and
// values are at most 40 characters; no key uses the driver's prefix.
func ValidateUserTags(tags map[string]string) error {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := tags[k]
		switch {
		case !labelKeyRe.MatchString(k):
			return fmt.Errorf("tag key %q is not a valid GCE label key (lower-case letters, digits, _ and -, "+
				"starting with a letter)", k)
		case strings.HasPrefix(k, LabelPrefix):
			return fmt.Errorf("tag key %q uses the reserved prefix %q", k, LabelPrefix)
		case len(k) > 40 || len(v) > 40:
			return fmt.Errorf("tag %q is longer than 40 characters", k)
		case !labelValueRe.MatchString(v):
			return fmt.Errorf("tag %q has a value that is not a valid GCE label value", k)
		}
	}
	return nil
}

// TagsOf reads the logical tags back from the labels of an instance.
func TagsOf(labels map[string]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.TagKeys {
		if v, ok := labels[TagKey(k)]; ok {
			out[k] = v
		}
	}
	return out
}

// LabelFilter builds the server-side list filter that matches the labels of the
// logical tags (each value through LabelValue).
func LabelFilter(tags map[vendors.TagKey]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, string(k))
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("(labels.%s = %q)", TagKey(vendors.TagKey(k)), LabelValue(tags[vendors.TagKey(k)])))
	}
	return strings.Join(parts, " AND ")
}

// ----- ownership marker of the resources that have no labels -----

// ResourceNamePrefix is the name prefix of the resources SCarpenter manages for a
// cluster (networks, subnets, firewall rules, resource policies have no labels).
func ResourceNamePrefix(cluster string) string { return LabelPrefix + cluster + "-" }

// MarkerDescription is the description that, with the name prefix, marks a
// managed resource (ADR-018's fallback): both are required before the garbage
// collector touches it.
func MarkerDescription(cluster, nodeClass, kind string) string {
	return fmt.Sprintf("scarpenter: cluster=%s nodeclass=%s managed-by=scarpenter resource-kind=%s",
		cluster, nodeClass, kind)
}

// ParseMarker reads a description written by MarkerDescription; ok is false for
// any other description.
func ParseMarker(desc string) (cluster, nodeClass, kind string, ok bool) {
	rest, found := strings.CutPrefix(desc, "scarpenter: ")
	if !found {
		return "", "", "", false
	}
	kv := map[string]string{}
	for f := range strings.FieldsSeq(rest) {
		if k, v, has := strings.Cut(f, "="); has {
			kv[k] = v
		}
	}
	if kv["managed-by"] != "scarpenter" || kv["cluster"] == "" || kv["resource-kind"] == "" {
		return "", "", "", false
	}
	return kv["cluster"], kv["nodeclass"], kv["resource-kind"], true
}

// ----- request IDs -----

// requestIDNamespace is the UUIDv5 namespace of the request IDs.
var requestIDNamespace = [16]byte{
	0x5c, 0xa4, 0x9e, 0x2d, 0x6a, 0x0b, 0x4c, 0x8e, 0x9b, 0x13, 0x61, 0x7c, 0x50, 0x65, 0x72, 0x73,
}

// RequestID is the requestId of an instances.insert: a UUIDv5 of the NodeClaim
// UID, zone, machine type and capacity type, so that only a retry of the same
// offering shares an ID (a replay in another zone must not). The ID also carries
// the driver's epoch (a random value per process start) and the attempt counter
// of the offering: GCE answers a replay of a request ID with the operation of
// the first request, and whether that holds for an operation that failed is
// unverified. After a failed insert the driver therefore counts the attempt up,
// so that the next try of the same offering is a new request and not the replay
// of a failure; a retry after a lost response keeps the counter, so it is still
// the replay of the original request. A restart changes the epoch: what an
// earlier process created is found by its idempotency label before any insert.
func RequestID(uid, zone, machineType, capacityType, epoch string, attempt int) string {
	return uuidV5(requestIDNamespace, strings.Join([]string{
		uid, zone, machineType, capacityType, epoch, strconv.Itoa(attempt)}, "\x00"))
}

func uuidV5(ns [16]byte, name string) string {
	h := sha1.New() //nolint:gosec // UUIDv5 is defined with SHA-1.
	h.Write(ns[:])
	h.Write([]byte(name))
	s := h.Sum(nil)[:16]
	s[6] = (s[6] & 0x0f) | 0x50
	s[8] = (s[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}
