/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func baseRequest() vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: "ap-southeast-1", Zone: "ap-southeast-1a", InstanceType: "ecs.g8y.large",
		CapacityType: vendors.CapacityOnDemand, Arch: "arm64",
		Image:    vendors.Image{Ref: "m-flatcar-arm64", RootDiskBytes: 9 << 30},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: "default-abc12", vendors.TagNodePool: "default",
			vendors.TagNodeClass: "default", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "11111111-2222-3333-4444-555555555555",
	}
}

func baseResources() LaunchResources {
	return LaunchResources{VSwitchID: "vsw-1", SecurityGroupIDs: []string{"sg-1"}}
}

func golden(t *testing.T, name string, in any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(in); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("%s differs from the request (go test -update to rewrite):\n%s", name, buf.String())
	}
}

func TestRunInstancesDefaultsGolden(t *testing.T) {
	in, err := BuildRunInstances(baseRequest(), baseResources(), 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "runinstances_defaults.golden.json", in)
}

func TestRunInstancesEverythingGolden(t *testing.T) {
	req := baseRequest()
	req.CapacityType = vendors.CapacitySpot
	req.ExtraTags = map[string]string{"team": "data", "env": "ci"}
	req.Settings = vendors.VendorSettings{
		Raw: json.RawMessage(`{
			"storage": {
				"root": {"category": "cloud_auto", "size": "100Gi", "encrypted": true, "kmsKeyID": "key-1",
				         "provisionedIops": 5000, "burstingEnabled": true},
				"data": [
					{"name": "d", "category": "cloud_auto", "size": "200Gi", "provisionedIops": 6000, "burstingEnabled": false,
					 "filesystem": "xfs", "mountPath": "/var/lib/d"},
					{"name": "e", "category": "cloud_essd", "performanceLevel": "PL1", "size": "50Gi", "encrypted": true}
				]
			},
			"internetChargeType": "PayByBandwidth",
			"metadataOptions": {"httpTokens": "optional"},
			"resourceGroupId": "rg-1"
		}`),
		Network:    json.RawMessage(`{"internetMaxBandwidthOut": 50}`),
		Networking: common.Networking{PublicIPv4: new(true)},
	}
	lr := LaunchResources{VSwitchID: "vsw-1", SecurityGroupIDs: []string{"sg-1", "sg-run"}, DeploymentSetID: "ds-1"}
	in, err := BuildRunInstances(req, lr, 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "runinstances_everything.golden.json", in)
}

