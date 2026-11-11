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
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

// callGCP calls the method of the guard with an empty request; a panic of the fake on the
// empty request is not what is tested.
func callGCP(t *testing.T, g *roCompute, name string) (err error) {
	t.Helper()
	m := reflect.ValueOf(g).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("the guard has no method %s", name)
	}
	in := reflect.New(m.Type().In(1).Elem())
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	out := m.Call([]reflect.Value{reflect.ValueOf(context.Background()), in})
	if e, ok := reflect.TypeAssert[error](out[len(out)-1]); ok && e != nil {
		return e
	}
	return nil
}

// Every method of the driver's Compute interface is either a read that reaches the inner
// client or a write that is refused before it: the guard cannot let a write through by
// omission, and a method added to the interface fails the build of the wrapper first.
func TestGCPGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	typ := reflect.TypeFor[gcp.ComputeAPI]()
	if typ.NumMethod() < 30 {
		t.Fatalf("the interface has only %d methods: is the reflection broken?", typ.NumMethod())
	}
	for method := range typ.Methods() {
		name := method.Name
		t.Run(name, func(t *testing.T) {
			perm, ok := permissions[name]
			if !ok {
				t.Fatalf("%s has no IAM permission in the table of gcp_guard.go", name)
			}
			inner := fake.NewSeeded("my-project")
			a := newAudit()
			g := &roCompute{inner: inner, audit: a}
			err := callGCP(t, g, name)
			if isGCPRead(name) {
				if errors.Is(err, errReadOnly) {
					t.Fatalf("the read %s was refused: %v", name, err)
				}
				if got := a.actions(); len(got) != 1 || got[0] != perm {
					t.Fatalf("the read %s was not recorded as %s: %v", name, perm, got)
				}
				if n := inner.Calls(name); n != 1 {
					t.Fatalf("the read %s reached the inner client %d times, want 1", name, n)
				}
				if len(a.refusals()) != 0 {
					t.Fatalf("a read was recorded as refused: %v", a.refusals())
				}
				return
			}
			if !errors.Is(err, errReadOnly) {
				t.Fatalf("the write %s was not refused: %v", name, err)
			}
			if n := inner.Calls(name); n != 0 {
				t.Fatalf("the write %s reached the inner client %d times", name, n)
			}
			if got := a.refusals(); len(got) != 1 || got[0] != perm {
				t.Fatalf("the refusal of %s was not recorded as %s: %v", name, perm, got)
			}
			if len(a.actions()) != 0 {
				t.Fatalf("a refused write was recorded as a read: %v", a.actions())
			}
		})
	}
}

// The table of permissions has no entry for a method the interface does not have, so that a
// removed method cannot leave a permission behind in the canary's account of itself.
func TestGCPPermissionTableMatchesTheInterface(t *testing.T) {
	var methods []string
	for m := range reflect.TypeFor[gcp.ComputeAPI]().Methods() {
		methods = append(methods, m.Name)
	}
	for name := range permissions {
		if !slices.Contains(methods, name) {
			t.Errorf("the table maps %s, which is not a method of gcp.ComputeAPI", name)
		}
	}
	if len(permissions) != len(methods) {
		t.Errorf("the table has %d entries for %d methods", len(permissions), len(methods))
	}
}

func TestGCPWritesAreWritePermissionsAndReadsAreNot(t *testing.T) {
	for name, perm := range permissions {
		write := strings.HasSuffix(perm, ".create") || strings.HasSuffix(perm, ".delete")
		if write == isGCPRead(name) {
			t.Errorf("%s (%s): the verb and the permission disagree about whether it writes", name, perm)
		}
	}
}

func TestIsGCPRead(t *testing.T) {
	for op, want := range map[string]bool{
		"GetImage": true, "ListZones": true, "AggregatedListInstances": true, "WaitZoneOperation": true,
		"InsertInstance": false, "DeleteNetwork": false, "SetLabels": false, "SetIamPolicy": false,
		"get": false, "": false, "Resize": false, "Start": false,
	} {
		if got := isGCPRead(op); got != want {
			t.Errorf("isGCPRead(%q) = %t, want %t", op, got, want)
		}
	}
}

func TestGuardedComputeIsTheGuard(t *testing.T) {
	a := newAudit()
	api := readOnlyCompute(fake.NewSeeded("my-project"), a)
	if _, ok := api.(*roCompute); !ok {
		t.Fatalf("the guarded client is %T", api)
	}
	// The wrapper that gives every call a deadline sits inside the guard in the command: a
	// refused call never reaches it, and what is passed on does.
	wrapped := readOnlyCompute(gcp.WithTimeouts(fake.NewSeeded("my-project"), 0, 0), a)
	if _, err := wrapped.InsertInstance(context.Background(), nil); !errors.Is(err, errReadOnly) {
		t.Fatalf("a write through the full stack: %v", err)
	}
}
