/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the adapters repeat the SDK method signatures
package alicloud

import (
	"context"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
)

// The adapters below follow the method list of api.go; they only add the
// context and unwrap the response body. A response without a body is returned
// as an empty one, so callers never dereference nil.

// ecsAdapter adapts the SDK's ECS client to ECSAPI.
type ecsAdapter struct{ c *ecs.Client }

// vpcAdapter adapts the SDK's VPC client to VPCAPI.
type vpcAdapter struct{ c *vpc.Client }

var (
	_ ECSAPI = ecsAdapter{}
	_ VPCAPI = vpcAdapter{}
)

func runtimeOptions() *dara.RuntimeOptions { return &dara.RuntimeOptions{} }

func (a ecsAdapter) DescribeRegions(ctx context.Context, in *ecs.DescribeRegionsRequest) (*ecs.DescribeRegionsResponseBody, error) {
	r, err := a.c.DescribeRegionsWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeRegionsResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeZones(ctx context.Context, in *ecs.DescribeZonesRequest) (*ecs.DescribeZonesResponseBody, error) {
	r, err := a.c.DescribeZonesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeZonesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeInstanceTypes(ctx context.Context, in *ecs.DescribeInstanceTypesRequest) (*ecs.DescribeInstanceTypesResponseBody, error) {
	r, err := a.c.DescribeInstanceTypesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeInstanceTypesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeAvailableResource(ctx context.Context, in *ecs.DescribeAvailableResourceRequest) (*ecs.DescribeAvailableResourceResponseBody, error) {
	r, err := a.c.DescribeAvailableResourceWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeAvailableResourceResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeAccountAttributes(ctx context.Context, in *ecs.DescribeAccountAttributesRequest) (*ecs.DescribeAccountAttributesResponseBody, error) {
	r, err := a.c.DescribeAccountAttributesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeAccountAttributesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeImages(ctx context.Context, in *ecs.DescribeImagesRequest) (*ecs.DescribeImagesResponseBody, error) {
	r, err := a.c.DescribeImagesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeImagesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) RunInstances(ctx context.Context, in *ecs.RunInstancesRequest) (*ecs.RunInstancesResponseBody, error) {
	r, err := a.c.RunInstancesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.RunInstancesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeInstances(ctx context.Context, in *ecs.DescribeInstancesRequest) (*ecs.DescribeInstancesResponseBody, error) {
	r, err := a.c.DescribeInstancesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeInstancesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DeleteInstance(ctx context.Context, in *ecs.DeleteInstanceRequest) (*ecs.DeleteInstanceResponseBody, error) {
	r, err := a.c.DeleteInstanceWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DeleteInstanceResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeSecurityGroups(ctx context.Context, in *ecs.DescribeSecurityGroupsRequest) (*ecs.DescribeSecurityGroupsResponseBody, error) {
	r, err := a.c.DescribeSecurityGroupsWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeSecurityGroupsResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) CreateSecurityGroup(ctx context.Context, in *ecs.CreateSecurityGroupRequest) (*ecs.CreateSecurityGroupResponseBody, error) {
	r, err := a.c.CreateSecurityGroupWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.CreateSecurityGroupResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) AuthorizeSecurityGroup(ctx context.Context, in *ecs.AuthorizeSecurityGroupRequest) (*ecs.AuthorizeSecurityGroupResponseBody, error) {
	r, err := a.c.AuthorizeSecurityGroupWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.AuthorizeSecurityGroupResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) RevokeSecurityGroup(ctx context.Context, in *ecs.RevokeSecurityGroupRequest) (*ecs.RevokeSecurityGroupResponseBody, error) {
	r, err := a.c.RevokeSecurityGroupWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.RevokeSecurityGroupResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DeleteSecurityGroup(ctx context.Context, in *ecs.DeleteSecurityGroupRequest) (*ecs.DeleteSecurityGroupResponseBody, error) {
	r, err := a.c.DeleteSecurityGroupWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DeleteSecurityGroupResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeSecurityGroupAttribute(ctx context.Context, in *ecs.DescribeSecurityGroupAttributeRequest) (*ecs.DescribeSecurityGroupAttributeResponseBody, error) {
	r, err := a.c.DescribeSecurityGroupAttributeWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeSecurityGroupAttributeResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) CreateDeploymentSet(ctx context.Context, in *ecs.CreateDeploymentSetRequest) (*ecs.CreateDeploymentSetResponseBody, error) {
	r, err := a.c.CreateDeploymentSetWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.CreateDeploymentSetResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DescribeDeploymentSets(ctx context.Context, in *ecs.DescribeDeploymentSetsRequest) (*ecs.DescribeDeploymentSetsResponseBody, error) {
	r, err := a.c.DescribeDeploymentSetsWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DescribeDeploymentSetsResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) DeleteDeploymentSet(ctx context.Context, in *ecs.DeleteDeploymentSetRequest) (*ecs.DeleteDeploymentSetResponseBody, error) {
	r, err := a.c.DeleteDeploymentSetWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.DeleteDeploymentSetResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) TagResources(ctx context.Context, in *ecs.TagResourcesRequest) (*ecs.TagResourcesResponseBody, error) {
	r, err := a.c.TagResourcesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.TagResourcesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a ecsAdapter) ListTagResources(ctx context.Context, in *ecs.ListTagResourcesRequest) (*ecs.ListTagResourcesResponseBody, error) {
	r, err := a.c.ListTagResourcesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &ecs.ListTagResourcesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) DescribeVpcs(ctx context.Context, in *vpc.DescribeVpcsRequest) (*vpc.DescribeVpcsResponseBody, error) {
	r, err := a.c.DescribeVpcsWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.DescribeVpcsResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) CreateVpc(ctx context.Context, in *vpc.CreateVpcRequest) (*vpc.CreateVpcResponseBody, error) {
	r, err := a.c.CreateVpcWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.CreateVpcResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) DeleteVpc(ctx context.Context, in *vpc.DeleteVpcRequest) (*vpc.DeleteVpcResponseBody, error) {
	r, err := a.c.DeleteVpcWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.DeleteVpcResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) DescribeVSwitches(ctx context.Context, in *vpc.DescribeVSwitchesRequest) (*vpc.DescribeVSwitchesResponseBody, error) {
	r, err := a.c.DescribeVSwitchesWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.DescribeVSwitchesResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) CreateVSwitch(ctx context.Context, in *vpc.CreateVSwitchRequest) (*vpc.CreateVSwitchResponseBody, error) {
	r, err := a.c.CreateVSwitchWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.CreateVSwitchResponseBody{}, nil
	}
	return r.Body, nil
}

func (a vpcAdapter) DeleteVSwitch(ctx context.Context, in *vpc.DeleteVSwitchRequest) (*vpc.DeleteVSwitchResponseBody, error) {
	r, err := a.c.DeleteVSwitchWithContext(ctx, in, runtimeOptions())
	if err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return &vpc.DeleteVSwitchResponseBody{}, nil
	}
	return r.Body, nil
}
