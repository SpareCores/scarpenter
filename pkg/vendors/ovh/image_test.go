/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ovhapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

func req(region, arch string, use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: region, Arch: arch, Channel: "stable", Version: "4757.2.3", Use: use}
}

func flatcar(arch string) osapi.Image {
	prop := "x86_64"
	if arch == archARM64 {
		prop = "aarch64"
	}
	return osapi.Image{
		Name: ImageName("stable", "4757.2.3", arch), Properties: map[string]string{"architecture": prop},
		Tags: []string{"flatcar", "flatcar-stable-4757.2.3"},
	}
}

func TestImageRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		im           osapi.Image
		channel, ver string
		conflict     bool
	}{
		"name":           {osapi.Image{Name: "flatcar-stable-4757.2.3-amd64"}, "stable", "4757.2.3", false},
		"properties":     {osapi.Image{Name: "x", Properties: map[string]string{propFlatcarChannel: "Beta", propFlatcarVersion: "4800.0.1"}}, "beta", "4800.0.1", false},
		"tag":            {osapi.Image{Name: "x", Tags: []string{"flatcar", "flatcar-lts-4081.3.2"}}, "lts", "4081.3.2", false},
		"all agree":      {osapi.Image{Name: "flatcar-stable-4757.2.3-arm64", Tags: []string{"flatcar-stable-4757.2.3"}, Properties: map[string]string{propFlatcarVersion: "4757.2.3"}}, "stable", "4757.2.3", false},
		"name vs tag":    {osapi.Image{Name: "flatcar-stable-4757.2.3-amd64", Tags: []string{"flatcar-stable-4500.1.0"}}, "stable", "4757.2.3", true},
		"name vs chan":   {osapi.Image{Name: "flatcar-stable-4757.2.3-amd64", Tags: []string{"flatcar-beta-4757.2.3"}}, "stable", "4757.2.3", true},
		"says nothing":   {osapi.Image{Name: "ubuntu", Tags: []string{"flatcar"}}, "", "", false},
		"no channel yet": {osapi.Image{Name: "x", Properties: map[string]string{propFlatcarVersion: "4757.2.3"}, Tags: []string{"flatcar-stable-4757.2.3"}}, "stable", "4757.2.3", false},
	} {
		c, v, conflict := imageRelease(tc.im)
		if c != tc.channel || v != tc.ver || conflict != tc.conflict {
			t.Errorf("%s: %q %q %v", name, c, v, conflict)
		}
	}
}

func TestResolveImageByConvention(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	id := w.cloud.AddImage(testRegion, flatcar(archAMD64))
	arm := w.cloud.AddImage(testRegion, flatcar(archARM64))
	// A public image of another project with the same name is never taken: the
	// default lookup is the project's own private images.
	other := flatcar(archAMD64)
	other.Owner, other.Visibility = "someone-else", "public"
	w.cloud.AddImage(testRegion, other)
	// An image that is not active is not found.
	broken := flatcar(archAMD64)
	broken.Status = "queued"
	w.cloud.AddImage(multiRegion, broken)

	got, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{}))
	if err != nil || got.Ref != id || got.Version != "4757.2.3" || got.Channel != "stable" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := d.ResolveImage(ctx, req(testRegion, archARM64, vendors.UseRef{})); err != nil || got.Ref != arm {
		t.Errorf("arm64: %+v %v", got, err)
	}
	_, err = d.ResolveImage(ctx, req(multiRegion, archAMD64, vendors.UseRef{}))
	if !vendors.IsClass(err, vendors.NotFound) || !strings.Contains(err.Error(), "ovh-images.md") {
		t.Errorf("no active image: %v", err)
	}
	if _, err := d.ResolveImage(ctx, req(testRegion, "s390x", vendors.UseRef{})); vendors.CodeOf(err) != CodeImageArch {
		t.Errorf("an architecture without a name: %v", err)
	}
	// Another release is pinned: the convention name says 4757.2.3.
	r := req(testRegion, archAMD64, vendors.UseRef{})
	r.Version = "4800.0.0"
	if _, err := d.ResolveImage(ctx, r); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("another version: %v", err)
	}
	// The miss is remembered for a while; the hit is cached.
	w.cloud.InjectError(testRegion, "ListImages", &osapi.APIError{Service: "glance", Status: 500}, 0)
	if got, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{})); err != nil || got.Ref != id {
		t.Errorf("cached: %+v %v", got, err)
	}
	if _, err := d.ResolveImage(ctx, req(multiRegion, archAMD64, vendors.UseRef{})); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("cached miss: %v", err)
	}
	w.cloud.ClearErrors()
}

