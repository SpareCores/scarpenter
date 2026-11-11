/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	clocktesting "k8s.io/utils/clock/testing"
)

const (
	region = "ap-southeast-1"
	zoneA  = "ap-southeast-1a"
	zoneB  = "ap-southeast-1b"
	typ    = "ecs.g8y.large"
)

type world struct {
	t       *testing.T
	f       *Cloud
	clk     *clocktesting.FakeClock
	ecs     *ECS
	vpc     *VPC
	vswitch map[string]string
	vpcID   string
	group   string
	image   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	f := New(clk)
	f.AddRegion(region, zoneA, zoneB)
	f.AddType(TypeSpec{Name: typ, VCPUs: 2, MemoryGiB: 8})
	f.AddType(TypeSpec{Name: "ecs.g7.xlarge", VCPUs: 4, MemoryGiB: 16})
	f.OfferEverywhere(region)
	w := &world{t: t, f: f, clk: clk, ecs: f.ECS(region), vpc: f.VPC(region)}
	w.vpcID, w.vswitch = f.AddDefaultNetwork(region)
	w.image = f.AddImage(region, ImageSpec{Name: "Flatcar-stable-1-amd64"})
	g, err := w.ecs.CreateSecurityGroup(context.Background(), &ecs.CreateSecurityGroupRequest{VpcId: &w.vpcID})
	if err != nil {
		t.Fatal(err)
	}
	w.group = *g.SecurityGroupId
	return w
}

func code(err error) string {
	if e, ok := errors.AsType[*dara.SDKError](err); ok {
		return dara.StringValue(e.Code)
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

func (w *world) run(mutate func(*ecs.RunInstancesRequest)) (*ecs.RunInstancesResponseBody, error) {
	in := &ecs.RunInstancesRequest{
		RegionId: new(region), ZoneId: new(zoneA), ImageId: &w.image, InstanceType: new(typ),
		VSwitchId: new(w.vswitch[zoneA]), SecurityGroupIds: []*string{&w.group}, Amount: dara.Int32(1), MinAmount: dara.Int32(1),
		InstanceChargeType: new("PostPaid"), InstanceName: new("n1"), InternetMaxBandwidthOut: dara.Int32(10),
		UserData: new(base64.StdEncoding.EncodeToString([]byte("{}"))),
		Tag:      []*ecs.RunInstancesRequestTag{{Key: new("a"), Value: new("1")}},
	}
	if mutate != nil {
		mutate(in)
	}
	return w.ecs.RunInstances(context.Background(), in)
}

func (w *world) describe(in *ecs.DescribeInstancesRequest) *ecs.DescribeInstancesResponseBody {
	w.t.Helper()
	out, err := w.ecs.DescribeInstances(context.Background(), in)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func TestRunInstancesValidation(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, userDataMax+1))
	cases := map[string]struct {
		mutate func(*world, *ecs.RunInstancesRequest)
		want   string
	}{
		"image":        {func(_ *world, r *ecs.RunInstancesRequest) { r.ImageId = new("m-none") }, "InvalidImageId.NotFound"},
		"vswitch":      {func(_ *world, r *ecs.RunInstancesRequest) { r.VSwitchId = new("vsw-none") }, "InvalidVSwitchId.NotFound"},
		"zone":         {func(w *world, r *ecs.RunInstancesRequest) { r.ZoneId = new(zoneB) }, "InvalidParameter.Mismatch"},
		"no group":     {func(_ *world, r *ecs.RunInstancesRequest) { r.SecurityGroupIds = nil }, "MissingParameter.SecurityGroupId"},
		"group gone":   {func(_ *world, r *ecs.RunInstancesRequest) { r.SecurityGroupIds = []*string{new("sg-none")} }, "InvalidSecurityGroupId.NotFound"},
		"user data":    {func(_ *world, r *ecs.RunInstancesRequest) { r.UserData = new(big) }, "InvalidUserData.SizeExceeded"},
		"not base64":   {func(_ *world, r *ecs.RunInstancesRequest) { r.UserData = new("***") }, "InvalidUserData.NotBase64"},
		"tag prefix":   {func(_ *world, r *ecs.RunInstancesRequest) { r.Tag[0].Key = new("aliyun-x") }, "InvalidTagKey.Malformed"},
		"tag http":     {func(_ *world, r *ecs.RunInstancesRequest) { r.Tag[0].Key = new("http://x") }, "InvalidTagKey.Malformed"},
		"type":         {func(_ *world, r *ecs.RunInstancesRequest) { r.InstanceType = new("ecs.none.large") }, "InvalidInstanceType.ValueNotSupported"},
		"charge type":  {func(_ *world, r *ecs.RunInstancesRequest) { r.InstanceChargeType = new("PrePaid") }, "InvalidChargeType.ValueNotSupported"},
		"token length": {func(_ *world, r *ecs.RunInstancesRequest) { r.ClientToken = new(strings.Repeat("a", 65)) }, "InvalidClientToken.ValueNotSupported"},
		"disk size": {func(_ *world, r *ecs.RunInstancesRequest) {
			r.SystemDisk = &ecs.RunInstancesRequestSystemDisk{Size: new("10")}
		}, "InvalidSystemDiskSize.ValueNotSupported"},
		"disk kind": {func(_ *world, r *ecs.RunInstancesRequest) {
			r.DataDisk = []*ecs.RunInstancesRequestDataDisk{{Category: new("floppy")}}
		}, "InvalidDiskCategory.NotSupported"},
		"amount": {func(_ *world, r *ecs.RunInstancesRequest) { r.Amount = dara.Int32(2) }, "InvalidParameter"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			_, err := w.run(func(r *ecs.RunInstancesRequest) { c.mutate(w, r) })
			if got := code(err); got != c.want {
				t.Fatalf("code = %s, want %s", got, c.want)
			}
			if len(w.f.InstanceIDs(region)) != 0 {
				t.Fatal("a failed call must not create an instance")
			}
		})
	}
	// Twenty tags are fine, 21 are not.
	w := newWorld(t)
	tags := func(n int) []*ecs.RunInstancesRequestTag {
		out := make([]*ecs.RunInstancesRequestTag, 0, n)
		for i := range n {
			out = append(out, &ecs.RunInstancesRequestTag{Key: new(fmt.Sprint("k", i)), Value: new("v")})
		}
		return out
	}
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.Tag = tags(20) }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.Tag = tags(21) }); code(err) != "QuotaExceed.Tags" {
		t.Fatalf("21 tags: %v", err)
	}
}

