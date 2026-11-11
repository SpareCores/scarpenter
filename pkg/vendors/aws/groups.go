/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
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

// PlacementGroupParams are the create parameters of a placement group.
type PlacementGroupParams struct {
	Strategy       string `json:"strategy"`
	PartitionCount *int32 `json:"partitionCount,omitempty"`
}

// spreadLimit is the number of instances a spread placement group takes per
// zone.
const spreadLimit = 7

// sgDescription is the description of the groups SCarpenter creates (EC2 allows
// a limited character set).
const sgDescription = "Managed by SCarpenter"

func (d *Driver) resolveSecurityGroups(ctx context.Context, cl Clients, kind vendors.ResourceKind, region string,
	ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	filters, none, err := useFilters(ref, groupFilters)
	if err != nil {
		return nil, err
	}
	var groups []ec2types.SecurityGroup
	if !none {
		if groups, err = d.describeGroups(ctx, cl, filters); err != nil {
			return nil, err
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[kind], groups, groupFields)
	d.noteMissing(kind, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: deref(g.GroupId), Name: deref(g.GroupName)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *Driver) describeGroups(
	ctx context.Context, cl Clients, filters []ec2types.Filter) ([]ec2types.SecurityGroup, error,
) {
	var out []ec2types.SecurityGroup
	in := &ec2.DescribeSecurityGroupsInput{Filters: filters}
	for {
		res, err := cl.EC2.DescribeSecurityGroups(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		out = append(out, res.SecurityGroups...)
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// ensureSecurityGroup finds the group of the name in the VPC or creates it with
// the ownership marker and the rules of its parameters; the rules of an existing
// managed group are brought in line (a creation interrupted after the group
// existed is finished by the next call).
func (d *Driver) ensureSecurityGroup(ctx context.Context, cl Clients, kind vendors.ResourceKind, region string,
	spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var p SecurityGroupParams
	if err := json.Unmarshal(spec.Params, &p); err != nil || p.VPC == "" {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
			fmt.Errorf("a security group needs the parameter vpcId: %v", err))
	}
	find := func() (*ec2types.SecurityGroup, error) {
		gs, err := d.describeGroups(ctx, cl, []ec2types.Filter{
			{Name: aws.String("group-name"), Values: []string{spec.Name}}, {Name: aws.String("vpc-id"),
				Values: []string{p.VPC}},
		})
		if err != nil || len(gs) == 0 {
			return nil, err
		}
		return &gs[0], nil
	}
	g, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if g == nil {
		out, err := cl.EC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName: aws.String(spec.Name), Description: aws.String(sgDescription), VpcId: aws.String(p.VPC),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSecurityGroup,
				Tags: d.resourceTags(spec)}},
		})
		switch {
		case err == nil:
			created = true
			g = &ec2types.SecurityGroup{GroupId: out.GroupId, GroupName: aws.String(spec.Name), VpcId: aws.String(p.VPC),
				Tags: d.resourceTags(spec)}
		case isCode(err, "InvalidGroup.Duplicate"):
			// Lost a race with another creator of the same name: adopt it.
			if g, err = find(); err != nil || g == nil {
				return vendors.ResourceRef{}, false, classify(fmt.Errorf("the group %s vanished: %w", spec.Name, err),
					errContext{})
			}
		default:
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
	}
	tags := d.markerTags(g.Tags)
	if !owned(tags, spec.Tags[vendors.TagCluster]) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a security group named %s exists in %s without the ownership marker", spec.Name, p.VPC)
	}
	if err := d.syncIngress(ctx, cl, g, p); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	return vendors.ResourceRef{
		Kind: kind, Region: region, ID: deref(g.GroupId), Name: spec.Name, Managed: true, Tags: tags,
	}, created, nil
}

// ingressRule is a comparable view of one rule of a group.
type ingressRule struct {
	protocol string
	from, to int32
	source   string
}

