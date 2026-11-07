/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

func nodeClass(mutate func(*v1alpha1.SCNodeClass)) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors:    v1alpha1.VendorBlocks{AWS: &aws.Settings{}},
		},
		Status: v1alpha1.SCNodeClassStatus{Resolved: &v1alpha1.ResolvedArtifacts{
			Kubernetes: map[string]v1alpha1.Artifact{
				"amd64": {URL: k8sArtifact("amd64").URL, SHA256: k8sArtifact("amd64").SHA256},
				"arm64": {URL: k8sArtifact("arm64").URL, SHA256: k8sArtifact("arm64").SHA256},
			},
			Tailscale: map[string]v1alpha1.Artifact{
				"amd64": {URL: tsArtifact("amd64").URL, SHA256: tsArtifact("amd64").SHA256},
				"arm64": {URL: tsArtifact("arm64").URL, SHA256: tsArtifact("arm64").SHA256},
			},
		}},
	}
	if mutate != nil {
		mutate(nc)
	}
	return nc
}

func tailscaleClass(nc *v1alpha1.SCNodeClass) {
	nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
		Version: "v1.98.10", Tags: []string{"tag:k8s-node"},
		AuthKeySecretRef: v1alpha1.SecretKeyRef{Name: "ts-node-key", Namespace: "scarpenter", Key: "authkey"},
	}}
}

func kubeRefs(t *testing.T, objs ...client.Object) *bootstrap.KubeReferences {
	t.Helper()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	return &bootstrap.KubeReferences{Reader: kube, Namespace: "scarpenter"}
}

func rootCA() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "scarpenter"},
		Data: map[string]string{"ca.crt": testCA}}
}

func newClaim() *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-abc12", Labels: map[string]string{"karpenter.sh/nodepool": "default"}}}
	nc.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	nc.Spec.StartupTaints = []corev1.Taint{{Key: "example.com/warming", Effect: corev1.TaintEffectNoSchedule}}
	return nc
}

func TestRendererRendersTheNode(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ts-node-key", Namespace: "scarpenter"}, Data: map[string][]byte{"authkey": []byte(testAuthKey + "\n")}}
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{
		Refs: kubeRefs(t, rootCA(), secret), APIServerEndpoint: "https://api.my-cluster.example.com:6443",
		ClusterDNS: []string{"10.96.0.10"}, ClusterDomain: "cluster.local",
	})
	nc := nodeClass(tailscaleClass)
	userData, err := r.Render(t.Context(), bootstrap.RenderRequest{
		NodeClass: nc, NodeClaim: newClaim(), Vendor: "aws", Region: "us-east-1", Arch: "arm64",
		Token: testToken, NodeName: "default-abc12", ProviderID: "scarpenter://aws/us-east-1/default-abc12",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, parts := decode(t, userData)
	if len(parts) != 2 {
		t.Fatalf("%d parts, want base and network", len(parts))
	}
	// The files' contents are encoded in the Ignition config: read them as text.
	base, network := readable(t, parts[0]), readable(t, parts[1])
	for _, want := range []string{
		"kubernetes-v1.37.1-arm64.raw", // the arm64 image from the status, not amd64
		"--provider-id=scarpenter://aws/us-east-1/default-abc12",
		"--register-with-taints=karpenter.sh/unregistered:NoExecute,dedicated=gpu:NoSchedule,example.com/warming:NoSchedule",
		testToken, "clusterDNS", "10.96.0.10",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("the base config lacks %q", want)
		}
	}
	for _, want := range []string{"tailscale-v1.98.10-arm64.raw", testAuthKey, "--advertise-tags=tag:k8s-node"} {
		if !strings.Contains(network, want) {
			t.Errorf("the network config lacks %q", want)
		}
	}
	if strings.Contains(network, testAuthKey+"\\n") || strings.Contains(network, "x86-64") {
		t.Error("the auth key was not trimmed, or the wrong architecture was used")
	}
}

