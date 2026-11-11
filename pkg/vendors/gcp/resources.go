/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The resource kinds of the GCP driver (plan 16, section 2). The first, second and
// fourth are keys of spec.vendors.gcp.resources. runFirewalls is not
// configurable: it is the rule of a run key (isolation: run), which lives as long
// as an instance of the run uses it.
const (
	// KindSubnets is the regional network: the project's default network and its
	// default subnetwork as found, or, where the project has none (or it cannot
	// honour the request) or the NodeClass says create, the cluster's managed
	// global VPC with one subnetwork per region (network.go). The network is
	// implied by the subnetwork.
	KindSubnets vendors.ResourceKind = "subnets"
	// KindFirewalls is the network tag of a NodeClass and the rules that target it.
	// A tag is not a resource: `use` names tags that rules of the project mention.
	KindFirewalls vendors.ResourceKind = "firewalls"
	// KindRunFirewalls is the network tag and rule of a run key.
	KindRunFirewalls vendors.ResourceKind = "runFirewalls"
	// KindResourcePolicies is a placement policy of a run key and profile.
	KindResourcePolicies vendors.ResourceKind = "resourcePolicies"
	// KindImages is the boot image: a kind that only accepts `use`, so it is not one
	// of Kinds and Resolve does not serve it; ResolveImage does.
	KindImages = selector.ImagesKind
)

// Kinds implements vendors.ManagedResources. The deletion order is policies and
// run rules first (nothing depends on them but instances), then the NodeClass
// rules, then the network, which needs its rules gone.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindResourcePolicies, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindRunFirewalls, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 25,
			InNetwork: KindSubnets},
		{Kind: KindFirewalls, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30,
			InNetwork: KindSubnets},
		{Kind: KindSubnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	switch kind {
	case KindSubnets, KindFirewalls, KindRunFirewalls, KindResourcePolicies:
		return true
	}
	return false
}

// ----- the marker of managed resources -----

// markerFor is the description of a managed resource: the marker of names.go with
// the region, which the garbage collector needs for the rules (global) of a
// per-region kind.
func markerFor(spec vendors.CreateSpec, kind vendors.ResourceKind, region string) string {
	return MarkerDescription(spec.Tags[vendors.TagCluster], spec.Tags[vendors.TagNodeClass], string(kind)) +
		" region=" + region
}

func markerRegion(desc string) string {
	for f := range strings.FieldsSeq(desc) {
		if v, ok := strings.CutPrefix(f, "region="); ok {
			return v
		}
	}
	return ""
}

// ownedBy says whether a resource carries the ownership marker of the cluster: the
// name prefix and the description, both (ADR-018's fallback).
func ownedBy(cluster, name, desc string) (nodeClass, kind string, ok bool) {
	if cluster == "" || !strings.HasPrefix(name, ResourceNamePrefix(cluster)) {
		return "", "", false
	}
	c, nc, k, parsed := ParseMarker(desc)
	if !parsed || c != cluster {
		return "", "", false
	}
	return nc, k, true
}

func (d *Driver) markerTags(nodeClass, kind string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: d.cfg.ClusterName, vendors.TagNodeClass: nodeClass,
		vendors.TagManagedBy: vendors.ManagedByValue, vendors.TagResource: kind,
	}
}

// shorten makes a name of at most 63 characters, keeping what it can and adding
// a hash of the whole when it has to cut.
func shorten(name string) string {
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:])[:8]
	return strings.TrimRight(name[:63-9], "-") + "-" + h
}

// ruleName is the name of a rule of a tag: the tag with a suffix.
func ruleName(tag, suffix string) string { return shorten(tag + "-" + suffix) }

// ----- resolving what the NodeClass brings -----

// Resolve implements vendors.ManagedResources: existing resources by IDs, name
// pattern or the other fields, never modified. An empty selector selects nothing:
// the project's default is Default's.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the GCP driver has no resource kind %q", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the vendor's default is Default's")
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	switch kind {
	case KindSubnets:
		return d.resolveSubnets(ctx, api, region, ref)
	case KindResourcePolicies:
		return d.resolvePolicies(ctx, api, region, ref)
	}
	return d.resolveFirewalls(ctx, api, region, ref)
}

func (d *Driver) subnetRef(region string, s *computepb.Subnetwork) vendors.ResourceRef {
	return vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: subnetID(s, d.project, region), Name: s.GetName(),
		Network: lastSegment(s.GetNetwork()), CreatedAt: parseTime(s.GetCreationTimestamp())}
}

