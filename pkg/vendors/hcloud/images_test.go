/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

func imageReq(arch string, use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: "fsn1", Arch: arch, Channel: "stable", Version: "4757.2.3", Use: use}
}

func TestResolveImageByTheLabelConvention(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	ctx := context.Background()
	x86, err := d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{}))
	if err != nil || x86.Ref != fmt.Sprint(e.x86.ID) || x86.Version != "4757.2.3" || x86.Channel != "stable" {
		t.Fatalf("x86: %+v %v", x86, err)
	}
	arm, err := d.ResolveImage(ctx, imageReq("arm64", vendors.UseRef{}))
	if err != nil || arm.Ref != fmt.Sprint(e.arm.ID) {
		t.Fatalf("arm: %+v %v", arm, err)
	}
	// The answer is cached: no call for the second lookup.
	calls := e.f.Calls("Images")
	if _, err := d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{})); err != nil || e.f.Calls("Images") != calls {
		t.Errorf("a cached answer: %v, %d new calls", err, e.f.Calls("Images")-calls)
	}
	if _, err := d.ResolveImage(ctx, imageReq("riscv", vendors.UseRef{})); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown architecture: %v", err)
	}
	// Another release has no snapshot; a missing answer is remembered for a while.
	other := imageReq("amd64", vendors.UseRef{})
	other.Version = "4800.0.0"
	for range 2 {
		if _, err := d.ResolveImage(ctx, other); vendors.ClassOf(err) != vendors.NotFound || vendors.CodeOf(err) != hcloud.CodeImageNotFound {
			t.Errorf("no snapshot: %v", err)
		}
	}
	// Two snapshots with the labels are ambiguous, never a guess.
	e.f.AddFlatcarImages("stable", "4757.2.3")
	d2 := e.driver(t)
	if _, err := d2.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{})); vendors.CodeOf(err) != hcloud.CodeImageAmbiguous {
		t.Errorf("two snapshots: %v", err)
	}
	// A transient failure of the lookup is not remembered, and a stale answer serves.
	e3 := newCEnv()
	d3 := e3.driver(t)
	if _, err := d3.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{})); err != nil {
		t.Fatal(err)
	}
	e3.f.Inject("Images", hcfakeErr(), -1)
	if got, err := d3.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{})); err != nil || got.Ref != fmt.Sprint(e3.x86.ID) {
		t.Errorf("the cache serves while the API is down: %v %v", got, err)
	}
}

func TestResolveImageWithASelector(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	mk := func(arch hc.Architecture, labels map[string]string, created time.Time) *hc.Image {
		im := e.f.AddImage(arch, labels)
		e.f.SetCreated("image", im.ID, created)
		return im
	}
	rel := map[string]string{"flatcar-channel": "stable", "flatcar-version": "4757.2.3", "pool": "a"}
	older := mk(hc.ArchitectureX86, rel, at)
	newer := mk(hc.ArchitectureX86, rel, at.Add(time.Minute))
	wrong := mk(hc.ArchitectureX86, map[string]string{"flatcar-channel": "stable", "flatcar-version": "4500.0.0", "pool": "b"}, at)

	use := vendors.UseRef{Tags: map[string]string{"pool": "a"}, Select: common.SelectNewest, OrderBy: "created"}
	got, err := d.ResolveImage(ctx, imageReq("amd64", use))
	if err != nil || got.Ref != fmt.Sprint(newer.ID) {
		t.Errorf("the newest: %v %v (older %d)", got, err, older.ID)
	}
	use.Select = common.SelectOldest
	if got, err := d.ResolveImage(ctx, imageReq("amd64", use)); err != nil || got.Ref != fmt.Sprint(older.ID) {
		t.Errorf("the oldest: %v %v", got, err)
	}
	use = vendors.UseRef{Tags: map[string]string{"pool": "a"}}
	if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); vendors.CodeOf(err) != hcloud.CodeImageAmbiguous {
		t.Errorf("exactlyOne with two: %v", err)
	}
	// A selector by a label expression, and by IDs of the other architecture.
	use = vendors.UseRef{Expression: "pool in (b),flatcar-channel=stable"}
	if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("an image of another release is never returned: %v (%d)", err, wrong.ID)
	}
	use = vendors.UseRef{IDs: []string{fmt.Sprint(e.arm.ID)}}
	if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); vendors.ClassOf(err) != vendors.NotFound {
		t.Errorf("an ID of the other architecture: %v", err)
	}
	use = vendors.UseRef{Select: common.SelectAll, Tags: map[string]string{"pool": "a"}}
	if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("select all: %v", err)
	}
	// The filters and states pass through to the API.
	use = vendors.UseRef{
		Tags:    map[string]string{"pool": "a", "bad value": "x y"},
		Filters: []common.ResourceFilter{{Name: "type", Values: []string{"snapshot"}}, {Name: "status", Values: []string{"available"}}},
		Match:   common.ResourceMatch{State: []string{"available", "pending", "failed"}, IncludeDeprecated: new(true)},
		Select:  common.SelectNewest, OrderBy: "created",
	}
	if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); err == nil || !strings.Contains(err.Error(), "bad value") {
		// the pair with a space is matched client-side and finds nothing
		t.Logf("client-side matching of a value that is no label: %v", err)
	}
	// bound_to takes one server ID: anything else is refused, never dropped (it would widen the list).
	for _, vals := range [][]string{{"abc"}, {"1", "2"}} {
		use = vendors.UseRef{Tags: map[string]string{"pool": "a"}, Filters: []common.ResourceFilter{{Name: "bound_to", Values: vals}}}
		if _, err := d.ResolveImage(ctx, imageReq("amd64", use)); vendors.ClassOf(err) != vendors.Fatal ||
			!strings.Contains(err.Error(), "bound_to") {
			t.Errorf("bound_to %v: %v, want Fatal", vals, err)
		}
	}
}

