/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

const (
	gpuFlatcar    = "4757.2.3"
	gpuMirror     = "https://mirror.example.com/flatcar"
	gpuRuntimeURL = "https://github.com/flatcar/sysext-bakery/releases/download/nvidia-runtime-v1.17.9/nvidia-runtime-v1.17.9-x86-64.raw"
)

// gpuSelection is the table's choice for a model with the given GPU count on
// the pinned stable release.
func gpuSelection(t *testing.T, model string, count float64) nvidia.Selection {
	t.Helper()
	sel, reason := nvidia.Select(nvidia.GPU{Manufacturer: "NVIDIA", Model: model, Count: count, AMD64: true}, gpuFlatcar)
	if reason != nvidia.Supported {
		t.Fatalf("%s is dropped: %s", model, reason)
	}
	return sel
}

func gpuParams(t *testing.T, template v1alpha1.NetworkTemplateName, sel nvidia.Selection, mirror string) bootstrap.Params {
	t.Helper()
	p := baseParams("amd64", template)
	p.NVIDIA = &bootstrap.NVIDIAParams{
		Selection: sel, FlatcarVersion: gpuFlatcar, RuntimeURL: gpuRuntimeURL, DriverBaseURL: mirror,
	}
	return p
}

func part(r *bootstrap.Rendered, name string) (string, bool) {
	for _, p := range r.Parts {
		if p.Name == name {
			return string(p.Ignition), true
		}
	}
	return "", false
}

// The golden files: a T4 (open modules) with the driver downloaded by the
// initrd, a V100 (proprietary) from a mirror on the tailscale template, and a
// 4-GPU Lean node from a mirror. Together with the matrix test below they cover
// both flavours, both sources, both templates, with and without Lean.
func TestGoldenNVIDIA(t *testing.T) {
	lean := gpuParams(t, v1alpha1.NetworkTemplateDirect, gpuSelection(t, "A100", 4), gpuMirror)
	lean.Kubelet.Footprint = v1alpha1.FootprintLean
	for name, p := range map[string]bootstrap.Params{
		"nvidia-direct-open-upstream":          gpuParams(t, v1alpha1.NetworkTemplateDirect, gpuSelection(t, "T4", 1), ""),
		"nvidia-tailscale-proprietary-mirror":  gpuParams(t, v1alpha1.NetworkTemplateTailscale, gpuSelection(t, "V100", 1), gpuMirror),
		"nvidia-lean-direct-open-mirror-4gpus": lean,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			testutil.Golden(t, "render-"+name+".golden", goldenText(t, r))
		})
	}
}

// Every combination renders, puts the nvidia part after the base (and volumes) and
// before the network config, and says what the selection says.
func TestNVIDIAPartAcrossVariants(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateDirect, v1alpha1.NetworkTemplateTailscale} {
		for _, model := range []string{"T4", "V100"} {
			for _, mirror := range []string{"", gpuMirror + "/"} {
				for _, lean := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/mirror=%t/lean=%t", template, model, mirror != "", lean)
					t.Run(name, func(t *testing.T) {
						sel := gpuSelection(t, model, 2)
						p := gpuParams(t, template, sel, mirror)
						if lean {
							p.Kubelet.Footprint = v1alpha1.FootprintLean
						}
						r, err := bootstrap.Render(p)
						if err != nil {
							t.Fatal(err)
						}
						order := make([]string, 0, len(r.Parts))
						for _, pt := range r.Parts {
							order = append(order, pt.Name)
						}
						wantOrder := "base,nvidia,network"
						if lean {
							wantOrder = "base,lean,nvidia,network"
						}
						if got := strings.Join(order, ","); got != wantOrder {
							t.Errorf("parts = %s, want %s", got, wantOrder)
						}
						text, _ := part(r, "nvidia")
						for _, want := range []string{
							sel.Sysext + "%0A", // enabled-sysext.conf
							"sha256-" + sel.RuntimeSHA256,
							"scarpenter-nvidia-check " + sel.Version + " 2",
							"Upholds=nvidia-persistenced.service",
							// Before the driver sysext's 10-flatcar-nvidia-drivers-*.conf:
							// modprobe uses the first install line of a module.
							"/etc/modprobe.d/00-scarpenter-nvidia.conf",
						} {
							if !strings.Contains(text, want) {
								t.Errorf("the nvidia part lacks %q", want)
							}
						}
						driverFile := "/etc/flatcar/sysext/flatcar-" + sel.Sysext + "-" + gpuFlatcar + ".raw"
						if mirror == "" {
							if strings.Contains(text, driverFile) || strings.Contains(text, "gzip") {
								t.Error("the driver file is written without a mirror")
							}
						} else {
							for _, want := range []string{
								driverFile, `"compression": "gzip"`, "sha256-" + sel.RawSHA256,
								gpuMirror + "/amd64-usr/" + gpuFlatcar + "/flatcar-" + sel.Sysext + ".raw.gz",
							} {
								if !strings.Contains(strings.ReplaceAll(text, " ", ""), strings.ReplaceAll(want, " ", "")) {
									t.Errorf("the mirror variant lacks %q", want)
								}
							}
						}
					})
				}
			}
		}
	}
}

