/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// SelectionCandidates exposes the candidates the local selection builds from
// the data, for the equivalence test against Keeper's /servers.
func SelectionCandidates(data *model.Data, vendor string, regionIDs []string,
	scores map[keeper.BenchmarkKey]map[string]float64, basis keeper.Allocation) []*selection.Candidate {
	return newLocalData(nil, data).candidates(vendor, regionIDs, scores, basis)
}

// BenchmarkScores exposes the scores of one benchmark by server id.
var BenchmarkScores = benchmarkScores
