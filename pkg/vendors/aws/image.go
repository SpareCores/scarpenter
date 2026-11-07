/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// imageTTL is how long a resolved image is trusted (overview section 9).
const imageTTL = time.Hour

// Architectures of the images, as the catalog and EC2 spell them.
// archARM64 is the architecture name of the catalog and of the image names.
const archARM64 = "arm64"

var imageArches = map[string]ec2types.ArchitectureValues{
	"amd64":   ec2types.ArchitectureValuesX8664,
	archARM64: ec2types.ArchitectureValuesArm64,
}

type imageKey struct{ region, arch, channel, version, owner string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	// err is a NotFound answer, remembered for imageMissTTL so that the NodeClass
	// status does not ask again at every reconcile for a region without an image.
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

// ResolveImage implements vendors.Driver: DescribeImages with the owner, the
// name of the release, state available and the architecture. Exactly one match
// is required. The root device name comes from the image (cached for the
// launch).
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	arch, ok := imageArches[req.Arch]
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
	}
	owner := req.Owner
	if owner == "" {
		owner = DefaultImageOwner
	}
	key := imageKey{req.Region, req.Arch, req.Channel, req.Version, owner}
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
	cl, err := d.clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	name := imageName(req.Channel, req.Version, req.Arch)
	out, err := cl.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{owner},
		Filters: []ec2types.Filter{
			{Name: aws.String("name"), Values: []string{name}},
			{Name: aws.String("state"), Values: []string{string(ec2types.ImageStateAvailable)}},
			{Name: aws.String("architecture"), Values: []string{string(arch)}},
		},
	})
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
	switch len(out.Images) {
	case 0:
		err := vendors.NewError(vendors.NotFound, codeImageNotFound,
			fmt.Errorf("no available image %q (%s) owned by %s in %s", name, req.Arch, owner, req.Region))
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return nil, err
	case 1:
	default:
		return nil, vendors.NewError(vendors.Fatal, codeImageAmbiguous,
			fmt.Errorf("%d available images named %q (%s) are owned by %s in %s; exactly one is required",
				len(out.Images), name, req.Arch, owner, req.Region))
	}
	im := out.Images[0]
	img := vendors.Image{Ref: deref(im.ImageId)}
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
				img, err := d.ResolveImage(ctx, vendors.ImageRequest{
					Region: region, Arch: arch, Channel: channel, Version: nc.Spec.Flatcar.Version, Owner: owner,
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					st.Ref, st.Ready = img.Ref, true
				case !vendors.IsClass(err, vendors.Retryable):
					// A definitive answer: no image, an ambiguous name, a region the
					// account cannot use.
					st.Message = err.Error()
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
