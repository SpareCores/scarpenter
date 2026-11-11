/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the SDK clients' method signatures are long
package azure

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/marketplaceordering/armmarketplaceordering"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
)

// pollInterval is how often a long-running operation is polled when the
// service gives no Retry-After (plan 17: 5 s).
const pollInterval = 5 * time.Second

// SDKOptions configure NewSDKClients.
type SDKOptions struct {
	// ApplicationID is the telemetry application ID (the user agent):
	// scarpenter/<version>.
	ApplicationID string
	// ClientOptions overrides the ARM client options (tests point the
	// transport at the SDK's fake servers); ApplicationID is applied on top.
	ClientOptions *azcore.ClientOptions
	// PollInterval overrides pollInterval (tests).
	PollInterval time.Duration
}

// NewSDKClients creates the real clients of a subscription. The credential
// comes from the caller (the chain of plan 17 section 2 is the driver's
// business); no call is made here.
func NewSDKClients(cred azcore.TokenCredential, subscriptionID string, opts SDKOptions) (Clients, error) {
	if cred == nil || subscriptionID == "" {
		return Clients{}, fmt.Errorf("a credential and a subscription ID are required")
	}
	co := azcore.ClientOptions{}
	if opts.ClientOptions != nil {
		co = *opts.ClientOptions
	}
	if opts.ApplicationID != "" {
		co.Telemetry.ApplicationID = opts.ApplicationID
	}
	armOpts := &arm.ClientOptions{ClientOptions: co}
	freq := opts.PollInterval
	if freq == 0 {
		freq = pollInterval
	}
	compute, err := armcompute.NewClientFactory(subscriptionID, cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("compute client: %w", err)
	}
	network, err := armnetwork.NewClientFactory(subscriptionID, cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("network client: %w", err)
	}
	res, err := armresources.NewClient(subscriptionID, cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("resources client: %w", err)
	}
	subs, err := armsubscriptions.NewClient(cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("subscriptions client: %w", err)
	}
	agr, err := armmarketplaceordering.NewMarketplaceAgreementsClient(subscriptionID, cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("marketplace ordering client: %w", err)
	}
	groups, err := armresources.NewResourceGroupsClient(subscriptionID, cred, armOpts)
	if err != nil {
		return Clients{}, fmt.Errorf("resource groups client: %w", err)
	}
	return Clients{
		VMs:        &sdkVMs{c: compute.NewVirtualMachinesClient(), freq: freq},
		NICs:       &sdkNICs{c: network.NewInterfacesClient(), freq: freq},
		PIPs:       &sdkPIPs{c: network.NewPublicIPAddressesClient(), freq: freq},
		Groups:     &sdkGroups{c: groups, res: res, freq: freq},
		VNets:      &sdkVNets{c: network.NewVirtualNetworksClient(), freq: freq},
		NSGs:       &sdkNSGs{c: network.NewSecurityGroupsClient(), freq: freq},
		ASGs:       &sdkASGs{c: network.NewApplicationSecurityGroupsClient(), freq: freq},
		PPGs:       &sdkPPGs{c: compute.NewProximityPlacementGroupsClient()},
		Resources:  &sdkResources{c: res},
		SKUs:       &sdkSKUs{c: compute.NewResourceSKUsClient()},
		Usages:     &sdkUsages{compute: compute.NewUsageClient(), network: network.NewUsagesClient()},
		Images:     &sdkImages{c: compute.NewVirtualMachineImagesClient()},
		Agreements: &sdkAgreements{c: agr},
		Locations:  &sdkLocations{c: subs, subscriptionID: subscriptionID},
	}, nil
}

// drain collects every page of a pager.
func drain[P any, T any](ctx context.Context, p *runtime.Pager[P], items func(P) []T) ([]T, error) {
	var out []T
	for p.More() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, items(page)...)
	}
	return out, nil
}

func pollOpts(freq time.Duration) *runtime.PollUntilDoneOptions {
	return &runtime.PollUntilDoneOptions{Frequency: freq}
}

type sdkVMs struct {
	c    *armcompute.VirtualMachinesClient
	freq time.Duration
}

func (s *sdkVMs) Get(ctx context.Context, rg, name string, instanceView bool) (armcompute.VirtualMachine, error) {
	var o *armcompute.VirtualMachinesClientGetOptions
	if instanceView {
		o = &armcompute.VirtualMachinesClientGetOptions{Expand: new(armcompute.InstanceViewTypesInstanceView)}
	}
	r, err := s.c.Get(ctx, rg, name, o)
	return r.VirtualMachine, err
}

