/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The image convention of plan 25, section 3: the operator's IaC imports Flatcar's
// oraclecloud image per region and architecture under this name and these freeform
// tags.
const (
	TagOS             = "os"
	TagFlatcarChannel = "flatcar-channel"
	TagFlatcarVersion = "flatcar-version"
	TagArch           = "arch"
	// OSFlatcar is the operating system of the imported image and the value of the os tag.
	OSFlatcar = "Flatcar"

	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	imageTTL     = time.Hour
	imageMissTTL = 5 * time.Minute
)

// ImageName is the display name of the image of a release and architecture.
func ImageName(channel, version, arch string) string {
	return "flatcar-" + channel + "-" + version + "-" + arch
}

type imageKey struct{ region, arch, channel, version, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

type cachedArch struct {
	arch  string
	until time.Time
}

// ResolveImage implements vendors.Driver (rung 3 of ADR-021): the image of the pinned
// release in the controller's compartment, found by its name and freeform tags, or by
// the NodeClass's images selector. An image whose architecture is not known never
// satisfies a request, and whatever is found must say it is the pinned release.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if req.Arch != ArchAMD64 && req.Arch != ArchARM64 {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
	}
	if ok, err := d.subscribed(ctx, req.Region); err != nil {
		return nil, err
	} else if !ok {
		return nil, vendors.NewError(vendors.Unsupported, codeUnknownReg,
			fmt.Errorf("the region %q is not subscribed by the tenancy", req.Region))
	}
	enc, _ := json.Marshal(req.Use)
	key := imageKey{req.Region, req.Arch, req.Channel, req.Version, string(enc)}
	d.mu.Lock()
	c, hit := d.images[key]
	d.mu.Unlock()
	if hit && d.clock.Now().Before(c.until) {
		if c.err != nil {
			return nil, c.err
		}
		img := c.image
		return &img, nil
	}
	remember := func(err error) error {
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	im, err := d.findImage(ctx, req)
	if err != nil {
		if hit && vendors.IsClass(err, vendors.Retryable) {
			// A transient failure serves the last answer.
			if c.err != nil {
				return nil, c.err
			}
			img := c.image
			return &img, nil
		}
		if vendors.IsClass(err, vendors.NotFound) || vendors.IsClass(err, vendors.Fatal) {
			return nil, remember(err)
		}
		return nil, err
	}
	channel, version := imageRelease(im)
	if err := vendors.CheckImageVersion(deref(im.Id), channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: deref(im.Id), Channel: channel, Version: version,
		RootDiskBytes: deref(im.SizeInMBs) << 20}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return &img, nil
}

// imageRelease reads the release an image says it is: the version from the tag
// flatcar-version, else the operating system version; the channel from the tag
// flatcar-channel, else none (compared only when present).
func imageRelease(im core.Image) (channel, version string) {
	version = im.FreeformTags[TagFlatcarVersion]
	if version == "" {
		version = deref(im.OperatingSystemVersion)
	}
	return im.FreeformTags[TagFlatcarChannel], version
}

// imageArch is the architecture of an image: the tag arch, else derived from the
// shapes it is compatible with (any shape of the Arm series means arm64, else
// amd64), else unknown ("").
func (d *Driver) imageArch(ctx context.Context, region string, im core.Image) (string, error) {
	switch a := im.FreeformTags[TagArch]; a {
	case ArchAMD64, ArchARM64:
		return a, nil
	}
	id := deref(im.Id)
	d.mu.Lock()
	c, ok := d.archs[id]
	d.mu.Unlock()
	if ok && d.clock.Now().Before(c.until) {
		return c.arch, nil
	}
	arch := ""
	req := core.ListImageShapeCompatibilityEntriesRequest{ImageId: im.Id}
	for {
		res, err := d.api.Compute.ListImageShapeCompatibilityEntries(ctx, region, req)
		if err != nil {
			return "", classify(err, kindRead)
		}
		for _, e := range res.Items {
			shape := deref(e.Shape)
			if Arm(shape) {
				arch = ArchARM64
			} else if arch == "" && (strings.HasPrefix(shape, "VM.") || strings.HasPrefix(shape, "BM.")) {
				arch = ArchAMD64
			}
		}
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			break
		}
		req.Page = res.OpcNextPage
	}
	d.mu.Lock()
	d.archs[id] = cachedArch{arch: arch, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return arch, nil
}

