/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the wrappers repeat the SDK method signatures
package main

import (
	"context"
	"os"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

// The read-only guard of the OCI canary (the audit and errReadOnly are aws_guard.go's). The
// canary runs the driver's own read paths with an identity that cannot write; this guard is the
// second wall: the driver is given clients that refuse every call that is not a get or a list
// before it reaches the SDK. The four wrappers implement oci.ComputeAPI, NetworkAPI, IdentityAPI
// and LimitsAPI method by method (a method added to an interface breaks the build here), and
// oci_guard_test.go checks by reflection that every method of the interfaces is either passed
// through as a read or refused, and that every method has the policy statement the audit and
// test/e2e-oci/canary-policy.sh speak in.

// osGetenv is the environment of the real canary (a variable, so that the tests can point it elsewhere).
var osGetenv = os.Getenv

// The statements several methods share.
const (
	ociReadNSG   = "read network-security-groups in compartment"
	ociManageNSG = "manage network-security-groups in compartment"
)

// ociStatements maps each method of the driver's API interfaces to the IAM policy statement
// its call needs ("<verb> <resource type> in compartment|tenancy"; the same table as
// pkg/vendors/oci/iam_test.go, UNVERIFIED like it, spike S12). The writes are listed too: a
// refusal is recorded with the statement it would have needed.
var ociStatements = map[string]string{
	"ComputeAPI.LaunchInstance":                        "manage instances in compartment",
	"ComputeAPI.GetInstance":                           "read instances in compartment",
	"ComputeAPI.ListInstances":                         "inspect instances in compartment",
	"ComputeAPI.TerminateInstance":                     "manage instances in compartment",
	"ComputeAPI.ListVnicAttachments":                   "read vnic-attachments in compartment",
	"ComputeAPI.ListShapes":                            "inspect instances in compartment",
	"ComputeAPI.ListImages":                            "inspect instance-images in compartment",
	"ComputeAPI.GetImage":                              "read instance-images in compartment",
	"ComputeAPI.ListImageShapeCompatibilityEntries":    "read instance-images in compartment",
	"NetworkAPI.GetVnic":                               "read vnics in compartment",
	"NetworkAPI.GetSubnet":                             "read subnets in compartment",
	"NetworkAPI.ListSubnets":                           "inspect subnets in compartment",
	"NetworkAPI.CreateSubnet":                          "manage subnets in compartment",
	"NetworkAPI.DeleteSubnet":                          "manage subnets in compartment",
	"NetworkAPI.ListVcns":                              "inspect vcns in compartment",
	"NetworkAPI.CreateVcn":                             "manage vcns in compartment",
	"NetworkAPI.DeleteVcn":                             "manage vcns in compartment",
	"NetworkAPI.ListInternetGateways":                  "inspect internet-gateways in compartment",
	"NetworkAPI.CreateInternetGateway":                 "manage internet-gateways in compartment",
	"NetworkAPI.DeleteInternetGateway":                 "manage internet-gateways in compartment",
	"NetworkAPI.ListRouteTables":                       "inspect route-tables in compartment",
	"NetworkAPI.CreateRouteTable":                      "manage route-tables in compartment",
	"NetworkAPI.DeleteRouteTable":                      "manage route-tables in compartment",
	"NetworkAPI.ListSecurityLists":                     "inspect security-lists in compartment",
	"NetworkAPI.CreateSecurityList":                    "manage security-lists in compartment",
	"NetworkAPI.DeleteSecurityList":                    "manage security-lists in compartment",
	"NetworkAPI.GetNetworkSecurityGroup":               ociReadNSG,
	"NetworkAPI.ListNetworkSecurityGroups":             "inspect network-security-groups in compartment",
	"NetworkAPI.CreateNetworkSecurityGroup":            ociManageNSG,
	"NetworkAPI.DeleteNetworkSecurityGroup":            ociManageNSG,
	"NetworkAPI.AddNetworkSecurityGroupSecurityRules":  ociManageNSG,
	"NetworkAPI.ListNetworkSecurityGroupSecurityRules": ociReadNSG,
	"NetworkAPI.ListNetworkSecurityGroupVnics":         ociReadNSG,
	"IdentityAPI.ListRegionSubscriptions":              "inspect tenancies in tenancy",
	"IdentityAPI.ListAvailabilityDomains":              "inspect compartments in tenancy",
	"LimitsAPI.GetResourceAvailability":                "inspect resource-availability in tenancy",
	"LimitsAPI.ListLimitDefinitions":                   "inspect limits in tenancy",
}

// isOCIRead reports whether a method of the API is a read: a get or a list. Everything else
// (Launch, Terminate, Create, Delete, Add, and any verb a later version of the interfaces adds)
// is refused.
func isOCIRead(method string) bool {
	return strings.HasPrefix(method, "Get") || strings.HasPrefix(method, "List")
}

// ociStatementOf is the statement the audit records for a method of a service; a method the
// table does not know is recorded as unmapped (and oci_guard_test.go fails on it).
func ociStatementOf(service, method string) string {
	if s, ok := ociStatements[service+"."+method]; ok {
		return s
	}
	return "unmapped:" + service + "." + method
}

// ociReadOnlyClients wraps the four clients: every call they make is guarded. The audit is the
// one of the AWS guard: its actions here are policy statements.
func ociReadOnlyClients(inner oci.Clients, a *audit) oci.Clients {
	return oci.Clients{
		Compute:  &ociROCompute{inner: inner.Compute, audit: a},
		Network:  &ociRONetwork{inner: inner.Network, audit: a},
		Identity: &ociROIdentity{inner: inner.Identity, audit: a},
		Limits:   &ociROLimits{inner: inner.Limits, audit: a},
	}
}

// ociROCompute is the Compute client that passes the reads on and refuses the writes.
type ociROCompute struct {
	inner oci.ComputeAPI
	audit *audit
}

var _ oci.ComputeAPI = (*ociROCompute)(nil)

func (g *ociROCompute) LaunchInstance(context.Context, string, core.LaunchInstanceRequest,
) (core.LaunchInstanceResponse, error) {
	return core.LaunchInstanceResponse{}, g.audit.deny(ociStatementOf("ComputeAPI", "LaunchInstance"))
}

func (g *ociROCompute) GetInstance(ctx context.Context, region string, req core.GetInstanceRequest,
) (core.GetInstanceResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "GetInstance"))
	return g.inner.GetInstance(ctx, region, req)
}

