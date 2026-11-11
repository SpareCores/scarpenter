/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/marketplaceordering/armmarketplaceordering"
	marketfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/marketplaceordering/armmarketplaceordering/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	networkfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	resourcesfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	subsfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions/fake"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// token is a credential that needs no network.
type token struct{}

func (token) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// router sends each request to the SDK fake server of its service.
type router struct{ compute, network, resources, subs, market policy.Transporter }

func (r router) Do(req *http.Request) (*http.Response, error) {
	p := req.URL.Path
	switch {
	case strings.Contains(p, "/providers/Microsoft.MarketplaceOrdering/"):
		return r.market.Do(req)
	case strings.Contains(p, "/providers/Microsoft.Compute/"):
		return r.compute.Do(req)
	case strings.Contains(p, "/providers/Microsoft.Network/"):
		return r.network.Do(req)
	case strings.HasSuffix(p, "/locations"):
		return r.subs.Do(req)
	default:
		return r.resources.Do(req)
	}
}

func sdkClients(t *testing.T, cf computefake.ServerFactory, nf networkfake.ServerFactory, rs resourcesfake.Server, ss subsfake.Server) Clients {
	t.Helper()
	cl, err := NewSDKClients(token{}, "sub-1", SDKOptions{
		ApplicationID: "scarpenter/test", PollInterval: time.Millisecond,
		ClientOptions: &azcore.ClientOptions{
			Transport: router{
				compute:   computefake.NewServerFactoryTransport(&cf),
				network:   networkfake.NewServerFactoryTransport(&nf),
				resources: resourcesfake.NewServerTransport(&rs),
				subs:      subsfake.NewServerTransport(&ss),
			},
			Retry: policy.RetryOptions{MaxRetries: -1},
		},
	})
	require.NoError(t, err)
	return cl
}

func TestSDKVMs(t *testing.T) {
	var gotExpand *armcompute.InstanceViewTypes
	var gotForce *bool
	cf := computefake.ServerFactory{VirtualMachinesServer: computefake.VirtualMachinesServer{
		Get: func(_ context.Context, rg, name string, o *armcompute.VirtualMachinesClientGetOptions) (azfake.Responder[armcompute.VirtualMachinesClientGetResponse], azfake.ErrorResponder) {
			var r azfake.Responder[armcompute.VirtualMachinesClientGetResponse]
			var e azfake.ErrorResponder
			if name == "missing" {
				e.SetResponseError(http.StatusNotFound, "ResourceNotFound")
				return r, e
			}
			if o != nil {
				gotExpand = o.Expand
			}
			r.SetResponse(http.StatusOK, armcompute.VirtualMachinesClientGetResponse{VirtualMachine: armcompute.VirtualMachine{Name: &name}}, nil)
			return r, e
		},
		BeginCreateOrUpdate: func(_ context.Context, rg, name string, vm armcompute.VirtualMachine, _ *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse], azfake.ErrorResponder) {
			var p azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse]
			var e azfake.ErrorResponder
			if name == "full" {
				// The failure comes inside the long-running operation.
				p.AddNonTerminalResponse(http.StatusCreated, nil)
				p.SetTerminalError(http.StatusConflict, "ZonalAllocationFailed")
				return p, e
			}
			p.AddNonTerminalResponse(http.StatusCreated, nil)
			p.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachinesClientCreateOrUpdateResponse{VirtualMachine: vm}, nil)
			return p, e
		},
		BeginDelete: func(_ context.Context, _, _ string, o *armcompute.VirtualMachinesClientBeginDeleteOptions) (azfake.PollerResponder[armcompute.VirtualMachinesClientDeleteResponse], azfake.ErrorResponder) {
			if o != nil {
				gotForce = o.ForceDeletion
			}
			var p azfake.PollerResponder[armcompute.VirtualMachinesClientDeleteResponse]
			p.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachinesClientDeleteResponse{}, nil)
			return p, azfake.ErrorResponder{}
		},
		NewListAllPager: func(*armcompute.VirtualMachinesClientListAllOptions) azfake.PagerResponder[armcompute.VirtualMachinesClientListAllResponse] {
			var p azfake.PagerResponder[armcompute.VirtualMachinesClientListAllResponse]
			for _, names := range [][]string{{"a", "b"}, {"c"}} {
				var vms []*armcompute.VirtualMachine
				for _, n := range names {
					vms = append(vms, &armcompute.VirtualMachine{Name: new(n)})
				}
				p.AddPage(http.StatusOK, armcompute.VirtualMachinesClientListAllResponse{VirtualMachineListResult: armcompute.VirtualMachineListResult{Value: vms}}, nil)
			}
			return p
		},
	}}
	c := sdkClients(t, cf, networkfake.ServerFactory{}, resourcesfake.Server{}, subsfake.Server{})
	ctx := context.Background()

	vm, err := c.VMs.Get(ctx, "rg", "vm", true)
	require.NoError(t, err)
	require.Equal(t, "vm", *vm.Name)
	require.Equal(t, armcompute.InstanceViewTypesInstanceView, *gotExpand)
	_, err = c.VMs.Get(ctx, "rg", "missing", false)
	require.True(t, isNotFound(err))
	require.Equal(t, vendors.NotFound, vendors.ClassOf(classify(err, errContext{})))

	out, err := c.VMs.CreateOrUpdate(ctx, "rg", "ok", armcompute.VirtualMachine{Location: new("westeurope")})
	require.NoError(t, err)
	require.Equal(t, "westeurope", *out.Location)
	_, err = c.VMs.CreateOrUpdate(ctx, "rg", "full", armcompute.VirtualMachine{})
	require.Equal(t, vendors.InsufficientCapacity, vendors.ClassOf(classify(err, errContext{})), "the poller's error carries the ARM code")
	require.Equal(t, "ZonalAllocationFailed", vendors.CodeOf(classify(err, errContext{})))

	require.NoError(t, c.VMs.Delete(ctx, "rg", "vm", true))
	require.True(t, *gotForce)

	all, err := c.VMs.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3, "every page is read")
}

