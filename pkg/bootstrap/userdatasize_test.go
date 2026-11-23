/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// worstCaseParams is the largest realistic user-data of a default template: a
// node IP range, two volumes, 24 local disks with one mount each, the Lean
// footprint and the metadata restriction.
func worstCaseParams(t *testing.T, template v1alpha1.NetworkTemplateName) bootstrap.Params {
	t.Helper()
	f, ok, err := vendors.RestrictMetadata(&hcloud.Driver{})
	if err != nil || !ok {
		t.Fatalf("RestrictMetadata = %v, %v", ok, err)
	}
	p := baseParams("amd64", template)
	p.NodeIPCIDR = "10.0.0.0/16"
	p.Volumes = []bootstrap.VolumeHint{
		{Name: "data", Filesystem: "ext4", MountPath: "/data", SerialPattern: "vendor-disk-serial-*", SizeBytes: 16 << 40},
		{Name: "data2", Filesystem: "xfs", MountPath: "/data2", SerialPattern: "vendor-disk-serial-*", SizeBytes: 16 << 40},
	}
	p.Local = &bootstrap.LocalHint{
		Policy: "mount", MountPath: "/mnt/local", DevicePattern: "nvme-Amazon_EC2_NVMe_Instance_Storage_*", Count: 24,
	}
	p.Kubelet.Footprint = v1alpha1.FootprintLean
	p.VendorFragments = []bootstrap.Fragment{{Name: f.Name, Butane: f.Butane}}
	return p
}

// The default templates with everything switched on stay within the budget of
// the default templates (12 KiB, 4 KiB under the limit of every vendor), so that
// a user's Butane has room. Scripts are embedded as percent-encoded text without
// their comments, which gzip compresses best (base64 inside the gzip is 30 % bigger).
func TestWorstCaseUserDataStaysInTheBudget(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateDirect, v1alpha1.NetworkTemplateTailscale} {
		t.Run(string(template), func(t *testing.T) {
			r, err := bootstrap.Render(worstCaseParams(t, template))
			if err != nil {
				t.Fatal(err)
			}
			if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
				t.Errorf("the worst case is %d bytes, the budget of the default templates is %d", n, bootstrap.DefaultBudgetBytes)
			} else {
				t.Logf("%s worst case: %d bytes of user-data", template, n)
			}
			for _, part := range r.Parts {
				if strings.Contains(string(part.Ignition), "data:;base64,") {
					t.Errorf("the %s part holds base64 text, which compresses badly", part.Name)
				}
			}
		})
	}
}

// spotWorstCaseParams is worstCaseParams of a spot node: the fragments the core
// builds for a vendor with a notice check (the agent inside the metadata
// restriction's fragment).
func spotWorstCaseParams(t *testing.T, template v1alpha1.NetworkTemplateName) bootstrap.Params {
	t.Helper()
	frags, err := vendors.BootFragmentsOf(noticeDriver{&hcloud.Driver{}}, true, true)
	if err != nil || len(frags) != 1 {
		t.Fatalf("BootFragmentsOf = %v, %v", frags, err)
	}
	p := worstCaseParams(t, template)
	p.Spot = true
	p.VendorFragments = []bootstrap.Fragment{{Name: frags[0].Name, Butane: frags[0].Butane}}
	return p
}

// A spot node adds the interruption agent: the worst case with it stays within the
// budget plus the agent's allowance.
func TestWorstCaseSpotUserDataStaysInTheBudget(t *testing.T) {
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateDirect, v1alpha1.NetworkTemplateTailscale} {
		t.Run(string(template), func(t *testing.T) {
			r, err := bootstrap.Render(spotWorstCaseParams(t, template))
			if err != nil {
				t.Fatal(err)
			}
			if n, limit := len(r.UserData), bootstrap.DefaultBudgetBytes+bootstrap.SpotAgentBytes; n > limit {
				t.Errorf("the spot worst case is %d bytes, the budget with the agent is %d", n, limit)
			} else {
				t.Logf("%s spot worst case: %d bytes of user-data", template, n)
			}
		})
	}
}

// Validate sizes its trial rendering with a node IP range, because a vendor with
// a private network renders the node IP unit: a NodeClass that only fits without
// it is rejected at validation, not when a node launches.
func TestValidateCountsTheNodeIPUnit(t *testing.T) {
	// A cluster CA of a size at which the direct template fits only without the unit.
	// (the middle of the sizes that do, the trial rendering has a few more bytes).
	var fits []string
	for n := 4096; n < 16*1024; n += 64 {
		plain := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
		plain.ClusterCA = fakeCA(n)
		ranged := plain
		ranged.NodeIPCIDR = "10.0.0.0/16"
		_, errPlain := bootstrap.Render(plain)
		_, errRanged := bootstrap.Render(ranged)
		if errPlain == nil && errRanged != nil {
			fits = append(fits, plain.ClusterCA)
		}
	}
	if len(fits) == 0 {
		t.Fatal("no CA size separates the direct template with and without a node IP range")
	}
	ca := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "scarpenter"},
		Data: map[string]string{"ca.crt": fits[len(fits)/2]}}
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{
		Refs: kubeRefs(t, ca), APIServerEndpoint: "https://api.example.com:6443"})
	err := r.Validate(t.Context(), nodeClass(nil), "amd64", bootstrap.Inputs{})
	if err == nil || !strings.Contains(err.Error(), "over the budget") || !strings.Contains(err.Error(), "node IP range") {
		t.Fatalf("Validate = %v, want the budget error that names the node IP range", err)
	}
}

// Alibaba Cloud takes 32 KB of user-data before Base64, twice the budget: its spot
// worst case (its tailnet fragment, the restriction and the agent with its own
// check) is held to the same budget as the other vendors, with room to spare.
func TestAlicloudSpotWorstCaseIsFarUnderItsLimit(t *testing.T) {
	const alicloudLimit = 32 * 1000
	frags, err := vendors.BootFragmentsOf(alicloud.New(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range []v1alpha1.NetworkTemplateName{v1alpha1.NetworkTemplateDirect, v1alpha1.NetworkTemplateTailscale} {
		t.Run(string(template), func(t *testing.T) {
			p := spotWorstCaseParams(t, template)
			p.VendorFragments = nil
			for _, f := range frags {
				p.VendorFragments = append(p.VendorFragments, bootstrap.Fragment{Name: f.Name, Butane: f.Butane})
			}
			r, err := bootstrap.Render(p)
			if err != nil {
				t.Fatal(err)
			}
			n := len(r.UserData)
			if limit := bootstrap.DefaultBudgetBytes + bootstrap.SpotAgentBytes; n > limit {
				t.Errorf("the Alibaba spot worst case is %d bytes, over the budget %d", n, limit)
			}
			t.Logf("%s Alibaba spot worst case: %d bytes of user-data, limit %d", template, n, alicloudLimit)
		})
	}
}
