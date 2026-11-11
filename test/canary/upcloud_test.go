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

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

const upcloudZone = "fi-hel1"

func upcloudConfig() *config {
	// The UpCloud checks make no HTTP call; the artifact checks are shared and tested elsewhere.
	return &config{
		vendor: "upcloud", region: upcloudZone, flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types:    []string{"upcloud.STARTER-1xCPU-1GB", "upcloud.STARTER-2xCPU-4GB"},
		minCores: 10, minMemoryMB: 12288,
	}
}

// upcloudWorld is a fake account that satisfies the lane: the zone, its plans, a Flatcar
// template of the pinned release and limits above what the suite needs.
func upcloudWorld() *fake.Fake {
	plan := func(name string, cores, memGB, disk int) fake.PlanSpec {
		return fake.PlanSpec{Plan: upcloud.Plan{
			Name: name, CoreNumber: cores, MemoryAmount: memGB * 1024, StorageSize: disk, StorageTier: upcloud.StorageTierMaxIOPS}}
	}
	f := fake.New(fake.Options{
		Zones:  []string{upcloudZone, "de-fra1"},
		Plans:  []fake.PlanSpec{plan("STARTER-1xCPU-1GB", 1, 1, 10), plan("STARTER-2xCPU-4GB", 2, 4, 30)},
		Limits: upcloud.ResourceLimits{Cores: 20, Memory: 32768},
	})
	f.AddTemplate(upcloudZone, "flatcar-stable-4757.2.3-amd64-maxiops", upcloud.StorageTierMaxIOPS, 10, map[string]string{
		"scarpenter-image": "flatcar", "flatcar-channel": "stable", "flatcar-version": "4757.2.3",
		"flatcar-arch": "amd64", "flatcar-tier": "maxiops", "scarpenter-use": "true",
	})
	return f
}

func runUpCloudCanary(t *testing.T, f *fake.Fake, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	runUpCloud(context.Background(), cfg, rep, readOnlyUpCloud(f, a), a)
	return rep, a
}

func TestUpCloudCanaryPassesOnAHealthyWorld(t *testing.T) {
	rep, a := runUpCloudCanary(t, upcloudWorld(), upcloudConfig())
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"API token", "regions and zones fi-hel1", "Flatcar stable 4757.2.3 amd64 template",
		"account limits cores and memory", "plans offered in fi-hel1", "read-only guard"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
	for _, act := range a.actions() {
		if !strings.HasPrefix(act, "upcloud:Get") {
			t.Errorf("the canary recorded %q, which is not a read", act)
		}
	}
}

func TestUpCloudCanaryReportsWhatMoved(t *testing.T) {
	t.Run("no template", func(t *testing.T) {
		f2 := fake.New(fake.Options{Zones: []string{upcloudZone}, Limits: upcloud.ResourceLimits{Cores: 20, Memory: 32768}})
		rep, _ := runUpCloudCanary(t, f2, upcloudConfig())
		if got := failed(rep); !slices.Contains(got, "Flatcar stable 4757.2.3 amd64 template") {
			t.Errorf("failed %v, want the template", got)
		}
	})
	t.Run("limits too small", func(t *testing.T) {
		f := upcloudWorld()
		f.SetLimits(upcloud.ResourceLimits{Cores: 2, Memory: 1024})
		rep, _ := runUpCloudCanary(t, f, upcloudConfig())
		if got := failed(rep); !slices.Equal(got, []string{"account limits cores and memory"}) {
			t.Errorf("failed %v", got)
		}
	})
	t.Run("no limits at all", func(t *testing.T) {
		f := upcloudWorld()
		f.SetLimits(upcloud.ResourceLimits{})
		rep, _ := runUpCloudCanary(t, f, upcloudConfig())
		if got := failed(rep); !slices.Contains(got, "account limits cores and memory") {
			t.Errorf("failed %v", got)
		}
	})
	t.Run("a plan is gone", func(t *testing.T) {
		cfg := upcloudConfig()
		cfg.types = append(cfg.types, "upcloud.STARTER-9xCPU-99GB")
		rep, _ := runUpCloudCanary(t, upcloudWorld(), cfg)
		if got := failed(rep); !slices.Equal(got, []string{"plans offered in fi-hel1"}) {
			t.Errorf("failed %v", got)
		}
	})
	t.Run("the zone is not the account's", func(t *testing.T) {
		cfg := upcloudConfig()
		cfg.region = "no-such1"
		rep, _ := runUpCloudCanary(t, upcloudWorld(), cfg)
		if got := failed(rep); !slices.Contains(got, "regions and zones no-such1") {
			t.Errorf("failed %v", got)
		}
	})
	t.Run("the token is refused", func(t *testing.T) {
		f := upcloudWorld()
		f.InjectCode("GetAccount", "AUTHENTICATION_FAILED", 401, 0)
		rep, _ := runUpCloudCanary(t, f, upcloudConfig())
		got := failed(rep)
		if !slices.Contains(got, "API token") {
			t.Errorf("failed %v", got)
		}
		var b bytes.Buffer
		rep.write(&b)
		if !strings.Contains(b.String(), "rotate UPCLOUD_TOKEN") {
			t.Errorf("the failure does not say what to do:\n%s", b.String())
		}
	})
}