// Without a driver selection nothing of the nvidia part is rendered: the output is
// the one of a node without it.
func TestNoNVIDIAPartWithoutSelection(t *testing.T) {
	r, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateDirect))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := part(r, "nvidia"); ok {
		t.Error("a node without a GPU has an nvidia part")
	}
	if strings.Contains(string(goldenText(t, r)), "nvidia") {
		t.Error("a node without a GPU mentions nvidia")
	}
}

func TestNVIDIARejectsWhatItCannotRender(t *testing.T) {
	good := gpuSelection(t, "T4", 1)
	tests := map[string]func(p *bootstrap.Params){
		"arm64": func(p *bootstrap.Params) {
			p.Arch, p.Kubernetes = "arm64", k8sArtifact("arm64")
		},
		"no pin":     func(p *bootstrap.Params) { p.NVIDIA.Selection.RawSHA256 = "" },
		"no runtime": func(p *bootstrap.Params) { p.NVIDIA.Selection.RuntimeSHA256 = "" },
		"no count":   func(p *bootstrap.Params) { p.NVIDIA.Selection.Count = 0 },
		"nvswitch":   func(p *bootstrap.Params) { p.NVIDIA.Selection.NVSwitch = true },
		"no url":     func(p *bootstrap.Params) { p.NVIDIA.RuntimeURL = "" },
		"bad name":   func(p *bootstrap.Params) { p.NVIDIA.Selection.Sysext = "nvidia drivers" },
		"collision": func(p *bootstrap.Params) {
			p.ExtraSysexts = []v1alpha1.Sysext{{Name: "nvidia-runtime", Arch: "amd64", URL: "https://example.com/x.raw", SHA256: strings.Repeat("a", 64)}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := gpuParams(t, v1alpha1.NetworkTemplateDirect, good, "")
			mutate(&p)
			if _, err := bootstrap.Render(p); err == nil {
				t.Error("Render succeeded")
			}
		})
	}
}

// The worst case of a GPU node (every other feature on, 16 GPUs, the mirror, which
// is the largest variant) stays within the budget of GPU nodes, so that a user's
// Butane has room under the limit of every vendor.
func TestWorstCaseGPUUserDataStaysInTheBudget(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateDirect, v1alpha1.NetworkTemplateTailscale} {
		t.Run(string(template), func(t *testing.T) {
			p := worstCaseParams(t, template)
			sel := gpuSelection(t, "T4", 16)
			p.NVIDIA = &bootstrap.NVIDIAParams{
				Selection: sel, FlatcarVersion: gpuFlatcar, RuntimeURL: gpuRuntimeURL, DriverBaseURL: gpuMirror,
			}
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(r.UserData); n > bootstrap.GPUBudgetBytes {
				t.Errorf("the GPU worst case is %d bytes, the budget is %d", n, bootstrap.GPUBudgetBytes)
			} else {
				t.Logf("%s GPU worst case: %d bytes of user-data", template, n)
			}
			for _, pt := range r.Parts {
				if strings.Contains(string(pt.Ignition), "data:;base64,") {
					t.Errorf("the %s part holds base64 text, which compresses badly", pt.Name)
				}
			}
		})
	}
}

