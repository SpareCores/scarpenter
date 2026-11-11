/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

const gcpProject = "my-project"

func gcpTestConfig() *config {
	return &config{
		vendor: "gcp", project: gcpProject, region: fake.SeedRegion, flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types:   []string{"gcp.e2-small", "gcp.t2a-standard-1", "gcp.c3-standard-4", "gcp.e2-small", "gcp.e2-medium"},
		armType: "gcp.t2a-standard-1", minOnDemand: 16, minSpot: 4,
	}
}

// gcpAccount is a fake project that satisfies the lane: the region and its zones, the lane's
// types, the Flatcar image with GVNIC, and quota (the seeded world has CPUS 24, C3_CPUS 8 and no
// preemptible quota: Spot runs on CPUS, so 16 + 4 fit).
func gcpAccount() *fake.Compute {
	c := fake.NewSeeded(gcpProject)
	for _, z := range []string{fake.SeedZoneA, fake.SeedZoneB} {
		c.AddMachineType(z, "e2-medium", 2, 4096, "X86_64")
	}
	return c
}

func runGCPCanary(t *testing.T, c *fake.Compute, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	runGCP(context.Background(), cfg, rep, readOnlyCompute(c, a), a)
	return rep, a
}

func TestGCPCanaryPassesOnAHealthyWorld(t *testing.T) {
	rep, a := runGCPCanary(t, gcpAccount(), gcpTestConfig())
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"regions and zones " + fake.SeedRegion, "Flatcar stable 4757.2.3 amd64", "Flatcar stable 4757.2.3 amd64 guest OS features",
		"quota vCPUs", "machine types offered", "read-only guard", "Flatcar arm64"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	// The arm64 type is not checked when the lane does not run arm64.
	if strings.Contains(b.String(), "t2a-standard-1(") {
		t.Errorf("the arm64 type was checked without E2E_ARM64:\n%s", b.String())
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
}

func TestGCPCanaryChecksArm64WhenTheLaneRunsIt(t *testing.T) {
	cfg := gcpTestConfig()
	cfg.arm64, cfg.arm64ImageProject = true, fake.SeedARMImageProject
	rep, _ := runGCPCanary(t, gcpAccount(), cfg)
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"Flatcar stable 4757.2.3 arm64", "Flatcar stable 4757.2.3 arm64 guest OS features", "t2a-standard-1("} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no %q:\n%s", want, b.String())
		}
	}
	// An arm64 image that does not exist fails only its own rows.
	cfg.arm64ImageProject = "no-such-images"
	rep, _ = runGCPCanary(t, gcpAccount(), cfg)
	f := failed(rep)
	if !slices.Contains(f, "Flatcar stable 4757.2.3 arm64") || slices.Contains(f, "Flatcar stable 4757.2.3 amd64") {
		t.Fatalf("failed: %v", f)
	}
}

func TestGCPCanaryReportsWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fake.Compute, *config)
		want   string
	}{
		"the Flatcar version is not published": {mutate: func(_ *fake.Compute, c *config) { c.flatcarVersion = "9999.0.0" }, want: "Flatcar stable 9999.0.0 amd64"},
		"the image lost its GVNIC feature": {mutate: func(c *fake.Compute, _ *config) {
			c.AddImage(fake.SeedImageProject, fake.SeedImage, "flatcar-stable", "X86_64", "UEFI_COMPATIBLE")
		}, want: "Flatcar stable 4757.2.3 amd64 guest OS features"},
		"the image is arm64": {mutate: func(c *fake.Compute, _ *config) {
			c.AddImage(fake.SeedImageProject, fake.SeedImage, "flatcar-stable", "ARM64", "GVNIC")
		}, want: "Flatcar stable 4757.2.3 amd64 guest OS features"},
		"the CPUS quota shrank":             {mutate: func(c *fake.Compute, _ *config) { c.SetLimit(fake.SeedRegion, "CPUS", 8) }, want: "quota vCPUs " + fake.SeedRegion},
		"the CPUS quota is gone":            {mutate: func(c *fake.Compute, _ *config) { c.SetLimit(fake.SeedRegion, "CPUS", 0) }, want: "quota vCPUs " + fake.SeedRegion},
		"Spot needs the standard quota too": {mutate: func(c *fake.Compute, _ *config) { c.SetLimit(fake.SeedRegion, "CPUS", 18) }, want: "quota vCPUs " + fake.SeedRegion},
		"the preemptible quota is too low": {mutate: func(c *fake.Compute, _ *config) {
			c.SetLimit(fake.SeedRegion, "PREEMPTIBLE_CPUS", 2)
		}, want: "quota vCPUs " + fake.SeedRegion},
		"the project-wide quota shrank": {mutate: func(c *fake.Compute, _ *config) { c.SetLimit("", "CPUS_ALL_REGIONS", 10) }, want: "quota vCPUs " + fake.SeedRegion},
		"a type is not offered": {mutate: func(c *fake.Compute, _ *config) {
			c.RemoveMachineType(fake.SeedZoneA, "c3-standard-4")
			c.RemoveMachineType(fake.SeedZoneB, "c3-standard-4")
		}, want: "machine types offered in " + fake.SeedRegion},
		"a type is unknown":         {mutate: func(_ *fake.Compute, c *config) { c.types = append(c.types, "gcp.nonexistent-standard-2") }, want: "machine types offered in " + fake.SeedRegion},
		"the region does not exist": {mutate: func(_ *fake.Compute, c *config) { c.region = "mars-north1" }, want: "regions and zones mars-north1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := gcpAccount()
			cfg := gcpTestConfig()
			tc.mutate(c, cfg)
			rep, _ := runGCPCanary(t, c, cfg)
			if f := failed(rep); !slices.Contains(f, tc.want) {
				var b bytes.Buffer
				rep.write(&b)
				t.Fatalf("want %q among the failed checks, got %v:\n%s", tc.want, f, b.String())
			}
		})
	}
}

