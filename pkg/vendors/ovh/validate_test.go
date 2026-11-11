/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ovhapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/ovh"
)

func TestValidateNodeClass(t *testing.T) {
	d := New()
	q := resource.MustParse
	f := false
	use := func(u common.ResourceUse) *common.Resource { return &common.Resource{Use: &u} }
	manyTags := map[string]string{}
	for i := range 101 {
		manyTags[strings.Repeat("k", 1)+string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	ids := func(region, id string) map[string]common.IDList { return map[string]common.IDList{region: {id}} }
	const uuid = "9c1f4b8e-2a7d-4c63-9a55-0d1e6f3b7a21"
	for name, tc := range map[string]struct {
		mod  func(*v1alpha1.SCNodeClass)
		want string
	}{
		"fine":          {func(*v1alpha1.SCNodeClass) {}, ""},
		"a region name": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.OVH.Regions = []string{"gra11"} }, "upper case"},
		"private only":  {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Networking.PublicIPv4 = &f }, "publicIPv4: false is refused"},
		"override private only": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Networking = &common.NetworkingOverride{PublicIPv4: &f}
		}, "publicIPv4: false is refused"},
		"inbound CIDR": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"10.0.0.1"}}}}}
		}, "not a CIDR"},
		"local storage": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Storage = &ovhapi.Storage{LocalStorage: &common.LocalStorage{Policy: "mount"}}
		}, "localStorage: not supported"},
		"boot volume size": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Storage = &ovhapi.Storage{Root: &ovhapi.Root{BootFromVolume: &ovhapi.VolumeSpec{VolumeType: "classic", Size: q("5Gi")}}}
		}, "10 to 12000 GiB"},
		"volume type": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Storage = &ovhapi.Storage{Root: &ovhapi.Root{BootFromVolume: &ovhapi.VolumeSpec{VolumeType: "nvme", Size: q("50Gi")}}}
		}, "not one of"},
		"duplicate volume": {func(nc *v1alpha1.SCNodeClass) {
			v := ovhapi.DataVolume{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}, VolumeType: "classic", Size: q("10Gi")}
			nc.Spec.Vendors.OVH.Storage = &ovhapi.Storage{Data: []ovhapi.DataVolume{v, v}}
		}, "used twice"},
		"profile storage": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"p": {OVH: &ovhapi.Storage{LocalStorage: &common.LocalStorage{}}}}
		}, "profiles.storage.p.ovh"},
		"profile placement": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"p": {OVH: &ovhapi.Placement{ServerGroup: &ovhapi.ServerGroup{}}}}
		}, "exactly one of policy and use"},
		"profile policy": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"p": {OVH: &ovhapi.Placement{ServerGroup: &ovhapi.ServerGroup{Policy: "soft"}}}}
		}, "not affinity or anti-affinity"},
		"image id": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{Images: use(common.ResourceUse{IDs: ids("GRA11", "ami-123")})}
		}, "not a valid ID"},
		"image id region": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{Images: use(common.ResourceUse{IDs: ids("gra11", uuid)})}
		}, "not a region"},
		"image id ok": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{Images: use(common.ResourceUse{IDs: ids("GRA11", uuid)}),
				SecurityGroups: use(common.ResourceUse{Name: "nodes-*"}), ServerGroups: use(common.ResourceUse{Name: "g"})}
		}, ""},
		"security group tags": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{SecurityGroups: use(common.ResourceUse{Tags: map[string]string{"a": "b"}})}
		}, "tags are not supported"},
		"server group zone": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Resources = &ovhapi.Resources{ServerGroups: use(common.ResourceUse{Match: &common.ResourceMatch{Zone: []string{"z"}}})}
		}, "match.zone is not supported"},
		"too many tags":  {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = manyTags }, "at most 100"},
		"a long tag key": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{strings.Repeat("k", 250): "v"} }, "longer than"},
		"a long tag":     {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"k": strings.Repeat("v", 256)} }, "longer than 255"},
		"an empty key":   {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"": "v"} }, "is empty"},
		"a reserved tag": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"scarpenter.sparecores.com/x": "v"} }, "reserved"},
		"data volume too": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OVH.Storage = &ovhapi.Storage{Data: make([]ovhapi.DataVolume, 9)}
		}, "at most 8"},
	} {
		nc := &v1alpha1.SCNodeClass{}
		nc.Spec.Vendors.OVH = &ovhapi.Settings{}
		tc.mod(nc)
		err := d.ValidateNodeClass(ctx, nc)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
