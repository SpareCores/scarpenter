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

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

func ociTestConfig() *config {
	return &config{
		vendor: "oci", region: fake.SeedRegion, compartment: fake.SeedCompartment, tenancy: fake.SeedTenancy,
		flatcarChannel: "stable", flatcarVersion: fake.SeedVersion,
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types: []string{"oci.VM.Standard.E5.Flex.1-4", "oci.VM.Standard.A1.Flex.1-2", "oci.VM.Standard.A2.Flex.1-8",
			"oci.VM.Standard.E5.Flex.1-4", "oci.VM.Standard.E5.Flex.1-8", "oci.VM.Standard.E5.Flex.2-8"},
		armType: "oci.VM.Standard.A1.Flex.1-2", minOCPU: 8, minMemoryGB: 32,
	}
}

// ociAccount is a fake tenancy that satisfies the lane: the region with its three availability
// domains, the E5, A1 and A2 flexible shapes with their limits, and the Flatcar image of both
// architectures registered for them.
func ociAccount() *fake.Cloud {
	c := fake.NewSeeded()
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E5.Flex", 94, 1049, 1, 64, 0, 0, "standard-e5-core-count", "standard-e5-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A1.Flex", 80, 512, 1, 64, 0, 0, "standard-a1-core-count", "standard-a1-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A2.Flex", 78, 946, 1, 64, 0, 0, "standard-a2-core-count", "standard-a2-memory-count"))
	arm := fake.FlatcarImage("stable", fake.SeedVersion, "arm64")
	c.AddImage(fake.SeedRegion, arm, "VM.Standard.A1.Flex", "VM.Standard.A2.Flex")
	return c
}

func runOCICanary(t *testing.T, c *fake.Cloud, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	runOCI(context.Background(), cfg, rep, ociReadOnlyClients(oci.Clients{Compute: c, Network: c, Identity: c, Limits: c}, a), a)
	return rep, a
}

func TestOCICanaryPassesOnAHealthyWorld(t *testing.T) {
	rep, a := runOCICanary(t, ociAccount(), ociTestConfig())
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"regions and availability domains " + fake.SeedRegion, "Flatcar stable 4757.2.3 amd64",
		"Flatcar stable 4757.2.3 amd64 shape compatibility", "quota OCPUs and memory", "instance types offered", "read-only guard", "Flatcar arm64"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	// The Arm types are not checked when the lane does not run arm64.
	if strings.Contains(b.String(), "A1.Flex") || strings.Contains(b.String(), "A2.Flex") {
		t.Errorf("an Arm type was checked without E2E_ARM64:\n%s", b.String())
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
}

func TestOCICanaryChecksArm64WhenTheLaneRunsIt(t *testing.T) {
	cfg := ociTestConfig()
	cfg.arm64 = true
	rep, _ := runOCICanary(t, ociAccount(), cfg)
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"Flatcar stable 4757.2.3 arm64", "Flatcar stable 4757.2.3 arm64 shape compatibility", "VM.Standard.A1.Flex.1-2", "standard-a1-core-count"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no %q:\n%s", want, b.String())
		}
	}
	// An arm64 image that is gone fails only its own rows.
	c2 := fake.NewSeeded() // no arm64 image at all
	c2.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E5.Flex", 94, 1049, 1, 64, 0, 0, "standard-e5-core-count", "standard-e5-memory-count"))
	c2.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A1.Flex", 80, 512, 1, 64, 0, 0, "standard-a1-core-count", "standard-a1-memory-count"))
	c2.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A2.Flex", 78, 946, 1, 64, 0, 0, "standard-a2-core-count", "standard-a2-memory-count"))
	rep, _ = runOCICanary(t, c2, cfg)
	f := failed(rep)
	if !slices.Contains(f, "Flatcar stable 4757.2.3 arm64") || slices.Contains(f, "Flatcar stable 4757.2.3 amd64") {
		t.Fatalf("failed: %v", f)
	}
}