// wantedRules are the ingress rules a group with the parameters has.
func wantedRules(groupID string, p SecurityGroupParams) []ingressRule {
	var out []ingressRule
	if p.SelfReference {
		out = append(out, ingressRule{protocol: "-1", source: "group:" + groupID})
	}
	for _, r := range p.Inbound {
		proto, from, to := "-1", int32(0), int32(0)
		switch r.Protocol {
		case common.Protocol("tcp"), common.Protocol("udp"):
			proto, from, to = string(r.Protocol), 0, 65535
			if r.FromPort != nil {
				from, to = *r.FromPort, *r.FromPort
				if r.ToPort != nil {
					to = *r.ToPort
				}
			}
		case common.Protocol("icmp"):
			proto, from, to = "icmp", -1, -1
		}
		for _, c := range r.CIDRs {
			p := proto
			if p == "icmp" && strings.Contains(c, ":") {
				// "icmp" matches ICMPv4 only: an IPv6 source needs ICMPv6 (protocol 58).
				p = "58"
			}
			out = append(out, ingressRule{protocol: p, from: from, to: to, source: "cidr:" + c})
		}
	}
	return out
}

func rulesOf(g *ec2types.SecurityGroup) []ingressRule {
	var out []ingressRule
	for _, p := range g.IpPermissions {
		base := ingressRule{protocol: deref(p.IpProtocol), from: deref(p.FromPort), to: deref(p.ToPort)}
		if base.protocol == "-1" {
			base.from, base.to = 0, 0
		}
		for _, c := range p.IpRanges {
			r := base
			r.source = "cidr:" + deref(c.CidrIp)
			out = append(out, r)
		}
		for _, c := range p.Ipv6Ranges {
			r := base
			r.source = "cidr:" + deref(c.CidrIpv6)
			out = append(out, r)
		}
		for _, c := range p.UserIdGroupPairs {
			r := base
			r.source = "group:" + deref(c.GroupId)
			out = append(out, r)
		}
	}
	return out
}

func permission(r ingressRule) ec2types.IpPermission {
	p := ec2types.IpPermission{IpProtocol: aws.String(r.protocol)}
	if r.protocol != "-1" {
		p.FromPort, p.ToPort = aws.Int32(r.from), aws.Int32(r.to)
	}
	switch kind, v, _ := strings.Cut(r.source, ":"); {
	case kind == "group":
		p.UserIdGroupPairs = []ec2types.UserIdGroupPair{{GroupId: aws.String(v)}}
	case strings.Contains(v, ":"):
		p.Ipv6Ranges = []ec2types.Ipv6Range{{CidrIpv6: aws.String(v)}}
	default:
		p.IpRanges = []ec2types.IpRange{{CidrIp: aws.String(v)}}
	}
	return p
}

// syncIngress adds the rules the group lacks and removes the ones it should not
// have. The default egress (everything) stays as EC2 creates it.
func (d *Driver) syncIngress(
	ctx context.Context, cl Clients, g *ec2types.SecurityGroup, p SecurityGroupParams,
) error {
	id := deref(g.GroupId)
	want := wantedRules(id, p)
	have := rulesOf(g)
	var add, drop []ec2types.IpPermission
	for _, r := range want {
		if !slices.Contains(have, r) {
			add = append(add, permission(r))
		}
	}
	for _, r := range have {
		if !slices.Contains(want, r) {
			drop = append(drop, permission(r))
		}
	}
	if len(drop) > 0 {
		if _, err := cl.EC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(id),
			IpPermissions: drop}); err != nil {
			return classify(err, errContext{})
		}
	}
	if len(add) > 0 {
		if _, err := cl.EC2.AuthorizeSecurityGroupIngress(ctx,
			&ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(id), IpPermissions: add}); err != nil &&
			!isCode(err, "InvalidPermission.Duplicate") {
			return classify(err, errContext{})
		}
	}
	return nil
}

