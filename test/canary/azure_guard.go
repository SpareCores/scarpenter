/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the signatures repeat the SDK types, whose names are long
package main

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"

	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// The read-only guard of the Azure canary, the counterpart of aws_guard.go: the driver is
// given clients that refuse every create, update and delete before it reaches the SDK,
// whatever the identity behind them can do. The wrappers implement the driver's narrow
// interfaces method by method and do not embed them, so a method added to an interface
// breaks the build here instead of passing through unguarded; azure_guard_test.go checks by
// reflection that every method is a known read (passed on, audited) or a known write
// (refused, never forwarded). The audit and the refusal error are the AWS guard's.

// readOnlyAzure wraps the clients of the subscription: every API of them is guarded.
func readOnlyAzure(c azure.Clients, a *audit) azure.Clients {
	return azure.Clients{
		VMs: &roVMs{c.VMs, a}, NICs: &roNICs{c.NICs, a}, PIPs: &roPIPs{c.PIPs, a}, Groups: &roGroups{c.Groups, a},
		VNets: &roVNets{c.VNets, a}, NSGs: &roNSGs{c.NSGs, a}, ASGs: &roASGs{c.ASGs, a}, PPGs: &roPPGs{c.PPGs, a},
		Resources: &roResources{c.Resources, a}, SKUs: &roSKUs{c.SKUs, a}, Usages: &roUsages{c.Usages, a},
		Images: &roAzureImages{c.Images, a}, Agreements: &roAgreements{c.Agreements, a}, Locations: &roLocations{c.Locations, a},
	}
}

var (
	_ azure.VMAPI        = (*roVMs)(nil)
	_ azure.NICAPI       = (*roNICs)(nil)
	_ azure.PIPAPI       = (*roPIPs)(nil)
	_ azure.GroupsAPI    = (*roGroups)(nil)
	_ azure.VNetAPI      = (*roVNets)(nil)
	_ azure.NSGAPI       = (*roNSGs)(nil)
	_ azure.ASGAPI       = (*roASGs)(nil)
	_ azure.PPGAPI       = (*roPPGs)(nil)
	_ azure.ResourcesAPI = (*roResources)(nil)
	_ azure.SKUAPI       = (*roSKUs)(nil)
	_ azure.UsageAPI     = (*roUsages)(nil)
	_ azure.ImageAPI     = (*roAzureImages)(nil)
	_ azure.AgreementAPI = (*roAgreements)(nil)
	_ azure.LocationAPI  = (*roLocations)(nil)
)

type roVMs struct {
	inner azure.VMAPI
	audit *audit
}

func (g *roVMs) Get(ctx context.Context, rg, name string, instanceView bool) (armcompute.VirtualMachine, error) {
	g.audit.read("VMs.Get")
	return g.inner.Get(ctx, rg, name, instanceView)
}

func (g *roVMs) CreateOrUpdate(_ context.Context, _, _ string, _ armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	return armcompute.VirtualMachine{}, g.audit.deny("VMs.CreateOrUpdate")
}

func (g *roVMs) Delete(_ context.Context, _, _ string, _ bool) error {
	return g.audit.deny("VMs.Delete")
}

func (g *roVMs) ListAll(ctx context.Context) ([]armcompute.VirtualMachine, error) {
	g.audit.read("VMs.ListAll")
	return g.inner.ListAll(ctx)
}

type roNICs struct {
	inner azure.NICAPI
	audit *audit
}

func (g *roNICs) Get(ctx context.Context, rg, name string) (armnetwork.Interface, error) {
	g.audit.read("NICs.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roNICs) CreateOrUpdate(_ context.Context, _, _ string, _ armnetwork.Interface) (armnetwork.Interface, error) {
	return armnetwork.Interface{}, g.audit.deny("NICs.CreateOrUpdate")
}

func (g *roNICs) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("NICs.Delete")
}

func (g *roNICs) ListInGroup(ctx context.Context, rg string) ([]armnetwork.Interface, error) {
	g.audit.read("NICs.ListInGroup")
	return g.inner.ListInGroup(ctx, rg)
}

type roPIPs struct {
	inner azure.PIPAPI
	audit *audit
}

func (g *roPIPs) Get(ctx context.Context, rg, name string) (armnetwork.PublicIPAddress, error) {
	g.audit.read("PIPs.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roPIPs) CreateOrUpdate(_ context.Context, _, _ string, _ armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error) {
	return armnetwork.PublicIPAddress{}, g.audit.deny("PIPs.CreateOrUpdate")
}

func (g *roPIPs) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("PIPs.Delete")
}

type roGroups struct {
	inner azure.GroupsAPI
	audit *audit
}

func (g *roGroups) Get(ctx context.Context, name string) (armresources.ResourceGroup, error) {
	g.audit.read("Groups.Get")
	return g.inner.Get(ctx, name)
}

func (g *roGroups) CreateOrUpdate(_ context.Context, _ string, _ armresources.ResourceGroup) (armresources.ResourceGroup, error) {
	return armresources.ResourceGroup{}, g.audit.deny("Groups.CreateOrUpdate")
}

func (g *roGroups) Delete(_ context.Context, _ string) error {
	return g.audit.deny("Groups.Delete")
}

func (g *roGroups) List(ctx context.Context) ([]armresources.ResourceGroup, error) {
	g.audit.read("Groups.List")
	return g.inner.List(ctx)
}

func (g *roGroups) Resources(ctx context.Context, name string) ([]*armresources.GenericResourceExpanded, error) {
	g.audit.read("Groups.Resources")
	return g.inner.Resources(ctx, name)
}

type roVNets struct {
	inner azure.VNetAPI
	audit *audit
}

