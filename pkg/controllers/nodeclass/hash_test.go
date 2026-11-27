/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
)

func base() *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{
		Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
		Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
		Vendors: v1alpha1.VendorBlocks{
			AWS:  &aws.Settings{Regions: []string{"us-east-1"}, InstanceProfile: "a"},
			Fake: &fake.Settings{},
		},
	}}
}

func hash(t *testing.T, nc *v1alpha1.SCNodeClass, vendor string) string {
	t.Helper()
	h, err := nodeclass.Hash(nc, vendor, nodeclass.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashDependsOnWhatChangesTheNode(t *testing.T) {
	ref := hash(t, base(), "aws")
	if again := hash(t, base(), "aws"); again != ref {
		t.Fatalf("the hash is not deterministic: %s != %s", again, ref)
	}
	for name, change := range map[string]func(*v1alpha1.SCNodeClass){
		"flatcar version":     func(nc *v1alpha1.SCNodeClass) { nc.Spec.Flatcar.Version = "4757.2.4" },
		"kubernetes version":  func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubernetes.Version = "v1.37.2" },
		"kubelet":             func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.MaxPods = ptr.To[int32](50) },
		"kubelet footprint":   func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.Footprint = v1alpha1.FootprintLean },
		"poweroff":            func(nc *v1alpha1.SCNodeClass) { nc.Spec.PoweroffAfter = &metav1.Duration{Duration: 3600} },
		"tags":                func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"a": "b"} },
		"the vendor's block":  func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.InstanceProfile = "b" },
		"networking override": func(nc *v1alpha1.SCNodeClass) { nc.Spec.Networking.PublicIPv4 = new(false) },
	} {
		nc := base()
		change(nc)
		if got := hash(t, nc, "aws"); got == ref {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
}

// TestHashKeepsNodeClassesWithoutFootprint pins what the hash reads of the
// kubelet block: a NodeClass that does not set the footprint hashes the same as
// before the field existed, so that adding it drifts no node.
func TestHashKeepsNodeClassesWithoutFootprint(t *testing.T) {
	raw, err := json.Marshal(base().Spec.Kubelet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "footprint") {
		t.Errorf("an unset footprint appears in the hashed kubelet block: %s", raw)
	}
	// maxPods lost its CRD default: an unset maxPods hashes as the 110 that
	// NodeClasses stored before carry.
	stored, unset := base(), base()
	stored.Spec.Kubelet.MaxPods = ptr.To[int32](110)
	unset.Spec.Kubelet.MaxPods = nil
	if hash(t, stored, "aws") != hash(t, unset, "aws") {
		t.Error("an unset maxPods hashes differently from the stored default 110")
	}
}

func TestHashIgnoresWhatDoesNotChangeTheNode(t *testing.T) {
	ref := hash(t, base(), "aws")
	for name, change := range map[string]func(*v1alpha1.SCNodeClass){
		"regions":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"eu-west-1"} },
		"another vendor": func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.Fake = nil },
		"metrics":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Metrics = map[string]v1alpha1.Metric{"a": {}} },
		"selections":     func(nc *v1alpha1.SCNodeClass) { nc.Spec.Selections = map[string]v1alpha1.Selection{"a": {}} },
		"the billing retention": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Billing = &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true}
		},
		"the launch policy": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Launch = &v1alpha1.LaunchSpec{MaxAttempts: new(int32(10)), MaxDuration: &metav1.Duration{Duration: time.Minute}}
		},
		"the catalog settings": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Catalog = &v1alpha1.CatalogSpec{CPUFlagLabels: []string{"avx2"}}
		},
	} {
		nc := base()
		change(nc)
		if got := hash(t, nc, "aws"); got != ref {
			t.Errorf("changing the %s changes the hash", name)
		}
	}
}

func TestHashIsPerVendor(t *testing.T) {
	nc := base()
	if hash(t, nc, "aws") == hash(t, nc, "fake") {
		t.Error("two vendors share a hash")
	}
}

