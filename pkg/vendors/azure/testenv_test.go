/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

const (
	testCluster = "scarpenter-test"
	testRG      = "rg-westeurope"
	testSubnet  = "/subscriptions/sub-1/resourceGroups/rg-westeurope/providers/Microsoft.Network/virtualNetworks/vnet/subnets/nodes"
	testNSG     = "/subscriptions/sub-1/resourceGroups/rg-westeurope/providers/Microsoft.Network/networkSecurityGroups/nodes"
	testRegion  = "westeurope"
)

// env is a fake subscription with the clients of the driver over it.
type env struct {
	sub     *fake.Subscription
	clients Clients
	c       *Compute
	imgs    *Images
}

// newEnv builds the subscription of the tests: two zonal locations and one
// without zones, a table of sizes (including the ones v1 drops), quotas,
// the Flatcar images and a resource group.
func newEnv(t *testing.T) *env {
	t.Helper()
	sub := fake.New("sub-1")
	for _, l := range []struct {
		name  string
		zonal bool
	}{{"westeurope", true}, {"northeurope", true}, {"westcentralus", false}} {
		sub.AddLocation(l.name, l.zonal)
		for _, k := range []fake.SKU{
			{Name: "Standard_D2s_v5", Family: "standardDSv5Family", VCPUs: 2},
			{Name: "Standard_D4s_v5", Family: "standardDSv5Family", VCPUs: 4},
			{Name: "Standard_D8s_v5", Family: "standardDSv5Family", VCPUs: 8, RestrictedZones: []string{"3"}},
			{Name: "Standard_B2ats_v2", Family: "standardBasv2Family", VCPUs: 2, NoSpot: true, NoAcceleratedNet: true},
			{Name: "Standard_D2ps_v6", Family: "standardDPSv6Family", VCPUs: 2, Arm: true, Ephemeral: true},
			{Name: "Standard_A2_v2", Family: "standardAv2Family", VCPUs: 2, NoGen2: true, NoPremium: true},
			{Name: "Standard_D2as_v6", Family: "standardDasv6Family", VCPUs: 2, NVMeOnly: true},
			{Name: "Standard_NC6", Family: "standardNCFamily", VCPUs: 6, RestrictedLocation: true},
		} {
			require.NoError(t, sub.AddSKU(l.name, k))
		}
		sub.SetLimit(l.name, "cores", 100)
		sub.SetLimit(l.name, "lowPriorityCores", 20)
		sub.SetLimit(l.name, "standardDSv5Family", 16)
		sub.SetLimit(l.name, "standardBasv2Family", 10)
		sub.SetLimit(l.name, "standardDPSv6Family", 10)
		sub.SetLimit(l.name, "standardAv2Family", 10)
		sub.SetLimit(l.name, "standardDasv6Family", 10)
		sub.SetLimit(l.name, "StandardPublicIPAddresses", 10)
	}
	sub.AddResourceGroup(testRG)
	for _, im := range []struct{ offer, sku, version string }{
		{OfferAMD64, "stable-gen2", "4547.1.0"},
		{OfferAMD64, "stable-gen2", "4547.0.0"},
		{OfferAMD64, "lts2024-gen2", "4081.2.1"},
		{OfferAMD64, "lts2023-gen2", "3510.2.8"},
		{OfferARM64, "stable", "4547.1.0"},
	} {
		sub.AddImage(DefaultPublisher, im.offer, im.sku, im.version, nil)
	}
	e := &env{sub: sub}
	e.clients = clientsOf(sub)
	e.c = &Compute{Clients: e.clients, Cluster: testCluster}
	e.imgs = &Images{API: e.clients.Images, Terms: e.clients.Agreements}
	return e
}

// request is a launch request builder.
type request struct {
	nodeClaim, zone, size, capacity string
	raw                             string
	userData                        []byte
	extraTags                       map[string]string
}

func (r request) build(t *testing.T) (vendors.CreateRequest, ImageRef) {
	t.Helper()
	if r.zone == "" {
		r.zone = "1"
	}
	if r.size == "" {
		r.size = "Standard_D2s_v5"
	}
	if r.capacity == "" {
		r.capacity = vendors.CapacityOnDemand
	}
	if r.userData == nil {
		r.userData = []byte(`{"ignition":{"version":"3.4.0"}}`)
	}
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: testCluster, vendors.TagNodeClaim: r.nodeClaim, vendors.TagNodePool: "pool",
		vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
	}
	return vendors.CreateRequest{
		Region: testRegion, Zone: r.zone, InstanceType: r.size, CapacityType: r.capacity, Arch: "amd64",
		UserData: r.userData, Tags: tags, ExtraTags: r.extraTags,
		Settings: vendors.VendorSettings{Raw: []byte(r.raw)},
		Prepared: &vendors.Prepared{Resources: []vendors.ResourceRef{
			{Kind: KindResourceGroups, ID: testRG}, {Kind: KindSubnets, ID: testSubnet}, {Kind: KindSecurityGroups, ID: testNSG},
		}},
		Node: vendors.NodeInfo{Name: r.nodeClaim},
	}, ImageRef{Publisher: DefaultPublisher, Offer: OfferAMD64, SKU: "stable-gen2", Version: "4547.1.0"}
}

// spec builds the LaunchSpec of a request against the env's SKU table.
func (e *env) spec(t *testing.T, r request) LaunchSpec {
	t.Helper()
	req, img := r.build(t)
	skus, err := e.clients.SKUs.List(context.Background(), req.Region)
	require.NoError(t, err)
	var sku *SKU
	if s, ok := ParseSKUs(skus, req.Region)[req.InstanceType]; ok {
		sku = &s
	}
	l, err := NewLaunchSpec(req, testCluster, img, nil, sku, "ssh-ed25519 AAAA test")
	require.NoError(t, err)
	return l
}

func (e *env) launch(t *testing.T, r request) (*vendors.Instance, error) {
	t.Helper()
	return e.c.Launch(context.Background(), e.spec(t, r))
}

func planOf(p *ImagePlan) *armcompute.PurchasePlan {
	return &armcompute.PurchasePlan{Name: new(p.Name), Product: new(p.Product), Publisher: new(p.Publisher)}
}

// clientsOf wires the driver's clients over a fake subscription.
func clientsOf(sub *fake.Subscription) Clients {
	return Clients{
		VMs: sub.VMs(), NICs: sub.NICs(), PIPs: sub.PIPs(), Groups: sub.Groups(), VNets: sub.VNets(), NSGs: sub.NSGs(),
		ASGs: sub.ASGs(), PPGs: sub.PPGs(), Resources: sub.Resources(), SKUs: sub.SKUs(), Usages: sub.Usages(),
		Images: sub.Images(), Agreements: sub.Agreements(), Locations: sub.Locations(),
	}
}

// driver builds an initialised Driver over the env's subscription.
func (e *env) driver(t *testing.T, opts ...Option) *Driver {
	t.Helper()
	opts = append([]Option{WithClients(e.clients), WithSubscription("sub-1"), WithPrices(StaticPrices{})}, opts...)
	d := New(opts...)
	require.NoError(t, d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Log: logr.Discard()}))
	return d
}