func (g *ociROCompute) ListInstances(ctx context.Context, region string, req core.ListInstancesRequest,
) (core.ListInstancesResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "ListInstances"))
	return g.inner.ListInstances(ctx, region, req)
}

func (g *ociROCompute) TerminateInstance(context.Context, string, core.TerminateInstanceRequest,
) (core.TerminateInstanceResponse, error) {
	return core.TerminateInstanceResponse{}, g.audit.deny(ociStatementOf("ComputeAPI", "TerminateInstance"))
}

func (g *ociROCompute) ListVnicAttachments(ctx context.Context, region string, req core.ListVnicAttachmentsRequest,
) (core.ListVnicAttachmentsResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "ListVnicAttachments"))
	return g.inner.ListVnicAttachments(ctx, region, req)
}

func (g *ociROCompute) ListShapes(ctx context.Context, region string, req core.ListShapesRequest,
) (core.ListShapesResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "ListShapes"))
	return g.inner.ListShapes(ctx, region, req)
}

func (g *ociROCompute) ListImages(ctx context.Context, region string, req core.ListImagesRequest,
) (core.ListImagesResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "ListImages"))
	return g.inner.ListImages(ctx, region, req)
}

func (g *ociROCompute) GetImage(ctx context.Context, region string, req core.GetImageRequest,
) (core.GetImageResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "GetImage"))
	return g.inner.GetImage(ctx, region, req)
}

func (g *ociROCompute) ListImageShapeCompatibilityEntries(ctx context.Context, region string, req core.ListImageShapeCompatibilityEntriesRequest,
) (core.ListImageShapeCompatibilityEntriesResponse, error) {
	g.audit.read(ociStatementOf("ComputeAPI", "ListImageShapeCompatibilityEntries"))
	return g.inner.ListImageShapeCompatibilityEntries(ctx, region, req)
}

// ociRONetwork is the Virtual Network client that passes the reads on and refuses the writes.
type ociRONetwork struct {
	inner oci.NetworkAPI
	audit *audit
}