func TestSDKSKUsUsagesAndLocations(t *testing.T) {
	var filter string
	cf := computefake.ServerFactory{
		ResourceSKUsServer: computefake.ResourceSKUsServer{NewListPager: func(o *armcompute.ResourceSKUsClientListOptions) azfake.PagerResponder[armcompute.ResourceSKUsClientListResponse] {
			filter = *o.Filter
			var p azfake.PagerResponder[armcompute.ResourceSKUsClientListResponse]
			for _, n := range []string{"Standard_D2s_v5", "Standard_D4s_v5"} {
				p.AddPage(http.StatusOK, armcompute.ResourceSKUsClientListResponse{ResourceSKUsResult: armcompute.ResourceSKUsResult{
					Value: []*armcompute.ResourceSKU{{Name: new(n)}}}}, nil)
			}
			return p
		}},
		UsageServer: computefake.UsageServer{NewListPager: func(loc string, _ *armcompute.UsageClientListOptions) azfake.PagerResponder[armcompute.UsageClientListResponse] {
			var p azfake.PagerResponder[armcompute.UsageClientListResponse]
			p.AddPage(http.StatusOK, armcompute.UsageClientListResponse{ListUsagesResult: armcompute.ListUsagesResult{Value: []*armcompute.Usage{
				{Name: &armcompute.UsageName{Value: new("cores")}, CurrentValue: new(int32(3)), Limit: new(int64(10))}}}}, nil)
			return p
		}},
	}
	nf := networkfake.ServerFactory{UsagesServer: networkfake.UsagesServer{NewListPager: func(string, *armnetwork.UsagesClientListOptions) azfake.PagerResponder[armnetwork.UsagesClientListResponse] {
		var p azfake.PagerResponder[armnetwork.UsagesClientListResponse]
		p.AddPage(http.StatusOK, armnetwork.UsagesClientListResponse{UsagesListResult: armnetwork.UsagesListResult{Value: []*armnetwork.Usage{
			{Name: &armnetwork.UsageName{Value: new("StandardPublicIPAddresses")}, CurrentValue: new(int64(1)), Limit: new(int64(20))}}}}, nil)
		return p
	}}}
	ss := subsfake.Server{NewListLocationsPager: func(sub string, _ *armsubscriptions.ClientListLocationsOptions) azfake.PagerResponder[armsubscriptions.ClientListLocationsResponse] {
		var p azfake.PagerResponder[armsubscriptions.ClientListLocationsResponse]
		p.AddPage(http.StatusOK, armsubscriptions.ClientListLocationsResponse{LocationListResult: armsubscriptions.LocationListResult{Value: []*armsubscriptions.Location{{Name: new("westeurope")}}}}, nil)
		return p
	}}
	c := sdkClients(t, cf, nf, resourcesfake.Server{}, ss)
	ctx := context.Background()

	skus, err := c.SKUs.List(ctx, "westeurope")
	require.NoError(t, err)
	require.Len(t, skus, 2)
	require.Equal(t, "location eq 'westeurope'", filter)

	cu, err := c.Usages.ComputeUsages(ctx, "westeurope")
	require.NoError(t, err)
	require.EqualValues(t, 3, *cu[0].CurrentValue)
	nu, err := c.Usages.NetworkUsages(ctx, "westeurope")
	require.NoError(t, err)
	require.EqualValues(t, 20, *nu[0].Limit)
	locs, err := c.Locations.List(ctx)
	require.NoError(t, err)
	require.Equal(t, "westeurope", *locs[0].Name)

	// The quota reader runs on the real adapters too.
	snap, err := NewQuotaReader(c.Usages).Quotas(ctx, "westeurope")
	require.NoError(t, err)
	require.Len(t, snap.Buckets, 2)
}

