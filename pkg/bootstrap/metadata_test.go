/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

// TestMetadataFragmentOfEachVendor pins the Butane that the core builds from each
// driver's declaration of its metadata addresses and allowed units, and renders it
// into a node's user-data (Butane translates it, the size stays in the budget).
func TestMetadataFragmentOfEachVendor(t *testing.T) {
	for name, d := range map[string]vendors.Driver{
		"alicloud": &alicloud.Driver{}, "aws": &aws.Driver{}, "azure": &azure.Driver{}, "gcp": &gcp.Driver{},
		"hcloud": &hcloud.Driver{}, "oci": &oci.Driver{}, "ovh": &ovh.Driver{}, "upcloud": &upcloud.Driver{},
		"vultr": &vultr.Driver{},
	} {
		for mode, restrict := range map[string]func(vendors.Driver) (vendors.BootFragment, bool, error){
			"": vendors.RestrictMetadata, "-hostonly": vendors.HostOnlyMetadata,
		} {
			t.Run(name+mode, func(t *testing.T) {
				f, ok, err := restrict(d)
				if err != nil || !ok {
					t.Fatalf("restricting the metadata = %v, %v", ok, err)
				}
				testutil.Golden(t, "metadata-"+name+mode+".golden", []byte(f.Butane))

				p := baseParams("amd64", v1alpha1.NetworkTemplateTailscale)
				p.VendorFragments = []bootstrap.Fragment{{Name: f.Name, Butane: f.Butane}}
				r, err := bootstrap.Render(p)
				if err != nil {
					t.Fatal(err)
				}
				if n := len(r.UserData); n > bootstrap.DefaultBudgetBytes {
					t.Errorf("%d bytes with the fragment, the budget of the default templates is %d", n, bootstrap.DefaultBudgetBytes)
				}
				var ignition string
				for _, part := range r.Parts {
					if part.Name == "vendor-"+f.Name {
						ignition = string(part.Ignition)
					}
				}
				for _, want := range []string{"scarpenter-metadata.slice", "scarpenter-metadata-firewall.service",
					"coreos-metadata.service", "coreos-metadata-sshkeys@.service", "15-scarpenter-metadata.conf"} {
					if !strings.Contains(ignition, want) {
						t.Errorf("the Ignition of the fragment lacks %s", want)
					}
				}
			})
		}
	}
}

// TestAlicloudTailnetFragment pins the Butane of the Alibaba Cloud tailnet fragment
// (tailscaled's CGNAT drop must not apply to the 100.100.0.0/16 services) and
// renders it into the user-data of a tailscale node, whose unit passes the flag.
func TestAlicloudTailnetFragment(t *testing.T) {
	frags := (&alicloud.Driver{}).BootFragments(true)
	if len(frags) != 1 {
		t.Fatalf("%d fragments, want 1", len(frags))
	}
	testutil.Golden(t, "tailnet-alicloud.golden", []byte(frags[0].Butane))

	p := baseParams("amd64", v1alpha1.NetworkTemplateTailscale)
	p.VendorFragments = []bootstrap.Fragment{{Name: frags[0].Name, Butane: frags[0].Butane}}
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	var vendor, network bool
	for _, part := range r.Parts {
		vendor = vendor || (part.Name == "vendor-tailnet" && strings.Contains(string(part.Ignition), "20-alicloud-netfilter.conf"))
		network = network || (part.Name == "network" && strings.Contains(string(part.Ignition), "SCARPENTER_TAILSCALE_UP_ARGS"))
	}
	if !vendor || !network {
		t.Errorf("vendor drop-in present: %v, the tailscale unit passes the variable: %v", vendor, network)
	}
}
