//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package imagecopy_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcloudapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/imagecopy"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const idleTTL = 720 * time.Hour

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func TestDecide(t *testing.T) {
	avail := vendors.ImageCopy{State: vendors.CopyAvailable, Created: t0}
	tests := []struct {
		name   string
		mutate func(*imagecopy.Facts)
		want   string // the reason; empty keeps the copy
	}{
		{"fresh and pinned", func(f *imagecopy.Facts) {}, ""},
		{"idle for the TTL (a)", func(f *imagecopy.Facts) { f.Now = t0.Add(idleTTL) }, imagecopy.ReasonIdle},
		{"idle for less than the TTL", func(f *imagecopy.Facts) { f.Now = t0.Add(idleTTL - time.Second) }, ""},
		{"used at the vendor", func(f *imagecopy.Facts) {
			f.Now, f.Copy.LastUsed = t0.Add(idleTTL), t0.Add(24*time.Hour)
		}, ""},
		{"resolved by a launch", func(f *imagecopy.Facts) {
			f.Now, f.LastResolve = t0.Add(idleTTL), t0.Add(time.Hour)
		}, ""},
		{"idle again after the last use", func(f *imagecopy.Facts) {
			f.Now, f.Copy.LastUsed = t0.Add(idleTTL+time.Hour), t0.Add(time.Hour)
		}, imagecopy.ReasonIdle},
		{"primary appeared, never resolved (b)", func(f *imagecopy.Facts) { f.PrimaryAvailable = true }, imagecopy.ReasonPrimaryAvailable},
		{"primary appeared, resolved 2h ago", func(f *imagecopy.Facts) {
			f.PrimaryAvailable, f.Now, f.LastResolve = true, t0.Add(3*time.Hour), t0.Add(time.Hour)
		}, imagecopy.ReasonPrimaryAvailable},
		{"primary appeared, resolved 1h59m ago", func(f *imagecopy.Facts) {
			f.PrimaryAvailable, f.Now, f.LastResolve = true, t0.Add(3*time.Hour), t0.Add(61*time.Minute)
		}, ""},
		{"unpinned for 2h (c)", func(f *imagecopy.Facts) {
			f.Pinned, f.UnpinnedSince, f.Now = false, t0, t0.Add(2*time.Hour)
		}, imagecopy.ReasonUnpinned},
		{"unpinned for 1h59m", func(f *imagecopy.Facts) {
			f.Pinned, f.UnpinnedSince, f.Now = false, t0, t0.Add(2*time.Hour-time.Second)
		}, ""},
		{"duplicate (d)", func(f *imagecopy.Facts) { f.Duplicate = true }, imagecopy.ReasonDuplicate},
		{"failed (e)", func(f *imagecopy.Facts) { f.Copy.State = vendors.CopyFailed }, imagecopy.ReasonFailed},
		{"pending", func(f *imagecopy.Facts) {
			f.Copy.State, f.Now = vendors.CopyPending, t0.Add(imagecopy.CopyTimeout-time.Second)
		}, ""},
		{"pending too long (e)", func(f *imagecopy.Facts) {
			f.Copy.State, f.Now = vendors.CopyPending, t0.Add(imagecopy.CopyTimeout)
		}, imagecopy.ReasonFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := imagecopy.Facts{Copy: avail, Now: t0, IdleTTL: idleTTL, Pinned: true}
			tc.mutate(&f)
			d := imagecopy.Decide(f)
			if d.Delete != (tc.want != "") || d.Reason != tc.want || (!d.Delete && d.Kept == "") {
				t.Errorf("Decide = %+v; want reason %q", d, tc.want)
			}
		})
	}
}

type recorder struct{ events []events.Event }

func (r *recorder) Publish(e ...events.Event) { r.events = append(r.events, e...) }

type gcEnv struct {
	*env
	pins []imagecopy.Pin
	rec  *recorder
	gc   *imagecopy.Collector
}

func newGCEnv(t *testing.T) *gcEnv {
	t.Helper()
	g := &gcEnv{env: newEnv(t), rec: &recorder{}}
	g.pins = []imagecopy.Pin{{Channel: channel, Version: version}}
	g.gc = imagecopy.NewCollector(imagecopy.CollectorOptions{
		Managers: []*imagecopy.Manager{g.m},
		Pinned:   func(context.Context, string) ([]imagecopy.Pin, error) { return g.pins, nil },
		IdleTTL:  idleTTL, Clock: g.clk, Recorder: g.rec,
		DeploymentRef: func() *corev1.ObjectReference { return &corev1.ObjectReference{Kind: "Deployment", Name: "scarpenter"} },
	})
	return g
}

