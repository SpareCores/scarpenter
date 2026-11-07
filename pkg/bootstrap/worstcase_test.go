/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Every storage setup is rendered, and local disks count as the most a type has
// (24: the AWS d3 and d2 families), because with the policy mount each disk is a
// mount unit.
func TestWorstCaseVolumesCoverEverySetupWith24LocalDisks(t *testing.T) {
	data := []aws.DataVolume{{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}}}
	nc := &v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{
		Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{}},
		Profiles: v1alpha1.Profiles{Storage: map[string]v1alpha1.StorageProfile{
			"data":  {AWS: &aws.Storage{Data: data}},
			"local": {AWS: &aws.Storage{LocalStorage: &common.LocalStorage{Policy: common.LocalStorageMount, MountPath: "/mnt/local"}}},
			"raid":  {AWS: &aws.Storage{LocalStorage: &common.LocalStorage{Policy: common.LocalStorageRaid0}}},
		}},
	}}
	got, err := worstCaseVolumes(nc)
	if err != nil {
		t.Fatal(err)
	}
	var withData, mount, raid int
	for _, w := range got {
		switch {
		case len(w.volumes) == 1 && w.local == nil:
			withData++
		case w.local != nil && w.local.Policy == localPolicyMount:
			mount++
			if w.local.Count != 24 {
				t.Errorf("the local disks are counted as %d, want 24", w.local.Count)
			}
			// 24 mount units are a real addition to the user-data.
			if _, local, err := volumesData(w.volumes, w.local); err != nil || len(local.Mounts) != 24 {
				t.Errorf("rendering 24 local mount disks: %v mounts, %v", local, err)
			}
		case w.local != nil && w.local.Policy == localPolicyRaid0:
			raid++
			if w.local.MountPath == "" {
				t.Error("a local storage without a mount path is rendered with none")
			}
		}
	}
	if withData != 1 || mount != 1 || raid != 1 || len(got) != 3 {
		t.Errorf("%d setups (data %d, mount %d, raid0 %d), want one of each of the three profiles", len(got), withData, mount, raid)
	}
}
