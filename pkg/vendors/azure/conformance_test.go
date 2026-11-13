/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:prealloc,lll,dupl,gocyclo,goconst // table-heavy tests: small expectations, repeated fixtures, long SDK signatures
package azure_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
)

const (
	conformanceCluster = "conformance"
	conformanceSub     = "00000000-0000-0000-0000-000000000001"
	confRegion         = "westeurope"
	confZone           = "1"
	confType           = "Standard_D2s_v5"
	confVersion        = "4757.2.3"
	fixtureRG          = "fixture-rg"
)

// snapshotRow is the part of a /table/server row the tests need.
type snapshotRow struct {
	VendorID     string `json:"vendor_id"`
	APIReference string `json:"api_reference"`
	Status       string `json:"status"`
	Family       string `json:"family"`
	VCPUs        int    `json:"vcpus"`
	Arch         string `json:"cpu_architecture"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// azureServers returns the active Azure rows of the committed Keeper snapshot:
// the complete type list of the vendor (plan 17 section 8).
func azureServers(t testing.TB) []snapshotRow {
	t.Helper()
	snapshotOnce.Do(func() {
		rows := vendortest.SnapshotRows[snapshotRow](t, "server", "azure")
		for _, r := range rows {
			if r.VendorID == "azure" && r.Status == "active" {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no active Azure servers")
	}
	return snapshot
}

// quotaFamily is the family the SKU table names for a Keeper family (DSv5 is
// standardDSv5Family): a test fixture, the driver never derives it from names.
func quotaFamily(keeperFamily string) string { return "standard" + keeperFamily + "Family" }

// clientsOf wires the driver's clients over the fake subscription.
func clientsOf(sub *fake.Subscription) azure.Clients {
	return azure.Clients{
		VMs: sub.VMs(), NICs: sub.NICs(), PIPs: sub.PIPs(), Groups: sub.Groups(), VNets: sub.VNets(), NSGs: sub.NSGs(),
		ASGs: sub.ASGs(), PPGs: sub.PPGs(), Resources: sub.Resources(), SKUs: sub.SKUs(), Usages: sub.Usages(),
		Images: sub.Images(), Agreements: sub.Agreements(), Locations: sub.Locations(),
	}
}

// newSubscription is the fake account of the conformance run: two zonal regions
// with every active Azure size of the snapshot, quotas high enough to launch,
// the Flatcar images, and the network a user brings (a resource group, a
// virtual network with a subnet and a network security group).
func newSubscription(t testing.TB, types []snapshotRow) *fake.Subscription {
	t.Helper()
	sub := fake.New(conformanceSub)
	families := map[string]bool{}
	for _, loc := range []string{confRegion, "northeurope"} {
		sub.AddLocation(loc, true)
		for _, r := range types {
			if err := sub.AddSKU(loc, fake.SKU{
				Name: r.APIReference, Family: quotaFamily(r.Family), VCPUs: r.VCPUs, Arm: r.Arch == "arm64",
			}); err != nil {
				t.Fatal(err)
			}
			families[quotaFamily(r.Family)] = true
		}
		for f := range families {
			sub.SetLimit(loc, f, 1_000_000)
		}
		for _, u := range []string{"cores", "lowPriorityCores", "StandardPublicIPAddresses"} {
			sub.SetLimit(loc, u, 1_000_000)
		}
		sub.AddResourceGroup(fixtureRG + "-" + loc)
	}
	for _, im := range []struct{ offer, sku string }{
		{azure.OfferAMD64, "stable-gen2"}, {azure.OfferAMD64, "lts2024-gen2"}, {azure.OfferARM64, "stable"},
	} {
		sub.AddImage(azure.DefaultPublisher, im.offer, im.sku, confVersion, nil)
	}
	return sub
}

// fixtureNetwork makes the network of the fixture group in the fake: the
// subnet and the network security group a user brought.
func fixtureNetwork(t testing.TB, sub *fake.Subscription, loc string) (subnetID, nsgID string) {
	t.Helper()
	ctx := context.Background()
	rg := fixtureRG + "-" + loc
	v, err := sub.VNets().CreateOrUpdate(ctx, rg, "vnet", armnetwork.VirtualNetwork{
		Location: new(loc), Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.1.0.0/16")}},
			Subnets: []*armnetwork.Subnet{{Name: new("nodes"),
				Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.0.0/20")}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := sub.NSGs().CreateOrUpdate(ctx, rg, "nodes", armnetwork.SecurityGroup{Location: new(loc)})
	if err != nil {
		t.Fatal(err)
	}
	return *v.Properties.Subnets[0].ID, *n.ID
}

// driver is the conformance fixture's Driver: a launch without the resources
// Prepare makes gets the fixture's network for the kinds it lacks (the suite
// launches without calling Prepare, and Azure has no default network), and the
// image the suite names "image" is the Flatcar URN of the fake. Everything else
// is the driver's own.
type driver struct {
	*azure.Driver
	sub *fake.Subscription
}

func (d driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if _, err := azure.ParseURN(req.Image.Ref); err != nil {
		offer := azure.OfferAMD64
		sku := "stable-gen2"
		if req.Arch == "arm64" {
			offer, sku = azure.OfferARM64, "stable"
		}
		req.Image.Ref = azure.ImageRef{Publisher: azure.DefaultPublisher, Offer: offer, SKU: sku, Version: confVersion}.URN()
	}
	loc := req.Region
	rg := fixtureRG + "-" + loc
	subnetID, nsgID := fixtureNetworkIDs(d.sub, loc)
	p := &vendors.Prepared{}
	if req.Prepared != nil {
		*p = *req.Prepared
	}
	has := map[vendors.ResourceKind]bool{}
	for _, r := range p.Resources {
		has[r.Kind] = true
	}
	if !has[azure.KindResourceGroups] {
		p.Resources = append(p.Resources, vendors.ResourceRef{Kind: azure.KindResourceGroups, ID: rg})
	}
	if !has[azure.KindSubnets] {
		p.Resources = append(p.Resources, vendors.ResourceRef{Kind: azure.KindSubnets, ID: subnetID})
	}
	if !has[azure.KindSecurityGroups] && !has[azure.KindRunSecurityGroups] {
		p.Resources = append(p.Resources, vendors.ResourceRef{Kind: azure.KindSecurityGroups, ID: nsgID})
	}
	req.Prepared = p
	return d.Driver.CreateInstance(ctx, req)
}

func fixtureNetworkIDs(sub *fake.Subscription, loc string) (subnetID, nsgID string) {
	rg := fixtureRG + "-" + loc
	base := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network", sub.ID, rg)
	return base + "/virtualNetworks/vnet/subnets/nodes", base + "/networkSecurityGroups/nodes"
}

type world struct {
	sub *fake.Subscription
	d   driver
}

var (
	worldsMu sync.Mutex
	worlds   = map[vendors.Driver]*world{}
)

func worldOf(t *testing.T, d vendors.Driver) *world {
	t.Helper()
	worldsMu.Lock()
	defer worldsMu.Unlock()
	w, ok := worlds[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return w
}

func newDriver(t testing.TB, sub *fake.Subscription) *world {
	t.Helper()
	d := azure.New(azure.WithClients(clientsOf(sub)), azure.WithSubscription(conformanceSub), azure.WithPrices(azure.StaticPrices{}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: conformanceCluster, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	w := &world{sub: sub, d: driver{Driver: d, sub: sub}}
	worldsMu.Lock()
	worlds[w.d] = w
	worldsMu.Unlock()
	return w
}

// conformanceWorld builds a driver over the complete active Azure type list of
// the snapshot and warms the SKU table the way the quota tracker does.
func conformanceWorld(t *testing.T) *world {
	t.Helper()
	sub := newSubscription(t, azureServers(t))
	for _, loc := range []string{confRegion, "northeurope"} {
		fixtureNetwork(t, sub, loc)
	}
	w := newDriver(t, sub)
	for _, loc := range []string{confRegion, "northeurope"} {
		if _, err := w.d.LaunchableTypes(context.Background(), loc); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

var injected = map[vendors.Class]struct {
	status int
	code   string
	msg    string
}{
	vendors.InsufficientCapacity: {http.StatusConflict, "ZonalAllocationFailed", "Allocation failed."},
	vendors.Unsupported:          {http.StatusBadRequest, "SkuNotAvailable", "The size is not available."},
	vendors.QuotaExceeded:        {http.StatusConflict, "QuotaExceeded", "Quota exceeded."},
	vendors.ZoneUnusable:         {http.StatusBadRequest, "ZoneNotAvailableForSubscription", "The zone is not available."},
	vendors.Retryable:            {http.StatusInternalServerError, "InternalServerError", "Try again."},
	vendors.Fatal:                {http.StatusForbidden, "AuthorizationFailed", "Not allowed."},
}

var seedN atomic.Int32

func conformanceFixture(
	t *testing.T, kind vendors.ResourceKind, params func(*testing.T, vendors.Driver, string) json.RawMessage,
) conformance.Fixture {
	fx := conformanceFixture0(t, kind, params)
	if kind != azure.KindResourceGroups {
		fx.Seed = nil // the selector cases cover the kinds once, in the resource groups run
		fx.SeedKinds = nil
	}
	return fx
}

func conformanceFixture0(
	t *testing.T, kind vendors.ResourceKind, params func(*testing.T, vendors.Driver, string) json.RawMessage,
) conformance.Fixture {
	var all []string
	for _, r := range azureServers(t) {
		all = append(all, r.APIReference)
	}
	return conformance.Fixture{
		New:    func(t *testing.T) vendors.Driver { return conformanceWorld(t).d },
		Region: confRegion, Zone: confZone, InstanceType: confType, MaxUserData: azure.MaxUserData,
		InstanceTypes: all,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			e := injected[class]
			worldOf(t, d).sub.FailNext("vm.put", fake.Error(e.status, e.code, e.msg), 1)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			parts := strings.Split(ref.ID(), "/")
			if err := worldOf(t, d).sub.PowerOff(parts[0], parts[1], false); err != nil {
				t.Fatal(err)
			}
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, req vendors.CreateRequest) {
			t.Helper()
			sub := worldOf(t, d).sub
			for _, u := range []string{"cores", "lowPriorityCores"} {
				sub.SetLimit(req.Region, u, 0)
			}
			for _, r := range azureServers(t) {
				sub.SetLimit(req.Region, quotaFamily(r.Family), 0)
			}
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","storageAccountType":"Premium_LRS","diskSizeGB":100,"mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"cheap": json.RawMessage(`{"root":{"storageAccountType":"StandardSSD_LRS","diskSizeGB":100}}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
			},
			vendors.ProfilePlacement: {"colocated": json.RawMessage(`{"proximityPlacementGroup":{"strategy":"cluster"}}`)},
			vendors.ProfileNetwork: {
				"fast":   json.RawMessage(`{"acceleratedNetworking":{"enabled":"true"}}`),
				"slow":   json.RawMessage(`{"acceleratedNetworking":{"enabled":"false"}}`),
				"adapts": json.RawMessage(`{"acceleratedNetworking":{"enabled":"auto"}}`),
			},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "azure", APIReference: "Standard_D2s_v5", Family: "DSv5", VCPUs: 2},
			{Vendor: "azure", APIReference: "Standard_D2ps_v6", Family: "Dpsv6", VCPUs: 2, Arch: "arm64"},
			{Vendor: "azure", APIReference: "Standard_B2ats_v2", Family: "Basv2", VCPUs: 2},
		},
		ResourceKind: kind, ResourceParams: params,
		Seed: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
			t.Helper()
			return seed(t, worldOf(t, d), k, region, spec.Name, spec.Tags)
		},
		SeedKinds: []vendors.ResourceKind{azure.KindResourceGroups, azure.KindSecurityGroups, azure.KindPlacement},
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return seed(t, worldOf(t, d), k, region, fmt.Sprintf("user-%s-%d", k, seedN.Add(1)), tags)
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			return newDriver(t, worldOf(t, d).sub).d
		},
	}
}

