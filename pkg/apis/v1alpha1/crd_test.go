/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// These tests install the generated CRD into an envtest API server and send
// objects exactly as users write them (unstructured YAML), so they exercise
// the schema, defaulting and CEL rules the way kubectl apply does.

var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(func() int {
		cfg, stop, err := testutil.StartEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if err := stop(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
		scheme := runtime.NewScheme()
		if err := v1alpha1.AddToScheme(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}())
}

// baseSpec is a minimal valid spec; tests overlay fields on it.
const baseSpec = `
flatcar: {version: "4757.2.3"}
kubernetes: {version: "v1.37.1"}
network: {template: direct}
vendors: {aws: {}}
`

// merge overlays b on a: maps merge recursively, null deletes a key, everything
// else is replaced.
func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		if v == nil { // null deletes the key
			delete(a, k)
			continue
		}
		if bm, ok := v.(map[string]any); ok {
			if am, ok := a[k].(map[string]any); ok {
				a[k] = merge(am, bm)
				continue
			}
		}
		a[k] = v
	}
	return a
}

func mustYAML(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("parsing YAML: %v\n%s", err, doc)
	}
	return m
}

func parseObject(t *testing.T, name, specYAML string) *unstructured.Unstructured {
	t.Helper()
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "scarpenter.sparecores.com/v1alpha1",
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": name},
		"spec":       mustYAML(t, specYAML),
	}}
}

// withSpec returns baseSpec with the given YAML merged on top.
func withSpec(t *testing.T, extra string) string {
	t.Helper()
	out, err := yaml.Marshal(merge(mustYAML(t, baseSpec), mustYAML(t, extra)))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func create(t *testing.T, obj *unstructured.Unstructured) error {
	t.Helper()
	// Strict field validation, like kubectl: unknown fields are errors.
	err := k8sClient.Create(context.Background(), obj, client.FieldValidation("Strict"))
	if err == nil {
		t.Cleanup(func() {
			if err := k8sClient.Delete(context.Background(), obj); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup: %v", err)
			}
		})
	}
	return err
}

func loadObject(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, &obj.Object); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return obj
}

