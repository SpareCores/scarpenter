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
	"strings"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// LabelLocation is the label that records the location a managed firewall or
// placement group was made for: Hetzner has these project-wide, SCarpenter
// names and counts them per location.
const LabelLocation = "scarpenter.sparecores.com/location"

// Kinds implements vendors.ManagedResources. The delete order is servers, then
// volumes, placement groups, firewalls; networks are never deleted (use or the
// opt-in label only: an optional kind, a server without one has public
// addresses only).
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindVolumes, Scope: vendors.ScopeNode, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 10},
		{Kind: KindPlacementGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindFirewalls, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
		{Kind: KindNetworks, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleRetain, DeleteOrder: 40,
			Optional: true},
	}
}

func isManagedKind(kind vendors.ResourceKind) bool {
	return kind == KindFirewalls || kind == KindPlacementGroups || kind == KindVolumes
}

// owned reports whether the labels carry this cluster's ownership marker.
func (d *Driver) owned(labels map[string]string) bool {
	cluster := d.cfg.ClusterName
	return cluster != "" && labels[TagKey(vendors.TagManagedBy)] == vendors.ManagedByValue &&
		labels[TagKey(vendors.TagCluster)] == LabelValue(cluster)
}

// resourceLabels are the labels of a managed resource: the marker tags, the
// NodeClass's own tags and the location.
func resourceLabels(spec vendors.CreateSpec, region string) (map[string]string, error) {
	labels, err := Labels(spec.Tags, spec.ExtraTags)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	labels[LabelLocation] = LabelValue(region)
	return labels, nil
}

func (d *Driver) ref(kind vendors.ResourceKind, region string, id int64, name string, labels map[string]string,
	created time.Time,
) vendors.ResourceRef {
	return vendors.ResourceRef{
		Kind: kind, Region: region, ID: itoa(id), Name: name, Managed: d.owned(labels), Tags: TagsOf(labels),
		CreatedAt: created,
	}
}