// listSubnets reads every page of the region's subnetworks with the filter.
func (d *Driver) listSubnets(ctx context.Context, api ComputeAPI, region, filter string,
) ([]*computepb.Subnetwork, error) {
	req := &computepb.ListSubnetworksRequest{Project: d.project, Region: region}
	if filter != "" {
		req.Filter = &filter
	}
	var out []*computepb.Subnetwork
	for {
		res, err := api.ListSubnetworks(ctx, req)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.GetItems()...)
		if res.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = res.NextPageToken
	}
}

// subnetRegionOf returns the project and region of a subnetwork URL.
func subnetParts(url string) (project, region, name string, ok bool) {
	u, found := fromProjects(url)
	if !found {
		return "", "", "", false
	}
	p := strings.Split(u, "/")
	if len(p) != 6 || p[2] != "regions" || p[4] != "subnetworks" {
		return "", "", "", false
	}
	return p[1], p[3], p[5], true
}

func (d *Driver) resolveSubnets(ctx context.Context, api ComputeAPI, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	var subs []*computepb.Subnetwork
	if len(ref.IDs) > 0 {
		ids := make([]string, len(ref.IDs))
		for i, id := range ref.IDs {
			ids[i] = SubnetworkURL(d.project, region, id)
			p, r, n, ok := subnetParts(ids[i])
			if !ok {
				continue
			}
			s, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: r, Subnetwork: n})
			switch {
			case err == nil:
				subs = append(subs, s)
			case IsNotFound(err):
			default:
				return nil, classify(err, errContext{})
			}
		}
		ref.IDs = ids
	} else {
		var err error
		if subs, err = d.listSubnets(ctx, api, region, listFilter(ref)); err != nil {
			return nil, err
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[KindSubnets], subs, func(s *computepb.Subnetwork) selector.Fields {
		return subnetFields(s, d.project, region)
	})
	d.noteMissing(KindSubnets, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, d.subnetRef(region, s))
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *Driver) policyRef(region string, p *computepb.ResourcePolicy) vendors.ResourceRef {
	return vendors.ResourceRef{Kind: KindResourcePolicies, Region: region, ID: policyID(p, d.project, region),
		Name: p.GetName(), CreatedAt: parseTime(p.GetCreationTimestamp())}
}

func (d *Driver) listPolicies(ctx context.Context, api ComputeAPI, region, filter string,
) ([]*computepb.ResourcePolicy, error) {
	req := &computepb.ListResourcePoliciesRequest{Project: d.project, Region: region}
	if filter != "" {
		req.Filter = &filter
	}
	var out []*computepb.ResourcePolicy
	for {
		res, err := api.ListResourcePolicies(ctx, req)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.GetItems()...)
		if res.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = res.NextPageToken
	}
}

func (d *Driver) resolvePolicies(ctx context.Context, api ComputeAPI, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	var pols []*computepb.ResourcePolicy
	if len(ref.IDs) > 0 {
		ids := make([]string, len(ref.IDs))
		for i, id := range ref.IDs {
			name := id[strings.LastIndex(id, "/")+1:]
			ids[i] = ProjectPrefix(d.project) + "regions/" + region + "/resourcePolicies/" + name
			p, err := api.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{
				Project: d.project, Region: region, ResourcePolicy: name})
			switch {
			case err == nil:
				pols = append(pols, p)
			case IsNotFound(err):
			default:
				return nil, classify(err, errContext{})
			}
		}
		ref.IDs = ids
	} else {
		var err error
		if pols, err = d.listPolicies(ctx, api, region, listFilter(ref)); err != nil {
			return nil, err
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[KindResourcePolicies], pols,
		func(p *computepb.ResourcePolicy) selector.Fields { return policyFields(p, d.project, region) })
	d.noteMissing(KindResourcePolicies, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, p := range res.Items {
		out = append(out, d.policyRef(region, p))
	}
	return out, nil
}

// listFirewalls reads every page of the project's firewall rules with the filter.
func (d *Driver) listFirewalls(ctx context.Context, api ComputeAPI, filter string) ([]*computepb.Firewall, error) {
	req := &computepb.ListFirewallsRequest{Project: d.project}
	if filter != "" {
		req.Filter = &filter
	}
	var out []*computepb.Firewall
	for {
		res, err := api.ListFirewalls(ctx, req)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.GetItems()...)
		if res.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = res.NextPageToken
	}
}

