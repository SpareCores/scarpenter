/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// ImageSpec describes an image a user brought, for the image subtests.
type ImageSpec struct {
	// Arch is amd64 or arm64.
	Arch string
	// Channel and Version are the Flatcar release the image says it is, in the
	// way the vendor's convention reads it back (labels, tags, name); empty
	// Version means the image says nothing about its release.
	Channel string
	Version string
	// Tags are the image's tags or labels.
	Tags map[string]string
	// Created is the creation time; zero lets the vendor choose.
	Created time.Time
}

// SeedImageFunc makes an image the way a user would, in the region, and returns
// its ID. The image is in the scope of the driver's default image owner, so that
// a selector without match.owner finds it too.
type SeedImageFunc func(t *testing.T, d vendors.Driver, region string, spec ImageSpec) string

// The release the image cases pin.
const (
	imageChannel = "stable"
	imageVersion = "4757.2.3"
)

// imageCases is what the cases of the `images` kind work with.
type imageCases struct {
	t    *testing.T
	e    *env
	caps vendors.SelectorCaps
	tag  string
}

// images runs the cases of the `images` resource kind (plan 14 A1) for a driver
// that accepts it: the same assertions for every vendor. Explicit IDs, a selector
// by tags, the architecture of the request, and above all the version check of
// ADR-017: an image that says another release, or none, is never returned.
func images(t *testing.T, e *env) {
	mr, ok := e.d.(vendors.ManagedResources)
	if !ok {
		t.Skip("the driver has no managed resources")
	}
	caps, ok := mr.Selectors()[selector.ImagesKind]
	if !ok {
		t.Skip("the driver accepts no images selector")
	}
	if e.fx.SeedImage == nil {
		t.Skip("the fixture has no SeedImage")
	}
	c := &imageCases{t: t, e: e, caps: caps, tag: fmt.Sprintf("conf-image-%d", counter.Add(1))}
	c.explicitIDs()
	c.versionCheck()
	c.ordering()
	c.placeholders()
	c.defaultImage()
}

func (c *imageCases) seed(spec ImageSpec) string {
	c.t.Helper()
	tags := map[string]string{"conf-image": c.tag}
	maps.Copy(tags, spec.Tags)
	spec.Tags = tags
	return c.e.fx.SeedImage(c.t, c.e.d, c.e.region, spec)
}

func (c *imageCases) good(arch string) ImageSpec {
	return ImageSpec{Arch: arch, Channel: imageChannel, Version: imageVersion}
}

// req builds the request the way the core does: the selector with the region's
// IDs and the placeholders expanded.
func (c *imageCases) req(arch string, u common.ResourceUse) vendors.ImageRequest {
	c.t.Helper()
	x := selector.Context{Region: c.e.region, Arch: arch, Channel: imageChannel, Version: imageVersion,
		NodeClass: nodeClassName}
	if an, ok := c.e.d.(vendors.ArchNamer); ok {
		x.VendorArch = an.VendorArch
	}
	ref, err := selector.Build(&u, x, c.caps)
	if err != nil {
		c.t.Fatalf("building %+v: %v", u, err)
	}
	return vendors.ImageRequest{Region: c.e.region, Arch: arch, Channel: imageChannel, Version: imageVersion, Use: ref}
}

func (c *imageCases) ids(id ...string) common.ResourceUse {
	return common.ResourceUse{IDs: map[string]common.IDList{c.e.region: id}}
}

func (c *imageCases) resolve(arch string, u common.ResourceUse) (*vendors.Image, error) {
	c.t.Helper()
	return c.e.d.ResolveImage(c.t.Context(), c.req(arch, u))
}

