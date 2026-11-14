/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	ocidata "github.com/SpareCores/scarpenter/pkg/catalog/embeddeddata/oci"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

func embeddedOCI(t testing.TB) *catalog.EmbeddedSource {
	t.Helper()
	src, err := catalog.NewEmbeddedSource("oci", ocidata.FS)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// ociFacts are the zones of the embedded region and zone files, named as the
// driver's tenancy would (Keeper's zone ID as ID, the zone's api_reference as name).
func ociFacts(t testing.TB) catalog.StaticFacts {
	t.Helper()
	read := func(path string) *keeper.SnapshotBody {
		gz, err := ocidata.FS.ReadFile(path + ".json.gz")
		if err != nil {
			t.Fatal(err)
		}
		return &keeper.SnapshotBody{Gzip: gz}
	}
	regions, err := keeper.DecodeSnapshotRows[keeper.Region](read("region/oci"), -1)
	if err != nil {
		t.Fatal(err)
	}
	zones, err := keeper.DecodeSnapshotRows[keeper.Zone](read("zone/oci"), -1)
	if err != nil {
		t.Fatal(err)
	}
	byRegion := map[string][]catalog.Zone{}
	for _, r := range regions {
		if r.Status != "active" {
			continue
		}
		for _, z := range zones {
			if z.RegionID == r.RegionID && z.Status == "active" {
				byRegion[r.APIReference] = append(byRegion[r.APIReference], catalog.Zone{ID: z.ZoneID, Name: z.APIReference})
			}
		}
	}
	return catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{"oci": byRegion}}
}

