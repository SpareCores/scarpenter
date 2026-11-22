/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

// testCA is a fixed, obviously fake CA of the size of an RSA-2048 CA (about
// 1.1 KiB of PEM, as kubeadm and EKS clusters have), made of hashes so that it
// compresses as badly as a real one. The size budget is measured with it.
var testCA = fakeCA(780)

func fakeCA(derBytes int) string {
	sum := sha256.Sum256([]byte("scarpenter fake CA"))
	raw := make([]byte, 0, derBytes+32)
	for len(raw) < derBytes {
		raw = append(raw, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	enc := base64.StdEncoding.EncodeToString(raw[:derBytes])
	var b strings.Builder
	b.WriteString("-----BEGIN CERTIFICATE-----\n")
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n-----END CERTIFICATE-----")
	return b.String()
}

const (
	testToken   = "abcdef.0123456789abcdef"
	testAuthKey = "tskey-auth-EXAMPLEEXAMPLE-0123456789abcdefEXAMPLE"
)

func k8sArtifact(arch string) artifacts.Artifact {
	if arch == "arm64" {
		return artifacts.Artifact{
			URL:    "https://github.com/flatcar/sysext-bakery/releases/download/kubernetes-v1.37.1/kubernetes-v1.37.1-arm64.raw",
			SHA256: "2e2a6faf04230c6b719656dbea20b76f1fbf9a5b3e61c0787c0850f052e40f6b",
		}
	}
	return artifacts.Artifact{
		URL:    "https://github.com/flatcar/sysext-bakery/releases/download/kubernetes-v1.37.1/kubernetes-v1.37.1-x86-64.raw",
		SHA256: "75072a32a1d87cfbe67907a0f6ebe3f0a860c2b2e15dbf1d8a354d5162e63b3b",
	}
}

func tsArtifact(arch string) artifacts.Artifact {
	if arch == "arm64" {
		return artifacts.Artifact{
			URL:    "https://github.com/flatcar/sysext-bakery/releases/download/tailscale-v1.98.10/tailscale-v1.98.10-arm64.raw",
			SHA256: "580eade1a34f60161427f17d11c14581164c02133811670a5f554f4ff05d7ce6",
		}
	}
	return artifacts.Artifact{
		URL:    "https://github.com/flatcar/sysext-bakery/releases/download/tailscale-v1.98.10/tailscale-v1.98.10-x86-64.raw",
		SHA256: "d28c68ae6ed525bbeed0e95ef68b9f660b3e79eb0ca834ebf481ac86f7a886d4",
	}
}

func baseParams(arch string, template v1alpha1.NetworkTemplateName) bootstrap.Params {
	p := bootstrap.Params{
		NodeName: "default-abc12", ProviderID: "scarpenter://aws/us-east-1/default-abc12",
		Vendor: "aws", Region: "us-east-1", Arch: arch,
		Labels: map[string]string{"karpenter.sh/nodepool": "default"},
		Taints: []corev1.Taint{
			karpv1.UnregisteredNoExecuteTaint,
			{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
		},
		APIServerEndpoint: "https://api.my-cluster.example.com:6443", ClusterCA: testCA, BootstrapToken: testToken,
		Kubelet:    v1alpha1.KubeletSpec{MaxPods: new(int32(110))},
		Kubernetes: k8sArtifact(arch),
		Network:    bootstrap.Network{Template: template},
	}
	if template == v1alpha1.NetworkTemplateTailscale {
		p.Network.Tailscale = bootstrap.TailscaleParams{
			Version: "v1.98.10", AuthKey: testAuthKey, Tags: []string{"tag:k8s-node"}, Sysext: tsArtifact(arch),
		}
	}
	return p
}

// decode reads the user-data back: the outer config and the gzip data URLs of
// the parts, in merge order.
func decode(t *testing.T, userData []byte) (outer map[string]any, parts [][]byte) {
	t.Helper()
	var cfg struct {
		Ignition struct {
			Version string
			Config  struct {
				Merge []struct{ Compression, Source string }
			}
		}
	}
	if err := json.Unmarshal(userData, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Ignition.Version != "3.4.0" {
		t.Errorf("outer Ignition version = %s, want 3.4.0", cfg.Ignition.Version)
	}
	for _, m := range cfg.Ignition.Config.Merge {
		if m.Compression != "gzip" || !strings.HasPrefix(m.Source, "data:;base64,") {
			t.Fatalf("merge entry %+v is not a gzip data URL", m)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(m.Source, "data:;base64,"))
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		part, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, part)
	}
	return nil, parts
}

// readable indents a translated config and replaces the data URLs of file
// contents by their text, so that the golden files can be reviewed like code.
func readable(t *testing.T, b []byte) string {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	storage, _ := cfg["storage"].(map[string]any) // a config of units only has none
	files, _ := storage["files"].([]any)
	for _, f := range files {
		contents, ok := f.(map[string]any)["contents"].(map[string]any)
		if !ok {
			continue
		}
		src, _ := contents["source"].(string)
		rest, ok := strings.CutPrefix(src, "data:")
		if !ok {
			continue
		}
		meta, payload, _ := strings.Cut(rest, ",")
		var text string
		if strings.HasSuffix(meta, ";base64") {
			raw, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				t.Fatal(err)
			}
			text = string(raw)
		} else {
			var err error
			if text, err = url.PathUnescape(payload); err != nil {
				t.Fatal(err)
			}
		}
		delete(contents, "source")
		delete(contents, "compression")
		contents["inline"] = text
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// goldenText is the readable form of a rendering: each merged config, decoded
// and indented. The compressed bytes are not golden (they depend on the Go
// version's deflate), the sizes are asserted separately.
func goldenText(t *testing.T, r *bootstrap.Rendered) []byte {
	t.Helper()
	_, decoded := decode(t, r.UserData)
	if len(decoded) != len(r.Parts) {
		t.Fatalf("%d merge entries for %d parts", len(decoded), len(r.Parts))
	}
	var b strings.Builder
	for i, p := range r.Parts {
		if !bytes.Equal(decoded[i], p.Ignition) {
			t.Errorf("part %s does not round-trip through the user-data", p.Name)
		}
		fmt.Fprintf(&b, "=== %s ===\n%s\n", p.Name, readable(t, p.Ignition))
	}
	return []byte(b.String())
}

// TestGoldenDefaultTemplates renders every default network template for every
// architecture, compares them with the golden files and holds them to the size
// budget of overview section 8.2.
func TestGoldenDefaultTemplates(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateTailscale, v1alpha1.NetworkTemplateDirect} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(string(template)+"-"+arch, func(t *testing.T) {
				r, err := bootstrap.Render(baseParams(arch, template))
				if err != nil {
					t.Fatal(err)
				}
				testutil.Golden(t, fmt.Sprintf("render-%s-%s.golden", template, arch), goldenText(t, r))
				if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
					t.Errorf("the user-data is %d bytes, the budget of the default templates is %d", n, bootstrap.DefaultBudgetBytes)
				} else {
					t.Logf("%s %s: %d bytes of user-data", template, arch, n)
				}
			})
		}
	}
}

