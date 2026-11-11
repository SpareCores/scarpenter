/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

func TestCreateServerOnFake(t *testing.T) {
	e := newEnv(t, fake.Options{})
	r, err := BuildCreateServer(e.spec("nc-a", "1xCPU-2GB"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := CreateServer(e.ctx, e.f, r)
	if err != nil {
		t.Fatal(err)
	}
	// Cloning: the server is in maintenance, pending for the core.
	if d.State != "maintenance" || InstanceState(d.State) != vendors.StatePending {
		t.Fatalf("state = %s", d.State)
	}
	e.f.Settle()
	got, err := GetServer(e.ctx, e.f, d.UUID)
	if err != nil {
		t.Fatal(err)
	}
	in := ToInstance(got)
	if in.State != vendors.StateRunning || in.Type != "1xCPU-2GB" || in.Tags[vendors.TagNodeClaim] != "nc-a" {
		t.Fatalf("instance = %+v", in)
	}
	if !got.Metadata.Bool() || len(got.StorageDevices) != 1 || got.StorageDevices[0].Size != 50 {
		t.Errorf("details: metadata %v devices %+v", got.Metadata, got.StorageDevices)
	}
	if len(in.Addresses) != 2 {
		t.Errorf("addresses = %v, want public and utility", in.Addresses)
	}
	if _, ok := BillingStart(got); !ok {
		t.Error("no billing start label")
	}
}

func TestFindByIdempotencyKey(t *testing.T) {
	e := newEnv(t, fake.Options{})
	if d, err := FindByIdempotencyKey(e.ctx, e.f, "uid-nc-a"); err != nil || d != nil {
		t.Fatalf("empty account: %v %v", d, err)
	}
	if d, err := FindByIdempotencyKey(e.ctx, e.f, ""); err != nil || d != nil {
		t.Fatalf("empty key: %v %v", d, err)
	}
	first := e.launch("nc-a", "1xCPU-2GB")
	e.launch("nc-b", "1xCPU-1GB")
	e.f.AddServer(fake.ServerSpec{Zone: "fi-hel1", Plan: "1xCPU-1GB", Title: "other"})
	got, err := FindByIdempotencyKey(e.ctx, e.f, "uid-nc-a")
	if err != nil || got == nil || got.UUID != first.UUID {
		t.Fatalf("lookup = %v %v, want %s", got, err, first.UUID)
	}
	if got.Labels == nil {
		t.Error("the lookup returned a list summary without labels")
	}
	// One list call for all zones, then the details of the match.
	e.f.Clear("")
	before := e.f.Calls("GetServersWithFilters")
	if _, err := FindByIdempotencyKey(e.ctx, e.f, "uid-nc-a"); err != nil {
		t.Fatal(err)
	}
	if e.f.Calls("GetServersWithFilters")-before != 1 {
		t.Error("the lookup is not a single list call")
	}
}

// TestReplayWithoutLookupDuplicates shows why the lookup exists: UpCloud has no
// idempotency token, so a replayed create makes a second server.
func TestReplayWithoutLookupDuplicates(t *testing.T) {
	e := newEnv(t, fake.Options{})
	e.launch("nc-a", "1xCPU-2GB")
	e.launch("nc-a", "1xCPU-2GB")
	if got := len(e.f.Servers()); got != 2 {
		t.Fatalf("servers = %d", got)
	}
	// A lost create response: the lookup finds the server by its label, in whatever zone or plan.
	e2 := newEnv(t, fake.Options{})
	e2.f.Inject("CreateServer", errLostResponse(), 0)
	r, _ := BuildCreateServer(e2.spec("nc-a", "1xCPU-2GB"))
	e2.f.Clear("CreateServer")
	if _, err := CreateServer(e2.ctx, e2.f, r); err != nil {
		t.Fatal(err)
	}
	e2.f.Settle()
	if d, _ := FindByIdempotencyKey(e2.ctx, e2.f, "uid-nc-a"); d == nil {
		t.Fatal("the lookup lost the server")
	}
}

func TestFindByIdempotencyKeyReapsPartial(t *testing.T) {
	e := newEnv(t, fake.Options{AutoSettle: true})
	// A server with the label and no storage: a create that failed half way.
	partial := e.f.AddServer(fake.ServerSpec{
		Zone: "fi-hel1", Plan: "CLOUDNATIVE-2xCPU-4GB", Title: "nc-p",
		Labels: map[string]string{"scarpenter-idempotency-key": "uid-p"},
	})
	got, err := FindByIdempotencyKey(e.ctx, e.f, "uid-p")
	if err != nil || got != nil {
		t.Fatalf("lookup = %v %v: a partial server is not a result", got, err)
	}
	if _, ok := e.f.Server(partial); ok {
		t.Error("the partial server was not deleted")
	}
}

func TestTerminate(t *testing.T) {
	e := newEnv(t, fake.Options{})
	d := e.launch("nc-a", "1xCPU-2GB")
	nStorages := len(e.f.Storages())
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.f.Server(d.UUID); ok {
		t.Error("server still exists")
	}
	if got := len(e.f.Storages()); got != nStorages-1 {
		t.Errorf("storages %d -> %d: the root disk must go with the server", nStorages, got)
	}
	// Gone, and a malformed one, are success: DeleteInstance is idempotent.
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Errorf("second terminate: %v", err)
	}
}

func TestTerminateStates(t *testing.T) {
	e := newEnv(t, fake.Options{})
	// Cloning: Retryable, nothing is touched.
	r, _ := BuildCreateServer(e.spec("nc-a", "1xCPU-2GB"))
	d, err := CreateServer(e.ctx, e.f, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := Terminate(e.ctx, e.f, d.UUID); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("maintenance: %v", err)
	}
	if _, ok := e.f.Server(d.UUID); !ok {
		t.Fatal("a server in maintenance was deleted")
	}
	if e.f.Calls("StopServer") != 0 {
		t.Error("stop called on a server in maintenance")
	}
	e.f.Settle()
	// A stopped server (guest poweroff) is deleted without a stop call.
	e.f.ForceState(d.UUID, upcloud.ServerStateStopped)
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatal(err)
	}
	if e.f.Calls("StopServer") != 0 {
		t.Error("stop called on a stopped server")
	}
	// A server in error is stopped first as well.
	d2 := e.launch("nc-b", "1xCPU-2GB")
	e.f.ForceState(d2.UUID, upcloud.ServerStateError)
	if err := Terminate(e.ctx, e.f, d2.UUID); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("error state: the fake refuses a stop of a server in error, got %v", err)
	}
}