func ociCatalog(t testing.TB, src catalog.Source) *catalog.Catalog {
	t.Helper()
	c := catalog.New(catalog.Options{Source: src, Facts: ociFacts(t)})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

// offeringsOf counts the offerings of a type by capacity type and region,
// and collects the zones.
func offeringsOf(it *cloudprovider.InstanceType) (counts map[string]int, zones map[string]bool) {
	counts, zones = map[string]int{}, map[string]bool{}
	for _, o := range it.Offerings {
		capacity := o.Requirements.Get("karpenter.sh/capacity-type").Any()
		region := o.Requirements.Get(corev1.LabelTopologyRegion).Any()
		counts[capacity+"/"+region]++
		zones[o.Requirements.Get(corev1.LabelTopologyZone).Any()] = true
	}
	return counts, zones
}

var sizedOCIName = regexp.MustCompile(`^oci\.(.+)\.[1-9][0-9]*-[1-9][0-9]*$`)

func TestEmbeddedCatalogHasTheSizedTypesWithPricesInEveryAD(t *testing.T) {
	c := ociCatalog(t, embeddedOCI(t))
	its := all(t, c, catalog.Filter{})
	sized, shapes := 0, map[string]bool{}
	for _, it := range its {
		if m := sizedOCIName.FindStringSubmatch(it.Name); m != nil {
			sized++
			shapes[m[1]] = true
		}
	}
	// Rule R: one type per size, not one per shape (the rows share their api_reference).
	if sized != 256 || len(shapes) != 12 {
		t.Errorf("%d sized types of %d shapes, want 256 of 12 (%d types in all)", sized, len(shapes), len(its))
	}

	it := byName(its, "oci.VM.Standard.E5.Flex.2-16")
	if it == nil {
		t.Fatal("oci.VM.Standard.E5.Flex.2-16 is not in the catalog")
	}
	counts, zones := offeringsOf(it)
	for _, capacity := range []string{"on-demand", "spot"} {
		for _, region := range []string{"eu-frankfurt-1", "us-ashburn-1", "us-phoenix-1"} {
			if n := counts[capacity+"/"+region]; n != 3 {
				t.Errorf("%s offerings in %s = %d, want 3 (one per AD)", capacity, region, n)
			}
		}
	}
	if len(zones) != 9 || !zones["EU-FRANKFURT-1-AD-1"] || !zones["PHX-AD-3"] {
		t.Errorf("zones = %v", zones)
	}
	if len(it.Offerings) != 18 {
		t.Errorf("%d offerings, want 18", len(it.Offerings))
	}

	// The fixed shapes are types of their own, with their on-demand price.
	for _, name := range []string{"oci.BM.Standard3.64", "oci.VM.Standard2.1"} {
		fixed := byName(its, name)
		if fixed == nil {
			t.Errorf("%s is not in the catalog", name)
			continue
		}
		if counts, _ := offeringsOf(fixed); counts["on-demand/eu-frankfurt-1"] == 0 {
			t.Errorf("%s has no on-demand offering in Frankfurt: %v", name, counts)
		}
	}
	// The sample data has no benchmark scores: they are not read.
	if v := c.Version(); v == "" {
		t.Error("the catalog has no version")
	}
}

// The same rows served as Keeper's snapshot give the same catalog: the swap of
// the plan (remove the vendor from the flag) changes no type, label or offering.
func TestEmbeddedCatalogEqualsTheSameRowsServedByKeeper(t *testing.T) {
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	overrides := map[string][]byte{}
	for _, path := range []string{
		"vendor", "region/oci", "zone/oci", "server/oci", "server_price/oci", "storage_price/oci", "ipv4_price/oci",
	} {
		gz, err := ocidata.FS.ReadFile(path + ".json.gz")
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		if overrides[path], err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.AddHash(catalogtest.NewHash, overrides); err != nil {
		t.Fatal(err)
	}
	if err := h.SetCurrent(catalogtest.NewHash); err != nil {
		t.Fatal(err)
	}
	srv := keepertest.NewServer(t, h)

	fromKeeper := ociCatalog(t, catalogtest.SourceAt(srv.URL))
	embedded := ociCatalog(t, embeddedOCI(t))
	want, got := all(t, fromKeeper, catalog.Filter{}), all(t, embedded, catalog.Filter{})
	if len(got) != len(want) || len(got) < 256 {
		t.Fatalf("%d types from the embedded data, %d from Keeper", len(got), len(want))
	}
	byN := func(its []*cloudprovider.InstanceType) {
		slices.SortFunc(its, func(a, b *cloudprovider.InstanceType) int { return strings.Compare(a.Name, b.Name) })
	}
	byN(want)
	byN(got)
	for i := range want {
		if w, g := dump(want[i]), dump(got[i]); w != g {
			t.Fatalf("type %s differs:\nKeeper:\n%s\nembedded:\n%s", want[i].Name, w, g)
		}
	}
}

func TestEmbeddedSourceDataFacts(t *testing.T) {
	src := embeddedOCI(t)
	req := catalog.Request{
		Scope:      catalog.Scope{"oci": {"eu-frankfurt-1"}},
		Metrics:    []catalog.MetricQuery{{ID: "m1", Vendor: "oci"}},
		Selections: []catalog.SelectionQuery{{ID: "s1", Vendor: "oci"}},
	}
	d, err := src.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !d.PublishedAt.IsZero() || d.SnapshotRead || !d.AuxPricesRead || d.FetchedAt.IsZero() || d.Version == "" {
		t.Errorf("publishedAt %v, snapshotRead %v, auxPricesRead %v, fetchedAt %v, version %q",
			d.PublishedAt, d.SnapshotRead, d.AuxPricesRead, d.FetchedAt, d.Version)
	}
	if d.Version != src.Version() || src.DataDate() != "2026-11-13" {
		t.Errorf("version %q (source %q), data date %q", d.Version, src.Version(), src.DataDate())
	}
	if len(d.Vendors) != 1 || d.Vendors[0].ID != "oci" || len(d.StoragePrices) != 4*44 || len(d.IPv4Prices) != 0 {
		// Storage prices are not selected by scope; the IPv4 price is free (0), which Keeper's rows drop too.
		t.Errorf("%d vendors, %d storage prices, %d IPv4 prices",
			len(d.Vendors), len(d.StoragePrices), len(d.IPv4Prices))
	}
	if len(d.Scores) != 0 || len(d.Selected) != 0 || d.Invalid["m1"] == "" || d.Invalid["s1"] == "" {
		t.Errorf("scores %v, selected %v, invalid %v", d.Scores, d.Selected, d.Invalid)
	}
	// Rule R named the sized types by their server ID.
	for _, s := range d.Servers {
		if s.APIReference != s.ID {
			t.Fatalf("server %s has api_reference %q", s.ID, s.APIReference)
		}
	}
	// The same request gets the same data, a request for other vendors none.
	if again, err := src.Load(t.Context(), req); err != nil || again != d {
		t.Errorf("a repeated request got other data (%v)", err)
	}
	other, err := src.Load(t.Context(), catalog.Request{Scope: catalog.Scope{"aws": nil}})
	if err != nil || len(other.Servers) != 0 || len(other.Regions) != 0 {
		t.Errorf("a request for another vendor got %d servers (%v)", len(other.Servers), err)
	}
}

func TestEmbeddedSourceRejectsDamagedFiles(t *testing.T) {
	copyFS := func(mutate func(fstest.MapFS)) fstest.MapFS {
		m := fstest.MapFS{}
		for _, p := range []string{
			"manifest", "vendor", "region/oci", "zone/oci", "server/oci", "server_price/oci", "storage_price/oci", "ipv4_price/oci",
		} {
			b, err := ocidata.FS.ReadFile(p + ".json.gz")
			if err != nil {
				t.Fatal(err)
			}
			m[p+".json.gz"] = &fstest.MapFile{Data: b}
		}
		mutate(m)
		return m
	}
	scope := catalog.Request{Scope: catalog.Scope{"oci": nil}}

	if _, err := catalog.NewEmbeddedSource("oci", fstest.MapFS{}); err == nil {
		t.Error("no manifest: no error")
	}
	if _, err := catalog.NewEmbeddedSource("../oci", ocidata.FS); err == nil {
		t.Error("a path as vendor: no error")
	}
	// A file that does not match the manifest's SHA-256.
	src, err := catalog.NewEmbeddedSource("oci", copyFS(func(m fstest.MapFS) {
		m["server/oci.json.gz"] = &fstest.MapFile{Data: m["zone/oci.json.gz"].Data}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(t.Context(), scope); err == nil || !strings.Contains(err.Error(), "server/oci") {
		t.Errorf("a damaged server file: error %v", err)
	}
	// A file the manifest lists but the files lack.
	src, err = catalog.NewEmbeddedSource("oci", copyFS(func(m fstest.MapFS) { delete(m, "zone/oci.json.gz") }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(t.Context(), scope); err == nil {
		t.Error("a missing zone file: no error")
	}
	// A vendor the manifest has no files of.
	if _, err := catalog.NewEmbeddedSource("aws", ocidata.FS); err == nil {
		t.Error("a vendor without files: no error")
	}
}

// A request for the embedded vendor alone never reaches the default source.
func TestMuxServesEmbeddedVendorsWithoutTheDefaultSource(t *testing.T) {
	def := &fakeSource{data: synthetic()}
	mux := &catalog.MuxSource{Default: def, Vendors: map[string]catalog.Source{"oci": embeddedOCI(t)}}
	d, err := mux.Load(t.Context(), catalog.Request{Scope: catalog.Scope{"oci": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if def.loadCount() != 0 {
		t.Errorf("the default source was read %d times", def.loadCount())
	}
	if len(d.Servers) == 0 || d.SnapshotRead {
		t.Errorf("%d servers, snapshotRead %v", len(d.Servers), d.SnapshotRead)
	}
}

// With Keeper's data in the mix, the merged data keeps Keeper's data age and
// status message, and carries both sources' storage and IPv4 prices.
func TestMuxMergesEmbeddedPricesAndKeepsKeepersPublishedAt(t *testing.T) {
	published := time.Date(2026, 11, 14, 0, 8, 26, 0, time.UTC)
	kd := synthetic()
	kd.Version, kd.SnapshotRead, kd.AuxPricesRead, kd.PublishedAt = "kv", true, true, published
	kd.StoragePrices = []model.StoragePrice{{Vendor: "v", RegionID: "region-1", StorageID: "ssd", Unit: "GB/month", USD: 0.1}}
	kd.IPv4Prices = []model.IPv4Price{{Vendor: "v", RegionID: "region-1", Unit: "hour", USD: 0.005}}
	sink := &priceSink{StaticFacts: ociFacts(t)}
	sink.ZonesByVendor["v"] = syntheticFacts().ZonesByVendor["v"]
	mux := &catalog.MuxSource{Default: &fakeSource{data: kd}, Vendors: map[string]catalog.Source{"oci": embeddedOCI(t)}}
	c := catalog.New(catalog.Options{Source: mux, Facts: sink})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil, "oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := c.Status("c")
	if !st.Ready || !strings.Contains(st.Message, "published 2026-11-14T00:08:26Z") || !st.KeeperAuth.Authorized {
		t.Errorf("status = %+v", st)
	}
	if sink.calls != 1 {
		t.Fatalf("%d price sink calls, want 1", sink.calls)
	}
	vendors := map[string]int{}
	for _, p := range sink.storage {
		vendors[p.Vendor]++
	}
	ipv4 := map[string]int{}
	for _, p := range sink.ipv4 {
		ipv4[p.Vendor]++
	}
	if vendors["v"] != 1 || vendors["oci"] != 4*44 || ipv4["v"] != 1 || ipv4["oci"] != 0 {
		t.Errorf("storage prices %v, IPv4 prices %v", vendors, ipv4)
	}
	if byName(all(t, c, catalog.Filter{}), "oci.VM.Standard.E5.Flex.2-16") == nil {
		t.Error("the embedded type is missing from the merged catalog")
	}
}

// An OCI-only load sets the embedded prices on the driver, and leaves Keeper's
// state (auth, data version, published-at) unknown: Keeper is not read.
func TestEmbeddedOnlyLoadFeedsThePriceSinkAndLeavesKeeperStateUnknown(t *testing.T) {
	sink := &priceSink{StaticFacts: ociFacts(t)}
	mux := &catalog.MuxSource{Default: &fakeSource{err: io.ErrUnexpectedEOF}, Vendors: map[string]catalog.Source{"oci": embeddedOCI(t)}}
	c := catalog.New(catalog.Options{Source: mux, Facts: sink})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || len(sink.storage) != 4*44 || len(sink.ipv4) != 0 || sink.version == "" {
		t.Fatalf("price sink: %d calls, %d storage and %d IPv4 prices (free: none)", sink.calls, len(sink.storage), len(sink.ipv4))
	}
	st := c.Status("c")
	if !st.Ready || strings.Contains(st.Message, "Keeper") || st.KeeperAuth.Known {
		t.Errorf("status = %+v", st)
	}
}

func TestEmbeddedVendorGaugeAndNoKeeperPublishedAtMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := catalog.New(catalog.Options{
		Source: embeddedOCI(t), Facts: ociFacts(t), Registerer: reg, EmbeddedVendors: []string{"oci"},
	})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	gauge := map[string]float64{}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "scarpenter_catalog_embedded_vendor":
			for _, m := range mf.GetMetric() {
				gauge[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
			}
		case "scarpenter_catalog_data_published_timestamp_seconds", "scarpenter_catalog_keeper_unauthorized":
			t.Errorf("%s is exported for data that did not come from Keeper", mf.GetName())
		}
	}
	if len(gauge) != 1 || gauge["oci"] != 1 {
		t.Errorf("embedded vendor gauge = %v", gauge)
	}
}
