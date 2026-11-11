/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// engineEnv is the driver behind the real resource engine, which decides where
// each resource comes from (use, discovery, managed), and a Kubernetes client that
// holds the NodeClass, which Prepare reads the resource configuration from.
type engineEnv struct {
	*env
	engine *resources.Engine
	nc     *v1alpha1.SCNodeClass
}

func newEngineEnv(t *testing.T, nc *v1alpha1.SCNodeClass) *engineEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nc).Build()
	e := newEnv(t, envOptions{reader: kube})
	return &engineEnv{env: e, engine: newEngine(e, kube), nc: nc}
}

func newEngine(e *env, kube client.Reader) *resources.Engine {
	return resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(e.d), Reader: kube, Clock: e.clock, ClusterName: testCluster,
		Regions: func(string) []string { return []string{testZone} },
		Zones:   func(string, string) []string { return []string{testZone} },
	})
}

func (e *engineEnv) d2reader() client.Reader { return e.opts.reader }

func (e *engineEnv) prepare(s vendors.VendorSettings, runKey string) (*vendors.Prepared, error) {
	return e.d.Prepare(e.ctx(), vendors.PrepareRequest{
		Region: testZone, Zone: testZone, InstanceType: testType, CapacityType: vendors.CapacityOnDemand,
		Type:     vendors.CatalogType{Vendor: "upcloud", APIReference: testType, LocalStorageGB: 50},
		Settings: s, RunKey: runKey,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClaim: "claim", vendors.TagNodeClass: e.nc.Name,
			vendors.TagManagedBy: vendors.ManagedByValue,
		},
		Resources: e.engine.Provider(e.nc, "upcloud", testZone, testZone),
	})
}

func ids(p *vendors.Prepared, kind vendors.ResourceKind) []string {
	var out []string
	for _, r := range p.Resources {
		if r.Kind == kind {
			out = append(out, r.ID)
		}
	}
	return out
}

func TestPrepareGivesNoPrivateNetworkWithoutConfiguration(t *testing.T) {
	e := newEngineEnv(t, class(testVersion, nil, nil))
	p, err := e.prepare(settings(t, e.d, "", defaultNet(), nil, options.Selection{}), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Resources) != 0 {
		t.Errorf("resources %v, want none: there is no default network", p.Resources)
	}
	if n := len(mustList(t, e.env)); n != 0 {
		t.Errorf("%d managed resources created", n)
	}
}

