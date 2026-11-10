/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// The read-only guard of the AWS canary. The canary runs the driver's own read paths with
// credentials that cannot write; this guard is the second wall: the driver is given clients
// that refuse every call that is not a Describe, Get or List before it reaches the SDK. The
// wrappers implement awsdriver.EC2API method by method (a method added to the interface
// breaks the build here), the write methods below are generated from it by name, and
// guard_test.go checks by reflection that every method of the interface is either passed
// through as a read or refused.

// errReadOnly is what a refused call returns.
var errReadOnly = errors.New("the canary is read-only: refusing a write call")

// isRead reports whether an operation name (without the service) is a read.
func isRead(op string) bool {
	return strings.HasPrefix(op, "Describe") || strings.HasPrefix(op, "Get") || strings.HasPrefix(op, "List")
}

// audit records the operations the canary made and the writes it was refused.
type audit struct {
	mu      sync.Mutex
	reads   map[string]int
	refused []string
}

func newAudit() *audit { return &audit{reads: map[string]int{}} }

func (a *audit) read(action string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads[action]++
}

func (a *audit) deny(action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refused = append(a.refused, action)
	return fmt.Errorf("%w: %s", errReadOnly, action)
}

// actions returns the IAM actions the canary read, sorted.
func (a *audit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.reads))
	for k := range a.reads {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// refusals returns the writes that were attempted (and refused).
func (a *audit) refusals() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.refused)
}

// readOnly wraps a client factory: every client it returns is guarded.
func readOnly(inner awsdriver.ClientFactory, a *audit) awsdriver.ClientFactory {
	return func(ctx context.Context, region string) (awsdriver.Clients, error) {
		c, err := inner(ctx, region)
		if err != nil {
			return awsdriver.Clients{}, err
		}
		return awsdriver.Clients{EC2: &roEC2{inner: c.EC2, audit: a}, Quotas: &roQuotas{inner: c.Quotas, audit: a}}, nil
	}
}

type roQuotas struct {
	inner awsdriver.ServiceQuotasAPI
	audit *audit
}

func (g *roQuotas) GetServiceQuota(
	ctx context.Context, in *servicequotas.GetServiceQuotaInput, opts ...func(*servicequotas.Options),
) (*servicequotas.GetServiceQuotaOutput, error) {
	g.audit.read("servicequotas:GetServiceQuota")
	return g.inner.GetServiceQuota(ctx, in, opts...)
}

func (g *roQuotas) ListServiceQuotas(
	ctx context.Context, in *servicequotas.ListServiceQuotasInput, opts ...func(*servicequotas.Options),
) (*servicequotas.ListServiceQuotasOutput, error) {
	g.audit.read("servicequotas:ListServiceQuotas")
	return g.inner.ListServiceQuotas(ctx, in, opts...)
}

type roEC2 struct {
	inner awsdriver.EC2API
	audit *audit
}

var (
	_ awsdriver.EC2API           = (*roEC2)(nil)
	_ awsdriver.ServiceQuotasAPI = (*roQuotas)(nil)
)

func (g *roEC2) DescribeRegions(
	ctx context.Context, in *ec2.DescribeRegionsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeRegionsOutput, error) {
	g.audit.read("ec2:DescribeRegions")
	return g.inner.DescribeRegions(ctx, in, opts...)
}

func (g *roEC2) DescribeAvailabilityZones(
	ctx context.Context, in *ec2.DescribeAvailabilityZonesInput, opts ...func(*ec2.Options),
) (*ec2.DescribeAvailabilityZonesOutput, error) {
	g.audit.read("ec2:DescribeAvailabilityZones")
	return g.inner.DescribeAvailabilityZones(ctx, in, opts...)
}

func (g *roEC2) DescribeImages(
	ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options),
) (*ec2.DescribeImagesOutput, error) {
	g.audit.read("ec2:DescribeImages")
	return g.inner.DescribeImages(ctx, in, opts...)
}

func (g *roEC2) DescribeSubnets(
	ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeSubnetsOutput, error) {
	g.audit.read("ec2:DescribeSubnets")
	return g.inner.DescribeSubnets(ctx, in, opts...)
}

func (g *roEC2) DescribeVpcs(
	ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeVpcsOutput, error) {
	g.audit.read("ec2:DescribeVpcs")
	return g.inner.DescribeVpcs(ctx, in, opts...)
}

func (g *roEC2) DescribeSecurityGroups(
	ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeSecurityGroupsOutput, error) {
	g.audit.read("ec2:DescribeSecurityGroups")
	return g.inner.DescribeSecurityGroups(ctx, in, opts...)
}

