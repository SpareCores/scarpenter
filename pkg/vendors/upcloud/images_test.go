/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ucapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/upcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
)

func imageReq(zone string) vendors.ImageRequest {
	return vendors.ImageRequest{Region: zone, Arch: "amd64", Channel: "stable", Version: testVersion}
}

func TestResolveImageFindsTheConventionAndReportsTheTemplateSize(t *testing.T) {
	e := newEnv(t, envOptions{})
	img, err := e.d.ResolveImage(e.ctx(), imageReq(testZone))
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != e.template(testZone) || img.Version != testVersion || img.Channel != "stable" || img.RootDiskBytes != 10<<30 {
		t.Errorf("image %+v", img)
	}
	_, err = e.d.ResolveImage(e.ctx(), imageReq("de-fra1"))
	if !vendors.IsClass(err, vendors.NotFound) || !strings.Contains(err.Error(), "import the Flatcar template for "+testVersion) {
		t.Errorf("a zone without a template: %v", err)
	}
	other := imageReq(testZone)
	other.Version = "4500.1.0"
	if _, err := e.d.ResolveImage(e.ctx(), other); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("another release (no template of it): %v", err)
	}
	arm := imageReq(testZone)
	arm.Arch = "arm64"
	if _, err := e.d.ResolveImage(e.ctx(), arm); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("arm64 by default: %v", err)
	}
}

func TestATemplateOfAnotherTierIsTheSiblingOfTheSameRelease(t *testing.T) {
	e := newEnv(t, envOptions{})
	std := e.f.AddTemplate(testZone, "flatcar-stable-"+testVersion+"-amd64-standard", upcloud.StorageTierStandard, 10,
		flatcarLabels(upcloud.StorageTierStandard, testVersion))
	max := e.template(testZone)
	img, err := e.d.ResolveImage(e.ctx(), imageReq(testZone))
	if err != nil || img.Ref != max {
		t.Fatalf("both tiers: %+v, %v; want the MaxIOPS one %s", img, err, max)
	}
	// A STARTER plan includes Standard storage: its default disk takes the Standard
	// template, a PREMIUM plan the MaxIOPS one; both come from the one image.
	for plan, want := range map[string]string{"STARTER-1xCPU-1GB": upcloud.StorageTierStandard, testType: upcloud.StorageTierMaxIOPS} {
		req := e.request("n-"+strings.ToLower(plan[:4]), plan)
		req.Image.Ref = img.Ref
		inst, err := e.d.CreateInstance(e.ctx(), req)
		if err != nil {
			t.Fatalf("%s: %v", plan, err)
		}
		srv, _ := e.f.Server(inst.Ref.ID())
		if got := srv.StorageDevices[0].Tier; got != want {
			t.Errorf("%s: root tier %s, want %s", plan, got, want)
		}
	}
	// An explicit tier picks the sibling too.
	req := e.request("n-explicit", testType)
	req.Image.Ref = img.Ref
	req.Settings = settings(t, e.d, `{"storage":{"root":{"tier":"standard"}}}`, defaultNet(), nil, options.Selection{})
	inst, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	if srv, _ := e.f.Server(inst.Ref.ID()); srv.StorageDevices[0].Tier != upcloud.StorageTierStandard {
		t.Errorf("explicit standard root: %+v", srv.StorageDevices)
	}
	_ = std
	// Two templates of one tier are ambiguous under the default select.
	e.f.AddTemplate(testZone, "second", upcloud.StorageTierMaxIOPS, 10, flatcarLabels(upcloud.StorageTierMaxIOPS, testVersion))
	if _, err := e.d.ResolveImage(e.ctx(), imageReq(testZone)); vendors.CodeOf(err) != "ImageAmbiguous" {
		t.Errorf("two MaxIOPS templates: %v", err)
	}
}

