/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

func TestDefaults(t *testing.T) {
	e := newEnv(t, envOptions{})
	d := e.d.Defaults()
	var m map[string]any
	if err := json.Unmarshal(d.Raw, &m); err != nil {
		t.Fatal(err)
	}
	root := m["storage"].(map[string]any)["root"].(map[string]any)
	if root["volumeType"] != "gp3" || root["volumeSize"] != "64Gi" || root["encrypted"] != true {
		t.Errorf("root volume default %v, want gp3, 64Gi, encrypted", root)
	}
	if d.RootDiskBytes != 64<<30 || e.d.RootDiskBytes(d) != 64<<30 {
		t.Errorf("root disk %d, want 64 GiB", d.RootDiskBytes)
	}
	// The NodeClass's block overrides the root volume key by key; a profile too.
	s := resolveSettings(t, e.d, `{"storage":{"root":{"volumeSize":"100Gi"}}}`, defaultNetworking(), nil, options.Selection{})
	if s.RootDiskBytes != 100<<30 {
		t.Errorf("root disk %d, want the block's 100 GiB", s.RootDiskBytes)
	}
	s = resolveSettings(t, e.d, "", defaultNetworking(),
		vendors.Profiles{vendors.ProfileStorage: {"big": json.RawMessage(`{"root":{"volumeSize":"200Gi","volumeType":"gp2"}}`)}},
		options.Selection{Storage: "big"})
	if s.RootDiskBytes != 200<<30 {
		t.Errorf("root disk %d, want the profile's 200 GiB", s.RootDiskBytes)
	}
	if got := e.d.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{`)}); got != 64<<30 {
		t.Errorf("undecodable settings: %d, want the default", got)
	}
}

func TestSupported(t *testing.T) {
	e := newEnv(t, envOptions{types: snapshotTypes(t)})
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {
			"local":    json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt"}}`),
			"ignore":   json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
			"ebs":      json.RawMessage(`{"data":[{"name":"d","volumeType":"gp3","volumeSize":"10Gi","mountPath":"/d"}]}`),
			"blockexp": json.RawMessage(`{"root":{"volumeType":"io2","volumeSize":"500Gi","iops":100000}}`),
			"broken":   json.RawMessage(`{"data":"nope"}`),
		},
		vendors.ProfileNetwork: {
			"ena": json.RawMessage(`{"enaExpress":{"enabled":true}}`), "off": json.RawMessage(`{"enaExpress":{"enabled":false}}`),
		},
		vendors.ProfilePlacement: {
			"cluster": json.RawMessage(`{"placementGroup":{"strategy":"cluster"}}`),
			"spread":  json.RawMessage(`{"placementGroup":{"strategy":"spread"}}`),
			"part":    json.RawMessage(`{"placementGroup":{"strategy":"partition","partitionCount":3}}`),
			"named":   json.RawMessage(`{"placementGroup":{"use":{"name":"x"}}}`),
			"dedic":   json.RawMessage(`{"tenancy":"dedicated"}`),
		},
	}
	typ := func(name string) vendors.CatalogType { return vendors.CatalogType{Vendor: "aws", APIReference: name} }

	// Until the instance type table is read, what depends on it is not offered
	// (nothing is degraded silently); what does not depend on it is.
	before := e.d.Supported(typ("c7gn.large"), testZone, profiles)
	if contains(before[vendors.ProfileNetwork], "ena") || contains(before[vendors.ProfilePlacement], "cluster") ||
		contains(before[vendors.ProfileStorage], "local") {
		t.Errorf("before the table is read: %v", before)
	}
	if !contains(before[vendors.ProfileNetwork], "off") || !contains(before[vendors.ProfilePlacement], "dedic") ||
		!contains(before[vendors.ProfilePlacement], "named") || !contains(before[vendors.ProfileStorage], "ebs") {
		t.Errorf("before the table is read, the profiles that need nothing are missing: %v", before)
	}
	epoch := e.d.NodeOptionsEpoch()
	warmTypes(t, e)
	if e.d.NodeOptionsEpoch() == epoch {
		t.Error("the epoch did not change when the type table arrived: the catalog would not rebuild")
	}

	for _, tc := range []struct {
		typ       string
		storage   []string
		network   []string
		placement []string
	}{
		// a Nitro type with ENA Express (c7gn), no instance store
		{"c7gn.large", []string{"blockexp", "ebs", "ignore"}, []string{"ena", "off"}, []string{"cluster", "dedic", "named", "part", "spread"}},
		// instance store, no ENA Express
		{"m6id.large", []string{"blockexp", "ebs", "ignore", "local"}, []string{"off"}, []string{"cluster", "dedic", "named", "part", "spread"}},
		// burstable: no cluster placement groups
		{"t3.micro", []string{"blockexp", "ebs", "ignore"}, []string{"off"}, []string{"dedic", "named", "part", "spread"}},
		// not Nitro: no io2 Block Express
		{"m3.large", []string{"ebs", "ignore"}, []string{"off"}, []string{"cluster", "dedic", "named", "part", "spread"}},
	} {
		got := e.d.Supported(typ(tc.typ), testZone, profiles)
		check := func(kind string, want []string) {
			if !equalStrings(got[kind], want) {
				t.Errorf("%s %s: %v, want %v", tc.typ, kind, got[kind], want)
			}
		}
		check(vendors.ProfileStorage, tc.storage)
		check(vendors.ProfileNetwork, tc.network)
		check(vendors.ProfilePlacement, tc.placement)
	}
	// A type the table does not know offers only what needs nothing.
	got := e.d.Supported(typ("zz9.large"), testZone, profiles)
	if contains(got[vendors.ProfileNetwork], "ena") || contains(got[vendors.ProfileStorage], "local") {
		t.Errorf("an unknown type: %v", got)
	}
	if empty := e.d.Supported(typ("m7i.large"), testZone, vendors.Profiles{}); len(empty) != 0 {
		t.Errorf("no profiles: %v", empty)
	}
}

