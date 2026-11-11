/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:prealloc,lll,dupl,gocyclo,goconst // table-heavy tests: small expectations, repeated fixtures, long SDK signatures
package azure_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	azapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/azure"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// engineWorld is the driver behind the real resource engine, which decides where
// each resource comes from: ADR-022's order for a vendor without a default
// resource (the shared suite's order cases assume one): an explicit use, the
// opt-in tag, creation, and a failure that says what was tried.
type engineWorld struct {
	*world
	engine *resources.Engine
	kube   *fake.ClientBuilder
	clock  *clocktesting.FakeClock
}

func newEngineWorld(t *testing.T, objs ...*v1alpha1.SCNodeClass) *engineWorld {
	t.Helper()
	w := conformanceWorld(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	eng := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(w.d), Reader: b.Build(), Clock: clk, ClusterName: conformanceCluster,
		Regions: func(string) []string { return []string{confRegion} },
		Zones: func(_, region string) []string {
			zs, _ := w.d.Zones(context.Background(), region)
			var names []string
			for _, z := range zs {
				names = append(names, z.Name)
			}
			return names
		},
	})
	return &engineWorld{world: w, engine: eng, kube: b, clock: clk}
}

func classOf(res *azapi.Resources) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Azure: &azapi.Settings{Resources: res}}},
	}
}

func (w *engineWorld) require(nc *v1alpha1.SCNodeClass, kind vendors.ResourceKind) ([]vendors.ResourceRef, error) {
	return w.engine.Provider(nc, azure.Vendor, confRegion, confZone).Require(context.Background(), vendors.ResourceRequest{Kind: kind})
}