func TestGoldenVariants(t *testing.T) {
	custom := func() bootstrap.Params {
		p := baseParams("amd64", v1alpha1.NetworkTemplateCustom)
		p.Network.CustomSource = "ConfigMap example/net key net.bu"
		p.Network.Custom = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /etc/scarpenter/custom-network
      mode: 0644
      contents:
        inline: |
          node {{ .NodeName }} in {{ .Region }} on {{ .Vendor }}, pool {{ index .Labels "karpenter.sh/nodepool" }}
`
		return p
	}
	withUser := func() bootstrap.Params {
		p := baseParams("arm64", v1alpha1.NetworkTemplateDirect)
		p.UserSource = "ConfigMap example/extras key config.bu"
		p.UserButane = `variant: flatcar
version: 1.0.0
storage:
  files:
    - path: /etc/motd
      mode: 0644
      overwrite: true
      contents:
        inline: "{{ .NodeName }}\n"
`
		return p
	}
	full := func() bootstrap.Params {
		p := baseParams("amd64", v1alpha1.NetworkTemplateTailscale)
		p.PoweroffAfter = 24 * time.Hour
		p.Network.Tailscale.LoginServer = "https://headscale.example.com"
		p.Network.Tailscale.Tags = []string{"tag:k8s-node", "tag:ci"}
		p.Kubelet = v1alpha1.KubeletSpec{
			MaxPods:        new(int32(58)),
			SystemReserved: map[string]string{"cpu": "100m", "memory": "100Mi"},
			KubeReserved:   map[string]string{"cpu": "200m"},
			EvictionHard:   map[string]string{"memory.available": "200Mi"},
		}
		p.ClusterDNS = []string{"10.96.0.10"}
		p.ExtraSysexts = []v1alpha1.Sysext{
			{Name: "zfs", Arch: "amd64", URL: "https://example.com/zfs-v2.raw", SHA256: strings.Repeat("ab", 32)},
			{Name: "zfs", Arch: "arm64", URL: "https://example.com/zfs-v2-arm.raw", SHA256: strings.Repeat("cd", 32)},
		}
		return p
	}
	for name, p := range map[string]bootstrap.Params{"custom": custom(), "user": withUser(), "full": full()} {
		t.Run(name, func(t *testing.T) {
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			testutil.Golden(t, "render-"+name+".golden", goldenText(t, r))
		})
	}
}

// TestGoldenTailscaleAcceptRoutes renders the tailscale template with
// acceptRoutes set; the flag is part of the unit's ExecStart, so it runs on
// every boot.
func TestGoldenTailscaleAcceptRoutes(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			p := baseParams(arch, v1alpha1.NetworkTemplateTailscale)
			p.Network.Tailscale.AcceptRoutes = true
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			got := goldenText(t, r)
			if !strings.Contains(string(got), "--accept-dns=false --accept-routes") {
				t.Error("tailscale up does not carry --accept-routes")
			}
			testutil.Golden(t, "render-tailscale-accept-routes-"+arch+".golden", got)
			def, err := bootstrap.Render(baseParams(arch, v1alpha1.NetworkTemplateTailscale))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(goldenText(t, def)), "--accept-routes") {
				t.Error("the default rendering carries --accept-routes")
			}
		})
	}
}

// TestGoldenLean renders the Lean footprint: the lean part with the daemons'
// limits and the masks, the kubelet's preset settings and, with the tailscale
// template only, the limit of tailscaled.
func TestGoldenLean(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateTailscale, v1alpha1.NetworkTemplateDirect} {
		t.Run(string(template), func(t *testing.T) {
			p := baseParams("amd64", template)
			p.Kubelet = v1alpha1.KubeletSpec{Footprint: v1alpha1.FootprintLean}
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			got := string(goldenText(t, r))
			testutil.Golden(t, "render-lean-"+string(template)+".golden", []byte(got))
			if hasTailscaled := strings.Contains(got, `"name": "tailscaled.service"`); hasTailscaled != (template == v1alpha1.NetworkTemplateTailscale) {
				t.Errorf("a tailscaled drop-in with the %s template: %v", template, hasTailscaled)
			}
			if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
				t.Errorf("the user-data is %d bytes, the budget of the default templates is %d", n, bootstrap.DefaultBudgetBytes)
			}
		})
	}
	standard, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateTailscale))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range standard.Parts {
		if part.Name == "lean" {
			t.Error("the standard footprint renders the lean part")
		}
	}
	for _, absent := range []string{"GOMEMLIMIT", "journald", "serializeImagePulls"} {
		if strings.Contains(string(goldenText(t, standard)), absent) {
			t.Errorf("the standard footprint carries %q", absent)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateTailscale))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateTailscale))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.UserData, b.UserData) {
		t.Error("two renderings of the same input differ")
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(p *bootstrap.Params)
		wantErr string
	}{
		{"missing token", func(p *bootstrap.Params) { p.BootstrapToken = "" }, "bootstrap token is empty"},
		{"missing CA", func(p *bootstrap.Params) { p.ClusterCA = " " }, "cluster CA is empty"},
		{"bad arch", func(p *bootstrap.Params) { p.Arch = "riscv" }, "unsupported architecture"},
		{"unresolved kubernetes", func(p *bootstrap.Params) { p.Kubernetes = artifacts.Artifact{} }, "kubernetes sysext is not resolved"},
		{"tailscale key with whitespace", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: bootstrap.TailscaleParams{
				AuthKey: "tskey a", Sysext: tsArtifact("amd64")}}
		}, "auth key"},
		{"tailscale sysext unresolved", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: bootstrap.TailscaleParams{AuthKey: "tskey-x"}}
		}, "tailscale sysext is not resolved"},
		{"unknown template", func(p *bootstrap.Params) { p.Network.Template = "wireguard" }, "unknown network template"},
		{"node name with a space", func(p *bootstrap.Params) { p.NodeName = "a b" }, "cannot go on a kubelet command line"},
		{"extra sysext collides", func(p *bootstrap.Params) {
			p.ExtraSysexts = []v1alpha1.Sysext{{Name: "kubernetes", Arch: "amd64", URL: "https://example.com/k.raw", SHA256: strings.Repeat("a", 64)}}
		}, "collides"},
		{"bad kubelet quantity", func(p *bootstrap.Params) {
			p.Kubelet.SystemReserved = map[string]string{"cpu": "lots"}
		}, "systemReserved"},
		{"custom template with an unknown variable", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateCustom, CustomSource: "ConfigMap a/b key c",
				Custom: "variant: flatcar\nversion: 1.1.0\n# {{ .NoSuchVariable }}\n"}
		}, "NoSuchVariable"},
		{"custom template with a syntax error", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateCustom, CustomSource: "ConfigMap a/b key c", Custom: "{{ .NodeName "}
		}, "parsing the network (ConfigMap a/b key c) template"},
		{"custom template with the wrong variant", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateCustom, CustomSource: "ConfigMap a/b key c",
				Custom: "variant: fcos\nversion: 1.5.0\n"}
		}, "must be 'variant: flatcar'"},
		{"custom template with the experimental version", func(p *bootstrap.Params) {
			p.Network = bootstrap.Network{Template: v1alpha1.NetworkTemplateCustom, CustomSource: "ConfigMap a/b key c",
				Custom: "variant: flatcar\nversion: 1.2.0-experimental\n"}
		}, "must be 'variant: flatcar'"},
		{"a relative path is an error", func(p *bootstrap.Params) {
			p.UserSource = "ConfigMap a/b key c"
			p.UserButane = "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: relative/path\n"
		}, "translating the butane"},
		{"user butane with unknown field", func(p *bootstrap.Params) {
			p.UserSource = "ConfigMap a/b key c"
			p.UserButane = "variant: flatcar\nversion: 1.1.0\nbogus: true\n"
		}, "translating the butane"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
			tc.mutate(&p)
			_, err := bootstrap.Render(p)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestStrictModeRejectsWarnings: a setuid mode only warns in Butane (spec
// versions before 3.6.0 ignore the bit), and strict mode turns the warning into
// an error. Without strict mode the config would silently lose its setuid bit.
func TestStrictModeRejectsWarnings(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.UserSource = "ConfigMap a/b key c"
	p.UserButane = "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/x\n      mode: 04755\n      contents:\n        inline: x\n"
	_, err := bootstrap.Render(p)
	if err == nil || !strings.Contains(err.Error(), "strict mode") || !strings.Contains(err.Error(), "setuid") {
		t.Fatalf("error = %v, want a strict mode error naming the setuid warning", err)
	}
}

// TestSizeBudget: a custom snippet that does not fit yields a clear error, not
// a launch that the vendor rejects.
func TestSizeBudget(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.UserSource = "ConfigMap example/extras key config.bu"
	// Random data does not compress: 20 KiB of it cannot fit into 16 KiB.
	raw := make([]byte, 10*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	snippet := hex.EncodeToString(raw) // 20 KiB
	if len(snippet) != 20*1024 {
		t.Fatalf("snippet is %d bytes", len(snippet))
	}
	p.UserButane = "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/big\n      mode: 0644\n      contents:\n        inline: " + snippet + "\n"
	_, err := bootstrap.Render(p)
	if err == nil {
		t.Fatal("a 20 KiB snippet rendered")
	}
	for _, want := range []string{"over the budget of 16384 bytes", "user", "sysexts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	t.Log(err)

	// A snippet that compresses well fits: the budget is about the encoded size.
	p.UserButane = "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/big\n      mode: 0644\n      contents:\n        inline: " + strings.Repeat("a", 20*1024) + "\n"
	if _, err := bootstrap.Render(p); err != nil {
		t.Errorf("a compressible 20 KiB snippet should fit: %v", err)
	}
}

func TestKubeletConfiguration(t *testing.T) {
	got, err := bootstrap.KubeletConfiguration(v1alpha1.KubeletSpec{}, 0, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"serverTLSBootstrap: true", "cgroupDriver: systemd", "maxPods: 110", "clusterDomain: cluster.local",
		"clientCAFile: /etc/kubernetes/pki/ca.crt", "mode: Webhook", "resolvConf: /run/systemd/resolve/resolv.conf",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the default configuration lacks %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"clusterDNS", "systemReserved", "kubeReserved", "evictionHard"} {
		if strings.Contains(got, absent) {
			t.Errorf("the default configuration has %q:\n%s", absent, got)
		}
	}
}

func TestKubeletConfigurationLean(t *testing.T) {
	got, err := bootstrap.KubeletConfiguration(v1alpha1.KubeletSpec{Footprint: v1alpha1.FootprintLean}, 0, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"maxPods: 10", "memory: 192Mi", "memory: 128Mi", "memory.available: 50Mi", "nodefs.available: 10%", "imagefs.inodesFree: 5%",
		"serializeImagePulls: true", "containerLogMaxSize: 5Mi", "containerLogMaxFiles: 2", "nodeStatusMaxImages: 10",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the lean configuration lacks %q:\n%s", want, got)
		}
	}

	// What the NodeClass sets overrides the preset, key by key.
	got, err = bootstrap.KubeletConfiguration(v1alpha1.KubeletSpec{
		Footprint: v1alpha1.FootprintLean, MaxPods: new(int32(30)),
		KubeReserved:   map[string]string{"memory": "256Mi", "cpu": "100m"},
		SystemReserved: map[string]string{"memory": "64Mi"},
		EvictionHard:   map[string]string{"memory.available": "80Mi"},
	}, 0, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maxPods: 30", "memory: 256Mi", "cpu: 100m", "memory: 64Mi", "memory.available: 80Mi", "nodefs.available: 10%"} {
		if !strings.Contains(got, want) {
			t.Errorf("the overridden lean configuration lacks %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"192Mi", "128Mi", "maxPods: 10\n", "50Mi"} { // the 1-2 GiB values
		if strings.Contains(got, absent) {
			t.Errorf("the overridden lean configuration has %q:\n%s", absent, got)
		}
	}
}

// The custom template of the how-to guide must render: the guide's example is
// extracted and run through the real pipeline.
func TestHowToCustomTemplateRenders(t *testing.T) {
	doc, err := os.ReadFile("../../docs/how-to-guides/choose-a-network-template.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "```yaml\nvariant: flatcar")
	if !ok {
		t.Fatal("the how-to guide has no custom template example")
	}
	example, _, _ := strings.Cut(rest, "```")
	p := baseParams("amd64", v1alpha1.NetworkTemplateCustom)
	p.Network.CustomSource = "the how-to guide"
	p.Network.Custom = "variant: flatcar" + example
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Parts[1].Ignition), "default-abc12") {
		t.Errorf("the example's variables were not substituted: %s", r.Parts[1].Ignition)
	}
}

