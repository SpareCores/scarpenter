/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// DefaultImageOwner is the owner of the images when the NodeClass names none:
// the account of the credentials (rung 3 of ADR-021: the operator's own images,
// imported or copied into each region).
const DefaultImageOwner = "self"

// Cache lifetimes of the image resolution (overview section 9).
const (
	imageTTL = time.Hour
	// imageMissTTL is how long a definitive "no image" is not looked up again, so
	// that the NodeClass status does not ask at every reconcile for a region
	// without an image.
	imageMissTTL = 5 * time.Minute
)

type imageKey struct{ region, arch, channel, version, owner, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

// imageName is the name of the image of a Flatcar release by the convention of
// the images repository: Flatcar-<channel>-<version>-<arch> (letters, digits and
// .:_- are the characters ECS allows).
func imageName(channel, version, arch string) string {
	return fmt.Sprintf("Flatcar-%s-%s-%s", channel, version, arch)
}

// The tags with which an image says which Flatcar release it is when its name
// does not (a copy under another name, an image of the operator's own build).
const (
	tagFlatcarChannel = "flatcar-channel"
	tagFlatcarVersion = "flatcar-version"
)

var imageNamePattern = regexp.MustCompile(`^Flatcar-([a-z]+)-([0-9]+\.[0-9]+\.[0-9]+)-(?:amd64|arm64)$`)

func imageTags(im *ecs.DescribeImagesResponseBodyImagesImage) map[string]string {
	m := map[string]string{}
	if im.Tags != nil {
		for _, t := range im.Tags.Tag {
			m[deref(t.TagKey)] = deref(t.TagValue)
		}
	}
	return m
}

// imageRelease reads the Flatcar release an image says it is: from its name when
// that is the convention, else from the tags flatcar-channel and flatcar-version.
// Empty when it says neither (the version check then refuses it, ADR-017). An
// image whose name and tags name two different releases says nothing it can be
// trusted on: conflict is true and the image is refused.
func imageRelease(im *ecs.DescribeImagesResponseBodyImagesImage) (channel, version string, conflict bool) {
	tags := imageTags(im)
	tc, tv := tags[tagFlatcarChannel], tags[tagFlatcarVersion]
	if m := imageNamePattern.FindStringSubmatch(deref(im.ImageName)); m != nil {
		conflict = (tv != "" && tv != m[2]) || (tc != "" && !strings.EqualFold(tc, m[1]))
		return m[1], m[2], conflict
	}
	return tc, tv, false
}

// rootBytes is the size of the image's system disk: its system disk mapping,
// else the size of the image.
func rootBytes(im *ecs.DescribeImagesResponseBodyImagesImage) int64 {
	if im.DiskDeviceMappings != nil {
		for _, m := range im.DiskDeviceMappings.DiskDeviceMapping {
			if strings.EqualFold(deref(m.Type), "system") {
				if g, err := strconv.ParseInt(deref(m.Size), 10, 64); err == nil && g > 0 {
					return g * giB
				}
			}
		}
	}
	return int64(deref(im.Size)) * giB
}

// imageUse is the selector of a request: the NodeClass's, narrowed to the
// request's architecture; without one the built-in selector, the name of the
// pinned release in the owner's account in state available. owners is the list
// for the call: match.owner, else the owner of the block, except for a selector
// that names IDs (an ID is exact; the version check still applies), never "every
// image the account can see": a stranger's image of the same name must not win.
func imageUse(req vendors.ImageRequest, owner string) (use vendors.UseRef, owners []string, err error) {
	if req.Use.IsDefault() {
		use = vendors.UseRef{
			Name:   imageName(req.Channel, req.Version, req.Arch),
			Match:  common.ResourceMatch{Architecture: []string{req.Arch}, State: []string{stateAvailable}},
			Select: common.SelectExactlyOne,
		}
		return use, []string{owner}, nil
	}
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return use, nil, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
	}
	switch {
	case len(use.Match.Owner) > 0:
		owners = use.Match.Owner
	case len(use.IDs) == 0:
		owners = []string{owner}
	}
	if use.Select == common.SelectAll {
		return use, nil, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	return use, owners, nil
}

// ResolveImage implements vendors.Driver: DescribeImages with the selector (the
// NodeClass's images.use, narrowed to the architecture; without one the owner,
// the name of the release, state available and the architecture), finished by the
// shared matcher, and exactly one match under the default select. The chosen
// image must say it is the pinned release (ADR-017): by its name or its tags
// flatcar-channel and flatcar-version; another release is ImageVersionMismatch,
// none ImageVersionUnknown, and the image is not returned.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if _, ok := d.VendorArch(req.Arch); !ok {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
	}
	owner := req.Owner
	if owner == "" {
		owner = DefaultImageOwner
	}
	enc, _ := json.Marshal(req.Use)
	key := imageKey{req.Region, req.Arch, req.Channel, req.Version, owner, string(enc)}
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
	use, owners, err := imageUse(req, owner)
	if err != nil {
		return nil, err
	}
	images, err := d.describeImages(ctx, req.Region, use, owners)
	if err != nil {
		// A transient failure serves the last good image, even past its TTL.
		d.mu.Lock()
		stale, ok := d.images[key]
		d.mu.Unlock()
		if ok && vendors.IsClass(err, vendors.Retryable) {
			if stale.err != nil {
				return nil, stale.err
			}
			img := stale.image
			return &img, nil
		}
		return nil, err
	}
	remember := func(err error) error {
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	res, err := selector.Apply(use, d.Selectors()[KindImages], images, imageFields)
	d.noteMissing(KindImages, req.Region, res.Missing)
	if err != nil {
		if _, none := errors.AsType[*selector.NoMatchError](err); none && req.Use.IsDefault() {
			err = vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
				"no available image %q (%s) owned by %s in %s: %w", use.Name, req.Arch, owner, req.Region, err))
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
			"the image %s in %s is named Flatcar %s %s, but its tags %s and %s say otherwise; "+
				"an image of unknown version is never booted",
			deref(im.ImageId), req.Region, channel, version, tagFlatcarChannel, tagFlatcarVersion)))
	}
	if err := vendors.CheckImageVersion(deref(im.ImageId), channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: deref(im.ImageId), RootDiskBytes: rootBytes(im), Channel: channel, Version: version}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	if im.Features != nil && deref(im.Features.NvmeSupport) != "" {
		d.nvmeOf[req.Region+"/"+img.Ref] = deref(im.Features.NvmeSupport) == "supported"
	}
	d.mu.Unlock()
	return &img, nil
}

