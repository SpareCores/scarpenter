/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:prealloc,lll,dupl,gocyclo,goconst // table-heavy tests: small expectations, repeated fixtures, long SDK signatures
package azure

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	networkfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	resourcesfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	subsfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions/fake"
	"github.com/stretchr/testify/require"
)

func respond[T any](v T) azfake.Responder[T] {
	var r azfake.Responder[T]
	r.SetResponse(http.StatusOK, v, nil)
	return r
}

func done[T any](v T) azfake.PollerResponder[T] {
	var p azfake.PollerResponder[T]
	p.AddNonTerminalResponse(http.StatusAccepted, nil)
	p.SetTerminalResponse(http.StatusOK, v, nil)
	return p
}

func created[T any](v T) azfake.PollerResponder[T] {
	var p azfake.PollerResponder[T]
	p.AddNonTerminalResponse(http.StatusCreated, nil)
	p.SetTerminalResponse(http.StatusOK, v, nil)
	return p
}

func pages[T any](vs ...T) azfake.PagerResponder[T] {
	var p azfake.PagerResponder[T]
	for _, v := range vs {
		p.AddPage(http.StatusOK, v, nil)
	}
	return p
}

func TestSDKResourceGroups(t *testing.T) {
	var deleted, listedGroup string
	rg := resourcesfake.ServerFactory{
		ResourceGroupsServer: resourcesfake.ResourceGroupsServer{
			Get: func(_ context.Context, name string, _ *armresources.ResourceGroupsClientGetOptions) (azfake.Responder[armresources.ResourceGroupsClientGetResponse], azfake.ErrorResponder) {
				return respond(armresources.ResourceGroupsClientGetResponse{ResourceGroup: armresources.ResourceGroup{Name: &name}}), azfake.ErrorResponder{}
			},
			CreateOrUpdate: func(_ context.Context, name string, g armresources.ResourceGroup, _ *armresources.ResourceGroupsClientCreateOrUpdateOptions) (azfake.Responder[armresources.ResourceGroupsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
				g.Name = &name
				return respond(armresources.ResourceGroupsClientCreateOrUpdateResponse{ResourceGroup: g}), azfake.ErrorResponder{}
			},
			BeginDelete: func(_ context.Context, name string, _ *armresources.ResourceGroupsClientBeginDeleteOptions) (azfake.PollerResponder[armresources.ResourceGroupsClientDeleteResponse], azfake.ErrorResponder) {
				deleted = name
				return done(armresources.ResourceGroupsClientDeleteResponse{}), azfake.ErrorResponder{}
			},
			NewListPager: func(*armresources.ResourceGroupsClientListOptions) azfake.PagerResponder[armresources.ResourceGroupsClientListResponse] {
				return pages(
					armresources.ResourceGroupsClientListResponse{ResourceGroupListResult: armresources.ResourceGroupListResult{Value: []*armresources.ResourceGroup{{Name: new("a")}}}},
					armresources.ResourceGroupsClientListResponse{ResourceGroupListResult: armresources.ResourceGroupListResult{Value: []*armresources.ResourceGroup{{Name: new("b")}}}})
			},
		},
		Server: resourcesfake.Server{NewListByResourceGroupPager: func(rg string, _ *armresources.ClientListByResourceGroupOptions) azfake.PagerResponder[armresources.ClientListByResourceGroupResponse] {
			listedGroup = rg
			return pages(armresources.ClientListByResourceGroupResponse{ResourceListResult: armresources.ResourceListResult{Value: []*armresources.GenericResourceExpanded{{Name: new("vm1")}}}})
		}},
	}
	c := sdkClientsWith(t, computefake.ServerFactory{}, networkfake.ServerFactory{}, resourcesfake.NewServerFactoryTransport(&rg))
	ctx := context.Background()
	g, err := c.Groups.Get(ctx, "rg")
	require.NoError(t, err)
	require.Equal(t, "rg", *g.Name)
	g, err = c.Groups.CreateOrUpdate(ctx, "new", armresources.ResourceGroup{Location: new("westeurope")})
	require.NoError(t, err)
	require.Equal(t, "new", *g.Name)
	require.NoError(t, c.Groups.Delete(ctx, "old"))
	require.Equal(t, "old", deleted)
	all, err := c.Groups.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	res, err := c.Groups.Resources(ctx, "rg")
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, "rg", listedGroup)
}

