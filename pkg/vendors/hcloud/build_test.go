/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apricote/hcloud-upload-image/hcloudimages/v2"
	"github.com/go-logr/logr"
	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

// fakeUploader plays hcloud-upload-image against the fake API: it adds the
// temporary server with the build's labels, waits for the test to release it,
// and then either fails (leaving the server, as a timed-out cleanup does) or
// deletes the server and adds the snapshot.
type fakeUploader struct {
	f       *fake.Fake
	release chan error

	mu    sync.Mutex
	calls []hcloudimages.UploadOptions
}

func newFakeUploader(f *fake.Fake) *fakeUploader {
	return &fakeUploader{f: f, release: make(chan error)}
}

func (u *fakeUploader) Upload(ctx context.Context, o hcloudimages.UploadOptions) (*hc.Image, error) {
	u.mu.Lock()
	u.calls = append(u.calls, o)
	u.mu.Unlock()
	typ := "cx23"
	if o.Architecture == hc.ArchitectureARM {
		typ = "cax11"
	}
	s := u.f.AddForeignServer("fsn1", typ, o.Labels)
	k := u.f.AddSSHKey(fmt.Sprintf("build-%d", s.ID), o.Labels)
	if err := <-u.release; err != nil {
		return nil, err
	}
	if err := u.f.DeleteSSHKey(ctx, k); err != nil {
		return nil, err
	}
	a, err := u.f.DeleteServer(ctx, s)
	if err != nil {
		return nil, err
	}
	if err := u.f.WaitForAction(ctx, a); err != nil {
		return nil, err
	}
	return u.f.AddImage(o.Architecture, o.Labels), nil
}

func (u *fakeUploader) uploads() []hcloudimages.UploadOptions {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]hcloudimages.UploadOptions(nil), u.calls...)
}

// buildEnv is a project without snapshots, a fake clock and a fake uploader.
type buildEnv struct {
	f   *fake.Fake
	clk *clocktesting.FakeClock
	up  *fakeUploader
	d   *hcloud.Driver
	nc  *v1alpha1.SCNodeClass
}

func newBuildEnv(t *testing.T, opts ...hcloud.Option) *buildEnv {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	f := fake.New(fake.WithClock(clk))
	up := newFakeUploader(f)
	d := hcloud.New(append([]hcloud.Option{
		hcloud.WithAPI(f), hcloud.WithUploader(up), hcloud.WithRegisterer(nopRegisterer{}),
		hcloud.WithPrices(hcloud.StaticPrices{}),
	}, opts...)...)
	if err := d.Init(context.Background(), vendors.Config{ClusterName: "c1", Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	nc := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "nc"}, Spec: v1alpha1.SCNodeClassSpec{
		Flatcar: v1alpha1.FlatcarSpec{Version: "4800.0.0"},
		Vendors: v1alpha1.VendorBlocks{HCloud: &hcapi.Settings{Regions: []string{"fsn1", "nbg1"}}},
	}}
	return &buildEnv{f: f, clk: clk, up: up, d: d, nc: nc}
}

// report returns the image status per architecture, of the first region (a
// snapshot is global; the regions can differ only for a report that races the
// end of a build).
func (e *buildEnv) report(t *testing.T) map[string]v1alpha1.ImageStatus {
	t.Helper()
	st, err := e.d.ReportImages(context.Background(), e.nc)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]v1alpha1.ImageStatus{}
	for _, s := range st {
		if _, ok := out[s.Arch]; !ok {
			out[s.Arch] = s
		}
	}
	return out
}