func TestOCICanaryReportsWhatMoved(t *testing.T) {
	const e5 = "standard-e5-core-count"
	for name, tc := range map[string]struct {
		mutate func(*fake.Cloud, *config)
		want   string
	}{
		"the Flatcar version is not published": {mutate: func(_ *fake.Cloud, c *config) { c.flatcarVersion = "9999.0.0" }, want: "Flatcar stable 9999.0.0 amd64"},
		"the image lost its shape": {mutate: func(c *fake.Cloud, _ *config) {
			img := fake.FlatcarImage("stable", fake.SeedVersion, "amd64")
			img.Id = new(fake.SeedImage)
			c.AddImage(fake.SeedRegion, img, "VM.Standard2.1")
		}, want: "Flatcar stable 4757.2.3 amd64 shape compatibility"},
		"the image is not available": {mutate: func(c *fake.Cloud, _ *config) {
			c.SetImageState(fake.SeedImage, "DISABLED")
		}, want: "Flatcar stable 4757.2.3 amd64"},
		"the core limit shrank":     {mutate: func(c *fake.Cloud, _ *config) { c.SetLimit(fake.SeedRegion, "", e5, 4) }, want: "quota OCPUs and memory " + fake.SeedRegion},
		"the core limit is gone":    {mutate: func(c *fake.Cloud, _ *config) { c.SetLimit(fake.SeedRegion, "", e5, 0) }, want: "quota OCPUs and memory " + fake.SeedRegion},
		"the memory limit shrank":   {mutate: func(c *fake.Cloud, _ *config) { c.SetLimit(fake.SeedRegion, "", "standard-e5-memory-count", 8) }, want: "quota OCPUs and memory " + fake.SeedRegion},
		"the limit is not readable": {mutate: func(c *fake.Cloud, _ *config) { c.SetUnsupported(e5) }, want: "quota OCPUs and memory " + fake.SeedRegion},
		"a type is not offered": {mutate: func(c *fake.Cloud, _ *config) {
			for _, ad := range []string{fake.SeedZoneA, fake.SeedZoneB, fake.SeedZoneC} {
				c.RemoveShape(fake.SeedRegion, "VM.Standard.E5.Flex", fake.ADFull(ad))
			}
		}, want: "instance types offered in " + fake.SeedRegion},
		"a type is unknown":            {mutate: func(_ *fake.Cloud, c *config) { c.types = append(c.types, "oci.VM.Standard.Nope.Flex.1-4") }, want: "instance types offered in " + fake.SeedRegion},
		"a type does not parse":        {mutate: func(_ *fake.Cloud, c *config) { c.types = append(c.types, "oci.VM.Standard.E5.Flex") }, want: "instance types offered in " + fake.SeedRegion},
		"the region is not subscribed": {mutate: func(_ *fake.Cloud, c *config) { c.region = fake.SeedRegionUnsubscribed }, want: "regions and availability domains " + fake.SeedRegionUnsubscribed},
		"the region does not exist":    {mutate: func(_ *fake.Cloud, c *config) { c.region = "mars-north-1" }, want: "regions and availability domains mars-north-1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := ociAccount()
			cfg := ociTestConfig()
			tc.mutate(c, cfg)
			rep, _ := runOCICanary(t, c, cfg)
			if f := failed(rep); !slices.Contains(f, tc.want) {
				var b bytes.Buffer
				rep.write(&b)
				t.Fatalf("want %q among the failed checks, got %v:\n%s", tc.want, f, b.String())
			}
		})
	}
}

func TestOCICanaryCompartmentMismatchFails(t *testing.T) {
	cfg := ociTestConfig()
	cfg.compartment = "ocid1.compartment.oc1..someothercompartment" // the fake answers 404, like OCI
	rep, _ := runOCICanary(t, ociAccount(), cfg)
	if len(failed(rep)) == 0 {
		t.Fatal("a canary for another compartment passed")
	}
}

func TestOCICanaryImageCompartment(t *testing.T) {
	const imageComp = "ocid1.compartment.oc1..aaaaimagecompartment"
	c := fake.NewSeeded()
	c.AddImageCompartment(imageComp)
	for _, arch := range []string{"amd64", "arm64"} {
		img := fake.FlatcarImage("stable", "4800.0.0", arch)
		img.CompartmentId = &[]string{imageComp}[0]
		c.AddImage(fake.SeedRegion, img, "VM.Standard.E5.Flex", "VM.Standard.A1.Flex", "VM.Standard.A2.Flex")
	}
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E5.Flex", 94, 1049, 1, 64, 0, 0, "standard-e5-core-count", "standard-e5-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A1.Flex", 80, 512, 1, 64, 0, 0, "standard-a1-core-count", "standard-a1-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.A2.Flex", 78, 946, 1, 64, 0, 0, "standard-a2-core-count", "standard-a2-memory-count"))
	cfg := ociTestConfig()
	cfg.flatcarVersion = "4800.0.0" // only in the image compartment (the seed has the pinned one)
	rep, _ := runOCICanary(t, c, cfg)
	if f := failed(rep); len(f) == 0 {
		t.Fatal("images in another compartment were found without the option")
	}
	cfg.imageCompartment = imageComp
	rep, _ = runOCICanary(t, c, cfg)
	if f := failed(rep); len(f) > 0 {
		t.Errorf("failed with the option: %v", f)
	}
}

func TestOCICanaryNeedsAWorkingDriver(t *testing.T) {
	cfg := ociTestConfig()
	cfg.compartment = "not-an-ocid"
	rep, _ := runOCICanary(t, ociAccount(), cfg)
	if f := failed(rep); !slices.Equal(f, []string{"OCI driver"}) {
		t.Errorf("failed: %v", f)
	}
}