// tagsOfRules groups the ingress rules by the network tags they target.
func tagsOfRules(rules []*computepb.Firewall) []fwTag {
	by := map[string]*fwTag{}
	for _, r := range rules {
		if r.GetDirection() == "EGRESS" {
			continue
		}
		for _, tag := range r.GetTargetTags() {
			t := by[tag]
			if t == nil {
				t = &fwTag{tag: tag}
				by[tag] = t
			}
			t.rules = append(t.rules, r)
		}
	}
	out := make([]fwTag, 0, len(by))
	for _, k := range sortedKeys(by) {
		out = append(out, *by[k])
	}
	return out
}

func (d *Driver) resolveFirewalls(ctx context.Context, api ComputeAPI, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	// The name of a firewall is a tag, found client-side: the filter is the
	// expression and the native filters only.
	pre := ref
	pre.Name = ""
	rules, err := d.listFirewalls(ctx, api, listFilter(pre))
	if err != nil {
		return nil, err
	}
	res, err := selector.Apply(ref, d.Selectors()[KindFirewalls], tagsOfRules(rules), firewallFields)
	d.noteMissing(KindFirewalls, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, t := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: KindFirewalls, Region: region, ID: t.tag, Name: t.tag,
			Network: lastSegment(t.rules[0].GetNetwork())})
	}
	return out, nil
}

// Default implements vendors.ResourceDefaults: the project's default network's
// subnetwork of the region (an auto-mode network has one per region), where the
// project has them. Firewalls and policies have no default: the default network's
// rules target no tag and cannot isolate runs, so SCarpenter creates its own.
func (d *Driver) Default(ctx context.Context, kind vendors.ResourceKind, region string) ([]vendors.ResourceRef, error) {
	if kind != KindSubnets {
		return nil, nil
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	_, err = api.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: d.project, Network: defaultNetwork})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, classify(err, errContext{})
	}
	s, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{
		Project: d.project, Region: region, Subnetwork: defaultNetwork})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, classify(err, errContext{})
	}
	if lastSegment(s.GetNetwork()) != defaultNetwork {
		return nil, nil
	}
	return []vendors.ResourceRef{d.subnetRef(region, s)}, nil
}

var (
	_ vendors.ResourceDefaults = (*Driver)(nil)
	_ vendors.DefaultChecker   = (*Driver)(nil)
)

// DefaultSuits implements vendors.DefaultChecker (ADR-017: the default never
// degrades the request). The default network cannot honour publicIPv6 (its
// subnetworks are IPv4 only), and its own firewall rules (internal traffic and
// SSH from anywhere, unless the project removed them) apply to every instance of
// the network: GCP rules only add allows, so isolation and the inbound rules
// cannot be honoured while such a rule exists. The default is used only when no
// enabled ingress rule of the network applies to every instance.
func (d *Driver) DefaultSuits(
	ctx context.Context, kind vendors.ResourceKind, region string, defaults []vendors.ResourceRef,
	s vendors.VendorSettings,
) (bool, string, error) {
	if kind != KindSubnets {
		return true, "", nil
	}
	if boolOr(s.Networking.PublicIPv6, false) {
		return false, "publicIPv6 needs a subnetwork with an external IPv6 range and the default network's have none", nil
	}
	api, err := d.compute(ctx)
	if err != nil {
		return false, "", err
	}
	rules, err := d.listFirewalls(ctx, api, "")
	if err != nil {
		return false, "", err
	}
	var open []string
	for _, r := range rules {
		if r.GetDisabled() || r.GetDirection() == "EGRESS" || lastSegment(r.GetNetwork()) != defaultNetwork {
			continue
		}
		if len(r.GetTargetTags()) == 0 && len(r.GetTargetServiceAccounts()) == 0 {
			open = append(open, r.GetName())
		}
	}
	if len(open) > 0 {
		slices.Sort(open)
		return false, fmt.Sprintf("the default network's firewall rules %s apply to every instance and allow traffic "+
			"that isolation and the inbound rules would restrict", strings.Join(open, ", ")), nil
	}
	return true, "", nil
}

// chooseSubnet picks the subnetwork of the launch among the resolved ones: the
// first that can honour publicIPv6 (a dual-stack subnetwork with an external
// range). The subnetworks are regional: the zone does not matter.
func (d *Driver) chooseSubnet(ctx context.Context, region string, refs []vendors.ResourceRef, wantIPv6 bool,
) (vendors.ResourceRef, error) {
	if len(refs) == 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no subnetwork in %s", region))
	}
	if !wantIPv6 {
		return refs[0], nil
	}
	api, err := d.compute(ctx)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	for _, r := range refs {
		p, rg, n, ok := subnetParts(r.ID)
		if !ok {
			continue
		}
		s, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: rg, Subnetwork: n})
		if err != nil {
			return vendors.ResourceRef{}, classify(err, errContext{})
		}
		if s.GetStackType() == "IPV4_IPV6" && s.GetIpv6AccessType() == "EXTERNAL" {
			return r, nil
		}
	}
	return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, fmt.Errorf(
		"publicIPv6 needs a subnetwork with stackType IPV4_IPV6 and an external IPv6 range, and none of the "+
			"subnetworks in %s has one", region))
}

