/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The label convention of the Flatcar templates (rung 3 of ADR-021, plan 22): the
// operator imports the image the Spare Cores images repository builds into a
// storage per zone and tier, makes a template of it and labels it. The driver
// only resolves and reports them; it never imports.
const (
	LabelImage          = "scarpenter-image"
	ImageFlatcar        = "flatcar"
	LabelFlatcarChannel = "flatcar-channel"
	LabelFlatcarVersion = "flatcar-version"
	LabelFlatcarArch    = "flatcar-arch"
	LabelFlatcarTier    = "flatcar-tier"

	archAMD64 = "amd64"
)

// Template is the resolved boot image of a zone: a private custom template made
// by the operator (rung 3 of ADR-021), found by labels or ID.
type Template struct {
	UUID   string
	Zone   string
	Tier   string
	SizeGB int
	// Channel and Version are the Flatcar release the template's labels say it is.
	Channel string
	Version string
	// Arch is the architecture its label says, "" when it has none (UpCloud runs
	// x86_64 only, so an unlabelled template is taken as amd64).
	Arch string
}

func labelMap(ls []upcloud.Label) map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Key] = l.Value
	}
	return m
}

func templateOf(s upcloud.Storage) Template {
	l := labelMap(s.Labels)
	tier := s.Tier
	if tier == "" {
		tier = l[LabelFlatcarTier]
	}
	return Template{
		UUID: s.UUID, Zone: s.Zone, Tier: tier, SizeGB: s.Size,
		Channel: l[LabelFlatcarChannel], Version: l[LabelFlatcarVersion], Arch: l[LabelFlatcarArch],
	}
}

// amd64 says whether a template can boot an UpCloud server: its label says
// amd64, or it has none.
func (t Template) amd64() bool { return t.Arch == "" || t.Arch == archAMD64 }

// templateState normalises a storage state.
func templateState(s string) string {
	switch s {
	case upcloud.StorageStateOnline:
		return "available"
	case upcloud.StorageStateError:
		return "failed"
	}
	// maintenance, syncing, cloning: still being made.
	return "pending"
}

func templateFields(s upcloud.Storage) selector.Fields {
	l := labelMap(s.Labels)
	attrs := map[string][]string{
		vendors.MatchState: {templateState(s.State)},
		vendors.MatchZone:  {s.Zone},
	}
	if a := l[LabelFlatcarArch]; a != "" {
		attrs[vendors.MatchArchitecture] = []string{a}
	}
	return selector.Fields{ID: s.UUID, Name: s.Title, Tags: l, Created: s.Created, Attrs: attrs}
}

// listTemplates lists the account's private templates of every zone in one call.
func (d *Driver) listTemplates(ctx context.Context) ([]upcloud.Storage, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	out, err := d.api.GetStorages(cctx, &request.GetStoragesRequest{
		Access: upcloud.StorageAccessPrivate, Type: upcloud.StorageTypeTemplate,
	})
	if err != nil {
		return nil, Classify(err)
	}
	return out.Storages, nil
}

// templateFilters are the native filters of the images kind, evaluated on the
// client (the list call has none): tier and template_type.
func filterTemplates(list []upcloud.Storage, fs []common.ResourceFilter) []upcloud.Storage {
	for _, f := range fs {
		list = slices.DeleteFunc(slices.Clone(list), func(s upcloud.Storage) bool {
			var have string
			switch f.Name {
			case FilterTier:
				have = s.Tier
			case FilterTemplateType:
				have = s.TemplateType
			default:
				return false
			}
			return !slices.ContainsFunc(f.Values, func(v string) bool { return selector.Glob(v, have) })
		})
	}
	return list
}

// Native filter names of the images and networks kinds.
const (
	FilterTier         = "tier"
	FilterTemplateType = "template_type"
	FilterType         = "type"
)

// codeImageNotFound is the reason of a zone without a template of the release.
const codeImageNotFound = selector.CodeImageNotFound

// tierPreference orders the tiers a zone's templates may have: MaxIOPS is the
// tier of most plans and the preferred one when a selector names templates of both.
var tierPreference = []string{upcloud.StorageTierMaxIOPS, upcloud.StorageTierStandard}

func tierRank(t string) int {
	if i := slices.Index(tierPreference, t); i >= 0 {
		return i
	}
	return len(tierPreference)
}