var _ oci.NetworkAPI = (*ociRONetwork)(nil)

func (g *ociRONetwork) GetVnic(ctx context.Context, region string, req core.GetVnicRequest,
) (core.GetVnicResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "GetVnic"))
	return g.inner.GetVnic(ctx, region, req)
}

func (g *ociRONetwork) GetSubnet(ctx context.Context, region string, req core.GetSubnetRequest,
) (core.GetSubnetResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "GetSubnet"))
	return g.inner.GetSubnet(ctx, region, req)
}

func (g *ociRONetwork) ListSubnets(ctx context.Context, region string, req core.ListSubnetsRequest,
) (core.ListSubnetsResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListSubnets"))
	return g.inner.ListSubnets(ctx, region, req)
}

func (g *ociRONetwork) CreateSubnet(context.Context, string, core.CreateSubnetRequest,
) (core.CreateSubnetResponse, error) {
	return core.CreateSubnetResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateSubnet"))
}

func (g *ociRONetwork) DeleteSubnet(context.Context, string, core.DeleteSubnetRequest,
) (core.DeleteSubnetResponse, error) {
	return core.DeleteSubnetResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteSubnet"))
}

func (g *ociRONetwork) ListVcns(ctx context.Context, region string, req core.ListVcnsRequest,
) (core.ListVcnsResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListVcns"))
	return g.inner.ListVcns(ctx, region, req)
}

func (g *ociRONetwork) CreateVcn(context.Context, string, core.CreateVcnRequest,
) (core.CreateVcnResponse, error) {
	return core.CreateVcnResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateVcn"))
}

func (g *ociRONetwork) DeleteVcn(context.Context, string, core.DeleteVcnRequest,
) (core.DeleteVcnResponse, error) {
	return core.DeleteVcnResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteVcn"))
}

func (g *ociRONetwork) ListInternetGateways(ctx context.Context, region string, req core.ListInternetGatewaysRequest,
) (core.ListInternetGatewaysResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListInternetGateways"))
	return g.inner.ListInternetGateways(ctx, region, req)
}

func (g *ociRONetwork) CreateInternetGateway(context.Context, string, core.CreateInternetGatewayRequest,
) (core.CreateInternetGatewayResponse, error) {
	return core.CreateInternetGatewayResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateInternetGateway"))
}

func (g *ociRONetwork) DeleteInternetGateway(context.Context, string, core.DeleteInternetGatewayRequest,
) (core.DeleteInternetGatewayResponse, error) {
	return core.DeleteInternetGatewayResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteInternetGateway"))
}

func (g *ociRONetwork) ListRouteTables(ctx context.Context, region string, req core.ListRouteTablesRequest,
) (core.ListRouteTablesResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListRouteTables"))
	return g.inner.ListRouteTables(ctx, region, req)
}

func (g *ociRONetwork) CreateRouteTable(context.Context, string, core.CreateRouteTableRequest,
) (core.CreateRouteTableResponse, error) {
	return core.CreateRouteTableResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateRouteTable"))
}

func (g *ociRONetwork) DeleteRouteTable(context.Context, string, core.DeleteRouteTableRequest,
) (core.DeleteRouteTableResponse, error) {
	return core.DeleteRouteTableResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteRouteTable"))
}

func (g *ociRONetwork) ListSecurityLists(ctx context.Context, region string, req core.ListSecurityListsRequest,
) (core.ListSecurityListsResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListSecurityLists"))
	return g.inner.ListSecurityLists(ctx, region, req)
}

func (g *ociRONetwork) CreateSecurityList(context.Context, string, core.CreateSecurityListRequest,
) (core.CreateSecurityListResponse, error) {
	return core.CreateSecurityListResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateSecurityList"))
}

func (g *ociRONetwork) DeleteSecurityList(context.Context, string, core.DeleteSecurityListRequest,
) (core.DeleteSecurityListResponse, error) {
	return core.DeleteSecurityListResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteSecurityList"))
}

func (g *ociRONetwork) GetNetworkSecurityGroup(ctx context.Context, region string, req core.GetNetworkSecurityGroupRequest,
) (core.GetNetworkSecurityGroupResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "GetNetworkSecurityGroup"))
	return g.inner.GetNetworkSecurityGroup(ctx, region, req)
}