func TestResolveImageSelectors(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	community := flatcar(archAMD64)
	community.Name, community.Visibility, community.Owner = "public-flatcar", osapi.VisibilityComm, "sparecores"
	cid := w.cloud.AddImage(testRegion, community)
	withProps := flatcar(archAMD64)
	withProps.Name = "mine"
	withProps.Properties["team"] = "db"
	pid := w.cloud.AddImage(testRegion, withProps)

	// Community images are listed only when the visibility asks for them.
	use := vendors.UseRef{Name: "public-flatcar", Filters: []common.ResourceFilter{{Name: "visibility", Values: []string{"community"}}}}
	if got, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); err != nil || got.Ref != cid {
		t.Errorf("community: %+v %v", got, err)
	}
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{Name: "public-flatcar"})); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("community without the filter: %v", err)
	}
	// An owner of another project, and the project itself.
	use = vendors.UseRef{Name: "public-flatcar", Match: common.ResourceMatch{Owner: []string{"sparecores"}},
		Filters: []common.ResourceFilter{{Name: "visibility", Values: []string{"community"}}}}
	if got, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); err != nil || got.Ref != cid {
		t.Errorf("owner: %+v %v", got, err)
	}
	r := req(testRegion, archAMD64, vendors.UseRef{Name: "mine", Match: common.ResourceMatch{Owner: []string{"self"}}})
	if got, err := d.ResolveImage(ctx, r); err != nil || got.Ref != pid {
		t.Errorf("self: %+v %v", got, err)
	}
	r = req(testRegion, archAMD64, vendors.UseRef{Name: "mine"})
	r.Owner = "proj-0"
	if got, err := d.ResolveImage(ctx, r); err != nil || got.Ref != pid {
		t.Errorf("the owner of the settings: %+v %v", got, err)
	}
	// A property filter is re-applied to the response.
	use = vendors.UseRef{Name: "mine", Filters: []common.ResourceFilter{{Name: "property:team", Values: []string{"db", "web"}}}}
	if got, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); err != nil || got.Ref != pid {
		t.Errorf("property: %+v %v", got, err)
	}
	use.Filters[0].Values = []string{"web"}
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a property that does not match: %v", err)
	}
	// A tag selector (k=v strings) and a glob name.
	use = vendors.UseRef{Name: "m*", Tags: map[string]string{"flatcar-stable-4757.2.3": "*"}}
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a plain tag is not a k=v tag: %v", err)
	}
	// Select all is refused, the architecture of the match is narrowed.
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{Name: "mine", Select: common.SelectAll})); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("select all: %v", err)
	}
	use = vendors.UseRef{Name: "mine", Match: common.ResourceMatch{Architecture: []string{archARM64}}}
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, use)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an excluded architecture: %v", err)
	}
	// The release must be the pinned one and must not contradict itself.
	wrong := flatcar(archAMD64)
	wrong.Name = "wrong"
	wrong.Tags = []string{"flatcar-stable-4500.1.0"}
	wid := w.cloud.AddImage(testRegion, wrong)
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{IDs: []string{wid}})); vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("another release: %v", err)
	}
	conflict := flatcar(archAMD64)
	conflict.Tags = []string{"flatcar-stable-4500.1.0"}
	conflict.Name = "flatcar-stable-4757.2.3-amd64"
	cfl := w.cloud.AddImage(testRegion, conflict)
	if _, err := d.ResolveImage(ctx, req(testRegion, archAMD64, vendors.UseRef{IDs: []string{cfl}})); vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("a contradiction: %v", err)
	}
}

func TestResolveImageRetriesWithTheLastAnswer(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	id := w.cloud.AddImage(testRegion, flatcar(archAMD64))
	r := req(testRegion, archAMD64, vendors.UseRef{})
	if _, err := d.ResolveImage(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Expire the cache entry and fail the list: the stale image is served, not an error.
	d.mu.Lock()
	for k, c := range d.images {
		c.until = d.clock.Now().Add(-1)
		d.images[k] = c
	}
	d.mu.Unlock()
	w.cloud.InjectError(testRegion, "ListImages", &osapi.APIError{Service: "glance", Status: 503}, 1)
	if got, err := d.ResolveImage(ctx, r); err != nil || got.Ref != id {
		t.Errorf("stale on a transient error: %+v %v", got, err)
	}
	r2 := req(multiRegion, archAMD64, vendors.UseRef{})
	w.cloud.InjectError(multiRegion, "ListImages", &osapi.APIError{Service: "glance", Status: 503}, 1)
	if _, err := d.ResolveImage(ctx, r2); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a transient error without a last answer: %v", err)
	}
}

func TestReportImages(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	id := w.cloud.AddImage(testRegion, flatcar(archAMD64))
	bad := flatcar(archAMD64)
	bad.Name, bad.Tags = "x", []string{"flatcar-stable-4500.1.0"}
	w.cloud.AddImage(multiRegion, bad)
	nc := &v1alpha1.SCNodeClass{}
	nc.Name = "nc"
	nc.Spec.Flatcar = v1alpha1.FlatcarSpec{Version: "4757.2.3"}
	nc.Spec.Vendors.OVH = &ovhapi.Settings{}
	got, err := d.ReportImages(ctx, nc)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	byRegion := map[string]v1alpha1.ImageStatus{got[0].Region: got[0], got[1].Region: got[1]}
	if s := byRegion[testRegion]; !s.Ready || s.Ref != id || s.Arch != archAMD64 {
		t.Errorf("%+v", s)
	}
	if s := byRegion[multiRegion]; s.Ready || s.Message == "" {
		t.Errorf("%+v", s)
	}
	// The NodeClass's own selector, with the version mismatch as the reason.
	nc.Spec.Vendors.OVH.Regions = []string{multiRegion}
	nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{Name: "x"}}}
	got, err = d.ReportImages(ctx, nc)
	if err != nil || len(got) != 1 || got[0].Ready || got[0].Reason != vendors.CodeImageVersionMismatch {
		t.Errorf("%+v %v", got, err)
	}
	w.cloud.InjectError(multiRegion, "ListImages", &osapi.APIError{Service: "glance", Status: 503}, 0)
	d2 := w.driver(t)
	if _, err := d2.ReportImages(ctx, nc); err == nil {
		t.Error("a transient failure is an error, not a verdict")
	}
}
