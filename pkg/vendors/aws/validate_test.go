/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

func q(s string) *resource.Quantity { v := resource.MustParse(s); return &v }

func TestValidateNodeClass(t *testing.T) {
	e := newEnv(t, envOptions{})
	gp3 := func(size string, iops, tp *int32) *awsapi.Storage {
		return &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "gp3", VolumeSize: q(size), IOPS: iops, Throughput: tp}}
	}
	block := func(s awsapi.Settings) *v1alpha1.SCNodeClass {
		nc := &v1alpha1.SCNodeClass{}
		nc.Spec.Vendors.AWS = &s
		return nc
	}
	data := func(typ awsapi.VolumeType, size string, iops, tp *int32) awsapi.DataVolume {
		return awsapi.DataVolume{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, VolumeType: typ, VolumeSize: *q(size), IOPS: iops, Throughput: tp}
	}
	cases := []struct {
		name string
		nc   *v1alpha1.SCNodeClass
		want string // a substring of the error; "" means valid
	}{
		{"empty block", block(awsapi.Settings{}), ""},
		{"a full valid block", block(awsapi.Settings{
			Regions: []string{"us-east-1", "eu-central-1"}, InstanceProfile: "my-node_profile+1.x@y", ImageOwner: "123456789012",
			Storage:         gp3("100Gi", new(int32(5000)), new(int32(500))),
			MetadataOptions: &awsapi.MetadataOptions{HTTPTokens: "required", HTTPPutResponseHopLimit: new(int32(2))},
		}), ""},
		{"a region name", block(awsapi.Settings{Regions: []string{"us-east-1", "useast1"}}), `"useast1" is not an AWS region name`},
		{"an instance profile name", block(awsapi.Settings{InstanceProfile: "has space"}), "not a valid instance profile name"},
		{"hop limit too high", block(awsapi.Settings{MetadataOptions: &awsapi.MetadataOptions{HTTPPutResponseHopLimit: new(int32(65))}}), "1 to 64"},
		{"hop limit zero", block(awsapi.Settings{MetadataOptions: &awsapi.MetadataOptions{HTTPPutResponseHopLimit: new(int32(0))}}), "1 to 64"},
		{"a data volume of no size", block(awsapi.Settings{Storage: &awsapi.Storage{Data: []awsapi.DataVolume{data("gp3", "0", nil, nil)}}}), "gp3 volumes are 1 to 65536 GiB"},
		{"root gp3 size too big", block(awsapi.Settings{Storage: gp3("70Ti", nil, nil)}), "gp3 volumes are 1 to 65536 GiB"},
		{"gp3 iops below the minimum", block(awsapi.Settings{Storage: gp3("100Gi", new(int32(2000)), nil)}), "gp3 iops are 3000 to 80000"},
		{"gp3 iops above the size's ratio", block(awsapi.Settings{Storage: gp3("10Gi", new(int32(6000)), nil)}), "at most 500 IOPS per GiB, 6000 IOPS need 12 GiB"},
		{"gp3 throughput needs iops", block(awsapi.Settings{Storage: gp3("100Gi", nil, new(int32(1000)))}), "needs at least 4000 IOPS"},
		{"gp3 throughput above the limit", block(awsapi.Settings{Storage: gp3("100Gi", new(int32(10000)), new(int32(2500)))}), "gp3 throughput is 125 to 2000"},
		{"io1 without iops", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "io1", VolumeSize: q("100Gi")}}}), "need iops"},
		{"io2 iops ratio", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "io2", VolumeSize: q("10Gi"), IOPS: new(int32(20000))}}}), "at most 1000 IOPS per GiB"},
		{"iops on gp2", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "gp2", IOPS: new(int32(3000))}}}), "iops is not valid for gp2"},
		{"throughput on io2", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "io2", VolumeSize: q("100Gi"), IOPS: new(int32(3000)), Throughput: new(int32(200))}}}), "throughput is only valid for gp3"},
		{"st1 root", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: "st1", VolumeSize: q("500Gi")}}}), "cannot be boot volumes"},
		{"st1 too small", block(awsapi.Settings{Storage: &awsapi.Storage{Data: []awsapi.DataVolume{data("st1", "100Gi", nil, nil)}}}), "st1 volumes are 125 to 16384 GiB"},
		{"st1 data volume", block(awsapi.Settings{Storage: &awsapi.Storage{Data: []awsapi.DataVolume{data("st1", "500Gi", nil, nil)}}}), ""},
		{"standard maximum", block(awsapi.Settings{Storage: &awsapi.Storage{Data: []awsapi.DataVolume{data("standard", "2Ti", nil, nil)}}}), "standard volumes are 1 to 1024 GiB"},
		{"kms without encryption", block(awsapi.Settings{Storage: &awsapi.Storage{Root: &awsapi.RootVolume{KMSKeyID: "alias/x", Encrypted: new(false)}}}), "kmsKeyID needs encrypted"},
		{"duplicate data volume names", block(awsapi.Settings{Storage: &awsapi.Storage{Data: []awsapi.DataVolume{data("gp3", "10Gi", nil, nil), data("gp3", "10Gi", nil, nil)}}}), "used twice"},
		{"local storage without a mount path", block(awsapi.Settings{Storage: &awsapi.Storage{LocalStorage: &common.LocalStorage{Policy: "mount"}}}), "needs a mountPath"},
		{"inbound CIDR", block(awsapi.Settings{Networking: &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"not-a-cidr"}}}}}}), "is not a CIDR"},
		{"subnet ids", block(awsapi.Settings{Resources: &awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{IDs: map[string][]string{"us-east-1": {"subnet-0123456789abcdef0", "sg-1"}}}}}}), `"sg-1" is not a valid ID`},
		{"ids for a bad region", block(awsapi.Settings{Resources: &awsapi.Resources{SecurityGroups: &common.Resource{Use: &common.ResourceUse{IDs: map[string][]string{"nowhere": {"sg-0123456789abcdef0"}}}}}}), `"nowhere" is not a region`},
		{"selector tag key", block(awsapi.Settings{Resources: &awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Selector: &common.TagSelector{Tags: map[string]string{"aws:cloudformation:stack": "x"}}}}}}), "reserved prefix aws:"},
		{"a name pattern is fine", block(awsapi.Settings{Resources: &awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Name: "nodes-{zone}"}}}}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.d.ValidateNodeClass(context.Background(), tc.nc)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateProfilesAndTags(t *testing.T) {
	e := newEnv(t, envOptions{})
	nc := &v1alpha1.SCNodeClass{}
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{
		"ok":  {AWS: &awsapi.Storage{Data: []awsapi.DataVolume{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, VolumeType: "gp3", VolumeSize: *q("10Gi")}}}},
		"bad": {AWS: &awsapi.Storage{Data: []awsapi.DataVolume{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, VolumeType: "gp3", VolumeSize: *q("100Ti")}}}},
	}
	nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{
		"two":   {AWS: &awsapi.Placement{PlacementGroup: &awsapi.PlacementGroup{Strategy: "cluster", Use: &awsapi.PlacementGroupRef{Name: "x"}}}},
		"count": {AWS: &awsapi.Placement{PlacementGroup: &awsapi.PlacementGroup{Strategy: "spread", PartitionCount: new(int32(3))}}},
		"range": {AWS: &awsapi.Placement{PlacementGroup: &awsapi.PlacementGroup{Strategy: "partition", PartitionCount: new(int32(9))}}},
		"fine":  {AWS: &awsapi.Placement{PlacementGroup: &awsapi.PlacementGroup{Strategy: "partition", PartitionCount: new(int32(7))}}},
	}
	nc.Spec.Tags = map[string]string{"team": "data", "aws:reserved": "x", "scarpenter.sparecores.com/cluster": "x", "bad key!": "x"}
	err := e.d.ValidateNodeClass(context.Background(), nc)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"profiles.storage.bad.aws.data[0]", "set exactly one of strategy and use", "partitionCount is only valid with the partition strategy",
		"partitionCount is 1 to 7", "reserved prefix aws:", "is reserved for SCarpenter", `"bad key!" is not a valid tag key`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	for _, unwanted := range []string{"storage.ok", "placement.fine"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the error blames a valid profile (%s): %v", unwanted, err)
		}
	}
	many := &v1alpha1.SCNodeClass{}
	many.Spec.Tags = map[string]string{}
	for i := range 41 {
		many.Spec.Tags["k"+strings.Repeat("x", i)] = "v"
	}
	if err := e.d.ValidateNodeClass(context.Background(), many); err == nil || !strings.Contains(err.Error(), "at most 40") {
		t.Errorf("41 tags: %v", err)
	}
	// A NodeClass without an aws block validates (the controller asks only for
	// vendors the NodeClass has, but the call must be safe).
	if err := e.d.ValidateNodeClass(context.Background(), &v1alpha1.SCNodeClass{}); err != nil {
		t.Errorf("no aws block: %v", err)
	}
}
