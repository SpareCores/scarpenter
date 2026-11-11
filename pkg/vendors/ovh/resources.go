/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// Neutron security groups and Nova server groups have no tags (the tag extension
// is unverified on OVH, spike S5), so ownership is carried where the resource has
// room for it: a security group's description holds the marker of the tags of
// every managed resource (cluster, nodeclass, managed-by, resource-kind), and a
// server group, which has only a name, is ours when its name starts with the
// deterministic prefix of the cluster (resources.NamePrefix), which the
// engine gives every managed resource. A server group's NodeClass is therefore
// not known to the driver; its lifecycle is perUse, which does not need it.
const (
	markerPrefix   = "SCarpenter managed "
	maxDescription = 255
)

// Neutron rule vocabulary.
const (
	dirIngress = "ingress"
	etherIPv4  = "IPv4"
	etherIPv6  = "IPv6"
)

// Codes of the resource calls.
const (
	CodeNoMarker      = "NoOwnershipMarker"
	CodeNotOurs       = "NotCreatedBySCarpenter"
	CodeMarkerTooLong = "OwnershipMarkerTooLong"
	CodeGroupPolicy   = "InvalidServerGroupPolicy"
)

// SecurityGroupParams are the create parameters of a managed security group
// (CreateSpec.Params): the rules of the group besides Neutron's egress defaults.
type SecurityGroupParams struct {
	// SelfReference admits all traffic between the members of the group (the
	// isolation nodeclass and run).
	SelfReference bool                 `json:"selfReference,omitempty"`
	Inbound       []common.InboundRule `json:"inbound,omitempty"`
}

// ServerGroupParams are the create parameters of a managed server group.
type ServerGroupParams struct {
	Policy string `json:"policy"`
}

// Kinds implements vendors.ManagedResources.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindServerGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindRunSecurityGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 25},
		{Kind: KindSecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
	}
}

func isKind(k vendors.ResourceKind) bool {
	return k == KindSecurityGroups || k == KindRunSecurityGroups || k == KindServerGroups
}

func isGroupKind(k vendors.ResourceKind) bool {
	return k == KindSecurityGroups || k == KindRunSecurityGroups
}

func encodeMarker(tags map[vendors.TagKey]string) (string, error) {
	m := map[string]string{}
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := tags[k]; ok {
			m[string(k)] = v
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	s := markerPrefix + string(b)
	if len(s) > maxDescription {
		return "", vendors.NewError(vendors.Fatal, CodeMarkerTooLong, fmt.Errorf(
			"the ownership marker of a security group is %d characters, Neutron's description takes %d: "+
				"shorten the cluster or NodeClass name", len(s), maxDescription))
	}
	return s, nil
}

// decodeMarker reads the tags back from a security group's description; nil when
// the description is not a marker.
func decodeMarker(desc string) map[vendors.TagKey]string {
	rest, ok := strings.CutPrefix(desc, markerPrefix)
	if !ok {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(rest), &m); err != nil {
		return nil
	}
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := m[string(k)]; ok {
			out[k] = v
		}
	}
	return out
}

func owned(tags map[vendors.TagKey]string, cluster string) bool {
	return tags[vendors.TagManagedBy] == vendors.ManagedByValue && tags[vendors.TagCluster] == cluster && cluster != ""
}

// serverGroupTags are the tags a server group has: ours by its name.
func (d *Driver) serverGroupTags(name string) map[vendors.TagKey]string {
	c := d.cfg.ClusterName
	if c == "" || !strings.HasPrefix(name, resources.NamePrefix(c)) {
		return nil
	}
	return map[vendors.TagKey]string{
		vendors.TagCluster: c, vendors.TagManagedBy: vendors.ManagedByValue, vendors.TagResource: string(KindServerGroups),
	}
}

func sortRefs(out []vendors.ResourceRef) {
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+"/"+a.ID, string(b.Kind)+"/"+b.ID)
	})
}

