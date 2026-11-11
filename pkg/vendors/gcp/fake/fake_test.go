/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

const proj = "test-project"

var ctx = context.Background()

func subnetURL() string {
	return "projects/" + proj + "/regions/" + SeedRegion + "/subnetworks/" + SeedSubnet
}

func imageURL() string {
	return "projects/" + SeedImageProject + "/global/images/" + SeedImage
}

func instance(name, mt string) *computepb.Instance {
	return &computepb.Instance{
		Name: new(name), MachineType: new("zones/" + SeedZoneA + "/machineTypes/" + mt),
		Disks: []*computepb.AttachedDisk{{
			Boot: new(true), AutoDelete: new(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: new(imageURL())},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{{
			Subnetwork:    new(subnetURL()),
			AccessConfigs: []*computepb.AccessConfig{{Type: new("ONE_TO_ONE_NAT")}},
		}},
		Labels:     map[string]string{"scarpenter-cluster": "c1"},
		Tags:       &computepb.Tags{Items: []string{"nodes"}},
		Scheduling: &computepb.Scheduling{ProvisioningModel: new("STANDARD"), OnHostMaintenance: new("MIGRATE")},
	}
}

func insertReq(in *computepb.Instance, zone, requestID string) *computepb.InsertInstanceRequest {
	r := &computepb.InsertInstanceRequest{Project: proj, Zone: zone, InstanceResource: in}
	if requestID != "" {
		r.RequestId = new(requestID)
	}
	return r
}

// run inserts and waits for the operation, returning the finished operation.
func run(t *testing.T, c *Compute, in *computepb.Instance, zone string) *computepb.Operation {
	t.Helper()
	op, err := c.InsertInstance(ctx, insertReq(in, zone, ""))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	done, err := c.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{Project: proj, Zone: zone, Operation: op.GetName()})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return done
}

func statusOf(t *testing.T, c *Compute, zone, name string) string {
	t.Helper()
	in, err := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: zone, Instance: name})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return in.GetStatus()
}

func httpCode(err error) int {
	if ge, ok := errors.AsType[*googleapi.Error](err); ok {
		return ge.Code
	}
	return 0
}

func reason(err error) string {
	if ge, ok := errors.AsType[*googleapi.Error](err); ok && len(ge.Errors) > 0 {
		return ge.Errors[0].Reason
	}
	return ""
}

func TestInsertLifecycle(t *testing.T) {
	c := NewSeeded(proj)
	op, err := c.InsertInstance(ctx, insertReq(instance("n1", "e2-small"), SeedZoneA, ""))
	if err != nil {
		t.Fatal(err)
	}
	if op.GetStatus() != computepb.Operation_RUNNING {
		t.Fatalf("a fresh operation is %s", op.GetStatus())
	}
	if got := statusOf(t, c, SeedZoneA, "n1"); got != "PROVISIONING" {
		t.Fatalf("status before the operation completes = %s", got)
	}
	// Get polls: with the default of one poll the first Get reports DONE.
	got, err := c.GetZoneOperation(ctx, &computepb.GetZoneOperationRequest{Project: proj, Zone: SeedZoneA, Operation: op.GetName()})
	if err != nil || got.GetStatus() != computepb.Operation_DONE {
		t.Fatalf("get operation = %v, %v", got.GetStatus(), err)
	}
	in, _ := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"})
	if in.GetStatus() != "RUNNING" {
		t.Fatalf("status after = %s", in.GetStatus())
	}
	nic := in.GetNetworkInterfaces()[0]
	if nic.GetNetworkIP() != "10.240.0.2" || nic.GetAccessConfigs()[0].GetNatIP() == "" {
		t.Fatalf("addresses = %q %q", nic.GetNetworkIP(), nic.GetAccessConfigs()[0].GetNatIP())
	}
	if !strings.HasSuffix(in.GetMachineType(), "/zones/"+SeedZoneA+"/machineTypes/e2-small") {
		t.Fatalf("machine type = %s", in.GetMachineType())
	}
	// Delete: STOPPING until the operation completes, then gone, then 404.
	del, err := c.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, c, SeedZoneA, "n1"); got != "STOPPING" {
		t.Fatalf("status while deleting = %s", got)
	}
	if _, err := c.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{Project: proj, Zone: SeedZoneA, Operation: del.GetName()}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"})
	if httpCode(err) != http.StatusNotFound {
		t.Fatalf("get after delete = %v", err)
	}
	_, err = c.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"})
	if httpCode(err) != http.StatusNotFound {
		t.Fatalf("second delete = %v", err)
	}
	// The name and the address are free again.
	if op := run(t, c, instance("n1", "e2-small"), SeedZoneA); op.GetError() != nil {
		t.Fatalf("reuse: %v", op.GetError())
	}
	if in, _ := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"}); in.GetNetworkInterfaces()[0].GetNetworkIP() != "10.240.0.2" {
		t.Fatalf("address not reused: %s", in.GetNetworkInterfaces()[0].GetNetworkIP())
	}
}