func (d *Driver) listSecurityGroups(
	ctx context.Context, cl Clients, region string, filters []ec2types.Filter) ([]vendors.ResourceRef, error,
) {
	groups, err := d.describeGroups(ctx, cl, filters)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, g := range groups {
		tags := d.markerTags(g.Tags)
		kind := d.kindOfTags(tags, KindSecurityGroups)
		if kind != KindSecurityGroups && kind != KindRunSecurityGroups {
			continue
		}
		out = append(out, vendors.ResourceRef{
			Kind: kind, Region: region, ID: deref(g.GroupId), Name: deref(g.GroupName),
			Managed: tags[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: tags,
		})
	}
	return out, nil
}

func (d *Driver) deleteSecurityGroup(ctx context.Context, cl Clients, ref vendors.ResourceRef) error {
	gs, err := d.describeGroups(ctx, cl, []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{ref.ID}}})
	if err != nil {
		return err
	}
	if len(gs) == 0 {
		return nil
	}
	if !d.markedFor(gs[0].Tags, ref) {
		return vendors.Errorf(vendors.Fatal, "the security group %s was not created by SCarpenter and is never deleted",
			ref.ID)
	}
	if _, err := cl.EC2.DeleteSecurityGroup(ctx,
		&ec2.DeleteSecurityGroupInput{GroupId: aws.String(ref.ID)}); err != nil &&
		!isCode(err, "InvalidGroup.NotFound") {
		return classify(err, errContext{})
	}
	return nil
}

// ---- placement groups ----

func (d *Driver) describePlacementGroups(
	ctx context.Context, cl Clients, filters []ec2types.Filter) ([]ec2types.PlacementGroup, error,
) {
	out, err := cl.EC2.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{Filters: filters})
	if err != nil {
		return nil, classify(err, errContext{})
	}
	return out.PlacementGroups, nil
}

func (d *Driver) resolvePlacementGroups(
	ctx context.Context, cl Clients, region string, ref vendors.UseRef) ([]vendors.ResourceRef, error,
) {
	if ref.IsDefault() {
		return nil, nil
	}
	filters, none, err := useFilters(ref, pgFilters)
	if err != nil {
		return nil, err
	}
	var groups []ec2types.PlacementGroup
	if !none {
		if groups, err = d.describePlacementGroups(ctx, cl, filters); err != nil {
			return nil, err
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[KindPlacementGroups], groups, placementGroupFields)
	d.noteMissing(KindPlacementGroups, region, res.Missing)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.ResourceRef, 0, len(res.Items))
	for _, g := range res.Items {
		out = append(out, vendors.ResourceRef{Kind: KindPlacementGroups, Region: region, ID: deref(g.GroupId),
			Name: deref(g.GroupName)})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// ensurePlacementGroup finds the group of the name or creates it with the
// strategy of its parameters (cluster without any).
func (d *Driver) ensurePlacementGroup(ctx context.Context, cl Clients, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	p := PlacementGroupParams{Strategy: string(ec2types.PlacementStrategyCluster)}
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeInvalidSettings,
				fmt.Errorf("the placement group parameters: %w", err))
		}
	}
	find := func() (*ec2types.PlacementGroup, error) {
		gs, err := d.describePlacementGroups(ctx, cl, []ec2types.Filter{{Name: aws.String("group-name"),
			Values: []string{spec.Name}}})
		if err != nil || len(gs) == 0 {
			return nil, err
		}
		return &gs[0], nil
	}
	g, err := find()
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	created := false
	if g == nil {
		in := &ec2.CreatePlacementGroupInput{
			GroupName: aws.String(spec.Name), Strategy: ec2types.PlacementStrategy(p.Strategy),
			PartitionCount: p.PartitionCount,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypePlacementGroup,
				Tags: d.resourceTags(spec)}},
		}
		out, err := cl.EC2.CreatePlacementGroup(ctx, in)
		switch {
		case err == nil:
			created = true
			g = out.PlacementGroup
			g.Tags = d.resourceTags(spec)
		case isCode(err, "InvalidPlacementGroup.Duplicate"):
			if g, err = find(); err != nil || g == nil {
				return vendors.ResourceRef{}, false, classify(fmt.Errorf("the placement group %s vanished: %w", spec.Name, err),
					errContext{})
			}
		default:
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
	}
	tags := d.markerTags(g.Tags)
	if !owned(tags, spec.Tags[vendors.TagCluster]) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a placement group named %s exists without the ownership marker", spec.Name)
	}
	return vendors.ResourceRef{
		Kind: KindPlacementGroups, Region: region, ID: deref(g.GroupId), Name: spec.Name, Managed: true, Tags: tags,
	}, created, nil
}