// findImage is the lookup: the convention without a selector, the selector with.
func (d *Driver) findImage(ctx context.Context, req vendors.ImageRequest) (core.Image, error) {
	use := req.Use
	if use.IsDefault() {
		use = vendors.UseRef{Name: ImageName(req.Channel, req.Version, req.Arch),
			Filters: []common.ResourceFilter{{Name: filterOS, Values: []string{OSFlatcar}}}}
	} else {
		var ok bool
		if use, ok = selector.NarrowArchitecture(req.Use, req.Arch); !ok {
			return core.Image{}, vendors.NewError(vendors.NotFound, selector.CodeImageNotFound, fmt.Errorf(
				"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
		}
	}
	if use.Select == common.SelectAll {
		return core.Image{}, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	images, err := d.listImages(ctx, req.Region, use)
	if err != nil {
		return core.Image{}, err
	}
	// An image of another or unknown architecture never satisfies the request.
	var fit []core.Image
	for _, im := range images {
		a, err := d.imageArch(ctx, req.Region, im)
		if err != nil {
			return core.Image{}, err
		}
		if a == req.Arch {
			fit = append(fit, im)
		}
	}
	if req.Use.IsDefault() {
		switch len(fit) {
		case 0:
			return core.Image{}, selector.ImageError(&selector.NoMatchError{
				Stages: []selector.Stage{{Name: "listed", Count: len(images)},
					{Name: "name", Detail: fmt.Sprintf("%q", use.Name)}}},
				req.Region, req.Arch)
		case 1:
			return fit[0], nil
		}
		return core.Image{}, selector.ImageError(&selector.AmbiguousError{
			Stages: []selector.Stage{{Name: "listed", Count: len(fit)}}}, req.Region, req.Arch)
	}
	res, err := selector.Apply(use, d.Selectors()[KindImages], fit, imageFields)
	d.noteMissing(KindImages, req.Region, res.Missing)
	if err != nil {
		return core.Image{}, selector.ImageError(err, req.Region, req.Arch)
	}
	return res.Items[0], nil
}

func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

// The native filters of the images kind: the parameters of ListImages.
const (
	filterOS        = "operating-system"
	filterOSVersion = "operating-system-version"
	filterShape     = "shape"
	filterState     = "lifecycle-state"
)

// listImages reads the candidates of a use block: the IDs (a get each: the image may live in
// a compartment other than the controller's), or the compartment's images narrowed on the
// server by an exact name, the native filters and the state (available unless match.state
// says otherwise); the matcher re-applies everything.
func (d *Driver) listImages(ctx context.Context, region string, use vendors.UseRef) ([]core.Image, error) {
	if len(use.IDs) > 0 {
		var out []core.Image
		for _, id := range use.IDs {
			res, err := d.api.Compute.GetImage(ctx, region, core.GetImageRequest{ImageId: &id})
			if err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, classify(err, kindRead)
			}
			out = append(out, res.Image)
		}
		return out, nil
	}
	req := core.ListImagesRequest{CompartmentId: &d.compartment}
	if use.Name != "" && !selector.HasWildcard(use.Name) {
		req.DisplayName = &use.Name
	}
	req.LifecycleState = core.ImageLifecycleStateAvailable
	if len(use.Match.State) > 0 {
		req.LifecycleState = ""
	}
	for _, f := range use.Filters {
		if len(f.Values) != 1 {
			return nil, vendors.NewError(vendors.Fatal, codeSettings, fmt.Errorf(
				"the images filter %s takes one value, not %q", f.Name, f.Values))
		}
		v := f.Values[0]
		switch f.Name {
		case filterOS:
			req.OperatingSystem = &v
		case filterOSVersion:
			req.OperatingSystemVersion = &v
		case filterShape:
			req.Shape = &v
		case filterState:
			req.LifecycleState = core.ImageLifecycleStateEnum(strings.ToUpper(v))
		}
	}
	var out []core.Image
	for {
		res, err := d.api.Compute.ListImages(ctx, region, req)
		if err != nil {
			return nil, classify(err, kindRead)
		}
		out = append(out, res.Items...)
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			return out, nil
		}
		req.Page = res.OpcNextPage
	}
}

// imageState normalises the lifecycle state of an image (available, pending, failed).
func imageState(s core.ImageLifecycleStateEnum) string {
	switch s {
	case core.ImageLifecycleStateAvailable:
		return "available"
	case core.ImageLifecycleStateProvisioning, core.ImageLifecycleStateImporting, core.ImageLifecycleStateExporting:
		return "pending"
	}
	return "failed"
}

func imageFields(im core.Image) selector.Fields {
	f := selector.Fields{
		ID: deref(im.Id), Name: deref(im.DisplayName), Tags: im.FreeformTags,
		Attrs: map[string][]string{vendors.MatchState: {imageState(im.LifecycleState)}},
	}
	if im.TimeCreated != nil {
		f.Created = im.TimeCreated.Time
	}
	return f
}

// ReportImages implements vendors.ImageReporter: one image per region and architecture, for
// the regions of the NodeClass (or every subscribed one). A named architecture without an
// image is not ready.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	if nc.Spec.Vendors.OCI != nil {
		regions = nc.Spec.Vendors.OCI.Regions
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
		transient error
		wg        sync.WaitGroup
	)
	for _, region := range regions {
		for _, arch := range []string{ArchAMD64, ArchARM64} {
			wg.Go(func() {
				st := v1alpha1.ImageStatus{Region: region, Arch: arch}
				use, _, err := selector.ImageUse(nc, d, region, arch)
				var img *vendors.Image
				if err == nil {
					img, err = d.ResolveImage(ctx, vendors.ImageRequest{
						Region: region, Arch: arch, Channel: channel, Version: nc.Spec.Flatcar.Version, Use: use,
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
					transient = errors.Join(transient, fmt.Errorf("%s/%s: %w", region, arch, err))
					return
				}
				out = append(out, st)
			})
		}
	}
	wg.Wait()
	if transient != nil {
		return nil, transient
	}
	slices.SortFunc(out, func(a, b v1alpha1.ImageStatus) int {
		if a.Region != b.Region {
			return strings.Compare(a.Region, b.Region)
		}
		return strings.Compare(a.Arch, b.Arch)
	})
	return out, nil
}

var _ vendors.ImageReporter = (*Driver)(nil)
