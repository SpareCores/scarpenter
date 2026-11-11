/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:prealloc,lll,dupl,gocyclo,goconst // table-heavy tests: small expectations, repeated fixtures, long SDK signatures
package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func tp(m map[string]string) map[string]*string { return tagPtrs(m) }

// provider is the resource provider of the tests: the `use` of a kind when the
// NodeClass has one, else the managed resource of a deterministic name.
type provider struct {
	d      *Driver
	region string
	use    map[vendors.ResourceKind]vendors.UseRef
	reqs   []vendors.ResourceRequest
}

func (p *provider) Require(ctx context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	p.reqs = append(p.reqs, req)
	if req.Use != nil {
		return p.d.Resolve(ctx, req.Kind, p.region, *req.Use)
	}
	if u, ok := p.use[req.Kind]; ok {
		return p.d.Resolve(ctx, req.Kind, p.region, u)
	}
	sum := sha256.Sum256([]byte(req.Key + string(req.Params)))
	name := fmt.Sprintf("scarpenter-%s-%s-%s", req.Kind, p.region, hex.EncodeToString(sum[:])[:8])
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: testCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: string(req.Kind),
	}
	ref, _, err := p.d.Ensure(ctx, req.Kind, p.region, vendors.CreateSpec{Name: name, Tags: tags, Params: req.Params})
	if err != nil {
		return nil, err
	}
	return []vendors.ResourceRef{ref}, nil
}

func (p *provider) kinds() []vendors.ResourceKind {
	var out []vendors.ResourceKind
	for _, r := range p.reqs {
		out = append(out, r.Kind)
	}
	return out
}

func (e *env) userNetwork(t *testing.T, rg, vnet string, tags map[string]string, prefix string) armnetwork.VirtualNetwork {
	t.Helper()
	e.sub.AddResourceGroup(rg)
	v, err := e.sub.VNets().CreateOrUpdate(context.Background(), rg, vnet, armnetwork.VirtualNetwork{
		Location: new(testRegion), Tags: tp(tags),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.0.0.0/8")}},
			Subnets: []*armnetwork.Subnet{
				{Name: new("a"), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(prefix)}},
				{Name: new("b"), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.9.9.0/24")}},
			},
		},
	})
	require.NoError(t, err)
	return v
}

