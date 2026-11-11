/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// serve starts a test server and returns a service client for it plus the
// requests it saw ("METHOD path?query" and the body).
type seen struct{ line, body, version string }

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*gophercloud.ServiceClient, *[]seen) {
	t.Helper()
	var log []seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log = append(log, seen{r.Method + " " + r.URL.RequestURI(), string(b), r.Header.Get("X-OpenStack-Nova-API-Version")})
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(ts.Close)
	pc := &gophercloud.ProviderClient{HTTPClient: *ts.Client()}
	return &gophercloud.ServiceClient{ProviderClient: pc, Endpoint: ts.URL + "/", Type: "compute", Microversion: "2.67"}, &log
}

func status(w http.ResponseWriter, code int, body string) {
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func TestWrapErrors(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		code    int
		body    string
		service string
		call    func(sc *gophercloud.ServiceClient) error
		want    string
	}{
		"nova quota": {403, `{"forbidden": {"message": "Quota exceeded for cores: Requested 4, but already used 8 of 8 cores", "code": 403}}`, "nova",
			func(sc *gophercloud.ServiceClient) error { return NewCompute(sc).DeleteServer(ctx, "x") },
			"Quota exceeded for cores: Requested 4, but already used 8 of 8 cores"},
		"nova not found": {404, `{"itemNotFound": {"message": "Instance x could not be found.", "code": 404}}`, "nova",
			func(sc *gophercloud.ServiceClient) error { _, err := NewCompute(sc).GetServer(ctx, "x"); return err },
			"Instance x could not be found."},
		"neutron overquota": {409, `{"NeutronError": {"type": "OverQuota", "message": "Quota exceeded for resources: ['security_group'].", "detail": ""}}`, "neutron",
			func(sc *gophercloud.ServiceClient) error {
				_, err := NewNetwork(sc).CreateSecurityGroup(ctx, "g", "")
				return err
			},
			"OverQuota: Quota exceeded for resources: ['security_group']."},
		"plain text": {502, `Bad Gateway`, "glance",
			func(sc *gophercloud.ServiceClient) error {
				_, err := NewImages(sc).ListImages(ctx, osapi.ImageFilter{})
				return err
			},
			"Bad Gateway"},
	} {
		t.Run(name, func(t *testing.T) {
			sc, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { status(w, tc.code, tc.body) })
			err := tc.call(sc)
			var ae *osapi.APIError
			if !errors.As(err, &ae) {
				t.Fatalf("not an APIError: %v", err)
			}
			if ae.Status != tc.code || ae.Service != tc.service || ae.Message != tc.want {
				t.Fatalf("got %d %s %q", ae.Status, ae.Service, ae.Message)
			}
		})
	}
	if wrap("nova", nil) != nil {
		t.Fatal("nil stays nil")
	}
	plain := errors.New("connection refused")
	if wrap("nova", plain) != plain {
		t.Fatal("transport errors pass through")
	}
}

const serverJSON = `{"id":"%s","name":"nc-%s","status":"%s","created":"2026-11-10T12:00:00Z",
 "flavor":{"original_name":"b3-8","vcpus":2,"ram":8192,"disk":50},"image":"",
 "metadata":{"scarpenter-cluster":"c"},"tags":["scarpenter-cluster=c"],
 "addresses":{"Ext-Net":[{"addr":"2001:db8::1","version":6},{"addr":"198.51.100.7","version":4}]},
 "OS-EXT-AZ:availability_zone":"nova","OS-EXT-STS:task_state":null,
 "fault":{"code":500,"message":"No valid host was found."},
 "os-extended-volumes:volumes_attached":[{"id":"vol-1"}]}`

