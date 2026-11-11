/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// imageTTL is how long a resolved image is trusted (overview section 9);
// imageMissTTL how long a definitive miss is remembered.
const (
	imageTTL     = time.Hour
	imageMissTTL = 5 * time.Minute
)

// Codes of the image errors.
const codeImageNotFound = selector.CodeImageNotFound

// Architectures, as the catalog and Compute spell them.
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

var imageArches = map[string]string{archAMD64: "X86_64", archARM64: "ARM64"}

type imageKey struct{ region, arch, channel, version, owner, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
	err   error
}

// imageName is the name of the Flatcar image of the release in the public
// project: flatcar-<channel>-<major>-<minor>-<patch> with dashes for dots (the
// stable shape is documented by Flatcar; lts, beta and alpha are UNVERIFIED),
// and the same with "-arm64" for the images Spare Cores publishes (plan 22's
// naming).
func imageName(channel, version, arch string) string {
	n := "flatcar-" + channel + "-" + strings.ReplaceAll(version, ".", "-")
	if arch == archARM64 {
		n += "-arm64"
	}
	return n
}

var (
	imageNamePattern = regexp.MustCompile(`^flatcar-([a-z]+)-([0-9]+)-([0-9]+)-([0-9]+)(?:-arm64)?$`)
	versionPattern   = regexp.MustCompile(`^[0-9]+([._-][0-9]+){2}$`)
)

// The labels with which an image says which Flatcar release it is when its name
// does not (a copy under another name, an image of the operator's own build). A
// version label may spell the dots as "-" or "_" (label values have no dots).
const (
	labelFlatcarChannel = "flatcar-channel"
	labelFlatcarVersion = "flatcar-version"
)

func normalizeVersion(v string) string {
	if !versionPattern.MatchString(v) {
		return v
	}
	return strings.NewReplacer("-", ".", "_", ".").Replace(v)
}

// imageRelease reads the Flatcar release an image says it is: from its name when
// that is the official naming, else from the labels flatcar-channel and
// flatcar-version. Empty when it says neither. An image whose name and labels
// name two releases says nothing it can be trusted on: conflict is true.
func imageRelease(im *computepb.Image) (channel, version string, conflict bool) {
	lc, lv := im.GetLabels()[labelFlatcarChannel], normalizeVersion(im.GetLabels()[labelFlatcarVersion])
	if m := imageNamePattern.FindStringSubmatch(im.GetName()); m != nil {
		v := m[2] + "." + m[3] + "." + m[4]
		conflict = (lv != "" && lv != v) || (lc != "" && !strings.EqualFold(lc, m[1]))
		return m[1], v, conflict
	}
	return lc, lv, false
}

// imageID is the canonical reference of an image: projects/<p>/global/images/<n>.
func imageID(im *computepb.Image, project string) string {
	if u, ok := fromProjects(im.GetSelfLink()); ok && strings.Contains(u, "/images/") {
		return u
	}
	return ProjectPrefix(project) + "global/images/" + im.GetName()
}

// splitImageRef returns the project and name of an image reference; a short name
// gets the default project.
func splitImageRef(project, ref string) (p, name string) {
	u := ImageURL(project, ref)
	parts := strings.Split(u, "/")
	return parts[1], parts[len(parts)-1]
}

