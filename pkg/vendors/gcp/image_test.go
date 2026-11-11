/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

func imageReq(arch string) vendors.ImageRequest {
	return vendors.ImageRequest{Region: testRegion, Arch: arch, Channel: "stable", Version: "4757.2.3"}
}

func TestResolveDefaultImage(t *testing.T) {
	e := newEnv(t, envOptions{})
	img, err := e.d.ResolveImage(t.Context(), imageReq("amd64"))
	if err != nil || img.Ref != "projects/kinvolk-public/global/images/flatcar-stable-4757-2-3" ||
		img.Channel != "stable" || img.Version != "4757.2.3" || img.RootDiskBytes != 12<<30 {
		t.Fatalf("amd64 = %+v, %v", img, err)
	}
	// The answer is cached: a second resolution reads nothing.
	before := e.c.Calls("GetImage")
	if _, err := e.d.ResolveImage(t.Context(), imageReq("amd64")); err != nil || e.c.Calls("GetImage") != before {
		t.Errorf("the image is read again within the hour")
	}
	// A release that is not published is NotFound, remembered for a while.
	r := imageReq("amd64")
	r.Version = "9999.0.0"
	if _, err := e.d.ResolveImage(t.Context(), r); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an unknown release: %v", err)
	}
	// arm64 has no image of Flatcar's and the Spare Cores project is not configured.
	if _, err := e.d.ResolveImage(t.Context(), imageReq("arm64")); !vendors.IsClass(err, vendors.NotFound) ||
		!strings.Contains(err.Error(), "imageProject") {
		t.Errorf("arm64 by default: %v", err)
	}
	// An image project of the block (rung 3) serves arm64.
	r = imageReq("arm64")
	r.Owner = fake.SeedARMImageProject
	if img, err := e.d.ResolveImage(t.Context(), r); err != nil || img.Ref != "projects/"+fake.SeedARMImageProject+"/global/images/flatcar-stable-4757-2-3-arm64" {
		t.Errorf("arm64 from the image project: %+v, %v", img, err)
	}
	if _, err := e.d.ResolveImage(t.Context(), imageReq("riscv")); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("riscv: %v", err)
	}
	// The wrong architecture in the project's image is refused.
	r = imageReq("amd64")
	r.Owner = fake.SeedARMImageProject
	r.Version = "4757.2.3"
	if _, err := e.d.ResolveImage(t.Context(), r); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an amd64 request over the arm64 project: %v", err)
	}
}

func TestImageStatesAndTransientFailures(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.AddImageSpec(fake.ImageSpec{Project: "kinvolk-public", Name: "flatcar-beta-4800-0-0", Arch: "X86_64",
		Deprecated: "OBSOLETE"})
	r := imageReq("amd64")
	r.Channel, r.Version = "beta", "4800.0.0"
	if _, err := e.d.ResolveImage(t.Context(), r); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an obsolete image: %v", err)
	}
	// A deprecated one is still a pinned release: old versions stay bootable.
	e.c.AddImageSpec(fake.ImageSpec{Project: "kinvolk-public", Name: "flatcar-beta-4801-0-0", Arch: "X86_64",
		Deprecated: "DEPRECATED"})
	r.Version = "4801.0.0"
	if _, err := e.d.ResolveImage(t.Context(), r); err != nil {
		t.Errorf("a deprecated image: %v", err)
	}
	// A transient failure serves the last good image, even past its TTL.
	if _, err := e.d.ResolveImage(t.Context(), imageReq("amd64")); err != nil {
		t.Fatal(err)
	}
	e.clock.Step(2 * time.Hour)
	e.c.Inject("GetImage", fake.Error(503, "backendError", "unavailable"), 1)
	if img, err := e.d.ResolveImage(t.Context(), imageReq("amd64")); err != nil || img == nil {
		t.Errorf("a transient failure: %v", err)
	}
	e.c.Inject("GetImage", fake.Error(503, "backendError", "unavailable"), 1)
	if _, err := e.d.ResolveImage(t.Context(), imageReq("arm64")); err == nil {
		t.Error("an error is swallowed")
	}
}

func seedImage1(e *env, name, arch string, labels map[string]string, created time.Time) string {
	e.c.AddImageSpec(fake.ImageSpec{Project: testProject, Name: name, Arch: arch, Labels: labels, Created: created})
	return "projects/" + testProject + "/global/images/" + name
}