func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPriceDelta(t *testing.T) {
	prices := aws.StaticPrices{
		Storage: map[string]float64{"us-east-1/gp3": 0.08, "us-east-1/gp2": 0.10, "us-east-1/io2": 0.125},
		IPv4:    map[string]float64{"us-east-1": 0.005},
	}
	e := newEnv(t, envOptions{prices: prices})
	off := vendors.Offering{Region: testRegion, Zone: testZone, CapacityType: vendors.CapacityOnDemand, APIReference: testType}
	hour := float64(730)
	for _, tc := range []struct {
		name       string
		block      string
		networking common.Networking
		placement  string
		profiles   vendors.Profiles
		sel        options.Selection
		want       float64
		complete   bool
		region     string
	}{
		{name: "defaults: a 64 GiB gp3 volume and the public IPv4", want: 64*0.08/hour + 0.005, complete: true},
		{name: "no public IPv4", networking: common.Networking{PublicIPv4: new(false)}, want: 64 * 0.08 / hour, complete: true},
		{name: "a bigger root", block: `{"storage":{"root":{"volumeSize":"100Gi"}}}`, want: 100*0.08/hour + 0.005, complete: true},
		{name: "another volume type", block: `{"storage":{"root":{"volumeType":"gp2"}}}`, want: 64*0.10/hour + 0.005, complete: true},
		{name: "gp3 iops within what is included", block: `{"storage":{"root":{"iops":3000,"throughput":125}}}`, want: 64*0.08/hour + 0.005, complete: true},
		{name: "provisioned gp3 iops", block: `{"storage":{"root":{"iops":6000}}}`, want: 64*0.08/hour + 0.005, complete: false},
		{name: "provisioned gp3 throughput", block: `{"storage":{"root":{"throughput":250}}}`, want: 64*0.08/hour + 0.005, complete: false},
		{name: "io2 iops are not priced", block: `{"storage":{"root":{"volumeType":"io2","iops":1000}}}`, want: 64*0.125/hour + 0.005, complete: false},
		{
			name: "data volumes", profiles: vendors.Profiles{vendors.ProfileStorage: {"db": json.RawMessage(
				`{"data":[{"name":"a","volumeType":"gp3","volumeSize":"100Gi","mountPath":"/a"},{"name":"b","volumeType":"gp2","volumeSize":"50Gi","mountPath":"/b"}]}`)}},
			sel: options.Selection{Storage: "db"}, want: (64*0.08+100*0.08+50*0.10)/hour + 0.005, complete: true,
		},
		{
			name: "dedicated tenancy is not priced", profiles: vendors.Profiles{vendors.ProfilePlacement: {"d": json.RawMessage(`{"tenancy":"dedicated"}`)}},
			sel: options.Selection{Placement: "d"}, want: 64*0.08/hour + 0.005, complete: false,
		},
		{name: "a volume type without a price", block: `{"storage":{"root":{"volumeType":"st1","volumeSize":"125Gi"}}}`, want: 0.005, complete: false},
		{name: "a region without prices", region: "eu-central-1", want: 0, complete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := tc.networking
			if net.Isolation == "" {
				net = options.ResolveNetworking(net, nil)
			}
			s := resolveSettings(t, e.d, tc.block, net, tc.profiles, tc.sel)
			o := off
			if tc.region != "" {
				o.Region = tc.region
			}
			got, complete := e.d.PriceDelta(s, o)
			if math.Abs(got-tc.want) > 1e-9 || complete != tc.complete {
				t.Errorf("PriceDelta = %.9f, %v; want %.9f, %v", got, complete, tc.want, tc.complete)
			}
		})
	}
	// Without any price source nothing is complete.
	e2 := newEnv(t, envOptions{})
	if got, complete := e2.d.PriceDelta(e2.d.Defaults(), off); complete {
		t.Errorf("no prices: %v, %v", got, complete)
	}
}
