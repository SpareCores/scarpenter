/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels

import (
	"strings"

	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// LocationKeys are the location label keys of an offering (00a section 6.4).
var LocationKeys = []string{
	v1alpha1.LabelCountry, v1alpha1.LabelContinent, v1alpha1.LabelCity,
	v1alpha1.LabelState, v1alpha1.LabelGreenEnergy,
}

// Location returns the location requirements of an offering in a region, from
// Keeper's region table: country (lower case ISO code), continent (a static
// country to continent map), city and state as slugs, green energy as a
// boolean. Unknown values are DoesNotExist.
func Location(r *model.Region) []*scheduling.Requirement {
	out := make([]*scheduling.Requirement, 0, len(LocationKeys))
	country := strings.ToLower(strings.TrimSpace(r.CountryID))
	if c, ok := Slug(country); ok {
		out = append(out, In(v1alpha1.LabelCountry, c))
	} else {
		out = append(out, Absent(v1alpha1.LabelCountry))
	}
	if c, ok := ContinentOf(country); ok {
		out = append(out, In(v1alpha1.LabelContinent, c))
	} else {
		out = append(out, Absent(v1alpha1.LabelContinent))
	}
	for _, f := range []struct{ key, text string }{
		{v1alpha1.LabelCity, r.City}, {v1alpha1.LabelState, r.State},
	} {
		if s, ok := Slug(f.text); ok {
			out = append(out, In(f.key, s))
		} else {
			out = append(out, Absent(f.key))
		}
	}
	if r.GreenEnergy != nil {
		out = append(out, Bool(v1alpha1.LabelGreenEnergy, *r.GreenEnergy))
	} else {
		out = append(out, Absent(v1alpha1.LabelGreenEnergy))
	}
	return out
}

// Continents are the continent label values.
const (
	Africa       = "africa"
	Asia         = "asia"
	Europe       = "europe"
	NorthAmerica = "north-america"
	SouthAmerica = "south-america"
	Oceania      = "oceania"
	Antarctica   = "antarctica"
)

// continentCountries lists the ISO 3166-1 alpha-2 codes per continent. The
// transcontinental countries follow the UN geoscheme (Russia in Europe;
// Cyprus, Turkey, Georgia, Armenia and Azerbaijan in Asia); Mexico and the
// Caribbean are North America.
//
//nolint:lll // a data table: one line of codes per continent
var continentCountries = map[string]string{
	Africa:       "AO BF BI BJ BW CD CF CG CI CM CV DJ DZ EG EH ER ET GA GH GM GN GQ GW KE KM LR LS LY MA MG ML MR MU MW MZ NA NE NG RE RW SC SD SH SL SN SO SS ST SZ TD TG TN TZ UG YT ZA ZM ZW",
	Asia:         "AE AF AM AZ BD BH BN BT CN CY GE HK ID IL IN IQ IR JO JP KG KH KP KR KW KZ LA LB LK MM MN MO MV MY NP OM PH PK PS QA SA SG SY TH TJ TL TM TR TW UZ VN YE",
	Europe:       "AD AL AT AX BA BE BG BY CH CZ DE DK EE ES FI FO FR GB GG GI GR HR HU IE IM IS IT JE LI LT LU LV MC MD ME MK MT NL NO PL PT RO RS RU SE SI SJ SK SM UA VA XK",
	NorthAmerica: "AG AI AW BB BL BM BQ BS BZ CA CR CU CW DM DO GD GL GP GT HN HT JM KN KY LC MF MQ MS MX NI PA PM PR SV SX TC TT US VC VG VI",
	SouthAmerica: "AR BO BR CL CO EC FK GF GY PE PY SR UY VE",
	Oceania:      "AS AU CC CK CX FJ FM GU KI MH MP NC NF NR NU NZ PF PG PN PW SB TK TO TV VU WF WS",
	Antarctica:   "AQ BV GS HM TF",
}

var continentOf = func() map[string]string {
	m := map[string]string{}
	for continent, codes := range continentCountries {
		for c := range strings.FieldsSeq(codes) {
			m[strings.ToLower(c)] = continent
		}
	}
	return m
}()

// ContinentOf returns the continent label value of a lower-case ISO country
// code.
func ContinentOf(country string) (string, bool) {
	c, ok := continentOf[strings.ToLower(country)]
	return c, ok
}