func TestSDKNetworkResources(t *testing.T) {
	var deleted []string
	nf := networkfake.ServerFactory{
		VirtualNetworksServer: networkfake.VirtualNetworksServer{
			Get: func(_ context.Context, rg, name string, _ *armnetwork.VirtualNetworksClientGetOptions) (azfake.Responder[armnetwork.VirtualNetworksClientGetResponse], azfake.ErrorResponder) {
				return respond(armnetwork.VirtualNetworksClientGetResponse{VirtualNetwork: armnetwork.VirtualNetwork{Name: &name}}), azfake.ErrorResponder{}
			},
			BeginCreateOrUpdate: func(_ context.Context, rg, name string, v armnetwork.VirtualNetwork, _ *armnetwork.VirtualNetworksClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armnetwork.VirtualNetworksClientCreateOrUpdateResponse], azfake.ErrorResponder) {
				v.Name = &name
				return created(armnetwork.VirtualNetworksClientCreateOrUpdateResponse{VirtualNetwork: v}), azfake.ErrorResponder{}
			},
			BeginDelete: func(_ context.Context, rg, name string, _ *armnetwork.VirtualNetworksClientBeginDeleteOptions) (azfake.PollerResponder[armnetwork.VirtualNetworksClientDeleteResponse], azfake.ErrorResponder) {
				deleted = append(deleted, "vnet/"+name)
				return done(armnetwork.VirtualNetworksClientDeleteResponse{}), azfake.ErrorResponder{}
			},
			NewListAllPager: func(*armnetwork.VirtualNetworksClientListAllOptions) azfake.PagerResponder[armnetwork.VirtualNetworksClientListAllResponse] {
				return pages(armnetwork.VirtualNetworksClientListAllResponse{VirtualNetworkListResult: armnetwork.VirtualNetworkListResult{Value: []*armnetwork.VirtualNetwork{{Name: new("v1")}, {Name: new("v2")}}}})
			},
		},
		SecurityGroupsServer: networkfake.SecurityGroupsServer{
			Get: func(_ context.Context, rg, name string, _ *armnetwork.SecurityGroupsClientGetOptions) (azfake.Responder[armnetwork.SecurityGroupsClientGetResponse], azfake.ErrorResponder) {
				return respond(armnetwork.SecurityGroupsClientGetResponse{SecurityGroup: armnetwork.SecurityGroup{Name: &name}}), azfake.ErrorResponder{}
			},
			BeginCreateOrUpdate: func(_ context.Context, rg, name string, n armnetwork.SecurityGroup, _ *armnetwork.SecurityGroupsClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armnetwork.SecurityGroupsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
				n.Name = &name
				return created(armnetwork.SecurityGroupsClientCreateOrUpdateResponse{SecurityGroup: n}), azfake.ErrorResponder{}
			},
			BeginDelete: func(_ context.Context, rg, name string, _ *armnetwork.SecurityGroupsClientBeginDeleteOptions) (azfake.PollerResponder[armnetwork.SecurityGroupsClientDeleteResponse], azfake.ErrorResponder) {
				deleted = append(deleted, "nsg/"+name)
				return done(armnetwork.SecurityGroupsClientDeleteResponse{}), azfake.ErrorResponder{}
			},
			NewListAllPager: func(*armnetwork.SecurityGroupsClientListAllOptions) azfake.PagerResponder[armnetwork.SecurityGroupsClientListAllResponse] {
				return pages(armnetwork.SecurityGroupsClientListAllResponse{SecurityGroupListResult: armnetwork.SecurityGroupListResult{Value: []*armnetwork.SecurityGroup{{Name: new("n1")}}}})
			},
		},
		ApplicationSecurityGroupsServer: networkfake.ApplicationSecurityGroupsServer{
			Get: func(_ context.Context, rg, name string, _ *armnetwork.ApplicationSecurityGroupsClientGetOptions) (azfake.Responder[armnetwork.ApplicationSecurityGroupsClientGetResponse], azfake.ErrorResponder) {
				return respond(armnetwork.ApplicationSecurityGroupsClientGetResponse{ApplicationSecurityGroup: armnetwork.ApplicationSecurityGroup{Name: &name}}), azfake.ErrorResponder{}
			},
			BeginCreateOrUpdate: func(_ context.Context, rg, name string, a armnetwork.ApplicationSecurityGroup, _ *armnetwork.ApplicationSecurityGroupsClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armnetwork.ApplicationSecurityGroupsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
				a.Name = &name
				return created(armnetwork.ApplicationSecurityGroupsClientCreateOrUpdateResponse{ApplicationSecurityGroup: a}), azfake.ErrorResponder{}
			},
			BeginDelete: func(_ context.Context, rg, name string, _ *armnetwork.ApplicationSecurityGroupsClientBeginDeleteOptions) (azfake.PollerResponder[armnetwork.ApplicationSecurityGroupsClientDeleteResponse], azfake.ErrorResponder) {
				deleted = append(deleted, "asg/"+name)
				return done(armnetwork.ApplicationSecurityGroupsClientDeleteResponse{}), azfake.ErrorResponder{}
			},
			NewListAllPager: func(*armnetwork.ApplicationSecurityGroupsClientListAllOptions) azfake.PagerResponder[armnetwork.ApplicationSecurityGroupsClientListAllResponse] {
				return pages(armnetwork.ApplicationSecurityGroupsClientListAllResponse{ApplicationSecurityGroupListResult: armnetwork.ApplicationSecurityGroupListResult{Value: []*armnetwork.ApplicationSecurityGroup{{Name: new("a1")}}}})
			},
		},
		InterfacesServer: networkfake.InterfacesServer{
			NewListPager: func(rg string, _ *armnetwork.InterfacesClientListOptions) azfake.PagerResponder[armnetwork.InterfacesClientListResponse] {
				return pages(armnetwork.InterfacesClientListResponse{InterfaceListResult: armnetwork.InterfaceListResult{Value: []*armnetwork.Interface{{Name: &rg}}}})
			},
		},
	}
	cf := computefake.ServerFactory{
		ProximityPlacementGroupsServer: computefake.ProximityPlacementGroupsServer{
			Get: func(_ context.Context, rg, name string, _ *armcompute.ProximityPlacementGroupsClientGetOptions) (azfake.Responder[armcompute.ProximityPlacementGroupsClientGetResponse], azfake.ErrorResponder) {
				return respond(armcompute.ProximityPlacementGroupsClientGetResponse{ProximityPlacementGroup: armcompute.ProximityPlacementGroup{Name: &name}}), azfake.ErrorResponder{}
			},
			CreateOrUpdate: func(_ context.Context, rg, name string, p armcompute.ProximityPlacementGroup, _ *armcompute.ProximityPlacementGroupsClientCreateOrUpdateOptions) (azfake.Responder[armcompute.ProximityPlacementGroupsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
				p.Name = &name
				return respond(armcompute.ProximityPlacementGroupsClientCreateOrUpdateResponse{ProximityPlacementGroup: p}), azfake.ErrorResponder{}
			},
			Delete: func(_ context.Context, rg, name string, _ *armcompute.ProximityPlacementGroupsClientDeleteOptions) (azfake.Responder[armcompute.ProximityPlacementGroupsClientDeleteResponse], azfake.ErrorResponder) {
				deleted = append(deleted, "ppg/"+name)
				return respond(armcompute.ProximityPlacementGroupsClientDeleteResponse{}), azfake.ErrorResponder{}
			},
			NewListBySubscriptionPager: func(*armcompute.ProximityPlacementGroupsClientListBySubscriptionOptions) azfake.PagerResponder[armcompute.ProximityPlacementGroupsClientListBySubscriptionResponse] {
				return pages(armcompute.ProximityPlacementGroupsClientListBySubscriptionResponse{ProximityPlacementGroupListResult: armcompute.ProximityPlacementGroupListResult{Value: []*armcompute.ProximityPlacementGroup{{Name: new("p1")}}}})
			},
		},
		VirtualMachineImagesServer: computefake.VirtualMachineImagesServer{
			ListSKUs: func(_ context.Context, loc, pub, offer string, _ *armcompute.VirtualMachineImagesClientListSKUsOptions) (azfake.Responder[armcompute.VirtualMachineImagesClientListSKUsResponse], azfake.ErrorResponder) {
				return respond(armcompute.VirtualMachineImagesClientListSKUsResponse{VirtualMachineImageResourceArray: []*armcompute.VirtualMachineImageResource{{Name: new("stable-gen2")}, {Name: new("lts2024-gen2")}}}), azfake.ErrorResponder{}
			},
			List: func(_ context.Context, loc, pub, offer, sku string, _ *armcompute.VirtualMachineImagesClientListOptions) (azfake.Responder[armcompute.VirtualMachineImagesClientListResponse], azfake.ErrorResponder) {
				return respond(armcompute.VirtualMachineImagesClientListResponse{VirtualMachineImageResourceArray: []*armcompute.VirtualMachineImageResource{{Name: new("1.0.0")}, {Name: new("2.0.0")}}}), azfake.ErrorResponder{}
			},
		},
	}
	c := sdkClients(t, cf, nf, resourcesfake.Server{}, subsfake.Server{})
	ctx := context.Background()

	v, err := c.VNets.Get(ctx, "rg", "v")
	require.NoError(t, err)
	require.Equal(t, "v", *v.Name)
	v, err = c.VNets.CreateOrUpdate(ctx, "rg", "v2", armnetwork.VirtualNetwork{})
	require.NoError(t, err)
	require.Equal(t, "v2", *v.Name)
	require.NoError(t, c.VNets.Delete(ctx, "rg", "v"))
	vs, err := c.VNets.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, vs, 2)

	n, err := c.NSGs.Get(ctx, "rg", "n")
	require.NoError(t, err)
	require.Equal(t, "n", *n.Name)
	n, err = c.NSGs.CreateOrUpdate(ctx, "rg", "n2", armnetwork.SecurityGroup{})
	require.NoError(t, err)
	require.Equal(t, "n2", *n.Name)
	require.NoError(t, c.NSGs.Delete(ctx, "rg", "n"))
	ns, err := c.NSGs.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, ns, 1)

	a, err := c.ASGs.Get(ctx, "rg", "a")
	require.NoError(t, err)
	require.Equal(t, "a", *a.Name)
	a, err = c.ASGs.CreateOrUpdate(ctx, "rg", "a2", armnetwork.ApplicationSecurityGroup{})
	require.NoError(t, err)
	require.Equal(t, "a2", *a.Name)
	require.NoError(t, c.ASGs.Delete(ctx, "rg", "a"))
	as, err := c.ASGs.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, as, 1)

	p, err := c.PPGs.Get(ctx, "rg", "p")
	require.NoError(t, err)
	require.Equal(t, "p", *p.Name)
	p, err = c.PPGs.CreateOrUpdate(ctx, "rg", "p2", armcompute.ProximityPlacementGroup{})
	require.NoError(t, err)
	require.Equal(t, "p2", *p.Name)
	require.NoError(t, c.PPGs.Delete(ctx, "rg", "p"))
	ps, err := c.PPGs.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, ps, 1)
	require.Equal(t, []string{"vnet/v", "nsg/n", "asg/a", "ppg/p"}, deleted)

	nics, err := c.NICs.ListInGroup(ctx, "my-rg")
	require.NoError(t, err)
	require.Equal(t, "my-rg", *nics[0].Name)

	skus, err := c.Images.ListSKUs(ctx, "westeurope", "kinvolk", "offer")
	require.NoError(t, err)
	require.Equal(t, []string{"stable-gen2", "lts2024-gen2"}, skus)
	versions, err := c.Images.ListVersions(ctx, "westeurope", "kinvolk", "offer", "sku")
	require.NoError(t, err)
	require.Equal(t, []string{"1.0.0", "2.0.0"}, versions)
	_ = armsubscriptions.Location{}
}

// sdkClientsWith is sdkClients with a resources transport of the caller's (the
// factory that serves resource groups as well as resources).
func sdkClientsWith(t *testing.T, cf computefake.ServerFactory, nf networkfake.ServerFactory, resources policy.Transporter) Clients {
	t.Helper()
	cl, err := NewSDKClients(token{}, "sub-1", SDKOptions{
		ApplicationID: "scarpenter/test", PollInterval: time.Millisecond,
		ClientOptions: &azcore.ClientOptions{
			Transport: router{
				compute:   computefake.NewServerFactoryTransport(&cf),
				network:   networkfake.NewServerFactoryTransport(&nf),
				resources: resources,
			},
			Retry: policy.RetryOptions{MaxRetries: -1},
		},
	})
	require.NoError(t, err)
	return cl
}
