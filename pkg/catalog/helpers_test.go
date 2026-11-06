/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// refreshed returns a catalog over the replayed snapshot that has read the
// scope for one class named "c".
func refreshed(t testing.TB, scope catalog.Scope, vendorsWithZones ...string) (*catalog.Catalog, catalog.Class) {
	t.Helper()
	cl := catalog.NewClass("c", scope)
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, vendorsWithZones...)})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, cl
}

func all(t testing.TB, c *catalog.Catalog, f catalog.Filter) []*cloudprovider.InstanceType {
	t.Helper()
	its, err := c.InstanceTypes(context.Background(), catalog.Query{Class: "c", Filter: f})
	if err != nil {
		t.Fatal(err)
	}
	return its
}

func byName(its []*cloudprovider.InstanceType, name string) *cloudprovider.InstanceType {
	for _, it := range its {
		if it.Name == name {
			return it
		}
	}
	return nil
}

// reqString renders a requirement deterministically.
func reqString(r *scheduling.Requirement) string {
	vals := r.Values()
	slices.Sort(vals)
	return fmt.Sprintf("%s %s [%s]", r.Key, r.Operator(), strings.Join(vals, ","))
}

func reqLines(reqs scheduling.Requirements) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, reqString(r))
	}
	slices.Sort(out)
	return out
}

func quantities(rl corev1.ResourceList) string {
	out := make([]string, 0, len(rl))
	for k, v := range rl {
		out = append(out, fmt.Sprintf("%s=%s", k, v.String()))
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// dump renders an instance type deterministically for golden files.
func dump(it *cloudprovider.InstanceType) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\ncapacity: %s\n", it.Name, quantities(it.Capacity))
	fmt.Fprintf(&b, "kubeReserved: %s\nsystemReserved: %s\nevictionThreshold: %s\n",
		quantities(it.Overhead.KubeReserved), quantities(it.Overhead.SystemReserved), quantities(it.Overhead.EvictionThreshold))
	b.WriteString("requirements:\n")
	for _, l := range reqLines(it.Requirements) {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	offs := slices.Clone(it.Offerings)
	slices.SortFunc(offs, func(a, c *cloudprovider.Offering) int {
		return strings.Compare(strings.Join(reqLines(a.Requirements), ";"), strings.Join(reqLines(c.Requirements), ";"))
	})
	fmt.Fprintf(&b, "offerings: %d\n", len(offs))
	for _, o := range offs {
		fmt.Fprintf(&b, "- price: %s\n  available: %t\n", resource.NewScaledQuantity(int64(o.Price*1e6), -6).String(), o.Available)
		for _, l := range reqLines(o.Requirements) {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	return b.String()
}

// fakeSource serves fixed data and counts loads; its error can be switched on.
type fakeSource struct {
	mu    sync.Mutex
	data  *model.Data
	err   error
	loads int
	reqs  []catalog.Request
}

func (f *fakeSource) Load(_ context.Context, req catalog.Request) (*model.Data, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSource) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// synthetic is a small data set for the vendor "v": one region "r1" with three
// zones' worth of prices and a few servers.
func synthetic() *model.Data {
	cores := 1
	return &model.Data{
		Version: "v1",
		Servers: []model.Server{
			{Vendor: "v", ID: "1", APIReference: "small", Family: "f", VCPUs: 2, CPUCores: &cores, Arch: "amd64", MemoryMiB: 4096, CPUManufacturer: "Intel"},
			{Vendor: "v", ID: "2", APIReference: "big", Family: "f", VCPUs: 16, Arch: "arm64", MemoryMiB: 65536},
		},
		Regions: []model.Region{{Vendor: "v", ID: "r1", Name: "region-1", Active: true, CountryID: "DE", City: "Frankfurt"}},
		Zones: []model.Zone{
			{Vendor: "v", RegionID: "r1", ID: "z1", Name: "zone-1", Active: true},
			{Vendor: "v", RegionID: "r1", ID: "z2", Name: "zone-2", Active: true},
		},
		Prices: []model.Price{
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "1", CapacityType: model.CapacityOnDemand, USDPerHour: 0.10},
			{Vendor: "v", RegionID: "r1", ZoneID: "z2", ServerID: "1", CapacityType: model.CapacityOnDemand, USDPerHour: 0.11},
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "1", CapacityType: model.CapacitySpot, USDPerHour: 0.03},
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "2", CapacityType: model.CapacityOnDemand, USDPerHour: 0.80},
		},
	}
}

func syntheticFacts() catalog.StaticFacts {
	return catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{
		"v": {"region-1": {{ID: "z1", Name: "zone-1"}, {ID: "z2", Name: "zone-2"}}},
	}}
}

func newSynthetic(t testing.TB, mutate func(*model.Data), facts catalog.Facts) *catalog.Catalog {
	t.Helper()
	data := synthetic()
	if mutate != nil {
		mutate(data)
	}
	src := &fakeSource{data: data}
	if facts == nil {
		facts = syntheticFacts()
	}
	c := catalog.New(catalog.Options{Source: src, Facts: facts})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func catalogtestFacts(t testing.TB) catalog.StaticFacts { return catalogtest.Facts(t, "aws") }

func sourceFor(t testing.TB) *catalog.KeeperSource { return catalogtest.Source(t) }

func itoa(n int) string { return strconv.Itoa(n) }