// The fields the plan lists are asserted by name as well, so a golden update
// cannot silently drop one.
func TestRunInstancesFields(t *testing.T) {
	req := baseRequest()
	in, err := BuildRunInstances(req, baseResources(), 0)
	if err != nil {
		t.Fatal(err)
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	checks := map[string][2]string{
		"RegionId": {str(in.RegionId), "ap-southeast-1"}, "ZoneId": {str(in.ZoneId), "ap-southeast-1a"},
		"ImageId": {str(in.ImageId), "m-flatcar-arm64"}, "InstanceType": {str(in.InstanceType), "ecs.g8y.large"},
		"VSwitchId": {str(in.VSwitchId), "vsw-1"}, "InstanceChargeType": {str(in.InstanceChargeType), "PostPaid"},
		"InstanceName": {str(in.InstanceName), "default-abc12"}, "HostName": {str(in.HostName), "default-abc12"},
		"HttpTokens": {str(in.HttpTokens), "optional"}, "HttpEndpoint": {str(in.HttpEndpoint), "enabled"},
		"SystemDisk.Category": {str(in.SystemDisk.Category), "cloud_essd"}, "SystemDisk.Size": {str(in.SystemDisk.Size), "64"},
		"SystemDisk.PerformanceLevel": {str(in.SystemDisk.PerformanceLevel), "PL0"},
		"InternetChargeType":          {str(in.InternetChargeType), "PayByTraffic"},
		"ClientToken": {str(in.ClientToken), ClientToken(req.IdempotencyKey, req.Region, req.Zone, req.InstanceType,
			req.CapacityType, "m-flatcar-arm64", "vsw-1", 0)},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if *in.Amount != 1 || *in.MinAmount != 1 || *in.InternetMaxBandwidthOut != 100 {
		t.Errorf("amount %d/%d, bandwidth %d", *in.Amount, *in.MinAmount, *in.InternetMaxBandwidthOut)
	}
	if in.SystemDisk.Encrypted != nil {
		t.Errorf("the default root disk is not encrypted, got Encrypted=%q", *in.SystemDisk.Encrypted)
	}
	if got, _ := base64.StdEncoding.DecodeString(str(in.UserData)); string(got) != string(req.UserData) {
		t.Errorf("user data = %q", got)
	}
	if in.SpotStrategy != nil || in.SpotDuration != nil || in.DeploymentSetId != nil || in.ResourceGroupId != nil {
		t.Error("on-demand without a deployment set or resource group sets none of them")
	}
	// The tags: five logical tags under the scarpenter key, none of the plan's
	// "Name" tag, in a fixed order.
	keys := make([]string, 0, len(in.Tag))
	for _, tg := range in.Tag {
		keys = append(keys, *tg.Key+"="+*tg.Value)
	}
	want := []string{
		"scarpenter.sparecores.com/cluster=my-cluster", "scarpenter.sparecores.com/managed-by=scarpenter",
		"scarpenter.sparecores.com/nodeclaim=default-abc12", "scarpenter.sparecores.com/nodeclass=default",
		"scarpenter.sparecores.com/nodepool=default",
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("tags = %v", keys)
	}
}

func TestRunInstancesSpotAndPublicIP(t *testing.T) {
	req := baseRequest()
	req.CapacityType = vendors.CapacitySpot
	req.Settings.Networking = common.Networking{PublicIPv4: new(false)}
	in, err := BuildRunInstances(req, baseResources(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if *in.SpotStrategy != "SpotAsPriceGo" || *in.SpotDuration != 0 || *in.SpotInterruptionBehavior != "Terminate" {
		t.Errorf("spot = %s/%d/%s", *in.SpotStrategy, *in.SpotDuration, *in.SpotInterruptionBehavior)
	}
	if *in.InternetMaxBandwidthOut != 0 {
		t.Errorf("publicIPv4 false must be bandwidth 0, got %d", *in.InternetMaxBandwidthOut)
	}
	// internetMaxBandwidthOut: 0 means the same.
	req = baseRequest()
	req.Settings.Raw = json.RawMessage(`{"internetMaxBandwidthOut": 0}`)
	in, err = BuildRunInstances(req, baseResources(), 0)
	if err != nil || *in.InternetMaxBandwidthOut != 0 {
		t.Fatalf("bandwidth 0: %v", err)
	}
}

func TestRunInstancesTokenDependsOnAttemptAndDropsWithoutKey(t *testing.T) {
	a, _ := BuildRunInstances(baseRequest(), baseResources(), 0)
	b, _ := BuildRunInstances(baseRequest(), baseResources(), 1)
	if *a.ClientToken == *b.ClientToken {
		t.Fatal("the retry token must be salted")
	}
	req := baseRequest()
	req.IdempotencyKey = ""
	c, err := BuildRunInstances(req, baseResources(), 0)
	if err != nil || c.ClientToken != nil {
		t.Fatalf("without a key there is no token: %v", err)
	}
}

func TestRunInstancesRefusals(t *testing.T) {
	tags := func(n int) map[string]string {
		m := map[string]string{}
		for i := range n {
			m[fmt.Sprintf("k%02d", i)] = "v"
		}
		return m
	}
	cases := []struct {
		name   string
		mutate func(*vendors.CreateRequest, *LaunchResources)
		class  vendors.Class
		code   string
	}{
		{"user data over the budget", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.UserData = bytes.Repeat([]byte("x"), MaxUserData+1)
		}, vendors.Fatal, codeUserDataTooLarge},
		{"no image", func(r *vendors.CreateRequest, _ *LaunchResources) { r.Image.Ref = "" }, vendors.Fatal, codeImageNotFound},
		{"no vSwitch", func(_ *vendors.CreateRequest, l *LaunchResources) { l.VSwitchID = "" }, vendors.ZoneUnusable, codeNoVSwitch},
		{"no group", func(_ *vendors.CreateRequest, l *LaunchResources) { l.SecurityGroupIDs = nil }, vendors.Fatal, codeInvalidSettings},
		{"no node claim name", func(r *vendors.CreateRequest, _ *LaunchResources) { delete(r.Tags, vendors.TagNodeClaim) }, vendors.Fatal, codeInvalidSettings},
		{"publicIPv6", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Networking.PublicIPv6 = new(true)
		}, vendors.Fatal, codeInvalidSettings},
		{"root disk smaller than the image", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Raw = json.RawMessage(`{"storage":{"root":{"size":"20Gi"}}}`)
			r.Image.RootDiskBytes = 30 << 30
		}, vendors.Fatal, codeInvalidSettings},
		{"root disk below 20 GiB", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Raw = json.RawMessage(`{"storage":{"root":{"size":"10Gi"}}}`)
		}, vendors.Fatal, codeInvalidSettings},
		{"root disk above 2048 GiB", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Raw = json.RawMessage(`{"storage":{"root":{"size":"3Ti"}}}`)
		}, vendors.Fatal, codeInvalidSettings},
		{"bandwidth above 100", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Raw = json.RawMessage(`{"internetMaxBandwidthOut": 200}`)
		}, vendors.Fatal, codeInvalidSettings},
		{"too many tags", func(r *vendors.CreateRequest, _ *LaunchResources) { r.ExtraTags = tags(16) }, vendors.Fatal, codeInvalidSettings},
		{"broken settings", func(r *vendors.CreateRequest, _ *LaunchResources) {
			r.Settings.Raw = json.RawMessage(`{"storage": 3}`)
		}, vendors.Fatal, codeInvalidSettings},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, lr := baseRequest(), baseResources()
			c.mutate(&req, &lr)
			_, err := BuildRunInstances(req, lr, 0)
			if err == nil {
				t.Fatal("expected an error")
			}
			if vendors.ClassOf(err) != c.class || vendors.CodeOf(err) != c.code {
				t.Fatalf("got %s (%s): %v", vendors.ClassOf(err), vendors.CodeOf(err), err)
			}
		})
	}
	// Exactly 15 NodeClass tags plus the five logical ones is the limit of 20.
	req := baseRequest()
	req.ExtraTags = tags(15)
	if _, err := BuildRunInstances(req, baseResources(), 0); err != nil {
		t.Fatalf("20 tags are allowed: %v", err)
	}
}

