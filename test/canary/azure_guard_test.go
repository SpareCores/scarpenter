/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

// azureWrites are the write methods of the driver's API interfaces, azureReads the reads.
// A method of neither kind fails the test below: a new method must be classified.
var (
	azureWrites = map[string]bool{"CreateOrUpdate": true, "Delete": true}
	azureReads  = map[string]bool{
		"Get": true, "ListAll": true, "ListInGroup": true, "List": true, "Resources": true, "ListByTag": true,
		"ComputeUsages": true, "NetworkUsages": true, "ListSKUs": true, "ListVersions": true, "Accepted": true,
	}
	// fakeOps maps the field of the clients to the prefix of the fake's operation names.
	fakeOps = map[string]string{"VMs": "vm", "NICs": "nic", "PIPs": "pip", "Groups": "group", "VNets": "vnet", "NSGs": "nsg", "ASGs": "asg", "PPGs": "ppg"}
)

func fakeClients(sub *fake.Subscription) azure.Clients {
	return azure.Clients{
		VMs: sub.VMs(), NICs: sub.NICs(), PIPs: sub.PIPs(), Groups: sub.Groups(), VNets: sub.VNets(), NSGs: sub.NSGs(),
		ASGs: sub.ASGs(), PPGs: sub.PPGs(), Resources: sub.Resources(), SKUs: sub.SKUs(), Usages: sub.Usages(),
		Images: sub.Images(), Agreements: sub.Agreements(), Locations: sub.Locations(),
	}
}

// callAzure calls a method of a guarded API with empty arguments; a panic of the fake on them
// is not what is tested.
func callAzure(api reflect.Value, name string) (err error) {
	m := api.MethodByName(name)
	args := []reflect.Value{reflect.ValueOf(context.Background())}
	for i := 1; i < m.Type().NumIn(); i++ {
		args = append(args, reflect.Zero(m.Type().In(i)))
	}
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	out := m.Call(args)
	if e, ok := reflect.TypeAssert[error](out[len(out)-1]); ok && e != nil {
		return e
	}
	return nil
}

// Every method of every API interface of the driver is either a read that reaches the inner
// client and is audited, or a write that is refused before it: the guard cannot let a write
// through by omission.
func TestAzureGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	clientsType := reflect.TypeFor[azure.Clients]()
	if clientsType.NumField() < 14 {
		t.Fatalf("Clients has only %d APIs: is the reflection broken?", clientsType.NumField())
	}
	writes := 0
	for i := range clientsType.NumField() {
		field := clientsType.Field(i)
		for method := range field.Type.Methods() {
			op := field.Name + "." + method.Name
			t.Run(op, func(t *testing.T) {
				sub := fake.New("00000000-0000-0000-0000-000000000000")
				a := newAudit()
				guarded := readOnlyAzure(fakeClients(sub), a)
				api := reflect.ValueOf(guarded).Field(i)
				if api.IsNil() {
					t.Fatalf("the guard has no %s", field.Name)
				}
				err := callAzure(api, method.Name)
				switch {
				case azureReads[method.Name]:
					if errors.Is(err, errReadOnly) {
						t.Fatalf("the read %s was refused: %v", op, err)
					}
					if got := a.actions(); len(got) != 1 || got[0] != op {
						t.Fatalf("the read %s was not passed on and recorded: %v", op, got)
					}
					if len(a.refusals()) != 0 {
						t.Fatalf("a read was recorded as refused: %v", a.refusals())
					}
				case azureWrites[method.Name]:
					writes++
					if !errors.Is(err, errReadOnly) {
						t.Fatalf("the write %s was not refused: %v", op, err)
					}
					suffix := ".put"
					if method.Name == "Delete" {
						suffix = ".delete"
					}
					if n := sub.Calls(fakeOps[field.Name] + suffix); n != 0 {
						t.Fatalf("the write %s reached the inner client %d times", op, n)
					}
					if got := a.refusals(); len(got) != 1 || got[0] != op {
						t.Fatalf("the refusal of %s was not recorded: %v", op, got)
					}
					if len(a.actions()) != 0 {
						t.Fatalf("a refused write was recorded as a read: %v", a.actions())
					}
				default:
					t.Fatalf("%s is neither a known read nor a known write: classify it in the guard and here", op)
				}
			})
		}
	}
	// VMs, NICs, PIPs, Groups, VNets, NSGs, ASGs and PPGs each have both writes.
	if writes != 16 {
		t.Errorf("%d write methods were checked, want 16", writes)
	}
}

// A refused write does not change the account: the guard is in front of the fake.
func TestAzureGuardLeavesTheAccountUntouched(t *testing.T) {
	sub := fake.New("00000000-0000-0000-0000-000000000000")
	sub.AddResourceGroup("rg-example")
	a := newAudit()
	g := readOnlyAzure(fakeClients(sub), a)
	if err := g.Groups.Delete(context.Background(), "rg-example"); !errors.Is(err, errReadOnly) {
		t.Fatalf("the delete was not refused: %v", err)
	}
	if _, err := g.Groups.Get(context.Background(), "rg-example"); err != nil {
		t.Fatalf("the group is gone: %v", err)
	}
}
