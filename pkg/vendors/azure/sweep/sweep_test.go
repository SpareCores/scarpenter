/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/sweep"
)

const (
	lane   = "rg-lane"
	region = "westeurope"
	old    = 3 * time.Hour
	young  = 30 * time.Minute
	// noAge is a resource without the created-at tag.
	noAge = time.Duration(-1)
	// noCluster is a resource without the cluster tag.
	noCluster = ""
	// anyAge asks the sweep for no age limit.
	anyAge = time.Duration(-1)
)

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type world struct {
	t   *testing.T
	sub *fake.Subscription
	c   azure.Clients
	// rg is the group the helpers create resources in.
	rg string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	sub := fake.New("00000000-0000-0000-0000-000000000000")
	sub.AddLocation(region, true)
	require.NoError(t, sub.AddSKU(region, fake.SKU{Name: "Standard_D2s_v5", Family: "fam", VCPUs: 2}))
	sub.AddImage("p", "o", "sku", "1.0.0", nil)
	sub.AddResourceGroup(lane)
	return &world{t: t, sub: sub, rg: lane, c: azure.Clients{
		VMs: sub.VMs(), NICs: sub.NICs(), PIPs: sub.PIPs(), Groups: sub.Groups(), VNets: sub.VNets(), NSGs: sub.NSGs(),
		ASGs: sub.ASGs(), PPGs: sub.PPGs(), Resources: sub.Resources(), SKUs: sub.SKUs(), Usages: sub.Usages(),
		Images: sub.Images(), Agreements: sub.Agreements(), Locations: sub.Locations(),
	}}
}

// tags are the tags of a resource of the cluster created age ago.
func tags(cluster string, age time.Duration) map[string]*string {
	m := map[string]*string{}
	if cluster != noCluster {
		m[azure.TagKey(vendors.TagCluster)] = &cluster
	}
	if age >= 0 {
		m[azure.TagKey("ci-created-at")] = new(now.Add(-age).Format(time.RFC3339))
	}
	return m
}

func (w *world) nic(name, cluster string, age time.Duration) string {
	w.t.Helper()
	n, err := w.sub.NICs().CreateOrUpdate(context.Background(), w.rg, name, armnetwork.Interface{
		Location: new(region), Tags: tags(cluster, age),
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
			Name: new("ip"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
				Subnet: &armnetwork.Subnet{ID: new("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/n")}},
		}}},
	})
	require.NoError(w.t, err)
	return *n.ID
}

// vm creates a VM with its NIC; the disks are the OS disk <name>-os and the
// data disks <name>-<d>, none with the Delete option (the inline disks of a VM
// that was deleted some other way stay behind).
func (w *world) vm(name, cluster string, age time.Duration, data ...string) {
	w.t.Helper()
	nicID := w.nic(name+"-nic", cluster, age)
	body := armcompute.VirtualMachine{
		Location: new(region), Tags: tags(cluster, age), Zones: []*string{new("1")},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes("Standard_D2s_v5"))},
			OSProfile:       &armcompute.OSProfile{CustomData: new(base64.StdEncoding.EncodeToString([]byte("x")))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{Publisher: new("p"), Offer: new("o"), SKU: new("sku"), Version: new("1.0.0")},
				OSDisk:         &armcompute.OSDisk{Name: new(name + "-os"), CreateOption: new(armcompute.DiskCreateOptionTypesFromImage)},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{ID: &nicID}}},
		},
	}
	for i, d := range data {
		body.Properties.StorageProfile.DataDisks = append(body.Properties.StorageProfile.DataDisks, &armcompute.DataDisk{
			Lun: new(int32(i)), Name: new(name + "-" + d), CreateOption: new(armcompute.DiskCreateOptionTypesEmpty),
		})
	}
	_, err := w.sub.VMs().CreateOrUpdate(context.Background(), w.rg, name, body)
	require.NoError(w.t, err)
}

func (w *world) pip(name, cluster string, age time.Duration) {
	w.t.Helper()
	_, err := w.sub.PIPs().CreateOrUpdate(context.Background(), w.rg, name, armnetwork.PublicIPAddress{Location: new(region), Tags: tags(cluster, age)})
	require.NoError(w.t, err)
}

