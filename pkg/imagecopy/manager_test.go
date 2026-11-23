//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package imagecopy_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/imagecopy"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

const (
	channel = "stable"
	version = "4757.2.3"
	// defaultOwner is the official AWS owner: spelling it out is still an owner.
	defaultOwner = "075585003325"
	srcRegion    = "us-east-1"
	dstRegion    = "ap-south-2"
)

// countingDriver counts the uses recorded at the vendor.
type countingDriver struct {
	*fake.Driver
	touches int
	tokens  []string
}

func (c *countingDriver) StartCopy(
	ctx context.Context, src vendors.ImageSource, region string, rel vendors.Release,
	tags map[vendors.TagKey]string, token string,
) (string, error) {
	c.tokens = append(c.tokens, token)
	return c.Driver.StartCopy(ctx, src, region, rel, tags, token)
}

func (c *countingDriver) TouchCopy(ctx context.Context, cp vendors.ImageCopy, at time.Time) error {
	c.touches++
	return c.Driver.TouchCopy(ctx, cp, at)
}

type env struct {
	t       *testing.T
	clk     *clocktesting.FakeClock
	drv     *countingDriver
	ctl     fake.ImageCopyControl
	m       *imagecopy.Manager
	invalid []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC))
	d := fake.New(fake.DefaultOptions())
	if err := d.Init(t.Context(), vendors.Config{ClusterName: "my-cluster", Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clk: clk, drv: &countingDriver{Driver: d}, ctl: d.ImageCopies()}
	e.m = e.manager()
	if err := e.ctl.PublishOfficial(t.Context(), srcRegion, channel, version); err != nil {
		t.Fatal(err)
	}
	return e
}

// manager returns a manager over the account (a second one is a restart).
func (e *env) manager() *imagecopy.Manager {
	return imagecopy.New(imagecopy.Options{
		Vendor: "aws", Cluster: "my-cluster", Driver: e.drv, Clock: e.clk,
		Regions: func(context.Context) ([]string, error) {
			return []string{srcRegion, dstRegion, "eu-south-2", "ca-west-1"}, nil
		},
		Invalidate: func(region, ref string) { e.invalid = append(e.invalid, region+"/"+ref) },
	})
}

func req(region, arch string) vendors.ImageRequest {
	return vendors.ImageRequest{Region: region, Arch: arch, Channel: channel, Version: version}
}

// lookup returns the image or the code of the error.
func (e *env) lookup(m *imagecopy.Manager, r vendors.ImageRequest, demand bool) (*vendors.Image, string) {
	e.t.Helper()
	img, err := m.Lookup(e.t.Context(), r, demand)
	if err != nil {
		if !vendors.IsClass(err, vendors.NotFound) {
			e.t.Fatalf("Lookup(%s %s): %v; an image that is not ready is NotFound", r.Region, r.Arch, err)
		}
		return nil, vendors.CodeOf(err)
	}
	return img, ""
}

func (e *env) want(m *imagecopy.Manager, region, arch string, demand bool, code string) {
	e.t.Helper()
	if img, got := e.lookup(m, req(region, arch), demand); got != code {
		e.t.Errorf("Lookup(%s %s, demand=%v) = %v, %q; want code %q", region, arch, demand, img, got, code)
	}
}

func (e *env) tick(m *imagecopy.Manager) {
	e.t.Helper()
	if err := m.Tick(e.t.Context()); err != nil {
		e.t.Fatalf("Tick: %v", err)
	}
}

