/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// resolveSettings resolves the settings of a node like the CloudProvider does:
// defaults, the NodeClass's vendor block, the selected profiles.
func resolveSettings(t testing.TB, d *aws.Driver, block string, networking common.Networking, profiles vendors.Profiles, sel options.Selection,
) vendors.VendorSettings {
	t.Helper()
	if block == "" {
		block = "{}"
	}
	s, err := options.Resolve(d, options.Input{Block: json.RawMessage(block), Networking: networking, Profiles: profiles}, sel)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runInput(t *testing.T, e *env) *ec2.RunInstancesInput {
	t.Helper()
	in := e.acct.Inputs("RunInstances")
	if len(in) == 0 {
		t.Fatal("no RunInstances call")
	}
	return in[len(in)-1].(*ec2.RunInstancesInput)
}

func golden(t *testing.T, name string, in *ec2.RunInstancesInput) {
	t.Helper()
	raw, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, name, append(raw, '\n'))
}

func subnetOf(e *env, region, zone string) string { return e.acct.DefaultSubnets(region)[zone] }

func TestRunInstancesInputGoldenDefaults(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Tags[vendors.TagNodeClaim] = "claim-1"
	req.IdempotencyKey = "11111111-2222-3333-4444-555555555555"
	req.Settings = resolveSettings(t, e.d, "", common.Networking{}, nil, options.Selection{})
	req.ExtraTags = map[string]string{"team": "data", "Name": "overridden-by-the-nodeclaim-name"}
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: aws.KindSubnets, ID: subnetOf(e, testRegion, testZone)},
	}}
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	in := runInput(t, e)
	in.NetworkInterfaces[0].SubnetId = new("subnet-redacted") // IDs of the fake depend on creation order
	golden(t, "runinstances_defaults.golden.json", in)
}

func TestRunInstancesInputGoldenEverything(t *testing.T) {
	e := newEnv(t, envOptions{})
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {"db": json.RawMessage(`{"data":[
			{"name":"pgdata","volumeType":"gp3","volumeSize":"100Gi","iops":4000,"throughput":250,"mountPath":"/var/lib/pgdata","filesystem":"xfs"},
			{"name":"wal","volumeType":"io2","volumeSize":"50Gi","iops":5000,"encrypted":true,"kmsKeyID":"alias/my-key","mountPath":"/wal"}]}`)},
		vendors.ProfileNetwork:   {"fast": json.RawMessage(`{"enaExpress":{"enabled":true,"udp":true}}`)},
		vendors.ProfilePlacement: {"pinned": json.RawMessage(`{"placementGroup":{"strategy":"cluster"},"tenancy":"dedicated"}`)},
	}
	block := `{"storage":{"root":{"volumeType":"gp3","volumeSize":"128Gi","iops":6000,"throughput":500,"kmsKeyID":"arn:aws:kms:us-east-1:123456789012:key/example"}},` +
		`"instanceProfile":"my-node-profile","metadataOptions":{"httpPutResponseHopLimit":2}}`
	req := request(testRegion, "us-east-1b", "c7gn.large", vendors.CapacitySpot)
	req.Arch = "arm64"
	req.Tags[vendors.TagNodeClaim] = "claim-2"
	req.IdempotencyKey = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	req.Settings = resolveSettings(t, e.d, block, common.Networking{PublicIPv4: new(false), PublicIPv6: new(true)}, profiles,
		options.Selection{Storage: "db", Network: "fast", Placement: "pinned"})
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: aws.KindSubnets, ID: subnetOf(e, testRegion, "us-east-1b")},
		{Kind: aws.KindSecurityGroups, ID: defaultSecurityGroup(t, e)},
	}}
	// A placement group the launch uses.
	pg := e.acct.EC2(testRegion)
	if _, err := pg.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: new("my-group"), Strategy: ec2types.PlacementStrategyCluster}); err != nil {
		t.Fatal(err)
	}
	req.Prepared.Resources = append(req.Prepared.Resources, vendors.ResourceRef{Kind: aws.KindPlacementGroups, Name: "my-group", ID: "pg-1"})
	// ipv6 and public ip off need a subnet; the default subnet has none, the
	// fake does not check it.
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	in := runInput(t, e)
	in.NetworkInterfaces[0].SubnetId = new("subnet-redacted")
	in.NetworkInterfaces[0].Groups = []string{"sg-redacted"}
	golden(t, "runinstances_everything.golden.json", in)
}