func (g *roEC2) CreateSecurityGroup(
	ctx context.Context, in *ec2.CreateSecurityGroupInput, opts ...func(*ec2.Options),
) (*ec2.CreateSecurityGroupOutput, error) {
	return nil, g.audit.deny("ec2:CreateSecurityGroup")
}

func (g *roEC2) AuthorizeSecurityGroupIngress(
	ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, opts ...func(*ec2.Options),
) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return nil, g.audit.deny("ec2:AuthorizeSecurityGroupIngress")
}

func (g *roEC2) RevokeSecurityGroupIngress(
	ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, opts ...func(*ec2.Options),
) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return nil, g.audit.deny("ec2:RevokeSecurityGroupIngress")
}

func (g *roEC2) DeleteSecurityGroup(
	ctx context.Context, in *ec2.DeleteSecurityGroupInput, opts ...func(*ec2.Options),
) (*ec2.DeleteSecurityGroupOutput, error) {
	return nil, g.audit.deny("ec2:DeleteSecurityGroup")
}

func (g *roEC2) CreatePlacementGroup(
	ctx context.Context, in *ec2.CreatePlacementGroupInput, opts ...func(*ec2.Options),
) (*ec2.CreatePlacementGroupOutput, error) {
	return nil, g.audit.deny("ec2:CreatePlacementGroup")
}

func (g *roEC2) DeletePlacementGroup(
	ctx context.Context, in *ec2.DeletePlacementGroupInput, opts ...func(*ec2.Options),
) (*ec2.DeletePlacementGroupOutput, error) {
	return nil, g.audit.deny("ec2:DeletePlacementGroup")
}

func (g *roEC2) DescribePlacementGroups(
	ctx context.Context, in *ec2.DescribePlacementGroupsInput, opts ...func(*ec2.Options),
) (*ec2.DescribePlacementGroupsOutput, error) {
	g.audit.read("ec2:DescribePlacementGroups")
	return g.inner.DescribePlacementGroups(ctx, in, opts...)
}

func (g *roEC2) CreateVpc(
	ctx context.Context, in *ec2.CreateVpcInput, opts ...func(*ec2.Options),
) (*ec2.CreateVpcOutput, error) {
	return nil, g.audit.deny("ec2:CreateVpc")
}

func (g *roEC2) DeleteVpc(
	ctx context.Context, in *ec2.DeleteVpcInput, opts ...func(*ec2.Options),
) (*ec2.DeleteVpcOutput, error) {
	return nil, g.audit.deny("ec2:DeleteVpc")
}

func (g *roEC2) CreateSubnet(
	ctx context.Context, in *ec2.CreateSubnetInput, opts ...func(*ec2.Options),
) (*ec2.CreateSubnetOutput, error) {
	return nil, g.audit.deny("ec2:CreateSubnet")
}

func (g *roEC2) DeleteSubnet(
	ctx context.Context, in *ec2.DeleteSubnetInput, opts ...func(*ec2.Options),
) (*ec2.DeleteSubnetOutput, error) {
	return nil, g.audit.deny("ec2:DeleteSubnet")
}

func (g *roEC2) CreateInternetGateway(
	ctx context.Context, in *ec2.CreateInternetGatewayInput, opts ...func(*ec2.Options),
) (*ec2.CreateInternetGatewayOutput, error) {
	return nil, g.audit.deny("ec2:CreateInternetGateway")
}

func (g *roEC2) DeleteInternetGateway(
	ctx context.Context, in *ec2.DeleteInternetGatewayInput, opts ...func(*ec2.Options),
) (*ec2.DeleteInternetGatewayOutput, error) {
	return nil, g.audit.deny("ec2:DeleteInternetGateway")
}

func (g *roEC2) AttachInternetGateway(
	ctx context.Context, in *ec2.AttachInternetGatewayInput, opts ...func(*ec2.Options),
) (*ec2.AttachInternetGatewayOutput, error) {
	return nil, g.audit.deny("ec2:AttachInternetGateway")
}

func (g *roEC2) DetachInternetGateway(
	ctx context.Context, in *ec2.DetachInternetGatewayInput, opts ...func(*ec2.Options),
) (*ec2.DetachInternetGatewayOutput, error) {
	return nil, g.audit.deny("ec2:DetachInternetGateway")
}

func (g *roEC2) DescribeInternetGateways(
	ctx context.Context, in *ec2.DescribeInternetGatewaysInput, opts ...func(*ec2.Options),
) (*ec2.DescribeInternetGatewaysOutput, error) {
	g.audit.read("ec2:DescribeInternetGateways")
	return g.inner.DescribeInternetGateways(ctx, in, opts...)
}

