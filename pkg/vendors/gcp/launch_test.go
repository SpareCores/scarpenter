/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

const uid = "0f8fad5b-d9cb-469f-a165-70867728950e"

func baseSpec() LaunchSpec {
	labels, _ := Labels(map[vendors.TagKey]string{vendors.TagCluster: "c1", vendors.TagNodeClaim: "claim-1"}, map[string]string{"team": "data"})
	return LaunchSpec{
		Project: "proj", Zone: fake.SeedZoneA, Name: "claim-1", MachineType: "e2-small",
		OnHostMaintenance: "MIGRATE",
		Image:             ImageURL("proj", "projects/"+fake.SeedImageProject+"/global/images/"+fake.SeedImage),
		Labels:            labels, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Root:       Disk{Type: "pd-balanced", SizeGb: 64},
		Subnetwork: SubnetworkURL("proj", fake.SeedRegion, fake.SeedSubnet), NetworkTags: []string{"nodes"},
		PublicIPv4: true, IdempotencyKey: uid, CapacityType: vendors.CapacityOnDemand,
	}
}

// TestBuildInsertGolden asserts every field of the request for an on-demand
// launch with a data volume, IPv6 and a placement policy.
func TestBuildInsertGolden(t *testing.T) {
	s := baseSpec()
	s.Data = []Disk{{Name: "data0", Type: "hyperdisk-balanced", SizeGb: 100, ProvisionedIops: 3000, ProvisionedThroughput: 200, Interface: "NVME", KMSKeyName: "projects/p/locations/l/keyRings/k/cryptoKeys/c"}}
	s.PublicIPv6 = true
	s.NicType, s.Tier1 = "GVNIC", true
	s.ServiceAccount = "node@proj.iam.gserviceaccount.com"
	s.ResourcePolicies = []string{"projects/proj/regions/europe-west3/resourcePolicies/scarpenter-c1-compact"}
	s.ExtraMetadata = map[string]string{"team-meta": "x"}
	req, err := BuildInsert(s)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetProject() != "proj" || req.GetZone() != fake.SeedZoneA {
		t.Fatalf("request scope = %s %s", req.GetProject(), req.GetZone())
	}
	if want := RequestID(uid, fake.SeedZoneA, "e2-small", vendors.CapacityOnDemand); req.GetRequestId() != want {
		t.Fatalf("request id = %s, want %s", req.GetRequestId(), want)
	}
	in := req.GetInstanceResource()
	if in.GetName() != "claim-1" || in.GetMachineType() != "zones/europe-west3-a/machineTypes/e2-small" {
		t.Fatalf("name/type = %s %s", in.GetName(), in.GetMachineType())
	}

	checkDisks(t, in)
	checkNetwork(t, in)
	checkLabelsAndScheduling(t, in)
	checkMetadata(t, in, s)
	checkExtras(t, in, s)
}

func checkDisks(t *testing.T, in *computepb.Instance) {
	t.Helper()
	// Disks: boot first, then the data volume with autoDelete.
	if len(in.Disks) != 2 {
		t.Fatalf("%d disks", len(in.Disks))
	}
	boot := in.Disks[0]
	bp := boot.GetInitializeParams()
	if !boot.GetBoot() || !boot.GetAutoDelete() || boot.GetType() != "PERSISTENT" || bp.GetDiskName() != "claim-1" ||
		bp.GetDiskType() != "zones/europe-west3-a/diskTypes/pd-balanced" || bp.GetDiskSizeGb() != 64 ||
		bp.GetSourceImage() != "projects/kinvolk-public/global/images/flatcar-stable-4757-2-3" ||
		bp.GetProvisionedIops() != 0 || boot.GetDiskEncryptionKey() != nil {
		t.Fatalf("boot disk = %v", boot)
	}
	if bp.Labels["scarpenter-cluster"] != "c1" || bp.Labels["team"] != "data" {
		t.Fatalf("boot disk labels = %v", bp.Labels)
	}
	data := in.Disks[1]
	dp := data.GetInitializeParams()
	if data.GetBoot() || !data.GetAutoDelete() || data.GetDeviceName() != "data0" || data.GetInterface() != "NVME" ||
		dp.GetDiskName() != "claim-1-data0" || dp.GetDiskType() != "zones/europe-west3-a/diskTypes/hyperdisk-balanced" ||
		dp.GetDiskSizeGb() != 100 || dp.GetProvisionedIops() != 3000 || dp.GetProvisionedThroughput() != 200 ||
		dp.SourceImage != nil || data.GetDiskEncryptionKey().GetKmsKeyName() != "projects/p/locations/l/keyRings/k/cryptoKeys/c" {
		t.Fatalf("data disk = %v", data)
	}
	if dp.Labels[TagKey(vendors.TagIdempotencyKey)] != uid {
		t.Fatalf("data disk labels = %v", dp.Labels)
	}

}

