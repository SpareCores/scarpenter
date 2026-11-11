/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Image cache lifetimes, as the AWS driver's: a resolved image is trusted for
// an hour, a definitive "none" for five minutes.
const (
	imageTTL     = time.Hour
	imageMissTTL = 5 * time.Minute
)

type imageKey struct{ arch, channel, version, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

// SnapshotDescription is the description the images pipeline gives the Flatcar
// snapshot of a release: flatcar-<channel>-<version>-<arch>.
func SnapshotDescription(channel, version, arch string) string {
	return fmt.Sprintf("flatcar-%s-%s-%s", channel, version, arch)
}

// descriptionPattern reads the release a snapshot says it is: the pipeline's
// description, optionally followed by a space and a note ("flatcar-stable-4081.2.0-amd64
// rebuilt"). A snapshot has no tags, so the description is all it can say.
var descriptionPattern = regexp.MustCompile(`^flatcar-([a-z]+)-([0-9]+\.[0-9]+\.[0-9]+)-amd64(?: .*)?$`)

// snapshotRelease returns the Flatcar channel and version a snapshot says it is,
// empty when its description says neither (the version check then refuses it).
func snapshotRelease(description string) (channel, version string) {
	if m := descriptionPattern.FindStringSubmatch(description); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

// imageUse is the selector of a request: the NodeClass's narrowed to the
// architecture; without one the pipeline's description of the pinned release, in
// state available.
func imageUse(req vendors.ImageRequest) (vendors.UseRef, error) {
	if req.Use.IsDefault() {
		return vendors.UseRef{
			Name:   SnapshotDescription(req.Channel, req.Version, req.Arch),
			Match:  common.ResourceMatch{Architecture: []string{req.Arch}, State: []string{stateAvailable}},
			Select: common.SelectExactlyOne,
		}, nil
	}
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return use, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"no image for %s: match.architecture %v excludes it", req.Arch, req.Use.Match.Architecture))
	}
	if use.Select == common.SelectAll {
		return use, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	return use, nil
}

// listSnapshots lists the account's snapshots, with the description hint when
// the name has no wildcard (a superset of what the matcher keeps whether the
// API compares exactly or by substring: the matcher re-checks it).
func (d *Driver) listSnapshots(ctx context.Context, use vendors.UseRef) ([]govultr.Snapshot, error) {
	opts := govultr.ListOptions{}
	if use.Name != "" && !selector.HasWildcard(use.Name) && !strings.Contains(use.Name, `\`) && len(use.IDs) == 0 {
		opts.Description = use.Name
	}
	all, err := listAll(ctx, opOther, opts, d.api.ListSnapshots)
	if err != nil {
		return nil, err
	}
	return byFilters(all, use.Filters, snapshotValue), nil
}

// ResolveImage implements vendors.Driver: the snapshot the images selector
// chooses (the NodeClass's images.use narrowed to the architecture; without one
// the description flatcar-<channel>-<version>-<arch>, state complete), finished by
// the shared matcher, exactly one under the default select. The snapshot must say
// it is the pinned release by its description (ADR-017): another release is
// ImageVersionMismatch, none ImageVersionUnknown, and nothing is returned.
// Snapshots are global in the account, so the answer does not depend on the
// region. Vultr has no Arm instances: arm64 is Unsupported.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if req.Arch != archAMD64 {
		return nil, noInstancesOf(req.Arch)
	}
	enc, _ := json.Marshal(req.Use)
	key := imageKey{req.Arch, req.Channel, req.Version, string(enc)}
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
	use, err := imageUse(req)
	if err != nil {
		return nil, err
	}
	snaps, err := d.listSnapshots(ctx, use)
	if err != nil {
		// A transient failure serves the last good answer, even past its TTL.
		if hit && vendors.IsClass(err, vendors.Retryable) {
			if c.err != nil {
				return nil, c.err
			}
			img := c.image
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
	res, err := selector.Apply(use, d.Selectors()[KindImages], snaps, snapshotFields)
	d.noteMissing(KindImages, res.Missing)
	if err != nil {
		if _, none := errors.AsType[*selector.NoMatchError](err); none && req.Use.IsDefault() {
			err = vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
				"no complete snapshot is described %q in the account: %w", use.Name, err))
		} else {
			err = selector.ImageError(err, "the account", req.Arch)
		}
		if c := vendors.ClassOf(err); c == vendors.NotFound || c == vendors.Fatal {
			return nil, remember(err)
		}
		return nil, err
	}
	s := res.Items[0]
	channel, version := snapshotRelease(s.Description)
	if err := vendors.CheckImageVersion(s.ID, channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: s.ID, Channel: channel, Version: version}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return &img, nil
}

// noteMissing logs the IDs a `use` named that the vendor did not return.
func (d *Driver) noteMissing(kind vendors.ResourceKind, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "ids", ids)
	}
}

// ReportImages implements vendors.ImageReporter: the snapshot of the NodeClass's
// release in every region of its vendor block (every region of the API without a
// list), for amd64 only (Vultr has no Arm instances). A region without a usable
// snapshot is reported not ready with the reason; it does not fail the report. The
// resolutions are cached, so a report after the first costs no calls.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	var regions []string
	if nc.Spec.Vendors.Vultr != nil {
		regions = nc.Spec.Vendors.Vultr.Regions
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
	var out []v1alpha1.ImageStatus
	for _, region := range regions {
		st := v1alpha1.ImageStatus{Region: region, Arch: archAMD64}
		use, _, err := selector.ImageUse(nc, d, region, st.Arch)
		var img *vendors.Image
		if err == nil {
			img, err = d.ResolveImage(ctx, vendors.ImageRequest{
				Region: region, Arch: st.Arch, Channel: channel, Version: nc.Spec.Flatcar.Version, Use: use,
			})
		}
		switch {
		case err == nil:
			st.Ref, st.Ready = img.Ref, true
		case !vendors.IsClass(err, vendors.Retryable):
			st.Message = err.Error()
			if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) {
				st.Reason = code
			}
		default:
			// Throttling, a timeout: an unknown must not gate the region, so the
			// whole report fails and the status keeps the last verdict.
			return nil, fmt.Errorf("%s: %w", region, err)
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b v1alpha1.ImageStatus) int { return strings.Compare(a.Region, b.Region) })
	return out, nil
}
