/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// SecurityGroupParams are the create parameters of a security group (Params of
// the ResourceRequest, which the resource engine hashes into the group's name:
// a changed rule set is a new group, the old one is collected by its lifecycle).
type SecurityGroupParams struct {
	// VPC is the VPC the group lives in.
	VPC string `json:"vpcId"`
	// SelfReference allows all traffic between the members of the group: the
	// rule of isolation nodeclass (the NodeClass's group) and of isolation run
	// (the run's group).
	SelfReference bool `json:"selfReference,omitempty"`
	// Inbound are the rules of spec.networking.inbound.
	Inbound []common.InboundRule `json:"inbound,omitempty"`
}

// DeploymentSetParams are the create parameters of a deployment set.
type DeploymentSetParams struct {
	// Strategy is LowLatency or Availability.
	Strategy string `json:"strategy"`
}

// ---- security groups ---------------------------------------------------------------

// ensureSecurityGroup finds the group of the name in the VPC or creates it with
// the ownership marker and the rules of its parameters; the rules of an existing
// managed group are brought in line (a creation interrupted after the group
// existed is finished by the next call).
func (d *Driver) ensureSecurityGroup(
	ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var p SecurityGroupParams
	if err := json.Unmarshal(spec.Params, &p); err != nil || p.VPC == "" {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
			fmt.Errorf("a security group needs the parameter vpcId: %v", err))
	}
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	find := func() (*groupInfo, error) {
		gs, err := d.pageGroups(ctx, region, &ecs.DescribeSecurityGroupsRequest{
			RegionId: new(region), VpcId: new(p.VPC), SecurityGroupName: new(spec.Name), MaxResults: dara.Int32(groupPage),
		})
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			if deref(g.SecurityGroupName) == spec.Name {
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
		in := &ecs.CreateSecurityGroupRequest{
			RegionId: new(region), VpcId: new(p.VPC), SecurityGroupName: new(spec.Name), Description: new(sgDescription),
			SecurityGroupType: new("normal"), Tag: d.groupCreateTags(spec),
			ClientToken: new(clientToken("sg", p.VPC, spec.Name)),
		}
		out, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.CreateSecurityGroupResponseBody, error) {
				return cl.ECS.CreateSecurityGroup(ctx, in)
			})
		if err != nil {
			return vendors.ResourceRef{}, false, err
		}
		created = true
		g = &groupInfo{SecurityGroupId: out.SecurityGroupId, SecurityGroupName: new(spec.Name), VpcId: new(p.VPC)}
		g.Tags = &ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroupTags{}
		for _, t := range in.Tag {
			g.Tags.Tag = append(g.Tags.Tag, &ecs.DescribeSecurityGroupsResponseBodySecurityGroupsSecurityGroupTagsTag{
				TagKey: t.Key, TagValue: t.Value,
			})
		}
	}
	tags := d.markerOfGroup(g)
	if !owned(tags, spec.Tags[vendors.TagCluster]) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a security group named %s exists in %s without the ownership marker", spec.Name, p.VPC)
	}
	if err := d.syncIngress(ctx, region, deref(g.SecurityGroupId), p); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	return vendors.ResourceRef{
		Kind: kind, Region: region, ID: deref(g.SecurityGroupId), Name: spec.Name, Managed: true, Tags: tags,
		Network: p.VPC,
	}, created, nil
}

// ingressRule is a comparable view of one inbound rule of a group.
type ingressRule struct {
	protocol  string
	portRange string
	source    string // cidr:<range> or group:<id>
}

const (
	protoAll = "all"
	// noPorts is the port range of the protocols without ports.
	noPorts = "-1/-1"
)

// wantedRules are the inbound rules a group with the parameters has.
func wantedRules(groupID string, p SecurityGroupParams) []ingressRule {
	var out []ingressRule
	if p.SelfReference {
		out = append(out, ingressRule{protocol: protoAll, portRange: noPorts, source: "group:" + groupID})
	}
	for _, r := range p.Inbound {
		proto, ports := protoAll, noPorts
		switch r.Protocol {
		case common.Protocol("tcp"), common.Protocol("udp"):
			proto, ports = string(r.Protocol), "1/65535"
			if r.FromPort != nil {
				to := *r.FromPort
				if r.ToPort != nil {
					to = *r.ToPort
				}
				ports = fmt.Sprintf("%d/%d", *r.FromPort, to)
			}
		case common.Protocol("icmp"):
			proto = "icmp"
		}
		for _, c := range r.CIDRs {
			out = append(out, ingressRule{protocol: proto, portRange: ports, source: "cidr:" + c})
		}
	}
	return out
}