func TestRendererReadsTheConfigMaps(t *testing.T) {
	cms := []client.Object{
		rootCA(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "scarpenter"}, Data: map[string]string{"net.bu": "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/custom\n      mode: 0644\n      contents:\n        inline: |\n          {{ .NodeName }} {{ .Vendor }}\n"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "extras", Namespace: "scarpenter"}, Data: map[string]string{"config.bu": "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/extra\n      mode: 0644\n      contents:\n        inline: extra\n"}},
	}
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t, cms...), APIServerEndpoint: "https://api.example.com:6443"})
	nc := nodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateCustom, Custom: &v1alpha1.CustomNetworkSpec{
			ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "net", Namespace: "scarpenter", Key: "net.bu"}}}
		nc.Spec.Butane = &v1alpha1.ButaneSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "extras", Namespace: "scarpenter", Key: "config.bu"}}
		nc.Spec.PoweroffAfter = &metav1.Duration{Duration: 2 * time.Hour}
	})
	userData, err := r.Render(t.Context(), bootstrap.RenderRequest{
		NodeClass: nc, Vendor: "aws", Region: "us-east-1", Arch: "amd64", Token: testToken, NodeName: "n1", ProviderID: "scarpenter://aws/us-east-1/n1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, parts := decode(t, userData)
	if len(parts) != 3 {
		t.Fatalf("%d parts, want base, network and user", len(parts))
	}
	// Merge order: the user's configuration comes last and so wins.
	if !strings.Contains(readable(t, parts[1]), "n1 aws") || !strings.Contains(readable(t, parts[2]), "extra") {
		t.Errorf("network: %s\nuser: %s", parts[1], parts[2])
	}
	if !strings.Contains(string(parts[0]), "OnBootSec=7200s") {
		t.Error("the poweroff timer is not in the base config")
	}
	// Without a NodeClaim only the Karpenter taint is registered.
	if !strings.Contains(string(parts[0]), "--register-with-taints=karpenter.sh/unregistered:NoExecute ") {
		t.Error("the unregistered taint is not the only registered one")
	}
}

func TestRendererErrors(t *testing.T) {
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t, rootCA()), APIServerEndpoint: "https://api.example.com:6443"})
	req := func(nc *v1alpha1.SCNodeClass, arch string) bootstrap.RenderRequest {
		return bootstrap.RenderRequest{NodeClass: nc, Vendor: "aws", Region: "r", Arch: arch, Token: testToken, NodeName: "n1", ProviderID: "scarpenter://aws/r/n1"}
	}
	tests := []struct {
		name    string
		req     bootstrap.RenderRequest
		wantErr string
	}{
		{"artifacts not resolved", req(nodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Status.Resolved = nil }), "amd64"), "not resolved yet"},
		{"no sysext for the architecture", req(nodeClass(func(nc *v1alpha1.SCNodeClass) {
			delete(nc.Status.Resolved.Kubernetes, "arm64")
		}), "arm64"), "not resolved for arm64"},
		{"tailscale secret missing", req(nodeClass(tailscaleClass), "amd64"), "ts-node-key"},
		{"no NodeClass", req(nil, "amd64"), "no SCNodeClass"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Render(t.Context(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	// A missing Secret is ErrNotFound, which the NodeClass controller turns into
	// a reason of its own.
	_, err := r.Render(t.Context(), req(nodeClass(tailscaleClass), "amd64"))
	if !errors.Is(err, bootstrap.ErrNotFound) {
		t.Errorf("error %v is not ErrNotFound", err)
	}
	// No cluster CA anywhere.
	noCA := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t), APIServerEndpoint: "https://api.example.com:6443"})
	if _, err := noCA.Render(t.Context(), req(nodeClass(nil), "amd64")); err == nil || !strings.Contains(err.Error(), "kube-root-ca.crt") {
		t.Errorf("error = %v, want the CA to be named", err)
	}
}

func TestValidateRendersWithPlaceholders(t *testing.T) {
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t), APIServerEndpoint: "https://api.example.com:6443"})
	// The cluster CA is not needed: validation does not depend on it.
	for _, arch := range []string{"amd64", "arm64"} {
		if err := r.Validate(t.Context(), nodeClass(nil), arch, bootstrap.Inputs{}); err != nil {
			t.Errorf("Validate(%s): %v", arch, err)
		}
	}
	bad := nodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Status.Resolved = nil })
	if err := r.Validate(t.Context(), bad, "amd64", bootstrap.Inputs{}); err == nil {
		t.Error("Validate accepted a NodeClass without resolved artifacts")
	}
}

