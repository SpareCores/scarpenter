/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// RegionAlias is one row of the region table: Keeper's region name (its
// api_reference, mostly the legacy OVH names) and the name the OpenStack
// catalog of a project uses for it. API is empty for a Keeper region that is
// deliberately not launched; Note says why.
//
// Plan 14 item A8: the Keystone catalog is the truth for what a project can use
// and the NodeClass `regions` list and the hand-off use the API names; renaming
// rows in Keeper is not ours to do. Today the two agree for every Keeper
// region with prices, so most rows are identities. They are listed anyway: the
// test makes every region of the committed Keeper snapshot appear here, so a
// new Keeper name fails the test until somebody decides what it maps to.
type RegionAlias struct {
	Keeper string
	API    string
	Note   string
}

const (
	noteTrigram  = "region group name without prices in Keeper; the catalog's region names are the numbered ones (S4)"
	noteInactive = "inactive in Keeper"
)

// regionAliases is the table. The API names of the rows with prices are the
// legacy names and the two new-style ones (EU-WEST-PAR, EU-SOUTH-MIL) as Keeper
// has them; whether the catalog of a given project says otherwise is spike S4
// (OVH's announcement of new names such as eu-west-gra gives no mapping to GRA11
// and no source shows them as Keystone names). A row where they differ is a
// RegionAlias with another API name, or SetAlias at runtime.
var regionAliases = []RegionAlias{
	// Priced, 1-AZ regions under their legacy names.
	{"GRA7", "GRA7", ""}, {"GRA9", "GRA9", ""}, {"GRA11", "GRA11", ""},
	{"SBG5", "SBG5", ""}, {"SBG7", "SBG7", ""},
	{"DE1", "DE1", ""}, {"UK1", "UK1", ""}, {"WAW1", "WAW1", ""},
	{"BHS5", "BHS5", ""}, {"SGP1", "SGP1", ""}, {"SYD1", "SYD1", ""},
	{"RBX-A", "RBX-A", ""},
	{"AP-SOUTH-MUM-1", "AP-SOUTH-MUM-1", ""}, {"AP-SOUTHEAST-SYD-2", "AP-SOUTHEAST-SYD-2", ""},
	// Priced, 3-AZ regions under their new-style names.
	{"EU-WEST-PAR", "EU-WEST-PAR", ""}, {"EU-SOUTH-MIL", "EU-SOUTH-MIL", ""},
	// Keeper rows without prices.
	{"GRA", "", noteTrigram}, {"SBG", "", noteTrigram}, {"RBX", "", noteTrigram},
	{"DE", "", noteTrigram}, {"UK", "", noteTrigram}, {"WAW", "", noteTrigram},
	{"SGP", "", noteTrigram}, {"SYD", "", noteTrigram}, {"BHS", "", noteTrigram},
	{"AP-SOUTH-MUM", "", noteTrigram},
	{"AP-SOUTHEAST-SYD", "", noteInactive}, {"CA-EAST-TOR", "", noteInactive}, {"RBX-ARCHIVE", "", noteInactive},
}

// RegionMap maps Keeper's region names to the OpenStack API region names and
// back. API names are upper case, as Keystone has them; lookups ignore case.
type RegionMap struct {
	toAPI    map[string]string
	toKeeper map[string]string
	unmapped map[string]string
}