func (g *roEC2) CreateRouteTable(
	ctx context.Context, in *ec2.CreateRouteTableInput, opts ...func(*ec2.Options),
) (*ec2.CreateRouteTableOutput, error) {
	return nil, g.audit.deny("ec2:CreateRouteTable")
}

func (g *roEC2) DeleteRouteTable(
	ctx context.Context, in *ec2.DeleteRouteTableInput, opts ...func(*ec2.Options),
) (*ec2.DeleteRouteTableOutput, error) {
	return nil, g.audit.deny("ec2:DeleteRouteTable")
}

func (g *roEC2) CreateRoute(
	ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options),
) (*ec2.CreateRouteOutput, error) {
	return nil, g.audit.deny("ec2:CreateRoute")
}

func (g *roEC2) AssociateVpcCidrBlock(
	ctx context.Context, in *ec2.AssociateVpcCidrBlockInput, opts ...func(*ec2.Options),
) (*ec2.AssociateVpcCidrBlockOutput, error) {
	return nil, g.audit.deny("ec2:AssociateVpcCidrBlock")
}

func (g *roEC2) AssociateSubnetCidrBlock(
	ctx context.Context, in *ec2.AssociateSubnetCidrBlockInput, opts ...func(*ec2.Options),
) (*ec2.AssociateSubnetCidrBlockOutput, error) {
	return nil, g.audit.deny("ec2:AssociateSubnetCidrBlock")
}

func (g *roEC2) ReplaceRoute(
	ctx context.Context, in *ec2.ReplaceRouteInput, opts ...func(*ec2.Options),
) (*ec2.ReplaceRouteOutput, error) {
	return nil, g.audit.deny("ec2:ReplaceRoute")
}

func (g *roEC2) AssociateRouteTable(
	ctx context.Context, in *ec2.AssociateRouteTableInput, opts ...func(*ec2.Options),
) (*ec2.AssociateRouteTableOutput, error) {
	return nil, g.audit.deny("ec2:AssociateRouteTable")
}

func (g *roEC2) DisassociateRouteTable(
	ctx context.Context, in *ec2.DisassociateRouteTableInput, opts ...func(*ec2.Options),
) (*ec2.DisassociateRouteTableOutput, error) {
	return nil, g.audit.deny("ec2:DisassociateRouteTable")
}

func (g *roEC2) DescribeRouteTables(
	ctx context.Context, in *ec2.DescribeRouteTablesInput, opts ...func(*ec2.Options),
) (*ec2.DescribeRouteTablesOutput, error) {
	g.audit.read("ec2:DescribeRouteTables")
	return g.inner.DescribeRouteTables(ctx, in, opts...)
}

func (g *roEC2) RunInstances(
	ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options),
) (*ec2.RunInstancesOutput, error) {
	return nil, g.audit.deny("ec2:RunInstances")
}

func (g *roEC2) DescribeInstances(
	ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options),
) (*ec2.DescribeInstancesOutput, error) {
	g.audit.read("ec2:DescribeInstances")
	return g.inner.DescribeInstances(ctx, in, opts...)
}

func (g *roEC2) TerminateInstances(
	ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options),
) (*ec2.TerminateInstancesOutput, error) {
	return nil, g.audit.deny("ec2:TerminateInstances")
}

func (g *roEC2) DescribeInstanceTypes(
	ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options),
) (*ec2.DescribeInstanceTypesOutput, error) {
	g.audit.read("ec2:DescribeInstanceTypes")
	return g.inner.DescribeInstanceTypes(ctx, in, opts...)
}

func (g *roEC2) DescribeInstanceTypeOfferings(
	ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	g.audit.read("ec2:DescribeInstanceTypeOfferings")
	return g.inner.DescribeInstanceTypeOfferings(ctx, in, opts...)
}

func (g *roEC2) DescribeSpotInstanceRequests(
	ctx context.Context, in *ec2.DescribeSpotInstanceRequestsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	g.audit.read("ec2:DescribeSpotInstanceRequests")
	return g.inner.DescribeSpotInstanceRequests(ctx, in, opts...)
}

func (g *roEC2) DescribeCapacityReservations(
	ctx context.Context, in *ec2.DescribeCapacityReservationsInput, opts ...func(*ec2.Options),
) (*ec2.DescribeCapacityReservationsOutput, error) {
	g.audit.read("ec2:DescribeCapacityReservations")
	return g.inner.DescribeCapacityReservations(ctx, in, opts...)
}