// The hash covers what the spec points at, not only the spec: a changed custom
// template, Butane snippet or auth key Secret, or a re-published image, drifts
// the nodes (overview section 6.3).
func TestHashCoversTheResolvedInputs(t *testing.T) {
	nc := base()
	hashWith := func(in nodeclass.Inputs) string {
		t.Helper()
		h, err := nodeclass.Hash(nc, "aws", in)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	resolved := func(sum string) *v1alpha1.ResolvedArtifacts {
		return &v1alpha1.ResolvedArtifacts{Kubernetes: map[string]v1alpha1.Artifact{"amd64": {URL: "https://example.com/k.raw", SHA256: sum}}}
	}
	ref := hashWith(nodeclass.Inputs{Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"})
	if again := hashWith(nodeclass.Inputs{Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"}); again != ref {
		t.Fatal("the hash is not deterministic")
	}
	for name, in := range map[string]nodeclass.Inputs{
		"image digest":    {Resolved: resolved("bb"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"},
		"custom template": {Resolved: resolved("aa"), CustomTemplate: "changed", UserButane: "b", AuthKeyVersion: "1"},
		"butane":          {Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "changed", AuthKeyVersion: "1"},
		"auth key secret": {Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "2"},
	} {
		if hashWith(in) == ref {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
}

// The deprecated spec.bootstrap.hardening stays in the hash as it was, so that a
// stored NodeClass that sets it does not drift when SCarpenter is upgraded
// (ADR-025). The values are the hashes the code before the deprecation computed
// for this NodeClass: for the vendors whose drivers applied the hardening it is
// part of the hash, for the others it is not.
func TestDeprecatedHardeningKeepsTheOldHash(t *testing.T) {
	nc := base()
	// maxPods is 110 as on every stored NodeClass: the CRD defaulted it before the
	// Lean footprint, and the pins were computed with it set.
	nc.Spec.Kubelet.MaxPods = ptr.To[int32](110)
	nc.Spec.Bootstrap = &v1alpha1.BootstrapSpec{Hardening: []v1alpha1.Hardening{v1alpha1.HardeningMetadataFromPods}}
	const (
		wantFake = "e053ab65f7a94843c1ff2fdfc965dfb079c56675bc6afd84b06c755ff951e94e"
		wantAWS  = "b310824b670685c5120dd548c125c7f57792106140f6488b273a3a74332f9af5"
	)
	if got := hash(t, nc, "fake"); got != wantFake {
		t.Errorf("fake hash = %s, want %s", got, wantFake)
	}
	if got := hash(t, nc, "aws"); got != wantAWS {
		t.Errorf("aws hash = %s, want %s", got, wantAWS)
	}
	plain := base()
	plain.Spec.Kubelet.MaxPods = ptr.To[int32](110)
	if hash(t, nc, "aws") != hash(t, plain, "aws") {
		t.Error("a vendor that never applied the hardening drifts")
	}
}

// TestHashIsPinned guards against drift by accident: the node IP hint of the
// direct template derives from the resolved resources of a launch, not from the
// spec, so a change of the rendering that does not bump HashVersion must leave
// the hash of an existing NodeClass as it is. If this fails, either the hash
// changed by mistake or HashVersion was bumped on purpose (and every node drifts
// once): then update the value here.
func TestHashIsPinned(t *testing.T) {
	const want = "b310824b670685c5120dd548c125c7f57792106140f6488b273a3a74332f9af5"
	if got := hash(t, base(), "aws"); got != want {
		t.Errorf("hash of the base NodeClass = %s, want %s", got, want)
	}
}

// TestHashOfTheExamplesIsPinned pins the hash of every NodeClass of docs/examples
// per vendor, with the inputs left empty: an API change that does not set a new
// field must not drift the nodes of a stored NodeClass. Update the golden file only
// when HashVersion is bumped on purpose.
func TestHashOfTheExamplesIsPinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "docs", "examples", "*.yaml"))
	if err != nil || len(files) < 4 {
		t.Fatalf("expected the examples under docs/examples, got %v (err %v)", files, err)
	}
	var out strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // the repository's own examples
		if err != nil {
			t.Fatal(err)
		}
		nc := &v1alpha1.SCNodeClass{}
		if err := yaml.UnmarshalStrict(data, nc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, vendor := range nc.Spec.Vendors.Names() {
			fmt.Fprintf(&out, "%s %s %s\n", filepath.Base(f), vendor, hash(t, nc, vendor))
		}
	}
	testutil.Golden(t, "example-hashes.txt", []byte(out.String()))
}

func tailscaleBase() *v1alpha1.SCNodeClass {
	nc := base()
	nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
		Version:          "v1.98.10",
		AuthKeySecretRef: &v1alpha1.SecretKeyRef{Name: "ts", Namespace: "scarpenter", Key: "authkey"},
		Tags:             []string{"tag:node"},
	}}
	return nc
}

// TestHashTreatsStaticAsUnset: writing the default keySource drifts nothing, and
// switching to PerNode drifts the nodes once.
func TestHashTreatsStaticAsUnset(t *testing.T) {
	unset, static := tailscaleBase(), tailscaleBase()
	static.Spec.Network.Tailscale.KeySource = v1alpha1.TailnetKeyStatic
	if hash(t, unset, "aws") != hash(t, static, "aws") {
		t.Error("an explicit keySource Static changes the hash")
	}
	if static.Spec.Network.Tailscale.KeySource != v1alpha1.TailnetKeyStatic {
		t.Error("hashing changed the NodeClass")
	}
	perNode := tailscaleBase()
	perNode.Spec.Network.Tailscale.KeySource = v1alpha1.TailnetKeyPerNode
	perNode.Spec.Network.Tailscale.AuthKeySecretRef = nil
	if hash(t, unset, "aws") == hash(t, perNode, "aws") {
		t.Error("switching to keySource PerNode does not change the hash")
	}
}

// storedNodeClasses are NodeClasses as stored before keySource existed, with the
// inputs the controller hashes with them: a tailscale template with every field
// and the resourceVersion of its Secret, one with only the required fields, and
// the direct template.
var storedNodeClasses = []struct {
	name, spec string
	in         nodeclass.Inputs
}{
	{"tailscale-full", `network: {template: tailscale, tailscale: {version: v1.98.10, loginServer: "https://hs.example.com",
  authKeySecretRef: {name: ts, namespace: scarpenter, key: authkey}, tags: ["tag:k8s-node", "tag:bench"], acceptRoutes: true}}`,
		nodeclass.Inputs{AuthKeyVersion: "4711"}},
	{"tailscale-minimal", `network: {template: tailscale, tailscale: {version: v1.98.10, authKeySecretRef: {name: ts, namespace: scarpenter, key: authkey}}}`,
		nodeclass.Inputs{AuthKeyVersion: "1"}},
	{"direct", `network: {template: direct}`, nodeclass.Inputs{}},
}

// TestHashOfStoredNodeClassesIsPinned pins the hash of the NodeClasses above, per
// vendor: the golden values come from the code before keySource.
func TestHashOfStoredNodeClassesIsPinned(t *testing.T) {
	var out strings.Builder
	for _, tc := range storedNodeClasses {
		nc := &v1alpha1.SCNodeClass{}
		doc := "spec: {flatcar: {version: \"4757.2.3\"}, kubernetes: {version: v1.37.1}, vendors: {aws: {regions: [us-east-1]}, hcloud: {regions: [fsn1]}}, " +
			tc.spec + "}"
		if err := yaml.UnmarshalStrict([]byte(doc), nc); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, vendor := range nc.Spec.Vendors.Names() {
			h, err := nodeclass.Hash(nc, vendor, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "%s %s %s\n", tc.name, vendor, h)
		}
	}
	testutil.Golden(t, "stored-hashes.txt", []byte(out.String()))
}

// TestRootVolumeChangesTheHashOnlyWhenSet: an unset rootVolume hashes as before
// (the pins above), a set one replaces the nodes.
func TestRootVolumeChangesTheHashOnlyWhenSet(t *testing.T) {
	set := base()
	set.Spec.RootVolume = &v1alpha1.RootVolumeSpec{MemoryMultiplier: resource.MustParse("1")}
	if hash(t, set, "aws") == hash(t, base(), "aws") {
		t.Error("setting rootVolume does not change the hash")
	}
	same := base()
	same.Spec.RootVolume = &v1alpha1.RootVolumeSpec{MemoryMultiplier: resource.MustParse("1000m")}
	if hash(t, set, "aws") != hash(t, same, "aws") {
		t.Error("1 and 1000m hash differently")
	}
	other := base()
	other.Spec.RootVolume = &v1alpha1.RootVolumeSpec{MemoryMultiplier: resource.MustParse("2")}
	if hash(t, set, "aws") == hash(t, other, "aws") {
		t.Error("another multiplier hashes the same")
	}
}