// imageUse is the selector of a request: the NodeClass's, narrowed to the
// request's architecture; without one it is nil (the driver's own convention).
func (d *Driver) imageUse(req vendors.ImageRequest) (use vendors.UseRef, err error) {
	use, ok := selector.NarrowArchitecture(req.Use, req.Arch)
	if !ok {
		return use, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", req.Arch, req.Region, req.Use.Match.Architecture))
	}
	if use.Select == common.SelectAll {
		return use, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	return use, nil
}

// owners are the projects a selector without ids lists: match.owner (self is the
// controller's project), else the block's imageProject, else the controller's
// project (the user's own images; the public project is reachable by name only).
func (d *Driver) owners(use vendors.UseRef, owner string) []string {
	var out []string
	switch {
	case len(use.Match.Owner) > 0:
		for _, o := range use.Match.Owner {
			if o == "self" {
				o = d.project
			}
			out = append(out, o)
		}
	case owner != "":
		out = []string{owner}
	default:
		out = []string{d.project}
	}
	return out
}

// ResolveImage implements vendors.Driver. Without a selector the pinned release
// is read by name in the image project (images.get: the public images cannot be
// listed). With one, the images of its ids are read by images.get, an exact name
// or family too, and the rest is listed per owner project with the filter Compute
// can evaluate; the shared matcher finishes. The chosen image must say it is the
// pinned release (ADR-017), by its name or its labels; another release is
// ImageVersionMismatch, none ImageVersionUnknown, and the image is not returned.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if _, ok := imageArches[req.Arch]; !ok {
		return nil, vendors.Errorf(vendors.Fatal, "unsupported architecture %q (amd64 and arm64 are)", req.Arch)
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
	remember := func(err error) error {
		d.mu.Lock()
		d.images[key] = cachedImage{err: err, until: d.clock.Now().Add(imageMissTTL)}
		d.mu.Unlock()
		return err
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	var im *computepb.Image
	var id string
	if req.Use.IsDefault() {
		im, id, err = d.defaultImage(ctx, api, req)
	} else {
		im, id, err = d.selectedImage(ctx, api, req)
	}
	if err != nil {
		// A transient failure serves the last good image, even past its TTL.
		if vendors.IsClass(err, vendors.Retryable) {
			d.mu.Lock()
			stale, ok := d.images[key]
			d.mu.Unlock()
			if ok && stale.err == nil {
				img := stale.image
				return &img, nil
			}
			return nil, err
		}
		if vendors.ClassOf(err) == vendors.NotFound || vendors.ClassOf(err) == vendors.Fatal {
			return nil, remember(err)
		}
		return nil, err
	}
	channel, version, conflict := imageRelease(im)
	if conflict {
		return nil, remember(vendors.NewError(vendors.Fatal, vendors.CodeImageVersionUnknown, fmt.Errorf(
			"the image %s is named Flatcar %s %s, but its labels %s and %s say otherwise; "+
				"an image of unknown version is never booted", id, channel, version, labelFlatcarChannel, labelFlatcarVersion)))
	}
	if err := vendors.CheckImageVersion(id, channel, version, req); err != nil {
		return nil, remember(err)
	}
	img := vendors.Image{Ref: id, Channel: channel, Version: version, RootDiskBytes: im.GetDiskSizeGb() * giB}
	d.mu.Lock()
	d.images[key] = cachedImage{image: img, until: d.clock.Now().Add(imageTTL)}
	d.mu.Unlock()
	return &img, nil
}

// checkImageArch refuses an image of another architecture than the launch's.
func checkImageArch(im *computepb.Image, id, arch string) error {
	if a := im.GetArchitecture(); a != "" && a != imageArches[arch] {
		return vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"the image %s is %s, not %s", id, a, imageArches[arch]))
	}
	return nil
}

// defaultImage is the lookup of the driver's own convention: the pinned release
// by name in the image project (rungs 1 and 2 of ADR-021).
func (d *Driver) defaultImage(ctx context.Context, api ComputeAPI, req vendors.ImageRequest,
) (*computepb.Image, string, error) {
	project := req.Owner
	if project == "" {
		project = DefaultImageProject
		if req.Arch == archARM64 {
			project = SpareCoresImageProject
		}
	}
	if project == "" {
		return nil, "", vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"there is no %s Flatcar image for GCE: Flatcar publishes none and the Spare Cores image project is not "+
				"configured; set vendors.gcp.imageProject or resources.images.use", req.Arch))
	}
	name := imageName(req.Channel, req.Version, req.Arch)
	im, err := api.GetImage(ctx, &computepb.GetImageRequest{Project: project, Image: name})
	if err != nil {
		if IsNotFound(err) {
			return nil, "", vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
				"no image %q (%s) in the project %s: %w", name, req.Arch, project, err))
		}
		return nil, "", classify(err, errContext{})
	}
	id := ProjectPrefix(project) + "global/images/" + name
	if err := checkImageArch(im, id, req.Arch); err != nil {
		return nil, "", err
	}
	if st := im.GetStatus(); st != "" && st != statusReady {
		return nil, "", vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf("the image %s is %s", id, st))
	}
	if st := im.GetDeprecated().GetState(); st == "DELETED" || st == "OBSOLETE" {
		return nil, "", vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf("the image %s is %s", id, st))
	}
	return im, id, nil
}

