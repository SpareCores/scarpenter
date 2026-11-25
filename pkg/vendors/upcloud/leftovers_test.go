/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"errors"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

func (e *env) hasStorage(uuid string) bool {
	for _, st := range e.f.Storages() {
		if st.UUID == uuid {
			return true
		}
	}
	return false
}

func nodeLabels(cluster, claim string) map[string]string {
	return map[string]string{
		"scarpenter-cluster": cluster, "scarpenter-managed-by": vendors.ManagedByValue, "scarpenter-nodeclaim": claim,
	}
}

func TestACollectorRemovesAFailedStorageDeleteOnceTheNodeClaimIsGone(t *testing.T) {
	e := newEnv(t, envOptions{})
	inst, err := e.d.CreateInstance(e.ctx(), e.request("nc-a", testType))
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, _ := uc.ParseRef(inst.Ref)
	srv, _ := e.f.Server(uuid)
	own := srv.StorageDevices[0].UUID

	// The server delete works, the storage delete fails: the storage is left behind.
	e.f.InjectCode("DeleteStorage", "INTERNAL_ERROR", 500, 1)
	if err := e.d.DeleteInstance(e.ctx(), inst.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("delete with a failing storage delete: %v", err)
	}
	if err := e.d.DeleteInstance(e.ctx(), inst.Ref); err != nil {
		t.Fatalf("the retry finds the server gone: %v", err)
	}
	if !e.hasStorage(own) {
		t.Fatal("the storage is gone")
	}

	// Storages the collector must not touch. One is attached to a server of someone
	// else, whatever its labels say.
	attached := e.f.AddStorage(testZone, "attached", "maxiops", 10, nodeLabels(testCluster, "nc-gone"))
	other := e.f.AddServer(fake.ServerSpec{Zone: testZone, Plan: testType, Title: "theirs"})
	if err := e.f.AttachStorage(other, attached); err != nil {
		t.Fatal(err)
	}
	offered := nodeLabels(testCluster, "nc-gone")
	offered[uc.LabelUse] = "true"
	keep := []string{
		attached,
		e.f.AddStorage(testZone, "unlabelled", "maxiops", 10, nil),
		e.f.AddStorage(testZone, "other cluster", "maxiops", 10, nodeLabels("another-cluster", "nc-gone")),
		e.f.AddStorage(testZone, "cluster only", "maxiops", 10, map[string]string{"scarpenter-cluster": testCluster}),
		e.f.AddStorage(testZone, "offered", "maxiops", 10, offered),
		e.f.AddStorage("de-fra1", "other zone", "maxiops", 10, nodeLabels(testCluster, "nc-gone")),
	}

	// The NodeClaim still exists: nothing is deleted.
	n, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return true })
	if err != nil || n != 0 || !e.hasStorage(own) {
		t.Fatalf("with the NodeClaim alive: %d %v", n, err)
	}
	// The NodeClaim is gone: only the labelled, unattached storage goes.
	n, err = e.d.CollectLeftovers(e.ctx(), testZone, func(name string) bool { return name != "nc-a" })
	if err != nil || n != 1 {
		t.Fatalf("with the NodeClaim gone: %d %v", n, err)
	}
	if e.hasStorage(own) {
		t.Error("the leftover storage survived")
	}
	for _, id := range keep {
		if !e.hasStorage(id) {
			t.Errorf("storage %s was deleted", id)
		}
	}
	// Even with every NodeClaim gone, what is not ours stays.
	n, err = e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false })
	if err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
	for _, id := range keep {
		if !e.hasStorage(id) {
			t.Errorf("storage %s was deleted by the second pass", id)
		}
	}
}

func TestACollectorSkipsAStorageThatIsNotOnline(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.f.AddStorage(testZone, "cloning", "maxiops", 10, nodeLabels(testCluster, "nc-gone"))
	e.f.SetStorageState(id, upcloud.StorageStateMaintenance)
	if n, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false }); err != nil || n != 0 {
		t.Fatalf("collect: %d %v", n, err)
	}
	if !e.hasStorage(id) {
		t.Error("a storage in maintenance was deleted")
	}
}

func TestACollectorReportsAFailedDeleteAndCountsTheRest(t *testing.T) {
	e := newEnv(t, envOptions{})
	a := e.f.AddStorage(testZone, "a", "maxiops", 10, nodeLabels(testCluster, "nc-1"))
	b := e.f.AddStorage(testZone, "b", "maxiops", 10, nodeLabels(testCluster, "nc-2"))
	e.f.InjectCode("DeleteStorage", "INTERNAL_ERROR", 500, 1)
	n, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false })
	if !vendors.IsClass(err, vendors.Retryable) || n != 1 {
		t.Fatalf("collect: %d %v", n, err)
	}
	if e.hasStorage(a) == e.hasStorage(b) {
		t.Error("exactly one storage should be left")
	}
}

func TestACollectorFailsOnAListError(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.f.Inject("GetStorages", errors.New("boom"), 1)
	if _, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false }); err == nil {
		t.Fatal("no error")
	}
}

func TestACollectorDoesNotCountAStorageAttachedBeforeItsDelete(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := e.f.AddStorage(testZone, "leftover", "maxiops", 10, nodeLabels(testCluster, "nc-gone"))
	other := e.f.AddServer(fake.ServerSpec{Zone: testZone, Plan: testType, Title: "theirs"})
	// Attached after the collector's fresh read, just before its delete.
	e.f.BeforeNext("DeleteStorage", func() {
		if err := e.f.AttachStorage(other, id); err != nil {
			t.Error(err)
		}
	})
	n, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false })
	if err != nil || n != 0 {
		t.Fatalf("collect: %d %v, want nothing deleted and no error", n, err)
	}
	if !e.hasStorage(id) {
		t.Error("an attached storage was deleted")
	}
	// A storage that is gone before its delete is not counted either.
	gone := e.f.AddStorage(testZone, "gone", "maxiops", 10, nodeLabels(testCluster, "nc-gone"))
	e.f.BeforeNext("DeleteStorage", func() { e.f.RemoveStorage(gone) })
	if n, err := e.d.CollectLeftovers(e.ctx(), testZone, func(string) bool { return false }); err != nil || n != 0 {
		t.Fatalf("collect with a vanished storage: %d %v", n, err)
	}
}
