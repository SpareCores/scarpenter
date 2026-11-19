/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

// TestGoldenDirectWithNodeIPCIDR renders the direct template for a node on a
// private network: the unit that picks the node's address inside the range, and
// the kubelet drop-in that orders it first. Without a range (the other golden
// files of the direct template) none of it is rendered.
func TestGoldenDirectWithNodeIPCIDR(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.NodeIPCIDR = "10.0.0.0/16"
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	got := goldenText(t, r)
	testutil.Golden(t, "render-direct-private-amd64.golden", got)
	for _, want := range []string{"scarpenter-node-ip.service", "--cidr 10.0.0.0/16", "Before=kubelet.service", "Requires=scarpenter-node-ip.service"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the rendering lacks %q", want)
		}
	}
	if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
		t.Errorf("the user-data is %d bytes, the budget of the default templates is %d", n, bootstrap.DefaultBudgetBytes)
	} else {
		t.Logf("direct with a node IP range: %d bytes of user-data", n)
	}

	plain, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateDirect))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(goldenText(t, plain)), "scarpenter-node-ip") {
		t.Error("the direct template without a range renders the node IP unit")
	}
}

// TestNodeIPCIDRIsIgnoredByTheTailscaleTemplate: the tailnet address stays the
// node IP, whatever the driver says.
func TestNodeIPCIDRIsIgnoredByTheTailscaleTemplate(t *testing.T) {
	plain, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateTailscale))
	if err != nil {
		t.Fatal(err)
	}
	p := baseParams("amd64", v1alpha1.NetworkTemplateTailscale)
	p.NodeIPCIDR = "10.0.0.0/16"
	with, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(goldenText(t, with)) != string(goldenText(t, plain)) {
		t.Error("the tailscale rendering changes with a node IP range")
	}
}

func TestNodeIPCIDRErrors(t *testing.T) {
	for _, cidr := range []string{"fd00::/64", "10.0.0.1", "nonsense"} {
		p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
		p.NodeIPCIDR = cidr
		if _, err := bootstrap.Render(p); err == nil || !strings.Contains(err.Error(), "not an IPv4 prefix") {
			t.Errorf("%q: %v, want an IPv4 prefix error", cidr, err)
		}
	}
	// A host address inside the range is rendered as the range.
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.NodeIPCIDR = "10.1.2.3/24"
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goldenText(t, r)), "--cidr 10.1.2.0/24") {
		t.Error("the range is not rendered in its canonical form")
	}
}

// nodeIPRun runs the script with a stub of `ip` that lists the addresses in the
// format of `ip -4 -o addr show scope global`.
func nodeIPRun(t *testing.T, addrs []string, args ...string) (env string, out string, err error) {
	t.Helper()
	dir := t.TempDir()
	var lines strings.Builder
	for i, a := range addrs {
		fmt.Fprintf(&lines, "%d: eth%d    inet %s scope global eth%d\n", i+2, i, a, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "addrs"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/bash\ncat \"$STUB/addrs\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(stub), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "run", "node-ip.env")
	cmd := exec.Command("bash", append([]string{filepath.Join("templates", "scarpenter-node-ip.sh")}, args...)...) //nolint:gosec // the script is the repository's
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STUB="+dir, "SCARPENTER_NODE_IP_ENV="+envFile)
	b, err := cmd.CombinedOutput()
	got, _ := os.ReadFile(envFile) //nolint:gosec // temp dir of the test
	return string(got), string(b), err
}

func TestNodeIPScriptPicksTheAddressInsideTheRange(t *testing.T) {
	public, private := "203.0.113.7/32", "10.0.3.9/24"
	for name, tc := range map[string]struct {
		addrs []string
		cidr  string
		want  string
	}{
		"private second":     {[]string{public, private}, "10.0.0.0/16", "10.0.3.9"},
		"private first":      {[]string{private, public}, "10.0.0.0/16", "10.0.3.9"},
		"host bits in range": {[]string{public, private}, "10.0.3.77/24", "10.0.3.9"},
		"first of two":       {[]string{"10.0.3.9/24", "10.0.3.10/24"}, "10.0.0.0/8", "10.0.3.9"},
		"edge of the range":  {[]string{public, "10.0.255.254/16"}, "10.0.0.0/16", "10.0.255.254"},
		"slash 32":           {[]string{public, "192.168.1.5/32"}, "192.168.1.5/32", "192.168.1.5"},
		"everything":         {[]string{public}, "0.0.0.0/0", "203.0.113.7"},
	} {
		env, out, err := nodeIPRun(t, tc.addrs, "--cidr", tc.cidr, "--timeout", "0")
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
			continue
		}
		if want := "SCARPENTER_NODE_IP_ARGS=--node-ip=" + tc.want + "\n"; env != want {
			t.Errorf("%s: env file = %q, want %q", name, env, want)
		}
	}
}

func TestNodeIPScriptFailsWithoutAnAddressInsideTheRange(t *testing.T) {
	env, out, err := nodeIPRun(t, []string{"203.0.113.7/32", "10.1.0.4/24"}, "--cidr", "10.0.0.0/16", "--timeout", "0")
	if err == nil {
		t.Fatalf("the script succeeded without an address in the range: %s", out)
	}
	if env != "" {
		t.Errorf("the env file was written: %q", env)
	}
	if !strings.Contains(out, "no IPv4 address inside 10.0.0.0/16") || !strings.Contains(out, "will not start") {
		t.Errorf("the failure does not say why: %s", out)
	}
	if _, out, err = nodeIPRun(t, nil, "--cidr", "10.0.0.0/16", "--timeout", "0"); err == nil {
		t.Fatalf("the script succeeded without any address: %s", out)
	}
}

func TestNodeIPScriptRejectsBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"no cidr":      {"--timeout", "0"},
		"ipv6":         {"--cidr", "fd00::/64"},
		"prefix > 32":  {"--cidr", "10.0.0.0/33"},
		"unknown flag": {"--bogus", "x"},
	} {
		if _, out, err := nodeIPRun(t, []string{"10.0.0.5/24"}, args...); err == nil {
			t.Errorf("%s: the script succeeded: %s", name, out)
		}
	}
}
