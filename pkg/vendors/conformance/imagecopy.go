/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"slices"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// ImageCopyFixture connects the image copy subtests to a driver whose vendor
// has regional images and a copy API.
type ImageCopyFixture struct {
	// Source and Dest are two regions of the driver: the release's official
	// image exists in Source only.
	Source, Dest string
	// Cluster is the cluster name the driver was initialised with: the copies
	// the suite starts carry it as their ownership tag.
	Cluster string
	// PublishOfficial makes the official image of the pinned release (the
	// suite's channel and version, every architecture the driver serves) appear
	// in the region.
	PublishOfficial func(t *testing.T, d vendors.Driver, region string, rel vendors.Release)
	// Finish completes the copies in flight, as the vendor's time would.
	Finish func(t *testing.T, d vendors.Driver)
}

// copyCases is what the image copy subtests work with.
type copyCases struct {
	t   *testing.T
	e   *env
	fx  *ImageCopyFixture
	cp  vendors.ImageCopier
	rel vendors.Release
}

// imageCopies runs the cases of the optional ImageCopier capability: the lookup
// of the primary image and of the sources, a copy's tags and states, an
// idempotent start, the listing after a restart, and a delete that only touches
// this cluster's copies.
func imageCopies(t *testing.T, e *env) {
	cp, ok := e.d.(vendors.ImageCopier)
	if !ok {
		t.Skip("the driver cannot copy images")
	}
	if e.fx.ImageCopy == nil {
		t.Skip("the fixture has no ImageCopy")
	}
	fx := e.fx.ImageCopy
	c := &copyCases{t: t, e: e, fx: fx, cp: cp,
		rel: vendors.Release{Channel: imageChannel, Version: imageVersion, Arch: archAMD64}}
	fx.PublishOfficial(t, e.d, fx.Source, c.rel)
	for _, tc := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"lookup", c.lookup}, {"lifecycle", c.lifecycle}, {"idempotent start", c.idempotentStart},
		{"restart", c.restart}, {"delete only copies", c.deleteOnlyCopies},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.t = t
			defer c.clear()
			tc.fn(t)
		})
	}
}

// clear deletes the copies a case left, so that the cases do not see each
// other's.
func (c *copyCases) clear() {
	for _, cp := range c.list(c.cp) {
		if err := c.cp.DeleteCopy(c.t.Context(), cp); err != nil {
			c.t.Errorf("DeleteCopy(%s): %v", cp.Ref, err)
		}
	}
}

func (c *copyCases) start(token string) string { return c.startFor(c.fx.Cluster, token) }

// startFor starts a copy that carries the cluster's ownership tag.
func (c *copyCases) startFor(cluster, token string) string {
	c.t.Helper()
	srcs, err := c.cp.CopySources(c.t.Context(), c.rel)
	if err != nil || len(srcs) == 0 {
		c.t.Fatalf("CopySources: %v, %v", srcs, err)
	}
	src := srcs[0]
	ref, err := c.cp.StartCopy(c.t.Context(), src, c.fx.Dest, c.rel, map[vendors.TagKey]string{
		vendors.TagCluster: cluster, vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: vendors.ResourceKindImageCopy, vendors.TagImageArch: c.rel.Arch,
		vendors.TagImageSource: src.Region + "/" + src.Ref,
	}, token)
	if err != nil || ref == "" {
		c.t.Fatalf("StartCopy: %q, %v", ref, err)
	}
	return ref
}

func (c *copyCases) list(d vendors.ImageCopier) []vendors.ImageCopy {
	c.t.Helper()
	got, err := d.ListCopies(c.t.Context(), []string{c.fx.Dest})
	if err != nil {
		c.t.Fatalf("ListCopies: %v", err)
	}
	return got
}

// lookup: the primary image is found where it is and not elsewhere, the sources
// name it, and the architectures are listed.
func (c *copyCases) lookup(t *testing.T) {
	if _, found, err := c.cp.Primary(t.Context(), c.fx.Source, c.rel); err != nil || !found {
		t.Errorf("Primary(source) = %v, %v; want found", found, err)
	}
	if ref, found, err := c.cp.Primary(t.Context(), c.fx.Dest, c.rel); err != nil || found {
		t.Errorf("Primary(dest) = %q, %v, %v; want not found", ref, found, err)
	}
	srcs, err := c.cp.CopySources(t.Context(), c.rel)
	if err != nil || len(srcs) == 0 || srcs[0].Region != c.fx.Source {
		t.Errorf("CopySources = %v, %v; want the source region first", srcs, err)
	}
	archs, err := c.cp.Architectures(t.Context(), c.fx.Dest)
	if err != nil || !slices.Contains(archs, archAMD64) {
		t.Errorf("Architectures = %v, %v; want amd64 among them", archs, err)
	}
	if got := c.list(c.cp); len(got) != 0 {
		t.Errorf("ListCopies before any copy = %v", got)
	}
}