func TestTerminateInjectedErrors(t *testing.T) {
	e := newEnv(t, fake.Options{})
	d := e.launch("nc-a", "1xCPU-2GB")
	// Throttled stop: Retryable, server untouched.
	e.f.InjectCode("StopServer", "SERVICE_ERROR", 503, 1)
	if err := Terminate(e.ctx, e.f, d.UUID); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("stop 503: %v", err)
	}
	// The server stopped between the steps: the delete finds it gone: success.
	e.f.InjectCode("DeleteServerAndStorages", "SERVER_NOT_FOUND", 404, 1)
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatalf("delete 404: %v", err)
	}
	// Permission errors are Fatal.
	d2 := e.launch("nc-b", "1xCPU-2GB")
	e.f.InjectCode("StopServer", "ACTION_FORBIDDEN", 403, 1)
	if err := Terminate(e.ctx, e.f, d2.UUID); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("forbidden: %v", err)
	}
}

func TestStateIllegalIsRetryable(t *testing.T) {
	e := newEnv(t, fake.Options{})
	d := e.launch("nc-a", "1xCPU-2GB")
	// A stop already in progress: the second stop is SERVER_STATE_ILLEGAL and the next call retries.
	if _, err := e.f.StopServer(e.ctx, &request.StopServerRequest{UUID: d.UUID}); err != nil {
		t.Fatal(err)
	}
	if err := Terminate(e.ctx, e.f, d.UUID); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("in-progress stop: %v", err)
	}
	e.f.Settle()
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchErrorsClassified(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *env)
		plan  string
		class vendors.Class
		code  string
		wide  bool
	}{
		"out of stock": {func(e *env) { e.f.SetOutOfStock("fi-hel1", "1xCPU-2GB", true) }, "1xCPU-2GB",
			vendors.InsufficientCapacity, "SERVER_RESOURCES_UNAVAILABLE", false},
		"core limit": {func(e *env) { e.f.SetLimits(upcloud.ResourceLimits{Cores: 1}) }, "2xCPU-4GB",
			vendors.QuotaExceeded, "SERVER_CORE_LIMIT_REACHED", true},
		"memory limit": {func(e *env) { e.f.SetLimits(upcloud.ResourceLimits{Memory: 1024}) }, "1xCPU-2GB",
			vendors.QuotaExceeded, "SERVER_MEMORY_LIMIT_REACHED", true},
		"storage limit": {func(e *env) { e.f.SetLimits(upcloud.ResourceLimits{StorageMaxIOPS: 40}) }, "1xCPU-2GB",
			vendors.QuotaExceeded, "MAXIOPS_STORAGE_LIMIT_REACHED", true},
		"ip limit": {func(e *env) {
			e.f.SetLimits(upcloud.ResourceLimits{PublicIPv4: 1})
			e.f.AddServer(fake.ServerSpec{
				Zone: "de-fra1", Plan: "1xCPU-1GB", PublicIPv4: true})
		}, "1xCPU-2GB",
			vendors.QuotaExceeded, "SERVER_IP_LIMIT_REACHED", true},
		"no addresses": {func(e *env) { e.f.SetNoAddresses("fi-hel1", true) }, "1xCPU-2GB",
			vendors.InsufficientCapacity, "IP_ADDRESS_RESOURCES_UNAVAILABLE", false},
		"creating limit": {func(e *env) { e.f.Inject("CreateServer", fake.Problem("SERVER_CREATING_LIMIT_REACHED", 429), 1) }, "1xCPU-2GB",
			vendors.Retryable, "SERVER_CREATING_LIMIT_REACHED", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fake.Options{})
			tc.setup(e)
			servers, storages := len(e.f.Servers()), len(e.f.Storages())
			r, err := BuildCreateServer(e.spec("nc-a", tc.plan))
			if err != nil {
				t.Fatal(err)
			}
			_, err = CreateServer(e.ctx, e.f, r)
			if vendors.ClassOf(err) != tc.class || vendors.CodeOf(err) != tc.code || vendors.IsAccountWide(err) != tc.wide {
				t.Fatalf("err = %v: class %s code %s wide %v", err, vendors.ClassOf(err), vendors.CodeOf(err), vendors.IsAccountWide(err))
			}
			if len(e.f.Servers()) != servers || len(e.f.Storages()) != storages {
				t.Errorf("a failed create left %d servers and %d storages", len(e.f.Servers())-servers, len(e.f.Storages())-storages)
			}
		})
	}
}