func TestEnsureManagedResources(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	spec := func(name string, kind vendors.ResourceKind, p ResourceParams) vendors.CreateSpec {
		tags := map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagResource: string(kind),
		}
		raw, _ := json.Marshal(p)
		return vendors.CreateSpec{Name: name, Tags: tags, ExtraTags: map[string]string{"team": "x"}, Params: raw}
	}

	// A resource group, then the same again (adoption).
	rg, created, err := d.Ensure(ctx, KindResourceGroups, testRegion, spec("scarpenter-c-rg", KindResourceGroups, ResourceParams{}))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "scarpenter-c-rg", rg.ID)
	g, err := e.sub.Groups().Get(ctx, "scarpenter-c-rg")
	require.NoError(t, err)
	require.Equal(t, "x", *g.Tags["team"], "the NodeClass's own tags")
	require.Equal(t, "scarpenter-c-rg", *g.Tags["Name"])
	_, created, err = d.Ensure(ctx, KindResourceGroups, testRegion, spec("scarpenter-c-rg", KindResourceGroups, ResourceParams{}))
	require.NoError(t, err)
	require.False(t, created)

	// A resource of the name without the marker is not adopted.
	e.sub.AddResourceGroup("someone-elses")
	_, _, err = d.Ensure(ctx, KindResourceGroups, testRegion, spec("someone-elses", KindResourceGroups, ResourceParams{}))
	require.Equal(t, codeNameConflict, vendors.CodeOf(err))

	// The managed network: one virtual network with one subnet.
	p := ResourceParams{ResourceGroup: rg.ID}
	sn, created, err := d.Ensure(ctx, KindSubnets, testRegion, spec("scarpenter-c-net", KindSubnets, p))
	require.NoError(t, err)
	require.True(t, created)
	require.Contains(t, sn.ID, "/virtualNetworks/scarpenter-c-net/subnets/nodes")
	require.Contains(t, sn.Network, "/virtualNetworks/scarpenter-c-net")
	_, created, err = d.Ensure(ctx, KindSubnets, testRegion, spec("scarpenter-c-net", KindSubnets, p))
	require.NoError(t, err)
	require.False(t, created)

	// The network security group: allow between the nodes, the inbound rules,
	// the deny of the virtual network's traffic; and its application group.
	nsgp := ResourceParams{
		ResourceGroup: rg.ID, SelfReference: true,
		Inbound: []common.InboundRule{
			{Protocol: "udp", FromPort: new(int32(41641)), CIDRs: []string{"0.0.0.0/0"}},
			{Protocol: "tcp", FromPort: new(int32(8000)), ToPort: new(int32(8010)), CIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"}},
			{Protocol: "icmp", CIDRs: []string{"10.0.0.0/8"}},
			{Protocol: "any", CIDRs: []string{"172.16.0.0/12"}},
		},
	}
	nsg, created, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec("scarpenter-c-nsg", KindSecurityGroups, nsgp))
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, ASGIDOf(nsg.ID), fmt.Sprintf("/subscriptions/sub-1/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/scarpenter-c-nsg-nodes", rg.ID))
	_, err = e.sub.ASGs().Get(ctx, rg.ID, "scarpenter-c-nsg-nodes")
	require.NoError(t, err)
	n, err := e.sub.NSGs().Get(ctx, rg.ID, "scarpenter-c-nsg")
	require.NoError(t, err)
	type rule struct {
		name, access, proto, ports string
		prio                       int32
	}
	var got []rule
	for _, r := range n.Properties.SecurityRules {
		got = append(got, rule{*r.Name, string(*r.Properties.Access), string(*r.Properties.Protocol), *r.Properties.DestinationPortRange, *r.Properties.Priority})
	}
	require.Equal(t, []rule{
		{"allow-nodes", "Allow", "*", "*", 50},
		{"inbound-1", "Allow", "Udp", "41641", 100},
		{"inbound-2", "Allow", "Tcp", "8000-8010", 101},
		{"inbound-3", "Allow", "Icmp", "*", 102},
		{"inbound-4", "Allow", "*", "*", 103},
		{"deny-vnet-inbound", "Deny", "*", "*", 4000},
	}, got)
	require.Equal(t, "VirtualNetwork", *n.Properties.SecurityRules[5].Properties.SourceAddressPrefix)
	require.Len(t, n.Properties.SecurityRules[2].Properties.SourceAddressPrefixes, 2)
	require.NotEmpty(t, n.Properties.SecurityRules[0].Properties.SourceApplicationSecurityGroups)

	// Without the allow there is no application group.
	n2, _, err := d.Ensure(ctx, KindRunSecurityGroups, testRegion, spec("scarpenter-c-run", KindRunSecurityGroups, ResourceParams{ResourceGroup: rg.ID}))
	require.NoError(t, err)
	require.Equal(t, KindRunSecurityGroups, n2.Kind)
	_, err = e.sub.ASGs().Get(ctx, rg.ID, "scarpenter-c-run-nodes")
	require.True(t, isNotFound(err))

	// The proximity placement group.
	ppg, created, err := d.Ensure(ctx, KindPlacement, testRegion, spec("scarpenter-c-ppg", KindPlacement, ResourceParams{ResourceGroup: rg.ID, Strategy: "cluster"}))
	require.NoError(t, err)
	require.True(t, created)
	_, created, err = d.Ensure(ctx, KindPlacement, testRegion, spec("scarpenter-c-ppg", KindPlacement, ResourceParams{ResourceGroup: rg.ID}))
	require.NoError(t, err)
	require.False(t, created)
	_, _, err = d.Ensure(ctx, KindPlacement, testRegion, spec("other", KindPlacement, ResourceParams{ResourceGroup: rg.ID, Strategy: "spread"}))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// Parameters are checked.
	_, _, err = d.Ensure(ctx, KindSubnets, testRegion, spec("x", KindSubnets, ResourceParams{}))
	require.ErrorContains(t, err, "resourceGroup")
	_, _, err = d.Ensure(ctx, KindSubnets, testRegion, vendors.CreateSpec{Name: "x", Params: []byte("{")})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	_, _, err = d.Ensure(ctx, "nope", testRegion, spec("x", "nope", ResourceParams{}))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	_, _, err = d.Ensure(ctx, KindSubnets, testRegion, vendors.CreateSpec{})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// An existing group of the name without the marker, for each kind.
	e.userNetwork(t, "user-rg", "user-vnet", nil, "10.0.0.0/24")
	_, err = e.sub.NSGs().CreateOrUpdate(ctx, "user-rg", "user-nsg", armnetwork.SecurityGroup{Location: new(testRegion)})
	require.NoError(t, err)
	_, err = e.sub.PPGs().CreateOrUpdate(ctx, "user-rg", "user-ppg", armcompute.ProximityPlacementGroup{Location: new(testRegion)})
	require.NoError(t, err)
	for kind, name := range map[vendors.ResourceKind]string{KindSubnets: "user-vnet", KindSecurityGroups: "user-nsg", KindPlacement: "user-ppg"} {
		_, _, err := d.Ensure(ctx, kind, testRegion, spec(name, kind, ResourceParams{ResourceGroup: "user-rg"}))
		require.Equal(t, codeNameConflict, vendors.CodeOf(err), kind)
	}

	// List: the managed resources of the cluster, per region and tag.
	list, err := d.List(ctx, []string{testRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	require.NoError(t, err)
	kinds := map[vendors.ResourceKind]int{}
	for _, r := range list {
		kinds[r.Kind]++
		require.True(t, r.Managed)
		require.Equal(t, testRegion, r.Region)
	}
	require.Equal(t, map[vendors.ResourceKind]int{
		KindResourceGroups: 1, KindSubnets: 1, KindSecurityGroups: 1, KindRunSecurityGroups: 1, KindPlacement: 1}, kinds)
	for _, r := range list {
		if r.Kind == KindSubnets {
			require.Equal(t, sn.ID, r.ID)
			require.Equal(t, sn.Network, r.Network)
		}
	}
	all, err := d.List(ctx, nil, nil)
	require.NoError(t, err)
	require.Len(t, all, len(list), "no regions: every region")

	// Deletion: never a resource without the marker, never one in use.
	for _, r := range []vendors.ResourceRef{
		{Kind: KindResourceGroups, ID: "someone-elses"}, {Kind: KindSubnets, ID: "/subscriptions/sub-1/resourceGroups/user-rg/providers/Microsoft.Network/virtualNetworks/user-vnet/subnets/a"},
		{Kind: KindSecurityGroups, ID: "/subscriptions/sub-1/resourceGroups/user-rg/providers/Microsoft.Network/networkSecurityGroups/user-nsg"},
		{Kind: KindPlacement, ID: "/subscriptions/sub-1/resourceGroups/user-rg/providers/Microsoft.Compute/proximityPlacementGroups/user-ppg"},
	} {
		require.Equal(t, codeNotOwned, vendors.CodeOf(d.Delete(ctx, r)), "%+v", r)
	}
	require.Equal(t, vendors.Fatal, vendors.ClassOf(d.Delete(ctx, vendors.ResourceRef{Kind: KindSubnets, ID: "garbage"})))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(d.Delete(ctx, vendors.ResourceRef{Kind: KindSecurityGroups, ID: "garbage"})))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(d.Delete(ctx, vendors.ResourceRef{Kind: KindPlacement, ID: "garbage"})))
	require.Equal(t, vendors.Fatal, vendors.ClassOf(d.Delete(ctx, vendors.ResourceRef{Kind: "nope"})))

	// The resource group holds the network, so it is in use: it goes last.
	used, _, err := d.InUse(ctx, rg)
	require.NoError(t, err)
	require.False(t, used, "SCarpenter's own resources do not keep a managed group")
	// A resource of someone else in it does.
	_, err = e.sub.NSGs().CreateOrUpdate(ctx, rg.ID, "foreign", armnetwork.SecurityGroup{Location: new(testRegion)})
	require.NoError(t, err)
	used, last, err := d.InUse(ctx, rg)
	require.NoError(t, err)
	require.True(t, used)
	require.False(t, last.IsZero())
	require.True(t, vendors.IsClass(d.Delete(ctx, rg), vendors.Retryable))
	require.NoError(t, e.sub.NSGs().Delete(ctx, rg.ID, "foreign"))

	// Delete the rest, twice each.
	for _, r := range []vendors.ResourceRef{n2, nsg, ppg, sn, rg} {
		for range 2 {
			require.NoError(t, d.Delete(ctx, r), "%+v", r)
		}
	}
	require.Empty(t, e.sub.ASGs().Names(), "the application security group goes with its network security group")
	list, err = d.List(ctx, nil, nil)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestInUseOfEveryKind(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	_, err := d.LaunchableTypes(ctx, testRegion)
	require.NoError(t, err)
	p := &provider{d: d, region: testRegion}
	prep, err := d.Prepare(ctx, vendors.PrepareRequest{
		Region: testRegion, Zone: "1", Resources: p, Tags: map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
		Settings: vendors.VendorSettings{Networking: common.Networking{Isolation: common.IsolationNodeClass}},
		RunKey:   "run",
	})
	require.NoError(t, err)
	req := e.launchReq(t, "user")
	req.Prepared = prep
	inst, err := d.CreateInstance(ctx, req)
	require.NoError(t, err)
	for _, r := range prep.Resources {
		if r.Kind == KindAppSecGroups {
			continue
		}
		used, _, err := d.InUse(ctx, r)
		require.NoError(t, err, r.Kind)
		require.True(t, used, "%s is used by the node", r.Kind)
		require.True(t, vendors.IsClass(d.Delete(ctx, r), vendors.Retryable), "%s: %v", r.Kind, d.Delete(ctx, r))
	}
	require.NoError(t, d.DeleteInstance(ctx, inst.Ref))
	for _, r := range []vendors.ResourceKind{KindSecurityGroups, KindSubnets, KindResourceGroups} {
		for _, ref := range prep.Resources {
			if ref.Kind != r {
				continue
			}
			used, _, err := d.InUse(ctx, ref)
			require.NoError(t, err)
			require.False(t, used, r)
			require.NoError(t, d.Delete(ctx, ref), r)
			_, _, err = d.InUse(ctx, ref)
			require.True(t, vendors.IsClass(err, vendors.NotFound), "%s: %v", r, err)
		}
	}
	for _, ref := range []vendors.ResourceRef{
		{Kind: KindSubnets, ID: "garbage"}, {Kind: KindSecurityGroups, ID: "garbage"}, {Kind: KindPlacement, ID: "garbage"}, {Kind: "nope"},
	} {
		_, _, err := d.InUse(ctx, ref)
		require.Error(t, err, ref.Kind)
	}
}