// imagesOf collects the candidate images of a selector.
func (d *Driver) imagesOf(ctx context.Context, api ComputeAPI, use vendors.UseRef, owner string,
) ([]*computepb.Image, error) {
	var out []*computepb.Image
	seen := map[string]bool{}
	add := func(project string, im *computepb.Image) {
		if im == nil {
			return
		}
		id := imageID(im, project)
		if !seen[id] {
			seen[id] = true
			if im.SelfLink == nil || !strings.Contains(im.GetSelfLink(), "/images/") {
				im.SelfLink = &id
			}
			out = append(out, im)
		}
	}
	get := func(project, name string) error {
		im, err := api.GetImage(ctx, &computepb.GetImageRequest{Project: project, Image: name})
		switch {
		case err == nil:
			add(project, im)
		case IsNotFound(err):
		default:
			return classify(err, errContext{})
		}
		return nil
	}
	for _, id := range use.IDs {
		p, n := splitImageRef(d.project, id)
		if err := get(p, n); err != nil {
			return nil, err
		}
	}
	if len(use.IDs) > 0 {
		return out, nil
	}
	owners := d.owners(use, owner)
	family := ""
	var native []common.ResourceFilter
	for _, f := range use.Filters {
		if f.Name == "family" && len(f.Values) == 1 {
			family = f.Values[0]
			continue
		}
		native = append(native, f)
	}
	exact := use.Name != "" && !selector.HasWildcard(use.Name)
	for _, p := range owners {
		if exact {
			if err := get(p, use.Name); err != nil {
				return nil, err
			}
		}
		if family != "" {
			im, err := api.GetImageFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: p, Family: family})
			switch {
			case err == nil:
				add(p, im)
			case !IsNotFound(err):
				return nil, classify(err, errContext{})
			}
		}
	}
	// A selector that only names an exact name or a family is answered by the
	// gets: publicly shared images cannot be listed. Anything else is listed.
	if (exact || family != "") && len(use.Tags) == 0 && len(native) == 0 && use.Expression == "" &&
		!matchSet(use.Match) {
		return out, nil
	}
	listed, err := d.listImages(ctx, api, owners, imageFilter(use, native))
	if err != nil {
		return nil, err
	}
	for _, l := range listed {
		add(l.project, l.image)
	}
	return out, nil
}

type listedImage struct {
	project string
	image   *computepb.Image
}

// listImages lists the images of the owner projects with the filter, every page.
func (d *Driver) listImages(ctx context.Context, api ComputeAPI, owners []string, filter string,
) ([]listedImage, error) {
	var out []listedImage
	for _, p := range owners {
		req := &computepb.ListImagesRequest{Project: p}
		if filter != "" {
			req.Filter = &filter
		}
		for {
			res, err := api.ListImages(ctx, req)
			if err != nil {
				return nil, classify(err, errContext{})
			}
			for _, im := range res.GetItems() {
				out = append(out, listedImage{p, im})
			}
			if res.GetNextPageToken() == "" {
				break
			}
			req.PageToken = res.NextPageToken
		}
	}
	return out, nil
}

// matchSet says whether the selector sets a match attribute beyond the
// architecture, which the request always narrows.
func matchSet(m common.ResourceMatch) bool {
	return len(m.State) > 0 || m.IncludeDeprecated != nil || m.CreatedAfter != nil || m.CreatedBefore != nil ||
		m.Description != ""
}