func (s *sdkVMs) CreateOrUpdate(ctx context.Context, rg, name string, vm armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, vm, nil)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.VirtualMachine, err
}

func (s *sdkVMs) Delete(ctx context.Context, rg, name string, force bool) error {
	var o *armcompute.VirtualMachinesClientBeginDeleteOptions
	if force {
		o = &armcompute.VirtualMachinesClientBeginDeleteOptions{ForceDeletion: new(true)}
	}
	p, err := s.c.BeginDelete(ctx, rg, name, o)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkVMs) ListAll(ctx context.Context) ([]armcompute.VirtualMachine, error) {
	vms, err := drain(ctx, s.c.NewListAllPager(nil), func(p armcompute.VirtualMachinesClientListAllResponse) []*armcompute.VirtualMachine {
		return p.Value
	})
	return derefAll(vms), err
}

type sdkNICs struct {
	c    *armnetwork.InterfacesClient
	freq time.Duration
}

func (s *sdkNICs) Get(ctx context.Context, rg, name string) (armnetwork.Interface, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.Interface, err
}

func (s *sdkNICs) CreateOrUpdate(ctx context.Context, rg, name string, nic armnetwork.Interface) (armnetwork.Interface, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, nic, nil)
	if err != nil {
		return armnetwork.Interface{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.Interface, err
}

func (s *sdkNICs) Delete(ctx context.Context, rg, name string) error {
	p, err := s.c.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkNICs) ListInGroup(ctx context.Context, rg string) ([]armnetwork.Interface, error) {
	nics, err := drain(ctx, s.c.NewListPager(rg, nil),
		func(p armnetwork.InterfacesClientListResponse) []*armnetwork.Interface { return p.Value })
	return derefAll(nics), err
}

type sdkPIPs struct {
	c    *armnetwork.PublicIPAddressesClient
	freq time.Duration
}

func (s *sdkPIPs) Get(ctx context.Context, rg, name string) (armnetwork.PublicIPAddress, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.PublicIPAddress, err
}

func (s *sdkPIPs) CreateOrUpdate(ctx context.Context, rg, name string, pip armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, pip, nil)
	if err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.PublicIPAddress, err
}