func defaultSecurityGroup(t *testing.T, e *env) string {
	t.Helper()
	out, err := e.acct.EC2(testRegion).DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{Name: new("group-name"), Values: []string{"default"}}, {Name: new("vpc-id"), Values: []string{defaultVPC(&testing.T{}, e, testRegion)}}},
	})
	if err != nil || len(out.SecurityGroups) == 0 {
		t.Fatalf("default group: %v", err)
	}
	return *out.SecurityGroups[0].GroupId
}

func TestIMDSv2WithHopLimitOneByDefault(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Settings = e.d.Defaults()
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	m := runInput(t, e).MetadataOptions
	if m.HttpTokens != ec2types.HttpTokensStateRequired || deref(m.HttpPutResponseHopLimit) != 1 ||
		m.HttpEndpoint != ec2types.InstanceMetadataEndpointStateEnabled || m.InstanceMetadataTags != ec2types.InstanceMetadataTagsStateDisabled {
		t.Errorf("metadata options = %+v, want IMDSv2 required, hop limit 1, tags in metadata off (overview section 8.6)", m)
	}
	// Even a launch with no settings at all gets it.
	req2 := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	if _, err := e.d.CreateInstance(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	if m := runInput(t, e).MetadataOptions; m.HttpTokens != ec2types.HttpTokensStateRequired || deref(m.HttpPutResponseHopLimit) != 1 {
		t.Errorf("without settings: %+v", m)
	}
}

func TestIdempotentCreateReturnsTheOriginal(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	first, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := deref(runInput(t, e).ClientToken); got != req.IdempotencyKey {
		t.Errorf("ClientToken %q, want the idempotency key %q", got, req.IdempotencyKey)
	}
	again, err := e.d.CreateInstance(ctx, req)
	if err != nil || again.Ref != first.Ref {
		t.Fatalf("a replay returned %v, %v; want %s", again, err, first.Ref)
	}
	if n := e.acct.Calls("RunInstances"); n != 2 {
		t.Errorf("%d RunInstances calls, want 2 (EC2 answers an identical replay itself)", n)
	}
}

func TestIdempotentParameterMismatchResolvesToTheOriginalInstance(t *testing.T) {
	// EC2 refuses a replay of a ClientToken with other parameters (another zone,
	// type or capacity type) with IdempotentParameterMismatch. The contract says
	// the replay returns the original instance (plan 04 review).
	for name, mutate := range map[string]func(*vendors.CreateRequest){
		"zone":          func(r *vendors.CreateRequest) { r.Zone = "us-east-1b" },
		"type":          func(r *vendors.CreateRequest) { r.InstanceType = "m7i.xlarge" },
		"capacity type": func(r *vendors.CreateRequest) { r.CapacityType = vendors.CapacitySpot },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			ctx := context.Background()
			req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
			first, err := e.d.CreateInstance(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			replay := req
			mutate(&replay)
			got, err := e.d.CreateInstance(ctx, replay)
			if err != nil {
				t.Fatalf("the replay failed: %v", err)
			}
			if got.Ref != first.Ref || got.Zone != first.Zone || got.Type != first.Type || got.CapacityType != first.CapacityType {
				t.Errorf("the replay returned %+v, want the original %+v", got, first)
			}
			if n := len(e.acct.InstanceStates(testRegion)); n != 1 {
				t.Errorf("%d instances, want 1", n)
			}
		})
	}
}

