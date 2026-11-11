/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// ruleAccept is the action of an accepting ruleset rule.
const ruleAccept = "accept"

// LabelRegion records the zone of a managed resource that is account-wide at
// UpCloud (rulesets and server groups): List and the sweeper attribute them to it.
const LabelRegion = "scarpenter-region"

// Kinds implements vendors.ManagedResources. The deletion order is server groups
// first (nothing depends on them but servers), then the rulesets, which must be
// detached from the servers, then the networks, which need their servers gone.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindServerGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindFirewalls, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
		{Kind: KindNetworks, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	return kind == KindNetworks || kind == KindFirewalls || kind == KindServerGroups
}

// resourceLabels are the labels of a managed resource: the NodeClass's own tags,
// then the ownership marker and the zone, which win.
func (d *Driver) resourceLabels(spec vendors.CreateSpec, region string) []upcloud.Label {
	m := map[string]string{}
	maps.Copy(m, spec.ExtraTags)
	for k, v := range spec.Tags {
		m[TagKey(k)] = v
	}
	m[LabelRegion] = region
	out := make([]upcloud.Label, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, upcloud.Label{Key: k, Value: m[k]})
	}
	return out
}

// markerTags are the logical tags a managed resource carries, read from labels.
func markerTags(labels []upcloud.Label) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, l := range labels {
		for _, k := range vendors.ResourceTagKeys {
			if l.Key == TagKey(k) {
				out[k] = l.Value
			}
		}
	}
	return out
}

// owned says whether the tags carry the ownership marker of the cluster: cluster
// and managed-by (the NodeClass and kind tags are descriptive).
func owned(tags map[vendors.TagKey]string, cluster string) bool {
	return tags[vendors.TagManagedBy] == vendors.ManagedByValue && tags[vendors.TagCluster] == cluster && cluster != ""
}

func (d *Driver) ref(
	kind vendors.ResourceKind, region, id, name string, labels []upcloud.Label, managed bool,
) vendors.ResourceRef {
	r := vendors.ResourceRef{Kind: kind, Region: region, ID: id, Name: name, Managed: managed}
	if managed {
		r.Tags = markerTags(labels)
	}
	if kind == KindNetworks {
		r.Network = id
	}
	return r
}

// Resolve implements vendors.ManagedResources: existing resources by IDs, name
// pattern, tags or match, never modified. A zone's networks are the networks of
// that zone only. An empty reference selects nothing: UpCloud has no default
// network, ruleset or group.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the UpCloud driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: UpCloud has no default %s", kind)
	}
	caps := d.Selectors()[kind]
	switch kind {
	case KindNetworks:
		nets, err := d.listNetworks(ctx, labelQueryFilters(ref.Tags)...)
		if err != nil {
			return nil, err
		}
		types := networkTypes(ref.Filters)
		nets = slices.DeleteFunc(nets, func(n upcloud.Network) bool {
			return n.Zone != region || !slices.ContainsFunc(types, func(t string) bool { return selector.Glob(t, n.Type) })
		})
		res, err := selector.Apply(ref, caps, nets, networkFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, n := range res.Items {
			out = append(out, d.ref(kind, region, n.UUID, n.Name, n.Labels, false))
		}
		return out, nil
	case KindFirewalls:
		sets, err := d.listRulesets(ctx)
		if err != nil {
			return nil, err
		}
		res, err := selector.Apply(ref, caps, sets, rulesetFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		out := make([]vendors.ResourceRef, 0, len(res.Items))
		for _, r := range res.Items {
			out = append(out, d.ref(kind, region, r.UUID, r.Name, r.Labels, false))
		}
		return out, nil
	}
	groups, err := d.listGroups(ctx, labelQueryFilters(ref.Tags)...)
	if err != nil {
		return nil, err
	}
	res, err := selector.Apply(ref, caps, groups, groupFields)
	d.noteMissing(kind, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, d.ref(kind, region, g.UUID, g.Title, g.Labels, false))
	}
	return out, nil
}

func (d *Driver) listNetworks(ctx context.Context, fs ...request.QueryFilter) ([]upcloud.Network, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	out, err := d.api.GetNetworks(cctx, fs...)
	if err != nil {
		return nil, Classify(err)
	}
	return out.Networks, nil
}

func (d *Driver) listGroups(ctx context.Context, fs ...request.QueryFilter) (upcloud.ServerGroups, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	out, err := d.api.GetServerGroups(cctx, &request.GetServerGroupsRequest{Filters: fs})
	if err != nil {
		return nil, Classify(err)
	}
	return out, nil
}

func (d *Driver) listRulesets(ctx context.Context) ([]api.Ruleset, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	out, err := d.api.GetFirewallRulesets(cctx)
	if err != nil {
		return nil, Classify(err)
	}
	return out, nil
}

