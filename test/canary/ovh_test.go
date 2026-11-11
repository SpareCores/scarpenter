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

	ovhdriver "github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

func testOVHConfig() *config {
	return &config{
		vendor: "ovh", region: "GRA11", flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types:       []string{"ovh.d2-2", "ovh.b3-8", "ovh.d2-4"},
		minOnDemand: 16,
	}
}

// ovhCloud is a fake project that satisfies the lane.
func ovhCloud() *fake.Cloud {
	c := fake.New()
	flavors := []osapi.Flavor{
		{Name: "d2-2", VCPUs: 1, RAMMiB: 2048, DiskGB: 25}, {Name: "d2-4", VCPUs: 2, RAMMiB: 4096, DiskGB: 50},
		{Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50}, {Name: "t1-45", VCPUs: 8, RAMMiB: 46080, DiskGB: 400},
	}
	c.AddRegion("GRA11", fake.RegionConfig{Flavors: flavors, Limits: osapi.Limits{MaxInstances: 20, MaxCores: 64, MaxRAMMiB: 262144}})
	c.AddRegion("SBG5", fake.RegionConfig{Flavors: flavors})
	for _, r := range []string{"GRA11", "SBG5"} {
		c.AddImage(r, osapi.Image{
			Name: "flatcar-stable-4757.2.3-amd64", Properties: map[string]string{"architecture": "x86_64"},
		})
	}
	return c
}

func runOVHCanary(t *testing.T, c *fake.Cloud, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	runOVH(context.Background(), cfg, rep, readOnlyOVH(c, a), a)
	return rep, a
}

func TestOVHCanaryPassesOnAHealthyProject(t *testing.T) {
	rep, a := runOVHCanary(t, ovhCloud(), testOVHConfig())
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"regions and zones GRA11", "Ext-Net network GRA11", "Flatcar stable 4757.2.3 amd64",
		"quota cores and instances GRA11", "flavors offered in GRA11", "read-only guard"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
	for _, act := range a.actions() {
		if _, op, _ := strings.Cut(act, ":"); !isRead(op) {
			t.Errorf("the canary made the call %s, which is not a read", act)
		}
	}
}

func TestOVHCanaryReportsWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fake.Cloud, *config)
		want   string
	}{
		"the image is not uploaded":   {func(_ *fake.Cloud, c *config) { c.flatcarVersion = "9999.0.0" }, "Flatcar stable 9999.0.0 amd64"},
		"the quota shrank":            {func(c *fake.Cloud, _ *config) { c.SetLimits("GRA11", osapi.Limits{MaxInstances: 20, MaxCores: 8}) }, "quota cores and instances GRA11"},
		"too few instances":           {func(c *fake.Cloud, _ *config) { c.SetLimits("GRA11", osapi.Limits{MaxInstances: 3, MaxCores: 64}) }, "quota cores and instances GRA11"},
		"a flavor is gone":            {func(_ *fake.Cloud, c *config) { c.types = append(c.types, "ovh.b3-9999") }, "flavors offered in GRA11"},
		"a flavor is not launched":    {func(_ *fake.Cloud, c *config) { c.types = append(c.types, "ovh.t1-45") }, "flavors offered in GRA11"},
		"the region left the catalog": {func(_ *fake.Cloud, c *config) { c.region = "WAW1" }, "regions and zones WAW1"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cfg := ovhCloud(), testOVHConfig()
			tc.mutate(c, cfg)
			rep, _ := runOVHCanary(t, c, cfg)
			if f := failed(rep); !slices.Contains(f, tc.want) {
				var b bytes.Buffer
				rep.write(&b)
				t.Fatalf("want %q among the failed checks, got %v:\n%s", tc.want, f, b.String())
			}
		})
	}
}

// Every method of the driver's OpenStack interfaces is either a read that reaches the inner
// client or a write that is refused before it: the guard cannot let a write through by
// omission.
func TestOVHGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	c := ovhCloud()
	a := newAudit()
	p := readOnlyOVH(c, a)
	cl, err := p.Clients(context.Background(), "GRA11")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, iface := range []any{cl.Compute, cl.Network, cl.Images, cl.Volumes} {
		v := reflect.ValueOf(iface)
		for i := range v.NumMethod() {
			name := v.Type().Method(i).Name
			total++
			t.Run(name, func(t *testing.T) {
				before := len(a.refusals())
				readsBefore := len(a.actions())
				m := v.Method(i)
				args := make([]reflect.Value, m.Type().NumIn())
				for j := range args {
					args[j] = reflect.New(m.Type().In(j)).Elem()
				}
				args[0] = reflect.ValueOf(context.Background())
				var err error
				func() {
					defer func() { _ = recover() }() // the fake on an empty input is not what is tested
					out := m.Call(args)
					if e, ok := reflect.TypeAssert[error](out[len(out)-1]); ok {
						err = e
					}
				}()
				if isRead(name) {
					if errors.Is(err, errReadOnly) {
						t.Fatalf("the read %s was refused", name)
					}
					if len(a.refusals()) != before {
						t.Fatalf("the read %s was recorded as a refusal", name)
					}
					_ = readsBefore
					return
				}
				if !errors.Is(err, errReadOnly) {
					t.Fatalf("the write %s was not refused: %v", name, err)
				}
				if len(a.refusals()) != before+1 {
					t.Fatalf("the refusal of %s was not recorded", name)
				}
			})
		}
	}
	if total < 15 {
		t.Fatalf("only %d methods: is the reflection broken?", total)
	}
	// The servers the refused create would have made do not exist.
	if ids := c.ServerIDs("GRA11"); len(ids) != 0 {
		t.Errorf("a write reached the cloud: %v", ids)
	}
	if _, err := p.Regions(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestOVHConfig(t *testing.T) {
	env := map[string]string{
		"OS_REGION_NAME": "GRA11", "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "ovh.d2-2", "E2E_TYPE_DB": "ovh.b3-8", "E2E_TYPES_DESCRIBED": "ovh.d2-2, ovh.d2-4",
		"E2E_MIN_VCPU_OD": "16",
	}
	c, err := loadConfig("ovh", func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("an OVH lane has no arm64 type and no spot quota: %v", err)
	}
	if c.region != "GRA11" || c.minOnDemand != 16 || !slices.Equal(c.types, []string{"ovh.d2-2", "ovh.b3-8", "ovh.d2-2", "ovh.d2-4"}) {
		t.Errorf("config %+v", c)
	}
	delete(env, "OS_REGION_NAME")
	if _, err := loadConfig("ovh", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "OS_REGION_NAME") {
		t.Errorf("a missing region: %v", err)
	}
	if _, err := loadConfig("aws", func(k string) string { return env[k] }); err == nil {
		t.Error("an AWS lane still needs its arm64 type and spot quota")
	}
}

func TestOVHCanaryNeedsTheDriverAndTheCatalog(t *testing.T) {
	c := fake.New() // an empty project: no region
	rep, _ := runOVHCanary(t, c, testOVHConfig())
	if f := failed(rep); !slices.Contains(f, "regions and zones GRA11") {
		t.Errorf("failed: %v", f)
	}
	_ = ovhdriver.Name
}
