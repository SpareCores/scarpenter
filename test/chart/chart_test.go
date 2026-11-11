//go:build chart

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package chart tests the Helm chart (charts/scarpenter) without a cluster:
// the rendered manifests against golden files, the values schema and the
// template checks against bad values, and, against the code and config/, that
// every flag of the controller can be set from the chart and that the RBAC of
// the chart is the RBAC of config/rbac and config/rbac-core. Run it with
// `hack/dev.sh make test-chart` (helm is a tool of the dev container).
package chart

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/operator/options"
)

const (
	release   = "scarpenter"
	namespace = "scarpenter-system"
)

var (
	root     = filepath.Join("..", "..")
	chartDir = filepath.Join(root, "charts", "scarpenter")
	required = []string{
		"--set", "cluster.name=my-cluster",
		"--set", "cluster.apiServerEndpoint=https://api.example.com:6443",
		"--set", "cluster.dns={10.96.0.10}",
		"--set", "vendors.aws.credentials.webIdentity.roleArn=arn:aws:iam::123456789012:role/scarpenter",
	}
)

// helm renders the chart; err carries helm's output when it fails.
func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmBinary(), append([]string{"template", release, chartDir, "--namespace", namespace}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", errors.New(err.Error() + ": " + errOut.String())
	}
	return out.String(), nil
}

// helmBinary is helm, or $HELM (to run the tests with another Helm major).
func helmBinary() string {
	if h := os.Getenv("HELM"); h != "" {
		return h
	}
	return "helm"
}

func mustHelm(t *testing.T, args ...string) string {
	t.Helper()
	out, err := helm(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func valuesFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "values", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no values files under testdata/values: %v", err)
	}
	return files
}

func name(path string) string { return strings.TrimSuffix(filepath.Base(path), ".yaml") }

// decode splits a multi-document manifest into typed objects of the kinds the
// tests look at; other kinds are returned as unstructured-free runtime objects
// and ignored.
func decode(t *testing.T, manifest string) []runtime.Object {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	dec := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	var objs []runtime.Object
	d := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		var raw runtime.RawExtension
		if err := d.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw.Raw) == 0 || string(bytes.TrimSpace(raw.Raw)) == "null" {
			continue
		}
		obj, _, err := dec.Decode(raw.Raw, nil, nil)
		if err != nil {
			if runtime.IsNotRegisteredError(err) {
				continue // a kind the tests do not inspect (SCNodeClass, NodePool, ServiceMonitor)
			}
			t.Fatalf("decoding %s: %v", raw.Raw, err)
		}
		objs = append(objs, obj)
	}
	return objs
}

func deployment(t *testing.T, manifest string) *appsv1.Deployment {
	t.Helper()
	for _, o := range decode(t, manifest) {
		if d, ok := o.(*appsv1.Deployment); ok {
			return d
		}
	}
	t.Fatal("no Deployment rendered")
	return nil
}

func TestGolden(t *testing.T) {
	for _, f := range valuesFiles(t) {
		t.Run(name(f), func(t *testing.T) {
			testutil.Golden(t, name(f)+".yaml", []byte(mustHelm(t, "-f", f)))
		})
	}
}

// parseControllerArgs feeds the arguments of the rendered Deployment to the
// controller's own flag parser (core's flags and SCarpenter's), so a flag the
// chart renders that the controller does not know, or a value it rejects, fails.
func parseControllerArgs(t *testing.T, args []string) (*options.Options, *coreoptions.Options, *flag.FlagSet) {
	t.Helper()
	fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("chart", flag.ContinueOnError)}
	core := &coreoptions.Options{}
	core.AddFlags(fs)
	o := &options.Options{}
	o.AddFlags(fs)
	// The controller parses the same arguments twice, with core's options and with
	// SCarpenter's (core's injection and operator.NewOperator).
	if err := core.Parse(fs, args...); err != nil {
		t.Fatalf("core rejects the chart's arguments %v: %v", args, err)
	}
	if err := o.Parse(fs, args...); err != nil {
		t.Fatalf("the controller rejects the chart's arguments %v: %v", args, err)
	}
	return o, core, fs.FlagSet
}

func TestArgumentsAreAcceptedByTheController(t *testing.T) {
	for _, f := range valuesFiles(t) {
		t.Run(name(f), func(t *testing.T) {
			d := deployment(t, mustHelm(t, "-f", f))
			parseControllerArgs(t, d.Spec.Template.Spec.Containers[0].Args)
		})
	}
}