func TestOpPolls(t *testing.T) {
	c := NewSeeded(proj)
	c.SetOpPolls(3)
	op, _ := c.InsertInstance(ctx, insertReq(instance("n1", "e2-small"), SeedZoneA, ""))
	for i := 1; i <= 3; i++ {
		got, _ := c.GetZoneOperation(ctx, &computepb.GetZoneOperationRequest{Project: proj, Zone: SeedZoneA, Operation: op.GetName()})
		if want := i == 3; (got.GetStatus() == computepb.Operation_DONE) != want {
			t.Fatalf("poll %d: status %s", i, got.GetStatus())
		}
	}
	if _, err := c.GetZoneOperation(ctx, &computepb.GetZoneOperationRequest{Project: proj, Zone: SeedZoneA, Operation: "operation-999"}); httpCode(err) != 404 {
		t.Fatalf("unknown operation = %v", err)
	}
}

func TestUniqueNamePerZone(t *testing.T) {
	c := NewSeeded(proj)
	run(t, c, instance("dup", "e2-small"), SeedZoneA)
	_, err := c.InsertInstance(ctx, insertReq(instance("dup", "e2-small"), SeedZoneA, ""))
	if httpCode(err) != http.StatusConflict || reason(err) != "alreadyExists" {
		t.Fatalf("same zone = %v", err)
	}
	// Another zone is another namespace; the subnet is regional.
	in := instance("dup", "e2-small")
	in.MachineType = new("zones/" + SeedZoneB + "/machineTypes/e2-small")
	if op := run(t, c, in, SeedZoneB); op.GetError() != nil {
		t.Fatalf("other zone: %v", op.GetError())
	}
}

func TestRequestIDReplay(t *testing.T) {
	c := NewSeeded(proj)
	id := "11111111-1111-5111-8111-111111111111"
	op1, err := c.InsertInstance(ctx, insertReq(instance("n1", "e2-small"), SeedZoneA, id))
	if err != nil {
		t.Fatal(err)
	}
	// The replay returns the same operation, not a 409.
	op2, err := c.InsertInstance(ctx, insertReq(instance("n1", "e2-small"), SeedZoneA, id))
	if err != nil || op2.GetName() != op1.GetName() {
		t.Fatalf("replay = %v, %v", op2.GetName(), err)
	}
	if len(c.Instances()) != 1 {
		t.Fatalf("%d instances", len(c.Instances()))
	}
	// A different zone with the same ID is a different request.
	in := instance("n1", "e2-small")
	in.MachineType = new("zones/" + SeedZoneB + "/machineTypes/e2-small")
	op3, err := c.InsertInstance(ctx, insertReq(in, SeedZoneB, id))
	if err != nil || op3.GetName() == op1.GetName() {
		t.Fatalf("other zone replay = %v, %v", op3.GetName(), err)
	}
}

func TestAsyncErrors(t *testing.T) {
	codeOf := func(op *computepb.Operation) string {
		if op.GetError() == nil {
			return ""
		}
		return op.GetError().GetErrors()[0].GetCode()
	}
	t.Run("stockout", func(t *testing.T) {
		c := NewSeeded(proj)
		c.Stockout(SeedZoneA, "n2-standard-2", false, true)
		op := run(t, c, instance("n1", "n2-standard-2"), SeedZoneA)
		if codeOf(op) != "ZONE_RESOURCE_POOL_EXHAUSTED" || op.GetHttpErrorStatusCode() != 503 {
			t.Fatalf("op error = %v", op.GetError())
		}
		if len(c.Instances()) != 0 {
			t.Fatal("a failed insert must leave nothing behind")
		}
		// The failure is per zone, type and capacity type.
		c.Stockout(SeedZoneA, "n2-standard-2", false, false)
		if op := run(t, c, instance("n1", "n2-standard-2"), SeedZoneA); op.GetError() != nil {
			t.Fatalf("after the stockout: %v", op.GetError())
		}
	})
	t.Run("org policy", func(t *testing.T) {
		c := NewSeeded(proj)
		c.SetOrgPolicyViolation(true)
		if op := run(t, c, instance("n1", "e2-small"), SeedZoneA); codeOf(op) != "CONDITION_NOT_MET" {
			t.Fatalf("op error = %v", op.GetError())
		}
	})
	t.Run("injected operation error", func(t *testing.T) {
		c := NewSeeded(proj)
		c.InjectOpError("InsertInstance", "INTERNAL_ERROR", "x", 500)
		if op := run(t, c, instance("n1", "e2-small"), SeedZoneA); codeOf(op) != "INTERNAL_ERROR" {
			t.Fatalf("op error = %v", op.GetError())
		}
		if op := run(t, c, instance("n1", "e2-small"), SeedZoneA); op.GetError() != nil {
			t.Fatal("the injection must be consumed")
		}
	})
	t.Run("subnet full", func(t *testing.T) {
		c := NewSeeded(proj)
		// /28: 16 addresses, 4 reserved: 12 usable.
		c.mu.Lock()
		c.subnets[SeedRegion+"/"+SeedSubnet].IpCidrRange = new("10.240.0.0/28")
		c.mu.Unlock()
		c.SetLimit(SeedRegion, "CPUS", 1000)
		c.SetLimit("", "CPUS_ALL_REGIONS", 1000)
		for i := range 12 {
			if op := run(t, c, instance(fmt.Sprintf("n%d", i), "e2-small"), SeedZoneA); op.GetError() != nil {
				t.Fatalf("instance %d: %v", i, op.GetError())
			}
		}
		if op := run(t, c, instance("last", "e2-small"), SeedZoneA); codeOf(op) != "IP_SPACE_EXHAUSTED" {
			t.Fatalf("op error = %v", op.GetError())
		}
	})
}