func TestListServersPaginatesAndParses(t *testing.T) {
	var base string
	sc, log := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("marker") == "" {
			status(w, 200, `{"servers":[`+fmt.Sprintf(serverJSON, "s1", "1", "ACTIVE")+`],"servers_links":[{"rel":"next","href":"`+base+`/servers/detail?marker=s1"}]}`)
			return
		}
		status(w, 200, `{"servers":[`+fmt.Sprintf(serverJSON, "s2", "2", "SHUTOFF")+`]}`)
	})
	base = strings.TrimSuffix(sc.Endpoint, "/")
	got, err := NewCompute(sc).ListServers(context.Background(), osapi.ServerFilter{Name: "^nc-1$", Tags: []string{"a", "b"}, Status: "ACTIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s1" || got[1].Status != "SHUTOFF" {
		t.Fatalf("pages: %+v", got)
	}
	s := got[0]
	if s.FlavorName != "b3-8" || s.FlavorID != "" || s.ImageID != "" || s.AvailabilityZone != "nova" || s.TaskState != "" ||
		s.Created.Year() != 2026 || len(s.Addresses) != 2 || s.Addresses[0].Addr != "2001:db8::1" ||
		s.Fault == nil || s.Fault.Message != "No valid host was found." || len(s.Volumes) != 1 ||
		s.Metadata["scarpenter-cluster"] != "c" || s.Tags[0] != "scarpenter-cluster=c" {
		t.Fatalf("server: %+v", s)
	}
	first := (*log)[0].line
	for _, want := range []string{"/servers/detail", "name=%5Enc-1%24", "tags=a%2Cb", "status=ACTIVE"} {
		if !strings.Contains(first, want) {
			t.Errorf("query %q lacks %q", first, want)
		}
	}
	if (*log)[0].version != "2.67" {
		t.Errorf("microversion header %q", (*log)[0].version)
	}
}

func TestCreateBody(t *testing.T) {
	cfg := true
	body, err := CreateBody(osapi.CreateServerRequest{
		Name: "nc-1", FlavorID: "f1", ImageID: "i1", NetworkIDs: []string{"net1"}, SecurityGroups: []string{"nodes"},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), ConfigDrive: &cfg, AvailabilityZone: "eu-west-par-a",
		Metadata: map[string]string{"scarpenter-idempotency-key": "uid"}, Tags: []string{"t"}, SchedulerGroup: "7f3c0f9e-2d1a-4a55-9d62-0a7a3d9f6b11",
		BlockDevices: []osapi.BlockDevice{{SourceType: "blank", DestinationType: "volume", BootIndex: -1, SizeGiB: 100, VolumeType: "high-speed", DeleteOnTermination: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(body)
	want := `{"os:scheduler_hints":{"group":"7f3c0f9e-2d1a-4a55-9d62-0a7a3d9f6b11"},"server":{"availability_zone":"eu-west-par-a","block_device_mapping_v2":[{"boot_index":-1,"delete_on_termination":true,"destination_type":"volume","source_type":"blank","volume_size":100,"volume_type":"high-speed"}],"config_drive":true,"flavorRef":"f1","imageRef":"i1","metadata":{"scarpenter-idempotency-key":"uid"},"name":"nc-1","networks":[{"uuid":"net1"}],"security_groups":[{"name":"nodes"}],"tags":["t"],"user_data":"eyJpZ25pdGlvbiI6eyJ2ZXJzaW9uIjoiMy40LjAifX0="}}`
	if string(got) != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	// No hint: none sent; user data that is valid base64 is still encoded
	// (gophercloud would send it as is).
	body, _ = CreateBody(osapi.CreateServerRequest{
		Name: "n", FlavorID: "f", ImageID: "i", NetworkIDs: []string{"net1"}, UserData: []byte("abcd"),
	})
	srv, _ := body["server"].(map[string]any)
	ud, _ := srv["user_data"].(*string)
	if body["os:scheduler_hints"] != nil || ud == nil || *ud != "YWJjZA==" {
		t.Fatalf("defaults: %v", body)
	}
	// No network is refused: never "auto" (Neutron's auto-allocated topology,
	// not Ext-Net) and never a server without a port.
	if _, err := CreateBody(osapi.CreateServerRequest{Name: "n", FlavorID: "f", ImageID: "i"}); !errors.Is(err, errNoNetworks) {
		t.Fatalf("no network: %v", err)
	}
}

func TestCreateServerReadsBack(t *testing.T) {
	sc, log := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			status(w, 202, `{"server":{"id":"new-1","links":[]}}`)
			return
		}
		status(w, 200, `{"server":`+fmt.Sprintf(serverJSON, "new-1", "1", "BUILD")+`}`)
	})
	s, err := NewCompute(sc).CreateServer(context.Background(), osapi.CreateServerRequest{Name: "nc-1", FlavorID: "f", ImageID: "i", NetworkIDs: []string{"n"}})
	if err != nil || s.ID != "new-1" || s.Status != "BUILD" || s.FlavorName != "b3-8" {
		t.Fatalf("%+v %v", s, err)
	}
	if len(*log) != 2 || !strings.HasPrefix((*log)[1].line, "GET /servers/new-1") {
		t.Fatalf("calls: %v", *log)
	}
	// The read-back failing must not lose the ID of the server that was created.
	sc, _ = serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			status(w, 202, `{"server":{"id":"new-2"}}`)
			return
		}
		status(w, 503, `{}`)
	})
	s, err = NewCompute(sc).CreateServer(context.Background(), osapi.CreateServerRequest{Name: "nc-2", FlavorID: "f", ImageID: "i", NetworkIDs: []string{"n"}})
	if err != nil || s.ID != "new-2" || s.Name != "nc-2" || s.Status != osapi.StatusBuild {
		t.Fatalf("%+v %v", s, err)
	}
	// A failing create is the error.
	sc, _ = serve(t, func(w http.ResponseWriter, r *http.Request) {
		status(w, 403, `{"forbidden":{"message":"Quota exceeded for instances: Requested 1, but already used 10 of 10 instances"}}`)
	})
	if _, err = NewCompute(sc).CreateServer(context.Background(), osapi.CreateServerRequest{Name: "x", NetworkIDs: []string{"n"}}); !strings.Contains(err.Error(), "Quota exceeded") {
		t.Fatalf("%v", err)
	}
}