// TestEveryFlagIsSettable renders every values file and checks that the flags
// the chart sets, together, cover every flag of the controller's options: a new
// flag without a value fails here.
func TestEveryFlagIsSettable(t *testing.T) {
	fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("flags", flag.ContinueOnError)}
	(&options.Options{}).AddFlags(fs)
	var all []string
	fs.VisitAll(func(f *flag.Flag) { all = append(all, f.Name) })
	if len(all) < 30 {
		t.Fatalf("found only %d flags: %v", len(all), all)
	}
	set := map[string]bool{}
	for _, f := range valuesFiles(t) {
		for _, a := range deployment(t, mustHelm(t, "-f", f)).Spec.Template.Spec.Containers[0].Args {
			if k, _, ok := strings.Cut(strings.TrimPrefix(a, "--"), "="); ok {
				set[k] = true
			}
		}
	}
	for _, f := range all {
		if !set[f] {
			t.Errorf("no values file sets --%s through the chart (add a value to the Deployment template and set it in testdata/values/full.yaml)", f)
		}
	}
}

// TestFullValuesReachTheController checks the mapping of values to options for
// the file that sets everything: each value lands in the field it is named for.
func TestFullValuesReachTheController(t *testing.T) {
	d := deployment(t, mustHelm(t, "-f", filepath.Join("testdata", "values", "full.yaml")))
	c := d.Spec.Template.Spec.Containers[0]
	o, core, _ := parseControllerArgs(t, c.Args)
	checks := []struct {
		what      string
		got, want any
	}{
		{"cluster name", o.ClusterName, "prod_1"},
		{"API endpoint", o.APIServerEndpoint, "https://api.example.com:6443"},
		{"cluster CA file", o.ClusterCAFile, "/etc/scarpenter/cluster-ca/ca.pem"},
		{"kubelet client CA file", o.KubeletClientCAFile, "/etc/scarpenter/kubelet-client-ca/client-ca.crt"},
		{"cluster DNS", o.ClusterDNS, []string{"10.96.0.10", "fd00::10"}},
		{"cluster domain", o.ClusterDomain, "cluster.example"},
		{"vendors", o.Vendors, []string{"aws"}},
		{"Keeper URL", o.KeeperURL, "https://keeper.example.com"},
		{"Keeper API key file", o.KeeperAPIKeyFile, "/etc/scarpenter/keeper/api-key"},
		{"Keeper regions per request", o.KeeperPricesRegionsPerRequest, 3},
		{"catalog refresh", o.CatalogRefreshInterval, 30 * time.Minute},
		{"catalog max age", o.CatalogMaxAge, 12 * time.Hour},
		{"sysext base URL", o.SysextBaseURL, "https://mirror.example.com/sysext"},
		{"Flatcar base URL", o.FlatcarBaseURL, "https://mirror.example.com/flatcar/{channel}"},
		{"bootstrap token TTL", o.BootstrapTokenTTL, 45 * time.Minute},
		{"tailnet prefixes", o.TailnetPrefixes, []string{"100.64.0.0/10", "10.1.0.0/16"}},
		{"max launch attempts", o.MaxLaunchAttempts, 5},
		{"quota refresh", o.QuotaRefreshInterval, 90 * time.Second},
		{"quota limits refresh", o.QuotaLimitsRefreshInterval, 20 * time.Minute},
		{"quota max staleness", o.QuotaMaxStaleness, 30 * time.Minute},
		{"quota launchable refresh", o.QuotaLaunchableInterval, 2 * time.Hour},
		{"GC interval", o.GCInterval, 3 * time.Minute},
		{"GC grace", o.GCGracePeriod, 10 * time.Minute},
		{"GC dry run", o.GCDryRun, true},
		{"billing retention (default true)", o.BillingRetention, false},
		{"unavailable insufficient capacity", o.UnavailableInsufficientCapacityTTL, 4 * time.Minute},
		{"unavailable unsupported", o.UnavailableUnsupportedTTL, 12 * time.Hour},
		{"unavailable quota exceeded", o.UnavailableQuotaExceededTTL, time.Hour},
		{"unavailable zone unusable", o.UnavailableZoneUnusableTTL, 15 * time.Minute},
		{"resource default TTL", o.ResourceDefaultTTL, 48 * time.Hour},
		{"resource per-use grace", o.ResourcePerUseGrace, 3 * time.Minute},
		{"resource check interval", o.ResourceCheckInterval, 45 * time.Second},
		{"resource full sweep", o.ResourceFullSweepEvery, 20},
		{"AWS home region", o.AWSHomeRegion, "eu-central-1"},
		{"core log level", core.LogLevel, "debug"},
		{"core batch max", core.BatchMaxDuration, 20 * time.Second},
		{"core batch idle", core.BatchIdleDuration, 2 * time.Second},
		{"core metrics port", core.MetricsPort, 9090},
		{"core health port", core.HealthProbePort, 9091},
		{"core node repair gate", core.FeatureGates.NodeRepair, true},
		{"core profiling (extraArgs)", core.EnableProfiling, true},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	for k, want := range map[string]string{
		"AWS_REGION":                  "eu-west-1",
		"SCARPENTER_AWS_IPV4_PRICE":   "0.0036",
		"AWS_ROLE_ARN":                "arn:aws:iam::123456789012:role/scarpenter-prod",
		"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/aws/token",
		"AWS_ROLE_SESSION_NAME":       "scarpenter",
		"SCARPENTER_DEPLOYMENT_NAME":  "scarpenter",
		"HTTPS_PROXY":                 "http://proxy.example.com:3128",
	} {
		if got := env[k].Value; got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"POD_NAMESPACE", "LEADER_ELECTION_NAMESPACE"} {
		if env[k].ValueFrom == nil || env[k].ValueFrom.FieldRef == nil || env[k].ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
			t.Errorf("env %s must come from metadata.namespace, got %+v", k, env[k])
		}
	}
	// Core reads these as bytes and millicores: the divisors must say so.
	if r := env["MEMORY_LIMIT"].ValueFrom.ResourceFieldRef; r.Resource != "limits.memory" || r.Divisor.String() != "1" {
		t.Errorf("MEMORY_LIMIT = %+v, want limits.memory in bytes", r)
	}
	if r := env["CPU_REQUESTS"].ValueFrom.ResourceFieldRef; r.Resource != "requests.cpu" || r.Divisor.String() != "1m" {
		t.Errorf("CPU_REQUESTS = %+v, want requests.cpu in millicores", r)
	}
	if got := c.Image; got != "registry.example.com/scarpenter@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("image = %s, want the digest to win over the tag", got)
	}
}

