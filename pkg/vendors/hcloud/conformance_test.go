/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

const conformanceCluster = "conformance"

// cenv is one fake project with the drivers built over it.
type cenv struct {
	f   *fake.Fake
	x86 *hc.Image
	arm *hc.Image
}

// wrapped is the driver of a conformance run. The suite launches with the image
// reference "image"; Hetzner wants an ID, so the wrapper puts the project's x86
// snapshot in its place.
type wrapped struct {
	*hcloud.Driver
	env *cenv
}

func (w *wrapped) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if req.Image.Ref == "image" {
		req.Image.Ref = fmt.Sprint(w.env.x86.ID)
	}
	return w.Driver.CreateInstance(ctx, req)
}

var (
	driversMu sync.Mutex
	drivers   = map[vendors.Driver]*cenv{}
)

func (e *cenv) driver(t *testing.T, opts ...hcloud.Option) *wrapped {
	t.Helper()
	d := hcloud.New(append([]hcloud.Option{
		hcloud.WithAPI(e.f), hcloud.WithHCloudServerLimit(10000), hcloud.WithRegisterer(nopRegisterer{}),
		hcloud.WithPrices(hcloud.StaticPrices{Volume: map[string]float64{"fsn1": 0.0484, "nbg1": 0.0484}, IPv4: 0.0007}),
	}, opts...)...)
	if err := d.Init(context.Background(), vendors.Config{
		ClusterName: conformanceCluster, Clock: clock.RealClock{}, Log: logr.Discard(),
	}); err != nil {
		t.Fatal(err)
	}
	w := &wrapped{Driver: d, env: e}
	driversMu.Lock()
	drivers[w] = e
	driversMu.Unlock()
	return w
}

func envOf(t *testing.T, d vendors.Driver) *cenv {
	t.Helper()
	driversMu.Lock()
	defer driversMu.Unlock()
	e, ok := drivers[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return e
}

func newCEnv() *cenv {
	f := fake.New()
	x86, arm := f.AddFlatcarImages("stable", "4757.2.3")
	return &cenv{f: f, x86: x86, arm: arm}
}

var injected = map[vendors.Class]struct {
	code   hc.ErrorCode
	fields []string
}{
	vendors.InsufficientCapacity: {code: hc.ErrorCodeResourceUnavailable},
	vendors.Unsupported:          {code: hc.ErrorCodeInvalidInput, fields: []string{"server_type"}},
	vendors.QuotaExceeded:        {code: hc.ErrorCodeResourceLimitExceeded},
	vendors.ZoneUnusable:         {code: hc.ErrorCodeNoSubnetAvailable},
	vendors.Retryable:            {code: hc.ErrorCodeServerError},
	vendors.Fatal:                {code: hc.ErrorCodeUnauthorized},
}

func conformanceTypes() (all, unknown []string) {
	for _, st := range fake.DefaultServerTypes() {
		all = append(all, st.Name)
		unknown = append(unknown, st.Name+"/"+vendors.CapacitySpot)
	}
	return all, unknown
}

var seedNet atomic.Int32

func conformanceFixture(t *testing.T, kind vendors.ResourceKind, params func() json.RawMessage) conformance.Fixture {
	all, unknown := conformanceTypes()
	fx := conformance.Fixture{
		New:    func(t *testing.T) vendors.Driver { return newCEnv().driver(t) },
		Region: "fsn1", Zone: "fsn1", InstanceType: "cx23", MaxUserData: hcloud.MaxUserData,
		InstanceTypes: all, UnknownQuotaTypes: unknown, StoresIdempotencyKey: true,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			in := injected[class]
			envOf(t, d).f.Inject("CreateServer", fake.APIError(in.code, "injected", in.fields...), 1)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			id, err := hcloud.ServerID(ref.ID())
			if err != nil {
				t.Fatal(err)
			}
			envOf(t, d).f.PowerOff(id)
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			e := envOf(t, d)
			e.f.SetLimits(fake.Limits{Servers: e.f.ServerCount()})
		},
		AccountWideQuota: true,
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","size":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"two":   json.RawMessage(`{"data":[{"name":"a","size":"20Gi","mountPath":"/a"},{"name":"b","size":"30Gi","mountPath":"/b"}]}`),
				"tiny":  json.RawMessage(`{"data":[{"name":"t","size":"1Gi","mountPath":"/t"}]}`),
				"empty": json.RawMessage(`{}`),
			},
			vendors.ProfilePlacement: {
				"spread": json.RawMessage(`{"placementGroup":{"type":"spread"}}`),
			},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "hcloud", APIReference: "cx23", Family: "cx", VCPUs: 2},
			{Vendor: "hcloud", APIReference: "cax11", Family: "cax", VCPUs: 2, Arch: "arm64"},
		},
		ResourceKind: kind, ResourceParams: func(*testing.T, vendors.Driver, string) json.RawMessage {
			if params == nil {
				return nil
			}
			return params()
		},
		Seed: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, spec)
		},
		SeedImage: seedImage,
		SeedKinds: []vendors.ResourceKind{hcloud.KindNetworks, hcloud.KindFirewalls, hcloud.KindPlacementGroups},
		SeedAttrs: []string{"created", "cidr"},
		SeedCIDRs: [2]string{"10.201.0.0/24", "10.202.0.0/24"}, SeedWithin: "10.0.0.0/8",
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, conformance.SeedSpec{Name: "user-" + string(k), Tags: tags})
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			return envOf(t, d).driver(t)
		},
	}
	if kind == hcloud.KindVolumes {
		fx.PreCreate = nil // volumes are internal: no use form
	}
	if kind != hcloud.KindFirewalls {
		// The selector and image cases cover every kind once, in the firewalls run.
		fx.Seed, fx.SeedImage = nil, nil
	} else {
		// The resolution order on the firewalls (regional, with labels). Hetzner has no
		// default resource of any kind: no SetDefault, the cases about it are skipped.
		fx.OrderClass = orderClass
	}
	return fx
}