// rulesOf reads the inbound rules of a group; the ID of each rule is kept to
// revoke it.
func (d *Driver) rulesOf(
	ctx context.Context, region, groupID string,
) (rules []ingressRule, ids []string, err error) {
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return nil, nil, err
	}
	in := &ecs.DescribeSecurityGroupAttributeRequest{
		RegionId: new(region), SecurityGroupId: new(groupID), Direction: new("ingress"),
	}
	out, err := call(ctx, d.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DescribeSecurityGroupAttributeResponseBody, error) {
			return cl.ECS.DescribeSecurityGroupAttribute(ctx, in)
		})
	if err != nil {
		return nil, nil, err
	}
	if out.Permissions == nil {
		return nil, nil, nil
	}
	for _, p := range out.Permissions.Permission {
		if deref(p.Direction) != "ingress" || (deref(p.Policy) != "" && !strings.EqualFold(deref(p.Policy), "accept")) {
			continue
		}
		r := ingressRule{protocol: strings.ToLower(deref(p.IpProtocol)), portRange: deref(p.PortRange)}
		switch {
		case deref(p.SourceGroupId) != "":
			r.source = "group:" + deref(p.SourceGroupId)
		default:
			r.source = "cidr:" + deref(p.SourceCidrIp)
		}
		rules = append(rules, r)
		ids = append(ids, deref(p.SecurityGroupRuleId))
	}
	return rules, ids, nil
}

// syncIngress adds the rules the group lacks and removes the ones it should not
// have. The default outbound rule (everything) stays as ECS creates it.
func (d *Driver) syncIngress(ctx context.Context, region, groupID string, p SecurityGroupParams) error {
	want := wantedRules(groupID, p)
	have, ids, err := d.rulesOf(ctx, region, groupID)
	if err != nil {
		return err
	}
	cl, err := d.cloud.Clients(ctx, region)
	if err != nil {
		return err
	}
	var drop []*string
	for i, r := range have {
		if !slices.Contains(want, r) {
			drop = append(drop, new(ids[i]))
		}
	}
	if len(drop) > 0 {
		in := &ecs.RevokeSecurityGroupRequest{RegionId: new(region), SecurityGroupId: new(groupID), SecurityGroupRuleId: drop}
		if _, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.RevokeSecurityGroupResponseBody, error) {
				return cl.ECS.RevokeSecurityGroup(ctx, in)
			}); err != nil {
			return err
		}
	}
	for _, r := range want {
		if slices.Contains(have, r) {
			continue
		}
		in := &ecs.AuthorizeSecurityGroupRequest{
			RegionId: new(region), SecurityGroupId: new(groupID), IpProtocol: new(r.protocol), PortRange: new(r.portRange),
			Policy: new("accept"), Priority: new("1"), NicType: new("intranet"),
		}
		if id, ok := strings.CutPrefix(r.source, "group:"); ok {
			in.SourceGroupId = new(id)
		} else {
			in.SourceCidrIp = new(strings.TrimPrefix(r.source, "cidr:"))
		}
		if _, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.AuthorizeSecurityGroupResponseBody, error) {
				return cl.ECS.AuthorizeSecurityGroup(ctx, in)
			}); err != nil && !isCode(err, duplicateRuleCodes...) {
			return err
		}
	}
	return nil
}

// duplicateRuleCodes are the codes of an Authorize for a rule that exists (a race
// with another creator of the same group); the code ECS answers is unverified,
// the likely spellings are accepted.
var duplicateRuleCodes = []string{
	"InvalidPermission.Duplicate", "InvalidPermission.Duplicated", "InvalidSecurityGroupRule.Duplicate",
}

func (d *Driver) listSecurityGroups(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	groups, err := d.describeGroups(ctx, region, vendors.UseRef{Tags: d.tagFilter(tags)})
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, g := range groups {
		marker := d.markerOfGroup(g)
		kind := vendors.ResourceKind(marker[vendors.TagResource])
		if kind != KindSecurityGroups && kind != KindRunSecurityGroups {
			continue
		}
		if !matchesTags(marker, tags) {
			continue
		}
		out = append(out, vendors.ResourceRef{
			Kind: kind, Region: region, ID: deref(g.SecurityGroupId), Name: deref(g.SecurityGroupName),
			Managed: marker[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: marker, Network: deref(g.VpcId),
			CreatedAt: parseTime(deref(g.CreationTime)),
		})
	}
	return out, nil
}

func (d *Driver) deleteSecurityGroup(ctx context.Context, ref vendors.ResourceRef) error {
	gs, err := d.describeGroups(ctx, ref.Region, vendors.UseRef{IDs: []string{ref.ID}})
	if err != nil {
		return err
	}
	if len(gs) == 0 {
		return nil
	}
	if !d.markedFor(d.markerOfGroup(gs[0]), ref) {
		return vendors.Errorf(vendors.Fatal,
			"the security group %s was not created by SCarpenter and is never deleted", ref.ID)
	}
	cl, err := d.cloud.Clients(ctx, ref.Region)
	if err != nil {
		return err
	}
	in := &ecs.DeleteSecurityGroupRequest{RegionId: new(ref.Region), SecurityGroupId: new(ref.ID)}
	_, err = call(ctx, d.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DeleteSecurityGroupResponseBody, error) {
			return cl.ECS.DeleteSecurityGroup(ctx, in)
		})
	if err != nil && !isCode(err, "InvalidSecurityGroupId.NotFound") {
		return err
	}
	return nil
}

