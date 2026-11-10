/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// releases serves what the canary reads over HTTP: the SHA256SUMS of the sysext releases
// and the checksum file of the k3s release. A path in missing answers 404.
func releases(t *testing.T, missing ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range missing {
			if strings.Contains(r.URL.Path, m) {
				http.NotFound(w, r)
				return
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			rel := strings.Split(r.URL.Path, "/")[1]
			_, _ = fmt.Fprintf(w, "%s  %s-x86-64.raw\n%s  %s-arm64.raw\n", sum, rel, sum, rel)
		case strings.HasSuffix(r.URL.Path, "/sha256sum-amd64.txt"):
			_, _ = fmt.Fprintf(w, "%s  k3s\n%s  k3s-airgap-images-amd64.tar\n", sum, sum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(srv *httptest.Server) *config {
	return &config{
		vendor: "aws", region: "us-east-2", flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types:       []string{"aws.t3a.small", "aws.t4g.small", "aws.r6a.large"},
		minOnDemand: 16, minSpot: 4, sysextBase: srv.URL, k3sBase: srv.URL,
	}
}

// account is a fake account that satisfies the lane: the region, its types, images and quota.
func account() *fake.Account {
	acct := fake.New(fake.WithRegions(fake.DefaultRegion("us-east-2", "use2"), fake.DefaultRegion("eu-central-1", "euc1")))
	acct.SetInstanceTypes([]fake.InstanceType{
		{Name: "t3a.small", VCPUs: 2, Burstable: true},
		{Name: "t4g.small", VCPUs: 2, Arch: "arm64", Burstable: true},
		{Name: "r6a.large", VCPUs: 2},
	})
	for _, r := range acct.Regions() {
		acct.SetQuota(r, standardOnDemand, 64)
		acct.SetQuota(r, standardSpot, 32)
		acct.AddImage(r, fake.Image{ID: "ami-amd64", Name: "Flatcar-stable-4757.2.3-hvm", Owner: awsdriver.DefaultImageOwner, Arch: "x86_64"})
		acct.AddImage(r, fake.Image{ID: "ami-arm64", Name: "Flatcar-stable-4757.2.3-arm64-hvm", Owner: awsdriver.DefaultImageOwner, Arch: "arm64"})
	}
	return acct
}

func runCanary(t *testing.T, acct *fake.Account, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	factory := func(_ context.Context, region string) (awsdriver.Clients, error) {
		return awsdriver.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}
	checkArtifacts(context.Background(), cfg, rep)
	runAWS(context.Background(), cfg, rep, readOnly(factory, a), a)
	return rep, a
}

func failed(rep *report) []string {
	var out []string
	for _, r := range rep.rows {
		if r.err != nil {
			out = append(out, r.name)
		}
	}
	return out
}

func TestCanaryPassesOnAHealthyWorld(t *testing.T) {
	cfg := testConfig(releases(t))
	rep, a := runCanary(t, account(), cfg)
	if f := failed(rep); len(f) > 0 {
		var b bytes.Buffer
		rep.write(&b)
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	var b bytes.Buffer
	rep.write(&b)
	for _, want := range []string{"sysext kubernetes v1.37.1", "sysext tailscale v1.98.10", "k3s v1.37.2+k3s1", "regions and zones us-east-2",
		"Flatcar stable 4757.2.3 amd64", "Flatcar stable 4757.2.3 arm64", "quota Standard vCPUs", "instance types offered", "read-only guard"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no row %q:\n%s", want, b.String())
		}
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
}

func TestCanaryReportsWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fake.Account, *config)
		missing []string
		want    string
	}{
		"the Flatcar version is not published": {mutate: func(_ *fake.Account, c *config) { c.flatcarVersion = "9999.0.0" }, want: "Flatcar stable 9999.0.0 amd64"},
		"the quota shrank":                     {mutate: func(a *fake.Account, _ *config) { a.SetQuota("us-east-2", standardOnDemand, 8) }, want: "quota Standard vCPUs us-east-2"},
		"the spot quota is gone":               {mutate: func(a *fake.Account, _ *config) { a.RemoveQuota("us-east-2", standardSpot) }, want: "quota Standard vCPUs us-east-2"},
		"a type is not offered":                {mutate: func(a *fake.Account, _ *config) { a.SetOffering("us-east-2", "r6a.large", "use2-az9") }, want: "instance types offered in us-east-2"},
		"a type is unknown":                    {mutate: func(_ *fake.Account, c *config) { c.types = append(c.types, "aws.nonexistent.large") }, want: "instance types offered in us-east-2"},
		"the region is not enabled":            {mutate: func(_ *fake.Account, c *config) { c.region = "ap-south-9" }, want: "regions and zones ap-south-9"},
		"the sysext checksums are gone":        {missing: []string{"kubernetes-v1.37.1"}, want: "sysext kubernetes v1.37.1"},
		"the k3s release is gone":              {missing: []string{"sha256sum-amd64.txt"}, want: "k3s v1.37.2+k3s1"},
	} {
		t.Run(name, func(t *testing.T) {
			acct := account()
			cfg := testConfig(releases(t, tc.missing...))
			if tc.mutate != nil {
				tc.mutate(acct, cfg)
			}
			rep, _ := runCanary(t, acct, cfg)
			if f := failed(rep); !slices.Contains(f, tc.want) {
				var b bytes.Buffer
				rep.write(&b)
				t.Fatalf("want %q among the failed checks, got %v:\n%s", tc.want, f, b.String())
			}
		})
	}
}

func TestMissingImageFailsOnlyItsArchitecture(t *testing.T) {
	acct := fake.New(fake.WithRegions(fake.DefaultRegion("us-east-2", "use2")))
	acct.SetInstanceTypes([]fake.InstanceType{{Name: "t3a.small", VCPUs: 2}, {Name: "t4g.small", VCPUs: 2, Arch: "arm64"}, {Name: "r6a.large", VCPUs: 2}})
	acct.SetQuota("us-east-2", standardOnDemand, 64)
	acct.SetQuota("us-east-2", standardSpot, 32)
	acct.AddImage("us-east-2", fake.Image{ID: "ami-amd64", Name: "Flatcar-stable-4757.2.3-hvm", Owner: awsdriver.DefaultImageOwner})
	rep, _ := runCanary(t, acct, testConfig(releases(t)))
	if f := failed(rep); !slices.Equal(f, []string{"Flatcar stable 4757.2.3 arm64"}) {
		t.Fatalf("failed: %v", f)
	}
}

func TestTableAndExitStatus(t *testing.T) {
	rep := &report{}
	rep.check("fine", func() (string, error) { return "all\ngood", nil })
	rep.check("broken", func() (string, error) { return "partial", fmt.Errorf("it moved\n  badly") })
	var b bytes.Buffer
	rep.write(&b)
	out := b.String()
	for _, want := range []string{"STATUS", "ok", "FAIL", "all good", "broken: it moved badly"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table lacks %q:\n%s", want, out)
		}
	}
	if rep.failures() != 1 {
		t.Errorf("failures = %d", rep.failures())
	}
}

func TestRunUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"-vendor", "nope"}, func(string) string { return "" }, &out, &errb); rc != 2 {
		t.Errorf("an unknown vendor: rc %d", rc)
	}
	if rc := run([]string{"-vendor", "aws"}, func(string) string { return "" }, &out, &errb); rc != 2 || !strings.Contains(errb.String(), "run test/canary/run.sh aws") {
		t.Errorf("no pins: rc %d, %q", rc, errb.String())
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"AWS_REGION": "us-east-2", "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "aws.t3a.small", "E2E_TYPE_ARM64": "aws.t4g.small", "E2E_TYPE_DB": "aws.r6a.large",
		"E2E_TYPES_DESCRIBED": "aws.t3a.small, aws.t3a.medium", "E2E_LOCAL_STORAGE_TYPE": "aws.c6gd.medium",
		"E2E_MIN_VCPU_OD": "16", "E2E_MIN_VCPU_SPOT": "4",
	}
	c, err := loadConfig("aws", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"aws.t3a.small", "aws.t4g.small", "aws.r6a.large", "aws.t3a.small", "aws.t3a.medium", "aws.c6gd.medium"}; !slices.Equal(c.types, want) {
		t.Errorf("types %v", c.types)
	}
	if c.minOnDemand != 16 || c.minSpot != 4 || c.region != "us-east-2" {
		t.Errorf("config %+v", c)
	}
	env["E2E_MIN_VCPU_OD"] = "many"
	if _, err := loadConfig("aws", func(k string) string { return env[k] }); err == nil {
		t.Error("a bad quota number was accepted")
	}
	delete(env, "E2E_K3S_VERSION")
	env["E2E_MIN_VCPU_OD"] = "16"
	if _, err := loadConfig("aws", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "E2E_K3S_VERSION") {
		t.Errorf("a missing pin: %v", err)
	}
}