func TestComputeSmallCalls(t *testing.T) {
	ctx := context.Background()
	sc, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flavors/detail"):
			status(w, 200, `{"flavors":[{"id":"abc","name":"b3-8","vcpus":2,"ram":8192,"disk":50}]}`)
		case r.URL.Path == "/limits":
			status(w, 200, `{"limits":{"absolute":{"maxTotalInstances":20,"totalInstancesUsed":3,"maxTotalCores":40,"totalCoresUsed":6,"maxTotalRAMSize":163840,"totalRAMUsed":24576,"maxServerGroups":10,"totalServerGroupsUsed":1,"maxSecurityGroups":50,"totalSecurityGroupsUsed":2}}}`)
		case r.URL.Path == "/os-availability-zone":
			status(w, 200, `{"availabilityZoneInfo":[{"zoneName":"eu-west-par-a","zoneState":{"available":true},"hosts":null},{"zoneName":"eu-west-par-b","zoneState":{"available":false},"hosts":null}]}`)
		case r.URL.Path == "/os-server-groups" && r.Method == http.MethodGet:
			status(w, 200, `{"server_groups":[{"id":"g1","name":"n","policy":"anti-affinity","members":["s1"]},{"id":"g0","name":"old","policies":["affinity"],"members":[]}]}`)
		case r.URL.Path == "/os-server-groups":
			status(w, 200, `{"server_group":{"id":"g2","name":"n","policy":"affinity","members":[]}}`)
		}
	})
	c := NewCompute(sc)
	fl, err := c.ListFlavors(ctx)
	if err != nil || len(fl) != 1 || fl[0] != (osapi.Flavor{ID: "abc", Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50}) {
		t.Fatalf("flavors %v %v", fl, err)
	}
	l, err := c.GetLimits(ctx)
	want := osapi.Limits{MaxInstances: 20, UsedInstances: 3, MaxCores: 40, UsedCores: 6, MaxRAMMiB: 163840, UsedRAMMiB: 24576, MaxServerGroups: 10, UsedServerGroups: 1, MaxSecurityGroups: 50, UsedSecurityGroups: 2}
	if err != nil || *l != want {
		t.Fatalf("limits %+v %v", l, err)
	}
	zs, err := c.ListAvailabilityZones(ctx)
	if err != nil || len(zs) != 2 || !zs[0].Available || zs[1].Available {
		t.Fatalf("zones %v %v", zs, err)
	}
	gs, err := c.ListServerGroups(ctx)
	if err != nil || len(gs) != 2 || gs[0].Policy != "anti-affinity" || gs[1].Policy != "affinity" {
		t.Fatalf("groups %v %v (policy field from 2.64, policies before)", gs, err)
	}
	g, err := c.CreateServerGroup(ctx, "n", "affinity")
	if err != nil || g.ID != "g2" || g.Policy != "affinity" {
		t.Fatalf("create group %v %v", g, err)
	}
}

