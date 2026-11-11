/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
)

// The read-only guard of the Alibaba Cloud canary: the second wall behind the role's policy
// (test/e2e-alicloud/canary-policy.sh), as aws_guard.go is for AWS. The driver is given clients
// that refuse every call that is not a Describe, Get or List before it reaches the SDK. The
// wrappers implement alicloud.ECSAPI and alicloud.VPCAPI method by method (a method added to
// an interface breaks the build here), and guard_test.go checks by reflection that every
// method of both interfaces is either passed through as a read or refused. The audit, the
// refusal error and isRead are shared with the AWS guard.

// readOnlyAlicloud wraps a client factory: every client it returns is guarded.
func readOnlyAlicloud(inner alicloud.ClientFactory, a *audit) alicloud.ClientFactory {
	return func(ctx context.Context, region string) (alicloud.Clients, error) {
		c, err := inner(ctx, region)
		if err != nil {
			return alicloud.Clients{}, err
		}
		return alicloud.Clients{ECS: &roECS{inner: c.ECS, audit: a}, VPC: &roVPC{inner: c.VPC, audit: a}}, nil
	}
}

type roECS struct {
	inner alicloud.ECSAPI
	audit *audit
}

type roVPC struct {
	inner alicloud.VPCAPI
	audit *audit
}

var (
	_ alicloud.ECSAPI = (*roECS)(nil)
	_ alicloud.VPCAPI = (*roVPC)(nil)
)

func (g *roECS) DescribeRegions(
	ctx context.Context, in *ecs.DescribeRegionsRequest,
) (*ecs.DescribeRegionsResponseBody, error) {
	g.audit.read("ecs:DescribeRegions")
	return g.inner.DescribeRegions(ctx, in)
}

func (g *roECS) DescribeZones(
	ctx context.Context, in *ecs.DescribeZonesRequest,
) (*ecs.DescribeZonesResponseBody, error) {
	g.audit.read("ecs:DescribeZones")
	return g.inner.DescribeZones(ctx, in)
}

func (g *roECS) DescribeInstanceTypes(
	ctx context.Context, in *ecs.DescribeInstanceTypesRequest,
) (*ecs.DescribeInstanceTypesResponseBody, error) {
	g.audit.read("ecs:DescribeInstanceTypes")
	return g.inner.DescribeInstanceTypes(ctx, in)
}

func (g *roECS) DescribeAvailableResource(
	ctx context.Context, in *ecs.DescribeAvailableResourceRequest,
) (*ecs.DescribeAvailableResourceResponseBody, error) {
	g.audit.read("ecs:DescribeAvailableResource")
	return g.inner.DescribeAvailableResource(ctx, in)
}

func (g *roECS) DescribeAccountAttributes(
	ctx context.Context, in *ecs.DescribeAccountAttributesRequest,
) (*ecs.DescribeAccountAttributesResponseBody, error) {
	g.audit.read("ecs:DescribeAccountAttributes")
	return g.inner.DescribeAccountAttributes(ctx, in)
}

func (g *roECS) DescribeImages(
	ctx context.Context, in *ecs.DescribeImagesRequest,
) (*ecs.DescribeImagesResponseBody, error) {
	g.audit.read("ecs:DescribeImages")
	return g.inner.DescribeImages(ctx, in)
}

func (g *roECS) RunInstances(
	ctx context.Context, in *ecs.RunInstancesRequest,
) (*ecs.RunInstancesResponseBody, error) {
	return nil, g.audit.deny("ecs:RunInstances")
}

func (g *roECS) DescribeInstances(
	ctx context.Context, in *ecs.DescribeInstancesRequest,
) (*ecs.DescribeInstancesResponseBody, error) {
	g.audit.read("ecs:DescribeInstances")
	return g.inner.DescribeInstances(ctx, in)
}

func (g *roECS) DeleteInstance(
	ctx context.Context, in *ecs.DeleteInstanceRequest,
) (*ecs.DeleteInstanceResponseBody, error) {
	return nil, g.audit.deny("ecs:DeleteInstance")
}

func (g *roECS) DescribeSecurityGroups(
	ctx context.Context, in *ecs.DescribeSecurityGroupsRequest,
) (*ecs.DescribeSecurityGroupsResponseBody, error) {
	g.audit.read("ecs:DescribeSecurityGroups")
	return g.inner.DescribeSecurityGroups(ctx, in)
}