func (d *Driver) listPlacementGroups(
	ctx context.Context, cl Clients, region string, filters []ec2types.Filter) ([]vendors.ResourceRef, error,
) {
	groups, err := d.describePlacementGroups(ctx, cl, filters)
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, g := range groups {
		tags := d.markerTags(g.Tags)
		if d.kindOfTags(tags, KindPlacementGroups) != KindPlacementGroups {
			continue
		}
		out = append(out, vendors.ResourceRef{
			Kind: KindPlacementGroups, Region: region, ID: deref(g.GroupId), Name: deref(g.GroupName),
			Managed: tags[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: tags,
		})
	}
	return out, nil
}

func (d *Driver) deletePlacementGroup(ctx context.Context, cl Clients, ref vendors.ResourceRef) error {
	gs, err := d.describePlacementGroups(ctx, cl, []ec2types.Filter{{Name: aws.String("group-name"),
		Values: []string{ref.Name}}})
	if err != nil {
		return err
	}
	if len(gs) == 0 {
		return nil
	}
	if !d.markedFor(gs[0].Tags, ref) {
		return vendors.Errorf(vendors.Fatal, "the placement group %s was not created by SCarpenter and is never deleted",
			ref.Name)
	}
	if _, err := cl.EC2.DeletePlacementGroup(ctx,
		&ec2.DeletePlacementGroupInput{GroupName: aws.String(ref.Name)}); err != nil &&
		!isCode(err, "InvalidPlacementGroup.Unknown") {
		return classify(err, errContext{})
	}
	return nil
}

// groupInfo is what the launch needs to know about a placement group: its
// strategy, to apply the size limit of spread groups.
type groupInfo struct{ strategy string }

type cachedGroup struct {
	info  groupInfo
	until time.Time
}

type cachedSG struct {
	vpc   string
	until time.Time
}

// groupStrategy returns the strategy of the placement group by name (cached
// for the subnet TTL).
func (d *Driver) groupStrategy(ctx context.Context, region, name string) (string, error) {
	key := region + "/" + name
	d.mu.Lock()
	if c, ok := d.pgs[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return c.info.strategy, nil
	}
	d.mu.Unlock()
	cl, err := d.clients(ctx, region)
	if err != nil {
		return "", err
	}
	gs, err := d.describePlacementGroups(ctx, cl, []ec2types.Filter{{Name: aws.String("group-name"),
		Values: []string{name}}})
	if err != nil {
		return "", err
	}
	if len(gs) == 0 {
		return "", vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("the placement group %q does not exist in %s", name, region))
	}
	strategy := string(gs[0].Strategy)
	d.mu.Lock()
	d.pgs[key] = cachedGroup{info: groupInfo{strategy: strategy}, until: d.clock.Now().Add(subnetTTL)}
	d.mu.Unlock()
	return strategy, nil
}

// checkGroupSize applies the size limit of a spread placement group: seven
// instances per zone. A full group fails like a capacity error, without a
// launch that EC2 would refuse (the AWS provider recognises the refusal by its
// message; the check here avoids it).
func (d *Driver) checkGroupSize(ctx context.Context, region, zone, name string) error {
	strategy, err := d.groupStrategy(ctx, region, name)
	if err != nil {
		return err
	}
	if strategy != string(ec2types.PlacementStrategySpread) {
		return nil
	}
	insts, err := d.describeInstances(ctx, region, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("placement-group-name"), Values: []string{name}},
		{Name: aws.String("availability-zone"), Values: []string{zone}},
		{Name: aws.String("instance-state-name"), Values: launchStates},
	}})
	if err != nil {
		return err
	}
	if len(insts) >= spreadLimit {
		return vendors.NewError(vendors.InsufficientCapacity, codePlacementFull,
			fmt.Errorf("the spread placement group %s has %d instances in %s; EC2 takes at most %d per zone", name, len(insts),
				zone, spreadLimit))
	}
	return nil
}