func (e *env) finish() {
	e.t.Helper()
	if err := e.ctl.Finish(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) copies() []vendors.ImageCopy {
	e.t.Helper()
	got, err := e.drv.ListCopies(e.t.Context(), []string{srcRegion, dstRegion, "eu-south-2", "ca-west-1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// ready makes a copy of the release available in the region and returns the
// amd64 image's ref.
func (e *env) ready(region string) string {
	e.t.Helper()
	e.want(e.m, region, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	e.finish()
	e.clk.Step(imagecopy.PollInterval)
	e.tick(e.m)
	img, code := e.lookup(e.m, req(region, "amd64"), false)
	if img == nil {
		e.t.Fatalf("the copy is not ready: %q", code)
	}
	return img.Ref
}

func TestReasonsOfTheStatus(t *testing.T) {
	e := newEnv(t)

	// A report never starts a copy: the region is on demand.
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopyOnDemand)
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 0 {
		t.Fatalf("a report started %d copies", n)
	}

	// A launch enqueues both architectures as one unit.
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.want(e.m, dstRegion, "arm64", false, vendors.CodeImageCopying)
	select {
	case <-e.m.Signal():
	default:
		t.Error("the demand did not signal the NodeClass controller")
	}
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 2 {
		t.Fatalf("Starts = %d; want both architectures", n)
	}
	got := e.copies()
	if len(got) != 2 || !strings.HasPrefix(got[0].Source, srcRegion+"/") || !strings.HasPrefix(got[1].Source, srcRegion+"/") {
		t.Errorf("copies = %+v; want two copies from the source region", got)
	}
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopying)

	// Done: both architectures are served from the copy.
	e.finish()
	e.clk.Step(imagecopy.PollInterval)
	e.tick(e.m)
	for _, arch := range []string{"amd64", "arm64"} {
		img, code := e.lookup(e.m, req(dstRegion, arch), false)
		if img == nil || img.Version != version || img.Channel != channel || img.Ref == "" {
			t.Errorf("Lookup(%s) = %+v, %q; want the copy", arch, img, code)
		}
	}
	select {
	case <-e.m.Signal():
	default:
		t.Error("the finished copy did not signal")
	}
}

// metric returns the value of the series of the family whose labels are all
// among want, or -1.
func metric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			match := len(m.GetLabel()) == len(want)
			for _, l := range m.GetLabel() {
				match = match && want[l.GetName()] == l.GetValue()
			}
			if match {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	e := newEnv(t)
	e.m = imagecopy.New(imagecopy.Options{
		Vendor: "aws", Cluster: "my-cluster", Driver: e.drv, Clock: e.clk, Metrics: imagecopy.NewMetrics(reg),
		Regions: func(context.Context) ([]string, error) { return []string{dstRegion}, nil },
	})
	e.ctl.RefuseStarts(1)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	e.finish()
	e.clk.Step(2 * time.Minute)
	e.tick(e.m)
	vr := map[string]string{"vendor": "aws", "region": dstRegion}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"scarpenter_image_copies_started_total", vr, 1},
		{"scarpenter_image_copies_completed_total", vr, 1},
		{"scarpenter_image_copies_failed_total", map[string]string{"vendor": "aws", "region": dstRegion, "code": "OptInRequired"}, 1},
		{"scarpenter_image_copies", map[string]string{"vendor": "aws", "state": "available"}, 1},
		{"scarpenter_image_copies", map[string]string{"vendor": "aws", "state": "failed"}, 1},
		{"scarpenter_image_copies", map[string]string{"vendor": "aws", "state": "queued"}, 0},
	} {
		if got := metric(t, reg, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s%v = %v; want %v", tc.name, tc.labels, got, tc.want)
		}
	}
	cp := e.copies()[0]
	if err := e.m.Delete(t.Context(), cp, imagecopy.ReasonIdle); err != nil {
		t.Fatal(err)
	}
	if got := metric(t, reg, "scarpenter_image_copies_deleted_total", map[string]string{"vendor": "aws", "reason": "Idle"}); got != 1 {
		t.Errorf("deleted = %v; want 1", got)
	}
}

func TestFailedCopyIsRetriedAfterTheFailureTTL(t *testing.T) {
	e := newEnv(t)
	e.ctl.FailCopies(1)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	e.finish()
	e.clk.Step(imagecopy.PollInterval)
	e.tick(e.m)

	// One architecture failed and was deleted, the other is ready.
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopyFailed)
	if img, code := e.lookup(e.m, req(dstRegion, "arm64"), false); img == nil {
		t.Errorf("the other architecture is not ready: %q", code)
	}
	if len(e.copies()) != 1 || len(e.invalid) != 1 {
		t.Errorf("copies = %+v, invalidated = %v; the failed image must be deleted and its caches dropped",
			e.copies(), e.invalid)
	}
	// A demand does not retry before the TTL.
	e.clk.Step(imagecopy.FailureTTL - time.Minute)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopyFailed)
	e.clk.Step(time.Minute)
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopyOnDemand)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	if n := len(e.copies()); n != 2 {
		t.Errorf("copies = %d; want the retried copy next to the ready one", n)
	}
}