func TestSettingsResolution(t *testing.T) {
	// The network profile lays over the vendor block.
	st, err := decodeSettings(vendors.VendorSettings{
		Raw:     json.RawMessage(`{"internetMaxBandwidthOut": 10, "internetChargeType": "PayByBandwidth"}`),
		Network: json.RawMessage(`{"internetMaxBandwidthOut": 25}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mbit, charge := bandwidth(st, nil)
	if mbit != 25 || charge != "PayByBandwidth" {
		t.Errorf("bandwidth = %d %s", mbit, charge)
	}
	if RootDiskBytes(vendors.VendorSettings{}) != 64<<30 {
		t.Error("the default root disk is 64 GiB")
	}
	if got := RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"size":"100Gi"}}}`)}); got != 100<<30 {
		t.Errorf("root disk = %d", got)
	}
	if RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{`)}) != 64<<30 {
		t.Error("broken settings fall back to the default root disk")
	}
	// A non-ESSD category drops the default performance level.
	d := rootDisk(Settings{Storage: &Storage{Root: &Disk{Category: CategoryAuto}}})
	if d.Category != CategoryAuto || d.Level != "" {
		t.Errorf("cloud_auto root = %+v", d)
	}
	// Prepared resources: the first vSwitch, deduplicated groups, the set.
	lr := resourcesOf(&vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: KindSubnets, ID: "vsw-1"}, {Kind: KindSubnets, ID: "vsw-2"},
		{Kind: KindSecurityGroups, ID: "sg-1"}, {Kind: KindRunSecurityGroups, ID: "sg-2"}, {Kind: KindSecurityGroups, ID: "sg-1"},
		{Kind: KindDeploymentSets, ID: "ds-1"},
	}})
	if lr.VSwitchID != "vsw-1" || strings.Join(lr.SecurityGroupIDs, ",") != "sg-1,sg-2" || lr.DeploymentSetID != "ds-1" {
		t.Errorf("resources = %+v", lr)
	}
	if got := resourcesOf(nil); got.VSwitchID != "" {
		t.Errorf("nil prepared = %+v", got)
	}
}
