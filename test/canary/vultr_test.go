/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	vfake "github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

func vultrConfig() *config {
	return &config{
		vendor: "vultr", region: "ewr", flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		types: []string{"vultr.vc2-1c-1gb", "vultr.vhf-1c-1gb"},
	}
}

func vultrAccount() *vfake.Account {
	a := vfake.New()
	a.AddPlan("vc2-1c-1gb", 1, 1024, 25)
	a.AddPlan("vhf-1c-1gb", 1, 1024, 32)
	a.AddRegion("ewr", "vc2-1c-1gb", "vhf-1c-1gb")
	a.AddRegion("fra", "vc2-1c-1gb")
	a.AddSnapshotAs("", "flatcar-stable-4757.2.3-amd64", vfake.SnapshotComplete, time.Time{})
	return a
}

func runVultrCanary(t *testing.T, a *vfake.Account, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	au := newAudit()
	runVultr(context.Background(), cfg, rep, readOnlyVultr(a, au), au)
	return rep, au
}

func TestVultrCanaryPassesOnAHealthyWorld(t *testing.T) {
	rep, a := runVultrCanary(t, vultrAccount(), vultrConfig())
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"API key", "region ewr", "Flatcar snapshot stable 4757.2.3 amd64", "plans in stock in ewr", "read-only guard"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
}

func TestVultrCanaryReportsWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*vfake.Account, *config)
		want   string
	}{
		"the snapshot is gone": {func(_ *vfake.Account, c *config) { c.flatcarVersion = "9999.0.0" }, "Flatcar snapshot stable 9999.0.0 amd64"},
		"the snapshot is twice": {func(a *vfake.Account, _ *config) {
			a.AddSnapshotAs("", "flatcar-stable-4757.2.3-amd64", vfake.SnapshotComplete, time.Time{})
		}, "Flatcar snapshot stable 4757.2.3 amd64"},
		"the region is gone":     {func(_ *vfake.Account, c *config) { c.region = "zzz" }, "region zzz"},
		"a plan is out of stock": {func(a *vfake.Account, _ *config) { a.SetAvailable("ewr", "vc2-1c-1gb") }, "plans in stock in ewr"},
		"a plan is unknown":      {func(_ *vfake.Account, c *config) { c.types = append(c.types, "vultr.nope-1c-1gb") }, "plans in stock in ewr"},
		"the key is refused": {func(a *vfake.Account, _ *config) {
			a.FailNext("ListInstances", -1, &vfake.Error{Status: 401, Message: "Unauthorized IP"})
		}, "API key"},
	} {
		t.Run(name, func(t *testing.T) {
			a, cfg := vultrAccount(), vultrConfig()
			tc.mutate(a, cfg)
			rep, _ := runVultrCanary(t, a, cfg)
			if f := failed(rep); !slices.Contains(f, tc.want) {
				var b bytes.Buffer
				rep.write(&b)
				t.Fatalf("want %q among the failed checks, got %v:\n%s", tc.want, f, b.String())
			}
		})
	}
}

func TestVultrLoadConfig(t *testing.T) {
	env := map[string]string{
		"VULTR_REGION": "ewr", "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "vultr.vc2-1c-1gb", "E2E_TYPE_SECOND": "vultr.vhf-1c-1gb",
		"E2E_TYPES_DESCRIBED": "vultr.vc2-1c-1gb, vultr.vc2-2c-4gb",
	}
	c, err := loadConfig("vultr", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vultr.vc2-1c-1gb", "vultr.vhf-1c-1gb", "vultr.vc2-1c-1gb", "vultr.vc2-2c-4gb"}; !slices.Equal(c.types, want) || c.region != "ewr" {
		t.Errorf("config %+v", c)
	}
	delete(env, "VULTR_REGION")
	if _, err := loadConfig("vultr", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "VULTR_REGION") {
		t.Errorf("a missing region: %v", err)
	}
}

// callVultr calls the method of the guard with zero-valued arguments; a panic of the fake on
// them is not what is tested.
func callVultr(g *roVultr, name string) (err error) {
	m := reflect.ValueOf(g).MethodByName(name)
	args := make([]reflect.Value, m.Type().NumIn())
	for i := range args {
		args[i] = reflect.Zero(m.Type().In(i))
	}
	args[0] = reflect.ValueOf(context.Background())
	for i := range args {
		if m.Type().In(i).Kind() == reflect.Pointer {
			args[i] = reflect.New(m.Type().In(i).Elem())
		}
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

// Every method of the driver's API interface is either a read that reaches the inner client
// or a write that is refused before it.
func TestVultrGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	typ := reflect.TypeFor[vultr.API]()
	if typ.NumMethod() < 20 {
		t.Fatalf("the interface has only %d methods: is the reflection broken?", typ.NumMethod())
	}
	for method := range typ.Methods() {
		name := method.Name
		t.Run(name, func(t *testing.T) {
			acct := vfake.New()
			a := newAudit()
			g := &roVultr{inner: acct, audit: a}
			err := callVultr(g, name)
			if isReadVultr(name) {
				if errors.Is(err, errReadOnly) {
					t.Fatalf("the read %s was refused: %v", name, err)
				}
				if got := a.actions(); len(got) != 1 || got[0] != "vultr:"+name {
					t.Fatalf("the read %s was not passed on and recorded: %v", name, got)
				}
				if acct.Calls(name) != 1 {
					t.Fatalf("the read %s did not reach the inner client", name)
				}
				return
			}
			if !errors.Is(err, errReadOnly) {
				t.Fatalf("the write %s was not refused: %v", name, err)
			}
			if n := acct.Calls(name); n != 0 {
				t.Fatalf("the write %s reached the inner client %d times", name, n)
			}
			if got := a.refusals(); len(got) != 1 || got[0] != "vultr:"+name {
				t.Fatalf("the refusal of %s was not recorded: %v", name, got)
			}
		})
	}
}

func TestIsReadVultr(t *testing.T) {
	for op, want := range map[string]bool{
		"ListInstances": true, "GetVPC": true, "RegionAvailability": true,
		"CreateInstance": false, "DeleteFirewallGroup": false, "CreateFirewallRule": false, "": false,
	} {
		if got := isReadVultr(op); got != want {
			t.Errorf("isReadVultr(%q) = %t, want %t", op, got, want)
		}
	}
}
