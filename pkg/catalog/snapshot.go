/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// snapshot is one successful refresh: the data, indexed by vendor, and the
// instance types built from it, built lazily per NodeClass and vendor.
type snapshot struct {
	id         uint64
	data       *model.Data
	scope      Scope
	requestKey string
	loadedAt   time.Time
	vendors    map[string]*vendorData
	// vendorRows is the vendor table by vendor ID.
	vendorRows map[string]*model.Vendor

	mu     sync.Mutex
	views  map[string]*classView
	builds map[buildKey]*vendorBuild
	// buildEpoch is the facts epoch the builds were made with.
	buildEpoch uint64
}

type buildKey struct{ classHash, vendor string }

type zoneKey struct{ region, zone string }

// vendorData indexes the data of one vendor.
type vendorData struct {
	servers  []*model.Server // by APIReference, one per reference
	regions  map[string]*model.Region
	prices   map[string][]model.Price // by server ID
	zones    map[zoneKey]bool         // zone table: active or not
	nTypes   int                      // types with a price
	nPrices  int
	byAPIRef map[string]*model.Server
}

func newSnapshot(id uint64, data *model.Data, scope Scope, requestKey string, now time.Time) *snapshot {
	s := &snapshot{
		id: id, data: data, scope: scope, requestKey: requestKey, loadedAt: now,
		vendors: map[string]*vendorData{}, vendorRows: map[string]*model.Vendor{},
		views: map[string]*classView{}, builds: map[buildKey]*vendorBuild{},
	}
	for i := range data.Vendors {
		s.vendorRows[data.Vendors[i].ID] = &data.Vendors[i]
	}
	vd := func(vendor string) *vendorData {
		v, ok := s.vendors[vendor]
		if !ok {
			v = &vendorData{
				regions: map[string]*model.Region{}, prices: map[string][]model.Price{},
				zones: map[zoneKey]bool{}, byAPIRef: map[string]*model.Server{},
			}
			s.vendors[vendor] = v
		}
		return v
	}
	for vendor := range scope {
		vd(vendor)
	}
	for i := range data.Regions {
		r := &data.Regions[i]
		vd(r.Vendor).regions[r.ID] = r
	}
	for _, z := range data.Zones {
		vd(z.Vendor).zones[zoneKey{z.RegionID, z.ID}] = z.Active
	}
	// Servers sorted by ID, so that of two servers with the same API reference
	// the lower ID wins, deterministically. Sources make the references unique
	// (uniqueAPIReferences), so a duplicate here is a source bug: it is logged.
	servers := make([]*model.Server, len(data.Servers))
	for i := range data.Servers {
		servers[i] = &data.Servers[i]
	}
	slices.SortFunc(servers, func(a, b *model.Server) int {
		return strings.Compare(a.Vendor+"\x00"+a.ID, b.Vendor+"\x00"+b.ID)
	})
	for _, srv := range servers {
		v := vd(srv.Vendor)
		if first, dup := v.byAPIRef[srv.APIReference]; dup {
			log.Log.WithName("catalog").Error(nil, "two servers share an api_reference, the second is ignored",
				"vendor", srv.Vendor, "apiReference", srv.APIReference, "kept", first.ID, "ignored", srv.ID)
			continue
		}
		v.byAPIRef[srv.APIReference] = srv
		v.servers = append(v.servers, srv)
	}
	for _, p := range data.Prices {
		v := vd(p.Vendor)
		v.prices[p.ServerID] = append(v.prices[p.ServerID], p)
	}
	for _, v := range s.vendors {
		for _, srv := range v.servers {
			if len(v.prices[srv.ID]) > 0 {
				v.nTypes++
				v.nPrices += len(v.prices[srv.ID])
			}
		}
	}
	return s
}

// classView is what a NodeClass adds to the data: metric values and selection
// membership. It is computed once per snapshot and class.
type classView struct {
	metrics map[string]*labels.MetricValues
	// invalidMetrics maps metric names the source could not answer to the reason.
	invalidMetrics map[string]string
	// members maps a server to the selection profiles it belongs to.
	members map[model.Key][]string
	// selection holds the ranking facts of each valid profile.
	selection map[string]*selectionView
	// invalidSelections maps profile names to the reason.
	invalidSelections map[string]string
}

// selectionView is the ranking side of a selection profile: what prices the
// offerings of a NodeClaim that pins the profile (00a section 7.3).
type selectionView struct {
	objective     selapi.Objective
	lowerIsBetter bool
	scores        map[model.Key]float64
}