// Every method of the driver's API is either a read that reaches the inner client or a write
// that is refused before it: the guard cannot let a write through by omission.
func TestUpCloudGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	typ := reflect.TypeFor[api.API]()
	if typ.NumMethod() < 25 {
		t.Fatalf("the interface has only %d methods: is the reflection broken?", typ.NumMethod())
	}
	for method := range typ.Methods() {
		name := method.Name
		t.Run(name, func(t *testing.T) {
			f := upcloudWorld()
			a := newAudit()
			g := &roUpCloud{inner: f, audit: a}
			err := callUpCloud(g, name)
			if isUpCloudRead(name) {
				if errors.Is(err, errReadOnly) {
					t.Fatalf("the read %s was refused: %v", name, err)
				}
				if got := a.actions(); len(got) != 1 || got[0] != "upcloud:"+name {
					t.Fatalf("the read %s was not recorded: %v", name, got)
				}
				if f.Calls(name) != 1 {
					t.Fatalf("the read %s reached the inner client %d times", name, f.Calls(name))
				}
				return
			}
			if !errors.Is(err, errReadOnly) {
				t.Fatalf("the write %s was not refused: %v", name, err)
			}
			if n := f.Calls(name); n != 0 {
				t.Fatalf("the write %s reached the inner client %d times", name, n)
			}
			if got := a.refusals(); len(got) != 1 || got[0] != "upcloud:"+name {
				t.Fatalf("the refusal of %s was not recorded: %v", name, got)
			}
			if len(a.actions()) != 0 {
				t.Fatalf("a refused write was recorded as a read: %v", a.actions())
			}
		})
	}
}

// callUpCloud calls a method of the guard with empty inputs and returns its error; a panic of
// the inner fake on an empty input is not what is tested.
func callUpCloud(g *roUpCloud, name string) (err error) {
	m := reflect.ValueOf(g).MethodByName(name)
	mt := m.Type()
	n := mt.NumIn()
	if mt.IsVariadic() {
		n--
	}
	args := make([]reflect.Value, 0, n)
	args = append(args, reflect.ValueOf(context.Background()))
	for i := 1; i < n; i++ {
		in := mt.In(i)
		if in.Kind() == reflect.Pointer {
			args = append(args, reflect.New(in.Elem()))
		} else {
			args = append(args, reflect.Zero(in))
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

func TestIsUpCloudRead(t *testing.T) {
	for op, want := range map[string]bool{
		"GetZones": true, "GetServersWithFilters": true, "GetFirewallRulesets": true,
		"CreateServer": false, "DeleteServerAndStorages": false, "WaitForServerState": false, "ModifyStorage": false,
		"AttachPrivateFirewallRuleset": false, "get": false, "": false,
	} {
		if got := isUpCloudRead(op); got != want {
			t.Errorf("isUpCloudRead(%q) = %t, want %t", op, got, want)
		}
	}
}

func TestLoadConfigUpCloud(t *testing.T) {
	env := map[string]string{
		"UPCLOUD_ZONE": upcloudZone, "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64":      "upcloud.STARTER-1xCPU-1GB",
		"E2E_TYPES_DESCRIBED": "upcloud.STARTER-1xCPU-1GB, upcloud.STARTER-2xCPU-4GB",
		"E2E_MIN_CORES":       "10", "E2E_MIN_MEMORY_MB": "12288",
	}
	get := func(k string) string { return env[k] }
	c, err := loadConfig("upcloud", get)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"upcloud.STARTER-1xCPU-1GB", "upcloud.STARTER-1xCPU-1GB", "upcloud.STARTER-2xCPU-4GB"}; !slices.Equal(c.types, want) {
		t.Errorf("types %v", c.types)
	}
	if c.minCores != 10 || c.minMemoryMB != 12288 || c.region != upcloudZone {
		t.Errorf("config %+v", c)
	}
	env["E2E_MIN_CORES"] = "many"
	if _, err := loadConfig("upcloud", get); err == nil {
		t.Error("a bad number was accepted")
	}
	env["E2E_MIN_CORES"] = "10"
	delete(env, "E2E_MIN_MEMORY_MB")
	if _, err := loadConfig("upcloud", get); err == nil || !strings.Contains(err.Error(), "E2E_MIN_MEMORY_MB") {
		t.Errorf("a missing pin: %v", err)
	}
}

func TestRunUpCloudNeedsItsPins(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"-vendor", "upcloud"}, func(string) string { return "" }, &out, &errb); rc != 2 ||
		!strings.Contains(errb.String(), "run test/canary/run.sh upcloud") {
		t.Errorf("no pins: rc %d, %q", rc, errb.String())
	}
}