func TestMismatchOfATerminatedOriginalStillResolves(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	first, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.DeleteInstance(ctx, first.Ref); err != nil {
		t.Fatal(err)
	}
	req.Zone = "us-east-1b"
	got, err := e.d.CreateInstance(ctx, req)
	if err != nil || got.Ref != first.Ref || !got.Gone() {
		t.Errorf("replay of a terminated instance: %+v, %v; want the original, gone", got, err)
	}
}

func TestAFailedCreateLeavesTheKeyUnused(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	e.acct.Fail(fake.Failure{Op: "RunInstances", Code: "InsufficientInstanceCapacity"})
	if _, err := e.d.CreateInstance(ctx, req); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("got %v, want InsufficientCapacity", err)
	}
	req.Zone = "us-east-1b"
	inst, err := e.d.CreateInstance(ctx, req)
	if err != nil || inst.Zone != "us-east-1b" {
		t.Errorf("the next attempt with the same key: %+v, %v", inst, err)
	}
}

func TestMismatchWithoutAnOriginalIsFatal(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	e.acct.Fail(fake.Failure{Op: "RunInstances", Code: "IdempotentParameterMismatch"})
	if _, err := e.d.CreateInstance(context.Background(), req); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a mismatch for a token no instance has: %v", err)
	}
}

func TestUserDataLimit(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.UserData = []byte(strings.Repeat("x", 16*1024))
	if _, err := e.d.CreateInstance(ctx, req); err != nil {
		t.Fatalf("user-data of exactly 16 KiB: %v", err)
	}
	calls := e.acct.Calls("RunInstances")
	req = request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.UserData = []byte(strings.Repeat("x", 16*1024+1))
	err := func() error { _, err := e.d.CreateInstance(ctx, req); return err }()
	if vendors.ClassOf(err) != vendors.Fatal || !strings.Contains(err.Error(), "16384") {
		t.Errorf("one byte too much: %v, want Fatal naming the limit", err)
	}
	if e.acct.Calls("RunInstances") != calls {
		t.Error("an oversized user-data reached EC2")
	}
	// The user-data is sent base64-encoded.
	enc := deref(runInput(t, e).UserData)
	if raw, err := base64.StdEncoding.DecodeString(enc); err != nil || len(raw) != 16*1024 {
		t.Errorf("user-data on the wire: %d bytes, %v", len(raw), err)
	}
}

func TestRootVolumeSmallerThanTheImageIsFatal(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Image.RootDiskBytes = 100 << 30
	req.Settings = e.d.Defaults() // 64 GiB
	err := func() error { _, err := e.d.CreateInstance(context.Background(), req); return err }()
	if vendors.ClassOf(err) != vendors.Fatal || !strings.Contains(err.Error(), "100 GiB") {
		t.Errorf("got %v", err)
	}
}

func TestRootDeviceNameComesFromTheImage(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.acct.AddImage(testRegion, fake.Image{ID: "ami-0000000000sda1", Name: "other", Owner: "x", RootDevice: "/dev/sda1"})
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Image.Ref = "ami-0000000000sda1"
	req.Settings = e.d.Defaults()
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := deref(runInput(t, e).BlockDeviceMappings[0].DeviceName); got != "/dev/sda1" {
		t.Errorf("root device %q, want the image's /dev/sda1", got)
	}
	// An image DescribeImages does not know falls back to Flatcar's.
	req = request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Settings = e.d.Defaults()
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := deref(runInput(t, e).BlockDeviceMappings[0].DeviceName); got != "/dev/xvda" {
		t.Errorf("root device %q, want /dev/xvda", got)
	}
}