func TestImagesSelector(t *testing.T) {
	e := newEnv(t, envOptions{})
	good := map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757-2-3", "team": "x"}
	older := seedImage1(e, "mine-a", "X86_64", good, time.Now().Add(-48*time.Hour))
	newer := seedImage1(e, "mine-b", "X86_64", good, time.Now().Add(-24*time.Hour))
	use := func(u vendors.UseRef) vendors.ImageRequest { r := imageReq("amd64"); r.Use = u; return r }
	for name, tc := range map[string]struct {
		u    vendors.UseRef
		want string
	}{
		"by an exact name":  {vendors.UseRef{Name: "mine-a"}, older},
		"by a short ID":     {vendors.UseRef{IDs: []string{"mine-b"}}, newer},
		"by labels, newest": {vendors.UseRef{Tags: map[string]string{"team": "x"}, Select: common.SelectNewest, OrderBy: "created"}, newer},
		"by labels, oldest": {vendors.UseRef{Tags: map[string]string{"team": "*"}, Select: common.SelectOldest, OrderBy: "created"}, older},
		"by a native filter": {vendors.UseRef{Name: "mine-*", Filters: []common.ResourceFilter{
			{Name: "architecture", Values: []string{"X86_64"}}}, Select: common.SelectNewest, OrderBy: "created"}, newer},
		"by a family": {vendors.UseRef{Filters: []common.ResourceFilter{{Name: "family", Values: []string{"flatcar-stable"}}},
			Match: common.ResourceMatch{Owner: []string{"kinvolk-public"}}}, "projects/kinvolk-public/global/images/flatcar-stable-4757-2-3"},
		"by an owner": {vendors.UseRef{Name: "flatcar-stable-4757-2-3", Match: common.ResourceMatch{Owner: []string{"kinvolk-public"}}},
			"projects/kinvolk-public/global/images/flatcar-stable-4757-2-3"},
	} {
		img, err := e.d.ResolveImage(t.Context(), use(tc.u))
		if err != nil || img.Ref != tc.want {
			t.Errorf("%s: %+v, %v; want %s", name, img, err, tc.want)
		}
	}
	// Two matches under exactlyOne are ambiguous; select all is not valid for images.
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{Tags: map[string]string{"team": "x"}})); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an ambiguous selector: %v", err)
	}
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{Name: "mine-a", Select: common.SelectAll})); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("select all: %v", err)
	}
	// An expression is sent as written; a state and a creation window are applied.
	after := metav1.NewTime(time.Now().Add(-36 * time.Hour))
	img, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{Tags: map[string]string{"team": "x"},
		Match: common.ResourceMatch{CreatedAfter: &after}}))
	if err != nil || img.Ref != newer {
		t.Errorf("createdAfter: %+v, %v", img, err)
	}
	if img, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{Expression: `labels.team = "x" AND name = "mine-a"`})); err != nil || img.Ref != older {
		t.Errorf("an expression: %+v, %v", img, err)
	}
	// An image that does not say its release is never booted, and neither is a
	// conflicting one.
	silent := seedImage1(e, "silent", "X86_64", nil, time.Time{})
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{IDs: []string{silent}})); vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("a silent image: %v", err)
	}
	conflict := seedImage1(e, "flatcar-stable-4757-2-3", "X86_64", map[string]string{"flatcar-version": "4000-0-0"}, time.Time{})
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{IDs: []string{conflict}})); vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("a conflicting image: %v", err)
	}
	other := seedImage1(e, "old", "X86_64", map[string]string{"flatcar-channel": "stable", "flatcar-version": "4500_1_0"}, time.Time{})
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{IDs: []string{other}})); vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("another release: %v", err)
	}
	// The selector's own architecture excludes the request.
	if _, err := e.d.ResolveImage(t.Context(), use(vendors.UseRef{Name: "mine-a", Match: common.ResourceMatch{Architecture: []string{"arm64"}}})); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an excluded architecture: %v", err)
	}
}

func TestReportImages(t *testing.T) {
	e := newEnv(t, envOptions{})
	nc := &v1alpha1.SCNodeClass{}
	nc.Name = "class"
	nc.Spec.Flatcar.Version = "4757.2.3"
	nc.Spec.Vendors.GCP = &gcpapi.Settings{Regions: []string{testRegion}}
	got, err := e.d.ReportImages(t.Context(), nc)
	if err != nil || len(got) != 2 {
		t.Fatalf("report = %+v, %v", got, err)
	}
	if got[0].Arch != "amd64" || !got[0].Ready || got[0].Ref == "" {
		t.Errorf("amd64: %+v", got[0])
	}
	if got[1].Arch != "arm64" || got[1].Ready || got[1].Message == "" {
		t.Errorf("arm64 (no image, not ready): %+v", got[1])
	}
	// Without a region list every region of the project is reported, with an
	// images selector of the block.
	nc.Spec.Vendors.GCP.Regions = nil
	nc.Spec.Vendors.GCP.Resources = &gcpapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
		IDs: map[string]common.IDList{"*": {"projects/kinvolk-public/global/images/flatcar-stable-4757-2-3"}}}}}
	got, err = e.d.ReportImages(t.Context(), nc)
	if err != nil || len(got) != 4 {
		t.Fatalf("report = %+v, %v", got, err)
	}
	// A pinned release the images do not say is reported with the reason.
	nc.Spec.Flatcar.Version = "1.0.0"
	got, _ = e.d.ReportImages(t.Context(), nc)
	if got[0].Ready || got[0].Reason != vendors.CodeImageVersionMismatch {
		t.Errorf("a mismatching release: %+v", got[0])
	}
	// A transient failure fails the report: an unknown must not gate the region.
	nc.Spec.Flatcar.Version = "4757.2.3"
	e.c.Inject("GetImage", fake.Error(503, "backendError", "unavailable"), 8)
	e2 := restart(t, e.c, envOptions{})
	if _, err := e2.d.ReportImages(t.Context(), nc); err == nil {
		t.Error("a transient failure did not fail the report")
	}
}

func TestImageNames(t *testing.T) {
	for _, tc := range []struct{ id, ref string }{
		{"flatcar-stable-4757-2-3", "projects/p/global/images/flatcar-stable-4757-2-3"},
	} {
		if got := gcp.ImageURL("p", tc.id); got != tc.ref {
			t.Errorf("ImageURL = %s", got)
		}
	}
}