func (g *gcEnv) sweep() {
	g.t.Helper()
	if err := g.gc.Sweep(g.t.Context()); err != nil {
		g.t.Fatalf("Sweep: %v", err)
	}
}

func (g *gcEnv) wantCopies(n int, why string) {
	g.t.Helper()
	if got := len(g.copies()); got != n {
		g.t.Errorf("%d copies; want %d (%s)", got, n, why)
	}
}

func TestSweepDeletesIdleCopies(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.wantCopies(2, "both architectures")
	g.clk.Step(idleTTL - time.Hour)
	g.sweep()
	g.wantCopies(2, "idle for less than the TTL")
	// A launch resolves the copies: the idle time restarts.
	g.lookup(g.m, req(dstRegion, "amd64"), true)
	g.lookup(g.m, req(dstRegion, "arm64"), true)
	g.clk.Step(idleTTL - time.Minute)
	g.sweep()
	g.wantCopies(2, "resolved less than the TTL ago")
	g.clk.Step(time.Minute)
	g.sweep()
	g.wantCopies(0, "idle for the TTL")
}

func TestSweepIdleDeletesEachImage(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.clk.Step(time.Hour)
	g.lookup(g.m, req(dstRegion, "amd64"), true) // only amd64 is used
	g.clk.Step(idleTTL - time.Hour)
	g.sweep()
	got := g.copies()
	if len(got) != 1 || got[0].Release.Arch != "amd64" {
		t.Fatalf("copies = %+v; want only the used amd64 copy left", got)
	}
	if len(g.invalid) != 1 {
		t.Errorf("invalidated = %v; a deletion must drop the cached ID", g.invalid)
	}
	if len(g.rec.events) != 1 || g.rec.events[0].Reason != imagecopy.EventDeleted ||
		g.rec.events[0].InvolvedObject.(*corev1.ObjectReference).Kind != "Deployment" {
		t.Errorf("events = %+v; want one ImageCopyDeleted on the Deployment", g.rec.events)
	}
	g.want(g.m, dstRegion, "arm64", false, vendors.CodeImageCopyOnDemand)
}

func TestSweepNeverDeletesACopyInUse(t *testing.T) {
	g := newGCEnv(t)
	ref := g.ready(dstRegion)
	g.ctl.SetInUse(ref, true)
	g.pins = nil // unpinned too
	g.sweep()
	g.clk.Step(idleTTL + 3*time.Hour)
	// Every rule applies: idle, unpinned, and a primary image appeared.
	if err := g.ctl.PublishOfficial(t.Context(), dstRegion, channel, version); err != nil {
		t.Fatal(err)
	}
	g.sweep()
	got := g.copies()
	if len(got) != 1 || got[0].Ref == "" || got[0].Release.Arch == "arm64" {
		t.Fatalf("copies = %+v; want the in-use amd64 copy kept and the unused arm64 one deleted", got)
	}
	if got[0].Ref != ref {
		t.Errorf("kept %s; want the in-use %s", got[0].Ref, ref)
	}
}

func TestSweepUnpinnedNeedsTwoContinuousHours(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.pins = nil
	g.sweep() // the release is first seen unpinned
	g.clk.Step(time.Hour + 59*time.Minute)
	g.sweep()
	g.wantCopies(2, "unpinned for 1h59m")
	// Pinned again: the clock restarts.
	g.pins = []imagecopy.Pin{{Channel: channel, Version: version}}
	g.sweep()
	g.pins = nil
	g.clk.Step(time.Minute)
	g.sweep()
	g.clk.Step(time.Hour + 59*time.Minute)
	g.sweep()
	g.wantCopies(2, "unpinned for 1h59m since it was pinned again")
	g.clk.Step(time.Minute)
	g.sweep()
	g.wantCopies(0, "unpinned for 2h")
}

func TestSweepAfterARestartWaitsTheUnpinnedGraceAgain(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.pins = nil
	g.clk.Step(24 * time.Hour)
	g.sweep() // the process saw the release unpinned for the first time just now
	g.wantCopies(2, "the clock of rule (c) starts with the process")
	g.clk.Step(2 * time.Hour)
	g.sweep()
	g.wantCopies(0, "2h after the first sighting")
}

