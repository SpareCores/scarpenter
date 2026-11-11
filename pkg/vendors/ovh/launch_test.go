/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi/sdk"
)

// golden compares v, as indented JSON, to testdata/<name>.golden.json; UPDATE=1
// rewrites it (review the diff like code).
func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if os.Getenv("UPDATE") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from the golden file (UPDATE=1 rewrites it):\n%s", name, got)
	}
}

func baseRequest() vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: "GRA11", Zone: "gra11-a", InstanceType: "b3-8", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image:    vendors.Image{Ref: "9c1f0000-0000-4000-8000-000000000001"},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: "default-abc12", vendors.TagNodePool: "default",
			vendors.TagNodeClass: "flatcar", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		ExtraTags:      map[string]string{"team": "data"},
		IdempotencyKey: "0f1e2d3c-4b5a-4968-8776-655443322110",
		Node:           vendors.NodeInfo{Name: "default-abc12", ProviderID: ProviderID("GRA11", "default-abc12")},
	}
}

// The create request golden: every field of the plan's POST /servers, as it goes
// over the wire.
func TestCreateRequestGolden(t *testing.T) {
	settings, err := ParseSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildCreateRequest(LaunchPlan{
		Request: baseRequest(), Settings: settings, FlavorID: "flavor-b3-8", ExternalNetworkID: "net-ext",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := sdk.CreateBody(req)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_default", body)

	raw := json.RawMessage(`{"configDrive":false,"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"64Gi"}},
	  "data":[{"name":"scratch","volumeType":"classic","size":"100Gi","filesystem":"ext4","mountPath":"/data"}]}}`)
	settings, err = ParseSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := baseRequest()
	r.Region, r.Zone = "EU-WEST-PAR", "eu-west-par-b"
	req, err = BuildCreateRequest(LaunchPlan{
		Request: r, Settings: settings, FlavorID: "flavor-b3-8", ExternalNetworkID: "net-ext",
		SecurityGroups: []string{"scarpenter-my-cluster-flatcar"}, ServerGroupID: "7f3c0f9e-2d1a-4a55-9d62-0a7a3d9f6b11", MultiAZ: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err = sdk.CreateBody(req)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_everything", body)
}

func TestBuildCreateRequestRules(t *testing.T) {
	plan := func() LaunchPlan {
		s, _ := ParseSettings(nil)
		return LaunchPlan{Request: baseRequest(), Settings: s, FlavorID: "f", ExternalNetworkID: "n"}
	}
	req, err := BuildCreateRequest(plan())
	if err != nil {
		t.Fatal(err)
	}
	if req.AvailabilityZone != "" {
		t.Error("a 1-AZ region omits the availability zone")
	}
	if req.Metadata["scarpenter-idempotency-key"] != "0f1e2d3c-4b5a-4968-8776-655443322110" ||
		req.Metadata["scarpenter-cluster"] != "my-cluster" || req.Metadata["scarpenter-tag-team"] != "data" {
		t.Errorf("metadata %v", req.Metadata)
	}
	if req.ConfigDrive == nil || !*req.ConfigDrive {
		t.Error("config drive defaults to true")
	}
	if len(req.BlockDevices) != 0 || req.ImageID == "" {
		t.Error("the default root is the flavor's local disk from the image")
	}
	for name, mod := range map[string]func(*LaunchPlan){
		"userdata":  func(p *LaunchPlan) { p.Request.UserData = make([]byte, MaxUserData+1) },
		"noname":    func(p *LaunchPlan) { p.Request.Node.Name, p.Request.Tags = "", nil },
		"noflavor":  func(p *LaunchPlan) { p.FlavorID = "" },
		"nonet":     func(p *LaunchPlan) { p.ExternalNetworkID = "" },
		"noimage":   func(p *LaunchPlan) { p.Request.Image.Ref = "" },
		"metaLen":   func(p *LaunchPlan) { p.Request.ExtraTags = map[string]string{"k": strings.Repeat("v", 256)} },
		"metaCount": func(p *LaunchPlan) { p.Request.ExtraTags = manyTags(130) },
	} {
		p := plan()
		mod(&p)
		if _, err := BuildCreateRequest(p); err == nil || vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("%s: want a Fatal error, got %v", name, err)
		}
	}
	p := plan()
	p.Request.UserData = make([]byte, MaxUserData+1)
	if _, err := BuildCreateRequest(p); vendors.CodeOf(err) != CodeUserDataTooLarge {
		t.Errorf("user data code %q", vendors.CodeOf(err))
	}
	// The NodeClaim name falls back to the nodeclaim tag.
	p = plan()
	p.Request.Node.Name = ""
	if r, err := BuildCreateRequest(p); err != nil || r.Name != "default-abc12" {
		t.Errorf("name from the tag: %v %v", r.Name, err)
	}
}

func manyTags(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	return m
}

func TestClusterTag(t *testing.T) {
	if got := ClusterTag("my-cluster"); got != "scarpenter-cluster=my-cluster" {
		t.Errorf("short: %s", got)
	}
	a := ClusterTag(strings.Repeat("a", 80))
	b := ClusterTag(strings.Repeat("a", 79) + "b")
	if len(a) != MaxServerTagLen || len(b) != MaxServerTagLen || a == b {
		t.Errorf("long names are cut to %d with a hash: %q %q", MaxServerTagLen, a, b)
	}
	if exact := ClusterTag(strings.Repeat("c", 41)); len(exact) != 60 || strings.Contains(exact, "-") && strings.HasSuffix(exact, "-") {
		t.Errorf("a name that just fits is kept: %q", exact)
	}
	if got := ClusterTag("a/b,c"); strings.ContainsAny(got, "/,") {
		t.Errorf("characters Nova rejects: %s", got)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	tags := vendors.IdempotencyTag(baseRequest().Tags, "uid")
	md, err := Metadata(tags, map[string]string{"team": "data"})
	if err != nil {
		t.Fatal(err)
	}
	md["unrelated"] = "x"
	back := TagsFromMetadata(md)
	if len(back) != len(tags) || back[vendors.TagIdempotencyKey] != "uid" || back[vendors.TagCluster] != "my-cluster" {
		t.Errorf("round trip %v", back)
	}
	if TagKey(vendors.TagNodeClaim) != "scarpenter-nodeclaim" {
		t.Error("TagKey")
	}
}

func TestInstanceFrom(t *testing.T) {
	created := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := osapi.Server{
		ID: "9c1f0000-0000-4000-8000-0000000000aa", Status: "ACTIVE", FlavorName: "b3-8", Created: created,
		Metadata: map[string]string{"scarpenter-nodeclaim": "n1", "other": "x"},
		Addresses: []osapi.Address{
			{Network: "Ext-Net", Addr: "2001:db8::7", Version: 6}, {Network: "Ext-Net", Addr: "198.51.100.7", Version: 4},
		},
	}
	i := InstanceFrom("GRA11", "gra11-a", s)
	if i.Ref != "GRA11/9c1f0000-0000-4000-8000-0000000000aa" || i.Type != "b3-8" || i.State != vendors.StateRunning ||
		i.Zone != "gra11-a" || i.CapacityType != vendors.CapacityOnDemand || !i.CreatedAt.Equal(created) ||
		i.Tags[vendors.TagNodeClaim] != "n1" || len(i.Addresses) != 2 || i.Addresses[0] != "198.51.100.7" {
		t.Errorf("%+v", i)
	}
	if PrimaryAddress(s) != "198.51.100.7" || PrimaryAddress(osapi.Server{}) != "" {
		t.Error("primary address is the public IPv4")
	}
}

// The request builder against the in-memory cloud: a create over the kept Keeper
// types of the snapshot, the NoValidHost fault classified as capacity, quota as
// quota, and the instance mapping from what the cloud returns.
func TestLaunchAgainstTheFake(t *testing.T) {
	ctx := context.Background()
	cloud := fake.New()
	var flavors []osapi.Flavor
	kept := 0
	for _, s := range ovhServers(t) {
		if ok, _ := Launchable(s.APIReference); ok {
			kept++
			flavors = append(flavors, osapi.Flavor{
				Name: s.APIReference, VCPUs: s.VCPUs, RAMMiB: s.MemoryMiB,
				DiskGB: int64(RootDiskGB(s.APIReference, s.StorageSize, storageSizes(s))),
			})
		}
	}
	if kept == 0 {
		t.Fatal("no kept types in the snapshot")
	}
	cloud.AddRegion("GRA11", fake.RegionConfig{Flavors: flavors})
	cl, err := cloud.Clients(ctx, "GRA11")
	if err != nil {
		t.Fatal(err)
	}
	img := cloud.AddImage("GRA11", osapi.Image{Name: "flatcar-stable-4757.2.3-amd64", Tags: []string{"flatcar"}, VirtualSizeBytes: 12300 << 20})
	fl, _ := cl.Compute.ListFlavors(ctx)
	ids := map[string]string{}
	for _, f := range fl {
		ids[f.Name] = f.ID
	}
	nets, _ := cl.Network.ListNetworks(ctx)

	launch := func(flavor string) (*osapi.Server, error) {
		r := baseRequest()
		r.InstanceType, r.Image.Ref = flavor, img
		settings, _ := ParseSettings(nil)
		req, err := BuildCreateRequest(LaunchPlan{Request: r, Settings: settings, FlavorID: ids[flavor], ExternalNetworkID: nets[0].ID})
		if err != nil {
			return nil, err
		}
		s, err := cl.Compute.CreateServer(ctx, req)
		return s, Classify(err)
	}
	// Every kept type fits the 12.3 GiB image and launches.
	for _, f := range fl {
		if _, err := launch(f.Name); err != nil {
			t.Fatalf("launch %s: %v", f.Name, err)
		}
	}
	// The scheduler fails one: the fault is capacity.
	cloud.SetNoValidHost("GRA11", "b3-8", true)
	s, err := launch("b3-8")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := cl.Compute.GetServer(ctx, s.ID)
	if g.Status != osapi.StatusError || !vendors.IsClass(ClassifyFault(g.Fault), vendors.InsufficientCapacity) {
		t.Fatalf("fault %+v -> %v", g.Fault, ClassifyFault(g.Fault))
	}
	if st := InstanceFrom("GRA11", "gra11-a", *g).State; st != vendors.StateTerminating {
		t.Errorf("an ERROR server is terminating, got %s", st)
	}
	// A project quota is quota, not capacity.
	cloud.SetLimits("GRA11", osapi.Limits{MaxInstances: int64(kept + 1)})
	if _, err := launch("d2-2"); !vendors.IsClass(err, vendors.QuotaExceeded) || vendors.IsAccountWide(err) {
		t.Fatalf("want a region QuotaExceeded, got %v", err)
	}
	// A flavor that does not exist in the region.
	r := baseRequest()
	r.Image.Ref = img
	st, _ := ParseSettings(nil)
	req, _ := BuildCreateRequest(LaunchPlan{Request: r, Settings: st, FlavorID: "nope", ExternalNetworkID: nets[0].ID})
	if _, err := cl.Compute.CreateServer(ctx, req); !vendors.IsClass(Classify(err), vendors.Unsupported) {
		t.Fatalf("missing flavor: %v", err)
	}
}

func storageSizes(s snapServer) []float64 {
	out := make([]float64, 0, len(s.Storages))
	for _, st := range s.Storages {
		out = append(out, st.Size)
	}
	return out
}