func TestUnlistedCopyFailsAfterTheGrace(t *testing.T) {
	e := newEnv(t)
	e.ctl.SetDuration(time.Hour)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	var ref string
	for _, c := range e.copies() {
		if c.Release.Arch == "amd64" {
			ref = c.Ref
		}
	}
	if err := e.ctl.DeleteByHand(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(imagecopy.MissingGrace - time.Second)
	e.tick(e.m)
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopying)
	e.clk.Step(time.Second)
	e.tick(e.m)
	_, err := e.m.Lookup(t.Context(), req(dstRegion, "amd64"), false)
	if vendors.CodeOf(err) != vendors.CodeImageCopyFailed || !strings.Contains(err.Error(), "CopyMissing") {
		t.Errorf("Lookup = %v; want ImageCopyFailed with CopyMissing", err)
	}
	e.want(e.m, dstRegion, "arm64", false, vendors.CodeImageCopying)
}

func TestTokens(t *testing.T) {
	e := newEnv(t)
	// The retries of one start repeat the token.
	e.ctl.LimitStarts(1)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	e.clk.Step(imagecopy.BackoffMin)
	e.tick(e.m)
	if len(e.drv.tokens) < 2 || e.drv.tokens[0] != e.drv.tokens[1] {
		t.Fatalf("tokens = %v; want the retry of a limited start to repeat the token", e.drv.tokens)
	}
	// A new demand after a failure, on a restarted leader (no memory of the
	// failure), never repeats the failed copy's token.
	e2 := newEnv(t)
	e2.ctl.RefuseStarts(1)
	e2.want(e2.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e2.tick(e2.m)
	failed := e2.drv.tokens[0]
	e2.clk.Step(time.Second)
	m2 := e2.manager()
	e2.want(m2, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e2.tick(m2)
	if got := e2.drv.tokens[len(e2.drv.tokens)-1]; got == failed {
		t.Errorf("the retry after a restart repeated the failed copy's token %s", failed)
	}
}

func TestRefusedCopyFails(t *testing.T) {
	e := newEnv(t)
	e.ctl.RefuseStarts(2)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	for _, arch := range []string{"amd64", "arm64"} {
		_, err := e.m.Lookup(t.Context(), req(dstRegion, arch), false)
		if vendors.CodeOf(err) != vendors.CodeImageCopyFailed {
			t.Errorf("%s: %v; want ImageCopyFailed", arch, err)
		}
	}
	if _, err := e.m.Lookup(t.Context(), req(dstRegion, "amd64"), false); err == nil || !strings.Contains(err.Error(), "OptInRequired") {
		t.Errorf("the failure does not carry the vendor's code: %v", err)
	}
}

func TestNoSourceIsReportedAndRetried(t *testing.T) {
	e := newEnv(t)
	// Nobody has an image of this release.
	r := vendors.ImageRequest{Region: dstRegion, Arch: "amd64", Channel: channel, Version: "9.9.9"}
	if _, code := e.lookup(e.m, r, true); code != vendors.CodeImageCopying {
		t.Fatalf("code = %q", code)
	}
	e.tick(e.m)
	if _, code := e.lookup(e.m, r, false); code != vendors.CodeImageCopySourceMissing {
		t.Errorf("code = %q; want ImageCopySourceMissing", code)
	}
	if n := e.ctl.Starts(); n != 0 {
		t.Errorf("Starts = %d; no copy can start without a source", n)
	}
	e.clk.Step(imagecopy.SourceMissingRetry)
	if _, code := e.lookup(e.m, r, false); code != vendors.CodeImageCopyOnDemand {
		t.Errorf("code = %q; want on demand again", code)
	}
}

func TestSourceIsNeverTheDestination(t *testing.T) {
	e := newEnv(t)
	if err := e.ctl.PublishOfficial(t.Context(), "eu-south-2", channel, version); err != nil {
		t.Fatal(err)
	}
	// The sources in order are eu-south-2 and us-east-1; the copy into
	// eu-south-2 must come from us-east-1.
	e.want(e.m, "eu-south-2", "amd64", true, vendors.CodeImageCopying)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	for _, c := range e.copies() {
		want := "us-east-1/"
		if c.Region == dstRegion {
			want = "eu-south-2/" // the first source in the vendor's order
		}
		if !strings.HasPrefix(c.Source, want) {
			t.Errorf("the copy in %s is from %s; want a source in %s", c.Region, c.Source, want)
		}
	}
}

func TestOneCopyPerKeyUnderConcurrentDemand(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			arch := []string{"amd64", "arm64"}[i%2]
			if _, err := e.m.Lookup(t.Context(), req(dstRegion, arch), true); vendors.CodeOf(err) != vendors.CodeImageCopying {
				t.Errorf("Lookup: %v", err)
			}
		})
	}
	wg.Wait()
	e.tick(e.m)
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 2 {
		t.Errorf("Starts = %d; want one copy per architecture", n)
	}
}

