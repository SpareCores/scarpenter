/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the fake repeats the SDK method signatures
package fake

import (
	"context"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"
)

// imageRec is an image of the fake with its region and the shapes it is
// compatible with (ListImageShapeCompatibilityEntries).
type imageRec struct {
	img    core.Image
	region string
	shapes []string
}

// AddImage adds an image to a region and returns its OCID when the image has no
// ID. compartment "" is the controller's compartment, "platform" an Oracle platform
// image that every compartment lists; shapes are its compatibility entries.
func (c *Cloud) AddImage(regionName string, img core.Image, shapes ...string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if img.Id == nil {
		img.Id = new(c.nextID("image", regionName))
	}
	if img.CompartmentId == nil {
		img.CompartmentId = &c.compartment
	}
	if img.LifecycleState == "" {
		img.LifecycleState = core.ImageLifecycleStateAvailable
	}
	if img.TimeCreated == nil {
		img.TimeCreated = c.stamp()
	}
	c.images[*img.Id] = &imageRec{img: img, region: regionName, shapes: shapes}
	return *img.Id
}

// SetImageState changes the lifecycle state of an image.
func (c *Cloud) SetImageState(id string, state core.ImageLifecycleStateEnum) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.images[id]; ok {
		r.img.LifecycleState = state
	}
}

// FlatcarImage is an image of the convention of plan 25, section 3: the name
// flatcar-<channel>-<version>-<arch> and the freeform tags.
func FlatcarImage(channel, version, arch string) core.Image {
	return core.Image{
		DisplayName: new("flatcar-" + channel + "-" + version + "-" + arch), OperatingSystem: new("Flatcar"),
		OperatingSystemVersion: new(version), SizeInMBs: new(int64(47719)),
		FreeformTags: map[string]string{"flatcar-channel": channel, "flatcar-version": version, "arch": arch, "os": "flatcar"},
	}
}

// PlatformImage is an Oracle platform image: the API lists it for every compartment.
func PlatformImage(name string) core.Image {
	return core.Image{DisplayName: &name, OperatingSystem: new("Oracle Linux"), OperatingSystemVersion: new("9"),
		CompartmentId: new("")}
}

func (c *Cloud) ListImages(ctx context.Context, regionName string, req core.ListImagesRequest) (core.ListImagesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListImages", regionName); err != nil {
		return core.ListImagesResponse{}, err
	}
	if err := c.inCompartment(req.CompartmentId); err != nil {
		return core.ListImagesResponse{}, err
	}
	var all []core.Image
	for _, r := range c.images {
		i := r.img
		platform := deref(i.CompartmentId) == ""
		switch {
		case r.region != regionName:
		case !platform && deref(i.CompartmentId) != deref(req.CompartmentId):
		case req.DisplayName != nil && deref(i.DisplayName) != *req.DisplayName:
		case req.OperatingSystem != nil && deref(i.OperatingSystem) != *req.OperatingSystem:
		case req.OperatingSystemVersion != nil && deref(i.OperatingSystemVersion) != *req.OperatingSystemVersion:
		case req.LifecycleState != "" && i.LifecycleState != req.LifecycleState:
		case req.Shape != nil && !slices.Contains(r.shapes, *req.Shape):
		default:
			all = append(all, i)
		}
	}
	slices.SortFunc(all, func(a, b core.Image) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListImagesResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) GetImage(ctx context.Context, regionName string, req core.GetImageRequest) (core.GetImageResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "GetImage", regionName); err != nil {
		return core.GetImageResponse{}, err
	}
	r, ok := c.images[deref(req.ImageId)]
	if !ok || r.region != regionName {
		return core.GetImageResponse{}, notFound("image " + deref(req.ImageId))
	}
	return core.GetImageResponse{Image: r.img}, nil
}

func (c *Cloud) ListImageShapeCompatibilityEntries(ctx context.Context, regionName string,
	req core.ListImageShapeCompatibilityEntriesRequest,
) (core.ListImageShapeCompatibilityEntriesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListImageShapeCompatibilityEntries", regionName); err != nil {
		return core.ListImageShapeCompatibilityEntriesResponse{}, err
	}
	r, ok := c.images[deref(req.ImageId)]
	if !ok || r.region != regionName {
		return core.ListImageShapeCompatibilityEntriesResponse{}, notFound("image " + deref(req.ImageId))
	}
	var all []core.ImageShapeCompatibilitySummary
	for _, s := range r.shapes {
		all = append(all, core.ImageShapeCompatibilitySummary{ImageId: r.img.Id, Shape: new(s)})
	}
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListImageShapeCompatibilityEntriesResponse{Items: page, OpcNextPage: next}, nil
}
