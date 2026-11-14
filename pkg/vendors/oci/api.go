/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the interface repeats the SDK method signatures
package oci

import (
	"context"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"
)

// The narrow interfaces of the OCI services the driver uses (plan 25, section 5):
// the SDK's generated clients are concrete structs with one client per region,
// so each method takes the region first and the SDK's own request and response
// types after it. The SDK adapter (SDK) implements them, per-region clients and
// per-call deadlines included; the in-memory fake (pkg/vendors/oci/fake) too.
// Every method has a statement in docs/reference/oci-iam-policy.txt, which
// iam_test.go checks.

// ComputeAPI is the part of the Compute service the driver calls.
type ComputeAPI interface {
	// LaunchInstance starts an instance. It is never retried by a retry policy
	// (plan 25, finding 7): an HTTP 500 "Out of host capacity" must reach the
	// driver, which classifies it.
	LaunchInstance(ctx context.Context, region string, req core.LaunchInstanceRequest,
	) (core.LaunchInstanceResponse, error)
	GetInstance(ctx context.Context, region string, req core.GetInstanceRequest) (core.GetInstanceResponse, error)
	ListInstances(ctx context.Context, region string, req core.ListInstancesRequest,
	) (core.ListInstancesResponse, error)
	TerminateInstance(ctx context.Context, region string, req core.TerminateInstanceRequest,
	) (core.TerminateInstanceResponse, error)
	ListVnicAttachments(ctx context.Context, region string, req core.ListVnicAttachmentsRequest,
	) (core.ListVnicAttachmentsResponse, error)
	ListShapes(ctx context.Context, region string, req core.ListShapesRequest) (core.ListShapesResponse, error)
	ListImages(ctx context.Context, region string, req core.ListImagesRequest) (core.ListImagesResponse, error)
	GetImage(ctx context.Context, region string, req core.GetImageRequest) (core.GetImageResponse, error)
	ListImageShapeCompatibilityEntries(ctx context.Context, region string, req core.ListImageShapeCompatibilityEntriesRequest) (core.ListImageShapeCompatibilityEntriesResponse, error)
}

// NetworkAPI is the part of the Virtual Network service the driver calls: the
// VNICs of the instances, the subnets and network security groups it resolves,
// and the VCN, internet gateway, route table, security list, subnet and group it
// creates and deletes for a managed network.
type NetworkAPI interface {
	GetVnic(ctx context.Context, region string, req core.GetVnicRequest) (core.GetVnicResponse, error)
	GetSubnet(ctx context.Context, region string, req core.GetSubnetRequest) (core.GetSubnetResponse, error)
	ListSubnets(ctx context.Context, region string, req core.ListSubnetsRequest) (core.ListSubnetsResponse, error)
	CreateSubnet(ctx context.Context, region string, req core.CreateSubnetRequest) (core.CreateSubnetResponse, error)
	DeleteSubnet(ctx context.Context, region string, req core.DeleteSubnetRequest) (core.DeleteSubnetResponse, error)
	ListVcns(ctx context.Context, region string, req core.ListVcnsRequest) (core.ListVcnsResponse, error)
	CreateVcn(ctx context.Context, region string, req core.CreateVcnRequest) (core.CreateVcnResponse, error)
	DeleteVcn(ctx context.Context, region string, req core.DeleteVcnRequest) (core.DeleteVcnResponse, error)
	ListInternetGateways(ctx context.Context, region string, req core.ListInternetGatewaysRequest) (core.ListInternetGatewaysResponse, error)
	CreateInternetGateway(ctx context.Context, region string, req core.CreateInternetGatewayRequest) (core.CreateInternetGatewayResponse, error)
	DeleteInternetGateway(ctx context.Context, region string, req core.DeleteInternetGatewayRequest) (core.DeleteInternetGatewayResponse, error)
	ListRouteTables(ctx context.Context, region string, req core.ListRouteTablesRequest) (core.ListRouteTablesResponse, error)
	CreateRouteTable(ctx context.Context, region string, req core.CreateRouteTableRequest) (core.CreateRouteTableResponse, error)
	DeleteRouteTable(ctx context.Context, region string, req core.DeleteRouteTableRequest) (core.DeleteRouteTableResponse, error)
	ListSecurityLists(ctx context.Context, region string, req core.ListSecurityListsRequest) (core.ListSecurityListsResponse, error)
	CreateSecurityList(ctx context.Context, region string, req core.CreateSecurityListRequest) (core.CreateSecurityListResponse, error)
	DeleteSecurityList(ctx context.Context, region string, req core.DeleteSecurityListRequest) (core.DeleteSecurityListResponse, error)
	GetNetworkSecurityGroup(ctx context.Context, region string, req core.GetNetworkSecurityGroupRequest) (core.GetNetworkSecurityGroupResponse, error)
	ListNetworkSecurityGroups(ctx context.Context, region string, req core.ListNetworkSecurityGroupsRequest) (core.ListNetworkSecurityGroupsResponse, error)
	CreateNetworkSecurityGroup(ctx context.Context, region string, req core.CreateNetworkSecurityGroupRequest) (core.CreateNetworkSecurityGroupResponse, error)
	DeleteNetworkSecurityGroup(ctx context.Context, region string, req core.DeleteNetworkSecurityGroupRequest) (core.DeleteNetworkSecurityGroupResponse, error)
	AddNetworkSecurityGroupSecurityRules(ctx context.Context, region string, req core.AddNetworkSecurityGroupSecurityRulesRequest) (core.AddNetworkSecurityGroupSecurityRulesResponse, error)
	ListNetworkSecurityGroupSecurityRules(ctx context.Context, region string, req core.ListNetworkSecurityGroupSecurityRulesRequest) (core.ListNetworkSecurityGroupSecurityRulesResponse, error)
	ListNetworkSecurityGroupVnics(ctx context.Context, region string, req core.ListNetworkSecurityGroupVnicsRequest) (core.ListNetworkSecurityGroupVnicsResponse, error)
}

// IdentityAPI is the part of the Identity service the driver calls.
type IdentityAPI interface {
	// ListRegionSubscriptions is answered by the home region's endpoint.
	ListRegionSubscriptions(ctx context.Context, req identity.ListRegionSubscriptionsRequest,
	) (identity.ListRegionSubscriptionsResponse, error)
	ListAvailabilityDomains(ctx context.Context, region string, req identity.ListAvailabilityDomainsRequest,
	) (identity.ListAvailabilityDomainsResponse, error)
}

// LimitsAPI is the part of the Limits service the driver calls.
type LimitsAPI interface {
	GetResourceAvailability(ctx context.Context, region string, req limits.GetResourceAvailabilityRequest,
	) (limits.GetResourceAvailabilityResponse, error)
	ListLimitDefinitions(ctx context.Context, region string, req limits.ListLimitDefinitionsRequest,
	) (limits.ListLimitDefinitionsResponse, error)
}

// Clients bundles the four services; the SDK adapter and the fake each provide
// all of them.
type Clients struct {
	Compute  ComputeAPI
	Network  NetworkAPI
	Identity IdentityAPI
	Limits   LimitsAPI
}