// TestKubeletClientCA: with a separate CA for the API server's kubelet client
// certificates (k3s) it is written next to the cluster CA and the kubelet
// trusts it for client authentication; without one the kubelet trusts the
// cluster CA, as before.
func TestKubeletClientCA(t *testing.T) {
	clientCA := fakeCA(700)
	with := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	with.KubeletClientCA = clientCA + "\n"
	got, err := bootstrap.Render(with)
	if err != nil {
		t.Fatal(err)
	}
	text := string(goldenText(t, got))
	if !strings.Contains(text, "/etc/kubernetes/pki/kubelet-client-ca.crt") || !strings.Contains(text, "clientCAFile: /etc/kubernetes/pki/kubelet-client-ca.crt") {
		t.Errorf("the client CA is not written and used:\n%s", text)
	}
	if !strings.Contains(text, "certificate-authority: /etc/kubernetes/pki/ca.crt") {
		t.Error("the bootstrap kubeconfig must keep trusting the cluster CA for the API server")
	}
	if !strings.Contains(text, strings.Split(clientCA, "\n")[1]) {
		t.Error("the client CA's PEM is not in the user-data")
	}
	plain, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateDirect))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(goldenText(t, plain)); strings.Contains(s, "kubelet-client-ca") || !strings.Contains(s, "clientCAFile: /etc/kubernetes/pki/ca.crt") {
		t.Errorf("without a client CA the kubelet must trust the cluster CA:\n%s", s)
	}
}