// imageUse is the selector of a request: the NodeClass's, narrowed to amd64;
// without one the label convention, narrowed by the pinned channel and version.
// UpCloud has x86_64 plans only: a request for another architecture is
// Unsupported whatever the selector, and a selector's templates labelled with
// another architecture never match.
func imageUse(req vendors.ImageRequest) (vendors.UseRef, error) {
	if req.Arch != "" && req.Arch != archAMD64 {
		return vendors.UseRef{}, vendors.NewError(vendors.Unsupported, "UnsupportedArch", fmt.Errorf(
			"UpCloud has x86_64 plans only, no %s: there is no %s template", req.Arch, req.Arch))
	}
	if req.Use.IsDefault() {
		return vendors.UseRef{
			Tags: map[string]string{
				LabelImage: ImageFlatcar, LabelFlatcarChannel: req.Channel, LabelFlatcarVersion: req.Version,
				LabelFlatcarArch: archAMD64,
			},
			Match:  common.ResourceMatch{State: []string{"available"}},
			Select: common.SelectExactlyOne,
		}, nil
	}
	use, ok := selector.NarrowArchitecture(req.Use, archAMD64)
	if !ok {
		return use, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
			"no image for %s in %s: match.architecture %v excludes it", archAMD64, req.Region, req.Use.Match.Architecture))
	}
	if use.Select == common.SelectAll {
		return use, vendors.Errorf(vendors.Fatal, "select all is not valid for images: a launch needs one image")
	}
	return use, nil
}

// resolveTemplates applies the request to the listed templates of the zone. The
// selector may name templates of both tiers (the operator imports one per
// tier), which is not ambiguous: one template per tier is allowed, and the
// returned image is the MaxIOPS one when both exist. All the templates that
// match and say they are the pinned release are returned, for the tier choice of
// CreateInstance.
func (d *Driver) resolveTemplates(
	list []upcloud.Storage, req vendors.ImageRequest,
) (*vendors.Image, []Template, error) {
	use, err := imageUse(req)
	if err != nil {
		return nil, nil, err
	}
	zone := req.Region
	var inZone []upcloud.Storage
	for _, s := range list {
		if s.Zone == zone {
			inZone = append(inZone, s)
		}
	}
	inZone = filterTemplates(inZone, use.Filters)
	caps := d.Selectors()[KindImages]
	sel := use.Select
	if sel == "" {
		sel = caps.DefaultSelect
	}
	q := use
	if sel == common.SelectExactlyOne {
		q.Select = common.SelectAll
	}
	res, err := selector.Apply(q, caps, inZone, templateFields)
	d.noteMissing(KindImages, zone, res.Missing)
	if err != nil {
		if _, none := errors.AsType[*selector.NoMatchError](err); none && req.Use.IsDefault() {
			return nil, nil, vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf(
				"no online Flatcar template for %s %s in %s (labels %s=%s, %s, %s, %s=%s): import the Flatcar template for %s: %w",
				req.Channel, req.Version, zone, LabelImage, ImageFlatcar, LabelFlatcarChannel, LabelFlatcarVersion,
				LabelFlatcarArch, archAMD64, req.Version, err))
		}
		return nil, nil, selector.ImageError(err, zone, archAMD64)
	}
	tmpls := make([]Template, 0, len(res.Items))
	for _, s := range res.Items {
		tmpls = append(tmpls, templateOf(s))
	}
	if sel == common.SelectExactlyOne {
		byTier := map[string][]string{}
		for _, t := range tmpls {
			byTier[t.Tier] = append(byTier[t.Tier], t.UUID)
		}
		for tier, ids := range byTier {
			if len(ids) > 1 {
				slices.Sort(ids)
				return nil, nil, vendors.NewError(vendors.Fatal, selector.CodeImageAmbiguous, fmt.Errorf(
					"the images selector is ambiguous for %s in %s: %d %s templates match (%s); select exactly one per tier",
					archAMD64, zone, len(ids), tier, strings.Join(ids, ", ")))
			}
		}
	}
	// The version check of ADR-017 for every template; those of another release
	// are dropped, and when none says the pinned release the first failure is the
	// answer.
	var ok []Template
	var first error
	for _, t := range tmpls {
		if err := vendors.CheckImageVersion(t.UUID, t.Channel, t.Version, req); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		ok = append(ok, t)
	}
	if len(ok) == 0 {
		return nil, nil, first
	}
	slices.SortStableFunc(ok, func(a, b Template) int { return tierRank(a.Tier) - tierRank(b.Tier) })
	best := ok[0]
	return &vendors.Image{
		Ref: best.UUID, RootDiskBytes: int64(best.SizeGB) * giB, Channel: best.Channel, Version: best.Version,
	}, ok, nil
}

// noteMissing logs the IDs a `use` named that the vendor did not return: a
// missing ID does not fail the others, so it would otherwise go unnoticed.
func (d *Driver) noteMissing(kind vendors.ResourceKind, region string, ids []string) {
	if len(ids) > 0 {
		d.log.Info("IDs of a use selector are not in the vendor's answer", "kind", kind, "region", region, "ids", ids)
	}
}

func (d *Driver) rememberTemplates(zone string, tmpls []Template) {
	d.mu.Lock()
	changed := !slices.Equal(d.templates[zone], tmpls)
	d.templates[zone] = slices.Clone(tmpls)
	d.mu.Unlock()
	if changed {
		d.epoch.Add(1)
	}
}

