/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

const (
	imageTTL     = time.Hour
	imageMissTTL = 5 * time.Minute

	// The labels of the image convention (plan 15): a snapshot of a Flatcar
	// release carries all three, set when it is created.
	LabelOS             = "os"
	LabelFlatcarChannel = "flatcar-channel"
	LabelFlatcarVersion = "flatcar-version"
)

type imageKey struct{ arch, channel, version, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

// ResolveImage implements vendors.Driver (rung 3 of ADR-021): the snapshot of
// the pinned release in the user's own project, found by the labels os=flatcar,
// flatcar-channel and flatcar-version and the architecture, or by the NodeClass's
// images selector. Snapshots are global in the project, so the region is not
// part of the lookup. Whatever is found must say it is the pinned release.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	ha, ok := ImageArch(req.Arch)
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
	}
	enc, _ := json.Marshal(req.Use)
	key := imageKey{req.Arch, req.Channel, req.Version, string(enc)}
	d.mu.Lock()
	if c, ok := d.images[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		if c.err != nil {
			return nil, c.err
		}
		img := c.image
		return &img, nil
	}
	stale, haveStale := d.images[key]
	d.mu.Unlock()

	remember := func(err error) error {
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	im, err := d.findImage(ctx, req, ha)
	if err != nil {
		if haveStale && vendors.IsClass(err, vendors.Retryable) {
			if stale.err != nil {
				return nil, stale.err
			}
			img := stale.image
			return &img, nil
		}
		if vendors.IsClass(err, vendors.NotFound) || vendors.IsClass(err, vendors.Fatal) {
			return nil, remember(err)
		}
		return nil, err
	}
	channel, version := im.Labels[LabelFlatcarChannel], im.Labels[LabelFlatcarVersion]
	if err := vendors.CheckImageVersion(itoa(im.ID), channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: itoa(im.ID), Channel: channel, Version: version}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return &img, nil
}

// findImage is the lookup: the convention without a selector, the selector with.
func (d *Driver) findImage(ctx context.Context, req vendors.ImageRequest, ha hc.Architecture) (*hc.Image, error) {
	if req.Use.IsDefault() {
		return d.p.FindImage(ctx, req.Arch, req.Channel, req.Version)
	}
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return nil, vendors.NewError(vendors.NotFound, CodeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
	}
	if use.Select == "all" {
		return nil, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	images, err := d.listImages(ctx, use, ha)
	if err != nil {
		return nil, err
	}
	res, err := selector.Apply(use, d.Selectors()[KindImages], images, imageFields)
	d.noteMissing(KindImages, req.Region, res.Missing)
	if err != nil {
		return nil, selector.ImageError(err, req.Region, req.Arch)
	}
	return res.Items[0], nil
}

func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

// listImages lists the images a use block can select: snapshots unless the
// `type` filter says otherwise, available unless match.state says otherwise,
// narrowed on the server by architecture, labels and an exact name. The matcher
// re-applies everything. ha is zero for no architecture constraint.
func (d *Driver) listImages(ctx context.Context, use vendors.UseRef, ha hc.Architecture) ([]*hc.Image, error) {
	opts := hc.ImageListOpts{
		ListOpts: hc.ListOpts{LabelSelector: labelSelector(use)},
		Type:     []hc.ImageType{hc.ImageTypeSnapshot},
		Status:   imageStatuses(use.Match.State),
		Name:     exactName(use.Name),
		Sort:     []string{"created:desc"},
	}
	if ha != "" {
		opts.Architecture = []hc.Architecture{ha}
	} else if len(use.Match.Architecture) > 0 {
		for _, a := range use.Match.Architecture {
			if h, ok := ImageArch(a); ok {
				opts.Architecture = append(opts.Architecture, h)
			}
		}
	}
	if use.Match.IncludeDeprecated != nil {
		opts.IncludeDeprecated = *use.Match.IncludeDeprecated
	}
	for _, f := range use.Filters {
		switch f.Name {
		case filterType:
			opts.Type = nil
			for _, v := range f.Values {
				opts.Type = append(opts.Type, hc.ImageType(v))
			}
		case filterStatus:
			opts.Status = nil
			for _, v := range f.Values {
				opts.Status = append(opts.Status, hc.ImageStatus(v))
			}
		case filterBound:
			if len(f.Values) > 0 {
				if id, err := strconv.ParseInt(f.Values[0], 10, 64); err == nil {
					opts.BoundTo = &hc.Server{ID: id}
				}
			}
		}
	}
	out, err := d.api.Images(ctx, opts)
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// ReportImages implements vendors.ImageReporter: one snapshot per architecture,
// reported for every location of the NodeClass (snapshots are global in the
// project). A named architecture without a snapshot is not ready.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	var regions []string
	if nc.Spec.Vendors.HCloud != nil {
		regions = nc.Spec.Vendors.HCloud.Regions
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
			wg.Add(1)
			go func() {
				defer wg.Done()
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