func gpuRenderer(t *testing.T, opts bootstrap.RendererOptions, ca string) *bootstrap.Renderer {
	t.Helper()
	cm := rootCA()
	cm.Data["ca.crt"] = ca
	opts.Refs, opts.APIServerEndpoint = kubeRefs(t, cm), "https://api.example.com:6443"
	return bootstrap.NewRenderer(opts)
}

// The renderer takes the runtime sysext from the sysext mirror and the driver from the
// driver mirror, and reads the Flatcar release from the NodeClass.
func TestRendererNVIDIAMirrors(t *testing.T) {
	sel := gpuSelection(t, "L4", 1)
	r := gpuRenderer(t, bootstrap.RendererOptions{
		SysextBaseURL: "https://sysext.example.com/bakery/", NVIDIADriverBaseURL: "https://drivers.example.com",
	}, testCA)
	userData, err := r.Render(t.Context(), bootstrap.RenderRequest{
		NodeClass: nodeClass(nil), NodeClaim: newClaim(), Vendor: "aws", Region: "us-east-1", Arch: "amd64", NVIDIA: &sel,
		Token: testToken, NodeName: "default-abc12", ProviderID: "scarpenter://aws/us-east-1/default-abc12",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, parts := decode(t, userData)
	if len(parts) != 3 {
		t.Fatalf("%d parts, want base, nvidia and network", len(parts))
	}
	text := readable(t, parts[1])
	for _, want := range []string{
		"https://sysext.example.com/bakery/nvidia-runtime-v1.17.9/nvidia-runtime-v1.17.9-x86-64.raw",
		"https://drivers.example.com/amd64-usr/4757.2.3/flatcar-nvidia-drivers-570-open.raw.gz",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the nvidia part lacks %q", want)
		}
	}
}

// Validate sizes its trial rendering with the largest NVIDIA variant of the NodeClass's
// release: a NodeClass that only fits without the driver is rejected there, and not when
// a GPU node launches. A release without a driver table is not affected.
func TestValidateCountsTheNVIDIAPart(t *testing.T) {
	var fits []string
	for n := 4096; n < 16*1024; n += 64 {
		plain := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
		plain.ClusterCA = fakeCA(n)
		plain.NodeIPCIDR = "255.255.255.255/32"
		gpu := plain
		gpu.NVIDIA = &bootstrap.NVIDIAParams{
			Selection: gpuSelection(t, "T4", 16), FlatcarVersion: gpuFlatcar, RuntimeURL: gpuRuntimeURL,
		}
		_, errPlain := bootstrap.Render(plain)
		_, errGPU := bootstrap.Render(gpu)
		if errPlain == nil && errGPU != nil {
			fits = append(fits, plain.ClusterCA)
		}
	}
	if len(fits) == 0 {
		t.Fatal("no CA size separates the direct template with and without the nvidia part")
	}
	// Close to the edge: the trial rendering has a few more bytes than this one.
	ca := fits[len(fits)/2]
	r := gpuRenderer(t, bootstrap.RendererOptions{}, ca)
	err := r.Validate(t.Context(), nodeClass(nil), "amd64", bootstrap.Inputs{})
	if err == nil || !strings.Contains(err.Error(), "over the budget") || !strings.Contains(err.Error(), "NVIDIA driver") {
		t.Fatalf("Validate = %v, want the budget error that names the NVIDIA driver", err)
	}
	// arm64 has no driver sysexts, a release the table does not know has none either.
	if err := r.Validate(t.Context(), nodeClass(nil), "arm64", bootstrap.Inputs{}); err != nil {
		t.Errorf("arm64: %v", err)
	}
	unknown := nodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Flatcar.Version = "4000.0.0" })
	if err := r.Validate(t.Context(), unknown, "amd64", bootstrap.Inputs{}); err != nil {
		t.Errorf("a release without a driver table: %v", err)
	}
}