// The role's policy (test/e2e-aws/canary-policy.sh) is exactly what the canary calls: every
// action the run made is allowed, every allowed action is read-only, and what the policy
// allows beyond the run is the one conditional call (the quota listing the driver makes
// for a limit GetServiceQuota does not know).
func TestPolicyIsExactlyWhatTheCanaryCalls(t *testing.T) {
	out, err := exec.Command("bash", "../e2e-aws/canary-policy.sh", "123456789012", "us-east-2").Output()
	if err != nil {
		t.Skipf("the policy script cannot run here (bash and jq are needed): %v", err)
	}
	var p struct {
		Statement []struct {
			Effect    string
			Action    []string
			Resource  string
			Condition map[string]map[string][]string
		}
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("the policy is not JSON: %v\n%s", err, out)
	}
	if len(p.Statement) != 1 || p.Statement[0].Effect != "Allow" {
		t.Fatalf("want one Allow statement: %s", out)
	}
	allowed := p.Statement[0].Action
	for _, act := range allowed {
		_, op, _ := strings.Cut(act, ":")
		if !isRead(op) || strings.ContainsAny(act, "*?") {
			t.Errorf("the policy allows %q, which is not a plain read", act)
		}
	}
	if got := p.Statement[0].Condition["StringEquals"]["aws:RequestedRegion"]; !slices.Equal(got, []string{"us-east-2"}) {
		t.Errorf("the policy is not limited to the region: %v", p.Statement[0].Condition)
	}
	_, a := runCanary(t, account(), testConfig(releases(t)))
	for _, act := range a.actions() {
		if !slices.Contains(allowed, act) {
			t.Errorf("the canary calls %s, which the policy does not allow", act)
		}
	}
	for _, act := range allowed {
		if !slices.Contains(a.actions(), act) && act != "servicequotas:ListServiceQuotas" {
			t.Errorf("the policy allows %s, which the canary never calls", act)
		}
	}
}
