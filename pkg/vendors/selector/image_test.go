/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// imageDriver is an "aws" driver with an images declaration and a spelling of
// architectures; the embedded interfaces are never called.
type imageDriver struct {
	vendors.Driver
	vendors.ManagedResources
	caps map[vendors.ResourceKind]vendors.SelectorCaps
}

func (d *imageDriver) Vendor() string { return "aws" }
func (d *imageDriver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	return d.caps
}
func (d *imageDriver) VendorArch(arch string) (string, bool) {
	return map[string]string{"amd64": "x86_64", "arm64": "arm64"}[arch], arch == "amd64" || arch == "arm64"
}

func imagesClass(use *common.ResourceUse) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{}
	nc.Name = "default"
	nc.Spec.Flatcar = v1alpha1.FlatcarSpec{Version: "4757.2.3"} // the channel is not defaulted here
	nc.Spec.Vendors.AWS = &awsapi.Settings{Resources: &awsapi.Resources{Images: &common.Resource{Use: use}}}
	return nc
}

func TestImageUseExpandsTheSelectorPerRegionAndArchitecture(t *testing.T) {
	d := &imageDriver{caps: map[vendors.ResourceKind]vendors.SelectorCaps{
		selector.ImagesKind: {Name: &vendors.FieldCap{Side: vendors.SideServer}, Tags: vendors.TagsMap,
			DefaultSelect: common.SelectExactlyOne},
	}}
	nc := imagesClass(&common.ResourceUse{
		IDs:     map[string]common.IDList{"*": {"ami-1"}, "us-east-1": {"ami-2"}},
		Name:    "fc-{channel}-{version}-{arch}",
		Filters: []common.ResourceFilter{{Name: "architecture", Values: []string{"{vendorArch}"}}},
	})
	ref, given, err := selector.ImageUse(nc, d, "us-east-1", "arm64")
	if err != nil || !given {
		t.Fatalf("%v, given %v", err, given)
	}
	if ref.Name != "fc-stable-4757.2.3-arm64" || !slices.Equal(ref.IDs, []string{"ami-1", "ami-2"}) ||
		ref.Filters[0].Values[0] != "arm64" || ref.Select != common.SelectExactlyOne {
		t.Errorf("ref = %+v", ref)
	}
	if ref, _, _ := selector.ImageUse(nc, d, "eu-west-1", "amd64"); ref.Filters[0].Values[0] != "x86_64" ||
		!slices.Equal(ref.IDs, []string{"ami-1"}) {
		t.Errorf("amd64 in another region: %+v", ref)
	}
}

func TestImageUseWithoutASelectorIsNotGiven(t *testing.T) {
	d := &imageDriver{caps: map[vendors.ResourceKind]vendors.SelectorCaps{}}
	nc := imagesClass(nil)
	if ref, given, err := selector.ImageUse(nc, d, "r", "amd64"); err != nil || given || !ref.IsDefault() {
		t.Errorf("%+v, %v, %v", ref, given, err)
	}
	nc.Spec.Vendors.AWS.Resources = nil
	if _, given, err := selector.ImageUse(nc, d, "r", "amd64"); err != nil || given {
		t.Errorf("no resources: %v, %v", given, err)
	}
}

func TestImageUseErrors(t *testing.T) {
	declared := &imageDriver{caps: map[vendors.ResourceKind]vendors.SelectorCaps{selector.ImagesKind: {}}}
	undeclared := &imageDriver{caps: map[vendors.ResourceKind]vendors.SelectorCaps{}}
	ids := &common.ResourceUse{IDs: map[string]common.IDList{"eu-west-1": {"ami-1"}}}
	for name, tc := range map[string]struct {
		d    *imageDriver
		use  *common.ResourceUse
		want string
	}{
		"driver without the kind": {undeclared, ids, "does not accept an images selector"},
		"no IDs for the region":   {declared, ids, "no IDs are configured for the region us-east-1"},
		"unknown placeholder":     {declared, &common.ResourceUse{Name: "x-{nope}"}, "unknown placeholder"},
		"empty use":               {declared, &common.ResourceUse{}, "selects nothing"},
	} {
		_, _, err := selector.ImageUse(imagesClass(tc.use), tc.d, "us-east-1", "amd64")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestNarrowArchitecture(t *testing.T) {
	ref := vendors.UseRef{Name: "x"}
	if got, ok := selector.NarrowArchitecture(ref, "arm64"); !ok || !slices.Equal(got.Match.Architecture, []string{"arm64"}) {
		t.Errorf("no architecture given: %+v, %v", got, ok)
	}
	ref.Match.Architecture = []string{"amd64", "arm64"}
	if got, ok := selector.NarrowArchitecture(ref, "arm64"); !ok || !slices.Equal(got.Match.Architecture, []string{"arm64"}) {
		t.Errorf("both given: %+v, %v", got, ok)
	}
	ref.Match.Architecture = []string{"amd64"}
	if _, ok := selector.NarrowArchitecture(ref, "arm64"); ok {
		t.Error("an excluded architecture was accepted")
	}
	if len(ref.Match.Architecture) != 1 {
		t.Error("the argument was changed")
	}
}

func TestImageErrorMapsSelectionToDriverClasses(t *testing.T) {
	none := selector.ImageError(&selector.NoMatchError{Stages: []selector.Stage{{Name: "listed", Count: 0}}}, "r", "amd64")
	many := selector.ImageError(&selector.AmbiguousError{Count: 2, Stages: []selector.Stage{{Name: "listed", Count: 2}}}, "r", "amd64")
	other := errors.New("boom")
	if vendors.ClassOf(none) != vendors.NotFound || vendors.CodeOf(none) != selector.CodeImageNotFound ||
		vendors.ClassOf(many) != vendors.Fatal || vendors.CodeOf(many) != selector.CodeImageAmbiguous ||
		selector.ImageError(other, "r", "amd64") != other {
		t.Errorf("%v / %v", none, many)
	}
}