func (w *world) vnet(name, cluster string, age time.Duration) {
	w.t.Helper()
	_, err := w.sub.VNets().CreateOrUpdate(context.Background(), w.rg, name, armnetwork.VirtualNetwork{
		Location: new(region), Tags: tags(cluster, age),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.240.0.0/16")}},
			Subnets: []*armnetwork.Subnet{{Name: new("nodes"),
				Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.240.0.0/20")}}},
		},
	})
	require.NoError(w.t, err)
}

// nsg creates a network security group and its application security group.
func (w *world) nsg(name, cluster string, age time.Duration) {
	w.t.Helper()
	_, err := w.sub.NSGs().CreateOrUpdate(context.Background(), w.rg, name, armnetwork.SecurityGroup{Location: new(region), Tags: tags(cluster, age)})
	require.NoError(w.t, err)
	w.asg(azure.ASGName(name), cluster, age)
}

func (w *world) asg(name, cluster string, age time.Duration) {
	w.t.Helper()
	_, err := w.sub.ASGs().CreateOrUpdate(context.Background(), w.rg, name, armnetwork.ApplicationSecurityGroup{Location: new(region), Tags: tags(cluster, age)})
	require.NoError(w.t, err)
}

func (w *world) ppg(name, cluster string, age time.Duration) {
	w.t.Helper()
	_, err := w.sub.PPGs().CreateOrUpdate(context.Background(), w.rg, name, armcompute.ProximityPlacementGroup{Location: new(region), Tags: tags(cluster, age)})
	require.NoError(w.t, err)
}

// group creates a resource group with the marker tags of a managed one.
func (w *world) group(name string, managed bool, cluster string, age time.Duration) {
	w.t.Helper()
	tg := tags(cluster, age)
	if managed {
		tg[azure.TagKey(vendors.TagManagedBy)] = new(vendors.ManagedByValue)
	}
	_, err := w.sub.Groups().CreateOrUpdate(context.Background(), name, armresources.ResourceGroup{Location: new(region), Tags: tg})
	require.NoError(w.t, err)
}

func (w *world) run(o sweep.Options) *sweep.Report {
	w.t.Helper()
	if o.ResourceGroup == "" {
		o.ResourceGroup = w.rg
	}
	switch o.MaxAge {
	case 0:
		o.MaxAge = 2 * time.Hour
	case anyAge:
		o.MaxAge = 0
	}
	o.Now = func() time.Time { return now }
	rep, err := sweep.Run(context.Background(), w.c, w.sub.ManagedDisks(), o)
	require.NoError(w.t, err)
	return rep
}

// vmName is the name of the VM of the cluster's node.
func vmName(cluster, node string) string { return azure.VMName(cluster, node) }

// setGroup makes the helpers create their resources in another group.
func (w *world) setGroup(name string) { w.rg = name }

func (w *world) left() []string {
	out := slices.Clone(w.sub.VMNames())
	out = append(out, w.sub.NICNames()...)
	out = append(out, w.sub.PIPNames()...)
	out = append(out, w.sub.Disks()...)
	return out
}

func kinds(rep *sweep.Report, o sweep.Outcome) []string {
	var out []string
	for _, i := range rep.Items {
		if i.Outcome == o {
			out = append(out, i.Kind+" "+i.Name)
		}
	}
	return out
}

const (
	oldCluster   = "scarpenter-ci-100"
	youngCluster = "scarpenter-ci-200"
)

func TestSweepsOldTaggedResourcesAndNothingElse(t *testing.T) {
	w := newWorld(t)
	oldVM, youngVM := vmName(oldCluster, "n1"), vmName(youngCluster, "n2")
	w.vm(oldVM, oldCluster, old, "data")
	w.vm(youngVM, youngCluster, young, "data")
	w.pip("pip-old", oldCluster, old)
	w.pip("pip-young", youngCluster, young)
	w.pip("pip-noage", oldCluster, noAge)
	w.pip("pip-foreign", "production", old)
	w.pip("pip-untagged", noCluster, old)
	w.pip("pip-similar", "xscarpenter-ci-1", old)
	w.pip("pip-bare-prefix", "scarpenter-ci-", old)
	w.vnet("vnet-old", oldCluster, old)
	w.vnet("vnet-foreign", "production", old)
	w.nsg("nsg-old", oldCluster, old)
	w.nsg("nsg-young", youngCluster, young)
	w.ppg("ppg-old", oldCluster, old)
	w.ppg("ppg-young", youngCluster, young)

	rep := w.run(sweep.Options{})

	require.ElementsMatch(t, []string{
		"vm " + oldVM, "nic " + oldVM + "-nic", "public-ip pip-old", "disk " + oldVM + "-os", "disk " + oldVM + "-data",
		"nsg nsg-old", "asg nsg-old-nodes", "vnet vnet-old", "ppg ppg-old",
	}, kinds(rep, sweep.Deleted))
	require.Equal(t, 0, rep.Count(sweep.Failed))

	// Everything else is still there: young, no age, foreign and lookalike clusters.
	require.Equal(t, []string{"rg-lane/" + youngVM}, w.sub.VMNames())
	require.Equal(t, []string{
		"rg-lane/" + youngVM + "-nic",
	}, w.sub.NICNames())
	require.Equal(t, []string{"rg-lane/pip-bare-prefix", "rg-lane/pip-foreign", "rg-lane/pip-noage", "rg-lane/pip-similar",
		"rg-lane/pip-untagged", "rg-lane/pip-young"}, w.sub.PIPNames())
	require.Equal(t, []string{"rg-lane/" + youngVM + "-data", "rg-lane/" + youngVM + "-os"}, w.sub.Disks())
	vnets, _ := w.c.VNets.ListAll(context.Background())
	require.Len(t, vnets, 1)
	require.Equal(t, "vnet-foreign", *vnets[0].Name)
	nsgs, _ := w.c.NSGs.ListAll(context.Background())
	require.Len(t, nsgs, 1)
	require.Equal(t, "nsg-young", *nsgs[0].Name)
	asgs, _ := w.c.ASGs.ListAll(context.Background())
	require.Len(t, asgs, 1)
	ppgs, _ := w.c.PPGs.ListAll(context.Background())
	require.Len(t, ppgs, 1)

	// The kept resources of a swept cluster are reported with the reason, the
	// foreign ones are not the sweeper's and not mentioned.
	skipped := kinds(rep, sweep.Skipped)
	require.Contains(t, skipped, "public-ip pip-young")
	require.Contains(t, skipped, "public-ip pip-noage")
	require.NotContains(t, strings.Join(skipped, " "), "pip-foreign")
	require.NotContains(t, strings.Join(skipped, " "), "pip-untagged")
	for _, i := range rep.Items {
		if i.Name == "pip-noage" {
			require.Contains(t, i.Reason, "creation time")
		}
		if i.Name == "pip-young" {
			require.Contains(t, i.Reason, "only 30m0s old")
		}
	}
}

func TestOrderIsDependenciesFirst(t *testing.T) {
	w := newWorld(t)
	vm := vmName(oldCluster, "n1")
	w.ppg("ppg", oldCluster, old)
	w.vnet("vnet", oldCluster, old)
	w.nsg("nsg", oldCluster, old)
	w.pip("pip", oldCluster, old)
	w.vm(vm, oldCluster, old)

	rep := w.run(sweep.Options{})

	order := make([]string, 0, len(rep.Items))
	for _, i := range rep.Items {
		order = append(order, i.Kind)
	}
	require.Equal(t, []string{"vm", "nic", "public-ip", "disk", "nsg", "asg", "vnet", "ppg"}, order)
	require.Zero(t, rep.Count(sweep.Failed))
}

func TestDisksAreMatchedByTheNameOfTheVM(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	swept := vmName(oldCluster, "n1")
	w.vm(swept, oldCluster, old, "data")
	// A VM that is already gone: deleted without its detached disks, while its
	// cluster still has an old resource (the NIC) to select it by.
	gone := vmName(oldCluster, "n2")
	w.vm(gone, oldCluster, old, "data")
	require.NoError(t, w.sub.VMs().Delete(ctx, lane, gone, true))
	// The same shape in a cluster with something young: not guessed at.
	goneYoung := vmName(youngCluster, "n3")
	w.vm(goneYoung, youngCluster, old)
	require.NoError(t, w.sub.VMs().Delete(ctx, lane, goneYoung, true))
	w.pip("pip-young", youngCluster, young)
	// A VM that is kept: its disks are attached and never touched.
	kept := vmName(youngCluster, "n4")
	w.vm(kept, youngCluster, young, "data")
	// Disks of someone else, with names that look similar.
	foreign := vmName("production", "n5")
	w.vm(foreign, "production", old)
	require.NoError(t, w.sub.VMs().Delete(ctx, lane, foreign, true))
	w.vm("other-vm", noCluster, old)
	require.NoError(t, w.sub.VMs().Delete(ctx, lane, "other-vm", true))

	rep := w.run(sweep.Options{})

	require.ElementsMatch(t, []string{"disk " + swept + "-os", "disk " + swept + "-data", "disk " + gone + "-os", "disk " + gone + "-data"},
		slices.DeleteFunc(kinds(rep, sweep.Deleted), func(s string) bool { return !strings.HasPrefix(s, "disk ") }))
	require.ElementsMatch(t, []string{
		"rg-lane/" + kept + "-data", "rg-lane/" + kept + "-os",
		"rg-lane/" + goneYoung + "-os", "rg-lane/" + foreign + "-os", "rg-lane/other-vm-os",
	}, w.sub.Disks())
	require.Contains(t, kinds(rep, sweep.Skipped), "disk "+kept+"-os")
}

func TestDryRunDeletesNothing(t *testing.T) {
	w := newWorld(t)
	vm := vmName(oldCluster, "n1")
	w.vm(vm, oldCluster, old, "data")
	w.vm(vmName(youngCluster, "n2"), youngCluster, young)
	w.pip("pip", oldCluster, old)
	w.vnet("vnet", oldCluster, old)
	w.nsg("nsg", oldCluster, old)
	w.ppg("ppg", oldCluster, old)
	before := w.left()

	rep := w.run(sweep.Options{DryRun: true})

	require.Equal(t, before, w.left())
	require.Zero(t, rep.Count(sweep.Deleted))
	require.ElementsMatch(t, []string{
		"vm " + vm, "nic " + vm + "-nic", "public-ip pip", "disk " + vm + "-os", "disk " + vm + "-data",
		"nsg nsg", "asg nsg-nodes", "vnet vnet", "ppg ppg",
	}, kinds(rep, sweep.WouldDelete))
	for _, n := range []string{"vm.delete", "nic.delete", "pip.delete", "nsg.delete", "asg.delete", "vnet.delete", "ppg.delete", "disk.delete", "group.delete"} {
		require.Zero(t, w.sub.Calls(n), n)
	}
}

func TestFailuresAreCollectedAndTheSweepGoesOn(t *testing.T) {
	w := newWorld(t)
	bad, good := vmName(oldCluster, "n1"), vmName(oldCluster, "n2")
	w.vm(bad, oldCluster, old, "data")
	w.vm(good, oldCluster, old)
	w.pip("pip", oldCluster, old)
	w.vnet("vnet", oldCluster, old)
	w.sub.FailNext("vm.delete", fake.Error(http.StatusConflict, "OperationNotAllowed", "busy"), 1)
	w.sub.FailNext("pip.delete", fake.Error(http.StatusInternalServerError, "InternalServerError", "boom"), 1)

	rep := w.run(sweep.Options{})

	failed := kinds(rep, sweep.Failed)
	// The VM that stayed keeps its NIC (in use) and its disks.
	require.Equal(t, []string{"vm " + bad, "nic " + bad + "-nic", "public-ip pip"}, failed)
	require.Contains(t, kinds(rep, sweep.Skipped), "disk "+bad+"-os")
	require.Contains(t, kinds(rep, sweep.Skipped), "disk "+bad+"-data")
	require.Equal(t, []string{"rg-lane/" + bad}, w.sub.VMNames())
	require.Contains(t, kinds(rep, sweep.Deleted), "vm "+good)
	require.Contains(t, kinds(rep, sweep.Deleted), "vnet vnet", "the later kinds are still swept")
	for _, i := range rep.Items {
		if i.Outcome == sweep.Failed {
			require.NotEmpty(t, i.Reason)
		}
	}
	// The next run finishes the job.
	rep = w.run(sweep.Options{})
	require.Zero(t, rep.Count(sweep.Failed))
	require.Empty(t, w.left())
}

func TestExactClusterAndNoAgeLimit(t *testing.T) {
	w := newWorld(t)
	a, b := vmName(oldCluster, "n1"), vmName(youngCluster, "n2")
	w.vm(a, oldCluster, young)
	w.vm(b, youngCluster, noAge)

	w.run(sweep.Options{Cluster: youngCluster, MaxAge: anyAge})

	require.Equal(t, []string{"rg-lane/" + a}, w.sub.VMNames(), "the exact cluster, young or without a creation time")
}

func TestMissingGroupIsNothingToSweep(t *testing.T) {
	w := newWorld(t)
	rep, err := sweep.Run(context.Background(), w.c, w.sub.ManagedDisks(), sweep.Options{ResourceGroup: "rg-none", MaxAge: time.Hour})
	require.NoError(t, err)
	require.Empty(t, rep.Items)
}

func TestListFailureIsAnError(t *testing.T) {
	w := newWorld(t)
	w.sub.FailNext("group.resources", fake.Error(http.StatusForbidden, "AuthorizationFailed", "no"), 1)
	_, err := sweep.Run(context.Background(), w.c, w.sub.ManagedDisks(), sweep.Options{ResourceGroup: lane})
	require.ErrorContains(t, err, "AuthorizationFailed")
}

func TestOptionsAreChecked(t *testing.T) {
	w := newWorld(t)
	_, err := sweep.Run(context.Background(), w.c, w.sub.ManagedDisks(), sweep.Options{})
	require.Error(t, err)
	_, err = sweep.Run(context.Background(), w.c, nil, sweep.Options{ResourceGroup: lane})
	require.Error(t, err)
	_, err = sweep.Run(context.Background(), w.c, nil, sweep.Options{ResourceGroup: lane, DryRun: true})
	require.NoError(t, err)
}

func TestApplicationSecurityGroupOfAnotherClusterStays(t *testing.T) {
	w := newWorld(t)
	w.nsg("nsg", oldCluster, old)
	_, err := w.sub.ASGs().CreateOrUpdate(context.Background(), w.rg, azure.ASGName("nsg"),
		armnetwork.ApplicationSecurityGroup{Location: new(region), Tags: tags("production", old)})
	require.NoError(t, err)

	w.run(sweep.Options{})

	asgs, _ := w.c.ASGs.ListAll(context.Background())
	require.Len(t, asgs, 1)
	nsgs, _ := w.c.NSGs.ListAll(context.Background())
	require.Empty(t, nsgs)
}

func TestDisposableResourceGroup(t *testing.T) {
	disposable := "scarpenter-" + oldCluster
	for name, tc := range map[string]struct {
		group    func(*world)
		content  func(*world)
		deleted  bool
		reason   string
		dryRunOK bool
	}{
		"managed, marked, old and empty after the sweep": {
			group:   func(w *world) { w.group(disposable, true, oldCluster, old) },
			content: func(w *world) { w.pip("pip", oldCluster, old) },
			deleted: true,
		},
		"something young is left": {
			group:   func(w *world) { w.group(disposable, true, oldCluster, old) },
			content: func(w *world) { w.pip("pip", youngCluster, young) },
			reason:  "not empty: 1 resources left",
		},
		"something of another cluster is left": {
			group:   func(w *world) { w.group(disposable, true, oldCluster, old) },
			content: func(w *world) { w.pip("pip", "production", old) },
			reason:  "not empty",
		},
		"the group is too young": {
			group:   func(w *world) { w.group(disposable, true, oldCluster, young) },
			content: func(w *world) {},
			reason:  "only 30m0s old",
		},
		"not managed by SCarpenter": {
			group:   func(w *world) { w.group(disposable, false, oldCluster, old) },
			content: func(w *world) {},
		},
		"another cluster's group": {
			group:   func(w *world) { w.group(disposable, true, "production", old) },
			content: func(w *world) {},
		},
		"a name that is not a run's": {
			group:   func(w *world) { w.group("scarpenter-lane", true, oldCluster, old) },
			content: func(w *world) {},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, dry := range []bool{false, true} {
				w := newWorld(t)
				tc.group(w)
				rg := disposable
				if strings.Contains(name, "name that is not") {
					rg = "scarpenter-lane"
				}
				// The resources go into the group under test.
				w.setGroup(rg)
				tc.content(w)
				rep := w.run(sweep.Options{ResourceGroup: rg, DryRun: dry})
				var got *sweep.Item
				for _, i := range rep.Items {
					if i.Kind == sweep.KindResourceGroup {
						got = &i
					}
				}
				groups, _ := w.c.Groups.List(context.Background())
				exists := slices.ContainsFunc(groups, func(g armresources.ResourceGroup) bool { return *g.Name == rg })
				switch {
				case tc.deleted && dry:
					require.NotNil(t, got)
					require.Equal(t, sweep.WouldDelete, got.Outcome)
					require.True(t, exists)
				case tc.deleted:
					require.NotNil(t, got)
					require.Equal(t, sweep.Deleted, got.Outcome)
					require.False(t, exists)
				case tc.reason != "":
					require.NotNil(t, got)
					require.Equal(t, sweep.Skipped, got.Outcome)
					require.Contains(t, got.Reason, tc.reason)
					require.True(t, exists)
				default:
					require.Nil(t, got, "a group that is not disposable is not even mentioned")
					require.True(t, exists)
				}
			}
		})
	}
}

// The lane's own pre-provisioned group is never deleted, however it is tagged
// and empty.
func TestTheLanesOwnGroupIsNeverDeleted(t *testing.T) {
	w := newWorld(t)
	w.group(lane, true, oldCluster, old)
	w.pip("pip", oldCluster, old)

	rep := w.run(sweep.Options{})

	require.Equal(t, []string{"public-ip pip"}, kinds(rep, sweep.Deleted))
	require.Equal(t, 0, w.sub.Calls("group.delete"))
}