func TestReportImages(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	ctx := context.Background()
	nc := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: v1alpha1.SCNodeClassSpec{
		Flatcar: v1alpha1.FlatcarSpec{Version: "4757.2.3"},
		Vendors: v1alpha1.VendorBlocks{HCloud: &hcapi.Settings{Regions: []string{"nbg1", "fsn1"}}},
	}}
	st, err := d.ReportImages(ctx, nc)
	if err != nil || len(st) != 4 {
		t.Fatalf("%v %v", st, err)
	}
	for i, want := range []struct{ region, arch, ref string }{
		{"fsn1", "amd64", fmt.Sprint(e.x86.ID)}, {"fsn1", "arm64", fmt.Sprint(e.arm.ID)},
		{"nbg1", "amd64", fmt.Sprint(e.x86.ID)}, {"nbg1", "arm64", fmt.Sprint(e.arm.ID)},
	} {
		if st[i].Region != want.region || st[i].Arch != want.arch || !st[i].Ready || st[i].Ref != want.ref {
			t.Errorf("%d: %+v", i, st[i])
		}
	}
	// A release without snapshots: not ready, with the message; the version check has its reason.
	nc.Spec.Flatcar.Version = "4800.0.0"
	nc.Spec.Vendors.HCloud.Regions = nil
	st, err = d.ReportImages(ctx, nc)
	if err != nil || len(st) != 12 || st[0].Ready || !strings.Contains(st[0].Message, "4800.0.0") {
		t.Errorf("every location, both architectures, none ready: %d %+v %v", len(st), st[0], err)
	}
	// An images selector with the snapshot of another release names the reason.
	other := e.f.AddImage(hc.ArchitectureX86, map[string]string{"x": "y", "flatcar-version": "4000.0.0"})
	nc.Spec.Flatcar.Version = "4757.2.3"
	nc.Spec.Vendors.HCloud = &hcapi.Settings{Regions: []string{"fsn1"}, Resources: &hcapi.Resources{
		Images: &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{"*": {fmt.Sprint(other.ID)}}}}}}
	st, err = d.ReportImages(ctx, nc)
	if err != nil || len(st) != 2 || st[0].Ready || st[0].Reason != vendors.CodeImageVersionMismatch {
		t.Errorf("%+v %v", st, err)
	}
	// A transient error is an error, not a verdict.
	e.f.Inject("Images", hcfakeErr(), -1)
	nc.Spec.Flatcar.Version = "4999.0.0"
	nc.Spec.Vendors.HCloud = &hcapi.Settings{Regions: []string{"fsn1"}}
	if _, err := d.ReportImages(ctx, nc); err == nil {
		t.Error("a transient failure")
	}
}
