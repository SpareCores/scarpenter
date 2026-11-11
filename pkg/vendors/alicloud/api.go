/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the SDK method signatures are long, as the interface repeats them
package alicloud

import (
	"context"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
)

// ECSAPI is the part of the ECS API the driver calls. It is narrow on purpose:
// the in-memory fake (pkg/vendors/alicloud/fake) implements exactly this, and
// so does the adapter around the SDK client (the generated SDK methods take no
// context and return a response wrapper; the adapter calls the WithContext
// variants and returns the response body). The RAM policy of the driver grants
// these actions and no others.
type ECSAPI interface {
	DescribeRegions(ctx context.Context, in *ecs.DescribeRegionsRequest) (*ecs.DescribeRegionsResponseBody, error)
	DescribeZones(ctx context.Context, in *ecs.DescribeZonesRequest) (*ecs.DescribeZonesResponseBody, error)
	DescribeInstanceTypes(ctx context.Context, in *ecs.DescribeInstanceTypesRequest) (*ecs.DescribeInstanceTypesResponseBody, error)
	DescribeAvailableResource(ctx context.Context, in *ecs.DescribeAvailableResourceRequest) (*ecs.DescribeAvailableResourceResponseBody, error)
	DescribeAccountAttributes(ctx context.Context, in *ecs.DescribeAccountAttributesRequest) (*ecs.DescribeAccountAttributesResponseBody, error)
	DescribeImages(ctx context.Context, in *ecs.DescribeImagesRequest) (*ecs.DescribeImagesResponseBody, error)
	RunInstances(ctx context.Context, in *ecs.RunInstancesRequest) (*ecs.RunInstancesResponseBody, error)
	DescribeInstances(ctx context.Context, in *ecs.DescribeInstancesRequest) (*ecs.DescribeInstancesResponseBody, error)
	DeleteInstance(ctx context.Context, in *ecs.DeleteInstanceRequest) (*ecs.DeleteInstanceResponseBody, error)
	DescribeSecurityGroups(ctx context.Context, in *ecs.DescribeSecurityGroupsRequest) (*ecs.DescribeSecurityGroupsResponseBody, error)
	CreateSecurityGroup(ctx context.Context, in *ecs.CreateSecurityGroupRequest) (*ecs.CreateSecurityGroupResponseBody, error)
	AuthorizeSecurityGroup(ctx context.Context, in *ecs.AuthorizeSecurityGroupRequest) (*ecs.AuthorizeSecurityGroupResponseBody, error)
	RevokeSecurityGroup(ctx context.Context, in *ecs.RevokeSecurityGroupRequest) (*ecs.RevokeSecurityGroupResponseBody, error)
	DeleteSecurityGroup(ctx context.Context, in *ecs.DeleteSecurityGroupRequest) (*ecs.DeleteSecurityGroupResponseBody, error)
	DescribeSecurityGroupAttribute(ctx context.Context, in *ecs.DescribeSecurityGroupAttributeRequest) (*ecs.DescribeSecurityGroupAttributeResponseBody, error)
	CreateDeploymentSet(ctx context.Context, in *ecs.CreateDeploymentSetRequest) (*ecs.CreateDeploymentSetResponseBody, error)
	DescribeDeploymentSets(ctx context.Context, in *ecs.DescribeDeploymentSetsRequest) (*ecs.DescribeDeploymentSetsResponseBody, error)
	DeleteDeploymentSet(ctx context.Context, in *ecs.DeleteDeploymentSetRequest) (*ecs.DeleteDeploymentSetResponseBody, error)
	TagResources(ctx context.Context, in *ecs.TagResourcesRequest) (*ecs.TagResourcesResponseBody, error)
	ListTagResources(ctx context.Context, in *ecs.ListTagResourcesRequest) (*ecs.ListTagResourcesResponseBody, error)
}

// VPCAPI is the part of the VPC API the driver calls.
type VPCAPI interface {
	DescribeVpcs(ctx context.Context, in *vpc.DescribeVpcsRequest) (*vpc.DescribeVpcsResponseBody, error)
	CreateVpc(ctx context.Context, in *vpc.CreateVpcRequest) (*vpc.CreateVpcResponseBody, error)
	DeleteVpc(ctx context.Context, in *vpc.DeleteVpcRequest) (*vpc.DeleteVpcResponseBody, error)
	DescribeVSwitches(ctx context.Context, in *vpc.DescribeVSwitchesRequest) (*vpc.DescribeVSwitchesResponseBody, error)
	CreateVSwitch(ctx context.Context, in *vpc.CreateVSwitchRequest) (*vpc.CreateVSwitchResponseBody, error)
	DeleteVSwitch(ctx context.Context, in *vpc.DeleteVSwitchRequest) (*vpc.DeleteVSwitchResponseBody, error)
}

// Clients are the API clients of one region.
type Clients struct {
	ECS ECSAPI
	VPC VPCAPI
}

// ClientFactory creates the clients of a region. The real one (NewClientFactory)
// builds SDK clients from the default credential chain; tests pass the fake's.
type ClientFactory func(ctx context.Context, region string) (Clients, error)