// ---- deployment sets ---------------------------------------------------------------

// ensureDeploymentSet finds the set of the name or creates it with the strategy of
// its parameters (Availability without any). The set carries the ownership marker
// in its description.
func (d *Driver) ensureDeploymentSet(
	ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	p := DeploymentSetParams{Strategy: "Availability"}
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
				fmt.Errorf("the deployment set parameters: %w", err))
		}
	}
	find := func() (*setInfo, error) {
		sets, err := d.describeSets(ctx, region, vendors.UseRef{Name: spec.Name})
		if err != nil {
			return nil, err
		}
		for _, s := range sets {
			if deref(s.DeploymentSetName) == spec.Name {
				return s, nil
			}
		}
		return nil, nil
	}
	s, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if s == nil {
		cl, err := d.cloud.Clients(ctx, region)
		if err != nil {
			return vendors.ResourceRef{}, false, err
		}
		in := &ecs.CreateDeploymentSetRequest{
			RegionId: new(region), DeploymentSetName: new(spec.Name), Description: new(setDescription(spec)),
			Strategy: new(p.Strategy), ClientToken: new(clientToken("ds", spec.Name)),
		}
		out, err := call(ctx, d.cloud, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.CreateDeploymentSetResponseBody, error) {
				return cl.ECS.CreateDeploymentSet(ctx, in)
			})
		switch {
		case err == nil:
			created = true
			s = &setInfo{
				DeploymentSetId: out.DeploymentSetId, DeploymentSetName: new(spec.Name), DeploymentSetDescription: in.Description,
			}
		case isCode(err, "InvalidDeploymentSetName.Duplicated", "InvalidParameter.DeploymentSetName.Duplicated"):
			// Lost a race with another creator of the same name: adopt it.
			if s, err = find(); err != nil || s == nil {
				return vendors.ResourceRef{}, false, classify(
					fmt.Errorf("the deployment set %s vanished: %w", spec.Name, err), errContext{})
			}
		default:
			return vendors.ResourceRef{}, false, err
		}
	}
	tags := markerOfSet(s)
	if !owned(tags, spec.Tags[vendors.TagCluster]) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a deployment set named %s exists without the ownership marker", spec.Name)
	}
	return vendors.ResourceRef{
		Kind: KindDeploymentSets, Region: region, ID: deref(s.DeploymentSetId), Name: spec.Name, Managed: true, Tags: tags,
	}, created, nil
}

func (d *Driver) listDeploymentSets(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	// A set takes no tags: list the sets of the account in the region and read the
	// marker of each from its description.
	sets, err := d.describeSets(ctx, region, vendors.UseRef{Name: "scarpenter-*"})
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, s := range sets {
		marker := markerOfSet(s)
		if len(marker) == 0 || !matchesTags(marker, tags) {
			continue
		}
		out = append(out, vendors.ResourceRef{
			Kind: KindDeploymentSets, Region: region, ID: deref(s.DeploymentSetId), Name: deref(s.DeploymentSetName),
			Managed: marker[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: marker,
			CreatedAt: parseTime(deref(s.CreationTime)),
		})
	}
	return out, nil
}

func (d *Driver) deleteDeploymentSet(ctx context.Context, ref vendors.ResourceRef) error {
	sets, err := d.describeSets(ctx, ref.Region, vendors.UseRef{IDs: []string{ref.ID}})
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return nil
	}
	if !d.markedFor(markerOfSet(sets[0]), ref) {
		return vendors.Errorf(vendors.Fatal,
			"the deployment set %s was not created by SCarpenter and is never deleted", ref.ID)
	}
	cl, err := d.cloud.Clients(ctx, ref.Region)
	if err != nil {
		return err
	}
	in := &ecs.DeleteDeploymentSetRequest{RegionId: new(ref.Region), DeploymentSetId: new(ref.ID)}
	_, err = call(ctx, d.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DeleteDeploymentSetResponseBody, error) {
			return cl.ECS.DeleteDeploymentSet(ctx, in)
		})
	if err != nil && !isCode(err, "InvalidDeploymentSetId.NotFound") {
		return err
	}
	return nil
}