func (g *ociRONetwork) ListNetworkSecurityGroups(ctx context.Context, region string, req core.ListNetworkSecurityGroupsRequest,
) (core.ListNetworkSecurityGroupsResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListNetworkSecurityGroups"))
	return g.inner.ListNetworkSecurityGroups(ctx, region, req)
}

func (g *ociRONetwork) CreateNetworkSecurityGroup(context.Context, string, core.CreateNetworkSecurityGroupRequest,
) (core.CreateNetworkSecurityGroupResponse, error) {
	return core.CreateNetworkSecurityGroupResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "CreateNetworkSecurityGroup"))
}

func (g *ociRONetwork) DeleteNetworkSecurityGroup(context.Context, string, core.DeleteNetworkSecurityGroupRequest,
) (core.DeleteNetworkSecurityGroupResponse, error) {
	return core.DeleteNetworkSecurityGroupResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "DeleteNetworkSecurityGroup"))
}

func (g *ociRONetwork) AddNetworkSecurityGroupSecurityRules(context.Context, string, core.AddNetworkSecurityGroupSecurityRulesRequest,
) (core.AddNetworkSecurityGroupSecurityRulesResponse, error) {
	return core.AddNetworkSecurityGroupSecurityRulesResponse{}, g.audit.deny(ociStatementOf("NetworkAPI", "AddNetworkSecurityGroupSecurityRules"))
}

func (g *ociRONetwork) ListNetworkSecurityGroupSecurityRules(ctx context.Context, region string, req core.ListNetworkSecurityGroupSecurityRulesRequest,
) (core.ListNetworkSecurityGroupSecurityRulesResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListNetworkSecurityGroupSecurityRules"))
	return g.inner.ListNetworkSecurityGroupSecurityRules(ctx, region, req)
}

func (g *ociRONetwork) ListNetworkSecurityGroupVnics(ctx context.Context, region string, req core.ListNetworkSecurityGroupVnicsRequest,
) (core.ListNetworkSecurityGroupVnicsResponse, error) {
	g.audit.read(ociStatementOf("NetworkAPI", "ListNetworkSecurityGroupVnics"))
	return g.inner.ListNetworkSecurityGroupVnics(ctx, region, req)
}

// ociROIdentity is the Identity client that passes the reads on and refuses the writes.
type ociROIdentity struct {
	inner oci.IdentityAPI
	audit *audit
}

var _ oci.IdentityAPI = (*ociROIdentity)(nil)

func (g *ociROIdentity) ListRegionSubscriptions(ctx context.Context, req identity.ListRegionSubscriptionsRequest,
) (identity.ListRegionSubscriptionsResponse, error) {
	g.audit.read(ociStatementOf("IdentityAPI", "ListRegionSubscriptions"))
	return g.inner.ListRegionSubscriptions(ctx, req)
}

func (g *ociROIdentity) ListAvailabilityDomains(ctx context.Context, region string, req identity.ListAvailabilityDomainsRequest,
) (identity.ListAvailabilityDomainsResponse, error) {
	g.audit.read(ociStatementOf("IdentityAPI", "ListAvailabilityDomains"))
	return g.inner.ListAvailabilityDomains(ctx, region, req)
}

// ociROLimits is the Limits client that passes the reads on and refuses the writes.
type ociROLimits struct {
	inner oci.LimitsAPI
	audit *audit
}

var _ oci.LimitsAPI = (*ociROLimits)(nil)

func (g *ociROLimits) GetResourceAvailability(ctx context.Context, region string, req limits.GetResourceAvailabilityRequest,
) (limits.GetResourceAvailabilityResponse, error) {
	g.audit.read(ociStatementOf("LimitsAPI", "GetResourceAvailability"))
	return g.inner.GetResourceAvailability(ctx, region, req)
}

func (g *ociROLimits) ListLimitDefinitions(ctx context.Context, region string, req limits.ListLimitDefinitionsRequest,
) (limits.ListLimitDefinitionsResponse, error) {
	g.audit.read(ociStatementOf("LimitsAPI", "ListLimitDefinitions"))
	return g.inner.ListLimitDefinitions(ctx, region, req)
}