// Resolve implements vendors.ManagedResources: the existing resources a `use`
// selects. One project-wide list per call (label selector and exact name on the
// server), then the shared matcher.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the vendor's default is Default's")
	}
	caps, ok := d.Selectors()[kind]
	if kind == KindVolumes {
		// Volumes are internal (no use block): this is the lookup of the engine and tests.
		caps, ok = vendors.SelectorCaps{
			Tags: vendors.TagsLabels, Name: &vendors.FieldCap{Field: fieldName, Side: vendors.SideServerClient},
			DefaultSelect: common.SelectAll,
		}, true
	}
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "the Hetzner driver has no resource kind %q", kind)
	}
	switch kind {
	case KindVolumes:
		list, err := d.api.Volumes(ctx, hc.VolumeListOpts{
			ListOpts: hc.ListOpts{LabelSelector: labelSelector(ref)}, Name: exactName(ref.Name),
		})
		if err != nil {
			return nil, classify(err)
		}
		res, err := selector.Apply(ref, caps, list, func(v *hc.Volume) selector.Fields {
			return selector.Fields{ID: itoa(v.ID), Name: v.Name, Tags: v.Labels}
		})
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, v := range res.Items {
			out = append(out, d.ref(kind, region, v.ID, v.Name, v.Labels, v.Created))
		}
		return out, nil
	case KindImages:
		images, err := d.listImages(ctx, ref, "")
		if err != nil {
			return nil, err
		}
		res, err := selector.Apply(ref, caps, images, imageFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, im := range res.Items {
			out = append(out, d.ref(kind, region, im.ID, im.Name, nil, im.Created))
		}
		return out, nil
	case KindNetworks:
		return d.resolveNetworks(ctx, caps, region, ref)
	case KindFirewalls:
		list, err := d.api.Firewalls(ctx, hc.FirewallListOpts{
			ListOpts: hc.ListOpts{LabelSelector: labelSelector(ref)}, Name: exactName(ref.Name),
		})
		if err != nil {
			return nil, classify(err)
		}
		res, err := selector.Apply(ref, caps, list, firewallFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, f := range res.Items {
			out = append(out, d.ref(kind, region, f.ID, f.Name, f.Labels, f.Created))
		}
		return out, nil
	case KindPlacementGroups:
		list, err := d.api.PlacementGroups(ctx, hc.PlacementGroupListOpts{
			ListOpts: hc.ListOpts{LabelSelector: labelSelector(ref)}, Name: exactName(ref.Name),
		})
		if err != nil {
			return nil, classify(err)
		}
		res, err := selector.Apply(ref, caps, list, placementGroupFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, g := range res.Items {
			out = append(out, d.ref(kind, region, g.ID, g.Name, g.Labels, g.Created))
		}
		return out, nil
	}
	return nil, vendors.Errorf(vendors.Fatal, "the Hetzner driver has no resource kind %q to use", kind)
}

// resolveNetworks selects networks that have a cloud subnet in the network zone
// of the location: a network without one cannot host a server there.
func (d *Driver) resolveNetworks(
	ctx context.Context, caps vendors.SelectorCaps, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	zone, err := d.p.NetworkZone(ctx, region)
	if err != nil {
		return nil, err
	}
	list, err := d.api.Networks(ctx, hc.NetworkListOpts{
		ListOpts: hc.ListOpts{LabelSelector: labelSelector(ref)}, Name: exactName(ref.Name),
	})
	if err != nil {
		return nil, classify(err)
	}
	listed := len(list)
	list = slices.DeleteFunc(list, func(n *hc.Network) bool { return !NetworkHasSubnetIn(n, zone) })
	res, err := selector.Apply(ref, caps, list, networkFields)
	d.noteMissing(KindNetworks, region, res.Missing)
	if err != nil {
		if nm, ok := errors.AsType[*selector.NoMatchError](err); ok && len(list) < listed {
			stages := []selector.Stage{
				{Name: "listed", Count: listed},
				{Name: "subnet", Detail: fmt.Sprintf("in the network zone %q of %s", zone, region), Count: len(list)},
			}
			if len(nm.Stages) > 1 {
				stages = append(stages, nm.Stages[1:]...)
			}
			return nil, &selector.NoMatchError{Stages: stages, Missing: nm.Missing, Mismatched: nm.Mismatched}
		}
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, vendors.ResourceRef{
			Kind: KindNetworks, Region: region, ID: itoa(n.ID), Name: n.Name, Tags: TagsOf(n.Labels), CreatedAt: n.Created,
		})
	}
	return out, nil
}

// Ensure implements vendors.ManagedResources: it finds the resource by its
// deterministic name and ownership marker, or creates it in one call with the
// labels; a name that exists without the marker is refused.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if err := d.ready(); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if !isManagedKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"the Hetzner driver does not create %q (networks are brought with use; images are made "+
				"with hcloud-upload-image)", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	labels, err := resourceLabels(spec, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	switch kind {
	case KindFirewalls:
		return d.ensureFirewall(ctx, region, spec, labels)
	case KindPlacementGroups:
		return d.ensurePlacementGroup(ctx, region, spec, labels)
	}
	return d.ensureVolume(ctx, region, spec)
}

func notOwned(kind vendors.ResourceKind, name string) error {
	return vendors.NewError(vendors.Fatal, codeNotOwned, fmt.Errorf(
		"a %s named %s exists without the ownership marker of this cluster", kind, name))
}

func (d *Driver) ensureFirewall(
	ctx context.Context, region string, spec vendors.CreateSpec, labels map[string]string,
) (vendors.ResourceRef, bool, error) {
	var params FirewallParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings,
				fmt.Errorf("the firewall parameters: %w", err))
		}
	}
	rules, err := FirewallRules(params.Rules)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	find := func() (*hc.Firewall, error) {
		list, err := d.api.Firewalls(ctx, hc.FirewallListOpts{Name: spec.Name})
		if err != nil {
			return nil, classify(err)
		}
		for _, f := range list {
			if f.Name == spec.Name {
				return f, nil
			}
		}
		return nil, nil
	}
	f, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if f == nil {
		f, err = d.api.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: spec.Name, Labels: labels, Rules: rules})
		switch {
		case err == nil:
			created = true
		case isCode(err, hc.ErrorCodeUniquenessError):
			if f, err = find(); err != nil || f == nil {
				return vendors.ResourceRef{}, false, vendors.NewError(vendors.Retryable, CodeNameInUse,
					fmt.Errorf("the firewall %s vanished: %v", spec.Name, err))
			}
		default:
			return vendors.ResourceRef{}, false, classify(err)
		}
	}
	if !d.owned(f.Labels) {
		return vendors.ResourceRef{}, false, notOwned(KindFirewalls, spec.Name)
	}
	return d.ref(KindFirewalls, region, f.ID, f.Name, f.Labels, f.Created), created, nil
}