func TestClientTokenSemantics(t *testing.T) {
	w := newWorld(t)
	tok := func(r *ecs.RunInstancesRequest) { r.ClientToken = new("token-1") }
	first, err := w.run(tok)
	if err != nil {
		t.Fatal(err)
	}
	// The same token and parameters: the same instance, no second one.
	again, err := w.run(tok)
	if err != nil || *again.InstanceIdSets.InstanceIdSet[0] != *first.InstanceIdSets.InstanceIdSet[0] {
		t.Fatalf("replay: %v %v", again, err)
	}
	if len(w.f.InstanceIDs(region)) != 1 {
		t.Fatal("a replay must not create an instance")
	}
	// The same token with other parameters is a mismatch, whatever the parameter.
	for name, m := range map[string]func(*ecs.RunInstancesRequest){
		"user data": func(r *ecs.RunInstancesRequest) {
			r.UserData = new(base64.StdEncoding.EncodeToString([]byte("{1}")))
		},
		"type": func(r *ecs.RunInstancesRequest) { r.InstanceType = new("ecs.g7.xlarge") },
		"zone": func(r *ecs.RunInstancesRequest) {
			r.ZoneId = new(zoneB)
			r.VSwitchId = new(w.vswitch[zoneB])
		},
	} {
		_, err := w.run(func(r *ecs.RunInstancesRequest) { tok(r); m(r) })
		if code(err) != "IdempotentParameterMismatch" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A failed call leaves the token unused by default...
	w.f.Offer(region, zoneA, OnDemand, StatusSoldOut, typ)
	failTok := func(r *ecs.RunInstancesRequest) { r.ClientToken = new("token-2") }
	if _, err := w.run(failTok); code(err) != "OperationDenied.NoStock" {
		t.Fatalf("sold out: %v", err)
	}
	w.f.Offer(region, zoneA, OnDemand, StatusAvailable, typ)
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { failTok(r); r.UserData = new("e1sxfQ==") }); err != nil {
		t.Fatalf("an unused token with other parameters launches: %v", err)
	}
	// ...and consumes it when the option says so.
	w.f.ConsumeTokenOnFailure = true
	w.f.Offer(region, zoneA, OnDemand, StatusSoldOut, typ)
	spent := func(r *ecs.RunInstancesRequest) { r.ClientToken = new("token-3") }
	if _, err := w.run(spent); err == nil {
		t.Fatal("expected sold out")
	}
	w.f.Offer(region, zoneA, OnDemand, StatusAvailable, typ)
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { spent(r); r.UserData = new("e1sxfQ==") }); code(err) != "IdempotentParameterMismatch" {
		t.Fatalf("a spent token with other parameters: %v", err)
	}
	// Two launches without a token are two instances.
	n := len(w.f.InstanceIDs(region))
	if _, err := w.run(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.run(nil); err != nil {
		t.Fatal(err)
	}
	if got := len(w.f.InstanceIDs(region)); got != n+2 {
		t.Fatalf("instances = %d, want %d", got, n+2)
	}
}