func TestLoadConfigOCI(t *testing.T) {
	env := map[string]string{
		"OCI_REGION": "eu-frankfurt-1", "OCI_COMPARTMENT": "ocid1.compartment.oc1..x", "OCI_TENANCY": "ocid1.tenancy.oc1..y",
		"E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "oci.VM.Standard.E5.Flex.1-4", "E2E_TYPE_ARM64": "oci.VM.Standard.A1.Flex.1-2",
		"E2E_TYPE_ARM64_ALT": "oci.VM.Standard.A2.Flex.1-8", "E2E_TYPES_DESCRIBED": "oci.VM.Standard.E5.Flex.1-8, oci.VM.Standard.E5.Flex.2-8",
		"E2E_MIN_OCPU": "8", "E2E_MIN_MEMORY_GB": "48", "E2E_ARM64": "true",
		"OCI_IMAGE_COMPARTMENT": " ocid1.compartment.oc1..img ",
	}
	get := func(k string) string { return env[k] }
	c, err := loadConfig("oci", get)
	if err != nil {
		t.Fatal(err)
	}
	if c.imageCompartment != "ocid1.compartment.oc1..img" || c.compartment != "ocid1.compartment.oc1..x" || c.tenancy != "ocid1.tenancy.oc1..y" || c.region != "eu-frankfurt-1" ||
		c.minOCPU != 8 || c.minMemoryGB != 48 || !c.arm64 || len(c.types) != 5 || c.minOnDemand != 0 {
		t.Errorf("config %+v", c)
	}
	for _, missing := range []string{"OCI_COMPARTMENT", "OCI_TENANCY", "OCI_REGION", "E2E_MIN_OCPU", "E2E_MIN_MEMORY_GB", "E2E_TYPE_ARM64_ALT"} {
		saved := env[missing]
		delete(env, missing)
		if _, err := loadConfig("oci", get); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: %v", missing, err)
		}
		env[missing] = saved
	}
	env["E2E_MIN_OCPU"] = "many"
	if _, err := loadConfig("oci", get); err == nil {
		t.Error("a minimum that is not a number was accepted")
	}
}

func TestOCILaneTypes(t *testing.T) {
	cfg := ociTestConfig()
	if got := ociLaneTypes(cfg); !slices.Equal(got, []string{"VM.Standard.E5.Flex.1-4", "VM.Standard.E5.Flex.1-8", "VM.Standard.E5.Flex.2-8"}) {
		t.Errorf("ociLaneTypes = %v", got)
	}
	cfg.arm64 = true
	if got := ociLaneTypes(cfg); !slices.Contains(got, "VM.Standard.A1.Flex.1-2") || !slices.Contains(got, "VM.Standard.A2.Flex.1-8") {
		t.Errorf("the Arm types are missing when arm64 runs: %v", got)
	}
}

// statements reduces policy statements ("<verb> <resource> in <scope>") to the highest verb per
// resource and scope: read includes inspect, and the canary needs only the highest of each.
func statements(in []string) []string {
	rank := map[string]int{"inspect": 1, "read": 2, "use": 3, "manage": 4}
	best := map[string]string{}
	for _, s := range in {
		verb, rest, _ := strings.Cut(s, " ")
		if cur, ok := best[rest]; !ok || rank[verb] > rank[cur] {
			best[rest] = verb
		}
	}
	out := make([]string, 0, len(best))
	for rest, verb := range best {
		out = append(out, verb+" "+rest)
	}
	slices.Sort(out)
	return out
}

// The policy (test/e2e-oci/canary-policy.sh) is exactly what the canary calls: every statement of
// the run is in the policy (at its highest verb), every statement of the policy is a plain read
// (inspect or read), and the policy holds nothing the canary never uses.
func TestOCIPolicyIsExactlyWhatTheCanaryCalls(t *testing.T) {
	out, err := exec.Command("bash", "../e2e-oci/canary-policy.sh", "statements").Output()
	if err != nil {
		t.Skipf("the policy script cannot run here (bash is needed): %v", err)
	}
	var allowed []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			allowed = append(allowed, line)
		}
	}
	if len(allowed) == 0 {
		t.Fatalf("no statements in the policy:\n%s", out)
	}
	for _, s := range allowed {
		if verb, _, _ := strings.Cut(s, " "); verb != "inspect" && verb != "read" {
			t.Errorf("the policy grants %q, which is not a plain read", s)
		}
	}
	// The run with the arm64 lane covers every call the canary can make.
	cfg := ociTestConfig()
	cfg.arm64 = true
	_, a := runOCICanary(t, ociAccount(), cfg)
	used := statements(a.actions())
	want := statements(allowed)
	if !slices.Equal(used, want) {
		t.Errorf("the canary uses\n  %s\nthe policy grants\n  %s", strings.Join(used, "\n  "), strings.Join(want, "\n  "))
	}
}