func TestPostCreate(t *testing.T) {
	e := newEnv(t, fake.Options{})
	net, err := e.f.CreateNetwork(e.ctx, &request.CreateNetworkRequest{
		Name: "n", Zone: "fi-hel1", IPNetworks: upcloud.IPNetworkSlice{{Address: "10.20.0.0/24", DHCP: upcloud.True, Family: "IPv4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := e.f.CreateFirewallRuleset(e.ctx, apiRuleset("rs"))
	if err != nil {
		t.Fatal(err)
	}
	s := e.spec("nc-a", "1xCPU-2GB")
	s.Networking.PrivateNetwork = net.UUID
	r, err := BuildCreateServer(s)
	if err != nil {
		t.Fatal(err)
	}
	d, err := CreateServer(e.ctx, e.f, r, StorageLabels(TagsOf(*r.Labels))...)
	if err != nil {
		t.Fatal(err)
	}
	// The storages are labelled by the create call itself, while the clone
	// still runs, when the API refuses to modify them.
	for _, dev := range d.StorageDevices {
		st, _ := e.f.GetStorageDetails(e.ctx, &request.GetStorageDetailsRequest{UUID: dev.UUID})
		if st.State != upcloud.StorageStateMaintenance || len(st.Labels) != 3 {
			t.Errorf("storage %s right after create: state %s labels %v", dev.UUID, st.State, st.Labels)
		}
		_, err := e.f.ModifyStorage(e.ctx, &request.ModifyStorageRequest{UUID: dev.UUID, Title: "x"})
		if !vendors.IsClass(Classify(err), vendors.Retryable) {
			t.Errorf("modify a storage in maintenance: %v", err)
		}
	}
	rules, _ := PublicFirewallRules(FirewallOptions{})
	pc := PostCreate{Rules: rules, PrivateRuleset: rs.UUID}
	if err := pc.Run(e.ctx, e.f, d); err != nil {
		t.Fatal(err)
	}
	got, _ := e.f.Server(d.UUID)
	if got.FirewallPrivate != "on" {
		t.Errorf("private firewall = %q", got.FirewallPrivate)
	}
	if srv := e.f.RulesetServers(rs.UUID); len(srv) != 1 || srv[0] != d.UUID {
		t.Errorf("ruleset attached to %v", srv)
	}
	if len(e.f.ServerFirewall(d.UUID)) != len(rules) {
		t.Error("public firewall not written")
	}
	for _, st := range e.f.Storages() {
		if st.Type != upcloud.StorageTypeDisk {
			continue
		}
		keys := map[string]string{}
		for _, l := range st.Labels {
			keys[l.Key] = l.Value
		}
		if keys["scarpenter-cluster"] != "c1" || keys["scarpenter-nodeclaim"] != "nc-a" || keys["scarpenter-managed-by"] != "scarpenter" {
			t.Errorf("storage labels = %v", st.Labels)
		}
	}
	// A ruleset still attached refuses deletion; the server's deletion detaches it.
	if err := e.f.DeleteFirewallRuleset(e.ctx, rs.UUID); !vendors.IsClass(Classify(err), vendors.Retryable) {
		t.Errorf("delete attached ruleset: %v", Classify(err))
	}
	// A network with an interface refuses deletion.
	if err := e.f.DeleteNetwork(e.ctx, &request.DeleteNetworkRequest{UUID: net.UUID}); !vendors.IsClass(Classify(err), vendors.Retryable) {
		t.Errorf("delete network in use: %v", Classify(err))
	}
	e.f.Settle()
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatal(err)
	}
	if err := e.f.DeleteFirewallRuleset(e.ctx, rs.UUID); err != nil {
		t.Errorf("delete ruleset after the server: %v", err)
	}
	if err := e.f.DeleteNetwork(e.ctx, &request.DeleteNetworkRequest{UUID: net.UUID}); err != nil {
		t.Errorf("delete network after the server: %v", err)
	}
}

func TestPostCreateStepFailures(t *testing.T) {
	for _, method := range []string{"CreateFirewallRules", "AttachPrivateFirewallRuleset", "ModifyServer"} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t, fake.Options{})
			net, _ := e.f.CreateNetwork(e.ctx, &request.CreateNetworkRequest{
				Zone: "fi-hel1", IPNetworks: upcloud.IPNetworkSlice{{Address: "10.20.0.0/24"}},
			})
			rs, _ := e.f.CreateFirewallRuleset(e.ctx, apiRuleset("rs"))
			s := e.spec("nc-a", "1xCPU-2GB")
			s.Networking.PrivateNetwork = net.UUID
			r, _ := BuildCreateServer(s)
			d, _ := CreateServer(e.ctx, e.f, r)
			e.f.InjectCode(method, "SERVICE_ERROR", 500, 0)
			err := PostCreate{PrivateRuleset: rs.UUID}.Run(e.ctx, e.f, d)
			if !vendors.IsClass(err, vendors.Retryable) {
				t.Fatalf("err = %v, want Retryable", err)
			}
		})
	}
}