func ids(refs []vendors.ResourceRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

func TestResolutionOrderWithoutDefaults(t *testing.T) {
	ctx := context.Background()
	optIn := map[string]*string{"scarpenter-use": new("true")}
	mk := func(t *testing.T, w *engineWorld, name string, tags map[string]*string) {
		t.Helper()
		if _, err := w.sub.Groups().CreateOrUpdate(ctx, name, armresources.ResourceGroup{Location: new(confRegion), Tags: tags}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(t *testing.T, w *engineWorld) int {
		t.Helper()
		l, err := w.d.List(ctx, []string{confRegion}, map[vendors.TagKey]string{vendors.TagCluster: conformanceCluster})
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}

	t.Run("an explicit use beats the tagged one", func(t *testing.T) {
		w := newEngineWorld(t)
		mk(t, w, "tagged", optIn)
		mk(t, w, "named", map[string]*string{"x": new("y")})
		nc := classOf(&azapi.Resources{ResourceGroups: &common.Resource{Use: &common.ResourceUse{Name: "named"}}})
		refs, err := w.require(nc, azure.KindResourceGroups)
		if err != nil || len(refs) != 1 || refs[0].ID != "named" || refs[0].Managed {
			t.Fatalf("%v, %v", refs, err)
		}
		rep := w.engine.Check(ctx, nc, azure.Vendor, []string{confRegion})
		if len(rep.Resources) == 0 || rep.Resources[0].Source != v1alpha1.ResourceSourceUse {
			t.Errorf("the check says %+v", rep.Resources)
		}
	})

	t.Run("the opt-in tag beats creation, and nothing is created", func(t *testing.T) {
		w := newEngineWorld(t)
		mk(t, w, "untagged", map[string]*string{"x": new("y")})
		mk(t, w, "tagged", optIn)
		nc := classOf(nil)
		refs, err := w.require(nc, azure.KindResourceGroups)
		if err != nil || len(refs) != 1 || refs[0].ID != "tagged" {
			t.Fatalf("%v, %v", refs, err)
		}
		if n := count(t, w); n != 0 {
			t.Errorf("%d managed resources although a tagged group served", n)
		}
		rep := w.engine.Check(ctx, nc, azure.Vendor, []string{confRegion})
		var src v1alpha1.ResourceSource
		for _, r := range rep.Resources {
			if r.Kind == string(azure.KindResourceGroups) && r.ID == "tagged" {
				src = r.Source
			}
		}
		if src != v1alpha1.ResourceSourceDiscovered {
			t.Errorf("the check says %q for the tagged group", src)
		}
	})

	t.Run("nothing found creates, and there is no default to find", func(t *testing.T) {
		w := newEngineWorld(t)
		nc := classOf(nil)
		refs, err := w.require(nc, azure.KindResourceGroups)
		if err != nil || len(refs) != 1 || !refs[0].Managed {
			t.Fatalf("%v, %v", refs, err)
		}
		if refs[0].ID != "scarpenter-conformance-resource-groups-westeurope" {
			t.Errorf("the name of the managed group is %s", refs[0].ID)
		}
		if n := count(t, w); n != 1 {
			t.Errorf("%d managed resources, want the created one", n)
		}
		// The second launch finds it again.
		again, err := w.require(nc, azure.KindResourceGroups)
		if err != nil || ids(again)[0] != refs[0].ID || count(t, w) != 1 {
			t.Errorf("%v, %v", again, err)
		}
	})

	t.Run("an explicit use that matches nothing fails and does not fall through", func(t *testing.T) {
		w := newEngineWorld(t)
		mk(t, w, "tagged", optIn)
		nc := classOf(&azapi.Resources{ResourceGroups: &common.Resource{Use: &common.ResourceUse{Name: "absent"}}})
		_, err := w.require(nc, azure.KindResourceGroups)
		if vendors.CodeOf(err) != vendors.CodeResourceMissing {
			t.Fatalf("%v, want ResourceMissing", err)
		}
		if n := count(t, w); n != 0 {
			t.Errorf("%d managed resources: the order fell through to creation", n)
		}
		if bad, _ := w.engine.Check(ctx, nc, azure.Vendor, []string{confRegion}).NotReady(); len(bad) != 1 {
			t.Errorf("the check: not ready %v", bad)
		}
	})

	t.Run("an explicit create skips the tagged resource", func(t *testing.T) {
		w := newEngineWorld(t)
		mk(t, w, "tagged", optIn)
		nc := classOf(&azapi.Resources{ResourceGroups: &common.Resource{Create: &common.ResourceCreate{}}})
		refs, err := w.require(nc, azure.KindResourceGroups)
		if err != nil || len(refs) != 1 || !refs[0].Managed || refs[0].ID == "tagged" {
			t.Fatalf("%v, %v", refs, err)
		}
	})

	t.Run("forbid ends at the failure with the kind, region and steps", func(t *testing.T) {
		w := newEngineWorld(t)
		nc := classOf(&azapi.Resources{ResourceGroups: &common.Resource{Forbid: true}})
		_, err := w.require(nc, azure.KindResourceGroups)
		if vendors.CodeOf(err) != vendors.CodeResourceForbidden {
			t.Fatalf("%v, want ResourceForbidden", err)
		}
		for _, want := range []string{"resourceGroups", confRegion, "nothing is tagged", "forbidden"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the failure %q does not name %q", err, want)
			}
		}
		if n := count(t, w); n != 0 {
			t.Errorf("%d managed resources after a forbidden creation", n)
		}
	})
}

// TestLaunchThroughTheEngine launches with the resources of a NodeClass that
// brings its network by tags, and with the managed ones, through the engine's
// Prepare; then the garbage collector takes the managed network apart in the
// driver's order once nothing uses it.
func TestLaunchThroughTheEngine(t *testing.T) {
	ctx := context.Background()
	nc := classOf(nil)
	nc.Spec.Networking = common.Networking{Isolation: common.IsolationNodeClass}
	w := newEngineWorld(t, nc)
	no := vendors.NodeOptions(w.d)
	in, has, err := options.InputFromNodeClass(nc, azure.Vendor)
	if err != nil || !has {
		t.Fatal(err, has)
	}
	settings, err := options.Resolve(no, in, options.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	prepare := func() *vendors.Prepared {
		t.Helper()
		prep, err := no.Prepare(ctx, vendors.PrepareRequest{
			Region: confRegion, Zone: confZone, InstanceType: confType, CapacityType: vendors.CapacityOnDemand, Settings: settings,
			Tags:      map[vendors.TagKey]string{vendors.TagNodeClass: nc.Name},
			Resources: w.engine.Provider(nc, azure.Vendor, confRegion, confZone),
		})
		if err != nil {
			t.Fatal(err)
		}
		return prep
	}
	prep := prepare()
	kinds := map[vendors.ResourceKind]vendors.ResourceRef{}
	for _, r := range prep.Resources {
		kinds[r.Kind] = r
	}
	for _, k := range []vendors.ResourceKind{azure.KindResourceGroups, azure.KindSubnets, azure.KindSecurityGroups} {
		if !kinds[k].Managed {
			t.Errorf("%s is not managed: %+v", k, kinds[k])
		}
	}
	if _, ok := kinds[azure.KindAppSecGroups]; !ok {
		t.Error("isolation nodeclass: the node does not join the application security group")
	}

	req := vendors.CreateRequest{
		Region: confRegion, Zone: confZone, InstanceType: confType, CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image:    vendors.Image{Ref: azure.ImageRef{Publisher: azure.DefaultPublisher, Offer: azure.OfferAMD64, SKU: "stable-gen2", Version: confVersion}.URN()},
		UserData: []byte(`{}`), Prepared: prep, Settings: settings,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: "claim", vendors.TagNodePool: "pool",
			vendors.TagNodeClass: nc.Name, vendors.TagManagedBy: vendors.ManagedByValue,
		},
		Node: vendors.NodeInfo{Name: "claim"},
	}
	inst, err := w.d.Driver.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.sub.VMs().Get(ctx, kinds[azure.KindResourceGroups].ID, azure.VMName(conformanceCluster, "claim"), false); err != nil {
		t.Fatalf("the VM is not in the managed resource group: %v", err)
	}
	if inst.State != vendors.StateRunning {
		t.Errorf("the node is %s", inst.State)
	}

	// The garbage collector: nothing is deleted while the node uses the network.
	if err := w.engine.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	w.clock.Step(48 * time.Hour)
	if err := w.engine.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := w.d.List(ctx, []string{confRegion}, nil); len(l) != 3 {
		t.Fatalf("%d managed resources while a node runs, want the group, the network and the security group", len(l))
	}
	if err := w.d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	// Idle for a day (the kind's TTL) the network and the security group go, in
	// order; the resource group is retained for the NodeClass.
	for range 3 {
		w.clock.Step(25 * time.Hour)
		if err := w.engine.Sweep(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	l, err := w.d.List(ctx, []string{confRegion}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 1 || l[0].Kind != azure.KindResourceGroups {
		t.Fatalf("after the TTL: %+v, want only the retained resource group", l)
	}
	if asgs := w.sub.ASGs().Names(); len(asgs) != 0 {
		t.Errorf("the application security group stayed: %v", asgs)
	}
}