func TestSamplesAreAccepted(t *testing.T) {
	samples, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "config", "samples", "scarpenter_*.yaml"))
	if err != nil || len(samples) < 2 {
		t.Fatalf("expected the samples under config/samples, got %v (err %v)", samples, err)
	}
	for _, f := range samples {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if err := create(t, loadObject(t, f)); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func TestValidSpecsAreAccepted(t *testing.T) {
	tests := map[string]string{
		"minimal":                           ``,
		"every vendor":                      `vendors: {aws: {}, fake: {}, qemu: {}}`,
		"template tailscale":                `network: {template: tailscale, tailscale: {version: v1.98.10, authKeySecretRef: {name: nm, namespace: ns, key: k}}}`,
		"template custom":                   `network: {template: custom, custom: {configMapRef: {name: nm, namespace: ns, key: k}}}`,
		"headscale login server":            `network: {template: tailscale, tailscale: {version: v1.98.10, loginServer: "https://hs.example.com", authKeySecretRef: {name: nm, namespace: ns, key: k}}}`,
		"inbound rules":                     `networking: {inbound: {rules: [{protocol: tcp, fromPort: 8000, toPort: 8100, cidrs: [10.0.0.0/8, "2001:db8::/32"]}, {protocol: icmp, cidrs: [0.0.0.0/0]}]}}`,
		"resource use ids":                  `vendors: {aws: {resources: {subnets: {use: {ids: {us-east-1: [subnet-0123]}}}}}}`,
		"resource use name":                 `vendors: {aws: {resources: {subnets: {use: {name: "sc-{region}"}}}}}`,
		"resource use tags":                 `vendors: {aws: {resources: {subnets: {use: {tags: {env: prod, owner: "*"}}}}}}`,
		"resource use everything":           `vendors: {aws: {resources: {subnets: {use: {ids: {"*": [subnet-1], us-east-1: [subnet-2]}, name: "a-*", tags: {env: prod}, match: {zone: [use1-az1], network: [vpc-1], cidr: 10.0.0.0/24, within: 10.0.0.0/16, state: [available], createdAfter: "2026-01-01T00:00:00Z", description: "x*"}, filters: [{name: state, values: [available]}], select: newest, orderBy: "label:version"}}}}}`,
		"resource use select all":           `vendors: {aws: {resources: {securityGroups: {use: {tags: {env: prod}, select: all}}}}}`,
		"resource forbid":                   `vendors: {aws: {resources: {placementGroups: {forbid: true}}}}`,
		"resource forbid false with create": `vendors: {aws: {resources: {securityGroups: {forbid: false, create: {}}}}}`,
		"lifecycle retain":                  `vendors: {fake: {resources: {networks: {create: {lifecycle: {policy: retain}}}}}}`,
		"pre-create":                        `vendors: {fake: {resources: {networks: {create: {timing: {mode: preCreate, regions: [r1]}}}}}}`,
		"on-demand timing":                  `vendors: {fake: {resources: {networks: {create: {timing: {mode: onDemand}}}}}}`,
		"io2 root":                          `vendors: {aws: {storage: {root: {volumeType: io2, iops: 5000}}}}`,
		"placement group partition":         `profiles: {placement: {p: {aws: {placementGroup: {strategy: partition, partitionCount: 3}}}}}`,
		"profile for other vendors":         `profiles: {storage: {s: {qemu: {root: {size: 20Gi, format: raw}}, fake: {}}}}`,
		"selection ranking only":            `selections: {s: {ranking: {score: {workloadProfile: web}, families: {max: 3}}}}`,
		"selection selector only":           `selections: {s: {selector: {vcpus: {min: 2}}}}`,
		"kubelet eviction signals":          `kubelet: {evictionHard: {nodefs.available: 10%, imagefs.available: 15%}, kubeReserved: {cpu: 200m, pid: "1000"}}`,
		"poweroffAfter composite":           `poweroffAfter: 1h30m`,
		"curated cpu flags":                 `catalog: {cpuFlagLabels: [avx2, avx512_vnni, amx_tile, sha_ni, vaes, gfni]}`,
		"alpha channel":                     `flatcar: {channel: alpha}`,
		"images by ids":                     `vendors: {aws: {resources: {images: {use: {ids: {us-east-1: [ami-0123456789abcdef0], "*": [ami-0123456789abcdef1]}}}}}}`,
		"images by name and match":          `vendors: {aws: {resources: {images: {use: {name: "Flatcar-{channel}-{version}-*", match: {owner: ["123456789012"], architecture: [amd64]}, select: newest, orderBy: created}}}}}`,
		"images by tags, select exactlyOne": `vendors: {fake: {resources: {images: {use: {tags: {flatcar-version: "{version}"}, select: exactlyOne}}}}}`,
		"images with filters":               `vendors: {aws: {resources: {images: {use: {filters: [{name: root-device-type, values: [ebs]}]}}}}}`,
		"bootstrap hardening":               `bootstrap: {hardening: [metadata-from-pods]}`,
		"bootstrap without hardening":       `bootstrap: {}`,
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if err := create(t, parseObject(t, "valid-"+slug(name), withSpec(t, spec))); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}

func slug(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
}

func get(t *testing.T, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDefaultsAreApplied(t *testing.T) {
	obj := parseObject(t, "defaults", withSpec(t, `
metrics: {m: {workloadProfile: web}}
vendors: {qemu: {}}
profiles:
  storage: {s: {fake: {data: [{name: d, size: 1Gi, mountPath: /d}]}}}
`))
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := get(t, obj)
	want := map[string]any{
		"spec.flatcar.channel":                            "stable",
		"spec.networking.publicIPv4":                      true,
		"spec.networking.publicIPv6":                      false,
		"spec.networking.isolation":                       "run",
		"spec.kubelet.maxPods":                            int64(110),
		"spec.kubelet.vmMemoryOverheadPercent":            7.5,
		"spec.pricing.allowIncomplete":                    true,
		"spec.metrics.m.normalize":                        "None",
		"spec.metrics.m.scale":                            int64(1),
		"spec.vendors.qemu.accelerator":                   "auto",
		"spec.profiles.storage.s.fake.data[0].filesystem": "ext4",
	}
	for path, w := range want {
		g, found, err := nestedField(got.Object, path)
		if err != nil || !found {
			t.Errorf("%s: not found (err %v)", path, err)
			continue
		}
		if fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("%s = %v, want %v", path, g, w)
		}
	}
	// Vendor blocks must not default anything the driver defaults itself.
	if _, found, _ := nestedField(got.Object, "spec.vendors.qemu.storage"); found {
		t.Error("spec.vendors.qemu.storage was defaulted")
	}
}

func TestExplicitValuesOverrideDefaults(t *testing.T) {
	obj := parseObject(t, "explicit", withSpec(t, `
networking: {publicIPv4: false, publicIPv6: true, isolation: none}
kubelet: {maxPods: 30, vmMemoryOverheadPercent: 0}
pricing: {allowIncomplete: false}
`))
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := get(t, obj)
	for path, want := range map[string]string{
		"spec.networking.publicIPv4":           "false",
		"spec.networking.publicIPv6":           "true",
		"spec.networking.isolation":            "none",
		"spec.kubelet.maxPods":                 "30",
		"spec.kubelet.vmMemoryOverheadPercent": "0",
		"spec.pricing.allowIncomplete":         "false",
	} {
		g, found, err := nestedField(got.Object, path)
		if err != nil || !found {
			t.Errorf("%s: not found (err %v)", path, err)
			continue
		}
		if fmt.Sprint(g) != want {
			t.Errorf("%s = %v, want %s", path, g, want)
		}
	}
}

// nestedField reads a dotted path such as spec.a.b[0].c.
func nestedField(obj map[string]any, path string) (any, bool, error) {
	var cur any = obj
	for part := range strings.SplitSeq(path, ".") {
		idx := -1
		if i := strings.Index(part, "["); i >= 0 {
			if _, err := fmt.Sscanf(part[i:], "[%d]", &idx); err != nil {
				return nil, false, err
			}
			part = part[:i]
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		if cur, ok = m[part]; !ok {
			return nil, false, nil
		}
		if idx >= 0 {
			l, ok := cur.([]any)
			if !ok || idx >= len(l) {
				return nil, false, nil
			}
			cur = l[idx]
		}
	}
	return cur, true, nil
}

func TestInvalidObjectsAreRejected(t *testing.T) {
	const tailscale = `{version: v1.98.10, authKeySecretRef: {name: nm, namespace: ns, key: k}}`
	const custom = `{configMapRef: {name: nm, namespace: ns, key: k}}`
	tests := []struct {
		name string
		spec string
		// substring of the API error
		wantErr string
	}{
		// network template (overview section 6.1)
		{"tailscale template without tailscale block", `network: {template: tailscale}`, "network.tailscale is required with template tailscale"},
		{"direct template with tailscale block", `network: {template: direct, tailscale: ` + tailscale + `}`, "invalid otherwise"},
		{"custom template without custom block", `network: {template: custom}`, "network.custom is required with template custom"},
		{"direct template with custom block", `network: {template: direct, custom: ` + custom + `}`, "network.custom is required with template custom and invalid otherwise"},
		{"tailscale template with custom block", `network: {template: tailscale, tailscale: ` + tailscale + `, custom: ` + custom + `}`, "network.custom is required with template custom and invalid otherwise"},
		{"unknown template", `network: {template: wireguard}`, "Unsupported value"},
		{"tailscale tag without prefix", `network: {template: tailscale, tailscale: {version: v1.98.10, tags: [k8s], authKeySecretRef: {name: nm, namespace: ns, key: k}}}`, "should match"},
		{"tailscale login server not a URL", `network: {template: tailscale, tailscale: {version: v1.98.10, loginServer: "ftp://x", authKeySecretRef: {name: nm, namespace: ns, key: k}}}`, "should match"},
		// versions
		{"flatcar version not numeric", `flatcar: {version: stable}`, "should match"},
		{"flatcar version with v prefix", `flatcar: {version: v4757.2.3}`, "should match"},
		{"unknown flatcar channel", `flatcar: {channel: nightly}`, "Unsupported value"},
		{"kubernetes version without v", `kubernetes: {version: "1.37.1"}`, "should match"},
		{"tailscale version without v", `network: {template: tailscale, tailscale: {version: "1.98.10", authKeySecretRef: {name: nm, namespace: ns, key: k}}}`, "should match"},
		// required fields
		{"flatcar without version", `flatcar: {version: null}`, "Required value"},
		{"vendors without any block", `vendors: {aws: null}`, "should have at least 1 properties"},
		// shared settings
		{"cpu flag outside the curated set", `catalog: {cpuFlagLabels: [mmx]}`, "Unsupported value"},
		{"unknown isolation", `networking: {isolation: all}`, "Unsupported value"},
		{"inbound ports with icmp", `networking: {inbound: {rules: [{protocol: icmp, fromPort: 8, cidrs: [10.0.0.0/8]}]}}`, "ports are only valid for tcp and udp"},
		{"inbound toPort without fromPort", `networking: {inbound: {rules: [{protocol: tcp, toPort: 80, cidrs: [10.0.0.0/8]}]}}`, "toPort needs fromPort"},
		{"inbound toPort below fromPort", `networking: {inbound: {rules: [{protocol: tcp, fromPort: 90, toPort: 80, cidrs: [10.0.0.0/8]}]}}`, "toPort must not be smaller than fromPort"},
		{"inbound cidr invalid", `networking: {inbound: {rules: [{protocol: tcp, cidrs: [not-a-cidr]}]}}`, "cidrs must be valid CIDR notation"},
		{"inbound rule without cidrs", `networking: {inbound: {rules: [{protocol: tcp, cidrs: []}]}}`, "should have at least 1 items"},
		{"inbound port zero", `networking: {inbound: {rules: [{protocol: tcp, fromPort: 0, cidrs: [10.0.0.0/8]}]}}`, "should be greater than or equal to 1"},
		{"kubelet maxPods zero", `kubelet: {maxPods: 0}`, "should be greater than or equal to 1"},
		{"kubelet overhead above 50", `kubelet: {vmMemoryOverheadPercent: 60}`, "should be less than or equal to 50"},
		{"systemReserved unknown key", `kubelet: {systemReserved: {gpu: "1"}}`, "systemReserved keys must be"},
		{"kubeReserved unknown key", `kubelet: {kubeReserved: {gpu: "1"}}`, "kubeReserved keys must be"},
		{"evictionHard unknown signal", `kubelet: {evictionHard: {disk.available: 1Gi}}`, "evictionHard keys must be eviction signals"},
		{"poweroffAfter not a duration", `poweroffAfter: tomorrow`, "should match"},
		{"tag key in the reserved domain", `tags: {scarpenter.sparecores.com/owner: me}`, "tag keys in the scarpenter.sparecores.com domain are reserved"},
		{"sysext sha256 too short", `sysexts: [{name: x, arch: amd64, url: "https://example.com/x.raw", sha256: abc}]`, "should match"},
		{"sysext url not https", `sysexts: [{name: x, arch: amd64, url: "http://example.com/x.raw", sha256: "0000000000000000000000000000000000000000000000000000000000000000"}]`, "should match"},
		{"sysext unknown arch", `sysexts: [{name: x, arch: riscv64, url: "https://example.com/x.raw", sha256: "0000000000000000000000000000000000000000000000000000000000000000"}]`, "Unsupported value"},
		{"butane without key", `butane: {configMapRef: {name: nm}}`, "Required value"},
		{"butane without configMapRef", `butane: {}`, "Required value"},
		// metrics
		{"metric with neither benchmark nor profile", `metrics: {m: {scale: 10}}`, "set exactly one of benchmark and workloadProfile"},
		{"metric with both", `metrics: {m: {workloadProfile: web, benchmark: {id: "geekbench:score"}}}`, "set exactly one of benchmark and workloadProfile"},
		{"metric name not a slug", `metrics: {Single_Core: {workloadProfile: web}}`, "metric names are lower-case label-safe slugs"},
		{"metric name too long", `metrics: {` + strings.Repeat("a", 41) + `: {workloadProfile: web}}`, "metric names are lower-case label-safe slugs"},
		{"metric unknown workload profile", `metrics: {m: {workloadProfile: gaming}}`, "Unsupported value"},
		{"metric normalize PerPrice", `metrics: {m: {workloadProfile: web, normalize: PerPrice}}`, "Unsupported value"},
		{"metric scale zero", `metrics: {m: {workloadProfile: web, scale: 0}}`, "should be greater than or equal to 1"},
		// selections
		{"selection without selector and ranking", `selections: {s: {}}`, "set selector, ranking or both"},
		{"selection name not a slug", `selections: {Fast_Cores: {selector: {vcpus: {min: 2}}}}`, "selection names are lower-case label-safe slugs"},
		{"selection ranking without band or families.max", `selections: {s: {ranking: {score: {workloadProfile: web}}}}`, "ranking needs band or families.max"},
		{"selection selector vcpus min above max", `selections: {s: {selector: {vcpus: {min: 8, max: 4}}}}`, "min must not exceed max"},
		// profiles
		{"storage profile named default", `profiles: {storage: {default: {aws: {}}}}`, "the profile name 'default' is reserved"},
		{"network profile named default", `profiles: {network: {default: {aws: {enaExpress: {enabled: true}}}}}`, "the profile name 'default' is reserved"},
		{"placement profile named default", `profiles: {placement: {default: {aws: {tenancy: dedicated}}}}`, "the profile name 'default' is reserved"},
		{"profile name not a slug", `profiles: {storage: {DB_IO2: {aws: {}}}}`, "profile names are lower-case label-safe slugs"},
		{"network profile name not a slug", `profiles: {network: {Fast_NIC: {aws: {enaExpress: {enabled: true}}}}}`, "profile names are lower-case label-safe slugs"},
		{"placement profile name not a slug", `profiles: {placement: {Colo_1: {aws: {tenancy: dedicated}}}}`, "profile names are lower-case label-safe slugs"},
		{"profile without vendor blocks", `profiles: {storage: {empty: {}}}`, "should have at least 1 properties"},
		{"profile with unknown vendor block", `profiles: {storage: {s: {hcloud: {}}}}`, "unknown field"},
		// vendor resources (use | create | forbid)
		{"resource with use and create", `vendors: {aws: {resources: {subnets: {use: {name: nm}, create: {}}}}}`, "at most one of use, create and forbid may be set"},
		{"resource with use and forbid", `vendors: {aws: {resources: {subnets: {use: {name: nm}, forbid: true}}}}`, "at most one of use, create and forbid may be set"},
		{"resource with create and forbid", `vendors: {aws: {resources: {securityGroups: {create: {}, forbid: true}}}}`, "at most one of use, create and forbid may be set"},
		{"resource with all three", `vendors: {aws: {resources: {placementGroups: {use: {name: nm}, create: {}, forbid: true}}}}`, "at most one of use, create and forbid may be set"},
		{"resource use without anything", `vendors: {aws: {resources: {subnets: {use: {}}}}}`, "set at least one of ids, name, tags, match, filters and expression"},
		{"resource use with only a select", `vendors: {aws: {resources: {subnets: {use: {select: all}}}}}`, "set at least one of ids"},
		{"resource use with the old selector", `vendors: {aws: {resources: {subnets: {use: {selector: {tags: {a: b}}}}}}}`, "unknown field"},
		{"resource use with empty tags", `vendors: {aws: {resources: {subnets: {use: {tags: {}}}}}}`, "should have at least 1 properties"},
		{"resource use with an empty match", `vendors: {aws: {resources: {subnets: {use: {match: {}}}}}}`, "match sets no attribute"},
		{"resource use with an unknown match attribute", `vendors: {aws: {resources: {subnets: {use: {match: {zoen: [a]}}}}}}`, "unknown field"},
		{"resource use with an empty ids key", `vendors: {aws: {resources: {subnets: {use: {ids: {"": [subnet-1]}}}}}}`, "ids keys are a region name or *"},
		{"resource use with no IDs in a region", `vendors: {aws: {resources: {subnets: {use: {ids: {us-east-1: []}}}}}}`, "should have at least 1 items"},
		{"resource use with a bad select", `vendors: {aws: {resources: {subnets: {use: {name: a, select: some}}}}}`, "Unsupported value"},
		{"resource use orderBy without select", `vendors: {aws: {resources: {subnets: {use: {name: a, orderBy: name}}}}}`, "orderBy is only valid with select newest or oldest"},
		{"resource use orderBy with select all", `vendors: {aws: {resources: {subnets: {use: {name: a, select: all, orderBy: name}}}}}`, "orderBy is only valid with select newest or oldest"},
		{"resource use with a bad orderBy", `vendors: {aws: {resources: {subnets: {use: {name: a, select: newest, orderBy: age}}}}}`, "orderBy is created, name or label:<key>"},
		{"resource use with an empty label", `vendors: {aws: {resources: {subnets: {use: {name: a, select: newest, orderBy: "label:"}}}}}`, "orderBy is created, name or label:<key>"},
		{"resource use cidr", `vendors: {aws: {resources: {subnets: {use: {match: {cidr: 10.0.0.0}}}}}}`, "cidr must be a CIDR"},
		{"resource use within", `vendors: {aws: {resources: {subnets: {use: {match: {within: nope}}}}}}`, "within must be a CIDR"},
		{"resource use createdAfter", `vendors: {aws: {resources: {subnets: {use: {match: {createdAfter: yesterday}}}}}}`, "createdAfter"},
		{"resource use filter without values", `vendors: {aws: {resources: {subnets: {use: {filters: [{name: state, values: []}]}}}}}`, "should have at least 1 items"},
		{"resource use filter without a name", `vendors: {aws: {resources: {subnets: {use: {filters: [{name: "", values: [a]}]}}}}}`, "should be at least 1 chars long"},
		{"resource use with a bad architecture", `vendors: {aws: {resources: {subnets: {use: {match: {architecture: [mips]}}}}}}`, "Unsupported value"},
		{"resource unknown kind", `vendors: {aws: {resources: {vpcs: {forbid: true}}}}`, "unknown field"},
		{"fake resource with use and create", `vendors: {fake: {resources: {networks: {use: {name: nm}, create: {}}}}}`, "at most one of use, create and forbid may be set"},
		{"qemu resource with use and forbid", `vendors: {qemu: {resources: {networks: {use: {name: nm}, forbid: true}}}}`, "at most one of use, create and forbid may be set"},
		{"lifecycle perUse with ttl", `vendors: {aws: {resources: {subnets: {create: {lifecycle: {policy: perUse, ttl: 1h}}}}}}`, "ttl is only valid with policy idleTTL"},
		{"lifecycle without policy", `vendors: {aws: {resources: {subnets: {create: {lifecycle: {ttl: 1h}}}}}}`, "Required value"},
		{"unknown lifecycle policy", `vendors: {aws: {resources: {subnets: {create: {lifecycle: {policy: forever}}}}}}`, "Unsupported value"},
		{"preCreate without regions", `vendors: {aws: {resources: {subnets: {create: {timing: {mode: preCreate}}}}}}`, "regions is required with mode preCreate"},
		{"onDemand with regions", `vendors: {aws: {resources: {subnets: {create: {timing: {mode: onDemand, regions: [r1]}}}}}}`, "regions is required with mode preCreate and invalid otherwise"},
		{"timing regions without mode (defaults to onDemand)", `vendors: {aws: {resources: {subnets: {create: {timing: {regions: [r1]}}}}}}`, "invalid otherwise"},
		// aws
		{"aws image owner not an account ID", `vendors: {aws: {imageOwner: "flatcar"}}`, "should match"},
		{"aws metadata tokens unknown", `vendors: {aws: {metadataOptions: {httpTokens: maybe}}}`, "Unsupported value"},
		{"aws metadata hop limit zero", `vendors: {aws: {metadataOptions: {httpPutResponseHopLimit: 0}}}`, "should be greater than or equal to 1"},
		{"aws root iops on gp2", `vendors: {aws: {storage: {root: {volumeType: gp2, iops: 3000}}}}`, "iops is only valid for gp3, io1 and io2"},
		{"aws root throughput on io2", `vendors: {aws: {storage: {root: {volumeType: io2, iops: 3000, throughput: 200}}}}`, "throughput is only valid for gp3"},
		{"aws root io2 without iops", `vendors: {aws: {storage: {root: {volumeType: io2}}}}`, "io1 and io2 need iops"},
		{"aws root io1 without iops", `vendors: {aws: {storage: {root: {volumeType: io1}}}}`, "io1 and io2 need iops"},
		{"aws root kms key without encryption", `vendors: {aws: {storage: {root: {encrypted: false, kmsKeyID: alias/x}}}}`, "kmsKeyID needs encrypted"},
		{"aws root unknown volume type", `vendors: {aws: {storage: {root: {volumeType: io3}}}}`, "Unsupported value"},
		{"aws root iops below 100", `vendors: {aws: {storage: {root: {volumeType: gp3, iops: 50}}}}`, "should be greater than or equal to 100"},
		{"aws data iops on st1", `vendors: {aws: {storage: {data: [{name: d, volumeType: st1, volumeSize: 500Gi, iops: 3000, mountPath: /d}]}}}`, "iops is only valid for gp3, io1 and io2"},
		{"aws data throughput on gp2", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp2, volumeSize: 10Gi, throughput: 200, mountPath: /d}]}}}`, "throughput is only valid for gp3"},
		{"aws data io1 without iops", `vendors: {aws: {storage: {data: [{name: d, volumeType: io1, volumeSize: 10Gi, mountPath: /d}]}}}`, "io1 and io2 need iops"},
		{"aws data kms key without encryption", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp3, volumeSize: 10Gi, encrypted: false, kmsKeyID: k, mountPath: /d}]}}}`, "kmsKeyID needs encrypted"},
		{"aws data without mountPath", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp3, volumeSize: 10Gi}]}}}`, "Required value"},
		{"aws data relative mountPath", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp3, volumeSize: 10Gi, mountPath: data}]}}}`, "should match"},
		{"aws data unknown filesystem", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp3, volumeSize: 10Gi, mountPath: /d, filesystem: ntfs}]}}}`, "Unsupported value"},
		{"aws data duplicate names", `vendors: {aws: {storage: {data: [{name: d, volumeType: gp3, volumeSize: 10Gi, mountPath: /d}, {name: d, volumeType: gp3, volumeSize: 10Gi, mountPath: /e}]}}}`, "Duplicate value"},
		{"aws data name not a slug", `vendors: {aws: {storage: {data: [{name: Data_1, volumeType: gp3, volumeSize: 10Gi, mountPath: /d}]}}}`, "should match"},
		{"aws local storage unknown policy", `vendors: {aws: {storage: {localStorage: {policy: raid5}}}}`, "Unsupported value"},
		{"aws profile iops on gp2", `profiles: {storage: {s: {aws: {root: {volumeType: gp2, iops: 3000}}}}}`, "iops is only valid for gp3, io1 and io2"},
		{"placement group with strategy and use", `profiles: {placement: {p: {aws: {placementGroup: {strategy: cluster, use: {name: pg}}}}}}`, "set exactly one of strategy and use"},
		{"placement group with neither", `profiles: {placement: {p: {aws: {placementGroup: {}}}}}`, "set exactly one of strategy and use"},
		{"placement group partitionCount with cluster", `profiles: {placement: {p: {aws: {placementGroup: {strategy: cluster, partitionCount: 3}}}}}`, "partitionCount is only valid with strategy partition"},
		{"placement group partitionCount with use", `profiles: {placement: {p: {aws: {placementGroup: {use: {name: pg}, partitionCount: 3}}}}}`, "partitionCount is only valid with strategy partition"},
		{"placement group partitionCount above 7", `profiles: {placement: {p: {aws: {placementGroup: {strategy: partition, partitionCount: 8}}}}}`, "should be less than or equal to 7"},
		{"placement unknown strategy", `profiles: {placement: {p: {aws: {placementGroup: {strategy: ring}}}}}`, "Unsupported value"},
		{"unknown tenancy", `profiles: {placement: {p: {aws: {tenancy: host}}}}`, "Unsupported value"},
		{"ena express without enabled", `profiles: {network: {n: {aws: {enaExpress: {udp: true}}}}}`, "Required value"},
		// images (plan 14 A1): use only, one image per launch
		{"images with create", `vendors: {aws: {resources: {images: {create: {}}}}}`, "images accepts use only"},
		{"images forbid", `vendors: {aws: {resources: {images: {forbid: true}}}}`, "images accepts use only"},
		{"images empty", `vendors: {aws: {resources: {images: {}}}}`, "images accepts use only"},
		{"images use and create", `vendors: {fake: {resources: {images: {use: {name: x}, create: {}}}}}`, "at most one of use, create and forbid"},
		{"images select all", `vendors: {aws: {resources: {images: {use: {name: x, select: all}}}}}`, "images cannot use select all"},
		{"images fake select all", `vendors: {fake: {resources: {images: {use: {name: x, select: all}}}}}`, "images cannot use select all"},
		{"images empty use", `vendors: {aws: {resources: {images: {use: {}}}}}`, "set at least one of ids, name, tags, match, filters and expression"},
		{"images orderBy without select", `vendors: {aws: {resources: {images: {use: {name: x, orderBy: created}}}}}`, "orderBy is only valid with select newest or oldest"},
		{"images match architecture unknown", `vendors: {aws: {resources: {images: {use: {match: {architecture: [riscv]}}}}}}`, "Unsupported value"},
		{"images unknown field", `vendors: {aws: {resources: {images: {use: {selector: {tags: {a: b}}}}}}}`, "unknown field"},
		// bootstrap hardening (plan 14 A7)
		{"unknown hardening", `bootstrap: {hardening: [no-pods]}`, "Unsupported value"},
		{"hardening twice", `bootstrap: {hardening: [metadata-from-pods, metadata-from-pods]}`, "Duplicate value"},
		// fake and qemu
		{"qemu unknown accelerator", `vendors: {qemu: {accelerator: hvf}}`, "Unsupported value"},
		{"qemu unknown disk format", `profiles: {storage: {s: {qemu: {root: {format: vmdk}}}}}`, "Unsupported value"},
		{"qemu unknown nic model", `profiles: {network: {n: {qemu: {model: rtl8139}}}}`, "Unsupported value"},
		{"fake unknown group strategy", `profiles: {placement: {p: {fake: {group: {strategy: ring}}}}}`, "Unsupported value"},
		{"fake data disk without mountPath", `vendors: {fake: {storage: {data: [{name: d, size: 1Gi}]}}}`, "Required value"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := create(t, parseObject(t, fmt.Sprintf("invalid-%d", i), withSpec(t, tc.spec)))
			if err == nil {
				t.Fatalf("object was accepted, want error containing %q", tc.wantErr)
			}
			// Unknown fields are a bad request under strict field validation.
			if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
				t.Fatalf("want an Invalid or BadRequest error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestMissingSpecFieldsAreRejected(t *testing.T) {
	for name, spec := range map[string]string{
		"no flatcar":    `{kubernetes: {version: v1.37.1}, network: {template: direct}, vendors: {aws: {}}}`,
		"no kubernetes": `{flatcar: {version: "4757.2.3"}, network: {template: direct}, vendors: {aws: {}}}`,
		"no network":    `{flatcar: {version: "4757.2.3"}, kubernetes: {version: v1.37.1}, vendors: {aws: {}}}`,
		"no vendors":    `{flatcar: {version: "4757.2.3"}, kubernetes: {version: v1.37.1}, network: {template: direct}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := create(t, parseObject(t, "missing-"+slug(name), spec))
			if err == nil || !strings.Contains(err.Error(), "Required value") {
				t.Fatalf("want a Required value error, got %v", err)
			}
		})
	}
}