// imageFilter builds the list filter of a selector in AIP-160 syntax (the two
// syntaxes cannot be mixed in one request): the labels of `tags`, an exact name,
// and the native filters as equalities; `expression` is sent as written and
// replaces them. The shared matcher re-applies everything it has data for.
func imageFilter(use vendors.UseRef, native []common.ResourceFilter) string {
	if use.Expression != "" {
		return use.Expression
	}
	var parts []string
	for _, k := range sortedKeys(use.Tags) {
		if use.Tags[k] == "*" {
			parts = append(parts, fmt.Sprintf("(labels.%s:*)", k))
		} else {
			parts = append(parts, fmt.Sprintf("(labels.%s = %q)", k, use.Tags[k]))
		}
	}
	if use.Name != "" && !selector.HasWildcard(use.Name) {
		parts = append(parts, fmt.Sprintf("(name = %q)", use.Name))
	}
	for _, f := range native {
		if len(f.Values) == 1 {
			parts = append(parts, fmt.Sprintf("(%s = %q)", f.Name, f.Values[0]))
		} else if len(f.Values) > 1 {
			alts := make([]string, len(f.Values))
			for i, v := range f.Values {
				alts[i] = fmt.Sprintf("(%s = %q)", f.Name, v)
			}
			parts = append(parts, "("+strings.Join(alts, " OR ")+")")
		}
	}
	return strings.Join(parts, " AND ")
}

// selectedImage resolves the images selector of a NodeClass.
func (d *Driver) selectedImage(ctx context.Context, api ComputeAPI, req vendors.ImageRequest,
) (*computepb.Image, string, error) {
	use, err := d.imageUse(req)
	if err != nil {
		return nil, "", err
	}
	cands, err := d.imagesOf(ctx, api, use, req.Owner)
	if err != nil {
		return nil, "", err
	}
	if len(use.IDs) > 0 {
		ids := make([]string, len(use.IDs))
		for i, id := range use.IDs {
			p, n := splitImageRef(d.project, id)
			ids[i] = ProjectPrefix(p) + "global/images/" + n
		}
		use.IDs = ids
	}
	res, err := selector.Apply(use, d.Selectors()[KindImages], cands, func(im *computepb.Image) selector.Fields {
		return imageFields(im, d.project)
	})
	d.noteMissing(KindImages, req.Region, res.Missing)
	if err != nil {
		return nil, "", selector.ImageError(err, req.Region, req.Arch)
	}
	im := res.Items[0]
	id := imageID(im, d.project)
	if err := checkImageArch(im, id, req.Arch); err != nil {
		return nil, "", err
	}
	return im, id, nil
}

// noteMissing logs the IDs a `use` named that the vendor did not return.
func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

// imageFields reads what the matcher re-applies of an image.
func imageFields(im *computepb.Image, project string) selector.Fields {
	state := im.GetStatus()
	switch state {
	case statusReady, "":
		state = stateAvailable
	case "PENDING":
		state = "pending"
	case "FAILED", "DELETING":
		state = stateFailed
	}
	attrs := map[string][]string{vendors.MatchState: {state}, vendors.MatchDescription: {im.GetDescription()}}
	for k, v := range imageArches {
		if v == im.GetArchitecture() {
			attrs[vendors.MatchArchitecture] = []string{k}
		}
	}
	if st := im.GetDeprecated().GetState(); st != "" && st != "ACTIVE" {
		attrs[vendors.MatchIncludeDeprecated] = []string{"true"}
	}
	f := selector.Fields{ID: imageID(im, project), Name: im.GetName(), Tags: im.GetLabels(), Attrs: attrs}
	if t, err := time.Parse(time.RFC3339, im.GetCreationTimestamp()); err == nil {
		f.Created = t
	}
	return f
}

// VendorArch implements vendors.ArchNamer: Compute's architecture enum, for
// {vendorArch}.
func (d *Driver) VendorArch(arch string) (string, bool) {
	v, ok := imageArches[arch]
	return v, ok
}

// reportParallel bounds the resolutions at once.
const reportParallel = 8

// ReportImages implements vendors.ImageReporter: the image of the NodeClass's
// release for both architectures in every region of its vendor block (every
// region of the project without a list). Images are global, so one lookup serves
// every region unless the selector names region keys; resolutions are cached for
// an hour. A region or architecture without an image is reported not ready with
// the reason; it does not fail the report.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var regions []string
	var owner string
	if b := nc.Spec.Vendors.GCP; b != nil {
		regions, owner = b.Regions, b.ImageProject
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