func (s *sdkPIPs) Delete(ctx context.Context, rg, name string) error {
	p, err := s.c.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

type sdkGroups struct {
	c    *armresources.ResourceGroupsClient
	res  *armresources.Client
	freq time.Duration
}

func (s *sdkGroups) Get(ctx context.Context, name string) (armresources.ResourceGroup, error) {
	r, err := s.c.Get(ctx, name, nil)
	return r.ResourceGroup, err
}

func (s *sdkGroups) CreateOrUpdate(ctx context.Context, name string, rg armresources.ResourceGroup) (armresources.ResourceGroup, error) {
	r, err := s.c.CreateOrUpdate(ctx, name, rg, nil)
	return r.ResourceGroup, err
}

func (s *sdkGroups) Delete(ctx context.Context, name string) error {
	p, err := s.c.BeginDelete(ctx, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkGroups) List(ctx context.Context) ([]armresources.ResourceGroup, error) {
	gs, err := drain(ctx, s.c.NewListPager(nil),
		func(p armresources.ResourceGroupsClientListResponse) []*armresources.ResourceGroup { return p.Value })
	return derefAll(gs), err
}

func (s *sdkGroups) Resources(ctx context.Context, name string) ([]*armresources.GenericResourceExpanded, error) {
	return drain(ctx, s.res.NewListByResourceGroupPager(name, nil),
		func(p armresources.ClientListByResourceGroupResponse) []*armresources.GenericResourceExpanded {
			return p.Value
		})
}

type sdkVNets struct {
	c    *armnetwork.VirtualNetworksClient
	freq time.Duration
}

func (s *sdkVNets) Get(ctx context.Context, rg, name string) (armnetwork.VirtualNetwork, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.VirtualNetwork, err
}

func (s *sdkVNets) CreateOrUpdate(ctx context.Context, rg, name string, v armnetwork.VirtualNetwork) (armnetwork.VirtualNetwork, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, v, nil)
	if err != nil {
		return armnetwork.VirtualNetwork{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.VirtualNetwork, err
}

func (s *sdkVNets) Delete(ctx context.Context, rg, name string) error {
	p, err := s.c.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkVNets) ListAll(ctx context.Context) ([]armnetwork.VirtualNetwork, error) {
	vs, err := drain(ctx, s.c.NewListAllPager(nil),
		func(p armnetwork.VirtualNetworksClientListAllResponse) []*armnetwork.VirtualNetwork { return p.Value })
	return derefAll(vs), err
}

type sdkNSGs struct {
	c    *armnetwork.SecurityGroupsClient
	freq time.Duration
}

func (s *sdkNSGs) Get(ctx context.Context, rg, name string) (armnetwork.SecurityGroup, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.SecurityGroup, err
}

func (s *sdkNSGs) CreateOrUpdate(ctx context.Context, rg, name string, n armnetwork.SecurityGroup) (armnetwork.SecurityGroup, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, n, nil)
	if err != nil {
		return armnetwork.SecurityGroup{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.SecurityGroup, err
}

func (s *sdkNSGs) Delete(ctx context.Context, rg, name string) error {
	p, err := s.c.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkNSGs) ListAll(ctx context.Context) ([]armnetwork.SecurityGroup, error) {
	ns, err := drain(ctx, s.c.NewListAllPager(nil),
		func(p armnetwork.SecurityGroupsClientListAllResponse) []*armnetwork.SecurityGroup { return p.Value })
	return derefAll(ns), err
}

type sdkASGs struct {
	c    *armnetwork.ApplicationSecurityGroupsClient
	freq time.Duration
}

func (s *sdkASGs) Get(ctx context.Context, rg, name string) (armnetwork.ApplicationSecurityGroup, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.ApplicationSecurityGroup, err
}

func (s *sdkASGs) CreateOrUpdate(ctx context.Context, rg, name string, a armnetwork.ApplicationSecurityGroup) (armnetwork.ApplicationSecurityGroup, error) {
	p, err := s.c.BeginCreateOrUpdate(ctx, rg, name, a, nil)
	if err != nil {
		return armnetwork.ApplicationSecurityGroup{}, err
	}
	r, err := p.PollUntilDone(ctx, pollOpts(s.freq))
	return r.ApplicationSecurityGroup, err
}

func (s *sdkASGs) Delete(ctx context.Context, rg, name string) error {
	p, err := s.c.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(ctx, pollOpts(s.freq))
	return err
}

func (s *sdkASGs) ListAll(ctx context.Context) ([]armnetwork.ApplicationSecurityGroup, error) {
	as, err := drain(ctx, s.c.NewListAllPager(nil),
		func(p armnetwork.ApplicationSecurityGroupsClientListAllResponse) []*armnetwork.ApplicationSecurityGroup {
			return p.Value
		})
	return derefAll(as), err
}

type sdkPPGs struct {
	c *armcompute.ProximityPlacementGroupsClient
}

func (s *sdkPPGs) Get(ctx context.Context, rg, name string) (armcompute.ProximityPlacementGroup, error) {
	r, err := s.c.Get(ctx, rg, name, nil)
	return r.ProximityPlacementGroup, err
}

func (s *sdkPPGs) CreateOrUpdate(ctx context.Context, rg, name string, p armcompute.ProximityPlacementGroup) (armcompute.ProximityPlacementGroup, error) {
	r, err := s.c.CreateOrUpdate(ctx, rg, name, p, nil)
	return r.ProximityPlacementGroup, err
}

func (s *sdkPPGs) Delete(ctx context.Context, rg, name string) error {
	_, err := s.c.Delete(ctx, rg, name, nil)
	return err
}

func (s *sdkPPGs) ListAll(ctx context.Context) ([]armcompute.ProximityPlacementGroup, error) {
	ps, err := drain(ctx, s.c.NewListBySubscriptionPager(nil),
		func(p armcompute.ProximityPlacementGroupsClientListBySubscriptionResponse) []*armcompute.ProximityPlacementGroup {
			return p.Value
		})
	return derefAll(ps), err
}

type sdkResources struct{ c *armresources.Client }

func (s *sdkResources) ListByTag(ctx context.Context, name, value string) ([]*armresources.GenericResourceExpanded, error) {
	filter := fmt.Sprintf("tagName eq '%s' and tagValue eq '%s'", name, value)
	return drain(ctx, s.c.NewListPager(&armresources.ClientListOptions{Filter: &filter}),
		func(p armresources.ClientListResponse) []*armresources.GenericResourceExpanded { return p.Value })
}

type sdkSKUs struct {
	c *armcompute.ResourceSKUsClient
}

func (s *sdkSKUs) List(ctx context.Context, location string) ([]*armcompute.ResourceSKU, error) {
	filter := fmt.Sprintf("location eq '%s'", location)
	return drain(ctx, s.c.NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: &filter}),
		func(p armcompute.ResourceSKUsClientListResponse) []*armcompute.ResourceSKU { return p.Value })
}

type sdkUsages struct {
	compute *armcompute.UsageClient
	network *armnetwork.UsagesClient
}

func (s *sdkUsages) ComputeUsages(ctx context.Context, location string) ([]*armcompute.Usage, error) {
	return drain(ctx, s.compute.NewListPager(location, nil),
		func(p armcompute.UsageClientListResponse) []*armcompute.Usage { return p.Value })
}

func (s *sdkUsages) NetworkUsages(ctx context.Context, location string) ([]*armnetwork.Usage, error) {
	return drain(ctx, s.network.NewListPager(location, nil),
		func(p armnetwork.UsagesClientListResponse) []*armnetwork.Usage { return p.Value })
}

type sdkImages struct {
	c *armcompute.VirtualMachineImagesClient
}

func (s *sdkImages) Get(ctx context.Context, location, publisher, offer, sku, version string) (armcompute.VirtualMachineImage, error) {
	r, err := s.c.Get(ctx, location, publisher, offer, sku, version, nil)
	return r.VirtualMachineImage, err
}

func (s *sdkImages) ListSKUs(ctx context.Context, location, publisher, offer string) ([]string, error) {
	r, err := s.c.ListSKUs(ctx, location, publisher, offer, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.VirtualMachineImageResourceArray))
	for _, i := range r.VirtualMachineImageResourceArray {
		out = append(out, deref(i.Name))
	}
	return out, nil
}

func (s *sdkImages) ListVersions(ctx context.Context, location, publisher, offer, sku string) ([]string, error) {
	r, err := s.c.List(ctx, location, publisher, offer, sku, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.VirtualMachineImageResourceArray))
	for _, i := range r.VirtualMachineImageResourceArray {
		out = append(out, deref(i.Name))
	}
	return out, nil
}