// ----- ensuring managed resources -----

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the creating call. created is false when an
// existing resource was adopted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the GCP driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	api, err := d.compute(ctx)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	switch kind {
	case KindSubnets:
		return d.ensureSubnet(ctx, api, region, spec)
	case KindResourcePolicies:
		return d.ensurePolicy(ctx, api, region, spec)
	}
	return d.ensureFirewall(ctx, api, kind, region, spec)
}

func (d *Driver) ensureSubnet(ctx context.Context, api ComputeAPI, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var params SubnetParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	poolStr := params.Pool
	if poolStr == "" {
		poolStr = DefaultPool
	}
	pool, err := ParsePool(poolStr)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	name := shorten(spec.Name)
	network := d.managedNetworkName()
	created := false
	netw, err := api.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: d.project, Network: network})
	switch {
	case err == nil:
		if _, _, ok := ownedBy(d.cfg.ClusterName, network, netw.GetDescription()); !ok {
			return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
				"a network named %s exists and does not carry the SCarpenter marker of cluster %q", network, d.cfg.ClusterName)
		}
	case IsNotFound(err):
		desc := networkMarker(spec)
		op, err := api.InsertNetwork(ctx, &computepb.InsertNetworkRequest{Project: d.project,
			NetworkResource: &computepb.Network{Name: &network, Description: &desc, AutoCreateSubnetworks: new(false),
				RoutingConfig: &computepb.NetworkRoutingConfig{RoutingMode: new("REGIONAL")}}})
		if err != nil && !IsAlreadyExists(err) {
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
		if err == nil {
			if err := d.finish(ctx, api, op); err != nil {
				return vendors.ResourceRef{}, false, err
			}
		}
		created = true
	default:
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	// An existing subnetwork of the name is the one this call, or an earlier run of
	// the controller, created: its range stays, whatever the pool says now.
	sub, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{
		Project: d.project, Region: region, Subnetwork: name})
	switch {
	case err == nil:
		if _, _, ok := ownedBy(d.cfg.ClusterName, name, sub.GetDescription()); !ok {
			return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
				"a subnetwork named %s exists in %s and does not carry the SCarpenter marker of cluster %q", name, region,
				d.cfg.ClusterName)
		}
	case IsNotFound(err):
		if err := d.insertSubnet(ctx, api, region, name, network, markerFor(spec, KindSubnets, region), pool); err != nil {
			return vendors.ResourceRef{}, false, err
		}
		created = true
		if sub, err = api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{
			Project: d.project, Region: region, Subnetwork: name}); err != nil {
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
	default:
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	ref := d.subnetRef(region, sub)
	ref.Managed, ref.Tags = true, d.markerTags(spec.Tags[vendors.TagNodeClass], string(KindSubnets))
	return ref, created, nil
}

// policyResource builds the placement policy of the parameters.
func policyResource(name, desc string, p PolicyParams) *computepb.ResourcePolicy {
	r := &computepb.ResourcePolicy{Name: &name, Description: &desc}
	if p.Collocated {
		r.GroupPlacementPolicy = &computepb.ResourcePolicyGroupPlacementPolicy{Collocation: new("COLLOCATED")}
	} else {
		r.GroupPlacementPolicy = &computepb.ResourcePolicyGroupPlacementPolicy{
			AvailabilityDomainCount: new(p.AvailabilityDomainCount)}
	}
	return r
}

func (d *Driver) ensurePolicy(ctx context.Context, api ComputeAPI, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var params PolicyParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	if !params.Collocated && params.AvailabilityDomainCount < 1 {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
			fmt.Errorf("a placement policy needs collocated or availabilityDomainCount"))
	}
	name := shorten(spec.Name)
	created := false
	p, err := api.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: d.project, Region: region,
		ResourcePolicy: name})
	switch {
	case err == nil:
		if _, _, ok := ownedBy(d.cfg.ClusterName, name, p.GetDescription()); !ok {
			return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
				"a resource policy named %s exists and does not carry the SCarpenter marker of cluster %q", name,
				d.cfg.ClusterName)
		}
	case IsNotFound(err):
		op, err := api.InsertResourcePolicy(ctx, &computepb.InsertResourcePolicyRequest{Project: d.project, Region: region,
			ResourcePolicyResource: policyResource(name, markerFor(spec, KindResourcePolicies, region), params)})
		if err != nil && !IsAlreadyExists(err) {
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
		if err == nil {
			if err := d.finish(ctx, api, op); err != nil {
				return vendors.ResourceRef{}, false, err
			}
		}
		created = true
		if p, err = api.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: d.project, Region: region,
			ResourcePolicy: name}); err != nil {
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
	default:
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	ref := d.policyRef(region, p)
	ref.Managed, ref.Tags = true, d.markerTags(spec.Tags[vendors.TagNodeClass], string(KindResourcePolicies))
	return ref, created, nil
}

