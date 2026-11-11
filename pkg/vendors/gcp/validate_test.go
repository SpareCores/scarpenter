/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
)

func nodeClass(b *gcpapi.Settings) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{}
	nc.Name = "class"
	nc.Spec.Vendors.GCP = b
	return nc
}

func use(ids map[string]common.IDList) *common.Resource {
	return &common.Resource{Use: &common.ResourceUse{IDs: ids}}
}

func useSubnets(ids map[string]common.IDList) *gcpapi.SubnetResource {
	return &gcpapi.SubnetResource{Use: &common.ResourceUse{IDs: ids}}
}

func TestValidateNodeClass(t *testing.T) {
	e := newEnv(t, envOptions{bare: true})
	ok := func(name string, nc *v1alpha1.SCNodeClass) {
		t.Helper()
		if err := e.d.ValidateNodeClass(t.Context(), nc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := func(name string, nc *v1alpha1.SCNodeClass, want string) {
		t.Helper()
		err := e.d.ValidateNodeClass(t.Context(), nc)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error with %q", name, err, want)
		}
	}
	ok("an empty block", nodeClass(&gcpapi.Settings{}))
	ok("a full block", nodeClass(&gcpapi.Settings{
		Regions: []string{"europe-west3", "us-east1"}, Project: testProject, ImageProject: "kinvolk-public",
		ServiceAccount: "node-sa1@my-project.iam.gserviceaccount.com", Metadata: map[string]string{"a": "b"},
		Storage: &gcpapi.Storage{Root: &gcpapi.RootDisk{DiskType: "pd-ssd", SizeGb: new(int64(100))},
			Data: []gcpapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, DiskType: "hyperdisk-balanced",
				SizeGb: 100, ProvisionedIops: new(int64(5000))}}},
		Resources: &gcpapi.Resources{
			Subnets:          useSubnets(map[string]common.IDList{"europe-west3": {"projects/my-project/regions/europe-west3/subnetworks/n", "short"}}),
			Firewalls:        use(map[string]common.IDList{"*": {"nodes"}}),
			Images:           use(map[string]common.IDList{"*": {"projects/my-project/global/images/flatcar-stable-4757-2-3"}}),
			ResourcePolicies: use(map[string]common.IDList{"europe-west3": {"policy"}}),
		}}))
	bad("a region", nodeClass(&gcpapi.Settings{Regions: []string{"Europe"}}), "not a GCP region")
	bad("a project", nodeClass(&gcpapi.Settings{Project: "A"}), "not a project ID")
	bad("another project", nodeClass(&gcpapi.Settings{Project: "other-project"}), "not the project of the controller")
	bad("an image project", nodeClass(&gcpapi.Settings{ImageProject: "x"}), "not a project ID")
	bad("a service account", nodeClass(&gcpapi.Settings{ServiceAccount: "me@example.com"}), "service account e-mail")
	bad("a reserved metadata key", nodeClass(&gcpapi.Settings{Metadata: map[string]string{"user-data": "x"}}), "reserved")
	bad("a root disk that cannot boot", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "hyperdisk-ml", SizeGb: new(int64(100))}}}), "cannot be boot disks")
	bad("a root disk too small", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{SizeGb: new(int64(5))}}}), "boot disks are 10 to 65536")
	bad("IOPS on a PD", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "pd-ssd", ProvisionedIops: new(int64(5000))}}}), "provisionedIops is not valid")
	bad("too many IOPS", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "hyperdisk-balanced", ProvisionedIops: new(int64(500000))}}}), "at most 160000 IOPS")
	bad("throughput on a PD", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "pd-ssd", ProvisionedThroughput: new(int64(500))}}}), "provisionedThroughput is not valid")
	bad("too much throughput", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "hyperdisk-balanced", ProvisionedThroughput: new(int64(5000))}}}), "at most 2400")
	bad("a KMS key", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Root: &gcpapi.RootDisk{DiskType: "pd-ssd", KMSKeyName: "key"}}}), "kmsKeyName")
	bad("extreme without IOPS", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Data: []gcpapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, DiskType: "hyperdisk-extreme", SizeGb: 100}}}}), "need provisionedIops")
	bad("a disk twice", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{Data: []gcpapi.DataDisk{
		{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, DiskType: "pd-ssd", SizeGb: 100},
		{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/e"}, DiskType: "pd-ssd", SizeGb: 100}}}}), "used twice")
	bad("an unknown disk type", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		Data: []gcpapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, DiskType: "tape", SizeGb: 100}}}}), "unknown disk type")
	bad("local storage without a path", nodeClass(&gcpapi.Settings{Storage: &gcpapi.Storage{
		LocalStorage: &common.LocalStorage{Policy: common.LocalStorageMount}}}), "needs a mountPath")
	bad("a CIDR", nodeClass(&gcpapi.Settings{Networking: &common.NetworkingOverride{Inbound: &common.Inbound{
		Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"nope"}}}}}}), "not a CIDR")
	bad("a region key", nodeClass(&gcpapi.Settings{Resources: &gcpapi.Resources{Subnets: useSubnets(map[string]common.IDList{"Mars": {"x"}})}}), "not a region")
	bad("a subnet ID", nodeClass(&gcpapi.Settings{Resources: &gcpapi.Resources{Subnets: useSubnets(map[string]common.IDList{"*": {"Bad Name"}})}}), "not a valid ID")
	pool := func(p string) *gcpapi.Settings {
		return &gcpapi.Settings{Resources: &gcpapi.Resources{Subnets: &gcpapi.SubnetResource{
			Create: &gcpapi.SubnetCreate{Pool: p}}}}
	}
	for _, p := range []string{"10.240.0.0/12", "10.0.0.0/8", "172.16.0.0/20", "192.168.64.0/18"} {
		ok("pool "+p, nodeClass(pool(p)))
	}
	bad("a pool that is no CIDR", nodeClass(pool("nope")), "not an IPv4 CIDR")
	bad("an IPv6 pool", nodeClass(pool("fd00::/8")), "not an IPv4 CIDR")
	bad("a public pool", nodeClass(pool("8.8.0.0/16")), "not inside 10.0.0.0/8")
	bad("a pool bigger than a private range", nodeClass(pool("172.0.0.0/8")), "not inside 10.0.0.0/8")
	bad("a pool too small", nodeClass(pool("10.1.0.0/24")), "no subnetwork fits")
	bad("a pool with host bits", nodeClass(pool("10.1.1.0/16")), "host bits")
	bad("tags on a subnet", nodeClass(&gcpapi.Settings{Resources: &gcpapi.Resources{Subnets: &gcpapi.SubnetResource{Use: &common.ResourceUse{
		Tags: map[string]string{"a": "b"}}}}}), "tags are not supported")
	bad("a bad label key on images", nodeClass(&gcpapi.Settings{Resources: &gcpapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
		Tags: map[string]string{"Bad Key": "b"}}}}}), "not a valid label key")
	bad("no public address and nothing to give egress", func() *v1alpha1.SCNodeClass {
		nc := nodeClass(&gcpapi.Settings{})
		nc.Spec.Networking.PublicIPv4 = new(false)
		return nc
	}(), "no egress")
	ok("no public address with own subnets", func() *v1alpha1.SCNodeClass {
		nc := nodeClass(&gcpapi.Settings{Resources: &gcpapi.Resources{Subnets: useSubnets(map[string]common.IDList{"*": {"mine"}})}})
		nc.Spec.Networking.PublicIPv4 = new(false)
		return nc
	}())
	// Profiles and tags.
	nc := nodeClass(&gcpapi.Settings{})
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"p": {GCP: &gcpapi.Storage{Root: &gcpapi.RootDisk{DiskType: "pd-ssd", SizeGb: new(int64(1))}}}}
	nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{
		"a": {GCP: &gcpapi.Placement{ResourcePolicy: &gcpapi.ResourcePolicy{Spread: &gcpapi.Spread{AvailabilityDomainCount: 20}}}},
		"b": {GCP: &gcpapi.Placement{ResourcePolicy: &gcpapi.ResourcePolicy{}}},
		"c": {GCP: &gcpapi.Placement{ResourcePolicy: &gcpapi.ResourcePolicy{Use: &gcpapi.ResourcePolicyRef{Name: "Bad"}}}},
		"d": {GCP: &gcpapi.Placement{}},
	}
	nc.Spec.Tags = map[string]string{"scarpenter-x": "y"}
	err := e.d.ValidateNodeClass(t.Context(), nc)
	for _, want := range []string{"profiles.storage.p.gcp.root", "availabilityDomainCount is 1 to 8", "set exactly one", "not a resource policy name", "reserved prefix"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("profiles and tags: %v, want %q", err, want)
		}
	}
	many := map[string]string{}
	for i := range 45 {
		many["k"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	nc = nodeClass(&gcpapi.Settings{})
	nc.Spec.Tags = many
	bad("too many tags", nc, "at most 40")
}
