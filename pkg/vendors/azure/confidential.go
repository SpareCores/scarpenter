/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// codeConfidentialGPU is the code of the Unsupported error of a confidential size with GPUs.
const codeConfidentialGPU = "ConfidentialGPUNotSupported"

// codeConfidentialImageNotFound is the code of the Unsupported error of a
// region without the confidential VM image.
const codeConfidentialImageNotFound = "ConfidentialImageNotFound"

// confidentialImage returns the ID of the newest version of the confidential
// VM image (Azure Container Linux in a community gallery) in the region, cached
// like the other images. A region the image is not in is Unsupported.
func (d *Driver) confidentialImage(ctx context.Context, region string) (string, error) {
	key := "confidential|" + strings.ToLower(region)
	d.mu.Lock()
	if c, ok := d.imgCache[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return c.img.Ref, c.err
	}
	d.mu.Unlock()
	id, err := d.latestConfidentialImage(ctx, region)
	if err != nil && !vendors.IsClass(err, vendors.Unsupported) {
		return "", err
	}
	ttl := imageTTL
	if err != nil {
		ttl = imageMissTTL
	}
	d.mu.Lock()
	d.imgCache[key] = cachedResolution{img: vendors.Image{Ref: id}, err: err, until: d.clock.Now().Add(ttl)}
	d.mu.Unlock()
	return id, err
}

func (d *Driver) latestConfidentialImage(ctx context.Context, region string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	versions, err := d.clients.Images.CommunityVersions(cctx, region, ConfidentialGallery, ConfidentialImageDefinition)
	cancel()
	if err != nil && !isNotFound(err) {
		return "", classify(err, errContext{})
	}
	var latest *armcompute.CommunityGalleryImageVersion
	for _, v := range versions {
		if v == nil || v.Name == nil || v.Properties != nil && deref(v.Properties.ExcludeFromLatest) {
			continue
		}
		if latest == nil || published(v).After(published(latest)) {
			latest = v
		}
	}
	if latest == nil {
		return "", vendors.NewError(vendors.Unsupported, codeConfidentialImageNotFound, fmt.Errorf(
			"confidential VM sizes boot the community gallery image %s/%s, which has no version in %s",
			ConfidentialGallery, ConfidentialImageDefinition, region))
	}
	return fmt.Sprintf("/CommunityGalleries/%s/Images/%s/Versions/%s",
		ConfidentialGallery, ConfidentialImageDefinition, *latest.Name), nil
}

func published(v *armcompute.CommunityGalleryImageVersion) (t time.Time) {
	if v.Properties != nil && v.Properties.PublishedDate != nil {
		return *v.Properties.PublishedDate
	}
	return t
}