// GetNetwork reads a network, NotFound (classified) when it is gone.
func GetNetwork(ctx context.Context, a api.API, uuid string) (*upcloud.Network, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	n, err := a.GetNetworkDetails(cctx, &request.GetNetworkDetailsRequest{UUID: uuid})
	return n, Classify(err)
}

// markerFilters are the label filters of the ownership marker of tags.
func (d *Driver) markerFilters(tags map[vendors.TagKey]string) []request.QueryFilter {
	return labelQueryFilters(d.tagLabels(tags))
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the creating call. created is false when an
// existing resource was adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the UpCloud driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	switch kind {
	case KindNetworks:
		return d.ensureNetwork(ctx, region, spec)
	case KindFirewalls:
		return d.ensureRuleset(ctx, region, spec)
	}
	return d.ensureGroup(ctx, region, spec)
}

// ensureNetwork finds the network by name and marker in the zone or creates it: an
// IPv4 /24 with DHCP, no default route and no router, inside the managed range and
// clear of every network of the account (networks of an account must not overlap).
func (d *Driver) ensureNetwork(ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	all, err := d.listNetworks(ctx)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	var taken []netip.Prefix
	for _, n := range all {
		if n.Zone == region && n.Name == spec.Name && owned(markerTags(n.Labels), spec.Tags[vendors.TagCluster]) {
			return d.ref(KindNetworks, region, n.UUID, n.Name, n.Labels, true), false, nil
		}
		for _, in := range n.IPNetworks {
			if p, err := netip.ParsePrefix(in.Address); err == nil {
				taken = append(taken, p)
			}
		}
	}
	cidr, err := pickNetwork(d.networkRange, spec.Name, taken)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, "NetworkRange", err)
	}
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	n, err := d.api.CreateNetwork(cctx, &request.CreateNetworkRequest{
		Name: spec.Name, Zone: region,
		IPNetworks: upcloud.IPNetworkSlice{{
			Address: cidr.String(), Family: upcloud.IPAddressFamilyIPv4, DHCP: upcloud.True, DHCPDefaultRoute: upcloud.False,
		}},
		Labels: d.resourceLabels(spec, region),
	})
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	return d.ref(KindNetworks, region, n.UUID, n.Name, n.Labels, true), true, nil
}

// checkRange validates the managed range: an IPv4 /16.
func checkRange(cidr string) error {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() || p.Bits() != 16 {
		return fmt.Errorf("%q is not an IPv4 /16", cidr)
	}
	return nil
}