func TestInstanceLifecycle(t *testing.T) {
	w := newWorld(t)
	w.f.PendingFor = time.Minute
	out, err := w.run(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := *out.InstanceIdSets.InstanceIdSet[0]
	status := func() string {
		inst := w.describe(&ecs.DescribeInstancesRequest{InstanceIds: new(`["` + id + `"]`)}).Instances.Instance
		if len(inst) != 1 {
			return "gone"
		}
		return *inst[0].Status
	}
	if status() != "Pending" {
		t.Fatalf("status = %s", status())
	}
	w.clk.Step(time.Minute)
	if status() != "Running" {
		t.Fatalf("status = %s", status())
	}
	if _, err := w.ecs.DeleteInstance(context.Background(), &ecs.DeleteInstanceRequest{InstanceId: &id}); code(err) != "IncorrectInstanceStatus" {
		t.Fatalf("a running instance needs Force: %v", err)
	}
	w.f.Stop(region, id)
	if status() != "Stopped" {
		t.Fatalf("status = %s", status())
	}
	if _, err := w.ecs.DeleteInstance(context.Background(), &ecs.DeleteInstanceRequest{InstanceId: &id}); err != nil {
		t.Fatalf("a stopped instance can be released without Force: %v", err)
	}
	if status() != "gone" {
		t.Fatalf("status = %s", status())
	}
	if _, err := w.ecs.DeleteInstance(context.Background(), &ecs.DeleteInstanceRequest{InstanceId: &id, Force: new(true)}); code(err) != "InvalidInstanceId.NotFound" {
		t.Fatalf("second delete: %v", err)
	}
	// Force releases a running instance; a pending one is refused either way.
	out, _ = w.run(nil)
	id2 := *out.InstanceIdSets.InstanceIdSet[0]
	if _, err := w.ecs.DeleteInstance(context.Background(), &ecs.DeleteInstanceRequest{InstanceId: &id2, Force: new(true)}); code(err) != "IncorrectInstanceStatus" {
		t.Fatalf("pending: %v", err)
	}
	w.clk.Step(time.Minute)
	if _, err := w.ecs.DeleteInstance(context.Background(), &ecs.DeleteInstanceRequest{InstanceId: &id2, Force: new(true)}); err != nil {
		t.Fatal(err)
	}
}

func TestVSwitchAddressesAreCountedAndReleased(t *testing.T) {
	w := newWorld(t)
	free := func() int64 {
		out, err := w.vpc.DescribeVSwitches(context.Background(), &vpc.DescribeVSwitchesRequest{VSwitchId: new(w.vswitch[zoneA])})
		if err != nil {
			t.Fatal(err)
		}
		return *out.VSwitches.VSwitch[0].AvailableIpAddressCount
	}
	if free() != 4096-reservedAddresses {
		t.Fatalf("free = %d", free())
	}
	out, _ := w.run(nil)
	if free() != 4096-reservedAddresses-1 {
		t.Fatalf("free after a launch = %d", free())
	}
	w.f.Release(region, *out.InstanceIdSets.InstanceIdSet[0])
	if free() != 4096-reservedAddresses {
		t.Fatalf("free after the release = %d", free())
	}
	w.f.Fill(region, w.vswitch[zoneA], 4096-reservedAddresses)
	if _, err := w.run(nil); code(err) != "QuotaExceeded.PrivateIpAddress" {
		t.Fatalf("full vSwitch: %v", err)
	}
	// The addresses of two instances differ.
	w2 := newWorld(t)
	a, _ := w2.run(nil)
	b, _ := w2.run(nil)
	ips := map[string]bool{}
	for _, id := range []string{*a.InstanceIdSets.InstanceIdSet[0], *b.InstanceIdSets.InstanceIdSet[0]} {
		in := w2.describe(&ecs.DescribeInstancesRequest{InstanceIds: new(`["` + id + `"]`)}).Instances.Instance[0]
		ips[*in.VpcAttributes.PrivateIpAddress.IpAddress[0]] = true
	}
	if len(ips) != 2 {
		t.Fatalf("addresses = %v", ips)
	}
}

func TestDescribeInstancesFiltersAndPaging(t *testing.T) {
	w := newWorld(t)
	for i := range 25 {
		_, err := w.run(func(r *ecs.RunInstancesRequest) {
			r.InstanceName = new(fmt.Sprint("n", i))
			r.Tag = []*ecs.RunInstancesRequestTag{{Key: new("a"), Value: new("1")}}
			if i%5 == 0 {
				r.Tag = append(r.Tag, &ecs.RunInstancesRequestTag{Key: new("b"), Value: new("2")})
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The default page is 10; NextToken continues; the last page has no token.
	var total int
	var next *string
	pages := 0
	for {
		out := w.describe(&ecs.DescribeInstancesRequest{NextToken: next})
		total += len(out.Instances.Instance)
		pages++
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	if total != 25 || pages != 3 {
		t.Fatalf("total %d in %d pages", total, pages)
	}
	if out := w.describe(&ecs.DescribeInstancesRequest{MaxResults: dara.Int32(1000)}); len(out.Instances.Instance) != 25 || out.NextToken != nil {
		t.Fatalf("max page size: %d", len(out.Instances.Instance))
	}
	// Tags are ANDed.
	both := w.describe(&ecs.DescribeInstancesRequest{Tag: []*ecs.DescribeInstancesRequestTag{
		{Key: new("a"), Value: new("1")}, {Key: new("b"), Value: new("2")}}})
	if len(both.Instances.Instance) != 5 {
		t.Fatalf("AND of two tags: %d", len(both.Instances.Instance))
	}
	none := w.describe(&ecs.DescribeInstancesRequest{Tag: []*ecs.DescribeInstancesRequestTag{
		{Key: new("a"), Value: new("2")}}})
	if len(none.Instances.Instance) != 0 {
		t.Fatal("a tag value that differs must not match")
	}
	if out := w.describe(&ecs.DescribeInstancesRequest{InstanceName: new("n3")}); len(out.Instances.Instance) != 1 {
		t.Fatalf("by name: %d", len(out.Instances.Instance))
	}
	if _, err := w.ecs.DescribeInstances(context.Background(), &ecs.DescribeInstancesRequest{NextToken: new("junk")}); code(err) != "InvalidNextToken.Malformed" {
		t.Fatalf("bad token: %v", err)
	}
	out := w.describe(&ecs.DescribeInstancesRequest{MaxResults: dara.Int32(1)})
	in := out.Instances.Instance[0]
	if *in.SpotStrategy != "NoSpot" || *in.InstanceChargeType != "PostPaid" || len(in.PublicIpAddress.IpAddress) != 1 ||
		*in.VpcAttributes.VpcId != w.vpcID || *in.CreationTime != "2026-11-10T12:00Z" {
		t.Fatalf("instance = %v", in)
	}
}

func TestStockQuotaAndSpot(t *testing.T) {
	w := newWorld(t)
	w.f.Offer(region, zoneA, Spot, StatusSoldOut, typ)
	spot := func(r *ecs.RunInstancesRequest) { r.SpotStrategy = new("SpotAsPriceGo") }
	if _, err := w.run(spot); code(err) != "OperationDenied.NoStock" {
		t.Fatalf("spot sold out while on demand is not: %v", err)
	}
	if _, err := w.run(nil); err != nil {
		t.Fatalf("on demand: %v", err)
	}
	w.f.SetQuota(region, 3, 100)
	if _, err := w.run(nil); err == nil { // 2 used + 2 > 3
		t.Fatal("the vCPU quota must stop the second launch")
	} else if code(err) != "QuotaExceed.ElasticQuota" {
		t.Fatal(err)
	}
	// Foreign instances use the account's quota.
	w.f.SetQuota(region, 100, 100)
	w.f.AddForeignInstance(region, zoneA, "ecs.g7.xlarge", false)
	attrs, err := w.ecs.DescribeAccountAttributes(context.Background(), &ecs.DescribeAccountAttributesRequest{
		AttributeName: []*string{new("used-postpaid-instance-vcpu-count"), new("max-spot-instance-vcpu-count"), new("unknown")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range attrs.AccountAttributeItems.AccountAttributeItem {
		got[*it.AttributeName] = *it.AttributeValues.ValueItem[0].Value
	}
	if len(got) != 2 || got["used-postpaid-instance-vcpu-count"] != "6" || got["max-spot-instance-vcpu-count"] != "100" {
		t.Fatalf("attributes = %v", got)
	}
	w.f.DisableSpot(region)
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { spot(r) }); code(err) != "InvalidAccount.NotSupportSpot" {
		t.Fatalf("spot disabled: %v", err)
	}
	// DescribeAvailableResource lists the stock per zone and capacity type.
	res, err := w.ecs.DescribeAvailableResource(context.Background(), &ecs.DescribeAvailableResourceRequest{
		DestinationResource: new("InstanceType"), ResourceType: new("instance"), InstanceChargeType: new("PostPaid"),
		SpotStrategy: new("SpotAsPriceGo")})
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, z := range res.AvailableZones.AvailableZone {
		for _, sr := range z.AvailableResources.AvailableResource[0].SupportedResources.SupportedResource {
			status[*z.ZoneId+"/"+*sr.Value] = *sr.Status + "/" + *sr.StatusCategory
		}
	}
	if status[zoneA+"/"+typ] != "SoldOut/WithoutStock" || status[zoneB+"/"+typ] != "Available/WithStock" {
		t.Fatalf("stock = %v", status)
	}
	if _, err := w.ecs.DescribeAvailableResource(context.Background(), &ecs.DescribeAvailableResourceRequest{DestinationResource: new("SystemDisk")}); err == nil {
		t.Fatal("only InstanceType is modelled")
	}
}

func TestInstanceTypesPaging(t *testing.T) {
	w := newWorld(t)
	for i := range 30 {
		w.f.AddType(TypeSpec{Name: fmt.Sprintf("ecs.t%02d.large", i), VCPUs: 2})
	}
	var n int
	var next *string
	pages := 0
	for {
		out, err := w.ecs.DescribeInstanceTypes(context.Background(), &ecs.DescribeInstanceTypesRequest{NextToken: next})
		if err != nil {
			t.Fatal(err)
		}
		n += len(out.InstanceTypes.InstanceType)
		pages++
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	if n != 32 || pages != 4 {
		t.Fatalf("%d types in %d pages", n, pages)
	}
	names := make([]*string, 11)
	for i := range names {
		names[i] = new("ecs.g7.xlarge")
	}
	if _, err := w.ecs.DescribeInstanceTypes(context.Background(), &ecs.DescribeInstanceTypesRequest{InstanceTypes: names}); err == nil {
		t.Fatal("more than ten names must fail")
	}
}

func TestVanishAndInjection(t *testing.T) {
	w := newWorld(t)
	w.f.VanishAfter(region, zoneA, OnDemand, typ, time.Minute)
	if _, err := w.run(nil); err != nil {
		t.Fatal(err)
	}
	if len(w.f.InstanceIDs(region)) != 1 {
		t.Fatal("the instance exists at first")
	}
	w.clk.Step(2 * time.Minute)
	if len(w.f.InstanceIDs(region)) != 0 {
		t.Fatal("the instance must vanish")
	}
	// Injected errors: n times, then normal; the call counter counts them.
	w.f.Inject("DescribeZones", APIError("Throttling", 400, "x"), 2)
	for range 2 {
		if _, err := w.ecs.DescribeZones(context.Background(), nil); code(err) != "Throttling" {
			t.Fatalf("err = %v", err)
		}
	}
	if _, err := w.ecs.DescribeZones(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if w.f.Calls("DescribeZones") != 3 {
		t.Fatalf("calls = %d", w.f.Calls("DescribeZones"))
	}
	w.f.Deny(region)
	if _, err := w.ecs.DescribeZones(context.Background(), nil); code(err) != "RegionUnauthorized" {
		t.Fatalf("denied: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.f.Clear("")
	if _, err := w.f.ECS("ap-southeast-1").DescribeInstances(ctx, &ecs.DescribeInstancesRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled context: %v", err)
	}
	if _, err := w.f.ECS("nowhere").DescribeZones(context.Background(), nil); code(err) != "InvalidRegionId.NotFound" {
		t.Fatalf("unknown region: %v", err)
	}
}

func TestSecurityGroups(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	mk := func(name, key string) string {
		out, err := w.ecs.CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{
			VpcId: &w.vpcID, SecurityGroupName: &name, Tag: []*ecs.CreateSecurityGroupRequestTag{{Key: &key, Value: new("true")}}})
		if err != nil {
			t.Fatal(err)
		}
		return *out.SecurityGroupId
	}
	a, b := mk("scarpenter-c-nodes-1", "marker"), mk("other", "other")
	if _, err := w.ecs.CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{VpcId: new("vpc-none")}); code(err) != "InvalidVpcId.NotFound" {
		t.Fatalf("no VPC: %v", err)
	}
	list := func(in *ecs.DescribeSecurityGroupsRequest) []string {
		out, err := w.ecs.DescribeSecurityGroups(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(out.SecurityGroups.SecurityGroup))
		for _, g := range out.SecurityGroups.SecurityGroup {
			ids = append(ids, *g.SecurityGroupId)
		}
		return ids
	}
	if got := list(&ecs.DescribeSecurityGroupsRequest{Tag: []*ecs.DescribeSecurityGroupsRequestTag{{Key: new("marker"), Value: new("true")}}}); len(got) != 1 || got[0] != a {
		t.Fatalf("by tag: %v", got)
	}
	if got := list(&ecs.DescribeSecurityGroupsRequest{SecurityGroupName: new("scarpenter")}); len(got) != 0 {
		t.Fatalf("exact name must not match a prefix: %v", got)
	}
	if got := list(&ecs.DescribeSecurityGroupsRequest{SecurityGroupName: new("scarpenter"), FuzzyQuery: new(true)}); len(got) != 1 {
		t.Fatalf("fuzzy name: %v", got)
	}
	if got := list(&ecs.DescribeSecurityGroupsRequest{SecurityGroupIds: new(fmt.Sprintf(`["%s","%s"]`, a, b))}); len(got) != 2 {
		t.Fatalf("by ids: %v", got)
	}
	for range 12 {
		mk("x", "x")
	}
	if got := list(&ecs.DescribeSecurityGroupsRequest{VpcId: &w.vpcID}); len(got) != 10 {
		t.Fatalf("first page = %d", len(got))
	}
	// Rules: add, list, remove; a rule that is not there is an error.
	rule := &ecs.AuthorizeSecurityGroupRequest{SecurityGroupId: &a, IpProtocol: new("udp"), PortRange: new("41641/41641"), SourceCidrIp: new("0.0.0.0/0")}
	for range 2 { // a duplicate is kept once
		if _, err := w.ecs.AuthorizeSecurityGroup(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}
	self := &ecs.AuthorizeSecurityGroupRequest{SecurityGroupId: &a, IpProtocol: new("all"), PortRange: new("-1/-1"), SourceGroupId: &a}
	if _, err := w.ecs.AuthorizeSecurityGroup(ctx, self); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ecs.AuthorizeSecurityGroup(ctx, &ecs.AuthorizeSecurityGroupRequest{SecurityGroupId: &a}); err == nil {
		t.Fatal("a rule needs a protocol")
	}
	attr, err := w.ecs.DescribeSecurityGroupAttribute(ctx, &ecs.DescribeSecurityGroupAttributeRequest{SecurityGroupId: &a})
	if err != nil || len(attr.Permissions.Permission) != 2 || *attr.VpcId != w.vpcID {
		t.Fatalf("attribute = %v, %v", attr, err)
	}
	if _, err := w.ecs.RevokeSecurityGroup(ctx, &ecs.RevokeSecurityGroupRequest{SecurityGroupId: &a, IpProtocol: new("udp"),
		PortRange: new("41641/41641"), SourceCidrIp: new("0.0.0.0/0")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ecs.RevokeSecurityGroup(ctx, &ecs.RevokeSecurityGroupRequest{SecurityGroupId: &a, SecurityGroupRuleId: []*string{new("sgr-none")}}); code(err) != "InvalidPermission.NotFound" {
		t.Fatalf("revoke of nothing: %v", err)
	}
	// A group that instances use cannot be deleted; an unused one can.
	out, _ := w.run(func(r *ecs.RunInstancesRequest) { r.SecurityGroupIds = []*string{&a} })
	if _, err := w.ecs.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{SecurityGroupId: &a}); code(err) != "DependencyViolation.Instance" {
		t.Fatalf("in use: %v", err)
	}
	w.f.Release(region, *out.InstanceIdSets.InstanceIdSet[0])
	if _, err := w.ecs.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{SecurityGroupId: &a}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ecs.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{SecurityGroupId: &a}); code(err) != "InvalidSecurityGroupId.NotFound" {
		t.Fatalf("second delete: %v", err)
	}
	// A group in another VPC than the vSwitch is refused at launch.
	other, err := w.vpc.CreateVpc(ctx, &vpc.CreateVpcRequest{CidrBlock: new("10.240.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := w.ecs.CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{VpcId: other.VpcId})
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.SecurityGroupIds = []*string{g.SecurityGroupId} }); code(err) != "InvalidSecurityGroup.VpcMismatch" {
		t.Fatalf("vpc mismatch: %v", err)
	}
}

func TestDeploymentSets(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	out, err := w.ecs.CreateDeploymentSet(ctx, &ecs.CreateDeploymentSetRequest{DeploymentSetName: new("scarpenter-c-ds-r"), Strategy: new("LowLatency")})
	if err != nil {
		t.Fatal(err)
	}
	id := *out.DeploymentSetId
	if _, err := w.ecs.CreateDeploymentSet(ctx, &ecs.CreateDeploymentSetRequest{DeploymentSetName: new("scarpenter-c-ds-r")}); code(err) != "InvalidDeploymentSetName.Duplicated" {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := w.ecs.CreateDeploymentSet(ctx, &ecs.CreateDeploymentSetRequest{Strategy: new("Bogus")}); err == nil {
		t.Fatal("a bad strategy must fail")
	}
	w.f.SetDeploymentSetLimit(region, id, 2)
	for range 2 {
		if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.DeploymentSetId = &id }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.DeploymentSetId = &id }); code(err) != "QuotaExceed.DeploymentSetInstanceQuotaFull" {
		t.Fatalf("a full set: %v", err)
	}
	if _, err := w.run(func(r *ecs.RunInstancesRequest) { r.DeploymentSetId = new("ds-none") }); code(err) != "InvalidDeploymentSetId.NotFound" {
		t.Fatalf("missing set: %v", err)
	}
	d, err := w.ecs.DescribeDeploymentSets(ctx, &ecs.DescribeDeploymentSetsRequest{DeploymentSetName: new("scarpenter-c-ds-r")})
	if err != nil || len(d.DeploymentSets.DeploymentSet) != 1 || *d.DeploymentSets.DeploymentSet[0].InstanceAmount != 2 ||
		*d.DeploymentSets.DeploymentSet[0].Strategy != "LowLatency" {
		t.Fatalf("describe = %v, %v", d, err)
	}
	if _, err := w.ecs.DeleteDeploymentSet(ctx, &ecs.DeleteDeploymentSetRequest{DeploymentSetId: &id}); code(err) != "DependencyViolation.Instance" {
		t.Fatalf("in use: %v", err)
	}
	for _, i := range w.f.InstanceIDs(region) {
		w.f.Release(region, i)
	}
	if _, err := w.ecs.DeleteDeploymentSet(ctx, &ecs.DeleteDeploymentSetRequest{DeploymentSetId: &id}); err != nil {
		t.Fatal(err)
	}
	// A deployment set is not a taggable resource: the marker is in the name.
	if _, err := w.ecs.TagResources(ctx, &ecs.TagResourcesRequest{ResourceType: new("deploymentset"), ResourceId: []*string{&id},
		Tag: []*ecs.TagResourcesRequestTag{{Key: new("a"), Value: new("b")}}}); code(err) != "InvalidResourceType.NotSupported" {
		t.Fatalf("tagging a deployment set: %v", err)
	}
}

func TestVPCAndVSwitches(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tags := []*vpc.CreateVpcRequestTag{{Key: new("scarpenter.sparecores.com/use"), Value: new("true")}}
	out, err := w.vpc.CreateVpc(ctx, &vpc.CreateVpcRequest{CidrBlock: new("10.240.0.0/16"), VpcName: new("scarpenter-c-subnets-r"), Tag: tags, ClientToken: new("t1")})
	if err != nil {
		t.Fatal(err)
	}
	// A replay of the token returns the same VPC.
	again, _ := w.vpc.CreateVpc(ctx, &vpc.CreateVpcRequest{CidrBlock: new("10.240.0.0/16"), ClientToken: new("t1")})
	if *again.VpcId != *out.VpcId {
		t.Fatal("a replayed token must return the same VPC")
	}
	if _, err := w.vpc.CreateVpc(ctx, &vpc.CreateVpcRequest{CidrBlock: new("nonsense")}); code(err) != "InvalidCidrBlock.Malformed" {
		t.Fatalf("bad cidr: %v", err)
	}
	vsw := func(zone, cidr string) (*vpc.CreateVSwitchResponseBody, error) {
		return w.vpc.CreateVSwitch(ctx, &vpc.CreateVSwitchRequest{VpcId: out.VpcId, ZoneId: &zone, CidrBlock: &cidr,
			Tag: []*vpc.CreateVSwitchRequestTag{{Key: new("k"), Value: new("v")}}})
	}
	a, err := vsw(zoneA, "10.240.0.0/20")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"zone":      {"nowhere-1a", "10.240.16.0/20"},
		"outside":   {zoneB, "192.168.0.0/24"},
		"overlap":   {zoneB, "10.240.8.0/22"},
		"too small": {zoneB, "10.240.32.0/30"},
	} {
		if _, err := vsw(c[0], c[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := vsw(zoneB, "10.240.16.0/20"); err != nil {
		t.Fatal(err)
	}
	// Discovery by tag and by default flag; the default network is found as such.
	vs, err := w.vpc.DescribeVpcs(ctx, &vpc.DescribeVpcsRequest{IsDefault: new(true)})
	if err != nil || len(vs.Vpcs.Vpc) != 1 || *vs.Vpcs.Vpc[0].VpcId != w.vpcID || len(vs.Vpcs.Vpc[0].VSwitchIds.VSwitchId) != 2 {
		t.Fatalf("default vpc = %v, %v", vs, err)
	}
	vs, _ = w.vpc.DescribeVpcs(ctx, &vpc.DescribeVpcsRequest{Tag: []*vpc.DescribeVpcsRequestTag{{Key: new("scarpenter.sparecores.com/use"), Value: new("true")}}})
	if len(vs.Vpcs.Vpc) != 1 || *vs.Vpcs.Vpc[0].VpcId != *out.VpcId {
		t.Fatalf("by tag = %v", vs)
	}
	sw, _ := w.vpc.DescribeVSwitches(ctx, &vpc.DescribeVSwitchesRequest{VpcId: out.VpcId, ZoneId: new(zoneA)})
	if len(sw.VSwitches.VSwitch) != 1 || *sw.VSwitches.VSwitch[0].AvailableIpAddressCount != 4096-reservedAddresses {
		t.Fatalf("vswitch = %v", sw)
	}
	dv, _ := w.vpc.DescribeVSwitches(ctx, &vpc.DescribeVSwitchesRequest{IsDefault: new(true)})
	if len(dv.VSwitches.VSwitch) != 2 {
		t.Fatalf("default vswitches = %d", len(dv.VSwitches.VSwitch))
	}
	// Dependencies: a VPC with vSwitches, a vSwitch with instances.
	if _, err := w.vpc.DeleteVpc(ctx, &vpc.DeleteVpcRequest{VpcId: out.VpcId}); code(err) != "DependencyViolation.VSwitch" {
		t.Fatalf("vpc with vswitches: %v", err)
	}
	g, _ := w.ecs.CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{VpcId: out.VpcId})
	inst, err := w.run(func(r *ecs.RunInstancesRequest) {
		r.VSwitchId = a.VSwitchId
		r.SecurityGroupIds = []*string{g.SecurityGroupId}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.vpc.DeleteVSwitch(ctx, &vpc.DeleteVSwitchRequest{VSwitchId: a.VSwitchId}); code(err) != "DependencyViolation.Instance" {
		t.Fatalf("vswitch with instances: %v", err)
	}
	w.f.Release(region, *inst.InstanceIdSets.InstanceIdSet[0])
	for _, id := range []*string{a.VSwitchId} {
		if _, err := w.vpc.DeleteVSwitch(ctx, &vpc.DeleteVSwitchRequest{VSwitchId: id}); err != nil {
			t.Fatal(err)
		}
	}
	sws, _ := w.vpc.DescribeVSwitches(ctx, &vpc.DescribeVSwitchesRequest{VpcId: out.VpcId})
	for _, s := range sws.VSwitches.VSwitch {
		if _, err := w.vpc.DeleteVSwitch(ctx, &vpc.DeleteVSwitchRequest{VSwitchId: s.VSwitchId}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.vpc.DeleteVpc(ctx, &vpc.DeleteVpcRequest{VpcId: out.VpcId}); code(err) != "DependencyViolation.SecurityGroup" {
		t.Fatalf("vpc with groups: %v", err)
	}
	if _, err := w.ecs.DeleteSecurityGroup(ctx, &ecs.DeleteSecurityGroupRequest{SecurityGroupId: g.SecurityGroupId}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.vpc.DeleteVpc(ctx, &vpc.DeleteVpcRequest{VpcId: out.VpcId}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.vpc.DeleteVpc(ctx, &vpc.DeleteVpcRequest{VpcId: out.VpcId}); code(err) != "InvalidVpcID.NotFound" {
		t.Fatalf("second delete: %v", err)
	}
}

func TestImagesAndTags(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	self := w.f.AddImage(region, ImageSpec{Name: "my-flatcar-stable-1-arm64", Arch: "arm64", Tags: map[string]string{"flatcar-version": "1"}})
	w.f.AddImage(region, ImageSpec{Name: "my-flatcar-stable-2-arm64", Arch: "arm64", Tags: map[string]string{"flatcar-version": "2"}, Owner: "others", Public: true})
	w.f.AddImage(region, ImageSpec{Name: "Ubuntu", Owner: "system", Public: true})
	w.f.AddImage(region, ImageSpec{Name: "broken", Status: "CreateFailed"})
	list := func(in *ecs.DescribeImagesRequest) []string {
		out, err := w.ecs.DescribeImages(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(out.Images.Image))
		for _, im := range out.Images.Image {
			names = append(names, *im.ImageName)
		}
		return names
	}
	if got := list(&ecs.DescribeImagesRequest{}); len(got) != 4 {
		t.Fatalf("available images (CreateFailed is hidden): %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{ImageName: new("flatcar")}); len(got) != 2 {
		t.Fatalf("fuzzy name: %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{ImageOwnerAlias: new("self"), Architecture: new("arm64")}); len(got) != 1 {
		t.Fatalf("owner and architecture: %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{IsPublic: new(true)}); len(got) != 2 {
		t.Fatalf("public: %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{Tag: []*ecs.DescribeImagesRequestTag{{Key: new("flatcar-version"), Value: new("2")}}}); len(got) != 1 {
		t.Fatalf("by tag: %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{ImageId: new(self)}); len(got) != 1 {
		t.Fatalf("by id: %v", got)
	}
	if got := list(&ecs.DescribeImagesRequest{Status: new("CreateFailed")}); len(got) != 1 {
		t.Fatalf("by status: %v", got)
	}
	for i := range 12 {
		w.f.AddImage(region, ImageSpec{Name: fmt.Sprint("bulk", i)})
	}
	out, _ := w.ecs.DescribeImages(ctx, &ecs.DescribeImagesRequest{PageSize: dara.Int32(5), PageNumber: dara.Int32(2)})
	if len(out.Images.Image) != 5 || *out.TotalCount != 16 {
		t.Fatalf("page 2 = %d of %d", len(out.Images.Image), *out.TotalCount)
	}
	// Tags on instances and groups through TagResources and ListTagResources.
	inst, _ := w.run(nil)
	id := *inst.InstanceIdSets.InstanceIdSet[0]
	if _, err := w.ecs.TagResources(ctx, &ecs.TagResourcesRequest{ResourceType: new("instance"), ResourceId: []*string{&id},
		Tag: []*ecs.TagResourcesRequestTag{{Key: new("late"), Value: new("yes")}}}); err != nil {
		t.Fatal(err)
	}
	lt, err := w.ecs.ListTagResources(ctx, &ecs.ListTagResourcesRequest{ResourceType: new("instance"),
		Tag: []*ecs.ListTagResourcesRequestTag{{Key: new("late"), Value: new("yes")}}})
	if err != nil || len(lt.TagResources.TagResource) != 2 { // the tag filter keeps the instance; both its tags are listed
		t.Fatalf("list tags = %v, %v", lt, err)
	}
	if _, err := w.ecs.TagResources(ctx, &ecs.TagResourcesRequest{ResourceType: new("instance"), ResourceId: []*string{new("i-none")},
		Tag: []*ecs.TagResourcesRequestTag{{Key: new("a"), Value: new("b")}}}); code(err) != "InvalidResourceId.NotFound" {
		t.Fatalf("tagging nothing: %v", err)
	}
	if _, err := w.ecs.ListTagResources(ctx, &ecs.ListTagResourcesRequest{ResourceType: new("vpc")}); err == nil {
		t.Fatal("unsupported type")
	}
}

func TestRegionsAndPanics(t *testing.T) {
	w := newWorld(t)
	w.f.AddRegion("eu-central-1", "eu-central-1a")
	out, err := w.ecs.DescribeRegions(context.Background(), nil)
	if err != nil || len(out.Regions.Region) != 2 || *out.Regions.Region[0].RegionId != region {
		t.Fatalf("regions = %v, %v", out, err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown region in a setup call must panic")
		}
	}()
	w.f.SetQuota("nowhere", 1, 1)
}
