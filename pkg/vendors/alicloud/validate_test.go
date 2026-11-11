/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

func q(s string) *resource.Quantity { v := resource.MustParse(s); return &v }

func ncWith(b *api.Settings) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: v1alpha1.SCNodeClassSpec{
		Vendors: v1alpha1.VendorBlocks{Alicloud: b}}}
}

func TestValidateNodeClass(t *testing.T) {
	d := New()
	use := func(r common.ResourceUse) *common.Resource { return &common.Resource{Use: &r} }
	cases := map[string]struct {
		nc   *v1alpha1.SCNodeClass
		want string // "" valid
	}{
		"empty block": {ncWith(&api.Settings{}), ""},
		"bad region":  {ncWith(&api.Settings{Regions: []string{"Mars"}}), "not an Alibaba Cloud region"},
		"tokens required": {ncWith(&api.Settings{MetadataOptions: &api.MetadataOptions{HTTPTokens: "required"}}),
			"hardened mode"},
		"tokens optional": {ncWith(&api.Settings{MetadataOptions: &api.MetadataOptions{HTTPTokens: "optional"}}), ""},
		"bandwidth":       {ncWith(&api.Settings{InternetMaxBandwidthOut: new(int32(500))}), "0 to 100"},
		"small root":      {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{Size: q("10Gi")}}}), "20 to 2048"},
		"big root":        {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{Size: q("3Ti")}}}), "20 to 2048"},
		"PL1 minimum": {ncWith(&api.Settings{Storage: &api.Storage{Data: []api.DataDisk{{
			GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}, Size: *q("10Gi"), PerformanceLevel: "PL2"}}}}), "461"},
		"iops on essd": {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{ProvisionedIops: new(int64(100))}}}),
			"only valid for cloud_auto"},
		"iops range": {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{Category: "cloud_auto", Size: q("40Gi"),
			ProvisionedIops: new(int64(49999))}}}), "provisionedIops of a 40 GiB"},
		"unknown category": {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{Category: "tape"}}}), "unknown disk category"},
		"kms without encryption": {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{KMSKeyID: "k"}}}),
			"kmsKeyID needs encrypted"},
		"level on auto": {ncWith(&api.Settings{Storage: &api.Storage{Root: &api.Disk{Category: "cloud_auto", PerformanceLevel: "PL1"}}}),
			"only valid for cloud_essd"},
		"duplicate names": {ncWith(&api.Settings{Storage: &api.Storage{Data: []api.DataDisk{
			{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}, Size: *q("20Gi")},
			{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/b"}, Size: *q("20Gi")}}}}), "used twice"},
		"local storage": {ncWith(&api.Settings{Storage: &api.Storage{LocalStorage: &common.LocalStorage{Policy: "mount", MountPath: "/m"}}}),
			"not supported on Alibaba Cloud yet"},
		"ipv6 inbound": {ncWith(&api.Settings{Networking: &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{
			{Protocol: "tcp", CIDRs: []string{"2001:db8::/32", "bogus"}}}}}}), "IPv6"},
		"images ids": {ncWith(&api.Settings{Resources: &api.Resources{Images: use(common.ResourceUse{
			IDs: map[string]common.IDList{"mars": {"bogus"}}})}}), "not a region"},
		"vswitch id": {ncWith(&api.Settings{Resources: &api.Resources{Subnets: use(common.ResourceUse{
			IDs: map[string]common.IDList{"ap-southeast-1": {"subnet-1"}}})}}), "not a valid ID"},
		"filter twice": {ncWith(&api.Settings{Resources: &api.Resources{Images: use(common.ResourceUse{Filters: []common.ResourceFilter{
			{Name: "ImageFamily", Values: []string{"a*"}}, {Name: "ImageFamily", Values: []string{"b"}}}})}}), "given twice"},
		"filter wildcard": {ncWith(&api.Settings{Resources: &api.Resources{SecurityGroups: use(common.ResourceUse{Filters: []common.ResourceFilter{
			{Name: "SecurityGroupType", Values: []string{"n*"}}}})}}), "without wildcards"},
		"reserved tag in use": {ncWith(&api.Settings{Resources: &api.Resources{Subnets: use(common.ResourceUse{
			Tags: map[string]string{"aliyun-x": "1"}})}}), "reserved prefix"},
		"owner": {ncWith(&api.Settings{Resources: &api.Resources{Images: use(common.ResourceUse{
			Match: &common.ResourceMatch{Owner: []string{"someone"}}})}}), "not an account UID"},
		"owner ok": {ncWith(&api.Settings{Resources: &api.Resources{Images: use(common.ResourceUse{
			Tags: map[string]string{"x": "*"}, Match: &common.ResourceMatch{Owner: []string{"123", "self"}}})}}), ""},
		"deployment set id": {ncWith(&api.Settings{Resources: &api.Resources{DeploymentSets: use(common.ResourceUse{
			IDs: map[string]common.IDList{"*": {"ds-abc12345"}}})}}), ""},
		"public ipv6": {func() *v1alpha1.SCNodeClass {
			n := ncWith(&api.Settings{})
			n.Spec.Networking.PublicIPv6 = new(true)
			return n
		}(), "publicIPv6: true is refused"},
		"no public address": {ncWith(&api.Settings{InternetMaxBandwidthOut: new(int32(0))}), "no egress"},
		"no public address with own vswitches": {ncWith(&api.Settings{InternetMaxBandwidthOut: new(int32(0)), Resources: &api.Resources{
			Subnets: use(common.ResourceUse{Name: "x"})}}), ""},
		"too many tags": {func() *v1alpha1.SCNodeClass {
			n := ncWith(&api.Settings{})
			n.Spec.Tags = map[string]string{}
			for i := range 16 {
				n.Spec.Tags[strings.Repeat("k", i+1)] = "v"
			}
			return n
		}(), "at most 15"},
		"bad tags": {func() *v1alpha1.SCNodeClass {
			n := ncWith(&api.Settings{})
			n.Spec.Tags = map[string]string{"acs:x": "1", "http://x": "1", " lead": "1", "ok": strings.Repeat("v", 129),
				"scarpenter.sparecores.com/cluster": "x"}
			return n
		}(), "reserved"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := d.ValidateNodeClass(bg(), c.nc)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("error = %v, want %q", err, c.want)
			}
		})
	}
	// Profiles.
	nc := ncWith(nil)
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"p": {Alicloud: &api.Storage{Root: &api.Disk{Size: q("1Gi")}}}}
	nc.Spec.Profiles.Network = map[string]v1alpha1.NetworkProfile{"p": {Alicloud: &api.Network{InternetMaxBandwidthOut: new(int32(999))}}}
	nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"p": {Alicloud: &api.Placement{DeploymentSet: &api.DeploymentSet{}}}}
	err := d.ValidateNodeClass(bg(), nc)
	for _, w := range []string{"profiles.storage.p.alicloud.root", "profiles.network.p.alicloud", "exactly one of strategy and use"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("profiles: %v lacks %q", err, w)
		}
	}
}
