/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

func TestLaunchRunning(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	require.Equal(t, InstanceRefOf(testRegion, testRG, VMName(testCluster, "nc-1")), inst.Ref)
	require.Equal(t, vendors.StateRunning, inst.State)
	require.Equal(t, "1", inst.Zone)
	require.Equal(t, "Standard_D2s_v5", inst.Type)
	require.Equal(t, vendors.CapacityOnDemand, inst.CapacityType)
	require.Equal(t, testRegion, inst.Region)
	require.Equal(t, "nc-1", inst.Tags[vendors.TagNodeClaim])
	require.Equal(t, testCluster, inst.Tags[vendors.TagCluster])
	require.False(t, inst.CreatedAt.IsZero())
	require.Len(t, inst.Addresses, 2, "private and public address")
	require.Regexp(t, `^10\.240\.`, inst.Addresses[0])
	require.Regexp(t, `^20\.`, inst.Addresses[1])
	require.Len(t, e.sub.NICNames(), 1)
	require.Len(t, e.sub.PIPNames(), 1)
	require.Equal(t, []string{testRG + "/" + VMName(testCluster, "nc-1") + "-os"}, e.sub.Disks())
}

func TestLaunchWithoutPublicIP(t *testing.T) {
	e := newEnv(t)
	req, img := request{nodeClaim: "nc-1"}.build(t)
	f := false
	req.Settings.Networking.PublicIPv4 = &f
	l, err := NewLaunchSpec(req, testCluster, img, nil, nil, "key")
	require.NoError(t, err)
	inst, err := e.c.Launch(context.Background(), l)
	require.NoError(t, err)
	require.Len(t, inst.Addresses, 1)
	require.Empty(t, e.sub.PIPNames())
}

func TestLaunchSpotAndGet(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-s", capacity: vendors.CapacitySpot})
	require.NoError(t, err)
	require.Equal(t, vendors.CapacitySpot, inst.CapacityType)
	got, err := e.c.Get(context.Background(), inst.Ref)
	require.NoError(t, err)
	require.Equal(t, inst.Ref, got.Ref)
	require.Equal(t, vendors.StateRunning, got.State)
}

func TestLaunchReplayIgnoresZoneTypeAndCapacity(t *testing.T) {
	e := newEnv(t)
	first, err := e.launch(t, request{nodeClaim: "nc-1", zone: "1"})
	require.NoError(t, err)
	puts := e.sub.Calls("vm.put")

	// The replay asks for another zone, size and capacity type: the original comes back, nothing is created.
	again, err := e.launch(t, request{nodeClaim: "nc-1", zone: "2", size: "Standard_D4s_v5", capacity: vendors.CapacitySpot})
	require.NoError(t, err)
	require.Equal(t, first.Ref, again.Ref)
	require.Equal(t, "1", again.Zone)
	require.Equal(t, "Standard_D2s_v5", again.Type)
	require.Equal(t, vendors.CapacityOnDemand, again.CapacityType)
	require.Equal(t, puts, e.sub.Calls("vm.put"))
	require.Len(t, e.sub.VMNames(), 1)
}

func TestLaunchNameConflictWithForeignVM(t *testing.T) {
	e := newEnv(t)
	l := e.spec(t, request{nodeClaim: "nc-1"})
	// A VM of that name exists without this cluster's tag.
	foreign := e.spec(t, request{nodeClaim: "nc-1"})
	foreign.Tags = map[string]string{"owner": "someone"}
	nic, err := e.clients.NICs.CreateOrUpdate(context.Background(), testRG, "n", l.NIC(""))
	require.NoError(t, err)
	_, err = e.clients.VMs.CreateOrUpdate(context.Background(), testRG, l.VMName(), foreign.VM(*nic.ID))
	require.NoError(t, err)
	_, err = e.c.Launch(context.Background(), l)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	require.Equal(t, codeNameConflict, vendors.CodeOf(err))
}