// firewallRule is one rule of a tag, with its name.
type firewallRule struct {
	name string
	rule *computepb.Firewall
}

// rulesFor builds the rules of a firewall kind: for the NodeClass's tag one rule
// per inbound rule, plus one that lets the nodes of the tag reach each other when
// asked; for the run tag the self rule only.
func rulesFor(kind vendors.ResourceKind, tag, network, desc string, p FirewallParams) []firewallRule {
	base := func(name string) *computepb.Firewall {
		return &computepb.Firewall{
			Name: &name, Description: &desc, Network: new(network), Direction: new("INGRESS"),
			Priority: new(int32(1000)), TargetTags: []string{tag},
		}
	}
	var out []firewallRule
	if p.SelfReference || kind == KindRunFirewalls {
		n := ruleName(tag, "self")
		r := base(n)
		r.SourceTags = []string{tag}
		r.Allowed = []*computepb.Allowed{{IPProtocol: new("all")}}
		out = append(out, firewallRule{n, r})
	}
	if kind == KindRunFirewalls {
		return out
	}
	// The deny rule says what the implied one does (ingress is denied unless a rule
	// allows it): the NodeClass's firewall is never an empty object, so it can be
	// found, held and deleted, and the intent shows in the console. The lowest
	// priority lets every allow rule win.
	deny := ruleName(tag, "deny")
	dr := base(deny)
	dr.Priority = new(int32(65534))
	dr.SourceRanges = []string{"0.0.0.0/0", "::/0"}
	dr.Denied = []*computepb.Denied{{IPProtocol: new("all")}}
	out = append(out, firewallRule{deny, dr})
	for i, in := range p.Inbound {
		n := ruleName(tag, "in"+strconv.Itoa(i))
		r := base(n)
		r.SourceRanges = slices.Clone(in.CIDRs)
		al := &computepb.Allowed{IPProtocol: new(protocolName(in.Protocol))}
		if in.FromPort != nil {
			to := *in.FromPort
			if in.ToPort != nil {
				to = *in.ToPort
			}
			port := strconv.Itoa(int(*in.FromPort))
			if to != *in.FromPort {
				port += "-" + strconv.Itoa(int(to))
			}
			al.Ports = []string{port}
		}
		r.Allowed = []*computepb.Allowed{al}
		out = append(out, firewallRule{n, r})
	}
	return out
}

func protocolName(p common.Protocol) string {
	if p == common.Protocol("any") {
		return "all"
	}
	return string(p)
}

func (d *Driver) networkURL(network string) string {
	if u, ok := fromProjects(network); ok {
		return u
	}
	return ProjectPrefix(d.project) + "global/networks/" + lastSegment(network)
}

// ownedRulesOf lists the rules of the cluster that target the tag.
func (d *Driver) ownedRulesOf(ctx context.Context, api ComputeAPI, tag string) ([]*computepb.Firewall, error) {
	rules, err := d.listFirewalls(ctx, api, "")
	if err != nil {
		return nil, err
	}
	var out []*computepb.Firewall
	for _, r := range rules {
		_, _, ok := ownedBy(d.cfg.ClusterName, r.GetName(), r.GetDescription())
		if ok && slices.Contains(r.GetTargetTags(), tag) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *Driver) ensureFirewall(ctx context.Context, api ComputeAPI, kind vendors.ResourceKind, region string,
	spec vendors.CreateSpec) (vendors.ResourceRef, bool, error) {
	var params FirewallParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &params); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	if params.Network == "" {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
			fmt.Errorf("the firewall needs the network of its subnetwork"))
	}
	tag := shorten(spec.Name)
	existing, err := d.ownedRulesOf(ctx, api, tag)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[r.GetName()] = true
	}
	want := rulesFor(kind, tag, d.networkURL(params.Network), markerFor(spec, kind, region), params)
	created := false
	for _, fr := range want {
		if have[fr.name] {
			continue
		}
		op, err := api.InsertFirewall(ctx, &computepb.InsertFirewallRequest{Project: d.project, FirewallResource: fr.rule})
		if err != nil {
			if IsAlreadyExists(err) {
				// A rule of this name that is not ours (it did not list as owned).
				return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
					"a firewall rule named %s exists and does not carry the SCarpenter marker of cluster %q", fr.name,
					d.cfg.ClusterName)
			}
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
		if err := d.finish(ctx, api, op); err != nil {
			return vendors.ResourceRef{}, false, err
		}
		created = true
	}
	ref := vendors.ResourceRef{Kind: kind, Region: region, ID: tag, Name: tag, Managed: true,
		Network: lastSegment(params.Network), Tags: d.markerTags(spec.Tags[vendors.TagNodeClass], string(kind))}
	return ref, created || len(existing) == 0, nil
}