func (d *Driver) ensurePlacementGroup(
	ctx context.Context, region string, spec vendors.CreateSpec, labels map[string]string,
) (vendors.ResourceRef, bool, error) {
	params := PlacementGroupParams{Type: string(hc.PlacementGroupTypeSpread)}
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings,
				fmt.Errorf("the placement group parameters: %w", err))
		}
	}
	find := func() (*hc.PlacementGroup, error) {
		list, err := d.api.PlacementGroups(ctx, hc.PlacementGroupListOpts{Name: spec.Name})
		if err != nil {
			return nil, classify(err)
		}
		for _, g := range list {
			if g.Name == spec.Name {
				return g, nil
			}
		}
		return nil, nil
	}
	g, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if g == nil {
		g, err = d.api.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{
			Name: spec.Name, Labels: labels, Type: hc.PlacementGroupType(params.Type),
		})
		switch {
		case err == nil:
			created = true
		case isCode(err, hc.ErrorCodeUniquenessError):
			if g, err = find(); err != nil || g == nil {
				return vendors.ResourceRef{}, false, vendors.NewError(vendors.Retryable, CodeNameInUse,
					fmt.Errorf("the placement group %s vanished: %v", spec.Name, err))
			}
		default:
			return vendors.ResourceRef{}, false, classify(err)
		}
	}
	if !d.owned(g.Labels) {
		return vendors.ResourceRef{}, false, notOwned(KindPlacementGroups, spec.Name)
	}
	return d.ref(KindPlacementGroups, region, g.ID, g.Name, g.Labels, g.Created), created, nil
}

func (d *Driver) ensureVolume(ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var params VolumeParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings,
				fmt.Errorf("the volume parameters: %w", err))
		}
	}
	if params.SizeGB == 0 {
		params.SizeGB = MinVolumeGB
	}
	opts, err := BuildVolumeOpts(VolumeSpec{
		Name: spec.Name, Location: region, SizeGB: params.SizeGB, Tags: spec.Tags, ExtraTags: spec.ExtraTags,
	})
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	opts.Labels[LabelLocation] = LabelValue(region)
	find := func() (*hc.Volume, error) {
		list, err := d.api.Volumes(ctx, hc.VolumeListOpts{Name: opts.Name})
		if err != nil {
			return nil, classify(err)
		}
		for _, v := range list {
			if v.Name == opts.Name {
				return v, nil
			}
		}
		return nil, nil
	}
	v, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if v == nil {
		v, err = d.api.CreateVolume(ctx, opts)
		switch {
		case err == nil:
			created = true
		case isCode(err, hc.ErrorCodeUniquenessError):
			if v, err = find(); err != nil || v == nil {
				return vendors.ResourceRef{}, false, vendors.NewError(vendors.Retryable, CodeNameInUse,
					fmt.Errorf("the volume %s vanished: %v", opts.Name, err))
			}
		default:
			return vendors.ResourceRef{}, false, classify(err)
		}
	}
	if !d.owned(v.Labels) {
		return vendors.ResourceRef{}, false, notOwned(KindVolumes, opts.Name)
	}
	return d.ref(KindVolumes, region, v.ID, v.Name, v.Labels, v.Created), created, nil
}

// InUse implements vendors.ManagedResources: from the resource's own record of
// what uses it (a firewall's applied_to, a placement group's servers, a volume's
// server). The time of the last use is not recorded, so the engine counts from
// its first sighting.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if err := d.ready(); err != nil {
		return false, time.Time{}, err
	}
	id, err := parseID(ref.ID)
	if err != nil {
		return false, time.Time{}, err
	}
	var used bool
	switch ref.Kind {
	case KindFirewalls:
		f, err := d.api.GetFirewall(ctx, id)
		if err != nil {
			return false, time.Time{}, classify(err)
		}
		if f == nil {
			return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the firewall %s does not exist", ref.ID)
		}
		used = len(f.AppliedTo) > 0
	case KindPlacementGroups:
		g, err := d.api.GetPlacementGroup(ctx, id)
		if err != nil {
			return false, time.Time{}, classify(err)
		}
		if g == nil {
			return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the placement group %s does not exist", ref.ID)
		}
		used = len(g.Servers) > 0
	case KindVolumes:
		v, err := d.api.GetVolume(ctx, id)
		if err != nil {
			return false, time.Time{}, classify(err)
		}
		if v == nil {
			return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the volume %s does not exist", ref.ID)
		}
		used = v.Server != nil
	case KindNetworks:
		// A brought network is never ours to delete; whether servers are attached is
		// not something SCarpenter needs to know.
		return true, d.clock.Now(), nil
	default:
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the Hetzner driver has no resource kind %q", ref.Kind)
	}
	if used {
		return true, d.clock.Now(), nil
	}
	return false, time.Time{}, nil
}