func TestCapOfFourAndQueueOrder(t *testing.T) {
	e := newEnv(t)
	regions := []string{"eu-south-2", dstRegion, "ca-west-1"}
	for _, r := range regions {
		e.want(e.m, r, "amd64", true, vendors.CodeImageCopying)
		e.clk.Step(time.Second)
	}
	e.tick(e.m)
	if n := e.ctl.Starts(); n != imagecopy.MaxInFlight {
		t.Fatalf("Starts = %d; want %d in flight", n, imagecopy.MaxInFlight)
	}
	inflight := map[string]int{}
	for _, c := range e.copies() {
		inflight[c.Region]++
	}
	if inflight["eu-south-2"] != 2 || inflight[dstRegion] != 2 || inflight["ca-west-1"] != 0 {
		t.Errorf("in flight per region = %v; want the first two regions demanded", inflight)
	}
	e.want(e.m, "ca-west-1", "amd64", false, vendors.CodeImageCopying)
	// A slot frees: the queued region starts in the same tick that sees the
	// copies finish.
	e.finish()
	e.clk.Step(imagecopy.PollInterval)
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 6 {
		t.Errorf("Starts = %d; want the queued region started after the slots freed", n)
	}
}

func TestBackoffOnLimitError(t *testing.T) {
	e := newEnv(t)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.ctl.LimitStarts(6)
	waits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		imagecopy.BackoffMax, imagecopy.BackoffMax}
	starts := 0
	for _, w := range waits {
		e.tick(e.m)
		starts++
		if n := e.ctl.Starts(); n != starts {
			t.Fatalf("Starts = %d; want %d", n, starts)
		}
		e.clk.Step(w - time.Second)
		e.tick(e.m)
		if n := e.ctl.Starts(); n != starts {
			t.Fatalf("Starts = %d during the %s backoff; want %d", n, w, starts)
		}
		e.clk.Step(time.Second)
	}
	// The limit is over: the copy starts and stays queued behind nothing.
	e.tick(e.m)
	if n := len(e.copies()); n != 2 {
		t.Errorf("copies = %d; want both architectures started once the limit lifted", n)
	}
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopying)
}