func TestLaunchFailuresAreClassifiedAndLeaveNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		r     request
		class vendors.Class
		code  string
	}{
		{"no capacity in the zone", func(e *env) { e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "1", false) },
			request{nodeClaim: "a"}, vendors.InsufficientCapacity, "ZonalAllocationFailed"},
		{"no capacity anywhere", func(e *env) { e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "", false) },
			request{nodeClaim: "a", zone: "0"}, vendors.InsufficientCapacity, "AllocationFailed"},
		{"spot capacity", func(e *env) { e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "", true) },
			request{nodeClaim: "a", capacity: vendors.CapacitySpot}, vendors.InsufficientCapacity, "ZonalAllocationFailed"},
		{"overconstrained", func(e *env) {
			e.sub.FailAllocation(testRegion, "Standard_D2s_v5", "1", "OverconstrainedZonalAllocationRequest")
		},
			request{nodeClaim: "a"}, vendors.InsufficientCapacity, "OverconstrainedZonalAllocationRequest"},
		{"sku not available on-demand", func(e *env) { e.sub.FailAllocation(testRegion, "Standard_D2s_v5", "1", "SkuNotAvailable") },
			request{nodeClaim: "a"}, vendors.Unsupported, "SkuNotAvailable"},
		{"sku not available spot", func(e *env) { e.sub.FailAllocation(testRegion, "Standard_D2s_v5", "1", "SkuNotAvailable") },
			request{nodeClaim: "a", capacity: vendors.CapacitySpot}, vendors.InsufficientCapacity, "SkuNotAvailable"},
		{"family quota", func(e *env) { e.sub.SetForeignUsage(testRegion, "standardDSv5Family", 15) },
			request{nodeClaim: "a"}, vendors.QuotaExceeded, "OperationNotAllowed"},
		{"regional quota", func(e *env) { e.sub.SetForeignUsage(testRegion, "cores", 99) },
			request{nodeClaim: "a"}, vendors.QuotaExceeded, "OperationNotAllowed"},
		{"spot quota", func(e *env) { e.sub.SetForeignUsage(testRegion, "lowPriorityCores", 19) },
			request{nodeClaim: "a", capacity: vendors.CapacitySpot}, vendors.QuotaExceeded, "OperationNotAllowed"},
		{"public IP quota", func(e *env) { e.sub.SetForeignUsage(testRegion, "StandardPublicIPAddresses", 10) },
			request{nodeClaim: "a"}, vendors.QuotaExceeded, "PublicIPCountLimitReached"},
		{"restricted zone", func(e *env) {}, request{nodeClaim: "a", size: "Standard_D8s_v5", zone: "3"}, vendors.Unsupported, "SkuNotAvailable"},
		{"throttled", func(e *env) { e.sub.FailNext("vm.put", fake.Error(429, "TooManyRequests", "slow down"), 1) },
			request{nodeClaim: "a"}, vendors.Retryable, "TooManyRequests"},
		{"denied", func(e *env) { e.sub.FailNext("vm.put", fake.Error(403, "AuthorizationFailed", "no"), 1) },
			request{nodeClaim: "a"}, vendors.Fatal, "AuthorizationFailed"},
		{"missing resource group", func(e *env) { e.sub.FailNext("nic.put", fake.Error(404, "ResourceGroupNotFound", "no rg"), 1) },
			request{nodeClaim: "a"}, vendors.NotFound, "ResourceGroupNotFound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			_, err := e.launch(t, c.r)
			require.Error(t, err)
			require.Equal(t, c.class, vendors.ClassOf(err), "%v", err)
			require.Equal(t, c.code, vendors.CodeOf(err), "%v", err)
			// A failed create leaves the key unused: no VM, no disk.
			require.Empty(t, e.sub.VMNames())
			require.Empty(t, e.sub.Disks())
		})
	}
}

func TestFailedCreateThenNextOfferingLaunches(t *testing.T) {
	e := newEnv(t)
	e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "1", false)
	_, err := e.launch(t, request{nodeClaim: "nc-1", zone: "1"})
	require.Equal(t, vendors.InsufficientCapacity, vendors.ClassOf(err))
	// The attempt loop tries another zone with the same NodeClaim.
	inst, err := e.launch(t, request{nodeClaim: "nc-1", zone: "2"})
	require.NoError(t, err)
	require.Equal(t, "2", inst.Zone)
}

func TestFailedVMLeftBehindIsDeleted(t *testing.T) {
	e := newEnv(t)
	e.sub.LeaveFailedVMs(true)
	e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "1", false)
	_, err := e.launch(t, request{nodeClaim: "nc-1", zone: "1"})
	require.Equal(t, vendors.InsufficientCapacity, vendors.ClassOf(err))
	require.Empty(t, e.sub.VMNames(), "the VM of the failed attempt is deleted before the error returns")
}

func TestFailedLeftoverOfAnotherZoneIsReplaced(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-1", zone: "1"})
	require.NoError(t, err)
	_, rg, name, _ := ParseInstanceRef(inst.Ref)
	require.NoError(t, e.sub.SetProvisioningState(rg, name, "Failed"))

	// Same zone: the Failed VM is the replay (it exists and costs; the NodeClaim deletes it).
	same, err := e.launch(t, request{nodeClaim: "nc-1", zone: "1"})
	require.NoError(t, err)
	require.Equal(t, inst.Ref, same.Ref)
	require.Equal(t, vendors.StateRunning, same.State)

	// Another zone: a leftover, deleted and launched afresh.
	other, err := e.launch(t, request{nodeClaim: "nc-1", zone: "2"})
	require.NoError(t, err)
	require.Equal(t, "2", other.Zone)
	require.Equal(t, vendors.StateRunning, other.State)
	require.Len(t, e.sub.VMNames(), 1)
}