func TestClusterCASources(t *testing.T) {
	ctx := t.Context()
	// From the ConfigMap, cached.
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(rootCA()).Build()
	clk := clocktesting.NewFakeClock(time.Now())
	refs := &bootstrap.KubeReferences{Reader: kube, Namespace: "scarpenter", Clock: clk}
	got, err := refs.ClusterCA(ctx)
	if err != nil || !strings.HasPrefix(got, "-----BEGIN CERTIFICATE-----") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("ClusterCA = %q, %v", got, err)
	}
	if err := kube.Delete(ctx, rootCA()); err != nil {
		t.Fatal(err)
	}
	if again, err := refs.ClusterCA(ctx); err != nil || again != got {
		t.Errorf("the CA was not served from the cache: %q, %v", again, err)
	}
	clk.Step(10 * time.Minute)
	if _, err := refs.ClusterCA(ctx); !errors.Is(err, bootstrap.ErrNotFound) {
		t.Errorf("after the cache expired: %v, want ErrNotFound", err)
	}

	// From --cluster-ca-file.
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, []byte(testCA), 0o600); err != nil {
		t.Fatal(err)
	}
	withFile := &bootstrap.KubeReferences{CAFile: file}
	if got, err := withFile.ClusterCA(ctx); err != nil || !strings.Contains(got, "BEGIN CERTIFICATE") {
		t.Errorf("ClusterCA from a file = %q, %v", got, err)
	}
	if err := os.WriteFile(file, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := withFile.ClusterCA(ctx); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("a file that is no certificate: %v", err)
	}
	if _, err := (&bootstrap.KubeReferences{CAFile: file + ".missing"}).ClusterCA(ctx); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestLoadInputs(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ts-node-key", Namespace: "scarpenter", ResourceVersion: ""}, Data: map[string][]byte{"authkey": []byte("tskey-x")}}
	refs := kubeRefs(t, secret)
	in, err := bootstrap.LoadInputs(context.Background(), refs, nodeClass(tailscaleClass))
	if err != nil || in.AuthKey != "tskey-x" || in.AuthKeyVersion == "" {
		t.Fatalf("LoadInputs = %+v, %v", in, err)
	}
	// Another key of the Secret does not exist.
	nc := nodeClass(tailscaleClass)
	nc.Spec.Network.Tailscale.AuthKeySecretRef.Key = "other"
	if _, err := bootstrap.LoadInputs(context.Background(), refs, nc); !errors.Is(err, bootstrap.ErrNotFound) {
		t.Errorf("a missing key: %v, want ErrNotFound", err)
	}
	// A NodeClass that references nothing loads nothing, without reading.
	if in, err := bootstrap.LoadInputs(context.Background(), kubeRefs(t), nodeClass(nil)); err != nil || in != (bootstrap.Inputs{}) {
		t.Errorf("LoadInputs of a plain NodeClass = %+v, %v", in, err)
	}
}

// Validate measures with the real cluster CA: a NodeClass that only fits with a
// small CA must not pass validation and then fail every launch.
func TestValidateUsesTheRealClusterCA(t *testing.T) {
	small := bootstrap.NewRenderer(bootstrap.RendererOptions{
		Refs: kubeRefs(t, rootCA()), APIServerEndpoint: "https://api.example.com:6443"})
	if err := small.Validate(t.Context(), nodeClass(nil), "amd64", bootstrap.Inputs{}); err != nil {
		t.Fatal(err)
	}
	huge := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "scarpenter"},
		Data: map[string]string{"ca.crt": fakeCA(14 * 1024)}}
	big := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t, huge), APIServerEndpoint: "https://api.example.com:6443"})
	err := big.Validate(t.Context(), nodeClass(nil), "amd64", bootstrap.Inputs{})
	if err == nil || !strings.Contains(err.Error(), "over the budget") {
		t.Fatalf("a 14 KiB CA bundle passed validation: %v", err)
	}
}