func checkNetwork(t *testing.T, in *computepb.Instance) {
	t.Helper()
	// Network.
	nic := in.NetworkInterfaces[0]
	if len(in.NetworkInterfaces) != 1 || nic.GetSubnetwork() != "projects/proj/regions/europe-west3/subnetworks/nodes" ||
		nic.GetStackType() != "IPV4_IPV6" || nic.GetNicType() != "GVNIC" ||
		len(nic.AccessConfigs) != 1 || nic.AccessConfigs[0].GetType() != "ONE_TO_ONE_NAT" ||
		len(nic.Ipv6AccessConfigs) != 1 || nic.Ipv6AccessConfigs[0].GetType() != "DIRECT_IPV6" {
		t.Fatalf("nic = %v", nic)
	}
	if in.GetNetworkPerformanceConfig().GetTotalEgressBandwidthTier() != "TIER_1" {
		t.Fatalf("network performance = %v", in.NetworkPerformanceConfig)
	}
	if !reflect.DeepEqual(in.GetTags().GetItems(), []string{"nodes"}) {
		t.Fatalf("tags = %v", in.Tags)
	}

}

func checkLabelsAndScheduling(t *testing.T, in *computepb.Instance) {
	t.Helper()
	// Labels, including the idempotency key.
	wantLabels := map[string]string{
		"scarpenter-cluster": "c1", "scarpenter-nodeclaim": "claim-1", "scarpenter-idempotency-key": uid, "team": "data",
	}
	if !reflect.DeepEqual(in.Labels, wantLabels) {
		t.Fatalf("labels = %v", in.Labels)
	}

	// Scheduling: on-demand.
	sc := in.Scheduling
	if sc.GetProvisioningModel() != "STANDARD" || sc.GetAutomaticRestart() || sc.GetOnHostMaintenance() != "MIGRATE" || sc.InstanceTerminationAction != nil {
		t.Fatalf("scheduling = %v", sc)
	}

}

func checkMetadata(t *testing.T, in *computepb.Instance, s LaunchSpec) {
	t.Helper()
	// Metadata: sorted, user-data raw, SSH keys blocked, OS Login off.
	keys := make([]string, 0, len(in.Metadata.Items))
	meta := map[string]string{}
	for _, it := range in.Metadata.Items {
		keys = append(keys, it.GetKey())
		meta[it.GetKey()] = it.GetValue()
	}
	if !reflect.DeepEqual(keys, []string{"block-project-ssh-keys", "enable-oslogin", "team-meta", "user-data"}) {
		t.Fatalf("metadata keys = %v", keys)
	}
	if meta["user-data"] != string(s.UserData) || meta["block-project-ssh-keys"] != "TRUE" || meta["enable-oslogin"] != "FALSE" || meta["team-meta"] != "x" {
		t.Fatalf("metadata = %v", meta)
	}
	if _, has := meta["ssh-keys"]; has {
		t.Fatal("no ssh-keys metadata")
	}

}

func checkExtras(t *testing.T, in *computepb.Instance, s LaunchSpec) {
	t.Helper()
	if len(in.ServiceAccounts) != 1 || in.ServiceAccounts[0].GetEmail() != "node@proj.iam.gserviceaccount.com" {
		t.Fatalf("service accounts = %v", in.ServiceAccounts)
	}
	if !reflect.DeepEqual(in.ResourcePolicies, s.ResourcePolicies) {
		t.Fatalf("resource policies = %v", in.ResourcePolicies)
	}
	if in.ShieldedInstanceConfig != nil {
		t.Fatal("shielded config must be unset")
	}
}

