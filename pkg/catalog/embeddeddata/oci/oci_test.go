/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/catalog/embeddeddata/oci"
)

func body(t *testing.T, path string) *keeper.SnapshotBody {
	t.Helper()
	b, err := oci.FS.ReadFile(path + ".json.gz")
	if err != nil {
		t.Fatal(err)
	}
	return &keeper.SnapshotBody{Gzip: b}
}

func rows[T any](t *testing.T, m keeper.Manifest, path string) []T {
	t.Helper()
	f, ok := m.Lookup(path)
	if !ok {
		t.Fatalf("%s is not in the manifest", path)
	}
	b := body(t, path)
	if err := b.Verify(f.SHA256); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out, err := keeper.DecodeSnapshotRows[T](b, f.Rows)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(out) != f.Rows {
		t.Fatalf("%s: %d rows decoded, the manifest says %d", path, len(out), f.Rows)
	}
	return out
}

func manifest(t *testing.T) keeper.Manifest {
	t.Helper()
	m, err := keeper.DecodeManifest(body(t, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Every file of the manifest exists, matches its SHA-256 and row count, and no
// embedded file is missing from the manifest.
func TestFilesMatchTheManifest(t *testing.T) {
	m := manifest(t)
	want := []string{
		"ipv4_price/oci", "region/oci", "server/oci", "server_price/oci", "storage_price/oci", "vendor", "zone/oci",
	}
	got := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		got = append(got, f.Path)
		b := body(t, f.Path)
		if err := b.Verify(f.SHA256); err != nil {
			t.Errorf("%s: %v", f.Path, err)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("manifest paths = %v, want %v (sorted)", got, want)
	}
}

// The files decode into the row types of sc-keeper-go, so that a real snapshot
// of OCI parses the same way (the next three tests; the servers below).
func TestVendorRegionsAndZones(t *testing.T) {
	m := manifest(t)
	vendors := rows[keeper.Vendor](t, m, "vendor")
	if len(vendors) != 1 || vendors[0].VendorID != "oci" || vendors[0].Status != "active" ||
		vendors[0].BillingIncrementSeconds == nil || *vendors[0].BillingIncrementSeconds != 1 ||
		vendors[0].MinimumBillingSeconds == nil || *vendors[0].MinimumBillingSeconds != 60 ||
		vendors[0].StoppedServerCharged == nil || *vendors[0].StoppedServerCharged {
		t.Errorf("vendor = %+v", vendors)
	}

	regions := rows[keeper.Region](t, m, "region/oci")
	active := map[string]bool{}
	for _, r := range regions {
		if r.VendorID != "oci" || r.RegionID != r.APIReference {
			t.Errorf("region %+v", r)
		}
		if r.Status == "active" {
			active[r.RegionID] = true
		}
	}
	if len(regions) != 44 || len(active) != 3 {
		t.Errorf("%d regions, %d active; want 44 and 3", len(regions), len(active))
	}

	zones := rows[keeper.Zone](t, m, "zone/oci")
	perRegion := map[string]int{}
	for _, z := range zones {
		perRegion[z.RegionID]++
		if z.Status != "active" || !active[z.RegionID] || !strings.HasPrefix(z.ZoneID, z.RegionID+"-ad-") {
			t.Errorf("zone %+v", z)
		}
	}
	if len(zones) != 9 || len(perRegion) != 3 {
		t.Errorf("%d zones in %d regions, want 9 in 3 (three ADs each)", len(zones), len(perRegion))
	}

}

func TestStorageAndIPv4Prices(t *testing.T) {
	m := manifest(t)
	regions := rows[keeper.Region](t, m, "region/oci")
	storage := rows[keeper.StoragePrice](t, m, "storage_price/oci")
	levels := map[string]float64{}
	for _, s := range storage {
		if s.Unit != "GB/month" || s.Currency != "USD" || s.Status != "active" {
			t.Errorf("storage price %+v", s)
		}
		if s.RegionID == "eu-frankfurt-1" {
			levels[s.StorageID] = s.Price
		}
	}
	wantLevels := map[string]float64{
		"lower-cost": 0.0255, "balanced": 0.0425, "higher-performance": 0.0595, "ultra-high-performance": 0.0765,
	}
	if len(storage) != 4*len(regions) || len(levels) != 4 {
		t.Errorf("%d storage prices, levels %v", len(storage), levels)
	}
	for k, v := range wantLevels {
		if d := levels[k] - v; d > 1e-9 || d < -1e-9 {
			t.Errorf("storage %s = %v, want %v", k, levels[k], v)
		}
	}

	ipv4 := rows[keeper.IPv4Price](t, m, "ipv4_price/oci")
	if len(ipv4) != len(regions) {
		t.Errorf("%d IPv4 prices, want one per region", len(ipv4))
	}
	for _, p := range ipv4 {
		if p.Unit != "month" || p.Price != 0 || p.Currency != "USD" || p.Status != "active" {
			t.Errorf("IPv4 price %+v", p)
		}
	}
}

var sizedTail = regexp.MustCompile(`^[1-9][0-9]*-[1-9][0-9]*$`)

// sized reports the shape and the size of a sized name (plan 25, section 2:
// split at the last dot, the tail is <ocpus>-<gb>).
func sized(name string) (shape string, ok bool) {
	i := strings.LastIndex(name, ".")
	if i < 0 || !sizedTail.MatchString(name[i+1:]) {
		return "", false
	}
	return name[:i], true
}

// The counts of plan 25, section 2.
func TestServerCounts(t *testing.T) {
	m := manifest(t)
	servers := rows[keeper.TableServer](t, m, "server/oci")
	var activeN, sizedN, bm, fixedVM int
	shapes := map[string]bool{}
	for i := range servers {
		s := &servers[i]
		if s.VendorID != "oci" || s.Name != s.ServerID || s.DisplayName != s.ServerID {
			t.Errorf("server %s: vendor %q name %q display name %q", s.ServerID, s.VendorID, s.Name, s.DisplayName)
		}
		shape, isSized := sized(s.ServerID)
		switch {
		case isSized:
			if s.APIReference != shape {
				t.Errorf("%s: api_reference %q, want the shape %q", s.ServerID, s.APIReference, shape)
			}
		case s.APIReference != s.ServerID:
			t.Errorf("fixed shape %s has api_reference %q", s.ServerID, s.APIReference)
		}
		if s.Status != "active" {
			if s.Status != "inactive" {
				t.Errorf("%s: status %q", s.ServerID, s.Status)
			}
			continue
		}
		activeN++
		switch {
		case isSized:
			sizedN++
			shapes[shape] = true
		case strings.HasPrefix(s.ServerID, "BM."):
			bm++
		case strings.HasPrefix(s.ServerID, "VM."):
			fixedVM++
		default:
			t.Errorf("unexpected server %s", s.ServerID)
		}
		if s.CPUArchitecture != "x86_64" && s.CPUArchitecture != "arm64" || s.CPUAllocation != "Dedicated" {
			t.Errorf("%s: architecture %q, allocation %q", s.ServerID, s.CPUArchitecture, s.CPUAllocation)
		}
	}
	if len(servers) != 288 || activeN != 287 || sizedN != 256 || len(shapes) != 12 || bm != 16 || fixedVM != 15 {
		t.Errorf("servers %d, active %d, sized %d of %d shapes, bare metal %d, fixed VMs %d; want 288, 287, 256 of 12, 16, 15",
			len(servers), activeN, sizedN, len(shapes), bm, fixedVM)
	}
}

// Sized rows: vcpus = OCPUs x k(shape) and memory = GB x 1024 MiB (plan 25,
// section 2).
func TestSizedRowsAreConsistent(t *testing.T) {
	servers := rows[keeper.TableServer](t, manifest(t), "server/oci")
	checked := 0
	for i := range servers {
		s := &servers[i]
		shape, ok := sized(s.ServerID)
		if !ok {
			continue
		}
		a, b, _ := strings.Cut(s.ServerID[len(shape)+1:], "-")
		ocpus, errO := strconv.Atoi(a)
		gb, errG := strconv.Atoi(b)
		if errO != nil || errG != nil {
			t.Fatalf("%s: not a size", s.ServerID)
		}
		k := 2
		if strings.HasPrefix(shape, "VM.Standard.A1.") {
			k = 1
		}
		if s.VCPUs != ocpus*k {
			t.Errorf("%s: %d vCPUs, want %d x %d", s.ServerID, s.VCPUs, ocpus, k)
		}
		if s.MemoryAmount != float64(gb*1024) {
			t.Errorf("%s: memory %v MiB, want %d", s.ServerID, s.MemoryAmount, gb*1024)
		}
		checked++
	}
	if checked != 256 {
		t.Errorf("checked %d sized rows, want 256", checked)
	}
}

// The prices: counts, the one price per type and allocation in every AD and
// region, and the spot rule (plan 25, section 6): spot rows exist for the
// preemptible series of VMs, and not for the sizes beyond one NUMA node.
func TestPricesAndTheSpotRule(t *testing.T) {
	m := manifest(t)
	servers := rows[keeper.TableServer](t, m, "server/oci")
	prices := rows[keeper.ServerPrice](t, m, "server_price/oci")

	type key struct {
		server string
		alloc  keeper.PriceAllocation
	}
	price := map[key]float64{}
	var ondemand, spot int
	for _, p := range prices {
		if p.VendorID != "oci" || p.OperatingSystem != "Linux" || p.Unit != "hour" || p.Currency != "USD" ||
			p.Status != "active" || p.Price <= 0 {
			t.Fatalf("price row %+v", p)
		}
		switch p.Allocation {
		case keeper.PriceAllocationOnDemand:
			ondemand++
		case keeper.PriceAllocationSpot:
			spot++
		default:
			t.Fatalf("allocation %q", p.Allocation)
		}
		k := key{p.ServerID, p.Allocation}
		if prev, seen := price[k]; seen && prev != p.Price {
			t.Errorf("%s %s: %v and %v in different zones; prices are per region and equal in every AD", p.ServerID, p.Allocation, prev, p.Price)
		}
		price[k] = p.Price
	}
	if ondemand != 2180 || spot != 1481 {
		t.Errorf("%d on-demand and %d spot rows, want 2180 and 1481", ondemand, spot)
	}

	preemptible := map[string]bool{
		"Standard1": true, "Standard.B1": true, "Standard2": true, "Standard3": true,
		"Standard.E2": true, "Standard.E3": true, "Standard.E4": true, "Standard.E5": true, "Standard.E6": true,
		"Standard.A1": true, "Standard.A4": true, "Optimized3": true,
	}
	// Beyond one NUMA node: needs a separate limit, cannot be preemptible.
	extended := []string{
		"VM.Standard.E5.Flex.126-504", "VM.Standard.E5.Flex.126-1008", "VM.Standard.E5.Flex.126-2016",
		"VM.Standard.E4.Flex.114-456", "VM.Standard.E4.Flex.114-912",
		"VM.Standard.E3.Flex.114-456", "VM.Standard.E3.Flex.114-912",
		"VM.Standard3.Flex.56-224", "VM.Standard3.Flex.56-448", "VM.Standard3.Flex.56-896",
	}
	var noSpot []string
	for i := range servers {
		s := &servers[i]
		if s.Status != "active" {
			continue
		}
		_, hasOnDemand := price[key{s.ServerID, keeper.PriceAllocationOnDemand}]
		_, hasSpot := price[key{s.ServerID, keeper.PriceAllocationSpot}]
		if !hasOnDemand && s.ServerID != "BM.DenseIO.A4.Ax.72" {
			t.Errorf("%s has no on-demand price", s.ServerID)
		}
		want := strings.HasPrefix(s.ServerID, "VM.") && s.Series != nil && preemptible[*s.Series] &&
			!slices.Contains(extended, s.ServerID)
		if hasSpot != want {
			t.Errorf("%s (series %v): spot price %t, want %t", s.ServerID, s.Series, hasSpot, want)
		}
		if _, ok := sized(s.ServerID); ok && !hasSpot && s.Series != nil && preemptible[*s.Series] {
			noSpot = append(noSpot, s.ServerID)
		}
	}
	slices.Sort(noSpot)
	slices.Sort(extended)
	if !slices.Equal(noSpot, extended) {
		t.Errorf("sized rows of preemptible series without a spot price = %v, want the extended-memory sizes %v", noSpot, extended)
	}
}
