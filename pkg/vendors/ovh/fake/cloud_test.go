/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

var flavors = []osapi.Flavor{
	{Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50},
	{Name: "d2-2", VCPUs: 1, RAMMiB: 2048, DiskGB: 25},
}

func newCloud(t *testing.T) (*Cloud, osapi.Clients, string) {
	t.Helper()
	c := New()
	c.AddRegion("GRA11", RegionConfig{Flavors: flavors})
	c.AddRegion("EU-WEST-PAR", RegionConfig{Flavors: flavors, AZs: []string{"eu-west-par-a", "eu-west-par-b"}})
	c.AddRegion("SBG5", RegionConfig{Flavors: flavors, NoEndpoint: true})
	cl, err := c.Clients(context.Background(), "GRA11")
	if err != nil {
		t.Fatal(err)
	}
	img := c.AddImage("GRA11", osapi.Image{Name: "flatcar", Tags: []string{"flatcar"}, VirtualSizeBytes: 12 << 30})
	return c, cl, img
}

func flavorID(t *testing.T, cl osapi.Clients, name string) string {
	t.Helper()
	fs, err := cl.Compute.ListFlavors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("no flavor %s", name)
	return ""
}

func status(t *testing.T, err error) int {
	t.Helper()
	var ae *osapi.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("not an APIError: %v", err)
	}
	return ae.Status
}

func TestCatalogAndFlavorIDs(t *testing.T) {
	c, cl, _ := newCloud(t)
	regs, _ := c.Regions(context.Background())
	if strings.Join(regs, ",") != "GRA11,EU-WEST-PAR" {
		t.Fatalf("regions %v (SBG5 has no endpoint)", regs)
	}
	if _, err := c.Clients(context.Background(), "SBG5"); !errors.Is(err, osapi.ErrNoEndpoint) {
		t.Fatalf("want ErrNoEndpoint, got %v", err)
	}
	if _, err := c.Clients(context.Background(), "nope"); !errors.Is(err, osapi.ErrNoEndpoint) {
		t.Fatalf("unknown region: %v", err)
	}
	par, _ := c.Clients(context.Background(), "EU-WEST-PAR")
	if flavorID(t, cl, "b3-8") == flavorID(t, par, "b3-8") {
		t.Fatal("flavor IDs must differ per region")
	}
}