// The bootstrap kubeconfig points at the CA file instead of embedding the CA a
// second time.
func TestKubeconfigReferencesTheCAFile(t *testing.T) {
	r, err := bootstrap.Render(baseParams("amd64", v1alpha1.NetworkTemplateDirect))
	if err != nil {
		t.Fatal(err)
	}
	base := readable(t, r.Parts[0].Ignition)
	if !strings.Contains(base, "certificate-authority: /etc/kubernetes/pki/ca.crt") || strings.Contains(base, "certificate-authority-data") {
		t.Errorf("the kubeconfig does not reference the CA file")
	}
}

// A Secret the controller may not read says which RoleBinding is missing.
func TestForbiddenSecretNamesTheRoleBinding(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "ts-node-key", errors.New("no"))
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return forbidden
		},
	}).Build()
	refs := &bootstrap.KubeReferences{Reader: kube}
	_, err := bootstrap.LoadInputs(t.Context(), refs, nodeClass(tailscaleClass))
	if err == nil || !strings.Contains(err.Error(), "RoleBinding") || !strings.Contains(err.Error(), "namespace scarpenter") {
		t.Fatalf("error = %v", err)
	}
}

// The first-boot volume unit adds to the user-data only for nodes that have
// volumes, so a plain trial rendering misses it: Validate also renders the largest
// storage setup of each vendor (plan 06 review).
func TestValidateRendersTheLargestStorageSetup(t *testing.T) {
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{
		Refs: kubeRefs(t, rootCA()), APIServerEndpoint: "https://api.example.com:6443"})
	data := make([]aws.DataVolume, 0, 8)
	for i := range 8 {
		data = append(data, aws.DataVolume{GuestVolume: common.GuestVolume{
			Name: fmt.Sprintf("volume-%d", i), Filesystem: common.FilesystemXFS, MountPath: fmt.Sprintf("/var/lib/data/volume-%d", i),
		}})
	}
	withProfile := func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{
			"small": {AWS: &aws.Storage{Data: data[:1]}},
			"big": {AWS: &aws.Storage{
				Data: data, LocalStorage: &common.LocalStorage{Policy: common.LocalStorageMount, MountPath: "/mnt/local"},
			}},
		}
	}
	// A user Butane that makes the plain rendering fit with a little room, found
	// by growing it: random-looking so that it compresses as badly as real
	// content.
	blob := func(n int) string {
		var b strings.Builder
		sum := sha256.Sum256([]byte("blob"))
		for b.Len() < n {
			b.WriteString(hex.EncodeToString(sum[:]))
			sum = sha256.Sum256(sum[:])
		}
		return "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/blob\n      contents:\n        inline: " + b.String()[:n] + "\n"
	}
	nc := nodeClass(nil)
	profileOnly, profiled := nodeClass(withProfile), nodeClass(withProfile)
	found := false
	for n := 8000; n < 20000; n += 250 {
		in := bootstrap.Inputs{UserButane: blob(n)}
		if err := r.Validate(t.Context(), nc, "amd64", in); err != nil {
			break
		}
		if err := r.Validate(t.Context(), profiled, "amd64", in); err != nil {
			if !strings.Contains(err.Error(), "storage setup of aws (8 volumes and local storage)") || !strings.Contains(err.Error(), "over the budget") {
				t.Fatalf("the error does not say what was too large: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no user Butane size fits without volumes and overflows with the largest storage profile: the volume unit is not part of the trial rendering")
	}
	// A profile alone never makes a NodeClass without profiles fail.
	if err := r.Validate(t.Context(), profileOnly, "amd64", bootstrap.Inputs{}); err != nil {
		t.Errorf("a NodeClass with a large profile and no other content: %v", err)
	}
}

func dnsService(name string, labels map[string]string, ips ...string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec:       corev1.ServiceSpec{ClusterIP: ips[0], ClusterIPs: ips},
	}
}