// ResolveImage implements vendors.Driver: the private templates of the zone, the
// NodeClass's images.use narrowed to the architecture (without one the label
// convention, narrowed by channel and version), finished by the shared matcher,
// and exactly one match per tier under the default select. The chosen template
// must say it is the pinned release (ADR-017): by its labels flatcar-channel and
// flatcar-version; another release is ImageVersionMismatch, none
// ImageVersionUnknown, and the image is not returned. UpCloud has no public
// Flatcar template and none can be shared across accounts (rung 3), so a zone
// without the operator's template is NotFound.
func (d *Driver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	list, err := d.listTemplates(ctx)
	if err != nil {
		return nil, err
	}
	img, tmpls, err := d.resolveTemplates(list, req)
	if err != nil {
		return nil, err
	}
	d.rememberTemplates(req.Region, tmpls)
	return img, nil
}

// ReportImages implements vendors.ImageReporter: the template of the NodeClass's
// release in every zone of its vendor block (every zone of the account without a
// list), amd64 only. One list call serves every zone. A zone without a template
// is reported not ready with the reason; it does not fail the report.
func (d *Driver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var zones []string
	if nc.Spec.Vendors.UpCloud != nil {
		zones = nc.Spec.Vendors.UpCloud.Regions
	}
	if len(zones) == 0 {
		all, err := d.Regions(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			zones = append(zones, r.Name)
		}
	}
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = selector.DefaultChannel
	}
	list, err := d.listTemplates(ctx)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.ImageStatus
	for _, zone := range zones {
		st := v1alpha1.ImageStatus{Region: zone, Arch: archAMD64}
		use, _, err := selector.ImageUse(nc, d, zone, archAMD64)
		var img *vendors.Image
		if err == nil {
			var tmpls []Template
			img, tmpls, err = d.resolveTemplates(list, vendors.ImageRequest{
				Region: zone, Arch: archAMD64, Channel: channel, Version: nc.Spec.Flatcar.Version, Use: use,
			})
			if err == nil {
				d.rememberTemplates(zone, tmpls)
			}
		}
		if err == nil {
			st.Ref, st.Ready = img.Ref, true
		} else {
			st.Message = err.Error()
			if code := vendors.CodeOf(err); vendors.IsImageVersionCode(code) {
				st.Reason = code
			}
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b v1alpha1.ImageStatus) int { return strings.Compare(a.Region, b.Region) })
	return out, nil
}

// templateFor returns the template a launch clones: the resolved one (image.Ref)
// or, when the root tier asks for another, its sibling of the same release in the
// tier. A reference the driver has not resolved itself (a restart, a test) is
// read from the API.
func (d *Driver) templateFor(ctx context.Context, zone, ref, wantTier string, plan Plan) (Template, error) {
	d.mu.Lock()
	known := slices.Clone(d.templates[zone])
	d.mu.Unlock()
	base, ok := Template{}, false
	for _, t := range known {
		if t.UUID == ref {
			base, ok = t, true
		}
	}
	if !ok {
		cctx, cancel := CallContext(ctx, CallTimeout)
		defer cancel()
		s, err := d.api.GetStorageDetails(cctx, &request.GetStorageDetailsRequest{UUID: ref})
		if err != nil {
			e := Classify(err)
			if vendors.IsClass(e, vendors.NotFound) {
				return Template{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing, fmt.Errorf(
					"the Flatcar template %s does not exist in %s: %w", ref, zone, e))
			}
			return Template{}, e
		}
		if s.Zone != zone || s.Type != upcloud.StorageTypeTemplate {
			return Template{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing, fmt.Errorf(
				"%s is not a template of %s", ref, zone))
		}
		base = templateOf(s.Storage)
		if !base.amd64() {
			return Template{}, vendors.NewError(vendors.Unsupported, "UnsupportedArch", fmt.Errorf(
				"the template %s is labelled %s=%s: UpCloud has x86_64 plans only", ref, LabelFlatcarArch, base.Arch))
		}
		known = append(known, base)
	}
	want := wantTier
	if want == "" {
		// The default tier: the plan's included tier where a template of it exists,
		// else the tier of the resolved template.
		want = base.Tier
		if pt := plan.DefaultRootTier(); plan.StorageGB > 0 && slices.ContainsFunc(known, func(t Template) bool {
			return t.Tier == pt && t.Channel == base.Channel && t.Version == base.Version
		}) {
			want = pt
		}
	}
	if want == base.Tier {
		return base, nil
	}
	for _, t := range known {
		if t.Tier == want && t.Channel == base.Channel && t.Version == base.Version && t.amd64() {
			return t, nil
		}
	}
	return Template{}, vendors.NewError(vendors.Unsupported, "TierMismatch", fmt.Errorf(
		"a %s root disk needs a %s Flatcar template of %s %s in %s; only a %s template is known "+
			"(import one, label it %s=%s, or set storage.root.tier to %s)",
		want, want, base.Channel, base.Version, zone, base.Tier, LabelFlatcarTier, want, base.Tier))
}

// VendorArch implements vendors.ArchNamer: UpCloud has x86_64 plans only.
func (d *Driver) VendorArch(arch string) (string, bool) {
	if arch == archAMD64 {
		return archAMD64, true
	}
	return "", false
}