func TestBuildInsertSpotAndMinimal(t *testing.T) {
	s := baseSpec()
	s.Spot, s.CapacityType = true, vendors.CapacitySpot
	s.OnHostMaintenance = "MIGRATE" // ignored for Spot
	s.PublicIPv4 = false
	s.NetworkTags, s.IdempotencyKey = nil, ""
	req, err := BuildInsert(s)
	if err != nil {
		t.Fatal(err)
	}
	in := req.GetInstanceResource()
	sc := in.Scheduling
	if sc.GetProvisioningModel() != "SPOT" || sc.GetInstanceTerminationAction() != "DELETE" || sc.GetOnHostMaintenance() != "TERMINATE" || sc.GetAutomaticRestart() {
		t.Fatalf("spot scheduling = %v", sc)
	}
	nic := in.NetworkInterfaces[0]
	if len(nic.AccessConfigs) != 0 || nic.GetStackType() != "IPV4_ONLY" || nic.Ipv6AccessConfigs != nil || nic.NicType != nil {
		t.Fatalf("nic = %v", nic)
	}
	if in.Tags != nil || in.ServiceAccounts != nil || in.NetworkPerformanceConfig != nil || len(in.ResourcePolicies) != 0 {
		t.Fatalf("optional fields set: %v", in)
	}
	if _, has := in.Labels[TagKey(vendors.TagIdempotencyKey)]; has {
		t.Fatal("no idempotency label without a key")
	}
	// On-demand defaults to MIGRATE, TERMINATE when asked.
	s.Spot, s.OnHostMaintenance = false, ""
	req, _ = BuildInsert(s)
	if got := req.InstanceResource.Scheduling.GetOnHostMaintenance(); got != "MIGRATE" {
		t.Fatalf("default maintenance = %s", got)
	}
	s.OnHostMaintenance = "TERMINATE"
	req, _ = BuildInsert(s)
	if got := req.InstanceResource.Scheduling.GetOnHostMaintenance(); got != "TERMINATE" {
		t.Fatalf("maintenance = %s", got)
	}
}

func TestBuildInsertValidation(t *testing.T) {
	tests := map[string]func(*LaunchSpec){
		"no project":  func(s *LaunchSpec) { s.Project = "" },
		"no zone":     func(s *LaunchSpec) { s.Zone = "" },
		"no name":     func(s *LaunchSpec) { s.Name = "" },
		"no type":     func(s *LaunchSpec) { s.MachineType = "" },
		"no image":    func(s *LaunchSpec) { s.Image = "" },
		"no subnet":   func(s *LaunchSpec) { s.Subnetwork = "" },
		"no root":     func(s *LaunchSpec) { s.Root = Disk{} },
		"root size 0": func(s *LaunchSpec) { s.Root.SizeGb = 0 },
		"bad nic":     func(s *LaunchSpec) { s.NicType = "E1000" },
		"data no name": func(s *LaunchSpec) {
			s.Data = []Disk{{Type: "pd-balanced", SizeGb: 10}}
		},
		"data no size": func(s *LaunchSpec) {
			s.Data = []Disk{{Name: "d", Type: "pd-balanced"}}
		},
	}
	for _, key := range ReservedMetadataKeys {
		tests["reserved "+key] = func(s *LaunchSpec) { s.ExtraMetadata = map[string]string{key: "x"} }
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := baseSpec()
			mutate(&s)
			if _, err := BuildInsert(s); err == nil {
				t.Fatal("must fail")
			}
		})
	}
}

func TestDefaultRootDiskType(t *testing.T) {
	for mt, want := range map[string]string{
		"e2-small": "pd-balanced", "n2-standard-2": "pd-balanced", "t2a-standard-1": "pd-balanced", "c3-standard-4": "pd-balanced",
		"c4-standard-4": "hyperdisk-balanced", "n4-standard-2": "hyperdisk-balanced", "c4a-standard-4": "hyperdisk-balanced",
		"n4a-standard-4": "hyperdisk-balanced", "a3-highgpu-8g": "hyperdisk-balanced",
	} {
		if got := DefaultRootDiskType(mt); got != want {
			t.Errorf("DefaultRootDiskType(%s) = %s, want %s", mt, got, want)
		}
	}
	if DataDevicePath("data0") != "/dev/disk/by-id/google-data0" {
		t.Fatal("data device path")
	}
}