func TestLaunchWithoutPreparedResourcesUsesTheDefaultSubnet(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(testRegion, "us-east-1c", "m7i.large", vendors.CapacityOnDemand)
	if _, err := e.d.CreateInstance(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got, want := deref(runInput(t, e).NetworkInterfaces[0].SubnetId), subnetOf(e, testRegion, "us-east-1c"); got != want {
		t.Errorf("subnet %q, want the default subnet of the zone %q", got, want)
	}
	// A zone the account has no subnet in is unusable.
	req = request(testRegion, "us-east-1z", "m7i.large", vendors.CapacityOnDemand)
	if _, err := e.d.CreateInstance(context.Background(), req); !vendors.IsClass(err, vendors.ZoneUnusable) {
		t.Errorf("a zone without a subnet: %v, want ZoneUnusable", err)
	}
}

func TestInstanceStatesAndAddresses(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	inst, err := e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand))
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != vendors.StateRunning || inst.Zone != testZone || inst.Type != "m7i.large" || inst.Region != testRegion {
		t.Errorf("instance %+v", inst)
	}
	if len(inst.Addresses) != 2 || !strings.HasPrefix(inst.Addresses[0], "10.") || !strings.HasPrefix(inst.Addresses[1], "203.") {
		t.Errorf("addresses %v, want the private and the public IPv4", inst.Addresses)
	}
	id := inst.Ref.ID()
	for state, want := range map[ec2types.InstanceStateName]string{
		ec2types.InstanceStateNamePending:      vendors.StatePending,
		ec2types.InstanceStateNameStopping:     vendors.StateRunning,
		ec2types.InstanceStateNameStopped:      vendors.StateRunning,
		ec2types.InstanceStateNameShuttingDown: vendors.StateTerminating,
		ec2types.InstanceStateNameTerminated:   vendors.StateTerminated,
	} {
		e.acct.SetInstanceState(testRegion, id, state)
		got, err := e.d.GetInstance(ctx, inst.Ref)
		if err != nil || got.State != want {
			t.Errorf("EC2 state %s: %+v, %v; want %s", state, got, err, want)
		}
	}
	spot, _ := e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", vendors.CapacitySpot))
	if spot.CapacityType != vendors.CapacitySpot {
		t.Errorf("a spot instance is %q", spot.CapacityType)
	}
}