func TestSweepDeletesWhenAPrimaryImageAppears(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.lookup(g.m, req(dstRegion, "amd64"), true)
	g.lookup(g.m, req(dstRegion, "arm64"), true)
	if err := g.ctl.PublishOfficial(t.Context(), dstRegion, channel, version); err != nil {
		t.Fatal(err)
	}
	g.clk.Step(time.Hour)
	g.sweep()
	g.wantCopies(2, "a launch resolved the copies an hour ago: the launch cache may hold them")
	g.clk.Step(time.Hour)
	g.sweep()
	g.wantCopies(0, "the primary image exists and no launch resolved the copies for 2h")
}

func TestSweepDeletesDuplicatesAndFailedImages(t *testing.T) {
	g := newGCEnv(t)
	g.ctl.SetDuration(time.Minute)
	start := func(token string) string {
		t.Helper()
		ref, err := g.drv.StartCopy(t.Context(), vendors.ImageSource{Region: srcRegion, Ref: "ami-official-us-east-1-stable-4757.2.3-amd64"},
			dstRegion, vendors.Release{Channel: channel, Version: version, Arch: "amd64"}, map[vendors.TagKey]string{
				vendors.TagCluster: "my-cluster", vendors.TagManagedBy: vendors.ManagedByValue,
				vendors.TagResource: vendors.ResourceKindImageCopy,
			}, token)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	oldest := start("a")
	g.clk.Step(time.Second)
	start("b")
	g.ctl.FailCopies(1)
	g.clk.Step(time.Second)
	start("c") // fails
	g.clk.Step(time.Minute)
	g.sweep()
	got := g.copies()
	if len(got) != 1 || got[0].Ref != oldest {
		t.Fatalf("copies = %+v; want only the oldest available copy %s", got, oldest)
	}
	if img, code := g.lookup(g.m, req(dstRegion, "amd64"), false); img == nil || img.Ref != oldest {
		t.Errorf("Lookup = %+v, %q; the oldest copy is the one served", img, code)
	}
}

func TestSweepUnpinnedClockIsClearedForPinnedReleases(t *testing.T) {
	g := newGCEnv(t)
	g.ready(dstRegion)
	g.pins = nil
	g.sweep()
	g.clk.Step(time.Hour)
	// Pinned in a sweep that sees no copy of the release (the region's list
	// is empty for it): the stale unpinned time must not survive.
	g.pins = []imagecopy.Pin{{Channel: channel, Version: version}}
	g.sweep()
	g.clk.Step(90 * time.Minute)
	g.pins = nil
	g.sweep()
	g.wantCopies(2, "unpinned again for less than 2h; the earlier unpinned time is not inherited")
}

func TestDeletingTheServedCopyServesTheDuplicate(t *testing.T) {
	g := newGCEnv(t)
	g.ctl.SetDuration(time.Minute)
	oldRef := g.ready(dstRegion)
	// A second, newer copy of the key (a race).
	g.clk.Step(time.Second)
	newRef, err := g.drv.StartCopy(t.Context(), vendors.ImageSource{Region: srcRegion, Ref: "ami-official-us-east-1-stable-4757.2.3-amd64"},
		dstRegion, vendors.Release{Channel: channel, Version: version, Arch: "amd64"}, map[vendors.TagKey]string{
			vendors.TagCluster: "my-cluster", vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagResource: vendors.ResourceKindImageCopy,
		}, "race")
	if err != nil {
		t.Fatal(err)
	}
	g.clk.Step(2 * time.Minute)
	var old vendors.ImageCopy
	for _, c := range g.copies() {
		if c.Ref == oldRef {
			old = c
		}
	}
	if err := g.m.Delete(t.Context(), old, imagecopy.ReasonIdle); err != nil {
		t.Fatal(err)
	}
	if img, code := g.lookup(g.m, req(dstRegion, "amd64"), false); img == nil || img.Ref != newRef {
		t.Errorf("Lookup = %+v, %q; want the remaining copy %s", img, code, newRef)
	}
}

func TestSweepForgetsACopyDeletedByHand(t *testing.T) {
	g := newGCEnv(t)
	ref := g.ready(dstRegion)
	if err := g.ctl.DeleteByHand(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	g.sweep()
	g.want(g.m, dstRegion, "amd64", false, vendors.CodeImageCopyOnDemand)
}

func TestSweepBumpsTheNewRelease(t *testing.T) {
	g := newGCEnv(t)
	if err := g.ctl.PublishOfficial(t.Context(), srcRegion, channel, "4800.0.0"); err != nil {
		t.Fatal(err)
	}
	ref := g.ready(dstRegion)
	g.ctl.SetInUse(ref, true)
	g.pins = []imagecopy.Pin{{Channel: channel, Version: "4800.0.0"}}
	g.sweep()
	next := vendors.ImageRequest{Region: dstRegion, Arch: "amd64", Channel: channel, Version: "4800.0.0"}
	if _, code := g.lookup(g.m, next, false); code != vendors.CodeImageCopying {
		t.Errorf("code = %q; want the pinned release queued for the region of the running node", code)
	}
	// The old release is kept while the node runs from it, though unpinned and
	// long after the grace.
	g.clk.Step(3 * time.Hour)
	g.sweep()
	for _, c := range g.copies() {
		if c.Ref == ref {
			return
		}
	}
	t.Errorf("the copy the node runs from was deleted: %+v", g.copies())
}

func TestPinnedReleases(t *testing.T) {
	scheme := fake.NewClientBuilder().Build().Scheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := func(name, ch, ver string, vendorBlocks v1alpha1.VendorBlocks) *v1alpha1.SCNodeClass {
		return &v1alpha1.SCNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.SCNodeClassSpec{
				Flatcar: v1alpha1.FlatcarSpec{Channel: v1alpha1.FlatcarChannel(ch), Version: ver}, Vendors: vendorBlocks,
			},
		}
	}
	aws := func(regions ...string) v1alpha1.VendorBlocks {
		return v1alpha1.VendorBlocks{AWS: &awsapi.Settings{Regions: regions}}
	}
	images := v1alpha1.VendorBlocks{AWS: &awsapi.Settings{Resources: &awsapi.Resources{
		Images: &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{"*": {"ami-1"}}}},
	}}}
	owner := v1alpha1.VendorBlocks{AWS: &awsapi.Settings{ImageOwner: "123456789012"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		class("a", "", "4757.2.3", aws("us-east-1")), class("b", "stable", "4757.2.3", aws("eu-west-1", "us-east-1")),
		class("c", "lts", "4081.3.12", aws()),
		class("hcloud-only", "stable", "4800.0.0", v1alpha1.VendorBlocks{HCloud: &hcloudapi.Settings{}}),
		class("names-images", "beta", "4900.0.0", images), class("owner", "beta", "4901.0.0", owner),
	).Build()
	got, err := imagecopy.PinnedReleases(t.Context(), c, "aws")
	if err != nil {
		t.Fatal(err)
	}
	want := []imagecopy.Pin{
		{Channel: "stable", Version: "4757.2.3", Regions: []string{"us-east-1", "eu-west-1"}},
		{Channel: "lts", Version: "4081.3.12"},
	}
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Errorf("PinnedReleases = %+v; want %+v: one pin per release with the regions of its classes (none for a class without a list), and no class without an aws block, naming its images or an owner", got, want)
	}
}

// A pin with regions is copied only into those regions.
func TestBumpHonoursTheRegionsOfAPin(t *testing.T) {
	e := newEnv(t)
	if err := e.ctl.PublishOfficial(t.Context(), srcRegion, channel, "4800.0.0"); err != nil {
		t.Fatal(err)
	}
	ref := e.ready(dstRegion)
	e.ctl.SetInUse(ref, true)
	next := vendors.ImageRequest{Region: dstRegion, Arch: "amd64", Channel: channel, Version: "4800.0.0"}
	if err := e.m.Bump(t.Context(), []imagecopy.Pin{{Channel: channel, Version: "4800.0.0", Regions: []string{"eu-south-2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, code := e.lookup(e.m, next, false); code != vendors.CodeImageCopyOnDemand {
		t.Errorf("code = %q; a class that does not use %s must not make a copy there", code, dstRegion)
	}
	if err := e.m.Bump(t.Context(), []imagecopy.Pin{{Channel: channel, Version: "4800.0.0", Regions: []string{dstRegion}}}); err != nil {
		t.Fatal(err)
	}
	if _, code := e.lookup(e.m, next, false); code != vendors.CodeImageCopying {
		t.Errorf("code = %q; want the release queued in a region its classes use", code)
	}
}