func TestFailedLaunchWhoseCleanupFailsIsRetryable(t *testing.T) {
	e := newEnv(t)
	e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "1", false)
	e.sub.FailNext("vm.delete", fake.Error(409, "ScopeLocked", "locked"), -1)
	_, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.Equal(t, vendors.Retryable, vendors.ClassOf(err))
}

func TestLaunchWaitDeadlineReturnsPendingAndReplayFindsIt(t *testing.T) {
	e := newEnv(t)
	e.c.CreateTimeout = 50 * time.Millisecond
	e.sub.HoldCreates(true)
	inst, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	require.Equal(t, vendors.StatePending, inst.State, "still Creating at the deadline")

	// The operation finishes; the replay returns the VM, now running.
	e.sub.HoldCreates(false)
	e.sub.CompleteCreating()
	again, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	require.Equal(t, inst.Ref, again.Ref)
	require.Equal(t, vendors.StateRunning, again.State)
	require.Len(t, e.sub.VMNames(), 1)
}

func TestLaunchCanceledIsRetryableAndKeepsTheVM(t *testing.T) {
	e := newEnv(t)
	e.sub.HoldCreates(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := e.c.Launch(ctx, e.spec(t, request{nodeClaim: "nc-1"}))
	require.Equal(t, vendors.Retryable, vendors.ClassOf(err))
}

func TestGetNotFoundAndMalformedRef(t *testing.T) {
	e := newEnv(t)
	for _, ref := range []vendors.InstanceRef{
		InstanceRefOf(testRegion, testRG, "nope"), "westeurope/i-never-existed", "", InstanceRefOf(testRegion, "no-such-rg", "vm"),
	} {
		_, err := e.c.Get(context.Background(), ref)
		require.Equal(t, vendors.NotFound, vendors.ClassOf(err), string(ref))
	}
}

func TestPowerOffIsTerminating(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	_, rg, name, _ := ParseInstanceRef(inst.Ref)
	for _, dealloc := range []bool{false, true} {
		require.NoError(t, e.sub.PowerOff(rg, name, dealloc))
		got, err := e.c.Get(context.Background(), inst.Ref)
		require.NoError(t, err)
		require.Equal(t, vendors.StateTerminating, got.State)
		require.True(t, got.Gone())
	}
	// The garbage collector finds it in the list and deletes it.
	list, err := e.c.List(context.Background(), []string{testRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, e.c.Delete(context.Background(), inst.Ref))
	require.Empty(t, e.sub.VMNames())
}

func TestDeleteCascades(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-1", raw: `{"storage":{"data":[{"name":"d","diskSizeGB":10}]}}`})
	require.NoError(t, err)
	require.Len(t, e.sub.Disks(), 2)
	require.NoError(t, e.c.Delete(context.Background(), inst.Ref))
	require.Empty(t, e.sub.VMNames())
	require.Empty(t, e.sub.NICNames(), "the NIC goes with the VM")
	require.Empty(t, e.sub.PIPNames(), "and the public IP")
	require.Empty(t, e.sub.Disks(), "and the disks")
	_, err = e.c.Get(context.Background(), inst.Ref)
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err), "Azure keeps no terminated VMs")
}

func TestDeleteIdempotentAndMalformed(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.c.Delete(context.Background(), InstanceRefOf(testRegion, testRG, "gone")))
	require.NoError(t, e.c.Delete(context.Background(), "westeurope/i-never-existed"))
	require.NoError(t, e.c.Delete(context.Background(), InstanceRefOf(testRegion, "no-such-rg", "vm")))
}

func TestDeleteRefusesForeignVM(t *testing.T) {
	e := newEnv(t)
	other := &Compute{Clients: e.clients, Cluster: "another-cluster"}
	inst, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	err = other.Delete(context.Background(), inst.Ref)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	require.Equal(t, codeNotOwned, vendors.CodeOf(err))
	require.Len(t, e.sub.VMNames(), 1)
}

func TestDeleteErrorsAreClassified(t *testing.T) {
	e := newEnv(t)
	inst, err := e.launch(t, request{nodeClaim: "nc-1"})
	require.NoError(t, err)
	e.sub.FailNext("vm.delete", fake.Error(409, "ScopeLocked", "locked"), 1)
	require.Equal(t, vendors.Retryable, vendors.ClassOf(e.c.Delete(context.Background(), inst.Ref)))
	require.NoError(t, e.c.Delete(context.Background(), inst.Ref))
}

