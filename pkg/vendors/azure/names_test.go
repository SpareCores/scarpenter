/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestVMName(t *testing.T) {
	n := VMName("cluster-a", "nodeclaim-abc")
	require.Equal(t, "nodeclaim-abc-"+hash8("cluster-a"), n)
	require.Equal(t, n, VMName("cluster-a", "nodeclaim-abc"), "deterministic")
	require.NotEqual(t, n, VMName("cluster-b", "nodeclaim-abc"), "the cluster is part of the name")

	long1 := strings.Repeat("a", 63)
	long2 := strings.Repeat("a", 62) + "b"
	n1, n2 := VMName("c", long1), VMName("c", long2)
	require.LessOrEqual(t, len(n1), maxVMName)
	require.Len(t, n1, maxVMName)
	require.NotEqual(t, n1, n2, "two long names stay distinct")
	require.True(t, strings.HasSuffix(n1, "-"+hash8("c")))
	// Exactly at the limit stays whole.
	fit := strings.Repeat("x", maxVMName-9)
	require.Equal(t, fit+"-"+hash8("c"), VMName("c", fit))
}

func TestTagKeyAndTags(t *testing.T) {
	require.Equal(t, "scarpenter-cluster", TagKey(vendors.TagCluster))
	require.Equal(t, "scarpenter-idempotency-key", TagKey(vendors.TagIdempotencyKey))
	for _, k := range vendors.TagKeys {
		require.NoError(t, ValidateTags(map[string]string{TagKey(k): "v"}), "%s is a valid Azure tag name", k)
	}
	tags, err := Tags(map[vendors.TagKey]string{vendors.TagCluster: "c", vendors.TagNodeClaim: "nc"}, map[string]string{"team": "x"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"scarpenter-cluster": "c", "scarpenter-nodeclaim": "nc", "Name": "nc", "team": "x"}, tags)

	// ExtraTags cannot take the logical names, in any case.
	_, err = Tags(map[vendors.TagKey]string{vendors.TagCluster: "c"}, map[string]string{"Scarpenter-Cluster": "x"})
	require.Error(t, err)
	_, err = Tags(nil, map[string]string{"name": "x"})
	require.Error(t, err)
}

func TestValidateTags(t *testing.T) {
	require.Error(t, ValidateTags(map[string]string{"a/b": "x"}))
	for _, c := range "<>%&\\?/" {
		require.Error(t, ValidateTags(map[string]string{"a" + string(c): "x"}), string(c))
	}
	require.Error(t, ValidateTags(map[string]string{"": "x"}))
	require.Error(t, ValidateTags(map[string]string{"k": strings.Repeat("v", 257)}))
	require.NoError(t, ValidateTags(map[string]string{"k": strings.Repeat("v", 256)}))
	require.Error(t, ValidateTags(map[string]string{"Env": "a", "env": "b"}), "names are case insensitive")
	many := map[string]string{}
	for i := range 51 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	require.Error(t, ValidateTags(many))
}

func TestLogicalTags(t *testing.T) {
	got := LogicalTags(map[string]*string{"Scarpenter-Cluster": new("c"), "scarpenter-nodeclaim": new("nc"), "other": new("x"), "scarpenter-nodepool": nil})
	require.Equal(t, map[vendors.TagKey]string{vendors.TagCluster: "c", vendors.TagNodeClaim: "nc"}, got)
}

func TestProviderID(t *testing.T) {
	id := ProviderID("westeurope", "nc-1")
	require.Equal(t, "scarpenter://azure/westeurope/nc-1", id)
	region, nc, err := ParseProviderID(id)
	require.NoError(t, err)
	require.Equal(t, "westeurope", region)
	require.Equal(t, "nc-1", nc)
	for _, bad := range []string{"", "azure/westeurope/nc", "scarpenter://aws/us-east-1/nc", "scarpenter://azure/westeurope", "scarpenter://azure//nc"} {
		_, _, err := ParseProviderID(bad)
		require.Error(t, err, bad)
	}
}

func TestInstanceRef(t *testing.T) {
	ref := InstanceRefOf("westeurope", "my-rg", "vm-1")
	require.Equal(t, vendors.InstanceRef("westeurope/my-rg/vm-1"), ref)
	require.Equal(t, "westeurope", ref.Region())
	region, rg, vm, ok := ParseInstanceRef(ref)
	require.True(t, ok)
	require.Equal(t, []string{"westeurope", "my-rg", "vm-1"}, []string{region, rg, vm})
	// The conformance suite's malformed reference, and other shapes.
	for _, bad := range []vendors.InstanceRef{"westeurope/i-never-existed", "", "a/b/c/d", "a//c", "/b/c"} {
		_, _, _, ok := ParseInstanceRef(bad)
		require.False(t, ok, string(bad))
	}
}

func TestURN(t *testing.T) {
	r := ImageRef{Publisher: "kinvolk", Offer: OfferAMD64, SKU: "stable-gen2", Version: "4547.1.0"}
	require.Equal(t, "kinvolk:flatcar-container-linux-corevm-amd64:stable-gen2:4547.1.0", r.URN())
	back, err := ParseURN(r.URN())
	require.NoError(t, err)
	require.Equal(t, r, back)
	for _, bad := range []string{"", "a:b:c", "a:b:c:d:e", "a::c:d"} {
		_, err := ParseURN(bad)
		require.Error(t, err, bad)
	}
}

func TestStateOf(t *testing.T) {
	vm := func(prov, power string) armcompute.VirtualMachine {
		p := &armcompute.VirtualMachineProperties{ProvisioningState: &prov}
		if power != "" {
			p.InstanceView = &armcompute.VirtualMachineInstanceView{Statuses: []*armcompute.InstanceViewStatus{
				{Code: new("ProvisioningState/succeeded")}, {Code: new("PowerState/" + power)}}}
		}
		return armcompute.VirtualMachine{Properties: p}
	}
	cases := []struct{ prov, power, want string }{
		{"Creating", "", vendors.StatePending},
		{"Creating", PowerRunning, vendors.StatePending},
		{"Succeeded", PowerStarting, vendors.StatePending},
		{"Succeeded", PowerRunning, vendors.StateRunning},
		{"Updating", PowerRunning, vendors.StateRunning},
		{"Succeeded", PowerStopping, vendors.StateTerminating},
		{"Succeeded", PowerStopped, vendors.StateTerminating},
		{"Succeeded", PowerDeallocating, vendors.StateTerminating},
		{"Succeeded", PowerDeallocated, vendors.StateTerminating},
		{"Deleting", PowerRunning, vendors.StateTerminating},
		{"Failed", "", vendors.StateRunning},
		{"Failed", PowerDeallocated, vendors.StateRunning},
		{"Succeeded", "", vendors.StatePending},
	}
	for _, c := range cases {
		require.Equal(t, c.want, StateOf(vm(c.prov, c.power)), "%s/%s", c.prov, c.power)
	}
	require.Equal(t, vendors.StatePending, StateOf(armcompute.VirtualMachine{}))
}
