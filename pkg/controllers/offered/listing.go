/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// ListingKey is the key in the binaryData of the ConfigMap that holds the
// listing: the JSON of Listing, gzipped.
const ListingKey = "types.json.gz"

// MaxListingBytes caps the compressed listing: a ConfigMap holds at most 1 MiB.
// Past the cap the zones are left out, and past that nothing is written.
const MaxListingBytes = 800 << 10

// Listing is the content of the ConfigMap of one class and vendor: what the
// catalog offers Karpenter and why the rest is not offered. The layout is a
// contract with automation (how-to: which types does a class offer).
type Listing struct {
	// GeneratedAt is when the listing was written; it is rewritten at least
	// every ten minutes, so readers can tell a stale listing.
	GeneratedAt string `json:"generatedAt"`
	Class       string `json:"class"`
	Vendor      string `json:"vendor"`
	// Truncated is always false, kept for readers that index it: no type is ever
	// left out of the listing.
	Truncated bool `json:"truncated"`
	// ZonesOmitted says that the listing was over the cap, so every offering has
	// an empty list of zones.
	ZonesOmitted bool `json:"zonesOmitted,omitempty"`
	// Types are keyed by the instance type name.
	Types map[string]ListingType `json:"types"`
}

// ListingType is a type of the catalog, offered or not.
type ListingType struct {
	// APIReference is the value of the api-reference label, omitted when the type
	// has no such label.
	APIReference string `json:"apiReference,omitempty"`
	// Available is true when at least one offering is available.
	Available bool `json:"available"`
	// CapacityTypes are the capacity types with an available offering.
	CapacityTypes []string `json:"capacityTypes"`
	// Reason is the dominant reason code and Detail its data as text for humans;
	// both only when the type is not available.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Offerings are grouped by capacity type and state; empty for a type that
	// is not in the catalog at all.
	Offerings []ListingOffering `json:"offerings"`
}

// ListingOffering is the zones that share a capacity type, a state and an
// expiry. For an offering the unavailable-offerings cache marked, Reason is the
// launch-error class (InsufficientCapacity, QuotaExceeded, DailyLimit, ...) and
// Until the RFC3339 time the mark expires.
type ListingOffering struct {
	CapacityType string   `json:"capacityType"`
	Zones        []string `json:"zones"`
	Available    bool     `json:"available"`
	Reason       string   `json:"reason,omitempty"`
	Until        string   `json:"until,omitempty"`
}

// newListing builds the listing of one class and vendor from the types of the
// report.
func newListing(class, vendor, generatedAt string, types []catalog.TypeReport) Listing {
	l := Listing{GeneratedAt: generatedAt, Class: class, Vendor: vendor, Types: make(map[string]ListingType, len(types))}
	for _, t := range types {
		l.Types[t.InstanceType] = listingType(t)
	}
	return l
}

// withoutZones is the listing with the zones of every offering emptied.
func (l Listing) withoutZones() Listing {
	types := make(map[string]ListingType, len(l.Types))
	for name, lt := range l.Types {
		lt.Offerings = slices.Clone(lt.Offerings)
		for i := range lt.Offerings {
			lt.Offerings[i].Zones = []string{}
		}
		types[name] = lt
	}
	l.Types, l.ZonesOmitted = types, true
	return l
}

// encode is the gzipped JSON of the listing, at most limit bytes. A listing that
// does not fit is encoded without the zones (zonesOmitted); one that still does
// not fit is an error. No type is ever dropped.
func encode(l Listing, limit int) (data []byte, zonesOmitted bool, err error) {
	data, err = gzipJSON(l)
	if err != nil || len(data) <= limit {
		return data, false, err
	}
	if data, err = gzipJSON(l.withoutZones()); err != nil || len(data) <= limit {
		return data, true, err
	}
	return nil, true, fmt.Errorf("the offered listing of class %s vendor %s is %d bytes compressed even without zones, "+
		"over the %d cap", l.Class, l.Vendor, len(data), limit)
}

// gzipJSON is deterministic: the gzip header carries no time.
func gzipJSON(l Listing) ([]byte, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("encoding the listing: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("compressing the listing: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compressing the listing: %w", err)
	}
	return buf.Bytes(), nil
}

// listingType converts a type, grouping its offerings by capacity type and state.
func listingType(t catalog.TypeReport) ListingType {
	lt := ListingType{APIReference: t.APIReferenceLabel, CapacityTypes: []string{}, Offerings: []ListingOffering{}}
	if t.Dropped.Code != "" {
		if v, ok := labels.Value(t.APIReference); ok && v == t.APIReference {
			lt.APIReference = t.APIReference
		}
		lt.Reason, lt.Detail = string(t.Dropped.Code), t.Dropped.Detail
		return lt
	}
	type group struct {
		capacityType, reason, until string
		available                   bool
	}
	groups := map[group][]string{}
	counts := map[catalog.Code]int{}
	details := map[catalog.Code]string{}
	// The dominant reason is taken from the offerings Karpenter is given, when
	// any is unavailable: one left out (its zone, price or launchability) cannot
	// become available, so a gated type with more left-out zones still says
	// QuotaGate.
	listedDown := slices.ContainsFunc(t.Offerings, func(o catalog.OfferingReport) bool {
		return !o.Available && !o.Removed
	})
	for _, o := range t.Offerings {
		available := o.Available && !o.Removed
		g := group{capacityType: o.CapacityType, reason: string(o.Reason.Code), available: available}
		if o.Reason.Code == catalog.UnavailableCache && !available {
			g.reason, g.until = o.Reason.Class, o.Reason.Until.UTC().Format(time.RFC3339)
		}
		groups[g] = append(groups[g], o.Zone)
		if available {
			lt.Available = true
			if !slices.Contains(lt.CapacityTypes, o.CapacityType) {
				lt.CapacityTypes = append(lt.CapacityTypes, o.CapacityType)
			}
			continue
		}
		if o.Removed && listedDown {
			continue
		}
		if counts[o.Reason.Code]++; details[o.Reason.Code] == "" {
			details[o.Reason.Code] = o.Reason.Detail
		}
	}
	slices.Sort(lt.CapacityTypes)
	for g, zones := range groups {
		slices.Sort(zones)
		lt.Offerings = append(lt.Offerings, ListingOffering{
			CapacityType: g.capacityType, Zones: slices.Compact(zones), Available: g.available, Reason: g.reason,
			Until: g.until,
		})
	}
	slices.SortFunc(lt.Offerings, func(a, b ListingOffering) int {
		return cmp.Or(cmp.Compare(a.CapacityType, b.CapacityType), cmp.Compare(b2i(b.Available), b2i(a.Available)),
			cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Until, b.Until))
	})
	if !lt.Available {
		// The dominant reason: the most offerings, the first code on a tie.
		best := catalog.Code("")
		for code, n := range counts {
			if n > counts[best] || (n == counts[best] && code < best) {
				best = code
			}
		}
		lt.Reason, lt.Detail = string(best), details[best]
	}
	return lt
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