// Resolve implements vendors.ManagedResources: the resources of the region that
// the selector selects; the images kind is a kind of the selector only and is
// resolved by ResolveImage.
func (d *Driver) Resolve(
	ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if !isKind(kind) {
		return nil, vendors.Errorf(vendors.Fatal, "the OVH driver has no resource kind %q to resolve", kind)
	}
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the vendor's default is Default's")
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	if kind == KindServerGroups {
		groups, err := cl.Compute.ListServerGroups(ctx)
		if err != nil {
			return nil, Classify(err)
		}
		res, err := selector.Apply(ref, d.Selectors()[kind], groups, serverGroupFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		for _, g := range res.Items {
			out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: g.ID, Name: g.Name})
		}
	} else {
		groups, err := cl.Network.ListSecurityGroups(ctx)
		if err != nil {
			return nil, Classify(err)
		}
		res, err := selector.Apply(ref, d.Selectors()[kind], groups, groupFields)
		d.noteMissing(kind, region, res.Missing)
		if err != nil {
			return nil, err
		}
		for _, g := range res.Items {
			out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: g.ID, Name: g.Name})
		}
	}
	sortRefs(out)
	return out, nil
}

// Ensure implements vendors.ManagedResources: finds the resource by its
// deterministic name, owned by the cluster, or creates it.
func (d *Driver) Ensure(
	ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"the OVH driver has no resource kind %q to create", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if kind == KindServerGroups {
		return d.ensureServerGroup(ctx, cl, region, spec)
	}
	return d.ensureSecurityGroup(ctx, cl, kind, region, spec)
}

func (d *Driver) ensureServerGroup(
	ctx context.Context, cl osapi.Clients, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	p := ServerGroupParams{Policy: "anti-affinity"}
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings,
				fmt.Errorf("the server group parameters: %w", err))
		}
	}
	if p.Policy != "affinity" && p.Policy != "anti-affinity" {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeGroupPolicy,
			fmt.Errorf("the server group policy %q is not affinity or anti-affinity", p.Policy))
	}
	cluster := spec.Tags[vendors.TagCluster]
	prefix := resources.NamePrefix(cluster)
	if cluster == "" || !strings.HasPrefix(spec.Name, prefix) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"the server group name %q does not start with %q: the name is the only ownership marker of a server group",
			spec.Name, prefix)
	}
	groups, err := cl.Compute.ListServerGroups(ctx)
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	ref := func(g osapi.ServerGroup) vendors.ResourceRef {
		return vendors.ResourceRef{
			Kind: KindServerGroups, Region: region, ID: g.ID, Name: g.Name, Managed: true, Tags: spec.Tags,
		}
	}
	if i := slices.IndexFunc(groups, func(g osapi.ServerGroup) bool { return g.Name == spec.Name }); i >= 0 {
		return ref(groups[i]), false, nil
	}
	g, err := cl.Compute.CreateServerGroup(ctx, spec.Name, p.Policy)
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	return ref(*g), true, nil
}

// ingress is the canonical form of an ingress rule, for the comparison of the
// wanted and the present rules.
func ingress(r osapi.SecurityRule) string {
	cidr := r.RemoteCIDR
	if p, err := netip.ParsePrefix(cidr); err == nil {
		cidr = p.Masked().String()
	}
	return fmt.Sprintf("%s|%s|%d|%d|%s|%s", r.EtherType, r.Protocol, r.PortMin, r.PortMax, cidr, r.RemoteGroupID)
}

// wantedRules are the ingress rules of a managed group: the members reach each
// other when SelfReference is set, and the NodeClass's inbound rules.
func wantedRules(groupID string, p SecurityGroupParams) []osapi.SecurityRule {
	var out []osapi.SecurityRule
	if p.SelfReference {
		for _, ether := range []string{etherIPv4, etherIPv6} {
			out = append(out, osapi.SecurityRule{Direction: dirIngress, EtherType: ether, RemoteGroupID: groupID})
		}
	}
	for _, r := range p.Inbound {
		for _, c := range r.CIDRs {
			rule := osapi.SecurityRule{Direction: dirIngress, EtherType: etherIPv4, RemoteCIDR: c}
			if strings.Contains(c, ":") {
				rule.EtherType = etherIPv6
			}
			switch r.Protocol {
			case common.Protocol("tcp"), common.Protocol("udp"):
				rule.Protocol = string(r.Protocol)
				if r.FromPort != nil {
					rule.PortMin, rule.PortMax = int(*r.FromPort), int(*r.FromPort)
					if r.ToPort != nil {
						rule.PortMax = int(*r.ToPort)
					}
				}
			case common.Protocol("icmp"):
				rule.Protocol = "icmp"
				if rule.EtherType == etherIPv6 {
					rule.Protocol = "ipv6-icmp"
				}
			}
			out = append(out, rule)
		}
	}
	return out
}

