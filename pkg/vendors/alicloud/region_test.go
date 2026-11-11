/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"slices"
	"testing"

	"github.com/aliyun/credentials-go/credentials"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
)

func TestRegionsIntersectKeeper(t *testing.T) {
	e := newEnv(t)
	e.f.AddRegion("cn-wuhan-lr", "cn-wuhan-lr-a")
	ctx := context.Background()
	all, err := e.cloud.Regions(ctx, DefaultHomeRegion, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("regions = %v, %v", all, err)
	}
	// Keeper's catalog has a region the account never sees and lacks one it sees.
	got, err := e.cloud.Regions(ctx, DefaultHomeRegion, []string{testRegion, "cn-hangzhou-acdr-ut-3", "cn-wuhan-lr"})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{testRegion, "cn-wuhan-lr"}) {
		t.Fatalf("regions = %v", names)
	}
	if got, _ := e.cloud.Regions(ctx, DefaultHomeRegion, []string{}); len(got) != 0 {
		t.Fatalf("an empty active list means none: %v", got)
	}
}

func TestZones(t *testing.T) {
	e := newEnv(t)
	zones, err := e.cloud.Zones(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	want := []vendors.Zone{{ID: testZoneA, Name: testZoneA}, {ID: testZoneB, Name: testZoneB}}
	if !slices.Equal(zones, want) {
		t.Fatalf("zones = %v", zones)
	}
	// A region the credentials cannot use is skipped by the facts.
	e.f.Deny("eu-central-1")
	_, err = e.cloud.Zones(context.Background(), "eu-central-1")
	if !vendors.IsClass(err, vendors.RegionUnavailable) || vendors.CodeOf(err) != "RegionUnauthorized" {
		t.Fatalf("denied region: %v", err)
	}
	// Anything else keeps its class.
	e.f.Inject("DescribeZones", fake.APIError("Forbidden.RAM", 403, "no"), 1)
	if _, err := e.cloud.Zones(context.Background(), testRegion); vendors.ClassOf(err) != vendors.Fatal {
		t.Fatalf("Forbidden.RAM: %v", err)
	}
}

func TestLaunchableTypes(t *testing.T) {
	e := newEnv(t)
	e.f.Offer(testRegion, testZoneA, fake.Spot, fake.StatusSoldOut, testType)
	e.f.Offer(testRegion, testZoneB, fake.OnDemand, fake.StatusSoldOut, "ecs.g7.xlarge")
	ctx := context.Background()
	od, err := e.cloud.LaunchableTypes(ctx, testRegion, vendors.CapacityOnDemand)
	if err != nil {
		t.Fatal(err)
	}
	if _, bare := od["ecs.ebmg7.32xlarge"]; bare {
		t.Error("bare metal must be dropped")
	}
	if !slices.Equal(od[testType], []string{testZoneA, testZoneB}) {
		t.Errorf("%s on-demand zones = %v", testType, od[testType])
	}
	if !slices.Equal(od["ecs.g7.xlarge"], []string{testZoneA}) {
		t.Errorf("sold out in B: %v", od["ecs.g7.xlarge"])
	}
	if !slices.Equal(od["ecs.i4.xlarge"], []string{testZoneA, testZoneB}) {
		t.Errorf("local-disk families stay launchable: %v", od["ecs.i4.xlarge"])
	}
	spot, err := e.cloud.LaunchableTypes(ctx, testRegion, vendors.CapacitySpot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spot[testType], []string{testZoneB}) {
		t.Errorf("%s spot zones = %v", testType, spot[testType])
	}
}

func TestTypeTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.cloud.LoadTypes(ctx, testRegion, []string{testType, "ecs.g7.xlarge", "ecs.unknown.large"}); err != nil {
		t.Fatal(err)
	}
	ti, ok := e.cloud.Types.Get(testType)
	if !ok || ti.VCPUs != 2 || ti.MemoryMiB != 8192 || ti.Arch != "arm64" || !ti.NVMe || ti.Family != "ecs.g8y" || ti.DiskQuantity != 17 {
		t.Fatalf("%s = %+v", testType, ti)
	}
	if ti, _ := e.cloud.Types.Get("ecs.g7.xlarge"); ti.Arch != "amd64" || ti.NVMe {
		t.Fatalf("g7 = %+v", ti)
	}
	if _, ok := e.cloud.Types.Get("ecs.unknown.large"); ok {
		t.Fatal("an unknown type stays unknown")
	}
	// Known types are not asked for again; more than ten names are batched.
	before := e.f.Calls("DescribeInstanceTypes")
	if err := e.cloud.LoadTypes(ctx, testRegion, []string{testType}); err != nil || e.f.Calls("DescribeInstanceTypes") != before {
		t.Fatalf("a known type was read again: %v", err)
	}
	names := make([]string, 0, 25)
	for i := range 25 {
		n := "ecs.bulk" + string(rune('a'+i)) + ".large"
		e.f.AddType(fake.TypeSpec{Name: n, VCPUs: 2})
		names = append(names, n)
	}
	if err := e.cloud.LoadTypes(ctx, testRegion, names); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := e.cloud.Types.Get(n); !ok {
			t.Fatalf("%s was not loaded", n)
		}
	}
	// The full listing pages by NextToken.
	fresh := NewCloud(testCluster, e.cloud.factory, e.clk)
	if err := fresh.LoadAllTypes(ctx, testRegion); err != nil || fresh.Types.Len() != 4+25 {
		t.Fatalf("all types: %d, %v", fresh.Types.Len(), err)
	}
}

func TestRealClientFactoryBuildsWithoutACall(t *testing.T) {
	cred, err := credentials.NewCredential(new(credentials.Config).
		SetType("access_key").SetAccessKeyId("test-id").SetAccessKeySecret("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	factory := NewClientFactory(ClientOptions{UserAgent: "scarpenter/test", Credential: cred})
	cl, err := factory(context.Background(), testRegion)
	if err != nil || cl.ECS == nil || cl.VPC == nil {
		t.Fatalf("factory: %v", err)
	}
	if _, err := factory(context.Background(), ""); err == nil {
		t.Fatal("a region is required")
	}
	if got := DefaultEndpoint("ecs", testRegion); got != "ecs.ap-southeast-1.aliyuncs.com" {
		t.Fatalf("endpoint = %s", got)
	}
}