func TestAdoptsPendingCopiesAfterARestart(t *testing.T) {
	e := newEnv(t)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	m2 := e.manager()
	// The new leader finds the copies in flight and does not start others.
	e.want(m2, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.want(m2, dstRegion, "arm64", false, vendors.CodeImageCopying)
	e.tick(m2)
	if n := e.ctl.Starts(); n != 2 {
		t.Errorf("Starts = %d; want the pending copies adopted, not repeated", n)
	}
	e.finish()
	e.clk.Step(imagecopy.PollInterval)
	e.tick(m2)
	if img, code := e.lookup(m2, req(dstRegion, "amd64"), false); img == nil {
		t.Errorf("the adopted copy is not served: %q", code)
	}
}

func TestAdoptsAvailableCopyAfterARestart(t *testing.T) {
	e := newEnv(t)
	ref := e.ready(dstRegion)
	m2 := e.manager()
	// A report on the new leader sees the copy, without starting anything.
	img, code := e.lookup(m2, req(dstRegion, "amd64"), false)
	if img == nil || img.Ref != ref {
		t.Errorf("Lookup after the restart = %+v, %q; want %s", img, code, ref)
	}
}

func TestCopyTimesOut(t *testing.T) {
	e := newEnv(t)
	e.ctl.SetDuration(10 * time.Hour)
	e.want(e.m, dstRegion, "amd64", true, vendors.CodeImageCopying)
	e.tick(e.m)
	e.clk.Step(imagecopy.CopyTimeout - time.Minute)
	e.tick(e.m)
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopying)
	e.clk.Step(time.Minute)
	e.tick(e.m)
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopyFailed)
	if len(e.copies()) != 0 || len(e.invalid) != 2 {
		t.Errorf("copies = %+v, invalidated = %v; the timed out images must be deleted", e.copies(), e.invalid)
	}
}

func TestNamedImagesAndOwnersNeverCopy(t *testing.T) {
	e := newEnv(t)
	for name, mutate := range map[string]func(*vendors.ImageRequest){
		"use": func(r *vendors.ImageRequest) { r.Use = vendors.UseRef{IDs: []string{"ami-1"}} },
		"use by tags": func(r *vendors.ImageRequest) {
			r.Use = vendors.UseRef{Tags: map[string]string{"os": "flatcar"}}
		},
		"imageOwner":         func(r *vendors.ImageRequest) { r.Owner = "123456789012" },
		"the default owner":  func(r *vendors.ImageRequest) { r.Owner = defaultOwner },
		"owner and selector": func(r *vendors.ImageRequest) { r.Owner = "self"; r.Use = vendors.UseRef{Name: "x*"} },
	} {
		for _, demand := range []bool{true, false} {
			r := req(dstRegion, "amd64")
			mutate(&r)
			img, err := e.m.Lookup(t.Context(), r, demand)
			if img != nil || err != nil {
				t.Errorf("%s (demand=%v): Lookup = %v, %v; want no say", name, demand, img, err)
			}
		}
	}
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 0 || len(e.copies()) != 0 {
		t.Errorf("Starts = %d, copies = %v; a named image or owner must never copy", n, e.copies())
	}
	// The manager did not even enqueue the region: a report of a class
	// without them still sees it on demand.
	e.want(e.m, dstRegion, "amd64", false, vendors.CodeImageCopyOnDemand)
}