// ----- listing, use and deletion of managed resources -----

// regionsOf returns the regions to list: the given ones the project can use, or
// every region of the project.
func (d *Driver) regionsOf(ctx context.Context, regions []string) ([]string, error) {
	all, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var usable []string
	for _, r := range all {
		if len(regions) == 0 || slices.Contains(regions, r.Name) {
			usable = append(usable, r.Name)
		}
	}
	return usable, nil
}

// matches says whether a marker carries the tags (the ownership marker).
func (d *Driver) markerMatches(nodeClass, kind string, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		switch k {
		case vendors.TagCluster:
			if v != d.cfg.ClusterName {
				return false
			}
		case vendors.TagNodeClass:
			if v != nodeClass {
				return false
			}
		case vendors.TagManagedBy:
			if v != vendors.ManagedByValue {
				return false
			}
		case vendors.TagResource:
			if v != kind {
				return false
			}
		}
	}
	return true
}

// List implements vendors.ManagedResources: the managed resources carrying all the
// tags (name prefix and description marker both), top-level resources only: the
// subnetwork of a managed network stands for its VPC, and a VPC whose subnetwork
// is gone is listed as the network so that it is not leaked.
func (d *Driver) List(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	if d.cfg.ClusterName == "" {
		return nil, nil
	}
	usable, err := d.regionsOf(ctx, regions)
	if err != nil {
		return nil, err
	}
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, region := range usable {
		subs, err := d.listSubnets(ctx, api, region, "")
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", region, err)
		}
		for _, s := range subs {
			nc, _, ok := ownedBy(d.cfg.ClusterName, s.GetName(), s.GetDescription())
			if !ok {
				continue
			}
			if !d.markerMatches(nc, string(KindSubnets), tags) {
				continue
			}
			ref := d.subnetRef(region, s)
			ref.Managed, ref.Tags = true, d.markerTags(nc, string(KindSubnets))
			out = append(out, ref)
		}
		pols, err := d.listPolicies(ctx, api, region, "")
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", region, err)
		}
		for _, p := range pols {
			nc, _, ok := ownedBy(d.cfg.ClusterName, p.GetName(), p.GetDescription())
			if !ok || !d.markerMatches(nc, string(KindResourcePolicies), tags) {
				continue
			}
			ref := d.policyRef(region, p)
			ref.Managed, ref.Tags = true, d.markerTags(nc, string(KindResourcePolicies))
			out = append(out, ref)
		}
	}
	orphans, err := d.listOrphanNetworks(ctx, api, usable, tags)
	if err != nil {
		return nil, err
	}
	out = append(out, orphans...)
	rules, err := d.listManagedFirewalls(ctx, api, usable, tags)
	if err != nil {
		return nil, err
	}
	out = append(out, rules...)
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
	return out, nil
}

// listOrphanNetworks lists the managed networks that have no subnetwork in any
// region (an interrupted creation, or every subnetwork deleted) as subnets
// resources, so that they are held and deleted and not leaked. The network is
// global: it is listed in the first region asked for.
func (d *Driver) listOrphanNetworks(ctx context.Context, api ComputeAPI, usable []string,
	tags map[vendors.TagKey]string) ([]vendors.ResourceRef, error) {
	if len(usable) == 0 {
		return nil, nil
	}
	nets, err := d.listNetworks(ctx, api)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, n := range nets {
		nc, _, ok := ownedBy(d.cfg.ClusterName, n.GetName(), n.GetDescription())
		if !ok || len(n.GetSubnetworks()) > 0 || !d.markerMatches(nc, string(KindSubnets), tags) {
			continue
		}
		out = append(out, vendors.ResourceRef{Kind: KindSubnets, Region: usable[0], ID: networkID(d.project, n.GetName()),
			Name: n.GetName(), Network: n.GetName(), Managed: true, CreatedAt: parseTime(n.GetCreationTimestamp()),
			Tags: d.markerTags(nc, string(KindSubnets))})
	}
	return out, nil
}