// lifecycle: a started copy is listed pending with its release and source, is
// available after the vendor's time, is not in use, and is deleted (twice).
func (c *copyCases) lifecycle(t *testing.T) {
	ref := c.start("lifecycle")
	got := c.list(c.cp)
	if len(got) != 1 || got[0].Ref != ref || got[0].Region != c.fx.Dest || got[0].Release != c.rel ||
		got[0].State != vendors.CopyPending || got[0].Source == "" {
		t.Fatalf("ListCopies after the start = %+v; want %s pending in %s as %+v", got, ref, c.fx.Dest, c.rel)
	}
	c.fx.Finish(t, c.e.d)
	got = c.list(c.cp)
	if len(got) != 1 || got[0].State != vendors.CopyAvailable {
		t.Fatalf("ListCopies after the copy finished = %+v; want available", got)
	}
	if inUse, err := c.cp.CopyInUse(t.Context(), got[0]); err != nil || inUse {
		t.Errorf("CopyInUse = %v, %v; want false", inUse, err)
	}
	if err := c.cp.TouchCopy(t.Context(), got[0], time.Now()); err != nil {
		t.Errorf("TouchCopy: %v", err)
	}
	for range 2 {
		if err := c.cp.DeleteCopy(t.Context(), got[0]); err != nil {
			t.Errorf("DeleteCopy: %v (a second delete is not an error)", err)
		}
	}
	if got := c.list(c.cp); len(got) != 0 {
		t.Errorf("ListCopies after the delete = %+v", got)
	}
}

// idempotentStart: the same token returns the same image and makes one copy.
func (c *copyCases) idempotentStart(t *testing.T) {
	first, second := c.start("idem"), c.start("idem")
	if first != second {
		t.Errorf("StartCopy with one token returned %s and %s", first, second)
	}
	if got := c.list(c.cp); len(got) != 1 {
		t.Errorf("ListCopies = %+v; want one copy", got)
	}
}

// restart: a new driver over the same account finds the copy in flight and the
// finished one, so that a restarted leader adopts them.
func (c *copyCases) restart(t *testing.T) {
	if c.e.fx.Restart == nil {
		t.Skip("the fixture has no Restart")
	}
	ref := c.start("restart")
	d2 := c.e.fx.Restart(t, c.e.d)
	got := c.list(d2.(vendors.ImageCopier))
	if len(got) != 1 || got[0].Ref != ref || got[0].State != vendors.CopyPending || got[0].Release != c.rel {
		t.Fatalf("ListCopies after the restart = %+v; want %s pending", got, ref)
	}
	c.fx.Finish(t, d2)
	if got := c.list(d2.(vendors.ImageCopier)); len(got) != 1 || got[0].State != vendors.CopyAvailable {
		t.Errorf("ListCopies after the copy finished = %+v; want available", got)
	}
}

// deleteOnlyCopies: an image that is not a copy of this cluster (the official
// image, or a copy another cluster made) is refused as Fatal and left in place,
// and another cluster's copy is not listed.
func (c *copyCases) deleteOnlyCopies(t *testing.T) {
	ref, found, err := c.cp.Primary(t.Context(), c.fx.Source, c.rel)
	if err != nil || !found {
		t.Fatalf("Primary: %q, %v, %v", ref, found, err)
	}
	err = c.cp.DeleteCopy(t.Context(), vendors.ImageCopy{Region: c.fx.Source, Ref: ref, Release: c.rel})
	if !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("DeleteCopy of the official image = %v; want a Fatal refusal", err)
	}
	if _, found, err := c.cp.Primary(t.Context(), c.fx.Source, c.rel); err != nil || !found {
		t.Errorf("the official image is gone after the refused delete: %v, %v", found, err)
	}
	foreign := c.startFor("another-cluster", "foreign")
	if got := c.list(c.cp); len(got) != 0 {
		t.Errorf("ListCopies = %+v; another cluster's copy must not be listed", got)
	}
	err = c.cp.DeleteCopy(t.Context(), vendors.ImageCopy{Region: c.fx.Dest, Ref: foreign, Release: c.rel})
	if !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("DeleteCopy of another cluster's copy = %v; want a Fatal refusal", err)
	}
}