func TestReleaseBump(t *testing.T) {
	e := newEnv(t)
	if err := e.ctl.PublishOfficial(t.Context(), srcRegion, channel, "4800.0.0"); err != nil {
		t.Fatal(err)
	}
	ref := e.ready(dstRegion)
	pins := []imagecopy.Pin{{Channel: channel, Version: version}, {Channel: channel, Version: "4800.0.0"}}
	next := vendors.ImageRequest{Region: dstRegion, Arch: "amd64", Channel: channel, Version: "4800.0.0"}

	// No instance runs from the old copy: nothing is copied ahead.
	if err := e.m.Bump(t.Context(), pins); err != nil {
		t.Fatal(err)
	}
	if _, code := e.lookup(e.m, next, false); code != vendors.CodeImageCopyOnDemand {
		t.Errorf("code = %q; want on demand while nothing runs from the old copy", code)
	}

	// A node runs from it: the new release is copied at once, both architectures.
	e.ctl.SetInUse(ref, true)
	if err := e.m.Bump(t.Context(), pins); err != nil {
		t.Fatal(err)
	}
	if _, code := e.lookup(e.m, next, false); code != vendors.CodeImageCopying {
		t.Errorf("code = %q; want the new release queued", code)
	}
	next.Arch = "arm64"
	if _, code := e.lookup(e.m, next, false); code != vendors.CodeImageCopying {
		t.Errorf("arm64: code = %q; want the unit queued", code)
	}
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 4 {
		t.Errorf("Starts = %d; want the two of the bump after the first two", n)
	}
}

func TestBumpSkipsARegionWithAPrimaryImage(t *testing.T) {
	e := newEnv(t)
	ref := e.ready(dstRegion)
	e.ctl.SetInUse(ref, true)
	if err := e.ctl.PublishOfficial(t.Context(), dstRegion, channel, "4800.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Bump(t.Context(), []imagecopy.Pin{{Channel: channel, Version: "4800.0.0"}}); err != nil {
		t.Fatal(err)
	}
	e.tick(e.m)
	if n := e.ctl.Starts(); n != 2 {
		t.Errorf("Starts = %d; a region that has the new release's primary image needs no copy", n)
	}
}

func TestDeleteInvalidatesAndForgets(t *testing.T) {
	e := newEnv(t)
	ref := e.ready(dstRegion)
	var cp vendors.ImageCopy
	for _, c := range e.copies() {
		if c.Ref == ref {
			cp = c
		}
	}
	if err := e.m.Delete(t.Context(), cp, imagecopy.ReasonIdle); err != nil {
		t.Fatal(err)
	}
	if len(e.invalid) != 1 || e.invalid[0] != dstRegion+"/"+ref {
		t.Errorf("invalidated = %v; want the deleted image", e.invalid)
	}
	// A lookup after the delete never serves the old ID.
	if img, code := e.lookup(e.m, req(dstRegion, "amd64"), false); img != nil || code != vendors.CodeImageCopyOnDemand {
		t.Errorf("Lookup after the delete = %+v, %q; want on demand", img, code)
	}
}

func TestUseIsRecordedAtMostDaily(t *testing.T) {
	e := newEnv(t)
	e.ready(dstRegion)
	e.want(e.m, dstRegion, "amd64", false, "")
	e.tick(e.m)
	if e.drv.touches != 0 {
		t.Errorf("a report counted as a use")
	}
	_, _ = e.lookup(e.m, req(dstRegion, "amd64"), true)
	e.tick(e.m)
	e.tick(e.m)
	if e.drv.touches != 1 {
		t.Fatalf("touches = %d; want the first use recorded once", e.drv.touches)
	}
	e.clk.Step(time.Hour)
	_, _ = e.lookup(e.m, req(dstRegion, "amd64"), true)
	e.tick(e.m)
	if e.drv.touches != 1 {
		t.Errorf("touches = %d; a second use within a day is not recorded", e.drv.touches)
	}
	e.clk.Step(imagecopy.TouchInterval)
	_, _ = e.lookup(e.m, req(dstRegion, "amd64"), true)
	e.tick(e.m)
	if e.drv.touches != 2 {
		t.Errorf("touches = %d; want a use recorded after a day", e.drv.touches)
	}
}
