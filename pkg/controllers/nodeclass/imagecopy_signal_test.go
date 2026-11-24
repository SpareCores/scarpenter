//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/imagecopy"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

const (
	copyChannel = "stable"
	copyVersion = "4757.2.3"
	srcRegion   = "r1"
	dstRegion   = "r2"
)

// copyDriver is the stub driver with the image report of a vendor that has
// copies on: the official image where the fake account has one, else what the
// copy manager says (a report never asks for demand).
type copyDriver struct {
	*driver
	account *fake.Driver
	copies  vendors.ImageCopies
}

func (d *copyDriver) ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	var out []v1alpha1.ImageStatus
	for _, region := range []string{srcRegion, dstRegion} {
		for _, arch := range []string{"amd64", "arm64"} {
			req := vendors.ImageRequest{
				Region: region, Arch: arch, Channel: copyChannel, Version: nc.Spec.Flatcar.Version,
			}
			st := v1alpha1.ImageStatus{Region: region, Arch: arch}
			rel := vendors.Release{Channel: req.Channel, Version: req.Version, Arch: arch}
			ref, found, err := d.account.Primary(ctx, region, rel)
			if err != nil {
				return nil, err
			}
			var img *vendors.Image
			if found {
				img = &vendors.Image{Ref: ref}
			} else if img, err = d.copies.Lookup(ctx, req, false); err != nil {
				st.Reason, st.Message = vendors.CodeOf(err), err.Error()
			}
			if img != nil {
				st.Ref, st.Ready = img.Ref, true
			}
			out = append(out, st)
		}
	}
	return out, nil
}

type copyEnv struct {
	*env
	clk     *clocktesting.FakeClock
	account *fake.Driver
	control fake.ImageCopyControl
	m       *imagecopy.Manager
	gate    *gateRecorder
}

func newCopyEnv(t *testing.T, version string) *copyEnv {
	t.Helper()
	e := newEnv(t)
	clk := clocktesting.NewFakeClock(time.Now())
	account := fake.New(fake.DefaultOptions())
	if err := account.Init(t.Context(), vendors.Config{ClusterName: "my-cluster", Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	ce := &copyEnv{env: e, clk: clk, account: account, control: account.ImageCopies(), gate: &gateRecorder{}}
	if err := ce.control.PublishOfficial(t.Context(), srcRegion, copyChannel, version); err != nil {
		t.Fatal(err)
	}
	ce.m = imagecopy.New(imagecopy.Options{
		Vendor: "aws", Cluster: "my-cluster", Driver: account, Clock: clk,
		Regions: func(context.Context) ([]string, error) { return []string{srcRegion, dstRegion}, nil },
	})
	cd := &copyDriver{driver: e.drv, account: account, copies: ce.m}
	ce.ctrl = e.controllerWith(func(o *nodeclass.Options) {
		o.Registry = vendors.NewRegistryFrom(cd)
		o.ImageGate = ce.gate
		o.ImageCopies = []*imagecopy.Manager{ce.m}
		// Only the signal can bring the next reconcile in time.
		o.Interval = time.Hour
	})
	return ce
}

// finish completes the copies in flight and lets the manager see them.
func (ce *copyEnv) finish() {
	ce.t.Helper()
	if err := ce.control.Finish(ce.t.Context()); err != nil {
		ce.t.Fatal(err)
	}
	ce.clk.Step(imagecopy.PollInterval)
	if err := ce.m.Tick(ce.t.Context()); err != nil {
		ce.t.Fatal(err)
	}
}

func (ce *copyEnv) eventually(what string, check func(nc *v1alpha1.SCNodeClass) error) {
	ce.t.Helper()
	var last error
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		nc := &v1alpha1.SCNodeClass{}
		if err := ce.c.Get(ce.t.Context(), client.ObjectKey{Name: ce.name}, nc); err != nil {
			last = err
			continue
		}
		if last = check(nc); last == nil {
			return
		}
	}
	ce.t.Fatalf("%s: timed out; last: %v", what, last)
}

// imagesReadyIs checks the ImagesReady reason and the gap the gate got for the
// destination region.
func (ce *copyEnv) imagesReadyIs(reason string, gap catalog.Code) func(*v1alpha1.SCNodeClass) error {
	return func(nc *v1alpha1.SCNodeClass) error {
		c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
			return fmt.Errorf("ImagesReady = %+v, want True %s", c, reason)
		}
		if got := ce.gate.gapsOf(ce.name)[dstRegion]; got != gap {
			return fmt.Errorf("gap of %s = %q, want %q", dstRegion, got, gap)
		}
		return nil
	}
}