// TestKubeletConfigurationLeanBySize pins the Lean memory values of the
// kubelet configuration at every size, and the allocatable they leave (the
// node's memory less kubeReserved, systemReserved and the threshold). The
// catalog's table in pkg/catalog/overhead_test.go has the same rows, by the
// catalog's estimate of the node's memory.
func TestKubeletConfigurationLeanBySize(t *testing.T) {
	for _, tc := range []struct {
		nodeMiB                          int64
		kube, system, eviction, allocMiB int64
	}{
		{448, 144, 96, 16, 192},
		{947, 192, 128, 50, 577},
		{1894, 192, 128, 50, 1524},
		{3788, 192, 236, 50, 3310},
		{60620, 192, 384, 50, 59994},
	} {
		t.Run(fmt.Sprintf("%dMi", tc.nodeMiB), func(t *testing.T) {
			got, err := bootstrap.KubeletConfiguration(
				v1alpha1.KubeletSpec{Footprint: v1alpha1.FootprintLean}, tc.nodeMiB, nil, "", "")
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				KubeReserved   map[string]string `json:"kubeReserved"`
				SystemReserved map[string]string `json:"systemReserved"`
				EvictionHard   map[string]string `json:"evictionHard"`
			}
			if err := yaml.Unmarshal([]byte(got), &cfg); err != nil {
				t.Fatal(err)
			}
			want := func(mib int64) string { return fmt.Sprintf("%dMi", mib) }
			if cfg.KubeReserved["memory"] != want(tc.kube) || cfg.SystemReserved["memory"] != want(tc.system) ||
				cfg.EvictionHard["memory.available"] != want(tc.eviction) {
				t.Errorf("kube %s, system %s, eviction %s; want %d, %d, %d",
					cfg.KubeReserved["memory"], cfg.SystemReserved["memory"], cfg.EvictionHard["memory.available"],
					tc.kube, tc.system, tc.eviction)
			}
			if alloc := tc.nodeMiB - tc.kube - tc.system - tc.eviction; alloc != tc.allocMiB {
				t.Errorf("allocatable %dMi, want %dMi", alloc, tc.allocMiB)
			} else if tc.nodeMiB == 448 && (alloc < 150 || alloc > 200) {
				t.Errorf("a 0.5 GiB node leaves %dMi, want 150-200Mi", alloc)
			}
		})
	}
}