func TestAWSCredentialModes(t *testing.T) {
	env := func(t *testing.T, args ...string) (map[string]corev1.EnvVar, *appsv1.Deployment) {
		t.Helper()
		d := deployment(t, mustHelm(t, args...))
		m := map[string]corev1.EnvVar{}
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			m[e.Name] = e
		}
		return m, d
	}
	base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com"}

	t.Run("webIdentity", func(t *testing.T) {
		m, d := env(t, append(slices.Clone(base), "--set", "vendors.aws.credentials.webIdentity.roleArn=arn:aws:iam::123456789012:role/r")...)
		if m["AWS_ROLE_ARN"].Value == "" || m["AWS_WEB_IDENTITY_TOKEN_FILE"].Value == "" {
			t.Errorf("web identity environment missing: %v", m)
		}
		found := false
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken.Audience == "sts.amazonaws.com" {
				found = true
			}
		}
		if !found {
			t.Error("no projected ServiceAccount token volume with the STS audience")
		}
		if _, ok := m["AWS_ACCESS_KEY_ID"]; ok {
			t.Error("static keys set next to web identity")
		}
	})
	t.Run("secret", func(t *testing.T) {
		m, _ := env(t, append(slices.Clone(base), "--set", "vendors.aws.credentials.mode=secret", "--set", "vendors.aws.credentials.secret.existingSecret=aws-keys")...)
		ref := m["AWS_SECRET_ACCESS_KEY"].ValueFrom
		if ref == nil || ref.SecretKeyRef.Name != "aws-keys" || ref.SecretKeyRef.Key != "AWS_SECRET_ACCESS_KEY" {
			t.Errorf("AWS_SECRET_ACCESS_KEY = %+v", m["AWS_SECRET_ACCESS_KEY"])
		}
		if _, ok := m["AWS_ROLE_ARN"]; ok {
			t.Error("web identity set next to static keys")
		}
	})
	t.Run("none", func(t *testing.T) {
		m, d := env(t, append(slices.Clone(base), "--set", "vendors.aws.credentials.mode=none")...)
		for _, k := range []string{"AWS_ROLE_ARN", "AWS_ACCESS_KEY_ID", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s set with mode none", k)
			}
		}
		if m["AWS_REGION"].Value != "us-east-1" {
			t.Errorf("AWS_REGION = %q, want the default us-east-1 so that the SDK has a region", m["AWS_REGION"].Value)
		}
		if len(d.Spec.Template.Spec.Volumes) != 1 {
			t.Errorf("volumes %v: only tmp expected", d.Spec.Template.Spec.Volumes)
		}
	})
	t.Run("aws disabled", func(t *testing.T) {
		m, _ := env(t, append(slices.Clone(base), "--set", "vendors.aws.enabled=false", "--set", "vendors.extra={fake}")...)
		if _, ok := m["AWS_REGION"]; ok {
			t.Error("AWS_REGION set with the AWS vendor disabled")
		}
	})
}