// With preemptible quota in the region the standard quota need not cover Spot.
func TestGCPCanarySpotQuotaIsSeparateWhenTheRegionHasIt(t *testing.T) {
	c := gcpAccount()
	c.SetLimit(fake.SeedRegion, "PREEMPTIBLE_CPUS", 8)
	c.SetLimit(fake.SeedRegion, "CPUS", 16) // exactly the on-demand need
	rep, _ := runGCPCanary(t, c, gcpTestConfig())
	if f := failed(rep); len(f) > 0 {
		var b bytes.Buffer
		rep.write(&b)
		t.Fatalf("failed: %v\n%s", f, b.String())
	}
}

func TestGCPCanaryProjectMismatchFails(t *testing.T) {
	cfg := gcpTestConfig()
	cfg.project = "some-other-project" // the fake refuses calls for another project, like a 403
	rep, _ := runGCPCanary(t, gcpAccount(), cfg)
	if len(failed(rep)) == 0 {
		t.Fatal("a canary for another project passed")
	}
}

func TestLoadConfigGCP(t *testing.T) {
	env := map[string]string{
		"GCP_REGION": "us-central1", "GCP_PROJECT": "my-project", "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "gcp.e2-small", "E2E_TYPE_ARM64": "gcp.t2a-standard-1", "E2E_TYPE_DB": "gcp.c3-standard-4",
		"E2E_TYPES_DESCRIBED": "gcp.e2-small, gcp.e2-medium", "E2E_MIN_VCPU_OD": "24", "E2E_MIN_VCPU_SPOT": "4",
		"E2E_ARM64": "true", "E2E_ARM64_IMAGE_PROJECT": "my-images",
	}
	get := func(k string) string { return env[k] }
	c, err := loadConfig("gcp", get)
	if err != nil {
		t.Fatal(err)
	}
	if c.project != "my-project" || c.region != "us-central1" || !c.arm64 || c.arm64ImageProject != "my-images" || c.armType != "gcp.t2a-standard-1" {
		t.Errorf("config %+v", c)
	}
	delete(env, "GCP_PROJECT")
	if _, err := loadConfig("gcp", get); err == nil || !strings.Contains(err.Error(), "GCP_PROJECT") {
		t.Errorf("a missing project: %v", err)
	}
	env["GCP_PROJECT"] = "my-project"
	delete(env, "GCP_REGION")
	if _, err := loadConfig("gcp", get); err == nil || !strings.Contains(err.Error(), "GCP_REGION") {
		t.Errorf("a missing region: %v", err)
	}
	// Without the flag the lane does not run arm64.
	env["GCP_REGION"] = "us-central1"
	delete(env, "E2E_ARM64")
	if c, err = loadConfig("gcp", get); err != nil || c.arm64 {
		t.Errorf("arm64 without E2E_ARM64: %+v, %v", c, err)
	}
}

func TestGCPLaneTypes(t *testing.T) {
	cfg := gcpTestConfig()
	if got := laneTypes(cfg); !slices.Equal(got, []string{"gcp.e2-small", "gcp.c3-standard-4", "gcp.e2-medium"}) {
		t.Errorf("laneTypes = %v", got)
	}
	cfg.arm64 = true
	if got := laneTypes(cfg); !slices.Contains(got, "gcp.t2a-standard-1") {
		t.Errorf("the arm64 type is missing when arm64 runs: %v", got)
	}
}

// The role (test/e2e-gcp/canary-policy.sh) is exactly what the canary calls: every permission
// of the run is in the role, every permission in the role is a plain get or list, and the role
// holds nothing the canary never calls (the one exception is a permission only the arm64 run and a
// driver fallback use).
func TestGCPRoleIsExactlyWhatTheCanaryCalls(t *testing.T) {
	out, err := exec.Command("bash", "../e2e-gcp/canary-policy.sh", "role").Output()
	if err != nil {
		t.Skipf("the role script cannot run here (bash is needed): %v", err)
	}
	var allowed []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "- "); ok {
			allowed = append(allowed, p)
		}
	}
	if len(allowed) == 0 {
		t.Fatalf("no permissions in the role:\n%s", out)
	}
	for _, p := range allowed {
		parts := strings.Split(p, ".")
		verb := parts[len(parts)-1]
		if len(parts) != 3 || parts[0] != "compute" || (verb != "get" && verb != "list" && verb != "getFromFamily") {
			t.Errorf("the role grants %q, which is not a plain read", p)
		}
	}
	// The run with the arm64 lane covers every call the canary can make.
	cfg := gcpTestConfig()
	cfg.arm64, cfg.arm64ImageProject = true, fake.SeedARMImageProject
	_, a := runGCPCanary(t, gcpAccount(), cfg)
	for _, p := range a.actions() {
		if !slices.Contains(allowed, p) {
			t.Errorf("the canary uses %s, which the role does not grant", p)
		}
	}
	for _, p := range allowed {
		if !slices.Contains(a.actions(), p) {
			t.Errorf("the role grants %s, which the canary never uses", p)
		}
	}
}