// explicitIDs: an explicit ID of the pinned release resolves and says its release; a
// list that holds both architectures is decided by the request's architecture, a
// match.architecture that excludes it finds nothing, and a missing ID is NotFound.
func (c *imageCases) explicitIDs() {
	t := c.t
	amd := c.seed(c.good(archAMD64))
	img, err := c.resolve(archAMD64, c.ids(amd))
	if err != nil || img.Ref != amd {
		t.Fatalf("an explicit ID: %+v, %v; want %s", img, err, amd)
	}
	if img.Version != "" && (img.Version != imageVersion || img.Channel != imageChannel) {
		t.Errorf("the image says %s %s, want %s %s", img.Channel, img.Version, imageChannel, imageVersion)
	}
	arm := c.seed(c.good(archARM64))
	for arch, want := range map[string]string{archAMD64: amd, archARM64: arm} {
		if got, err := c.resolve(arch, c.ids(amd, arm)); err != nil || got.Ref != want {
			t.Errorf("both architectures listed, %s: %+v, %v; want %s", arch, got, err, want)
		}
	}
	excl := c.ids(amd)
	excl.Match = &common.ResourceMatch{Architecture: []string{archARM64}}
	if _, err := c.resolve(archAMD64, excl); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an excluded architecture: %v, want NotFound", err)
	}
	if _, err := c.resolve(archAMD64, c.ids(amd+"-missing")); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a missing ID: %v, want NotFound", err)
	}
}

// versionCheck: another release, another channel, or none, is never returned,
// whoever named the image.
func (c *imageCases) versionCheck() {
	wrongVersion := c.good(archAMD64)
	wrongVersion.Version = "4500.1.0"
	wrongChannel := c.good(archAMD64)
	wrongChannel.Channel = "beta"
	silent := c.good(archAMD64)
	silent.Version, silent.Channel = "", ""
	for name, tc := range map[string]struct {
		spec ImageSpec
		code string
	}{
		"another version": {wrongVersion, vendors.CodeImageVersionMismatch},
		"another channel": {wrongChannel, vendors.CodeImageVersionMismatch},
		"no version":      {silent, vendors.CodeImageVersionUnknown},
	} {
		got, err := c.resolve(archAMD64, c.ids(c.seed(tc.spec)))
		if got != nil || vendors.CodeOf(err) != tc.code || vendors.ClassOf(err) != vendors.Fatal {
			c.t.Errorf("%s: %+v, %v; want a Fatal %s and no image", name, got, err, tc.code)
		}
	}
}

// ordering: a selector by tags with several matches: exactlyOne is ambiguous,
// newest and oldest choose by creation time.
func (c *imageCases) ordering() {
	t := c.t
	t0 := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	own := c.tag + "-order"
	mk := func(created time.Time) string {
		spec := c.good(archAMD64)
		spec.Created = created
		spec.Tags = map[string]string{"conf-image-order": own}
		return c.e.fx.SeedImage(t, c.e.d, c.e.region, spec)
	}
	olderID, newerID := mk(t0), mk(t0.Add(24*time.Hour))
	ordered := common.ResourceUse{Tags: map[string]string{"conf-image-order": own}}
	if _, err := c.resolve(archAMD64, ordered); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("two matches under exactlyOne: %v, want Fatal", err)
	}
	for sel, want := range map[common.Select]string{common.SelectNewest: newerID, common.SelectOldest: olderID} {
		u := ordered
		u.Select, u.OrderBy = sel, orderCreated
		if got, err := c.resolve(archAMD64, u); err != nil || got.Ref != want {
			t.Errorf("%s: %+v, %v; want %s", sel, got, err, want)
		}
	}
}

// placeholders: a tag value is expanded with the pinned release before the vendor
// is asked.
func (c *imageCases) placeholders() {
	spec := c.good(archAMD64)
	spec.Tags = map[string]string{"conf-release": imageChannel + "-" + imageVersion}
	rel := c.seed(spec)
	byRelease := common.ResourceUse{Tags: map[string]string{"conf-release": "{channel}-{version}"}}
	if got, err := c.resolve(archAMD64, byRelease); err != nil || got.Ref != rel {
		c.t.Errorf("a selector with {channel} and {version}: %+v, %v; want %s", got, err, rel)
	}
}

// defaultImage: without a selector the driver finds the release by its own
// convention, or has none; whatever it returns says it is the pinned release.
func (c *imageCases) defaultImage() {
	got, err := c.e.d.ResolveImage(c.t.Context(), vendors.ImageRequest{
		Region: c.e.region, Arch: archAMD64, Channel: imageChannel, Version: imageVersion})
	switch {
	case err != nil && !vendors.IsClass(err, vendors.NotFound):
		c.t.Errorf("the default image: %v, want an image or NotFound", err)
	case err == nil && got.Version != "" && got.Version != imageVersion:
		c.t.Errorf("the default image says %s, want %s", got.Version, imageVersion)
	}
}