type sdkAgreements struct {
	c *armmarketplaceordering.MarketplaceAgreementsClient
}

// Accepted reads the terms of the plan (MarketplaceAgreements.Get with the
// offer type virtualmachine, what `az vm image terms show` reads): it answers
// with accepted=false for terms never accepted. GetAgreement reads a signed
// agreement instead, which does not exist before the first acceptance. A 404
// is read as not accepted.
func (s *sdkAgreements) Accepted(ctx context.Context, publisher, offer, plan string) (bool, error) {
	r, err := s.c.Get(ctx, armmarketplaceordering.OfferTypeVirtualmachine, publisher, offer, plan, nil)
	if err != nil {
		if re, ok := responseError(err); ok && re.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return r.Properties != nil && deref(r.Properties.Accepted), nil
}

type sdkLocations struct {
	c              *armsubscriptions.Client
	subscriptionID string
}

func (s *sdkLocations) List(ctx context.Context) ([]*armsubscriptions.Location, error) {
	return drain(ctx, s.c.NewListLocationsPager(s.subscriptionID, nil),
		func(p armsubscriptions.ClientListLocationsResponse) []*armsubscriptions.Location { return p.Value })
}

var (
	_ VMAPI        = (*sdkVMs)(nil)
	_ NICAPI       = (*sdkNICs)(nil)
	_ PIPAPI       = (*sdkPIPs)(nil)
	_ GroupsAPI    = (*sdkGroups)(nil)
	_ VNetAPI      = (*sdkVNets)(nil)
	_ NSGAPI       = (*sdkNSGs)(nil)
	_ ASGAPI       = (*sdkASGs)(nil)
	_ PPGAPI       = (*sdkPPGs)(nil)
	_ ResourcesAPI = (*sdkResources)(nil)
	_ SKUAPI       = (*sdkSKUs)(nil)
	_ UsageAPI     = (*sdkUsages)(nil)
	_ ImageAPI     = (*sdkImages)(nil)
	_ AgreementAPI = (*sdkAgreements)(nil)
	_ LocationAPI  = (*sdkLocations)(nil)
)

// deref returns *p, the zero value for nil.
func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

func derefAll[T any](in []*T) []T {
	out := make([]T, 0, len(in))
	for _, p := range in {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}
