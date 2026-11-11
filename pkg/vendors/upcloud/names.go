/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package upcloud is the foundation of the UpCloud driver (plan 19): the
// narrow API and its in-memory fake (subpackages api and fake), error
// classification, the mapping of catalog names to UpCloud's (zones, plans,
// labels, provider and instance references, server states), the launch
// request builder, the Public & Utility firewall rules, the lifecycle helpers
// (idempotent lookup by label, stop-then-delete) and the account quota reader.
//
// It does not implement vendors.Driver yet: the seams for the rest are marked
// TODO(plan 14) and sit where the shared groundwork (the images kind, the unified
// use selector, RootDiskForType) plugs in. UpCloud's regions are its zones: a
// region and a dummy zone carry the same ID.
package upcloud

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Vendor is the Keeper vendor ID.
const Vendor = "upcloud"

// Label keys of the driver. Label keys are 2 to 32 printable ASCII characters,
// and scarpenter.sparecores.com/idempotency-key has 33 and more: the logical tags
// are written as scarpenter-<key> (26 characters for the longest).
const (
	LabelPrefix = "scarpenter-"
	// LabelCreated holds the creation time (RFC 3339, UTC; epoch seconds are
	// read as well), written in the create call: BillingStart reads it back.
	LabelCreated = "scarpenter-created"
	// LabelUse marks resources an operator made for SCarpenter to use; never
	// touched by the sweeper.
	LabelUse = "scarpenter-use"
	// LabelResourceKind marks the kind of a managed resource.
	LabelResourceKind = "scarpenter-resource-kind"
)

// Limits of label keys and counts.
const (
	MinLabelKey    = 2
	MaxLabelKey    = 32
	MaxLabelValue  = 255
	MaxLabels      = 255
	maxServerNameL = 128
)

// TagKey maps a logical tag key to the label key.
func TagKey(k vendors.TagKey) string { return LabelPrefix + string(k) }

var tagByLabel = func() map[string]vendors.TagKey {
	m := map[string]vendors.TagKey{}
	for _, k := range vendors.TagKeys {
		m[TagKey(k)] = k
	}
	return m
}()

// TagsOf reads the logical tags back from labels; other labels are ignored.
func TagsOf(labels []upcloud.Label) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, l := range labels {
		if k, ok := tagByLabel[l.Key]; ok {
			out[k] = l.Value
		}
	}
	return out
}

// ValidateLabelKey checks a label key: 2 to 32 printable ASCII characters, not
// starting with an underscore (reserved).
func ValidateLabelKey(k string) error {
	if len(k) < MinLabelKey || len(k) > MaxLabelKey {
		return fmt.Errorf("label key %q must be %d to %d characters", k, MinLabelKey, MaxLabelKey)
	}
	if strings.HasPrefix(k, "_") {
		return fmt.Errorf("label key %q must not start with an underscore", k)
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("label key %q must be printable ASCII without spaces", k)
		}
	}
	return nil
}