// NewRegionMap builds the map from the built-in table; extra rows (Keeper name
// to API name, "" to leave a Keeper region unmapped) replace the table's. It
// fails when two Keeper names would share one API name, which the reverse
// lookup could not resolve.
func NewRegionMap(extra map[string]string) (*RegionMap, error) {
	m := &RegionMap{toAPI: map[string]string{}, toKeeper: map[string]string{}, unmapped: map[string]string{}}
	rows := slices.Clone(regionAliases)
	for k, v := range extra {
		k = strings.ToUpper(k)
		i := slices.IndexFunc(rows, func(r RegionAlias) bool { return r.Keeper == k })
		row := RegionAlias{Keeper: k, API: strings.ToUpper(v), Note: "set by configuration"}
		if i >= 0 {
			rows[i] = row
		} else {
			rows = append(rows, row)
		}
	}
	for _, r := range rows {
		if r.API == "" {
			m.unmapped[r.Keeper] = r.Note
			continue
		}
		if other, dup := m.toKeeper[r.API]; dup {
			return nil, fmt.Errorf("Keeper regions %s and %s both map to API region %s", other, r.Keeper, r.API)
		}
		m.toAPI[r.Keeper], m.toKeeper[r.API] = r.API, r.Keeper
	}
	return m, nil
}

// API returns the OpenStack region name of a Keeper region; false for an
// unmapped or unknown one.
func (m *RegionMap) API(keeper string) (string, bool) {
	a, ok := m.toAPI[strings.ToUpper(keeper)]
	return a, ok
}

// Keeper returns the Keeper region of an API region name; false when Keeper has
// no row for it.
func (m *RegionMap) Keeper(api string) (string, bool) {
	k, ok := m.toKeeper[strings.ToUpper(api)]
	return k, ok
}

// Unmapped returns the Keeper regions listed as not launched, with the reason,
// sorted.
func (m *RegionMap) Unmapped() []string {
	out := make([]string, 0, len(m.unmapped))
	for k := range m.unmapped {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Known reports whether the Keeper region has a row, mapped or unmapped.
func (m *RegionMap) Known(keeper string) bool {
	k := strings.ToUpper(keeper)
	_, mapped := m.toAPI[k]
	_, un := m.unmapped[k]
	return mapped || un
}

// Join intersects the compute regions of the Keystone catalog with the table:
// the usable regions with the Keeper name each joins to, in the catalog's order.
// A region keeps the catalog's spelling: gophercloud matches endpoint regions
// exactly, so a name changed here would find no endpoint.
// A catalog region Keeper has no row for is returned in unknown (the catalog is
// the truth, but without a Keeper row there are no offerings or prices for it).
func (m *RegionMap) Join(catalog []string) (joined []vendors.Region, unknown []string) {
	for _, c := range catalog {
		if _, ok := m.Keeper(c); ok {
			joined = append(joined, vendors.Region{Name: c})
		} else {
			unknown = append(unknown, c)
		}
	}
	// vendors.Region has no field for the Keeper name it joins to (the A8 text of
	// plan 14 proposes one, a core change): Name is the API name, and the facts
	// join the catalog on it, which holds while the table rows are identities
	// (today they are, for every Keeper region with prices). Keeper(Name) is the
	// join the driver itself uses.
	return joined, unknown
}

// ZoneIDs maps the availability zones Nova reports in a region to Keeper's zone
// IDs, which are the lower-case Keeper region name plus a letter: gra11-a for a
// 1-AZ region (whatever Nova calls its one zone, "nova" in most), eu-west-par-a
// to -c for the 3-AZ regions. Names ending in -a, -b or -c keep their letter;
// otherwise the zones are lettered in sorted order. The zone's Name is Nova's.
func ZoneIDs(keeperRegion string, azs []string) []vendors.Zone {
	base := strings.ToLower(keeperRegion)
	names := slices.Clone(azs)
	slices.Sort(names)
	out := make([]vendors.Zone, 0, len(names))
	if len(names) == 1 {
		return append(out, vendors.Zone{ID: base + "-a", Name: names[0]})
	}
	for i, n := range names {
		letter := string(rune('a' + i))
		if l := strings.ToLower(n); len(l) > 2 && l[len(l)-2] == '-' && l[len(l)-1] >= 'a' && l[len(l)-1] <= 'c' {
			letter = l[len(l)-1:]
		}
		out = append(out, vendors.Zone{ID: base + "-" + letter, Name: n})
	}
	return out
}