func TestATierWithoutATemplateIsUnsupportedAndSaysWhatToImport(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, err := e.d.ResolveImage(e.ctx(), imageReq(testZone)); err != nil {
		t.Fatal(err)
	}
	req := e.request("n1", testType)
	req.Settings = settings(t, e.d, `{"storage":{"root":{"tier":"standard"}}}`, defaultNet(), nil, options.Selection{})
	_, err := e.d.CreateInstance(e.ctx(), req)
	if vendors.CodeOf(err) != "TierMismatch" || !vendors.IsClass(err, vendors.Unsupported) || !strings.Contains(err.Error(), "flatcar-tier") {
		t.Errorf("a Standard root without a Standard template: %v", err)
	}
	// Supported now knows the tiers of the zone: a Standard profile is left out.
	p := vendors.Profiles{vendors.ProfileStorage: {
		"std": []byte(`{"root":{"tier":"standard"}}`), "max": []byte(`{"root":{"tier":"maxiops","size":"30Gi"}}`),
		"huge": []byte(`{"root":{"size":"2000Gi"}}`), "many": []byte(`{"data":[{"name":"a","tier":"hdd","size":"10Gi","mountPath":"/a"},` +
			`{"name":"b","tier":"hdd","size":"10Gi","mountPath":"/b"},{"name":"c","tier":"hdd","size":"10Gi","mountPath":"/c"},` +
			`{"name":"d","tier":"hdd","size":"10Gi","mountPath":"/d"}]}`),
		"small": []byte(`{"data":[{"name":"a","tier":"hdd","size":"5Gi","mountPath":"/a"}]}`),
		"bad":   []byte(`{"root":`),
	}}
	got := e.d.Supported(vendors.CatalogType{APIReference: testType}, testZone, p)
	if g := got[vendors.ProfileStorage]; len(g) != 1 || g[0] != "max" {
		t.Errorf("Supported = %v, want only the maxiops profile", got)
	}
	// A zone whose templates are unknown offers every tier.
	if g := e.d.Supported(vendors.CatalogType{}, "de-fra1", vendors.Profiles{vendors.ProfileStorage: {
		"std": []byte(`{"root":{"tier":"standard"}}`)}}); len(g[vendors.ProfileStorage]) != 1 {
		t.Errorf("an unknown zone: %v", g)
	}
	if g := e.d.Supported(vendors.CatalogType{}, testZone, vendors.Profiles{vendors.ProfilePlacement: {
		"ok": []byte(`{"serverGroup":{"antiAffinity":"strict"}}`), "none": []byte(`{}`), "bad": []byte(`[`),
	}}); len(g[vendors.ProfilePlacement]) != 1 {
		t.Errorf("placement profiles: %v", g)
	}
}

func TestImageSelectorsByIDsTagsAndFilters(t *testing.T) {
	e := newEnv(t, envOptions{})
	std := e.f.AddTemplate(testZone, "other-standard", upcloud.StorageTierStandard, 10,
		map[string]string{uc.LabelFlatcarChannel: "stable", uc.LabelFlatcarVersion: testVersion, "mine": "yes"})
	req := imageReq(testZone)
	req.Use = vendors.UseRef{Tags: map[string]string{"mine": "yes"}, Filters: []common.ResourceFilter{
		{Name: uc.FilterTier, Values: []string{"standard"}}, {Name: uc.FilterTemplateType, Values: []string{"native"}},
	}}
	img, err := e.d.ResolveImage(e.ctx(), req)
	if err != nil || img.Ref != std {
		t.Fatalf("by tag and tier filter: %+v, %v", img, err)
	}
	req.Use.Filters = []common.ResourceFilter{{Name: uc.FilterTier, Values: []string{"maxiops"}}}
	if _, err := e.d.ResolveImage(e.ctx(), req); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a tier filter that excludes it: %v", err)
	}
	req.Use = vendors.UseRef{IDs: []string{std}, Select: common.SelectAll}
	if _, err := e.d.ResolveImage(e.ctx(), req); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("select all: %v", err)
	}
	req.Use = vendors.UseRef{IDs: []string{std}, Match: common.ResourceMatch{Architecture: []string{"arm64"}}}
	if _, err := e.d.ResolveImage(e.ctx(), req); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an architecture that excludes it: %v", err)
	}
}

