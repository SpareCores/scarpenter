/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// ProviderID is scarpenter://vultr/<region>/<nodeclaim>.
func ProviderID(region, nodeClaim string) string {
	return scope.ProviderID(Vendor, region, nodeClaim)
}

// ParseProviderID splits a Vultr provider ID into region and NodeClaim name.
func ParseProviderID(id string) (region, nodeClaim string, err error) {
	vendor, region, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if vendor != Vendor {
		return "", "", fmt.Errorf("provider ID %q is not a %s provider ID", id, Vendor)
	}
	return region, nodeClaim, nil
}

// Zone is the one dummy zone of a region: Vultr has no zones, Keeper's zone ID
// equals the region ID, and so does the value of topology.kubernetes.io/zone.
func Zone(region string) vendors.Zone { return vendors.Zone{ID: region, Name: region} }

// RegionsOf maps the regions of GET /v2/regions to driver regions, sorted by
// ID. allowed is the NodeClass's region list; empty means every region the API
// lists.
func RegionsOf(listed []govultr.Region, allowed []string) []vendors.Region {
	var out []vendors.Region
	for _, r := range listed {
		if len(allowed) == 0 || slices.Contains(allowed, r.ID) {
			out = append(out, vendors.Region{Name: r.ID})
		}
	}
	slices.SortFunc(out, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// PlanOf is the Vultr plan of a catalog instance type: Keeper's api_reference
// (vc2-2c-4gb). Vultr has no Arm instances, so an arm64 type is Unsupported.
func PlanOf(t vendors.CatalogType) (string, error) {
	if t.Arch != "" && t.Arch != "amd64" && t.Arch != "x86_64" {
		return "", vendors.NewError(vendors.Unsupported, codeUnsupportedArch,
			fmt.Errorf("%s has no %s instances", Vendor, t.Arch))
	}
	if t.APIReference == "" {
		return "", vendors.NewError(vendors.Fatal, codeInvalid, fmt.Errorf("instance type without an API reference"))
	}
	return t.APIReference, nil
}

// RootDiskForType is the root disk of a plan, which is fixed by the plan (the
// plan's `disk`, Keeper's storage_size, in GB). It is the body of the optional
// NodeOptions.RootDiskForType of plan 14 (A2). GB is read as 10^9 bytes, the
// conservative reading for the scheduler's ephemeral-storage estimate. 0 when
// the type has no local disk.
func RootDiskForType(t vendors.CatalogType) int64 {
	return int64(t.LocalStorageGB * 1e9)
}

// TagStyle is how a logical tag key is spelled inside a tag string. Vultr
// instance tags are plain strings, so a tag is "<key>=<value>". The charset
// and length limits of tags are unverified (spike S6): if the slash or the dot
// is refused, the dash style is the fallback of overview section 7.1.
type TagStyle int

// The tag styles.
const (
	// TagStyleDomain is scarpenter.sparecores.com/<key>, as AWS.
	TagStyleDomain TagStyle = iota
	// TagStyleDash is scarpenter-<key>.
	TagStyleDash
)

// Key maps a logical key.
func (s TagStyle) Key(k vendors.TagKey) string {
	if s == TagStyleDash {
		return "scarpenter-" + string(k)
	}
	return vendors.DefaultTagKey(k)
}

// Tag is the tag string of a key and value.
func (s TagStyle) Tag(k vendors.TagKey, v string) string { return s.Key(k) + "=" + v }

// Tags builds the tag strings of an instance: the NodeClass's own tags as
// key=value, overridden by the logical tags, in a fixed order. The driver adds
// the idempotency key itself (vendors.IdempotencyTag).
func (s TagStyle) Tags(logical map[vendors.TagKey]string, extra map[string]string) []string {
	m := map[string]string{}
	maps.Copy(m, extra)
	for k, v := range logical {
		m[s.Key(k)] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// ParseTags reads the logical tags back from an instance's tag strings; tags
// that are not ours are ignored.
func (s TagStyle) ParseTags(tags []string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, t := range tags {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		for _, lk := range vendors.TagKeys {
			if k == s.Key(lk) {
				out[lk] = v
			}
		}
	}
	return out
}

// Marker is the ownership marker of a managed firewall group or VPC (ADR-018),
// written into the description because neither object has tags.
type Marker struct {
	Cluster, NodeClass, ResourceKind string
}

// FormatDescription is "<name> | cluster=<c> nodeclass=<n> managed-by=scarpenter
// resource-kind=<k>". The values must be free of spaces and "|". The length and
// charset limits of a description are unverified (spike S6).
func FormatDescription(name string, m Marker) (string, error) {
	for _, v := range []string{name, m.Cluster, m.NodeClass, m.ResourceKind} {
		if v == "" || strings.ContainsAny(v, " |=") {
			return "", fmt.Errorf("%q cannot be part of a resource description", v)
		}
	}
	return fmt.Sprintf("%s | cluster=%s nodeclass=%s managed-by=%s resource-kind=%s",
		name, m.Cluster, m.NodeClass, vendors.ManagedByValue, m.ResourceKind), nil
}

// ParseDescription reads a description FormatDescription wrote. ok is false for
// any other description, and for one whose managed-by is not scarpenter: the
// garbage collector touches only what carries both name and marker.
func ParseDescription(desc string) (name string, m Marker, ok bool) {
	name, rest, found := strings.Cut(desc, " | ")
	if !found || name == "" || strings.ContainsAny(name, " |=") {
		return "", Marker{}, false
	}
	fields := map[string]string{}
	for f := range strings.SplitSeq(rest, " ") {
		k, v, cut := strings.Cut(f, "=")
		if _, dup := fields[k]; !cut || v == "" || dup {
			return "", Marker{}, false
		}
		fields[k] = v
	}
	if len(fields) != 4 || fields["managed-by"] != vendors.ManagedByValue {
		return "", Marker{}, false
	}
	m = Marker{Cluster: fields["cluster"], NodeClass: fields["nodeclass"], ResourceKind: fields["resource-kind"]}
	if m.Cluster == "" || m.NodeClass == "" || m.ResourceKind == "" {
		return "", Marker{}, false
	}
	return name, m, true
}

// Plan families, by the plan ID's prefix up to the first dash. Every family of
// Vultr's plan list must be classified explicitly (a new one is "unclassified"
// and unsupported until someone decides), as AWS's mac rule.
var planFamilies = map[string]struct {
	supported bool
	reason    string
}{
	"vc2": {true, ""},
	"vhf": {true, ""},
	"vhp": {true, ""},
	"voc": {true, ""},
	// Supported only with a local disk, see ClassifyPlan.
	"vx1": {true, ""},
	"vcg": {false, "Cloud GPU plans are not launched in v1"},
	"vdm": {false, "GPU bare metal is another API and outside v1"},
	"vbm": {false, "bare metal is another API and outside v1"},
}

// freePlan is dropped: no stock guarantee.
const freePlan = "vc2-1c-0.5gb-free"

// ClassifyPlan says whether the driver launches a Vultr plan, and why not.
// diskGB is the plan's local disk in GB (Keeper's storage_size, which is 1 for
// a diskless plan; govultr's Plan has no storage_type field, so the plan's
// `disk` is the signal). A vx1 plan with no local disk boots from a volume,
// which v1 does not do (plan 20, section 5; S2 decides local_and_block_storage).
func ClassifyPlan(id string, diskGB int) (ok bool, reason string) {
	if id == freePlan {
		return false, "the free plan has no stock guarantee"
	}
	family, _, _ := strings.Cut(id, "-")
	f, known := planFamilies[family]
	switch {
	case !known:
		return false, fmt.Sprintf("plan family %q is unclassified", family)
	case !f.supported:
		return false, f.reason
	case family == "vx1" && diskGB <= 1:
		return false, "diskless vx1 plans boot from a block storage volume, which v1 does not support"
	}
	return true, ""
}