// listManagedFirewalls groups the managed rules by their network tag: a tag with
// its rules is one firewall resource.
func (d *Driver) listManagedFirewalls(ctx context.Context, api ComputeAPI, usable []string,
	tags map[vendors.TagKey]string) ([]vendors.ResourceRef, error) {
	rules, err := d.listFirewalls(ctx, api, "")
	if err != nil {
		return nil, err
	}
	byTag := map[string]*vendors.ResourceRef{}
	for _, r := range rules {
		nc, kind, ok := ownedBy(d.cfg.ClusterName, r.GetName(), r.GetDescription())
		region := markerRegion(r.GetDescription())
		if !ok || len(r.GetTargetTags()) == 0 || !slices.Contains(usable, region) {
			continue
		}
		if !d.markerMatches(nc, kind, tags) || (kind != string(KindFirewalls) && kind != string(KindRunFirewalls)) {
			continue
		}
		tag := r.GetTargetTags()[0]
		ref := byTag[tag]
		if ref == nil {
			ref = &vendors.ResourceRef{Kind: vendors.ResourceKind(kind), Region: region, ID: tag, Name: tag, Managed: true,
				Network: lastSegment(r.GetNetwork()), Tags: d.markerTags(nc, kind)}
			byTag[tag] = ref
		}
		if t := parseTime(r.GetCreationTimestamp()); ref.CreatedAt.IsZero() || t.Before(ref.CreatedAt) {
			ref.CreatedAt = t
		}
	}
	out := make([]vendors.ResourceRef, 0, len(byTag))
	for _, k := range sortedKeys(byTag) {
		out = append(out, *byTag[k])
	}
	return out, nil
}

func networkID(project, name string) string {
	return ProjectPrefix(project) + "global/networks/" + name
}

func (d *Driver) listNetworks(ctx context.Context, api ComputeAPI) ([]*computepb.Network, error) {
	req := &computepb.ListNetworksRequest{Project: d.project}
	var out []*computepb.Network
	for {
		res, err := api.ListNetworks(ctx, req)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.GetItems()...)
		if res.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = res.NextPageToken
	}
}

// InUse implements vendors.ManagedResources: whether an instance of the cluster
// uses the resource. When it is not in use the last use is unknown (Compute does
// not say), so the zero time is returned and the garbage collector counts the idle
// time from its first sighting.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if !isKind(ref.Kind) {
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the GCP driver has no resource kind %q", ref.Kind)
	}
	api, err := d.compute(ctx)
	if err != nil {
		return false, time.Time{}, err
	}
	exists, err := d.resourceExists(ctx, api, ref)
	if err != nil {
		return false, time.Time{}, err
	}
	if !exists {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the %s %s does not exist", ref.Kind, ref.ID)
	}
	var filter string
	if d.cfg.ClusterName != "" {
		filter = LabelFilter(map[vendors.TagKey]string{vendors.TagCluster: d.cfg.ClusterName})
	}
	insts, err := d.aggregated(ctx, api, filter)
	if err != nil {
		return false, time.Time{}, err
	}
	for _, in := range insts {
		if RegionOfZone(zoneOf(in)) != ref.Region || in.GetStatus() == StatusTerminated {
			continue
		}
		switch ref.Kind {
		case KindSubnets:
			for _, nic := range in.GetNetworkInterfaces() {
				if lastSegment(nic.GetSubnetwork()) == ref.Name || nic.GetSubnetwork() == ref.ID {
					return true, d.clock.Now(), nil
				}
			}
		case KindFirewalls, KindRunFirewalls:
			if slices.Contains(in.GetTags().GetItems(), ref.ID) {
				return true, d.clock.Now(), nil
			}
		case KindResourcePolicies:
			for _, p := range in.GetResourcePolicies() {
				if lastSegment(p) == ref.Name {
					return true, d.clock.Now(), nil
				}
			}
		}
	}
	return false, time.Time{}, nil
}