func TestSyncErrors(t *testing.T) {
	c := NewSeeded(proj)
	tests := []struct {
		name   string
		mutate func(in *computepb.Instance)
		zone   string
		code   int
		reason string
	}{
		{"unknown zone", nil, "mars-north1-a", 404, "notFound"},
		{"machine type not offered", func(in *computepb.Instance) {
			in.MachineType = new("zones/" + SeedZoneA + "/machineTypes/m9-huge")
		}, SeedZoneA, 404, "notFound"},
		{"unknown image", func(in *computepb.Instance) {
			in.Disks[0].InitializeParams.SourceImage = new("projects/kinvolk-public/global/images/nope")
		}, SeedZoneA, 404, "notFound"},
		{"unknown subnet", func(in *computepb.Instance) {
			in.NetworkInterfaces[0].Subnetwork = new("projects/" + proj + "/regions/" + SeedRegion + "/subnetworks/nope")
		}, SeedZoneA, 404, "notFound"},
		{"subnet of another region", func(in *computepb.Instance) {
			in.MachineType = new("zones/" + SeedOtherZ + "/machineTypes/e2-small")
		}, SeedOtherZ, 400, "invalid"},
		{"arch mismatch", func(in *computepb.Instance) {
			in.MachineType = new("zones/" + SeedZoneA + "/machineTypes/t2a-standard-1")
		}, SeedZoneA, 400, "invalid"},
		{"spot migrate", func(in *computepb.Instance) {
			in.Scheduling.ProvisioningModel = new("SPOT")
		}, SeedZoneA, 400, "invalid"},
		{"no name", func(in *computepb.Instance) { in.Name = new("") }, SeedZoneA, 400, "invalid"},
		{"unknown policy", func(in *computepb.Instance) {
			in.ResourcePolicies = []string{"projects/" + proj + "/regions/" + SeedRegion + "/resourcePolicies/nope"}
		}, SeedZoneA, 404, "notFound"},
		{"ipv6 without range", func(in *computepb.Instance) {
			in.NetworkInterfaces[0].Ipv6AccessConfigs = []*computepb.AccessConfig{{Type: new("DIRECT_IPV6")}}
			c.mu.Lock()
			c.subnets[SeedRegion+"/"+SeedSubnet].StackType = new("IPV4_ONLY")
			c.mu.Unlock()
		}, SeedZoneA, 400, "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := instance("n1", "e2-small")
			if tc.mutate != nil {
				tc.mutate(in)
			}
			_, err := c.InsertInstance(ctx, insertReq(in, tc.zone, ""))
			if httpCode(err) != tc.code || reason(err) != tc.reason {
				t.Fatalf("error = %v (%d %s)", err, httpCode(err), reason(err))
			}
		})
	}
	// Another project is a permission error.
	r := insertReq(instance("n1", "e2-small"), SeedZoneA, "")
	r.Project = "other"
	if _, err := c.InsertInstance(ctx, r); httpCode(err) != 403 {
		t.Fatalf("other project = %v", err)
	}
}