func orderClass(
	t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*common.Resource, ipv6 bool,
) *v1alpha1.SCNodeClass {
	t.Helper()
	b := &hcapi.Settings{Networking: &common.NetworkingOverride{PublicIPv6: &ipv6}}
	if len(res) > 0 {
		b.Resources = &hcapi.Resources{
			Networks: res[hcloud.KindNetworks], Firewalls: res[hcloud.KindFirewalls],
			PlacementGroups: res[hcloud.KindPlacementGroups],
		}
	}
	nc := &v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{HCloud: b}}}
	nc.Name = "conformance"
	return nc
}

func seed(t *testing.T, e *cenv, kind vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	labels := maps.Clone(spec.Tags)
	switch kind {
	case hcloud.KindNetworks:
		cidr := spec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("10.%d.0.0/16", 100+seedNet.Add(1))
		}
		n := e.f.AddNetwork(spec.Name, cidr, labels, hc.NetworkZoneEUCentral)
		if !spec.Created.IsZero() {
			e.f.SetCreated(fake.KindNetwork, n.ID, spec.Created)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: fmt.Sprint(n.ID), Name: spec.Name}
	case hcloud.KindFirewalls:
		x, err := e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: spec.Name, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		if !spec.Created.IsZero() {
			e.f.SetCreated(fake.KindFirewall, x.ID, spec.Created)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: fmt.Sprint(x.ID), Name: spec.Name}
	case hcloud.KindPlacementGroups:
		g, err := e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{
			Name: spec.Name, Labels: labels, Type: hc.PlacementGroupTypeSpread})
		if err != nil {
			t.Fatal(err)
		}
		if !spec.Created.IsZero() {
			e.f.SetCreated(fake.KindPlacementGroup, g.ID, spec.Created)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: fmt.Sprint(g.ID), Name: spec.Name}
	}
	t.Fatalf("seed: no kind %s", kind)
	return vendors.ResourceRef{}
}

// seedImage makes a snapshot the way a user would: with the labels of the
// convention when it says a release.
func seedImage(t *testing.T, d vendors.Driver, _ string, spec conformance.ImageSpec) string {
	t.Helper()
	labels := maps.Clone(spec.Tags)
	if labels == nil {
		labels = map[string]string{}
	}
	if spec.Version != "" {
		// Without os=flatcar: the default lookup (all three labels) finds no image the suite
		// seeded, whatever number of them; an explicit selector needs no os label.
		labels[hcloud.LabelFlatcarChannel], labels[hcloud.LabelFlatcarVersion] = spec.Channel, spec.Version
	}
	arch := hc.ArchitectureX86
	if spec.Arch == "arm64" {
		arch = hc.ArchitectureARM
	}
	e := envOf(t, d)
	im := e.f.AddImage(arch, labels)
	if !spec.Created.IsZero() {
		e.f.SetCreated(fake.KindImage, im.ID, spec.Created)
	}
	return fmt.Sprint(im.ID)
}

// TestConformance runs the shared driver suite once per resource kind the driver
// creates: the managed-resource subtests take one kind.
func TestConformance(t *testing.T) {
	volume := func() json.RawMessage { return json.RawMessage(`{"sizeGB":10}`) }
	for _, tc := range []struct {
		kind   vendors.ResourceKind
		params func() json.RawMessage
	}{
		{kind: hcloud.KindFirewalls},
		{kind: hcloud.KindPlacementGroups, params: func() json.RawMessage { return json.RawMessage(`{"type":"spread"}`) }},
		{kind: hcloud.KindVolumes, params: volume},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.params))
		})
	}
}