// Every selector, not only the label convention, gives amd64 templates only:
// arm64 is Unsupported, a template labelled with another architecture never
// matches, and a launch from such a template is refused.
func TestEverySelectorRejectsOtherArchitectures(t *testing.T) {
	e := newEnv(t, envOptions{})
	armLabels := flatcarLabels(upcloud.StorageTierMaxIOPS, testVersion)
	armLabels[uc.LabelFlatcarArch] = "arm64"
	armLabels["mine"] = "arm"
	arm := e.f.AddTemplate(testZone, "flatcar-arm64", upcloud.StorageTierMaxIOPS, 10, armLabels)
	for name, use := range map[string]vendors.UseRef{
		"by ID":   {IDs: []string{arm}},
		"by tags": {Tags: map[string]string{"mine": "arm"}},
	} {
		req := imageReq(testZone)
		req.Use = use
		if img, err := e.d.ResolveImage(e.ctx(), req); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("%s, an arm64 template for amd64: %+v, %v; want NotFound", name, img, err)
		}
		req.Arch = "arm64"
		if img, err := e.d.ResolveImage(e.ctx(), req); !vendors.IsClass(err, vendors.Unsupported) {
			t.Errorf("%s, arm64: %+v, %v; want Unsupported", name, img, err)
		}
	}
	req := e.request("n-arm", testType)
	req.Image.Ref = arm
	if _, err := e.d.CreateInstance(e.ctx(), req); !vendors.IsClass(err, vendors.Unsupported) ||
		!strings.Contains(err.Error(), "x86_64") {
		t.Errorf("a launch from an arm64 template: %v, want Unsupported", err)
	}
}

func class(version string, regions []string, images *common.Resource) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar: v1alpha1.FlatcarSpec{Channel: "stable", Version: version},
			Vendors: v1alpha1.VendorBlocks{UpCloud: &ucapi.Settings{Regions: regions, Resources: &ucapi.Resources{Images: images}}},
		},
	}
}

func TestReportImagesNamesTheZonesWithoutATemplate(t *testing.T) {
	e := newEnv(t, envOptions{})
	st, err := e.d.ReportImages(e.ctx(), class(testVersion, nil, nil))
	if err != nil || len(st) != 3 {
		t.Fatalf("ReportImages = %v, %v", st, err)
	}
	for _, s := range st {
		switch s.Region {
		case testZone:
			if !s.Ready || s.Ref != e.template(testZone) || s.Arch != "amd64" {
				t.Errorf("%+v", s)
			}
		default:
			if s.Ready || !strings.Contains(s.Message, "import the Flatcar template") {
				t.Errorf("%+v", s)
			}
		}
	}
	// A listed zone only, and a template that says another release.
	e.f.AddTemplate("de-fra1", "old", upcloud.StorageTierMaxIOPS, 10, flatcarLabels(upcloud.StorageTierMaxIOPS, "4500.1.0"))
	nc := class(testVersion, []string{"de-fra1"}, &common.Resource{Use: &common.ResourceUse{
		Tags: map[string]string{uc.LabelImage: "flatcar"},
	}})
	st, err = e.d.ReportImages(e.ctx(), nc)
	if err != nil || len(st) != 1 || st[0].Ready || st[0].Reason != vendors.CodeImageVersionMismatch {
		t.Errorf("a template of another release: %v, %v", st, err)
	}
	bad := class(testVersion, []string{testZone}, &common.Resource{Use: &common.ResourceUse{Name: "x-{nope}"}})
	if st, err := e.d.ReportImages(e.ctx(), bad); err != nil || len(st) != 1 || st[0].Ready || st[0].Message == "" {
		t.Errorf("a selector that does not build: %v, %v", st, err)
	}
	e.f.InjectCode("GetStorages", "AUTHENTICATION_FAILED", 401, 1)
	if _, err := e.d.ReportImages(e.ctx(), class(testVersion, nil, nil)); err == nil {
		t.Error("a failed list did not fail the report")
	}
}

func TestATemplateThatIsNotOnlineIsNotReady(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.f.SetStorageState(e.template(testZone), upcloud.StorageStateMaintenance)
	if _, err := e.d.ResolveImage(e.ctx(), imageReq(testZone)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a template in maintenance: %v", err)
	}
	e.f.SetStorageState(e.template(testZone), upcloud.StorageStateError)
	if _, err := e.d.ResolveImage(e.ctx(), imageReq(testZone)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a template in error: %v", err)
	}
}