// TestRBACMatchesConfig is the drift check: the rules and bindings the chart
// renders are those of config/rbac and config/rbac-core (the namespaces and
// names aside).
func TestRBACMatchesConfig(t *testing.T) {
	rendered := decode(t, mustHelm(t, "-f", filepath.Join("testdata", "values", "default.yaml")))
	cfg := append(decodeFile(t, filepath.Join(root, "config", "rbac", "controller.yaml")),
		append(decodeFile(t, filepath.Join(root, "config", "rbac", "node-join.yaml")),
			decodeFile(t, filepath.Join(root, "config", "rbac-core", "core.yaml"))...)...)

	clusterRoles := map[string][]rbacv1.PolicyRule{}
	roles := map[string][]rbacv1.PolicyRule{} // namespace/name
	bindings := map[string]*rbacv1.ClusterRoleBinding{}
	for _, o := range rendered {
		switch r := o.(type) {
		case *rbacv1.ClusterRole:
			clusterRoles[r.Name] = r.Rules
		case *rbacv1.Role:
			roles[r.Namespace+"/"+r.Name] = r.Rules
		case *rbacv1.ClusterRoleBinding:
			bindings[r.Name] = r
		}
	}
	// config name -> rendered name
	for _, o := range cfg {
		switch r := o.(type) {
		case *rbacv1.ClusterRole:
			got, ok := clusterRoles[r.Name]
			if !ok {
				t.Errorf("ClusterRole %s of config/ is not rendered by the chart", r.Name)
			} else if !reflect.DeepEqual(got, r.Rules) {
				t.Errorf("ClusterRole %s: chart rules differ from config/\n chart: %+v\nconfig: %+v", r.Name, got, r.Rules)
			}
		case *rbacv1.Role:
			ns := r.Namespace
			if ns == "scarpenter-system" {
				ns = namespace
			}
			got, ok := roles[ns+"/"+r.Name]
			if !ok {
				t.Errorf("Role %s/%s of config/ is not rendered by the chart (rendered: %v)", ns, r.Name, keys(roles))
			} else if !reflect.DeepEqual(got, r.Rules) {
				t.Errorf("Role %s/%s: chart rules differ from config/\n chart: %+v\nconfig: %+v", ns, r.Name, got, r.Rules)
			}
		case *rbacv1.ClusterRoleBinding:
			got, ok := bindings[r.Name]
			if !ok {
				t.Errorf("ClusterRoleBinding %s of config/ is not rendered by the chart", r.Name)
				continue
			}
			want := r.DeepCopy()
			for i := range want.Subjects {
				if want.Subjects[i].Kind == "ServiceAccount" {
					want.Subjects[i].Namespace = namespace
				}
			}
			if !reflect.DeepEqual(got.RoleRef, want.RoleRef) || !reflect.DeepEqual(got.Subjects, want.Subjects) {
				t.Errorf("ClusterRoleBinding %s differs\n chart: %+v %+v\nconfig: %+v %+v", r.Name, got.RoleRef, got.Subjects, want.RoleRef, want.Subjects)
			}
		}
	}
}