func TestTagsRoundTripThroughTheTagKeyMapping(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, k := range vendors.TagKeys {
		if got, want := e.d.TagKey(k), "scarpenter.sparecores.com/"+string(k); got != want {
			t.Errorf("TagKey(%s) = %q, want %q", k, got, want)
		}
	}
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.ExtraTags = map[string]string{"team": "data", "scarpenter.sparecores.com/cluster": "forged"}
	inst, err := e.d.CreateInstance(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Tags[vendors.TagCluster] != testCluster {
		t.Errorf("a NodeClass tag overrode the cluster tag: %v", inst.Tags)
	}
	tags := map[string]string{}
	for _, ts := range runInput(t, e).TagSpecifications {
		got := map[string]string{}
		for _, tg := range ts.Tags {
			got[*tg.Key] = *tg.Value
		}
		if tags[string(ts.ResourceType)] != "" {
			t.Fatal("duplicate resource type")
		}
		tags[string(ts.ResourceType)] = "x"
		if got["Name"] != req.Tags[vendors.TagNodeClaim] || got["team"] != "data" {
			t.Errorf("%s tags %v: want Name=<nodeclaim> and the NodeClass's tags", ts.ResourceType, got)
		}
	}
	for _, rt := range []string{"instance", "volume", "network-interface"} {
		if tags[rt] == "" {
			t.Errorf("no tag specification for %s", rt)
		}
	}
}

func TestFindListAndDelete(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	refs := make([]vendors.InstanceRef, 0, 7)
	for range 7 { // more than a page
		inst, err := e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, inst.Ref)
	}
	other := request("eu-central-1", "eu-central-1a", "m7i.large", vendors.CapacityOnDemand)
	other.Tags[vendors.TagCluster] = "another-cluster"
	if _, err := e.d.CreateInstance(ctx, other); err != nil {
		t.Fatal(err)
	}
	cluster := map[vendors.TagKey]string{vendors.TagCluster: testCluster}
	list, err := e.d.ListInstances(ctx, []string{testRegion, "eu-central-1", "nowhere-1"}, cluster)
	if err != nil || len(list) != 7 {
		t.Fatalf("listed %d instances, %v; want the 7 of this cluster (another cluster's and unknown regions excluded)", len(list), err)
	}
	if err := e.d.DeleteInstance(ctx, refs[0]); err != nil {
		t.Fatal(err)
	}
	e.acct.SetInstanceState(testRegion, refs[1].ID(), ec2types.InstanceStateNameShuttingDown)
	list, _ = e.d.ListInstances(ctx, []string{testRegion}, cluster)
	if len(list) != 5 {
		t.Errorf("listed %d, want 5 (terminated and shutting-down ones are gone)", len(list))
	}
	// Find picks the newest of the instances that are not gone.
	byClaim := map[vendors.TagKey]string{vendors.TagNodeClaim: list[0].Tags[vendors.TagNodeClaim]}
	found, err := e.d.FindInstance(ctx, testRegion, byClaim)
	if err != nil || found.Ref != list[0].Ref {
		t.Errorf("FindInstance = %v, %v", found, err)
	}
	if _, err := e.d.FindInstance(ctx, "nowhere-1", byClaim); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance in an unknown region: %v", err)
	}
	for _, ref := range []vendors.InstanceRef{refs[0], "us-east-1/i-00000000000000000", "us-east-1/not-an-instance", "us-east-1/"} {
		if err := e.d.DeleteInstance(ctx, ref); err != nil {
			t.Errorf("DeleteInstance(%s) = %v; a gone instance is success", ref, err)
		}
	}
	if _, err := e.d.GetInstance(ctx, "us-east-1/not-an-instance"); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance of a malformed ID: %v", err)
	}
	if _, err := e.d.GetInstance(ctx, "us-east-1/"); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance without an ID: %v", err)
	}
}

func TestDeleteErrorsAreClassified(t *testing.T) {
	e := newEnv(t, envOptions{})
	inst, err := e.d.CreateInstance(context.Background(), request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand))
	if err != nil {
		t.Fatal(err)
	}
	e.acct.Fail(fake.Failure{Op: "TerminateInstances", Code: "RequestLimitExceeded"})
	if err := e.d.DeleteInstance(context.Background(), inst.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("throttled delete: %v", err)
	}
}

func TestPerCallTimeouts(t *testing.T) {
	// A call that hangs ends with a Retryable error when its deadline passes
	// (plan 04 review N4): for the API calls and for RunInstances.
	for _, op := range []string{"DescribeInstances", "RunInstances", "DescribeAvailabilityZones", "GetServiceQuota"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t, envOptions{timeout: 30 * time.Millisecond})
			e.acct.Fail(fake.Failure{Op: op, Block: true, Count: -1})
			ctx := context.Background()
			var err error
			switch op {
			case "DescribeInstances":
				_, err = e.d.GetInstance(ctx, "us-east-1/i-00000000000000001")
			case "RunInstances":
				_, err = e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand))
			case "DescribeAvailabilityZones":
				_, err = e.d.Zones(ctx, testRegion)
			default:
				_, err = e.d.Quotas(ctx, testRegion)
			}
			if !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != "CallTimeout" {
				t.Errorf("a hung %s: %v, want Retryable CallTimeout", op, err)
			}
		})
	}
}

func TestACanceledCallerContextIsRetryable(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.d.GetInstance(ctx, "us-east-1/i-00000000000000001"); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("got %v", err)
	}
}