func TestClusterDNSFromTheService(t *testing.T) {
	label := map[string]string{"k8s-app": "kube-dns"}
	cases := map[string]struct {
		objs    []client.Object
		want    []string
		wantErr string
	}{
		"labelled, any name": {[]client.Object{dnsService("coredns", label, "10.43.0.10")}, []string{"10.43.0.10"}, ""},
		"dual stack":         {[]client.Object{dnsService("kube-dns", label, "10.96.0.10", "fd00::a")}, []string{"10.96.0.10", "fd00::a"}, ""},
		"kube-dns wins among labelled": {[]client.Object{
			dnsService("kube-dns-upstream", label, "10.96.0.11"), dnsService("kube-dns", label, "10.96.0.10"), dnsService("a-cache", label, "10.96.0.12")}, []string{"10.96.0.10"}, ""},
		"by name when none is named kube-dns": {[]client.Object{
			dnsService("zz-dns", label, "10.96.0.3"), dnsService("coredns", label, "10.96.0.2")}, []string{"10.96.0.2"}, ""},
		"headless labelled falls back to the name": {[]client.Object{
			dnsService("coredns", label, corev1.ClusterIPNone), dnsService("kube-dns", nil, "10.96.0.10")}, []string{"10.96.0.10"}, ""},
		"named, unlabelled": {[]client.Object{dnsService("kube-dns", nil, "10.96.0.10")}, []string{"10.96.0.10"}, ""},
		"label wins over the name": {[]client.Object{
			dnsService("kube-dns", nil, "10.96.0.99"), dnsService("rke2-coredns", label, "10.43.0.10")}, []string{"10.43.0.10"}, ""},
		"another namespace is not looked at": {[]client.Object{&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "other", Labels: label},
			Spec:       corev1.ServiceSpec{ClusterIP: "10.1.1.1"}}}, nil, "no cluster DNS Service found"},
		"headless":  {[]client.Object{dnsService("kube-dns", label, corev1.ClusterIPNone)}, nil, "no cluster DNS Service found"},
		"not found": {nil, nil, "set --cluster-dns"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(c.objs...).Build()
			got, err := (&bootstrap.KubeReferences{Reader: kube}).ClusterDNS(t.Context())
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("got %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

func TestRendererClusterDNSSources(t *testing.T) {
	lookupCalls := 0
	lookup := func(context.Context) ([]string, error) { lookupCalls++; return []string{"10.43.0.10"}, nil }
	failing := func(context.Context) ([]string, error) { return nil, errors.New("no DNS Service") }
	render := func(opts bootstrap.RendererOptions) (string, error) {
		opts.APIServerEndpoint = "https://api.example.com:6443"
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ts-node-key", Namespace: "scarpenter"}, Data: map[string][]byte{"authkey": []byte(testAuthKey)}}
		opts.Refs = kubeRefs(t, rootCA(), secret)
		out, err := bootstrap.NewRenderer(opts).Render(t.Context(), bootstrap.RenderRequest{
			NodeClass: nodeClass(tailscaleClass), NodeClaim: newClaim(), Vendor: "aws", Region: "us-east-1", Arch: "amd64",
			Token: testToken, NodeName: "n", ProviderID: "scarpenter://aws/us-east-1/n",
		})
		if err != nil {
			return "", err
		}
		_, parts := decode(t, out)
		return readable(t, parts[0]), nil
	}

	explicit, err := render(bootstrap.RendererOptions{ClusterDNS: []string{"10.96.0.10"}, ClusterDNSLookup: lookup})
	if err != nil || !strings.Contains(explicit, "10.96.0.10") || lookupCalls != 0 {
		t.Errorf("an explicit value must win without a lookup: err %v, lookups %d", err, lookupCalls)
	}
	found, err := render(bootstrap.RendererOptions{ClusterDNSLookup: lookup})
	if err != nil || !strings.Contains(found, "10.43.0.10") || lookupCalls != 1 {
		t.Errorf("the found Service must be rendered: err %v, lookups %d", err, lookupCalls)
	}
	none, err := render(bootstrap.RendererOptions{})
	if err != nil || strings.Contains(none, "clusterDNS") {
		t.Errorf("without a lookup (--cluster-dns=none) clusterDNS stays unset: err %v", err)
	}
	_, err = render(bootstrap.RendererOptions{ClusterDNSLookup: failing})
	if _, ok := errors.AsType[*bootstrap.ClusterDNSError](err); !ok {
		t.Errorf("a DNS Service that is not found must fail the rendering with a ClusterDNSError, got %v", err)
	}
}