// ValidateExtraTags checks the NodeClass's own tags (spec.tags) as UpCloud
// labels, for NodeClassValidator: the key rules, the 255 character values, the
// reserved scarpenter- prefix and the total of 255 labels with room for the
// driver's own.
func ValidateExtraTags(tags map[string]string) error {
	if len(tags)+len(vendors.TagKeys)+1 > MaxLabels {
		return fmt.Errorf("at most %d labels fit on a server", MaxLabels)
	}
	for _, k := range sortedKeys(tags) {
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if strings.HasPrefix(k, LabelPrefix) {
			return fmt.Errorf("label key %q uses the reserved prefix %s", k, LabelPrefix)
		}
		if len(tags[k]) > MaxLabelValue {
			return fmt.Errorf("the value of label %q is longer than %d characters", k, MaxLabelValue)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// ServerLabels builds the labels of a server: the logical tags as
// scarpenter-<key>, the idempotency key (the NodeClaim UID; there is no create
// token), the creation time and the NodeClass's own tags. The order is fixed.
func ServerLabels(
	tags map[vendors.TagKey]string, extra map[string]string, idempotencyKey string, now time.Time,
) ([]upcloud.Label, error) {
	if err := ValidateExtraTags(extra); err != nil {
		return nil, err
	}
	tags = vendors.IdempotencyTag(tags, idempotencyKey)
	m := map[string]string{LabelCreated: now.UTC().Format(time.RFC3339)}
	for k, v := range tags {
		m[TagKey(k)] = v
	}
	maps.Copy(m, extra)
	out := make([]upcloud.Label, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, upcloud.Label{Key: k, Value: m[k]})
	}
	return out, nil
}

// Created reads the creation time from the labels (RFC 3339, or epoch seconds
// as the fallback if UpCloud turns out to refuse colons in values).
func Created(labels []upcloud.Label) (time.Time, bool) {
	for _, l := range labels {
		if l.Key != LabelCreated {
			continue
		}
		if t, err := time.Parse(time.RFC3339, l.Value); err == nil {
			return t, true
		}
		if n, err := strconv.ParseInt(l.Value, 10, 64); err == nil {
			return time.Unix(n, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

// ProviderID is scarpenter://upcloud/<zone>/<nodeclaim>.
func ProviderID(zone, nodeClaim string) string { return scope.ProviderID(Vendor, zone, nodeClaim) }

// ParseProviderID splits an UpCloud provider ID into zone and NodeClaim name.
func ParseProviderID(id string) (zone, nodeClaim string, err error) {
	vendor, zone, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if vendor != Vendor {
		return "", "", fmt.Errorf("provider ID %q is not an UpCloud one", id)
	}
	return zone, nodeClaim, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsUUID reports whether s has the shape of an UpCloud UUID.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

// NewRef builds the instance reference <zone>/<server uuid>.
func NewRef(zone, uuid string) vendors.InstanceRef { return vendors.NewInstanceRef(zone, uuid) }

// ParseRef splits an instance reference. ok is false for a reference that
// cannot exist (no zone, an ID that is not a UUID): the contract says the
// driver answers NotFound for it, never Fatal (plan 14, A9).
func ParseRef(ref vendors.InstanceRef) (zone, uuid string, ok bool) {
	zone, uuid = ref.Region(), ref.ID()
	return zone, uuid, zone != "" && IsUUID(uuid)
}

// InstanceState maps an UpCloud server state to the contract's. A server that
// is stopped (by a stop call, a guest poweroff or found so) or in error is
// terminating (plan 14, A4): it is not coming back and the garbage collector
// deletes it. maintenance is the state while the storage is cloned and while a
// stop is in progress: pending. A state UpCloud adds later is pending too.
func InstanceState(s string) string {
	switch s {
	case upcloud.ServerStateStarted:
		return vendors.StateRunning
	case upcloud.ServerStateStopped, upcloud.ServerStateError:
		return vendors.StateTerminating
	default:
		return vendors.StatePending
	}
}

// Addresses lists a server's addresses: public IPv4 and IPv6 first, then the
// utility address and any SDN address, for the serving-certificate approver.
func Addresses(ips []upcloud.IPAddress) []string {
	rank := map[string]int{
		upcloud.IPAddressAccessPublic: 0, upcloud.IPAddressAccessUtility: 1, upcloud.IPAddressAccessPrivate: 2,
	}
	sorted := slices.Clone(ips)
	slices.SortStableFunc(sorted, func(a, b upcloud.IPAddress) int { return rank[a.Access] - rank[b.Access] })
	var out []string
	for _, ip := range sorted {
		if ip.Address != "" && !slices.Contains(out, ip.Address) {
			out = append(out, ip.Address)
		}
	}
	return out
}

// ToInstance converts server details to the contract's instance. CreatedAt is
// read from the creation label and is zero when the server has none.
func ToInstance(d *upcloud.ServerDetails) *vendors.Instance {
	created, _ := Created(d.Labels)
	return &vendors.Instance{
		Ref: NewRef(d.Zone, d.UUID), Region: d.Zone, Zone: d.Zone, Type: d.Plan,
		CapacityType: vendors.CapacityOnDemand, State: InstanceState(d.State), CreatedAt: created,
		Tags: TagsOf(d.Labels), Addresses: Addresses(d.IPAddresses),
	}
}

// BillingStart is the start of the server's billing period: the creation label
// (written before the server exists, so early: the safe side). Without it there
// is none and no retention for the node (plan 19, "Billing period").
func BillingStart(d *upcloud.ServerDetails) (time.Time, bool) { return Created(d.Labels) }

// ValidateName checks a server name (hostname and title): the NodeClaim name,
// lower case, at most 128 characters.
func ValidateName(n string) error {
	if n == "" || len(n) > maxServerNameL {
		return fmt.Errorf("server name %q must be 1 to %d characters", n, maxServerNameL)
	}
	if n != strings.ToLower(n) {
		return fmt.Errorf("server name %q must be lower case", n)
	}
	return nil
}

// Zone is the dummy zone of a region: UpCloud has no zones inside a data
// centre, so the zone ID and name are the region.
func Zone(region string) vendors.Zone { return vendors.Zone{ID: region, Name: region} }