// pickNetwork derives a /24 of the range from the name and moves on to the next
// one while it overlaps a taken network.
func pickNetwork(rng, name string, taken []netip.Prefix) (netip.Prefix, error) {
	base, err := netip.ParsePrefix(rng)
	if err != nil {
		return netip.Prefix{}, err
	}
	b := base.Masked().Addr().As4()
	sum := sha256.Sum256([]byte(name))
	start := int(binary.BigEndian.Uint16(sum[:2])) % 256
	for i := range 256 {
		c := netip.PrefixFrom(netip.AddrFrom4([4]byte{b[0], b[1], byte((start + i) % 256), 0}), 24)
		if !slices.ContainsFunc(taken, c.Overlaps) {
			return c, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("every /24 of %s overlaps a network of the account", rng)
}

// ensureRuleset finds the SDN ruleset by name or creates it: stateful, the
// NodeClass's inbound rules and, with isolation nodeclass, everything from the
// network's range; outbound is accepted. The list answer's labels are unverified:
// a ruleset whose answer has none is adopted by its deterministic name.
func (d *Driver) ensureRuleset(ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	sets, err := d.listRulesets(ctx)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	for _, r := range sets {
		if r.Name == spec.Name && d.rulesetOwned(r, spec.Tags[vendors.TagCluster]) {
			return d.ref(KindFirewalls, region, r.UUID, r.Name, r.Labels, true), false, nil
		}
	}
	var p FirewallParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	rules, err := rulesetRules(p)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	r, err := d.api.CreateFirewallRuleset(cctx, api.CreateRulesetRequest{
		Name: spec.Name, Description: "Managed by SCarpenter", Stateful: true, Labels: d.resourceLabels(spec, region),
		Rules: rules,
	})
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	return d.ref(KindFirewalls, region, r.UUID, spec.Name, d.resourceLabels(spec, region), true), true, nil
}

// rulesetOwned says whether a ruleset is ours: its labels carry the marker, or
// (the list answer carries no labels) its name has the cluster's prefix.
func (d *Driver) rulesetOwned(r api.Ruleset, cluster string) bool {
	if len(r.Labels) > 0 {
		return owned(markerTags(r.Labels), cluster)
	}
	return cluster != "" && strings.HasPrefix(r.Name, "scarpenter-")
}

// rulesetRules builds the rules of a managed ruleset.
func rulesetRules(p FirewallParams) ([]api.RulesetRule, error) {
	var rules []api.RulesetRule
	if p.CIDR != "" {
		if _, err := netip.ParsePrefix(p.CIDR); err != nil {
			return nil, fmt.Errorf("the network range %q: %w", p.CIDR, err)
		}
		rules = append(rules, api.RulesetRule{
			Action: ruleAccept, Direction: "in", Family: upcloud.IPAddressFamilyIPv4, CIDR: p.CIDR, Comment: "nodeclass",
		})
	}
	for _, in := range inboundOf(&common.Inbound{Rules: p.Inbound}) {
		fam := upcloud.IPAddressFamilyIPv4
		if in.CIDR != "" {
			pr, err := netip.ParsePrefix(in.CIDR)
			if err != nil {
				return nil, fmt.Errorf("inbound CIDR %q: %w", in.CIDR, err)
			}
			if pr.Addr().Is6() {
				fam = upcloud.IPAddressFamilyIPv6
			}
		}
		rules = append(rules, api.RulesetRule{
			Action: ruleAccept, Direction: "in", Family: fam, Protocol: in.Protocol, CIDR: in.CIDR,
			PortStart: in.PortStart, PortEnd: in.PortEnd, Comment: "inbound",
		})
	}
	rules = append(rules,
		api.RulesetRule{Action: "drop", Direction: "in", Comment: "default"},
		api.RulesetRule{Action: ruleAccept, Direction: "out", Comment: "outbound"},
	)
	return rules, nil
}

// ensureGroup finds the server group by title and marker or creates it with the
// policy of the parameters (strict by default).
func (d *Driver) ensureGroup(ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	groups, err := d.listGroups(ctx, d.markerFilters(map[vendors.TagKey]string{
		vendors.TagCluster: spec.Tags[vendors.TagCluster], vendors.TagManagedBy: vendors.ManagedByValue,
	})...)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	for _, g := range groups {
		if g.Title == spec.Name && owned(markerTags(g.Labels), spec.Tags[vendors.TagCluster]) {
			return d.ref(KindServerGroups, region, g.UUID, g.Title, g.Labels, true), false, nil
		}
	}
	var p ServerGroupParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	policy := upcloud.ServerGroupAntiAffinityPolicyStrict
	if p.AntiAffinity == "yes" {
		policy = upcloud.ServerGroupAntiAffinityPolicyBestEffort
	}
	labels := upcloud.LabelSlice(d.resourceLabels(spec, region))
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	g, err := d.api.CreateServerGroup(cctx, &request.CreateServerGroupRequest{
		Title: spec.Name, AntiAffinityPolicy: policy, Labels: &labels,
	})
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	return d.ref(KindServerGroups, region, g.UUID, spec.Name, labels, true), true, nil
}

// regionOf is the zone a managed resource belongs to: its zone label, else the
// zone its name ends in (scarpenter-<cluster>-<kind>-<region>[-hash]).
func regionOf(labels []upcloud.Label, name string, regions []string) string {
	for _, l := range labels {
		if l.Key == LabelRegion {
			return l.Value
		}
	}
	for _, r := range regions {
		if strings.Contains(name, "-"+r) {
			return r
		}
	}
	return ""
}

// List implements vendors.ManagedResources: the managed resources carrying all the
// tags in the zones (or every zone), of every kind.
func (d *Driver) List(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	all, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	known := make([]string, 0, len(all))
	for _, r := range all {
		known = append(known, r.Name)
	}
	want := func(region string) bool {
		return region != "" && (len(regions) == 0 || slices.Contains(regions, region))
	}
	hasTags := func(labels []upcloud.Label) bool {
		have := markerTags(labels)
		for k, v := range tags {
			if have[k] != v {
				return false
			}
		}
		return len(have) > 0
	}
	filters := d.markerFilters(tags)
	var out []vendors.ResourceRef
	nets, err := d.listNetworks(ctx, filters...)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		if want(n.Zone) && hasTags(n.Labels) {
			out = append(out, d.ref(KindNetworks, n.Zone, n.UUID, n.Name, n.Labels, true))
		}
	}
	sets, err := d.listRulesets(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range sets {
		if region := regionOf(r.Labels, r.Name, known); want(region) && hasTags(r.Labels) {
			out = append(out, d.ref(KindFirewalls, region, r.UUID, r.Name, r.Labels, true))
		}
	}
	groups, err := d.listGroups(ctx, filters...)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if region := regionOf(g.Labels, g.Title, known); want(region) && hasTags(g.Labels) {
			out = append(out, d.ref(KindServerGroups, region, g.UUID, g.Title, g.Labels, true))
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
	return out, nil
}

// InUse implements vendors.ManagedResources: a network is in use while a server
// has an interface on it, a server group while it has members, and a ruleset while
// a server of the cluster in the zone has a private interface (the API does not
// say which servers a ruleset is attached to, and the driver attaches it to every
// node with a private interface). When it is not in use the last use is unknown,
// so the zero time is returned and the garbage collector counts the idle time
// from its first sighting.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	switch ref.Kind {
	case KindNetworks:
		n, err := GetNetwork(ctx, d.api, ref.ID)
		if err != nil {
			return false, time.Time{}, err
		}
		return d.inUse(len(n.Servers) > 0)
	case KindServerGroups:
		cctx, cancel := CallContext(ctx, CallTimeout)
		defer cancel()
		g, err := d.api.GetServerGroup(cctx, &request.GetServerGroupRequest{UUID: ref.ID})
		if err != nil {
			return false, time.Time{}, Classify(err)
		}
		return d.inUse(len(g.Members) > 0)
	case KindFirewalls:
		cctx, cancel := CallContext(ctx, CallTimeout)
		_, err := d.api.GetFirewallRuleset(cctx, ref.ID)
		cancel()
		if err != nil {
			return false, time.Time{}, Classify(err)
		}
		servers, err := ServersByLabels(ctx, d.api, map[string]string{TagKey(vendors.TagCluster): d.cfg.ClusterName})
		if err != nil {
			return false, time.Time{}, err
		}
		for _, s := range servers {
			if s.Zone == ref.Region && slices.ContainsFunc(s.Networking.Interfaces, func(i upcloud.ServerInterface) bool {
				return i.Type == upcloud.NetworkTypePrivate
			}) {
				return d.inUse(true)
			}
		}
		return false, time.Time{}, nil
	}
	return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the UpCloud driver has no resource kind %q", ref.Kind)
}

func (d *Driver) inUse(used bool) (bool, time.Time, error) {
	if used {
		return true, d.clock.Now(), nil
	}
	return false, time.Time{}, nil
}

// Delete implements vendors.ManagedResources: only resources that carry the
// cluster's ownership marker are deleted (never one the user brought); a resource
// that is already gone is success; one that something still uses is Retryable
// (the API's dependency errors; a server group with members is refused here, the
// API deletes it and leaves the members).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	notOurs := func() error {
		return vendors.Errorf(vendors.Fatal, "the %s %s does not carry the marker of cluster %q and is never deleted",
			ref.Kind, ref.ID, d.cfg.ClusterName)
	}
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	switch ref.Kind {
	case KindNetworks:
		n, err := GetNetwork(ctx, d.api, ref.ID)
		if err != nil {
			if isGone(err) {
				return nil
			}
			return err
		}
		if !owned(markerTags(n.Labels), d.cfg.ClusterName) {
			return notOurs()
		}
		if err := d.api.DeleteNetwork(cctx, &request.DeleteNetworkRequest{UUID: ref.ID}); err != nil {
			return goneOrRetry(err)
		}
		return nil
	case KindFirewalls:
		r, err := d.api.GetFirewallRuleset(cctx, ref.ID)
		if err != nil {
			return goneOrRetry(err)
		}
		if !d.rulesetOwned(*r, d.cfg.ClusterName) {
			return notOurs()
		}
		if err := d.api.DeleteFirewallRuleset(cctx, ref.ID); err != nil {
			return goneOrRetry(err)
		}
		return nil
	case KindServerGroups:
		g, err := d.api.GetServerGroup(cctx, &request.GetServerGroupRequest{UUID: ref.ID})
		if err != nil {
			return goneOrRetry(err)
		}
		if !owned(markerTags(g.Labels), d.cfg.ClusterName) {
			return notOurs()
		}
		if len(g.Members) > 0 {
			return vendors.Errorf(vendors.Retryable, "the server group %s still has %d members", ref.ID, len(g.Members))
		}
		if err := d.api.DeleteServerGroup(cctx, &request.DeleteServerGroupRequest{UUID: ref.ID}); err != nil {
			return goneOrRetry(err)
		}
		return nil
	}
	return vendors.Errorf(vendors.Fatal, "the UpCloud driver has no resource kind %q", ref.Kind)
}

// goneOrRetry is the answer of a delete call: nothing to delete is success, a
// dependency is Retryable.
func goneOrRetry(err error) error {
	if isGone(err) || isGone(Classify(err)) {
		return nil
	}
	return classifyDelete(err)
}