// waitUploads waits until the uploader has been called n times (a build
// calls it from its own goroutine).
func (e *buildEnv) waitUploads(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.up.uploads()) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d uploads, want %d", len(e.up.uploads()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitFor reports until cond holds (a build finishes in its own goroutine).
func (e *buildEnv) waitFor(t *testing.T, what string, cond func(map[string]v1alpha1.ImageStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.report(t)
		if cond(st) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *buildEnv) buildServers(t *testing.T) []*hc.Server {
	t.Helper()
	e.f.Tick()
	s, err := e.f.Servers(context.Background(), hc.ServerListOpts{ListOpts: hc.ListOpts{LabelSelector: hcloud.LabelImageBuild}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildMissingSnapshots(t *testing.T) {
	e := newBuildEnv(t)
	st := e.report(t)
	e.waitUploads(t, 1)
	if st["amd64"].Ready || st["amd64"].Reason != vendors.CodeImageBuilding || !strings.Contains(st["amd64"].Message, "started") {
		t.Errorf("amd64 is being built: %+v", st["amd64"])
	}
	if st["arm64"].Reason != vendors.CodeImageBuilding || !strings.Contains(st["arm64"].Message, "waiting for the build of stable 4800.0.0 amd64") {
		t.Errorf("arm64 waits for amd64: %+v", st["arm64"])
	}
	// Reports while the build runs start nothing more.
	e.report(t)
	ups := e.up.uploads()
	if len(ups) != 1 {
		t.Fatalf("one build at a time: %d", len(ups))
	}
	o := ups[0]
	if o.Architecture != hc.ArchitectureX86 || o.ImageCompression != hcloudimages.CompressionBZ2 ||
		o.ImageURL.String() != "https://stable.release.flatcar-linux.net/amd64-usr/4800.0.0/flatcar_production_hetzner_image.bin.bz2" ||
		*o.Description != "Flatcar stable 4800.0.0 amd64" {
		t.Errorf("upload options: %+v %s", o, o.ImageURL)
	}
	want := map[string]string{"os": "flatcar", "flatcar-channel": "stable", "flatcar-version": "4800.0.0", hcloud.LabelImageBuild: "c1"}
	for k, v := range want {
		if o.Labels[k] != v {
			t.Errorf("label %s=%q, want %q", k, o.Labels[k], v)
		}
	}

	e.up.release <- nil
	e.waitFor(t, "amd64 ready, arm64 building", func(st map[string]v1alpha1.ImageStatus) bool {
		return st["amd64"].Ready && st["arm64"].Reason == vendors.CodeImageBuilding && len(e.up.uploads()) == 2
	})
	if a := e.up.uploads()[1].Architecture; a != hc.ArchitectureARM {
		t.Errorf("the second build: %s", a)
	}
	e.up.release <- nil
	e.waitFor(t, "both ready", func(st map[string]v1alpha1.ImageStatus) bool {
		return st["amd64"].Ready && st["arm64"].Ready
	})
	if s := e.buildServers(t); len(s) != 0 {
		t.Errorf("no build server is left: %v", s)
	}
}

func TestBuildFailureIsReportedSweptAndRetried(t *testing.T) {
	e := newBuildEnv(t)
	e.nc.Spec.Vendors.HCloud.Regions = []string{"hel1"}
	e.report(t)
	e.waitUploads(t, 1)
	e.up.release <- errors.New("failed to ssh into temporary server")
	e.waitFor(t, "the build failed", func(st map[string]v1alpha1.ImageStatus) bool {
		return st["amd64"].Reason == vendors.CodeImageBuildFailed
	})
	if st := e.report(t)["amd64"]; st.Ready || !strings.Contains(st.Message, "failed to ssh into temporary server") ||
		!strings.Contains(st.Message, "2026-11-21T12:15:00Z") {
		t.Errorf("the failure and the retry time: %+v", st)
	}
	// The failed build's server and key are gone; arm64 has started meanwhile.
	e.up.release <- nil
	e.waitFor(t, "arm64 ready", func(st map[string]v1alpha1.ImageStatus) bool { return st["arm64"].Ready })
	if s := e.buildServers(t); len(s) != 0 {
		t.Errorf("the failed build's server is swept: %v", s)
	}
	keys, _ := e.f.SSHKeys(context.Background(), hc.SSHKeyListOpts{ListOpts: hc.ListOpts{LabelSelector: hcloud.LabelImageBuild}})
	if len(keys) != 0 {
		t.Errorf("the failed build's key is swept: %v", keys)
	}
	// No retry before 15 minutes, then a new build.
	e.clk.Step(14 * time.Minute)
	if st := e.report(t)["amd64"]; st.Reason != vendors.CodeImageBuildFailed || len(e.up.uploads()) != 2 {
		t.Errorf("no retry yet: %+v, %d uploads", st, len(e.up.uploads()))
	}
	e.clk.Step(2 * time.Minute)
	if st := e.report(t)["amd64"]; st.Reason != vendors.CodeImageBuilding {
		t.Errorf("retried: %+v", st)
	}
	e.waitUploads(t, 3)
	e.up.release <- nil
	e.waitFor(t, "amd64 ready", func(st map[string]v1alpha1.ImageStatus) bool { return st["amd64"].Ready })
}

func TestBuildSweepsLeftoversAndRespectsOtherBuilds(t *testing.T) {
	e := newBuildEnv(t)
	release := map[string]string{"os": "flatcar", "flatcar-channel": "stable", "flatcar-version": "4800.0.0"}
	mine := map[string]string{hcloud.LabelImageBuild: "c1"}
	theirs := map[string]string{hcloud.LabelImageBuild: "c2"}
	maps.Copy(theirs, release)
	e.f.AddForeignServer("fsn1", "cx23", mine)
	e.f.AddSSHKey("mine", mine)
	other := e.f.AddForeignServer("fsn1", "cx23", theirs) // another cluster builds amd64
	e.f.AddSSHKey("theirs", theirs)
	// arm64's snapshot exists but is still being created.
	arm := e.f.AddImage(hc.ArchitectureARM, release)
	e.f.SetImageStatus(arm.ID, hc.ImageStatusCreating)

	st := e.report(t)
	if !strings.Contains(st["amd64"].Message, fmt.Sprintf("the server %d of the cluster \"c2\" is building it", other.ID)) {
		t.Errorf("amd64 waits for the other cluster: %+v", st["amd64"])
	}
	if !strings.Contains(st["arm64"].Message, fmt.Sprintf("the snapshot %d is creating", arm.ID)) {
		t.Errorf("arm64 waits for its snapshot: %+v", st["arm64"])
	}
	if n := len(e.up.uploads()); n != 0 {
		t.Errorf("nothing is built: %d", n)
	}
	s := e.buildServers(t)
	if len(s) != 1 || s[0].ID != other.ID {
		t.Errorf("only this cluster's leftover server is deleted: %v", s)
	}
	keys, _ := e.f.SSHKeys(context.Background(), hc.SSHKeyListOpts{ListOpts: hc.ListOpts{LabelSelector: hcloud.LabelImageBuild}})
	if len(keys) != 1 || keys[0].Name != "theirs" {
		t.Errorf("only this cluster's leftover key is deleted: %v", keys)
	}
}

func TestNoBuildWhenOffOrSelected(t *testing.T) {
	e := newBuildEnv(t, hcloud.WithBuildImages(false))
	if st := e.report(t)["amd64"]; st.Ready || st.Reason != "" || !strings.Contains(st.Message, "no available Flatcar snapshot") {
		t.Errorf("building is off: %+v", st)
	}
	// A selector names the operator's images: never built.
	e = newBuildEnv(t)
	e.nc.Spec.Vendors.HCloud.Resources = &hcapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
		Tags: map[string]string{"team": "x"}}}}
	if st := e.report(t)["amd64"]; st.Ready || st.Reason == vendors.CodeImageBuilding {
		t.Errorf("a selector: %+v", st)
	}
	if n := len(e.up.uploads()); n != 0 {
		t.Errorf("nothing is built: %d", n)
	}
}

func TestFlatcarImageURL(t *testing.T) {
	got := hcloud.FlatcarImageURL("beta", "4801.1.0", "arm64").String()
	if got != "https://beta.release.flatcar-linux.net/arm64-usr/4801.1.0/flatcar_production_hetzner_image.bin.bz2" {
		t.Error(got)
	}
}

// TestBuildRunsOnTheTypeWithTheSmallestDisk: the snapshot takes the disk size of
// its build server, so the type comes from the live list, smallest disk first.
func TestBuildRunsOnTheTypeWithTheSmallestDisk(t *testing.T) {
	types := fake.DefaultServerTypes()
	for _, st := range types {
		if st.Name == "cpx22" {
			st.Disk = 20
		}
	}
	e := newBuildEnv(t, hcloud.WithAPI(fake.New(fake.WithServerTypes(types))))
	e.report(t)
	e.waitUploads(t, 1)
	if st := e.up.uploads()[0].ServerType; st == nil || st.Name != "cpx22" {
		t.Errorf("build type: %+v, want cpx22 (20 GB, the smallest)", st)
	}
}