// resourceExists says whether the resource still exists.
func (d *Driver) resourceExists(ctx context.Context, api ComputeAPI, ref vendors.ResourceRef) (bool, error) {
	var err error
	switch ref.Kind {
	case KindSubnets:
		if strings.Contains(ref.ID, "/global/networks/") {
			_, err = api.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: d.project, Network: ref.Name})
		} else {
			_, err = api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: d.project, Region: ref.Region,
				Subnetwork: ref.Name})
		}
	case KindResourcePolicies:
		_, err = api.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: d.project, Region: ref.Region,
			ResourcePolicy: ref.Name})
	default:
		rules, lerr := d.ownedRulesOf(ctx, api, ref.ID)
		return len(rules) > 0, lerr
	}
	switch {
	case err == nil:
		return true, nil
	case IsNotFound(err):
		return false, nil
	}
	return false, classify(err, errContext{})
}

// Delete implements vendors.ManagedResources: only resources that carry the
// cluster's ownership marker are deleted (never one the user brought); a resource
// that is already gone is success; one that something still uses is Retryable
// (Compute's resourceInUseByAnotherResource).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	api, err := d.compute(ctx)
	if err != nil {
		return err
	}
	switch ref.Kind {
	case KindSubnets:
		return d.deleteSubnet(ctx, api, ref)
	case KindResourcePolicies:
		p, err := api.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: d.project, Region: ref.Region,
			ResourcePolicy: ref.Name})
		if err != nil {
			if IsNotFound(err) {
				return nil
			}
			return classify(err, errContext{})
		}
		if _, _, ok := ownedBy(d.cfg.ClusterName, p.GetName(), p.GetDescription()); !ok {
			return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.Name)
		}
		return d.deleteOp(ctx, api, func() (*computepb.Operation, error) {
			return api.DeleteResourcePolicy(ctx, &computepb.DeleteResourcePolicyRequest{Project: d.project,
				Region: ref.Region, ResourcePolicy: ref.Name})
		})
	case KindFirewalls, KindRunFirewalls:
		rules, err := d.ownedRulesOf(ctx, api, ref.ID)
		if err != nil {
			return err
		}
		if len(rules) == 0 {
			return nil
		}
		// Compute deletes a rule under running instances without a word, which would
		// cut their network: the driver refuses what the cloud does not.
		if inUse, _, err := d.InUse(ctx, ref); err != nil {
			return err
		} else if inUse {
			return vendors.Errorf(vendors.Retryable, "the network tag %s is still used by an instance", ref.ID)
		}
		for _, r := range rules {
			if err := d.deleteOp(ctx, api, func() (*computepb.Operation, error) {
				return api.DeleteFirewall(ctx, &computepb.DeleteFirewallRequest{Project: d.project, Firewall: r.GetName()})
			}); err != nil {
				return err
			}
		}
		return nil
	}
	return vendors.Errorf(vendors.Fatal, "the GCP driver has no resource kind %q", ref.Kind)
}

// deleteOp runs a delete call and waits for it; not found is success.
func (d *Driver) deleteOp(ctx context.Context, api ComputeAPI, call func() (*computepb.Operation, error)) error {
	op, err := call()
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return classify(err, errContext{})
	}
	if err := d.finish(ctx, api, op); err != nil && !vendors.IsClass(err, vendors.NotFound) {
		return err
	}
	return nil
}

// deleteSubnet deletes a managed subnetwork and, when it was the last of the
// network (which spans the regions), the network; or an orphaned managed network.
func (d *Driver) deleteSubnet(ctx context.Context, api ComputeAPI, ref vendors.ResourceRef) error {
	network := ref.Network
	if network == "" {
		network = d.managedNetworkName()
	}
	if !strings.Contains(ref.ID, "/global/networks/") {
		s, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: d.project, Region: ref.Region,
			Subnetwork: ref.Name})
		switch {
		case err == nil:
			if _, _, ok := ownedBy(d.cfg.ClusterName, s.GetName(), s.GetDescription()); !ok {
				return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.ID)
			}
			network = lastSegment(s.GetNetwork())
			if err := d.deleteOp(ctx, api, func() (*computepb.Operation, error) {
				return api.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{Project: d.project,
					Region: ref.Region, Subnetwork: ref.Name})
			}); err != nil {
				return err
			}
		case IsNotFound(err):
		default:
			return classify(err, errContext{})
		}
	}
	n, err := api.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: d.project, Network: network})
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return classify(err, errContext{})
	}
	if _, _, ok := ownedBy(d.cfg.ClusterName, n.GetName(), n.GetDescription()); !ok {
		return nil // the subnetwork was ours, the network is not: never touched
	}
	if len(n.GetSubnetworks()) > 0 {
		return nil // the network is global: the subnetworks of other regions keep it
	}
	return d.deleteOp(ctx, api, func() (*computepb.Operation, error) {
		return api.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: d.project, Network: network})
	})
}