// TestRBACInvariants are the privilege properties the plans promise, independent
// of config/: no cluster-wide access to Secrets, the join bindings name exactly
// the bootstrap group and system:nodes, and every binding names a Role that
// exists and the Deployment's ServiceAccount.
func TestRBACInvariants(t *testing.T) {
	manifest := mustHelm(t, "-f", filepath.Join("testdata", "values", "default.yaml"))
	rendered := decode(t, manifest)
	clusterRoles := map[string][]rbacv1.PolicyRule{}
	roles := map[string][]rbacv1.PolicyRule{}
	bindings := map[string]*rbacv1.ClusterRoleBinding{}
	for _, o := range rendered {
		switch r := o.(type) {
		case *rbacv1.ClusterRole:
			clusterRoles[r.Name] = r.Rules
		case *rbacv1.Role:
			roles[r.Namespace+"/"+r.Name] = r.Rules
		case *rbacv1.ClusterRoleBinding:
			bindings[r.Name] = r
		}
	}
	for name, rules := range clusterRoles {
		for _, r := range rules {
			if slices.Contains(r.Resources, "secrets") || slices.Contains(r.Resources, "*") {
				t.Errorf("ClusterRole %s grants access to secrets cluster-wide: %+v", name, r)
			}
		}
	}
	for _, name := range []string{"scarpenter-node-bootstrapper", "scarpenter-auto-approve-node-client-csrs", "scarpenter-auto-approve-node-client-renewals"} {
		b, ok := bindings[name]
		if !ok {
			t.Errorf("join binding %s not rendered", name)
			continue
		}
		for _, s := range b.Subjects {
			if s.Kind != "Group" || (s.Name != "system:bootstrappers:scarpenter" && s.Name != "system:nodes") {
				t.Errorf("join binding %s has subject %+v", name, s)
			}
		}
	}
	// Every binding of a rendered Role or ClusterRole names one that exists and
	// the ServiceAccount of the Deployment.
	d := deployment(t, manifest)
	for _, o := range rendered {
		switch b := o.(type) {
		case *rbacv1.RoleBinding:
			if _, ok := roles[b.Namespace+"/"+b.RoleRef.Name]; !ok {
				t.Errorf("RoleBinding %s/%s references the missing Role %s", b.Namespace, b.Name, b.RoleRef.Name)
			}
			if b.Subjects[0].Name != d.Spec.Template.Spec.ServiceAccountName || b.Subjects[0].Namespace != namespace {
				t.Errorf("RoleBinding %s/%s subject %+v is not the Deployment's ServiceAccount", b.Namespace, b.Name, b.Subjects[0])
			}
		case *rbacv1.ClusterRoleBinding:
			if b.Subjects[0].Kind != "ServiceAccount" {
				continue
			}
			if _, ok := clusterRoles[b.RoleRef.Name]; !ok {
				t.Errorf("ClusterRoleBinding %s references the missing ClusterRole %s", b.Name, b.RoleRef.Name)
			}
			if b.Subjects[0].Name != d.Spec.Template.Spec.ServiceAccountName {
				t.Errorf("ClusterRoleBinding %s subject %+v is not the Deployment's ServiceAccount", b.Name, b.Subjects[0])
			}
		}
	}
}

func TestAuthKeySecretsInOtherNamespaces(t *testing.T) {
	out := mustHelm(t, append(slices.Clone(required), "--set", "rbac.authKeySecrets[0].namespace=tailscale", "--set", "rbac.authKeySecrets[0].name=ts-node-key")...)
	var role *rbacv1.Role
	var binding *rbacv1.RoleBinding
	for _, o := range decode(t, out) {
		switch r := o.(type) {
		case *rbacv1.Role:
			if r.Namespace == "tailscale" {
				role = r
			}
		case *rbacv1.RoleBinding:
			if r.Namespace == "tailscale" {
				binding = r
			}
		}
	}
	if role == nil || binding == nil {
		t.Fatalf("no Role and RoleBinding in the namespace tailscale:\n%s", out)
	}
	want := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"ts-node-key"}, Verbs: []string{"get"}}}
	if !reflect.DeepEqual(role.Rules, want) {
		t.Errorf("rules = %+v, want get on that one Secret only", role.Rules)
	}
	if binding.RoleRef.Name != role.Name || binding.Subjects[0].Namespace != namespace {
		t.Errorf("binding %+v does not bind the controller's ServiceAccount to the Role", binding)
	}
}

func TestLeaderElectionOffRendersNoLeaseRole(t *testing.T) {
	out := mustHelm(t, append(slices.Clone(required), "--set", "leaderElection.enabled=false")...)
	for _, o := range decode(t, out) {
		if r, ok := o.(*rbacv1.Role); ok && strings.HasSuffix(r.Name, "-leader-election") {
			t.Error("leader election Role rendered with leader election off")
		}
	}
	d := deployment(t, out)
	var found bool
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		found = found || (e.Name == "DISABLE_LEADER_ELECTION" && e.Value == "true")
	}
	if !found {
		t.Error("DISABLE_LEADER_ELECTION=true not set")
	}
}

func TestPodDisruptionBudgetOnlyWithReplicas(t *testing.T) {
	if strings.Contains(mustHelm(t, required...), "kind: PodDisruptionBudget") {
		t.Error("a PodDisruptionBudget for a single replica blocks node drains")
	}
	if !strings.Contains(mustHelm(t, append(slices.Clone(required), "--set", "replicaCount=2")...), "kind: PodDisruptionBudget") {
		t.Error("no PodDisruptionBudget with two replicas")
	}
}

