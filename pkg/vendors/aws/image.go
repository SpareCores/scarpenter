/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// imageTTL is how long a resolved image is trusted (overview section 9).
const imageTTL = time.Hour

// Architectures of the images, as the catalog and EC2 spell them.
// archARM64 is the architecture name of the catalog and of the image names.
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

var imageArches = map[string]ec2types.ArchitectureValues{
	archAMD64: ec2types.ArchitectureValuesX8664,
	archARM64: ec2types.ArchitectureValuesArm64,
}

type imageKey struct{ region, arch, channel, version, owner, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	// err is a definitive answer that is not an image (none, several, another
	// release), remembered for imageMissTTL so that the NodeClass status does not
	// ask again at every reconcile for a region without an image.
	err error
}

// imageMissTTL is how long a missing image is not looked up again.
const imageMissTTL = 5 * time.Minute

// imageName is the name of the official Flatcar AMI of the release: the channel
// and version, "-hvm" for amd64 and "-arm64-hvm" for arm64 (overview section
// 11.3 and section 15 Q5; the LTS channel is named like the others, with lts as
// the channel). The names are asserted in image_test.go against the public
// release lists of both architectures.
func imageName(channel, version, arch string) string {
	if arch == archARM64 {
		return fmt.Sprintf("Flatcar-%s-%s-arm64-hvm", channel, version)
	}
	return fmt.Sprintf("Flatcar-%s-%s-hvm", channel, version)
}

// The tags with which an AMI says which Flatcar release it is when its name does
// not (a copy under another name, an image of the operator's own build).
const (
	tagFlatcarChannel = "flatcar-channel"
	tagFlatcarVersion = "flatcar-version"
)

var imageNamePattern = regexp.MustCompile(`^Flatcar-([a-z]+)-([0-9]+\.[0-9]+\.[0-9]+)(?:-arm64)?-hvm$`)

// imageRelease reads the Flatcar release an AMI says it is: from its name when
// that is the official naming, else from the tags flatcar-channel and
// flatcar-version. Empty when it says neither (the version check then refuses
// it, ADR-017).
func imageRelease(im ec2types.Image) (channel, version string) {
	if m := imageNamePattern.FindStringSubmatch(deref(im.Name)); m != nil {
		return m[1], m[2]
	}
	tags := tagMap(im.Tags)
	return tags[tagFlatcarChannel], tags[tagFlatcarVersion]
}

// imageUse is the selector of a request: the NodeClass's, narrowed to the
// request's architecture; without one the built-in selector, the name of the
// pinned release in the owner's account in state available. Owners is the list
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

// describeImages lists the AMIs of a selector: every field EC2 can evaluate goes
// into the call, the request parameters among the filters are lifted out.
func describeImages(
	ctx context.Context, cl Clients, use vendors.UseRef, owners []string,
) ([]ec2types.Image, error) {
	all, none, err := useFilters(use, imageFilters)
	if err != nil {
		return nil, vendors.Errorf(vendors.Fatal, "%w", err)
	}
	if none {
		return nil, nil // native filters that cannot all hold: nothing matches
	}
	in := &ec2.DescribeImagesInput{Owners: owners}
	var filters []ec2types.Filter
	for _, f := range all {
		switch deref(f.Name) {
		case paramExecutableUser:
			in.ExecutableUsers = append(in.ExecutableUsers, f.Values...)
		case paramIncludeDisabled:
			in.IncludeDisabled = aws.Bool(slices.Contains(f.Values, "true"))
		default:
			filters = append(filters, f)
		}
	}
	in.Filters = filters
	if use.Match.IncludeDeprecated != nil {
		in.IncludeDeprecated = use.Match.IncludeDeprecated
	}
	var out []ec2types.Image
	for {
		res, err := cl.EC2.DescribeImages(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Images...)
		if res.NextToken == nil || *res.NextToken == "" {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// ResolveImage implements vendors.Driver: DescribeImages with the selector (the
// NodeClass's images.use, narrowed to the architecture; without one the owner,
// the name of the release, state available and the architecture), finished by the
// shared matcher, and exactly one match under the default select. The chosen
// image must say it is the pinned release (ADR-017): by its name or its tags
// flatcar-channel and flatcar-version; another release is ImageVersionMismatch,
// none ImageVersionUnknown, and the image is not returned. The root device name
// comes from the image (cached for the launch).
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if _, ok := imageArches[req.Arch]; !ok {
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
	cl, err := d.clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	images, err := describeImages(ctx, cl, use, owners)
	if err != nil {
		// A transient failure serves the last good image, even past its TTL: the
		// answer of an earlier success is better than none.
		d.mu.Lock()
		stale, ok := d.images[key]
		d.mu.Unlock()
		if ok && vendors.IsClass(classify(err, errContext{}), vendors.Retryable) {
			if stale.err != nil {
				return nil, stale.err // the last answer was "no image": it still is the last answer
			}
			img := stale.image
			return &img, nil
		}
		return nil, classify(err, errContext{})
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
	channel, version := imageRelease(im)
	if err := vendors.CheckImageVersion(deref(im.ImageId), channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: deref(im.ImageId), Channel: channel, Version: version}
	for _, bd := range im.BlockDeviceMappings {
		if bd.DeviceName != nil && im.RootDeviceName != nil && *bd.DeviceName == *im.RootDeviceName && bd.Ebs != nil &&
			bd.Ebs.VolumeSize != nil {
			img.RootDiskBytes = int64(*bd.Ebs.VolumeSize) * giB
		}
	}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	if im.RootDeviceName != nil {
		d.roots[req.Region+"/"+img.Ref] = *im.RootDeviceName
	}
	d.mu.Unlock()
	return &img, nil
}

// reportParallel bounds the regions resolved at once.
const reportParallel = 8

// ReportImages implements vendors.ImageReporter: the image of the NodeClass's
// release in every region of its vendor block (every region of the account
// without a list) for both architectures. Resolutions are cached per region,
// architecture and release for an hour, so a report after the first costs no
// API calls. A region or architecture without an image is reported not ready
// with the reason; it does not fail the report.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	var owner string
	if nc.Spec.Vendors.AWS != nil {
		regions, owner = nc.Spec.Vendors.AWS.Regions, nc.Spec.Vendors.AWS.ImageOwner
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
	arches := []string{"amd64", archARM64}
	var (
		mu  sync.Mutex
		out []v1alpha1.ImageStatus
		wg  sync.WaitGroup
		sem = make(chan struct{}, reportParallel)

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