// checkImageFits refuses a launch ECS would: an NVMe-only instance type (the
// 8th generation and later) with an image that is known not to support NVMe
// (Features.NvmeSupport of the image, read when it was resolved). Without that
// knowledge the launch goes on and ECS's own answer, ImageNotSupportInstanceType,
// is Unsupported as well.
func (d *Driver) checkImageFits(req vendors.CreateRequest) error {
	ti, ok := d.cloud.Types.Get(req.InstanceType)
	if !ok || !ti.NVMe {
		return nil
	}
	d.mu.Lock()
	supported, known := d.nvmeOf[req.Region+"/"+req.Image.Ref]
	d.mu.Unlock()
	if known && !supported {
		return vendors.NewError(vendors.Unsupported, "ImageNotSupportInstanceType", fmt.Errorf(
			"%s needs NVMe and the image %s was imported without Features.NvmeSupport=supported",
			req.InstanceType, req.Image.Ref))
	}
	return nil
}

// noteMissing logs the IDs a `use` named that the vendor did not return: a
// missing ID does not fail the others, so it would otherwise go unnoticed.
func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

// reportParallel bounds the regions resolved at once.
const reportParallel = 8

// ReportImages implements vendors.ImageReporter: the image of the NodeClass's
// release in every region of its vendor block (every region of the account
// without a list) for both architectures. Resolutions are cached per region,
// architecture and release for an hour, so a report after the first costs no API
// calls. A region or architecture without an image is reported not ready with
// the reason; it does not fail the report.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	var owner string
	if nc.Spec.Vendors.Alicloud != nil {
		regions, owner = nc.Spec.Vendors.Alicloud.Regions, nc.Spec.Vendors.Alicloud.ImageOwner
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
		channel = "stable"
	}
	arches := []string{archAMD64, archARM64}
	var (
		mu        sync.Mutex
		out       []v1alpha1.ImageStatus
		wg        sync.WaitGroup
		sem       = make(chan struct{}, reportParallel)
		transient error
	)
	for _, region := range regions {
		for _, arch := range arches {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				st := v1alpha1.ImageStatus{Region: region, Arch: arch}
				use, _, err := selector.ImageUse(nc, d, region, arch)
				var img *vendors.Image
				if err == nil {
					img, err = d.ResolveImage(ctx, vendors.ImageRequest{
						Region: region, Arch: arch, Channel: channel, Version: nc.Spec.Flatcar.Version, Owner: owner, Use: use,
					})
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					st.Ref, st.Ready = img.Ref, true
				case !vendors.IsClass(err, vendors.Retryable):
					// A definitive answer: no image, an ambiguous name, a region the
					// account cannot use.
					st.Message = err.Error()
					if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) {
						st.Reason = code
					}
				default:
					// Throttling, a timeout: the answer is unknown, and an unknown must
					// not gate the region's offerings (the whole report fails and the
					// status keeps the last verdict).
					transient = errors.Join(transient, fmt.Errorf("%s/%s: %w", region, arch, err))
					return
				}
				out = append(out, st)
			}()
		}
	}
	wg.Wait()
	if transient != nil {
		return nil, transient
	}
	slices.SortFunc(out, func(a, b v1alpha1.ImageStatus) int {
		if a.Region != b.Region {
			return compare(a.Region, b.Region)
		}
		return compare(a.Arch, b.Arch)
	})
	return out, nil
}