func TestFindAndList(t *testing.T) {
	e := newEnv(t)
	a, err := e.launch(t, request{nodeClaim: "nc-a"})
	require.NoError(t, err)
	_, err = e.launch(t, request{nodeClaim: "nc-b"})
	require.NoError(t, err)
	// A VM of another cluster, and one in another region.
	other := &Compute{Clients: e.clients, Cluster: "another-cluster"}
	req, img := request{nodeClaim: "nc-x"}.build(t)
	req.Tags[vendors.TagCluster] = "another-cluster"
	l, err := NewLaunchSpec(req, "another-cluster", img, nil, nil, "key")
	require.NoError(t, err)
	_, err = other.Launch(context.Background(), l)
	require.NoError(t, err)

	ctx := context.Background()
	got, err := e.c.Find(ctx, testRegion, map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "nc-a"})
	require.NoError(t, err)
	require.Equal(t, a.Ref, got.Ref)

	// The nodeclaim tag alone also finds it (the filter is by one tag, the rest is client side).
	got, err = e.c.Find(ctx, testRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: "nc-a"})
	require.NoError(t, err)
	require.Equal(t, a.Ref, got.Ref)

	_, err = e.c.Find(ctx, testRegion, map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "nope"})
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err))
	_, err = e.c.Find(ctx, "northeurope", map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err), "other region")

	list, err := e.c.List(ctx, []string{testRegion, "northeurope"}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	require.NoError(t, err)
	require.Len(t, list, 2)
	all, err := e.c.List(ctx, []string{testRegion}, nil)
	require.NoError(t, err)
	require.Len(t, all, 2, "without tags the cluster's VMs")
}

func TestListFallsBackToEveryVMWhenTheTagListingFails(t *testing.T) {
	e := newEnv(t)
	_, err := e.launch(t, request{nodeClaim: "nc-a"})
	require.NoError(t, err)
	e.sub.FailNext("resources.list", fake.Error(500, "InternalServerError", "boom"), -1)
	list, err := e.c.List(context.Background(), []string{testRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, vendors.StateRunning, list[0].State, "the fallback reads the instance view")
	require.Equal(t, 1, e.sub.Calls("vm.list"))
}

func TestListErrorWhenBothFail(t *testing.T) {
	e := newEnv(t)
	e.sub.FailNext("resources.list", fake.Error(500, "InternalServerError", "boom"), -1)
	e.sub.FailNext("vm.list", fake.Error(403, "AuthorizationFailed", "no"), -1)
	_, err := e.c.List(context.Background(), []string{testRegion}, nil)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
}

func TestLaunchWithMarketplacePlan(t *testing.T) {
	e := newEnv(t)
	plan := &ImagePlan{Name: "stable-gen2", Product: "flatcar-container-linux-free", Publisher: "kinvolk"}
	e.sub.AddImage("kinvolk", "flatcar-container-linux-free", "stable-gen2", "4547.1.0", planOf(plan))
	req, _ := request{nodeClaim: "nc-1"}.build(t)
	img := ImageRef{Publisher: "kinvolk", Offer: "flatcar-container-linux-free", SKU: "stable-gen2", Version: "4547.1.0"}
	mk := func(p *ImagePlan) LaunchSpec {
		l, err := NewLaunchSpec(req, testCluster, img, p, nil, "key")
		require.NoError(t, err)
		return l
	}
	_, err := e.c.Launch(context.Background(), mk(nil))
	require.Equal(t, "VMMarketplaceInvalidInput", vendors.CodeOf(err), "an image with a plan needs the plan on the VM")
	_, err = e.c.Launch(context.Background(), mk(plan))
	require.Equal(t, "MarketplacePurchaseEligibilityFailed", vendors.CodeOf(err), "and accepted terms")
	e.sub.AcceptTerms("kinvolk", "flatcar-container-linux-free", "stable-gen2", true)
	_, err = e.c.Launch(context.Background(), mk(plan))
	require.NoError(t, err)
}

func TestMissingImage(t *testing.T) {
	e := newEnv(t)
	req, _ := request{nodeClaim: "nc-1"}.build(t)
	l, err := NewLaunchSpec(req, testCluster, ImageRef{Publisher: "kinvolk", Offer: OfferAMD64, SKU: "stable-gen2", Version: "1.0.0"}, nil, nil, "key")
	require.NoError(t, err)
	_, err = e.c.Launch(context.Background(), l)
	require.Equal(t, vendors.NotFound, vendors.ClassOf(err))
	require.Empty(t, e.sub.VMNames())
}