// TestControllerAvoidsNodesItManages: the default affinity prefers nodes that no
// NodePool owns (the controller must not run on a node it can remove).
func TestControllerAvoidsNodesItManages(t *testing.T) {
	d := deployment(t, mustHelm(t, required...))
	aff := d.Spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || len(aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("no preferred node affinity")
	}
	e := aff.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Preference.MatchExpressions[0]
	if e.Key != "karpenter.sh/nodepool" || e.Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Errorf("affinity expression = %+v, want karpenter.sh/nodepool DoesNotExist", e)
	}
	sc := d.Spec.Template.Spec.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("container security context %+v is not read-only and unprivileged", sc)
	}
	psc := d.Spec.Template.Spec.SecurityContext
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v: want runAsNonRoot and RuntimeDefault seccomp", psc)
	}
}

func TestKubeletClientCASources(t *testing.T) {
	d := deployment(t, mustHelm(t, "-f", filepath.Join("testdata", "values", "static-keys.yaml")))
	var vol *corev1.Volume
	for i, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "kubelet-client-ca" {
			vol = &d.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.ConfigMap == nil || vol.ConfigMap.Name != "k3s-client-ca" {
		t.Fatalf("kubelet client CA volume = %+v, want the ConfigMap k3s-client-ca", vol)
	}
	// The inline source renders the ConfigMap the volume points at.
	out := mustHelm(t, "-f", filepath.Join("testdata", "values", "full.yaml"))
	var found bool
	for _, o := range decode(t, out) {
		if cm, ok := o.(*corev1.ConfigMap); ok && strings.HasSuffix(cm.Name, "-kubelet-client-ca") {
			found = strings.Contains(cm.Data["client-ca.crt"], "BEGIN CERTIFICATE")
		}
	}
	if !found {
		t.Error("source inline renders no ConfigMap with the PEM")
	}
}

// TestRejectedValues are the negative cases: the values schema (the first part)
// and the checks of the templates (the second) refuse them.
func TestRejectedValues(t *testing.T) {
	cases := map[string]struct {
		set  []string
		want string // a substring of helm's error
	}{
		"unknown key":                  {[]string{"foo=bar"}, "additional properties"},
		"unknown nested key":           {[]string{"cluster.nme=x"}, "additional properties"},
		"http endpoint":                {[]string{"cluster.apiServerEndpoint=http://api.example.com"}, "apiServerEndpoint"},
		"bad cluster name":             {[]string{"cluster.name=-bad"}, "name"},
		"long cluster name":            {[]string{"cluster.name=" + strings.Repeat("a", 64)}, "name"},
		"bad credentials mode":         {[]string{"vendors.aws.credentials.mode=keys"}, "mode"},
		"bad role ARN":                 {[]string{"vendors.aws.credentials.webIdentity.roleArn=role"}, "roleArn"},
		"token expiry too short":       {[]string{"vendors.aws.credentials.webIdentity.expirationSeconds=60"}, "expirationSeconds"},
		"bad duration":                 {[]string{"gc.interval=soon"}, "interval"},
		"decimal token TTL":            {[]string{"bootstrap.tokenTTL=0.3h"}, "tokenTTL"},
		"negative ipv4 price":          {[]string{"vendors.aws.ipv4Price=-1"}, "ipv4Price"},
		"bad port":                     {[]string{"karpenter.metricsPort=70000"}, "metricsPort"},
		"bad log level":                {[]string{"karpenter.logLevel=trace"}, "logLevel"},
		"bad image digest":             {[]string{"image.digest=latest"}, "digest"},
		"bad kubelet CA source":        {[]string{"kubeletClientCA.source=file"}, "source"},
		"zero launch attempts":         {[]string{"launch.maxAttempts=0"}, "maxAttempts"},
		"zero full sweep":              {[]string{"managedResources.fullSweepEvery=0"}, "fullSweepEvery"},
		"auth key secret without name": {[]string{"rbac.authKeySecrets[0].namespace=ns"}, "name"},
		"string replicas":              {[]string{"replicaCount=many"}, "replicaCount"},

		"token TTL under 20m":       {[]string{"bootstrap.tokenTTL=19m"}, "at least 20m"},
		"token TTL in seconds":      {[]string{"bootstrap.tokenTTL=1000s"}, "at least 20m"},
		"role ARN missing":          {[]string{"vendors.aws.credentials.webIdentity.roleArn="}, "roleArn is required"},
		"secret missing":            {[]string{"vendors.aws.credentials.mode=secret"}, "existingSecret is required"},
		"no vendor":                 {[]string{"vendors.aws.enabled=false"}, "at least one vendor"},
		"inline CA without PEM":     {[]string{"kubeletClientCA.source=inline"}, "kubeletClientCA.inline is required"},
		"lookup CA without cluster": {[]string{"kubeletClientCA.source=extensionApiserverAuthentication"}, "live cluster"},
		"tailscale example":         {[]string{"examples.enabled=true", "examples.nodeClass.networkTemplate=tailscale"}, "tailscale.version is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			args := slices.Clone(required)
			for _, s := range c.set {
				args = append(args, "--set", s)
			}
			out, err := helm(t, args...)
			if err == nil {
				t.Fatalf("helm accepted --set %v", c.set)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			_ = out
		})
	}
	// The two required values are required.
	for _, drop := range []string{"cluster.name", "cluster.apiServerEndpoint"} {
		t.Run("missing "+drop, func(t *testing.T) {
			var args []string
			for i := 0; i < len(required); i += 2 {
				if !strings.HasPrefix(required[i+1], drop+"=") {
					args = append(args, required[i], required[i+1])
				}
			}
			if _, err := helm(t, args...); err == nil || !strings.Contains(err.Error(), drop+" is required") {
				t.Errorf("without %s: %v", drop, err)
			}
		})
	}
}

// TestCRDsAreSynced: the CRDs in the chart's crds/ are those of config/crd and of
// the Karpenter core version of go.mod (make chart-crds copies them).
func TestCRDsAreSynced(t *testing.T) {
	core := coreModuleDir(t)
	pairs := map[string]string{
		"scarpenter.sparecores.com_scnodeclasses.yaml": filepath.Join(root, "config", "crd", "bases", "scarpenter.sparecores.com_scnodeclasses.yaml"),
		"karpenter.sh_nodepools.yaml":                  filepath.Join(core, "pkg", "apis", "crds", "karpenter.sh_nodepools.yaml"),
		"karpenter.sh_nodeclaims.yaml":                 filepath.Join(core, "pkg", "apis", "crds", "karpenter.sh_nodeclaims.yaml"),
	}
	entries, err := os.ReadDir(filepath.Join(chartDir, "crds"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(pairs) {
		t.Errorf("chart crds/ has %d files, want %d (make chart-crds removes stale ones)", len(entries), len(pairs))
	}
	for chartFile, src := range pairs {
		got, err := os.ReadFile(filepath.Join(chartDir, "crds", chartFile)) //nolint:gosec // test data paths
		if err != nil {
			t.Errorf("%s: %v (make chart-crds)", chartFile, err)
			continue
		}
		want, err := os.ReadFile(src) //nolint:gosec // test data paths
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("crds/%s differs from %s (make chart-crds)", chartFile, src)
		}
	}
}

func coreModuleDir(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/karpenter")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func decodeFile(t *testing.T, path string) []runtime.Object {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // repository files
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, string(b))
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestKubernetesMinimum pins the minimum Kubernetes version: the SCNodeClass CRD
// uses the CEL CIDR library (isCIDR, Kubernetes 1.31); on an older API server the
// CRD is rejected, so the chart must not claim to support one.
func TestKubernetesMinimum(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `kubeVersion: ">=1.31.0-0"`) {
		t.Errorf("Chart.yaml kubeVersion is not >=1.31.0-0:\n%s", b)
	}
	crd, err := os.ReadFile(filepath.Join(chartDir, "crds", "scarpenter.sparecores.com_scnodeclasses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(crd), "isCIDR") {
		t.Error("the CRD no longer uses isCIDR: the 1.31 minimum may be lowered (and this test with it)")
	}
	// helm refuses to install on an older cluster.
	args := append(slices.Clone(required), "--kube-version", "1.30.0")
	if _, err := helm(t, args...); err == nil || !strings.Contains(err.Error(), "kubeVersion") {
		t.Errorf("rendering for Kubernetes 1.30 must be refused by kubeVersion: %v", err)
	}
	if _, err := helm(t, append(slices.Clone(required), "--kube-version", "1.31.0")...); err != nil {
		t.Errorf("Kubernetes 1.31 must be accepted: %v", err)
	}
}

// TestClusterDNSFlag: an explicit value is passed as it is, "none" switches the
// cluster DNS off, and with no value the flag is left out so that the controller
// finds the DNS Service itself (the chart looks nothing up).
func TestClusterDNSFlag(t *testing.T) {
	base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com", "--set", "vendors.aws.credentials.mode=none"}
	dnsArg := func(args ...string) []string {
		var out []string
		for _, a := range deployment(t, mustHelm(t, append(slices.Clone(base), args...)...)).Spec.Template.Spec.Containers[0].Args {
			if strings.HasPrefix(a, "--cluster-dns") {
				out = append(out, a)
			}
		}
		return out
	}
	if got := dnsArg("--set", "cluster.dns={10.0.0.10,fd00::a}"); !slices.Equal(got, []string{"--cluster-dns=10.0.0.10,fd00::a"}) {
		t.Errorf("explicit: %v", got)
	}
	if got := dnsArg("--set", "cluster.dns={none}"); !slices.Equal(got, []string{"--cluster-dns=none"}) {
		t.Errorf("none: %v", got)
	}
	if got := dnsArg(); len(got) != 0 {
		t.Errorf("without a value the flag must be left to the controller, got %v", got)
	}
	if strings.Contains(mustHelm(t, base...), "lookup") {
		t.Error("the chart must not look anything up")
	}
	// What the chart renders for the three cases is accepted by the controller.
	for _, a := range [][]string{{"--cluster-dns=none"}, {"--cluster-dns=10.0.0.10"}, nil} {
		o, _, _ := parseControllerArgs(t, append([]string{"--cluster-name=c", "--api-server-endpoint=https://a.example.com"}, a...))
		if len(a) > 0 && a[0] == "--cluster-dns=none" && !o.ClusterDNSNone {
			t.Error("the controller does not read --cluster-dns=none")
		}
	}
}

func TestPodAndLeaseSettings(t *testing.T) {
	d := deployment(t, mustHelm(t, required...))
	if d.Spec.Template.Annotations["karpenter.sh/do-not-disrupt"] != "true" {
		t.Error("the controller pod must carry karpenter.sh/do-not-disrupt")
	}
	var lease string
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "LEADER_ELECTION_NAME" {
			lease = e.Value
		}
	}
	if lease != "scarpenter-leader-election" {
		t.Errorf("lease name %q: must not be core's shared default karpenter-leader-election", lease)
	}
}

func TestNumericIPv4Price(t *testing.T) {
	for set, want := range map[string]string{"0.004": "0.004", "0": "0"} {
		d := deployment(t, mustHelm(t, append(slices.Clone(required), "--set", "vendors.aws.ipv4Price="+set)...))
		var got string
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "SCARPENTER_AWS_IPV4_PRICE" {
				got = e.Value
			}
		}
		if got != want {
			t.Errorf("ipv4Price=%s: env %q, want %q", set, got, want)
		}
	}
}