func neutronGlanceCinderServer(t *testing.T) (*gophercloud.ServiceClient, *[]seen) {
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.0/networks":
			status(w, 200, `{"networks":[{"id":"n1","name":"Ext-Net","router:external":true},{"id":"n2","name":"priv","router:external":false}]}`)
		case "/v2.0/security-groups":
			status(w, 200, `{"security_groups":[{"id":"g1","name":"default","description":"d","tags":["a"],"security_group_rules":[{"id":"r1","direction":"ingress","ethertype":"IPv4","protocol":"udp","port_range_min":41641,"port_range_max":41641,"remote_ip_prefix":"0.0.0.0/0","security_group_id":"g1"}]}]}`)
		case "/v2.0/security-group-rules":
			status(w, 201, `{"security_group_rule":{"id":"r2","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22,"remote_group_id":"g1","security_group_id":"g1"}}`)
		case "/v2/images":
			status(w, 200, `{"images":[{"id":"i1","name":"flatcar-stable-4757.2.3-amd64","status":"active","visibility":"private","owner":"p","tags":["flatcar"],"virtual_size":13200000000,"architecture":"x86_64","size":594542592,"created_at":"2026-11-10T12:00:00Z"}]}`)
		case "/v3/types":
			status(w, 200, `{"volume_types":[{"id":"t1","name":"high-speed"}]}`)
		default:
			status(w, 404, `{}`)
		}
	})
}

func TestNeutron(t *testing.T) {
	ctx := context.Background()
	sc, log := neutronGlanceCinderServer(t)
	sc.ResourceBase = sc.Endpoint + "v2.0/"
	n := NewNetwork(sc)
	nets, err := n.ListNetworks(ctx)
	if err != nil || len(nets) != 2 || !nets[0].External || nets[1].External {
		t.Fatalf("networks %v %v", nets, err)
	}
	gs, err := n.ListSecurityGroups(ctx)
	if err != nil || len(gs) != 1 || gs[0].Tags[0] != "a" || len(gs[0].Rules) != 1 ||
		gs[0].Rules[0] != (osapi.SecurityRule{ID: "r1", Direction: "ingress", EtherType: "IPv4", Protocol: "udp", PortMin: 41641, PortMax: 41641, RemoteCIDR: "0.0.0.0/0"}) {
		t.Fatalf("groups %+v %v", gs, err)
	}
	r, err := n.CreateSecurityRule(ctx, "g1", osapi.SecurityRule{Direction: "ingress", EtherType: "IPv4", Protocol: "tcp", PortMin: 22, PortMax: 22, RemoteGroupID: "g1"})
	if err != nil || r.RemoteGroupID != "g1" {
		t.Fatalf("rule %+v %v", r, err)
	}
	var sent map[string]map[string]any
	_ = json.Unmarshal([]byte((*log)[len(*log)-1].body), &sent)
	if sent["security_group_rule"]["security_group_id"] != "g1" || sent["security_group_rule"]["remote_group_id"] != "g1" {
		t.Fatalf("rule body %v", sent)
	}
}