func TestImages(t *testing.T) {
	c := NewSeeded(proj)
	img, err := c.GetImage(ctx, &computepb.GetImageRequest{Project: SeedImageProject, Image: SeedImage})
	if err != nil || img.GetArchitecture() != "X86_64" || len(img.GetGuestOsFeatures()) != 3 {
		t.Fatalf("image = %v, %v", img, err)
	}
	fam, err := c.GetImageFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: SeedImageProject, Family: "flatcar-stable"})
	if err != nil || fam.GetName() != SeedImage {
		t.Fatalf("family = %v, %v", fam.GetName(), err)
	}
	if _, err := c.GetImage(ctx, &computepb.GetImageRequest{Project: SeedImageProject, Image: "flatcar-stable-1-1-1"}); httpCode(err) != 404 {
		t.Fatalf("missing image = %v", err)
	}
	// A family reference works as a boot source.
	in := instance("n1", "e2-small")
	in.Disks[0].InitializeParams.SourceImage = new("projects/" + SeedImageProject + "/global/images/family/flatcar-stable")
	if op := run(t, c, in, SeedZoneA); op.GetError() != nil {
		t.Fatalf("family boot: %v", op.GetError())
	}
	// The arm64 image boots an arm64 type.
	arm := instance("a1", "t2a-standard-1")
	arm.Disks[0].InitializeParams.SourceImage = new("projects/" + SeedARMImageProject + "/global/images/" + SeedImageARM)
	if op := run(t, c, arm, SeedZoneA); op.GetError() != nil {
		t.Fatalf("arm boot: %v", op.GetError())
	}
}

func TestStatesAndQuota(t *testing.T) {
	c := NewSeeded(proj)
	c.AddForeignUsage(SeedRegion, "CPUS", 10)
	run(t, c, instance("n1", "n2-standard-2"), SeedZoneA)
	if got := c.Usage(SeedRegion, "CPUS"); got != 12 {
		t.Fatalf("CPUS usage = %v", got)
	}
	if got := c.Usage(SeedRegion, "N2_CPUS"); got != 2 {
		t.Fatalf("N2_CPUS usage = %v", got)
	}
	if got := c.Usage("", "CPUS_ALL_REGIONS"); got != 2 {
		t.Fatalf("CPUS_ALL_REGIONS usage = %v", got)
	}
	// e2 consumes only CPUS.
	run(t, c, instance("n2", "e2-small"), SeedZoneA)
	if got := c.Usage(SeedRegion, "E2_CPUS"); got != 0 {
		t.Fatalf("E2_CPUS usage = %v", got)
	}
	// A stopped instance does not consume CPU quota.
	c.SetStatus(SeedZoneA, "n1", "TERMINATED")
	if got := c.Usage(SeedRegion, "N2_CPUS"); got != 0 {
		t.Fatalf("N2_CPUS usage with a stopped instance = %v", got)
	}
	if c.SetStatus(SeedZoneA, "ghost", "STOPPED") {
		t.Fatal("SetStatus on a missing instance must report false")
	}
	reg, err := c.GetRegion(ctx, &computepb.GetRegionRequest{Project: proj, Region: SeedRegion})
	if err != nil {
		t.Fatal(err)
	}
	var cpus *computepb.Quota
	for _, q := range reg.GetQuotas() {
		if q.GetMetric() == "CPUS" {
			cpus = q
		}
	}
	if cpus == nil || cpus.GetLimit() != 24 || cpus.GetUsage() != 12 {
		t.Fatalf("region quota CPUS = %v", cpus)
	}
	p, err := c.GetProject(ctx, &computepb.GetProjectRequest{Project: proj})
	if err != nil || len(p.GetQuotas()) != 1 || p.GetQuotas()[0].GetMetric() != "CPUS_ALL_REGIONS" {
		t.Fatalf("project quotas = %v, %v", p.GetQuotas(), err)
	}
}