func TestResolve(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	v1 := e.userNetwork(t, "net-rg", "env-a", map[string]string{"env": "a", "scarpenter-use": "true"}, "10.1.0.0/24")
	_ = e.userNetwork(t, "other-rg", "env-b", map[string]string{"env": "b"}, "10.2.0.0/24")
	// A network in another region is never a candidate.
	_, err := e.sub.VNets().CreateOrUpdate(ctx, "net-rg", "far", armnetwork.VirtualNetwork{
		Location: new("northeurope"), Tags: tp(map[string]string{"env": "a"}),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.5.0.0/16")}},
			Subnets:      []*armnetwork.Subnet{{Name: new("a"), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.5.0.0/24")}}}},
	})
	require.NoError(t, err)

	resolve := func(kind vendors.ResourceKind, ref vendors.UseRef) ([]vendors.ResourceRef, error) {
		ref.Select = common.SelectAll
		return d.Resolve(ctx, kind, testRegion, ref)
	}
	ids := func(refs []vendors.ResourceRef) []string {
		var out []string
		for _, r := range refs {
			out = append(out, r.ID)
		}
		return out
	}
	subA := *v1.Properties.Subnets[0].ID
	subB := *v1.Properties.Subnets[1].ID

	// Tags are the network's: every subnet of a matching network is a candidate.
	refs, err := resolve(KindSubnets, vendors.UseRef{Tags: map[string]string{"env": "a"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{subA, subB}, ids(refs))
	require.Equal(t, *v1.ID, refs[0].Network)
	require.Equal(t, "env-a/a", refs[0].Name)
	// By name <vnet>/<subnet>, by the ARM ID in any case, by the range.
	refs, err = resolve(KindSubnets, vendors.UseRef{Name: "env-a/a"})
	require.NoError(t, err)
	require.Equal(t, []string{subA}, ids(refs))
	refs, err = resolve(KindSubnets, vendors.UseRef{IDs: []string{"/SUBSCRIPTIONS/SUB-1/RESOURCEGROUPS/NET-RG/providers/microsoft.network/virtualnetworks/ENV-A/subnets/B"}})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	refs, err = resolve(KindSubnets, vendors.UseRef{Tags: map[string]string{"env": "*"}, Match: common.ResourceMatch{Within: "10.1.0.0/16"}})
	require.NoError(t, err)
	require.Equal(t, []string{subA}, ids(refs))
	refs, err = resolve(KindSubnets, vendors.UseRef{Tags: map[string]string{"env": "*"}, Match: common.ResourceMatch{CIDR: "10.9.9.0/24", Network: []string{*v1.ID}}})
	require.NoError(t, err)
	require.Equal(t, []string{subB}, ids(refs))
	refs, err = resolve(KindSubnets, vendors.UseRef{Tags: map[string]string{"env": "*"},
		Filters: []common.ResourceFilter{{Name: filterResourceGroup, Values: []string{"OTHER-RG"}}}})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Contains(t, refs[0].ID, "other-rg")
	_, err = resolve(KindSubnets, vendors.UseRef{Tags: map[string]string{"env": "zzz"}})
	var nm *selector.NoMatchError
	require.ErrorAs(t, err, &nm)

	// Resource groups by name, ARM ID, tags and glob.
	e.sub.AddResourceGroup("my-env-westeurope")
	_, err = e.sub.Groups().CreateOrUpdate(ctx, "tagged", armresources.ResourceGroup{Location: new(testRegion), Tags: tp(map[string]string{"scarpenter-use": "true"})})
	require.NoError(t, err)
	for _, ref := range []vendors.UseRef{
		{IDs: []string{"MY-ENV-WESTEUROPE"}}, {IDs: []string{"/subscriptions/sub-1/resourceGroups/my-env-westeurope"}}, {Name: "my-env-*"},
	} {
		refs, err := d.Resolve(ctx, KindResourceGroups, testRegion, ref)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		require.Equal(t, "my-env-westeurope", refs[0].Name)
	}
	refs, err = d.Resolve(ctx, KindResourceGroups, testRegion, vendors.UseRef{Tags: map[string]string{"scarpenter-use": "true"}})
	require.NoError(t, err)
	require.Equal(t, "tagged", refs[0].ID)

	// Network security groups and proximity placement groups: in the region.
	_, err = e.sub.NSGs().CreateOrUpdate(ctx, "net-rg", "nodes", armnetwork.SecurityGroup{Location: new(testRegion), Tags: tp(map[string]string{"scarpenter-use": "true"})})
	require.NoError(t, err)
	_, err = e.sub.NSGs().CreateOrUpdate(ctx, "net-rg", "far-nodes", armnetwork.SecurityGroup{Location: new("northeurope"), Tags: tp(map[string]string{"scarpenter-use": "true"})})
	require.NoError(t, err)
	refs, err = d.Resolve(ctx, KindSecurityGroups, testRegion, vendors.UseRef{Tags: map[string]string{"scarpenter-use": "true"}})
	require.NoError(t, err)
	require.Len(t, refs, 1, "the group of the other region is not a candidate")
	_, err = d.Resolve(ctx, KindSecurityGroups, testRegion, vendors.UseRef{Name: "nodes", Filters: []common.ResourceFilter{{Name: filterResourceGroup, Values: []string{"nowhere"}}}})
	require.ErrorAs(t, err, &nm)
	_, err = e.sub.PPGs().CreateOrUpdate(ctx, "net-rg", "colo", armcompute.ProximityPlacementGroup{Location: new(testRegion)})
	require.NoError(t, err)
	refs, err = d.Resolve(ctx, KindPlacement, testRegion, vendors.UseRef{Name: "colo"})
	require.NoError(t, err)
	require.Len(t, refs, 1)

	// What it refuses.
	_, err = d.Resolve(ctx, KindSubnets, testRegion, vendors.UseRef{})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err), "an empty selector selects nothing")
	_, err = d.Resolve(ctx, "nope", testRegion, vendors.UseRef{Name: "x"})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	_, err = d.Resolve(ctx, KindResourceGroups, testRegion, vendors.UseRef{Expression: "resources | take 1"})
	require.Error(t, err)

	// Errors of the listing are classified.
	e.sub.FailNext("vnet.list", fake.Error(http.StatusTooManyRequests, "TooManyRequests", "slow"), 1)
	_, err = resolve(KindSubnets, vendors.UseRef{Name: "x"})
	require.True(t, vendors.IsClass(err, vendors.Retryable))
	for op, kind := range map[string]vendors.ResourceKind{"group.list": KindResourceGroups, "nsg.list": KindSecurityGroups, "ppg.list": KindPlacement} {
		e.sub.FailNext(op, fake.Error(http.StatusTooManyRequests, "TooManyRequests", "slow"), 1)
		_, err = resolve(kind, vendors.UseRef{Name: "x"})
		require.True(t, vendors.IsClass(err, vendors.Retryable), op)
	}
}

func TestSelectorHelpers(t *testing.T) {
	require.Equal(t, []string{"rg1", "plain"}, groupNames([]string{"/subscriptions/s/resourceGroups/rg1", "plain"}))
	require.Equal(t, "Mine", idSpelling([]string{"Mine"}, "mine"))
	require.Equal(t, "other", idSpelling([]string{"Mine"}, "other"))
	require.True(t, inGroup("/subscriptions/s/resourceGroups/Rg/providers/x/y/z", []string{"rg"}))
	require.False(t, inGroup("garbage", []string{"rg"}))
	require.True(t, inGroup("garbage", nil))
	require.True(t, sameLocation("West Europe", "westeurope"))
	rg, v, ok := vnetOf("/subscriptions/s/resourceGroups/g/providers/Microsoft.Network/virtualNetworks/v")
	require.True(t, ok)
	require.Equal(t, [2]string{"g", "v"}, [2]string{rg, v})
	_, _, ok = vnetOf("/subscriptions/s/resourceGroups/g/providers/Microsoft.Network/networkSecurityGroups/v")
	require.False(t, ok)
	_, _, ok = vnetOf("x")
	require.False(t, ok)
	require.Equal(t, "", ASGIDOf("garbage"))
	d := New()
	for kind, caps := range d.Selectors() {
		if kind != KindImages {
			require.True(t, isKind(kind), kind)
		}
		require.NotEmpty(t, caps.DefaultSelect)
	}
	require.Len(t, d.Kinds(), 5)
	require.False(t, isKind(KindImages))
}

var _ = fake.Error
