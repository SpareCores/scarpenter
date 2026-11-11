/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// alwaysOn is a fragment a vendor's image needs on every node (the shape of the
// GCP arm64 udev rule), with a template variable.
const alwaysOn = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /etc/udev/rules.d/65-vendor-disk.rules
      mode: 0644
      contents:
        inline: |
          # {{ .Vendor }} {{ .Arch }}
          KERNEL=="nvme*", ENV{ID_SERIAL_SHORT}=="?*", SYMLINK+="disk/by-id/vendor-$env{ID_SERIAL_SHORT}"
`

func fragments(t *testing.T) []bootstrap.Fragment {
	t.Helper()
	meta, err := vendors.BlockMetadataFromPods("169.254.169.254")
	if err != nil {
		t.Fatal(err)
	}
	return []bootstrap.Fragment{{Name: "udev", Butane: alwaysOn}, {Name: meta.Name, Butane: meta.Butane}}
}

// TestGoldenVendorFragments pins the Ignition of a node with an always-on vendor
// fragment and the metadata hardening, merged after the base and before the
// network and the user's config; the base config itself is unchanged.
func TestGoldenVendorFragments(t *testing.T) {
	p := baseParams("arm64", v1alpha1.NetworkTemplateDirect)
	p.Vendor = "example"
	p.UserSource = "ConfigMap example/extras key config.bu"
	p.UserButane = "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/motd\n      mode: 0644\n      contents:\n        inline: hi\n"
	p.VendorFragments = fragments(t)
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(r.Parts))
	for _, part := range r.Parts {
		names = append(names, part.Name)
	}
	if got, want := strings.Join(names, ","), "base,vendor-udev,vendor-block-metadata-from-pods,network,user"; got != want {
		t.Errorf("parts = %s, want %s", got, want)
	}
	testutil.Golden(t, "render-vendor-fragments.golden", goldenText(t, r))

	// Without fragments the rendering is what it was: the base config does not
	// change.
	p.VendorFragments = nil
	plain, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for i, part := range plain.Parts {
		if part.Name == "base" && string(part.Ignition) != string(r.Parts[0].Ignition) {
			t.Error("the base config differs with fragments")
		}
		_ = i
	}
	if len(plain.Parts) != 3 {
		t.Errorf("%d parts without fragments, want base, network and user", len(plain.Parts))
	}
}

// TestFragmentsStayWithinTheBudgetOfTheDefaultTemplates renders the largest default
// template (tailscale) with the hardening, under the budget that leaves room for
// custom snippets.
func TestFragmentsStayWithinTheBudgetOfTheDefaultTemplates(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		p := baseParams(arch, v1alpha1.NetworkTemplateTailscale)
		p.VendorFragments = fragments(t)
		r, err := bootstrap.Render(p)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
			t.Errorf("%s: %d bytes with the fragments, the budget of the default templates is %d", arch, n, bootstrap.DefaultBudgetBytes)
		}
	}
}

func TestFragmentErrors(t *testing.T) {
	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	for name, tc := range map[string]struct {
		f    bootstrap.Fragment
		want string
	}{
		"no name":          {bootstrap.Fragment{Butane: alwaysOn}, "has no name"},
		"not butane":       {bootstrap.Fragment{Name: "bad", Butane: "variant: fcos\nversion: 1.5.0\n"}, "vendor fragment bad"},
		"unknown variable": {bootstrap.Fragment{Name: "var", Butane: "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /x\n      contents:\n        inline: \"{{ .Nope }}\"\n"}, "vendor fragment var"},
		"over the budget":  {bootstrap.Fragment{Name: "big", Butane: bigFragment(t)}, "over the budget"},
	} {
		p.VendorFragments = []bootstrap.Fragment{tc.f}
		if _, err := bootstrap.Render(p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func bigFragment(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 10*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n    - path: /etc/big\n      mode: 0644\n      contents:\n        inline: " + hex.EncodeToString(raw) + "\n"
}

// TestValidateRendersTheFragmentsOfEachVendor: the trial rendering of the
// NodeClass controller includes the fragments of each vendor, so that one that
// does not translate or does not fit is found before the first launch.
func TestValidateRendersTheFragmentsOfEachVendor(t *testing.T) {
	r := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: kubeRefs(t), APIServerEndpoint: "https://api.example.com:6443"})
	nc := nodeClass(nil)
	for name, tc := range map[string]struct {
		fn      bootstrap.FragmentsFunc
		wantErr string
	}{
		"fits": {func(vendor, arch string) ([]bootstrap.Fragment, error) { return fragments(t), nil }, ""},
		"too big": {func(vendor, arch string) ([]bootstrap.Fragment, error) {
			return []bootstrap.Fragment{{Name: "big", Butane: bigFragment(t)}}, nil
		}, "with the boot fragments of aws"},
		"the driver fails": {func(vendor, arch string) ([]bootstrap.Fragment, error) {
			return nil, errBoom
		}, "boot fragments of aws"},
		"none": {func(vendor, arch string) ([]bootstrap.Fragment, error) { return nil, nil }, ""},
	} {
		err := r.ValidateWith(t.Context(), nc, "amd64", bootstrap.Inputs{}, tc.fn)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: %v, want %q", name, err, tc.wantErr)
		}
	}
}

type boom struct{}

func (boom) Error() string { return "boom" }

var errBoom error = boom{}

func TestBlockMetadataFragment(t *testing.T) {
	if _, err := vendors.BlockMetadataFromPods("not-an-address"); err == nil {
		t.Error("a bad address was accepted")
	}
	if _, err := vendors.BlockMetadataFromPods("fd00:ec2::254"); err == nil {
		t.Error("an IPv6 address was accepted by the IPv4 fragment")
	}
	f, err := vendors.BlockMetadataFromPods("100.100.100.200")
	if err != nil || !strings.Contains(f.Butane, "-d 100.100.100.200 -j DROP") || strings.Contains(f.Butane, "ADDRESS") {
		t.Errorf("fragment = %+v, %v", f, err)
	}
}