func ptrTags(m map[string]string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = new(v)
	}
	return out
}

// seed makes a resource the way a user would, with the name and tags: a resource
// group of its own, the others in the fixture's group.
func seed(t *testing.T, w *world, kind vendors.ResourceKind, region, name string, tags map[string]string) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	rg := fixtureRG + "-" + region
	switch kind {
	case azure.KindResourceGroups:
		g, err := w.sub.Groups().CreateOrUpdate(ctx, name, armresources.ResourceGroup{Location: new(region), Tags: ptrTags(tags)})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: *g.Name, Name: *g.Name}
	case azure.KindSubnets:
		v, err := w.sub.VNets().CreateOrUpdate(ctx, rg, name, armnetwork.VirtualNetwork{
			Location: new(region), Tags: ptrTags(tags),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.2.0.0/16")}},
				Subnets: []*armnetwork.Subnet{{Name: new("nodes"),
					Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.2.0.0/20")}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: *v.Properties.Subnets[0].ID, Name: name + "/nodes"}
	case azure.KindPlacement:
		g, err := w.sub.PPGs().CreateOrUpdate(ctx, rg, name, armcompute.ProximityPlacementGroup{
			Location: new(region), Tags: ptrTags(tags)})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: *g.ID, Name: name}
	}
	n, err := w.sub.NSGs().CreateOrUpdate(ctx, rg, name, armnetwork.SecurityGroup{Location: new(region), Tags: ptrTags(tags)})
	if err != nil {
		t.Fatal(err)
	}
	return vendors.ResourceRef{Kind: kind, Region: region, ID: *n.ID, Name: name}
}

func groupParams(t *testing.T, _ vendors.Driver, _ string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(azure.ResourceParams{ResourceGroup: fixtureRG + "-" + confRegion, SelfReference: true})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestConformance runs the shared driver suite over the complete active Azure
// type list of the committed snapshot, once per resource kind the driver
// creates: the suite's managed-resource subtests take one kind. The suite's
// resolution-order cases assume a vendor default resource, which Azure has not:
// resolution_test.go runs ADR-022's order against the engine without it.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		kind   vendors.ResourceKind
		params func(*testing.T, vendors.Driver, string) json.RawMessage
	}{
		{kind: azure.KindResourceGroups},
		{kind: azure.KindSubnets, params: groupParams},
		{kind: azure.KindSecurityGroups, params: groupParams},
		{kind: azure.KindRunSecurityGroups, params: groupParams},
		{kind: azure.KindPlacement, params: groupParams},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.params))
		})
	}
}