func (d *Driver) ensureSecurityGroup(
	ctx context.Context, cl osapi.Clients, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var p SecurityGroupParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeInvalidSettings,
				fmt.Errorf("the security group parameters: %w", err))
		}
	}
	desc, err := encodeMarker(spec.Tags)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	groups, err := cl.Network.ListSecurityGroups(ctx)
	if err != nil {
		return vendors.ResourceRef{}, false, Classify(err)
	}
	cluster := spec.Tags[vendors.TagCluster]
	var g *osapi.SecurityGroup
	named := 0
	for i := range groups {
		if groups[i].Name != spec.Name {
			continue
		}
		named++
		if owned(decodeMarker(groups[i].Description), cluster) && g == nil {
			g = &groups[i]
		}
	}
	if g == nil && named > 0 {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, CodeNoMarker, fmt.Errorf(
			"a security group named %s exists in %s without the ownership marker of this cluster: not adopted",
			spec.Name, region))
	}
	created := false
	if g == nil {
		made, err := cl.Network.CreateSecurityGroup(ctx, spec.Name, desc)
		if err != nil {
			return vendors.ResourceRef{}, false, Classify(err)
		}
		g, created = made, true
	}
	if err := d.syncRules(ctx, cl, g, p); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	return vendors.ResourceRef{
		Kind: kind, Region: region, ID: g.ID, Name: spec.Name, Managed: true, Tags: decodeMarker(desc),
	}, created, nil
}

// syncRules makes the group's ingress rules the wanted ones: missing rules are
// created, others removed. Egress rules (Neutron's defaults) are left alone.
func (d *Driver) syncRules(ctx context.Context, cl osapi.Clients, g *osapi.SecurityGroup, p SecurityGroupParams) error {
	want := wantedRules(g.ID, p)
	have := map[string]osapi.SecurityRule{}
	for _, r := range g.Rules {
		if r.Direction == dirIngress {
			have[ingress(r)] = r
		}
	}
	wanted := map[string]bool{}
	for _, r := range want {
		k := ingress(r)
		wanted[k] = true
		if _, ok := have[k]; ok {
			continue
		}
		if _, err := cl.Network.CreateSecurityRule(ctx, g.ID, r); err != nil {
			if ae, ok := errors.AsType[*osapi.APIError](err); ok && ae.Status == 409 &&
				strings.Contains(ae.Message, "SecurityGroupRuleExists") {
				continue
			}
			return Classify(err)
		}
	}
	for k, r := range have {
		if wanted[k] {
			continue
		}
		if err := cl.Network.DeleteSecurityRule(ctx, r.ID); err != nil && !IsNotFound(err) {
			return Classify(err)
		}
	}
	return nil
}

// regionsOf is the API regions of the list the catalog has.
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