func (g *roVNets) Get(ctx context.Context, rg, name string) (armnetwork.VirtualNetwork, error) {
	g.audit.read("VNets.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roVNets) CreateOrUpdate(_ context.Context, _, _ string, _ armnetwork.VirtualNetwork) (armnetwork.VirtualNetwork, error) {
	return armnetwork.VirtualNetwork{}, g.audit.deny("VNets.CreateOrUpdate")
}

func (g *roVNets) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("VNets.Delete")
}

func (g *roVNets) ListAll(ctx context.Context) ([]armnetwork.VirtualNetwork, error) {
	g.audit.read("VNets.ListAll")
	return g.inner.ListAll(ctx)
}

type roNSGs struct {
	inner azure.NSGAPI
	audit *audit
}

func (g *roNSGs) Get(ctx context.Context, rg, name string) (armnetwork.SecurityGroup, error) {
	g.audit.read("NSGs.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roNSGs) CreateOrUpdate(_ context.Context, _, _ string, _ armnetwork.SecurityGroup) (armnetwork.SecurityGroup, error) {
	return armnetwork.SecurityGroup{}, g.audit.deny("NSGs.CreateOrUpdate")
}

func (g *roNSGs) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("NSGs.Delete")
}

func (g *roNSGs) ListAll(ctx context.Context) ([]armnetwork.SecurityGroup, error) {
	g.audit.read("NSGs.ListAll")
	return g.inner.ListAll(ctx)
}

type roASGs struct {
	inner azure.ASGAPI
	audit *audit
}

func (g *roASGs) Get(ctx context.Context, rg, name string) (armnetwork.ApplicationSecurityGroup, error) {
	g.audit.read("ASGs.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roASGs) CreateOrUpdate(_ context.Context, _, _ string, _ armnetwork.ApplicationSecurityGroup) (armnetwork.ApplicationSecurityGroup, error) {
	return armnetwork.ApplicationSecurityGroup{}, g.audit.deny("ASGs.CreateOrUpdate")
}

func (g *roASGs) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("ASGs.Delete")
}

func (g *roASGs) ListAll(ctx context.Context) ([]armnetwork.ApplicationSecurityGroup, error) {
	g.audit.read("ASGs.ListAll")
	return g.inner.ListAll(ctx)
}

type roPPGs struct {
	inner azure.PPGAPI
	audit *audit
}

func (g *roPPGs) Get(ctx context.Context, rg, name string) (armcompute.ProximityPlacementGroup, error) {
	g.audit.read("PPGs.Get")
	return g.inner.Get(ctx, rg, name)
}

func (g *roPPGs) CreateOrUpdate(_ context.Context, _, _ string, _ armcompute.ProximityPlacementGroup) (armcompute.ProximityPlacementGroup, error) {
	return armcompute.ProximityPlacementGroup{}, g.audit.deny("PPGs.CreateOrUpdate")
}

func (g *roPPGs) Delete(_ context.Context, _, _ string) error {
	return g.audit.deny("PPGs.Delete")
}

func (g *roPPGs) ListAll(ctx context.Context) ([]armcompute.ProximityPlacementGroup, error) {
	g.audit.read("PPGs.ListAll")
	return g.inner.ListAll(ctx)
}

type roResources struct {
	inner azure.ResourcesAPI
	audit *audit
}

func (g *roResources) ListByTag(ctx context.Context, name, value string) ([]*armresources.GenericResourceExpanded, error) {
	g.audit.read("Resources.ListByTag")
	return g.inner.ListByTag(ctx, name, value)
}

type roSKUs struct {
	inner azure.SKUAPI
	audit *audit
}

func (g *roSKUs) List(ctx context.Context, location string) ([]*armcompute.ResourceSKU, error) {
	g.audit.read("SKUs.List")
	return g.inner.List(ctx, location)
}

type roUsages struct {
	inner azure.UsageAPI
	audit *audit
}

func (g *roUsages) ComputeUsages(ctx context.Context, location string) ([]*armcompute.Usage, error) {
	g.audit.read("Usages.ComputeUsages")
	return g.inner.ComputeUsages(ctx, location)
}

func (g *roUsages) NetworkUsages(ctx context.Context, location string) ([]*armnetwork.Usage, error) {
	g.audit.read("Usages.NetworkUsages")
	return g.inner.NetworkUsages(ctx, location)
}

type roAzureImages struct {
	inner azure.ImageAPI
	audit *audit
}

func (g *roAzureImages) Get(ctx context.Context, location, publisher, offer, sku, version string) (armcompute.VirtualMachineImage, error) {
	g.audit.read("Images.Get")
	return g.inner.Get(ctx, location, publisher, offer, sku, version)
}

func (g *roAzureImages) ListSKUs(ctx context.Context, location, publisher, offer string) ([]string, error) {
	g.audit.read("Images.ListSKUs")
	return g.inner.ListSKUs(ctx, location, publisher, offer)
}

func (g *roAzureImages) ListVersions(ctx context.Context, location, publisher, offer, sku string) ([]string, error) {
	g.audit.read("Images.ListVersions")
	return g.inner.ListVersions(ctx, location, publisher, offer, sku)
}

type roAgreements struct {
	inner azure.AgreementAPI
	audit *audit
}

func (g *roAgreements) Accepted(ctx context.Context, publisher, offer, plan string) (bool, error) {
	g.audit.read("Agreements.Accepted")
	return g.inner.Accepted(ctx, publisher, offer, plan)
}

type roLocations struct {
	inner azure.LocationAPI
	audit *audit
}

func (g *roLocations) List(ctx context.Context) ([]*armsubscriptions.Location, error) {
	g.audit.read("Locations.List")
	return g.inner.List(ctx)
}