func TestSDKTagListingAndImages(t *testing.T) {
	var filter string
	rs := resourcesfake.Server{NewListPager: func(o *armresources.ClientListOptions) azfake.PagerResponder[armresources.ClientListResponse] {
		filter = *o.Filter
		var p azfake.PagerResponder[armresources.ClientListResponse]
		p.AddPage(http.StatusOK, armresources.ClientListResponse{ResourceListResult: armresources.ResourceListResult{Value: []*armresources.GenericResourceExpanded{{Name: new("a")}}}}, nil)
		p.AddPage(http.StatusOK, armresources.ClientListResponse{ResourceListResult: armresources.ResourceListResult{Value: []*armresources.GenericResourceExpanded{{Name: new("b")}}}}, nil)
		return p
	}}
	cf := computefake.ServerFactory{VirtualMachineImagesServer: computefake.VirtualMachineImagesServer{
		Get: func(_ context.Context, loc, pub, offer, sku, version string, _ *armcompute.VirtualMachineImagesClientGetOptions) (azfake.Responder[armcompute.VirtualMachineImagesClientGetResponse], azfake.ErrorResponder) {
			var r azfake.Responder[armcompute.VirtualMachineImagesClientGetResponse]
			var e azfake.ErrorResponder
			if version == "0.0.0" {
				e.SetResponseError(http.StatusNotFound, "PlatformImageNotFound")
				return r, e
			}
			r.SetResponse(http.StatusOK, armcompute.VirtualMachineImagesClientGetResponse{VirtualMachineImage: armcompute.VirtualMachineImage{Name: &version}}, nil)
			return r, e
		}}}
	c := sdkClients(t, cf, networkfake.ServerFactory{}, rs, subsfake.Server{})
	ctx := context.Background()

	res, err := c.Resources.ListByTag(ctx, "scarpenter-cluster", "c1")
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.Equal(t, "tagName eq 'scarpenter-cluster' and tagValue eq 'c1'", filter)

	img, err := c.Images.Get(ctx, "westeurope", "kinvolk", "o", "s", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", *img.Name)
	_, err = c.Images.Get(ctx, "westeurope", "kinvolk", "o", "s", "0.0.0")
	require.True(t, isNotFound(err))
	require.Equal(t, vendors.NotFound, vendors.ClassOf(classify(err, errContext{})))
}

func TestSDKAgreements(t *testing.T) {
	var gotType armmarketplaceordering.OfferType
	srv := marketfake.MarketplaceAgreementsServer{
		Get: func(_ context.Context, offerType armmarketplaceordering.OfferType, pub, offer, plan string, _ *armmarketplaceordering.MarketplaceAgreementsClientGetOptions) (azfake.Responder[armmarketplaceordering.MarketplaceAgreementsClientGetResponse], azfake.ErrorResponder) {
			gotType = offerType
			var r azfake.Responder[armmarketplaceordering.MarketplaceAgreementsClientGetResponse]
			var e azfake.ErrorResponder
			switch plan {
			case "missing":
				e.SetResponseError(http.StatusNotFound, "NotFound")
			case "denied":
				e.SetResponseError(http.StatusForbidden, "AuthorizationFailed")
			default:
				// Terms never accepted are a 200 with accepted=false.
				r.SetResponse(http.StatusOK, armmarketplaceordering.MarketplaceAgreementsClientGetResponse{AgreementTerms: armmarketplaceordering.AgreementTerms{
					Properties: &armmarketplaceordering.AgreementProperties{Accepted: new(plan == "accepted")},
				}}, nil)
			}
			return r, e
		},
	}
	cl, err := NewSDKClients(token{}, "sub-1", SDKOptions{ClientOptions: &azcore.ClientOptions{
		Transport: router{market: marketfake.NewMarketplaceAgreementsServerTransport(&srv)},
		Retry:     policy.RetryOptions{MaxRetries: -1},
	}})
	require.NoError(t, err)
	ctx := context.Background()
	for plan, want := range map[string]bool{"accepted": true, "never": false, "missing": false} {
		ok, err := cl.Agreements.Accepted(ctx, "kinvolk", "offer", plan)
		require.NoError(t, err, plan)
		require.Equal(t, want, ok, plan)
	}
	require.Equal(t, armmarketplaceordering.OfferTypeVirtualmachine, gotType)
	_, err = cl.Agreements.Accepted(ctx, "kinvolk", "offer", "denied")
	require.Equal(t, vendors.Fatal, vendors.ClassOf(classify(err, errContext{})))
}

func TestNewSDKClientsRejectsNoCredential(t *testing.T) {
	_, err := NewSDKClients(nil, "sub-1", SDKOptions{})
	require.Error(t, err)
	_, err = NewSDKClients(token{}, "", SDKOptions{})
	require.Error(t, err)
}