func mustList(t *testing.T, e *env) []vendors.ResourceRef {
	t.Helper()
	l, err := e.d.List(e.ctx(), nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPrepareCreatesTheNetworkWhereTheNodeClassConfiguresIt(t *testing.T) {
	nc := class(testVersion, nil, nil)
	nc.Spec.Vendors.UpCloud.Resources.Networks = &common.Resource{Create: &common.ResourceCreate{}}
	e := newEngineEnv(t, nc)
	s := settings(t, e.d, "", defaultNet(), nil, options.Selection{})
	first, err := e.prepare(s, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(first, uc.KindNetworks); len(got) != 1 || len(ids(first, uc.KindFirewalls)) != 0 {
		t.Fatalf("resources %v", first.Resources)
	}
	second, err := e.prepare(s, "")
	if err != nil || ids(second, uc.KindNetworks)[0] != ids(first, uc.KindNetworks)[0] {
		t.Errorf("a second Prepare: %v, %v", second, err)
	}
	nets, _ := e.f.GetNetworks(e.ctx())
	if len(nets.Networks) != 1 || !strings.HasPrefix(nets.Networks[0].IPNetworks[0].Address, "10.241.") ||
		!strings.HasSuffix(nets.Networks[0].IPNetworks[0].Address, ".0/24") {
		t.Errorf("networks %+v", nets.Networks)
	}
}

func TestIsolationNodeClassNeedsTheNetworkAndARulesetOfItsRange(t *testing.T) {
	e := newEngineEnv(t, class(testVersion, nil, nil))
	net := defaultNet()
	net.Isolation = common.IsolationNodeClass
	net.Inbound = &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", FromPort: new(int32(443)), CIDRs: []string{"192.0.2.0/24"}}}}
	p, err := e.prepare(settings(t, e.d, "", net, nil, options.Selection{}), "")
	if err != nil {
		t.Fatal(err)
	}
	rs := ids(p, uc.KindFirewalls)
	if len(ids(p, uc.KindNetworks)) != 1 || len(rs) != 1 {
		t.Fatalf("resources %v", p.Resources)
	}
	sets, _ := e.f.GetFirewallRulesets(e.ctx())
	full, err := e.f.GetFirewallRuleset(e.ctx(), rs[0])
	if err != nil || len(sets) != 1 || !full.Stateful {
		t.Fatalf("rulesets %v, %v", sets, err)
	}
	var fromRange, inbound, drop bool
	for _, r := range full.Rules {
		fromRange = fromRange || (r.Comment == "nodeclass" && strings.HasPrefix(r.CIDR, "10.241."))
		inbound = inbound || (r.Comment == "inbound" && r.PortStart == 443 && r.CIDR == "192.0.2.0/24")
		drop = drop || (r.Action == "drop" && r.Direction == "in")
	}
	if !fromRange || !inbound || !drop {
		t.Errorf("rules %+v", full.Rules)
	}
	nodeWithTheRuleset(t, e, p, rs[0])
}

// nodeWithTheRuleset launches a node with the prepared resources: it has a private interface, the ruleset attached and
// the private firewall on; the ruleset is in use while it runs, refused for deletion, and goes after the node.
func nodeWithTheRuleset(t *testing.T, e *engineEnv, p *vendors.Prepared, ruleset string) {
	t.Helper()
	rs := []string{ruleset}
	req := e.request("n1", testType)
	req.Prepared = p
	inst, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := e.f.Server(inst.Ref.ID())
	var private bool
	for _, i := range srv.Networking.Interfaces {
		private = private || i.Type == upcloud.NetworkTypePrivate
	}
	if !private || srv.FirewallPrivate != "on" || len(e.f.RulesetServers(rs[0])) != 1 {
		t.Errorf("private %v firewall %q ruleset servers %v", private, srv.FirewallPrivate, e.f.RulesetServers(rs[0]))
	}
	// The ruleset is in use while a node has a private interface, and is refused
	// for deletion; it goes with the node.
	ref := vendors.ResourceRef{Kind: uc.KindFirewalls, Region: testZone, ID: rs[0]}
	if used, _, err := e.d.InUse(e.ctx(), ref); err != nil || !used {
		t.Errorf("InUse = %v, %v", used, err)
	}
	if err := e.d.Delete(e.ctx(), ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("Delete of an attached ruleset: %v", err)
	}
	if err := e.d.DeleteInstance(e.ctx(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if used, _, err := e.d.InUse(e.ctx(), ref); err != nil || used {
		t.Errorf("InUse after the node = %v, %v", used, err)
	}
	if err := e.d.Delete(e.ctx(), ref); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := e.d.Delete(e.ctx(), ref); err != nil {
		t.Errorf("a second Delete: %v", err)
	}
	if l := mustList(t, e.env); len(l) != 1 || l[0].Kind != uc.KindNetworks {
		t.Errorf("managed resources after the ruleset: %v", l)
	}
}

func TestIsolationRunWithARunKeyIsUnsupported(t *testing.T) {
	e := newEngineEnv(t, class(testVersion, nil, nil))
	net := defaultNet() // isolation run is the API default
	_, err := e.prepare(settings(t, e.d, "", net, nil, options.Selection{}), "run-1")
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != "IsolationRunUnsupported" {
		t.Errorf("isolation run with a run key: %v", err)
	}
	if _, err := e.prepare(settings(t, e.d, "", net, nil, options.Selection{}), ""); err != nil {
		t.Errorf("without a run key the nodes are one run: %v", err)
	}
}

func TestBroughtNetworksAndRulesetsAreUsedAsTheyAre(t *testing.T) {
	nc := class(testVersion, nil, nil)
	nc.Spec.Vendors.UpCloud.Resources.Networks = &common.Resource{Use: &common.ResourceUse{
		Tags: map[string]string{uc.LabelUse: "true"}}}
	nc.Spec.Vendors.UpCloud.Resources.Firewalls = &common.Resource{Use: &common.ResourceUse{Name: "byo-*"}}
	e := newEngineEnv(t, nc)
	if _, err := e.prepare(settings(t, e.d, "", defaultNet(), nil, options.Selection{}), ""); !vendors.IsClass(err, vendors.Unsupported) {
		t.Fatalf("a use that matches nothing: %v", err)
	}
	n, err := e.f.CreateNetwork(e.ctx(), &request.CreateNetworkRequest{
		Name: "byo", Zone: testZone, Labels: []upcloud.Label{{Key: uc.LabelUse, Value: "true"}},
		IPNetworks: upcloud.IPNetworkSlice{{Address: "172.30.0.0/24", Family: upcloud.IPAddressFamilyIPv4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.f.CreateFirewallRuleset(e.ctx(), api.CreateRulesetRequest{Name: "byo-1", Stateful: true})
	if err != nil {
		t.Fatal(err)
	}
	e.engine = newEngine(e.env, e.d2reader())
	p, err := e.prepare(settings(t, e.d, "", defaultNet(), nil, options.Selection{}), "")
	if err != nil {
		t.Fatal(err)
	}
	if ids(p, uc.KindNetworks)[0] != n.UUID || ids(p, uc.KindFirewalls)[0] != r.UUID {
		t.Errorf("resources %v, want the brought ones", p.Resources)
	}
	for _, ref := range p.Resources {
		if ref.Managed {
			t.Errorf("%s is managed", ref.ID)
		}
	}
	if l := mustList(t, e.env); len(l) != 0 {
		t.Errorf("brought resources show up as managed: %v", l)
	}
}

func TestServerGroupsOfThePlacementProfile(t *testing.T) {
	e := newEngineEnv(t, class(testVersion, nil, nil))
	d := e.d
	sel := options.Selection{Placement: "spread"}
	none := defaultNet()
	none.Isolation = common.IsolationNone
	prof := options.Input{Block: json.RawMessage(`{}`), Networking: none, Profiles: map[string]map[string]json.RawMessage{
		vendors.ProfilePlacement: {
			"spread":   json.RawMessage(`{"serverGroup":{"antiAffinity":"yes"}}`),
			"strict":   json.RawMessage(`{"serverGroup":{}}`),
			"existing": json.RawMessage(`{"serverGroup":{"use":{"name":"theirs"}}}`),
		},
	}}
	s, err := options.Resolve(d, prof, sel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepare(s, ""); vendors.CodeOf(err) != "InvalidSettings" {
		t.Errorf("a profile that creates groups without a run key: %v", err)
	}
	p, err := e.prepare(s, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	g := ids(p, uc.KindServerGroups)
	if len(g) != 1 {
		t.Fatalf("resources %v", p.Resources)
	}
	groups, _ := e.f.GetServerGroups(e.ctx(), &request.GetServerGroupsRequest{})
	if len(groups) != 1 || groups[0].AntiAffinityPolicy != upcloud.ServerGroupAntiAffinityPolicyBestEffort {
		t.Errorf("groups %+v", groups)
	}
	// A launch with the group is a member of it, and the group is in use.
	req := e.request("n1", testType)
	req.Prepared = p
	inst, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	ref := p.Resources[0]
	if used, _, err := e.d.InUse(e.ctx(), ref); err != nil || !used {
		t.Errorf("InUse = %v, %v", used, err)
	}
	if err := e.d.Delete(e.ctx(), ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("Delete of a group with members: %v", err)
	}
	if err := e.d.DeleteInstance(e.ctx(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	// The default policy is strict, and a group by title is brought.
	s2, _ := options.Resolve(d, prof, options.Selection{Placement: "strict"})
	if _, err := e.prepare(s2, "run-2"); err != nil {
		t.Fatal(err)
	}
	groups, _ = e.f.GetServerGroups(e.ctx(), &request.GetServerGroupsRequest{})
	var strict bool
	for _, g := range groups {
		strict = strict || g.AntiAffinityPolicy == upcloud.ServerGroupAntiAffinityPolicyStrict
	}
	if !strict {
		t.Errorf("groups %+v", groups)
	}
	s3, _ := options.Resolve(d, prof, options.Selection{Placement: "existing"})
	if _, err := e.prepare(s3, ""); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("a group that does not exist: %v", err)
	}
	if _, err := e.f.CreateServerGroup(e.ctx(), &request.CreateServerGroupRequest{Title: "theirs"}); err != nil {
		t.Fatal(err)
	}
	e.engine = newEngine(e.env, e.d2reader())
	if p, err := e.prepare(s3, ""); err != nil || len(ids(p, uc.KindServerGroups)) != 1 {
		t.Errorf("a brought group: %v, %v", p, err)
	}
}

func TestDataDisksGetDeviceHints(t *testing.T) {
	e := newEngineEnv(t, class(testVersion, nil, nil))
	s := settings(t, e.d, `{"storage":{"data":[{"name":"pg","tier":"maxiops","size":"100Gi","mountPath":"/var/lib/pg","filesystem":"xfs"}]}}`,
		defaultNet(), nil, options.Selection{})
	p, err := e.prepare(s, "")
	if err != nil || len(p.Devices) != 1 {
		t.Fatalf("Prepare: %v, %v", p, err)
	}
	d := p.Devices[0]
	if d.Name != "pg" || d.Filesystem != "xfs" || d.MountPath != "/var/lib/pg" || d.SerialPattern == "" || d.SizeBytes != 100<<30 || d.ByID != "" {
		t.Errorf("device hint %+v", d)
	}
}

func TestPrepareWithoutTheNodeClassReaderUsesNoConfiguration(t *testing.T) {
	e := newEnv(t, envOptions{})
	p, err := e.d.Prepare(context.Background(), vendors.PrepareRequest{
		Region: testZone, Zone: testZone, Settings: settings(t, e.d, "", defaultNet(), nil, options.Selection{}),
		Tags: map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
	})
	if err != nil || len(p.Resources) != 0 {
		t.Errorf("Prepare = %v, %v", p, err)
	}
}