func metricQuery(vendor string, m *metricDef) MetricQuery {
	return MetricQuery{
		ID: MetricQueryID(vendor, m.benchmark, m.profile), Vendor: vendor,
		Benchmark: m.benchmark, WorkloadProfile: m.profile,
	}
}

type metricDef struct {
	benchmark *selapi.BenchmarkRef
	profile   selapi.WorkloadProfileName
}

// prune drops the views and builds of classes that no longer exist (or whose
// content changed), so that edits do not pile up full builds until the next
// refresh.
func (s *snapshot) prune(classes []Class) {
	keep := make(map[string]bool, len(classes))
	for i := range classes {
		keep[classes[i].Hash] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for h := range s.views {
		if !keep[h] {
			delete(s.views, h)
		}
	}
	for k := range s.builds {
		if !keep[k.classHash] {
			delete(s.builds, k)
		}
	}
}

func (s *snapshot) view(class *Class) *classView {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.views[class.Hash]; ok {
		return v
	}
	v := &classView{
		metrics: map[string]*labels.MetricValues{}, invalidMetrics: map[string]string{},
		members: map[model.Key][]string{}, selection: map[string]*selectionView{},
		invalidSelections: map[string]string{},
	}
	var servers []*model.Server
	for _, vendor := range class.VendorNames() {
		if vd, ok := s.vendors[vendor]; ok {
			servers = append(servers, vd.servers...)
		}
	}
	for _, name := range class.MetricNames() {
		m := class.Metrics[name]
		def := &metricDef{benchmark: m.Benchmark, profile: m.WorkloadProfile}
		raw := map[model.Key]float64{}
		var invalid string
		for _, vendor := range class.VendorNames() {
			id := metricQuery(vendor, def).ID
			if reason, bad := s.data.Invalid[id]; bad {
				invalid = reason
				continue
			}
			vd := s.vendors[vendor]
			for ref, score := range s.data.Scores[id] {
				if srv, ok := vd.byAPIRef[ref]; ok {
					raw[srv.Key()] = score
				}
			}
		}
		if invalid != "" {
			v.invalidMetrics[name] = invalid
			continue
		}
		scale := float64(1)
		if m.Scale != nil {
			scale = float64(*m.Scale)
		}
		v.metrics[name] = labels.ComputeMetric(labels.Metric{
			Name: name, Scale: scale, PerVCPU: m.Normalize == selapi.RankingNormalizePerVcpu,
			LowerIsBetter: def.lowerIsBetter(),
		}, servers, raw)
	}
	for _, name := range slices.Sorted(maps.Keys(class.Selections)) {
		sel := class.Selections[name]
		spec := selapi.Spec{Selector: sel.Selector, Ranking: sel.Ranking}
		// Only a ranking can price by an objective: a selector-only profile
		// keeps the real price.
		sv := &selectionView{objective: selapi.ObjectivePrice, scores: map[model.Key]float64{}}
		if sel.Ranking != nil {
			sv.objective = selapi.ObjectiveScore
			if sel.Ranking.Objective != "" {
				sv.objective = sel.Ranking.Objective
			}
			sv.lowerIsBetter = sel.Ranking.Score.Benchmark != nil && keeper.LowerIsBetter(sel.Ranking.Score.Benchmark.ID)
		}
		// Each vendor is evaluated on its own: a profile that is invalid for one
		// vendor keeps its members at the others, and the reason names the vendor.
		var invalid []string
		valid := 0
		for _, vendor := range class.VendorNames() {
			id := SelectionQueryID(vendor, class.Vendors[vendor], &spec)
			if reason, bad := s.data.Invalid[id]; bad {
				invalid = append(invalid, "vendor "+vendor+": "+reason)
				continue
			}
			valid++
			vd := s.vendors[vendor]
			for ref, member := range s.data.Selected[id] {
				srv, ok := vd.byAPIRef[ref]
				if !ok {
					continue
				}
				v.members[srv.Key()] = append(v.members[srv.Key()], name)
				if member.Score != nil {
					sv.scores[srv.Key()] = *member.Score
				}
			}
		}
		if len(invalid) > 0 {
			v.invalidSelections[name] = strings.Join(invalid, "; ")
		}
		if valid > 0 {
			v.selection[name] = sv
		}
	}
	s.views[class.Hash] = v
	return v
}

func (d *metricDef) lowerIsBetter() bool {
	if d.benchmark != nil {
		return keeper.LowerIsBetter(d.benchmark.ID)
	}
	return false
}