func TestQuotaExceeded(t *testing.T) {
	msgOf := func(op *computepb.Operation) string {
		if op.GetError() == nil {
			return ""
		}
		return op.GetError().GetErrors()[0].GetCode() + ": " + op.GetError().GetErrors()[0].GetMessage()
	}
	t.Run("family", func(t *testing.T) {
		c := NewSeeded(proj)
		for i := range 4 { // 4 x 2 = 8 N2_CPUS
			run(t, c, instance(fmt.Sprintf("n%d", i), "n2-standard-2"), SeedZoneA)
		}
		got := msgOf(run(t, c, instance("over", "n2-standard-2"), SeedZoneA))
		if !strings.HasPrefix(got, "QUOTA_EXCEEDED: Quota 'N2_CPUS' exceeded.  Limit: 8.0 in region europe-west3.") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("region", func(t *testing.T) {
		c := NewSeeded(proj)
		c.AddForeignUsage(SeedRegion, "CPUS", 23)
		got := msgOf(run(t, c, instance("over", "e2-small"), SeedZoneA))
		if !strings.Contains(got, "Quota 'CPUS' exceeded.  Limit: 24.0 in region europe-west3.") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("all regions", func(t *testing.T) {
		c := NewSeeded(proj)
		c.AddForeignUsage("", "CPUS_ALL_REGIONS", 31)
		got := msgOf(run(t, c, instance("over", "e2-small"), SeedZoneA))
		if !strings.Contains(got, "Quota 'CPUS_ALL_REGIONS' exceeded.  Limit: 32.0.") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("spot with preemptible quota", func(t *testing.T) {
		c := NewSeeded(proj)
		c.SetLimit(SeedRegion, "PREEMPTIBLE_CPUS", 2)
		spot := instance("s1", "e2-small")
		spot.Scheduling = &computepb.Scheduling{ProvisioningModel: new("SPOT"), OnHostMaintenance: new("TERMINATE")}
		if op := run(t, c, spot, SeedZoneA); op.GetError() != nil {
			t.Fatalf("first spot: %v", op.GetError())
		}
		if got := c.Usage(SeedRegion, "PREEMPTIBLE_CPUS"); got != 2 {
			t.Fatalf("PREEMPTIBLE_CPUS usage = %v", got)
		}
		if got := c.Usage(SeedRegion, "CPUS"); got != 0 {
			t.Fatalf("a spot VM must not use CPUS once the region has preemptible quota: %v", got)
		}
		spot2 := proto.Clone(spot).(*computepb.Instance)
		spot2.Name = new("s2")
		if got := msgOf(run(t, c, spot2, SeedZoneA)); !strings.Contains(got, "Quota 'PREEMPTIBLE_CPUS' exceeded") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("spot without preemptible quota uses standard", func(t *testing.T) {
		c := NewSeeded(proj)
		spot := instance("s1", "e2-small")
		spot.Scheduling = &computepb.Scheduling{ProvisioningModel: new("SPOT"), OnHostMaintenance: new("TERMINATE")}
		run(t, c, spot, SeedZoneA)
		if got := c.Usage(SeedRegion, "CPUS"); got != 2 {
			t.Fatalf("CPUS usage = %v", got)
		}
	})
}

func labelFilter(f string) *computepb.ListInstancesRequest {
	return &computepb.ListInstancesRequest{Project: proj, Zone: SeedZoneA, Filter: new(f)}
}

func TestFilterAndPaging(t *testing.T) {
	c := NewSeeded(proj)
	for i := range 5 {
		in := instance(fmt.Sprintf("n%d", i), "e2-small")
		in.Labels["scarpenter-nodeclaim"] = fmt.Sprintf("claim-%d", i)
		if i%2 == 0 {
			in.Tags = &computepb.Tags{Items: []string{"even"}}
		}
		run(t, c, in, SeedZoneA)
	}
	tests := []struct {
		filter string
		want   int
	}{
		{`labels.scarpenter-cluster = "c1"`, 5},
		{`(labels.scarpenter-cluster = "c1") AND (labels.scarpenter-nodeclaim = "claim-3")`, 1},
		{`labels.scarpenter-cluster = "other"`, 0},
		{`name = "n1"`, 1},
		{`name eq n[0-2]`, 3},
		{`name ne n1`, 4},
		{`tags.items = even`, 3},
		{`labels.missing != x`, 5},
		{`status = RUNNING`, 5},
		{``, 5},
	}
	for _, tc := range tests {
		got, err := c.ListInstances(ctx, labelFilter(tc.filter))
		if err != nil || len(got.GetItems()) != tc.want {
			t.Errorf("filter %q: %d items, %v; want %d", tc.filter, len(got.GetItems()), err, tc.want)
		}
	}
	for _, bad := range []string{`labels.a = x OR labels.b = y`, `nonsense`, `bogus = 1`, `name = "("`} {
		if _, err := c.ListInstances(ctx, labelFilter(bad)); httpCode(err) != 400 {
			t.Errorf("filter %q = %v, want 400", bad, err)
		}
	}
	// Paging: pages of 2 cover everything once, in order.
	var names []string
	req := &computepb.ListInstancesRequest{Project: proj, Zone: SeedZoneA, MaxResults: proto.Uint32(2)}
	pages := 0
	for {
		page, err := c.ListInstances(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, in := range page.GetItems() {
			names = append(names, in.GetName())
		}
		if page.NextPageToken == nil {
			break
		}
		req.PageToken = page.NextPageToken
	}
	if pages != 3 || strings.Join(names, ",") != "n0,n1,n2,n3,n4" {
		t.Fatalf("pages = %d, names = %v", pages, names)
	}
	if _, err := c.ListInstances(ctx, &computepb.ListInstancesRequest{Project: proj, Zone: SeedZoneA, PageToken: new("zzz")}); httpCode(err) != 400 {
		t.Fatalf("bad token = %v", err)
	}
}

func TestAggregatedList(t *testing.T) {
	c := NewSeeded(proj)
	run(t, c, instance("a1", "e2-small"), SeedZoneA)
	in := instance("b1", "e2-small")
	in.MachineType = new("zones/" + SeedZoneB + "/machineTypes/e2-small")
	run(t, c, in, SeedZoneB)
	req := &computepb.AggregatedListInstancesRequest{Project: proj, Filter: new(`labels.scarpenter-cluster = "c1"`)}
	got, err := c.AggregatedListInstances(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || len(got.Items["zones/"+SeedZoneA].Instances) != 1 || len(got.Items["zones/"+SeedZoneB].Instances) != 1 {
		t.Fatalf("items = %v", got.Items)
	}
	// An unreachable zone is skipped and named.
	c.SetZoneUnreachable(SeedZoneB, true)
	got, err = c.AggregatedListInstances(ctx, req)
	if err != nil || len(got.Items) != 1 || len(got.Unreachables) != 1 || got.Unreachables[0] != "zones/"+SeedZoneB {
		t.Fatalf("partial = %v, %v, %v", got.Items, got.Unreachables, err)
	}
	// Paged aggregated list.
	c.SetZoneUnreachable(SeedZoneB, false)
	req.MaxResults = proto.Uint32(1)
	page1, _ := c.AggregatedListInstances(ctx, req)
	if page1.NextPageToken == nil {
		t.Fatal("the first page must have a next token")
	}
	req.PageToken = page1.NextPageToken
	page2, _ := c.AggregatedListInstances(ctx, req)
	if page2.NextPageToken != nil || len(page2.Items) != 1 {
		t.Fatalf("second page = %v", page2.Items)
	}
}

func TestZonesRegionsMachineTypes(t *testing.T) {
	c := NewSeeded(proj)
	zs, err := c.ListZones(ctx, &computepb.ListZonesRequest{Project: proj, Filter: new(`status = UP`)})
	if err != nil || len(zs.Items) != 3 {
		t.Fatalf("zones = %d, %v", len(zs.GetItems()), err)
	}
	for _, z := range zs.Items {
		if z.GetName() == SeedZoneA && z.GetId() != 2000 {
			t.Fatalf("zone id = %d", z.GetId())
		}
	}
	c.SetZoneStatus(SeedZoneB, "DOWN")
	zs, _ = c.ListZones(ctx, &computepb.ListZonesRequest{Project: proj, Filter: new(`status = UP`)})
	if len(zs.Items) != 2 {
		t.Fatalf("UP zones = %d", len(zs.Items))
	}
	rs, err := c.ListRegions(ctx, &computepb.ListRegionsRequest{Project: proj})
	if err != nil || len(rs.Items) != 2 || len(rs.Items[0].Quotas) == 0 {
		t.Fatalf("regions = %v, %v", rs, err)
	}
	if _, err := c.GetRegion(ctx, &computepb.GetRegionRequest{Project: proj, Region: "nowhere1"}); httpCode(err) != 404 {
		t.Fatalf("missing region = %v", err)
	}
	mts, err := c.ListMachineTypes(ctx, &computepb.ListMachineTypesRequest{Project: proj, Zone: SeedZoneA, MaxResults: proto.Uint32(3)})
	if err != nil || len(mts.Items) != 3 || mts.NextPageToken == nil {
		t.Fatalf("machine types = %d, %v", len(mts.GetItems()), err)
	}
	c.RemoveMachineType(SeedZoneA, "e2-small")
	mts, _ = c.ListMachineTypes(ctx, &computepb.ListMachineTypesRequest{Project: proj, Zone: SeedZoneA})
	for _, mt := range mts.Items {
		if mt.GetName() == "e2-small" {
			t.Fatal("the removed machine type is still listed")
		}
	}
	if _, err := c.ListMachineTypes(ctx, &computepb.ListMachineTypesRequest{Project: proj, Zone: "mars-north1-a"}); httpCode(err) != 404 {
		t.Fatalf("missing zone = %v", err)
	}
}

func TestNetworkResources(t *testing.T) {
	c := NewSeeded(proj)
	waitGlobal := func(op *computepb.Operation) {
		t.Helper()
		if _, err := c.WaitGlobalOperation(ctx, &computepb.WaitGlobalOperationRequest{Project: proj, Operation: op.GetName()}); err != nil {
			t.Fatal(err)
		}
	}
	// No default network in the seeded project.
	if _, err := c.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: proj, Network: "default"}); httpCode(err) != 404 {
		t.Fatalf("default network = %v", err)
	}
	op, err := c.InsertNetwork(ctx, &computepb.InsertNetworkRequest{Project: proj, NetworkResource: &computepb.Network{
		Name: new("scarpenter-c1-subnets"), AutoCreateSubnetworks: new(false), Description: new("scarpenter: x"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitGlobal(op)
	if _, err := c.InsertNetwork(ctx, &computepb.InsertNetworkRequest{Project: proj, NetworkResource: &computepb.Network{Name: new("scarpenter-c1-subnets")}}); httpCode(err) != 409 {
		t.Fatalf("duplicate network = %v", err)
	}
	netURL := "projects/" + proj + "/global/networks/scarpenter-c1-subnets"
	subReq := func(name, cidr string) *computepb.InsertSubnetworkRequest {
		return &computepb.InsertSubnetworkRequest{Project: proj, Region: SeedRegion, SubnetworkResource: &computepb.Subnetwork{
			Name: new(name), Network: new(netURL), IpCidrRange: new(cidr), StackType: new("IPV4_IPV6"),
		}}
	}
	if _, err := c.InsertSubnetwork(ctx, subReq("scarpenter-c1-subnets-europe-west3", "10.240.0.0/20")); err != nil {
		t.Fatal(err)
	}
	// Overlap is refused within a network, the same CIDR in the seeded VPC is fine.
	if _, err := c.InsertSubnetwork(ctx, subReq("overlap", "10.240.8.0/24")); httpCode(err) != 400 {
		t.Fatalf("overlap = %v", err)
	}
	// The network is global: a range taken in one region is taken in the others, with
	// the message of the real API; secondary ranges count.
	other := subReq("elsewhere", "10.240.0.0/22")
	other.Region = SeedOtherReg
	if _, err := c.InsertSubnetwork(ctx, other); httpCode(err) != 400 || !strings.Contains(err.Error(),
		"Invalid IPCidrRange: 10.240.0.0/22 conflicts with existing subnetwork 'scarpenter-c1-subnets-europe-west3' in region 'europe-west3'") {
		t.Fatalf("cross-region overlap = %v", err)
	}
	sec := subReq("withsecondary", "10.242.0.0/20")
	sec.Region = SeedOtherReg
	sec.SubnetworkResource.SecondaryIpRanges = []*computepb.SubnetworkSecondaryRange{{RangeName: new("pods"), IpCidrRange: new("10.243.0.0/16")}}
	if _, err := c.InsertSubnetwork(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertSubnetwork(ctx, subReq("insecondary", "10.243.4.0/22")); httpCode(err) != 400 {
		t.Fatalf("overlap with a secondary range = %v", err)
	}
	if _, err := c.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{Project: proj, Region: SeedOtherReg, Subnetwork: "withsecondary"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertSubnetwork(ctx, subReq("scarpenter-c1-subnets-europe-west3", "10.241.0.0/20")); httpCode(err) != 409 {
		t.Fatalf("duplicate subnet = %v", err)
	}
	bad := subReq("x", "not-a-cidr")
	if _, err := c.InsertSubnetwork(ctx, bad); httpCode(err) != 400 {
		t.Fatalf("bad cidr = %v", err)
	}
	bad = subReq("x", "10.250.0.0/20")
	bad.SubnetworkResource.Network = new("projects/" + proj + "/global/networks/nope")
	if _, err := c.InsertSubnetwork(ctx, bad); httpCode(err) != 404 {
		t.Fatalf("missing network = %v", err)
	}
	n, _ := c.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: proj, Network: "scarpenter-c1-subnets"})
	if len(n.Subnetworks) != 1 {
		t.Fatalf("network subnetworks = %v", n.Subnetworks)
	}
	subs, err := c.ListSubnetworks(ctx, &computepb.ListSubnetworksRequest{Project: proj, Region: SeedRegion, Filter: new(`name = "scarpenter-c1-.*"`)})
	if err != nil || len(subs.Items) != 1 {
		t.Fatalf("subnets = %v, %v", subs, err)
	}

	// A network with a subnet or a firewall rule is in use.
	if _, err := c.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: proj, Network: "scarpenter-c1-subnets"}); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("delete network with subnet = %v", err)
	}
	fw, err := c.InsertFirewall(ctx, &computepb.InsertFirewallRequest{Project: proj, FirewallResource: &computepb.Firewall{
		Name: new("scarpenter-c1-nc-europe-west3"), Network: new(netURL), TargetTags: []string{"scarpenter-c1-nc-europe-west3"},
		Allowed: []*computepb.Allowed{{IPProtocol: new("udp"), Ports: []string{"41641"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitGlobal(fw)
	if _, err := c.InsertFirewall(ctx, &computepb.InsertFirewallRequest{Project: proj, FirewallResource: &computepb.Firewall{Name: new("f"), Network: new("projects/" + proj + "/global/networks/nope")}}); httpCode(err) != 404 {
		t.Fatalf("firewall on missing network = %v", err)
	}
	fws, err := c.ListFirewalls(ctx, &computepb.ListFirewallsRequest{Project: proj, Filter: new(`name = "scarpenter-c1-.*"`)})
	if err != nil || len(fws.Items) != 1 {
		t.Fatalf("firewalls = %v, %v", fws, err)
	}
	if _, err := c.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{Project: proj, Region: SeedRegion, Subnetwork: "scarpenter-c1-subnets-europe-west3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: proj, Network: "scarpenter-c1-subnets"}); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("delete network with firewall = %v", err)
	}
	if _, err := c.DeleteFirewall(ctx, &computepb.DeleteFirewallRequest{Project: proj, Firewall: "scarpenter-c1-nc-europe-west3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteFirewall(ctx, &computepb.DeleteFirewallRequest{Project: proj, Firewall: "scarpenter-c1-nc-europe-west3"}); httpCode(err) != 404 {
		t.Fatalf("second firewall delete = %v", err)
	}
	if _, err := c.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: proj, Network: "scarpenter-c1-subnets"}); err != nil {
		t.Fatal(err)
	}

	// The seeded subnet is in use by an instance.
	run(t, c, instance("n1", "e2-small"), SeedZoneA)
	if _, err := c.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{Project: proj, Region: SeedRegion, Subnetwork: SeedSubnet}); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("delete subnet in use = %v", err)
	}
}

func TestDefaultNetwork(t *testing.T) {
	c := NewSeeded(proj)
	c.AddDefaultNetwork()
	n, err := c.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: proj, Network: "default"})
	if err != nil || len(n.Subnetworks) != 2 {
		t.Fatalf("default network = %v, %v", n, err)
	}
	s, err := c.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: proj, Region: SeedRegion, Subnetwork: "default"})
	if err != nil || s.GetIpCidrRange() == "" {
		t.Fatalf("default subnet = %v, %v", s, err)
	}
}

func TestResourcePolicies(t *testing.T) {
	c := NewSeeded(proj)
	op, err := c.InsertResourcePolicy(ctx, &computepb.InsertResourcePolicyRequest{Project: proj, Region: SeedRegion, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: new("scarpenter-c1-compact"), Description: new("scarpenter: x"),
		GroupPlacementPolicy: &computepb.ResourcePolicyGroupPlacementPolicy{Collocation: new("COLLOCATED")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitRegionOperation(ctx, &computepb.WaitRegionOperationRequest{Project: proj, Region: SeedRegion, Operation: op.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertResourcePolicy(ctx, &computepb.InsertResourcePolicyRequest{Project: proj, Region: SeedRegion, ResourcePolicyResource: &computepb.ResourcePolicy{Name: new("scarpenter-c1-compact")}}); httpCode(err) != 409 {
		t.Fatalf("duplicate = %v", err)
	}
	in := instance("n1", "n2-standard-2")
	in.ResourcePolicies = []string{"projects/" + proj + "/regions/" + SeedRegion + "/resourcePolicies/scarpenter-c1-compact"}
	run(t, c, in, SeedZoneA)
	// In use: refused until the instance is gone.
	del := &computepb.DeleteResourcePolicyRequest{Project: proj, Region: SeedRegion, ResourcePolicy: "scarpenter-c1-compact"}
	if _, err := c.DeleteResourcePolicy(ctx, del); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("delete in use = %v", err)
	}
	dop, _ := c.DeleteInstance(ctx, &computepb.DeleteInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "n1"})
	if _, err := c.WaitZoneOperation(ctx, &computepb.WaitZoneOperationRequest{Project: proj, Zone: SeedZoneA, Operation: dop.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteResourcePolicy(ctx, del); err != nil {
		t.Fatalf("delete after = %v", err)
	}
	if _, err := c.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: proj, Region: SeedRegion, ResourcePolicy: "scarpenter-c1-compact"}); httpCode(err) != 404 {
		t.Fatalf("get deleted = %v", err)
	}
	ps, err := c.ListResourcePolicies(ctx, &computepb.ListResourcePoliciesRequest{Project: proj, Region: SeedRegion})
	if err != nil || len(ps.Items) != 0 {
		t.Fatalf("list = %v, %v", ps, err)
	}
}

func TestInjectAndCalls(t *testing.T) {
	c := NewSeeded(proj)
	boom := Error(http.StatusServiceUnavailable, "backendError", "boom")
	c.Inject("GetInstance", boom, 2)
	for range 2 {
		if _, err := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "x"}); err != boom {
			t.Fatalf("injected = %v", err)
		}
	}
	if _, err := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "x"}); httpCode(err) != 404 {
		t.Fatalf("after the injections = %v", err)
	}
	if got := c.Calls("GetInstance"); got != 3 {
		t.Fatalf("calls = %d", got)
	}
}

func TestNetworkIPAndIPv6(t *testing.T) {
	c := NewSeeded(proj)
	in := instance("v6", "e2-small")
	in.NetworkInterfaces[0].StackType = new("IPV4_IPV6")
	in.NetworkInterfaces[0].Ipv6AccessConfigs = []*computepb.AccessConfig{{Type: new("DIRECT_IPV6")}}
	run(t, c, in, SeedZoneA)
	got, _ := c.GetInstance(ctx, &computepb.GetInstanceRequest{Project: proj, Zone: SeedZoneA, Instance: "v6"})
	nic := got.NetworkInterfaces[0]
	if nic.GetIpv6Address() == "" || nic.GetIpv6AccessConfigs()[0].GetExternalIpv6() == "" {
		t.Fatalf("no IPv6: %v", nic)
	}
}