func TestStatusIsWritableThroughTheSubresource(t *testing.T) {
	obj := parseObject(t, "status", baseSpec)
	if err := create(t, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := get(t, obj)
	got.Object["status"] = mustYAML(t, `
vendorNames: [aws]
conditions:
  - {type: Ready, status: "True", reason: Ready, message: ok, lastTransitionTime: "2026-01-01T00:00:00Z"}
resolved:
  kubernetes:
    amd64: {url: "https://example.com/k.raw", sha256: "0000000000000000000000000000000000000000000000000000000000000000"}
vendors:
  aws:
    hash: abc
    conditions:
      - {type: VendorReady, status: "True", reason: VendorReady, message: ok, lastTransitionTime: "2026-01-01T00:00:00Z"}
    images: [{region: us-east-1, arch: amd64, ref: ami-0123, ready: true}]
    resources: [{kind: subnets, region: us-east-1, id: subnet-0123, managed: true, lastUsed: "2026-01-01T00:00:00Z"}]
    quota: [{region: us-east-1, capacityType: on-demand, bucket: L-1216C47A, used: 64, limit: 64, reason: QuotaExhausted}]
`)
	if err := k8sClient.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("status update: %v", err)
	}
	typed := &v1alpha1.SCNodeClass{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), typed); err != nil {
		t.Fatal(err)
	}
	// Read the stored conditions directly: StatusConditions() initialises
	// missing ones to Unknown.
	if len(typed.Status.Conditions) != 1 || !typed.Status.Conditions[0].IsTrue() {
		t.Errorf("Ready condition not read back: %+v", typed.Status.Conditions)
	}
	if q := typed.Status.Vendors["aws"].Quota; len(q) != 1 || q[0].Limit != 64 {
		t.Errorf("quota not read back: %+v", q)
	}
}