func TestServerLifecycle(t *testing.T) {
	c, cl, img := newCloud(t)
	ctx := context.Background()
	cfg := true
	s, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{
		Name: "nc-1", FlavorID: flavorID(t, cl, "b3-8"), ImageID: img, ConfigDrive: &cfg,
		Metadata: map[string]string{"scarpenter-cluster": "x"}, Tags: []string{"t1"}, UserData: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != osapi.StatusBuild || s.TaskState != osapi.TaskScheduling {
		t.Fatalf("created: %s/%s", s.Status, s.TaskState)
	}
	g, _ := cl.Compute.GetServer(ctx, s.ID)
	if g.Status != osapi.StatusBuild || g.TaskState != osapi.TaskSpawning {
		t.Fatalf("first read: %s/%s", g.Status, g.TaskState)
	}
	g, _ = cl.Compute.GetServer(ctx, s.ID)
	if g.Status != osapi.StatusActive || len(g.Addresses) != 2 || g.Addresses[0].Network != osapi.ExternalNetwork {
		t.Fatalf("second read: %+v", g)
	}
	// Filters: anchored regex name, tags, status.
	for _, tc := range []struct {
		f    osapi.ServerFilter
		want int
	}{
		{osapi.ServerFilter{Name: "^nc-1$"}, 1}, {osapi.ServerFilter{Name: "^nc-$"}, 0}, {osapi.ServerFilter{Name: "nc"}, 1},
		{osapi.ServerFilter{Tags: []string{"t1"}}, 1}, {osapi.ServerFilter{Tags: []string{"t1", "t2"}}, 0},
		{osapi.ServerFilter{Status: "SHUTOFF"}, 0},
	} {
		l, err := cl.Compute.ListServers(ctx, tc.f)
		if err != nil || len(l) != tc.want {
			t.Errorf("%+v: %d servers, %v", tc.f, len(l), err)
		}
	}
	c.PowerOff("GRA11", s.ID)
	l, _ := cl.Compute.ListServers(ctx, osapi.ServerFilter{Status: osapi.StatusShutoff})
	if len(l) != 1 {
		t.Fatalf("a guest poweroff leaves a SHUTOFF server in the list, got %d", len(l))
	}
	if err := cl.Compute.DeleteServer(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Compute.GetServer(ctx, s.ID); status(t, err) != 404 {
		t.Fatalf("deleted server: %v", err)
	}
	if err := cl.Compute.DeleteServer(ctx, s.ID); status(t, err) != 404 {
		t.Fatalf("second delete: %v", err)
	}
}

func TestNoValidHostAndQuota(t *testing.T) {
	c, cl, img := newCloud(t)
	ctx := context.Background()
	c.SetNoValidHost("GRA11", "d2-2", true)
	s, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "a", FlavorID: flavorID(t, cl, "d2-2"), ImageID: img})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := cl.Compute.GetServer(ctx, s.ID)
	if g.Status != osapi.StatusError || g.Fault == nil || !strings.Contains(g.Fault.Message, "No valid host was found") {
		t.Fatalf("want NoValidHost ERROR: %+v", g)
	}
	// The ERROR server still counts against the limits until it is deleted.
	c.SetLimits("GRA11", osapi.Limits{MaxInstances: 2, MaxCores: 3})
	lim, _ := cl.Compute.GetLimits(ctx)
	if lim.UsedInstances != 1 || lim.UsedCores != 1 {
		t.Fatalf("limits %+v", lim)
	}
	c.AddForeignServer("GRA11", "other", "b3-8") // 2 cores, foreign
	lim, _ = cl.Compute.GetLimits(ctx)
	if lim.UsedInstances != 2 || lim.UsedCores != 3 {
		t.Fatalf("foreign servers count: %+v", lim)
	}
	_, err = cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "b", FlavorID: flavorID(t, cl, "d2-2"), ImageID: img})
	if status(t, err) != 403 || !strings.Contains(err.Error(), "Quota exceeded for instances") {
		t.Fatalf("instances: %v", err)
	}
	c.SetLimits("GRA11", osapi.Limits{MaxInstances: 10, MaxCores: 3})
	_, err = cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "b", FlavorID: flavorID(t, cl, "b3-8"), ImageID: img})
	if status(t, err) != 403 || !strings.Contains(err.Error(), "Quota exceeded for cores") {
		t.Fatalf("cores: %v", err)
	}
	c.SetLimits("GRA11", osapi.Limits{MaxRAMMiB: 4096})
	_, err = cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "b", FlavorID: flavorID(t, cl, "b3-8"), ImageID: img})
	if status(t, err) != 403 || !strings.Contains(err.Error(), "Quota exceeded for ram") {
		t.Fatalf("ram: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	c, cl, img := newCloud(t)
	ctx := context.Background()
	fid := flavorID(t, cl, "b3-8")
	notActive := c.AddImage("GRA11", osapi.Image{Name: "queued", Status: "queued"})
	big := c.AddImage("GRA11", osapi.Image{Name: "big", VirtualSizeBytes: 200 << 30})
	for name, tc := range map[string]struct {
		req  osapi.CreateServerRequest
		want string
	}{
		"flavor":    {osapi.CreateServerRequest{Name: "x", FlavorID: "nope", ImageID: img}, "Invalid flavorRef"},
		"image":     {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: "nope"}, "Invalid imageRef"},
		"inactive":  {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: notActive}, "is not active"},
		"disk":      {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: big}, "disk is too small"},
		"userdata":  {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, UserData: make([]byte, 49200)}, "User data too large"},
		"az":        {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, AvailabilityZone: "gra11-a"}, "availability zone"},
		"sg":        {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, SecurityGroups: []string{"nope"}}, "Unable to find security group"},
		"net":       {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, NetworkIDs: []string{"nope"}}, "could not be found"},
		"volume":    {osapi.CreateServerRequest{Name: "x", FlavorID: fid, BlockDevices: []osapi.BlockDevice{{DestinationType: "volume", VolumeType: "nope"}}}, "volume_type"},
		"group":     {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, SchedulerGroup: "nope"}, "Server group"},
		"metadata":  {osapi.CreateServerRequest{Name: "x", FlavorID: fid, ImageID: img, Metadata: map[string]string{"k": strings.Repeat("v", 256)}}, "Invalid metadata"},
		"emptyname": {osapi.CreateServerRequest{FlavorID: fid, ImageID: img}, "Invalid input for field/attribute name"},
	} {
		_, err := cl.Compute.CreateServer(ctx, tc.req)
		if err == nil || status(t, err) != 400 || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want 400 %q, got %v", name, tc.want, err)
		}
	}
	// 48 KiB of user data is 65,536 base64 bytes: over the limit; 48 KiB-3 fits.
	if _, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "ok", FlavorID: fid, ImageID: img, UserData: make([]byte, 49149)}); err != nil {
		t.Fatalf("user data at the limit: %v", err)
	}
}

