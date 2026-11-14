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

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

// ociServices are the four interfaces of the driver and the guard of each.
func ociServices(c *fake.Cloud, a *audit) []struct {
	name  string
	typ   reflect.Type
	guard any
} {
	return []struct {
		name  string
		typ   reflect.Type
		guard any
	}{
		{"ComputeAPI", reflect.TypeFor[oci.ComputeAPI](), &ociROCompute{inner: c, audit: a}},
		{"NetworkAPI", reflect.TypeFor[oci.NetworkAPI](), &ociRONetwork{inner: c, audit: a}},
		{"IdentityAPI", reflect.TypeFor[oci.IdentityAPI](), &ociROIdentity{inner: c, audit: a}},
		{"LimitsAPI", reflect.TypeFor[oci.LimitsAPI](), &ociROLimits{inner: c, audit: a}},
	}
}

// callOCI calls the method of a guard with an empty request; a panic of the fake on the empty
// request is not what is tested.
func callOCI(t *testing.T, g any, name string) (err error) {
	t.Helper()
	m := reflect.ValueOf(g).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("the guard has no method %s", name)
	}
	in := reflect.New(m.Type().In(m.Type().NumIn() - 1))
	args := []reflect.Value{reflect.ValueOf(context.Background())}
	if m.Type().NumIn() == 3 {
		args = append(args, reflect.ValueOf(fake.SeedRegion))
	}
	defer func() {
		if r := recover(); r != nil {
			err = nil
		}
	}()
	out := m.Call(append(args, in.Elem()))
	if e, ok := reflect.TypeAssert[error](out[len(out)-1]); ok && e != nil {
		return e
	}
	return nil
}

// Every method of the driver's four API interfaces is either a read that reaches the inner
// client or a write that is refused before it: the guard cannot let a write through by
// omission, and a method added to an interface fails the build of the wrappers first.
func TestOCIGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	total := 0
	for _, svc := range ociServices(fake.NewSeeded(), newAudit()) {
		total += svc.typ.NumMethod()
	}
	if total < 35 {
		t.Fatalf("the interfaces have only %d methods: is the reflection broken?", total)
	}
	for _, svc := range ociServices(fake.NewSeeded(), newAudit()) {
		for method := range svc.typ.Methods() {
			name := method.Name
			t.Run(svc.name+"/"+name, func(t *testing.T) {
				stmt, ok := ociStatements[svc.name+"."+name]
				if !ok {
					t.Fatalf("%s.%s has no policy statement in the table of oci_guard.go", svc.name, name)
				}
				inner := fake.NewSeeded()
				a := newAudit()
				var guard any
				for _, s := range ociServices(inner, a) {
					if s.name == svc.name {
						guard = s.guard
					}
				}
				err := callOCI(t, guard, name)
				if isOCIRead(name) {
					if errors.Is(err, errReadOnly) {
						t.Fatalf("the read %s was refused: %v", name, err)
					}
					if got := a.actions(); len(got) != 1 || got[0] != stmt {
						t.Fatalf("the read %s was not recorded as %q: %v", name, stmt, got)
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
				if got := a.refusals(); len(got) != 1 || got[0] != stmt {
					t.Fatalf("the refusal of %s was not recorded as %q: %v", name, stmt, got)
				}
				if len(a.actions()) != 0 {
					t.Fatalf("a refused write was recorded as a read: %v", a.actions())
				}
			})
		}
	}
}

// The reads reach the inner client (the fake counts the calls it answers).
func TestOCIGuardPassesTheReadsOn(t *testing.T) {
	for _, svc := range ociServices(fake.NewSeeded(), newAudit()) {
		for method := range svc.typ.Methods() {
			name := method.Name
			if !isOCIRead(name) {
				continue
			}
			inner := fake.NewSeeded()
			var guard any
			for _, s := range ociServices(inner, newAudit()) {
				if s.name == svc.name {
					guard = s.guard
				}
			}
			_ = callOCI(t, guard, name)
			if n := inner.Calls(name); n != 1 {
				t.Errorf("the read %s reached the inner client %d times, want 1", name, n)
			}
		}
	}
}

// The table has no entry for a method the interfaces do not have, so that a removed method
// cannot leave a statement behind in the canary's account of itself.
func TestOCIStatementTableMatchesTheInterfaces(t *testing.T) {
	var methods []string
	for _, svc := range ociServices(fake.NewSeeded(), newAudit()) {
		for m := range svc.typ.Methods() {
			methods = append(methods, svc.name+"."+m.Name)
		}
	}
	for name := range ociStatements {
		if !slices.Contains(methods, name) {
			t.Errorf("the table maps %s, which is not a method of the driver's interfaces", name)
		}
	}
	if len(ociStatements) != len(methods) {
		t.Errorf("the table has %d entries for %d methods", len(ociStatements), len(methods))
	}
}

func TestOCIWritesAreManageStatementsAndReadsAreNot(t *testing.T) {
	for name, stmt := range ociStatements {
		_, method, _ := strings.Cut(name, ".")
		manage := strings.HasPrefix(stmt, "manage ")
		if manage == isOCIRead(method) {
			t.Errorf("%s (%s): the verb and the statement disagree about whether it writes", name, stmt)
		}
		if !strings.HasSuffix(stmt, " in compartment") && !strings.HasSuffix(stmt, " in tenancy") {
			t.Errorf("%s: the statement %q has no scope", name, stmt)
		}
	}
}

func TestIsOCIRead(t *testing.T) {
	for op, want := range map[string]bool{
		"GetInstance": true, "ListShapes": true, "ListImageShapeCompatibilityEntries": true, "GetResourceAvailability": true,
		"LaunchInstance": false, "TerminateInstance": false, "CreateVcn": false, "DeleteSubnet": false,
		"AddNetworkSecurityGroupSecurityRules": false, "get": false, "": false, "Update": false,
	} {
		if got := isOCIRead(op); got != want {
			t.Errorf("isOCIRead(%q) = %t, want %t", op, got, want)
		}
	}
	if got := ociStatementOf("ComputeAPI", "Nope"); !strings.HasPrefix(got, "unmapped:") {
		t.Errorf("an unknown method: %q", got)
	}
}

func TestOCIGuardedClientsAreTheGuards(t *testing.T) {
	c := fake.NewSeeded()
	a := newAudit()
	g := ociReadOnlyClients(oci.Clients{Compute: c, Network: c, Identity: c, Limits: c}, a)
	if _, ok := g.Compute.(*ociROCompute); !ok {
		t.Errorf("the compute client is %T", g.Compute)
	}
	if _, ok := g.Network.(*ociRONetwork); !ok {
		t.Errorf("the network client is %T", g.Network)
	}
	if _, ok := g.Identity.(*ociROIdentity); !ok {
		t.Errorf("the identity client is %T", g.Identity)
	}
	if _, ok := g.Limits.(*ociROLimits); !ok {
		t.Errorf("the limits client is %T", g.Limits)
	}
}
