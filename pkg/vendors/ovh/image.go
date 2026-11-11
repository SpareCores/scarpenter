/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// CodeImageArch is the code of a request for an architecture the driver cannot
// name.
const CodeImageArch = "UnsupportedArchitecture"

type imageKey struct{ region, arch, channel, version, owner, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

// ImageName is the name the operator's image step gives the Flatcar image of a
// release (plan 18, "Pinned Flatcar version to image ID").
func ImageName(channel, version, arch string) string {
	return fmt.Sprintf("flatcar-%s-%s-%s", channel, version, arch)
}

// Release is read from, in this order, the image's name, its properties
// flatcar_channel and flatcar_version and its tag flatcar-<channel>-<version>.
const (
	propFlatcarChannel = "flatcar_channel"
	propFlatcarVersion = "flatcar_version"
)

var (
	imageNamePattern = regexp.MustCompile(`^flatcar-([a-z]+)-([0-9]+\.[0-9]+\.[0-9]+)-(?:amd64|arm64)$`)
	imageTagPattern  = regexp.MustCompile(`^flatcar-([a-z]+)-([0-9]+\.[0-9]+\.[0-9]+)$`)
)

// imageRelease reads the Flatcar release an image says it is. conflict is true
// when two of the sources say different releases: such an image is never booted.
func imageRelease(im osapi.Image) (channel, version string, conflict bool) {
	type rel struct{ c, v string }
	var found []rel
	if m := imageNamePattern.FindStringSubmatch(im.Name); m != nil {
		found = append(found, rel{m[1], m[2]})
	}
	if v := im.Properties[propFlatcarVersion]; v != "" {
		found = append(found, rel{strings.ToLower(im.Properties[propFlatcarChannel]), v})
	}
	for _, t := range im.Tags {
		if m := imageTagPattern.FindStringSubmatch(t); m != nil {
			found = append(found, rel{m[1], m[2]})
		}
	}
	if len(found) == 0 {
		return "", "", false
	}
	first := found[0]
	for _, f := range found[1:] {
		if f.v != first.v || (f.c != "" && first.c != "" && f.c != first.c) {
			conflict = true
		}
		if first.c == "" {
			first.c = f.c
		}
	}
	return first.c, first.v, conflict
}

// imageUse is the selector of the request: the pinned release by its convention
// name when the NodeClass gives none, else the NodeClass's own, narrowed to the
// architecture of the request.
func imageUse(req vendors.ImageRequest) (vendors.UseRef, error) {
	if req.Use.IsDefault() {
		return vendors.UseRef{
			Name:   ImageName(req.Channel, req.Version, req.Arch),
			Match:  common.ResourceMatch{Architecture: []string{req.Arch}},
			Select: common.SelectExactlyOne,
		}, nil
	}
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return use, vendors.NewError(vendors.NotFound, selector.CodeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
	}
	if use.Select == common.SelectAll {
		return use, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	return use, nil
}

// glanceFilter is what Glance can filter on of the selector: the exact name, the
// k=v tags, the visibility, the owner and the usual status. Everything else is
// the client's (selector.Apply and the property filters).
func glanceFilter(use vendors.UseRef, owner string) (osapi.ImageFilter, []propFilter) {
	f := osapi.ImageFilter{}
	if use.Name != "" && !selector.HasWildcard(use.Name) && !strings.Contains(use.Name, `\`) {
		f.Name = use.Name
	}
	for _, k := range slices.Sorted(maps.Keys(use.Tags)) {
		if use.Tags[k] != "*" {
			f.Tags = append(f.Tags, k+"="+use.Tags[k])
		}
	}
	var props []propFilter
	for _, fl := range use.Filters {
		switch {
		case fl.Name == filterVisibility && len(fl.Values) == 1:
			f.Visibility = fl.Values[0]
		case strings.HasPrefix(fl.Name, propertyPrefix):
			props = append(props, propFilter{key: strings.TrimPrefix(fl.Name, propertyPrefix), values: fl.Values})
		}
	}
	if len(use.Match.State) == 0 {
		f.Status = osapi.ImageActive
	}
	switch {
	case len(use.Match.Owner) == 1 && use.Match.Owner[0] != "self":
		f.Owner = use.Match.Owner[0]
	case owner != "" && len(use.Match.Owner) == 0:
		f.Owner = owner
	}
	if f.Owner == "" && f.Visibility == "" && len(use.IDs) == 0 {
		// The project's own images: a private image of the project, never a public
		// one of another (shared and community images need an owner or a filter).
		f.Visibility = osapi.VisibilityPriv
	}
	return f, props
}

// propFilter is a `property:<key>` filter of the selector: the image property
// must be one of the values.
type propFilter struct {
	key    string
	values []string
}

func propsMatch(im osapi.Image, props []propFilter) bool {
	for _, p := range props {
		if !slices.Contains(p.values, im.Properties[p.key]) {
			return false
		}
	}
	return true
}

// visible re-applies the visibility filter to the response.
func visible(im osapi.Image, use vendors.UseRef) bool {
	for _, f := range use.Filters {
		if f.Name == filterVisibility && !slices.Contains(f.Values, im.Visibility) {
			return false
		}
	}
	return true
}

// ResolveImage implements vendors.Driver: the Glance image of the request in the
// region, found by the NodeClass's `images` selector or by the naming convention
// (flatcar-<channel>-<version>-<arch>), and only when its name, properties or
// tags say it is the pinned release (ADR-017: an image of another or unknown
// release is never booted).
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if _, ok := archProperty[req.Arch]; !ok {
		return nil, vendors.NewError(vendors.Unsupported, CodeImageArch,
			fmt.Errorf("unsupported architecture %q (amd64 and arm64 are)", req.Arch))
	}
	enc, _ := json.Marshal(req.Use)
	key := imageKey{req.Region, req.Arch, req.Channel, req.Version, req.Owner, string(enc)}
	d.mu.Lock()
	if c, ok := d.images[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		if c.err != nil {
			return nil, c.err
		}
		img := c.image
		return &img, nil
	}
	d.mu.Unlock()
	use, err := imageUse(req)
	if err != nil {
		return nil, err
	}
	cl, err := d.clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	filter, props := glanceFilter(use, req.Owner)
	listed, err := cl.Images.ListImages(ctx, filter)
	if err != nil {
		cerr := Classify(err)
		d.mu.Lock()
		stale, ok := d.images[key]
		d.mu.Unlock()
		if ok && vendors.IsClass(cerr, vendors.Retryable) && stale.err == nil {
			img := stale.image
			return &img, nil
		}
		return nil, cerr
	}
	remember := func(err error) error {
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	listed = slices.DeleteFunc(listed, func(im osapi.Image) bool { return !propsMatch(im, props) || !visible(im, use) })
	// Owner "self" and the owner of the settings are server-side filters; the
	// selector must not compare them again.
	use.Match.Owner = nil
	res, err := selector.Apply(use, d.Selectors()[KindImages], listed, imageFields)
	d.noteMissing(KindImages, req.Region, res.Missing)
	if err != nil {
		if _, none := errors.AsType[*selector.NoMatchError](err); none && req.Use.IsDefault() {
			err = vendors.NewError(vendors.NotFound, selector.CodeImageNotFound, fmt.Errorf(
				"no active image named %q (%s) in %s: %w; upload the Flatcar image with the naming convention of "+
					"docs/how-to-guides/ovh-images.md or name it in vendors.ovh.resources.images",
				use.Name, req.Arch, req.Region, err))
		} else {
			err = selector.ImageError(err, req.Region, req.Arch)
		}
		if vendors.ClassOf(err) == vendors.NotFound || vendors.ClassOf(err) == vendors.Fatal {
			return nil, remember(err)
		}
		return nil, err
	}
	im := res.Items[0]
	channel, version, conflict := imageRelease(im)
	if conflict {
		return nil, remember(vendors.NewError(vendors.Fatal, vendors.CodeImageVersionUnknown, fmt.Errorf(
			"the image %s in %s says different Flatcar releases in its name, properties %s and %s and tags; "+
				"an image of unknown version is never booted", im.ID, req.Region, propFlatcarChannel, propFlatcarVersion)))
	}
	if err := vendors.CheckImageVersion(im.ID, channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: im.ID, Channel: channel, Version: version}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return &img, nil
}

func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

const reportParallel = 8

// ReportImages implements vendors.ImageReporter: the images of the NodeClass in
// every region it can use, for amd64 (v1 launches no arm64 type: Keeper has none
// for OVH).
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	var owner string
	if nc.Spec.Vendors.OVH != nil {
		regions, owner = nc.Spec.Vendors.OVH.Regions, nc.Spec.Vendors.OVH.ImageOwner
	}
	if len(regions) == 0 {
		all, err := d.Regions(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			regions = append(regions, r.Name)
		}
	}
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = selector.DefaultChannel
	}
	var (
		mu        sync.Mutex
		out       []v1alpha1.ImageStatus
		wg        sync.WaitGroup
		sem       = make(chan struct{}, reportParallel)
		transient error
	)
	for _, region := range regions {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			st := v1alpha1.ImageStatus{Region: region, Arch: archAMD64}
			use, _, err := selector.ImageUse(nc, d, region, archAMD64)
			var img *vendors.Image
			if err == nil {
				img, err = d.ResolveImage(ctx, vendors.ImageRequest{
					Region: region, Arch: archAMD64, Channel: channel, Version: nc.Spec.Flatcar.Version, Owner: owner, Use: use,
				})
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				st.Ref, st.Ready = img.Ref, true
			case !vendors.IsClass(err, vendors.Retryable):
				st.Message = err.Error()
				if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) {
					st.Reason = code
				}
			default:
				transient = errors.Join(transient, fmt.Errorf("%s/%s: %w", region, archAMD64, err))
				return
			}
			out = append(out, st)
		}()
	}
	wg.Wait()
	if transient != nil {
		return nil, transient
	}
	slices.SortFunc(out, func(a, b v1alpha1.ImageStatus) int { return strings.Compare(a.Region, b.Region) })
	return out, nil
}