func TestAvailabilityZones(t *testing.T) {
	c, _, _ := newCloud(t)
	ctx := context.Background()
	gra, _ := c.Clients(ctx, "GRA11")
	par, _ := c.Clients(ctx, "EU-WEST-PAR")
	z, _ := gra.Compute.ListAvailabilityZones(ctx)
	if len(z) != 1 || z[0].Name != "nova" {
		t.Fatalf("1-AZ region: %v", z)
	}
	if z, _ = par.Compute.ListAvailabilityZones(ctx); len(z) != 2 {
		t.Fatalf("3-AZ region: %v", z)
	}
	img := c.AddImage("EU-WEST-PAR", osapi.Image{Name: "f"})
	if _, err := par.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "x", FlavorID: flavorID(t, par, "b3-8"), ImageID: img, AvailabilityZone: "eu-west-par-b"}); err != nil {
		t.Fatal(err)
	}
}

func TestVolumesFollowServer(t *testing.T) {
	c, cl, img := newCloud(t)
	ctx := context.Background()
	s, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{
		Name: "v", FlavorID: flavorID(t, cl, "b3-8"),
		BlockDevices: []osapi.BlockDevice{
			{SourceType: "image", DestinationType: "volume", UUID: img, BootIndex: 0, SizeGiB: 64, VolumeType: "high-speed", DeleteOnTermination: true},
			{SourceType: "blank", DestinationType: "volume", BootIndex: -1, SizeGiB: 100, VolumeType: "classic", DeleteOnTermination: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.VolumeCount("GRA11") != 2 || len(s.Volumes) != 2 {
		t.Fatalf("volumes: %d %v", c.VolumeCount("GRA11"), s.Volumes)
	}
	if err := cl.Compute.DeleteServer(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if c.VolumeCount("GRA11") != 1 {
		t.Fatalf("only the delete_on_termination volume goes with the server, %d left", c.VolumeCount("GRA11"))
	}
	if vt, _ := cl.Volumes.ListVolumeTypes(ctx); len(vt) != 3 {
		t.Fatalf("volume types %v", vt)
	}
}

func TestServerGroups(t *testing.T) {
	c := New()
	ctx := context.Background()
	c.AddRegion("GRA11", RegionConfig{Flavors: flavors, Hosts: 1})
	cl, _ := c.Clients(ctx, "GRA11")
	img := c.AddImage("GRA11", osapi.Image{Name: "f"})
	if _, err := cl.Compute.CreateServerGroup(ctx, "g", "bogus"); status(t, err) != 400 {
		t.Fatalf("bad policy: %v", err)
	}
	g, err := cl.Compute.CreateServerGroup(ctx, "g", "anti-affinity")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 2 {
		s, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "m", FlavorID: flavorID(t, cl, "d2-2"), ImageID: img, SchedulerGroup: g.ID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	first, _ := cl.Compute.GetServer(ctx, ids[0])
	second, _ := cl.Compute.GetServer(ctx, ids[1])
	if first.Status == osapi.StatusError || second.Status != osapi.StatusError {
		t.Fatalf("one host: first %s, second %s", first.Status, second.Status)
	}
	c.SetLimits("GRA11", osapi.Limits{MaxServerGroups: 1})
	if _, err := cl.Compute.CreateServerGroup(ctx, "h", "affinity"); status(t, err) != 403 {
		t.Fatalf("server group quota: %v", err)
	}
	if err := cl.Compute.DeleteServerGroup(ctx, "nope"); status(t, err) != 404 {
		t.Fatalf("delete unknown: %v", err)
	}
}

func TestSecurityGroups(t *testing.T) {
	c, cl, img := newCloud(t)
	ctx := context.Background()
	nets, _ := cl.Network.ListNetworks(ctx)
	if len(nets) != 1 || nets[0].Name != osapi.ExternalNetwork || !nets[0].External {
		t.Fatalf("networks %v", nets)
	}
	gs, _ := cl.Network.ListSecurityGroups(ctx)
	if len(gs) != 1 || gs[0].Name != "default" {
		t.Fatalf("the project has a default group: %v", gs)
	}
	if err := cl.Network.DeleteSecurityGroup(ctx, gs[0].ID); status(t, err) != 409 {
		t.Fatalf("default group delete: %v", err)
	}
	g, err := cl.Network.CreateSecurityGroup(ctx, "nodes", "d")
	if err != nil || len(g.Rules) != 2 {
		t.Fatalf("a new group has the two egress rules: %+v %v", g, err)
	}
	rule := osapi.SecurityRule{Direction: "ingress", EtherType: "IPv4", Protocol: "udp", PortMin: 41641, PortMax: 41641, RemoteCIDR: "0.0.0.0/0"}
	if _, err := cl.Network.CreateSecurityRule(ctx, g.ID, rule); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Network.CreateSecurityRule(ctx, g.ID, rule); status(t, err) != 409 {
		t.Fatalf("duplicate rule: %v", err)
	}
	if _, err := cl.Network.CreateSecurityRule(ctx, "nope", rule); status(t, err) != 404 {
		t.Fatalf("unknown group: %v", err)
	}
	s, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "n", FlavorID: flavorID(t, cl, "d2-2"), ImageID: img, SecurityGroups: []string{"nodes"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Network.DeleteSecurityGroup(ctx, g.ID); status(t, err) != 409 || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("group in use: %v", err)
	}
	// Two groups of one name make the by-name attach ambiguous.
	if _, err := cl.Network.CreateSecurityGroup(ctx, "nodes", "dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "n2", FlavorID: flavorID(t, cl, "d2-2"), ImageID: img, SecurityGroups: []string{"nodes"}}); status(t, err) != 409 {
		t.Fatalf("ambiguous name: %v", err)
	}
	_ = cl.Compute.DeleteServer(ctx, s.ID)
	if err := cl.Network.DeleteSecurityGroup(ctx, g.ID); err != nil {
		t.Fatalf("delete after the server is gone: %v", err)
	}
	c.SetLimits("GRA11", osapi.Limits{MaxSecurityGroups: 2})
	if _, err := cl.Network.CreateSecurityGroup(ctx, "x", ""); status(t, err) != 409 || !strings.Contains(err.Error(), "OverQuota") {
		t.Fatalf("group quota: %v", err)
	}
}

func TestImages(t *testing.T) {
	c, cl, _ := newCloud(t)
	ctx := context.Background()
	c.AddImage("GRA11", osapi.Image{Name: "flatcar-a", Tags: []string{"flatcar", "x"}})
	c.AddImage("GRA11", osapi.Image{Name: "flatcar-a", Owner: "other", Visibility: osapi.VisibilityComm})
	c.AddImage("GRA11", osapi.Image{Name: "flatcar-a", Owner: "other", Visibility: osapi.VisibilityPriv})
	n := func(f osapi.ImageFilter) int { l, _ := cl.Images.ListImages(ctx, f); return len(l) }
	if got := n(osapi.ImageFilter{Name: "flatcar-a"}); got != 1 {
		t.Errorf("own images only without a visibility filter: %d", got)
	}
	if got := n(osapi.ImageFilter{Name: "flatcar-a", Visibility: osapi.VisibilityComm}); got != 1 {
		t.Errorf("community images when asked for: %d", got)
	}
	if got := n(osapi.ImageFilter{Tags: []string{"flatcar", "x"}, Status: osapi.ImageActive}); got != 1 {
		t.Errorf("tags and status: %d", got)
	}
	if got := n(osapi.ImageFilter{Owner: "other"}); got != 0 {
		t.Errorf("another project's private image never shows: %d", got)
	}
}

func TestInjectedErrors(t *testing.T) {
	c, cl, _ := newCloud(t)
	ctx := context.Background()
	boom := &osapi.APIError{Service: "nova", Status: 503, Message: "down"}
	c.InjectError("GRA11", "ListFlavors", boom, 1)
	if _, err := cl.Compute.ListFlavors(ctx); !errors.Is(err, boom) {
		t.Fatalf("injected: %v", err)
	}
	if _, err := cl.Compute.ListFlavors(ctx); err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	c.InjectError("", "Clients", &osapi.APIError{Service: "keystone", Status: 401, Message: "expired"}, 0)
	if _, err := c.Clients(ctx, "GRA11"); status(t, err) != 401 {
		t.Fatalf("token: %v", err)
	}
	c.ClearErrors()
	if _, err := c.Clients(ctx, "GRA11"); err != nil {
		t.Fatal(err)
	}
}
