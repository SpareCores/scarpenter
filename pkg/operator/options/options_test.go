/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options_test

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// parse builds the options the way core's injection does: flags are registered
// (reading the environment for their defaults), then the arguments are parsed.
func parse(t *testing.T, env map[string]string, args ...string) (*options.Options, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("test", flag.ContinueOnError)}
	o := &options.Options{}
	o.AddFlags(fs)
	return o, o.Parse(fs, args...)
}

var required = []string{"--cluster-name=my-cluster", "--api-server-endpoint=https://10.0.0.1:6443"}

func TestEmbeddedCatalogVendors(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"default is oci": {nil, nil, "oci"},
		"flag":           {nil, []string{"--embedded-catalog-vendors=oci, other"}, "oci,other"},
		"none":           {nil, []string{"--embedded-catalog-vendors=none"}, ""},
		"empty":          {nil, []string{"--embedded-catalog-vendors="}, ""},
		"environment":    {map[string]string{"SCARPENTER_EMBEDDED_CATALOG_VENDORS": "other"}, nil, "other"},
		"flag over env":  {map[string]string{"SCARPENTER_EMBEDDED_CATALOG_VENDORS": "other"}, []string{"--embedded-catalog-vendors=none"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			o, err := parse(t, tc.env, append(tc.args, required...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(o.EmbeddedCatalogVendors, ","); got != tc.want {
				t.Errorf("embedded vendors = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	o, err := parse(t, nil, required...)
	if err != nil {
		t.Fatal(err)
	}
	want := options.Options{
		ClusterName:                "my-cluster",
		APIServerEndpoint:          "https://10.0.0.1:6443",
		Vendors:                    []string{"aws"},
		KeeperURL:                  "https://keeper.sparecores.net",
		CatalogRefreshInterval:     time.Hour,
		CatalogMaxAge:              24 * time.Hour,
		BootstrapTokenTTL:          time.Hour,
		MaxLaunchAttempts:          3,
		QuotaRefreshInterval:       2 * time.Minute,
		QuotaLimitsRefreshInterval: 15 * time.Minute,
		QuotaMaxStaleness:          15 * time.Minute,
		GCInterval:                 2 * time.Minute,
		ResourceDefaultTTL:         24 * time.Hour,
		ResourceFullSweepEvery:     10,
		AWSHomeRegion:              "us-east-1",
	}
	if o.ClusterName != want.ClusterName || o.APIServerEndpoint != want.APIServerEndpoint ||
		o.KeeperURL != want.KeeperURL || o.CatalogRefreshInterval != want.CatalogRefreshInterval ||
		o.CatalogMaxAge != want.CatalogMaxAge || o.BootstrapTokenTTL != want.BootstrapTokenTTL ||
		o.MaxLaunchAttempts != want.MaxLaunchAttempts || o.QuotaRefreshInterval != want.QuotaRefreshInterval ||
		o.QuotaLimitsRefreshInterval != want.QuotaLimitsRefreshInterval || o.QuotaMaxStaleness != want.QuotaMaxStaleness ||
		o.GCInterval != want.GCInterval || o.ResourceDefaultTTL != want.ResourceDefaultTTL ||
		o.ResourceFullSweepEvery != want.ResourceFullSweepEvery ||
		o.AWSHomeRegion != want.AWSHomeRegion || strings.Join(o.Vendors, ",") != "aws" {
		t.Errorf("defaults differ:\n got %+v\nwant %+v", *o, want)
	}
	if !o.BillingRetention {
		t.Error("--billing-retention defaults to true: the NodeClass field is the real switch")
	}
	if wantGrace := 10 * time.Minute; o.TerminatingGrace != wantGrace || vendors.DefaultTerminatingGrace != wantGrace {
		t.Errorf("--terminating-grace defaults to %s, vendors.DefaultTerminatingGrace is %s; want both 10m",
			o.TerminatingGrace, vendors.DefaultTerminatingGrace)
	}
	if len(o.ClusterDNS) != 0 || o.ClusterDomain != "cluster.local" {
		t.Errorf("cluster DNS = %v, domain = %q; want none and cluster.local", o.ClusterDNS, o.ClusterDomain)
	}
	for name, v := range map[string]string{"cluster CA file": o.ClusterCAFile, "Keeper API key file": o.KeeperAPIKeyFile, "sysext base URL": o.SysextBaseURL, "Flatcar base URL": o.FlatcarBaseURL} {
		if v != "" {
			t.Errorf("%s defaults to %q, want empty", name, v)
		}
	}
}

func TestEnvironmentFallbacks(t *testing.T) {
	o, err := parse(t, map[string]string{
		"SCARPENTER_CLUSTER_NAME":                  "env-cluster",
		"SCARPENTER_API_SERVER_ENDPOINT":           "https://api.example.com:6443",
		"SCARPENTER_CLUSTER_CA_FILE":               "/etc/ca.pem",
		"SCARPENTER_VENDORS":                       "aws, fake",
		"SCARPENTER_KEEPER_URL":                    "http://keeper.example.com",
		"SCARPENTER_KEEPER_API_KEY_FILE":           "/etc/key",
		"SCARPENTER_CATALOG_REFRESH_INTERVAL":      "30m",
		"SCARPENTER_CATALOG_MAX_AGE":               "12h",
		"SCARPENTER_SYSEXT_BASE_URL":               "https://mirror.example.com/sysext",
		"SCARPENTER_FLATCAR_BASE_URL":              "https://mirror.example.com/flatcar",
		"SCARPENTER_BOOTSTRAP_TOKEN_TTL":           "30m",
		"SCARPENTER_MAX_LAUNCH_ATTEMPTS":           "5",
		"SCARPENTER_QUOTA_REFRESH_INTERVAL":        "1m",
		"SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL": "10m",
		"SCARPENTER_QUOTA_MAX_STALENESS":           "20m",
		"SCARPENTER_GC_INTERVAL":                   "5m",
		"SCARPENTER_RESOURCE_DEFAULT_TTL":          "48h",
		"SCARPENTER_AWS_HOME_REGION":               "eu-central-1",
		"SCARPENTER_GCP_PROJECT":                   "my-project",
		"SCARPENTER_CLUSTER_DNS":                   "10.96.0.10, fd00::a",
		"SCARPENTER_CLUSTER_DOMAIN":                "k8s.example.com",
		"SCARPENTER_BILLING_RETENTION":             "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.BillingRetention {
		t.Error("SCARPENTER_BILLING_RETENTION=false is not applied")
	}
	if o.ClusterName != "env-cluster" || o.APIServerEndpoint != "https://api.example.com:6443" ||
		o.ClusterCAFile != "/etc/ca.pem" || strings.Join(o.Vendors, ",") != "aws,fake" ||
		o.KeeperURL != "http://keeper.example.com" || o.KeeperAPIKeyFile != "/etc/key" ||
		o.CatalogRefreshInterval != 30*time.Minute || o.CatalogMaxAge != 12*time.Hour ||
		o.SysextBaseURL != "https://mirror.example.com/sysext" || o.FlatcarBaseURL != "https://mirror.example.com/flatcar" ||
		o.BootstrapTokenTTL != 30*time.Minute || o.MaxLaunchAttempts != 5 ||
		o.QuotaRefreshInterval != time.Minute || o.QuotaLimitsRefreshInterval != 10*time.Minute ||
		o.QuotaMaxStaleness != 20*time.Minute || o.GCInterval != 5*time.Minute ||
		o.ResourceDefaultTTL != 48*time.Hour || o.AWSHomeRegion != "eu-central-1" ||
		o.GCPProject != "my-project" ||
		strings.Join(o.ClusterDNS, ",") != "10.96.0.10,fd00::a" || o.ClusterDomain != "k8s.example.com" {
		t.Errorf("environment fallbacks not applied: %+v", *o)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	o, err := parse(t, map[string]string{
		"SCARPENTER_CLUSTER_NAME":        "env-cluster",
		"SCARPENTER_API_SERVER_ENDPOINT": "https://env.example.com",
		"SCARPENTER_MAX_LAUNCH_ATTEMPTS": "5",
	}, "--cluster-name=flag-cluster", "--max-launch-attempts=2", "--vendors=fake,qemu")
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "flag-cluster" || o.MaxLaunchAttempts != 2 || o.APIServerEndpoint != "https://env.example.com" {
		t.Errorf("flags did not override the environment: %+v", *o)
	}
	if !o.VendorEnabled("qemu") || !o.VendorEnabled("fake") || o.VendorEnabled("aws") {
		t.Errorf("VendorEnabled wrong for %v", o.Vendors)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no cluster name", []string{"--api-server-endpoint=https://x.example.com"}, "--cluster-name"},
		{"no api server endpoint", []string{"--cluster-name=c"}, "--api-server-endpoint"},
		{"cluster name with spaces", []string{"--cluster-name=my cluster", "--api-server-endpoint=https://x.example.com"}, "must match"},
		{"cluster name too long", []string{"--cluster-name=" + strings.Repeat("a", 64), "--api-server-endpoint=https://x.example.com"}, "must match"},
		{"api server over http", []string{"--cluster-name=c", "--api-server-endpoint=http://x.example.com"}, "absolute https URL"},
		{"api server without scheme", []string{"--cluster-name=c", "--api-server-endpoint=10.0.0.1:6443"}, "absolute https URL"},
		{"keeper url invalid", append([]string{"--keeper-url=keeper"}, required...), "--keeper-url"},
		{"sysext mirror invalid", append([]string{"--sysext-base-url=ftp://x"}, required...), "--sysext-base-url"},
		{"flatcar mirror invalid", append([]string{"--flatcar-base-url=nope"}, required...), "--flatcar-base-url"},
		{"no vendors", append([]string{"--vendors= , "}, required...), "at least one vendor"},
		{"invalid vendor name", append([]string{"--vendors=AWS"}, required...), "is not a vendor name"},
		{"duplicate vendor", append([]string{"--vendors=aws,aws"}, required...), "listed twice"},
		{"zero launch attempts", append([]string{"--max-launch-attempts=0"}, required...), "--max-launch-attempts"},
		{"zero catalog interval", append([]string{"--catalog-refresh-interval=0s"}, required...), "--catalog-refresh-interval must be positive"},
		{"negative token ttl", append([]string{"--bootstrap-token-ttl=-1m"}, required...), "--bootstrap-token-ttl must be positive"},
		{"token ttl shorter than core's registration timeout", append([]string{"--bootstrap-token-ttl=10m"}, required...), "--bootstrap-token-ttl must be at least 20m0s"},
		{"cluster dns not an ip", append([]string{"--cluster-dns=10.96.0.10,dns.example.com"}, required...), "--cluster-dns"},
		{"tailnet prefix not a cidr", append([]string{"--tailnet-prefixes=100.64.0.0/10,nope"}, required...), "--tailnet-prefixes"},
		{"embedded vendor invalid", append([]string{"--embedded-catalog-vendors=OCI"}, required...), "--embedded-catalog-vendors"},
		{"embedded vendor twice", append([]string{"--embedded-catalog-vendors=oci,oci"}, required...), "listed twice"},
		{"empty cluster domain", append([]string{"--cluster-domain="}, required...), "--cluster-domain must not be empty"},
		{"zero gc interval", append([]string{"--gc-interval=0s"}, required...), "--gc-interval must be positive"},
		{"zero terminating grace", append([]string{"--terminating-grace=0s"}, required...), "--terminating-grace must be positive"},
		{"zero full sweep", append([]string{"--resource-full-sweep-every=0"}, required...), "--resource-full-sweep-every must be at least 1"},
		{"zero resource ttl", append([]string{"--resource-default-ttl=0s"}, required...), "--resource-default-ttl must be positive"},
		{"zero quota staleness", append([]string{"--quota-max-staleness=0s"}, required...), "--quota-max-staleness must be positive"},
		{"max age below refresh interval", append([]string{"--catalog-max-age=10m"}, required...), "must not be shorter"},
		{"empty aws home region", append([]string{"--aws-home-region="}, required...), "--aws-home-region"},
		{"unknown flag", append([]string{"--nope=1"}, required...), "parsing flags"},
		{"bad duration", append([]string{"--gc-interval=soon"}, required...), "parsing flags"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, nil, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAllProblemsAreReportedAtOnce(t *testing.T) {
	_, err := parse(t, nil, "--gc-interval=0s")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"--cluster-name", "--api-server-endpoint", "--gc-interval"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestContext(t *testing.T) {
	if options.FromContext(context.Background()) != nil {
		t.Error("FromContext on an empty context must return nil")
	}
	o := &options.Options{ClusterName: "c"}
	if got := options.FromContext(o.ToContext(context.Background())); got != o {
		t.Errorf("FromContext = %v, want the stored options", got)
	}
}

func TestRegisteredWithCore(t *testing.T) {
	found := false
	for _, inj := range coreoptions.Injectables {
		if _, ok := inj.(*options.Options); ok {
			found = true
		}
	}
	if !found {
		t.Error("the SCarpenter options are not among core's Injectables")
	}
}

func TestClusterDNSNone(t *testing.T) {
	o, err := parse(t, nil, append(required, "--cluster-dns=none")...)
	if err != nil {
		t.Fatal(err)
	}
	if !o.ClusterDNSNone || len(o.ClusterDNS) != 0 {
		t.Errorf("none: ClusterDNSNone = %v, ClusterDNS = %v", o.ClusterDNSNone, o.ClusterDNS)
	}
	o, err = parse(t, nil, required...)
	if err != nil || o.ClusterDNSNone {
		t.Errorf("unset must stay unset (the controller then looks the Service up): %v %v", o.ClusterDNSNone, err)
	}
	if _, err = parse(t, nil, append(required, "--cluster-dns=none,10.0.0.1")...); err == nil {
		t.Error("none next to an address must be refused")
	}
}

func TestOCIOptions(t *testing.T) {
	const compartment = "ocid1.compartment.oc1..aaaa"
	base := []string{"--cluster-name=c", "--api-server-endpoint=https://10.0.0.1:6443", "--vendors=oci"}
	federation := []string{"--oci-auth=workloadIdentityFederation", "--oci-region=eu-frankfurt-1", "--oci-token-file=/t",
		"--oci-identity-domain-url=https://idcs.example.com", "--oci-client-id=id", "--oci-client-secret-file=/s"}
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string // an error substring; empty: valid
	}{
		"api key from flags":     {nil, []string{"--oci-compartment=" + compartment}, ""},
		"compartment from env":   {map[string]string{"SCARPENTER_OCI_COMPARTMENT": compartment}, nil, ""},
		"no compartment":         {nil, nil, "--oci-compartment"},
		"compartment of a kind":  {nil, []string{"--oci-compartment=ocid1.subnet.oc1..aaaa"}, "--oci-compartment"},
		"the tenancy root":       {nil, []string{"--oci-compartment=ocid1.tenancy.oc1..aaaa"}, ""},
		"unknown mode":           {nil, []string{"--oci-compartment=" + compartment, "--oci-auth=instancePrincipal"}, "--oci-auth must be apiKey or workloadIdentityFederation"},
		"federation complete":    {nil, append([]string{"--oci-compartment=" + compartment}, federation...), ""},
		"federation incomplete":  {nil, []string{"--oci-compartment=" + compartment, "--oci-auth=workloadIdentityFederation"}, "--oci-token-file is required"},
		"federation http domain": {nil, append(append([]string{"--oci-compartment=" + compartment}, federation...), "--oci-identity-domain-url=http://x"), "--oci-identity-domain-url"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, tc.env, append(append([]string{}, base...), tc.args...)...)
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
	// Without the oci vendor none of it is checked.
	if o, err := parse(t, nil, required...); err != nil || o.OCIAuth != "apiKey" {
		t.Errorf("without oci: %v, auth %q", err, o.OCIAuth)
	}
}