func TestGlance(t *testing.T) {
	ctx := context.Background()
	sc, log := neutronGlanceCinderServer(t)
	sc.ResourceBase = sc.Endpoint + "v2/"
	ims, err := NewImages(sc).ListImages(ctx, osapi.ImageFilter{Name: "flatcar-stable-4757.2.3-amd64", Status: "active", Tags: []string{"flatcar"}, Owner: "p"})
	if err != nil || len(ims) != 1 {
		t.Fatalf("images %v %v", ims, err)
	}
	im := ims[0]
	if im.ID != "i1" || im.Status != "active" || im.Visibility != "private" || im.Owner != "p" || im.VirtualSizeBytes != 13200000000 || im.Properties["architecture"] != "x86_64" {
		t.Fatalf("image %+v", im)
	}
	q := (*log)[len(*log)-1].line
	for _, want := range []string{"name=flatcar-stable-4757.2.3-amd64", "status=active", "tag=flatcar", "owner=p"} {
		if !strings.Contains(q, want) {
			t.Errorf("image query %q lacks %q", q, want)
		}
	}
}

func TestCinder(t *testing.T) {
	ctx := context.Background()
	sc, _ := neutronGlanceCinderServer(t)
	sc.ResourceBase = sc.Endpoint + "v3/"
	vts, err := NewVolumes(sc).ListVolumeTypes(ctx)
	if err != nil || len(vts) != 1 || vts[0].Name != "high-speed" {
		t.Fatalf("volume types %v %v", vts, err)
	}
	if _, err := (noVolumes{}).ListVolumeTypes(ctx); !errors.Is(err, osapi.ErrNoEndpoint) {
		t.Fatal(err)
	}
}

func TestComputeRegions(t *testing.T) {
	ep := func(region, iface string) tokens.Endpoint { return tokens.Endpoint{Region: region, Interface: iface} }
	cat := &tokens.ServiceCatalog{Entries: []tokens.CatalogEntry{
		{Type: "compute", Endpoints: []tokens.Endpoint{ep("GRA11", "public"), ep("GRA11", "internal"), ep("EU-WEST-PAR", "public"), ep("", "public")}},
		{Type: "network", Endpoints: []tokens.Endpoint{ep("SBG5", "public")}},
		{Type: "compute", Endpoints: []tokens.Endpoint{ep("DE1", "admin")}},
	}}
	if got := strings.Join(computeRegions(cat), ","); got != "EU-WEST-PAR,GRA11" {
		t.Fatalf("regions %q", got)
	}
}

func TestEndpointNotFound(t *testing.T) {
	err := convertEndpoint(&gophercloud.ErrEndpointNotFound{})
	if !errors.Is(err, osapi.ErrNoEndpoint) {
		t.Fatalf("%v", err)
	}
	if errors.Is(convertEndpoint(errors.New("x")), osapi.ErrNoEndpoint) {
		t.Fatal("other errors are not a missing endpoint")
	}
}