func TestTwoRegionsKeepTheirOwnInstances(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	a, _ := e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand))
	b, _ := e.d.CreateInstance(ctx, request("eu-central-1", "eu-central-1b", "m7i.large", vendors.CapacityOnDemand))
	if a.Ref.Region() != testRegion || b.Ref.Region() != "eu-central-1" || b.Zone != "eu-central-1b" {
		t.Errorf("%+v %+v", a, b)
	}
	if _, err := e.d.GetInstance(ctx, vendors.NewInstanceRef(testRegion, b.Ref.ID())); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an instance asked for in the wrong region: %v", err)
	}
	_ = slices.Contains[[]string]
}

func TestUserDataLimitIsTheBootstrapBudget(t *testing.T) {
	// The renderer holds the user-data to its budget (default templates are about
	// 5 KiB, with a cluster CA of the size of an RSA-2048 CA); EC2 refuses more
	// than 16 KiB raw. The two must be the same number, or a rendering that fits
	// the budget could still be refused by EC2, or the driver would refuse
	// what the budget allows. The real size with an EKS CA bundle is measured in
	// plan 10 (L5).
	if aws.MaxUserData != bootstrap.MaxUserDataSize {
		t.Errorf("the driver's user-data limit is %d, the bootstrap budget %d", aws.MaxUserData, bootstrap.MaxUserDataSize)
	}
}

func TestOnlySpotLaunchesTagTheSpotRequest(t *testing.T) {
	// The documented IAM policy allows RunInstances on spot-instances-request only
	// with the cluster request tag; EC2 refuses such tags on an on-demand launch.
	e := newEnv(t, envOptions{})
	for capacity, want := range map[string]bool{vendors.CapacitySpot: true, vendors.CapacityOnDemand: false} {
		if _, err := e.d.CreateInstance(context.Background(), request(testRegion, testZone, "m7i.large", capacity)); err != nil {
			t.Fatalf("%s: %v", capacity, err)
		}
		var tagged bool
		for _, ts := range runInput(t, e).TagSpecifications {
			if ts.ResourceType == ec2types.ResourceTypeSpotInstancesRequest {
				tagged = len(ts.Tags) > 0 && deref(ts.Tags[0].Key) != ""
				found := false
				for _, tg := range ts.Tags {
					found = found || deref(tg.Key) == "scarpenter.sparecores.com/cluster"
				}
				if !found {
					t.Errorf("the spot request lacks the cluster tag: %v", ts.Tags)
				}
			}
		}
		if tagged != want {
			t.Errorf("%s: spot request tagged = %v, want %v", capacity, tagged, want)
		}
	}
}

func TestDeleteInstanceRefusesAnInstanceThatIsNotOurs(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	foreign := e.acct.AddForeignInstance(testRegion, "m7i.large", false, ec2types.InstanceStateNameRunning)
	ref := vendors.NewInstanceRef(testRegion, foreign)
	err := e.d.DeleteInstance(ctx, ref)
	if vendors.ClassOf(err) != vendors.Fatal || !strings.Contains(err.Error(), foreign) {
		t.Errorf("deleting a foreign instance: %v, want Fatal naming it", err)
	}
	if e.acct.InstanceStates(testRegion)[foreign] != ec2types.InstanceStateNameRunning {
		t.Error("a foreign instance was terminated")
	}
	if e.acct.Calls("TerminateInstances") != 0 {
		t.Error("TerminateInstances was called for a foreign instance")
	}
	// An instance of another cluster is foreign too.
	req := request(testRegion, testZone, "m7i.large", vendors.CapacityOnDemand)
	req.Tags[vendors.TagCluster] = "another-cluster"
	other, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.DeleteInstance(ctx, other.Ref); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("another cluster's instance: %v", err)
	}
	// A foreign instance that is already gone is not a problem.
	e.acct.SetInstanceState(testRegion, foreign, ec2types.InstanceStateNameTerminated)
	if err := e.d.DeleteInstance(ctx, ref); err != nil {
		t.Errorf("a terminated foreign instance: %v", err)
	}
}