func hasTags(have, want map[vendors.TagKey]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// List implements vendors.ManagedResources: the managed resources (those with an
// ownership marker) of the regions whose tags include the given ones.
func (d *Driver) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	usable, err := d.regionsOf(ctx, regions)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, region := range usable {
		cl, err := d.clients(ctx, region)
		if err != nil {
			return nil, err
		}
		sgs, err := cl.Network.ListSecurityGroups(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", region, Classify(err))
		}
		for _, g := range sgs {
			m := decodeMarker(g.Description)
			if m == nil || !hasTags(m, tags) {
				continue
			}
			kind := vendors.ResourceKind(m[vendors.TagResource])
			if !isGroupKind(kind) {
				kind = KindSecurityGroups
			}
			out = append(out, vendors.ResourceRef{
				Kind: kind, Region: region, ID: g.ID, Name: g.Name,
				Managed: m[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: m,
			})
		}
		groups, err := cl.Compute.ListServerGroups(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the managed resources of %s: %w", region, Classify(err))
		}
		for _, g := range groups {
			m := d.serverGroupTags(g.Name)
			if m == nil || !hasTags(m, tags) {
				continue
			}
			out = append(out, vendors.ResourceRef{
				Kind: KindServerGroups, Region: region, ID: g.ID, Name: g.Name, Managed: true, Tags: m,
			})
		}
	}
	sortRefs(out)
	return out, nil
}

// InUse implements vendors.ManagedResources: a security group is in use while a
// server of the region lists it, a server group while it has members.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	if !isKind(ref.Kind) {
		return false, time.Time{}, vendors.Errorf(vendors.Fatal, "the OVH driver has no resource kind %q", ref.Kind)
	}
	cl, err := d.clients(ctx, ref.Region)
	if err != nil {
		if regionGone(err) {
			return false, time.Time{}, vendors.NewError(vendors.NotFound, CodeNotFound, err)
		}
		return false, time.Time{}, err
	}
	if ref.Kind == KindServerGroups {
		groups, err := cl.Compute.ListServerGroups(ctx)
		if err != nil {
			return false, time.Time{}, Classify(err)
		}
		i := slices.IndexFunc(groups, func(g osapi.ServerGroup) bool { return g.ID == ref.ID })
		if i < 0 {
			return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the server group %s does not exist", ref.ID)
		}
		return len(groups[i].Members) > 0, d.clock.Now(), nil
	}
	sgs, err := cl.Network.ListSecurityGroups(ctx)
	if err != nil {
		return false, time.Time{}, Classify(err)
	}
	i := slices.IndexFunc(sgs, func(g osapi.SecurityGroup) bool { return g.ID == ref.ID })
	if i < 0 {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "the security group %s does not exist", ref.ID)
	}
	servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
	if err != nil {
		return false, time.Time{}, Classify(err)
	}
	for _, s := range servers {
		if ServerState(s) != vendors.StateTerminated &&
			(slices.Contains(s.SecurityGroups, sgs[i].Name) || slices.Contains(s.SecurityGroups, sgs[i].ID)) {
			return true, d.clock.Now(), nil
		}
	}
	return false, time.Time{}, nil
}

// Delete implements vendors.ManagedResources: only a resource this cluster
// created, and not one that is in use (Neutron refuses a group of a server; the
// answer is Retryable). A resource that is gone is deleted.
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	if !isKind(ref.Kind) {
		return vendors.Errorf(vendors.Fatal, "the OVH driver has no resource kind %q", ref.Kind)
	}
	cl, err := d.clients(ctx, ref.Region)
	if err != nil {
		if regionGone(err) {
			return nil
		}
		return err
	}
	if ref.Kind == KindServerGroups {
		groups, err := cl.Compute.ListServerGroups(ctx)
		if err != nil {
			return Classify(err)
		}
		i := slices.IndexFunc(groups, func(g osapi.ServerGroup) bool { return g.ID == ref.ID })
		if i < 0 {
			return nil
		}
		if d.serverGroupTags(groups[i].Name) == nil {
			return vendors.NewError(vendors.Fatal, CodeNotOurs, fmt.Errorf(
				"the server group %s was not created by SCarpenter and is never deleted", groups[i].Name))
		}
		if len(groups[i].Members) > 0 {
			return vendors.NewError(vendors.Retryable, CodeConflict, fmt.Errorf(
				"the server group %s still has %d members", groups[i].Name, len(groups[i].Members)))
		}
		if err := cl.Compute.DeleteServerGroup(ctx, ref.ID); err != nil && !IsNotFound(err) {
			return Classify(err)
		}
		return nil
	}
	sgs, err := cl.Network.ListSecurityGroups(ctx)
	if err != nil {
		return Classify(err)
	}
	i := slices.IndexFunc(sgs, func(g osapi.SecurityGroup) bool { return g.ID == ref.ID })
	if i < 0 {
		return nil
	}
	if !owned(decodeMarker(sgs[i].Description), d.cfg.ClusterName) {
		return vendors.NewError(vendors.Fatal, CodeNotOurs, fmt.Errorf(
			"the security group %s was not created by this SCarpenter and is never deleted", sgs[i].Name))
	}
	if err := cl.Network.DeleteSecurityGroup(ctx, ref.ID); err != nil && !IsNotFound(err) {
		return Classify(err)
	}
	return nil
}
