/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"testing"

	"github.com/oracle/oci-go-sdk/v65/identity"
	corev1 "k8s.io/api/core/v1"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	ocidata "github.com/SpareCores/scarpenter/pkg/catalog/embeddeddata/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// The catalog built from the embedded data over the driver's own facts offers every type in every
// availability domain the tenancy lists for the region, whatever the crawler's AD numbering gave
// each shape (24 shape and region pairs of the data lack one or two ADs of Phoenix, Frankfurt or
// Ashburn): the prices are per region (decision of 2026-11-14), and the driver's check against
// ListShapes decides where a type can launch.
func TestCatalogOffersEveryTypeInEveryADOfTheTenancy(t *testing.T) {
	e := newEnv(t)
	// A Phoenix with the three ADs the crawler's tenancy had, named under this tenancy's prefix.
	e.c.AddRegion("us-phoenix-1", identity.RegionSubscriptionStatusReady, "tEsT:PHX-AD-1", "tEsT:PHX-AD-2", "tEsT:PHX-AD-3")
	src, err := catalog.NewEmbeddedSource("oci", ocidata.FS)
	if err != nil {
		t.Fatal(err)
	}
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(e.d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := catalog.New(catalog.Options{Source: src, Facts: f})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"oci": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	its, err := c.InstanceTypes(t.Context(), catalog.Query{Class: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) < 256 {
		t.Fatalf("%d types, want at least the 256 sized ones", len(its))
	}
	short := 0
	for _, it := range its {
		zones := map[string]map[string]bool{}
		for _, o := range it.Offerings {
			region := o.Requirements.Get(corev1.LabelTopologyRegion).Any()
			capacity := o.Requirements.Get("karpenter.sh/capacity-type").Any()
			if zones[capacity+"/"+region] == nil {
				zones[capacity+"/"+region] = map[string]bool{}
			}
			zones[capacity+"/"+region][o.Requirements.Get(corev1.LabelTopologyZone).Any()] = true
		}
		for key, z := range zones {
			// Frankfurt, Ashburn and Phoenix have three ADs in this tenancy.
			if len(z) != 3 {
				short++
				t.Errorf("%s: %s is offered in %d ADs, want 3", it.Name, key, len(z))
			}
		}
		if short > 10 {
			t.Fatal("too many types short of an AD")
		}
	}
}
