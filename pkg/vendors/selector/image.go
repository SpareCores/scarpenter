/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector

import (
	"errors"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultChannel is the Flatcar channel of a NodeClass that leaves it out (the
// CRD defaults it; an object built in code may not).
const DefaultChannel = "stable"

// ImageUse returns the `images` selector of the NodeClass for a request: the
// vendor block's resources.images.use with the IDs of the region gathered and the
// placeholders expanded for the region and the architecture ({arch}, and
// {vendorArch} where the driver names architectures its own way). given is false
// when the NodeClass has no images selector, which means the driver's own
// convention; a selector that selects nothing for the region (no IDs listed for
// it, a placeholder without a value) is an error.
func ImageUse(
	nc *v1alpha1.SCNodeClass, d vendors.Driver, region, arch string,
) (ref vendors.UseRef, given bool, err error) {
	vendor := d.Vendor()
	cfgs, err := options.ResourceConfig(nc, vendor)
	if err != nil {
		return vendors.UseRef{}, false, err
	}
	u := cfgs[string(ImagesKind)].Use
	if u == nil {
		return vendors.UseRef{}, false, nil
	}
	mr, ok := d.(vendors.ManagedResources)
	if !ok {
		return vendors.UseRef{}, false, fmt.Errorf("%s does not accept an images selector", vendor)
	}
	caps, ok := mr.Selectors()[ImagesKind]
	if !ok {
		return vendors.UseRef{}, false, fmt.Errorf("%s does not accept an images selector", vendor)
	}
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = DefaultChannel
	}
	x := Context{Region: region, Arch: arch, Channel: channel, Version: nc.Spec.Flatcar.Version, NodeClass: nc.Name}
	if an, ok := d.(vendors.ArchNamer); ok {
		x.VendorArch = an.VendorArch
	}
	ref, err = Build(u, x, caps)
	if err != nil {
		return vendors.UseRef{}, false, fmt.Errorf("vendors.%s.resources.images.use for %s in %s: %w",
			vendor, arch, region, err)
	}
	return ref, true, nil
}

// Codes of the image errors every driver reports the same way.
const (
	CodeImageNotFound  = "ImageNotFound"
	CodeImageAmbiguous = "ImageAmbiguous"
)

// NarrowArchitecture folds the request's architecture into the selector: an
// images selector is narrowed to the architecture the launch asks for, so an `ids`
// list that holds both architectures is not ambiguous (the image's own
// architecture decides). ok is false when the selector's own
// match.architecture excludes it: no image of that architecture can match.
func NarrowArchitecture(ref vendors.UseRef, arch string) (narrowed vendors.UseRef, ok bool) {
	have := ref.Match.Architecture
	if len(have) > 0 && !slices.Contains(have, arch) {
		return ref, false
	}
	ref.Match.Architecture = []string{arch}
	return ref, true
}

// ImageError turns what the shared matcher says about an images selector into
// the driver's error classes: nothing matched is NotFound (the region's images
// are not ready), several matches under exactlyOne Fatal; other errors pass
// through.
func ImageError(err error, region, arch string) error {
	if _, ok := errors.AsType[*NoMatchError](err); ok {
		return vendors.NewError(vendors.NotFound, CodeImageNotFound,
			fmt.Errorf("no image for %s in %s: %w", arch, region, err))
	}
	if _, ok := errors.AsType[*AmbiguousError](err); ok {
		return vendors.NewError(vendors.Fatal, CodeImageAmbiguous,
			fmt.Errorf("the images selector is ambiguous for %s in %s: %w", arch, region, err))
	}
	return err
}