// Delete implements vendors.ManagedResources. It deletes only what carries this
// cluster's ownership marker; a resource that is still in use is Retryable (the
// dependency is on its way out), one that is gone is success.
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	if err := d.ready(); err != nil {
		return err
	}
	if !isManagedKind(ref.Kind) {
		return vendors.Errorf(vendors.Fatal, "%s %s was not created by SCarpenter and is never deleted", ref.Kind, ref.ID)
	}
	id, err := parseID(ref.ID)
	if err != nil {
		return err
	}
	refuse := func() error {
		return vendors.NewError(vendors.Fatal, codeNotOwned, fmt.Errorf(
			"the %s %s does not carry this cluster's marker; refusing to delete it", ref.Kind, ref.ID))
	}
	var derr error
	switch ref.Kind {
	case KindFirewalls:
		f, err := d.api.GetFirewall(ctx, id)
		if err != nil || f == nil {
			return notFoundOK(err)
		}
		if !d.owned(f.Labels) {
			return refuse()
		}
		derr = d.api.DeleteFirewall(ctx, f)
	case KindPlacementGroups:
		g, err := d.api.GetPlacementGroup(ctx, id)
		if err != nil || g == nil {
			return notFoundOK(err)
		}
		if !d.owned(g.Labels) {
			return refuse()
		}
		derr = d.api.DeletePlacementGroup(ctx, g)
	case KindVolumes:
		v, err := d.api.GetVolume(ctx, id)
		if err != nil || v == nil {
			return notFoundOK(err)
		}
		if !d.owned(v.Labels) {
			return refuse()
		}
		derr = d.api.DeleteVolume(ctx, v)
	}
	if derr != nil && !isCode(derr, hc.ErrorCodeNotFound) {
		return classify(derr)
	}
	return nil
}

// notFoundOK is the result of a lookup before a delete that found nothing: a
// resource that is gone is a successful delete.
func notFoundOK(err error) error {
	if err == nil || isCode(err, hc.ErrorCodeNotFound) {
		return nil
	}
	return classify(err)
}

// List implements vendors.ManagedResources: the managed firewalls, placement
// groups and volumes with the tags, one list per kind for all locations.
func (d *Driver) List(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	opts := hc.ListOpts{LabelSelector: Selector(tags)}
	keep := func(labels map[string]string, region string) bool {
		if labels[TagKey(vendors.TagManagedBy)] != vendors.ManagedByValue {
			return false
		}
		return len(regions) == 0 || slices.Contains(regions, region)
	}
	var out []vendors.ResourceRef
	fws, err := d.api.Firewalls(ctx, hc.FirewallListOpts{ListOpts: opts})
	if err != nil {
		return nil, classify(err)
	}
	for _, f := range fws {
		if region := f.Labels[LabelLocation]; keep(f.Labels, region) {
			out = append(out, d.ref(KindFirewalls, region, f.ID, f.Name, f.Labels, f.Created))
		}
	}
	pgs, err := d.api.PlacementGroups(ctx, hc.PlacementGroupListOpts{ListOpts: opts})
	if err != nil {
		return nil, classify(err)
	}
	for _, g := range pgs {
		if region := g.Labels[LabelLocation]; keep(g.Labels, region) {
			out = append(out, d.ref(KindPlacementGroups, region, g.ID, g.Name, g.Labels, g.Created))
		}
	}
	vols, err := d.api.Volumes(ctx, hc.VolumeListOpts{ListOpts: opts})
	if err != nil {
		return nil, classify(err)
	}
	for _, v := range vols {
		region := v.Labels[LabelLocation]
		if v.Location != nil {
			region = v.Location.Name
		}
		if keep(v.Labels, region) {
			out = append(out, d.ref(KindVolumes, region, v.ID, v.Name, v.Labels, v.Created))
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+"/"+a.ID, string(b.Kind)+"/"+b.ID)
	})
	return out, nil
}
