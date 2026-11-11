/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

const (
	// imageTTL is how long a resolved image is trusted (overview section 9).
	imageTTL = time.Hour
	// imageMissTTL is how long a definitive "no image" (none, ambiguous, another
	// release, terms not accepted) is not looked up again.
	imageMissTTL = 5 * time.Minute
	// maxVersionCandidates bounds the versions an images name pattern may match:
	// each is read with its own call.
	maxVersionCandidates = 100
)

type cachedResolution struct {
	img   vendors.Image
	err   error
	until time.Time
}

// ResolveImage implements vendors.Driver. Without an images selector the pinned
// release is found in the official marketplace offers (rung 1 of ADR-021: the
// offer of the architecture, the SKU of the channel and generation, the version
// itself); with one, the NodeClass's URNs or version pattern are read the same
// way. Whatever the lookup, the image's version must be the pinned one
// (ADR-017), and an image with a purchase plan needs the marketplace terms
// accepted in the subscription: SCarpenter never accepts them, the error names
// the command.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if req.Arch != archAMD64 && req.Arch != archARM64 {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
	}
	enc, _ := json.Marshal(req.Use)
	key := strings.Join([]string{req.Region, req.Arch, req.Channel, req.Version, req.Owner, string(enc)}, "|")
	d.mu.Lock()
	if c, ok := d.imgCache[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		if c.err != nil {
			return nil, c.err
		}
		img := c.img
		return &img, nil
	}
	d.mu.Unlock()
	remember := func(err error) error {
		d.mu.Lock()
		d.imgCache[key] = cachedResolution{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	img, err := d.resolveImage(ctx, req)
	if err != nil {
		if c := vendors.ClassOf(err); c == vendors.NotFound || c == vendors.Fatal {
			return nil, remember(err)
		}
		return nil, err
	}
	d.mu.Lock()
	d.imgCache[key] = cachedResolution{img: *img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return img, nil
}

func (d *Driver) resolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	var res *Resolved
	var err error
	if req.Use.IsDefault() {
		res, err = d.images.Resolve(ctx, req)
	} else {
		res, err = d.resolveSelected(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	channel := ChannelOfSKU(res.Image.SKU)
	if err := vendors.CheckImageVersion(res.Image.URN(), channel, res.Image.Version, req); err != nil {
		return nil, err
	}
	if err := d.images.CheckTerms(ctx, res); err != nil {
		return nil, err
	}
	return &vendors.Image{Ref: res.Image.URN(), Channel: channel, Version: res.Image.Version}, nil
}

// imageCandidate is a marketplace image the selector lists.
type imageCandidate struct {
	res *Resolved
	img armcompute.VirtualMachineImage
}

func imageFields(c imageCandidate) selector.Fields {
	urn := c.res.Image.URN()
	f := selector.Fields{ID: urn, Name: urn, Attrs: map[string][]string{vendors.MatchState: {"available"}}}
	if a := ArchOfImage(c.img); a != "" {
		f.Attrs[vendors.MatchArchitecture] = []string{a}
	}
	return f
}

// resolveSelected reads the images the NodeClass's `use` names: exact URNs
// (ids), or the versions of one SKU that match a glob (name,
// publisher:offer:sku:<version glob>), then finishes with the shared matcher.
func (d *Driver) resolveSelected(ctx context.Context, req vendors.ImageRequest) (*Resolved, error) {
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return nil, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
	}
	if use.Select == common.SelectAll {
		return nil, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	if len(use.Tags) > 0 || len(use.Filters) > 0 || use.Expression != "" {
		return nil, vendors.Errorf(vendors.Fatal,
			"the marketplace has no tags, filters or expressions: select images by ids or name")
	}
	var cands []imageCandidate
	var err error
	switch {
	case len(use.IDs) > 0:
		cands, err = d.imagesByURN(ctx, req.Region, use.IDs)
	case use.Name != "":
		cands, err = d.imagesByPattern(ctx, req.Region, use.Name)
	default:
		return nil, vendors.Errorf(vendors.Fatal,
			"images.use needs ids (URNs) or a name (publisher:offer:sku:<version glob>)")
	}
	if err != nil {
		return nil, err
	}
	res, err := selector.Apply(use, d.Selectors()[selector.ImagesKind], cands, imageFields)
	if len(res.Missing) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", "images", "region", req.Region,
			"ids", res.Missing)
	}
	if err != nil {
		return nil, selector.ImageError(err, req.Region, req.Arch)
	}
	return res.Items[0].res, nil
}

// imagesByURN reads the images of the URNs; one that does not exist (or is not a
// URN) is left out, which the matcher reports as a missing ID.
func (d *Driver) imagesByURN(ctx context.Context, region string, ids []string) ([]imageCandidate, error) {
	var out []imageCandidate
	for _, id := range ids {
		ref, err := ParseURN(id)
		if err != nil {
			continue
		}
		res, img, err := d.images.Lookup(ctx, region, ref)
		if err != nil {
			if vendors.IsClass(err, vendors.NotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, imageCandidate{res: res, img: img})
	}
	return out, nil
}

// imagesByPattern lists the versions of the SKU the pattern names and reads the
// ones whose version matches the glob.
func (d *Driver) imagesByPattern(ctx context.Context, region, pattern string) ([]imageCandidate, error) {
	parts := strings.SplitN(pattern, ":", 4)
	if len(parts) != 4 || slices.ContainsFunc(parts[:3], inexactPart) {
		return nil, vendors.Errorf(vendors.Fatal,
			"the images name %q is not publisher:offer:sku:<version glob> (the first three parts are exact)", pattern)
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	versions, err := d.clients.Images.ListVersions(cctx, region, parts[0], parts[1], parts[2])
	cancel()
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, classify(err, errContext{})
	}
	var matched []string
	for _, v := range versions {
		if selector.Glob(parts[3], v) {
			matched = append(matched, v)
		}
	}
	if len(matched) > maxVersionCandidates {
		return nil, vendors.Errorf(vendors.Fatal,
			"the images name %q matches %d versions (at most %d): narrow the version glob", pattern, len(matched),
			maxVersionCandidates)
	}
	var out []imageCandidate
	for _, v := range matched {
		ref := ImageRef{Publisher: parts[0], Offer: parts[1], SKU: parts[2], Version: v}
		res, img, err := d.images.Lookup(ctx, region, ref)
		if err != nil {
			if vendors.IsClass(err, vendors.NotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, imageCandidate{res: res, img: img})
	}
	return out, nil
}

// reportParallel bounds the regions resolved at once.
const reportParallel = 8

// ReportImages implements vendors.ImageReporter: the image of the NodeClass's
// release in every region of its vendor block (every region of the subscription
// without a list) for both architectures. A region or architecture without an
// image is reported not ready with the reason; it does not fail the report.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	var owner string
	if b := nc.Spec.Vendors.Azure; b != nil {
		regions, owner = b.Regions, b.ImageOwner
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
		for _, arch := range []string{archAMD64, archARM64} {
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
					// A definitive answer: no image, another release, the terms.
					st.Message = err.Error()
					if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) || code == CodeMarketplaceTermsNotAccepted {
						st.Reason = code
					}
				default:
					// Throttling, a timeout: the answer is unknown, and an unknown must not
					// gate the region's offerings.
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
			return strings.Compare(a.Region, b.Region)
		}
		return strings.Compare(a.Arch, b.Arch)
	})
	return out, nil
}

// inexactPart says that a part of an images name that must be exact is empty or
// has a wildcard.
func inexactPart(p string) bool { return p == "" || selector.HasWildcard(p) }
