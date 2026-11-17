/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

// ociFacts are the zones of the committed OCI snapshot, named as the driver's
// tenancy would (Keeper's zone ID as ID, the zone's api_reference as name).
func ociFacts(t testing.TB) catalog.StaticFacts {
	t.Helper()
	regions := catalogtest.OCIRows[keeper.Region](t, "region/oci")
	zones := catalogtest.OCIRows[keeper.Zone](t, "zone/oci")
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

func ociSource(t testing.TB) *catalog.KeeperSource {
	t.Helper()
	return catalogtest.SourceAt(catalogtest.ServerWithOCI(t).URL)
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

// Keeper's OCI rows share one api_reference per flexible shape: rule R names
// the sized types by their server ID, one type per size.
func TestKeeperServesTheSizedOCITypesWithPricesInEveryAD(t *testing.T) {
	c := ociCatalog(t, ociSource(t))
	its := all(t, c, catalog.Filter{})
	sized, shapes := 0, map[string]bool{}
	for _, it := range its {
		if m := sizedOCIName.FindStringSubmatch(it.Name); m != nil {
			sized++
			shapes[m[1]] = true
		}
	}
	if sized != 256 || len(shapes) != 12 {
		t.Errorf("%d sized types of %d shapes, want 256 of 12 (%d types in all)", sized, len(shapes), len(its))
	}
	// Keeper lists the VM.Standard.E6-HM.Ax.Flex sizes without a price: the
	// catalog skips a server that has none.
	for _, it := range its {
		if strings.Contains(it.Name, "E6-HM") {
			t.Errorf("%s is in the catalog without a price", it.Name)
		}
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
}

func TestKeeperOCIDataFacts(t *testing.T) {
	src := ociSource(t)
	req := catalog.Request{
		Scope:   catalog.Scope{"oci": {"eu-frankfurt-1"}},
		Metrics: []catalog.MetricQuery{{ID: "m1", Vendor: "oci"}},
	}
	d, err := src.Load(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !d.SnapshotRead || !d.AuxPricesRead || d.PublishedAt.IsZero() || d.Version != catalogtest.NewHash {
		t.Errorf("snapshotRead %v, auxPricesRead %v, publishedAt %v, version %q",
			d.SnapshotRead, d.AuxPricesRead, d.PublishedAt, d.Version)
	}
	// The storage prices are not selected by scope; the IPv4 price is free (0),
	// which the catalog drops.
	if len(d.Vendors) != 1 || d.Vendors[0].ID != "oci" || len(d.StoragePrices) != 4*44 || len(d.IPv4Prices) != 0 {
		t.Errorf("%d vendors, %d storage prices, %d IPv4 prices", len(d.Vendors), len(d.StoragePrices), len(d.IPv4Prices))
	}
	// Keeper has no benchmark scores of OCI yet.
	if len(d.Scores) != 0 || d.Invalid["m1"] == "" {
		t.Errorf("scores %v, invalid %v", d.Scores, d.Invalid)
	}
	// Rule R named the sized servers by their server ID.
	for _, s := range d.Servers {
		if s.APIReference != s.ID {
			t.Fatalf("server %s has api_reference %q", s.ID, s.APIReference)
		}
	}
}

// Keeper's vendor row of OCI has one value for the whole vendor: a one hour
// minimum (bare metal) and stopped servers charged (the shapes that are not
// Standard, VM.Optimized3 or VM.GPU.A10). The OCI driver refines both per shape.
func TestKeeperOCIBillingFacts(t *testing.T) {
	c := ociCatalog(t, ociSource(t))
	f, ok := c.BillingFacts("oci")
	if !ok || f.Increment != time.Second || f.Minimum != time.Hour || f.StoppedCharged == nil || !*f.StoppedCharged || f.Comment == "" {
		t.Errorf("billing facts of oci = %+v, %v", f, ok)
	}
}

func TestKeeperOCIFeedsThePriceSink(t *testing.T) {
	sink := &priceSink{StaticFacts: ociFacts(t)}
	c := catalog.New(catalog.Options{Source: ociSource(t), Facts: sink})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || len(sink.storage) != 4*44 || len(sink.ipv4) != 0 || sink.version == "" {
		t.Fatalf("price sink: %d calls, %d storage and %d IPv4 prices (free: none)", sink.calls, len(sink.storage), len(sink.ipv4))
	}
	if st := c.Status("c"); !st.Ready || !strings.Contains(st.Message, "Keeper data version") {
		t.Errorf("status = %+v", st)
	}
}
