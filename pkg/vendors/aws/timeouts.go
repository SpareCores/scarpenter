/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
)

// Per-call timeouts (plan 04 review N4): every vendor API call runs under its
// own deadline, so a hung connection cannot hold a launch, a garbage
// collection sweep or a quota refresh forever. The deadline covers the SDK's
// retries of one call; paginated calls get it per page. A call that runs into
// it fails with a Retryable error (see classify).
const (
	// DefaultCallTimeout bounds one API call.
	DefaultCallTimeout = 45 * time.Second
	// DefaultLaunchTimeout bounds RunInstances, which waits for the placement.
	DefaultLaunchTimeout = 90 * time.Second
)

// withTimeouts wraps the clients of a region so that every call has a deadline.
func withTimeouts(c Clients, call, launch time.Duration) Clients {
	return Clients{EC2: &timedEC2{api: c.EC2, call: call, launch: launch}, Quotas: &timedQuotas{api: c.Quotas, call: call}}
}

type timedEC2 struct {
	api    EC2API
	call   time.Duration
	launch time.Duration
}

func (t *timedEC2) ctx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

func (t *timedEC2) DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, opts ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeRegions(ctx, in, opts...)
}

func (t *timedEC2) DescribeAvailabilityZones(ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeAvailabilityZones(ctx, in, opts...)
}

func (t *timedEC2) DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeImages(ctx, in, opts...)
}

func (t *timedEC2) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeSubnets(ctx, in, opts...)
}

func (t *timedEC2) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeVpcs(ctx, in, opts...)
}

func (t *timedEC2) DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeSecurityGroups(ctx, in, opts...)
}

func (t *timedEC2) CreateSecurityGroup(ctx context.Context, in *ec2.CreateSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateSecurityGroup(ctx, in, opts...)
}

func (t *timedEC2) AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, opts ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.AuthorizeSecurityGroupIngress(ctx, in, opts...)
}

func (t *timedEC2) RevokeSecurityGroupIngress(ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, opts ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.RevokeSecurityGroupIngress(ctx, in, opts...)
}

func (t *timedEC2) DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeleteSecurityGroup(ctx, in, opts...)
}

func (t *timedEC2) CreatePlacementGroup(ctx context.Context, in *ec2.CreatePlacementGroupInput, opts ...func(*ec2.Options)) (*ec2.CreatePlacementGroupOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreatePlacementGroup(ctx, in, opts...)
}

func (t *timedEC2) DeletePlacementGroup(ctx context.Context, in *ec2.DeletePlacementGroupInput, opts ...func(*ec2.Options)) (*ec2.DeletePlacementGroupOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeletePlacementGroup(ctx, in, opts...)
}

func (t *timedEC2) DescribePlacementGroups(ctx context.Context, in *ec2.DescribePlacementGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribePlacementGroupsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribePlacementGroups(ctx, in, opts...)
}

func (t *timedEC2) CreateVpc(ctx context.Context, in *ec2.CreateVpcInput, opts ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateVpc(ctx, in, opts...)
}

func (t *timedEC2) DeleteVpc(ctx context.Context, in *ec2.DeleteVpcInput, opts ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeleteVpc(ctx, in, opts...)
}

func (t *timedEC2) CreateSubnet(ctx context.Context, in *ec2.CreateSubnetInput, opts ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateSubnet(ctx, in, opts...)
}

func (t *timedEC2) DeleteSubnet(ctx context.Context, in *ec2.DeleteSubnetInput, opts ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeleteSubnet(ctx, in, opts...)
}

func (t *timedEC2) CreateInternetGateway(ctx context.Context, in *ec2.CreateInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateInternetGateway(ctx, in, opts...)
}

func (t *timedEC2) DeleteInternetGateway(ctx context.Context, in *ec2.DeleteInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeleteInternetGateway(ctx, in, opts...)
}

func (t *timedEC2) AttachInternetGateway(ctx context.Context, in *ec2.AttachInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.AttachInternetGateway(ctx, in, opts...)
}

func (t *timedEC2) DetachInternetGateway(ctx context.Context, in *ec2.DetachInternetGatewayInput, opts ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DetachInternetGateway(ctx, in, opts...)
}

func (t *timedEC2) DescribeInternetGateways(ctx context.Context, in *ec2.DescribeInternetGatewaysInput, opts ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeInternetGateways(ctx, in, opts...)
}

func (t *timedEC2) CreateRouteTable(ctx context.Context, in *ec2.CreateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateRouteTable(ctx, in, opts...)
}

func (t *timedEC2) DeleteRouteTable(ctx context.Context, in *ec2.DeleteRouteTableInput, opts ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DeleteRouteTable(ctx, in, opts...)
}

func (t *timedEC2) CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.CreateRoute(ctx, in, opts...)
}

func (t *timedEC2) AssociateRouteTable(ctx context.Context, in *ec2.AssociateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.AssociateRouteTable(ctx, in, opts...)
}

func (t *timedEC2) DisassociateRouteTable(ctx context.Context, in *ec2.DisassociateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.DisassociateRouteTableOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DisassociateRouteTable(ctx, in, opts...)
}

func (t *timedEC2) DescribeRouteTables(ctx context.Context, in *ec2.DescribeRouteTablesInput, opts ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeRouteTables(ctx, in, opts...)
}

func (t *timedEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.launch)
	defer cancel()
	return t.api.RunInstances(ctx, in, opts...)
}

func (t *timedEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeInstances(ctx, in, opts...)
}

func (t *timedEC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.TerminateInstances(ctx, in, opts...)
}

func (t *timedEC2) DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeInstanceTypes(ctx, in, opts...)
}

func (t *timedEC2) DescribeInstanceTypeOfferings(ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeInstanceTypeOfferings(ctx, in, opts...)
}

func (t *timedEC2) DescribeSpotInstanceRequests(ctx context.Context, in *ec2.DescribeSpotInstanceRequestsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeSpotInstanceRequests(ctx, in, opts...)
}

func (t *timedEC2) DescribeCapacityReservations(ctx context.Context, in *ec2.DescribeCapacityReservationsInput, opts ...func(*ec2.Options)) (*ec2.DescribeCapacityReservationsOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.DescribeCapacityReservations(ctx, in, opts...)
}

func (t *timedEC2) ReplaceRoute(ctx context.Context, in *ec2.ReplaceRouteInput, opts ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	ctx, cancel := t.ctx(ctx, t.call)
	defer cancel()
	return t.api.ReplaceRoute(ctx, in, opts...)
}

type timedQuotas struct {
	api  ServiceQuotasAPI
	call time.Duration
}

func (t *timedQuotas) GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, opts ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, t.call)
	defer cancel()
	return t.api.GetServiceQuota(ctx, in, opts...)
}

func (t *timedQuotas) ListServiceQuotas(ctx context.Context, in *servicequotas.ListServiceQuotasInput, opts ...func(*servicequotas.Options)) (*servicequotas.ListServiceQuotasOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, t.call)
	defer cancel()
	return t.api.ListServiceQuotas(ctx, in, opts...)
}