// New wires gophercloud's retries: a 429 is retried on any method (Retry-After
// honoured), a 502/503/504 only on idempotent methods, a POST never.
func TestRetries(t *testing.T) {
	var gets, posts, throttled int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/throttled":
			throttled++
			if throttled < 3 {
				w.Header().Set("Retry-After", "0")
				status(w, 429, `{"message":"slow down"}`)
				return
			}
			status(w, 202, `{}`)
		case r.Method == http.MethodGet:
			gets++
			if gets < 3 {
				status(w, 503, `{"message":"unavailable"}`)
				return
			}
			status(w, 200, `{}`)
		default:
			posts++
			status(w, 502, `{"message":"bad gateway"}`)
		}
	}))
	t.Cleanup(ts.Close)
	pc := &gophercloud.ProviderClient{HTTPClient: *ts.Client()}
	New(pc, Options{BaseBackoff: time.Millisecond, MaxBackoffRetries: 4})
	ctx := context.Background()
	if _, err := pc.Request(ctx, http.MethodGet, ts.URL+"/x", &gophercloud.RequestOpts{}); err != nil || gets != 3 {
		t.Fatalf("GET 503 twice then 200: err %v after %d calls", err, gets)
	}
	if _, err := pc.Request(ctx, http.MethodPost, ts.URL+"/servers", &gophercloud.RequestOpts{JSONBody: map[string]any{}}); err == nil || posts != 1 {
		t.Fatalf("POST 502 is not retried: err %v after %d calls", err, posts)
	}
	if _, err := pc.Request(ctx, http.MethodPost, ts.URL+"/throttled", &gophercloud.RequestOpts{JSONBody: map[string]any{}}); err != nil || throttled != 3 {
		t.Fatalf("429 twice then 200: err %v after %d calls", err, throttled)
	}
	gets = -10 // always 503 from now on, until the retries run out
	if _, err := pc.Request(ctx, http.MethodGet, ts.URL+"/x", &gophercloud.RequestOpts{}); err == nil || gets != -5 {
		t.Fatalf("GET retries stop at MaxBackoffRetries: err %v, calls %d", err, gets+10)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := backoff429(time.Hour)(cctx, nil, errors.New("e"), 1); err == nil {
		t.Fatal("a canceled context stops the backoff")
	}
	if d := backoffWait(time.Second, 40); d > 30*time.Second || d < 15*time.Second {
		t.Fatalf("backoff cap: %v", d)
	}
}

func TestDeletes(t *testing.T) {
	ctx := context.Background()
	sc, log := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			status(w, 404, `{"NeutronError":{"type":"SecurityGroupNotFound","message":"nope"}}`)
			return
		}
		status(w, 204, "")
	})
	sc.ResourceBase = sc.Endpoint + "v2.0/"
	n := NewNetwork(sc)
	if err := n.DeleteSecurityRule(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := n.DeleteSecurityGroup(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	var ae *osapi.APIError
	if err := n.DeleteSecurityRule(ctx, "gone"); !errors.As(err, &ae) || ae.Status != 404 || ae.Service != "neutron" {
		t.Errorf("a missing rule: %v", err)
	}
	got := make([]string, 0, len(*log))
	for _, s := range *log {
		got = append(got, s.line)
	}
	if got[0] != "DELETE /v2.0/security-group-rules/r1" || got[1] != "DELETE /v2.0/security-groups/g1" {
		t.Errorf("requests %v", got)
	}
	c := NewCompute(sc)
	c.sc.ResourceBase = sc.Endpoint
	if err := c.DeleteServerGroup(ctx, "sg1"); err != nil {
		t.Fatal(err)
	}
	if last := (*log)[len(*log)-1].line; last != "DELETE /os-server-groups/sg1" {
		t.Errorf("request %s", last)
	}
}

func TestEnvConfigured(t *testing.T) {
	for _, k := range []string{"OS_AUTH_URL", "OS_USERNAME", "OS_PASSWORD", "OS_PROJECT_ID", "OS_PROJECT_NAME", "OS_TENANT_ID", "OS_TENANT_NAME"} {
		t.Setenv(k, "")
	}
	if err := EnvConfigured(); err == nil {
		t.Error("an empty environment is not configured")
	}
	t.Setenv("OS_AUTH_URL", "https://auth.example.test/v3")
	t.Setenv("OS_USERNAME", "u")
	t.Setenv("OS_PASSWORD", "p")
	t.Setenv("OS_PROJECT_ID", "0a1b")
	if err := EnvConfigured(); err != nil {
		t.Errorf("a complete one is: %v", err)
	}
}