// TestLaunchThroughFake sends the built request through the fake and the
// classification: success, replay, a taken name, and the async errors.
func TestLaunchThroughFake(t *testing.T) {
	ctx := t.Context()
	c := fake.NewSeeded("proj")
	spec := baseSpec()
	spec.MachineType = "n2-standard-2"
	req, err := BuildInsert(spec)
	if err != nil {
		t.Fatal(err)
	}
	ec := errContext{machineType: spec.MachineType, zone: spec.Zone}
	insert := func() (*computepb.Operation, error) {
		op, err := c.InsertInstance(ctx, req)
		if err != nil {
			return nil, classify(err, ec)
		}
		done, err := c.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{Project: "proj", Zone: spec.Zone, Operation: op.GetName()})
		if err != nil {
			return nil, classify(err, ec)
		}
		return done, classifyOperation(done, ec)
	}
	op1, err := insert()
	if err != nil {
		t.Fatal(err)
	}
	// Replay with the same request ID: the same operation, one instance.
	op2, err := insert()
	if err != nil || op2.GetName() != op1.GetName() || len(c.Instances()) != 1 {
		t.Fatalf("replay = %v, %v, %d instances", op2.GetName(), err, len(c.Instances()))
	}
	// A fresh request ID for the same name: already exists, found by label.
	req.RequestId = nil
	_, err = insert()
	if !IsAlreadyExists(err) || vendors.ClassOf(err) != vendors.Fatal {
		t.Fatalf("taken name = %v", err)
	}
	found, err := c.AggregatedListInstances(ctx, &computepb.AggregatedListInstancesRequest{
		Project: "proj", Filter: new(LabelFilter(map[vendors.TagKey]string{vendors.TagIdempotencyKey: uid})),
	})
	if err != nil || len(found.Items[("zones/"+spec.Zone)].Instances) != 1 {
		t.Fatalf("lookup by idempotency label = %v, %v", found, err)
	}

	// Async errors classify. Every attempt below is a new offering or a new
	// request: a replayed request ID returns the first operation, failed or not.
	retry := func() (*computepb.Operation, error) { req.RequestId = nil; return insert() }
	c.Stockout(fake.SeedZoneB, "n2-standard-2", false, true)
	spec.Zone, spec.Name = fake.SeedZoneB, "claim-2"
	req, _ = BuildInsert(spec)
	ec = errContext{machineType: spec.MachineType, zone: spec.Zone}
	if _, err := insert(); vendors.ClassOf(err) != vendors.InsufficientCapacity {
		t.Fatalf("stockout = %v", err)
	}
	c.Stockout(fake.SeedZoneB, "n2-standard-2", false, false)
	c.AddForeignUsage("", "CPUS_ALL_REGIONS", 31)
	if _, err := retry(); vendors.ClassOf(err) != vendors.QuotaExceeded || !vendors.IsAccountWide(err) {
		t.Fatalf("all-regions quota = %v", err)
	}
	c.SetLimit("", "CPUS_ALL_REGIONS", 1000)
	c.AddForeignUsage(fake.SeedRegion, "N2_CPUS", 8)
	if _, err := retry(); vendors.ClassOf(err) != vendors.QuotaExceeded || vendors.IsAccountWide(err) || !strings.Contains(err.Error(), "N2_CPUS") {
		t.Fatalf("family quota = %v", err)
	}
	c.SetOrgPolicyViolation(true)
	if _, err := retry(); vendors.ClassOf(err) != vendors.Fatal || vendors.CodeOf(err) != CodeOrgPolicy {
		t.Fatalf("org policy = %v", err)
	}
	c.SetOrgPolicyViolation(false)
	// A machine type the zone does not offer is Unsupported.
	c.RemoveMachineType(fake.SeedZoneB, "n2-standard-2")
	if _, err := retry(); vendors.ClassOf(err) != vendors.Unsupported {
		t.Fatalf("missing machine type = %v", err)
	}
	// A throttled API is Retryable.
	c.Inject("InsertInstance", fake.Error(429, "rateLimitExceeded", "slow"), 1)
	if _, err := retry(); vendors.ClassOf(err) != vendors.Retryable {
		t.Fatalf("throttled = %v", err)
	}
}