// TestImageTagPolicy runs the release script's decision on which tags a release
// moves: "latest" and the major tag only for the highest stable version, the
// minor tag for the highest version of its minor, and none of them backwards.
func TestImageTagPolicy(t *testing.T) {
	existing := "v0.1.0\nv0.1.4\nv0.2.0\nv0.9.0\nv0.10.0\nv0.2.0-rc.1\n"
	cases := []struct {
		tag, prerelease string
		latest, minor   string
	}{
		{"v0.10.1", "false", "true", "true"},  // highest overall; sorts after 0.10.0, not as text
		{"v0.9.1", "false", "false", "true"},  // a patch of an older minor: its own minor tag only
		{"v0.1.5", "false", "false", "true"},  // v0.1.5 after v0.2.0 and v0.10.0 updates 0.1 but not latest
		{"v0.1.3", "false", "false", "false"}, // below v0.1.4: never backwards
		{"v0.11.0", "false", "true", "true"},
		{"v0.12.0-rc.1", "true", "false", "false"},
		{"v1.0.0", "false", "true", "true"},
	}
	check := func(existing, tag, prerelease, latest, minor string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(root, "hack", "release.sh"), "latest", tag, prerelease)
		cmd.Stdin = strings.NewReader(existing)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s (existing tags %q): %v\n%s", tag, existing, err, out)
		}
		want := "latest=" + latest + "\nminor=" + minor + "\n"
		if string(out) != want {
			t.Errorf("%s (existing tags %q): got %q, want %q", tag, existing, out, want)
		}
	}
	for _, c := range cases {
		check(existing, c.tag, c.prerelease, c.latest, c.minor)
	}
	// A new repository: the first release candidate (no stable tag at all, which
	// must not fail the script), the first release, and a re-run of a tag that
	// already exists.
	check("", "v0.1.0-rc.1", "true", "false", "false")
	check("v0.1.0-rc.1\n", "v0.1.0-rc.2", "true", "false", "false")
	check("v0.1.0-rc.1\n", "v0.1.0", "false", "true", "true")
	check("v0.1.0\nv0.1.0-rc.1\n", "v0.1.0", "false", "true", "true")
}