func (g *roECS) CreateSecurityGroup(
	ctx context.Context, in *ecs.CreateSecurityGroupRequest,
) (*ecs.CreateSecurityGroupResponseBody, error) {
	return nil, g.audit.deny("ecs:CreateSecurityGroup")
}

func (g *roECS) AuthorizeSecurityGroup(
	ctx context.Context, in *ecs.AuthorizeSecurityGroupRequest,
) (*ecs.AuthorizeSecurityGroupResponseBody, error) {
	return nil, g.audit.deny("ecs:AuthorizeSecurityGroup")
}

func (g *roECS) RevokeSecurityGroup(
	ctx context.Context, in *ecs.RevokeSecurityGroupRequest,
) (*ecs.RevokeSecurityGroupResponseBody, error) {
	return nil, g.audit.deny("ecs:RevokeSecurityGroup")
}

func (g *roECS) DeleteSecurityGroup(
	ctx context.Context, in *ecs.DeleteSecurityGroupRequest,
) (*ecs.DeleteSecurityGroupResponseBody, error) {
	return nil, g.audit.deny("ecs:DeleteSecurityGroup")
}

func (g *roECS) DescribeSecurityGroupAttribute(
	ctx context.Context, in *ecs.DescribeSecurityGroupAttributeRequest,
) (*ecs.DescribeSecurityGroupAttributeResponseBody, error) {
	g.audit.read("ecs:DescribeSecurityGroupAttribute")
	return g.inner.DescribeSecurityGroupAttribute(ctx, in)
}

func (g *roECS) CreateDeploymentSet(
	ctx context.Context, in *ecs.CreateDeploymentSetRequest,
) (*ecs.CreateDeploymentSetResponseBody, error) {
	return nil, g.audit.deny("ecs:CreateDeploymentSet")
}

func (g *roECS) DescribeDeploymentSets(
	ctx context.Context, in *ecs.DescribeDeploymentSetsRequest,
) (*ecs.DescribeDeploymentSetsResponseBody, error) {
	g.audit.read("ecs:DescribeDeploymentSets")
	return g.inner.DescribeDeploymentSets(ctx, in)
}

func (g *roECS) DeleteDeploymentSet(
	ctx context.Context, in *ecs.DeleteDeploymentSetRequest,
) (*ecs.DeleteDeploymentSetResponseBody, error) {
	return nil, g.audit.deny("ecs:DeleteDeploymentSet")
}

func (g *roECS) TagResources(
	ctx context.Context, in *ecs.TagResourcesRequest,
) (*ecs.TagResourcesResponseBody, error) {
	return nil, g.audit.deny("ecs:TagResources")
}

func (g *roECS) ListTagResources(
	ctx context.Context, in *ecs.ListTagResourcesRequest,
) (*ecs.ListTagResourcesResponseBody, error) {
	g.audit.read("ecs:ListTagResources")
	return g.inner.ListTagResources(ctx, in)
}

func (g *roVPC) DescribeVpcs(
	ctx context.Context, in *vpc.DescribeVpcsRequest,
) (*vpc.DescribeVpcsResponseBody, error) {
	g.audit.read("vpc:DescribeVpcs")
	return g.inner.DescribeVpcs(ctx, in)
}

func (g *roVPC) CreateVpc(
	ctx context.Context, in *vpc.CreateVpcRequest,
) (*vpc.CreateVpcResponseBody, error) {
	return nil, g.audit.deny("vpc:CreateVpc")
}

func (g *roVPC) DeleteVpc(
	ctx context.Context, in *vpc.DeleteVpcRequest,
) (*vpc.DeleteVpcResponseBody, error) {
	return nil, g.audit.deny("vpc:DeleteVpc")
}

func (g *roVPC) DescribeVSwitches(
	ctx context.Context, in *vpc.DescribeVSwitchesRequest,
) (*vpc.DescribeVSwitchesResponseBody, error) {
	g.audit.read("vpc:DescribeVSwitches")
	return g.inner.DescribeVSwitches(ctx, in)
}

func (g *roVPC) CreateVSwitch(
	ctx context.Context, in *vpc.CreateVSwitchRequest,
) (*vpc.CreateVSwitchResponseBody, error) {
	return nil, g.audit.deny("vpc:CreateVSwitch")
}

func (g *roVPC) DeleteVSwitch(
	ctx context.Context, in *vpc.DeleteVSwitchRequest,
) (*vpc.DeleteVSwitchResponseBody, error) {
	return nil, g.audit.deny("vpc:DeleteVSwitch")
}