// The status walks ImageCopyOnDemand, ImageCopying, ready for both
// architectures, and the gate follows each signal of the manager within one
// reconcile, although the NodeClass would not be reconciled for an hour.
func TestTheManagerSignalRequeuesTheNodeClasses(t *testing.T) {
	ce := newCopyEnv(t, copyVersion)
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme: ce.c.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ce.ctrl = ce.controllerWith(func(o *nodeclass.Options) {
		o.Client = mgr.GetClient()
		o.Registry = vendors.NewRegistryFrom(&copyDriver{driver: ce.drv, account: ce.account, copies: ce.m})
		o.ImageGate = ce.gate
		o.ImageCopies = []*imagecopy.Manager{ce.m}
		o.Interval = time.Hour
	})
	if err := ce.ctrl.Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	ce.create(ce.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = nil }))
	ce.eventually("on demand", ce.imagesReadyIs(nodeclass.ReasonImagesPartial, ""))

	// A launch wants r2: the copy is queued, the region is gated.
	for _, arch := range []string{"amd64", "arm64"} {
		req := vendors.ImageRequest{Region: dstRegion, Arch: arch, Channel: copyChannel, Version: copyVersion}
		if _, err := ce.m.Lookup(t.Context(), req, true); vendors.CodeOf(err) != vendors.CodeImageCopying {
			t.Fatalf("Lookup(%s) = %v, want ImageCopying", arch, err)
		}
	}
	ce.eventually("copying", func(nc *v1alpha1.SCNodeClass) error {
		if err := ce.imagesReadyIs(nodeclass.ReasonImagesCopying, catalog.ImageCopyPending)(nc); err != nil {
			return err
		}
		for _, img := range nc.Status.Vendors["aws"].Images {
			if img.Region == dstRegion && img.Reason != vendors.CodeImageCopying {
				return fmt.Errorf("image %+v is not copying", img)
			}
		}
		return nil
	})

	// The copies finish: the gate lifts and both architectures are ready.
	if err := ce.m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ce.finish()
	ce.eventually("ready", func(nc *v1alpha1.SCNodeClass) error {
		if err := ce.imagesReadyIs(nodeclass.ReasonReady, "")(nc); err != nil {
			return err
		}
		n := 0
		for _, img := range nc.Status.Vendors["aws"].Images {
			if img.Region == dstRegion && img.Ready {
				n++
			}
		}
		if n != 2 {
			return fmt.Errorf("%d ready images in %s, want both architectures", n, dstRegion)
		}
		return nil
	})
}

// Pinning a release this process has not seen reaches the manager at once: where
// a node runs from a copy of the old release, the new release is queued without
// waiting for a signal or the garbage collector (plan 29, section 6).
func TestPinningANewReleaseBumpsItIntoTheCopyRegions(t *testing.T) {
	const next = "4757.3.0"
	ce := newCopyEnv(t, copyVersion)
	// The new release is published in r1, the source of its copy.
	if err := ce.control.PublishOfficial(t.Context(), srcRegion, copyChannel, next); err != nil {
		t.Fatal(err)
	}
	ce.create(ce.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = nil }))
	ce.reconcile(ce.name)

	// A node runs from the copy of the pinned release in r2.
	refs := make([]string, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		req := vendors.ImageRequest{Region: dstRegion, Arch: arch, Channel: copyChannel, Version: copyVersion}
		if _, err := ce.m.Lookup(t.Context(), req, true); err == nil {
			t.Fatal("the copy cannot be there yet")
		}
	}
	if err := ce.m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ce.finish()
	for _, arch := range []string{"amd64", "arm64"} {
		req := vendors.ImageRequest{Region: dstRegion, Arch: arch, Channel: copyChannel, Version: copyVersion}
		img, err := ce.m.Lookup(t.Context(), req, true)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, img.Ref)
	}
	ce.control.SetInUse(refs[0], true)

	var nc v1alpha1.SCNodeClass
	if err := ce.c.Get(t.Context(), client.ObjectKey{Name: ce.name}, &nc); err != nil {
		t.Fatal(err)
	}
	nc.Spec.Flatcar.Version = next
	if err := ce.c.Update(t.Context(), &nc); err != nil {
		t.Fatal(err)
	}
	ce.reconcile(ce.name)

	for _, arch := range []string{"amd64", "arm64"} {
		req := vendors.ImageRequest{Region: dstRegion, Arch: arch, Channel: copyChannel, Version: next}
		if _, err := ce.m.Lookup(t.Context(), req, false); vendors.CodeOf(err) != vendors.CodeImageCopying {
			t.Errorf("the new release %s in %s = %v, want its copy queued by the bump", arch, dstRegion, err)
		}
	}
}

// A class that pins a release no source region has is not ready for the region
// that would need a copy: the report says ImageCopySourceMissing.
func TestAnUnpublishedReleaseIsSourceMissing(t *testing.T) {
	ce := newCopyEnv(t, copyVersion)
	ce.create(ce.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Flatcar.Version = "4999.0.0"
		nc.Spec.Vendors.AWS.Regions = []string{dstRegion}
	}))
	c := vcond(ce.reconcile(ce.name), "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != vendors.CodeImageCopySourceMissing {
		t.Errorf("ImagesReady = %+v, want False with ImageCopySourceMissing", c)
	}
	if gap := ce.gate.gapsOf(ce.name)[dstRegion]; gap != catalog.ImageGap {
		t.Errorf("gap = %q, want ImageGap", gap)
	}
}
